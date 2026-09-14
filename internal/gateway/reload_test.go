package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enowdev/antares/internal/config"
)

// fakeAdapter is a stand-in for a real platform connection. It records how
// many times its Start ran, how many are running concurrently, and blocks in
// Start until its context is cancelled — the same shape as a real long-poll
// or websocket adapter.
type fakeAdapter struct {
	name   string
	starts atomic.Int32
	stops  atomic.Int32
	active *atomic.Int32 // shared across a test's fakes to observe concurrency
	peak   *atomic.Int32
	// entered closes the first time Start is invoked. Tests wait on it
	// before doing lifecycle ops so they never race with the goroutine
	// scheduler — if the manager cancels the adapter context before the
	// goroutine gets its first slice, Start would never run and any
	// assertion against `starts`/`stops` would deadlock waiting for zero.
	entered chan struct{}
	// enterOnce guards `entered` so a restart-on-failure loop that re-enters
	// Start doesn't close the channel twice.
	enterOnce sync.Once
	// stopDelay lets a test model an adapter that ignores its context for a
	// little while, so we can prove the reload path bounds the wait and
	// refuses to install a replacement while the old goroutine still holds
	// its token.
	stopDelay time.Duration
	// startDelay optionally pauses inside Start before the adapter is
	// considered "active", so a test can arrange for two Start calls to race.
	startDelay time.Duration
}

func (f *fakeAdapter) Name() string { return f.name }
func (f *fakeAdapter) Start(ctx context.Context) error {
	f.starts.Add(1)
	f.enterOnce.Do(func() { close(f.entered) })
	if f.startDelay > 0 {
		time.Sleep(f.startDelay)
	}
	if f.active != nil {
		now := f.active.Add(1)
		if f.peak != nil {
			for {
				old := f.peak.Load()
				if now <= old || f.peak.CompareAndSwap(old, now) {
					break
				}
			}
		}
		defer f.active.Add(-1)
	}
	<-ctx.Done()
	if f.stopDelay > 0 {
		time.Sleep(f.stopDelay)
	}
	f.stops.Add(1)
	return ctx.Err()
}
func (f *fakeAdapter) Send(context.Context, Reply) (string, error) {
	return "", errors.New("no send in tests")
}
func (f *fakeAdapter) Connected() bool { return true }

// fakeRegistry hands out named fake adapters and remembers each one it built,
// keyed by platform. A test uses it as the manager's adapterFactory so
// buildAdapter never touches the real Telegram/Discord constructors.
type fakeRegistry struct {
	mu    sync.Mutex
	built map[string][]*fakeAdapter
	live  map[string]bool
	// active/peak are shared with every fake so a test can observe how many
	// adapters were simultaneously in their Start body.
	active atomic.Int32
	peak   atomic.Int32
	// stopDelay, if set, is applied to every fake handed out.
	stopDelay time.Duration
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{built: map[string][]*fakeAdapter{}, live: map[string]bool{}}
}

func (r *fakeRegistry) enable(platforms ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range platforms {
		r.live[p] = true
	}
}

func (r *fakeRegistry) disable(platform string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, platform)
}

func (r *fakeRegistry) factory(platform string, _ *config.Config) Adapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.live[platform] {
		return nil
	}
	a := &fakeAdapter{
		name:      platform,
		active:    &r.active,
		peak:      &r.peak,
		stopDelay: r.stopDelay,
		entered:   make(chan struct{}),
	}
	r.built[platform] = append(r.built[platform], a)
	return a
}

func (r *fakeRegistry) latest(platform string) *fakeAdapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.built[platform]
	if len(list) == 0 {
		return nil
	}
	return list[len(list)-1]
}

func (r *fakeRegistry) countBuilt(platform string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.built[platform])
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

// awaitEntered blocks until every fake's Start body has begun. Without this
// a lifecycle op can cancel the adapter context before its goroutine is
// scheduled, which is valid production behaviour but leaves fake counters
// (starts/stops/peak) at zero and turns test assertions into deadlocks. The
// entered channel is a real synchronization signal, not a fixed sleep.
func awaitEntered(t *testing.T, fakes ...*fakeAdapter) {
	t.Helper()
	for _, f := range fakes {
		select {
		case <-f.entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("adapter %q never entered Start", f.name)
		}
	}
}

// baseCfg is a config with the gateway master switch on and every per-platform
// switch flipped so the production buildAdapter would accept them. The
// factory hook decides what actually gets constructed.
func baseCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.Enabled = true
	cfg.Gateway.Telegram = config.Telegram{Enabled: true, BotToken: "tg-token"}
	cfg.Gateway.Discord = config.Discord{Enabled: true, BotToken: "dc-token"}
	return cfg
}

