package gateway

import (
	"fmt"
	"io/fs"
)

const (
	GatewayElevated     = "gateway_elevated"
	GatewayOtherSession = "gateway_other_session"
)

// Boundary code naming why a same user owner refused this client, empty to keep authorization
func ClassifyRefusedOwner(ownSession, ownerSession *uint32, ownElevated bool, ownerElevated *bool) string {
	if ownSession != nil && ownerSession != nil && *ownSession != *ownerSession {
		return GatewayOtherSession
	}
	if ownElevated {
		return ""
	}
	if (ownerElevated != nil && *ownerElevated) || (ownerElevated == nil && ownerSession != nil) {
		return GatewayElevated
	}
	return ""
}

// User facing remedy for a refused owner boundary
func RefusedOwnerRemedy(reason string, pid int, ownerSession *uint32) string {
	if reason == GatewayElevated {
		return fmt.Sprintf("The running gateway (PID %d) was started as administrator, so this non-elevated "+
			"app cannot reach it. Stop it from an administrator prompt (icebath gateway stop), then "+
			"start Icebath without elevation", pid)
	}
	where := "another session"
	if ownerSession != nil {
		where = fmt.Sprintf("session %d", *ownerSession)
	}
	return fmt.Sprintf("The running gateway (PID %d) runs in another Windows logon session (%s, e.g. a "+
		"boot-time scheduled task), so this app cannot reach it. Stop it (icebath gateway stop from an "+
		"administrator prompt) and let only one auto-start task launch the gateway", pid, where)
}

// RefusedOwner is this user's gateway behind a boundary the client must not cross, a PermissionError
type RefusedOwner struct {
	ReasonCode string
	Pid        int
	Remedy     string
}

func (e *RefusedOwner) Error() string        { return e.Remedy }
func (e *RefusedOwner) Is(target error) bool { return target == fs.ErrPermission }

// Swappable probe of a refused owner's boundary
var probeRefusedOwnerSeam = probeRefusedOwner
