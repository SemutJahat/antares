package agent

import (
	"context"
	"testing"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/store"
)

// The tool-call guardrail auto-continue hinges on incompleteTodos: it decides
// whether a run that hit the budget still has work to do. These tests pin that
// gate and the message that pushes the model to keep going.

// agentWithConfig builds an Agent for tests with its atomic config pointer set.
// The cfg field is an atomic.Pointer, so it cannot be set via a struct literal.
func agentWithConfig(cfg *config.Config) *Agent {
	a := &Agent{}
	a.cfg.Store(cfg)
	return a
}

func newKVAgent(t *testing.T) *Agent {
	t.Helper()
	db, err := store.Open(context.Background(), "memory", "", 1, 5000, false)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Agent{db: db}
}

func TestIncompleteTodosCountsOpenItems(t *testing.T) {
	a := newKVAgent(t)
	ctx := context.Background()
	sid := "s1"
	// two open, one completed, one blank (ignored)
	todo := `[
		{"content":"do A","status":"pending"},
		{"content":"do B","status":"in_progress"},
		{"content":"do C","status":"completed"},
		{"content":"","status":"pending"}
	]`
	if err := a.db.SetKV(ctx, "todo:"+sid, todo); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if got := a.incompleteTodos(ctx, sid); got != 2 {
		t.Fatalf("open count = %d, want 2", got)
	}
}

func TestIncompleteTodosZeroWhenAllDone(t *testing.T) {
	a := newKVAgent(t)
	ctx := context.Background()
	sid := "s2"
	if err := a.db.SetKV(ctx, "todo:"+sid, `[{"content":"x","status":"completed"}]`); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if got := a.incompleteTodos(ctx, sid); got != 0 {
		t.Fatalf("open count = %d, want 0 (all completed → guardrail should stop)", got)
	}
}

func TestIncompleteTodosZeroWithNoList(t *testing.T) {
	a := newKVAgent(t)
	// No todo written: a run with no task list must not auto-continue.
	if got := a.incompleteTodos(context.Background(), "missing"); got != 0 {
		t.Fatalf("open count = %d, want 0 when no list exists", got)
	}
}
