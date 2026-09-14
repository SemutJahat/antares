package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/llm"
	"github.com/enowdev/antares/internal/store"
	"github.com/enowdev/antares/internal/tools"
)

// noaskAgent builds an Agent with a live tool registry containing the real
// ask_user tool so resolveTools and executeTools exercise the production path.
func noaskAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Tools.ApprovalMode = "auto"
	a := agentWithConfig(cfg)
	a.reg = tools.NewRegistry()
	ask, ok := tools.Default().Get("ask_user")
	if !ok {
		t.Fatal("ask_user tool missing from tools.Default() — registry init changed?")
	}
	a.reg.Register(ask)
	return a
}

// TestResolveToolsHidesAskUserFromSubordinates: the tool schema handed to the
// model must omit ask_user for every subordinate identity and keep it for a
// real top-level run.
func TestResolveToolsHidesAskUserFromSubordinates(t *testing.T) {
	a := noaskAgent(t)
	a.config().Tools.Toolset = "coding"

	cases := []struct {
		name    string
		req     Request
		wantAsk bool
	}{
		{"top-level web", Request{Platform: "web", Depth: 0}, true},
		{"top-level cli", Request{Platform: "", Depth: 0}, true},
		{"delegated subagent", Request{Platform: "subagent", Depth: 1}, false},
		{"background task", Request{Platform: "background", Depth: 1}, false},
		{"nested depth only", Request{Platform: "web", Depth: 2}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := map[string]bool{}
			for _, tl := range a.resolveTools(tc.req) {
				names[tl.Name()] = true
			}
			if got := names["ask_user"]; got != tc.wantAsk {
				t.Fatalf("ask_user exposed = %v, want %v", got, tc.wantAsk)
			}
		})
	}
}

// TestSubordinateAskDispatchRejectsWithoutRegisteringPendingAsk: the
// load-bearing check. Even if a bad model hallucinates ask_user despite it
// being dropped from its schema, executeTools must refuse the call
// synchronously and leave the ask desk empty — never waiting for a person who
// is not there.
func TestSubordinateAskDispatchRejectsWithoutRegisteringPendingAsk(t *testing.T) {
	a := noaskAgent(t)
	askDeskBefore := len(a.PendingAsks())

	ask, _ := tools.Default().Get("ask_user")
	byName := map[string]tools.Tool{"ask_user": ask}
	calls := []llm.ToolCall{
		{ID: "call_ask", Name: "ask_user", Arguments: `{"question":"which env?"}`},
	}
	sess := &store.Session{ID: "sub_sess"}

	var mu sync.Mutex
	events := []Event{}
	emit := func(e Event) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
		return nil
	}

	done := make(chan []toolOutcome, 1)
	go func() {
		done <- a.executeTools(context.Background(), calls, byName,
			Request{Platform: "subagent", Depth: 1, SessionID: sess.ID}, sess, emit)
	}()

	select {
	case res := <-done:
		if len(res) != 1 {
			t.Fatalf("want 1 outcome, got %d", len(res))
		}
		if !res[0].isError {
			t.Fatalf("subordinate ask_user must be an error outcome, got: %+v", res[0])
		}
		if !strings.Contains(res[0].message.Content, "ask_user is not available") {
			t.Fatalf("expected refusal explanation, got: %q", res[0].message.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subordinate ask_user dispatch blocked instead of being refused")
	}

	if got := len(a.PendingAsks()); got != askDeskBefore {
		t.Fatalf("subordinate refusal must not register a pending ask; before=%d after=%d",
			askDeskBefore, got)
	}
}

// TestTopLevelAskStillReachesTheDesk: a real top-level run (Depth=0,
// human-facing platform) must still open a pending ask and receive the
// resolved answer. Without this the fix could over-shoot and mute every ask.
func TestTopLevelAskStillReachesTheDesk(t *testing.T) {
	a := noaskAgent(t)
	bridge := a.askBridge("top_sess", func(Event) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	answered := make(chan string, 1)
	go func() {
		ans, err := bridge(ctx, []tools.AskQuestion{{Question: "which env?"}})
		if err != nil {
			answered <- "ERR:" + err.Error()
			return
		}
		answered <- ans
	}()

	// Wait (bounded) for the ask to register.
	deadline := time.Now().Add(1 * time.Second)
	var mine string
	for time.Now().Before(deadline) && mine == "" {
		for _, p := range a.PendingAsks() {
			if p.SessionID == "top_sess" {
				mine = p.ID
				break
			}
		}
		if mine == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if mine == "" {
		t.Fatal("top-level bridge must register a pending ask")
	}
	if !a.ResolveAsk(mine, "staging") {
		t.Fatal("ResolveAsk returned false for a live pending id")
	}
	select {
	case got := <-answered:
		if got != "staging" {
			t.Fatalf("bridge returned %q, want staging", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("top-level bridge did not deliver the resolved answer")
	}
}
