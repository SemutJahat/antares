package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/enowdev/antares/internal/agent"
)

// The wake mechanism replaces the old "main agent polls its sub-agents" loop
// with the reverse: a finished background sub-agent pushes its result back to
// the delegating session, and the main agent is resumed to act on it.
//
// Two cases, both funnelled through one per-session queue of pending results:
//   - The session is idle (no live turn): a new turn is started at once, fed the
//     result — the "wake up" the user asked for.
//   - The session is mid-turn (streaming): the result is queued and drained when
//     the current turn ends, so it becomes the next turn without interrupting or
//     losing the streaming turn's context.
type wakeQueue struct {
	mu      sync.Mutex
	pending map[string][]string // session -> queued result notes
	running map[string]bool     // session -> a turn is in flight
}

func newWakeQueue() *wakeQueue {
	return &wakeQueue{pending: map[string][]string{}, running: map[string]bool{}}
}

// formatDone renders a finished sub-agent's outcome as the note that resumes the
// main agent. It is injected as hidden context (not a visible user message), so
// it is phrased as input for the model to act on and keeps enough of the output
// to be actionable.
func formatDone(d agent.BackgroundDone) string {
	who := d.Role
	if who == "" {
		who = "sub-agent"
	}
	head := fmt.Sprintf("[Background sub-agent finished] %s (task %s)", who, d.TaskID)
	if strings.TrimSpace(d.Task) != "" {
		head += "\nTask: " + d.Task
	}
	if strings.TrimSpace(d.Err) != "" {
		return head + "\nResult: FAILED — " + d.Err +
			"\n\nDecide how to proceed given this failure and your current work."
	}
	out := strings.TrimSpace(d.Output)
	if out == "" {
		out = "(the sub-agent produced no final answer)"
	}
	return head + "\nResult:\n" + out +
		"\n\nIncorporate this result into the work. If other sub-agents are still running, keep waiting for them; otherwise continue."
}

// onBackgroundDone is registered on the agent. It turns a finished sub-agent
// into either an immediate wake-up turn or a queued follow-up.
func (s *Server) onBackgroundDone(d agent.BackgroundDone) {
	if d.ParentSession == "" {
		return
	}
	note := formatDone(d)

	s.wake.mu.Lock()
	// If a turn is already running for this session (streaming, or an earlier
	// wake-up still going), queue the note; the running turn drains it on finish.
	if s.wake.running[d.ParentSession] {
		s.wake.pending[d.ParentSession] = append(s.wake.pending[d.ParentSession], note)
		s.wake.mu.Unlock()
		return
	}
	// Also queue if a live run exists that this queue does not know about (a
	// user turn started via handleChat). The hub is the source of truth for
	// "is something streaming right now".
	if s.hub.get(d.ParentSession) != nil {
		s.wake.pending[d.ParentSession] = append(s.wake.pending[d.ParentSession], note)
		s.wake.mu.Unlock()
		return
	}
	s.wake.mu.Unlock()

	// Idle session: wake it with a fresh turn carrying this result.
	s.startWakeTurn(d.ParentSession, note)
}

