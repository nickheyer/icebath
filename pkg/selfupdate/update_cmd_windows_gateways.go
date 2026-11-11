package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	serviceReadyTimeoutS = 60.0
	serviceReadyProbeS   = 5.0
	serviceRetryBaseS    = 60.0
	serviceRetryMaxS     = 3600.0
	readyIntervalS       = 0.4
)

// Liveness budget of the post relaunch poll and its confirmation window, in seconds
var (
	relaunchVerifyTimeoutS = 30.0
	readyConfirmS          = 2.0
)

// Service and gateway calls of the Windows pause and resume, the calls its tests replace
var (
	winServiceGet           = gatewaycli.WinServiceGet
	winFindProfileProcesses = func(ctx context.Context) ([]gatewaycli.ProfileGatewayProcess, error) {
		return gatewaycli.FindProfileGatewayProcesses(ctx, nil, true)
	}
	winFindServices          = gatewaycli.FindWindowsGatewayServicesFor
	winStopGateways          = stopWindowsGateways
	winRecordAttested        = recordAttestedColdStartProfiles
	winWriteAttestation      = gateway.WriteStartAttestation
	winRecordRestart         = updaterecord.RecordGatewayRestart
	winUnregisterExitHook    = unregisterUpdateExitHook
	winStopService           = stopWindowsGatewayService
	winStartService          = startWindowsGatewayService
	winRestoreService        = restoreWindowsGatewayService
	winServiceGatewayReady   = serviceGatewayReady
	winOwnedGatewayPids      = ownedGatewayPids
	winDesktopOwnsLifecycle  = desktopOwnsGatewayLifecycle
	winColdStartGateway      = coldStartWindowsGatewayAfterUpdate
	winRefreshLaunchers      = refreshWindowsGatewayLaunchers
	winLaunchDetachedRestart = gateway.LaunchDetachedProfileGatewayRestart
	winResumeAfterUpdate     = resumeWindowsGatewaysAfterUpdate
	winLiveGatewayPids       = liveGatewayPids
	winWaitForGatewayReady   = waitForGatewayReady
	winGatewayPidHome        = gateway.GatewayPidHome
	winProcessStartTime      = gateway.GetProcessStartTime
	winLooksLikeGateway      = gateway.LooksLikeGatewayProcess
	winNow                   = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	bootstrapUnsafeRef       = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	bootstrapCommitRef       = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
)

func init() {
	updaterecord.SetPauseGatewayResumer(func(ctx context.Context, token map[string]any, stdout io.Writer) error {
		return resumeWindowsGatewaysAfterUpdate(ctx, stdout, token)
	})
}

// Strings of a token list, a typed list read the same as a decoded one
func tokenStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return append([]string{}, list...)
	case []any:
		out := make([]string, len(list))
		for i, item := range list {
			out[i] = pyrepr.Str(item)
		}
		return out
	}
	return nil
}

func tokenMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func stringsAsAny(items []string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func sortedKeysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// The named SCM service
func winService(name string) (gatewaycli.WinService, error) { return winServiceGet(name) }

// Runs sc.exe with a verb, a failed exit an error unless the SCM already reports the settled status
func scExe(ctx context.Context, verb, name string, service gatewaycli.WinService, settledStatus string) error {
	result, err := pysubprocess.RunWith(ctx, []string{"sc.exe", verb, name}, pysubprocess.Options{Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	if result.Returncode == 0 {
		return nil
	}
	status, err := service.Status()
	if err != nil {
		return err
	}
	if status == settledStatus {
		return nil
	}
	detail := pystr.DecodeUTF8Replace(result.Stderr)
	if detail == "" {
		detail = pystr.DecodeUTF8Replace(result.Stdout)
	}
	if detail = pystr.Strip(detail); detail == "" {
		detail = fmt.Sprintf("sc.exe %s failed with %d", verb, result.Returncode)
	}
	return errors.New(detail)
}

// Whether the process of this exact pid and creation time still lives, an unreadable one counting as alive
func originalProcessIsAlive(pid int, createTime float64) bool {
	current, err := winBirth(pid)
	if errors.Is(err, proc.ErrNoSuchProcess) {
		return false
	}
	if err != nil {
		return true
	}
	return math.Abs(current-createTime) <= 0.001
}

// Creation time of a pid, an unreadable identity aborting the stop
func processCreateTime(pid int, label string) (float64, error) {
	created, err := winBirth(pid)
	if err != nil {
		return 0, &stepAborted{Msg: fmt.Sprintf("Windows %s process identity is unavailable before stop", label), Cause: err}
	}
	return created, nil
}

// Refuses to stop a service unless SCM state, process identities and ancestry all still match
func verifyServiceIdentities(name string, service gatewaycli.WinService, expectedService, expectedGateway *gatewaycli.ProcessIdentity) error {
	if expectedService != nil {
		status, err := service.Status()
		servicePid := 0
		if err == nil {
			servicePid, err = service.PID()
		}
		if err != nil {
			return &stepAborted{Msg: fmt.Sprintf("Windows service %s SCM identity is unavailable before stop", name), Cause: err}
		}
		if status != "running" {
			return fmt.Errorf("Windows service %s is not stably running before stop: %s", name, status)
		}
		if servicePid != expectedService.PID {
			return fmt.Errorf("Windows service %s SCM process identity changed before stop", name)
		}
	}
	for _, item := range []struct {
		label    string
		identity *gatewaycli.ProcessIdentity
	}{{"service", expectedService}, {"gateway", expectedGateway}} {
		if item.identity == nil {
			continue
		}
		created, err := processCreateTime(item.identity.PID, item.label)
		if err != nil {
			return err
		}
		if math.Abs(created-item.identity.CreateTime) > 0.001 {
			return fmt.Errorf("Windows %s process identity changed before stop", item.label)
		}
	}
	if expectedService != nil && expectedGateway != nil {
		ancestors, err := winParents(expectedGateway.PID)
		if err != nil {
			return &stepAborted{Msg: "Windows gateway ancestry is unavailable before service stop", Cause: err}
		}
		owned := false
		for _, ancestor := range ancestors {
			owned = owned || ancestor == expectedService.PID
		}
		if !owned {
			return fmt.Errorf("Windows gateway is no longer owned by service %s", name)
		}
	}
	return nil
}

// Polls an SCM condition every fifth of a second, the first probe failure ending the wait
func winPoll(timeout float64, settled func() (bool, error)) (bool, error) {
	var probeErr error
	done := pydeadline.PollUntil(func() (bool, bool) {
		ok, err := settled()
		if err != nil {
			probeErr = err
			return false, true
		}
		return ok, ok
	}, time.Duration(timeout*float64(time.Second)), 200*time.Millisecond)
	return done && probeErr == nil, probeErr
}

// Stops one verified Windows service and waits until the SCM reports it down with its process tree gone
func stopWindowsGatewayService(ctx context.Context, name string, expectedProcesses []gatewaycli.ProcessIdentity, expectedService, expectedGateway *gatewaycli.ProcessIdentity, timeout float64) error {
	service, err := winService(name)
	if err != nil {
		return err
	}
	if err := verifyServiceIdentities(name, service, expectedService, expectedGateway); err != nil {
		return err
	}
	if err := scExe(ctx, "stop", name, service, "stopped"); err != nil {
		return err
	}
	alive := func() []int {
		var pids []int
		for _, identity := range expectedProcesses {
			if originalProcessIsAlive(identity.PID, identity.CreateTime) {
				pids = append(pids, identity.PID)
			}
		}
		return pids
	}
	stopped, err := winPoll(timeout, func() (bool, error) {
		status, err := service.Status()
		return err == nil && status == "stopped" && len(alive()) == 0, err
	})
	if err != nil || stopped {
		return err
	}
	status, err := service.Status()
	if err != nil {
		return err
	}
	if status != "stopped" {
		return fmt.Errorf("Windows service %s did not stop within %.0fs; venv mutation unsafe.", name, timeout)
	}
	if left := alive(); len(left) > 0 {
		return fmt.Errorf("Windows service %s stopped but its process tree is still alive: %s", name, pyrepr.Repr(intsAsAny(left)))
	}
	return nil
}

func intsAsAny(items []int) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// Starts one previously paused Windows service and verifies it is running
func startWindowsGatewayService(ctx context.Context, name string, timeout float64) error {
	service, err := winService(name)
	if err != nil {
		return err
	}
	if err := scExe(ctx, "start", name, service, "running"); err != nil {
		return err
	}
	running, err := winPoll(timeout, func() (bool, error) {
		status, err := service.Status()
		return status == "running", err
	})
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("Windows service %s did not start within %.0fs", name, timeout)
	}
	return nil
}

// Restores a service after an uncertain stop, a stop still pending included
func restoreWindowsGatewayService(ctx context.Context, name string, timeout float64) error {
	service, err := winService(name)
	if err != nil {
		return err
	}
	settled, err := winPoll(timeout, func() (bool, error) {
		status, err := service.Status()
		if err != nil {
			return false, err
		}
		if status == "stopped" {
			if err := winStartService(ctx, name, 30.0); err != nil {
				return false, err
			}
		}
		return status == "running" || status == "stopped", nil
	})
	if err != nil {
		return err
	}
	if !settled {
		return fmt.Errorf("Windows service %s did not reach a restorable state within %.0fs", name, timeout)
	}
	return nil
}

// Pause token of the no running gateway case when an autostart entry exists and Desktop does not own the lifecycle
func windowsColdStartPlan(ctx context.Context) map[string]any {
	installed, err := gateway.WindowsIsInstalled(ctx)
	if err != nil {
		updateLog.DebugContext(ctx, fmt.Sprintf("Could not check Windows gateway autostart state before update: %s", launchErrorText(err)))
		return nil
	}
	if !installed {
		return nil
	}
	token := map[string]any{"resume_needed": true, "profiles": map[string]any{}, "unmapped_pids": []any{}, "unmapped": []any{}, "cold_start_if_installed": true}
	if winDesktopOwnsLifecycle(ctx) {
		generation, err := attestedDeathGeneration(ctx, []int{}, "")
		if err != nil {
			updateLog.DebugContext(ctx, fmt.Sprintf("Could not check Desktop gateway-lifecycle ownership before update: %s", launchErrorText(err)))
			return token
		}
		if generation == "" {
			updateLog.DebugContext(ctx, "Skipping Windows gateway cold-start plan: Desktop owns gateway lifecycle")
			return nil
		}
		token["attested_generation"] = generation
	}
	return token
}

// A gateway service could not be stopped, with what the rollback could not restart
type ServicePauseFailed struct {
	Msg              string
	RollbackFailures []string
	Cause            error
}

func (e *ServicePauseFailed) Error() string { return e.Msg }

func (e *ServicePauseFailed) Unwrap() error { return e.Cause }

// Stops each SCM gateway service and records them on the token, rolling everything back on a failure
func pauseWindowsGatewayServices(ctx context.Context, w io.Writer, serviceGateways []gatewaycli.WindowsGatewayService, token, profilesPaused map[string]any, unmapped []any) (map[string]any, error) {
	var paused []string
	current := ""
	var failure error
	for _, service := range serviceGateways {
		current = service.Name
		failure = winStopService(ctx, current, service.DescendantIdentities,
			&gatewaycli.ProcessIdentity{PID: service.ServicePID, CreateTime: service.ServiceCreateTime},
			&gatewaycli.ProcessIdentity{PID: service.GatewayPID, CreateTime: service.GatewayCreateTime}, 30.0)
		if failure != nil {
			break
		}
		paused = append(paused, current)
		current = ""
	}
	if failure == nil {
		if len(paused) > 0 {
			token["services"], token["expected_services"], token["restarted_services"] = stringsAsAny(paused), stringsAsAny(paused), []any{}
			owners := map[string]any{}
			for _, service := range serviceGateways {
				owners[service.Name] = service.Profile
			}
			token["service_profiles"] = owners
			fmt.Fprintln(w, "  ✓ Paused Windows gateway service(s): "+strings.Join(paused, ", "))
		}
		return token, nil
	}
	var restore []string
	if current != "" {
		restore = append(restore, current)
	}
	for i := len(paused) - 1; i >= 0; i-- {
		restore = append(restore, paused[i])
	}
	var rollbackFailures []string
	seen := map[string]bool{}
	for _, name := range restore {
		if seen[name] {
			continue
		}
		seen[name] = true
		if err := winRestoreService(ctx, name, 60.0); err != nil {
			rollbackFailures = append(rollbackFailures, name+": "+launchErrorText(err))
		}
	}
	if len(profilesPaused) > 0 || len(unmapped) > 0 {
		if err := winResumeAfterUpdate(ctx, w, token); err != nil {
			rollbackFailures = append(rollbackFailures, "ordinary gateways: "+launchErrorText(err))
		}
	}
	shown := current
	if shown == "" {
		shown = "unknown"
	}
	detail := fmt.Sprintf("Could not stop Windows gateway service %s: %s", shown, launchErrorText(failure))
	if len(rollbackFailures) > 0 {
		detail += "; rollback failures: " + strings.Join(rollbackFailures, "; ")
	}
	return nil, &ServicePauseFailed{Msg: detail, RollbackFailures: rollbackFailures, Cause: failure}
}

// The pids whose live home this update owns plus the kept ones, every other gateway left running
func ownedGatewayPids(ctx context.Context, w io.Writer, pids []int, keep map[int]bool, quiet bool) ([]int, error) {
	return scopedManualGatewayPids(ctx, w, pids, keep, quiet)
}

// Profile processes, service gateways, their gateway pids and the running pids this install owns
func discoverWindowsGateways(ctx context.Context, w io.Writer) (map[int]gatewaycli.ProfileGatewayProcess, []gatewaycli.WindowsGatewayService, map[int]bool, []int, error) {
	list, err := winFindProfileProcesses(ctx)
	if err != nil {
		return nil, nil, nil, nil, abortOnError("Could not map Windows gateway PIDs to profiles", err)
	}
	processes := map[int]gatewaycli.ProfileGatewayProcess{}
	for _, process := range list {
		processes[process.PID] = process
	}
	services, err := winFindServices(ctx, list)
	if err != nil {
		return nil, nil, nil, nil, abortOnError("Could not determine Windows gateway service ownership", err)
	}
	servicePids := map[int]bool{}
	for _, service := range services {
		servicePids[service.GatewayPID] = true
	}
	found, err := winFindGatewayPids(ctx)
	var running []int
	if err == nil {
		keep := map[int]bool{}
		var mapped, supervised []int
		for pid := range processes {
			keep[pid] = true
			mapped = append(mapped, pid)
		}
		for pid := range servicePids {
			keep[pid] = true
			supervised = append(supervised, pid)
		}
		sort.Ints(mapped)
		sort.Ints(supervised)
		seen := map[int]bool{}
		var candidates []int
		for _, pid := range append(append(found, mapped...), supervised...) {
			if !seen[pid] {
				seen[pid] = true
				candidates = append(candidates, pid)
			}
		}
		running, err = winOwnedGatewayPids(ctx, w, candidates, keep, false)
	}
	if err != nil {
		return nil, nil, nil, nil, abortOnError("Could not discover Windows gateway PIDs before update", err)
	}
	return processes, services, servicePids, running, nil
}

// Marker and socket first pause of every profile mapped gateway, the paused profiles, their pids and the socket acknowledgements
func requestSocketPauses(ctx context.Context, runningPids []int, processes map[int]gatewaycli.ProfileGatewayProcess, servicePids map[int]bool, onRequest func(pid int) error) (map[string]any, []int, []map[string]any, error) {
	paused := map[string]any{}
	var mapped []int
	var acks []map[string]any
	for _, pid := range runningPids {
		process, ok := processes[pid]
		if !ok || servicePids[pid] {
			continue
		}
		paused[process.Profile] = pid
		mapped = append(mapped, pid)
		requested, err := writeUpdatePlannedStopMarker(ctx, process.Path, pid)
		if err != nil {
			return nil, nil, nil, err
		}
		if requested && onRequest != nil {
			if err := onRequest(pid); err != nil {
				return nil, nil, nil, err
			}
		}
		ack := gateway.PauseGatewayForUpdate(process.Path, 2*time.Second)
		if ack == nil || ack.Len() == 0 || !(pyrepr.Truthy(ack.GetOr("pausing", nil)) || pyrepr.Truthy(ack.GetOr("already_stopping", nil))) {
			continue
		}
		plain := map[string]any{}
		for _, key := range ack.Keys {
			plain[key] = ack.GetOr(key, nil)
		}
		acks = append(acks, plain)
		if !requested && onRequest != nil {
			if err := onRequest(pid); err != nil {
				updateLog.DebugContext(ctx, fmt.Sprintf("Socket pause unavailable for gateway %d: %s", pid, launchErrorText(err)))
			}
		}
	}
	return paused, mapped, acks, nil
}

// Drain budget, the configured restart drain raised to a socket paused gateway's declared budget plus teardown grace
func gatewayDrainTimeout(ctx context.Context, w io.Writer, acks []map[string]any) float64 {
	drain := 10.0
	if configured, err := gateway.GetRestartDrainTimeout(ctx); err == nil {
		drain = math.Max(configured, 1.0)
	}
	if len(acks) == 0 {
		return drain
	}
	declared, known := 0.0, true
	for i, ack := range acks {
		value := 0.0
		if pyrepr.Truthy(ack["drain_timeout"]) {
			number, ok := winFloat(ack["drain_timeout"])
			if !ok {
				known = false
				break
			}
			value = number
		}
		if i == 0 || value > declared {
			declared = value
		}
	}
	if known {
		drain = math.Max(drain, declared+10.0)
	}
	fmt.Fprintf(w, "  → %d gateway(s) ACKed socket pause; waiting up to %ds for graceful exit\n", len(acks), int(drain))
	return drain
}

// Python float of a decoded number or numeric text
func winFloat(v any) (float64, bool) {
	switch number := v.(type) {
	case float64:
		return number, true
	case bool:
		if number {
			return 1, true
		}
		return 0, true
	case string:
		parsed, err := strconv.ParseFloat(pystr.Strip(number), 64)
		return parsed, err == nil
	}
	if number, ok := pyIntValue(v); ok {
		return float64(number), true
	}
	return 0, false
}

// Stops running Windows gateways before the checkout or venv changes, the paused set recorded before the first stop
func pauseWindowsGatewaysForUpdate(ctx context.Context, w io.Writer) (map[string]any, error) {
	if !winIsWindows() {
		return nil, nil
	}
	adopted, claims, err := updaterecord.AdoptPauseOrphans(ctx)
	if err != nil {
		return nil, abortOnError("Could not read the gateways an earlier update paused", err)
	}
	processes, services, servicePids, running, err := discoverWindowsGateways(ctx, w)
	if err != nil {
		return nil, err
	}
	if len(running) == 0 {
		return coldStartPauseToken(ctx, adopted, claims)
	}
	var unmappedPids []int
	born := map[int]any{}
	for _, pid := range running {
		if _, mapped := processes[pid]; !mapped && !servicePids[pid] {
			unmappedPids = append(unmappedPids, pid)
		}
		born[pid] = winProcessStartTime(pid)
	}
	homes := map[int]string{}
	var unproven []string
	for _, pid := range unmappedPids {
		if homes[pid] = winGatewayPidHome(ctx, pid); homes[pid] == "" {
			unproven = append(unproven, strconv.Itoa(pid))
		}
	}
	if len(unproven) > 0 {
		return nil, errors.New("Could not prove the Icebath home of unmapped gateway PID(s) " + strings.Join(unproven, ", ") + "; nothing was stopped")
	}
	unmapped := []any{}
	for _, pid := range unmappedPids {
		var argv any
		if captured := gateway.CaptureGatewayArgv(pid); captured != nil {
			argv = stringsAsAny(captured)
		}
		unmapped = append(unmapped, map[string]any{"pid": pid, "argv": argv, "home": homes[pid], "ct": born[pid]})
	}
	mappedProfiles, identities := map[string]any{}, map[string]any{}
	paths := map[int]string{}
	for _, pid := range running {
		if process, mapped := processes[pid]; mapped {
			paths[pid] = process.Path
			if !servicePids[pid] {
				mappedProfiles[process.Profile] = pid
			}
		}
		identities[strconv.Itoa(pid)] = updaterecord.PauseIdentity(pid)["ct"]
	}
	intended := map[string]any{"resume_needed": true, "unmapped_pids": intsAsAny(unmappedPids), "unmapped": unmapped,
		"profiles": mappedProfiles, "identities": identities}
	if len(services) > 0 {
		names, owners := make([]string, len(services)), map[string]any{}
		for i, service := range services {
			names[i], owners[service.Name] = service.Name, service.Profile
		}
		intended["services"], intended["expected_services"], intended["restarted_services"] = stringsAsAny(names), stringsAsAny(names), []any{}
		intended["service_profiles"] = owners
	}
	if _, err = updaterecord.RecordPause(ctx, intended, adopted, claims); err == nil {
		err = updaterecord.MarkStopRequested(ctx, intended, running, plannedStopMarkers(running, paths, servicePids))
	}
	if err != nil {
		return nil, abortOnError("Could not record the gateways this update pauses", err)
	}
	paused, err := winStopGateways(ctx, w, running, processes, servicePids, unmappedPids, unmapped,
		func(pid int) error { return updaterecord.MarkStopSent(ctx, intended, pid) }, born)
	if err != nil {
		return nil, err
	}
	token := map[string]any{"resume_needed": true, "profiles": paused, "unmapped_pids": intsAsAny(unmappedPids), "unmapped": unmapped}
	runningProfiles := map[string]bool{}
	for name := range paused {
		runningProfiles[name] = true
	}
	for _, process := range processes {
		runningProfiles[process.Profile] = true
	}
	for _, service := range services {
		runningProfiles[service.Profile] = true
	}
	winRecordAttested(ctx, token, runningProfiles)
	token, err = pauseWindowsGatewayServices(ctx, w, services, token, paused, unmapped)
	if err != nil {
		var failed *ServicePauseFailed
		if errors.As(err, &failed) && len(failed.RollbackFailures) == 0 {
			if abandonErr := updaterecord.AbandonPause(ctx, intended, adopted); abandonErr != nil {
				return nil, abandonErr
			}
		}
		return nil, err
	}
	return updaterecord.FinishPause(ctx, token, intended, adopted)
}

// The cold start plan of an install with no running gateway, plus any orphaned set adopted from a killed update
func coldStartPauseToken(ctx context.Context, adopted map[string]any, claims []string) (map[string]any, error) {
	token := windowsColdStartPlan(ctx)
	probe := token
	if probe == nil {
		probe = map[string]any{"resume_needed": true, "profiles": map[string]any{}, "unmapped_pids": []any{}, "unmapped": []any{}}
	}
	winRecordAttested(ctx, probe, map[string]bool{})
	if pyrepr.Truthy(probe["cold_start_profiles"]) {
		token = probe
	}
	if adopted == nil {
		if token == nil {
			return nil, nil
		}
		return updaterecord.StampTree(ctx, token, "")
	}
	if token == nil {
		token = map[string]any{"resume_needed": true, "profiles": map[string]any{}, "unmapped_pids": []any{}, "unmapped": []any{}}
	}
	recorded, err := updaterecord.RecordPause(ctx, token, adopted, claims)
	return recorded, abortOnError("Could not record the adopted paused-gateway set", err)
}

// Socket pauses the mapped gateways, drains, then tree kills survivors under the birth each was discovered with
func stopWindowsGateways(ctx context.Context, w io.Writer, runningPids []int, processes map[int]gatewaycli.ProfileGatewayProcess, servicePids map[int]bool,
	unmappedPids []int, unmapped []any, onRequest func(pid int) error, born map[int]any) (map[string]any, error) {
	var mapped []int
	births := map[int]any{}
	for pid, stamp := range born {
		births[pid] = stamp
	}
	for _, pid := range runningPids {
		process, ok := processes[pid]
		if !ok || servicePids[pid] {
			continue
		}
		mapped = append(mapped, pid)
		if process.CreateTime > 0 {
			births[pid] = int64(math.RoundToEven(process.CreateTime * 100))
		}
	}
	launchers := venvLauncherAncestors(mapped)
	for _, pid := range launchers {
		if _, known := births[pid]; !known {
			births[pid] = winProcessStartTime(pid)
		}
	}
	paused, mappedPids, acks, err := requestSocketPauses(ctx, runningPids, processes, servicePids, onRequest)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(w, "→ Stopping Windows gateway process(es) before updating Icebath...")
	survivors := waitForWindowsUpdateGatewayExit(mappedPids, gatewayDrainTimeout(ctx, w, acks))
	targets := map[int]bool{}
	for pid := range survivors {
		targets[pid] = true
	}
	for _, pid := range append(append([]int{}, unmappedPids...), launchers...) {
		targets[pid] = true
	}
	ordered := make([]int, 0, len(targets))
	for pid := range targets {
		ordered = append(ordered, pid)
	}
	sort.Ints(ordered)
	forceKilled := 0
	for _, pid := range ordered {
		err := gateway.TerminatePid(pid, true, births[pid])
		if err != nil && !isUpdateOSError(err) {
			return nil, err
		}
		if err == nil {
			forceKilled++
		}
	}
	if len(paused) > 0 {
		fmt.Fprintf(w, "  ✓ Paused gateway profile(s): %s\n", strings.Join(sortedKeysOf(paused), ", "))
	}
	if forceKilled > 0 {
		fmt.Fprintf(w, "  → Force-stopped %d gateway process(es)\n", forceKilled)
	}
	if len(unmappedPids) > 0 {
		fmt.Fprintf(w, "  → Stopped %d gateway process(es) without profile mapping\n", len(unmappedPids))
		for _, entry := range tokenEntries(unmapped) {
			if !pyrepr.Truthy(entry["argv"]) {
				fmt.Fprintln(w, "    Restart manually after update: icebath gateway run")
				break
			}
		}
	}
	return paused, nil
}

// Records a cold start obligation per known profile that is not running yet holds a dead start attestation
func recordAttestedColdStartProfiles(ctx context.Context, token map[string]any, runningProfiles map[string]bool) {
	if !winDesktopOwnsLifecycle(ctx) {
		return
	}
	active := profiles.GetActiveProfileName(ctx)
	if active == "" {
		active = "default"
	}
	served, err := profiles.ProfilesToServe(ctx, true, true, false)
	cold := map[string]any{}
	for _, profile := range served {
		if err != nil {
			break
		}
		if runningProfiles[profile.Name] || (profile.Name == active && pyrepr.Truthy(token["cold_start_if_installed"])) {
			continue
		}
		var generation string
		if generation, err = attestedDeathGeneration(ctx, []int{}, profile.Home); err == nil && generation != "" {
			cold[profile.Name] = generation
		}
	}
	if err != nil {
		updateLog.DebugContext(ctx, fmt.Sprintf("Could not evaluate per-profile attested cold-starts before update: %s", launchErrorText(err)))
		return
	}
	if len(cold) > 0 {
		token["cold_start_profiles"] = cold
	}
}

// Spawns each attested profile under its own home and consumes exactly the generation that authorized it
func coldStartAttestedProfiles(ctx context.Context, w io.Writer, token map[string]any) error {
	pending := tokenMap(token["cold_start_profiles"])
	if len(pending) == 0 {
		return nil
	}
	remaining := map[string]any{}
	for name, generation := range pending {
		remaining[name] = generation
	}
	token["cold_start_profiles"] = remaining
	for _, name := range sortedKeysOf(pending) {
		err := func() error {
			profileHome, err := profiles.GetProfileDir(name)
			if err != nil {
				return err
			}
			live, err := winLiveGatewayPids(ctx, profileHome, nil)
			if err != nil {
				return err
			}
			if len(live) == 0 {
				pid, err := spawnDetached(ctx, profileHome)
				if err != nil {
					return err
				}
				if pid == 0 {
					return errors.New("cold-start did not return a process ID")
				}
			}
			ready, err := winWaitForGatewayReady(ctx, nil, profileHome, false, nil)
			if err != nil {
				return err
			}
			if len(ready) == 0 {
				return fmt.Errorf("gateway profile %s did not become ready", name)
			}
			if err := consumeStartAttestation(ctx, pyrepr.Str(pending[name]), profileHome); err != nil {
				return err
			}
			winWriteAttestation(home.WithOverride(ctx, profileHome), ready, fmt.Sprintf("cold-start after update (profile %s)", name))
			fmt.Fprintf(w, "\n✓ Gateway profile %s started via cold-start after update (PID: %d)\n", name, ready[0])
			delete(remaining, name)
			return nil
		}()
		if err != nil {
			if isUpdateExit(err) {
				return err
			}
			updateLog.DebugContext(ctx, fmt.Sprintf("Could not cold-start Windows gateway profile %s after update: %s", name, launchErrorText(err)))
		}
	}
	if len(remaining) > 0 {
		return errors.New("Windows gateway cold-start was not verified for profile(s): " + strings.Join(sortedKeysOf(remaining), ", "))
	}
	delete(token, "cold_start_profiles")
	return nil
}

// Spawns a detached gateway after the update for an installed but down autostart, verified by the liveness poll
func coldStartWindowsGatewayAfterUpdate(ctx context.Context, w io.Writer, token map[string]any) (bool, error) {
	if !winIsWindows() {
		return true, nil
	}
	found, err := winFindGatewayPids(ctx)
	var owned []int
	if err == nil {
		owned, err = winOwnedGatewayPids(ctx, w, found, nil, true)
	}
	if err != nil {
		return false, abortOnError("Could not re-check gateway liveness before cold-start", err)
	}
	if len(owned) > 0 {
		return true, nil
	}
	generation := ""
	if recorded, carried := token["attested_generation"]; carried {
		if recorded != nil {
			generation = pyrepr.Str(recorded)
		}
	} else if generation, err = attestedDeathGeneration(ctx, []int{}, ""); err != nil {
		return false, abortOnError("Could not re-read the start attestation before cold-start", err)
	}
	if winDesktopOwnsLifecycle(ctx) && generation == "" {
		updateLog.DebugContext(ctx, "Skipping Windows gateway cold-start: Desktop owns gateway lifecycle")
		return true, nil
	}
	pid, err := spawnDetached(ctx, "")
	if err != nil {
		return false, abortOnError("Could not cold-start Windows gateway after update", err)
	}
	if pid == 0 {
		return false, errors.New("Windows gateway cold-start did not return a process ID")
	}
	ready, err := winWaitForGatewayReady(ctx, nil, "", false, nil)
	if err != nil {
		return false, err
	}
	if len(ready) == 0 {
		return false, fmt.Errorf("Windows gateway cold-start PID %d did not become ready", pid)
	}
	if generation != "" {
		if err := consumeStartAttestation(ctx, generation, ""); err != nil {
			return false, err
		}
	}
	shown := make([]string, len(ready))
	for i, readyPid := range ready {
		shown[i] = strconv.Itoa(readyPid)
	}
	fmt.Fprintf(w, "\n✓ Gateway started via cold-start after update (PID: %s)\n", strings.Join(shown, ", "))
	winWriteAttestation(ctx, ready, "cold-start after update")
	return true, nil
}

// Regenerates the installed Windows gateway launcher scripts after an update, never failing it
func refreshWindowsGatewayLaunchers(ctx context.Context, w io.Writer) {
	if !winIsWindows() {
		return
	}
	err := func() error {
		installed, err := gateway.WindowsIsInstalled(ctx)
		if err != nil || !installed {
			return err
		}
		if _, err := gateway.WriteWindowsTaskScript(ctx); err != nil {
			return err
		}
		fmt.Fprintln(w, "  ✓ Refreshed Windows gateway launcher scripts")
		done, warnings, err := gateway.ReconcileAutostartLaunchers(ctx)
		if err != nil {
			return err
		}
		for _, message := range done {
			fmt.Fprintf(w, "  ✓ %s\n", message)
		}
		for _, message := range warnings {
			fmt.Fprintf(w, "  ⚠ %s\n", message)
		}
		registered, err := gatewaycli.IsTaskRegistered(ctx)
		if err != nil || !registered {
			return err
		}
		name, err := gatewaycli.GetTaskName(ctx)
		if err != nil {
			return err
		}
		_, err = gateway.ReconcileScheduledTask(ctx, name)
		return err
	}()
	if err != nil {
		updateLog.DebugContext(ctx, fmt.Sprintf("Could not refresh Windows gateway launchers after update: %s", launchErrorText(err)))
	}
}

// Overwrites the cached bootstrap install scripts of a branch from the fresh checkout, never failing the update
func refreshBootstrapCacheScripts(ctx context.Context, w io.Writer, branch string) error {
	err := func() error {
		cacheDir := filepath.Join(home.Dir(ctx), "bootstrap-cache")
		if info, err := os.Stat(cacheDir); err != nil || !info.IsDir() {
			return nil
		}
		if branch == "" {
			branch = "main"
		}
		safeRef := bootstrapUnsafeRef.ReplaceAllString(branch, "_")
		if bootstrapCommitRef.MatchString(safeRef) {
			return nil
		}
		var refreshed []string
		for _, kind := range [][2]string{{"ps1", "install.ps1"}, {"sh", "install.sh"}} {
			source := filepath.Join(fleetProjectRoot(), "scripts", kind[1])
			cached := filepath.Join(cacheDir, "install-"+safeRef+"."+kind[0])
			if !takeoverIsFile(source) || !takeoverIsFile(cached) {
				continue
			}
			data, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			if kind[0] == "ps1" && !strings.HasPrefix(string(data), "\xef\xbb\xbf") {
				data = append([]byte("\xef\xbb\xbf"), data...)
			}
			current, err := os.ReadFile(cached)
			if err != nil {
				return err
			}
			if string(current) == string(data) {
				continue
			}
			temporary := cached + ".tmp"
			if err := os.WriteFile(temporary, data, 0o666); err != nil {
				return err
			}
			if err := os.Rename(temporary, cached); err != nil {
				return err
			}
			refreshed = append(refreshed, filepath.Base(cached))
		}
		if len(refreshed) > 0 {
			sort.Strings(refreshed)
			fmt.Fprintln(w, "  ✓ Refreshed installer bootstrap-cache script(s): "+strings.Join(refreshed, ", "))
		}
		return nil
	}()
	return bestEffort(ctx, "Could not refresh bootstrap-cache scripts after update: %s", func() error { return err })
}

// The stable gateway a service process supervises, in the profile's home when known
func serviceGatewayReady(ctx context.Context, name, profile string, timeoutS *float64) ([]int, error) {
	service, err := winService(name)
	if err != nil {
		return nil, err
	}
	underService := func(pids []int) ([]int, error) {
		servicePid, err := service.PID()
		if err != nil {
			return nil, nil
		}
		var owned []int
		for _, pid := range pids {
			ancestors, err := winParents(pid)
			var typed *pm.PyError
			if errors.As(err, &typed) && !isUpdateOSError(err) {
				return nil, err
			}
			if err != nil || servicePid == 0 {
				continue
			}
			for _, ancestor := range ancestors {
				if ancestor == servicePid {
					owned = append(owned, pid)
					break
				}
			}
		}
		return owned, nil
	}
	profileHome := ""
	if profile != "" {
		if profileHome, err = profiles.GetProfileDir(profile); err != nil {
			return nil, err
		}
	}
	timeout := serviceReadyTimeoutS
	if timeoutS != nil {
		timeout = *timeoutS
	}
	return winWaitForGatewayReady(ctx, &timeout, profileHome, profileHome == "", underService)
}

// Whether the SCM reports a service running, false when it cannot be read
func serviceRunning(name string) bool {
	service, err := winService(name)
	if err != nil {
		return false
	}
	status, err := service.Status()
	return err == nil && status == "running"
}

// Restarts the SCM services on the token, each leaving it only once the gateway it supervises is ready
func resumeWindowsServices(ctx context.Context, w io.Writer, token map[string]any) error {
	services := tokenStrings(token["services"])
	if _, ok := token["expected_services"]; !ok {
		token["expected_services"] = stringsAsAny(services)
	}
	verified := tokenStrings(token["restarted_services"])
	listed := map[string]bool{}
	for _, name := range services {
		listed[name] = true
	}
	retry := map[string]any{}
	for name, state := range tokenMap(token["service_retry"]) {
		if listed[name] {
			retry[name] = state
		}
	}
	recovering := pyrepr.Truthy(token["recovery"])
	owners := tokenMap(token["service_profiles"])
	var restarted, failed []string
	for _, name := range services {
		backoff := tokenMap(retry[name])
		nextAt, _ := winFloat(backoff["next_at"])
		err := func() error {
			if recovering && winNow() < nextAt && serviceRunning(name) {
				failed = append(failed, name)
				fmt.Fprintf(w, "  ⚠ Windows gateway service %s: its gateway was not ready; next check after %ds\n", name, int(nextAt-winNow()))
				return nil
			}
			if err := winStartService(ctx, name, 30.0); err != nil {
				return err
			}
			var probe *float64
			if recovering {
				short := serviceReadyProbeS
				probe = &short
			}
			profile := ""
			if owner := owners[name]; pyrepr.Truthy(owner) {
				profile = pyrepr.Str(owner)
			}
			ready, err := winServiceGatewayReady(ctx, name, profile, probe)
			if err != nil {
				return err
			}
			if len(ready) == 0 {
				attempts, _ := pyIntValue(backoff["attempts"])
				attempts++
				retry[name] = map[string]any{"attempts": attempts,
					"next_at": winNow() + math.Min(serviceRetryBaseS*math.Pow(2, float64(min(attempts-1, 6))), serviceRetryMaxS)}
				return fmt.Errorf("Windows service %s is running but its gateway did not become ready", name)
			}
			delete(retry, name)
			restarted = append(restarted, name)
			known := false
			for _, done := range verified {
				known = known || done == name
			}
			if !known {
				verified = append(verified, name)
			}
			return nil
		}()
		if err != nil {
			updateLog.WarnContext(ctx, fmt.Sprintf("Could not restart Windows gateway service %s after update: %s", name, launchErrorText(err)))
			fmt.Fprintf(w, "  ⚠ Could not restart Windows gateway service: %s\n", name)
			failed = append(failed, name)
		}
	}
	token["restarted_services"], token["services"] = stringsAsAny(verified), stringsAsAny(failed)
	if len(retry) > 0 {
		token["service_retry"] = retry
	} else {
		delete(token, "service_retry")
	}
	if len(failed) > 0 {
		return errors.New("Could not restart Windows gateway service(s): " + strings.Join(failed, ", "))
	}
	if len(restarted) > 0 {
		fmt.Fprintln(w, "\n  ✓ Restarted Windows gateway service(s): "+strings.Join(restarted, ", "))
	}
	return nil
}

// Spawns the restart watcher of each paused profile gateway and unmapped argv, the launched ones
func relaunchPausedGateways(ctx context.Context, paused map[string]any, unmapped []any) (map[string]any, []any) {
	launched := map[string]any{}
	for _, profile := range sortedKeysOf(paused) {
		oldPid, ok := pyIntValue(paused[profile])
		if !ok {
			updateLog.DebugContext(ctx, fmt.Sprintf("Could not restart Windows gateway profile %s after update: %s", profile, "invalid pid"))
			continue
		}
		if tryCall(ctx, func() (bool, error) { return winLaunchDetachedRestart(ctx, profile, int(oldPid)) }, false,
			"Could not restart Windows gateway profile %s after update: %s", profile) {
			launched[profile] = int(oldPid)
		}
	}
	launchedUnmapped := []any{}
	for _, entry := range tokenEntries(unmapped) {
		argv, oldPid := tokenStrings(entry["argv"]), entryPid(entry)
		if len(argv) == 0 || oldPid == 0 {
			continue
		}
		var servedHome *string
		if served, ok := entry["home"].(string); ok {
			servedHome = &served
		}
		if tryCall(ctx, func() (bool, error) {
			return gateway.LaunchDetachedGatewayRestartByCmdline(ctx, oldPid, argv, servedHome)
		}, false,
			"Could not restart unmapped Windows gateway (pid %s) after update: %s", strconv.Itoa(oldPid)) {
			launchedUnmapped = append(launchedUnmapped, entry)
		}
	}
	return launched, launchedUnmapped
}

// Old pids a restart watcher still waits on, sorted, empty when every relaunch can have run
func pendingRelaunchPids(paused map[string]any, unmapped []any, pidExists func(int) bool) []int {
	set := map[int]bool{}
	for _, pid := range paused {
		if number, ok := pyIntValue(pid); ok {
			set[int(number)] = true
		}
	}
	for _, entry := range tokenEntries(unmapped) {
		if pyrepr.Truthy(entry["argv"]) && pyrepr.Truthy(entry["pid"]) {
			set[entryPid(entry)] = true
		}
	}
	pids := []int{}
	for pid := range set {
		if pid > 0 && pidExists(pid) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

// Liveness budget of the post relaunch poll, extended by the watcher's own wait while an old pid lives
func relaunchVerifyTimeout(paused map[string]any, unmapped []any, pidExists func(int) bool) float64 {
	if len(pendingRelaunchPids(paused, unmapped, pidExists)) == 0 {
		return relaunchVerifyTimeoutS
	}
	return gatewayRestartWatcherTimeoutS + relaunchVerifyTimeoutS
}

// Filter matching the gateway an unmapped entry's replay started, on its home and born after the one it replaces
func unmappedReadyFilter(ctx context.Context, entry map[string]any, taken map[int]bool) func([]int) ([]int, error) {
	recorded := tokenStrings(entry["argv"])
	var tail []string
	if len(recorded) > 1 {
		tail = recorded[1:]
	}
	old := entryPid(entry)
	servedHome, haveHome := entry["home"]
	oldCt := entry["ct"]
	sameRuntime := func(pid int) (bool, error) {
		if !haveHome {
			return true, nil
		}
		if !pyrepr.Truthy(servedHome) {
			return false, nil
		}
		liveHome, ct := winGatewayPidHome(ctx, pid), winProcessStartTime(pid)
		if liveHome == "" {
			return false, nil
		}
		same, err := gateway.SameHermesHome(liveHome, pyrepr.Str(servedHome))
		if err != nil || !same {
			return false, err
		}
		if oldCt == nil || ct == nil {
			return true, nil
		}
		newer, newOk := pyIntValue(ct)
		older, oldOk := pyIntValue(oldCt)
		return newOk && oldOk && newer > older, nil
	}
	matches := func(pid int) (bool, error) {
		if pid == old || taken[pid] {
			return false, nil
		}
		argv, err := winCmdline(pid)
		if err != nil {
			return false, nil
		}
		var liveTail []string
		if len(argv) > 1 {
			liveTail = argv[1:]
		}
		if strings.Join(liveTail, "\x00") != strings.Join(tail, "\x00") || len(liveTail) != len(tail) || !winLooksLikeGateway(pid) {
			return false, nil
		}
		return sameRuntime(pid)
	}
	return func(pids []int) ([]int, error) {
		owned, err := winOwnedGatewayPids(ctx, io.Discard, pids, nil, true)
		if err != nil {
			return nil, err
		}
		var out []int
		for _, pid := range owned {
			ok, err := matches(pid)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, pid)
			}
		}
		return out, nil
	}
}

// One runtime the ready poll waits for, its probe reading the fleet scan only when it needs it
type readyTarget struct {
	Key      string
	Probe    func(scan func() ([]int, error)) ([]int, error)
	ClaimAll bool
}

// Pids per target whose gateway stayed visible for a whole confirmation window, all targets sharing one loop
func pollUntilReady(ctx context.Context, targets []readyTarget, deadline float64, taken map[int]bool, respawnWindow *float64) map[string][]int {
	ready := map[string][]int{}
	since := map[string]float64{}
	failed, graced, firstSeen := map[string]bool{}, map[string]bool{}, map[string]bool{}
	window := 0.0
	if respawnWindow != nil {
		window = *respawnWindow
	}
	hardStop := deadline + 2*window + readyConfirmS + readyIntervalS
	for {
		now := winMonotonic()
		if now >= hardStop {
			return ready
		}
		var pending []readyTarget
		for _, target := range targets {
			_, done := ready[target.Key]
			_, sighted := since[target.Key]
			if !done && !failed[target.Key] && (now < deadline || sighted) {
				pending = append(pending, target)
			}
		}
		if len(pending) == 0 {
			var starved []string
			for _, target := range targets {
				if _, done := ready[target.Key]; !done && !failed[target.Key] && !firstSeen[target.Key] && !graced[target.Key] {
					starved = append(starved, target.Key)
				}
			}
			if respawnWindow != nil && len(starved) > 0 {
				for _, key := range starved {
					graced[key] = true
				}
				deadline = now + window
				continue
			}
			return ready
		}
		var fleet []int
		scanned := false
		scan := func() ([]int, error) {
			if !scanned {
				found, err := winFindGatewayPids(ctx)
				if err != nil {
					return nil, err
				}
				fleet, scanned = found, true
			}
			return fleet, nil
		}
		for pid := range taken {
			delete(taken, pid)
		}
		for _, pids := range ready {
			for _, pid := range pids {
				taken[pid] = true
			}
		}
		for _, target := range pending {
			pids, err := target.Probe(scan)
			if err != nil {
				updateLog.WarnContext(ctx, fmt.Sprintf("Could not verify Windows gateway %s after update: %s", target.Key, launchErrorText(err)))
				failed[target.Key] = true
				continue
			}
			if len(pids) == 0 {
				delete(since, target.Key)
				continue
			}
			vouched := pids
			if !target.ClaimAll {
				vouched = pids[:1]
			}
			for _, pid := range vouched {
				taken[pid] = true
			}
			if respawnWindow != nil && !firstSeen[target.Key] {
				firstSeen[target.Key] = true
				deadline = math.Max(deadline, now+window)
			}
			if _, sighted := since[target.Key]; !sighted {
				since[target.Key] = now
			}
			if now-since[target.Key] >= readyConfirmS {
				ready[target.Key] = vouched
			}
		}
		if len(ready)+len(failed) < len(targets) {
			winSleep(readyIntervalS)
		}
	}
}

// Retires each relaunched runtime on its own readiness and names the ones that could not be verified
func verifyRelaunchedGatewaysAlive(ctx context.Context, w io.Writer, token, paused map[string]any, unmapped []any) error {
	deadline := winMonotonic() + relaunchVerifyTimeout(paused, unmapped, winPidExists)
	taken := map[int]bool{}
	var targets []readyTarget
	names := sortedKeysOf(paused)
	for _, name := range names {
		profileHome, err := profiles.GetProfileDir(name)
		if err != nil {
			return abortOnError("Could not load Windows gateway liveness helpers", err)
		}
		oldPid, _ := pyIntValue(paused[name])
		targets = append(targets, readyTarget{Key: "profile:" + name, ClaimAll: true, Probe: func(func() ([]int, error)) ([]int, error) {
			return winLiveGatewayPids(ctx, profileHome, func(pids []int) ([]int, error) {
				var others []int
				for _, pid := range pids {
					if pid != int(oldPid) {
						others = append(others, pid)
					}
				}
				return others, nil
			})
		}})
	}
	entries := tokenEntries(unmapped)
	for i, entry := range entries {
		filter := unmappedReadyFilter(ctx, entry, taken)
		targets = append(targets, readyTarget{Key: "unmapped:" + strconv.Itoa(i), Probe: func(scan func() ([]int, error)) ([]int, error) {
			fleet, err := scan()
			if err != nil {
				return nil, err
			}
			return filter(fleet)
		}})
	}
	window := relaunchVerifyTimeoutS
	ready := pollUntilReady(ctx, targets, deadline, taken, &window)
	var readyProfiles []string
	readyNames := map[string]bool{}
	for _, name := range names {
		if _, ok := ready["profile:"+name]; ok {
			readyProfiles = append(readyProfiles, name)
			readyNames[name] = true
		}
	}
	readyEntries := map[int]bool{}
	for i := range entries {
		if _, ok := ready["unmapped:"+strconv.Itoa(i)]; ok {
			readyEntries[i] = true
		}
	}
	readyPids := map[int]bool{}
	for _, pids := range ready {
		for _, pid := range pids {
			readyPids[pid] = true
		}
	}
	token["relaunched_profiles"] = stringsAsAny(readyProfiles)
	keptProfiles := map[string]any{}
	for name, pid := range tokenMap(token["profiles"]) {
		if !readyNames[name] {
			keptProfiles[name] = pid
		}
	}
	token["profiles"] = keptProfiles
	keptUnmapped := []any{}
	for _, owed := range tokenEntries(token["unmapped"]) {
		retired := false
		for i, entry := range entries {
			retired = retired || (readyEntries[i] && pyrepr.Equal(owed, entry))
		}
		if !retired {
			keptUnmapped = append(keptUnmapped, owed)
		}
	}
	token["unmapped"] = keptUnmapped
	if len(readyPids) > 0 {
		pids := make([]int, 0, len(readyPids))
		for pid := range readyPids {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		winWriteAttestation(ctx, pids, "post-update relaunch")
	}
	if len(readyProfiles) > 0 {
		fmt.Fprintf(w, "\n  ✓ Restarting Windows gateway profile(s): %s\n", strings.Join(readyProfiles, ", "))
	}
	if len(readyEntries) > 0 {
		fmt.Fprintf(w, "  ✓ Restarting %d unmapped Windows gateway process(es)\n", len(readyEntries))
	}
	var unready []string
	for _, name := range names {
		if !readyNames[name] {
			unready = append(unready, name)
		}
	}
	for i, entry := range entries {
		if !readyEntries[i] {
			unready = append(unready, "pid "+pyrepr.Str(entry["pid"]))
		}
	}
	if len(unready) > 0 {
		fmt.Fprintln(w, "\n  ⚠ Windows gateway restart could not be verified for: "+strings.Join(unready, ", ")+"\n"+
			"    (The respawned gateway may have been killed by a parent Job Object during updater teardown, #48820.)\n"+
			"    Recover with: icebath gateway restart")
		return errors.New("Windows gateway relaunch after update was not verified alive: " + strings.Join(unready, ", "))
	}
	return nil
}

// Restarts the gateways a token paused for the update, taking the exit hook's place once it runs
func resumeWindowsGatewaysAfterUpdate(ctx context.Context, w io.Writer, token map[string]any) error {
	if len(token) == 0 || !pyrepr.Truthy(token["resume_needed"]) {
		return nil
	}
	winUnregisterExitHook(resumeWindowsGatewaysHook)
	if !winIsWindows() && token["platform"] != "posix" {
		token["resume_needed"] = false
		return nil
	}
	return resumeUnlessTorn(ctx, w, token)
}

// Resumes the paused set, or leaves it owed when the checkout is not whole
func resumeUnlessTorn(ctx context.Context, w io.Writer, token map[string]any) error {
	if pyrepr.Truthy(token["pause_id"]) {
		whole, why, err := updaterecord.TreeIsWhole(ctx, token, "")
		if err != nil {
			return err
		}
		if !whole {
			fmt.Fprintf(w, "  ⚠ Paused gateway(s) left stopped: %s.\n    The next `icebath` command (or `icebath update`) restarts them.\n", why)
			token["resume_needed"] = false
			token["resume_deferred"] = why
			return updaterecord.SyncPause(ctx, token)
		}
	}
	err := resumePausedSetOf(ctx, w, token)
	if syncErr := updaterecord.SyncPause(ctx, token); syncErr != nil {
		return syncErr
	}
	return err
}

// Brings every paused runtime back, each retired from the token only on its own verified start
func resumePausedSetOf(ctx context.Context, w io.Writer, token map[string]any) error {
	if token["platform"] == "posix" {
		return resumePausedSet(ctx, w, token)
	}
	winRefreshLaunchers(ctx, w)
	var failures []string
	attempt := func(step func() error) {
		if err := step(); err != nil {
			failures = append(failures, launchErrorText(err))
		}
	}
	attempt(func() error { return resumeWindowsServices(ctx, w, token) })
	paused := map[string]any{}
	for name, pid := range tokenMap(token["profiles"]) {
		paused[name] = pid
	}
	unmapped := entriesAsAny(tokenEntries(token["unmapped"]))
	replayable := false
	for _, entry := range tokenEntries(unmapped) {
		replayable = replayable || pyrepr.Truthy(entry["argv"])
	}
	switch {
	case len(paused) > 0 || replayable:
		launched, launchedUnmapped := relaunchPausedGateways(ctx, paused, unmapped)
		if len(launched) > 0 || len(launchedUnmapped) > 0 {
			attempt(func() error { return verifyRelaunchedGatewaysAlive(ctx, w, token, launched, launchedUnmapped) })
		} else {
			token["relaunched_profiles"] = []any{}
		}
		var notLaunched []string
		for _, name := range sortedKeysOf(paused) {
			if _, ok := launched[name]; !ok {
				notLaunched = append(notLaunched, name)
			}
		}
		for _, entry := range tokenEntries(unmapped) {
			started := false
			for _, done := range tokenEntries(launchedUnmapped) {
				started = started || pyrepr.Equal(done, entry)
			}
			if !started {
				notLaunched = append(notLaunched, "pid "+pyrepr.Str(entry["pid"]))
			}
		}
		if len(notLaunched) > 0 {
			failures = append(failures, "Could not restart every paused Windows gateway: "+strings.Join(notLaunched, ", "))
		}
	case pyrepr.Truthy(token["cold_start_if_installed"]):
		attempt(func() error {
			started, err := winColdStartGateway(ctx, w, token)
			if err != nil {
				return err
			}
			if !started {
				return errors.New("Windows gateway cold-start was not verified")
			}
			token["cold_start_if_installed"] = false
			return nil
		})
	}
	attempt(func() error { return coldStartAttestedProfiles(ctx, w, token) })
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	token["resume_needed"] = false
	return nil
}

// Appends the items a list does not hold yet, each once and in order
func extendUnique(target []string, items []string) []string {
	for _, item := range items {
		known := false
		for _, have := range target {
			known = known || have == item
		}
		if !known {
			target = append(target, item)
		}
	}
	return target
}

// Resumes the gateways paused for a Windows update and folds the token into the restart bookkeeping, never failing
func resumeWindowsGatewaysAndMergeOutcome(ctx context.Context, w io.Writer, outcome *gatewayRestartOutcome, token map[string]any, gatewayMode bool) {
	if err := winResumeAfterUpdate(ctx, w, token); err != nil {
		outcome.Incomplete = true
		outcome.PhaseErrors = append(outcome.PhaseErrors, launchErrorText(err))
		fmt.Fprintf(w, "  ⚠ Windows gateway service restart incomplete: %s\n", launchErrorText(err))
	}
	if token == nil {
		return
	}
	outcome.RelaunchedProfiles = extendUnique(outcome.RelaunchedProfiles, tokenStrings(token["relaunched_profiles"]))
	restarted := tokenStrings(token["restarted_services"])
	owners := tokenMap(token["service_profiles"])
	outcome.RestartedServices = extendUnique(outcome.RestartedServices, restarted)
	var supervised, stale []string
	for _, name := range restarted {
		if owner := owners[name]; pyrepr.Truthy(owner) {
			supervised = append(supervised, pyrepr.Str(owner))
		}
	}
	outcome.RelaunchedProfiles = extendUnique(outcome.RelaunchedProfiles, supervised)
	for _, name := range tokenStrings(token["services"]) {
		if owner := owners[name]; pyrepr.Truthy(owner) {
			name = pyrepr.Str(owner)
		}
		stale = append(stale, name)
	}
	outcome.FailedOrStaleUnits = extendUnique(outcome.FailedOrStaleUnits, stale)
	killed := make([]int, 0, len(outcome.KilledPIDs))
	for pid := range outcome.KilledPIDs {
		killed = append(killed, pid)
	}
	sort.Ints(killed)
	func() {
		defer func() { _ = recover() }()
		winRecordRestart(ctx, updaterecord.GatewayRestart{RestartedServices: outcome.RestartedServices,
			RelaunchedProfiles: outcome.RelaunchedProfiles, ExternallySupervisedProfiles: outcome.ExternallySupervisedProfiles,
			KilledPIDs: killed, FailedUnits: outcome.FailedOrStaleUnits, Incomplete: outcome.Incomplete || len(outcome.FailedOrStaleUnits) > 0,
			PhaseError: strings.Join(outcome.PhaseErrors, "; ")})
	}()
}
