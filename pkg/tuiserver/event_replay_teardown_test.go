package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Event frame of one session as write_json stamps it
func replayFrame(sid string) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "method", "event",
		"params", pyjson.ObjectOf("type", "message.delta", "session_id", sid, "payload", pyjson.NewObject()))
}
