package gateway

import "slices"

// Runner descriptor a parked profile check reads, gateway/run.py session_runtime_descriptor
type runtimeDescriptorRunner interface {
	SessionRuntimeDescriptor() map[string]any
}

// True when the named profile is parked, unserved now but served again on a rescan
func profileParked(runner any, name string) bool {
	if name == "" {
		return false
	}
	holder, ok := runner.(runtimeDescriptorRunner)
	if !ok || holder == nil {
		return false
	}
	descriptor := holder.SessionRuntimeDescriptor()
	switch parked := descriptor["parked_profiles"].(type) {
	case map[string]any:
		_, found := parked[name]
		return found
	case map[string]string:
		_, found := parked[name]
		return found
	case []string:
		return slices.Contains(parked, name)
	case []any:
		return slices.Contains(parked, any(name))
	}
	return false
}
