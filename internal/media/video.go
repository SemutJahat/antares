package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// VideoJob is the trimmed view of a provider video job the workflow needs.
// Status mirrors the provider ("queued"|"in_progress"|"completed"|"failed"),
// Progress is 0..100, and Error is non-empty when the provider reports one.
type VideoJob struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Error    string `json:"error,omitempty"`
}

// videoResponse is the full provider envelope we decode. Only the fields we
// use are named; unknown fields are ignored.
type videoResponse struct {
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Progress float64         `json:"progress"`
	Error    json.RawMessage `json:"error"`
}

// CreateVideo POSTs /videos with the prompt, size, seconds, and an optional
// image reference encoded as data URL under input_reference.image_url. It
// returns the queued VideoJob synchronously; the actual render happens on the
// provider and is polled with GetVideo. Paid call, issued once — no retry.
func CreateVideo(ctx context.Context, ep Endpoint, prompt, size string, seconds int, referencePath string) (VideoJob, error) {
	if strings.TrimSpace(prompt) == "" {
		return VideoJob{}, errors.New("prompt is required")
	}
	if seconds <= 0 {
		seconds = 8
	}
	sizeStr := mustSize(size, "")
	payload := map[string]any{
		"model":  ep.Model,
		"prompt": prompt,
		// The provider expects "seconds" as a string enum ("4"|"8"|"12").
		"seconds": strconv.Itoa(seconds),
	}
	if sizeStr != "" {
		payload["size"] = sizeStr
	}
	if strings.TrimSpace(referencePath) != "" {
		// Sora requires input_reference to match the requested output size,
		// so letterbox the reference to sizeStr before base64-encoding it.
		wantW, wantH := 0, 0
		if sizeStr != "" {
			w, h, err := parseSize(sizeStr)
			if err != nil {
				return VideoJob{}, err
			}
			wantW, wantH = w, h
		}
		u, err := referenceDataURL(referencePath, wantW, wantH)
		if err != nil {
			return VideoJob{}, fmt.Errorf("reference %q: %w", referencePath, err)
		}
		payload["input_reference"] = map[string]any{"image_url": u}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return VideoJob{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.url("/videos"), bytes.NewReader(body))
	if err != nil {
		return VideoJob{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	ep.apply(req.Header)

	resp, err := jsonClient().Do(req)
	if err != nil {
		return VideoJob{}, fmt.Errorf("could not reach the video endpoint: %s", ep.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, readErr := readAllBounded(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return VideoJob{}, fmt.Errorf("the video endpoint returned %s: %s", resp.Status, ep.redact(truncate(string(raw), 400)))
	}
	if readErr != nil {
		return VideoJob{}, fmt.Errorf("could not read the video response: %s", ep.redact(readErr.Error()))
	}
	job, err := decodeVideo(raw)
	job.Error = ep.redact(job.Error)
	return job, err
}

// GetVideo polls /videos/{id}. The provider assigns a numeric progress and a
// discrete status; callers download the content only when Status=="completed".
func GetVideo(ctx context.Context, ep Endpoint, id string) (VideoJob, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return VideoJob{}, errors.New("video id is required")
	}
	seg, err := urlSegment(id)
	if err != nil {
		return VideoJob{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.url("/videos/"+seg), nil)
	if err != nil {
		return VideoJob{}, err
	}
	ep.apply(req.Header)
	resp, err := jsonClient().Do(req)
	if err != nil {
		return VideoJob{}, fmt.Errorf("could not reach the video endpoint: %s", ep.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, readErr := readAllBounded(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return VideoJob{}, fmt.Errorf("the video endpoint returned %s: %s", resp.Status, ep.redact(truncate(string(raw), 400)))
	}
	if readErr != nil {
		return VideoJob{}, fmt.Errorf("could not read the video response: %s", ep.redact(readErr.Error()))
	}
	job, err := decodeVideo(raw)
	job.Error = ep.redact(job.Error)
	return job, err
}

// DownloadVideo streams /videos/{id}/content to output atomically. Uses the
// provider bearer against the provider host; other hosts (e.g. presigned CDN
// URLs) would be reached through downloadURL, which does not forward the
// bearer.
func DownloadVideo(ctx context.Context, ep Endpoint, id, output string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("video id is required")
	}
	if strings.TrimSpace(output) == "" {
		return errors.New("output path is required")
	}
	seg, err := urlSegment(id)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.url("/videos/"+seg+"/content"), nil)
	if err != nil {
		return err
	}
	ep.apply(req.Header)
	resp, err := downloadClient().Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the video endpoint: %s", ep.redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := readAllBounded(resp.Body)
		return fmt.Errorf("video download returned %s: %s", resp.Status, ep.redact(truncate(string(raw), 400)))
	}
	if _, err := atomicStream(output, resp.Body, 0o644); err != nil {
		return err
	}
	return nil
}

// decodeVideo turns the provider envelope into a VideoJob. Errors coming back
// as an object are flattened to "code: message" so the UI has one string.
func decodeVideo(raw []byte) (VideoJob, error) {
	var v videoResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		return VideoJob{}, fmt.Errorf("could not decode the video response: %v", err)
	}
	job := VideoJob{
		ID:       v.ID,
		Status:   v.Status,
		Progress: int(v.Progress),
	}
	if len(v.Error) > 0 && !bytes.Equal(bytes.TrimSpace(v.Error), []byte("null")) {
		// Accept both a plain string and the documented object form.
		var s string
		if err := json.Unmarshal(v.Error, &s); err == nil {
			job.Error = s
		} else {
			var obj struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(v.Error, &obj); err == nil {
				switch {
				case obj.Code != "" && obj.Message != "":
					job.Error = obj.Code + ": " + obj.Message
				case obj.Message != "":
					job.Error = obj.Message
				case obj.Code != "":
					job.Error = obj.Code
				default:
					job.Error = string(v.Error)
				}
			} else {
				job.Error = string(v.Error)
			}
		}
	}
	return job, nil
}

// urlSegment validates and percent-encodes id as a single URL path segment.
// A video id is opaque provider text; we reject empties, dot-only forms, and
// anything containing a slash / query / fragment so a malicious id cannot
// pivot the request onto another endpoint or host.
func urlSegment(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("id is empty")
	}
	if s == "." || s == ".." {
		return "", fmt.Errorf("id %q is not a valid path segment", s)
	}
	if strings.ContainsAny(s, "/?#") {
		return "", fmt.Errorf("id %q contains a path separator", s)
	}
	return url.PathEscape(s), nil
}