func TestReconcileWithoutStartJustPublishes(t *testing.T) {
	m := NewManager(&config.Config{}, nil, nil)
	next := baseCfg()
	// Before Start the manager has no base context. A reload must not fail
	// just because the gateway has not booted yet — the dashboard would
	// otherwise report a spurious save error to the user.
	if err := m.Reconcile(next); err != nil {
		t.Fatalf("reconcile before start: %v", err)
	}
	if m.config() != next {
		t.Fatal("reconcile did not publish the new config")
	}
}

func TestReconcileNilConfigIsAnError(t *testing.T) {
	m := NewManager(&config.Config{}, nil, nil)
	if err := m.Reconcile(nil); err == nil {
		t.Fatal("reconcile with nil config should return an error")
	}
}

func TestReconcileLeavesUnchangedAdaptersAlone(t *testing.T) {
	cfg := baseCfg()
	reg := newFakeRegistry()
	reg.enable("telegram", "discord")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool {
		return reg.countBuilt("telegram") == 1 && reg.countBuilt("discord") == 1
	}, "adapters to start")

	tg := reg.latest("telegram")
	dc := reg.latest("discord")
	awaitEntered(t, tg, dc)

	// Reload with the same per-platform settings but a new binding. Bindings
	// are read by the inbound handler on each message, not closed over by an
	// adapter — so neither adapter should be restarted.
	next := *cfg
	next.Gateway.Bindings = []config.Binding{{ID: "b1", Platform: "telegram", Enabled: true}}
	if err := m.Reconcile(&next); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if reg.countBuilt("telegram") != 1 || reg.countBuilt("discord") != 1 {
		t.Fatalf("adapters were rebuilt for a bindings-only change: telegram=%d discord=%d",
			reg.countBuilt("telegram"), reg.countBuilt("discord"))
	}
	if tg.stops.Load() != 0 || dc.stops.Load() != 0 {
		t.Fatalf("adapters were stopped for a bindings-only change: tg=%d dc=%d",
			tg.stops.Load(), dc.stops.Load())
	}
	if m.config() != &next {
		t.Fatal("reconcile did not publish the new config")
	}

	m.StopAll()
}

func TestReconcileRestartsOnlyChangedAdapters(t *testing.T) {
	cfg := baseCfg()
	reg := newFakeRegistry()
	reg.enable("telegram", "discord")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool {
		return reg.countBuilt("telegram") == 1 && reg.countBuilt("discord") == 1
	}, "adapters to start")

	originalTg := reg.latest("telegram")
	originalDc := reg.latest("discord")
	// The manager's cancel path is legitimately fast enough that it can
	// cancel an adapter's context before the goroutine reaches Start. That
	// would leave `stops` at zero forever and the assertion below would
	// deadlock waiting on a non-event. Wait for the real signal — the
	// adapter has entered its Start body — before triggering the reload.
	awaitEntered(t, originalTg, originalDc)

	// Rotate only the telegram bot token: discord must not be disturbed.
	next := *cfg
	next.Gateway.Telegram = config.Telegram{Enabled: true, BotToken: "tg-token-2"}
	if err := m.Reconcile(&next); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	waitFor(t, func() bool { return originalTg.stops.Load() >= 1 }, "old telegram to stop")

	if reg.countBuilt("telegram") != 2 {
		t.Fatalf("changed telegram was not rebuilt: built=%d", reg.countBuilt("telegram"))
	}
	if reg.countBuilt("discord") != 1 {
		t.Fatalf("unchanged discord was rebuilt: built=%d", reg.countBuilt("discord"))
	}
	if originalDc.stops.Load() != 0 {
		t.Fatalf("unchanged discord was cancelled: stops=%d", originalDc.stops.Load())
	}

	m.StopAll()
}

func TestReconcileDisablingPerPlatformStopsIt(t *testing.T) {
	cfg := baseCfg()
	reg := newFakeRegistry()
	reg.enable("telegram", "discord")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool {
		return reg.countBuilt("telegram") == 1 && reg.countBuilt("discord") == 1
	}, "adapters to start")

	awaitEntered(t, reg.latest("telegram"), reg.latest("discord"))
	tg := reg.latest("telegram")

	// Flip only telegram off.
	next := *cfg
	next.Gateway.Telegram = config.Telegram{Enabled: false, BotToken: "tg-token"}
	reg.disable("telegram")
	if err := m.Reconcile(&next); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	waitFor(t, func() bool { return tg.stops.Load() >= 1 }, "telegram to stop")

	m.mu.RLock()
	_, telegramStillMapped := m.adapters["telegram"]
	_, discordStillMapped := m.adapters["discord"]
	m.mu.RUnlock()
	if telegramStillMapped {
		t.Fatal("disabled telegram should have been removed")
	}
	if !discordStillMapped {
		t.Fatal("untouched discord should still be tracked")
	}

	m.StopAll()
}

