package cron

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enowdev/antares/internal/store"
)

// memStore is a minimum viable store for exercising the runner without a
// database. Only the four methods the runner actually calls are implemented;
// every other Store method panics via the embedded nil interface, which is
// exactly the noise we want if a test accidentally reaches for one.
type memStore struct {
	store.Store

	mu   sync.Mutex
	jobs map[string]store.CronJob
	runs []store.CronRun
}

func newMemStore(jobs ...store.CronJob) *memStore {
	m := &memStore{jobs: map[string]store.CronJob{}}
	for _, j := range jobs {
		m.jobs[j.ID] = j
	}
	return m
}

func (m *memStore) GetCronJob(_ context.Context, id string) (*store.CronJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := j
	return &cp, nil
}

func (m *memStore) ListCronJobs(_ context.Context) ([]store.CronJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.CronJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	return out, nil
}

func (m *memStore) PutCronJob(_ context.Context, j *store.CronJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.ID] = *j
	return nil
}

func (m *memStore) PutCronRun(_ context.Context, r *store.CronRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Replace by id if it already exists (execute writes twice: start, then result).
	for i := range m.runs {
		if m.runs[i].ID == r.ID {
			m.runs[i] = *r
			return nil
		}
	}
	m.runs = append(m.runs, *r)
	return nil
}

// blockingExec is an executor that parks until released, so the test can
// hold a slot open and observe admission decisions.
type blockingExec struct {
	started chan struct{} // signalled once per admitted run
	release chan struct{} // callers close to let every parked run finish
	count   atomic.Int32
}

