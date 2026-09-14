package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/enowdev/antares/internal/llm"
)

// validateToolCallArguments catches provider streams that finish with a
// truncated JSON argument payload. Without this check the malformed call reaches
// the tool, fails Bind with unexpected EOF, and consumes the turn instead of
// using the existing provider-glitch retry path.
func validateToolCallArguments(resp *llm.Response) error {
	if resp == nil {
		return nil
	}
	for i := range resp.ToolCalls {
		call := &resp.ToolCalls[i]
		if strings.TrimSpace(call.Arguments) == "" {
			call.Arguments = "{}"
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return fmt.Errorf("malformed tool_call arguments for %s: %w", call.Name, err)
		}
	}
	return nil
}

// callModel runs one completion, streaming when enabled.
func (a *Agent) callModel(ctx context.Context, client llm.Client, req llm.Request, stream bool, emit Emit) (*llm.Response, error) {
	if !stream {
		resp, err := client.Chat(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.Reasoning != "" {
			_ = emit(Event{Type: EventReasoning, Delta: resp.Reasoning})
		}
		if resp.Content != "" {
			_ = emit(Event{Type: EventText, Delta: resp.Content})
		}
		return resp, nil
	}

	return client.Stream(ctx, req, func(ev llm.Event) error {
		switch ev.Type {
		case llm.EventText:
			return emit(Event{Type: EventText, Delta: ev.Delta})
		case llm.EventReasoning:
			if a.config().Display.ShowReasoning {
				return emit(Event{Type: EventReasoning, Delta: ev.Delta})
			}
		}
		return nil
	})
}
