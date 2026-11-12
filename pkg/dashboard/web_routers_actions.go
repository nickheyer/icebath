package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Calls into hermes_cli update_contract, source_check and gateway_migrate, packages above this one, assigned in cmd/icebath/wiring.go
var UpdateCLI struct {
	IsCommitBuild          func(projectRoot string) bool
	CommitBuildMessage     string
	UpdateAdmissionRefusal func(ctx context.Context, projectRoot string) (code, message, command string, err error)
	RecordUpdateRefusal    func(ctx context.Context, code, message, command string)
	CheckForUpdates        func(ctx context.Context, force bool) (*pyjson.Object, error)
	BuildMigrationPlan     func(ctx context.Context) (*pyjson.Object, error)
}

const (
	actionLogTailMaxBytes          = 256 * 1024
	actionLogTailInitialChunkBytes = 8 * 1024
	actionLogTailMaxChunkBytes     = 64 * 1024
	managedExternallyMessage       = "Icebath updates are managed outside this dashboard in containerized environments."
	updateLogStartedPrefix         = "=== icebath update started "
	updateActionStartedPrefix      = "=== hermes-update started "
)

var (
	updateActionCompletedRE = regexp.MustCompile(`^=== icebath-update completed ([0-9a-f]{32}) ===$`)
	updateActionStartedRE   = regexp.MustCompile(`^=== hermes-update started .* ([0-9a-f]{32}) ===$`)
	actionIDRE              = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Per kind dashboard error codes the UI keys on, by admission refusal code
var updateRefusalErrorCodes = map[string]string{
	"docker": "docker_update_unsupported", "image-marker": "docker_update_unsupported",
	"image-marker-invalid": "docker_update_unsupported", "apt": "apt_update_required",
	"nix": "nix_update_unsupported",
}

// Records a terminal result and drops the live process registries of the action
func finishAction(name string, exitCode *int, pid *int) {
	actionMu.Lock()
	defer actionMu.Unlock()
	var code, p any
	if exitCode != nil {
		code = int64(*exitCode)
	}
	if pid != nil {
		p = int64(*pid)
	}
	actionResults[name] = pyjson.ObjectOf("exit_code", code, "pid", p)
	delete(actionProcs, name)
	delete(actionCommands, name)
	delete(actionIDs, name)
}

// Records a result of an action that never spawned and appends its message to the action log
func recordCompletedAction(name, message string, exitCode int) error {
	logDir := actionLogDir()
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return err
	}
	actionMu.Lock()
	logName, ok := actionLogFiles[name]
	actionMu.Unlock()
	if !ok {
		return fmt.Errorf("KeyError: %s", pyrepr.Repr(name))
	}
	f, err := os.OpenFile(filepath.Join(logDir, logName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	text := "\n=== " + name + " completed " + time.Now().Format("2006-01-02 15:04:05") + " ===\n" + strings.ToValidUTF8(message, "?")
	if !strings.HasSuffix(message, "\n") {
		text += "\n"
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	finishAction(name, &exitCode, nil)
	return nil
}

// Last n lines of a log read backward in growing chunks, never past the tail byte bound
func tailLines(path string, n int) []string {
	st, err := os.Stat(path)
	if err != nil {
		return []string{}
	}
	size := st.Size()
	if n <= 0 || size <= 0 {
		return []string{}
	}
	minOffset := max(0, size-actionLogTailMaxBytes)
	offset := size
	chunkSize := int64(actionLogTailInitialChunkBytes)
	newlines := 0
	var chunks [][]byte
	dropPartialFirstLine := false
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	for offset > minOffset && newlines <= n {
		readSize := min(chunkSize, offset-minOffset)
		offset -= readSize
		chunk := make([]byte, readSize)
		got, err := f.ReadAt(chunk, offset)
		if err != nil && err != io.EOF {
			return []string{}
		}
		chunk = chunk[:got]
		chunks = append(chunks, chunk)
		newlines += bytes.Count(chunk, []byte("\n"))
		chunkSize = min(chunkSize*2, actionLogTailMaxChunkBytes)
	}
	if offset > 0 {
		one := make([]byte, 1)
		got, err := f.ReadAt(one, offset-1)
		if err != nil && err != io.EOF {
			return []string{}
		}
		dropPartialFirstLine = got != 1 || one[0] != '\n'
	}
	slices.Reverse(chunks)
	lines := pystr.Splitlines(pystr.DecodeUTF8Replace(bytes.Join(chunks, nil)), false)
	if dropPartialFirstLine && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// Latest successful update id of update.log whose completion marker follows the latest start marker
func durableCompletedUpdateActionID(lines []string) string {
	lastStart, lastCompleted := -1, -1
	completed := ""
	for i, line := range lines {
		if strings.HasPrefix(line, updateLogStartedPrefix) {
			lastStart = i
		}
		if m := updateActionCompletedRE.FindStringSubmatch(pystr.Strip(line)); m != nil {
			lastCompleted = i
			completed = m[1]
		}
	}
	if completed != "" && lastCompleted > lastStart {
		return completed
	}
	return ""
}

// Action id named by the latest update started header of this dashboard's action log
func latestSpawnedUpdateActionID(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], updateActionStartedPrefix) {
			if m := updateActionStartedRE.FindStringSubmatch(pystr.Strip(lines[i])); m != nil {
				return m[1]
			}
			return ""
		}
	}
	return ""
}

// Action id of the latest spawn of the action from its sidecar file, empty when unreadable or malformed
func persistedActionID(logDir, name string) string {
	raw, err := os.ReadFile(filepath.Join(logDir, name+".action_id"))
	if err != nil {
		return ""
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return ""
	}
	value := pystr.Strip(text)
	if actionIDRE.MatchString(value) {
		return value
	}
	return ""
}

// Starts a forced gateway restart that supersedes a drain
func restartGateway(ctx context.Context, profile string) (*pyjson.Object, error) {
	proc, _, err := spawnGatewayRestart(ctx, profile, true)
	if err := httpFailure(err, "Failed to spawn gateway restart", 500, "Failed to restart gateway", nil); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "pid", int64(proc.Pid()), "name", "gateway-restart"), nil
}

