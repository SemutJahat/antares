package providers

import "testing"

// TestMetaLookupBareID exercises Meta() for callers that only know the model
// id (e.g. agent.contextWindowFor). Should hit the generated table without
// needing a provider hint.
func TestMetaLookupBareID(t *testing.T) {
	// pick a model we know models.dev catalogues with a non-zero window.
	// Choose one whose id is unique across providers to keep the assertion
	// robust — a bare id lookup returns the first-matching entry.
	got, ok := Meta("claude-sonnet-4-6")
	if !ok {
		t.Fatalf("Meta(claude-sonnet-4-6): expected hit from generated snapshot")
	}
	if got.ContextWindow == 0 {
		t.Errorf("expected non-zero ContextWindow for claude-sonnet-4-6, got 0")
	}
	if got.Source != "generated" {
		t.Errorf("expected Source=generated, got %q", got.Source)
	}
}

// TestMetaLookupProviderQualified covers the "provider/id" shortcut path.
func TestMetaLookupProviderQualified(t *testing.T) {
	got, ok := Meta("anthropic/claude-sonnet-4-6")
	if !ok {
		t.Fatalf("Meta(anthropic/claude-sonnet-4-6): expected hit")
	}
	if got.Provider != "anthropic" {
		t.Errorf("expected Provider=anthropic, got %q", got.Provider)
	}
	if got.ContextWindow == 0 {
		t.Errorf("expected non-zero ContextWindow, got 0")
	}
}

// TestMetaLookupUnknown returns miss for a model id not in any layer.
func TestMetaLookupUnknown(t *testing.T) {
	_, ok := Meta("this-model-definitely-does-not-exist-42")
	if ok {
		t.Errorf("expected miss for unknown model, got hit")
	}
}

// TestMetaByProviderGeneratedHit — direct provider+model lookup on the
// generated snapshot.
func TestMetaByProviderGeneratedHit(t *testing.T) {
	got, ok := MetaByProvider("anthropic", "claude-sonnet-4-6")
	if !ok {
		t.Fatalf("MetaByProvider(anthropic, claude-sonnet-4-6): expected hit")
	}
	if got.ContextWindow == 0 {
		t.Errorf("expected non-zero context window")
	}
	if !got.ToolCall {
		t.Errorf("expected ToolCall=true for Claude Sonnet")
	}
	if got.Cost.Input == 0 {
		t.Errorf("expected non-zero input cost")
	}
}

// TestMetaByProviderMiss returns not-ok when the pair is unknown.
func TestMetaByProviderMiss(t *testing.T) {
	_, ok := MetaByProvider("anthropic", "nonexistent-model-xyz")
	if ok {
		t.Errorf("expected miss")
	}
}

// TestMetaCuratedOverridesGenerated verifies the hand-curated
// contextWindows map wins over the generated snapshot in
// MetaByProvider, so operators can patch a wrong upstream value
// without waiting for the sync script.
func TestMetaCuratedOverridesGenerated(t *testing.T) {
	// glm-5.2 sits in both the curated map (1_000_000) and the generated
	// snapshot. If the cascade order is correct, we get the curated value.
	got, ok := MetaByProvider("zai", "glm-5.2")
	if !ok {
		t.Fatalf("MetaByProvider(zai, glm-5.2): expected hit")
	}
	if got.ContextWindow != 1_000_000 {
		t.Errorf("expected curated 1_000_000, got %d", got.ContextWindow)
	}
	if got.Source != "curated" {
		t.Errorf("expected Source=curated, got %q", got.Source)
	}
}

// TestContextWindowUsesCascade confirms the package-level ContextWindow()
// helper is a thin wrapper on Meta() — no more standalone map lookup.
func TestContextWindowUsesCascade(t *testing.T) {
	if got := ContextWindow("claude-sonnet-4-6"); got == 0 {
		t.Errorf("expected non-zero ContextWindow from generated snapshot, got 0")
	}
	if got := ContextWindow("glm-5.2"); got != 1_000_000 {
		t.Errorf("expected 1_000_000 for glm-5.2, got %d", got)
	}
	if got := ContextWindow("bogus-model-name"); got != 0 {
		t.Errorf("expected 0 for unknown model, got %d", got)
	}
}

// TestMetaByProviderRejectsWrongProvider guards the fix for a bug where
// requesting a model under a first-party provider that does not carry the
// id (e.g. anthropic + kimi-k2.5) fell through to a random re-listing under
// opencode or similar, silently attributing the wrong context window. The
// strict pair lookup now returns miss so callers can decide how to fall back.
func TestMetaByProviderRejectsWrongProvider(t *testing.T) {
	cases := []struct{ provider, model string }{
		{"anthropic", "kimi-k2.5"},
		{"openai", "kimi-k2.5"},
		{"deepseek", "deepseek-v3.2"}, // v3.2 is not in the catalogue
		{"anthropic", "deepseek-v3.2"},
	}
	for _, c := range cases {
		if _, ok := MetaByProvider(c.provider, c.model); ok {
			t.Errorf("MetaByProvider(%q, %q): expected miss, got hit — cross-provider pin regressed",
				c.provider, c.model)
		}
	}
}

// TestMetaByProviderSlugVariants verifies dot↔dash normalisation still finds
// the entry when a caller passes a slug in the "other" convention.
func TestMetaByProviderSlugVariants(t *testing.T) {
	got, ok := MetaByProvider("anthropic", "claude-sonnet-4.6")
	if !ok {
		t.Fatalf("MetaByProvider(anthropic, claude-sonnet-4.6): expected hit via slug variant")
	}
	if got.ContextWindow == 0 {
		t.Errorf("expected non-zero context window, got 0")
	}
}

// TestMetaByAnyProviderResolvesProxy is the escape hatch OpenAI-compatible
// proxies use — the strict pair lookup misses because enx/kimi-k2.5 does not
// exist in the table, but the bare id has a canonical entry the picker
// should still surface.
func TestMetaByAnyProviderResolvesProxy(t *testing.T) {
	got, ok := MetaByAnyProvider("kimi-k2.5")
	if !ok {
		t.Fatalf("MetaByAnyProvider(kimi-k2.5): expected hit from generated snapshot")
	}
	if got.ContextWindow == 0 {
		t.Errorf("expected non-zero context window")
	}
}

// TestMetaByAnyProviderMissForUnknown — the loose lookup must still refuse
// to fabricate metadata for an id that genuinely does not exist.
func TestMetaByAnyProviderMissForUnknown(t *testing.T) {
	if _, ok := MetaByAnyProvider("this-id-does-not-exist-42"); ok {
		t.Errorf("expected miss for bogus id, got hit")
	}
}
