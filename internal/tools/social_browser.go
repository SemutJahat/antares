package tools

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// socialBrowserTool lets the Social Media agent check and control the persistent
// social media browser. It wraps the SocialBrowserManager from Deps.
type socialBrowserTool struct{}

func (socialBrowserTool) Name() string { return "social_browser" }
func (socialBrowserTool) Description() string {
	return "Manage the persistent social media browser that holds all social login sessions with a stable fingerprint. " +
		"Actions: 'status' (default), 'start' (launch the visible window), 'stop' (close it), " +
		"'acquire' (take an exclusive lease so a multi-step publish will not be interrupted by a peer), " +
		"'release' (drop your lease when done). " +
		"The browser is shared — you and the user can both see and interact with it, but only one automation lease is held at a time. Multi-step publishing MUST acquire before starting and release on success or block; single one-off actions may skip the lease. There is no auto-CAPTCHA handling; ask the user to solve any human check in the same window."
}
func (socialBrowserTool) RequiresApproval() bool { return true }

func (socialBrowserTool) Schema() map[string]any {
	return schema(map[string]any{
		"action":  propDefault("string", "What to do: status (default), start, stop, acquire, release.", "status"),
		"seconds": propDefault("integer", "For acquire: how many seconds to wait for the lease before giving up. 0 fails immediately if busy.", 30),
	})
}

// socialLeaseInterface is the optional lease API on the social browser
// manager. Declared locally so no change to Deps.SocialBrowserManager is
// required — existing mocks that only implement Status/Start/Stop keep
// compiling; real managers can satisfy this to enable the lease actions.
type socialLeaseInterface interface {
	Acquire(ctx context.Context, owner string, timeout time.Duration) error
	Release(owner string)
	Renew(owner string) bool
	Holder() string
}

func (socialBrowserTool) Execute(ctx context.Context, in Input) Result {
	var args struct {
		Action  string `json:"action"`
		Seconds int    `json:"seconds"`
	}
	if err := in.Bind(&args); err != nil {
		return Errorf("%v", err)
	}
	action := strings.TrimSpace(strings.ToLower(args.Action))
	if action == "" {
		action = "status"
	}

	if in.Deps == nil || in.Deps.SocialBrowser == nil {
		return Errorf("social browser is not available")
	}

	mgr := in.Deps.SocialBrowser
	state, errMsg := mgr.Status()

	// The lease API is optional; a mock manager may not provide it. Fail
	// clearly for the lease actions rather than pretending to succeed.
	lease, hasLease := mgr.(socialLeaseInterface)
	owner := in.SessionID
	if owner == "" {
		owner = "default"
	}

	switch action {
	case "status":
		result := fmt.Sprintf("Browser state: %s", state)
		if errMsg != "" {
			result += fmt.Sprintf("\nError: %s", errMsg)
		}
		if hasLease {
			if h := lease.Holder(); h != "" {
				if h == owner {
					result += "\nLease: held by you (this session)."
				} else {
					result += fmt.Sprintf("\nLease: held by another session %q.", h)
				}
			} else {
				result += "\nLease: free."
			}
		}
		if state == "running" {
			result += "\nThe browser is open and visible. All social media login sessions are available."
		} else if state == "stopped" {
			result += "\nUse action 'start' to launch the browser."
		}
		return Result{Content: result, Meta: map[string]any{"state": state}}

	case "start":
		if state == "running" {
			return Result{Content: "Browser is already running. All social media login sessions are available in the visible window."}
		}
		if err := mgr.Start(ctx); err != nil {
			return Errorf("failed to start browser: %v", err)
		}
		return Result{Content: "Browser started successfully. The browser window is now visible and ready. All social media login sessions are available."}

	case "stop":
		mgr.Stop()
		return Result{Content: "Browser stopped."}

	case "acquire":
		if !hasLease {
			return Errorf("this social browser manager does not support leases")
		}
		if state != "running" {
			return Errorf("social browser is %s — start it before acquiring a lease", state)
		}
		timeout := time.Duration(args.Seconds) * time.Second
		if args.Seconds < 0 {
			timeout = 0
		}
		if err := lease.Acquire(ctx, owner, timeout); err != nil {
			return Errorf("%v", err)
		}
		return Text("Lease acquired. You now hold the social browser exclusively; release it when the publish workflow completes.")

	case "release":
		if !hasLease {
			return Errorf("this social browser manager does not support leases")
		}
		lease.Release(owner)
		return Text("Lease released.")

	default:
		return Errorf("unknown action %q; use status, start, stop, acquire, or release", action)
	}
}

var _ Tool = socialBrowserTool{}
