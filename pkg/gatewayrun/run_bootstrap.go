package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	bootstrapAtExitMu    sync.Mutex
	bootstrapAtExitFuncs []func()
)

// Queues fn to run at process exit after every later registration, the atexit.register of hermes
func registerBootstrapAtExit(fn func()) {
	bootstrapAtExitMu.Lock()
	bootstrapAtExitFuncs = append(bootstrapAtExitFuncs, fn)
	bootstrapAtExitMu.Unlock()
}

// Runs and clears the exit functions the gateway bootstrap registered, newest first, a panic not stopping the rest
func BootstrapAtExit() {
	bootstrapAtExitMu.Lock()
	funcs := bootstrapAtExitFuncs
	bootstrapAtExitFuncs = nil
	bootstrapAtExitMu.Unlock()
	for i := len(funcs) - 1; i >= 0; i-- {
		func() {
			defer func() { _ = recover() }()
			funcs[i]()
		}()
	}
}

// Runs the state.db scan an unclean exit report deferred on a detached goroutine under the caller's scope
func startDeferredStateDBIntegrityCheck(ctx context.Context, evidence *pyjson.Object) {
	memory.SpawnContextThread(ctx, "state-db-integrity-check", func(ctx context.Context) {
		gateway.RunDeferredIntegrityCheck(ctx, evidence, "", gateway.StateDBIntegrityDeadlineS)
	}).Start()
}

// Replaces a live gateway of this home with replace, or refuses, false when startup must abort
func startGatewayReplaceExistingInstance(ctx context.Context, existingPid int, replace bool) (bool, error) {
	if !replace {
		runLog().Error(fmt.Sprintf("Another gateway instance is already running (PID %d, ICEBATH_HOME=%s) and did not "+
			"publish a host record this process could attach to.", existingPid, home.Dir(ctx)))
		fmt.Printf("\n❌ A gateway already owns this host (PID %d).\n"+
			"   One gateway per host serves every profile, so there is nothing to start here.\n"+
			"   Attach is impossible: PID %d published no usable host record\n"+
			"   (an older build, or an unwritable lock directory).\n"+
			"   Take the host over:  icebath gateway run --replace\n"+
			"   Or stop it first:    icebath gateway stop\n\n", existingPid, existingPid)
		return false, nil
	}
	if replaceTargetBelongsToOtherProfile(ctx, existingPid) {
		runLog().Error(fmt.Sprintf("Refusing --replace: PID %d cannot be proven to belong "+
			"to this profile's gateway (ICEBATH_HOME %s). Remove the "+
			"stale PID record or stop the owning profile explicitly.", existingPid, gateway.ProcessHome()))
		wait := 5 * time.Second
		if _, err := gateway.WriteRuntimeStatus(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{
			"gateway_state": "startup_failed", "exit_reason": "gateway_replace_ownership_unproven",
		}}, false, &wait); err != nil {
			runLog().Debug("Could not stamp runtime status after the ownership refusal", "error", err)
		}
		return false, nil
	}
	existingStartTime := gateway.GetProcessStartTime(existingPid)
	runLog().Info(fmt.Sprintf("Replacing existing gateway instance (PID %d) with --replace.", existingPid))
	gateway.WriteTakeoverMarker(ctx, existingPid, "", nil, false)
	oldGatewayChildren, _ := bestEffort(func() ([]gateway.GatewayChild, error) {
		return gateway.SnapshotGatewayChildren(existingPid), nil
	}, "")
	if err := gateway.TerminatePid(existingPid, false, nil); err != nil && !errors.Is(err, os.ErrProcessDone) {
		runLog().Error(fmt.Sprintf("Permission denied killing PID %d. Cannot replace.", existingPid))
		clearTakeoverMarkerQuiet()
		return false, nil
	}
	if !waitForPidExit(ctx, existingPid, 20, 500*time.Millisecond) {
		runLog().Warn(fmt.Sprintf("Old gateway (PID %d) did not exit after SIGTERM, sending SIGKILL.", existingPid))
		oldGatewayExited := false
		if err := gateway.TerminatePid(existingPid, true, existingStartTime); errors.Is(err, os.ErrProcessDone) {
			oldGatewayExited = true
		}
		if !oldGatewayExited && !waitForPidExit(ctx, existingPid, 20, 250*time.Millisecond) {
			runLog().Error(fmt.Sprintf("Old gateway (PID %d) still appears alive after SIGKILL; "+
				"aborting replacement to avoid a duplicate gateway.", existingPid))
			clearTakeoverMarkerQuiet()
			return false, nil
		}
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("Child reap for replaced gateway PID %d failed", existingPid), "error", rec)
			}
		}()
		gateway.ReapGatewayChildren(oldGatewayChildren, existingPid, 5*time.Second)
	}()
	gateway.RemovePidFile()
	_ = os.Remove(filepath.Join(home.Dir(ctx), "gateway.pid"))
	clearTakeoverMarkerQuiet()
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("Stale scoped lock release for replaced gateway PID %d failed", existingPid), "error", rec)
			}
		}()
		pid := existingPid
		if released := gateway.ReleaseAllScopedLocks(&pid, existingStartTime); released > 0 {
			runLog().Info(fmt.Sprintf("Released %d stale scoped lock(s) from old gateway.", released))
		}
	}()
	return true, nil
}

