// Package gateway connects Antares to messaging platforms. Every platform
// shares the same session store, memory, and tool surface.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/store"
)

// InboundMessage is one message received from a platform.
type InboundMessage struct {
	Platform  string
	ChannelID string
	// GuildID is the server a channel belongs to (Discord). Empty for platforms
	// without a server concept (Telegram) and for direct messages.
	GuildID  string
	UserID   string
	UserName string
	// DisplayName is the friendliest name for the sender — a Discord server
	// nickname, else the account display name, else the username. Empty falls
	// back to UserName. Shown to the agent so it can address the person.
	DisplayName string
	// Roles are the sender's server-role ids (Discord guild members). Used to
	// gate a server binding by role. Empty in DMs and on platforms without roles.
	Roles []string
	Text  string
	// IsDirect marks a 1:1 chat, where the agent replies without being addressed.
	IsDirect bool
	// MessageID lets adapters edit their own reply while streaming.
	MessageID string
}

// Reply is what an adapter sends back.
type Reply struct {
	ChannelID string
	Text      string
	// ReplyTo threads the response onto the triggering message when supported.
	ReplyTo string
	// EditID updates a previously sent message instead of posting a new one.
	EditID string
}

// Handler processes an inbound message and streams partial replies.
// The returned string is the final text.
type Handler func(ctx context.Context, msg InboundMessage, partial func(string)) (string, error)

// Adapter is one platform connection.
type Adapter interface {
	Name() string
	// Start blocks until ctx is cancelled or the connection fails fatally.
	Start(ctx context.Context) error
	// Send delivers a message; it returns the platform message id.
	Send(ctx context.Context, r Reply) (string, error)
	// Connected reports the live connection state for the dashboard.
	Connected() bool
}

// Manager supervises the enabled adapters.
type Manager struct {
	cfg     *config.Config
	db      store.Store
	handler Handler

	// opMu serializes lifecycle operations (Start, Sync, Reconcile, Stop*)
	// so a Reconcile racing with a manual Sync cannot end up with two live
	// goroutines connected to the same platform.
	opMu sync.Mutex

	mu       sync.RWMutex
	adapters map[string]Adapter
	cancels  map[string]context.CancelFunc
	// dones mirrors cancels: each adapter goroutine closes its channel on
	// exit, so a Stop caller can wait for the previous connection to release
	// its token before we launch a replacement with the same one.
	dones map[string]chan struct{}
	// applied records the per-platform settings that are currently live —
	// what the running adapter closed over (or the zero value when none is
	// running). Reconcile diffs against this, not the previous cfg pointer,
	// so a retry after a stubborn stop still sees the platform as dirty
	// instead of silently agreeing with the freshly-published config.
	applied map[string]any
	// appliedEnabled mirrors cfg.Gateway.Enabled at the moment applied
	// was last set, so a Reconcile that only flips the master switch still
	// detects drift even when the per-platform structs are unchanged.
	appliedEnabled bool
	// baseCtx is the process lifetime, kept so Sync can start an adapter that
	// was disabled at boot without waiting for a restart.
	baseCtx context.Context

	// adapterFactory is a test seam so reload_test.go can install fake
	// adapters without making real network calls. Nil in production; set by
	// tests directly on the struct. Kept small and honest — a real
	// abstraction would obscure the one-source-of-truth guarantee in
	// buildAdapter.
	adapterFactory func(platform string, cfg *config.Config) Adapter
}

// NewManager builds a gateway manager.
func NewManager(cfg *config.Config, db store.Store, handler Handler) *Manager {
	return &Manager{
		cfg: cfg, db: db, handler: handler,
		adapters: map[string]Adapter{},
		cancels:  map[string]context.CancelFunc{},
		dones:    map[string]chan struct{}{},
		applied:  map[string]any{},
	}
}

