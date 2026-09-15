package providers

import "testing"

// TestLookupBareIDStableAcrossRepeatCalls installs a table with two
// same-id entries (alibaba first-party for Qwen, opencode re-listing) and
// asserts every lookup returns alibaba. Guards the deterministic tie-break
// in lookupBareID: without it map iteration order picks the winner and
// callers see phantom pricing swaps between calls.
func TestLookupBareIDStableAcrossRepeatCalls(t *testing.T) {
	restore := swapTable(t, map[string]ModelMeta{
		"alibaba/qwen3.5-plus": {
			Provider: "alibaba", ID: "qwen3.5-plus",
			ContextWindow: 1_000_000,
			Cost:          Cost{Input: 0.4, Output: 1.2},
		},
		"opencode/qwen3.5-plus": {
			Provider: "opencode", ID: "qwen3.5-plus",
			ContextWindow: 128_000,
			Cost:          Cost{Input: 5, Output: 15},
		},
		// Slug variant on a third provider to prove the exact match wins
		// even when the slug-variant entry ranks the same.
		"openrouter/qwen/qwen3-5-plus": {
			Provider: "openrouter", ID: "qwen3-5-plus",
			ContextWindow: 64_000,
		},
	})
	defer restore()

	for i := 0; i < 32; i++ {
		got, ok := Meta("qwen3.5-plus")
		if !ok {
			t.Fatalf("iter %d: Meta(qwen3.5-plus): expected hit", i)
		}
		if got.Provider != "alibaba" {
			t.Fatalf("iter %d: expected first-party alibaba, got %q (ctx=%d)",
				i, got.Provider, got.ContextWindow)
		}
		if got.ContextWindow != 1_000_000 {
			t.Errorf("iter %d: alibaba context window swapped in wrong entry: %d",
				i, got.ContextWindow)
		}
	}
}

// TestLookupBareIDPrefersExactOverSlug covers the "exact ID beats slug
// variant" branch: an entry whose id matches verbatim wins over one that
// only matches after a dot/dash flip, even when the slug-variant entry
// ranks higher by provider.
func TestLookupBareIDPrefersExactOverSlug(t *testing.T) {
	restore := swapTable(t, map[string]ModelMeta{
		// Exact match, mid-tier provider.
		"perplexity/claude-opus-4.7": {
			Provider: "perplexity", ID: "claude-opus-4.7",
			ContextWindow: 200_000,
		},
		// Higher-ranked provider but only matches via slug variant
		// ("claude-opus-4.7" -> "claude-opus-4-7").
		"anthropic/claude-opus-4-7": {
			Provider: "anthropic", ID: "claude-opus-4-7",
			ContextWindow: 500_000,
		},
	})
	defer restore()

	got, ok := Meta("claude-opus-4.7")
	if !ok {
		t.Fatalf("Meta(claude-opus-4.7): expected hit")
	}
	if got.Provider != "perplexity" {
		t.Errorf("expected exact-match perplexity to beat slug-variant anthropic, got %q", got.Provider)
	}
	if got.ContextWindow != 200_000 {
		t.Errorf("expected 200_000 from the exact-match entry, got %d", got.ContextWindow)
	}
}

// TestFirstPartyRankAlibaba pins the Qwen-family provider into the
// well-known-lab tier so lookupBareID prefers it over opencode's
// re-listing of the same id. Without this rank change the two entries
// would tie and fall to the key-based tie-break — still stable, but it
// happens to pick alibaba by string ordering. The rank makes the
// preference intentional rather than incidental.
func TestFirstPartyRankAlibaba(t *testing.T) {
	if firstPartyRank("alibaba") >= firstPartyRank("opencode") {
		t.Errorf("alibaba (%d) should rank ahead of opencode (%d)",
			firstPartyRank("alibaba"), firstPartyRank("opencode"))
	}
	if firstPartyRank("alibaba") != firstPartyRank("mistral") {
		t.Errorf("alibaba should sit in the well-known-lab tier alongside mistral et al")
	}
}
