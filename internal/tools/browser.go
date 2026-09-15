package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/enowdev/antares/internal/browser"
	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/creator"
	"github.com/enowdev/antares/internal/textutil"
)

// browserSessions keeps one browser per conversation. A page that survives
// between tool calls is what makes multi-step work possible: log in once, then
// click through the site.
var browserSessions = struct {
	sync.Mutex
	byKey map[string]*browser.Session
	// social is a single, process-wide handle attached over CDP to the
	// persistent SocialBrowser manager's Chrome. It is not idle-reaped and
	// not per-conversation — every session that sets session="social" shares
	// this one attached controller so a lock held by one caller means the
	// same physical tab another caller sees.
	social *browser.Session
	// socialURL is the CDP debug URL we attached the social session against.
	// If the underlying manager stops/starts and the URL changes we drop the
	// attached session and re-attach on next use.
	socialURL string
	reap      sync.Once
}{byKey: map[string]*browser.Session{}}

// sessionFor returns the browser for a conversation, creating it on first use.
func sessionFor(key string, cfg *config.Config) *browser.Session {
	browserSessions.Lock()
	defer browserSessions.Unlock()

	if s, ok := browserSessions.byKey[key]; ok {
		return s
	}
	opts := browser.Options{Headless: true, Width: 1280, Height: 800}
	if cfg != nil {
		b := cfg.Tools.Browser
		opts.Executable = b.Executable
		opts.RemoteURL = b.RemoteURL
		opts.UserDataDir = config.Expand(b.UserDataDir)
		opts.Headless = !b.Headed
		if b.Width > 0 {
			opts.Width = b.Width
		}
		if b.Height > 0 {
			opts.Height = b.Height
		}
		opts.Stealth = b.Stealth
		opts.Proxy = b.Proxy
		opts.Timezone = b.Timezone
		opts.Locale = b.Locale
	}
	s := browser.New(opts)
	browserSessions.byKey[key] = s

	// One reaper for the process: an abandoned browser is a few hundred
	// megabytes that nobody will ever close by hand.
	browserSessions.reap.Do(func() {
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for range t.C {
				closeIdleBrowsers(15 * time.Minute)
			}
		}()
	})
	return s
}

// socialEndpoint is the optional CDP-attach interface a SocialBrowserManager
// may satisfy. Declared locally so no shared Deps change is required and the
// existing SocialBrowserManager interface + its mocks keep compiling
// unchanged. Real implementation lives on socialbrowser.Manager.
type socialEndpoint interface {
	DebugURL() (string, error)
	Acquire(ctx context.Context, owner string, timeout time.Duration) error
	Release(owner string)
	Renew(owner string) bool
	Holder() string
	TryReenter(owner string) bool
}

// socialSessionFor returns the process-wide CDP-attached view of the
// persistent social browser, creating and attaching it on first use. The
// caller MUST have coordinated with the manager's lease already (or be
// happy to hold no lease for a one-shot read). ep is the endpoint interface
// resolved from Deps.SocialBrowser.
func socialSessionFor(ep socialEndpoint) (*browser.Session, string, error) {
	debugURL, err := ep.DebugURL()
	if err != nil {
		return nil, "", err
	}
	browserSessions.Lock()
	defer browserSessions.Unlock()
	if browserSessions.social != nil && browserSessions.socialURL == debugURL {
		return browserSessions.social, debugURL, nil
	}
	// URL changed (or first attach): drop any stale attached session and
	// build a fresh one. Stop() is safe against a not-started session.
	if browserSessions.social != nil {
		browserSessions.social.Stop()
		browserSessions.social = nil
	}
	s := browser.New(browser.Options{
		// RemoteURL makes Start attach to the already-running Chrome instead
		// of spawning a second one — this is how we drive the same profile
		// the user is looking at, with the same login jar.
		RemoteURL: debugURL,
		Width:     1280,
		Height:    800,
	})
	browserSessions.social = s
	browserSessions.socialURL = debugURL
	return s, debugURL, nil
}