// SetConfig swaps in a reloaded configuration. Reload replaces the whole
// config pointer, so without this the manager would keep reading the values it
// was built with and Sync would reconcile against a stale file. It is kept as
// a plain setter for callers that only need the pointer refreshed; Reconcile
// is the entry point that also brings running adapters into line.
func (m *Manager) SetConfig(cfg *config.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// config returns the configuration currently in force.
func (m *Manager) config() *config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Start launches every enabled platform and keeps it running until ctx ends.
func (m *Manager) Start(ctx context.Context) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	m.baseCtx = ctx
	m.mu.Unlock()

	cfg := m.config()
	// Record what Start applied so a follow-up Reconcile can diff correctly.
	// This runs even when the gateway is disabled, so a later Reconcile that
	// enables it sees drift and boots the adapters.
	m.mu.Lock()
	m.appliedEnabled = cfg.Gateway.Enabled
	for _, p := range platformOrder {
		m.applied[p] = platformSettings(p, cfg)
	}
	m.mu.Unlock()
	if !cfg.Gateway.Enabled {
		slog.Debug("gateway disabled")
		return
	}
	for _, p := range platformOrder {
		if a := m.buildAdapter(p, cfg); a != nil {
			m.startAdapter(ctx, a)
		}
	}
}

// startAdapter runs one adapter with restart-on-failure backoff.
func (m *Manager) startAdapter(ctx context.Context, a Adapter) {
	adapterCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.mu.Lock()
	m.adapters[a.Name()] = a
	m.cancels[a.Name()] = cancel
	m.dones[a.Name()] = done
	m.mu.Unlock()

	go func() {
		defer close(done)
		backoff := time.Second
		for {
			if adapterCtx.Err() != nil {
				return
			}
			slog.Info("gateway connecting", "platform", a.Name())
			err := a.Start(adapterCtx)
			if adapterCtx.Err() != nil {
				return
			}
			if err != nil {
				slog.Error("gateway disconnected", "platform", a.Name(), "error", err, "retry_in", backoff)
			}
			select {
			case <-adapterCtx.Done():
				return
			case <-time.After(backoff):
			}
			// Exponential backoff, capped so a long outage still recovers promptly.
			if backoff < 2*time.Minute {
				backoff *= 2
			}
		}
	}()
}

// platformOrder is the canonical set of adapters. Iteration order matches the
// historical Start layout so log lines stay stable.
var platformOrder = []string{"telegram", "discord", "slack", "matrix", "signal", "whatsapp", "feishu"}

// buildAdapter returns a fresh adapter for the platform when the config says
// it should be live, or nil when it is disabled or missing credentials. It is
// the single source of truth for what "enabled and configured" means, so
// Start, Sync, and Reconcile agree.
func (m *Manager) buildAdapter(platform string, cfg *config.Config) Adapter {
	if m.adapterFactory != nil {
		return m.adapterFactory(platform, cfg)
	}
	switch platform {
	case "telegram":
		if tg := cfg.Gateway.Telegram; tg.Enabled && tg.BotToken != "" {
			return NewTelegram(tg, m)
		}
	case "discord":
		if dc := cfg.Gateway.Discord; dc.Enabled && dc.BotToken != "" {
			return NewDiscord(dc, m)
		}
	case "slack":
		if sl := cfg.Gateway.Slack; sl.Enabled && sl.AppToken != "" && sl.BotToken != "" {
			return NewSlack(sl, m)
		}
	case "matrix":
		if mx := cfg.Gateway.Matrix; mx.Enabled && mx.Homeserver != "" && mx.AccessToken != "" {
			return NewMatrix(mx, m)
		}
	case "signal":
		if sg := cfg.Gateway.Signal; sg.Enabled && sg.APIURL != "" && sg.Number != "" {
			return NewSignal(sg, m)
		}
	case "whatsapp":
		if wa := cfg.Gateway.WhatsApp; wa.Enabled && wa.Token != "" && wa.PhoneNumberID != "" {
			return NewWhatsApp(wa, m)
		}
	case "feishu":
		if fs := cfg.Gateway.Feishu; fs.Enabled && fs.AppID != "" && fs.AppSecret != "" {
			return NewFeishu(fs, m)
		}
	}
	return nil
}

