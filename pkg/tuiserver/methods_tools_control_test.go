package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Session id and payload of every session.control.update frame the recorder saw
func controlUpdates(r *frameRecorder) [][2]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][2]any
	for _, frame := range r.frames {
		raw, _ := frame.Get("params")
		params, isObj := raw.(*pyjson.Object)
		if !isObj {
			continue
		}
		if kind, _ := params.Get("type"); kind != "session.control.update" {
			continue
		}
		sid, _ := params.Get("session_id")
		payload, _ := params.Get("payload")
		out = append(out, [2]any{sid, payload})
	}
	return out
}
