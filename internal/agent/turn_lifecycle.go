package agent

// turn_lifecycle owns everything around the model/tool loop in run(): the
// preparation that turns a Request into the state the loop reads (session,
// history, tool wiring, system prompt, goal) and the finalisation that closes
// a turn out (title, learning, RAG folding, auto-continue signal). Keeping
// that setup and teardown here lets run() read as what it is — the loop —
// instead of a page of orchestration around a tight core.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/enowdev/antares/internal/llm"
	"github.com/enowdev/antares/internal/store"
	"github.com/enowdev/antares/internal/tools"
)

// runPreparation is the derived state one call to run() reads. It is produced
// once by prepareTurn and then only observed by the loop; the loop mutates
// history in place as turns accumulate.
type runPreparation struct {
	sess         *store.Session
	client       llm.Client
	modelName    string
	providerName string

	history []llm.Message

	toolSpecs []llm.Tool
	byName    map[string]tools.Tool

	systemPrompt string
	maxTurns     int

	goal    *Goal
	hasGoal bool
}

// prepareTurn resolves the session, role, model client, history, tool set,
// and system prompt for one call to run(). It mutates req in place so the
// rest of the turn (write confinement, sub-agents, checkpoint grouping) sees
// the backfilled project binding, stored role, and turn marker.
// run owns error persistence and terminal events, including preparation errors.
func (a *Agent) prepareTurn(ctx context.Context, req *Request, emit Emit) (*runPreparation, error) {
	cfg := a.config()

	sess, err := a.resolveSession(ctx, req)
	if err != nil {
		return nil, err
	}
	// A project binding is stored on the session, so it survives across turns
	// even though the client only sends project_dir on the first message.
	// Backfill the request from it so the rest of the turn — write
	// confinement and any delegated sub-agents — sees the project regardless
	// of which turn this is.
	if pd, _ := sess.Meta["project_dir"].(string); strings.TrimSpace(pd) != "" {
		req.ProjectDir = pd
		// First turn of a project that opted into RAG: index the folder into
		// its own collection in the background. IndexRAG is only set on turn
		// one.
		if req.IndexRAG {
			a.indexProject(sess.ID, pd)
		}
	}

	// A role folds its prompt, toolset, and model into the request. When the
	// request names none, the session's stored role applies — set once with
	// /role and remembered across turns. An explicit request value wins.
	if strings.TrimSpace(req.Role) == "" && a.db != nil {
		if stored, err := a.db.GetKV(ctx, "role:"+sess.ID); err == nil {
			req.Role = stored
		}
	}
	a.applyRole(req)
	if !req.Quiet {
		if err := emit(Event{Type: EventSession, ID: sess.ID, Title: sess.Title}); err != nil {
			return nil, err
		}
	}

	client, modelName, providerName, err := a.newClient(req.Model, sess.ID)
	if err != nil {
		return nil, err
	}

	history, err := a.loadHistory(ctx, sess, *req)
	if err != nil {
		return nil, err
	}

	// Persist the user turn before calling the model so a crash mid-run does
	// not lose it. A pure context-inject turn (no user message) skips this —
	// the note is added just below as hidden context, so there is no empty
	// user bubble.
	// turnMarker groups this turn's file checkpoints under the user message
	// that opened it, so an "edit message" rollback can revert exactly the
	// files this turn (and later ones) changed. It is the persisted user
	// message id.
	turnMarker := ""
	hasUserMsg := strings.TrimSpace(req.Message) != "" || len(req.Images) > 0
	if hasUserMsg {
		userMsg := llm.Message{Role: llm.RoleUser, Content: req.Message, Parts: req.Images}
		if !req.Quiet {
			attachments := ""
			if len(req.Images) > 0 {
				if b, err := json.Marshal(req.Images); err == nil {
					attachments = string(b)
				}
			}
			turnMarker = newID("msg")
			req.turnMarker = turnMarker
			if err := a.db.AppendMessage(ctx, &store.Message{
				ID: turnMarker, SessionID: sess.ID, Role: store.RoleUser,
				Content: req.Message, Attachments: attachments,
			}); err != nil {
				slog.Warn("persist user message failed", "error", err)
			}
		}
		history = append(history, userMsg)
	}

	// Background context (a finished sub-agent's result) is fed to the model
	// as input so the agent resumes and acts on it, but persisted hidden so
	// it is not rendered as a user message — the transcript shows only the
	// agent's continuation, not an injected prompt.
	if strings.TrimSpace(req.ContextInject) != "" {
		history = append(history, llm.Message{Role: llm.RoleUser, Content: req.ContextInject})
		if !req.Quiet {
			if err := a.db.AppendMessage(ctx, &store.Message{
				ID: newID("msg"), SessionID: sess.ID, Role: store.RoleUser,
				Content: req.ContextInject, Hidden: true,
			}); err != nil {
				slog.Warn("persist context inject failed", "error", err)
			}
		}
	}

	activeTools := a.resolveTools(*req)
	toolSpecs := make([]llm.Tool, 0, len(activeTools))
	byName := make(map[string]tools.Tool, len(activeTools))
	for _, t := range activeTools {
		toolSpecs = append(toolSpecs, llm.Tool{Name: t.Name(), Description: t.Description(), Parameters: t.Schema()})
		byName[t.Name()] = t
	}
	// Sub2API Antigravity treats a tool literally named "web_search" as
	// Google's built-in search and rejects mixing it with
	// functionDeclarations. Rename only on the wire for those routes;
	// execution still resolves to web_search.
	_, prov := a.config().ResolveProvider(providerName)
	toolSpecs, byName = sanitizeToolsForProvider(toolSpecs, byName, providerName, prov.BaseURL)

	systemPrompt := a.buildSystemPrompt(ctx, *req, sess, activeTools)

	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = cfg.Agent.MaxTurns
	}
	if maxTurns <= 0 {
		maxTurns = 50
	}

	goal, hasGoal := a.GetGoal(ctx, sess.ID)
	if hasGoal && (goal.Paused || goal.Done) {
		hasGoal = false
	}

	return &runPreparation{
		sess:         sess,
		client:       client,
		modelName:    modelName,
		providerName: providerName,
		history:      history,
		toolSpecs:    toolSpecs,
		byName:       byName,
		systemPrompt: systemPrompt,
		maxTurns:     maxTurns,
		goal:         goal,
		hasGoal:      hasGoal,
	}, nil
}

