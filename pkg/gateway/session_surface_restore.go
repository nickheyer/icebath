package gateway

import (
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Wire fields a submission may carry for its surface
var surfaceSubmitFields = map[string]bool{"surface": true, "voice_context": true, "interrupted": true, "voice_turn": true}

// Committed surface arriving across a process boundary, accepted only when admission itself could have committed it
func restoreSurface(committed any) (*jsonutil.Object, error) {
	object, isDict := committed.(*jsonutil.Object)
	if !isDict || object == nil || object.Len() == 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	params := map[string]any{}
	for _, key := range object.Keys() {
		if !surfaceSubmitFields[key] {
			return nil, runtimeRefusal("invalid_params")
		}
		params[key] = object.Value(key)
	}
	readmitted, err := admitSurface(params)
	if err != nil {
		return nil, err
	}
	if !anyutil.Equal(readmitted.ValueOr("surface_v1", jsonutil.NewObject()), object) {
		return nil, runtimeRefusal("invalid_params")
	}
	return object, nil
}
