package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/agent"
	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/store"
)

func testGoalDeps(t *testing.T) (Deps, string) {
	t.Helper()
	db, err := store.Open(context.Background(), "memory", "", 1, 5000, false)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	a := agent.New(config.Default(), db, nil, nil, nil)
	// A goal attaches to a session; create one so SetGoal has somewhere to land.
	sess := &store.Session{ID: "s1"}
	if err := db.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return Deps{Agent: a, Store: db}, sess.ID
}

func runGoal(t *testing.T, d Deps, sessionID, args string, platform, channel string) Result {
	t.Helper()
	res, err := cmdGoal(context.Background(), d, Input{
		Name: "goal", Args: args, SessionID: sessionID,
		Platform: platform, ChannelID: channel,
	})
	if err != nil {
		t.Fatalf("/goal %q: %v", args, err)
	}
	return res
}

func TestGoalNormalIsNotAutonomous(t *testing.T) {
	d, sid := testGoalDeps(t)
	runGoal(t, d, sid, "ship the feature", "", "")
	g, ok := d.Agent.GetGoal(context.Background(), sid)
	if !ok {
		t.Fatal("goal was not set")
	}
	if g.Autonomous {
		t.Error("a plain /goal must not be autonomous")
	}
	if g.Text != "ship the feature" {
		t.Errorf("text = %q", g.Text)
	}
}

func TestGoalAutoSetsAutonomousWithDefaultCap(t *testing.T) {
	d, sid := testGoalDeps(t)
	runGoal(t, d, sid, "auto get the suite green", "", "")
	g, _ := d.Agent.GetGoal(context.Background(), sid)
	if g == nil || !g.Autonomous {
		t.Fatalf("expected an autonomous goal, got %+v", g)
	}
	if g.Text != "get the suite green" {
		t.Errorf("text = %q, want the auto verb stripped", g.Text)
	}
	if g.Max != 0 {
		// Max unset means "use the configured default"; a bare `/goal auto`
		// leaves it 0 so autonomousMax falls back to config.
		t.Errorf("Max = %d, want 0 (use default) for a capless /goal auto", g.Max)
	}
}

func TestGoalAutoInlineCap(t *testing.T) {
	d, sid := testGoalDeps(t)
	runGoal(t, d, sid, "auto 25 refactor the module", "", "")
	g, _ := d.Agent.GetGoal(context.Background(), sid)
	if g == nil || !g.Autonomous {
		t.Fatal("expected autonomous goal")
	}
	if g.Max != 25 {
		t.Errorf("Max = %d, want 25", g.Max)
	}
	if g.Text != "refactor the module" {
		t.Errorf("text = %q, want the cap stripped", g.Text)
	}
}

func TestGoalAutoZeroCapMeansUnlimited(t *testing.T) {
	d, sid := testGoalDeps(t)
	runGoal(t, d, sid, "auto 0 keep polishing", "", "")
	g, _ := d.Agent.GetGoal(context.Background(), sid)
	if g == nil || !g.Autonomous {
		t.Fatal("expected autonomous goal")
	}
	if g.Max != 0 {
		t.Errorf("Max = %d, want 0 (unlimited)", g.Max)
	}
	if g.Text != "keep polishing" {
		t.Errorf("text = %q", g.Text)
	}
}

func TestGoalAutoBareNumberIsRejected(t *testing.T) {
	d, sid := testGoalDeps(t)
	_, err := cmdGoal(context.Background(), d, Input{Name: "goal", Args: "auto 5", SessionID: sid})
	if err == nil {
		t.Fatal("`/goal auto 5` with no text should error")
	}
}

func TestGoalAutoOnOffTogglesExisting(t *testing.T) {
	d, sid := testGoalDeps(t)
	runGoal(t, d, sid, "do the thing", "", "") // normal
	runGoal(t, d, sid, "auto on", "", "")
	if g, _ := d.Agent.GetGoal(context.Background(), sid); g == nil || !g.Autonomous {
		t.Fatal("auto on should make the goal autonomous")
	}
	runGoal(t, d, sid, "auto off", "", "")
	if g, _ := d.Agent.GetGoal(context.Background(), sid); g == nil || g.Autonomous {
		t.Fatal("auto off should clear autonomous")
	}
}

func TestGoalAutoRecordsGatewayOrigin(t *testing.T) {
	d, sid := testGoalDeps(t)
	runGoal(t, d, sid, "auto answer the thread", "discord", "chan-123")
	g, _ := d.Agent.GetGoal(context.Background(), sid)
	if g == nil || g.Platform != "discord" || g.ChannelID != "chan-123" {
		t.Fatalf("gateway origin not recorded: %+v", g)
	}
}

func TestGoalStatusShowsAutonomous(t *testing.T) {
	d, sid := testGoalDeps(t)
	runGoal(t, d, sid, "auto 0 win", "", "")
	res := runGoal(t, d, sid, "status", "", "")
	if !strings.Contains(res.Output, "autonomous") {
		t.Errorf("status should mention autonomous mode: %q", res.Output)
	}
	if !strings.Contains(res.Output, "unlimited") {
		t.Errorf("status should show the unlimited cap: %q", res.Output)
	}
}
