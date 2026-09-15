package creator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/media"
)

// Execute performs one media action on a project: generate a reference PNG,
// generate a keyframe PNG for a shot, submit a paid video job, poll a video
// job to completion (downloading it and extracting the last frame), or
// assemble the ordered ready shots into the final MP4.
//
// It is the only path that talks to the media provider. Guards:
//   - stageAllows("media") — a research/plan run cannot spend money.
//   - checkOwner — another run holding the project blocks writes.
//   - a shot that already carries a video_job_id in a non-terminal state is
//     never resubmitted; poll_video handles it instead.
//   - keyframes for continue shots come from the previous shot's last_frame,
//     not from References; a cut shot's keyframe is generated from References.
//   - each shot's PromptHash records the exact inputs that produced its media
//     so a matching precheck reuses artefacts and a mismatch refuses to serve
//     stale ones (Update already invalidates paths — this defends inside).
//   - assemble validates the total duration against target_seconds.
func (s *Service) Execute(ctx context.Context, cfg *config.Config, id, action, target string) (result Project, runErr error) {
	if ok, reason := stageAllows(ctx, "media"); !ok {
		return Project{}, errors.New(reason)
	}
	mu := lockProject(s.root, id)
	mu.Lock()
	defer mu.Unlock()

	p, err := s.load(ctx, id)
	if err != nil {
		return Project{}, err
	}
	if err := s.checkOwner(ctx, p); err != nil {
		return p, err
	}
	defer func() {
		if result.ID == "" {
			return
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if runErr != nil {
			result.Error = redactMedia(runErr)
			result.Status = "error"
		} else {
			result.Error = ""
			if result.Status == "error" {
				result.Status = "draft"
			}
		}
		if err := s.save(writeCtx, &result); err != nil && runErr == nil {
			runErr = err
		}
	}()

	dir, err := s.projectDir(id)
	if err != nil {
		return p, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return p, fmt.Errorf("prepare project directory: %w", err)
	}

	action = strings.TrimSpace(strings.ToLower(action))
	switch action {
	case "generate_reference":
		return s.executeGenerateReference(ctx, cfg, &p, dir, target)
	case "generate_keyframe":
		return s.executeGenerateKeyframe(ctx, cfg, &p, dir, target)
	case "generate_video":
		return s.executeGenerateVideo(ctx, cfg, &p, dir, target)
	case "poll_video":
		return s.executePollVideo(ctx, cfg, &p, dir, target)
	case "assemble":
		return s.executeAssemble(ctx, cfg, &p, dir)
	}
	return p, fmt.Errorf("unknown creator action %q", action)
}

// ----- Reference generation ------------------------------------------------

func (s *Service) executeGenerateReference(ctx context.Context, cfg *config.Config, p *Project, dir, refID string) (Project, error) {
	idx := findReference(p, refID)
	if idx < 0 {
		return *p, fmt.Errorf("reference %q is not in this project", refID)
	}
	ref := &p.References[idx]
	if strings.TrimSpace(ref.Prompt) == "" {
		return *p, errors.New("reference has no prompt yet")
	}
	if ref.Status == "ready" && ref.Path != "" {
		if _, err := s.Artifact(ctx, p.ID, ref.Path); err != nil {
			return *p, err
		}
		// Idempotent — Update() already cleared derived paths when the prompt
		// changed, so a ready ref here is still valid. Skip the paid call.
		return *p, nil
	}
	if ref.Status == "generating" {
		return *p, errors.New("reference is already generating")
	}
	ep, err := media.ImageEndpoint(cfg)
	if err != nil {
		ref.Status = "error"
		ref.Error = err.Error()
		_ = s.save(ctx, p)
		return *p, err
	}
	out := filepath.Join(dir, "reference-"+ref.ID+".png")
	ref.Status = "generating"
	ref.Error = ""
	if err := s.save(ctx, p); err != nil {
		return *p, err
	}

	// Paid call. On failure we record it and never auto-retry.
	err = media.GenerateImage(ctx, ep, buildReferencePrompt(*p, *ref), imageSize(cfg.ImageGen.Size), nil, out)

	// Reload+relock-mutate the fresh state; save incremented the revision so
	// we operate on the current snapshot.
	fresh, loadErr := s.load(ctx, p.ID)
	if loadErr != nil {
		return *p, loadErr
	}
	rIdx := findReference(&fresh, refID)
	if rIdx < 0 {
		return fresh, fmt.Errorf("reference %q disappeared during generation", refID)
	}
	target := &fresh.References[rIdx]
	if err != nil {
		target.Status = "error"
		target.Error = redactMedia(err)
		fresh.Error = target.Error
		_ = s.save(ctx, &fresh)
		return fresh, err
	}
	target.Path = filepath.Base(out)
	target.Status = "ready"
	target.Error = ""
	if err := s.save(ctx, &fresh); err != nil {
		return fresh, err
	}
	*p = fresh
	return fresh, nil
}

// ----- Keyframe generation -------------------------------------------------

func (s *Service) executeGenerateKeyframe(ctx context.Context, cfg *config.Config, p *Project, dir, shotID string) (Project, error) {
	idx := findShot(p, shotID)
	if idx < 0 {
		return *p, fmt.Errorf("shot %q is not in this project", shotID)
	}
	shot := &p.Shots[idx]
	if strings.TrimSpace(shot.Prompt) == "" {
		return *p, errors.New("shot has no prompt yet")
	}
	if shot.Status == "generating_keyframe" || shot.Status == "generating_video" || shot.Status == "polling" {
		return *p, errors.New("shot is already busy")
	}
	if shot.VideoJobID != "" && shot.VideoPath == "" {
		return *p, errors.New("shot has a pending video job; poll it first")
	}

	// Idempotency: a ready keyframe whose inputs still match is reused. This
	// stops a stray retry from spending money AND from invalidating downstream
	// clips/final. Explicit reset lives elsewhere (Main-owned reset action).
	if shot.KeyframePath != "" && shot.PromptHash != "" && shot.PromptHash == shotInputsHash(*p, *shot) {
		return *p, nil
	}

	continuity := shot.Continuity
	switch continuity {
	case "continue":
		// The previous shot's last frame IS the keyframe. Copy it — never
		// spend a paid image call that would break visual continuity.
		if idx == 0 {
			return *p, errors.New("first shot cannot continue a previous clip")
		}
		prev := p.Shots[idx-1]
		if prev.LastFramePath == "" {
			return *p, errors.New("previous shot has no last frame yet; poll it first")
		}
		src, err := s.Artifact(ctx, p.ID, prev.LastFramePath)
		if err != nil {
			return *p, fmt.Errorf("resolve previous last frame: %w", err)
		}
		out := filepath.Join(dir, "keyframe-"+shot.ID+".png")
		if err := copyFile(src, out); err != nil {
			return *p, fmt.Errorf("copy previous last frame: %w", err)
		}
		shot.KeyframePath = filepath.Base(out)
		shot.Status = "keyframe_ready"
		shot.Error = ""
		shot.PromptHash = shotInputsHash(*p, *shot)
		if err := s.save(ctx, p); err != nil {
			return *p, err
		}
		return *p, nil
	case "cut":
		if len(shot.ReferenceIDs) == 0 {
			return *p, errors.New("cut shot needs at least one approved reference")
		}
	default:
		return *p, fmt.Errorf("invalid continuity %q", continuity)
	}

	// Cut path — resolve every reference through Artifact so a rogue path
	// cannot smuggle an outside file into the image endpoint.
	var refPaths []string
	for _, rid := range shot.ReferenceIDs {
		rIdx := findReference(p, rid)
		if rIdx < 0 {
			return *p, fmt.Errorf("unknown reference %s", rid)
		}
		r := p.References[rIdx]
		if r.Status != "ready" || r.Path == "" {
			return *p, fmt.Errorf("reference %s is not ready", rid)
		}
		abs, err := s.Artifact(ctx, p.ID, r.Path)
		if err != nil {
			return *p, fmt.Errorf("resolve reference %s: %w", rid, err)
		}
		refPaths = append(refPaths, abs)
	}

	ep, err := media.ImageEndpoint(cfg)
	if err != nil {
		shot.Status = "error"
		shot.Error = err.Error()
		_ = s.save(ctx, p)
		return *p, err
	}
	out := filepath.Join(dir, "keyframe-"+shot.ID+".png")
	shot.Status = "generating_keyframe"
	shot.Error = ""
	if err := s.save(ctx, p); err != nil {
		return *p, err
	}

	err = media.GenerateImage(ctx, ep, buildShotPrompt(*p, *shot), imageSize(cfg.ImageGen.Size), refPaths, out)

	fresh, loadErr := s.load(ctx, p.ID)
	if loadErr != nil {
		return *p, loadErr
	}
	sIdx := findShot(&fresh, shotID)
	if sIdx < 0 {
		return fresh, fmt.Errorf("shot %q disappeared during generation", shotID)
	}
	target := &fresh.Shots[sIdx]
	if err != nil {
		target.Status = "error"
		target.Error = redactMedia(err)
		fresh.Error = target.Error
		_ = s.save(ctx, &fresh)
		return fresh, err
	}
	target.KeyframePath = filepath.Base(out)
	target.Status = "keyframe_ready"
	target.Error = ""
	target.PromptHash = shotInputsHash(fresh, *target)
	if err := s.save(ctx, &fresh); err != nil {
		return fresh, err
	}
	*p = fresh
	return fresh, nil
}

// ----- Video generation ----------------------------------------------------

func (s *Service) executeGenerateVideo(ctx context.Context, cfg *config.Config, p *Project, dir, shotID string) (Project, error) {
	idx := findShot(p, shotID)
	if idx < 0 {
		return *p, fmt.Errorf("shot %q is not in this project", shotID)
	}
	shot := &p.Shots[idx]
	// Continue shots auto-derive their keyframe from the previous last frame.
	// A missing keyframe here for a continue shot is expected on first video
	// call; copy it now so the video POST has a seed image.
	if shot.KeyframePath == "" && shot.Continuity == "continue" && idx > 0 {
		prev := p.Shots[idx-1]
		if prev.LastFramePath == "" {
			return *p, errors.New("previous shot has no last frame yet; poll it first")
		}
		src, err := s.Artifact(ctx, p.ID, prev.LastFramePath)
		if err != nil {
			return *p, fmt.Errorf("resolve previous last frame: %w", err)
		}
		out := filepath.Join(dir, "keyframe-"+shot.ID+".png")
		if err := copyFile(src, out); err != nil {
			return *p, fmt.Errorf("copy previous last frame: %w", err)
		}
		shot.KeyframePath = filepath.Base(out)
		shot.PromptHash = shotInputsHash(*p, *shot)
		if err := s.save(ctx, p); err != nil {
			return *p, err
		}
	}
	if shot.KeyframePath == "" {
		return *p, errors.New("generate the keyframe before the video")
	}
	// Idempotent: a ready clip whose inputs still match is reused. Callers
	// should call poll_video for an in-flight job, and use an explicit reset
	// to redo a completed shot.
	if shot.Status == "ready" && shot.VideoPath != "" {
		return *p, nil
	}
	if shot.VideoJobID != "" {
		// Never resubmit a paid job. Direct the caller to poll instead.
		return *p, fmt.Errorf("shot %s already has provider job %s; use poll_video", shot.ID, shot.VideoJobID)
	}
	if shot.Status == "submission_unknown" {
		return *p, errors.New("previous video POST outcome unknown; reconcile before retrying (do not resubmit)")
	}
	if shot.Status == "generating_video" || shot.Status == "polling" {
		return *p, errors.New("shot video is already in progress")
	}
	// Stale artefact guard: the shot's recorded inputs must match its
	// current definition. Update() invalidates on prompt/ref change; this
	// catches races where the keyframe was made under a different prompt.
	if want := shotInputsHash(*p, *shot); shot.PromptHash != "" && shot.PromptHash != want {
		return *p, errors.New("shot inputs changed since the keyframe was made; regenerate the keyframe")
	}
	ep, err := media.VideoEndpoint(cfg)
	if err != nil {
		shot.Status = "error"
		shot.Error = err.Error()
		p.Error = shot.Error
		_ = s.save(ctx, p)
		return *p, err
	}
	seconds := shot.DurationSeconds
	if seconds <= 0 {
		seconds = cfg.VideoGen.Seconds
	}
	fingerprint := providerFingerprint(ep)
	shot.Status = "submission_unknown"
	shot.Error = ""
	shot.Progress = 0
	shot.ProviderSnapshot = fingerprint
	if err := s.save(ctx, p); err != nil {
		return *p, err
	}

	keyframeAbs, err := s.Artifact(ctx, p.ID, shot.KeyframePath)
	if err != nil {
		return *p, fmt.Errorf("resolve keyframe: %w", err)
	}
	job, err := media.CreateVideo(ctx, ep, buildShotPrompt(*p, *shot), imageSize(p.Size), seconds, keyframeAbs)

	fresh, loadErr := s.load(ctx, p.ID)
	if loadErr != nil {
		return *p, loadErr
	}
	sIdx := findShot(&fresh, shotID)
	if sIdx < 0 {
		return fresh, fmt.Errorf("shot %q disappeared during job creation", shotID)
	}
	target := &fresh.Shots[sIdx]
	if err != nil {
		// Provider replied with a hard reject (parsed HTTP status) → definite
		// failure, safe to allow a reset. Anything else (transport error,
		// timeout, partial body) is ambiguous: the POST may have landed and
		// spent money. Mark submission_unknown so no auto-retry ever fires.
		msg := redactMedia(err)
		if isDefiniteReject(err) {
			target.Status = "error"
		} else {
			target.Status = "submission_unknown"
		}
		target.Error = msg
		fresh.Error = msg
		_ = s.save(ctx, &fresh)
		return fresh, err
	}
	if strings.TrimSpace(job.ID) == "" {
		target.Status = "submission_unknown"
		target.Error = "provider did not return a job id; reconcile before retrying"
		fresh.Error = target.Error
		_ = s.save(ctx, &fresh)
		return fresh, errors.New(target.Error)
	}
	target.VideoJobID = job.ID
	target.Status = "polling"
	target.ProviderSnapshot = fingerprint
	if job.Progress > 0 {
		target.Progress = job.Progress
	}
	if err := s.save(ctx, &fresh); err != nil {
		return fresh, err
	}
	*p = fresh
	return fresh, nil
}

// ----- Video polling -------------------------------------------------------

func (s *Service) executePollVideo(ctx context.Context, cfg *config.Config, p *Project, dir, shotID string) (Project, error) {
	idx := findShot(p, shotID)
	if idx < 0 {
		return *p, fmt.Errorf("shot %q is not in this project", shotID)
	}
	shot := &p.Shots[idx]
	if shot.VideoJobID == "" {
		return *p, errors.New("shot has no video job to poll")
	}
	if shot.VideoPath != "" && shot.LastFramePath != "" && shot.Status == "ready" {
		return *p, nil
	}
	ep, err := media.VideoEndpoint(cfg)
	if err != nil {
		shot.Status = "error"
		shot.Error = err.Error()
		p.Error = shot.Error
		_ = s.save(ctx, p)
		return *p, err
	}
	// Provider identity guard: a config swap between generate_video and
	// poll_video must not leak the current bearer to the previous provider's
	// host or claim a foreign job as ours. The operator resets explicitly.
	if shot.ProviderSnapshot != "" && shot.ProviderSnapshot != providerFingerprint(ep) {
		return *p, errors.New("video provider changed since this job was created; reconcile the shot before polling")
	}
	job, err := media.GetVideo(ctx, ep, shot.VideoJobID)
	if err != nil {
		// Poll failure is transient; keep the job id so the next poll retries.
		shot.Error = redactMedia(err)
		_ = s.save(ctx, p)
		return *p, err
	}
	shot.Progress = job.Progress
	switch strings.ToLower(job.Status) {
	case "queued", "in_progress", "processing", "pending":
		if shot.Status != "polling" {
			shot.Status = "polling"
		}
		shot.Error = ""
		if err := s.save(ctx, p); err != nil {
			return *p, err
		}
		return *p, nil
	case "failed", "error", "canceled", "cancelled":
		shot.Status = "failed"
		if job.Error != "" {
			shot.Error = job.Error
		} else {
			shot.Error = "provider reported failure"
		}
		if err := s.save(ctx, p); err != nil {
			return *p, err
		}
		return *p, errors.New(shot.Error)
	case "completed", "succeeded", "success":
		// Download and extract last frame under the project dir.
	default:
		shot.Error = fmt.Sprintf("provider returned unknown video status %q", job.Status)
		_ = s.save(ctx, p)
		return *p, errors.New(shot.Error)
	}

	videoOut := filepath.Join(dir, "clip-"+shot.ID+".mp4")
	if err := media.DownloadVideo(ctx, ep, shot.VideoJobID, videoOut); err != nil {
		shot.Error = redactMedia(err)
		_ = s.save(ctx, p)
		return *p, err
	}
	lastOut := filepath.Join(dir, "last-"+shot.ID+".png")
	if err := media.ExtractLastFrame(ctx, videoOut, lastOut, imageSize(p.Size)); err != nil {
		shot.Error = redactMedia(err)
		// The clip is real; expose it so the operator can inspect. Block a
		// continue-successor by leaving LastFramePath empty.
		shot.VideoPath = filepath.Base(videoOut)
		shot.Status = "ready_no_lastframe"
		_ = s.save(ctx, p)
		return *p, err
	}
	shot.VideoPath = filepath.Base(videoOut)
	shot.LastFramePath = filepath.Base(lastOut)
	shot.Status = "ready"
	shot.Progress = 100
	shot.Error = ""
	if err := s.save(ctx, p); err != nil {
		return *p, err
	}
	return *p, nil
}

// ----- Assembly ------------------------------------------------------------

func (s *Service) executeAssemble(ctx context.Context, cfg *config.Config, p *Project, dir string) (Project, error) {
	if len(p.Shots) == 0 {
		return *p, errors.New("no shots to assemble")
	}
	// Never rebuild a final while a publication attempt is live — the on-disk
	// artefact backs the recorded ArtifactHash, and a rebuild would either
	// silently invalidate the pending upload or, worse, publish a different
	// video than the one that was recorded/approved.
	switch p.Publication.Status {
	case "uploading":
		return *p, errors.New("publication is uploading; block or confirm it before reassembling")
	case "blocked":
		return *p, errors.New("publication is blocked; resolve it before reassembling")
	case "published":
		// Idempotent: the final is already recorded and published.
		if p.FinalPath != "" {
			return *p, nil
		}
	}
	// Idempotent: already assembled with a recorded hash and no dirty edits
	// (Update() clears FinalPath on structural change). Reuse it.
	if p.FinalPath != "" && p.FinalHash != "" && p.Status == "assembled" {
		return *p, nil
	}
	var clips []string
	var total int
	for i, shot := range p.Shots {
		if shot.Status != "ready" || shot.VideoPath == "" {
			return *p, fmt.Errorf("shot %d (%s) is not ready", i+1, shot.ID)
		}
		abs, err := s.Artifact(ctx, p.ID, shot.VideoPath)
		if err != nil {
			return *p, fmt.Errorf("resolve shot %s clip: %w", shot.ID, err)
		}
		clips = append(clips, abs)
		total += shot.DurationSeconds
	}
	if p.TargetSeconds > 0 {
		// Allow a small drift; assembling far past target is a design bug the
		// caller should resolve before we spend ffmpeg cycles.
		drift := total - p.TargetSeconds
		if drift < 0 {
			drift = -drift
		}
		if drift > max(3, p.TargetSeconds/5) {
			return *p, fmt.Errorf("total shot duration %ds diverges from target %ds by %ds; adjust shots first", total, p.TargetSeconds, drift)
		}
	}
	out := filepath.Join(dir, "final.mp4")
	if err := media.Assemble(ctx, clips, out, videoSize(p.Size)); err != nil {
		p.Status = "error"
		p.Error = redactMedia(err)
		_ = s.save(ctx, p)
		return *p, err
	}
	hash, err := fileSHA256(out)
	if err != nil {
		return *p, err
	}
	fresh, loadErr := s.load(ctx, p.ID)
	if loadErr != nil {
		return *p, loadErr
	}
	fresh.FinalPath = filepath.Base(out)
	fresh.FinalHash = hash
	fresh.Status = "assembled"
	fresh.Error = ""
	// A rebuild reached here only because publication is not_started or the
	// project was already assembled+published and being reset — treat as
	// fresh. Update() forbids publish-transitions after edit.
	if fresh.Publication.Status == "published" {
		fresh.Publication = Publication{Status: "not_started"}
	}
	if err := s.save(ctx, &fresh); err != nil {
		return fresh, err
	}
	return fresh, nil
}

// ----- helpers -------------------------------------------------------------

func findReference(p *Project, id string) int {
	for i := range p.References {
		if p.References[i].ID == id {
			return i
		}
	}
	return -1
}

func findShot(p *Project, id string) int {
	for i := range p.Shots {
		if p.Shots[i].ID == id {
			return i
		}
	}
	return -1
}

func imageSize(s string) string {
	if strings.TrimSpace(s) == "" {
		return "1024x1024"
	}
	return s
}

func videoSize(s string) string {
	if strings.TrimSpace(s) == "" {
		return "720x1280"
	}
	return s
}

func buildReferencePrompt(p Project, r Reference) string {
	var b strings.Builder
	b.WriteString(r.Prompt)
	if strings.TrimSpace(p.Style) != "" {
		b.WriteString("\n\nOverall visual style: ")
		b.WriteString(p.Style)
	}
	b.WriteString("\n\nReference kind: ")
	b.WriteString(r.Kind)
	if strings.TrimSpace(r.Name) != "" {
		b.WriteString(" (")
		b.WriteString(r.Name)
		b.WriteString(")")
	}
	return b.String()
}

func buildShotPrompt(p Project, s Shot) string {
	var b strings.Builder
	b.WriteString(s.Prompt)
	if strings.TrimSpace(p.Style) != "" {
		b.WriteString("\n\nVisual style: ")
		b.WriteString(p.Style)
	}
	if strings.TrimSpace(s.Notes) != "" {
		b.WriteString("\n\nNotes: ")
		b.WriteString(s.Notes)
	}
	return b.String()
}

// shotInputsHash captures the exact inputs that decide the generated media,
// so a mismatch after Update signals stale artefacts.
func shotInputsHash(p Project, s Shot) string {
	sorted := append([]string(nil), s.ReferenceIDs...)
	sort.Strings(sorted)
	h := sha256.New()
	fmt.Fprintf(h, "size=%s\nstyle=%s\nprompt=%s\ncontinuity=%s\nduration=%d\nrefs=%s\nnotes=%s\n",
		p.Size, p.Style, s.Prompt, s.Continuity, s.DurationSeconds, strings.Join(sorted, ","), s.Notes)
	return hex.EncodeToString(h.Sum(nil))
}

// fileSHA256 returns the hex sha256 of a file; used to fingerprint the final
// video so a publication cannot claim a hash it does not have on disk.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// redactMedia strips API-key-shaped tokens from an error string so they do
// not leak into project state or the UI. The media package already redacts
// its own errors; this is a defence-in-depth for anything that slipped past.
func redactMedia(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// Any long alphanumeric run that follows "Bearer " or "sk-" is scrubbed.
	for _, marker := range []string{"Bearer ", "bearer ", "sk-"} {
		start := 0
		for start < len(s) {
			rel := strings.Index(s[start:], marker)
			if rel < 0 {
				break
			}
			i := start + rel
			j := i + len(marker)
			for j < len(s) && !isDelim(s[j]) {
				j++
			}
			replacement := "[redacted]"
			s = s[:i] + replacement + s[j:]
			start = i + len(replacement)
		}
	}
	return s
}

func isDelim(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '"', '\'', ',', ';', ')', ']', '}':
		return true
	}
	return false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// copyFile duplicates src to dst atomically, preserving byte content only.
// Used to seed a continue shot's keyframe from the previous shot's last
// frame without spending a paid image call.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".copy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, dst)
}

// providerFingerprint identifies the video provider a shot's job belongs to.
// (base_url, model) is sufficient: a config swap moves the base URL or model
// and invalidates any recorded job. The API key is intentionally NOT part of
// the fingerprint so a rotated key against the same provider still polls.
func providerFingerprint(ep media.Endpoint) string {
	return strings.ToLower(strings.TrimRight(ep.BaseURL, "/")) + "|" + ep.Model
}

// Only a rejected request is known not to have queued a render; a 5xx may
// have occurred after the provider accepted it.
func isDefiniteReject(err error) bool {
	if err == nil {
		return false
	}
	var status int
	n, _ := fmt.Sscanf(err.Error(), "the video endpoint returned %d", &status)
	return n == 1 && status >= 400 && status < 500
}
