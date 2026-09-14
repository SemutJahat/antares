package agent

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enowdev/antares/internal/config"
)

// newAdmissionAgent builds a bare Agent wired for admission tests. The rest
// of the runtime (store, tools, providers) is untouched: every assertion in
// this file exercises Prepare/Close/Run/RunQueued outcomes only, never the
// agent's private counters.
func newAdmissionAgent(maxConcurrent int) *Agent {
	a := &Agent{active: map[string]context.CancelFunc{}}
	a.cfg.Store(&config.Config{MaxConcurrentSessions: maxConcurrent})
	return a
}

func mustPrepare(t *testing.T, a *Agent, id string, depth int) *PreparedRun {
	t.Helper()
	p, err := a.Prepare(context.Background(), Request{SessionID: id, Depth: depth})
	if err != nil {
		t.Fatalf("prepare %q depth=%d: %v", id, depth, err)
	}
	t.Cleanup(p.Close)
	return p
}

// A cap of one lets the parent session in and rejects the next top-level
// caller with the concurrent-limit sentinel — the whole point of the cap.
func TestPrepareEnforcesTopLevelConcurrencyLimit(t *testing.T) {
	a := newAdmissionAgent(1)

	mustPrepare(t, a, "parent", 0)

	if _, err := a.Prepare(context.Background(), Request{SessionID: "other", Depth: 0}); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("second top-level Prepare: want ErrSessionLimit, got %v", err)
	}
}

// Preparing the same session twice must be refused with ErrSessionBusy, or
// two turns would race on the same conversation.
func TestPrepareRejectsDuplicateSession(t *testing.T) {
	a := newAdmissionAgent(4)

	mustPrepare(t, a, "ses-1", 0)

	if _, err := a.Prepare(context.Background(), Request{SessionID: "ses-1", Depth: 0}); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("duplicate Prepare: want ErrSessionBusy, got %v", err)
	}
}

// A child run (Depth>0) does not consume a top-level slot, so it succeeds
// even when the cap is saturated by parents.
func TestPrepareChildBypassesTopLevelCap(t *testing.T) {
	a := newAdmissionAgent(1)

	mustPrepare(t, a, "parent", 0)
	mustPrepare(t, a, "child", 1)

	// The parent's slot is still taken — another top-level Prepare must fail
	// while a child slipped through, confirming children do not count.
	if _, err := a.Prepare(context.Background(), Request{SessionID: "other-parent", Depth: 0}); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("top-level Prepare under saturated cap after child: want ErrSessionLimit, got %v", err)
	}
}

// An already-canceled context must not sneak past Prepare — otherwise a
// caller that gave up would still burn a slot. Observable via the next
// Prepare succeeding under a cap of one.
func TestPrepareRejectsCanceledContext(t *testing.T) {
	a := newAdmissionAgent(1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if p, err := a.Prepare(ctx, Request{SessionID: "ghost", Depth: 0}); err == nil {
		p.Close()
		t.Fatal("Prepare with canceled context: want error, got nil")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare with canceled context: want context.Canceled, got %v", err)
	}

	// If the rejected Prepare had reserved the slot, this call would fail.
	mustPrepare(t, a, "real", 0)
}

// Close returns capacity, is safe to call more than once, and never
// double-releases the top-level slot. Observed by draining and re-taking the
// cap-1 slot several times.
func TestPrepareCloseIsIdempotentAndReleasesCapacity(t *testing.T) {
	a := newAdmissionAgent(1)

	for i := range 3 {
		p, err := a.Prepare(context.Background(), Request{SessionID: "ses", Depth: 0})
		if err != nil {
			t.Fatalf("iteration %d: prepare: %v", i, err)
		}
		p.Close()
		p.Close() // idempotent — a second Close must not corrupt capacity.
	}

	// After three take/release cycles the slot must still be reusable.
	mustPrepare(t, a, "ses", 0)
}

// Cap 0 means unlimited: many top-level reservations must co-exist. Observed
// by admitting many in a row without any ErrSessionLimit.
func TestPrepareUnlimitedWhenCapIsZero(t *testing.T) {
	a := newAdmissionAgent(0)

	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if _, err := a.Prepare(context.Background(), Request{SessionID: id, Depth: 0}); err != nil {
			t.Fatalf("unlimited cap: Prepare %q rejected: %v", id, err)
		}
	}
}

