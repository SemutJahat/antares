package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/creator"
)

// contentCreatorTool drives the Content Creator service from inside an agent
// turn. It is the only path the agent has to spend money on video generation
// or to publish a finished clip.
//
// Every mutating action goes through creator.Service so run-scope and paid-job
// guards fire consistently whether the caller is a chat, a cron job or the
// web UI.
type contentCreatorTool struct{}

func (contentCreatorTool) Name() string { return "content_creator" }

func (contentCreatorTool) Description() string {
	return "Manage Content Creator video projects: list, get, create, update, generate references and keyframes, submit video jobs, poll them, assemble a final MP4, and prepare/confirm/block a publication. " +
		"The service persists everything and enforces run-scope guards — a research/plan run cannot spend money, and publish requires publish_mode=auto or an explicit approved flag. " +
		"Never resubmit a video job on retry; use action=poll_video. If a video job is left in status=submission_unknown or provider_snapshot no longer matches, do not retry — the operator resolves it through the dashboard's Reset action. " +
		"Never claim a publish succeeded without an observed post URL on the project's platform host and a proof blurb captured from the browser DOM."
}

func (contentCreatorTool) RequiresApproval() bool { return true }

func (contentCreatorTool) Schema() map[string]any {
	referenceItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":     map[string]any{"type": "string", "description": "Stable id, [a-zA-Z0-9_-], unique within references."},
			"kind":   map[string]any{"type": "string", "enum": []string{"character", "setting", "style", "prop"}},
			"name":   map[string]any{"type": "string"},
			"prompt": map[string]any{"type": "string", "description": "Full generation prompt. Update changes clear the generated path."},
			"path":   map[string]any{"type": "string", "description": "Set by the service after generate_reference — never write this from the client."},
			"status": map[string]any{"type": "string", "description": "Set by service: draft|generating|ready|error."},
			"error":  map[string]any{"type": "string", "description": "Set by service on failure."},
		},
		"required": []string{"id", "kind", "prompt"},
	}
	shotItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":               map[string]any{"type": "string", "description": "Stable id, unique within shots."},
			"prompt":           map[string]any{"type": "string"},
			"reference_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Reference ids consumed by a cut shot. Ignored by continue shots."},
			"duration_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120},
			"continuity":       map[string]any{"type": "string", "enum": []string{"cut", "continue"}, "description": "cut: keyframe generated from references. continue: keyframe copied from the previous shot's last_frame_path (no paid image call)."},
			"notes":            map[string]any{"type": "string"},
			"keyframe_path":    map[string]any{"type": "string", "description": "Set by service."},
			"video_job_id":     map[string]any{"type": "string", "description": "Set by service on generate_video. Never edit."},
			"video_path":       map[string]any{"type": "string", "description": "Set by service on poll_video completion."},
			"last_frame_path":  map[string]any{"type": "string", "description": "Set by service on poll_video completion; feeds the next continue shot."},
			"status":           map[string]any{"type": "string", "description": "Set by service: draft|generating_keyframe|keyframe_ready|generating_video|polling|ready|ready_no_lastframe|failed|error|submission_unknown."},
			"error":            map[string]any{"type": "string"},
			"progress":         map[string]any{"type": "integer"},
		},
		"required": []string{"id", "prompt", "duration_seconds", "continuity"},
	}
	trendItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":         map[string]any{"type": "string", "description": "HTTPS source URL of the observed trend — no invented sources."},
			"title":       map[string]any{"type": "string"},
			"platform":    map[string]any{"type": "string"},
			"observed_at": map[string]any{"type": "string", "description": "RFC3339 timestamp when you actually observed the source."},
			"notes":       map[string]any{"type": "string"},
			"metrics":     map[string]any{"type": "object", "description": "Only numbers you actually read; never fabricate view/like counts."},
		},
		"required": []string{"url", "title", "observed_at"},
	}
	ideaItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":        map[string]any{"type": "string"},
			"title":     map[string]any{"type": "string"},
			"hook":      map[string]any{"type": "string"},
			"rationale": map[string]any{"type": "string"},
			"selected":  map[string]any{"type": "boolean", "description": "Exactly one idea should be selected before produce runs."},
		},
		"required": []string{"id", "title"},
	}
	projectObject := map[string]any{
		"type":        "object",
		"description": "The Project JSON. For create, id/revision/status/created_at/updated_at/final_path/final_hash/publication/run_* are set by the service — supply the plan. For update, include the full current object with the same revision you loaded via get (revision conflicts reject the write).",
		"properties": map[string]any{
			"id":             map[string]any{"type": "string", "description": "Set by service on create."},
			"title":          map[string]any{"type": "string"},
			"brief":          map[string]any{"type": "string", "description": "One or two paragraphs describing the video's story, tone and audience."},
			"platform":       map[string]any{"type": "string", "description": "Target platform (tiktok, instagram, youtube, x, ...)."},
			"account_id":     map[string]any{"type": "string", "description": "Social account id from social_account list."},
			"style":          map[string]any{"type": "string", "description": "Overall visual style, applied to every reference and shot prompt."},
			"size":           map[string]any{"type": "string", "description": "Even WIDTHxHEIGHT between 16 and 4096, e.g. 720x1280."},
			"target_seconds": map[string]any{"type": "integer", "description": "Target total duration. Assembly rejects a plan that diverges by more than max(3, target/5)."},
			"caption":        map[string]any{"type": "string", "description": "Caption for the published post."},
			"revision":       map[string]any{"type": "integer", "description": "Optimistic-locking token. Pass the value you got from get; the service rejects a stale write."},
			"publish_mode":   map[string]any{"type": "string", "enum": []string{"draft", "auto"}},
			"references":     map[string]any{"type": "array", "items": referenceItem},
			"shots":          map[string]any{"type": "array", "items": shotItem},
			"research":       map[string]any{"type": "array", "items": trendItem},
			"ideas":          map[string]any{"type": "array", "items": ideaItem},
		},
		"required": []string{"title", "brief", "platform", "size", "target_seconds", "publish_mode"},
	}

	return schema(map[string]any{
		"action": propEnum(
			"What to do.",
			"list", "get", "create", "update",
			"generate_reference", "generate_keyframe", "generate_video", "poll_video",
			"assemble",
			"prepare_publish", "confirm_publish", "block_publish",
		),
		"project_id": prop("string", "The project id for get/update/media/publish actions."),
		"project":    projectObject,
		"target_id":  prop("string", "The reference id (generate_reference) or shot id (generate_keyframe/video/poll_video)."),
		"approved":   prop("boolean", "Prepare-publish only: explicit operator approval, required when publish_mode is draft. Does not override run-stage guards."),
		"post_url":   prop("string", "Confirm-publish only: the observed live post URL. Must be on the project's platform host, HTTPS."),
		"proof":      prop("string", "Confirm-publish only: a short blurb of browser-observed evidence (page title, DOM excerpt, screenshot description). Never invent this."),
		"reason":     prop("string", "Block-publish only: why publication is blocked."),
	}, "action")
}

