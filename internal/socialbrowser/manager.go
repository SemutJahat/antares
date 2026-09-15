// Package socialbrowser manages the persistent stealth Chromium used by the
// Social Media agent. It wraps cloak-go to provide a singleton browser with
// a stable fingerprint seed and user-data-dir so cookies, localStorage, and
// login sessions survive across restarts.
package socialbrowser

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/enowdev/antares/internal/config"
)

// State describes the browser's current runtime status.
type State string

const (
	StateDisabled    State = "disabled"
	StateUnavailable State = "unavailable"
	StateStopped     State = "stopped"
	StateStarting    State = "starting"
	StateRunning     State = "running"
	StateError       State = "error"
)

// Manager owns one persistent social media browser. Only one controller may
// drive the browser at a time; a mutex serializes start/stop/control calls.
type Manager struct {
	mu         sync.Mutex
	state      State
	errMsg     string
	browser    interface{ Close() } // *cloak.Browser when launched
	cancel     context.CancelFunc
	profileDir string
	seedFile   string

	// debugPort is the CDP endpoint the launched Chrome listens on. We
	// allocate it before launch and pass it as --remote-debugging-port so the
	// browser tool can attach over CDP to the very same profile the user is
	// looking at, without spawning a second Chrome and without cloning the
	// social login jar.
	debugPort int
	debugURL  string

	// lockOwner is the current exclusive holder of the social browser, or ""
	// when no one is holding it. Ownership is cooperative: any tool that
	// wants a multi-step run without a peer stealing the tab in the middle
	// takes the lock, then releases it. Single actions may acquire briefly.
	lockOwner    string
	lockCh       chan struct{} // closed while free, replaced on Acquire
	lockDeadline time.Time     // when the current lease expires; refreshed on Renew
}

// New creates a Manager. The profile directory lives under the Antares home.
func New() *Manager {
	profileDir := config.Path("social-browser", "profile")
	seedFile := config.Path("social-browser", "seed")
	return &Manager{
		state:      StateStopped,
		profileDir: profileDir,
		seedFile:   seedFile,
	}
}

// Status returns the current browser state and any error detail.
func (m *Manager) Status() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.state), m.errMsg
}

// Start launches the persistent browser if it is not already running.
// This is synchronous but may take up to 45 seconds on first launch
// (binary download + verification). The caller should run it in a goroutine.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateRunning {
		m.mu.Unlock()
		return nil
	}
	if m.state == StateStarting {
		m.mu.Unlock()
		return fmt.Errorf("browser is already starting")
	}
	m.state = StateStarting
	m.errMsg = ""
	// The browser outlives the request/tool context that starts it. Stop owns
	// its lifetime; a finished upload must not destroy the saved login session.
	launchCtx, launchCancel := context.WithCancel(context.WithoutCancel(ctx))
	m.cancel = launchCancel
	m.mu.Unlock()
	started := false
	timer := time.AfterFunc(60*time.Second, launchCancel)
	defer func() {
		timer.Stop()
		if !started {
			launchCancel()
		}
	}()
	// Allocate a debug port BEFORE launch so we can pass --remote-debugging-port
	// on the command line via ExtraArgs. This is the only reliable way to
	// re-attach to the same Chrome process over CDP without shelling in a
	// second browser and without cloning the user profile — which would
	// separate the login cookies the user painstakingly created.
	port, err := allocFreePort()
	if err != nil {
		m.fail("cannot allocate debug port: %v", err)
		return err
	}

	// Ensure profile and seed directories exist.
	if err := os.MkdirAll(m.profileDir, 0o700); err != nil {
		m.fail("cannot create profile directory: %v", err)
		return err
	}

	// Load or create a stable fingerprint seed.
	seed, err := m.loadOrCreateSeed()
	if err != nil {
		m.fail("cannot initialize fingerprint seed: %v", err)
		return err
	}

	// Launch the browser via cloak-go. The import is deferred to avoid a
	// hard dependency when the social feature is not used.
	browser, cancel, err := m.launchCloak(launchCtx, seed, port)
	if err != nil {
		launchCancel()
		m.fail("browser launch failed: %v", err)
		return err
	}

	debugURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	m.mu.Lock()
	if m.state != StateStarting || launchCtx.Err() != nil {
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		browser.Close()
		return fmt.Errorf("social browser launch was stopped")
	}
	m.browser = browser
	m.cancel = func() {
		launchCancel()
		if cancel != nil {
			cancel()
		}
	}
	m.state = StateRunning
	m.debugPort = port
	m.debugURL = debugURL
	m.mu.Unlock()
	started = true
	return nil
}