// platformSettings extracts the per-platform config a running adapter closes
// over, so Reconcile can tell whether a live adapter is still current.
func platformSettings(platform string, cfg *config.Config) any {
	switch platform {
	case "telegram":
		return cfg.Gateway.Telegram
	case "discord":
		return cfg.Gateway.Discord
	case "slack":
		return cfg.Gateway.Slack
	case "matrix":
		return cfg.Gateway.Matrix
	case "signal":
		return cfg.Gateway.Signal
	case "whatsapp":
		return cfg.Gateway.WhatsApp
	case "feishu":
		return cfg.Gateway.Feishu
	}
	return nil
}

// Sync brings one platform in line with the current configuration: it stops a
// running adapter and starts a fresh one when the platform should be live.
// Without this, saving a token or flipping a switch in the dashboard only took
// effect after restarting the process, which is a poor thing to ask of someone
// who just pasted a token. Sync serializes with Reconcile and StopAll through
// opMu so two lifecycle ops cannot both try to install a replacement into the
// same slot.
func (m *Manager) Sync(platform string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	base := m.baseCtx
	m.mu.RUnlock()
	if base == nil {
		return errors.New("the gateway has not started yet")
	}

	if !knownPlatform(platform) {
		return fmt.Errorf("unknown platform %q", platform)
	}

	// Always tear the old connection down first: an adapter holds its token
	// and enable flag from when it was constructed, so the only way to pick
	// up a change is to build a new one. If the old adapter refuses to exit
	// within the bounded wait, do NOT launch a replacement — that would put
	// two live connections on the same token. Return the error so the caller
	// (dashboard or Reconcile) can retry, and leave the tracked cancel/done
	// in place so the next attempt reconciles the same slot.
	if err := m.stopAndWait(platform); err != nil {
		return err
	}

	cfg := m.config()
	if !cfg.Gateway.Enabled {
		m.markApplied(platform, cfg)
		return nil
	}
	if a := m.buildAdapter(platform, cfg); a != nil {
		m.startAdapter(base, a)
	}
	m.markApplied(platform, cfg)
	return nil
}

// Reconcile publishes the new configuration and restarts only the adapters
// whose per-platform settings (or the master Gateway.Enabled switch) actually
// changed since the last successful apply. Untouched settings like bindings
// just take effect on the next message because the inbound handler re-reads
// the live config.
//
// The diff is against applied-state, not the previously published config, so
// a retry after a stubborn stop still sees the platform as dirty rather than
// silently agreeing with the freshly-published pointer. If some platforms
// fail to reconcile within the bounded stop wait, Reconcile returns their
// combined error and leaves them tracked for the next attempt; the platforms
// that did reconcile stay reconciled.
//
// A nil baseCtx means Start has not been called yet: publish the config and
// let a later Start pick it up. Returning an error here would leave the
// dashboard reporting a failed save for a manager that simply is not up yet.
func (m *Manager) Reconcile(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("reconcile requires a config")
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()

	// Publish the new config first so the handler and Deliver see the fresh
	// bindings and routing immediately; adapter lifecycle is a separate axis
	// tracked by m.applied.
	m.mu.Lock()
	base := m.baseCtx
	m.cfg = cfg
	m.mu.Unlock()

	if base == nil {
		return nil
	}

	if !cfg.Gateway.Enabled {
		// Master switch off: stop every adapter. stopAllForReload only marks
		// applied for the platforms that actually released; a stuck adapter
		// keeps its old applied value so the next Reconcile still sees drift.
		err := m.stopAllForReload(cfg)
		if err == nil {
			m.mu.Lock()
			m.appliedEnabled = false
			m.mu.Unlock()
		}
		return err
	}

	m.mu.RLock()
	masterChanged := !m.appliedEnabled
	m.mu.RUnlock()

	var errs []error
	for _, p := range platformOrder {
		desired := platformSettings(p, cfg)
		m.mu.RLock()
		current := m.applied[p]
		m.mu.RUnlock()
		// The per-platform structs contain slices (allowed users/chats/roles),
		// so a direct == would panic on comparable-check. reflect.DeepEqual is
		// the honest way to ask "did the caller change anything the adapter
		// closed over?". masterChanged forces a walk of every platform when
		// the gateway switch flipped from off to on, even if the per-platform
		// structs are byte-identical, because no adapter is currently live.
		if !masterChanged && reflect.DeepEqual(current, desired) {
			continue
		}
		if err := m.stopAndWait(p); err != nil {
			errs = append(errs, fmt.Errorf("reconcile %s: %w", p, err))
			continue
		}
		if a := m.buildAdapter(p, cfg); a != nil {
			m.startAdapter(base, a)
		}
		m.markApplied(p, cfg)
	}
	if len(errs) > 0 {
		// Master stays "not fully applied" so a retry still walks every
		// platform that missed. Individual platforms that succeeded are
		// already committed in m.applied.
		return errors.Join(errs...)
	}
	m.mu.Lock()
	m.appliedEnabled = true
	m.mu.Unlock()
	return nil
}

