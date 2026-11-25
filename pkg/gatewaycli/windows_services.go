package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

// Pid and creation time of one process incarnation
type ProcessIdentity struct {
	PID        int
	CreateTime float64
}

// A real Windows service supervising a profile gateway process tree
type WindowsGatewayService struct {
	Name                 string
	Profile              string
	ServicePID           int
	GatewayPID           int
	DescendantPIDs       []int
	DescendantIdentities []ProcessIdentity
	ServiceCreateTime    float64
	GatewayCreateTime    float64
}

// RuntimeError of SCM discovery with the failure that caused it
type WindowsServiceError struct {
	Msg   string
	Cause error
}

func (e *WindowsServiceError) Error() string { return e.Msg }

func (e *WindowsServiceError) Unwrap() error { return e.Cause }

// One enumerated SCM service whose fields are queried when read
type scmService interface {
	Name() (string, error)
	BinPath() (string, error)
	Status() (string, error)
	PID() (int, error)
}

var (
	windowsServicesGOOS = runtime.GOOS
	// Every Win32 service of the SCM in any state, assigned by the Windows build
	scmEnumerate func() ([]scmService, error)
	// Creation time of a live process as epoch seconds
	winProcessCreateTime = proc.Birth
	// Ancestors of a process nearest first
	winProcessParents = proc.Parents
	// Every live descendant of a process with its creation time
	winProcessDescendants = func(pid int) ([]ProcessIdentity, error) {
		table, err := proc.Snapshot()
		if err != nil {
			return nil, err
		}
		var out []ProcessIdentity
		for _, child := range table.Descendants(pid) {
			created, err := proc.Birth(child)
			if err != nil {
				return nil, err
			}
			out = append(out, ProcessIdentity{PID: child, CreateTime: created})
		}
		return out, nil
	}
	windowsProfileProcesses = func(ctx context.Context) ([]ProfileGatewayProcess, error) {
		return FindProfileGatewayProcesses(ctx, nil, true)
	}
	windowsServiceRoots = gateway.IcebathServiceRoots
)

// Failure of one SCM query carrying its category, AccessDenied or NoSuchProcess
type scmQueryError struct {
	Class string
	Err   error
}

func (e *scmQueryError) Error() string { return e.Class + ": " + e.Err.Error() }

func (e *scmQueryError) Unwrap() error { return e.Err }

// Whether an SCM query failed because the service disappeared after it was enumerated
func scmServiceVanished(err error) bool {
	var query *scmQueryError
	return !errors.As(err, &query) && errors.Is(err, fs.ErrNotExist)
}

// Whether a service configuration cannot be read because access is refused or the operating system failed the query
func scmConfigUnreadable(err error) bool {
	var query *scmQueryError
	if errors.As(err, &query) {
		return query.Class == "AccessDenied"
	}
	return errorutil.IsSystem(err)
}

type scmStatusEntry struct{ name, status string }

// Running owned services by pid and owned services in a transitional state by pid
func ownedWindowsServices(roots []string) (map[int]map[string]bool, map[int][]scmStatusEntry, error) {
	namesByPID := map[int]map[string]bool{}
	indeterminateByPID := map[int][]scmStatusEntry{}
	services, err := scmEnumerate()
	if err != nil {
		return nil, nil, err
	}
	inspection := func(cause error) error {
		return &WindowsServiceError{Msg: "SCM service inspection failed", Cause: cause}
	}
	for _, service := range services {
		name, err := service.Name()
		if err != nil {
			if scmServiceVanished(err) {
				continue
			}
			return nil, nil, inspection(err)
		}
		if name == "" {
			return nil, nil, inspection(&WindowsServiceError{Msg: "SCM service has an empty name"})
		}
		owned := gateway.IcebathOwnsWindowsService(name, "", roots)
		if !owned {
			binpath, err := service.BinPath()
			if err != nil {
				if scmConfigUnreadable(err) {
					continue
				}
				return nil, nil, inspection(err)
			}
			owned = gateway.IcebathOwnsWindowsService(name, binpath, roots)
		}
		if !owned {
			continue
		}
		status, err := service.Status()
		pid := 0
		if err == nil {
			pid, err = service.PID()
		}
		if err != nil {
			if scmServiceVanished(err) {
				continue
			}
			return nil, nil, inspection(err)
		}
		if status == "stopped" {
			continue
		}
		if status != "running" {
			if pid > 0 {
				indeterminateByPID[pid] = append(indeterminateByPID[pid], scmStatusEntry{name, status})
			}
			continue
		}
		if pid <= 0 {
			return nil, nil, &WindowsServiceError{Msg: fmt.Sprintf("Running SCM service %s has no valid process ID", name)}
		}
		if namesByPID[pid] == nil {
			namesByPID[pid] = map[string]bool{}
		}
		namesByPID[pid][name] = true
	}
	return namesByPID, indeterminateByPID, nil
}

