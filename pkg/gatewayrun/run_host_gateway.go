package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Result of fn, the zero value with a debug record of the error when it fails or panics
func bestEffort[T any](fn func() (T, error), debugMsg string) (out T, ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			if debugMsg != "" {
				runLog().Debug(fmt.Sprintf(debugMsg, rec))
			}
			var zero T
			out, ok = zero, false
		}
	}()
	value, err := fn()
	if err != nil {
		if debugMsg != "" {
			runLog().Debug(fmt.Sprintf(debugMsg, pyrepr.ExceptionMessage(err)))
		}
		return out, false
	}
	return value, true
}

// Removes this home's takeover marker, swallowing any failure
func clearTakeoverMarkerQuiet() {
	defer func() { _ = recover() }()
	gateway.ClearTakeoverMarker("")
}

// Polls pid until it exits, true once gone, false after attempts polls spaced by delay
func waitForPidExit(ctx context.Context, pid, attempts int, delay time.Duration) bool {
	for range attempts {
		if !gateway.PidExists(pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
	}
	return false
}

// Whether the live host gateway is existingPid and serves this profile, so replacing it is ours to do
func replaceTargetIsHostServingUs(ctx context.Context, existingPid int) (serves bool) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("host served-set ownership probe failed for PID %d", existingPid), "error", rec)
			serves = false
		}
	}()
	owner := gateway.HostGatewayProbe(0)
	if owner == nil || owner.PID != existingPid || !owner.Serves(gateway.HostProfileNameForHome(home.Dir(ctx))) {
		return false
	}
	names := strings.Join(owner.Profiles, ", ")
	if names == "" {
		names = "unknown"
	}
	runLog().Warn(fmt.Sprintf("--replace target PID %d is the host gateway serving %d profile(s) (%s); "+
		"replacing it restarts the host process for all of them.", existingPid, len(owner.Profiles), names))
	return true
}

// True when replace must refuse to signal existingPid because this profile cannot be proven its owner
func replaceTargetBelongsToOtherProfile(ctx context.Context, existingPid int) (refused bool) {
	if replaceTargetIsHostServingUs(ctx, existingPid) {
		return false
	}
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Warn(fmt.Sprintf("cross-profile --replace ownership probe failed for PID %d; refusing to signal", existingPid), "error", rec)
			refused = true
		}
	}()
	ourHome := gateway.ProcessHome()
	refuse := func(level slog.Level, msg string) bool {
		runLog().Log(ctx, level, "Refusing --replace: "+msg)
		return true
	}
	record := gateway.ReadPidRecord(gateway.PidPath())
	if record == nil || !gateway.RecordLooksLikeGateway(record) {
		return refuse(slog.LevelWarn, fmt.Sprintf("no valid gateway pid record to prove ownership of PID %d.", existingPid))
	}
	recordPid, ok := gateway.PidFromRecord(record)
	if !ok || recordPid != existingPid {
		named := "None"
		if ok {
			named = fmt.Sprint(recordPid)
		}
		return refuse(slog.LevelWarn, fmt.Sprintf("pid record names %s, not target %d.", named, existingPid))
	}
	recordedStart, _ := record.Get("start_time")
	if !gateway.PyIsInt(recordedStart) {
		return true
	}
	if !gateway.PyValuesEqual(gateway.GetProcessStartTime(existingPid), recordedStart) {
		return refuse(slog.LevelWarn, fmt.Sprintf("pid record start-time does not match the live process %d (stale/PID-reuse record).", existingPid))
	}
	recordedHome, _ := record.Get("hermes_home")
	homeText, isText := recordedHome.(string)
	if !isText || strings.TrimSpace(homeText) == "" {
		return refuse(slog.LevelWarn, fmt.Sprintf("pid record predates hermes_home stampings; ownership of PID %d unprovable.", existingPid))
	}
	same, err := gateway.SameHermesHome(homeText, ourHome)
	if err != nil {
		panic(err)
	}
	if !same {
		return refuse(slog.LevelError, fmt.Sprintf("pid record belongs to a different ICEBATH_HOME (%s, ours %s). Remove the stale PID "+
			"record or stop the owning profile explicitly.", homeText, ourHome))
	}
	liveCmdline, _ := bestEffort(func() (string, error) {
		line, _ := gateway.ReadProcessCmdline(existingPid)
		return line, nil
	}, "")
	if liveCmdline != "" && looksLikeProfileConflictFromCmdline(liveCmdline, ourHome) {
		return refuse(slog.LevelError, fmt.Sprintf("target PID %d command line explicitly advertises a different profile than "+
			"ICEBATH_HOME %s.", existingPid, ourHome))
	}
	return false
}

