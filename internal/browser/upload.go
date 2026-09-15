package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Upload attaches a local file to the <input type="file"> element pointed to
// by the snapshot reference `ref` (e.g. "e7"). It is the native browser upload
// path — DOM.setFileInputFiles pushes the bytes at CDP level, so no fake
// keystrokes or drag-and-drop dances are needed and React/Vue onChange
// handlers fire exactly the same way they would for a human file-picker.
//
// The caller (tool layer) authorizes the path; here we only sanity-check that
// the file exists and is readable, so a broken path fails with a legible
// error before the CDP call.
//
// Some publisher UIs hide the real <input type=file> behind a styled button
// that clicks it on demand. When the ref points at such a container, this
// function walks one level of DOM (the ref itself, then its subtree, then a
// nearby form/section) looking for a file input. It refuses to upload against
// anything else — a text input, a link, a div — so the mistake is visible.
func (s *Session) Upload(ctx context.Context, ref, path string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("ref is required — snapshot the page and use the e-number of the file input")
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%q is a directory — pick a file", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", path)
	}
	if f, err := os.Open(abs); err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	} else {
		f.Close()
	}

	expr, err := refExpr(ref)
	if err != nil {
		return "", err
	}

	// One JS pass: resolve the nearest file input under the ref, return
	// it as a Runtime object. Runtime.evaluate hands back an objectId we
	// can convert to a DOM node id in the next call.
	js := fmt.Sprintf(`(() => {
      const orig = %s;
      if (!orig) return null;
      const isFile = (el) => el && el.tagName === 'INPUT' && (el.type || '').toLowerCase() === 'file';
      if (isFile(orig)) return orig;
      // Nested file input (styled-button wrappers).
      const child = orig.querySelector && orig.querySelector('input[type=file]');
      if (child) return child;
      // A sibling under the same form/section (upload dropzones).
      const parent = orig.closest && orig.closest('form, section, [role=dialog], div');
      if (parent) {
        const near = parent.querySelector('input[type=file]');
        if (near) return near;
      }
      return null;
    })()`, expr)

	raw, err := s.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    js,
		"returnByValue": false,
	})
	if err != nil {
		return "", fmt.Errorf("locate file input: %w", err)
	}
	var evalOut struct {
		Result struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			ObjectID string `json:"objectId"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails,omitempty"`
	}
	if err := json.Unmarshal(raw, &evalOut); err != nil {
		return "", fmt.Errorf("parse eval result: %w", err)
	}
	if len(evalOut.ExceptionDetails) > 0 {
		return "", fmt.Errorf("%s is no longer on the page — take a fresh snapshot", ref)
	}
	if evalOut.Result.Type == "object" && evalOut.Result.Subtype == "null" {
		return "", fmt.Errorf("%s is not a file input and no file input is nearby — snapshot again and use the e-number of the <input type=file>", ref)
	}
	if evalOut.Result.ObjectID == "" {
		return "", fmt.Errorf("%s did not resolve to a DOM element", ref)
	}

	// Best-effort release of the transient JS handle once we're done.
	defer func() {
		_, _ = s.call(ctx, "Runtime.releaseObject", map[string]any{"objectId": evalOut.Result.ObjectID})
	}()
	// Prime CDP's DOM tree — DOM.requestNode requires that the current
	// document has been fetched at least once in this session. A fresh
	// attach or a navigation may leave the tree unretrieved; call
	// DOM.getDocument first so the objectId → nodeId translation succeeds.
	if _, err := s.call(ctx, "DOM.getDocument", map[string]any{"depth": 0}); err != nil {
		return "", fmt.Errorf("prime DOM: %w", err)
	}

	// Convert to a DOM node id for setFileInputFiles.
	raw, err = s.call(ctx, "DOM.requestNode", map[string]any{
		"objectId": evalOut.Result.ObjectID,
	})
	if err != nil {
		return "", fmt.Errorf("resolve DOM node: %w", err)
	}
	var reqNode struct {
		NodeID int `json:"nodeId"`
	}
	_ = json.Unmarshal(raw, &reqNode)
	if reqNode.NodeID == 0 {
		return "", fmt.Errorf("resolve DOM node: empty nodeId")
	}

	// setFileInputFiles is the Chromium-approved way to attach files
	// without simulating a native picker; it uses the file path directly
	// (Chrome reads bytes on the browser-process side), so this only
	// works for local files — the workspace-confined tool layer is the
	// authority on that.
	if _, err := s.call(ctx, "DOM.setFileInputFiles", map[string]any{
		"files":  []string{abs},
		"nodeId": reqNode.NodeID,
	}); err != nil {
		return "", fmt.Errorf("attach file: %w", err)
	}
	return fmt.Sprintf("attached %s (%d bytes) to %s", filepath.Base(abs), info.Size(), ref), nil
}
