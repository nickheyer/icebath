package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/install"
)

const (
	bootstrapLogRingMax          = 500
	bootstrapMarkerName          = ".icebath-bootstrap-complete"
	bootstrapMarkerSchemaVersion = 1
	defaultUpdateBranch          = "main"
	maxBootstrapRepairSoft       = 3
	bootstrapFailureRemedy       = "Common causes: no internet connection, antivirus blocking the installer, or another copy of Icebath running. " +
		"Close other Icebath windows and choose Reload and retry; if it fails again, open the logs and send them to support."
)

var (
	stampCommitRE = regexp.MustCompile(`(?i)^[0-9a-f]{7,40}$`)
	stageWordsRE  = regexp.MustCompile(`[-_]+`)
	stageLabels   = map[string]string{
		"uv": "Package installer", "git": "Git", "node": "Node.js", "system-packages": "System packages", "repository": "Icebath source code",
		"python": "Python runtime", "venv": "Python environment", "dependencies": "Python packages", "node-deps": "Browser tool packages",
		"desktop": "Desktop app build", "platform-sdks": "Platform tools", "configure": "Settings", "config-templates": "Settings templates",
		"path": "Icebath command", "gateway": "Icebath service", "bootstrap-marker": "Finishing touches", "prerequisites": "System prerequisites",
		"python-deps": "Python packages", "config": "Settings", "setup": "Settings", "complete": "Finishing touches",
	}
)

// Bootstrap progress the renderer reads on mount and the bootstrap events keep current
type bootstrapState struct {
	Active              bool                      `json:"active"`
	Manifest            map[string]any            `json:"manifest"`
	Stages              map[string]map[string]any `json:"stages"`
	Error               any                       `json:"error"`
	Log                 []map[string]any          `json:"log"`
	StartedAt           any                       `json:"startedAt"`
	CompletedAt         any                       `json:"completedAt"`
	SetupChoice         map[string]any            `json:"setupChoice"`
	UnsupportedPlatform map[string]any            `json:"unsupportedPlatform"`
}

func emptyBootstrapState() bootstrapState {
	return bootstrapState{Stages: map[string]map[string]any{}, Log: []map[string]any{}}
}

func orNil(v any, ok bool) any {
	if !ok {
		return nil
	}
	return v
}

func orString(v any, fallback string) any {
	if !jsTruthy(v) {
		return fallback
	}
	return v
}

// Folds one bootstrap event into the snapshot and sends it to the main window
func (s *Service) BroadcastBootstrapEvent(ev map[string]any) {
	s.mu.Lock()
	st := &s.bootstrap
	now := time.Now().UnixMilli()
	switch ev["type"] {
	case "manifest":
		st.Manifest, st.Active, st.SetupChoice = ev, true, nil
		if !jsTruthy(st.StartedAt) {
			st.StartedAt = now
		}
		st.Stages = map[string]map[string]any{}
		stages, _ := ev["stages"].([]any)
		for _, raw := range stages {
			if stage, ok := raw.(map[string]any); ok {
				st.Stages[jsString(stage["name"])] = map[string]any{"state": "pending", "json": nil, "durationMs": nil, "error": nil}
			}
		}
	case "stage":
		duration, hasDuration := ev["durationMs"]
		frame, hasJSON := ev["json"]
		errText, hasErr := ev["error"]
		st.Stages[jsString(ev["name"])] = map[string]any{"state": ev["state"], "durationMs": orNil(duration, hasDuration), "json": orNil(frame, hasJSON), "error": orNil(errText, hasErr)}
	case "log":
		st.Log = append(st.Log, map[string]any{"ts": now, "stage": orNil(ev["stage"], jsTruthy(ev["stage"])), "line": ev["line"], "stream": orString(ev["stream"], "stdout")})
		if len(st.Log) > bootstrapLogRingMax {
			st.Log = append([]map[string]any(nil), st.Log[len(st.Log)-bootstrapLogRingMax:]...)
		}
	case "complete":
		st.Active, st.CompletedAt, st.Error, st.UnsupportedPlatform = false, now, nil, nil
	case "failed":
		st.Active, st.Error, st.SetupChoice = false, orString(ev["error"], "unknown error"), nil
	case "unsupported-platform":
		st.Active, st.SetupChoice = false, nil
		st.UnsupportedPlatform = map[string]any{"platform": ev["platform"], "activeRoot": ev["activeRoot"], "installCommand": ev["installCommand"], "docsUrl": ev["docsUrl"]}
	case "setup-choice":
		st.Active, st.Error, st.Manifest, st.Stages, st.UnsupportedPlatform = false, nil, nil, map[string]map[string]any{}, nil
		st.SetupChoice = nil
		if jsTruthy(ev["active"]) {
			st.SetupChoice = map[string]any{"platform": ev["platform"], "activeRoot": ev["activeRoot"], "local": orString(ev["local"], "none"), "bundled": s.installShape() == "bundled"}
		}
	case "dismissed":
		s.bootstrap = emptyBootstrapState()
	}
	s.mu.Unlock()
	if main := s.host.MainWindowID(); main != "" {
		s.host.Send(main, "bootstrap:event", ev)
	}
}