// Preflight of folding per profile gateways into one multiplexer, the plan the CLI prints
func gatewayMigratePlan(ctx context.Context) (*pyjson.Object, error) {
	return UpdateCLI.BuildMigrationPlan(ctx)
}

// Starts the multiplex migration detached, the CLI refusing into the action log when blocked
func gatewayMigrate(ctx context.Context) (*pyjson.Object, error) {
	proc, err := spawnHermesAction(ctx, []string{"gateway", "migrate", "--multiplex", "--yes"}, "gateway-migrate", nil)
	if err := httpFailure(err, "Failed to spawn gateway migrate", 500, "Failed to start gateway migration", nil); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "pid", int64(proc.Pid()), "name", "gateway-migrate"), nil
}

// Writes or removes the external drain marker the gateway watcher acts on
func gatewayDrain(ctx context.Context, req *Request) (*pyjson.Object, error) {
	parsed, err := req.JSON(ctx)
	var body any = pyjson.NewObject()
	if err == nil && pyrepr.Truthy(parsed) {
		body = parsed
	}
	obj, ok := body.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(body))
	}
	actionRaw, present := obj.Get("action")
	if !present {
		actionRaw = "drain"
	}
	action := pystr.Lower(pystr.Strip(pyrepr.Str(actionRaw)))
	principal := "dashboard"
	if tp, ok := req.State().Get("token_principal").(*gateway.TokenPrincipal); ok && tp != nil && tp.Principal != "" {
		principal = tp.Principal
	}
	if action == "cancel" {
		existed := gateway.ClearDrainRequest(ctx, "")
		routersLog().Info(fmt.Sprintf("Gateway drain CANCEL requested by %s (existed=%s)", principal, pyrepr.Str(existed)))
		return pyjson.ObjectOf("ok", true, "action", "cancel", "was_draining", existed), nil
	}
	if action != "drain" {
		return nil, NewHTTPException(400, fmt.Sprintf("Unknown drain action %s; expected 'drain' or 'cancel'", pyrepr.Repr(action)))
	}
	suppress, _ := obj.Get("suppress_notification")
	payload, err := gateway.WriteDrainRequest(ctx, principal, pyrepr.Truthy(suppress), "")
	if err != nil {
		return nil, err
	}
	suppressed, _ := payload.Get("suppress_notification")
	routersLog().Info(fmt.Sprintf("Gateway drain BEGIN requested by %s (suppress_notification=%s)", principal, pyrepr.Str(suppressed)))
	requestedAt, _ := payload.Get("requested_at")
	return pyjson.ObjectOf("ok", true, "action", "drain", "requested_at", requestedAt,
		"draining", gateway.DrainRequested(ctx, ""), "suppress_notification", suppressed), nil
}

