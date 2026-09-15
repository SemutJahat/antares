package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// GenerateImage produces a single PNG at output. When references is empty it
// calls POST /images/generations; when non-empty it calls POST /images/edits
// with images:[{image_url:dataURL}]. All non-URL fetches are bounded and the
// output is written atomically. Errors are redacted of the API key.
//
// This is a paid POST: it is issued once, exactly. No automatic retry, and no
// hidden fallback to a different endpoint on 4xx.
func GenerateImage(ctx context.Context, ep Endpoint, prompt, size string, references []string, output string) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("prompt is required")
	}
	if strings.TrimSpace(output) == "" {
		return errors.New("output path is required")
	}

	size = mustSize(size, "1024x1024")

	// Build the reference payload once; a bad reference short-circuits before
	// any network call so we never spend money on garbage input.
	var refs []map[string]any
	for _, p := range references {
		if strings.TrimSpace(p) == "" {
			continue
		}
		u, err := dataURL(p)
		if err != nil {
			return fmt.Errorf("reference %q: %w", p, err)
		}
		refs = append(refs, map[string]any{"image_url": u})
	}

	var (
		endpoint string
		payload  map[string]any
	)
	if len(refs) == 0 {
		endpoint = ep.url("/images/generations")
		payload = map[string]any{
			"model":  ep.Model,
			"prompt": prompt,
			"size":   size,
			"n":      1,
		}
	} else {
		endpoint = ep.url("/images/edits")
		payload = map[string]any{
			"model":         ep.Model,
			"prompt":        prompt,
			"size":          size,
			"n":             1,
			"images":        refs,
			"output_format": "png",
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	ep.apply(req.Header)

	resp, err := jsonClient().Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the image endpoint: %s", ep.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, readErr := readAllBounded(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("the image endpoint returned %s: %s", resp.Status, ep.redact(truncate(string(raw), 400)))
	}
	if readErr != nil {
		return fmt.Errorf("could not read the image response: %s", ep.redact(readErr.Error()))
	}

	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 {
		return errors.New("the image endpoint returned nothing usable")
	}
	first := out.Data[0]
	switch {
	case first.B64 != "":
		img, err := base64.StdEncoding.DecodeString(first.B64)
		if err != nil {
			return fmt.Errorf("could not decode the image: %v", err)
		}
		// The provider claims PNG, but we validate + re-encode so a garbage
		// payload can never land on disk as a passable image the dashboard
		// would serve as active content.
		return writeReencodedPNG(img, output)
	case first.URL != "":
		// Provider-hosted URL: fetch through the safe downloader (public
		// destination only, no bearer forwarded), then validate + re-encode.
		resp, err := safeGetURL(ctx, first.URL)
		if err != nil {
			return fmt.Errorf("could not download the image: %s", ep.redact(err.Error()))
		}
		return downloadAndReencodeAsPNG(ctx, resp, output)
	}
	return errors.New("the image endpoint returned neither data nor a url")
}

// truncate caps s at n runes and marks the elision. Used only for error text.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// n is a character budget; over-run only reduces the tail we show.
	return s[:n] + "…"
}
