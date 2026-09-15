package media

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	_ "image/gif"  // decoder registration
	_ "image/jpeg" // decoder registration
)

// normalizeReferencePNG loads path, letterboxes the decoded image onto a
// blank canvas of exactly wantW×wantH pixels (preserving aspect ratio), and
// returns the result as PNG bytes. This is what makes an OpenAI /videos call
// safe when the reference was drawn at 1024x1024 but the requested output is
// 720x1280 — Sora rejects mismatched input_reference dimensions, so we scale
// + pad here rather than shipping the raw file and getting a 400.
//
// It uses NearestNeighbor for the scale. NN keeps stdlib-only builds while
// still producing an OpenAI-acceptable input: the reference is only a visual
// seed, not the final rendered frame, so a small quality drop from NN is fine.
func normalizeReferencePNG(path string, wantW, wantH int) ([]byte, error) {
	if wantW <= 0 || wantH <= 0 {
		return nil, errors.New("target size must be positive")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode reference %s: %w", filepath.Base(path), err)
	}

	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 {
		return nil, errors.New("reference has zero dimensions")
	}

	// Fit inside wantW×wantH preserving aspect.
	scaleW := float64(wantW) / float64(sw)
	scaleH := float64(wantH) / float64(sh)
	s := scaleW
	if scaleH < s {
		s = scaleH
	}
	dstW := int(float64(sw)*s + 0.5)
	dstH := int(float64(sh)*s + 0.5)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	scaled := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	nearestScale(scaled, src)

	canvas := image.NewRGBA(image.Rect(0, 0, wantW, wantH))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw.Src)
	offX := (wantW - dstW) / 2
	offY := (wantH - dstH) / 2
	draw.Draw(canvas, image.Rect(offX, offY, offX+dstW, offY+dstH), scaled, image.Point{}, draw.Src)

	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&buf, canvas); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// nearestScale writes src, resampled nearest-neighbor, into dst.Rect.
func nearestScale(dst *image.RGBA, src image.Image) {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dw, dh := dst.Rect.Dx(), dst.Rect.Dy()
	for y := range dh {
		sy := sb.Min.Y + y*sh/dh
		for x := range dw {
			sx := sb.Min.X + x*sw/dw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
}

// referenceDataURL returns a PNG data URL for path, resized to wantW×wantH
// when both are non-zero. When they are zero, the reference is sent as-is via
// the ordinary dataURL helper. Used by CreateVideo so the seed matches the
// requested output resolution exactly.
func referenceDataURL(path string, wantW, wantH int) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("empty reference path")
	}
	if wantW == 0 || wantH == 0 {
		return dataURL(path)
	}
	// Reuse the validation guards from dataURL: file must exist, be regular,
	// not a symlink, be within size limits, and be a known image type.
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("reference must not be a symlink: %s", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("reference is not a regular file: %s", path)
	}
	if info.Size() > maxResponse {
		return "", fmt.Errorf("reference %s is larger than %d bytes", path, int64(maxResponse))
	}
	if mimeFor(abs) == "" {
		return "", fmt.Errorf("unsupported reference type: %s", filepath.Ext(abs))
	}
	if err := checkFileImageBudget(abs); err != nil {
		return "", fmt.Errorf("reference %s: %w", path, err)
	}
	png, err := normalizeReferencePNG(abs, wantW, wantH)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