// markApplied records that the manager's live state for `platform` matches
// what `cfg` asked for. Called after a successful stop-and-replace (or a
// successful stop-only when the platform should be off).
func (m *Manager) markApplied(platform string, cfg *config.Config) {
	m.mu.Lock()
	m.applied[platform] = platformSettings(platform, cfg)
	m.mu.Unlock()
}

// knownPlatform reports whether the name is one of the adapters this manager
// knows how to build; used to keep Sync's "unknown platform" error path even
// when the platform is simply disabled in config.
func knownPlatform(name string) bool {
	for _, p := range platformOrder {
		if p == name {
			return true
		}
	}
	return false
}

// Stop shuts down one platform. Held under opMu so a manual Stop cannot race
// a concurrent Reconcile or Sync onto the same slot. After a successful
// Stop the platform is treated as unapplied, so the next Reconcile against
// a config that wants it live will restart it — a manual Stop is a
// temporary override, not a permanent disable.
func (m *Manager) Stop(name string) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.stopAndWait(name); err != nil {
		// A stubborn adapter that missed the bounded wait keeps its tracking
		// so a follow-up reconcile can retry. There is nothing more Stop can
		// do here; the warning was already logged by stopAndWait.
		return
	}
	m.mu.Lock()
	delete(m.applied, name)
	m.mu.Unlock()
}

// stopAndWait cancels the adapter's context and waits (bounded) for its
// goroutine to exit, so a caller replacing the adapter can rely on the
// previous connection having released its token before the new one dials in.
// It returns an error and preserves the tracked cancel/done entries when the
// wait times out, so the caller does NOT install a replacement on top of a
// still-live connection. A subsequent stopAndWait call reuses the same
// tracking to keep waiting.
func (m *Manager) stopAndWait(name string) error {
	m.mu.Lock()
	cancel, hasCancel := m.cancels[name]
	done, hasDone := m.dones[name]
	m.mu.Unlock()
	if !hasCancel && !hasDone {
		return nil
	}
	if hasCancel {
		// Idempotent: calling cancel a second time is a no-op, so retrying
		// stopAndWait after a timeout is safe.
		cancel()
	}
	if hasDone {
		// Bound the wait: an adapter that ignores its context should not
		// wedge a config reload. Two seconds is generous for a graceful
		// shutdown and short enough that a stuck adapter surfaces as an
		// error the caller can retry.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			slog.Warn("gateway adapter did not stop in time", "platform", name)
			return fmt.Errorf("adapter %q did not stop within 2s", name)
		}
	}
	// Clean shutdown: release the tracking so buildAdapter can install a
	// fresh one into the same slot.
	m.mu.Lock()
	delete(m.cancels, name)
	delete(m.dones, name)
	delete(m.adapters, name)
	m.mu.Unlock()
	return nil
}