// finalizeTurn closes a completed run: title the session on the first real
// exchange, reflect on any tool failures the run recovered from, fold the
// exchange into conversation memory, emit EventDone, and — for a top-level
// autonomous goal that is still open — signal the host to start the next
// turn on its own. finalizeTurn always emits EventDone, so callers must not
// emit it themselves.
//
// The EventSession re-emit after titling can fail if the caller has closed
// the stream; that error is returned so run() surfaces it, matching the
// prior inline behaviour.
func (a *Agent) finalizeTurn(
	ctx context.Context,
	req Request,
	sess *store.Session,
	lastReply string,
	failures []toolFailure,
	emit Emit,
) error {
	if !req.Quiet {
		a.maybeTitle(ctx, sess, req.Message, lastReply)
		if err := emit(Event{Type: EventSession, ID: sess.ID, Title: sess.Title}); err != nil {
			return err
		}
		// The agent grows: if it hit tool errors but still produced a reply,
		// it recovered — reflect on those errors in the background and keep
		// any reusable lesson for next time.
		if len(failures) > 0 && strings.TrimSpace(lastReply) != "" {
			go a.learnFromErrors(context.Background(), req.Message, lastReply, failures)
		}
		// Fold this exchange into the conversation memory so later turns and
		// sessions can recall it. Non-blocking, best-effort.
		a.indexTurn(sess, req.Message, lastReply)
		// When per-user RAG is on, also distil what this turn reveals about
		// the gateway sender into their own collection.
		a.indexUserTurn(req, req.Message, lastReply)
	}
	_ = emit(Event{Type: EventDone})

	// Confident autonomous goal: if this top-level turn ended with the goal
	// still unmet and not paused, ask the host to start the next turn on its
	// own. The host owns turn-starting (and the no-overlap queue), so the
	// agent only signals. Skip when a background task is running — that path
	// resumes via OnBackgroundDone instead, and continuing here would
	// double-drive.
	if !req.Quiet && req.Depth == 0 &&
		a.onTurnEnd != nil && !a.bg.hasRunning(sess.ID) {
		if a.ShouldAutoContinueGoal(ctx, sess.ID) {
			a.onTurnEnd(TurnEnded{
				SessionID: sess.ID, Platform: req.Platform,
				ChannelID: req.ChannelID, UserID: req.UserID,
			})
		}
	}
	return nil
}