// Claims the runtime lock and the pid file, false when another gateway won the race
func startGatewayClaimPidFile(ctx context.Context, force bool) (bool, error) {
	currentPid, running, err := gateway.GetRunningPid("", true)
	if err != nil {
		return false, err
	}
	if running && currentPid != os.Getpid() {
		runLog().Error(fmt.Sprintf("Another gateway instance (PID %d) started during our startup. "+
			"Exiting to avoid double-running.", currentPid))
		return false, nil
	}
	if !gateway.AcquireGatewayRuntimeLock(ctx) {
		runLog().Error("Gateway runtime lock is already held by another instance. Exiting.")
		return false, nil
	}
	if err := gateway.WritePidFile(ctx); err != nil {
		if errors.Is(err, fs.ErrExist) {
			gateway.ReleaseGatewayRuntimeLock()
			runLog().Error("PID file race lost to another gateway instance. Exiting.")
			return false, nil
		}
		return false, err
	}
	registerBootstrapAtExit(gateway.RemovePidFile)
	registerBootstrapAtExit(gateway.ReleaseGatewayRuntimeLock)
	gateway.PublishPendingMultiplexDecision(ctx)
	if err := claimHostGatewayRole(ctx, force); err != nil {
		return false, err
	}
	return true, nil
}

// Whether the launching home may run the multiplexer, downgrading an implicit default on a named profile
func launchHomeMayMultiplex(ctx context.Context, cfg *gateway.GatewayConfig) (bool, error) {
	launch := home.Dir(ctx)
	name := home.ProfileNameFor(launch)
	if name == "" || name == "default" {
		return true, nil
	}
	if cfg != nil {
		explicit, err := gateway.ExplicitMultiplexFlag(ctx, launch)
		if err != nil {
			return false, err
		}
		if explicit == nil || !*explicit {
			decision := gateway.MultiplexDecision{Enabled: false, Source: "guard", Reason: fmt.Sprintf(
				"profile %s launched the gateway, and only the default profile runs the multiplexer "+
					"(start it with `icebath gateway run` from the default profile to serve every profile)", pyrepr.Repr(name))}
			cfg.MultiplexProfiles = boolRef(false)
			gateway.LogMultiplexDecision(ctx, decision)
			return true, nil
		}
	}
	runLog().Error(fmt.Sprintf("gateway.multiplex_profiles is set on profile %s, but only the default profile can run the "+
		"multiplexer. Unset the flag in this profile's config.yaml, or start the gateway from the "+
		"default profile (`icebath gateway run`), which serves this profile too.", pyrepr.Repr(name)))
	writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{
		"gateway_state": "startup_failed", "exit_reason": "multiplex_requires_default_profile"}})
	return false, &gateway.SystemExitError{Code: gateway.GatewayFatalConfigExitCode}
}

// Resolves an empty or directory free TERMINAL_CWD to the messaging default the gateway runs tools in
func startGatewayResolveTerminalCwd() error {
	configured := os.Getenv("TERMINAL_CWD")
	if configured != "" && !tools.CwdNamesNoDirectory(configured) {
		return nil
	}
	homeFallback, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	mount := strings.ToLower(envOr("TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false"))
	resolved, ok := tools.ResolvePlaceholderTerminalCwd(configured, os.Getenv("TERMINAL_ENV"), os.Getenv("MESSAGING_CWD"),
		mount == "true" || mount == "1" || mount == "yes", homeFallback)
	if !ok {
		return os.Unsetenv("TERMINAL_CWD")
	}
	return os.Setenv("TERMINAL_CWD", resolved)
}

// Environment value of name, fallback when unset
func envOr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

// Wait a booting gateway gives a browse reconcile holding a profile lock, its budget plus margin
func browseReconcileReserveWait() time.Duration {
	return time.Duration((state.BrowseReconcileBudgetS + 2.0) * float64(time.Second))
}

// Reserves every served home at once, waiting out transient holders for up to the browse window
func reserveWaitingOutBrowseReconcile(ctx context.Context, ownership *gateway.ProfileOwnership, homes []string, wait *time.Duration) error {
	window := browseReconcileReserveWait()
	if wait != nil {
		window = *wait
	}
	return gateway.ReserveWaitingOutTransientHolders(ctx, ownership, homes, window, nil)
}

// Reserves the launch home and every served profile home, false when startup must abort
func startGatewayReserveProfiles(ctx context.Context, profileHomes []profiles.ServedProfile) (bool, error) {
	homes := []string{home.Dir(ctx)}
	for _, p := range profileHomes {
		homes = append(homes, p.Home)
	}
	err := reserveWaitingOutBrowseReconcile(ctx, gateway.ProcessOwnership, homes, nil)
	var conflict *gateway.OwnershipConflict
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &conflict):
		runLog().Error(fmt.Sprintf("Cannot reserve gateway profiles: %s", conflict.Error()))
		served, probeErr := gateway.CurrentProfileServedByRunningMultiplexer(ctx)
		if probeErr != nil {
			return false, probeErr
		}
		if served {
			runLog().Error("The default gateway is running as a profile multiplexer and already serves this profile. " +
				"Manage it with `icebath gateway restart` from the default profile instead of starting a " +
				"second gateway for the profile.")
			writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{
				"gateway_state": "startup_failed", "exit_reason": conflict.Error()}})
			return false, &gateway.SystemExitError{Code: gateway.GatewayFatalConfigExitCode}
		}
		return false, nil
	case pypath.IsOSError(err):
		runLog().Error(fmt.Sprintf("Cannot reserve gateway profiles: %s", pyrepr.ExceptionMessage(err)))
		return false, nil
	}
	return false, err
}

// Starts the host toolchain probe in the background unless agent.environment_probe is off
func startGatewayEarlyEnvironmentProbe(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("early environment probe did not start", "error", rec)
		}
	}()
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		runLog().Debug("early environment probe did not start", "error", err)
		return
	}
	if agentCfg, ok := cfg["agent"].(map[string]any); ok {
		if probe, set := agentCfg["environment_probe"]; set && !pyrepr.Truthy(probe) {
			return
		}
	}
	tools.WarmEnvironmentProbeAsync()
}
