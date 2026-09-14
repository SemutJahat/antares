package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/enowdev/antares/internal/store"
)

// Executor runs one scheduled job's prompt and returns its final reply.
type Executor func(ctx context.Context, job store.CronJob) (sessionID, reply string, err error)

// Deliverer optionally forwards a job's output to a messaging target.
type Deliverer func(ctx context.Context, target, text string) error

// Runner ticks once a minute and executes jobs whose schedule has come due.
// Runtime configuration is swappable via Reconfigure without stopping the
// loop or cancelling jobs that are already executing.
type Runner struct {
	db      store.Store
	exec    Executor
	deliver Deliverer

	// cfg guards fields Reconfigure writes.
	cfg           sync.RWMutex
	enabled       bool
	location      *time.Location
	maxConcurrent int
	historyLimit  int

	// configCh coalesces reconfigure notifications.
	configCh chan struct{}

	// mu serializes admission and guards running.
	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// Options configures a Runner.
type Options struct {
	Store         store.Store
	Execute       Executor
	Deliver       Deliverer
	Timezone      string
	MaxConcurrent int
	HistoryLimit  int
	// Enabled controls scheduled activations. nil defaults to true; Main
	// should call Reconfigure at boot to apply the configured value.
	Enabled *bool
}

// New builds a Runner defaulting to enabled=true unless Options.Enabled is
// non-nil.
func New(o Options) *Runner {
	loc := parseTimezone(o.Timezone)
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 3
	}
	if o.HistoryLimit <= 0 {
		o.HistoryLimit = 200
	}
	enabled := true
	if o.Enabled != nil {
		enabled = *o.Enabled
	}
	return &Runner{
		db:            o.Store,
		exec:          o.Execute,
		deliver:       o.Deliver,
		location:      loc,
		enabled:       enabled,
		maxConcurrent: o.MaxConcurrent,
		historyLimit:  o.HistoryLimit,
		configCh:      make(chan struct{}, 1),
		running:       map[string]context.CancelFunc{},
	}
}

// parseTimezone resolves a timezone, warning and falling back to time.Local
// on unknown names.
func parseTimezone(tz string) *time.Location {
	tz = strings.TrimSpace(tz)
	if tz == "" || strings.EqualFold(tz, "local") {
		return time.Local
	}
	l, err := time.LoadLocation(tz)
	if err != nil {
		slog.Warn("unknown cron timezone; falling back to local", "timezone", tz, "error", err)
		return time.Local
	}
	return l
}

// validateTimezone returns the resolved location or an error, without side
// effects, so Reconfigure can reject bad input before mutating state.
func validateTimezone(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" || strings.EqualFold(tz, "local") {
		return time.Local, nil
	}
	l, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("cron: invalid timezone %q: %w", tz, err)
	}
	return l, nil
}

// Reconfigure applies new runtime configuration atomically. Inputs are
// validated first; a rejected call leaves the runner untouched. A recompute
// is signalled only when the timezone or enabled flag actually changes.
func (r *Runner) Reconfigure(enabled bool, timezone string, maxConcurrent int, historyLimit int) error {
	loc, err := validateTimezone(timezone)
	if err != nil {
		return err
	}
	if maxConcurrent <= 0 {
		return fmt.Errorf("cron: max_concurrent must be positive, got %d", maxConcurrent)
	}
	if historyLimit <= 0 {
		return fmt.Errorf("cron: history_limit must be positive, got %d", historyLimit)
	}

	// r.mu is taken with r.cfg so admit sees an atomic view of the cap.
	r.mu.Lock()
	r.cfg.Lock()
	scheduleChanged := r.enabled != enabled || !sameLocation(r.location, loc)
	unchanged := !scheduleChanged && r.maxConcurrent == maxConcurrent && r.historyLimit == historyLimit
	if unchanged {
		r.cfg.Unlock()
		r.mu.Unlock()
		return nil
	}
	r.enabled = enabled
	r.location = loc
	r.maxConcurrent = maxConcurrent
	r.historyLimit = historyLimit
	r.cfg.Unlock()
	r.mu.Unlock()

	if scheduleChanged {
		select {
		case r.configCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// sameLocation compares two *time.Location by identity or name.
func sameLocation(a, b *time.Location) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.String() == b.String()
}

// snapshot copies the runtime configuration under the read lock.
func (r *Runner) snapshot() (enabled bool, loc *time.Location, maxConcurrent int, historyLimit int) {
	r.cfg.RLock()
	defer r.cfg.RUnlock()
	return r.enabled, r.location, r.maxConcurrent, r.historyLimit
}

// Start runs the scheduler loop until ctx is cancelled. The loop runs even
// while disabled so Reconfigure can re-enable scheduling without a restart.
func (r *Runner) Start(ctx context.Context) {
	// Refresh next_run for every job so the dashboard is accurate immediately.
	if err := r.recomputeAll(ctx); err != nil {
		slog.Warn("cron: initial schedule computation failed", "error", err)
	}

	_, loc, _, _ := r.snapshot()
	slog.Info("cron scheduler started", "timezone", loc.String())

	// Align the first tick to the next whole minute so jobs fire on time.
	now := time.Now()
	first := now.Truncate(time.Minute).Add(time.Minute).Sub(now)
	timer := time.NewTimer(first)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("cron scheduler stopped")
			return
		case <-r.configCh:
			// Timezone or enable state moved; refresh next_run.
			if err := r.recomputeAll(ctx); err != nil {
				slog.Warn("cron: reconfigure recompute failed", "error", err)
			}
		case <-timer.C:
			enabled, loc, _, _ := r.snapshot()
			if enabled {
				r.tick(ctx, time.Now().In(loc))
			}
			next := time.Now()
			timer.Reset(next.Truncate(time.Minute).Add(time.Minute).Sub(next))
		}
	}
}