func closeIdleBrowsers(idle time.Duration) {
	browserSessions.Lock()
	var stale []*browser.Session
	for key, s := range browserSessions.byKey {
		if s.Started() && time.Since(s.LastUsed()) > idle {
			stale = append(stale, s)
			delete(browserSessions.byKey, key)
		}
	}
	browserSessions.Unlock()
	for _, s := range stale {
		s.Stop()
	}
}

// CloseBrowsers shuts every browser down, for process exit.
func CloseBrowsers() {
	browserSessions.Lock()
	all := make([]*browser.Session, 0, len(browserSessions.byKey))
	for key, s := range browserSessions.byKey {
		all = append(all, s)
		delete(browserSessions.byKey, key)
	}
	// The attached social session is a CDP client, not a spawned Chrome —
	// stopping it just closes our WebSocket. The real browser lives under
	// socialbrowser.Manager and is shut down there.
	if s := browserSessions.social; s != nil {
		all = append(all, s)
		browserSessions.social = nil
		browserSessions.socialURL = ""
	}
	browserSessions.Unlock()
	for _, s := range all {
		s.Stop()
	}
}

// ---- browser tool -----------------------------------------------------------

type browserTool struct{}

func (browserTool) Name() string { return "browser" }

func (browserTool) Description() string {
	return "Drive a real web browser: open pages, read them, click, type, submit forms, and attach files. " +
		"Use it for anything that needs JavaScript, a login, or a form — web_fetch is faster for plain documents.\n\n" +
		"The usual loop is: navigate, then snapshot to see what is on the page, then click or type " +
		"using the e-numbers the snapshot returned, then snapshot again to see what changed. " +
		"References like e7 only stay valid until the page changes, so take a fresh snapshot after every action " +
		"that navigates or re-renders.\n\n" +
		"Pass session=\"social\" to drive the persistent social-media browser instead of a fresh one — the same login jar the user sees. Long publish flows should take an exclusive lease first via the social_browser tool's acquire action, so a concurrent agent does not steal the tab mid-run; single one-off reads can skip the lease."
}

func (browserTool) Schema() map[string]any {
	return schema(map[string]any{
		"action": propEnum("What to do.",
			"navigate", "snapshot", "click", "type", "select", "text", "screenshot",
			"press", "scroll", "back", "wait_for", "eval", "console", "requests",
			"tabs", "upload", "close"),
		"url":        prop("string", "For navigate: the address to open."),
		"ref":        prop("string", "Element reference from a snapshot, like e7."),
		"text":       prop("string", "For type: the text to enter. For wait_for: the text to wait for. For select: the option."),
		"submit":     propDefault("boolean", "For type: press Enter afterwards.", false),
		"key":        prop("string", "For press: Enter, Tab, Escape, ArrowDown, or a single character."),
		"to":         propEnum("For scroll: which way.", "down", "up", "top", "bottom", "left", "right"),
		"amount":     propDefault("integer", "For scroll: pixels to move.", 600),
		"script":     prop("string", "For eval: JavaScript to run in the page. Its value is returned."),
		"full":       propDefault("boolean", "For screenshot: capture the whole page rather than the viewport.", false),
		"max":        propDefault("integer", "Cap on elements in a snapshot, or characters of text.", 0),
		"seconds":    propDefault("integer", "For wait_for: how long to wait.", 15),
		"path":       prop("string", "For upload: local file path. Without project_id, resolves inside the workspace/project. With project_id, resolves inside <config-home>/content-creator/<project_id>/ only."),
		"session":    propDefault("string", "Which browser to drive: default (per-conversation ephemeral) or \"social\" (the persistent social-media browser with saved logins).", "default"),
		"project_id": prop("string", "For upload of a Content Creator artifact: the project id whose registered artifact directory the path is resolved against. Uploads outside this directory are refused."),
	}, "action")
}

// RequiresApproval reports that this tool reaches the network and can act on
// a page as the signed-in user.
func (browserTool) RequiresApproval() bool { return true }

// UntrustedOutput reports that page text, snapshots, and console output are
// written by the site being driven.
func (browserTool) UntrustedOutput() bool { return true }

