package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/config"
)

// Disabling a block MUST refuse before any provider lookup. Any regression
// here risks a paid POST silently going through.
func TestVideoEndpoint_DisabledRefuses(t *testing.T) {
	cfg := &config.Config{}
	cfg.VideoGen.Enabled = false
	cfg.VideoGen.APIKey = "sk-live-anything"
	if _, err := VideoEndpoint(cfg); err == nil {
		t.Fatal("VideoEndpoint returned an endpoint for a disabled block")
	}
}

// A missing key on an enabled block MUST refuse rather than fall through.
func TestVideoEndpoint_NoKeyRefuses(t *testing.T) {
	cfg := &config.Config{}
	cfg.VideoGen.Enabled = true
	cfg.VideoGen.BaseURL = "https://api.example.com/v1"
	if _, err := VideoEndpoint(cfg); err == nil {
		t.Fatal("VideoEndpoint returned an endpoint with no API key")
	}
}

// GenerateImage with a bad reference must fail BEFORE any request is made
// (so we do not spend money to discover the reference was garbage).
func TestGenerateImage_BadReferenceShortCircuits(t *testing.T) {
	ep := Endpoint{BaseURL: "http://127.0.0.1:1", APIKey: "test", Model: "gpt-image-1"}
	err := GenerateImage(context.Background(), ep, "hi", "1024x1024", []string{"/nope/does-not-exist.png"}, t.TempDir()+"/out.png")
	if err == nil {
		t.Fatal("expected error for missing reference")
	}
	// The error must be a reference validation error, not an HTTP error.
	if !strings.Contains(err.Error(), "reference") {
		t.Fatalf("expected reference error, got %v", err)
	}
}

// The redact helper must strip the API key from any error string. This is
// what stops a leaked bearer from surfacing in the model transcript.
func TestEndpoint_RedactHidesKey(t *testing.T) {
	ep := Endpoint{APIKey: "sk-secret-abc"}
	if got := ep.redact("call failed with Bearer sk-secret-abc"); strings.Contains(got, "sk-secret-abc") {
		t.Fatalf("redact left the key in: %q", got)
	}
}

// referenceDataURL MUST resize a source image to exactly the requested
// canvas so Sora's dimension check passes. Regression: an earlier draft
// passed the raw file through when a size was requested.
func TestReferenceDataURL_ResizesToTarget(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for y := range 200 {
		for x := range 200 {
			src.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	path := t.TempDir() + "/src.png"
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, src); err != nil {
		t.Fatal(err)
	}
	f.Close()

	u, err := referenceDataURL(path, 720, 1280)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("wrong prefix: %q", u[:40])
	}
	raw, err := base64.StdEncoding.DecodeString(u[len(prefix):])
	if err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b := got.Bounds()
	if b.Dx() != 720 || b.Dy() != 1280 {
		t.Fatalf("resized to %dx%d, want 720x1280", b.Dx(), b.Dy())
	}
}

// urlSegment MUST refuse any id that could pivot the request onto another
// path or query. This is the primary defence against a malicious job id
// leaking the bearer to /admin?token=… or a sibling resource.
func TestURLSegment_RejectsInjection(t *testing.T) {
	bad := []string{"", " ", ".", "..", "vid/../secret", "vid?token=x", "vid#frag", "a/b"}
	for _, in := range bad {
		if _, err := urlSegment(in); err == nil {
			t.Fatalf("urlSegment accepted %q, want error", in)
		}
	}
	if got, err := urlSegment("video_abc-123"); err != nil || got != "video_abc-123" {
		t.Fatalf("urlSegment(good) = %q, %v; want video_abc-123, nil", got, err)
	}
}

// A response larger than the response cap MUST return errResponseTooLarge
// so callers can distinguish "too big" from a decode failure. Regression:
// an earlier draft silently truncated + tried to JSON-decode garbage.
func TestReadAllBounded_CapReported(t *testing.T) {
	big := bytes.Repeat([]byte("A"), maxResponse+128)
	b, err := readAllBounded(bytes.NewReader(big))
	if err != errResponseTooLarge {
		t.Fatalf("got err=%v, want errResponseTooLarge", err)
	}
	if len(b) != maxResponse {
		t.Fatalf("got %d bytes back, want %d", len(b), maxResponse)
	}
}

