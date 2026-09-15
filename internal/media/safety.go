package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	_ "image/gif"  // decoder registration
	_ "image/jpeg" // decoder registration
)

// maxImagePixels caps the total pixels of any image we accept from a provider
// response or read from disk as a reference. It stops a decoder-bomb (a small
// PNG that claims to be 40000x40000) from allocating gigabytes.
const maxImagePixels = 64_000_000 // ~64 MP, enough for 8K stills

// maxImageEdge is a hard per-side cap; combined with maxImagePixels it stops
// weird aspect ratios burning memory even under the pixel cap.
const maxImageEdge = 12_288

// validateImageBytesAsPNG decodes b, enforces the pixel/edge budget, and
// re-encodes as a PNG. The returned bytes are what should be written to disk
// so a provider that hands us HTML (or an unexpected format) never lands on
// disk as an "image" that a downstream ServeFile would happily render.
func validateImageBytesAsPNG(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("empty image bytes")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("not a decodable image: %w", err)
	}
	if err := checkImageBudget(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("could not decode image: %w", err)
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("could not re-encode as PNG: %w", err)
	}
	return buf.Bytes(), nil
}

// checkImageBudget rejects dimensions that exceed the per-edge or total-pixel
// caps. Callers apply it after DecodeConfig, before any full decode.
func checkImageBudget(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("image has invalid dimensions %dx%d", w, h)
	}
	if w > maxImageEdge || h > maxImageEdge {
		return fmt.Errorf("image %dx%d exceeds per-edge cap %d", w, h, maxImageEdge)
	}
	if int64(w)*int64(h) > int64(maxImagePixels) {
		return fmt.Errorf("image %dx%d exceeds pixel budget %d", w, h, maxImagePixels)
	}
	return nil
}

// checkFileImageBudget opens path just enough to read a header, applies the
// dimension checks, and returns without decoding the pixels. Used before the
// reference letterboxing decodes the file for real.
func checkFileImageBudget(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return fmt.Errorf("not a decodable image: %w", err)
	}
	return checkImageBudget(cfg.Width, cfg.Height)
}

// safeGetURL enforces the URL policy for any hosted asset we download for the
// user (currently only the /images/generations `url` variant):
//
//   - Scheme MUST be http or https.
//   - URL MUST NOT carry userinfo (embedded credentials).
//   - The resolved host MUST NOT be loopback, link-local, unspecified, or in a
//     private / carrier-grade / ULA / benchmarking range. This stops a provider
//     from steering us at an internal service on the operator's box.
//
// The check runs once here AND again inside a custom Transport dialer so a
// redirect to a private target is blocked even if we followed the redirect.
func safeGetURL(ctx context.Context, u string) (*http.Response, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if err := checkPublicURL(parsed); err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: httpDownloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := scrubOnCrossHostRedirect(req, via); err != nil {
				return err
			}
			if err := checkPublicURL(req.URL); err != nil {
				return err
			}
			return nil
		},
		Transport: &safeDialTransport{},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// checkPublicURL enforces scheme, forbids userinfo, and blocks private IPs.
func checkPublicURL(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("only http(s) URLs are allowed, got %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("URL must not contain userinfo")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL missing host")
	}
	// Resolve every A/AAAA and reject if ANY answer is private. That prevents
	// a DNS trick where the first answer looks public but the second is
	// 127.0.0.1 and the actual dialer picks the private one.
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
	if err != nil {
		return fmt.Errorf("could not resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if err := checkPublicIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// checkPublicIP rejects any non-globally-routable address.
func checkPublicIP(ip net.IP) error {
	if ip == nil {
		return errors.New("nil ip")
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
		return fmt.Errorf("refusing to fetch from non-public address %s", ip)
	}
	// Extra ranges Go's net.IP does not classify as "private" but which we
	// still refuse: CGNAT 100.64.0.0/10, IPv4-mapped IPv6, benchmarking, TEST-NET.
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1]&0xC0 == 64: // 100.64.0.0/10
			return fmt.Errorf("refusing to fetch from CGNAT address %s", ip)
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15
			return fmt.Errorf("refusing to fetch from benchmarking address %s", ip)
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 2: // 192.0.2.0/24
			return fmt.Errorf("refusing to fetch from TEST-NET-1 address %s", ip)
		case v4[0] == 198 && v4[1] == 51 && v4[2] == 100: // 198.51.100.0/24
			return fmt.Errorf("refusing to fetch from TEST-NET-2 address %s", ip)
		case v4[0] == 203 && v4[1] == 0 && v4[2] == 113: // 203.0.113.0/24
			return fmt.Errorf("refusing to fetch from TEST-NET-3 address %s", ip)
		}
	}
	return nil
}

// safeDialTransport wraps the default transport so every TCP dial revalidates
// the resolved IP. Belt and braces: checkPublicURL already resolved once, but
// DNS may have rotated between resolution and dial, and a CNAME chain could
// terminate somewhere new by then.
type safeDialTransport struct{}

func (t *safeDialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if err := checkPublicIP(ip); err != nil {
				return nil, err
			}
		}
		// Use the first accepted IP; every candidate already passed the check.
		d := &net.Dialer{}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	return tr.RoundTrip(req)
}

// download streams from resp.Body to output atomically, capped at maxDownload,
// then validates the on-disk bytes as an image and re-encodes as PNG so the
// file that lands on disk is guaranteed a real PNG.
func downloadAndReencodeAsPNG(_ context.Context, resp *http.Response, output string) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	// Buffer to memory (maxResponse cap) then validate; images are small
	// enough that streaming buys nothing here.
	body, err := readAllBounded(resp.Body)
	if err != nil {
		return err
	}
	png, err := validateImageBytesAsPNG(body)
	if err != nil {
		return err
	}
	return atomicWrite(output, png, 0o644)
}

// writeReencodedPNG validates b as an image and writes a re-encoded PNG.
// Its own atomic write step keeps the on-disk file all-or-nothing.
func writeReencodedPNG(b []byte, output string) error {
	png, err := validateImageBytesAsPNG(b)
	if err != nil {
		return err
	}
	return atomicWrite(output, png, 0o644)
}