// A batch of concurrent Prepare goroutines against cap 3 must admit exactly
// three and reject the rest with ErrSessionLimit — no torn counter, no
// under- or over-admission. All goroutines hold their permits at a barrier
// until every attempt has landed, so we observe the steady-state, not a
// serialised trickle.
func TestPrepareConcurrentAdmissionsMatchCap(t *testing.T) {
	const cap = 3
	const attempts = 12

	a := newAdmissionAgent(cap)

	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(attempts)

	var (
		admitted atomic.Int32
		limited  atomic.Int32
		other    atomic.Int32
	)

	for i := range attempts {
		id := "ses-" + string(rune('a'+i))
		go func() {
			defer wg.Done()
			p, err := a.Prepare(context.Background(), Request{SessionID: id, Depth: 0})
			switch {
			case err == nil:
				admitted.Add(1)
				<-release // hold the permit so the cap stays saturated.
				p.Close()
			case errors.Is(err, ErrSessionLimit):
				limited.Add(1)
			default:
				other.Add(1)
			}
		}()
	}

	// Let every goroutine finish its Prepare attempt before releasing holders.
	// A polling loop lets us wait without a sleep: settle when admitted +
	// limited == attempts.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if int(admitted.Load()+limited.Load()+other.Load()) == attempts {
			break
		}
		runtime.Gosched()
	}

	if got := admitted.Load(); got != cap {
		close(release)
		wg.Wait()
		t.Fatalf("concurrent admissions: want exactly %d admitted, got %d (limited=%d other=%d)", cap, got, limited.Load(), other.Load())
	}
	if got := limited.Load(); got != attempts-cap {
		close(release)
		wg.Wait()
		t.Fatalf("concurrent admissions: want %d rejected with ErrSessionLimit, got %d (admitted=%d other=%d)", attempts-cap, got, admitted.Load(), other.Load())
	}
	if got := other.Load(); got != 0 {
		close(release)
		wg.Wait()
		t.Fatalf("concurrent admissions: %d attempts returned an unexpected error", got)
	}

	close(release)
	wg.Wait()

	// Once every holder released, the cap is fully reusable.
	for _, id := range []string{"drain-1", "drain-2", "drain-3"} {
		mustPrepare(t, a, id, 0)
	}
}

// Close called before Run releases capacity immediately (started=false path).
// Run afterwards must return context.Canceled without touching the provider,
// and the permit MUST NOT be released a second time. Observed by re-taking
// the cap-1 slot exactly once with a fresh Prepare between Close and Run.
func TestCloseBeforeRunReleasesImmediatelyAndRunIsInert(t *testing.T) {
	a := newAdmissionAgent(1)

	p, err := a.Prepare(context.Background(), Request{SessionID: "holder", Depth: 0})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	p.Close()

	// Permit is genuinely back: Prepare a second session under cap 1.
	reclaim, err := a.Prepare(context.Background(), Request{SessionID: "reclaim", Depth: 0})
	if err != nil {
		t.Fatalf("Prepare after pre-Run Close: want success, got %v", err)
	}

	// Run on the closed PreparedRun MUST NOT execute the model loop nor
	// release the (already-held-by-reclaim) permit a second time. It surfaces
	// context.Canceled per the closed-guard in PreparedRun.Run.
	if _, err := p.Run(func(Event) error {
		t.Fatal("emit invoked on a Run started after Close: provider path must not be reached")
		return nil
	}); !errors.Is(err, context.Canceled) {
		reclaim.Close()
		t.Fatalf("Run after Close: want context.Canceled, got %v", err)
	}

	// Third Prepare must still fail — reclaim still holds the only slot, and
	// p.Run above must NOT have double-released.
	if _, err := a.Prepare(context.Background(), Request{SessionID: "third", Depth: 0}); !errors.Is(err, ErrSessionLimit) {
		reclaim.Close()
		t.Fatalf("cap must remain saturated by reclaim; got %v", err)
	}

	reclaim.Close()
}

// Canceling the context that Prepare was called with does NOT release the
// permit — only Close (or Run returning) does. A client that goes away must
// still call Close; otherwise the slot leaks. Observed by probing capacity
// with a fresh Prepare before and after Close.
func TestHolderContextCancelDoesNotReleasePermit(t *testing.T) {
	a := newAdmissionAgent(1)

	holderCtx, cancelHolder := context.WithCancel(context.Background())
	holder, err := a.Prepare(holderCtx, Request{SessionID: "holder", Depth: 0})
	if err != nil {
		t.Fatalf("prepare holder: %v", err)
	}

	// Cancel the holder's ctx. The permit MUST still be held.
	cancelHolder()
	if _, err := a.Prepare(context.Background(), Request{SessionID: "probe-1", Depth: 0}); !errors.Is(err, ErrSessionLimit) {
		holder.Close()
		t.Fatalf("Prepare after holder ctx cancel (no Close): want ErrSessionLimit, got %v", err)
	}

	// Close is what actually returns the permit.
	holder.Close()
	mustPrepare(t, a, "probe-2", 0)
}