// Stop closes the browser if it is running.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.state != StateRunning && m.state != StateStarting {
		m.mu.Unlock()
		return
	}
	browser := m.browser
	cancel := m.cancel
	m.browser = nil
	m.cancel = nil
	m.state = StateStopped
	m.debugPort = 0
	m.debugURL = ""
	// Force-drop any outstanding lease. A crashed browser must not leave a
	// zombie owner blocking every future tool call.
	m.lockOwner = ""
	if m.lockCh != nil {
		close(m.lockCh)
		m.lockCh = nil
	}
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if browser != nil {
		browser.Close()
	}
}

// fail sets the error state with a formatted message.
func (m *Manager) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	m.mu.Lock()
	m.state = StateError
	m.errMsg = msg
	m.mu.Unlock()
}

// loadOrCreateSeed reads or generates the persistent fingerprint seed.
// The seed is a random base64 string stored in a file with 0600 perms.
func (m *Manager) loadOrCreateSeed() (string, error) {
	if raw, err := os.ReadFile(m.seedFile); err == nil {
		s := string(raw)
		if s != "" {
			return s, nil
		}
	}
	seed := make([]byte, 16)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	s := base64.StdEncoding.EncodeToString(seed)
	if err := os.MkdirAll(filepath.Dir(m.seedFile), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(m.seedFile, []byte(s), 0o600); err != nil {
		return "", err
	}
	return s, nil
}

// ProfileDir returns the persistent browser profile path.
func (m *Manager) ProfileDir() string { return m.profileDir }

// Close stops the browser if running. Called on shutdown.
func (m *Manager) Close() {
	m.Stop()
}

// launchCloak launches the stealth browser via cloak-go. The debug port is
// wired through as a Chrome argument so the tool code can attach to the same
// process over CDP.
func (m *Manager) launchCloak(ctx context.Context, seed string, debugPort int) (interface{ Close() }, context.CancelFunc, error) {
	return launchCloakBrowser(ctx, m.profileDir, seed, debugPort)
}

// launchCloakBrowser is the actual cloak-go integration point. It is in a
// separate file so that the cloak-go import can be managed.
var launchCloakBrowser func(ctx context.Context, profileDir, seed string, debugPort int) (interface{ Close() }, context.CancelFunc, error) = defaultLaunchCloak

// defaultLaunchCloak is a placeholder that returns an error if cloak-go is
// not available. The real implementation is in cloak_launcher.go.
func defaultLaunchCloak(ctx context.Context, profileDir, seed string, debugPort int) (interface{ Close() }, context.CancelFunc, error) {
	return nil, nil, fmt.Errorf("social browser backend not configured")
}

// WaitForRunning polls the browser state until it is running or the timeout
// expires. Used by API handlers that started the browser asynchronously.
func (m *Manager) WaitForRunning(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, _ := m.Status()
		if state == string(StateRunning) {
			return true
		}
		if state == string(StateError) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// ---- CDP endpoint --------------------------------------------------------

// DebugURL returns the CDP HTTP endpoint of the launched social browser
// (e.g. "http://127.0.0.1:41234"), or an error if the browser is not
// running yet. Callers dial /json/version off this URL to get the WebSocket
// URL and attach as a controller.
func (m *Manager) DebugURL() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateRunning || m.debugURL == "" {
		return "", fmt.Errorf("social browser is not running")
	}
	return m.debugURL, nil
}

// ---- exclusive lease -----------------------------------------------------
//
// The social browser is a single Chrome window with one login jar. Two
// callers driving it at once trip over each other's page state (a click
// dispatched by A retargets the tab B was reading). Callers that run a
// multi-step flow — an upload followed by a publish confirm — take an
// exclusive lease first, then release it when done. Single one-off actions
// may still auto-acquire and release around themselves when the lease is
// free.
//
// Leases are bounded: a crashed agent that never releases must not block
// every future tool call. The bounded lifetime is refreshed by every action
// the owner runs (Renew), so a long publish that keeps working never times
// out mid-flight while a dead lease evaporates within LeaseTTL.

// LeaseTTL is how long a lease survives without a refresh from its owner.
// Every browser action the owner runs bumps the deadline, so a long publish
// keeps its lease as long as it is still touching the browser.
const LeaseTTL = 5 * time.Minute

// Acquire takes the exclusive lease for owner. If another owner already
// holds an unexpired lease, waits up to timeout for it to release or
// expire. Returns the current holder in the busy error message so the
// caller can tell the agent whose turn it is.
func (m *Manager) Acquire(ctx context.Context, owner string, timeout time.Duration) error {
	if owner == "" {
		return fmt.Errorf("acquire: owner must be non-empty")
	}
	deadline := time.Now().Add(timeout)
	for {
		m.mu.Lock()
		if m.lockOwner == "" || m.lockOwner == owner || time.Now().After(m.lockDeadline) {
			m.lockOwner = owner
			m.lockDeadline = time.Now().Add(LeaseTTL)
			if m.lockCh == nil {
				m.lockCh = make(chan struct{})
			}
			m.mu.Unlock()
			return nil
		}
		holder := m.lockOwner
		wait := m.lockCh
		m.mu.Unlock()

		if timeout <= 0 {
			return fmt.Errorf("social browser busy: held by %q", holder)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("social browser busy: held by %q", holder)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
			// released or expired — loop and retry
		case <-time.After(remaining):
			return fmt.Errorf("social browser busy: held by %q", holder)
		}
	}
}

// Release drops the lease if owner holds it. Wrong-owner releases are a
// no-op — a busy caller cannot yank the tab from the real holder by asking
// to release.
func (m *Manager) Release(owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lockOwner != owner || owner == "" {
		return
	}
	m.lockOwner = ""
	m.lockDeadline = time.Time{}
	if m.lockCh != nil {
		close(m.lockCh)
		m.lockCh = nil
	}
}

// Renew extends the lease if owner holds it. Every browser action the
// owner runs refreshes the deadline, so a long-running flow does not have
// its tab stolen by the expiry sweep while it is still working.
func (m *Manager) Renew(owner string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lockOwner != owner || owner == "" {
		return false
	}
	m.lockDeadline = time.Now().Add(LeaseTTL)
	return true
}

// Holder returns the current lease owner (or "" when free).
func (m *Manager) Holder() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lockOwner == "" {
		return ""
	}
	if time.Now().After(m.lockDeadline) {
		return ""
	}
	return m.lockOwner
}

// TryReenter reports whether owner can act on the browser right now
// without blocking: either it already holds the lease (refreshing it), or
// the lease is free (and it does NOT take it — the caller decides whether
// to acquire). Used by the browser tool to distinguish "same session
// re-entering its held lease" from "some other session peeking".
func (m *Manager) TryReenter(owner string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lockOwner == "" || time.Now().After(m.lockDeadline) {
		return true
	}
	if m.lockOwner == owner {
		m.lockDeadline = time.Now().Add(LeaseTTL)
		return true
	}
	return false
}

// ---- helpers -------------------------------------------------------------

// allocFreePort binds :0 to grab a port the kernel just picked, then hands
// it off. The listener is closed immediately, so a real Chrome can bind it
// straight away. There is a tiny race window if something else on the box
// grabs the same port between here and Chrome starting; in practice Chrome
// binds within a hundred milliseconds and this is fine.
func allocFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