func newBlockingExec() *blockingExec {
	return &blockingExec{
		started: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (b *blockingExec) fn() Executor {
	return func(ctx context.Context, job store.CronJob) (string, string, error) {
		b.count.Add(1)
		b.started <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
		}
		return "sess-" + job.ID, "ok", nil
	}
}

// waitStarted blocks until n runs have entered the executor, or fails the
// test if that does not happen quickly.
func (b *blockingExec) waitStarted(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for i := range n {
		select {
		case <-b.started:
		case <-deadline:
			t.Fatalf("only %d/%d runs started within timeout", i, n)
		}
	}
}

func newTestJob(id string) store.CronJob {
	return store.CronJob{
		ID:       id,
		Name:     id,
		Schedule: "* * * * *",
		Prompt:   "run " + id,
		Enabled:  true,
	}
}

func TestReconfigureValidatesInputs(t *testing.T) {
	r := New(Options{Store: newMemStore()})

	if err := r.Reconfigure(true, "Not/A/Zone", 3, 100); err == nil {
		t.Fatal("expected invalid timezone to be rejected")
	}
	if err := r.Reconfigure(true, "UTC", 0, 100); err == nil {
		t.Fatal("expected non-positive maxConcurrent to be rejected")
	}
	if err := r.Reconfigure(true, "UTC", 3, -1); err == nil {
		t.Fatal("expected non-positive historyLimit to be rejected")
	}

	// A rejected reconfigure must not leak state onto the runner. The
	// original defaults from New should still be in place.
	if got := r.Location(); got == nil || got.String() == "Not/A/Zone" {
		t.Fatalf("timezone changed after a rejected reconfigure: %v", got)
	}

	if err := r.Reconfigure(false, "UTC", 5, 42); err != nil {
		t.Fatalf("valid reconfigure failed: %v", err)
	}
	enabled, loc, max, hist := r.snapshot()
	if enabled {
		t.Fatal("enabled should be false after reconfigure")
	}
	if loc.String() != "UTC" {
		t.Fatalf("location = %s, want UTC", loc.String())
	}
	if max != 5 || hist != 42 {
		t.Fatalf("max=%d hist=%d, want 5/42", max, hist)
	}
}

func TestRunNowRespectsDuplicateGuard(t *testing.T) {
	db := newMemStore(newTestJob("j1"))
	exec := newBlockingExec()
	r := New(Options{Store: db, Execute: exec.fn(), MaxConcurrent: 3})

	if err := r.RunNow(context.Background(), "j1"); err != nil {
		t.Fatalf("first RunNow: %v", err)
	}
	exec.waitStarted(t, 1)

	err := r.RunNow(context.Background(), "j1")
	if err == nil {
		t.Fatal("expected duplicate RunNow to return an error")
	}

	close(exec.release)
}

func TestRunNowRespectsConcurrencyLimit(t *testing.T) {
	jobs := []store.CronJob{newTestJob("a"), newTestJob("b"), newTestJob("c")}
	db := newMemStore(jobs...)
	exec := newBlockingExec()
	r := New(Options{Store: db, Execute: exec.fn(), MaxConcurrent: 2})

	if err := r.RunNow(context.Background(), "a"); err != nil {
		t.Fatalf("RunNow a: %v", err)
	}
	if err := r.RunNow(context.Background(), "b"); err != nil {
		t.Fatalf("RunNow b: %v", err)
	}
	exec.waitStarted(t, 2)

	if err := r.RunNow(context.Background(), "c"); err == nil {
		t.Fatal("expected capacity-busy error for third RunNow")
	}

	close(exec.release)
}

func TestLaunchPreservesNextRunWhenBusy(t *testing.T) {
	// Two jobs share the runner: 'busy' holds the only slot, then a second
	// scheduled activation for 'other' arrives and must be deferred without
	// consuming its next_run.
	nextRun := time.Now().Add(-1 * time.Minute)
	busy := newTestJob("busy")
	busy.NextRun = &nextRun
	other := newTestJob("other")
	other.NextRun = &nextRun

	memory := newMemStore(busy, other)
	exec := newBlockingExec()
	r := New(Options{Store: memory, Execute: exec.fn(), MaxConcurrent: 1})

	// Occupy the single slot.
	if err := r.RunNow(context.Background(), "busy"); err != nil {
		t.Fatalf("RunNow busy: %v", err)
	}
	exec.waitStarted(t, 1)

	// A scheduled tick for 'other' should observe capacity pressure and
	// leave next_run untouched so the next tick retries.
	r.tick(context.Background(), time.Now())

	got, err := memory.GetCronJob(context.Background(), "other")
	if err != nil {
		t.Fatalf("get other: %v", err)
	}
	if got.NextRun == nil {
		t.Fatal("next_run was cleared even though the job never ran")
	}
	if !got.NextRun.Equal(nextRun) {
		t.Fatalf("next_run advanced past the busy tick: %v vs %v", got.NextRun, nextRun)
	}
	if exec.count.Load() != 1 {
		t.Fatalf("executor ran for the deferred job: count=%d", exec.count.Load())
	}

	close(exec.release)
}

func TestReconfigureEnableDisableViaLaunch(t *testing.T) {
	// A disabled runner rejects scheduled launches at the admission gate
	// so a tick that races with disable cannot slip a job through.
	past := time.Now().Add(-1 * time.Minute)
	job := newTestJob("cron-me")
	job.NextRun = &past

	memory := newMemStore(job)
	exec := newBlockingExec()
	r := New(Options{Store: memory, Execute: exec.fn(), MaxConcurrent: 1, Enabled: boolPtr(false)})

	// While disabled, launch admits nothing; next_run is preserved.
	r.launch(context.Background(), job)
	if exec.count.Load() != 0 {
		t.Fatalf("disabled runner launched a job: count=%d", exec.count.Load())
	}
	got, err := memory.GetCronJob(context.Background(), "cron-me")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.NextRun == nil || !got.NextRun.Equal(past) {
		t.Fatalf("next_run advanced while disabled: %v", got.NextRun)
	}

	// Enable and try again.
	if err := r.Reconfigure(true, "UTC", 1, 100); err != nil {
		t.Fatalf("reconfigure enable: %v", err)
	}
	r.launch(context.Background(), job)
	exec.waitStarted(t, 1)

	// Disable while a job is in flight: the running job must not be killed.
	if err := r.Reconfigure(false, "UTC", 1, 100); err != nil {
		t.Fatalf("reconfigure disable: %v", err)
	}
	close(exec.release)
	waitFor(t, 2*time.Second, func() bool {
		memory.mu.Lock()
		defer memory.mu.Unlock()
		for _, run := range memory.runs {
			if run.JobID == "cron-me" && run.Status == "ok" {
				return true
			}
		}
		return false
	})
}

func TestRunNowAllowedWhileDisabled(t *testing.T) {
	// RunNow is intentionally allowed when scheduling is disabled so an
	// operator can trigger a job manually without flipping the global switch.
	db := newMemStore(newTestJob("manual"))
	exec := newBlockingExec()
	r := New(Options{Store: db, Execute: exec.fn(), MaxConcurrent: 1, Enabled: boolPtr(false)})

	if err := r.RunNow(context.Background(), "manual"); err != nil {
		t.Fatalf("RunNow while disabled: %v", err)
	}
	exec.waitStarted(t, 1)
	close(exec.release)
}

func TestReconfigureNoOpDoesNotNotify(t *testing.T) {
	// A reconfigure that changes nothing must not signal the loop; that
	// would postpone due/overdue jobs on every unrelated config save.
	r := New(Options{Store: newMemStore()})
	if err := r.Reconfigure(true, "UTC", 4, 200); err != nil {
		t.Fatalf("initial reconfigure: %v", err)
	}
	// Drain the signal from the first reconfigure (timezone changed).
	select {
	case <-r.configCh:
	default:
	}
	// A no-op reconfigure must not fill the channel.
	if err := r.Reconfigure(true, "UTC", 4, 200); err != nil {
		t.Fatalf("no-op reconfigure: %v", err)
	}
	select {
	case <-r.configCh:
		t.Fatal("no-op Reconfigure signalled the loop")
	default:
	}
	// Changing only concurrency/history must not signal either.
	if err := r.Reconfigure(true, "UTC", 8, 400); err != nil {
		t.Fatalf("cap-only reconfigure: %v", err)
	}
	select {
	case <-r.configCh:
		t.Fatal("cap-only Reconfigure signalled the loop")
	default:
	}
	// Changing enabled MUST signal.
	if err := r.Reconfigure(false, "UTC", 8, 400); err != nil {
		t.Fatalf("enable-flip reconfigure: %v", err)
	}
	select {
	case <-r.configCh:
	default:
		t.Fatal("enable-flip Reconfigure did not signal the loop")
	}
}

func TestReconfigureNotifyIsNonBlocking(t *testing.T) {
	// Reconfigure must never block on a full notification channel; two
	// back-to-back reconfigures without a consumer would deadlock a naive
	// unbuffered implementation.
	r := New(Options{Store: newMemStore()})

	done := make(chan error, 1)
	go func() {
		// Both calls trigger notify (enabled flag alternates), so the
		// second finds the buffered channel full and MUST drop instead
		// of blocking.
		if err := r.Reconfigure(true, "UTC", 1, 100); err != nil {
			done <- err
			return
		}
		if err := r.Reconfigure(false, "UTC", 1, 100); err != nil {
			done <- err
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconfigure: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reconfigure blocked on notification channel")
	}
}

func TestRunNowSurfacesMissingJob(t *testing.T) {
	r := New(Options{Store: newMemStore(), Execute: func(context.Context, store.CronJob) (string, string, error) {
		return "", "", errors.New("unreachable")
	}})
	if err := r.RunNow(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown job")
	}
}

func boolPtr(b bool) *bool { return &b }

// waitFor polls until cond returns true or the deadline elapses.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}
