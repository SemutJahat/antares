package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/llm"
	"github.com/enowdev/antares/internal/store"
)

func TestRepeatTrackerTripsOnIdenticalCalls(t *testing.T) {
	r := newRepeatTracker(3)
	call := llm.ToolCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}

	if got := r.record([]llm.ToolCall{call}); len(got) != 0 {
		t.Fatalf("first call tripped: %v", got)
	}
	if got := r.record([]llm.ToolCall{call}); len(got) != 0 {
		t.Fatalf("second call tripped: %v", got)
	}
	got := r.record([]llm.ToolCall{call})
	if len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("third identical call should trip, got %v", got)
	}
	// It must fire once, not on every call after the limit, or the history
	// fills with the same nudge.
	if again := r.record([]llm.ToolCall{call}); len(again) != 0 {
		t.Fatalf("the nudge repeated: %v", again)
	}
}

func TestRepeatTrackerIgnoresDifferentArguments(t *testing.T) {
	r := newRepeatTracker(2)
	for _, path := range []string{"a", "b", "c", "d"} {
		got := r.record([]llm.ToolCall{{Name: "read_file", Arguments: `{"path":"` + path + `"}`}})
		if len(got) != 0 {
			t.Fatalf("reading %q tripped the guard: %v", path, got)
		}
	}
}

func TestRepeatTrackerNormalisesArguments(t *testing.T) {
	r := newRepeatTracker(2)
	// Same call, re-serialised with different key order and spacing.
	r.record([]llm.ToolCall{{Name: "grep", Arguments: `{"pattern":"x","path":"."}`}})
	got := r.record([]llm.ToolCall{{Name: "grep", Arguments: `{ "path": ".", "pattern": "x" }`}})
	if len(got) != 1 {
		t.Fatalf("a re-serialised identical call was not recognised: %v", got)
	}
}

func TestRepeatTrackerExceeded(t *testing.T) {
	r := newRepeatTracker(2)
	call := llm.ToolCall{Name: "terminal", Arguments: `{"command":"ls"}`}
	for i := 0; i < 3; i++ {
		r.record([]llm.ToolCall{call})
		if r.exceeded() {
			t.Fatalf("gave up after %d calls, too early", i+1)
		}
	}
	r.record([]llm.ToolCall{call})
	if !r.exceeded() {
		t.Fatal("expected the run to be abandoned after twice the limit")
	}
}

func TestRepeatTrackerAllowsManagedProcessPolling(t *testing.T) {
	r := newRepeatTracker(2)
	call := llm.ToolCall{Name: "process", Arguments: `{"action":"wait","process_id":"proc_123","timeout":30}`}
	for i := 0; i < 20; i++ {
		if got := r.record([]llm.ToolCall{call}); len(got) != 0 {
			t.Fatalf("managed process wait was treated as a loop after %d calls: %v", i+1, got)
		}
	}
	if r.exceeded() {
		t.Fatal("managed process polling tripped the repeat guard")
	}
}

func TestRepeatTrackerStillTracksProcessKill(t *testing.T) {
	r := newRepeatTracker(2)
	call := llm.ToolCall{Name: "process", Arguments: `{"action":"kill","process_id":"proc_123"}`}
	if got := r.record([]llm.ToolCall{call}); len(got) != 0 {
		t.Fatal(got)
	}
	if got := r.record([]llm.ToolCall{call}); len(got) != 1 || got[0] != "process" {
		t.Fatalf("repeated process kill was not tracked: %v", got)
	}
}

