package gatewayrun

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Attributes only the inbound, busy session, notification, goal and API modules set
type runnerInboundState struct {
	unauthorizedOwnerNotifier *UnauthorizedOwnerNotifier
	updateNotificationTask    *gatewayTask
	plannedRestartNoticeLock  sync.Mutex
	servedProfileHomes        *pydict.Dict[string, string]
	sessionAuthorities        *gateway.SessionAuthorities
}