// tick launches every job that is due at t.
func (r *Runner) tick(ctx context.Context, t time.Time) {
	jobs, err := r.db.ListCronJobs(ctx)
	if err != nil {
		slog.Error("cron: cannot list jobs", "error", err)
		return
	}
	for _, job := range jobs {
		if !job.Enabled || job.NextRun == nil {
			continue
		}
		if job.NextRun.After(t) {
			continue
		}
		r.launch(ctx, job)
	}
}

// ErrCapacityBusy is returned when the concurrency limit is full.
var ErrCapacityBusy = errors.New("cron: scheduler at capacity")

// ErrAlreadyRunning is returned when the same job is already executing.
var ErrAlreadyRunning = errors.New("cron: job already running")

// ErrDisabled is returned to scheduled callers when the runner is disabled.
// RunNow does not observe this error; manual runs are allowed regardless.
var ErrDisabled = errors.New("cron: scheduler disabled")

// admit is the single serialized entry point onto r.running. It reads the
// cap and (for scheduled callers) the enabled flag under r.mu so a
// concurrent Reconfigure cannot race the admission decision.
func (r *Runner) admit(ctx context.Context, id string, scheduled bool) (context.Context, func(), error) {
	r.mu.Lock()
	r.cfg.RLock()
	enabled := r.enabled
	max := r.maxConcurrent
	r.cfg.RUnlock()

	if scheduled && !enabled {
		r.mu.Unlock()
		return nil, nil, ErrDisabled
	}
	if _, busy := r.running[id]; busy {
		r.mu.Unlock()
		return nil, nil, ErrAlreadyRunning
	}
	if len(r.running) >= max {
		r.mu.Unlock()
		return nil, nil, ErrCapacityBusy
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.running[id] = cancel
	r.mu.Unlock()

	release := func() {
		r.mu.Lock()
		delete(r.running, id)
		r.mu.Unlock()
		cancel()
	}
	return runCtx, release, nil
}

// launch runs one due job. next_run advances only on successful admission.
func (r *Runner) launch(ctx context.Context, job store.CronJob) {
	runCtx, release, err := r.admit(ctx, job.ID, true)
	if err != nil {
		switch {
		case errors.Is(err, ErrAlreadyRunning):
			slog.Warn("cron: previous run still active, skipping", "job", job.Name)
		case errors.Is(err, ErrCapacityBusy):
			slog.Warn("cron: at capacity, deferring", "job", job.Name)
		case errors.Is(err, ErrDisabled):
			// Disabled between tick read and admit: defer silently.
		}
		return
	}

	_, loc, _, _ := r.snapshot()
	r.advance(ctx, &job, time.Now().In(loc))

	go func() {
		defer release()
		r.execute(runCtx, job)
	}()
}

// RunNow executes a job immediately, outside its schedule. It honours the
// concurrency limit and duplicate guard; unlike scheduled activations,
// RunNow is allowed while the runner is disabled.
func (r *Runner) RunNow(ctx context.Context, id string) error {
	job, err := r.db.GetCronJob(ctx, id)
	if err != nil {
		return err
	}

	// A manual run must survive an HTTP request cancellation.
	runCtx, release, err := r.admit(context.WithoutCancel(ctx), id, false)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			return fmt.Errorf("job %q is already running", job.Name)
		}
		if errors.Is(err, ErrCapacityBusy) {
			return fmt.Errorf("scheduler is at capacity; try again shortly")
		}
		return err
	}
	go func() {
		defer release()
		r.execute(runCtx, *job)
	}()
	return nil
}