// Bootstrap snapshot with whether this artifact bundles its runtime
func (s *Service) GetBootstrapState() map[string]any {
	s.mu.Lock()
	raw, _ := json.Marshal(s.bootstrap)
	s.mu.Unlock()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	out["bundled"] = s.installShape() == "bundled"
	return out
}

func (s *Service) resetBootstrapSnapshot() {
	s.mu.Lock()
	s.bootstrap = emptyBootstrapState()
	s.mu.Unlock()
}

// Label of an installer stage, its humanized name when it has no label
func bootstrapStageLabel(stage string) string {
	if stage == "" {
		return ""
	}
	if label, ok := stageLabels[stage]; ok {
		return label
	}
	words := strings.TrimSpace(stageWordsRE.ReplaceAllString(stage, " "))
	if words == "" {
		return ""
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

// Lead sentence, remedy and details line the install overlay shows for a failed bootstrap
func describeBootstrapFailure(failedStage, rawError string) string {
	lead := "Setting up Icebath stopped before it could finish."
	if label := bootstrapStageLabel(failedStage); label != "" {
		lead = fmt.Sprintf("Setting up Icebath stopped during the '%s' step.", label)
	}
	details := strings.TrimSpace(rawError)
	if details == "" {
		details = "unknown error"
	}
	return fmt.Sprintf("%s %s\nDetails: %s", lead, bootstrapFailureRemedy, details)
}

// RepairDecision says whether a repair reinstalls or only restarts
type RepairDecision struct {
	HardReinstall bool
	Reason        string
	Attempt       int
}

// Escalates a repair to a hard reinstall only past the soft restart budget
func decideBootstrapRepair(attempt, maxSoftAttempts int, primaryBackendAlive bool) RepairDecision {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > maxSoftAttempts {
		return RepairDecision{HardReinstall: true, Attempt: attempt,
			Reason: fmt.Sprintf("repair attempt %d exceeds soft-restart budget (%d); escalating to hard reinstall", attempt, maxSoftAttempts)}
	}
	if primaryBackendAlive {
		return RepairDecision{Attempt: attempt, Reason: fmt.Sprintf("repair attempt %d/%d: primary backend process still alive (likely transient stall, see #74874); restarting only, skipping installer", attempt, maxSoftAttempts)}
	}
	return RepairDecision{Attempt: attempt, Reason: fmt.Sprintf("repair attempt %d/%d: primary backend process has exited; restarting before escalating to reinstall", attempt, maxSoftAttempts)}
}

func isPinnedCommit(commit string) bool {
	return stampCommitRE.MatchString(commit) && !isFallbackCommit(commit)
}

// HEAD of a checkout when it is a real pin
func resolveCheckoutHead(root, gitBinary string) string {
	if root == "" {
		return ""
	}
	if gitBinary == "" {
		gitBinary = "git"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBinary, "-c", "windows.appendAtomically=false", "rev-parse", "HEAD")
	cmd.Dir = root
	detachUpdater(cmd, false)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	if sha := strings.TrimSpace(string(out)); isPinnedCommit(sha) {
		return sha
	}
	return ""
}

func readExistingPinnedCommit(root string) string {
	if root == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(root, bootstrapMarkerName))
	if err != nil {
		return ""
	}
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	if commit, ok := parsed["pinnedCommit"].(string); ok && isPinnedCommit(commit) {
		return commit
	}
	return ""
}

// Commit the bootstrap marker pins, the checkout HEAD, else the stamp's real pin, else the prior marker's
func resolveMarkerPinnedCommit(stamp *InstallStamp, root, gitBinary string) string {
	if head := resolveCheckoutHead(root, gitBinary); head != "" {
		return head
	}
	if stamp != nil && isPinnedCommit(stringOrEmpty(stamp.Commit)) {
		return *stamp.Commit
	}
	return readExistingPinnedCommit(root)
}

// Writes the bootstrap complete marker beside the runtime atomically
func (s *Service) writeBootstrapMarker(pinnedCommit, pinnedBranch string) (map[string]any, error) {
	file := filepath.Join(s.activeRoot(), bootstrapMarkerName)
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		return nil, err
	}
	nullable := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	merged := map[string]any{"schemaVersion": bootstrapMarkerSchemaVersion, "pinnedCommit": nullable(pinnedCommit), "pinnedBranch": nullable(pinnedBranch),
		"completedAt": install.ISO8601UTC(time.Now()), "desktopVersion": s.host.AppVersion()}
	body := fmt.Sprintf("{\n  \"schemaVersion\": %d,\n  \"pinnedCommit\": %s,\n  \"pinnedBranch\": %s,\n  \"completedAt\": %s,\n  \"desktopVersion\": %s\n}\n",
		bootstrapMarkerSchemaVersion, jsonText(merged["pinnedCommit"]), jsonText(merged["pinnedBranch"]), jsonText(merged["completedAt"]), jsonText(merged["desktopVersion"]))
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o666); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, file); err != nil {
		return nil, err
	}
	return merged, nil
}