// StopAll shuts down every platform. Fully resets the applied bookkeeping,
// so a subsequent Start or Reconcile treats every platform as freshly
// unconfigured.
func (m *Manager) StopAll() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	_ = m.stopAllForReload(nil)
	m.mu.Lock()
	m.applied = map[string]any{}
	m.appliedEnabled = false
	m.mu.Unlock()
}

// stopAllForReload cancels every running adapter and waits for their
// goroutines. When cfg is non-nil it also refreshes m.applied for each
// platform that released, so a subsequent Reconcile against the same cfg
// does not retry the ones that already stopped cleanly. Callers must hold
// opMu. Returns the joined error from any platform that missed its bound.
func (m *Manager) stopAllForReload(cfg *config.Config) error {
	m.mu.RLock()
	names := make([]string, 0, len(m.cancels))
	for name := range m.cancels {
		names = append(names, name)
	}
	m.mu.RUnlock()

	var errs []error
	for _, name := range names {
		if err := m.stopAndWait(name); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", name, err))
			continue
		}
		if cfg != nil {
			m.markApplied(name, cfg)
		} else {
			m.mu.Lock()
			delete(m.applied, name)
			m.mu.Unlock()
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Status reports each platform's connection state.
func (m *Manager) Status() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.adapters))
	for name, a := range m.adapters {
		out[name] = a.Connected()
	}
	return out
}

// Deliver sends text to "platform:channel", used by the cron scheduler.
func (m *Manager) Deliver(ctx context.Context, target, text string) error {
	platform, channel, ok := strings.Cut(target, ":")
	if !ok || channel == "" {
		return fmt.Errorf("delivery target must look like platform:channel, got %q", target)
	}
	m.mu.RLock()
	a, exists := m.adapters[platform]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("platform %q is not connected", platform)
	}
	_, err := a.Send(ctx, Reply{ChannelID: channel, Text: text})
	return err
}

// authorize applies the allow lists and the pairing flow.
// It returns the reply to send when access is refused.
func (m *Manager) authorize(ctx context.Context, msg InboundMessage, allowedUsers, allowedChats []string, requirePairing bool) (bool, string) {
	if len(allowedUsers) > 0 && contains(allowedUsers, msg.UserID) {
		return true, ""
	}
	if len(allowedChats) > 0 && contains(allowedChats, msg.ChannelID) {
		return true, ""
	}
	// An explicit allow list with no match is a hard denial.
	if len(allowedUsers) > 0 || len(allowedChats) > 0 {
		return false, ""
	}
	if !requirePairing {
		return true, ""
	}

	pairing, err := m.db.GetPairing(ctx, msg.Platform, msg.UserID)
	if err == nil && pairing.Status == "approved" {
		return true, ""
	}

	// First contact: record a pending pairing so the dashboard can approve it.
	if err != nil {
		code := pairingCode()
		p := &store.Pairing{
			ID: newID("pair"), Platform: msg.Platform, ExternalID: msg.UserID,
			DisplayName: msg.UserName, Status: "pending", Code: code,
		}
		if err := m.db.PutPairing(ctx, p); err != nil {
			slog.Warn("gateway: cannot record pairing", "error", err)
		}
		slog.Info("gateway pairing requested", "platform", msg.Platform, "user", msg.UserName, "code", code)
		return false, fmt.Sprintf(
			"This chat is not linked yet. Approve it in the Antares dashboard under Channels.\n\nPairing code: %s", code)
	}
	if pairing.Status == "revoked" {
		return false, "Access to this Antares instance has been revoked."
	}
	return false, fmt.Sprintf(
		"Waiting for approval in the Antares dashboard.\n\nPairing code: %s", pairing.Code)
}

// handle runs the agent for an inbound message and returns the reply text.
func (m *Manager) handle(ctx context.Context, msg InboundMessage, partial func(string)) (string, error) {
	if m.handler == nil {
		return "", fmt.Errorf("no handler configured")
	}
	return m.handler(ctx, msg, partial)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func pairingCode() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}
