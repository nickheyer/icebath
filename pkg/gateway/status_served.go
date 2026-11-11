package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Liveness pid proven on this host, false for a health probe answer whose pid belongs to a remote container
func LocalLivenessPID(liveness GatewayLiveness) (any, bool) {
	if liveness.Source == "health" || liveness.PID == nil {
		return nil, false
	}
	return liveness.PID, true
}

// Multiplexer record projected onto the profile at h when it is that profile live runtime record, nil otherwise
func ServedProfileRuntime(h string, ownRuntime *pyjson.Object, gatewayPID any) (*pyjson.Object, error) {
	if ownRuntime != nil {
		_, live, err := GetRuntimeStatusRunningPid(ownRuntime, h)
		if err != nil {
			return nil, err
		}
		if live {
			return nil, nil
		}
	}
	muxPID, record, ok, err := statusMultiplexerLivenessForProfile(h)
	if err != nil || !ok || record == nil || record.Len() == 0 {
		return nil, err
	}
	if gatewayPID != nil && !pidEquals(gatewayPID, muxPID) {
		return nil, nil
	}
	name, _ := ProfileNameForHome(h)
	if name == "" {
		name = "default"
	}
	projected := record.Copy()
	projected.Set("platforms", ProfilePlatformsFromMultiplexer(record, name))
	return projected, nil
}

// Whether a pid value of any integer type equals pid
func pidEquals(v any, pid int) bool {
	switch n := v.(type) {
	case int:
		return n == pid
	case int64:
		return n == int64(pid)
	case float64:
		return n == float64(pid)
	}
	return false
}