// Refused update answer, its message recorded as a failed update action
func updateRefused(errCode, message, updateCommand string) (*pyjson.Object, error) {
	if err := recordCompletedAction("hermes-update", message, 1); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", false, "pid", nil, "name", "hermes-update", "error", errCode, "message", message,
		"update_command", updateCommand), nil
}

// Starts icebath update in the background unless the install refuses an in place update
func updateHermes(ctx context.Context) (*pyjson.Object, error) {
	if UpdateCLI.IsCommitBuild(ProjectRoot()) {
		return updateRefused("commit-build", UpdateCLI.CommitBuildMessage, "")
	}
	if dashboardLocalUpdateManagedExternally(ctx) {
		return updateRefused("dashboard_update_managed_externally", managedExternallyMessage+" The built-in local updater is disabled here.", "")
	}
	refusalCode, refusalMessage, refusalCommand, err := UpdateCLI.UpdateAdmissionRefusal(ctx, ProjectRoot())
	if err != nil {
		return nil, err
	}
	if refusalCode != "" {
		code, ok := updateRefusalErrorCodes[refusalCode]
		if !ok {
			code = "update_not_in_place"
		}
		response, err := updateRefused(code, refusalMessage, refusalCommand)
		if err != nil {
			return nil, err
		}
		UpdateCLI.RecordUpdateRefusal(ctx, refusalCode, refusalMessage, refusalCommand)
		return response, nil
	}
	if existing := actionProcOf("hermes-update"); existing != nil && existing.Poll() == nil {
		response := pyjson.ObjectOf("ok", true, "pid", int64(existing.Pid()), "name", "hermes-update", "already_running", true)
		actionMu.Lock()
		actionID := actionIDs["hermes-update"]
		actionMu.Unlock()
		if actionID != "" {
			response.Set("action_id", actionID)
		}
		return response, nil
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	actionID := hex.EncodeToString(raw[:])
	proc, err := spawnHermesAction(ctx, []string{"update"}, "hermes-update", map[string]string{"ICEBATH_ACTION_ID": actionID})
	if err := httpFailure(err, "Failed to spawn hermes update", 500, "Failed to start update", nil); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "pid", int64(proc.Pid()), "name", "hermes-update", "action_id", actionID), nil
}

// Guidance for an install method the dashboard cannot update in place
func nonApplyableMessage(method string) (string, bool) {
	switch method {
	case "docker":
		return config.FormatDockerUpdateMessage(), true
	case "apt":
		return "Icebath is managed by Termux APT; run `pkg upgrade icebath-agent`.", true
	}
	if manager, ok := packageManagerNames[method]; ok {
		return "Icebath is managed by " + manager + "; run `" + config.RecommendedUpdateCommandForMethod(method) + "`.", true
	}
	return "", false
}