func jsonText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Outcome of one bootstrap run
type bootstrapResult struct {
	ok          bool
	cancelled   bool
	failedStage string
	err         string
	marker      map[string]any
}

// Desktop bootstrap event of an installer stage frame
func stageEvent(name string, state install.StageState, durationMs int64, frame *install.StageResult, errText string, withJSON bool) map[string]any {
	ev := map[string]any{"type": "stage", "name": name, "state": string(state), "durationMs": durationMs}
	if withJSON {
		if frame == nil {
			ev["json"] = nil
		} else {
			ev["json"] = frame
		}
	}
	if errText != "" {
		ev["error"] = errText
	}
	return ev
}

// Runs the install stages in process, teeing every event to the run log and onEvent, then writes the marker
func (s *Service) runBootstrap(ctx context.Context, onEvent func(map[string]any)) bootstrapResult {
	if ctx.Err() != nil {
		onEvent(map[string]any{"type": "failed", "error": "bootstrap cancelled by user"})
		return bootstrapResult{cancelled: true}
	}
	logRoot := filepath.Join(s.opts.IcebathHome, "logs")
	_ = os.MkdirAll(logRoot, 0o777)
	runLogPath := filepath.Join(logRoot, "bootstrap-"+strings.NewReplacer(":", "-", ".", "-").Replace(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))+".log")
	runLog, _ := os.OpenFile(runLogPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o666)
	if runLog != nil {
		defer runLog.Close()
	}
	emit := func(ev map[string]any) {
		if runLog != nil {
			line, _ := json.Marshal(ev)
			_, _ = runLog.Write(append(line, '\n'))
		}
		onEvent(ev)
	}
	stamp := s.stamp
	stampLabel := "<none>"
	if stamp != nil && stamp.Commit != nil {
		stampLabel = jsSlice(*stamp.Commit, 12)
	}
	activeRoot := s.activeRoot()
	emit(map[string]any{"type": "log", "line": fmt.Sprintf("[bootstrap] starting at %s; activeRoot=%s; stamp=%s; runLog=%s",
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), activeRoot, stampLabel, runLogPath)})
	existingCheckout := isGitCheckout(activeRoot)
	pinCommit := !existingCheckout
	if existingCheckout && stamp != nil && stringOrEmpty(stamp.Commit) != "" {
		emit(map[string]any{"type": "log", "line": fmt.Sprintf("[bootstrap] existing checkout detected at %s; not pinning to packaged install stamp %s", activeRoot, jsSlice(*stamp.Commit, 12))})
	}
	opts := install.Options{NonInteractive: true, JSON: true, Dir: activeRoot, Home: s.opts.IcebathHome, InstallMethod: install.MethodScript}
	if stamp != nil && stringOrEmpty(stamp.Branch) != "" {
		opts.Branch = *stamp.Branch
	}
	if pinCommit && stamp != nil && isPinnedCommit(stringOrEmpty(stamp.Commit)) {
		opts.Commit = *stamp.Commit
	}
	failure := func(stage, message string) bootstrapResult {
		if ctx.Err() != nil {
			emit(map[string]any{"type": "failed", "error": "bootstrap cancelled by user"})
			return bootstrapResult{cancelled: true}
		}
		ev := map[string]any{"type": "failed", "error": message}
		if stage != "" {
			ev["stage"] = stage
		}
		emit(ev)
		return bootstrapResult{failedStage: stage, err: message}
	}
	runner, err := install.NewRunner(opts, func(e install.Event) {
		if e.Type != install.EventLog {
			return
		}
		e = e.SanitizedForUI()
		ev := map[string]any{"type": "log", "line": e.Line, "stream": string(e.Stream)}
		if e.Stage != nil && *e.Stage != "" {
			ev["stage"] = *e.Stage
		}
		emit(ev)
	})
	if err != nil {
		return failure("", err.Error())
	}
	manifest := install.InstallManifest(false)
	stages := make([]any, len(manifest.Stages))
	for i, stage := range manifest.Stages {
		stages[i] = map[string]any{"name": stage.Name, "title": stage.Title, "category": stage.Category, "needs_user_input": stage.NeedsUserInput}
	}
	var protocol any
	if manifest.ProtocolVersion != nil {
		protocol = *manifest.ProtocolVersion
	}
	emit(map[string]any{"type": "manifest", "stages": stages, "protocolVersion": protocol})
	for _, stage := range manifest.Stages {
		if ctx.Err() != nil {
			emit(map[string]any{"type": "failed", "error": "bootstrap cancelled by user"})
			return bootstrapResult{cancelled: true}
		}
		started := time.Now()
		emit(map[string]any{"type": "stage", "name": stage.Name, "state": "running"})
		frame, code := runner.RunStage(ctx, stage.Name)
		duration := time.Since(started).Milliseconds()
		var ev map[string]any
		switch {
		case ctx.Err() != nil:
			ev = stageEvent(stage.Name, install.StateFailed, duration, nil, "cancelled by user", false)
		case frame.OK && frame.Skipped:
			ev = stageEvent(stage.Name, install.StateSkipped, duration, &frame, "", true)
		case frame.OK:
			ev = stageEvent(stage.Name, install.StateSucceeded, duration, &frame, "", true)
		default:
			reason := fmt.Sprintf("exit code %d", code)
			if frame.Reason != nil && *frame.Reason != "" {
				reason = *frame.Reason
			}
			ev = stageEvent(stage.Name, install.StateFailed, duration, &frame, reason, true)
		}
		emit(ev)
		if ev["state"] == "failed" {
			errText, _ := ev["error"].(string)
			if errText == "" {
				errText = "stage failed"
			}
			emit(map[string]any{"type": "failed", "stage": stage.Name, "error": errText})
			return bootstrapResult{failedStage: stage.Name, err: errText}
		}
	}
	pinned := resolveMarkerPinnedCommit(stamp, activeRoot, s.gitBinary())
	if pinned == "" {
		emit(map[string]any{"type": "log", "line": "[bootstrap] WARNING: could not resolve a real pinnedCommit for the bootstrap-complete marker; subsequent launches may re-run bootstrap"})
	} else if stamp != nil && !isPinnedCommit(stringOrEmpty(stamp.Commit)) {
		emit(map[string]any{"type": "log", "line": fmt.Sprintf("[bootstrap] fallback stamp resolved marker pin to %s from checkout", jsSlice(pinned, 12))})
	}
	branch := ""
	if stamp != nil {
		branch = stringOrEmpty(stamp.Branch)
	}
	marker, err := s.writeBootstrapMarker(pinned, branch)
	if err != nil {
		return failure("", nodeErrorMessage(err))
	}
	emit(map[string]any{"type": "complete", "marker": marker})
	return bootstrapResult{ok: true, marker: marker}
}

