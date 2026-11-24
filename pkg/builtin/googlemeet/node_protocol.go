package googlemeet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/google/uuid"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var validRequestTypes = map[string]bool{"start_bot": true, "stop": true, "status": true, "transcript": true, "say": true, "ping": true}

// Request envelope carrying the shared token, the id defaulting to a uuid4 hex
func makeRequest(typ string, token any, payload any, reqID string) (*jsonutil.Object, error) {
	if typ == "" {
		return nil, errors.New("type must be a non-empty string")
	}
	if !validRequestTypes[typ] {
		return nil, fmt.Errorf("unknown request type: %s", strconv.Quote(typ))
	}
	if _, ok := token.(string); !ok {
		return nil, errors.New("token must be a string")
	}
	if _, ok := payload.(*jsonutil.Object); !ok {
		return nil, errors.New("payload must be a dict")
	}
	if reqID == "" {
		id := uuid.New()
		reqID = hex.EncodeToString(id[:])
	}
	return jsonutil.ObjectOf("type", typ, "id", reqID, "token", token, "payload", payload), nil
}

// Success envelope correlated by id
func makeResponse(reqID any, payload any) (*jsonutil.Object, error) {
	if _, ok := payload.(*jsonutil.Object); !ok {
		return nil, errors.New("payload must be a dict")
	}
	return jsonutil.ObjectOf("type", "response", "id", reqID, "payload", payload), nil
}

// Error envelope
func makeError(reqID any, msg string) *jsonutil.Object {
	return jsonutil.ObjectOf("type", "error", "id", reqID, "error", msg)
}

// Compact JSON of an envelope with non ASCII kept
func encodeEnvelope(msg *jsonutil.Object) (string, error) {
	return jsonutil.Encode(msg)
}

// Parses an envelope object with string type and id from text or bytes
func decodeEnvelope(raw []byte) (*jsonutil.Object, error) {
	if !utf8.Valid(raw) {
		if _, err := jsonutil.Text(raw); err != nil {
			return nil, err
		}
	}
	v, err := jsonutil.Decode(string(raw))
	if err != nil {
		return nil, fmt.Errorf("malformed JSON: %v", err)
	}
	obj, ok := v.(*jsonutil.Object)
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
func validateRequest(msg *jsonutil.Object, expectedToken string) (bool, string) {
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
		return false, "unknown request type: " + anyutil.Quote(t)
	case !nonemptyStr(id):
		return false, "missing or non-string 'id'"
	case !nonemptyStr(token):
		return false, "missing token"
	case token != expectedToken:
		return false, "token mismatch"
	}
	if _, ok := payload.(*jsonutil.Object); !ok {
		return false, "payload must be a dict"
	}
	return true, ""
}