// Execute dispatches the action against the Service. The Service instance is
// constructed per call; it holds no per-request state — just db+root — so
// this is cheap and always sees the freshest config.
func (contentCreatorTool) Execute(ctx context.Context, in Input) Result {
	var args struct {
		Action    string          `json:"action"`
		ProjectID string          `json:"project_id"`
		Project   json.RawMessage `json:"project"`
		TargetID  string          `json:"target_id"`
		Approved  bool            `json:"approved"`
		PostURL   string          `json:"post_url"`
		Proof     string          `json:"proof"`
		Reason    string          `json:"reason"`
	}
	if err := in.Bind(&args); err != nil {
		return Errorf("%v", err)
	}
	if in.Deps == nil || in.Deps.Store == nil || in.Deps.Config == nil {
		return Errorf("content_creator is not configured (store/config missing)")
	}
	svc := creator.New(in.Deps.Store, config.Path("content-creator"))
	action := strings.TrimSpace(strings.ToLower(args.Action))

	switch action {
	case "list":
		items, err := svc.List(ctx)
		if err != nil {
			return Errorf("list projects: %v", err)
		}
		return jsonResult(map[string]any{"projects": items}, fmt.Sprintf("%d project(s).", len(items)))

	case "get":
		if args.ProjectID == "" {
			return Errorf("project_id is required")
		}
		p, err := svc.Get(ctx, args.ProjectID)
		if err != nil {
			return Errorf("get: %v", err)
		}
		return jsonResult(p, fmt.Sprintf("Project %s (%s).", p.ID, p.Status))

	case "create":
		if len(args.Project) == 0 {
			return Errorf("project object is required")
		}
		var p creator.Project
		if err := json.Unmarshal(args.Project, &p); err != nil {
			return Errorf("project JSON: %v", err)
		}
		out, err := svc.Create(ctx, p)
		if err != nil {
			return Errorf("create: %v", err)
		}
		return jsonResult(out, fmt.Sprintf("Created project %s.", out.ID))

	case "update":
		if args.ProjectID == "" {
			return Errorf("project_id is required")
		}
		if len(args.Project) == 0 {
			return Errorf("project object is required")
		}
		var p creator.Project
		if err := json.Unmarshal(args.Project, &p); err != nil {
			return Errorf("project JSON: %v", err)
		}
		out, err := svc.Update(ctx, args.ProjectID, p)
		if err != nil {
			return errorResult("update", err, out)
		}
		return jsonResult(out, fmt.Sprintf("Updated project %s (rev %d).", out.ID, out.Revision))

	case "generate_reference", "generate_keyframe", "generate_video", "poll_video", "assemble":
		if args.ProjectID == "" {
			return Errorf("project_id is required")
		}
		if action != "assemble" && args.TargetID == "" {
			return Errorf("target_id is required for %s", action)
		}
		in.Emit(Progress{Tool: "content_creator", Message: action + "…"})
		out, err := svc.Execute(ctx, in.Deps.Config, args.ProjectID, action, args.TargetID)
		if err != nil {
			return errorResult(action, err, out)
		}
		if action == "poll_video" {
			if i := indexShotCC(out, args.TargetID); i >= 0 && out.Shots[i].Status == "polling" {
				select {
				case <-ctx.Done():
					return Errorf("video polling interrupted: %v", ctx.Err())
				case <-time.After(5 * time.Second):
				}
			}
		}
		return jsonResult(out, describeAction(action, out, args.TargetID))

	case "prepare_publish":
		if args.ProjectID == "" {
			return Errorf("project_id is required")
		}
		pub, path, err := svc.PreparePublish(ctx, args.ProjectID, args.Approved)
		if err != nil {
			return Errorf("prepare_publish: %v", err)
		}
		// Also surface the project snapshot so the UI stays in sync with the
		// new "uploading" state without a second GET.
		snap, _ := svc.Get(ctx, args.ProjectID)
		payload := map[string]any{
			"project":     snap,
			"publication": pub,
			"path":        path,
			"project_id":  args.ProjectID,
			"platform":    snap.Platform,
			"account_id":  snap.AccountID,
			"caption":     snap.Caption,
		}
		return jsonResult(payload, fmt.Sprintf(
			"Ready to upload %s on %s using account %s. After the browser upload, call confirm_publish with the observed post URL and a proof blurb from the browser.",
			path, snap.Platform, snap.AccountID))

	case "confirm_publish":
		if args.ProjectID == "" {
			return Errorf("project_id is required")
		}
		post := strings.TrimSpace(args.PostURL)
		proof := strings.TrimSpace(args.Proof)
		if post == "" || proof == "" {
			return Errorf("post_url and proof are both required; do not fabricate them")
		}
		// Look up the recorded final hash so the tool cannot invent one.
		p, err := svc.Get(ctx, args.ProjectID)
		if err != nil {
			return Errorf("confirm_publish: %v", err)
		}
		if p.FinalHash == "" {
			return Errorf("the project has no assembled final video hash; assemble before publishing")
		}
		out, err := svc.ConfirmPublish(ctx, args.ProjectID, post, proof, p.FinalHash)
		if err != nil {
			return errorResult("confirm_publish", err, out)
		}
		return jsonResult(out, fmt.Sprintf("Published %s to %s.", out.ID, out.Publication.PostURL))

	case "block_publish":
		if args.ProjectID == "" {
			return Errorf("project_id is required")
		}
		if strings.TrimSpace(args.Reason) == "" {
			return Errorf("reason is required to block a publication")
		}
		out, err := svc.BlockPublish(ctx, args.ProjectID, args.Reason)
		if err != nil {
			return Errorf("block_publish: %v", err)
		}
		return jsonResult(out, fmt.Sprintf("Publication blocked for %s: %s", out.ID, args.Reason))
	}
	return Errorf("unknown action %q", action)
}

