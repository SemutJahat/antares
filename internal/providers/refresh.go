// refresh.go maintains a live copy of the models.dev catalogue on top of the
// bundled snapshot. The bundled JSON always answers immediately (offline,
// first-run, corporate firewalls); a background pull tops it up with fresher
// data and stores the result under $XDG_CACHE_HOME/antares/models.json for
// the next process.
//
// The pattern mirrors opencode's model registry: seed from the embedded
// snapshot, load a cached copy from disk if present, then fire a
// non-blocking HTTP fetch. Runtime callers never wait — they read whichever
// table is currently swapped in.

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Env vars operators can use to steer the refresh without editing config.
// Kept as flags rather than YAML fields because they are process-boot
// decisions (airgapped host, corporate proxy, offline dev) that the runtime
// config code path never needs to see.
const (
	envDisableFetch = "ANTARES_DISABLE_MODELS_FETCH"
	envSourceURL    = "ANTARES_MODELS_URL"
	envCachePath    = "ANTARES_MODELS_CACHE"
)

// defaultSourceURL is the models.dev catalogue. Kept in sync with the sync
// script's catalogueURL by convention — this is the runtime pull, that is
// the build-time pull.
const defaultSourceURL = "https://models.dev/api.json"

// refreshInterval bounds how often we re-pull models.dev in a long-lived
// process (the daemon can outlive the browser or TUI it serves). Matches
// opencode's 1h default — models.dev churn is measured in days, not hours,
// so anything shorter is just noise.
const refreshInterval = 1 * time.Hour

// httpTimeout keeps a stuck fetch from pinning the goroutine. Bounded well
// under the refreshInterval so an outage never queues multiple in-flight
// requests.
const httpTimeout = 10 * time.Second

// refreshOnce guards StartRefresh so accidental double-calls from init and
// runtime wiring only fire one goroutine per process.
var refreshOnce sync.Once

// StartRefresh kicks off the background pull, if fetching is enabled. Safe
// to call from any goroutine; only the first call spawns the loop. Idempotent
// so unit tests and cmd/antares wiring can both invoke it without contract.
//
// The caller passes a context that scopes the goroutine's lifetime — pass
// the runtime's root context so the loop exits when the process shuts down.
// A nil context is treated as context.Background() for one-shot uses.
func StartRefresh(ctx context.Context) {
	if os.Getenv(envDisableFetch) == "1" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	refreshOnce.Do(func() {
		go refreshLoop(ctx)
	})
}

// refreshLoop performs the immediate seed-from-disk-and-fetch, then
// re-polls on refreshInterval. Errors are swallowed to stderr — the bundled
// snapshot is always a working fallback, so a failed refresh degrades
// gracefully instead of announcing itself to the operator.
func refreshLoop(ctx context.Context) {
	// Disk cache first — persisted across restarts, so a warm boot on an
	// offline laptop still has whatever the last online session pulled.
	if raw, ok := readDiskCache(); ok {
		if table, err := parseGeneratedModels(raw); err == nil {
			setGeneratedModels(table)
		}
	}

	// Immediate first fetch so a fresh install sees live data within a few
	// seconds of startup rather than waiting a whole interval.
	tryRefresh(ctx)

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tryRefresh(ctx)
		}
	}
}

// tryRefresh performs one models.dev fetch and, on success, swaps the
// in-memory table + persists the payload to disk. Failures leave both
// unchanged and log at info-level via stderr; nothing is fatal because the
// bundled snapshot keeps the process useful.
func tryRefresh(ctx context.Context) {
	raw, err := fetchLive(ctx)
	if err != nil {
		return
	}
	// The live models.dev payload is not shaped like our snapshot — it is
	// keyed by provider, then by model, with a different field layout. Run
	// it through the same extract path as the sync script so the on-disk
	// cache and in-memory table both match the bundled schema.
	table, err := normaliseLive(raw)
	if err != nil {
		return
	}
	setGeneratedModels(table)

	// Persist the normalised bytes, not the raw upstream, so a warm boot
	// takes the fast parseGeneratedModels path without another normalise.
	if blob, err := encodeSnapshot(table); err == nil {
		writeDiskCache(blob)
	}
}

// fetchLive pulls the live models.dev catalogue. Bounded timeout and a
// polite user agent so operators can identify Antares in their access logs.
func fetchLive(ctx context.Context) ([]byte, error) {
	url := os.Getenv(envSourceURL)
	if url == "" {
		url = defaultSourceURL
	}
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "antares/models-refresh")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("models.dev: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap; catalogue is ~1 MiB
}