// Display name of each package manager install method the dashboard cannot update in place
var packageManagerNames = map[string]string{
	"chocolatey": "Chocolatey", "pip": "pipx", "homebrew": "Homebrew", "scoop": "Scoop", "winget": "WinGet",
	"dpkg": "its Debian package", "rpm": "its RPM package", "pacman": "its Arch package", "apk": "its Alpine package",
}

// Channel release label with its version when the source check named one
func sourceReleaseLabel(status *pyjson.Object) string {
	channel, _ := status.Get("channel")
	label := "stable"
	if pyrepr.Truthy(channel) {
		label = pyrepr.Str(channel)
	}
	label += " release"
	if version, _ := status.Get("sourceVersion"); pyrepr.Truthy(version) {
		label += " v" + pyrepr.Str(version)
	}
	return label
}

// Derived version of the running build
func derivedVersion() (string, error) {
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		return "", err
	}
	return info.DerivedVersion, nil
}

// Python floor division of a commit time in milliseconds to seconds
func floorDivThousand(v any) (any, error) {
	switch n := v.(type) {
	case int64:
		q := n / 1000
		if n%1000 != 0 && n < 0 {
			q--
		}
		return q, nil
	case float64:
		return math.Floor(n / 1000), nil
	case bool:
		return int64(0), nil
	}
	return nil, fmt.Errorf("TypeError: unsupported operand type(s) for //: '%s' and 'int'", pyrepr.TypeName(v))
}

// One commit row of the update check with its sha shortened and its time in seconds
func updateCommitRow(row any) (*pyjson.Object, error) {
	obj, ok := row.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("TypeError: 'Mapping' expected, got %s", pyrepr.TypeName(row))
	}
	sha, ok := obj.Get("sha")
	if !ok {
		return nil, fmt.Errorf("KeyError: 'sha'")
	}
	text, ok := sha.(string)
	if !ok {
		return nil, fmt.Errorf("TypeError: '%s' object is not subscriptable", pyrepr.TypeName(sha))
	}
	at, ok := obj.Get("at")
	if !ok {
		return nil, fmt.Errorf("KeyError: 'at'")
	}
	seconds, err := floorDivThousand(at)
	if err != nil {
		return nil, err
	}
	out := obj.Copy()
	if runes := []rune(text); len(runes) > 7 {
		text = string(runes[:7])
	}
	out.Set("sha", text)
	out.Set("at", seconds)
	return out, nil
}