// holder.Close returns the permit and MUST admit exactly one of many
// contending Prepare callers — no torn count, no over-admission. Under cap
// 1, N probes spin in the background trying to Prepare; none succeed while
// the holder is live; the moment holder.Close fires, precisely one wins.
func TestCloseUnblocksSubsequentPrepareCallers(t *testing.T) {
	a := newAdmissionAgent(1)

	holder, err := a.Prepare(context.Background(), Request{SessionID: "holder", Depth: 0})
	if err != nil {
		t.Fatalf("prepare holder: %v", err)
	}

	const probes = 8
	start := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(probes)

	var admitted atomic.Int32
	var refused atomic.Int32
	results := make(chan *PreparedRun, probes)

	for i := range probes {
		id := "probe-" + string(rune('0'+i))
		go func() {
			defer wg.Done()
			<-start
			seenRefusal := false
			for {
				select {
				case <-stop:
					return
				default:
				}
				p, err := a.Prepare(context.Background(), Request{SessionID: id, Depth: 0})
				if err == nil {
					admitted.Add(1)
					results <- p
					return
				}
				if !errors.Is(err, ErrSessionLimit) {
					return
				}
				if !seenRefusal {
					seenRefusal = true
					refused.Add(1)
				}
				runtime.Gosched()
			}
		}()
	}

	// Fire the probes and wait for every one to observe ErrSessionLimit at
	// least once. That is a positive rendezvous — no timed sleep — proving
	// each probe genuinely ran against a saturated cap before we release.
	close(start)
	waitDeadline := time.Now().Add(2 * time.Second)
	for refused.Load() < probes {
		if time.Now().After(waitDeadline) {
			close(stop)
			wg.Wait()
			t.Fatalf("only %d/%d probes observed ErrSessionLimit before release", refused.Load(), probes)
		}
		runtime.Gosched()
	}

	if got := admitted.Load(); got != 0 {
		close(stop)
		wg.Wait()
		t.Fatalf("probe admitted while holder held the only permit: got %d admitted", got)
	}

	// Release the holder. Exactly one probe must succeed.
	holder.Close()

	// Wait for the winner to record, then stop the rest.
	winDeadline := time.Now().Add(2 * time.Second)
	for admitted.Load() == 0 {
		if time.Now().After(winDeadline) {
			close(stop)
			wg.Wait()
			t.Fatal("no probe admitted after holder.Close")
		}
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()
	close(results)

	if got := admitted.Load(); got != 1 {
		t.Fatalf("holder.Close should admit exactly one probe; got %d", got)
	}
	for p := range results {
		t.Cleanup(p.Close)
	}
}

// Run on a PreparedRun whose context was canceled after admission must
// surface the cancellation, release its slot, and refuse to re-enter.
func TestPreparedRunHonoursCanceledContextAndReleases(t *testing.T) {
	a := newAdmissionAgent(1)

	ctx, cancel := context.WithCancel(context.Background())
	p, err := a.Prepare(ctx, Request{SessionID: "ses", Depth: 0})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// Cancel after successful admission; Run must refuse to enter the model
	// loop and unwind the reservation via its deferred release.
	cancel()

	res, err := p.Run(func(Event) error { return nil })
	if res != nil {
		t.Fatalf("canceled Run returned a non-nil result: %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Run: want context.Canceled, got %v", err)
	}

	// A second Run on the already-consumed PreparedRun must not re-enter.
	if _, err := p.Run(func(Event) error { return nil }); err == nil {
		t.Fatal("second Run on a consumed PreparedRun: want error, got nil")
	}

	// The permit is genuinely free again: the next Prepare under cap 1 wins.
	mustPrepare(t, a, "next", 0)
}

// RunQueued blocks until capacity frees. A canceled context on the waiter
// must return the context error, not stall, and no provider is engaged.
func TestRunQueuedReturnsContextErrorWhenCanceledWhileWaiting(t *testing.T) {
	a := newAdmissionAgent(1)

	// Fill the only slot with an idle PreparedRun (never Run — cleanup Closes it).
	mustPrepare(t, a, "holder", 0)

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.RunQueued(waiterCtx, Request{SessionID: "waiter", Depth: 0}, func(Event) error { return nil })
		done <- err
	}()

	// Confirm the waiter is parked: it must not return before the context
	// is canceled and no slot has been freed.
	select {
	case err := <-done:
		t.Fatalf("RunQueued returned before context cancel and before capacity freed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancelWaiter()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunQueued after ctx cancel: want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunQueued did not return after its context was canceled")
	}
}