// describeAction picks a short human line for the tool's Content field
// depending on which media action ran, so the agent knows what to say next.
func describeAction(action string, p creator.Project, target string) string {
	switch action {
	case "generate_reference":
		i := indexReferenceCC(p, target)
		if i < 0 {
			return fmt.Sprintf("Reference %s: no such id.", target)
		}
		r := p.References[i]
		if r.Status == "ready" {
			return fmt.Sprintf("Reference %s ready at %s.", r.ID, r.Path)
		}
		return fmt.Sprintf("Reference %s: %s (%s).", r.ID, r.Status, r.Error)
	case "generate_keyframe":
		i := indexShotCC(p, target)
		if i < 0 {
			return fmt.Sprintf("Shot %s: no such id.", target)
		}
		s := p.Shots[i]
		return fmt.Sprintf("Shot %s: %s.", s.ID, s.Status)
	case "generate_video":
		i := indexShotCC(p, target)
		if i < 0 {
			return fmt.Sprintf("Shot %s: no such id.", target)
		}
		s := p.Shots[i]
		return fmt.Sprintf("Shot %s: video job %s submitted. Poll with action=poll_video.", s.ID, s.VideoJobID)
	case "poll_video":
		i := indexShotCC(p, target)
		if i < 0 {
			return fmt.Sprintf("Shot %s: no such id.", target)
		}
		s := p.Shots[i]
		if s.Status == "ready" {
			return fmt.Sprintf("Shot %s ready at %s (last frame %s).", s.ID, s.VideoPath, s.LastFramePath)
		}
		return fmt.Sprintf("Shot %s: %s (%d%%). Poll again shortly.", s.ID, s.Status, s.Progress)
	case "assemble":
		if p.FinalPath != "" {
			return fmt.Sprintf("Assembled %s.", p.FinalPath)
		}
		return fmt.Sprintf("Assemble finished but no final path recorded: %s", p.Error)
	}
	return fmt.Sprintf("%s: %s", action, p.Status)
}

