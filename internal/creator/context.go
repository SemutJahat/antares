package creator

import "context"

// runContext carries the active run's stage/publish mode into Execute so a
// tool call made during a research/plan stage cannot spend money on video
// generation or publish behind the operator's back.
type runContext struct {
	ProjectID   string
	Stage       string // research|plan|produce|publish|full
	PublishMode string // draft|auto
	SessionID   string
}

type runContextKey struct{}

// WithRun attaches a run scope to ctx. Cron and the HTTP run handler wrap the
// per-turn context with this so every content_creator tool call — and every
// Service.Execute call the agent triggers — is bounded by the stage the
// operator authorised.
//
// Callers preallocate the sessionID (so it lines up with BeginRun's owner
// tracking) and pass it in; a mismatched sessionID on Execute/Update is what
// stops a stray tab from mutating a project another run holds.
//
// Passing an empty stage is treated as no active run (tool-driven, ad-hoc
// mutation): the strictest gates still apply (publish still needs
// publish_mode=auto or an explicit approved flag), but generation is allowed.
func WithRun(ctx context.Context, projectID, stage, publishMode, sessionID string) context.Context {
	return context.WithValue(ctx, runContextKey{}, runContext{
		ProjectID:   projectID,
		Stage:       stage,
		PublishMode: publishMode,
		SessionID:   sessionID,
	})
}

// RunFromContext returns the active run scope, or the zero value when there
// is none.
func RunFromContext(ctx context.Context) (runContext, bool) {
	if ctx == nil {
		return runContext{}, false
	}
	rc, ok := ctx.Value(runContextKey{}).(runContext)
	return rc, ok
}

// stageAllows reports whether the current stage permits an action. Actions
// map to stage buckets:
//
//   - "media"   -> reference/keyframe/video/poll/assemble
//   - "publish" -> prepare/confirm publish (block always permitted)
//
// Ad-hoc (no active run) is permissive for media; publish is enforced by the
// PreparePublish approve/publish_mode check itself.
func stageAllows(ctx context.Context, action string) (bool, string) {
	rc, ok := RunFromContext(ctx)
	if !ok {
		return true, ""
	}
	stage := rc.Stage
	switch action {
	case "media":
		switch stage {
		case "research", "plan":
			return false, "current run stage is " + stage + "; media generation is not allowed until produce"
		}
		return true, ""
	case "publish":
		if rc.PublishMode != "auto" {
			return false, "run publish_mode is draft; publishing requires publish_mode=auto"
		}
		switch stage {
		case "research", "plan", "produce":
			return false, "current run stage is " + stage + "; publishing is not allowed until the publish stage"
		}
		return true, ""
	}
	return true, ""
}
