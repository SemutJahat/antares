package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestUploadSmoke drives a real headless Chrome against a data-URL page that
// exposes an <input type=file>, uploads a temp file through the CDP path, and
// reads the file name and byte count back out via JS. It is skipped when no
// Chrome is on the machine — this is a smoke of the upload code path, not a
// unit test the CI must run everywhere. It is a regression guard against
// silent breakage of the DOM.setFileInputFiles wiring.
func TestUploadSmoke(t *testing.T) {
	if _, err := FindExecutable(""); err != nil {
		t.Skip("no Chrome on this machine")
	}
	dir := t.TempDir()
	payload := []byte("antares upload smoke bytes\n")
	src := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	html := `<!doctype html><html><body>
<input id="f" type="file">
<script>
window.__seen = null;
document.getElementById('f').addEventListener('change', (e) => {
  const f = e.target.files[0];
  window.__seen = {name: f.name, size: f.size};
});
</script>
</body></html>`
	page := filepath.Join(dir, "page.html")
	if err := os.WriteFile(page, []byte(html), 0o600); err != nil {
		t.Fatal(err)
	}
	pageURL := "file://" + page

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s := New(Options{Headless: true, Width: 1024, Height: 768})
	defer s.Stop()
	if err := s.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if _, err := s.Snapshot(ctx, 0); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// The <input id=f> is the first (and only) interactive element on the
	// synthesized page, so e1 pins to it. If snapshot heuristics ever
	// change, the assertion below fails loudly instead of silently
	// uploading against the wrong element.
	out, err := s.Upload(ctx, "e1", src)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !strings.Contains(out, "clip.mp4") {
		t.Fatalf("upload response missing filename: %q", out)
	}
	v, err := s.EvalString(ctx, `JSON.stringify(window.__seen)`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(v, `"name":"clip.mp4"`) || !strings.Contains(v, `"size":27`) {
		t.Fatalf("browser did not see the file (%q)", v)
	}
}
