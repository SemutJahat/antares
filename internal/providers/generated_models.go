// Package providers — see metadata.go for the type + cascade.
//
// This file wires the bundled models.dev snapshot (models_generated.json)
// into a lookup table used by Meta() and MetaByProvider(). The bundled
// snapshot is the offline-first fallback; refresh.go replaces it in place
// with a fresh models.dev pull once the process has network.
//
// Rationale for a JSON asset + //go:embed rather than a generated .go file
// full of struct literals: the snapshot is data, not code. Bundling it as a
// literal source file bloats every review with thousands of lines of key/
// value diffs whenever `make sync-models` runs. As a compact JSON asset the
// diff stays in one file GitHub already knows how to collapse, git history
// stays lean, and the runtime cost (one json.Unmarshal at process start) is
// trivial for ~1k entries.

package providers

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync/atomic"
)

//go:embed models_generated.json
var generatedModelsRaw []byte

// generatedTable holds the current snapshot behind an atomic.Pointer so a
// background refresh can hot-swap it without racing readers. The Meta()
// callers read millions of times per session; the write path fires once at
// startup and again every refreshInterval, so lock-free reads are worth the
// tiny allocation each refresh.
var generatedTable atomic.Pointer[map[string]ModelMeta]

func init() {
	initial := mustLoadGeneratedModels(generatedModelsRaw)
	generatedTable.Store(&initial)
}

// generatedModels returns the current snapshot. Callers get a value copy of
// the map header — the underlying map is never mutated in place, only
// replaced wholesale, so the returned reference stays consistent for the
// duration of one lookup even if a refresh lands mid-call.
func generatedModels() map[string]ModelMeta {
	p := generatedTable.Load()
	if p == nil {
		return nil
	}
	return *p
}

// setGeneratedModels swaps in a fresh snapshot. Called by the refresh
// goroutine in refresh.go once a live models.dev pull lands, and by tests
// that need a deterministic table.
func setGeneratedModels(m map[string]ModelMeta) {
	generatedTable.Store(&m)
}

// generatedModelsEnvelope matches the shape written by scripts/sync-models-dev.go.
// Only the models map is loaded into memory — the _source/_fetched_at fields
// stay in the file for humans reading the diff and are not needed at runtime.
type generatedModelsEnvelope struct {
	Models map[string]ModelMeta `json:"models"`
}

// mustLoadGeneratedModels parses the embedded snapshot at init time. A panic
// here would only fire on a malformed regeneration — a bug in the sync script
// or a hand-edit of the JSON — which is caught immediately by `go test`,
// never by an end user. Returning an empty map on decode failure would hide
// the bug behind silently missing metadata, which is worse.
func mustLoadGeneratedModels(raw []byte) map[string]ModelMeta {
	if len(raw) == 0 {
		return map[string]ModelMeta{}
	}
	var env generatedModelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		panic(fmt.Errorf("providers: parse embedded models_generated.json: %w", err))
	}
	if env.Models == nil {
		return map[string]ModelMeta{}
	}
	return env.Models
}

// parseGeneratedModels is the non-panicking sibling used by the disk cache
// and live-fetch paths where a corrupted payload should be an error (log +
// keep prior snapshot), not a process kill.
func parseGeneratedModels(raw []byte) (map[string]ModelMeta, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	var env generatedModelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Models == nil {
		return map[string]ModelMeta{}, nil
	}
	return env.Models, nil
}
