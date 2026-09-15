package media

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxResponse caps any provider response body we buffer in memory. Large
// generated assets are streamed to disk with maxDownload instead.
const maxResponse = 64 << 20 // 64 MiB

// maxDownload caps a streamed download to prevent unbounded disk fill.
const maxDownload = 512 << 20 // 512 MiB

// httpJSONTimeout is a generous ceiling for provider POST/GET calls.
var httpJSONTimeout = 180 * time.Second

// httpDownloadTimeout bounds a streamed asset download.
var httpDownloadTimeout = 30 * time.Minute

// errResponseTooLarge is returned when a provider response exceeds the cap.
// Callers surface it verbatim so the operator sees a size cap, not silent
// truncation followed by a JSON decode failure.
var errResponseTooLarge = errors.New("provider response exceeded 64 MiB cap")

// errDownloadTooLarge is returned when a streamed download exceeds the cap.
var errDownloadTooLarge = fmt.Errorf("download exceeded %d bytes", int64(maxDownload))

// refuseCrossHostRedirect is the redirect policy for control-plane and
// content-download clients that carry the provider bearer plus any custom
// Endpoint.Headers. Rather than try to enumerate which of the current
// request headers are "sensitive" (a provider could name a secret header
// anything at all), we refuse cross-host redirects outright. Same-host
// redirects still work — a provider that redirects /videos → /videos/ is
// fine; a provider that redirects to a CDN would need a two-step flow.
func refuseCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	prev := via[len(via)-1]
	if !strings.EqualFold(prev.URL.Host, req.URL.Host) {
		return fmt.Errorf("refusing cross-host redirect from %s to %s (would leak credentials)",
			prev.URL.Host, req.URL.Host)
	}
	return nil
}

// scrubOnCrossHostRedirect is the redirect policy for the anonymous CDN
// downloader (safeGetURL) which does NOT send Authorization or any
// Endpoint.Headers to begin with. It exists as belt-and-braces: strip
// every single custom header on a host change so an intermediary that
// injected one cannot make it follow. Public-IP re-check is added on top
// by safeGetURL's own policy composition.
func scrubOnCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	prev := via[len(via)-1]
	if strings.EqualFold(prev.URL.Host, req.URL.Host) {
		return nil
	}
	// Strip EVERY header the caller set, not just a heuristic subset. Keep
	// only what the transport itself needs on the redirected request.
	for h := range req.Header {
		lo := strings.ToLower(h)
		if lo == "user-agent" || lo == "accept" || lo == "accept-encoding" {
			continue
		}
		req.Header.Del(h)
	}
	return nil
}

// jsonClient is used for control-plane calls (POST /videos, GET /videos/:id,
// /images/*) that carry the provider bearer. Cross-host redirects refused.
func jsonClient() *http.Client {
	return &http.Client{Timeout: httpJSONTimeout, CheckRedirect: refuseCrossHostRedirect}
}

// downloadClient is used to stream /videos/:id/content with the provider
// bearer. Cross-host redirects refused for the same reason as jsonClient.
func downloadClient() *http.Client {
	return &http.Client{Timeout: httpDownloadTimeout, CheckRedirect: refuseCrossHostRedirect}
}

// readAllBounded reads up to maxResponse bytes. If the body is larger it
// returns errResponseTooLarge with what was read so far, so callers can log
// a bounded snippet rather than silently truncate + fail decoding.
func readAllBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxResponse+1))
	if err != nil {
		return b, err
	}
	if len(b) > maxResponse {
		return b[:maxResponse], errResponseTooLarge
	}
	return b, nil
}

// atomicWrite creates path via a same-directory temp file + rename so a
// concurrent reader or a mid-write crash cannot observe a partial file.
// perm is applied before rename.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return errors.New("no output path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".media-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any error path.
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// atomicStream copies from r to path via a same-directory temp file + rename,
// capping the total bytes at maxDownload.
func atomicStream(path string, r io.Reader, perm os.FileMode) (int64, error) {
	if path == "" {
		return 0, errors.New("no output path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".media-*.tmp")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(r, maxDownload+1))
	if err != nil {
		tmp.Close()
		return n, err
	}
	if n > maxDownload {
		tmp.Close()
		return n, errDownloadTooLarge
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return n, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return n, err
	}
	return n, nil
}

// dataURL loads a local image file and returns a data:image/<ext>;base64,...
// string suitable for OpenAI-style image_url fields. It refuses unreadable
// paths, non-regular files, and unknown extensions so a garbage reference
// never becomes a paid POST.
func dataURL(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("empty reference path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("reference must not be a symlink: %s", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("reference is not a regular file: %s", path)
	}
	if info.Size() > maxResponse {
		return "", fmt.Errorf("reference %s is larger than %d bytes", path, int64(maxResponse))
	}
	mime := mimeFor(abs)
	if mime == "" {
		return "", fmt.Errorf("unsupported reference type: %s", filepath.Ext(abs))
	}
	// Enforce pixel budget on the reference itself so a 40000x40000 PNG
	// cannot exhaust memory in a downstream decoder. Skip the pixel check
	// for webp (stdlib has no decoder without x/image); its file-size cap
	// still bounds it.
	if strings.ToLower(filepath.Ext(abs)) != ".webp" {
		if err := checkFileImageBudget(abs); err != nil {
			return "", fmt.Errorf("reference %s: %w", path, err)
		}
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b), nil
}

// mimeFor maps a file extension to an image MIME. Empty when not a supported
// image type, which callers treat as a reject.
func mimeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	}
	return ""
}
