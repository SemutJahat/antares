package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/enowdev/antares/internal/store"
)

// Error rows are visible to the user but never replayed as model output.
func isPersistedTurnError(m store.Message) bool {
	flagged, _ := m.Meta["is_error"].(bool)
	return m.Role == store.RoleAssistant && flagged
}

func (a *Agent) reportTurnError(ctx context.Context, req Request, err error, emit Emit) {
	payload, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: err.Error()})
	// Interrupts and deadlines must not cancel the write that explains them.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if !req.Quiet && a.db != nil && req.SessionID != "" {
		if _, lookupErr := a.db.GetSession(persistCtx, req.SessionID); lookupErr == nil {
			if req.turnMarker == "" && (req.Message != "" || len(req.Images) > 0) {
				attachments := ""
				if len(req.Images) > 0 {
					b, _ := json.Marshal(req.Images)
					attachments = string(b)
				}
				if persistErr := a.db.AppendMessage(persistCtx, &store.Message{
					ID: newID("msg"), SessionID: req.SessionID, Role: store.RoleUser,
					Content: req.Message, Attachments: attachments,
				}); persistErr != nil {
					slog.Error("persist failed prompt", "error", persistErr)
				}
			}
			if persistErr := a.db.AppendMessage(persistCtx, &store.Message{
				ID: newID("msg"), SessionID: req.SessionID, Role: store.RoleAssistant,
				Content: string(payload), Meta: store.Meta{"is_error": true},
			}); persistErr != nil {
				slog.Error("persist turn error failed", "error", persistErr)
			}
		}
	}
	if emit != nil {
		_ = emit(Event{Type: EventError, Err: string(payload)})
	}
}
