package gateway

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var apiItemIDPrefixes = map[string]string{"function_call": "fc", "function_call_output": "fco", "reasoning": "rs", "message": "msg"}

// Occurrence counter key of one output item kind
type apiItemCounterKey struct {
	Kind       string
	CallID     any
	Commentary bool
}

// Public response id of an Idempotency-Key request
func APIDurableResponseID(requestKey string) string {
	sum := sha256.Sum256([]byte(requestKey))
	return "resp_" + hex.EncodeToString(sum[:])[:28]
}

// Stable item id derived from the response id, the item kind, call id, commentary flag and occurrence
func apiNextItemID(responseID string, counters map[apiItemCounterKey]int, kind string, callID any, commentary bool) (string, error) {
	switch callID.(type) {
	case *pyjson.Object, map[string]any, []any:
		return "", &PyError{Kind: "TypeError", Msg: "unhashable type: '" + pyrepr.TypeName(callID) + "'"}
	}
	key := apiItemCounterKey{Kind: kind, CallID: callID, Commentary: commentary}
	occurrence := counters[key]
	counters[key] = occurrence + 1
	encoded, err := pyjson.Dumps([]any{responseID, kind, callID, commentary, occurrence}, pyjson.Separators(",", ":"))
	if err != nil {
		return "", err
	}
	prefix, ok := apiItemIDPrefixes[kind]
	if !ok {
		return "", &pyKeyError{kind}
	}
	sum := sha256.Sum256([]byte(encoded))
	return prefix + "_" + hex.EncodeToString(sum[:])[:24], nil
}

// Output items copied with their stable ids
func apiIdentifyOutput(items []*pyjson.Object, responseID string) ([]*pyjson.Object, error) {
	counters := map[apiItemCounterKey]int{}
	out := make([]*pyjson.Object, 0, len(items))
	for _, item := range items {
		kind, found := item.Get("type")
		if !found {
			return nil, &pyKeyError{"type"}
		}
		kindText, ok := kind.(string)
		if !ok {
			return nil, &pyKeyError{pyrepr.Str(kind)}
		}
		id, err := apiNextItemID(responseID, counters, kindText, item.GetOr("call_id", nil), item.GetOr("phase", nil) == "commentary")
		if err != nil {
			return nil, err
		}
		copied := item.Copy()
		copied.Set("id", id)
		out = append(out, copied)
	}
	return out, nil
}
