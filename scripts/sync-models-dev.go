// sync-models-dev fetches the models.dev community catalogue and generates
// internal/providers/generated_models.go: a lookup table Antares uses to
// enrich provider /models responses with context windows, pricing, and
// capability flags the provider APIs themselves rarely return.
//
// Run it with `make sync-models` (or `go run scripts/sync-models-dev.go`).
// Output is deterministic — providers and models are emitted in sorted order
// so re-runs against an unchanged upstream produce a zero-diff file.
//
// Source: https://models.dev/api.json — community-curated, provider-agnostic.
// Licence: models.dev is MIT-licensed. The generated file bundles a snapshot;
// re-sync periodically to pick up new models. The manual overrides in
// internal/providers/catalog.go (contextWindows) remain the escape hatch for
// entries where upstream is missing or wrong.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/enowdev/antares/internal/providers"
)

const (
	catalogueURL = "https://models.dev/api.json"
	outputPath   = "internal/providers/models_generated.json"
)

// providersOfInterest selects which models.dev provider blocks land in the
// generated file. The set is the shared runtime allowlist exported from
// internal/providers — any provider the runtime refresh keeps, the bundled
// snapshot MUST include, or a warm start pulls entries the runtime would
// then discard on the next live refresh.
//
// To add a provider: edit allowedProviders in internal/providers/refresh.go.
// Both this generator and the runtime refresh pick it up automatically.
var providersOfInterest = providers.AllowedProviders()

// apiPayload mirrors the pieces of models.dev/api.json this generator reads.
// Anything not named here is discarded — the catalogue carries a lot of fields
// (benchmarks, marketing copy, temperature ladders) that Antares does not use.
type apiPayload map[string]providerPayload

type providerPayload struct {
	ID     string                  `json:"id"`
	Name   string                  `json:"name"`
	Models map[string]modelPayload `json:"models"`
}

type modelPayload struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Family      string        `json:"family"`
	Attachment  bool          `json:"attachment"`
	Reasoning   bool          `json:"reasoning"`
	ToolCall    bool          `json:"tool_call"`
	Knowledge   string        `json:"knowledge"`
	ReleaseDate string        `json:"release_date"`
	LastUpdated string        `json:"last_updated"`
	OpenWeights bool          `json:"open_weights"`
	Modalities  modalityBlock `json:"modalities"`
	Limit       limitBlock    `json:"limit"`
	Cost        costBlock     `json:"cost"`
}

type modalityBlock struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type limitBlock struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type costBlock struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sync-models-dev:", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Fprintln(os.Stderr, "fetching", catalogueURL)
	payload, fetchedAt, err := fetch()
	if err != nil {
		return err
	}

	entries := extract(payload)
	fmt.Fprintf(os.Stderr, "collected %d models across %d providers\n",
		len(entries), countProviders(entries))

	blob, err := renderJSON(entries, fetchedAt)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outputPath, blob, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outputPath, err)
	}
	fmt.Fprintln(os.Stderr, "wrote", outputPath)
	return nil
}

func fetch() (apiPayload, time.Time, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, catalogueURL, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("User-Agent", "antares-sync-models-dev/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, catalogueURL)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, time.Time{}, err
	}
	var payload apiPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, time.Time{}, fmt.Errorf("decode api.json: %w", err)
	}
	return payload, time.Now().UTC(), nil
}

// entry is the flattened shape emitted to Go source. Key = "provider/model".
type entry struct {
	ProviderID     string
	ModelID        string
	Name           string
	Family         string
	ContextWindow  int
	MaxOutput      int
	CostInput      float64
	CostOutput     float64
	CostCacheRead  float64
	CostCacheWrite float64
	Reasoning      bool
	ToolCall       bool
	Attachment     bool
	Vision         bool
	PDF            bool
	OpenWeights    bool
	Knowledge      string
	ReleaseDate    string
}

func extract(payload apiPayload) []entry {
	var out []entry
	for providerID, provider := range payload {
		if !providersOfInterest[providerID] {
			continue
		}
		for modelID, m := range provider.Models {
			out = append(out, entry{
				ProviderID:     providerID,
				ModelID:        stripProviderPrefix(providerID, modelID),
				Name:           firstNonEmpty(m.Name, modelID),
				Family:         m.Family,
				ContextWindow:  m.Limit.Context,
				MaxOutput:      m.Limit.Output,
				CostInput:      m.Cost.Input,
				CostOutput:     m.Cost.Output,
				CostCacheRead:  m.Cost.CacheRead,
				CostCacheWrite: m.Cost.CacheWrite,
				Reasoning:      m.Reasoning,
				ToolCall:       m.ToolCall,
				Attachment:     m.Attachment,
				Vision:         hasModality(m.Modalities.Input, "image"),
				PDF:            hasModality(m.Modalities.Input, "pdf"),
				OpenWeights:    m.OpenWeights,
				Knowledge:      m.Knowledge,
				ReleaseDate:    m.ReleaseDate,
			})
		}
	}
	// Deterministic order — provider first, then model id.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].ModelID < out[j].ModelID
	})
	return out
}

