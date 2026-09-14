package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/enowdev/antares/internal/agent"
)

// subAgentInput is the JSON contract for the internal `_subagent` command.
// The parent's runSubprocess writes this on the child's stdin. Fields mirror
// the delegated-workstream fields the in-process sub-agent path already
// honours; anything absent falls back to the child's own defaults.
type subAgentInput struct {
	Prompt      string `json:"prompt"`
	SystemExtra string `json:"system_extra,omitempty"`
	Toolset     string `json:"toolset,omitempty"`
	Model       string `json:"model,omitempty"`
	Role        string `json:"role,omitempty"`
	MaxTurns    int    `json:"max_turns,omitempty"`
	Workspace   string `json:"workspace,omitempty"`
	ProjectDir  string `json:"project_dir,omitempty"`
}

// cmdSubAgent is the child half of subprocess delegation. It is invoked as
// `antares _subagent`, reads a JSON subAgentInput from stdin, boots the same
// runtime a normal invocation would, and runs a single Quiet turn tagged with
// Platform="subagent" and Depth=1 so every subordinate-guard (schema filter,
// ask_user dispatch guard, prompt guidance) trips exactly as it does in the
// in-process delegation path. The final reply goes to stdout; any error is
// returned so main formats it on stderr and exits non-zero.
//
// Hidden from `printUsage`: the command is an implementation detail of the
// parent's runSubprocess; a human should never type it.
func cmdSubAgent(args []string) error {
	_ = args // no flags: the payload is on stdin so a very long prompt is not an argv limit

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read _subagent stdin: %w", err)
	}
	if len(raw) == 0 {
		return fmt.Errorf("_subagent: stdin was empty; expected JSON payload")
	}
	var in subAgentInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("decode _subagent payload: %w", err)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return fmt.Errorf("_subagent: prompt is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := bootstrap(ctx)
	if err != nil {
		return err
	}
	defer rt.close()

	res, err := rt.agent.Run(ctx, agent.Request{
		Message:     in.Prompt,
		SystemExtra: in.SystemExtra,
		Toolset:     in.Toolset,
		Model:       in.Model,
		Role:        in.Role,
		MaxTurns:    in.MaxTurns,
		Workspace:   in.Workspace,
		ProjectDir:  in.ProjectDir,
		// The identity that trips isSubordinateRun. Quiet keeps this a
		// one-shot run: no session is persisted for the child, matching the
		// stateless nature of subprocess delegation.
		Platform: "subagent",
		Depth:    1,
		Quiet:    true,
	}, nil)
	if err != nil {
		return err
	}
	if res != nil && strings.TrimSpace(res.Reply) != "" {
		fmt.Print(res.Reply)
	}
	return nil
}