// Whether an update is available without applying it, with install method, guidance and the commits behind
func checkHermesUpdate(ctx context.Context, force bool, profile *string) (*pyjson.Object, error) {
	if UpdateCLI.IsCommitBuild(ProjectRoot()) {
		version, err := derivedVersion()
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("install_method", "desktop-app", "current_version", version, "behind", nil,
			"update_available", false, "can_apply", false, "update_command", "", "message", UpdateCLI.CommitBuildMessage), nil
	}
	if dashboardLocalUpdateManagedExternally(ctx) {
		version, err := derivedVersion()
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("install_method", "managed-runtime", "current_version", version, "behind", nil,
			"update_available", false, "can_apply", false, "update_command", "", "message", managedExternallyMessage), nil
	}
	method, err := config.DetectInstallMethod(ctx, home.Dir(ctx), ProjectRoot())
	if err != nil {
		return nil, err
	}
	version, err := derivedVersion()
	if err != nil {
		return nil, err
	}
	payload := pyjson.ObjectOf("install_method", method, "current_version", version, "behind", nil,
		"update_available", false, "can_apply", method == "git" || method == "script" || method == "setup-app",
		"update_command", config.RecommendedUpdateCommandForMethod(method), "message", nil)
	if message, ok := nonApplyableMessage(method); ok {
		payload.Set("message", message)
		return payload, nil
	}
	status, behind := runUpdateCheck(ctx, force, profile)
	payload.Set("behind", behind)
	held, _ := status.Get("held")
	switch {
	case behind == nil:
		payload.Set("message", "Couldn't reach the update source — try again later.")
	case pyrepr.Equal(behind, int64(0)) && pyrepr.Truthy(held):
		sourceVersion, _ := status.Get("sourceVersion")
		payload.Set("held", held)
		payload.Set("source_version", sourceVersion)
		payload.Set("message", "This build is newer than "+sourceReleaseLabel(status)+", which predates the gateway "+
			"runtime its sessions use. It stays on this build until a newer release ships.")
	case pyrepr.Equal(behind, int64(0)):
		payload.Set("message", "You're on the latest version.")
	default:
		payload.Set("update_available", true)
		if rollback, _ := status.Get("rollback"); pyrepr.Truthy(rollback) {
			sourceVersion, _ := status.Get("sourceVersion")
			payload.Set("rollback_to", sourceVersion)
			payload.Set("message", "This build is newer than "+sourceReleaseLabel(status)+". "+
				"Updating moves it back to that release.")
		}
		channel, _ := status.Get("channel")
		branch, _ := status.Get("branch")
		sourceVersion, _ := status.Get("sourceVersion")
		payload.Set("channel", channel)
		payload.Set("branch", branch)
		payload.Set("source_version", sourceVersion)
		rows := []any{}
		if raw, ok := status.Get("commits"); ok {
			items, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("TypeError: '%s' object is not subscriptable", pyrepr.TypeName(raw))
			}
			rows = items
		}
		commits := make([]any, 0, min(len(rows), 20))
		for _, row := range rows[:min(len(rows), 20)] {
			commit, err := updateCommitRow(row)
			if err != nil {
				return nil, err
			}
			commits = append(commits, commit)
		}
		payload.Set("commits", commits)
	}
	return payload, nil
}

// Source check of the requested profile, behind nil when the check could not run
func runUpdateCheck(ctx context.Context, force bool, profile *string) (status *pyjson.Object, behind any) {
	status = pyjson.NewObject()
	failed := func(err error) (*pyjson.Object, any) {
		routersLog().Error("Update check failed", "exc_info", err)
		return status, nil
	}
	selected := ""
	if profile != nil {
		selected = *profile
	}
	sctx, _, err := configProfileScope(ctx, selected)
	if err != nil {
		return failed(err)
	}
	checked, err := UpdateCLI.CheckForUpdates(sctx, force)
	if err != nil {
		return failed(err)
	}
	if checked != nil {
		status = checked
	}
	behind, _ = status.Get("behind")
	return status, behind
}

// Exit code of an action with no live process, the in memory result else durable evidence
func completedExitCode(result *pyjson.Object, durableActionID string, receipt *pyjson.Object) any {
	if result != nil {
		code, _ := result.Get("exit_code")
		return code
	}
	if durableActionID != "" {
		return int64(0)
	}
	if receipt != nil {
		outcome, _ := receipt.Get("outcome")
		if pyrepr.Equal(outcome, "success") {
			return int64(0)
		}
		if pyrepr.Equal(outcome, "partial") {
			return int64(1)
		}
	}
	return nil
}