// Profile gateways supervised by Icebath owned Windows services, sorted by service name, none off Windows
func FindWindowsGatewayServices(ctx context.Context) ([]WindowsGatewayService, error) {
	return findWindowsGatewayServices(ctx, nil, false)
}

// Gateways of the given profile processes that descend from a running Icebath owned service
func findWindowsGatewayServices(ctx context.Context, profileProcesses []ProfileGatewayProcess, haveProfiles bool) ([]WindowsGatewayService, error) {
	if windowsServicesGOOS != "windows" {
		return []WindowsGatewayService{}, nil
	}
	enumerationFailed := func(cause error) error {
		return &WindowsServiceError{Msg: "SCM service enumeration failed", Cause: cause}
	}
	if !haveProfiles {
		found, err := windowsProfileProcesses(ctx)
		if err != nil {
			return nil, enumerationFailed(err)
		}
		profileProcesses = found
	}
	namesByPID, indeterminateByPID, err := ownedWindowsServices(windowsServiceRoots(ctx))
	if err != nil {
		return nil, enumerationFailed(err)
	}
	found := map[string]WindowsGatewayService{}
	for _, profileProcess := range profileProcesses {
		service, err := windowsServiceOwning(profileProcess, namesByPID, indeterminateByPID)
		if err != nil {
			if _, isRuntime := err.(*WindowsServiceError); isRuntime {
				return nil, err
			}
			return nil, &WindowsServiceError{Msg: "Could not determine SCM ownership for gateway profile " + profileProcess.Profile, Cause: err}
		}
		if service != nil {
			found[service.Name] = *service
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]WindowsGatewayService, 0, len(names))
	for _, name := range names {
		out = append(out, found[name])
	}
	return out, nil
}

// Service whose process tree holds the profile gateway, nil when no single owned service is an ancestor
func windowsServiceOwning(profileProcess ProfileGatewayProcess, namesByPID map[int]map[string]bool, indeterminateByPID map[int][]scmStatusEntry) (*WindowsGatewayService, error) {
	gatewayCreateTime, err := winProcessCreateTime(profileProcess.PID)
	if err != nil {
		return nil, err
	}
	if profileProcess.CreateTime <= 0 || math.Abs(gatewayCreateTime-profileProcess.CreateTime) > 0.001 {
		return nil, &WindowsServiceError{Msg: "Gateway process identity changed during SCM discovery"}
	}
	ancestors, err := winProcessParents(profileProcess.PID)
	if err != nil {
		return nil, err
	}
	for _, pid := range ancestors {
		if entries := indeterminateByPID[pid]; len(entries) > 0 {
			return nil, &WindowsServiceError{Msg: fmt.Sprintf("SCM service %s has indeterminate status: %s", entries[0].name, entries[0].status)}
		}
	}
	var shared []string
	for _, pid := range ancestors {
		if len(namesByPID[pid]) > 1 {
			shared = append(shared, strconv.Itoa(pid))
		}
	}
	if len(shared) > 0 {
		return nil, &WindowsServiceError{Msg: "Gateway ownership is ambiguous under shared SCM host PID(s): " + strings.Join(shared, ", ")}
	}
	servicePID := 0
	for _, pid := range ancestors {
		if len(namesByPID[pid]) == 1 {
			servicePID = pid
			break
		}
	}
	if servicePID == 0 {
		return nil, nil
	}
	serviceName := ""
	for name := range namesByPID[servicePID] {
		serviceName = name
	}
	serviceCreateTime, err := winProcessCreateTime(servicePID)
	if err != nil {
		return nil, err
	}
	descendants, err := winProcessDescendants(servicePID)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var pids []int
	for _, child := range descendants {
		if !seen[child.PID] {
			seen[child.PID] = true
			pids = append(pids, child.PID)
		}
	}
	if !seen[profileProcess.PID] {
		return nil, nil
	}
	sort.Ints(pids)
	identities := append([]ProcessIdentity(nil), descendants...)
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].PID != identities[j].PID {
			return identities[i].PID < identities[j].PID
		}
		return identities[i].CreateTime < identities[j].CreateTime
	})
	return &WindowsGatewayService{Name: serviceName, Profile: profileProcess.Profile, ServicePID: servicePID, GatewayPID: profileProcess.PID,
		DescendantPIDs: pids, DescendantIdentities: identities, ServiceCreateTime: serviceCreateTime, GatewayCreateTime: gatewayCreateTime}, nil
}
