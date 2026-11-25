package gateway

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	case *jsonutil.Object, map[string]any, []any:
		return "", &errorutil.Error{Kind: "TypeError", Msg: "the tool call id is " + errchain.Kind(callID) + ", expected text"}
	}
	key := apiItemCounterKey{Kind: kind, CallID: callID, Commentary: commentary}
	occurrence := counters[key]
	counters[key] = occurrence + 1
	encoded, err := jsonutil.Encode([]any{responseID, kind, callID, commentary, occurrence}, jsonutil.ASCII())
	if err != nil {
		return "", err
	}
	prefix, ok := apiItemIDPrefixes[kind]
	if !ok {
		return "", &keyError{kind}
	}
	sum := sha256.Sum256([]byte(encoded))
	return prefix + "_" + hex.EncodeToString(sum[:])[:24], nil
}

// Output items copied with their stable ids
func apiIdentifyOutput(items []*jsonutil.Object, responseID string) ([]*jsonutil.Object, error) {
	counters := map[apiItemCounterKey]int{}
	out := make([]*jsonutil.Object, 0, len(items))
	for _, item := range items {
		kind, found := item.Get("type")
		if !found {
			return nil, &keyError{"type"}
		}
		kindText, ok := kind.(string)
		if !ok {
			return nil, &keyError{anyutil.String(kind)}
		}
		id, err := apiNextItemID(responseID, counters, kindText, item.ValueOr("call_id", nil), item.ValueOr("phase", nil) == "commentary")
		if err != nil {
			return nil, err
		}
		copied := item.Clone()
		copied.Set("id", id)
		out = append(out, copied)
	}
	return out, nil
}
