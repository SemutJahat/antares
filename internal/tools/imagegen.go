package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/media"
)

// imageGenerateTool turns a prompt into an image via an OpenAI-compatible
// images endpoint, and saves the result to disk. When reference_paths are
// supplied it uses /images/edits so the model composes the new image with
// the supplied references as visual guidance; otherwise it uses
// /images/generations for a plain text-to-image call.
type imageGenerateTool struct{}

func (imageGenerateTool) Name() string { return "image_generate" }

func (imageGenerateTool) Description() string {
	return "Generate an image from a text prompt and save it to a file. Describe what you want in detail — " +
		"the subject, the style, the composition. Optionally pass reference_paths to reuse existing characters, " +
		"settings or styles. Returns the path to the saved image."
}

func (imageGenerateTool) Schema() map[string]any {
	return schema(map[string]any{
		"prompt": prop("string", "A detailed description of the image to generate."),
		// A free-form string so custom models with non-DALLE dimensions
		// (e.g. gpt-image-1 / gpt-image-2 arbitrary WxH) still work. The
		// media backend validates the value against the provider.
		"size": prop("string", "The image size as WIDTHxHEIGHT (e.g. 1024x1024, 720x1280). Defaults to the configured image_gen.size."),
		"reference_paths": map[string]any{
			"type":        "array",
			"description": "Optional local image paths to feed as references. When present, the edit endpoint is used so the result stays visually consistent with the references. Paths are resolved inside the current session's workspace or write roots.",
			"items":       map[string]any{"type": "string"},
		},
	}, "prompt")
}

func (imageGenerateTool) RequiresApproval() bool { return true }

func (imageGenerateTool) Execute(ctx context.Context, in Input) Result {
	var args struct {
		Prompt         string   `json:"prompt"`
		Size           string   `json:"size"`
		ReferencePaths []string `json:"reference_paths"`
	}
	if err := in.Bind(&args); err != nil {
		return Errorf("%v", err)
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return Errorf("prompt is required")
	}
	if in.Deps == nil || in.Deps.Config == nil {
		return Errorf("image generation is not configured")
	}

	ep, err := media.ImageEndpoint(in.Deps.Config)
	if err != nil {
		return Errorf("%v", err)
	}
	size := args.Size
	if size == "" {
		size = firstNonBlank(in.Deps.Config.ImageGen.Size, "1024x1024")
	}

	// Resolve every reference against the session's read boundary so an
	// arbitrary "/etc/passwd" or "../.ssh/id_rsa" cannot slip in as a
	// reference and be base64-uploaded to the provider.
	confinedRefs := make([]string, 0, len(args.ReferencePaths))
	for _, p := range args.ReferencePaths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		resolved, err := resolveRead(in, p)
		if err != nil {
			return Errorf("reference %q: %v", p, err)
		}
		confinedRefs = append(confinedRefs, resolved)
	}

	// Output is a write. In an ordinary session it lands in the workspace's
	// .antares/images; in a project session that path is inside WriteRoots.
	// Fall back to the antares home when we have no workspace at all.
	dir := filepath.Join(config.Home(), "images")
	if in.Workspace != "" {
		if info, err := os.Stat(in.Workspace); err == nil && info.IsDir() {
			dir = filepath.Join(in.Workspace, ".antares", "images")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Errorf("%v", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("image-%d.png", time.Now().UnixMilli()))

	in.Emit(Progress{Tool: "image_generate", Message: "generating…"})
	if err := media.GenerateImage(ctx, ep, args.Prompt, size, confinedRefs, path); err != nil {
		return Errorf("%v", err)
	}
	info, _ := os.Stat(path)
	var bytes int64
	if info != nil {
		bytes = info.Size()
	}
	return Result{
		Content: fmt.Sprintf("Generated an image and saved it to %s (%d KB).", path, bytes/1024),
		Meta:    map[string]any{"path": path, "bytes": bytes},
	}
}
