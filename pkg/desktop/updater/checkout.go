package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

const (
	handoffClaimTimeout     = 20 * time.Second
	withdrawAttempts        = 3
	withdrawRetry           = 250 * time.Millisecond
	sourceCheckTimeout      = 360 * time.Second
	stateDBDefaultTimeout   = 5 * time.Minute
	stateDBMaxTimeout       = 30 * time.Minute
	stateDBCopyRate         = 5 * 1024 * 1024
	stateDBTimeoutSlack     = time.Minute
	desktopUpdateConfigFile = "updates.json"
)

var (
	stillRunningVerdicts    = map[string]bool{"held": true, "busy": true, "live": true}
	withdrawSettledVerdicts = map[string]bool{"withdrawn": true, "absent": true, "foreign": true}
	canaryTagRE             = regexp.MustCompile(`\+canary\.20\d{6}T\d{6}Z$`)
	internalArgPrefixes     = []string{"--type=", "--user-data-dir=", "--enable-features=", "--disable-features=", "--field-trial-handle=", "--enable-logging", "--log-file=", "--disable-gpu-sandbox", "--lang=", "--inspect", "--remote-debugging-port="}
	disabledBackupModes     = map[string]bool{"off": true, "false": true, "none": true, "disabled": true}
)

// How one script hand-off is correlated, protocol 2 scripts adopting the bridge that carries runID
type handoffPlan struct {
	protocol  int
	runID     string
	startedAt int64
}

// The checkout strategy, windows-handoff on Windows and posix-handoff elsewhere
type checkoutStrategy struct {
	s *Service
}

func (s *Service) checkoutStrategy() *checkoutStrategy { return &checkoutStrategy{s: s} }

func (c *checkoutStrategy) Mechanism() string {
	if c.s.isWindows() {
		return "windows-handoff"
	}
	return "posix-handoff"
}

func (c *checkoutStrategy) Check(force bool) (UpdaterStatus, error) { return c.check(force, "") }

// Source update status of the checkout, a probe less checkout moved to the default branch
func (c *checkoutStrategy) check(force bool, setChannel string) (UpdaterStatus, error) {
	root := c.s.resolveUpdateRoot()
	status, err := c.s.readSourceUpdate(root, force, setChannel)
	if err != nil {
		return UpdaterStatus{}, err
	}
	if status == nil {
		available := true
		status = &UpdaterStatus{Supported: true, UpdateAvailable: &available, Branch: defaultUpdateBranch, HermesRoot: root}
	}
	status.Mechanism = c.Mechanism()
	if status.CurrentSHA == "" {
		if stamped := readStampedCommit(root); stamped != "" {
			status.CurrentSHA = stamped
		}
	}
	return *status, nil
}

func (c *checkoutStrategy) Apply() (ApplyResult, error) {
	result, err := c.applyBody()
	if err != nil {
		return result, err
	}
	result.Mechanism = c.Mechanism()
	return result, nil
}

func (c *checkoutStrategy) markerHelper(op, runID string) MarkerHelperVerdict {
	return runMarkerHelper(op, c.s.resolveUpdateRoot(), c.s.opts.IcebathHome, os.Getpid(), runID, c.s.isWindows())
}

// Withdraws the bridge, asked again only while the sidecar lock is busy
func (c *checkoutStrategy) withdrawBridge(runID string) MarkerHelperVerdict {
	verdict := MarkerHelperVerdict{Kind: "error"}
	for attempt := 0; attempt < withdrawAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(withdrawRetry)
		}
		if verdict = c.markerHelper("withdraw", runID); verdict.Kind != "busy" {
			break
		}
	}
	return verdict
}

func (c *checkoutStrategy) refuse(message, code string) ApplyResult {
	c.s.log("[updates] refusing hand-off: " + message)
	c.s.emitUpdateProgress("error", message, nil)
	return ApplyResult{Error: code, Message: message}
}

