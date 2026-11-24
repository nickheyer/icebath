package tuihost

import (
	"context"

	"github.com/nickheyer/icebath/pkg/plugins"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Plugin hook a hosted room member's live runtime events are projected to
const RoomMemberActivityHook = "on_room_member_activity"

var (
	// Session event frame type to room activity kind, unlisted frames being session chrome
	kindByFrameType = map[string]string{
		"tool.start": "tool.started", "tool.complete": "tool.completed", "tool.output_risk": "tool.output_risk",
		"message.delta": "message.delta", "message.interim": "message.interim", "reasoning.delta": "reasoning.delta",
		"error": "turn.error",
	}
	// Server request methods that count as member activity
	kindByRequestMethod   = map[string]string{"approval": "request.opened"}
	roomCoordinateFields  = []string{"room_id", "thread_id", "member_id", "turn_id", "task_id", "execution_generation"}
	enqueueRoomStreamHook = plugins.EnqueuePluginStreamHook
)

// Field of a mapping held as a plain map or an ordered object, nil when absent
func mappingGet(m any, key string) any {
	switch x := m.(type) {
	case map[string]any:
		return x[key]
	case *jsonutil.Object:
		return x.ValueOr(key, nil)
	}
	return nil
}

// Plain copy of a mapping, empty for nil or a non mapping
func mappingCopy(m any) map[string]any {
	out := map[string]any{}
	switch x := m.(type) {
	case map[string]any:
		for k, v := range x {
			out[k] = v
		}
	case *jsonutil.Object:
		for _, k := range x.Keys() {
			out[k] = x.Value(k)
		}
	}
	return out
}

func isMapping(m any) bool {
	switch x := m.(type) {
	case map[string]any:
		return x != nil
	case *jsonutil.Object:
		return x != nil
	}
	return false
}

// Queues one activity event stamped with the room coordinates for every consumer, false when nobody listens
func EmitRoomMemberActivity(ctx context.Context, hostedTask any, kind string, payload any, seq any) bool {
	kwargs := map[string]any{}
	for _, field := range roomCoordinateFields {
		kwargs[field] = mappingGet(hostedTask, field)
	}
	kwargs["kind"], kwargs["seq"], kwargs["payload"] = kind, seq, mappingCopy(payload)
	return enqueueRoomStreamHook(ctx, RoomMemberActivityHook, kwargs)
}

// Fires the hook for an outgoing event or server request frame of a session that is running a room turn
func ProjectRoomMemberActivity(ctx context.Context, frame *jsonutil.Object, session func(sid string) *Session) bool {
	params, ok := frame.ValueOr("params", nil).(*jsonutil.Object)
	if !ok {
		return false
	}
	text := func(v any) string {
		if !anyutil.IsTrue(v) {
			return ""
		}
		return anyutil.String(v)
	}
	var kind string
	var known bool
	var payload any
	if frame.ValueOr("method", nil) == "event" {
		kind, known = kindByFrameType[text(params.ValueOr("type", nil))]
		payload = params.ValueOr("payload", nil)
	} else if _, hasID := frame.Get("id"); hasID {
		kind, known = kindByRequestMethod[text(frame.ValueOr("method", nil))]
		rest := jsonutil.NewObject()
		for _, k := range params.Keys() {
			if k != "session_id" {
				rest.Set(k, params.Value(k))
			}
		}
		payload = rest
	} else {
		return false
	}
	if !known {
		return false
	}
	hostedTask := session(text(params.ValueOr("session_id", nil))).Get("_hosted_room_task")
	if !isMapping(hostedTask) {
		return false
	}
	if !isMapping(payload) {
		payload = nil
	}
	return EmitRoomMemberActivity(ctx, hostedTask, kind, payload, params.ValueOr("seq", nil))
}