func TestReconcileMasterSwitchOffStopsEverything(t *testing.T) {
	cfg := baseCfg()
	reg := newFakeRegistry()
	reg.enable("telegram", "discord")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool {
		return reg.countBuilt("telegram") == 1 && reg.countBuilt("discord") == 1
	}, "adapters to start")

	tg := reg.latest("telegram")
	dc := reg.latest("discord")
	awaitEntered(t, tg, dc)

	next := *cfg
	next.Gateway.Enabled = false
	if err := m.Reconcile(&next); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	waitFor(t, func() bool { return tg.stops.Load() >= 1 && dc.stops.Load() >= 1 }, "adapters to stop")
	if len(m.Status()) != 0 {
		t.Fatalf("expected no adapters after master-off, got %v", m.Status())
	}
}

func TestReconcileMasterSwitchOnBringsAdaptersUp(t *testing.T) {
	cfg := &config.Config{} // gateway off, everything blank
	reg := newFakeRegistry()
	reg.enable("telegram")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())

	next := baseCfg()
	// The registry only allows telegram, matching a real config where only
	// telegram has credentials.
	next.Gateway.Discord = config.Discord{}
	if err := m.Reconcile(next); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	waitFor(t, func() bool { return reg.countBuilt("telegram") == 1 }, "telegram to boot")
	m.StopAll()
}

func TestStopWaitsForAdapterGoroutine(t *testing.T) {
	cfg := baseCfg()
	reg := newFakeRegistry()
	reg.enable("telegram")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool { return reg.countBuilt("telegram") == 1 }, "telegram to start")

	a := reg.latest("telegram")
	waitFor(t, func() bool { return a.starts.Load() >= 1 }, "adapter goroutine to enter Start")

	m.Stop("telegram")

	// The stop path waits for the adapter goroutine to exit so a replacement
	// is not launched while the old connection still holds the token — the
	// counter must therefore have advanced by the time Stop returns.
	if a.stops.Load() != 1 {
		t.Fatalf("Stop returned before the adapter goroutine exited: stops=%d", a.stops.Load())
	}
}

func TestConcurrentSyncAndReconcileDoNotOverlap(t *testing.T) {
	cfg := baseCfg()
	// Only telegram gets built; discord is left off so the sync/reconcile
	// operations really do target the same slot.
	cfg.Gateway.Discord = config.Discord{}
	reg := newFakeRegistry()
	reg.enable("telegram")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool { return reg.countBuilt("telegram") == 1 }, "telegram to start")
	awaitEntered(t, reg.latest("telegram"))

	next := *cfg
	next.Gateway.Telegram = config.Telegram{Enabled: true, BotToken: "tg-token-2"}

	// Fire Sync and Reconcile back-to-back from two goroutines. Serialization
	// through opMu means one runs to completion before the other starts, so
	// the second one never installs a replacement on top of a still-live
	// adapter. Without opMu two concurrent teardowns could each launch a
	// replacement into a half-cleared map, doubling live connections.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = m.Sync("telegram") }()
	go func() { defer wg.Done(); _ = m.Reconcile(&next) }()
	wg.Wait()

	// One adapter is live in the slot, and the total number of builds is
	// bounded by "one initial + one per lifecycle op" = 3.
	m.mu.RLock()
	live := len(m.adapters)
	m.mu.RUnlock()
	if live != 1 {
		t.Fatalf("expected exactly one live adapter, got %d", live)
	}
	if got := reg.countBuilt("telegram"); got > 3 {
		t.Fatalf("adapter rebuilt too many times (%d); concurrent lifecycle ops overlapped", got)
	}

	// Observable: the registry's shared active counter never exceeded 1 —
	// at no point in the reload did two goroutines both hold the same
	// telegram token.
	if peak := reg.peak.Load(); peak > 1 {
		t.Fatalf("two adapters were simultaneously active (peak=%d); reload broke exclusivity", peak)
	}

	m.StopAll()
}

func TestReconcileNeverAllowsTwoActiveAdaptersOnTheSameSlot(t *testing.T) {
	// A dedicated observable test: hammer Reconcile with alternating tokens
	// while a real fake adapter is in its Start body. The peak simultaneous
	// count must stay at one — otherwise we would have two clients on the
	// same slot holding two live connections against the same bot token.
	cfg := baseCfg()
	cfg.Gateway.Discord = config.Discord{}
	reg := newFakeRegistry()
	reg.enable("telegram")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool { return reg.peak.Load() >= 1 }, "first adapter to enter Start")

	for i := range 20 {
		next := *cfg
		next.Gateway.Telegram = config.Telegram{Enabled: true, BotToken: "rot-" + strings.Repeat("x", i+1)}
		if err := m.Reconcile(&next); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	if peak := reg.peak.Load(); peak != 1 {
		t.Fatalf("expected exactly one active adapter throughout, saw peak=%d", peak)
	}
	m.StopAll()
	if leftover := reg.active.Load(); leftover != 0 {
		t.Fatalf("adapters still active after StopAll: %d", leftover)
	}
}

