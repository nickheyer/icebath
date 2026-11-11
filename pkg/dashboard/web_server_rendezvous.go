package dashboard

import (
	"context"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs a startup step whose failure, a panic included, is only a debug line
func bestEffort(what string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			webServerLog().Debug(fmt.Sprintf("%s skipped: %v", what, r))
		}
	}()
	if err := fn(); err != nil {
		webServerLog().Debug(fmt.Sprintf("%s skipped: %s", what, err))
	}
}

// Whether the conflicting host owner was reaped or its dead record cleared so a re-claim is worth trying
func reclaimHostFromOrphanedOwner(role string) (reclaimed bool) {
	defer func() {
		if r := recover(); r != nil {
			reclaimed = false
		}
	}()
	owner := gateway.ReadHostRecord(role, false)
	if owner == nil {
		return gateway.DiscardDeadHostRecord(role)
	}
	if owner.PID == os.Getpid() {
		return false
	}
	if _, ok := tools.ReapOrphanedBackendOwner(owner.PID, owner.CreateTime, nil); !ok {
		return false
	}
	func() {
		defer func() { recover() }()
		gateway.DiscardDeadHostRecord(role)
	}()
	return true
}

// Publishes this backend's host record, the desktop serve role for a Desktop owned child
func publishHostRendezvous(ctx context.Context, app *FastAPI, host string, port int) {
	desktopChild := tools.IsDesktopOwnedBackend(nil)
	role := gateway.RoleServe
	if desktopChild {
		role = gateway.RoleDesktopServe
	}
	outcome, err := gateway.ClaimHostLock(ctx, role)
	if outcome == gateway.HostLockCouldNotOpen {
		reason := "None"
		if err != nil {
			reason = err.Error()
		}
		webServerLog().Warn(fmt.Sprintf("Host backend lock could not be opened (%s); this backend is not discoverable. "+
			"This is NOT another backend holding it.", reason))
		return
	}
	if outcome == gateway.HostLockHeldByOther && !desktopChild && reclaimHostFromOrphanedOwner(role) {
		outcome, _ = gateway.ClaimHostLock(ctx, role)
	}
	if outcome == gateway.HostLockHeldByOther {
		described := "owner unknown"
		if owner := gateway.ReadHostRecord(role, false); owner != nil {
			described = gateway.DescribeHostRecord(owner)
		}
		if desktopChild {
			webServerLog().Debug(fmt.Sprintf("another Desktop backend holds the %s record (%s)", role, described))
			return
		}
		webServerLog().Warn(fmt.Sprintf("Another backend already owns this host (%s); this one bound anyway "+
			"(observe-only). Multiplex-only expects exactly one backend per host.", described))
		return
	}
	app.State.Set("host_role", role)
	gateway.PublishHostRecord(ctx, role, gateway.HostPublishOptions{Host: host, Port: &port,
		Profiles: gateway.HostServedProfiles(nil), Token: SessionToken()})
	gateway.HostCleanupOnExit(role)
}
