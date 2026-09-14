package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/store"
)

// A failed turn must survive a reload: the dashboard renders and retries from
// the stored row, and the model must never read that row back as its own words.

func errorAgent(t *testing.T) (*Agent, *store.Session) {
	t.Helper()
	db, err := store.Open(context.Background(), "memory", "", 1, 5000, false)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	a := agentWithConfig(config.Default())
	a.db = db
	sess := &store.Session{ID: "ses_err", Platform: "web", Meta: store.Meta{}}
	if err := db.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return a, sess
}

func TestReportTurnErrorPersistsJSONOnlyPayload(t *testing.T) {
	a, sess := errorAgent(t)
	ctx := context.Background()

	var streamed []Event
	a.reportTurnError(ctx, Request{SessionID: sess.ID, Message: "build the thing"},
		errors.New("provider refused: 429"), func(e Event) error {
			streamed = append(streamed, e)
			return nil
		})

	if len(streamed) != 1 || streamed[0].Type != EventError {
		t.Fatalf("want a single error event, got %+v", streamed)
	}
	want := `{"error":"provider refused: 429"}`
	if streamed[0].Err != want {
		t.Fatalf("streamed payload = %q, want %q", streamed[0].Err, want)
	}

	rows, err := a.db.ListMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	// The prompt is persisted alongside the failure so Retry has something to
	// resend after a reload.
	if len(rows) != 2 || rows[0].Role != store.RoleUser || rows[0].Content != "build the thing" {
		t.Fatalf("want the failed prompt persisted first, got %+v", rows)
	}
	stored := rows[1]
	if stored.Content != want {
		t.Fatalf("stored content = %q, want the same JSON as the stream", stored.Content)
	}
	if !isPersistedTurnError(stored) {
		t.Fatalf("stored row is not flagged as a turn error: %+v", stored.Meta)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stored.Content), &decoded); err != nil {
		t.Fatalf("stored content is not JSON: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("payload carries extra fields %v; only the error text may be exposed", decoded)
	}
}

// A cancelled turn is the one place the write must still land: the context that
// died is the same one the caller passes in.
func TestReportTurnErrorPersistsAfterContextCancellation(t *testing.T) {
	a, sess := errorAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a.reportTurnError(ctx, Request{SessionID: sess.ID}, errors.New("timed out"), nil)

	rows, err := a.db.ListMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(rows) != 1 || !isPersistedTurnError(rows[0]) {
		t.Fatalf("want one persisted failure despite a dead context, got %+v", rows)
	}
}

// A sub-agent reports upward through the delegating tool, so its failure must
// not appear in the parent's transcript.
func TestReportTurnErrorSkipsQuietRuns(t *testing.T) {
	a, sess := errorAgent(t)

	a.reportTurnError(context.Background(),
		Request{SessionID: sess.ID, Quiet: true}, errors.New("sub-agent failed"), nil)

	rows, err := a.db.ListMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("quiet run persisted %d row(s); sub-agent failures belong to the caller", len(rows))
	}
}

func TestLoadHistoryExcludesPersistedTurnErrors(t *testing.T) {
	a, sess := errorAgent(t)
	ctx := context.Background()

	for _, m := range []*store.Message{
		{ID: "m1", SessionID: sess.ID, Role: store.RoleUser, Content: "do the work"},
		{ID: "m2", SessionID: sess.ID, Role: store.RoleAssistant,
			Content: `{"error":"provider refused: 429"}`, Meta: store.Meta{"is_error": true}},
		{ID: "m3", SessionID: sess.ID, Role: store.RoleUser, Content: "try again"},
	} {
		if err := a.db.AppendMessage(ctx, m); err != nil {
			t.Fatalf("append %s: %v", m.ID, err)
		}
	}

	history, err := a.loadHistory(ctx, sess, Request{})
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2 (the failure row must be skipped): %+v", len(history), history)
	}
	for _, m := range history {
		if m.Content == `{"error":"provider refused: 429"}` {
			t.Fatal("the model was shown its own failure row as history")
		}
	}
}

// Tool results carry the same is_error flag but are genuine context the model
// needs; only assistant failure rows are hidden.
func TestLoadHistoryKeepsErroredToolResults(t *testing.T) {
	a, sess := errorAgent(t)
	ctx := context.Background()

	calls := `[{"id":"call_1","name":"terminal","arguments":"{}"}]`
	for _, m := range []*store.Message{
		{ID: "m1", SessionID: sess.ID, Role: store.RoleUser, Content: "run it"},
		{ID: "m2", SessionID: sess.ID, Role: store.RoleAssistant, ToolCalls: calls},
		{ID: "m3", SessionID: sess.ID, Role: store.RoleTool, Content: "exit status 1",
			ToolCallID: "call_1", ToolName: "terminal", Meta: store.Meta{"is_error": true}},
	} {
		if err := a.db.AppendMessage(ctx, m); err != nil {
			t.Fatalf("append %s: %v", m.ID, err)
		}
	}

	history, err := a.loadHistory(ctx, sess, Request{})
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	found := false
	for _, m := range history {
		if m.Content == "exit status 1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("errored tool result was dropped from history: %+v", history)
	}
}
