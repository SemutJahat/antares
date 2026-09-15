package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/enowdev/antares/internal/cron"
	"github.com/enowdev/antares/internal/store"
)

// scheduleTool lets the agent set up its own recurring jobs — a morning brief, a
// nightly check, a follow-up — rather than scheduling being something only a
// person can do from the dashboard. Each job runs unattended on its schedule and
// its result is delivered to the configured channel.
type scheduleTool struct{}

func (scheduleTool) Name() string { return "schedule" }

func (scheduleTool) Description() string {
	return "Schedule a recurring task for yourself. `add` creates a job that runs a prompt on a cron schedule " +
		"(5 fields, or @daily/@hourly), `list` shows your jobs, and `remove` deletes one. Use it for anything the " +
		"user wants done repeatedly on a timetable."
}

func (scheduleTool) Schema() map[string]any {
	return schema(map[string]any{
		"action":             propEnum("What to do.", "list", "add", "remove"),
		"name":               prop("string", "For add: a short name for the job."),
		"schedule":           prop("string", "For add: a 5-field cron expression (e.g. \"0 8 * * *\") or @daily/@hourly/@weekly."),
		"prompt":             prop("string", "For add: the task to run each time, written to stand alone."),
		"id":                 prop("string", "For add (optional, upsert existing): the job id. For remove: the job id."),
		"role":               prop("string", "Optional. A specialist role to run the job as (e.g. content-creator, coder). Empty runs as the general assistant."),
		"workspace":          prop("string", "Optional. Working directory the scheduled run should operate in."),
		"content_project_id": prop("string", "Optional. For content-creator jobs, the persisted content project this job targets."),
		"content_stage":      propEnum("Optional. For content-creator jobs, the pipeline stage to run.", "research", "plan", "produce", "publish", "full"),
		"publish_mode":       propEnum("Optional. For content-creator jobs, whether the publish stage may actually post (auto) or must stop at a review-ready draft.", "draft", "auto"),
	}, "action")
}

// RequiresApproval gates it: a scheduled job runs unattended later.
func (scheduleTool) RequiresApproval() bool { return true }

func (scheduleTool) Execute(ctx context.Context, in Input) Result {
	var args struct {
		Action           string `json:"action"`
		Name             string `json:"name"`
		Schedule         string `json:"schedule"`
		Prompt           string `json:"prompt"`
		ID               string `json:"id"`
		Role             string `json:"role"`
		Workspace        string `json:"workspace"`
		ContentProjectID string `json:"content_project_id"`
		ContentStage     string `json:"content_stage"`
		PublishMode      string `json:"publish_mode"`
	}
	if err := in.Bind(&args); err != nil {
		return Errorf("%v", err)
	}
	if in.Deps == nil || in.Deps.Store == nil {
		return Errorf("scheduling is not available in this runtime")
	}
	db := in.Deps.Store

	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "list", "":
		jobs, err := db.ListCronJobs(ctx)
		if err != nil {
			return Errorf("%v", err)
		}
		if len(jobs) == 0 {
			return Text("No scheduled jobs. Add one with action=add.")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d scheduled job(s):\n\n", len(jobs))
		for _, j := range jobs {
			state := "enabled"
			if !j.Enabled {
				state = "disabled"
			}
			next := "—"
			if j.NextRun != nil {
				next = j.NextRun.Format("2006-01-02 15:04")
			}
			fmt.Fprintf(&b, "- %s  %s  [%s]  next %s\n  %s\n", j.ID, j.Name, state, next, truncateTool(j.Prompt, 100))
		}
		return Text(b.String())

	case "add", "new", "upsert":
		if strings.TrimSpace(args.Name) == "" || strings.TrimSpace(args.Schedule) == "" {
			return Errorf("name and schedule are required to add a job")
		}
		role := strings.TrimSpace(args.Role)
		stage := strings.ToLower(strings.TrimSpace(args.ContentStage))
		projectID := strings.TrimSpace(args.ContentProjectID)
		publishMode := strings.ToLower(strings.TrimSpace(args.PublishMode))
		if strings.TrimSpace(args.Prompt) == "" && role == "" {
			return Errorf("prompt is required unless a role is set")
		}
		if stage != "" && !validContentStage(stage) {
			return Errorf("content_stage %q is not one of research|plan|produce|publish|full", args.ContentStage)
		}
		if publishMode != "" && publishMode != "draft" && publishMode != "auto" {
			return Errorf("publish_mode %q is not draft or auto", args.PublishMode)
		}
		if (stage != "" || projectID != "" || publishMode != "") && role == "" {
			role = "content-creator"
		}
		loc := time.Local
		if in.Deps.Config != nil {
			if tz := in.Deps.Config.Agent.Timezone; tz != "" && tz != "Local" {
				if l, err := time.LoadLocation(tz); err == nil {
					loc = l
				}
			}
		}
		next, err := cron.Validate(args.Schedule, loc)
		if err != nil {
			return Errorf("invalid schedule %q: %v", args.Schedule, err)
		}
		now := time.Now()
		id := strings.TrimSpace(args.ID)
		var existing *store.CronJob
		if id != "" {
			if got, err := db.GetCronJob(ctx, id); err == nil {
				existing = got
			}
		}
		var job *store.CronJob
		if existing != nil {
			job = existing
			job.Name = args.Name
			job.Schedule = args.Schedule
			job.Prompt = args.Prompt
			job.Enabled = true
			job.NextRun = &next
			job.UpdatedAt = now
		} else {
			if id == "" {
				id = "cron_" + randHex(6)
			}
			job = &store.CronJob{
				ID: id, Name: args.Name, Schedule: args.Schedule, Prompt: args.Prompt,
				Enabled: true, NextRun: &next, CreatedAt: now, UpdatedAt: now,
			}
		}
		mergeCronMeta(job, role, strings.TrimSpace(args.Workspace), projectID, stage, publishMode)
		if err := db.PutCronJob(ctx, job); err != nil {
			return Errorf("%v", err)
		}
		return Text(fmt.Sprintf("Scheduled %s (%s) — next run %s.", job.ID, args.Name, next.Format("2006-01-02 15:04 MST")))

	case "remove", "delete":
		if strings.TrimSpace(args.ID) == "" {
			return Errorf("id is required to remove a job")
		}
		if err := db.DeleteCronJob(ctx, args.ID); err != nil {
			return Errorf("%v", err)
		}
		return Text("Removed " + args.ID + ".")

	default:
		return Errorf("unknown action %q (want list, add, or remove)", args.Action)
	}
}

// mergeCronMeta preserves prior Meta and overwrites only the fields the caller
// explicitly supplied. Empty strings delete their key so callers can clear a
// value on update; nil Meta is initialised on demand.
func mergeCronMeta(job *store.CronJob, role, workspace, projectID, stage, publishMode string) {
	if job.Meta == nil {
		job.Meta = store.Meta{}
	}
	setOrDelete := func(key, value string) {
		if value == "" {
			delete(job.Meta, key)
			return
		}
		job.Meta[key] = value
	}
	if role != "" {
		job.Meta["role"] = role
	}
	setOrDelete("workspace", workspace)
	setOrDelete("content_project_id", projectID)
	setOrDelete("content_stage", stage)
	setOrDelete("publish_mode", publishMode)
}

func validContentStage(s string) bool {
	switch s {
	case "research", "plan", "produce", "publish", "full":
		return true
	}
	return false
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b)
}
