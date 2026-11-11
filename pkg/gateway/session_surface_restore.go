package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Wire fields a submission may carry for its surface
var surfaceSubmitFields = map[string]bool{"surface": true, "voice_context": true, "interrupted": true, "voice_turn": true}

// Committed surface arriving across a process boundary, accepted only when admission itself could have committed it
func restoreSurface(committed any) (*pyjson.Object, error) {
	object, isDict := committed.(*pyjson.Object)
	if !isDict || object == nil || object.Len() == 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	params := map[string]any{}
	for _, key := range object.Keys {
		if !surfaceSubmitFields[key] {
			return nil, runtimeRefusal("invalid_params")
		}
		params[key] = object.Vals[key]
	}
	readmitted, err := admitSurface(params)
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(readmitted.GetOr("surface_v1", pyjson.NewObject()), object) {
		return nil, runtimeRefusal("invalid_params")
	}
	return object, nil
}