// Plans the hand-off, a protocol 2 script getting a bridge claimed in this desktop's name under a fresh run id
func (c *checkoutStrategy) planHandoff(scriptPath string, updateStartedAt int64) (*handoffPlan, *ApplyResult) {
	protocol := readHandoffProtocol(scriptPath)
	if protocol == 0 {
		protocol = 1
	}
	if protocol < 2 {
		c.s.log("[updates] hand-off script predates protocol 2; legacy acknowledgment (live owner + started_at)")
		return &handoffPlan{protocol: protocol, startedAt: updateStartedAt}, nil
	}
	runID, err := makeHandoffRunID(os.Getpid(), time.Now().UnixMilli())
	if err != nil {
		refusal := c.refuse("The updater could not take the update lock, so nothing was changed. Icebath keeps running on the previous version — try the update again.", "updater-spawn-failed")
		return nil, &refusal
	}
	for attempt := 0; attempt < 2; attempt++ {
		bridge := claimBridgeMarker(c.s.opts.IcebathHome, updateStartedAt, runID)
		if bridge.OK {
			return &handoffPlan{protocol: protocol, runID: runID, startedAt: updateStartedAt}, nil
		}
		if bridge.Conflict != nil {
			refusal := c.refuse(bridge.Conflict.Message, "update-already-running")
			return nil, &refusal
		}
		if attempt > 0 {
			break
		}
		if bridge.Existing != nil {
			op, run := "reclaim", ""
			if bridge.Existing.State == "ours" {
				op = "withdraw"
				if bridge.Existing.Marker != nil && bridge.Existing.Marker.Run != nil {
					run = *bridge.Existing.Marker.Run
				}
			}
			verdict := c.markerHelper(op, run)
			c.s.log(fmt.Sprintf("[updates] %s update marker: script helper %s => %s", bridge.Existing.State, op, verdict.Kind))
			if stillRunningVerdicts[verdict.Kind] {
				refusal := c.refuse("Another update still holds this installation. Wait for it to finish, then try again.", "update-already-running")
				return nil, &refusal
			}
		} else {
			c.s.log(fmt.Sprintf("[updates] could not write the bridge marker (%s); retrying once", bridge.Error))
		}
	}
	refusal := c.refuse("The updater could not take the update lock, so nothing was changed. Icebath keeps running on the previous version — try the update again.", "updater-spawn-failed")
	return nil, &refusal
}

// Waits for a live correlated script to hold the marker, the failure text when the hand-off did not start
func (c *checkoutStrategy) confirmScriptHandoff(child *UpdaterChild, plan *handoffPlan, started time.Time) string {
	failure := ""
	if outcome := observeUpdaterHandoff(child, updateHandoffDwell); !outcome.OK {
		failure = describeUpdaterHandoffFailure(outcome)
	}
	if failure == "" {
		remaining := handoffClaimTimeout - time.Since(started)
		if remaining < 0 {
			remaining = 0
		}
		var startedAt *float64
		if plan.runID == "" {
			value := float64(plan.startedAt)
			startedAt = &value
		}
		if pid := waitForHandoffClaim(c.s.opts.IcebathHome, os.Getpid(), plan.runID, startedAt, remaining); pid != 0 {
			c.s.log(fmt.Sprintf("[updates] hand-off script took the update marker (pid %d)", pid))
			return ""
		}
		failure = "The updater did not start, so nothing was changed. Icebath keeps running on the previous version — try the update again.\n\n" +
			fmt.Sprintf("Details: the hand-off script never took the update lock within %d s.", int(math.Round(handoffClaimTimeout.Seconds())))
	}
	c.s.log("[updates] hand-off not viable, aborting quit: " + failure)
	if plan.runID != "" {
		verdict := c.withdrawBridge(plan.runID)
		if verdict.Kind == "taken" {
			c.s.log(fmt.Sprintf("[updates] hand-off script took the update marker at the deadline (pid %d)", verdict.PID))
			return ""
		}
		if !withdrawSettledVerdicts[verdict.Kind] {
			c.s.log(fmt.Sprintf("[updates] bridge withdraw stayed %s: a late hand-off script can still adopt run %s, so this Desktop quits instead of restarting its backend beside it", verdict.Kind, plan.runID))
			return ""
		}
		c.s.log("[updates] bridge withdraw: " + verdict.Kind)
	}
	killHandoffTree(child, c.s.isWindows(), os.Getpid())
	return failure
}