// Tail of an action log, whether its process still runs and, for an update, its durable outcome
func getActionStatus(ctx context.Context, name string, lines int64) (*pyjson.Object, error) {
	actionMu.Lock()
	logFileName, ok := actionLogFiles[name]
	actionMu.Unlock()
	if !ok {
		return nil, NewHTTPException(404, "Unknown action: "+name)
	}
	logDir := actionLogDir()
	requested := int(min(max(lines, 1), 2000))
	tail := tailLines(filepath.Join(logDir, logFileName), requested)
	durableID := ""
	var updateReceipt, actionReceipt *pyjson.Object
	if name == "hermes-update" {
		updateLog, _ := logging.LogFilePath(ctx, "update")
		durableID = durableCompletedUpdateActionID(tailLines(updateLog, 2000))
		spawnedID := latestSpawnedUpdateActionID(tailLines(filepath.Join(logDir, logFileName), 2000))
		if spawnedID == "" {
			spawnedID = persistedActionID(logDir, name)
		}
		if spawnedID == "" {
			actionMu.Lock()
			spawnedID = actionIDs[name]
			actionMu.Unlock()
		}
		if durableID != spawnedID {
			durableID = ""
		}
		if durableID != "" {
			marker := "=== icebath-update completed " + durableID + " ==="
			if !slices.Contains(tail, marker) {
				tail = append(tail, marker)
				if len(tail) > requested {
					tail = tail[len(tail)-requested:]
				}
			}
		}
		updateReceipt = latestUpdateReceiptSummary(ctx, spawnedID)
		if spawnedID != "" && updateReceipt != nil {
			if id, _ := updateReceipt.Get("action_id"); pyrepr.Equal(id, spawnedID) {
				actionReceipt = updateReceipt
			}
		}
	}
	var running bool
	var exitCode, pid any
	if proc := actionProcOf(name); proc == nil {
		actionMu.Lock()
		result := actionResults[name]
		actionMu.Unlock()
		if result != nil && result.Len() > 0 {
			pid, _ = result.Get("pid")
		}
		exitCode = completedExitCode(result, durableID, actionReceipt)
	} else {
		code := proc.Poll()
		running = code == nil
		p := proc.Pid()
		pid = int64(p)
		if code != nil {
			exitCode = int64(*code)
			finishAction(name, code, &p)
		}
	}
	lineValues := make([]any, len(tail))
	for i, line := range tail {
		lineValues[i] = line
	}
	response := pyjson.ObjectOf("name", name, "running", running, "exit_code", exitCode, "pid", pid, "lines", lineValues)
	if durableID != "" {
		response.Set("action_id", durableID)
	}
	if updateReceipt != nil {
		response.Set("receipt", updateReceipt)
	}
	return response, nil
}

// Dict value of a receipt key, an empty dict for a falsy value and false for a truthy non dict
func receiptMapping(receipt *pyjson.Object, key string) (*pyjson.Object, bool) {
	v, _ := receipt.Get(key)
	if !pyrepr.Truthy(v) {
		return pyjson.NewObject(), true
	}
	m, ok := v.(*pyjson.Object)
	return m, ok
}

// Dict entries of a receipt list key, false when the value cannot be iterated
func receiptDictEntries(receipt *pyjson.Object, key string) ([]*pyjson.Object, bool) {
	v, _ := receipt.Get(key)
	if !pyrepr.Truthy(v) {
		return nil, true
	}
	var out []*pyjson.Object
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if m, ok := item.(*pyjson.Object); ok {
				out = append(out, m)
			}
		}
		return out, true
	case string, *pyjson.Object:
		return nil, true
	}
	return nil, false
}

