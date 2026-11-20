package googlemeet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var validRequestTypes = map[string]bool{"start_bot": true, "stop": true, "status": true, "transcript": true, "say": true, "ping": true}

// Request envelope carrying the shared token, the id defaulting to a uuid4 hex
func makeRequest(typ string, token any, payload any, reqID string) (*pyjson.Object, error) {
	if typ == "" {
		return nil, errors.New("type must be a non-empty string")
	}
	if !validRequestTypes[typ] {
		return nil, fmt.Errorf("unknown request type: %s", pyrepr.StrRepr(typ))
	}
	if _, ok := token.(string); !ok {
		return nil, errors.New("token must be a string")
	}
	if _, ok := payload.(*pyjson.Object); !ok {
		return nil, errors.New("payload must be a dict")
	}
	if reqID == "" {
		id := uuid.New()
		reqID = hex.EncodeToString(id[:])
	}
	return pyjson.ObjectOf("type", typ, "id", reqID, "token", token, "payload", payload), nil
}

// Success envelope correlated by id
func makeResponse(reqID any, payload any) (*pyjson.Object, error) {
	if _, ok := payload.(*pyjson.Object); !ok {
		return nil, errors.New("payload must be a dict")
	}
	return pyjson.ObjectOf("type", "response", "id", reqID, "payload", payload), nil
}

// Error envelope
func makeError(reqID any, msg string) *pyjson.Object {
	return pyjson.ObjectOf("type", "error", "id", reqID, "error", msg)
}

// Compact JSON of an envelope with non ASCII kept
func encodeEnvelope(msg *pyjson.Object) (string, error) {
	return pyjson.Dumps(msg, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
}

// Parses an envelope object with string type and id from text or bytes
func decodeEnvelope(raw []byte) (*pyjson.Object, error) {
	if !utf8.Valid(raw) {
		if _, err := pyjson.DecodeBytes(raw); err != nil {
			return nil, err
		}
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, fmt.Errorf("malformed JSON: %v", err)
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, errors.New("envelope must be a JSON object")
	}
	for _, key := range []string{"type", "id"} {
		if v, _ := obj.Get(key); func() bool { _, s := v.(string); return !s }() {
			return nil, fmt.Errorf("envelope missing string '%s'", key)
		}
	}
	return obj, nil
}

// Non empty string check of protocol fields
func nonemptyStr(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// First failing envelope check with a reason safe to send back, or ok
func validateRequest(msg *pyjson.Object, expectedToken string) (bool, string) {
	if msg == nil {
		return false, "envelope must be a dict"
	}
	t, _ := msg.Get("type")
	token, _ := msg.Get("token")
	id, _ := msg.Get("id")
	payload, _ := msg.Get("payload")
	ts, _ := t.(string)
	switch {
	case !nonemptyStr(t):
		return false, "missing or non-string 'type'"
	case !validRequestTypes[ts]:
		return false, "unknown request type: " + pyrepr.Repr(t)
	case !nonemptyStr(id):
		return false, "missing or non-string 'id'"
	case !nonemptyStr(token):
		return false, "missing token"
	case token != expectedToken:
		return false, "token mismatch"
	}
	if _, ok := payload.(*pyjson.Object); !ok {
		return false, "payload must be a dict"
	}
	return true, ""
}