// Spawns the staged setup app detached, pre writing its marker when it is new enough to adopt it
func (c *checkoutStrategy) spawnStagedUpdater(updater string, args []string, updateRoot string) *UpdaterChild {
	env := c.s.sourceUpdateEnvironment(updateRoot, c.s.log)
	child := spawnUpdaterProcess(updater, args, c.s.opts.IcebathHome, env, true)
	if pid := child.PID(); pid != 0 && stagedUpdaterSupportsPrewrittenMarker(updater) {
		if prewrite := writeUpdateMarker(c.s.opts.IcebathHome, pid); !prewrite.OK {
			c.s.log("[updates] skipping marker pre-write: " + describeSkippedPrewrite(prewrite))
		}
	} else if pid != 0 {
		c.s.log(fmt.Sprintf("[updates] skipping marker pre-write: staged updater predates self-adopt (%s); it would refuse its own claim", updater))
	}
	c.s.log(fmt.Sprintf("[updates] launched updater: %s %s; exiting desktop for application replacement", updater, strings.Join(args, " ")))
	return child
}

// Settle window of the hand-off, the failure text when the updater did not take over
func (c *checkoutStrategy) settleHandoff(child *UpdaterChild, plan *handoffPlan, started time.Time) string {
	if plan != nil {
		return c.confirmScriptHandoff(child, plan, started)
	}
	outcome := observeUpdaterHandoff(child, updateHandoffDwell)
	if outcome.OK {
		return ""
	}
	c.s.log("[updates] hand-off not viable, aborting quit: " + outcome.Message)
	return describeUpdaterHandoffFailure(outcome)
}

func (c *checkoutStrategy) restartBackend() {
	go func() { _, _ = c.s.backend.EnsureBackend("", backend.EnsureOptions{}) }()
}

// Quits after what is left of the dwell once the hand-off is confirmed
func (c *checkoutStrategy) quitAfterDwell(dwellStarted time.Time) {
	c.s.quittingForHandoff.Store(true)
	go func() {
		if wait := updateHandoffDwell - time.Since(dwellStarted); wait > 0 {
			time.Sleep(wait)
		}
		c.s.host.Quit()
	}()
}

