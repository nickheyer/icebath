package gateway

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Fields a submission commits for a local session's input, producers committing their own envelope
var localInputFields = map[string]bool{"text": true, "attachments_v1": true, "finite": true, "unattended": true, "surface_v1": true,
	"display_v1": true, "cli_turn_v1": true, "kanban_card_v1": true}

// Payload of an admission row
func rowPayload(row *pyjson.Object) *pyjson.Object {
	value, _ := row.Get("payload")
	payload, _ := value.(*pyjson.Object)
	if payload == nil {
		payload = pyjson.NewObject()
	}
	return payload
}

// True when the payload holds text and nothing outside the allowed fields
func textWithin(payload *pyjson.Object, allowed map[string]bool, extra string) bool {
	if _, ok := payload.Get("text"); !ok {
		return false
	}
	for _, key := range payload.Keys {
		if !allowed[key] && (extra == "" || key != extra) {
			return false
		}
	}
	return true
}

// True for a row a session viewer submitted as its own prompt, false for producer and hosted admissions
func isViewerInput(row *pyjson.Object) bool {
	requestID, _ := row.Get("request_id")
	return !strings.HasPrefix(pyrepr.Str(requestID), "hosted:") && textWithin(rowPayload(row), localInputFields, "local_operator_v1")
}

// Refuses a queued local input whose operator envelope or owner no longer matches its session
func checkLocalInput(a *SessionAuthority, ref SessionRef, row *pyjson.Object, source *SessionSource) error {
	payload := rowPayload(row)
	extra := ""
	principal, _ := row.Get("principal_id")
	if operator, ok := payload.Get("local_operator_v1"); ok {
		extra = "local_operator_v1"
		if !pyjson.Equal(operator, pyjson.ObjectOf("profile_id", a.ProfileID, "session_id", ref.SessionID, "principal_id", principal)) {
			return runtimeRefusal("permission_denied")
		}
	} else {
		if source == nil {
			live, ok := a.live(ref.SessionID)
			if !ok {
				return &pyKeyError{ref.SessionID}
			}
			source = live.Source
		}
		if !SameOwner(optStrAny(source.UserID), principal) {
			return runtimeRefusal("permission_denied")
		}
	}
	if !textWithin(payload, localInputFields, extra) {
		return runtimeRefusal("permission_denied")
	}
	return nil
}
