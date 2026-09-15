package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ffmpegBin returns the ffmpeg binary path or an error if it is not on PATH.
func ffmpegBin() (string, error) {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	return "", errors.New("ffmpeg is not installed on this machine")
}

// ffprobeBin returns the ffprobe binary path or an error if it is not on PATH.
func ffprobeBin() (string, error) {
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p, nil
	}
	return "", errors.New("ffprobe is not installed on this machine")
}

// HasFFmpeg reports whether ffmpeg is available. Callers use it to render a
// clear "install ffmpeg" hint instead of failing mid-workflow.
func HasFFmpeg() bool  { _, err := ffmpegBin(); return err == nil }
func HasFFprobe() bool { _, err := ffprobeBin(); return err == nil }

// ExtractLastFrame writes a single-frame image at output from the final frame
// of videoPath. If size is non-empty ("WxH") the frame is scaled+padded onto
// that canvas so it matches a later shot's resolution without stretching.
// Argv-only, no shell interpolation.
func ExtractLastFrame(ctx context.Context, videoPath, output, size string) error {
	if strings.TrimSpace(videoPath) == "" {
		return errors.New("video path is required")
	}
	if strings.TrimSpace(output) == "" {
		return errors.New("output path is required")
	}
	bin, err := ffmpegBin()
	if err != nil {
		return err
	}
	if _, err := os.Stat(videoPath); err != nil {
		return fmt.Errorf("source video: %w", err)
	}

	// Frame-exact final frame. We do NOT rely on `-ss D-1/fps` with
	// `-frames:v 1` — timestamp rounding at 3 decimals and non-CFR streams
	// can seek past the real last frame, yielding "one frame short" or an
	// error. Instead: decode the whole stream, tell ffmpeg to write the
	// image with `-update 1` in image2 muxer, which OVERWRITES the same
	// output file for every decoded frame. When the input ends, the file
	// on disk holds the LAST frame written — the actual final frame.
	//
	// Cost: the whole clip is decoded. For Content Creator clips these are
	// at most ~12s at ~1080p; decoding is fast and bounded, and the memory
	// footprint is one frame at a time (no full-stream buffer).
	info, err := probeVideo(ctx, videoPath)
	if err != nil {
		return err
	}
	if info.Duration <= 0 {
		return errors.New("source video has zero duration")
	}

	dir := filepath.Dir(output)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".frame-*.png")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	vf := ""
	if s := strings.TrimSpace(size); s != "" {
		w, h, err := parseSize(s)
		if err != nil {
			return err
		}
		vf = fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1", w, h, w, h)
	}

	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", videoPath,
	}
	if vf != "" {
		args = append(args, "-vf", vf)
	}
	args = append(args,
		// -update 1 with image2 rewrites tmpName every decoded frame; the
		// file surviving at EOF is therefore the last frame.
		"-fps_mode", "passthrough",
		"-update", "1",
		"-f", "image2",
		tmpName,
	)
	if out, err := runCombined(ctx, bin, args...); err != nil {
		return fmt.Errorf("ffmpeg extract-last-frame failed: %s", truncate(out, 400))
	}
	st, err := os.Stat(tmpName)
	if err != nil {
		return fmt.Errorf("ffmpeg did not produce a frame: %w", err)
	}
	if st.Size() == 0 {
		return errors.New("ffmpeg produced an empty frame")
	}
	return os.Rename(tmpName, output)
}

