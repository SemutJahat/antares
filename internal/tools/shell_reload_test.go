package tools

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enowdev/antares/internal/config"
)

// TestReconfigureNoopKeepsExistingShellReusable proves that Reconfiguring with
// an equal config does not disturb a live persistent shell: exported state set
// before the reload survives after it. If Reconfigure treated equal configs as
// a swap the retire path would either kill the shell or refuse the second run.
func TestReconfigureNoopKeepsExistingShellReusable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX persistent shell protocol does not apply on Windows")
	}
	cfg := config.Terminal{Sandbox: "none"}
	m := NewShellManager(cfg)
	t.Cleanup(m.CloseAll)

	sess, err := m.session("noop-session", t.TempDir())
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if _, code, err := sess.run(context.Background(), "export ANTARES_MARK=one", 2*time.Second, nil); err != nil || code != 0 {
		t.Fatalf("setting env: code %d err %v", code, err)
	}

	m.Reconfigure(cfg)

	sess2, err := m.session("noop-session", t.TempDir())
	if err != nil {
		t.Fatalf("second session after equal reconfigure: %v", err)
	}
	if sess2 != sess {
		t.Fatalf("equal-config Reconfigure replaced the persistent shell; wanted the same instance")
	}
	out, code, err := sess2.run(context.Background(), "printf %s \"$ANTARES_MARK\"", 2*time.Second, nil)
	if err != nil || code != 0 {
		t.Fatalf("read env: code %d err %v out=%q", code, err, out)
	}
	if out != "one" {
		t.Fatalf("exported env lost across equal-config Reconfigure: got %q, want %q", out, "one")
	}
}

// TestReconfigureRetiresIdleSessionOnNextLookup proves the retire branch of
// session(): after Reconfigure with a changed config, the next session()
// lookup for an idle shell hands out a *new* shell and the previous one is no
// longer usable for a subsequent command. The observable behavior is exported
// state loss: the pre-reload export is not visible in the new shell.
func TestReconfigureRetiresIdleSessionOnNextLookup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX persistent shell protocol does not apply on Windows")
	}
	m := NewShellManager(config.Terminal{Sandbox: "none"})
	t.Cleanup(m.CloseAll)

	sess, err := m.session("retire-session", t.TempDir())
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if _, code, err := sess.run(context.Background(), "export ANTARES_MARK=before", 2*time.Second, nil); err != nil || code != 0 {
		t.Fatalf("setting env: code %d err %v", code, err)
	}
	// Session is now idle (lease released by run's defer). Change the config.
	m.Reconfigure(config.Terminal{Sandbox: "none", HomeMode: "isolated"})

	fresh, err := m.session("retire-session", t.TempDir())
	if err != nil {
		t.Fatalf("session after reconfigure: %v", err)
	}
	if fresh == sess {
		t.Fatalf("Reconfigure did not retire the idle stale shell")
	}
	out, code, err := fresh.run(context.Background(), "printf %s \"${ANTARES_MARK:-empty}\"", 2*time.Second, nil)
	if err != nil || code != 0 {
		t.Fatalf("read env in fresh shell: code %d err %v", code, err)
	}
	if out != "empty" {
		t.Fatalf("new shell after reconfigure inherited stale env %q; want fresh process", out)
	}
}

// TestReconfigureRefusesRetireWhileCommandRuns is the load-bearing race test:
// a foreground command is still running when Reconfigure swaps the config,
// and a concurrent session() lookup arrives. The refusal path must fire
// (ErrShellConfigChanged) rather than killing the shell mid-command. The
// running command MUST complete on its original shell.
func TestReconfigureRefusesRetireWhileCommandRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX persistent shell protocol does not apply on Windows")
	}
	m := NewShellManager(config.Terminal{Sandbox: "none"})
	t.Cleanup(m.CloseAll)

	sess, err := m.session("busy-session", t.TempDir())
	if err != nil {
		t.Fatalf("first session: %v", err)
	}

	var (
		out  string
		code int
		runE error
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A short sleep is a foreground command in flight when Reconfigure fires.
		out, code, runE = sess.run(context.Background(), "sleep 1; printf done", 5*time.Second, nil)
	}()

	// Give the goroutine time to enter sess.mu.Lock() inside run.
	time.Sleep(150 * time.Millisecond)
	m.Reconfigure(config.Terminal{Sandbox: "none", HomeMode: "isolated"})

	// While the command is still running, a concurrent session() lookup for
	// the same id must refuse rather than kill it.
	if _, err := m.session("busy-session", t.TempDir()); !errors.Is(err, ErrShellConfigChanged) {
		t.Fatalf("session() during busy stale shell = %v, want ErrShellConfigChanged", err)
	}

	wg.Wait()
	if runE != nil {
		t.Fatalf("in-flight command was disrupted: %v (out=%q code=%d)", runE, out, code)
	}
	if code != 0 || out != "done" {
		t.Fatalf("in-flight command result = (%q, %d), want (%q, 0)", out, code, "done")
	}

	// After the busy command finishes, the next lookup must retire it and
	// return a fresh shell.
	fresh, err := m.session("busy-session", t.TempDir())
	if err != nil {
		t.Fatalf("session after busy shell drained: %v", err)
	}
	if fresh == sess {
		t.Fatalf("stale shell was not retired after its command finished")
	}
}

// TestReconfigureDoesNotRaceSessionRunGap covers the specific window between
// session() returning to the caller and the caller invoking run(). If the
// lease weren't held across that gap, a concurrent Reconfigure+session()
// racing pair could kill the not-yet-started foreground command. This test
// exercises the gap directly: session() returns, then Reconfigure fires,
// then a concurrent session() lookup for the same id — the second lookup
// must refuse (ErrShellConfigChanged) because the lease held by the first
// caller keeps the shell protected. run() on the leased handle then succeeds.
func TestReconfigureDoesNotRaceSessionRunGap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX persistent shell protocol does not apply on Windows")
	}
	m := NewShellManager(config.Terminal{Sandbox: "none"})
	t.Cleanup(m.CloseAll)

	sess, err := m.session("gap-session", t.TempDir())
	if err != nil {
		t.Fatalf("first session: %v", err)
	}

	// The caller has a lease on sess but has NOT called run() yet.
	m.Reconfigure(config.Terminal{Sandbox: "none", HomeMode: "isolated"})

	// A concurrent session() lookup must refuse because our lease is live.
	if _, err := m.session("gap-session", t.TempDir()); !errors.Is(err, ErrShellConfigChanged) {
		t.Fatalf("concurrent session() during leased-but-not-yet-run gap = %v, want ErrShellConfigChanged", err)
	}

	// The original caller's run() must succeed on the still-alive shell.
	out, code, err := sess.run(context.Background(), "printf survived", 2*time.Second, nil)
	if err != nil || code != 0 {
		t.Fatalf("run on leased handle failed: code %d err %v out=%q", code, err, out)
	}
	if !strings.Contains(out, "survived") {
		t.Fatalf("run on leased handle produced %q, want 'survived'", out)
	}
}