// The control-plane redirect policy MUST refuse cross-host redirects so a
// provider cannot bounce the bearer to a controlled host at all.
func TestRefuseCrossHostRedirect(t *testing.T) {
	next, _ := http.NewRequest("GET", "https://evil.example/other", nil)
	prev, _ := http.NewRequest("GET", "https://api.provider.com/v1/foo", nil)
	if err := refuseCrossHostRedirect(next, []*http.Request{prev}); err == nil {
		t.Fatal("cross-host redirect was allowed")
	}
}

// Same host redirect MUST be allowed (a plain path-only redirect on the
// provider is legitimate).
func TestRefuseCrossHostRedirect_SameHostAllowed(t *testing.T) {
	next, _ := http.NewRequest("GET", "https://api.provider.com/v1/other", nil)
	prev, _ := http.NewRequest("GET", "https://api.provider.com/v1/foo", nil)
	if err := refuseCrossHostRedirect(next, []*http.Request{prev}); err != nil {
		t.Fatalf("same-host redirect refused: %v", err)
	}
}

// The CDN download policy MUST drop every custom header on host change,
// not just a name-heuristic subset. A malicious provider could name a
// secret header anything at all.
func TestScrubOnCrossHostRedirect(t *testing.T) {
	next, _ := http.NewRequest("GET", "https://evil.example/other", nil)
	next.Header.Set("Authorization", "Bearer sk-secret")
	next.Header.Set("Foo", "sk-secret") // arbitrary, no name heuristic would catch this
	next.Header.Set("User-Agent", "antares/1")
	prev, _ := http.NewRequest("GET", "https://cdn.example/asset", nil)
	if err := scrubOnCrossHostRedirect(next, []*http.Request{prev}); err != nil {
		t.Fatal(err)
	}
	if next.Header.Get("Authorization") != "" {
		t.Fatal("Authorization survived scrub")
	}
	if next.Header.Get("Foo") != "" {
		t.Fatal("arbitrary custom header survived scrub")
	}
	if next.Header.Get("User-Agent") != "antares/1" {
		t.Fatal("User-Agent (transport-safe) should have been kept")
	}
}

// checkPublicURL MUST refuse loopback / userinfo / non-http schemes so an
// attacker-controlled URL cannot make us fetch from an internal target or
// smuggle credentials.
func TestCheckPublicURL_RefusesUnsafe(t *testing.T) {
	bad := []string{
		"file:///etc/passwd",
		"ftp://example.com/x",
		"http://user:pass@example.com/",
		"http://127.0.0.1/",
		"http://169.254.169.254/latest/",
	}
	for _, u := range bad {
		parsed, perr := url.Parse(u)
		if perr != nil {
			t.Fatalf("parse %q: %v", u, perr)
		}
		if err := checkPublicURL(parsed); err == nil {
			t.Fatalf("checkPublicURL accepted %q", u)
		}
	}
}

// validateImageBytesAsPNG MUST reject non-image bytes so an HTML error page
// masquerading as an image never lands on disk as active content the
// dashboard would then serve.
func TestValidateImageBytesAsPNG_RejectsHTML(t *testing.T) {
	if _, err := validateImageBytesAsPNG([]byte("<html>oops</html>")); err == nil {
		t.Fatal("validateImageBytesAsPNG accepted HTML")
	}
	if _, err := validateImageBytesAsPNG(nil); err == nil {
		t.Fatal("validateImageBytesAsPNG accepted empty")
	}
}

// A valid PNG MUST round-trip through the validator.
func TestValidateImageBytesAsPNG_RoundTrip(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	out, err := validateImageBytesAsPNG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatal("output is not a PNG")
	}
}