// Whether a target argv token exactly names a profile or home that contradicts ourHome
func looksLikeProfileConflictFromCmdline(command, ourHome string) bool {
	profileName := ""
	if pypath.PathName(pypath.PathParent(ourHome)) == "profiles" {
		profileName = pypath.PathName(ourHome)
	}
	tokens, err := pyshlex.SplitPOSIX(command, false)
	if err != nil {
		tokens = strings.Fields(command)
	}
	flagValue := func(flag string) (string, bool) {
		var values []string
		for i := 0; i < len(tokens); {
			tok := tokens[i]
			if tok == flag && i+1 < len(tokens) {
				values = append(values, tokens[i+1])
				i += 2
				continue
			}
			if strings.HasPrefix(tok, flag+"=") {
				values = append(values, tok[len(flag)+1:])
			}
			i++
		}
		if len(values) == 0 {
			return "", false
		}
		return values[len(values)-1], true
	}
	envHomeValue := func() (string, bool) {
		const prefix = "ICEBATH_HOME="
		for i := len(tokens) - 1; i >= 0; i-- {
			if strings.HasPrefix(tokens[i], prefix) {
				return tokens[i][len(prefix):], true
			}
		}
		return "", false
	}
	norm := func(path string) string { return pypath.Normcase(pypath.Normpath(path)) }
	for _, flag := range []string{"--profile", "-p"} {
		value, ok := flagValue(flag)
		if !ok {
			continue
		}
		if profileName == "" || profileName == "default" || value != profileName {
			return true
		}
	}
	homeValue, ok := flagValue("--icebath-home")
	if !ok || homeValue == "" {
		homeValue, ok = envHomeValue()
	}
	return ok && norm(homeValue) != norm(ourHome)
}

// Command that folds every profile onto one multiplexing gateway
func migrateCommand() string { return gateway.HostMigrateCommand }

// True when the host owner answers that it does not multiplex, false on any failure
func ownerIsStandalone(ctx context.Context) (standalone bool) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("standalone-owner probe failed; keeping the second-gateway refusal", "error", rec)
			standalone = false
		}
	}()
	owner := gateway.HostGatewayProbe(0)
	if owner == nil || owner.PID == os.Getpid() {
		return false
	}
	answered := gateway.RequestServeProfile(gateway.HostProfileNameForHome(home.Dir(ctx)), 8*time.Second, owner)
	return answered != nil && answered.Standalone
}

// Prints the named refusal and exits with the service restart code so a supervisor retries
func refuseSecondHostGateway(owner *gateway.HostRecord) error {
	who := "owner unknown (its record is gone)"
	if owner != nil {
		who = gateway.DescribeHostRecord(owner)
	}
	message := fmt.Sprintf("❌ Another gateway already owns this host: %s\n"+
		"   Exactly one gateway per host serves every profile, so this process will not start a\n"+
		"   second one (it would double-bind this profile's platforms).\n"+
		"   Fold every profile onto the owner:  %s\n"+
		"   Or stop the other gateway first, then start this one.\n"+
		"   Or start one anyway (skips the host-lock check):  icebath gateway run --force\n"+
		"   (--replace does not skip this check; it only replaces an owner that serves this profile.)", who, migrateCommand())
	runLog().Error(fmt.Sprintf("Refusing to start a second gateway on this host: %s", who))
	fmt.Println(message)
	return &gateway.SystemExitError{Code: gateway.GatewayServiceRestartExitCode}
}

