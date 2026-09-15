// Package creator persists Content Creator projects and drives the video
// production workflow (references, keyframes, clips, assembly, publishing).
//
// State lives in two places: authoritative JSON per project under the KV store
// (durable across process restarts) and generated media under an isolated root
// directory (config.Path("content-creator")/<projectID>/…). The service never
// stores plaintext credentials, never trusts caller-supplied filesystem paths
// beyond the registered artefact set, and refuses to re-run a paid media job
// that already has a pending provider id.
package creator

import "time"

// Project is one video the agent is producing. Everything the UI, the agent
// and the cron scheduler need to know about the shoot lives here; the media
// service reads it through Get and writes it through Update/Execute.
type Project struct {
	ID             string      `json:"id"`
	Title          string      `json:"title"`
	Brief          string      `json:"brief"`
	Platform       string      `json:"platform"`
	AccountID      string      `json:"account_id"`
	Style          string      `json:"style"`
	Size           string      `json:"size"`
	TargetSeconds  int         `json:"target_seconds"`
	Caption        string      `json:"caption"`
	Revision       int         `json:"revision"`
	PublishMode    string      `json:"publish_mode"` // draft|auto
	Status         string      `json:"status"`
	Error          string      `json:"error"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	References     []Reference `json:"references"`
	Shots          []Shot      `json:"shots"`
	Research       []Trend     `json:"research"`
	Ideas          []Idea      `json:"ideas"`
	FinalPath      string      `json:"final_path"`
	FinalHash      string      `json:"final_hash,omitempty"`
	Publication    Publication `json:"publication"`
	RunSessionID   string      `json:"run_session_id,omitempty"`
	RunStage       string      `json:"run_stage,omitempty"`
	RunPublishMode string      `json:"run_publish_mode,omitempty"`
	RunStatus      string      `json:"run_status,omitempty"` // idle|running|error
	LastRunError   string      `json:"last_run_error,omitempty"`
	RunStartedAt   string      `json:"run_started_at,omitempty"`
	RunFinishedAt  string      `json:"run_finished_at,omitempty"`
}

// Reference is a stable visual anchor (a character, setting, style or prop)
// the agent generates once and reuses across shots. Each reference lives in
// the project directory as a PNG the shots' keyframes may pass to the image
// endpoint as an inspiration image.
type Reference struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // character|setting|style|prop
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	Path   string `json:"path"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// Shot is one clip in the ordered timeline. Continuity is either "cut" (the
// keyframe is generated fresh from References) or "continue" (the keyframe is
// the previous shot's last frame).
type Shot struct {
	ID              string   `json:"id"`
	Prompt          string   `json:"prompt"`
	ReferenceIDs    []string `json:"reference_ids"`
	DurationSeconds int      `json:"duration_seconds"`
	Continuity      string   `json:"continuity"` // cut|continue
	Notes           string   `json:"notes"`
	KeyframePath    string   `json:"keyframe_path"`
	VideoJobID      string   `json:"video_job_id"`
	VideoPath       string   `json:"video_path"`
	LastFramePath   string   `json:"last_frame_path"`
	Status          string   `json:"status"`
	Error           string   `json:"error"`
	Progress        int      `json:"progress"`
	// PromptHash captures the (prompt, references, continuity) inputs that
	// produced the generated media. A caller-facing Update that changes any
	// input clears the derived artefacts so a stale clip cannot masquerade as
	// belonging to the new prompt.
	PromptHash string `json:"prompt_hash,omitempty"`
	// ProviderSnapshot pins the provider identity that owns this shot's
	// current VideoJobID. Poll/refetch MUST reject a mismatched config so a
	// swapped video provider cannot poll a foreign job or expose a different
	// bearer against the previous provider's host.
	ProviderSnapshot string `json:"provider_snapshot,omitempty"`
}

// Trend is one observed source of what is trending on the platform. The
// agent MUST NOT invent metrics; only URL, title and observed_at are required.
type Trend struct {
	URL        string             `json:"url"`
	Title      string             `json:"title"`
	Platform   string             `json:"platform"`
	ObservedAt string             `json:"observed_at"`
	Notes      string             `json:"notes"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
}

// Idea is one candidate story arc the agent proposed. Exactly one is
// typically flagged Selected before Produce runs.
type Idea struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Hook      string `json:"hook"`
	Rationale string `json:"rationale"`
	Selected  bool   `json:"selected"`
}

// Publication is the state of the current upload attempt to a social account.
// Status transitions: not_started -> uploading -> published | blocked.
type Publication struct {
	Status       string `json:"status"` // not_started|uploading|published|blocked
	PostURL      string `json:"post_url"`
	Error        string `json:"error"`
	AccountID    string `json:"account_id"`
	ArtifactHash string `json:"artifact_hash"`
	// Proof is an observed browser evidence blurb the agent must supply on
	// confirm; empty means the publication is not verified even if a URL is
	// set. Never invent this — it is what stops the tool from lying about a
	// successful upload.
	Proof       string `json:"proof,omitempty"`
	PreparedAt  string `json:"prepared_at,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
}

// runNow gives operations a deterministic UTC timestamp for JSON fields.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