// stripProviderPrefix normalises `openrouter/anthropic/claude-...` etc. down
// to the id a caller looks up. models.dev sometimes prefixes ids with the
// provider (e.g. `subconscious/glm-5.2`); we index on the trailing model id.
func stripProviderPrefix(provider, id string) string {
	if p := provider + "/"; strings.HasPrefix(id, p) {
		return strings.TrimPrefix(id, p)
	}
	return id
}

func countProviders(entries []entry) int {
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.ProviderID] = true
	}
	return len(seen)
}

func hasModality(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// renderJSON emits a deterministic JSON object keyed by "provider/model-id"
// carrying models.dev's snapshot fields. The Go embed at
// internal/providers/models_generated.go unmarshals this file at init time.
// A JSON asset keeps the review diff small: 300+ KB of metadata as data,
// not as Go source literals git ends up parsing every rebuild.
func renderJSON(entries []entry, fetchedAt time.Time) ([]byte, error) {
	// A slice of key+value pairs keeps output order deterministic without
	// having to worry about map iteration; the extract step already sorts by
	// (provider, model), so the JSON diff between regenerations is minimal.
	// The row shape and its tags must match providers.ModelMeta's json tags —
	// the runtime consumer unmarshals straight into ModelMeta, so a rename
	// here needs a matching rename there or a field silently reads as zero.
	type cost struct {
		Input      float64 `json:"input,omitempty"`
		Output     float64 `json:"output,omitempty"`
		CacheRead  float64 `json:"cache_read,omitempty"`
		CacheWrite float64 `json:"cache_write,omitempty"`
	}
	type row struct {
		Provider        string `json:"provider"`
		ID              string `json:"id"`
		Name            string `json:"name,omitempty"`
		Family          string `json:"family,omitempty"`
		ContextWindow   int    `json:"context_window,omitempty"`
		MaxOutput       int    `json:"max_output,omitempty"`
		Cost            *cost  `json:"cost,omitempty"`
		Reasoning       bool   `json:"reasoning,omitempty"`
		ToolCall        bool   `json:"tool_call,omitempty"`
		Attachment      bool   `json:"attachment,omitempty"`
		Vision          bool   `json:"vision,omitempty"`
		PDF             bool   `json:"pdf,omitempty"`
		OpenWeights     bool   `json:"open_weights,omitempty"`
		KnowledgeCutoff string `json:"knowledge_cutoff,omitempty"`
		ReleaseDate     string `json:"release_date,omitempty"`
	}
	type envelope struct {
		Source    string         `json:"_source"`
		FetchedAt string         `json:"_fetched_at"`
		Models    map[string]row `json:"models"`
	}

	models := make(map[string]row, len(entries))
	for _, e := range entries {
		key := e.ProviderID + "/" + e.ModelID
		name := e.Name
		if name == e.ModelID {
			name = "" // redundant with the id; omitempty drops it
		}
		var c *cost
		if e.CostInput > 0 || e.CostOutput > 0 || e.CostCacheRead > 0 || e.CostCacheWrite > 0 {
			c = &cost{
				Input:      e.CostInput,
				Output:     e.CostOutput,
				CacheRead:  e.CostCacheRead,
				CacheWrite: e.CostCacheWrite,
			}
		}
		models[key] = row{
			Provider:        e.ProviderID,
			ID:              e.ModelID,
			Name:            name,
			Family:          e.Family,
			ContextWindow:   e.ContextWindow,
			MaxOutput:       e.MaxOutput,
			Cost:            c,
			Reasoning:       e.Reasoning,
			ToolCall:        e.ToolCall,
			Attachment:      e.Attachment,
			Vision:          e.Vision,
			PDF:             e.PDF,
			OpenWeights:     e.OpenWeights,
			KnowledgeCutoff: e.Knowledge,
			ReleaseDate:     e.ReleaseDate,
		}
	}
	env := envelope{
		Source:    catalogueURL,
		FetchedAt: fetchedAt.Format(time.RFC3339),
		Models:    models,
	}
	// Compact form: this file is a bundled build-time snapshot, not a
	// document humans read line-by-line. Compact keeps the repo diff to a
	// single line so `make sync-models` runs don't churn thousands of
	// lines in every PR. Runtime parse cost is identical either way
	// (a few ms for 900+ entries), and reviewers who want to inspect
	// individual entries can pipe through `jq`.
	blob, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal generated models: %w", err)
	}
	return append(blob, '\n'), nil
}
