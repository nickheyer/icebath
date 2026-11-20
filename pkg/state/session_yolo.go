package state

import "context"

// Persists the per session YOLO flag so it survives a resume, a no op without a session id or row
func (d *SessionDB) SetSessionYolo(ctx context.Context, sessionID string, enabled bool) error {
	if sessionID == "" {
		return nil
	}
	return d.PatchSessionModelConfig(ctx, sessionID, map[string]any{"yolo_mode": enabled})
}