func TestStopAllReleasesEveryAdapter(t *testing.T) {
	cfg := baseCfg()
	reg := newFakeRegistry()
	reg.enable("telegram", "discord")

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool {
		return reg.countBuilt("telegram") == 1 && reg.countBuilt("discord") == 1
	}, "adapters to start")

	tg := reg.latest("telegram")
	dc := reg.latest("discord")
	awaitEntered(t, tg, dc)

	m.StopAll()

	if tg.stops.Load() != 1 || dc.stops.Load() != 1 {
		t.Fatalf("StopAll left adapters running: tg=%d dc=%d", tg.stops.Load(), dc.stops.Load())
	}
	if len(m.Status()) != 0 {
		t.Fatalf("StopAll did not clear the adapter map: %v", m.Status())
	}
}

func TestStopBoundedWaitOnStubbornAdapter(t *testing.T) {
	cfg := baseCfg()
	m := NewManager(cfg, nil, nil)
	// An adapter that lingers past the bounded wait models a stuck third-
	// party client. Stop must return within the bound rather than wedging
	// config reload behind a broken connection.
	stubborn := &fakeAdapter{name: "telegram", stopDelay: 3 * time.Second, entered: make(chan struct{})}
	m.adapterFactory = func(platform string, _ *config.Config) Adapter {
		if platform == "telegram" {
			return stubborn
		}
		return nil
	}
	m.Start(context.Background())
	waitFor(t, func() bool { return stubborn.starts.Load() >= 1 }, "stubborn adapter to start")

	start := time.Now()
	m.Stop("telegram")
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("Stop waited past its bound: %s", elapsed)
	}
	// Wait for the goroutine to actually finish so the test doesn't leak it
	// into the next case; the manager has already returned from Stop with
	// the stubborn adapter still tracked, which is exactly the contract we
	// exercised in TestStubbornAdapterKeepsTracking.
	waitFor(t, func() bool { return stubborn.stops.Load() >= 1 }, "stubborn goroutine to finally exit")
}

func TestStubbornAdapterKeepsTrackingForRetry(t *testing.T) {
	// A reconcile against a stubborn adapter must NOT install a replacement
	// while the old goroutine is still alive; it must also keep the platform
	// dirty so a follow-up reconcile retries. Two live connections against
	// the same token would burn credentials and duplicate deliveries.
	cfg := baseCfg()
	cfg.Gateway.Discord = config.Discord{}
	reg := newFakeRegistry()
	reg.enable("telegram")
	// Every fake this registry hands out ignores its context for 3s — longer
	// than the bounded stop wait.
	reg.stopDelay = 3 * time.Second

	m := NewManager(cfg, nil, nil)
	m.adapterFactory = reg.factory
	m.Start(context.Background())
	waitFor(t, func() bool { return reg.countBuilt("telegram") == 1 }, "telegram to start")
	awaitEntered(t, reg.latest("telegram"))

	next := *cfg
	next.Gateway.Telegram = config.Telegram{Enabled: true, BotToken: "tg-token-2"}
	err := m.Reconcile(&next)
	if err == nil {
		t.Fatal("expected reconcile to report the stubborn adapter")
	}
	if !strings.Contains(err.Error(), "telegram") {
		t.Fatalf("error should name the platform, got: %v", err)
	}
	// The registry must NOT have handed out a replacement: the old adapter
	// still holds the token, so the manager owes us a retry, not a second
	// live connection.
	if got := reg.countBuilt("telegram"); got != 1 {
		t.Fatalf("stubborn stop should have blocked replacement, but registry built %d adapters", got)
	}
	// Observable exclusivity: only one adapter was ever active at once.
	if peak := reg.peak.Load(); peak > 1 {
		t.Fatalf("two adapters simultaneously active (peak=%d) during stubborn reload", peak)
	}

	// Drain the stubborn goroutine before retrying so the second reconcile
	// completes cleanly.
	orig := reg.latest("telegram")
	waitFor(t, func() bool { return orig.stops.Load() >= 1 }, "stubborn goroutine to exit")
	// Loosen the stop delay so the retry can succeed.
	reg.mu.Lock()
	reg.stopDelay = 0
	reg.mu.Unlock()

	if err := m.Reconcile(&next); err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	if got := reg.countBuilt("telegram"); got != 2 {
		t.Fatalf("retry did not restart telegram: built=%d", got)
	}
	m.StopAll()
}
