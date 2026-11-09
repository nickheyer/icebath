package gatewaycli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// Unified view of gateway liveness for the current profile
type GatewayRuntimeSnapshot struct {
	Manager          string
	ServiceInstalled bool
	ServiceRunning   bool
	GatewayPIDs      []int
	ServiceScope     string
}

// Reports a running service or a scanned gateway process
func (s GatewayRuntimeSnapshot) Running() bool { return s.ServiceRunning || len(s.GatewayPIDs) > 0 }

// Reports an installed service whose gateway runs outside it
func (s GatewayRuntimeSnapshot) HasProcessServiceMismatch() bool {
	return s.ServiceInstalled && s.Running() && !s.ServiceRunning
}

// Label of the systemd scope, system or user
func serviceScopeLabel(system bool) string {
	if system {
		return "system"
	}
	return "user"
}

// Snapshot for an s6 supervised container gateway, false when s6 is not the service manager
func s6GatewaySnapshot(ctx context.Context, pids []int) (GatewayRuntimeSnapshot, bool, error) {
	manager, err := DetectServiceManager()
	if err != nil {
		return GatewayRuntimeSnapshot{}, false, err
	}
	if manager != "s6" {
		return GatewayRuntimeSnapshot{}, false, nil
	}
	profile, err := gateway.CurrentProfileName(ctx)
	if err != nil {
		return GatewayRuntimeSnapshot{}, false, err
	}
	serviceName := "gateway-" + profile
	mgr, err := GetServiceManager()
	if err != nil {
		return GatewayRuntimeSnapshot{}, false, err
	}
	installed := false
	if s6, ok := mgr.(*S6ServiceManager); ok {
		installed = isDirPath(filepath.Join(s6.Scandir, serviceName))
	}
	running := false
	if installed {
		live, err := mgr.IsRunning(serviceName)
		running = err == nil && live
	}
	return GatewayRuntimeSnapshot{Manager: "s6 (container supervisor)", ServiceInstalled: installed, ServiceRunning: running,
		GatewayPIDs: pids, ServiceScope: "s6"}, true, nil
}

// Reports whether a path exists
func snapshotPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Gateway liveness of the current profile under its service manager, Termux, s6, docker, systemd, launchd or a bare process
func GetGatewayRuntimeSnapshot(ctx context.Context, system bool) (GatewayRuntimeSnapshot, error) {
	pids, err := gateway.FindGatewayPIDsFor(ctx, nil, false)
	if err != nil {
		return GatewayRuntimeSnapshot{}, err
	}
	if hostinfo.IsTermux() {
		return GatewayRuntimeSnapshot{Manager: "Termux / manual process", GatewayPIDs: pids}, nil
	}
	if runtime.GOOS == "linux" && hostinfo.IsContainer() {
		if snapshot, ok, err := s6GatewaySnapshot(ctx, pids); err == nil && ok {
			return snapshot, nil
		}
		return GatewayRuntimeSnapshot{Manager: "docker (foreground)", GatewayPIDs: pids}, nil
	}
	if gateway.SupportsSystemdServices(ctx) {
		selected, running, err := gateway.ProbeSystemdServiceRunning(ctx, system)
		if err != nil {
			return GatewayRuntimeSnapshot{}, err
		}
		unit, err := gateway.GetSystemdUnitPath(ctx, selected)
		if err != nil {
			return GatewayRuntimeSnapshot{}, err
		}
		scope := serviceScopeLabel(selected)
		return GatewayRuntimeSnapshot{Manager: "systemd (" + scope + ")", ServiceInstalled: snapshotPathExists(unit),
			ServiceRunning: running, GatewayPIDs: pids, ServiceScope: scope}, nil
	}
	if runtime.GOOS == "darwin" {
		plist, err := gateway.GetLaunchdPlistPath(ctx)
		if err != nil {
			return GatewayRuntimeSnapshot{}, err
		}
		running, err := gateway.ProbeLaunchdServiceRunning(ctx)
		if err != nil {
			return GatewayRuntimeSnapshot{}, err
		}
		return GatewayRuntimeSnapshot{Manager: "launchd", ServiceInstalled: snapshotPathExists(plist), ServiceRunning: running,
			GatewayPIDs: pids, ServiceScope: "launchd"}, nil
	}
	return GatewayRuntimeSnapshot{Manager: "manual process", GatewayPIDs: pids}, nil
}