// Assemble concatenates clips into a single H.264/AAC MP4 at output. Each
// clip is normalised (scale+pad to size when set, silent AAC stereo added if
// missing, common fps) so a concat is guaranteed to succeed regardless of
// input diversity. The final file is probed and its actual duration/resolution
// are validated before the temp is renamed into place.
func Assemble(ctx context.Context, clips []string, output, size string) error {
	if len(clips) == 0 {
		return errors.New("no clips to assemble")
	}
	if strings.TrimSpace(output) == "" {
		return errors.New("output path is required")
	}
	bin, err := ffmpegBin()
	if err != nil {
		return err
	}
	for _, c := range clips {
		if strings.TrimSpace(c) == "" {
			return errors.New("empty clip path")
		}
		if _, err := os.Stat(c); err != nil {
			return fmt.Errorf("clip %s: %w", c, err)
		}
	}

	w, h := 720, 1280
	if strings.TrimSpace(size) != "" {
		pw, ph, err := parseSize(size)
		if err != nil {
			return err
		}
		w, h = pw, ph
	}

	// Probe each input for duration + audio presence. We need the exact
	// duration to bound any silent-audio filler with atrim, otherwise the
	// anullsrc / concat combination would produce an unbounded audio leg
	// and the concat filter would never advance past the first silent leg.
	// (-shortest at the OUTPUT stage does NOT rescue this: it clips the
	//  final render, but the concat filter itself deadlocks or emits an
	//  effectively infinite first segment.)
	perClip := make([]ProbeInfo, len(clips))
	var declaredTotal float64
	for i, c := range clips {
		info, err := probeVideo(ctx, c)
		if err != nil {
			return fmt.Errorf("probe clip %s: %w", c, err)
		}
		if info.Duration <= 0 {
			return fmt.Errorf("clip %s has zero duration", c)
		}
		perClip[i] = info
		declaredTotal += info.Duration
	}

	dir := filepath.Dir(output)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpOut, err := os.CreateTemp(dir, ".assemble-*.mp4")
	if err != nil {
		return err
	}
	tmpOutName := tmpOut.Name()
	tmpOut.Close()
	defer func() {
		if _, statErr := os.Stat(tmpOutName); statErr == nil {
			_ = os.Remove(tmpOutName)
		}
	}()

	args := []string{"-hide_banner", "-nostdin", "-y"}
	// Real inputs first.
	for _, c := range clips {
		args = append(args, "-i", c)
	}
	// One silent audio input per clip missing audio, all bounded upstream by
	// the atrim filter to that clip's video duration.
	for _, info := range perClip {
		if !info.HasAudio {
			_ = info // duration handled inside filter graph
			args = append(args, "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000")
		}
	}

	var fc strings.Builder
	silentBase := len(clips)
	silentCursor := 0
	for i, info := range perClip {
		// Video: scale+pad to WxH, common fps, reset PTS so concat starts each
		// clip at 0 relative time (otherwise later clips' timestamps land
		// past the earlier clip's end and the concat produces a broken timeline).
		fc.WriteString(fmt.Sprintf(
			"[%d:v]scale=w=%d:h=%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,fps=30,format=yuv420p,setpts=PTS-STARTPTS[v%d];",
			i, w, h, w, h, i,
		))
		if info.HasAudio {
			fc.WriteString(fmt.Sprintf(
				"[%d:a]aformat=sample_fmts=fltp:sample_rates=48000:channel_layouts=stereo,atrim=duration=%.3f,asetpts=PTS-STARTPTS[a%d];",
				i, info.Duration, i,
			))
		} else {
			// Silent input is infinite; bound it to this clip's duration
			// before it enters concat.
			fc.WriteString(fmt.Sprintf(
				"[%d:a]aformat=sample_fmts=fltp:sample_rates=48000:channel_layouts=stereo,atrim=duration=%.3f,asetpts=PTS-STARTPTS[a%d];",
				silentBase+silentCursor, info.Duration, i,
			))
			silentCursor++
		}
	}
	for i := range clips {
		fc.WriteString(fmt.Sprintf("[v%d][a%d]", i, i))
	}
	fc.WriteString(fmt.Sprintf("concat=n=%d:v=1:a=1[outv][outa]", len(clips)))

	args = append(args,
		"-filter_complex", fc.String(),
		"-map", "[outv]", "-map", "[outa]",
		"-c:v", "libx264", "-preset", "medium", "-crf", "20", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k",
		"-movflags", "+faststart",
		tmpOutName,
	)
	if out, err := runCombined(ctx, bin, args...); err != nil {
		return fmt.Errorf("ffmpeg assemble failed: %s", truncate(out, 800))
	}

	// Verify the actual result: resolution matches, duration matches the sum
	// of input durations within a two-frame tolerance (at 30 fps that is ~66ms).
	probeInfo, err := probeVideo(ctx, tmpOutName)
	if err != nil {
		return fmt.Errorf("assemble probe failed: %w", err)
	}
	if probeInfo.Duration <= 0 {
		return errors.New("assemble produced a zero-length file")
	}
	if probeInfo.Width != w || probeInfo.Height != h {
		return fmt.Errorf("assemble produced %dx%d, expected %dx%d", probeInfo.Width, probeInfo.Height, w, h)
	}
	const frameTolerance = 2.0 / 30.0
	drift := probeInfo.Duration - declaredTotal
	if drift < 0 {
		drift = -drift
	}
	if drift > frameTolerance {
		return fmt.Errorf("assemble duration drift %.3fs exceeds tolerance %.3fs (got %.3fs, expected %.3fs)",
			drift, frameTolerance, probeInfo.Duration, declaredTotal)
	}
	return os.Rename(tmpOutName, output)
}

// ProbeInfo is a subset of ffprobe output the workflow acts on.
type ProbeInfo struct {
	Duration float64
	Width    int
	Height   int
	HasAudio bool
}

// Probe returns basic media metadata for path.
func Probe(ctx context.Context, path string) (ProbeInfo, error) { return probeVideo(ctx, path) }

// probeVideo runs ffprobe and returns duration/resolution/audio presence.
func probeVideo(ctx context.Context, path string) (ProbeInfo, error) {
	bin, err := ffprobeBin()
	if err != nil {
		return ProbeInfo{}, err
	}
	out, err := runCombined(ctx, bin,
		"-v", "error",
		"-show_entries", "stream=codec_type,width,height:format=duration",
		"-of", "json",
		path,
	)
	if err != nil {
		return ProbeInfo{}, fmt.Errorf("ffprobe failed: %s", truncate(out, 400))
	}
	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return ProbeInfo{}, fmt.Errorf("ffprobe output was not JSON: %w", err)
	}
	info := ProbeInfo{}
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			if info.Width == 0 {
				info.Width, info.Height = s.Width, s.Height
			}
		case "audio":
			info.HasAudio = true
		}
	}
	if d, err := strconv.ParseFloat(strings.TrimSpace(parsed.Format.Duration), 64); err == nil {
		info.Duration = d
	}
	return info, nil
}

// parseSize splits "WxH" into positive integers.
func parseSize(s string) (int, int, error) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(s)), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("size must be WxH, got %q", s)
	}
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || w <= 0 {
		return 0, 0, fmt.Errorf("invalid width in %q", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || h <= 0 {
		return 0, 0, fmt.Errorf("invalid height in %q", s)
	}
	return w, h, nil
}

// runCombined runs argv without a shell and returns the combined output.
// Uses exec.CommandContext so an aborted request cancels the process.
func runCombined(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
