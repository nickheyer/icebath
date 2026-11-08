package gateway

import "github.com/nickheyer/icebath/pkg/kanban"

// Hands the kanban worker identity checks the status process facts and the instantiation epoch
func init() {
	kanban.GatewayStatusProbes = kanban.GatewayStatusProbeSet{
		PidExists:          func(pid int) bool { return statusPidExists(pid) },
		ProcessStartTime:   func(pid int) any { return statusProcessStartTime(pid) },
		StartTimesAgree:    startTimesAgree,
		InstantiationEpoch: func() string { return CurrentInstantiationEpoch() },
	}
}