// normaliseLive converts the raw models.dev api.json shape into our
// snapshot table. The upstream schema evolves; anything we do not recognise
// is dropped rather than propagated as a partial entry that would confuse
// callers walking the cascade.
func normaliseLive(raw []byte) (map[string]ModelMeta, error) {
	var payload map[string]struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Models map[string]struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			ReleaseDate     string `json:"release_date"`
			KnowledgeCutoff string `json:"knowledge"`
			Limit           struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Cost struct {
				Input      float64 `json:"input"`
				Output     float64 `json:"output"`
				CacheRead  float64 `json:"cache_read"`
				CacheWrite float64 `json:"cache_write"`
			} `json:"cost"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
			Reasoning   bool `json:"reasoning"`
			ToolCall    bool `json:"tool_call"`
			OpenWeights bool `json:"open_weights"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	out := make(map[string]ModelMeta, 1024)
	for providerID, prov := range payload {
		if !providerAllowed(providerID) {
			continue
		}
		for modelID, m := range prov.Models {
			id := m.ID
			if id == "" {
				id = modelID
			}
			id = stripPrefix(providerID, id)
			key := providerID + "/" + id
			name := m.Name
			if name == id {
				name = ""
			}
			out[key] = ModelMeta{
				Provider:        providerID,
				ID:              id,
				Name:            name,
				ContextWindow:   m.Limit.Context,
				MaxOutput:       m.Limit.Output,
				Cost:            Cost{Input: m.Cost.Input, Output: m.Cost.Output, CacheRead: m.Cost.CacheRead, CacheWrite: m.Cost.CacheWrite},
				Reasoning:       m.Reasoning,
				ToolCall:        m.ToolCall,
				Attachment:      hasAny(m.Modalities.Input, "image", "pdf", "audio", "video"),
				Vision:          hasAny(m.Modalities.Input, "image"),
				PDF:             hasAny(m.Modalities.Input, "pdf"),
				OpenWeights:     m.OpenWeights,
				KnowledgeCutoff: m.KnowledgeCutoff,
				ReleaseDate:     m.ReleaseDate,
			}
		}
	}
	return out, nil
}

// providerAllowed mirrors scripts/sync-models-dev.go's providersOfInterest.
// Kept in a small helper here so the two don't drift silently: any provider
// the sync script would bundle, the runtime refresh should also accept.
// The list is small; a slice keeps the diff obvious.
func providerAllowed(id string) bool {
	switch id {
	case "anthropic", "openai", "google", "google-vertex", "azure",
		"amazon-bedrock", "openrouter", "opencode", "deepseek", "mistral",
		"xai", "cohere", "groq", "fireworks-ai", "together-ai",
		"perplexity", "cerebras", "sambanova", "meta-llama":
		return true
	}
	return false
}

// stripPrefix drops a redundant "provider/" prefix some entries carry
// (e.g. models.dev sometimes ships "anthropic/claude-…" as the model id).
// The runtime key is provider + "/" + id, so a doubled prefix would miss.
func stripPrefix(provider, id string) string {
	prefix := provider + "/"
	if strings.HasPrefix(id, prefix) {
		return strings.TrimPrefix(id, prefix)
	}
	return id
}

func hasAny(haystack []string, needles ...string) bool {
	for _, h := range haystack {
		for _, n := range needles {
			if h == n {
				return true
			}
		}
	}
	return false
}

// encodeSnapshot writes the current table in the bundled snapshot's shape,
// so writeDiskCache / parseGeneratedModels can round-trip through the same
// envelope the //go:embed asset uses.
func encodeSnapshot(table map[string]ModelMeta) ([]byte, error) {
	env := generatedModelsEnvelope{Models: table}
	return json.Marshal(env)
}

// cachePath resolves the on-disk cache location. Env override wins; otherwise
// $XDG_CACHE_HOME/antares/models.json (Linux/BSD) or the OS-native cache dir.
// A blank return means the platform did not surface a cache dir — in that
// case we skip persistence rather than sprinkle files in $HOME.
func cachePath() string {
	if p := strings.TrimSpace(os.Getenv(envCachePath)); p != "" {
		return p
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "antares", "models.json")
}

func readDiskCache() ([]byte, bool) {
	p := cachePath()
	if p == "" {
		return nil, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Corrupt / permission failure — remove so a next successful
			// fetch can rewrite cleanly instead of tripping over the same
			// bad bytes every startup.
			_ = os.Remove(p)
		}
		return nil, false
	}
	return raw, true
}

func writeDiskCache(raw []byte) {
	p := cachePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	// Atomic write via temp file + rename so a killed process never leaves
	// a truncated cache the next startup would panic on.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
