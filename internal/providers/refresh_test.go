package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestTryRefreshKeepsPriorTableWhenLiveEmpty exercises the empty-result
// guard added to tryRefresh: a successful HTTP fetch that yields no
// supported entries (upstream returned {}, or every provider block sits
// outside allowedProviders) must leave both the in-memory table and the
// disk cache untouched. Without the guard the runtime silently blanks
// the bundled snapshot and every subsequent lookup misses.
func TestTryRefreshKeepsPriorTableWhenLiveEmpty(t *testing.T) {
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "models.json")
	t.Setenv(envCachePath, cachePath)

	// A canary the guard must protect; wraps a well-known provider so the
	// snapshot survives the swap.
	prior := map[string]ModelMeta{
		"anthropic/claude-sonnet-4-6": {
			Provider:      "anthropic",
			ID:            "claude-sonnet-4-6",
			ContextWindow: 200_000,
		},
	}
	restore := swapTable(t, prior)
	defer restore()

	// Pre-populate the disk cache so we can prove tryRefresh does not
	// overwrite it either.
	priorBlob, err := json.Marshal(generatedModelsEnvelope{Models: prior})
	if err != nil {
		t.Fatalf("marshal prior: %v", err)
	}
	if err := os.WriteFile(cachePath, priorBlob, 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	cases := []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		// Every provider in the payload is outside the allowlist — the
		// normalised table is empty even though the fetch "succeeded".
		{"only unknown providers", `{"totally-fake-provider":{"models":{"foo":{"id":"foo"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			t.Setenv(envSourceURL, srv.URL)

			tryRefresh(context.Background())

			// In-memory table survives.
			if _, ok := generatedModels()["anthropic/claude-sonnet-4-6"]; !ok {
				t.Fatalf("prior table clobbered: canary entry gone")
			}
			// Disk cache survives.
			got, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatalf("read cache: %v", err)
			}
			if string(got) != string(priorBlob) {
				t.Errorf("cache overwritten with empty payload: got %s", string(got))
			}
		})
	}
}

// TestAllowedProvidersCoversBundledSnapshot proves the shared allowlist and
// the bundled JSON agree: every provider present in the snapshot is one the
// runtime refresh would also keep, and every allowlisted provider that is
// not purely optional (ollama has no models to catalogue) actually appears.
// Guards against the previous drift where the runtime allowlist silently
// dropped zai/alibaba/kimi-for-coding/… on live refresh.
func TestAllowedProvidersCoversBundledSnapshot(t *testing.T) {
	allowed := AllowedProviders()

	// Copy defensively; nothing external should share state with the
	// runtime table.
	allowed["totally-not-real"] = true
	if allowedProviders["totally-not-real"] {
		t.Fatalf("AllowedProviders returned a live map — mutation leaked back into the runtime table")
	}
	allowed = AllowedProviders()

	seen := map[string]bool{}
	for key := range generatedModels() {
		// key = "provider/id"; take the first path segment.
		for i := 0; i < len(key); i++ {
			if key[i] == '/' {
				seen[key[:i]] = true
				break
			}
		}
	}
	for provider := range seen {
		if !allowed[provider] {
			t.Errorf("bundled snapshot ships %q but runtime allowlist would drop it on next refresh", provider)
		}
	}
	// Sanity: the specific providers the drift previously omitted at
	// runtime must now be present.
	for _, want := range []string{
		"zai", "alibaba", "kimi-for-coding", "minimax",
		"github-copilot", "nvidia", "togetherai",
	} {
		if !allowed[want] {
			t.Errorf("shared allowlist missing %q — runtime/generator drift regressed", want)
		}
	}
}

// TestNormaliseLiveVisionOnlyModality confirms normaliseLive keeps Vision
// separate from Attachment: an audio-only model must not be marked
// vision-capable just because it lists an input modality. The bug this
// guards let the picker feed image bytes to speech models.
func TestNormaliseLiveVisionOnlyModality(t *testing.T) {
	raw := []byte(`{
	  "openai": {
	    "id": "openai",
	    "name": "OpenAI",
	    "models": {
	      "gpt-audio-only": {
	        "id": "gpt-audio-only",
	        "modalities": {"input": ["text", "audio"], "output": ["text"]}
	      },
	      "gpt-image": {
	        "id": "gpt-image",
	        "modalities": {"input": ["text", "image"], "output": ["text"]}
	      }
	    }
	  }
	}`)
	table, err := normaliseLive(raw)
	if err != nil {
		t.Fatalf("normaliseLive: %v", err)
	}
	audio, ok := table["openai/gpt-audio-only"]
	if !ok {
		t.Fatalf("expected openai/gpt-audio-only in normalised table")
	}
	if audio.Vision {
		t.Errorf("audio-only model marked Vision=true — modality bleed regressed")
	}
	if !audio.Attachment {
		t.Errorf("audio modality should still count as an attachment surface")
	}
	image, ok := table["openai/gpt-image"]
	if !ok {
		t.Fatalf("expected openai/gpt-image in normalised table")
	}
	if !image.Vision {
		t.Errorf("image-input model should be Vision=true")
	}
}

// swapTable installs a controlled generated-models table for the duration
// of a test and returns a restore func the caller defers. Kept local so
// production code has no test-only hooks.
func swapTable(t *testing.T, replacement map[string]ModelMeta) func() {
	t.Helper()
	prior := generatedModels()
	setGeneratedModels(replacement)
	return func() { setGeneratedModels(prior) }
}
