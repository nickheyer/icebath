package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Frames of the recorder decoded into plain values
func approvalFrames(r *frameRecorder) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.frames))
	for _, f := range r.frames {
		out = append(out, pyjson.Plain(f).(map[string]any))
	}
	return out
}
