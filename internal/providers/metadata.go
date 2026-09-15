package providers

import "strings"

// ModelMeta describes a single model — context window, pricing, capability
// flags, and identity — as far as Antares needs to know. It is the shape the
// generated models.dev snapshot uses and the shape Meta() returns to callers.
//
// Zero-values mean "unknown" — callers walking the cascade fall through to the
// next layer instead of trusting a fabricated 0-token window or a $0 price.
type ModelMeta struct {
	Provider        string `json:"provider,omitempty"`
	ID              string `json:"id,omitempty"`
	Name            string `json:"name,omitempty"`
	Family          string `json:"family,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutput       int    `json:"max_output,omitempty"`
	Cost            Cost   `json:"cost,omitempty"`
	Reasoning       bool   `json:"reasoning,omitempty"`
	ToolCall        bool   `json:"tool_call,omitempty"`
	Attachment      bool   `json:"attachment,omitempty"`
	Vision          bool   `json:"vision,omitempty"`
	PDF             bool   `json:"pdf,omitempty"`
	OpenWeights     bool   `json:"open_weights,omitempty"`
	KnowledgeCutoff string `json:"knowledge_cutoff,omitempty"`
	ReleaseDate     string `json:"release_date,omitempty"`
	// Source records which cascade layer answered — useful in debug logs and
	// the dashboard so operators can tell whether metadata came from their
	// own config, the bundled models.dev snapshot, a hand-curated escape
	// hatch, or fell through to zero. Values: "user", "generated",
	// "curated", "" (miss). Not persisted in the bundled snapshot; the
	// caller stamps it after lookup.
	Source string `json:"-"`
}

// Cost is per 1M-token pricing in USD, matching the models.dev unit.
type Cost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// HasCost reports whether the entry carries any pricing data at all — used to
// decide whether the dashboard should draw a "$3/M in" chip or hide the field.
func (c Cost) HasCost() bool {
	return c.Input > 0 || c.Output > 0 || c.CacheRead > 0 || c.CacheWrite > 0
}

// Meta looks up model metadata by bare id (e.g. "claude-sonnet-4-6") without a
// caller-supplied provider hint. It walks the cascade in the same order as
// MetaByProvider and picks the first generated-table hit across every known
// provider — good enough for the agent's contextWindowFor() which only knows
// the model name, not always which provider it belongs to.
//
// Prefer MetaByProvider when the caller already knows the provider, because
// the same model id can appear under multiple provider prefixes
// (e.g. "claude-opus" via anthropic and openrouter) with different pricing.
func Meta(model string) (ModelMeta, bool) {
	if model == "" {
		return ModelMeta{}, false
	}
	// Fast path: id carries a provider prefix (openrouter's "anthropic/…").
	if provider, id, ok := strings.Cut(model, "/"); ok {
		if m, hit := MetaByProvider(provider, id); hit {
			return m, true
		}
	}
	// Slow path: scan the generated table for any provider that lists this id.
	if m, ok := lookupBareID(model); ok {
		return m, true
	}
	if w, ok := legacyContextWindow(model); ok {
		return ModelMeta{ID: model, ContextWindow: w, Source: "curated"}, true
	}
	return ModelMeta{}, false
}

// MetaByProvider looks up metadata for a specific provider+model pair,
// walking the full cascade:
//
//  1. Hand-curated overrides (legacyContextWindow) — an escape hatch for
//     entries where models.dev is missing or upstream is wrong. Kept small.
//  2. Generated snapshot from models.dev, exact key "provider/id" first,
//     then the same key with dots/dashes normalised (models.dev uses "4-6",
//     Anthropic's own API uses "4.7" — same model, different spelling).
//  3. Bare-id lookup across every provider — this is the escape hatch for
//     custom proxies (e.g. enxapi) that re-serve official models under
//     their own provider id. If exactly one entry in the table has that
//     bare id, use it; if several do, prefer any anthropic/openai/google
//     entry over noisy re-listings.
//  4. Miss — the caller falls through to whatever default it has.
//
// Note: layer 0 ("user override" via cfg.Providers[…].ModelMeta) is applied
// by higher-level callers that already hold *config.Config; the pure
// providers package stays free of runtime config to keep it importable from
// anywhere.
func MetaByProvider(provider, model string) (ModelMeta, bool) {
	if model == "" {
		return ModelMeta{}, false
	}
	// Layer 1: hand-curated override — highest priority within this package
	// so we can correct a models.dev entry without regenerating.
	if w, ok := legacyContextWindow(model); ok {
		return ModelMeta{Provider: provider, ID: model, ContextWindow: w, Source: "curated"}, true
	}
	// Layer 2: strict provider/id lookup. Try the exact key first, then the
	// same key with dot/dash slug variants (Anthropic ships "claude-opus-4.7"
	// on the wire; models.dev catalogues it as "claude-opus-4-7"). No
	// cross-provider fallback here — a first-party provider that does not
	// carry the id is authoritative "does not have this model", and pinning
	// to some other provider's re-listing would attribute the wrong
	// context/cost to a model the caller specifically asked about.
	if provider == "" {
		return ModelMeta{}, false
	}
	// Hoist the snapshot once so a mid-lookup refresh cannot make the two
	// keyed reads see different tables.
	table := generatedModels()
	if m, ok := table[provider+"/"+model]; ok {
		m.Source = "generated"
		return m, true
	}
	for _, variant := range slugVariants(model) {
		if m, ok := table[provider+"/"+variant]; ok {
			m.Source = "generated"
			return m, true
		}
	}
	return ModelMeta{}, false
}

// MetaByAnyProvider looks up metadata by bare id across every provider in the
// generated table. Used only by callers that know their provider is a proxy
// re-serving official models (e.g. an OpenAI-compatible endpoint like EnxAPI
// that fronts Anthropic, OpenAI, Google under their real ids). Callers that
// know a specific provider should use MetaByProvider — it refuses to pin the
// wrong entry when the provider genuinely does not carry the model.
//
// Preference order among candidates: first-party providers
// (anthropic > openai > google > well-known labs) win over noisy re-listings
// so "gpt-5" resolves to openai's numbers rather than some proxy's markup.
func MetaByAnyProvider(model string) (ModelMeta, bool) {
	if model == "" {
		return ModelMeta{}, false
	}
	if w, ok := legacyContextWindow(model); ok {
		return ModelMeta{ID: model, ContextWindow: w, Source: "curated"}, true
	}
	return lookupBareID(model)
}
// lookupBareID scans the generated table for any entry whose bare model id
// matches, with slug normalisation. Prefers first-party providers (anthropic,
// openai, google) when several proxies re-list the same model so a lookup for
// "claude-opus-4.7" doesn't accidentally pin to github-copilot's re-listing.
func lookupBareID(model string) (ModelMeta, bool) {
	variants := append([]string{model}, slugVariants(model)...)
	var best ModelMeta
	var found bool
	for _, m := range generatedModels() {
		if !containsAny(variants, m.ID) {
			continue
		}
		m.Source = "generated"
		if !found {
			best = m
			found = true
			continue
		}
		if firstPartyRank(m.Provider) < firstPartyRank(best.Provider) {
			best = m
		}
	}
	return best, found
}

// slugVariants returns the model id with common separator flips. Anthropic's
// own API uses dots ("claude-opus-4.7"); models.dev uses dashes
// ("claude-opus-4-6"). Return both spellings so a lookup succeeds regardless
// of which convention the caller passed in. Also handles the reverse.
func slugVariants(model string) []string {
	var out []string
	if strings.ContainsRune(model, '.') {
		out = append(out, strings.ReplaceAll(model, ".", "-"))
	}
	// Reverse: some model ids in the table use dots — unusual but safe.
	if strings.ContainsRune(model, '-') {
		// Only flip the trailing version segment to a dot; blindly replacing
		// every dash breaks "claude-opus" itself. Grab the last two segments
		// and see if they look like a version pair ("4-7" -> "4.7").
		parts := strings.Split(model, "-")
		if n := len(parts); n >= 2 {
			if _, err := parseUint(parts[n-1]); err == nil {
				if _, err := parseUint(parts[n-2]); err == nil {
					joined := strings.Join(parts[:n-2], "-") + "-" + parts[n-2] + "." + parts[n-1]
					out = append(out, joined)
				}
			}
		}
	}
	return out
}

// firstPartyRank returns a lower number for first-party providers so
// lookupBareID prefers them over re-listings. Unknown providers rank last.
func firstPartyRank(provider string) int {
	switch provider {
	case "anthropic":
		return 0
	case "openai":
		return 1
	case "google":
		return 2
	case "xai", "deepseek", "mistral", "cohere", "groq":
		return 3
	default:
		return 4
	}
}

func containsAny(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// parseUint is a strconv.ParseUint wrapper that keeps the metadata helpers
// free of a strconv import — the standard library dep is fine, this just
// keeps the diff on this file self-contained.
func parseUint(s string) (uint64, error) {
	if s == "" {
		return 0, errEmpty
	}
	var n uint64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errNotDigit
		}
		n = n*10 + uint64(r-'0')
	}
	return n, nil
}

var (
	errEmpty    = &parseErr{"empty"}
	errNotDigit = &parseErr{"not a digit"}
)

type parseErr struct{ msg string }

func (e *parseErr) Error() string { return e.msg }


// legacyContextWindow bridges the old package-level contextWindows map. It
// exists so metadata.go stays the single lookup surface even while callers
// still expect ContextWindow() to work. The map itself lives in catalog.go
// alongside the rest of the hand-curated provider data.
func legacyContextWindow(model string) (int, bool) {
	w, ok := contextWindows[model]
	if !ok || w <= 0 {
		return 0, false
	}
	return w, true
}