// execute performs the run and records its outcome.
func (r *Runner) execute(ctx context.Context, job store.CronJob) {
	run := &store.CronRun{
		ID: newID("run"), JobID: job.ID, Status: "running", StartedAt: time.Now(),
	}
	if err := r.db.PutCronRun(ctx, run); err != nil {
		slog.Warn("cron: cannot record run start", "error", err)
	}
	slog.Info("cron job started", "job", job.Name, "schedule", job.Schedule)

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	sessionID, reply, err := r.exec(runCtx, job)
	finished := time.Now()
	run.FinishedAt = &finished
	run.SessionID = sessionID
	run.Output = truncate(reply, 20000)

	state := "ok"
	if err != nil {
		state = "error"
		run.Status = "error"
		run.Error = err.Error()
		slog.Error("cron job failed", "job", job.Name, "error", err)
	} else {
		run.Status = "ok"
		slog.Info("cron job finished", "job", job.Name, "seconds", finished.Sub(run.StartedAt).Seconds())

		if target := strings.TrimSpace(job.Target); target != "" && r.deliver != nil && reply != "" {
			if derr := r.deliver(runCtx, target, reply); derr != nil {
				slog.Warn("cron: delivery failed", "job", job.Name, "target", target, "error", derr)
				run.Error = "delivery failed: " + derr.Error()
			}
		}
	}

	if err := r.db.PutCronRun(ctx, run); err != nil {
		slog.Warn("cron: cannot record run result", "error", err)
	}

	now := time.Now()
	job.LastRun = &now
	job.LastState = state
	if err := r.db.PutCronJob(ctx, &job); err != nil {
		slog.Warn("cron: cannot update job state", "error", err)
	}
}

// advance recomputes and persists a job's next activation.
func (r *Runner) advance(ctx context.Context, job *store.CronJob, from time.Time) {
	sched, err := r.scheduleFor(*job)
	if err != nil {
		slog.Warn("cron: invalid schedule, disabling job", "job", job.Name, "error", err)
		job.Enabled = false
		job.LastState = "invalid schedule"
		job.NextRun = nil
	} else {
		next := sched.Next(from)
		if next.IsZero() {
			job.NextRun = nil
		} else {
			job.NextRun = &next
		}
	}
	if err := r.db.PutCronJob(ctx, job); err != nil {
		slog.Warn("cron: cannot persist next run", "error", err)
	}
}

// Recompute refreshes one job's next activation, e.g. after an edit.
func (r *Runner) Recompute(ctx context.Context, job *store.CronJob) error {
	sched, err := r.scheduleFor(*job)
	if err != nil {
		return err
	}
	_, loc, _, _ := r.snapshot()
	next := sched.Next(time.Now().In(loc))
	if next.IsZero() {
		job.NextRun = nil
	} else {
		job.NextRun = &next
	}
	return r.db.PutCronJob(ctx, job)
}

func (r *Runner) recomputeAll(ctx context.Context) error {
	jobs, err := r.db.ListCronJobs(ctx)
	if err != nil {
		return err
	}
	for i := range jobs {
		if !jobs[i].Enabled {
			continue
		}
		if err := r.Recompute(ctx, &jobs[i]); err != nil {
			slog.Warn("cron: cannot compute next run", "job", jobs[i].Name, "error", err)
		}
	}
	return nil
}

// scheduleFor parses a job's expression in the job's own timezone when set.
func (r *Runner) scheduleFor(job store.CronJob) (*Schedule, error) {
	return Parse(job.Schedule)
}

// Location reports the scheduler timezone.
func (r *Runner) Location() *time.Location {
	_, loc, _, _ := r.snapshot()
	return loc
}

// Validate checks an expression and returns the next activation for previews.
func Validate(expr string, loc *time.Location) (time.Time, error) {
	s, err := Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = time.Local
	}
	return s.Next(time.Now().In(loc)), nil
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