// Git the desktop runs, the PATH one
func (s *Service) gitBinary() string {
	if found, err := exec.LookPath("git"); err == nil {
		return found
	}
	return "git"
}

// BootstrapError is a latched bootstrap failure with what the boot screen needs to tell it apart
type BootstrapError struct {
	backend.BootstrapFailureError
	HandedOff   bool
	Cancelled   bool
	FailedStage string
}

func (s *Service) latch(err *BootstrapError) error {
	s.mu.Lock()
	s.bootstrapFailure = err
	s.mu.Unlock()
	return err
}

// Installs the runtime a bootstrap-needed backend names and resolves the installed one, passing a usable one through
func (s *Service) EnsureRuntime(b backend.ResolvedBackend, assertStillOwned func() error) (backend.ResolvedBackend, error) {
	if err := s.backend.AssertCanStart(); err != nil {
		return b, err
	}
	if err := assertStillOwned(); err != nil {
		return b, err
	}
	if !b.Bootstrap {
		s.log("[backend] using " + b.Label)
		s.backend.AdvanceBootProgress("runtime.external", "Using "+b.Label, 32)
		return b, nil
	}
	if b.Kind != "bootstrap-needed" {
		return b, fmt.Errorf("Unexpected bootstrap backend: %s", b.Kind)
	}
	if s.installShape() == "bundled" {
		s.log("[bootstrap] REFUSING installer on a bundled install; payload missing or damaged — reinstall the app")
		return b, s.latch(&BootstrapError{BootstrapFailureError: backend.BootstrapFailureError{
			Message: "This app bundles its own Icebath runtime, but the runtime files are missing or damaged. Reinstall Icebath Desktop to restore it."}})
	}
	s.log("[bootstrap] no Icebath install found; starting first-launch bootstrap")
	if s.handOffWindowsBootstrapRecovery("bootstrap-needed") {
		return b, s.latch(&BootstrapError{BootstrapFailureError: backend.BootstrapFailureError{
			Message: "Icebath recovery was handed off to Icebath Setup. The desktop will restart when recovery completes."}, HandedOff: true})
	}
	s.BroadcastBootstrapEvent(map[string]any{"type": "manifest", "stages": []any{}, "protocolVersion": nil})
	if err := s.backend.AssertCanStart(); err != nil {
		return b, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.bootstrapAbort = cancel
	s.bootstrapRepairRequested = false
	s.bootstrapRepairAttempt = 0
	s.mu.Unlock()
	result := s.runBootstrap(ctx, func(ev map[string]any) {
		line, _ := json.Marshal(ev)
		s.log("[bootstrap] " + string(line))
		s.BroadcastBootstrapEvent(ev)
	})
	s.mu.Lock()
	s.bootstrapAbort = nil
	s.mu.Unlock()
	cancel()
	if result.cancelled {
		return b, s.latch(&BootstrapError{BootstrapFailureError: backend.BootstrapFailureError{Message: "Icebath install was cancelled."}, Cancelled: true})
	}
	if !result.ok {
		return b, s.latch(&BootstrapError{BootstrapFailureError: backend.BootstrapFailureError{Message: describeBootstrapFailure(result.failedStage, result.err)}, FailedStage: result.failedStage})
	}
	s.log("[bootstrap] bootstrap complete; marker written. Re-resolving backend.")
	resolved, err := s.opts.InstalledRuntime.AfterInstall(func() (*backend.ResolvedBackend, error) {
		next, err := s.opts.Resolve(b.Args)
		return &next, err
	})
	if err != nil {
		return b, err
	}
	return s.EnsureRuntime(*resolved, assertStillOwned)
}

// Branch the update settings pin, main when none is set
func (s *Service) readDesktopUpdateConfig() (string, bool) {
	raw, err := os.ReadFile(filepath.Join(s.opts.UserData, "updates.json"))
	if err != nil {
		return defaultUpdateBranch, false
	}
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil {
		return defaultUpdateBranch, false
	}
	branch, _ := parsed["branch"].(string)
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return defaultUpdateBranch, false
	}
	return branch, true
}

