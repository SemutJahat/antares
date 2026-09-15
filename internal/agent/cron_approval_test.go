package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/llm"
	"github.com/enowdev/antares/internal/store"
	"github.com/enowdev/antares/internal/tools"
)

func TestCronMutationDoesNotWaitForHumanApproval(t *testing.T) {
	cfg := config.Default()
	cfg.Tools.ApprovalMode = "prompt"
	a := agentWithConfig(cfg)
	tool, ok := tools.Default().Get("write_file")
	if !ok {
		t.Fatal("write tool missing")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "draft.txt")
	args, _ := json.Marshal(map[string]string{"path": path, "content": "draft"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	approval := false
	outcomes := a.executeTools(ctx, []llm.ToolCall{{ID: "write", Name: "write_file", Arguments: string(args)}}, map[string]tools.Tool{"write_file": tool}, Request{Platform: "cron"}, &store.Session{ID: "scheduled", Workspace: dir}, func(e Event) error {
		if e.Type == EventApproval {
			approval = true
		}
		return nil
	})
	if ctx.Err() != nil {
		t.Fatal("scheduled action waited for human input")
	}
	if approval || len(outcomes) != 1 || !outcomes[0].isError {
		t.Fatalf("scheduled mutation did not fail closed: %+v", outcomes)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unapproved draft was written: %v", err)
	}
}