func TestSteeringRequiresARunningSession(t *testing.T) {
	a := &Agent{active: map[string]context.CancelFunc{}}
	if a.Steer("nope", "do this instead") {
		t.Fatal("steering a session that is not running should report false")
	}

	a.active["s1"] = func() {}
	if !a.Steer("s1", "do this instead") {
		t.Fatal("steering a running session should be accepted")
	}
	if a.Steer("s1", "   ") {
		t.Fatal("an empty note should be rejected")
	}

	notes := drainSteering("s1")
	if len(notes) != 1 || notes[0] != "do this instead" {
		t.Fatalf("got %v", notes)
	}
	// Draining takes them, so a later turn does not replay old instructions.
	if again := drainSteering("s1"); len(again) != 0 {
		t.Fatalf("notes were replayed: %v", again)
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"complete":true}`:                                 `{"complete":true}`,
		"```json\n{\"complete\":false}\n```":                `{"complete":false}`,
		"Here is my verdict:\n{\"complete\": true}\nThanks": `{"complete": true}`,
		"no json here":                                      "no json here",
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormaliseArgsToleratesGarbage(t *testing.T) {
	// A model sometimes emits arguments that are not valid JSON at all; the
	// guard must still fingerprint them rather than panic.
	if got := normaliseArgs(`{"broken":`); got != `{"broken":` {
		t.Fatalf("got %q", got)
	}
}

func TestRepeatKeyWriteFileSamePathDifferentContent(t *testing.T) {
	r := newRepeatTracker(2)
	// Same path, different content — the old full-args fingerprint would not
	// trip. With the path-aware key, repeated writes to the same file are
	// recognised as a stuck loop.
	path := `{"path":"config.yaml","content":"v1"}`
	r.record([]llm.ToolCall{{Name: "write_file", Arguments: path}})
	path2 := `{"path":"config.yaml","content":"v2"}`
	got := r.record([]llm.ToolCall{{Name: "write_file", Arguments: path2}})
	if len(got) != 1 || got[0] != "write_file" {
		t.Fatalf("same-path different-content write should trip on 2nd call, got %v", got)
	}
}

func TestRepeatKeyEditFileSamePathDifferentContent(t *testing.T) {
	r := newRepeatTracker(2)
	r.record([]llm.ToolCall{{Name: "edit_file", Arguments: `{"path":"main.go","old_string":"a","new_string":"b"}`}})
	got := r.record([]llm.ToolCall{{Name: "edit_file", Arguments: `{"path":"main.go","old_string":"c","new_string":"d"}`}})
	if len(got) != 1 || got[0] != "edit_file" {
		t.Fatalf("same-path different-content edit should trip on 2nd call, got %v", got)
	}
}

func TestRepeatKeyVpsUploadSameRemotePath(t *testing.T) {
	r := newRepeatTracker(2)
	r.record([]llm.ToolCall{{Name: "vps_upload", Arguments: `{"remote_path":"/tmp/x","local_path":"/a/b"}`}})
	got := r.record([]llm.ToolCall{{Name: "vps_upload", Arguments: `{"remote_path":"/tmp/x","local_path":"/c/d"}`}})
	if len(got) != 1 || got[0] != "vps_upload" {
		t.Fatalf("same-remote-path different-local vps_upload should trip, got %v", got)
	}
}

func TestRepeatKeyWriteFileDifferentPathDoesNotTrip(t *testing.T) {
	r := newRepeatTracker(2)
	r.record([]llm.ToolCall{{Name: "write_file", Arguments: `{"path":"a.txt","content":"x"}`}})
	got := r.record([]llm.ToolCall{{Name: "write_file", Arguments: `{"path":"b.txt","content":"x"}`}})
	if len(got) != 0 {
		t.Fatalf("different paths should not trip: %v", got)
	}
}

func TestStuckEscalationTiers(t *testing.T) {
	if stuckEscalation(0) != "" {
		t.Error("no escalation expected when not stuck")
	}
	e1 := stuckEscalation(1)
	if !strings.Contains(e1, "different approach") {
		t.Errorf("tier 1 should push a different approach: %q", e1)
	}
	e2 := stuckEscalation(2)
	if !strings.Contains(e2, "documentation") || !strings.Contains(e2, "web_search") {
		t.Errorf("tier 2 should push docs + web search: %q", e2)
	}
	e3 := stuckEscalation(3)
	if !strings.Contains(e3, "delegate") {
		t.Errorf("tier 3 should push delegation: %q", e3)
	}
	// Beyond tier 3 stays at the strongest escalation, not empty.
	if stuckEscalation(9) == "" {
		t.Error("high stuck counts must still escalate")
	}
}

func TestAutonomousMaxUsesGoalThenConfigDefault(t *testing.T) {
	a := agentWithConfig(config.Default()) // GoalAutonomousMaxIterations default 50
	if got := a.autonomousMax(&Goal{}); got != 50 {
		t.Errorf("capless goal should use config default 50, got %d", got)
	}
	if got := a.autonomousMax(&Goal{Max: 12}); got != 12 {
		t.Errorf("goal's own cap should win, got %d", got)
	}
	if got := a.autonomousMax(&Goal{Max: 0}); got != 50 {
		t.Errorf("Max 0 falls back to config default (unlimited is decided by the caller), got %d", got)
	}
}

func TestKickAutonomousGoalFiresDriver(t *testing.T) {
	db, err := store.Open(context.Background(), "memory", "", 1, 5000, false)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	a := New(config.Default(), db, nil, nil, nil)

	var fired []TurnEnded
	a.OnTurnEnd(func(e TurnEnded) { fired = append(fired, e) })

	ctx := context.Background()
	// Autonomous goal -> kick fires with the session/platform/channel.
	if err := a.SetGoal(ctx, "s1", &Goal{Text: "win", Autonomous: true}); err != nil {
		t.Fatal(err)
	}
	a.KickAutonomousGoal(ctx, "s1", "discord", "c9")
	if len(fired) != 1 || fired[0].SessionID != "s1" || fired[0].Platform != "discord" || fired[0].ChannelID != "c9" {
		t.Fatalf("expected one kick for s1/discord/c9, got %+v", fired)
	}

	// A paused goal must not kick.
	fired = nil
	if err := a.SetGoal(ctx, "s2", &Goal{Text: "x", Autonomous: true, Paused: true}); err != nil {
		t.Fatal(err)
	}
	a.KickAutonomousGoal(ctx, "s2", "web", "")
	if len(fired) != 0 {
		t.Fatalf("paused goal should not kick, got %+v", fired)
	}

	// A normal (non-autonomous) goal must not kick.
	if err := a.SetGoal(ctx, "s3", &Goal{Text: "y"}); err != nil {
		t.Fatal(err)
	}
	a.KickAutonomousGoal(ctx, "s3", "web", "")
	if len(fired) != 0 {
		t.Fatalf("non-autonomous goal should not kick, got %+v", fired)
	}
}