// Releases the app owned backends for output replacement, a Windows rename refusal being the only blocker
func (s *Service) stopBackendsForUpdate() {
	if !s.isWindows() {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.backend.TeardownPrimary(false)
	}()
	s.backend.StopAllPoolBackends()
	<-done
}

// Quits after the hand-off dwell, the updater relaunching the desktop when it is done
func (s *Service) quitForHandoff(started time.Time) {
	s.quittingForHandoff.Store(true)
	go func() {
		if wait := updateHandoffDwell - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
		s.host.Quit()
	}()
}

// Hands a Windows packaged bootstrap to the staged setup app, true when the desktop now quits for it
func (s *Service) handOffWindowsBootstrapRecovery(reason string) bool {
	if !s.isWindows() || !s.opts.IsPackaged {
		return false
	}
	if s.installShape() == "bundled" {
		s.log("[bootstrap] refusing updater recovery hand-off on a bundled install; reinstall the app")
		return false
	}
	updater := resolveStagedUpdaterBinary(s.opts.IcebathHome, true)
	if updater == "" {
		return false
	}
	if conflict := updateHandoffConflict(s.opts.IcebathHome); conflict != nil {
		s.log("[bootstrap] refusing recovery hand-off: " + conflict.Message)
		s.quitForHandoff(time.Now())
		return true
	}
	updateRoot := s.resolveUpdateRoot()
	branch, _ := s.readDesktopUpdateConfig()
	args := []string{"--repair", "--branch", branch}
	if install.ResolveInstalledCommand(context.Background(), updateRoot) != "" {
		args = []string{"--update", "--branch", branch}
	}
	s.stopBackendsForUpdate()
	child := spawnUpdaterProcess(updater, args, s.opts.IcebathHome, append(os.Environ(), "ICEBATH_HOME="+s.opts.IcebathHome, "ICEBATH_INSTALL_ROOT="+updateRoot), true)
	if pid := child.PID(); pid != 0 && stagedUpdaterSupportsPrewrittenMarker(updater) {
		if prewrite := writeUpdateMarker(s.opts.IcebathHome, pid); !prewrite.OK {
			s.log("[bootstrap] skipping marker pre-write: " + describeSkippedPrewrite(prewrite))
		}
	} else if pid != 0 {
		s.log(fmt.Sprintf("[bootstrap] skipping marker pre-write: staged updater predates self-adopt (%s); it would refuse its own claim", updater))
	}
	s.log(fmt.Sprintf("[bootstrap] handed off %s recovery to updater: %s %s; exiting desktop to release its files", reason, updater, strings.Join(args, " ")))
	started := time.Now()
	if outcome := observeUpdaterHandoff(child, updateHandoffDwell); !outcome.OK {
		s.log("[bootstrap] recovery hand-off not viable, staying alive: " + outcome.Message)
		return false
	}
	s.quitForHandoff(started)
	return true
}

// Aborts the running first launch install, answering whether one was running
func (s *Service) CancelBootstrap() map[string]bool {
	s.mu.Lock()
	abort := s.bootstrapAbort
	s.mu.Unlock()
	if abort == nil {
		return map[string]bool{"ok": false, "cancelled": false}
	}
	abort()
	return map[string]bool{"ok": true, "cancelled": true}
}
