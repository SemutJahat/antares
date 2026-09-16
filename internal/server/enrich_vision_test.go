package server

import (
	"testing"

	"github.com/enowdev/antares/internal/llm"
	"github.com/enowdev/antares/internal/providers"
)

// TestEnrichModelInfoVisionOnlyFromMeta covers the Vision-vs-Attachment
// distinction in enrichModelInfo: the picker's Vision flag must reflect
// meta.Vision alone. The previous code folded meta.Attachment into it —
// PDF/audio/video count as attachments in models.dev's schema — so an
// audio-only model was silently marked vision-capable, letting the picker
// feed it image bytes it will refuse.
//
// Uses live bundled snapshot entries rather than injecting synthetic
// metadata so the assertion also validates the modality classification
// upstream at normaliseLive / models_generated.json.
func TestEnrichModelInfoVisionOnlyFromMeta(t *testing.T) {
	// Sanity: the bundled snapshot must actually carry the fixtures we
	// rely on, otherwise the test degrades to a no-op after the next
	// sync-models regeneration.
	audioMeta, ok := providers.MetaByProvider("mistral", "voxtral-small-latest")
	if !ok {
		t.Fatalf("bundled snapshot missing mistral/voxtral-small-latest; refresh fixture")
	}
	if audioMeta.Vision {
		t.Fatalf("fixture voxtral-small-latest has Vision=true — pick a different audio-only model")
	}
	if !audioMeta.Attachment {
		t.Fatalf("fixture voxtral-small-latest has Attachment=false — the test needs the OR-bleed condition")
	}

	got := enrichModelInfo("mistral", llm.ModelInfo{ID: "voxtral-small-latest"}, "openai")
	if got.Vision {
		t.Errorf("Vision leaked from Attachment for an audio-only model — regression in enrichModelInfo")
	}

	// Positive control: a model with Vision=true in the snapshot must
	// still get Vision=true through the enricher.
	visionMeta, ok := providers.MetaByProvider("anthropic", "claude-sonnet-4-6")
	if !ok || !visionMeta.Vision {
		t.Fatalf("bundled snapshot missing anthropic/claude-sonnet-4-6 vision fixture")
	}
	visionGot := enrichModelInfo("anthropic", llm.ModelInfo{ID: "claude-sonnet-4-6"}, "anthropic")
	if !visionGot.Vision {
		t.Errorf("Vision=true meta should still enrich Vision=true; got false")
	}
}