func quoteLauncherArg(value string, isWindows bool) string {
	if isWindows {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func (c *checkoutStrategy) applyBody() (ApplyResult, error) {
	status, err := c.check(true, "")
	if err != nil {
		return ApplyResult{}, err
	}
	if !status.Supported || status.Error != "" {
		code := status.Error
		if code == "" {
			code = status.Reason
		}
		return ApplyResult{Error: code, Message: status.Message}, nil
	}
	isWindows := c.s.isWindows()
	branch := status.Branch
	if branch == "" {
		branch = defaultUpdateBranch
	}
	targetArgs, targetLabel := []string{"--branch", branch}, branch
	manualCommand := buildManualUpdateCommand(branch)
	if status.Channel != "" {
		targetArgs, targetLabel = []string{"--channel", status.Channel}, status.Channel
		manualCommand = "icebath update --channel " + status.Channel
	}
	updater := resolveStagedUpdaterBinary(c.s.opts.IcebathHome, isWindows)
	root := c.s.resolveUpdateRoot()
	if directoryExists(filepath.Join(root, "pm")) && !pathExists(filepath.Join(root, "scripts", "desktop-update", "runtime.ps1")) {
		launcher := install.ResolveInstalledCommand(context.Background(), root)
		if launcher == "" {
			return ApplyResult{Error: "installation-launcher-missing"}, nil
		}
		quoted := make([]string, len(targetArgs))
		for i, arg := range targetArgs {
			quoted[i] = quoteLauncherArg(arg, isWindows)
		}
		prefix := ""
		if isWindows {
			prefix = "& "
		}
		return ApplyResult{OK: true, Manual: true, Command: prefix + quoteLauncherArg(launcher, isWindows) + " update " + strings.Join(quoted, " "), HermesRoot: root}, nil
	}
	if !isWindows && (updater == "" || status.Channel != "") {
		return c.applyPosixHandoff(targetArgs, targetLabel, manualCommand)
	}
	if updater == "" || status.Channel != "" {
		updateRoot := c.s.resolveUpdateRoot()
		if resolveUpdateScriptHandoff(updateRoot, isWindows) == nil {
			c.s.log(fmt.Sprintf("[updates] no staged updater; surfacing manual `%s` for CLI install at %s", manualCommand, updateRoot))
			c.s.emitUpdateProgress("manual", manualCommand, nil)
			return ApplyResult{OK: true, Manual: true, Command: manualCommand, HermesRoot: updateRoot}, nil
		}
		c.s.log("[updates] no staged updater; using repo hand-off script for CLI install")
	}
	if conflict := updateHandoffConflict(c.s.opts.IcebathHome); conflict != nil {
		c.s.log("[updates] refusing hand-off: " + conflict.Message)
		c.s.emitUpdateProgress("error", conflict.Message, nil)
		return ApplyResult{Error: "update-already-running", Message: conflict.Message}, nil
	}
	c.s.emitUpdateProgress("restart", "Updating Icebath — this window will close and the updater will open. Don’t reopen Icebath yourself; it restarts automatically when the update finishes.", percentOf(100))
	repairMacUpdaterHelper(updater, isMacOS(), c.s.log)
	updateRoot := c.s.resolveUpdateRoot()
	updaterArgs := append([]string{"--update"}, targetArgs...)
	if isMacOS() {
		if targetApp := runningAppBundle(); targetApp != "" {
			updaterArgs = append(updaterArgs, "--target-app", targetApp)
		}
	}
	if err := c.s.preflightStateDB(); err != nil {
		return ApplyResult{}, err
	}
	if isWindows && resolveUpdateScriptHandoff(updateRoot, true) != nil {
		if message := windowsUpdatePrerequisiteError(updateRoot); message != "" {
			c.s.emitUpdateProgress("error", message, nil)
			return ApplyResult{Error: message}, nil
		}
	}
	c.s.stopBackendsForUpdate()
	scriptHandoff := resolveUpdateScriptHandoff(updateRoot, isWindows)
	handoffStarted := time.Now()
	var child *UpdaterChild
	var plan *handoffPlan
	if scriptHandoff != nil {
		updateStartedAt := time.Now().Unix()
		planned, refusal := c.planHandoff(scriptHandoff.ScriptPath, updateStartedAt)
		if refusal != nil {
			c.restartBackend()
			return *refusal, nil
		}
		plan = planned
		var runID *string
		if plan.runID != "" {
			runID = &plan.runID
		}
		c.s.consent.bind(updateRoot, runID, c.s.log, time.Now)
		exe, _ := os.Executable()
		wrappedArgs := []string{"-InstallRoot", updateRoot}
		if status.Channel != "" {
			wrappedArgs = append(wrappedArgs, "-Channel", status.Channel)
		} else {
			wrappedArgs = append(wrappedArgs, "-Branch", branch)
		}
		wrappedArgs = append(wrappedArgs, "-DesktopPid", strconv.Itoa(os.Getpid()), "-RelaunchExe", exe)
		if c.s.conns.GlobalRemoteActive() {
			wrappedArgs = append(wrappedArgs, "-NoGateway")
		}
		if plan.runID != "" {
			wrappedArgs = append(wrappedArgs, "-HandoffRun", plan.runID)
		}
		command, args := wrapHandoffForDetachedConsole(*scriptHandoff, wrappedArgs, false)
		child = spawnUpdaterProcess(command, args, c.s.opts.IcebathHome, c.s.windowsHandoffEnvironment(updateRoot, updateStartedAt, plan.runID), false)
		c.s.log(fmt.Sprintf("[updates] launched repo hand-off script: %s (%s); exiting desktop for application replacement", scriptHandoff.ScriptPath, targetLabel))
	} else {
		child = c.spawnStagedUpdater(updater, updaterArgs, updateRoot)
	}
	dwellStarted := time.Now()
	if failure := c.settleHandoff(child, plan, handoffStarted); failure != "" {
		c.s.emitUpdateProgress("error", failure, nil)
		c.restartBackend()
		return ApplyResult{Error: "updater-spawn-failed", Message: failure}, nil
	}
	c.quitAfterDwell(dwellStarted)
	return ApplyResult{OK: true, HandedOff: true, Updater: updater}, nil
}

// Hands a macOS or Linux checkout to the repo owned posix script, the manual command when it predates it
func (c *checkoutStrategy) applyPosixHandoff(targetArgs []string, targetLabel, manualCommand string) (ApplyResult, error) {
	updateRoot := c.s.resolveUpdateRoot()
	handoff := resolvePosixScriptHandoff(updateRoot, false)
	if handoff == nil {
		c.s.emitUpdateProgress("manual", manualCommand, nil)
		return ApplyResult{OK: true, Manual: true, Command: manualCommand, HermesRoot: updateRoot}, nil
	}
	if conflict := updateHandoffConflict(c.s.opts.IcebathHome); conflict != nil {
		c.s.log("[updates] refusing posix hand-off: " + conflict.Message)
		c.s.emitUpdateProgress("error", conflict.Message, nil)
		return ApplyResult{Error: "update-already-running", Message: conflict.Message}, nil
	}
	if err := c.s.preflightStateDB(); err != nil {
		return ApplyResult{}, err
	}
	args := append(append(append([]string(nil), handoff.Args...), "--install-root", updateRoot), targetArgs...)
	args = append(args, "--desktop-pid", strconv.Itoa(os.Getpid()))
	if c.s.conns.GlobalRemoteActive() {
		args = append(args, "--no-gateway")
	}
	handoffStarted := time.Now()
	updateStartedAt := handoffStarted.Unix()
	plan, refusal := c.planHandoff(handoff.ScriptPath, updateStartedAt)
	if refusal != nil {
		return *refusal, nil
	}
	if plan.runID != "" {
		args = append(args, "--handoff-run", plan.runID)
	}
	exe, _ := os.Executable()
	targetApp := exe
	if isMacOS() {
		targetApp = runningAppBundle()
	}
	if targetApp != "" {
		args = append(args, "--relaunch-target", targetApp)
	}
	relaunchArgs := collectRelaunchArgs(c.s.opts.Args[min(1, len(c.s.opts.Args)):])
	if !isMacOS() {
		cwd, _ := os.Getwd()
		args = append(args, "--relaunch-cwd", cwd)
		if sandboxFallbackFromEnv(c.s.opts.Getenv, relaunchArgs) {
			args = append(args, "--sandbox-fallback")
		}
		if len(relaunchArgs) > 0 {
			args = append(append(args, "--"), relaunchArgs...)
		}
	}
	env := append(c.s.sourceUpdateEnvironment(updateRoot, c.s.log), "ICEBATH_UPDATE_STARTED_AT="+strconv.FormatInt(updateStartedAt, 10))
	child := spawnUpdaterProcess(handoff.Command, args, c.s.opts.IcebathHome, env, true)
	c.s.log(fmt.Sprintf("[updates] launched posix hand-off: %s (%s); quitting to hand off", handoff.ScriptPath, targetLabel))
	c.s.emitUpdateProgress("restart", "Updating Icebath — this window will close. Don’t reopen Icebath yourself; it restarts automatically when the update finishes.", percentOf(100))
	dwellStarted := time.Now()
	if failure := c.confirmScriptHandoff(child, plan, handoffStarted); failure != "" {
		c.s.emitUpdateProgress("error", failure, nil)
		return ApplyResult{Error: "updater-spawn-failed", Message: failure}, nil
	}
	c.quitAfterDwell(dwellStarted)
	return ApplyResult{OK: true, HandedOff: true, Updater: handoff.ScriptPath}, nil
}

func directoryExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Running macOS app bundle, empty off macOS or outside a bundle
func runningAppBundle() string {
	if !isMacOS() {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	if strings.HasSuffix(dir, ".app") {
		return dir
	}
	return ""
}

// Why a Windows script hand-off cannot run, empty when its launcher and maintained scripts are present
func windowsUpdatePrerequisiteError(updateRoot string) string {
	if install.ResolveInstalledCommand(context.Background(), updateRoot) == "" {
		return fmt.Sprintf("Update aborted: the installation launcher under %s is missing. Repair this installation before retrying.", updateRoot)
	}
	maintained := filepath.Join(updateRoot, "scripts", "desktop-update")
	if pathExists(maintained) {
		candidate := filepath.Join(maintained, "windows.ps1")
		if !stagedFileExists(candidate) {
			return fmt.Sprintf("Update aborted: %s is missing or unreadable. Repair the installation and review antivirus quarantine before retrying.", candidate)
		}
	}
	return ""
}

// The cmd wrapper that gives a hidden console to a PowerShell hand-off, start /b unless the caller waits
func wrapHandoffForDetachedConsole(handoff UpdateScriptHandoff, extraArgs []string, waitForExit bool) (string, []string) {
	args := []string{"/d", "/s", "/c"}
	if !waitForExit {
		args = append(args, "start", "", "/b")
	}
	args = append(append(append(args, handoff.Command), handoff.Args...), extraArgs...)
	return "cmd.exe", args
}

// Launch arguments worth replaying on relaunch, runtime internal switches dropped
func collectRelaunchArgs(argv []string) []string {
	var out []string
	for _, arg := range argv {
		if arg == "" {
			continue
		}
		internal := false
		for _, prefix := range internalArgPrefixes {
			if strings.HasSuffix(prefix, "=") && strings.HasPrefix(arg, prefix) || !strings.HasSuffix(prefix, "=") && (arg == prefix || strings.HasPrefix(arg, prefix+"=")) {
				internal = true
				break
			}
		}
		if !internal {
			out = append(out, arg)
		}
	}
	return out
}

// Whether the relaunched app must run without the sandbox, from the env switch or a no-sandbox launch
func sandboxFallbackFromEnv(getenv func(string) string, launchArgs []string) bool {
	disable := strings.TrimSpace(getenv("ELECTRON_DISABLE_SANDBOX"))
	if disable == "1" || strings.ToLower(disable) == "true" {
		return true
	}
	for _, arg := range launchArgs {
		if arg == "--no-sandbox" {
			return true
		}
	}
	return false
}

// Environment of the Windows script hand-off, its run id replacing any inherited correlation id
func (s *Service) windowsHandoffEnvironment(updateRoot string, updateStartedAt int64, runID string) []string {
	env := withoutEnv(s.sourceUpdateEnvironment(updateRoot, nil), "ICEBATH_UPDATE_CORRELATION_ID")
	env = append(env, "ICEBATH_UPDATE_STARTED_AT="+strconv.FormatInt(updateStartedAt, 10))
	if runID != "" {
		env = append(env, "ICEBATH_UPDATE_CORRELATION_ID="+runID)
	}
	return env
}

// Environment every update child runs under, the system proxy added and the runtime dir pin dropped
func (s *Service) sourceUpdateEnvironment(updateRoot string, note func(string)) []string {
	base := os.Environ()
	merged := envMap(base)
	for key, value := range updateProxyEnv(merged, note, updateRoot) {
		merged[key] = value
	}
	for key, value := range backend.BuildDesktopBackendEnv(merged, backendPlatform(), s.opts.Home) {
		merged[key] = value
	}
	merged["ICEBATH_HOME"] = s.opts.IcebathHome
	merged["ICEBATH_INSTALL_ROOT"] = updateRoot
	delete(merged, "ICEBATH_RUNTIME_DIR")
	return envList(merged)
}

// Source update status of root through the in process source check under the update environment
func (s *Service) readSourceUpdate(root string, force bool, setChannel string) (*UpdaterStatus, error) {
	if directoryExists(filepath.Join(root, "pm")) && install.ResolveInstalledCommand(context.Background(), root) == "" {
		return nil, errors.New("The source installation launcher is missing; repair this installation before checking updates.")
	}
	git := files.ResolveGitBinary()
	args := []string{"--install-root", root, "--home", s.opts.IcebathHome, "--git", git, "--branch-config-path", filepath.Join(s.opts.UserData, desktopUpdateConfigFile)}
	if setChannel != "" {
		args = append(args, "--set-channel", setChannel)
	}
	if force {
		args = append(args, "--force")
	}
	ctx, cancel := context.WithTimeout(selfupdate.WithSourceCheckEnviron(context.Background(), s.sourceUpdateEnvironment(root, nil)), sourceCheckTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := selfupdate.SourceCheckMain(ctx, args, &stdout, &stderr); code != 0 {
		return nil, fmt.Errorf("Command failed: icebath source-check %s\n%s", strings.Join(args, " "), stderr.String())
	}
	text := strings.TrimSpace(stdout.String())
	if text == "null" {
		return nil, nil
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &probe) != nil {
		return nil, errors.New("The source update check returned an invalid status.")
	}
	var supported bool
	if raw, ok := probe["supported"]; !ok || json.Unmarshal(raw, &supported) != nil {
		return nil, errors.New("The source update check returned an invalid status.")
	}
	var status UpdaterStatus
	if err := json.Unmarshal([]byte(text), &status); err != nil {
		return nil, errors.New("The source update check returned an invalid status.")
	}
	if status.Behind != nil && *status.Behind == -1 {
		status.Behind = nil
	}
	return &status, nil
}

// Snapshot timeout of the state database, the copy at 5 MiB per second plus a minute within five and thirty minutes
func stateDBPreflightTimeout(home string) time.Duration {
	var size int64
	for _, name := range []string{"state.db", "state.db-wal"} {
		if info, err := os.Stat(filepath.Join(home, name)); err == nil {
			size += info.Size()
		}
	}
	timeout := time.Duration(math.Ceil(float64(size)/stateDBCopyRate))*time.Second + stateDBTimeoutSlack
	return min(stateDBMaxTimeout, max(stateDBDefaultTimeout, timeout))
}

// Whether updates.pre_update_backup leaves the emergency snapshot on, an unreadable answer keeping it
func (s *Service) preUpdateBackupEnabled() bool {
	ctx := context.Background()
	cfg, err := config.LoadContext(ctx, s.opts.IcebathHome, config.EnvRefLookupContext(ctx))
	if err != nil {
		return true
	}
	value, found := cfg.Lookup("updates.pre_update_backup")
	if !found {
		return true
	}
	if value == nil || value == false {
		return false
	}
	text, isText := value.(string)
	return !isText || !disabledBackupModes[strings.ToLower(strings.TrimSpace(text))]
}

// Emergency state.db snapshot before an update touches anything, an error cancelling the update
func (s *Service) preflightStateDB() error {
	root := s.resolveUpdateRoot()
	if !s.preUpdateBackupEnabled() {
		s.log("[updates] emergency state.db backup disabled by updates.pre_update_backup")
		return nil
	}
	if directoryExists(filepath.Join(root, "pm")) && install.ResolveInstalledCommand(context.Background(), root) == "" {
		message := fmt.Sprintf("state.db pre-flight failed: the installation launcher under %s is missing. Update cancelled before backend shutdown. Repair this installation before retrying.", root)
		s.log("[updates] " + message)
		return errors.New(message)
	}
	home := s.opts.IcebathHome
	ctx, cancel := context.WithTimeout(context.Background(), stateDBPreflightTimeout(home))
	defer cancel()
	result, err := backup.PreflightStateDB(ctx, home)
	var text string
	if err == nil {
		text, err = pyjson.Dumps(result)
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		message := fmt.Sprintf("state.db pre-flight failed: %s. Update cancelled before backend shutdown. Update the selected installation with its icebath update command, then retry.", err.Error())
		s.log("[updates] " + message)
		return errors.New(message)
	}
	s.log("[updates] state.db pre-flight: " + strings.TrimSpace(text))
	return nil
}

func envMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		if key, value, ok := strings.Cut(kv, "="); ok {
			out[key] = value
		}
	}
	return out
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

func withoutEnv(environ []string, key string) []string {
	out := environ[:0:0]
	for _, kv := range environ {
		if name, _, _ := strings.Cut(kv, "="); name != key {
			out = append(out, kv)
		}
	}
	return out
}
