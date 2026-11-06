package gateway

import "github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"

// Hands the pause record the planned stop marker ttl, staleness rule and process start clock
func init() {
	updaterecord.SetPlannedStopProbe(updaterecord.PlannedStopProbe{TTL: plannedStopMarkerTTL, IsStale: markerIsStale, StartTime: GetProcessStartTime})
}