func (browserTool) Execute(ctx context.Context, in Input) Result {
	var args struct {
		Action    string `json:"action"`
		URL       string `json:"url"`
		Ref       string `json:"ref"`
		Text      string `json:"text"`
		Submit    bool   `json:"submit"`
		Key       string `json:"key"`
		To        string `json:"to"`
		Amount    int    `json:"amount"`
		Script    string `json:"script"`
		Full      bool   `json:"full"`
		Max       int    `json:"max"`
		Seconds   int    `json:"seconds"`
		Path      string `json:"path"`
		Session   string `json:"session"`
		ProjectID string `json:"project_id"`
	}
	if err := in.Bind(&args); err != nil {
		return Errorf("%v", err)
	}

	var cfg *config.Config
	if in.Deps != nil {
		cfg = in.Deps.Config
	}
	if cfg != nil && !cfg.Tools.Browser.Enabled {
		return Errorf("the browser tool is switched off (tools.browser.enabled = false)")
	}

	action := strings.ToLower(strings.TrimSpace(args.Action))
	sessionKind := strings.ToLower(strings.TrimSpace(args.Session))
	if sessionKind == "" {
		sessionKind = "default"
	}

	var (
		s           *browser.Session
		key         string
		social      bool
		owner       string
		heldByOwner bool
	)

	switch sessionKind {
	case "default":
		key = in.SessionID
		if key == "" {
			key = "default"
		}
		s = sessionFor(key, cfg)

	case "social":
		social = true
		if in.Deps == nil || in.Deps.SocialBrowser == nil {
			return Errorf("social browser is not available — the social feature is not configured")
		}
		ep, ok := in.Deps.SocialBrowser.(socialEndpoint)
		if !ok {
			return Errorf("the configured social browser manager does not expose a CDP endpoint — cannot drive it through this tool")
		}
		// Owner is always the tool session id; never a value the model
		// supplied. That is what keeps a second agent from impersonating
		// the lease holder and stealing the tab.
		owner = in.SessionID
		if owner == "" {
			owner = "default"
		}

		// Same-owner re-entry: if we already hold the lease we transparently
		// refresh it. If nobody holds it, we auto-acquire briefly for this
		// single action and release at the end. If someone else holds it,
		// bail with a clear busy error naming the holder — the caller is
		// expected to acquire explicitly via social_browser before starting
		// a multi-step publish.
		if ep.TryReenter(owner) {
			heldByOwner = ep.Renew(owner)
		}
		if !heldByOwner {
			if holder := ep.Holder(); holder != "" && holder != owner {
				return Errorf("social browser busy: another session %q is publishing — try again in a moment", holder)
			}
			// Briefly acquire for this one action; release on the way out.
			if err := ep.Acquire(ctx, owner, 0); err != nil {
				return Errorf("%v", err)
			}
			defer ep.Release(owner)
		}

		var attachErr error
		s, _, attachErr = socialSessionFor(ep)
		if attachErr != nil {
			return Errorf("attach to social browser: %v", attachErr)
		}

	default:
		return Errorf("unknown session %q — use \"default\" or \"social\"", args.Session)
	}

	// Everything but close and navigate expects a page that is already open.
	// The social session is different: the browser is already running under
	// the manager, but no page is attached yet — Start() attaches to the
	// existing browser and opens a fresh page. The check would false-fail on
	// first use, so skip it for social.
	if !social && action != "close" && action != "navigate" {
		if !s.Started() {
			return Errorf("no page is open yet — call browser with action \"navigate\" first")
		}
	}

	switch action {
	case "navigate":
		if strings.TrimSpace(args.URL) == "" {
			return Errorf("url is required to navigate")
		}
		in.Emit(Progress{Tool: "browser", Message: "opening " + args.URL})
		if err := s.Navigate(ctx, args.URL); err != nil {
			return Errorf("could not open %s: %v", args.URL, err)
		}
		// Automatic anti-bot handling: if the page is a Cloudflare/Turnstile/
		// captcha interstitial, wait for the stealth browser to clear it so the
		// agent gets the real content instead of the challenge page.
		if !social {
			autoClearChallenge(ctx, s, in.Emit)
		}
		return browserPageSummary(ctx, s, "Opened")

	case "snapshot":
		snap, err := s.Snapshot(ctx, args.Max)
		if err != nil {
			return Errorf("%v", err)
		}
		head, _ := browserHeader(ctx, s)
		if strings.TrimSpace(snap) == "" {
			return Text(head + "\n\nNothing interactive is on this page. Try action \"text\" to read it.")
		}
		return Text(head + "\n\n" + snap)

	case "click":
		out, err := s.Click(ctx, args.Ref)
		if err != nil {
			return Errorf("%v", err)
		}
		return browserPageSummary(ctx, s, out+" —")

	case "type":
		if args.Ref == "" {
			return Errorf("ref is required — take a snapshot and use one of its e-numbers")
		}
		out, err := s.Type(ctx, args.Ref, args.Text, args.Submit)
		if err != nil {
			return Errorf("%v", err)
		}
		if args.Submit {
			return browserPageSummary(ctx, s, out+" and submitted —")
		}
		return Text(out)

	case "select":
		out, err := s.Select(ctx, args.Ref, args.Text)
		if err != nil {
			return Errorf("%v", err)
		}
		return Text(out)

	case "text":
		body, err := s.Text(ctx, args.Ref, args.Max)
		if err != nil {
			return Errorf("%v", err)
		}
		head, _ := browserHeader(ctx, s)
		if strings.TrimSpace(body) == "" {
			return Text(head + "\n\nThe page has no readable text.")
		}
		return Text(head + "\n\n" + body)

	case "screenshot":
		png, err := s.Screenshot(ctx, args.Full)
		if err != nil {
			return Errorf("%v", err)
		}
		path, err := saveScreenshot(in.Workspace, png)
		if err != nil {
			return Errorf("%v", err)
		}
		return Result{
			Content: fmt.Sprintf("Saved a screenshot to %s (%d KB). Read it with a vision-capable model, or open it yourself.", path, len(png)/1024),
			Meta:    map[string]any{"path": path, "bytes": len(png)},
		}

	case "press":
		if args.Key == "" {
			return Errorf("key is required")
		}
		if err := s.PressKey(ctx, args.Key); err != nil {
			return Errorf("%v", err)
		}
		_ = s.WaitReady(ctx, 3*time.Second)
		return Text("pressed " + args.Key)

	case "scroll":
		out, err := s.Scroll(ctx, args.To, args.Amount)
		if err != nil {
			return Errorf("%v", err)
		}
		return Text(out)

	case "back":
		if err := s.Back(ctx); err != nil {
			return Errorf("%v", err)
		}
		return browserPageSummary(ctx, s, "Went back —")

	case "wait_for":
		if args.Text == "" {
			return Errorf("text is required")
		}
		out, err := s.WaitFor(ctx, args.Text, time.Duration(args.Seconds)*time.Second)
		if err != nil {
			return Errorf("%v", err)
		}
		return Text(out)

	case "eval":
		if strings.TrimSpace(args.Script) == "" {
			return Errorf("script is required")
		}
		v, err := s.Eval(ctx, args.Script)
		if err != nil {
			return Errorf("the script threw: %v", err)
		}
		out := string(v)
		if out == "" || out == "null" {
			out = "(no value)"
		}
		return Text(truncateTool(out, 8000))

	case "console":
		lines := s.Console()
		if len(lines) == 0 {
			return Text("The console has been quiet.")
		}
		return Text(truncateTool(strings.Join(lines, "\n"), 8000))

	case "requests":
		lines := s.Requests()
		if len(lines) == 0 {
			return Text("No responses have been recorded since the last check.")
		}
		return Text(truncateTool(strings.Join(lines, "\n"), 8000))

	case "tabs":
		head, err := browserHeader(ctx, s)
		if err != nil {
			return Errorf("%v", err)
		}
		return Text(head)

	case "upload":
		if args.Ref == "" {
			return Errorf("ref is required — snapshot the page and use the e-number of the <input type=file>")
		}
		if strings.TrimSpace(args.Path) == "" {
			return Errorf("path is required — the local file to attach")
		}
		abs, err := resolveUploadPath(ctx, in, args.ProjectID, args.Path)
		if err != nil {
			return Errorf("%v", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return Errorf("cannot read %s: %v", args.Path, err)
		}
		if info.IsDir() {
			return Errorf("%s is a directory — pick a file", args.Path)
		}
		if !info.Mode().IsRegular() {
			return Errorf("%s is not a regular file", args.Path)
		}
		out, err := s.Upload(ctx, args.Ref, abs)
		if err != nil {
			return Errorf("%v", err)
		}
		return Text(out)

	case "close":
		if social {
			// Never Close the shared social attach — that would leave the
			// next social caller with a stale attach that fails on first
			// use. Clearing it lets the next call re-attach fresh.
			browserSessions.Lock()
			if browserSessions.social != nil {
				browserSessions.social.Stop()
				browserSessions.social = nil
				browserSessions.socialURL = ""
			}
			browserSessions.Unlock()
			return Text("Detached from the social browser. The window itself stays open under the social manager.")
		}
		s.Stop()
		browserSessions.Lock()
		delete(browserSessions.byKey, key)
		browserSessions.Unlock()
		return Text("Closed the browser.")

	default:
		return Errorf("unknown action %q", args.Action)
	}
}

// browserHeader is the one-line "where am I" every result opens with.
func browserHeader(ctx context.Context, s *browser.Session) (string, error) {
	url, err := s.URL(ctx)
	if err != nil {
		return "", err
	}
	title, _ := s.Title(ctx)
	if title == "" {
		title = "(untitled)"
	}
	return fmt.Sprintf("%s\n%s", title, url), nil
}

// browserPageSummary reports where the page landed and what is on it, so a
// navigation or click does not need a second call to be useful.
func browserPageSummary(ctx context.Context, s *browser.Session, prefix string) Result {
	head, err := browserHeader(ctx, s)
	if err != nil {
		return Errorf("%v", err)
	}
	snap, err := s.Snapshot(ctx, 60)
	if err != nil {
		return Text(prefix + " " + head)
	}
	out := prefix + " " + head
	if strings.TrimSpace(snap) != "" {
		out += "\n\n" + snap
	}
	return Text(out)
}

// saveScreenshot writes a capture somewhere the user can find it.
func saveScreenshot(workspace string, png []byte) (string, error) {
	dir := filepath.Join(config.Home(), "screenshots")
	if workspace != "" {
		if info, err := os.Stat(workspace); err == nil && info.IsDir() {
			dir = filepath.Join(workspace, ".antares", "screenshots")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("shot-%d.png", time.Now().UnixMilli()))
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// truncateTool caps tool output at max characters and says how long the whole
// text was, so the model knows what it is missing.
func truncateTool(s string, max int) string {
	out := textutil.TruncateRunes(s, max)
	if out == s {
		return s
	}
	return out + fmt.Sprintf("\n\n… truncated, %d characters total", utf8.RuneCountInString(s))
}

// challengeDetectJS reports the kind of bot-challenge on the current page, or ""
// if the page looks like normal content.
const challengeDetectJS = `(() => {
  const t = (document.title||'').toLowerCase();
  if (t.includes('just a moment') || t.includes('attention required') || t.includes('verifying you are human') || t.includes('checking your browser')) return 'cloudflare';
  if (document.querySelector('.cf-turnstile, [name="cf-turnstile-response"]')) return 'turnstile';
  if (document.querySelector('.g-recaptcha, iframe[src*="recaptcha"]')) return 'recaptcha';
  if (document.querySelector('.h-captcha, iframe[src*="hcaptcha"]')) return 'hcaptcha';
  return '';
})()`

// autoClearChallenge waits for an anti-bot interstitial to pass. The stealth
// browser clears many challenges on its own; this polls until the challenge
// markers are gone or a short budget elapses, so normal browsing "just works"
// against bot-protected pages without a separate solve step.
func autoClearChallenge(ctx context.Context, s *browser.Session, emit func(Progress)) {
	kind := strings.Trim(strings.TrimSpace(evalStr(ctx, s)), `"`)
	if kind == "" {
		return
	}
	emit(Progress{Tool: "browser", Message: "bot challenge detected (" + kind + "), waiting for it to clear…"})
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if strings.Trim(strings.TrimSpace(evalStr(ctx, s)), `"`) == "" {
			emit(Progress{Tool: "browser", Message: "challenge cleared"})
			return
		}
	}
	emit(Progress{Tool: "browser", Message: "challenge still present after waiting — the page may need an interactive solve"})
}

func evalStr(ctx context.Context, s *browser.Session) string {
	v, err := s.EvalString(ctx, challengeDetectJS)
	if err != nil {
		return ""
	}
	return v
}

// creatorArtifactResolver is what the Content Creator service exposes for
// authorizing per-project artifact reads: only paths registered on a
// specific project (references[].path, shots[].keyframe_path/video_path/
// last_frame_path, final_path) resolve to an absolute path; everything
// else — .. traversal, symlink escapes, arbitrary files inside the project
// directory, unrelated projects — is refused. The tool code calls this via
// an interface rather than pinning the concrete Service type, so the
// upload gate is expressible without a Deps struct change.
type creatorArtifactResolver interface {
	Artifact(ctx context.Context, projectID, relPath string) (string, error)
}

// resolveUploadPath authorizes a file for browser upload. Without
// project_id it delegates to resolveRead so an ordinary session may upload
// workspace files (and a project session may reach the wider machine, per
// existing read semantics). With project_id it MUST resolve through the
// Content Creator service's Artifact resolver, which returns paths ONLY
// for artifacts registered on that project. This prevents a creator role
// from uploading anything else on the machine — even a stray file inside
// the project directory that is not on the registered artefact set,
// another project's clips, the KV database, or the social fingerprint
// seed — through the upload action.
func resolveUploadPath(ctx context.Context, in Input, projectID, path string) (string, error) {
	pid := strings.TrimSpace(projectID)
	if rc, scoped := creator.RunFromContext(ctx); scoped && pid == "" {
		pid = rc.ProjectID
	}
	if pid == "" {
		return resolveRead(in, path)
	}
	if in.Deps == nil || in.Deps.Store == nil {
		return "", fmt.Errorf("cannot resolve creator project %q: no store available", projectID)
	}
	svc := creator.New(in.Deps.Store, config.Path("content-creator"))

	// Stage guard: when a creator run context is active, uploads MUST match
	// the authorised run — same project, publish/full stage, auto mode for
	// full, and a Publication that has already reached "uploading" (which is
	// only set by prepare_publish). This closes the door on an LLM skipping
	// prepare_publish and dropping a video straight into a publisher UI
	// mid-research/mid-plan. Outside a creator run (ad-hoc chat) the guard
	// stands down and the artefact resolver alone gates the path — the same
	// behaviour the browser tool already provides for research/read flows.
	if rc, ok := creator.RunFromContext(ctx); ok && rc.ProjectID != "" {
		if rc.ProjectID != pid {
			return "", fmt.Errorf("upload rejected: active run is for project %q, not %q", rc.ProjectID, pid)
		}
		if rc.PublishMode != "auto" {
			return "", fmt.Errorf("upload rejected: draft runs cannot publish")
		}
		switch rc.Stage {
		case "publish":
			// allowed
		case "full":
			if rc.PublishMode != "auto" {
				return "", fmt.Errorf("upload rejected: full run in %q mode does not permit publishing (publish_mode must be auto)", rc.PublishMode)
			}
		default:
			return "", fmt.Errorf("upload rejected: current run stage %q does not permit publishing — run the publish stage first", rc.Stage)
		}
		proj, err := svc.Get(ctx, pid)
		if err != nil {
			return "", fmt.Errorf("creator project %q: %w", pid, err)
		}
		if proj.Publication.Status != "uploading" {
			return "", fmt.Errorf("upload rejected: publication status is %q — call prepare_publish first so the artefact is registered as uploading", proj.Publication.Status)
		}
	}

	var resolver creatorArtifactResolver = svc
	abs, err := resolver.Artifact(ctx, pid, path)
	if err != nil {
		return "", fmt.Errorf("creator project %q: %w", projectID, err)
	}
	return abs, nil
}