// Takes the host gateway lock and publishes the record, or refuses to be the second gateway
func claimHostGatewayRole(ctx context.Context, force bool) error {
	ourHome := home.Dir(ctx)
	if !force {
		decision, err := gateway.StandaloneAttachDecision(ctx, ourHome, nil)
		if err != nil {
			return err
		}
		if decision != nil && decision.Outcome == gateway.HostAttachRefuse {
			fmt.Println(decision.Message)
			return &gateway.SystemExitError{Code: gateway.GatewayServiceRestartExitCode}
		}
	}
	claimed := func() (done bool) {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("host gateway rendezvous failed", "error", rec)
				done = true
			}
		}()
		outcome, openErr := gateway.ClaimHostLock(ctx, gateway.RoleGateway)
		switch outcome {
		case gateway.HostLockAcquired:
			gateway.PublishHostRecord(ctx, gateway.RoleGateway, gateway.HostPublishOptions{Profiles: []string{}, Home: ourHome})
			gateway.HostCleanupOnExit(gateway.RoleGateway)
			return true
		case gateway.HostLockCouldNotOpen:
			runLog().Warn(fmt.Sprintf("Host gateway lock could not be opened (%s); this gateway is not discoverable. "+
				"No second gateway is implied — the lock directory itself is unusable.", pyrepr.ExceptionMessage(openErr)))
			return true
		}
		return false
	}()
	if claimed {
		return nil
	}
	var owner *gateway.HostRecord
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("host gateway record unreadable", "error", rec)
			}
		}()
		owner = gateway.ReadHostRecord(gateway.RoleGateway, true)
	}()
	if force {
		who := "another process"
		if owner != nil {
			who = gateway.DescribeHostRecord(owner)
		}
		runLog().Warn(fmt.Sprintf("--force: starting a second gateway although %s owns this host.", who))
		return nil
	}
	if owner != nil {
		other, err := gateway.LaunchedByOtherTenant(owner.Home, ourHome)
		if err != nil {
			return err
		}
		if other {
			runLog().Warn(fmt.Sprintf("Another Icebath home's gateway owns this host (%s); starting this home's gateway beside it.",
				gateway.DescribeHostRecord(owner)))
			return nil
		}
	}
	standalone, err := profiles.ProfileIsStandalone(ctx, ourHome)
	if err != nil {
		return err
	}
	if standalone {
		liveOwner := gateway.HostGatewayProbe(gateway.AttachChannelWait)
		decision, err := gateway.StandaloneAttachDecision(ctx, ourHome, liveOwner)
		if err != nil {
			return err
		}
		if decision != nil {
			if decision.Outcome == gateway.HostAttachStart {
				return nil
			}
			fmt.Println(decision.Message)
			return &gateway.SystemExitError{Code: gateway.GatewayServiceRestartExitCode}
		}
	}
	if ownerIsStandalone(ctx) {
		who := "owner unknown"
		if owner != nil {
			who = gateway.DescribeHostRecord(owner)
		}
		runLog().Warn(fmt.Sprintf("Another profile's standalone gateway owns this host (%s); starting beside it rather "+
			"than retrying a race no second profile can win. Fold every profile onto one gateway "+
			"with: %s", who, migrateCommand()))
		return nil
	}
	return refuseSecondHostGateway(owner)
}

// One warning per standalone profile the multiplexer leaves out of its served set at boot
func logStandaloneProfilesAtBoot(ctx context.Context, r *GatewayRunner) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Warn("standalone-profile boot notice failed", "error", rec)
		}
	}()
	if !r.multiplexing() {
		return
	}
	served := map[string]bool{}
	for _, name := range r.servedProfileNames() {
		served[name] = true
	}
	all, err := profiles.ProfilesToServe(ctx, true, true, true)
	if err != nil {
		panic(err)
	}
	for _, p := range all {
		if p.Name == "default" || served[p.Name] {
			continue
		}
		standalone, err := profiles.ProfileIsStandalone(ctx, p.Home)
		if err != nil {
			panic(err)
		}
		if standalone {
			runLog().Warn(fmt.Sprintf("profile '%s' is standalone (gateway.standalone: true); not served by "+
				"this gateway. %s", p.Name, gateway.StandaloneDeprecationNotice))
		}
	}
}

// Republishes the host record with the settled served set once the attach channel answers
func refreshHostGatewayRecord(ctx context.Context, r *GatewayRunner) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("host gateway record refresh failed", "error", rec)
		}
	}()
	if !gateway.OwnsHostLock(gateway.RoleGateway) {
		return
	}
	h := home.Dir(ctx)
	var served []string
	if r.multiplexing() {
		served = r.servedProfileNames()
	} else {
		served = []string{gateway.HostProfileNameForHome(h)}
	}
	gateway.PublishHostRecord(ctx, gateway.RoleGateway, gateway.HostPublishOptions{Profiles: served, Home: h})
}

// Attaches to, refuses beside or replaces the one host gateway, nil to start normally
func hostAttachOrNone(ctx context.Context, replace, force bool) (*bool, error) {
	if force {
		runLog().Warn("--force: starting a gateway without asking the host owner.")
		return nil, nil
	}
	decision, err := gateway.DecideHostAttach(ctx, home.Dir(ctx), replace)
	if err != nil {
		return nil, err
	}
	describe := func() string {
		if decision.Owner != nil {
			return decision.Owner.Describe()
		}
		return "unknown"
	}
	switch {
	case decision.Outcome == gateway.HostAttachAttach:
		runLog().Info(fmt.Sprintf("Attaching to the host gateway instead of starting a second one: %s", describe()))
		fmt.Println(decision.Message)
		return boolRef(true), nil
	case decision.Outcome == gateway.HostAttachRefuse:
		runLog().Error(fmt.Sprintf("Refusing to start a second gateway on this host: %s", describe()))
		fmt.Println(decision.Message)
		return boolRef(false), nil
	case decision.Outcome == gateway.HostAttachReplaceHost && decision.Owner != nil:
		replaced, err := startGatewayReplaceExistingInstance(ctx, decision.Owner.PID, true)
		if err != nil {
			return nil, err
		}
		if !replaced {
			return boolRef(false), nil
		}
	}
	return nil, nil
}

// Pointer to a copy of b
func boolRef(b bool) *bool { return &b }
