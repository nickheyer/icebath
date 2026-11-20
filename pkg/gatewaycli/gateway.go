package gatewaycli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// ProfileGatewayProcess is a running gateway pid mapped to the profile whose pid file names it
type ProfileGatewayProcess struct {
	Profile    string
	Path       string
	PID        int
	CreateTime float64
}

// Profile listing of the discovery walk, a seam over the profile lifecycle module
var listProfiles = func(ctx context.Context) ([]manage.ProfileInfo, error) { return manage.ListProfiles(ctx, false) }

// InspectError reports a profile whose gateway pid could not be inspected under strict discovery
type InspectError struct {
	Profile string
	Err     error
}

func (e *InspectError) Error() string {
	return fmt.Sprintf("Could not inspect gateway PID for profile %s", e.Profile)
}

func (e *InspectError) Unwrap() error { return e.Err }

// Running gateway pids mapped to profiles through their pid files, strict mode fails on ambiguous state
func FindProfileGatewayProcesses(ctx context.Context, excludePIDs map[int]bool, strict bool) ([]ProfileGatewayProcess, error) {
	processes := []ProfileGatewayProcess{}
	profiles, err := listProfiles(ctx)
	if err != nil {
		if strict {
			return nil, err
		}
		return processes, nil
	}
	seen := map[int]bool{}
	for _, profile := range profiles {
		pidPath := filepath.Join(profile.Path, "gateway.pid")
		var pid int
		var found bool
		createTime := 0.0
		if strict {
			pid, createTime, found, err = gateway.GetRunningPidIdentityStrict(pidPath)
			if !found {
				createTime = 0.0
			}
		} else {
			pid, found, err = gateway.GetRunningPid(pidPath, false)
		}
		if err != nil {
			if strict {
				return nil, &InspectError{Profile: profile.Name, Err: err}
			}
			continue
		}
		if !found || pid <= 0 || excludePIDs[pid] || seen[pid] {
			continue
		}
		seen[pid] = true
		processes = append(processes, ProfileGatewayProcess{Profile: profile.Name, Path: profile.Path, PID: pid, CreateTime: createTime})
	}
	return processes, nil
}