// startWakeTurn wakes an idle session: it starts a detached turn that resumes
// the main agent on a finished sub-agent's result. The result is fed via
// ContextInject (hidden context), NOT as a user message — so the agent simply
// continues rather than the transcript showing a fake user prompt. Events
// publish into a liveRun so a reattaching client sees the resumed turn, and any
// results that queue up while it runs are folded into one more turn.
func (s *Server) startWakeTurn(session, note string) {
	s.wake.mu.Lock()
	if s.wake.running[session] {
		// Someone else is driving; just queue.
		s.wake.pending[session] = append(s.wake.pending[session], note)
		s.wake.mu.Unlock()
		return
	}
	s.wake.running[session] = true
	s.wake.mu.Unlock()

	lr := newLiveRun()
	s.hub.put(session, lr)

	go func() {
		defer func() {
			lr.finish()
			s.hub.remove(session, lr)
			s.wake.mu.Lock()
			s.wake.running[session] = false
			queued := s.wake.pending[session]
			s.wake.pending[session] = nil
			s.wake.mu.Unlock()
			// Drain: fold every queued result into one more turn.
			if len(queued) > 0 {
				s.startWakeTurn(session, strings.Join(queued, "\n\n---\n\n"))
				return
			}
			// Autonomous goal: agent.OnTurnEnd fires while this turn is still
			// marked running (so it no-ops there); re-check now that the slot is
			// free, and drive the next iteration. The goal's own cap / pause /
			// done state is what ends the loop.
			if s.agent != nil && s.agent.ShouldAutoContinueGoal(context.Background(), session) {
				s.startWakeTurn(session, goalContinueNote)
			}
		}()
		req := agent.Request{
			SessionID:     session,
			ContextInject: note,
			Platform:      "web",
		}
		res, err := s.agent.Run(context.Background(), req, func(e agent.Event) error {
			lr.publish(e)
			return nil
		})
		if err != nil {
			slog.Debug("wake turn failed", "error", err, "session", session)
			return
		}
		// If this is an autonomous goal started from a messaging gateway, deliver
		// the continued turn's reply back to that chat, not only the dashboard.
		if res != nil && strings.TrimSpace(res.Reply) != "" && s.gateway != nil {
			if g, ok := s.agent.GetGoal(context.Background(), session); ok &&
				g.Autonomous && g.Platform != "" && g.ChannelID != "" {
				target := g.Platform + ":" + g.ChannelID
				if derr := s.gateway.Deliver(context.Background(), target, res.Reply); derr != nil {
					slog.Debug("gateway deliver of autonomous goal turn failed", "error", derr, "target", target)
				}
			}
		}
	}()
}

// goalContinueNote is injected (as hidden context, like a wake) to start the
// next iteration of a confident autonomous goal. The standing goal itself is
// already in the system prompt; this only tells the agent to carry on with it
// rather than waiting for the user.
const goalContinueNote = "[Autonomous goal] Continue working towards the standing goal now. " +
	"Do not wait for the user and do not ask whether to proceed — take the next concrete step yourself. " +
	"If you are stuck, change approach: read the documentation, search the web for how to do it, or delegate a research sub-agent."

// onTurnEnd is registered on the agent. When a turn ends with a confident
// autonomous goal still running, it starts the next turn on its own — through
// the same per-session wake queue, so it can never overlap a live turn or a
// sub-agent wake.
func (s *Server) onTurnEnd(e agent.TurnEnded) {
	if e.SessionID == "" {
		return
	}
	// If a turn is already running/queued for this session, do nothing: whatever
	// is driving will end and this same check runs again. Only an idle session
	// needs a fresh turn started here.
	s.wake.mu.Lock()
	running := s.wake.running[e.SessionID]
	s.wake.mu.Unlock()
	if running || s.hub.get(e.SessionID) != nil {
		return
	}
	s.startWakeTurn(e.SessionID, goalContinueNote)
}

// resumeAutonomousGoals restarts confident autonomous goals that were still
// running when the server last stopped, so an unattended goal survives a
// restart. Best-effort: a failure here must never block startup.
func (s *Server) resumeAutonomousGoals() {
	if s.agent == nil || s.db == nil {
		return
	}
	ctx := context.Background()
	goals, err := s.db.ListKV(ctx, "goal:")
	if err != nil {
		slog.Debug("resume autonomous goals: list failed", "error", err)
		return
	}
	for key := range goals {
		sessionID := strings.TrimPrefix(key, "goal:")
		if sessionID == "" {
			continue
		}
		g, ok := s.agent.GetGoal(ctx, sessionID)
		if !ok || g == nil || !g.Autonomous || g.Done || g.Paused {
			continue
		}
		slog.Info("resuming autonomous goal after restart", "session", sessionID)
		s.startWakeTurn(sessionID, goalContinueNote)
	}
}

// drainAfterTurn is called when a user-driven turn ends. If sub-agent results
// queued up while it streamed, it starts a wake turn to act on them — this is
// the "inject as the next turn" path for the streaming case.
func (s *Server) drainAfterTurn(session string) {
	if session == "" {
		return
	}
	s.wake.mu.Lock()
	queued := s.wake.pending[session]
	s.wake.pending[session] = nil
	running := s.wake.running[session]
	s.wake.mu.Unlock()
	if len(queued) == 0 || running {
		return
	}
	s.startWakeTurn(session, strings.Join(queued, "\n\n---\n\n"))
}