func indexReferenceCC(p creator.Project, id string) int {
	for i := range p.References {
		if p.References[i].ID == id {
			return i
		}
	}
	return -1
}

func indexShotCC(p creator.Project, id string) int {
	for i := range p.Shots {
		if p.Shots[i].ID == id {
			return i
		}
	}
	return -1
}

// jsonResult renders the current project as JSON in Meta.project for
// downstream tools, and returns the human blurb as Content.
func jsonResult(payload any, blurb string) Result {
	buf, err := json.Marshal(payload)
	if err != nil {
		return Errorf("encode result: %v", err)
	}
	return Result{
		Content: blurb + "\n\n" + string(buf),
		Meta:    map[string]any{"payload": json.RawMessage(buf)},
	}
}

// errorResult returns a failed result while surfacing the current project
// snapshot so the caller (or a live UI) can render up-to-date state.
func errorResult(action string, err error, p creator.Project) Result {
	body := fmt.Sprintf("%s: %v", action, err)
	if p.ID != "" {
		buf, _ := json.Marshal(p)
		return Result{
			Content: body + "\n\n" + string(buf),
			IsError: true,
			Meta:    map[string]any{"payload": json.RawMessage(buf)},
		}
	}
	return Result{Content: body, IsError: true}
}

// ensure the tool implements the interfaces the registry inspects.
var _ Tool = contentCreatorTool{}
var _ Approval = contentCreatorTool{}