// Value of a key, nil when absent
func objectValue(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// Compact summary of the action's receipt when one exists, else of the latest receipt, nil when none or malformed
func latestUpdateReceiptSummary(ctx context.Context, actionID string) *pyjson.Object {
	var receipt *pyjson.Object
	if actionID != "" {
		receipt = updaterecord.ReceiptObjectForAction(ctx, actionID)
	}
	if receipt == nil || receipt.Len() == 0 {
		receipt = updaterecord.LatestReceiptObject(ctx)
	}
	if receipt == nil || receipt.Len() == 0 {
		return nil
	}
	post, ok := receiptMapping(receipt, "post_update")
	if !ok {
		return nil
	}
	pre, ok := receiptMapping(receipt, "pre_update")
	if !ok {
		return nil
	}
	fleet, ok := receiptDictEntries(receipt, "fleet")
	if !ok {
		return nil
	}
	followupRows, ok := receiptDictEntries(receipt, "followups")
	if !ok {
		return nil
	}
	states := map[string]bool{}
	for _, e := range fleet {
		states[pyrepr.Str(objectValue(e, "state"))] = true
	}
	sortedStates := make([]string, 0, len(states))
	for s := range states {
		sortedStates = append(sortedStates, s)
	}
	sort.Strings(sortedStates)
	fleetStates := make([]any, len(sortedStates))
	for i, s := range sortedStates {
		fleetStates[i] = s
	}
	followups := make([]any, 0, len(followupRows))
	for _, f := range followupRows {
		reason := ""
		if r := objectValue(f, "reason"); pyrepr.Truthy(r) {
			reason = pyrepr.Str(r)
		}
		followups = append(followups, pyjson.ObjectOf("step", pyrepr.Str(objectValue(f, "step")), "reason", reason))
	}
	var userAction any
	if ua, ok := objectValue(receipt, "user_action").(*pyjson.Object); ok {
		userAction = ua
	}
	summary := pyjson.NewObject()
	for _, k := range []string{"outcome", "started_at", "finished_at", "action_id"} {
		summary.Set(k, objectValue(receipt, k))
	}
	summary.Set("pre_sha", objectValue(pre, "sha"))
	summary.Set("post_sha", objectValue(post, "sha"))
	summary.Set("post_version", objectValue(post, "version"))
	summary.Set("fleet_states", fleetStates)
	summary.Set("followups", followups)
	summary.Set("user_action", userAction)
	return summary
}

// Full latest update receipt plus its summary, 404 when no update has run since receipts landed
func getUpdateReceipt(ctx context.Context) (*pyjson.Object, error) {
	receipt := updaterecord.LatestReceiptObject(ctx)
	if receipt == nil || receipt.Len() == 0 {
		return nil, NewHTTPException(404, "No update receipt found (no `icebath update` run recorded).")
	}
	return pyjson.ObjectOf("receipt", receipt, "summary", latestUpdateReceiptSummary(ctx, "")), nil
}

// Router of the gateway lifecycle and update actions
func actionsRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Post("/api/gateway/restart", &Endpoint{
		Name:   "restart_gateway",
		Params: []Param{QueryParam("profile", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return restartGateway(ctx, c.Str("profile")) },
	})
	r.Get("/api/gateway/migrate/plan", &Endpoint{
		Name:   "gateway_migrate_plan",
		Handle: func(ctx context.Context, c *Call) (any, error) { return gatewayMigratePlan(ctx) },
	})
	r.Post("/api/gateway/migrate", &Endpoint{
		Name:   "gateway_migrate",
		Handle: func(ctx context.Context, c *Call) (any, error) { return gatewayMigrate(ctx) },
	})
	r.Post("/api/gateway/drain", &Endpoint{
		Name:   "gateway_drain",
		Handle: func(ctx context.Context, c *Call) (any, error) { return gatewayDrain(ctx, c.Request) },
	})
	r.Post("/api/icebath/update", &Endpoint{
		Name:   "update_hermes",
		Handle: func(ctx context.Context, c *Call) (any, error) { return updateHermes(ctx) },
	})
	r.Get("/api/icebath/update/check", &Endpoint{
		Name:   "check_hermes_update",
		Params: []Param{QueryParam("force", Bool, false), QueryParam("profile", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			var profile *string
			if s, ok := c.Value("profile").(string); ok {
				profile = &s
			}
			return checkHermesUpdate(ctx, c.Bool("force"), profile)
		},
	})
	return r
}

// Router of the action status and update receipt reads
func actionsStatusRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/actions/{name}/status", &Endpoint{
		Name:   "get_action_status",
		Params: []Param{PathParam("name", Str), QueryParam("lines", Int, int64(200))},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getActionStatus(ctx, c.Str("name"), c.Int("lines"))
		},
	})
	r.Get("/api/icebath/update/receipt", &Endpoint{
		Name:   "get_update_receipt",
		Handle: func(ctx context.Context, c *Call) (any, error) { return getUpdateReceipt(ctx) },
	})
	return r
}
