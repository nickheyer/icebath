package gatewayrun

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
)

// Attributes only the inbound, busy session, notification, goal and API modules set
type runnerInboundState struct {
	sessionsMu                sync.RWMutex
	unauthorizedOwnerNotifier *UnauthorizedOwnerNotifier
	updateNotificationTask    *gatewayTask
	plannedRestartNoticeLock  sync.Mutex
	servedProfileHomes        *orderedutil.Map[string, string]
	sessionAuthorities        *gateway.SessionAuthorities
	telegramLobbyReminderTS   map[string]float64
	heartbeatMu               sync.Mutex
	heartbeatWatch            map[string]heartbeatWatchEntry
	heartbeatPollTask         *gatewayTask
}

// Canonical route and session of one watched heartbeat
type heartbeatWatchEntry struct {
	source    *gateway.SessionSource
	sessionID string
}
