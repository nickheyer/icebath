package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hidden entry restarting supervised gateway profiles and serve units from a fresh process
const UpdateRestartRecoveryEntry = "update-restart-recovery"

const (
	restartRecoveryEnv         = "ICEBATH_UPDATE_RESTART_RECOVERY"
	recoveryProfileTimeout     = 90 * time.Second
	recoveryVerifyTimeout      = 15 * time.Second
	recoveryUnitRestartTimeout = 60 * time.Second
	recoveryUnitSettleAttempts = 10
	recoveryUnitSettleDelay    = time.Second
)

var (
	recoveryGatewayMarkers   = []string{"_ICEBATH_GATEWAY", "ICEBATH_GATEWAY", "ICEBATH_GATEWAY_MODE"}
	recoverySupervisorRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	recoveryUnitRe           = regexp.MustCompile(`^icebath-(serve|dashboard)(-[a-z0-9][a-z0-9_-]{0,63})?\.service$`)
	recoveryServeUnitPattern = []string{"icebath-serve*", "icebath-dashboard*"}
	recoveryScopeLabels      = []string{"user", "system"}
)

// Finished recovery command with its exit status and decoded stdout
type recoveryCompleted struct {
	ReturnCode int
	Stdout     string
}

// Options of one recovery command, its environment, stdin and session detachment
type recoveryRunOptions struct {
	Env      []string
	Detached bool
	DevNull  bool
}

// Recovery command runner and settle sleep, replaced by hermes tests
var (
	recoveryRun   = runRecoveryCommand
	recoverySleep = time.Sleep
)

// Runs argv capturing text output, nil when it cannot start or outlives its timeout
func runRecoveryCommand(argv []string, timeout time.Duration, o recoveryRunOptions) *recoveryCompleted {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = o.Env
	if o.Detached {
		cmd.SysProcAttr = recoveryDetachedAttr()
	}
	if !o.DevNull {
		cmd.Stdin = os.Stdin
	}
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return nil
	}
	return &recoveryCompleted{ReturnCode: cmd.ProcessState.ExitCode(), Stdout: pystr.DecodeUTF8Replace(stdout.Bytes())}
}

func recoveryStdout(result *recoveryCompleted) string {
	if result == nil {
		return ""
	}
	return pystr.Strip(result.Stdout)
}

func recoverySucceeded(result *recoveryCompleted) bool {
	return result != nil && result.ReturnCode == 0
}

// Profile this recovery process was launched as, the profiles directory child of the home else default
func recoveryLaunchProfile() string {
	if h := pystr.Strip(os.Getenv(home.EnvHome)); h != "" {
		path := filepath.Clean(h)
		if filepath.Base(filepath.Dir(path)) == "profiles" {
			return filepath.Base(path)
		}
	}
	return "default"
}

// Environment a recovery child runs in, never identifying as the gateway owner nor carrying another profile's gates
func recoveryChildEnvironment(ctx context.Context, profile *string) ([]string, error) {
	launch := recoveryLaunchProfile()
	target := launch
	if profile != nil {
		target = *profile
	}
	var env map[string]string
	if target == "default" && launch != "default" {
		var err error
		if env, err = tools.HostGatewayChildEnv(ctx, nil, nil); err != nil {
			return nil, err
		}
	} else {
		env = map[string]string{}
		for _, kv := range os.Environ() {
			if key, value, ok := strings.Cut(kv, "="); ok {
				env[key] = value
			}
		}
		if profile != nil && *profile != launch {
			tools.StripProfileGateEnv(env)
		}
	}
	for _, marker := range recoveryGatewayMarkers {
		delete(env, marker)
	}
	env[restartRecoveryEnv] = "1"
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out, nil
}

// Restarts one profile gateway in a detached child that inherits none of the updater process state
func runProfileRestart(ctx context.Context, profile string) (bool, error) {
	env, err := recoveryChildEnvironment(ctx, &profile)
	if err != nil {
		return false, err
	}
	exe, err := runningExecutable()
	if err != nil {
		return false, err
	}
	argv := []string{exe, "-p", profile, "gateway", "restart", "--interrupt", "--detach"}
	return recoverySucceeded(recoveryRun(argv, recoveryProfileTimeout, recoveryRunOptions{Env: env, Detached: true, DevNull: true})), nil
}

// Unit names the systemd gateway lifecycle gives a profile
func systemdUnitCandidates(profile string) []string {
	if profile == "default" {
		return []string{"icebath-gateway.service", "gateway.service", "gateway-default.service"}
	}
	return []string{"icebath-gateway-" + profile + ".service", "gateway-" + profile + ".service"}
}

// True only when systemd itself reports the unit of a profile active
func systemdVerifiedActive(profile string) bool {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil || systemctl == "" {
		return false
	}
	for _, unit := range systemdUnitCandidates(profile) {
		if recoveryUnitIsActive([]string{systemctl, "--user"}, unit, true) {
			return true
		}
	}
	return false
}

// Gateway lock directory the host rendezvous resolves, computed without the gateway package
func recoveryHostStateDir() string {
	if override := os.Getenv("ICEBATH_GATEWAY_LOCK_DIR"); override != "" {
		return override
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(stateHome) {
		userHome, _ := os.UserHomeDir()
		stateHome = filepath.Join(userHome, ".local", "state")
	}
	return filepath.Join(stateHome, "icebath", "gateway-locks")
}

// Liveness of pid by the psutil probe, else the signal zero probe that Windows never trusts
func recoveryPidIsLive(pid int) bool {
	if live, err := proc.PidExists(pid); err == nil {
		return live
	}
	return proc.SignalProbeLive(pid)
}

// Profiles the one live host gateway multiplexes per its rendezvous record, empty when unprovable
func hostServedProfiles() map[string]bool {
	served := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(recoveryHostStateDir(), "host-gateway.json"))
	if err != nil {
		return served
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return served
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return served
	}
	record, ok := decoded.(*pyjson.Object)
	if !ok || record.GetOr("role", nil) != "gateway" {
		return served
	}
	pid, isInt := pyIntValue(record.GetOr("pid", nil))
	profiles, isList := record.GetOr("profiles", nil).([]any)
	if !isInt || pid <= 0 || !isList || !recoveryPidIsLive(int(pid)) {
		return served
	}
	for _, name := range profiles {
		if text, ok := name.(string); ok && text != "" {
			served[text] = true
		}
	}
	return served
}

// Restarts the requested supervised profiles once per host gateway process and buckets each outcome
func restartProfiles(ctx context.Context, profiles []string, supervisors map[string]string) (*pyjson.Object, error) {
	verified, attempted, failed := []any{}, []any{}, []any{}
	seen := map[string]bool{}
	requested := []string{}
	for _, p := range profiles {
		if p != "" && !seen[p] {
			seen[p] = true
			requested = append(requested, p)
		}
	}
	sort.Strings(requested)
	served := hostServedProfiles()
	var group []string
	for _, p := range requested {
		if served[p] {
			group = append(group, p)
		}
	}
	representative := ""
	var covered []string
	if len(group) > 1 {
		representative, covered = group[0], group[1:]
	}
	for _, profile := range requested {
		if slices.Contains(covered, profile) {
			continue
		}
		ok, err := runProfileRestart(ctx, profile)
		if err != nil {
			return nil, err
		}
		bucket := &attempted
		switch {
		case !ok:
			bucket = &failed
		case supervisors[profile] == "systemd" && systemdVerifiedActive(profile):
			bucket = &verified
		}
		*bucket = append(*bucket, profile)
		if profile == representative {
			for _, other := range covered {
				*bucket = append(*bucket, other)
			}
		}
	}
	coveredMap := pyjson.NewObject()
	if representative != "" {
		rows := make([]any, 0, len(covered))
		for _, other := range covered {
			rows = append(rows, other)
		}
		coveredMap.Set(representative, rows)
	}
	return pyjson.ObjectOf("verified", verified, "relaunch_attempted", attempted, "failed", failed, "covered", coveredMap), nil
}

// Labelled systemctl argvs of the user and system scopes, none off Linux or without systemctl
func systemctlScopes() [][2]any {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil || systemctl == "" || runtime.GOOS != "linux" {
		return nil
	}
	return [][2]any{{"user", []string{systemctl, "--user"}}, {"system", []string{systemctl}}}
}

// Serve and dashboard units systemd lists in one scope, validated by exact name
func listedServeUnits(scope []string) []string {
	var units []string
	for _, pattern := range recoveryServeUnitPattern {
		result := recoveryRun(append(slices.Clone(scope), "list-units", pattern, "--plain", "--no-legend", "--no-pager"), recoveryVerifyTimeout, recoveryRunOptions{})
		if result == nil {
			continue
		}
		for _, line := range pystr.Splitlines(result.Stdout, false) {
			parts := pystr.SplitWhitespace(line, -1)
			if len(parts) > 0 && recoveryUnitRe.MatchString(parts[0]) && !slices.Contains(units, parts[0]) {
				units = append(units, parts[0])
			}
		}
	}
	return units
}

// Main pid systemd reports for a unit, zero when absent or unreadable
func recoveryUnitMainPid(scope []string, unit string) int {
	result := recoveryRun(append(slices.Clone(scope), "show", unit, "--property=MainPID", "--value"), recoveryVerifyTimeout, recoveryRunOptions{})
	if !recoverySucceeded(result) {
		return 0
	}
	text := recoveryStdout(result)
	if text == "" {
		return 0
	}
	n, ok := pyint.Parse(text)
	if !ok || !n.IsInt64() {
		return 0
	}
	return int(n.Int64())
}

// True when systemd reports the unit active, requiring a zero exit when asked
func recoveryUnitIsActive(scope []string, unit string, requireRC0 bool) bool {
	result := recoveryRun(append(slices.Clone(scope), "is-active", unit), recoveryVerifyTimeout, recoveryRunOptions{})
	if result == nil || (requireRC0 && !recoverySucceeded(result)) {
		return false
	}
	return recoveryStdout(result) == "active"
}

// True once an active unit runs a main pid other than the one before its restart
func serveUnitReplaced(scope []string, unit string, previousPid int) bool {
	for attempt := range recoveryUnitSettleAttempts {
		if attempt > 0 {
			recoverySleep(recoveryUnitSettleDelay)
		}
		if recoveryUnitIsActive(scope, unit, false) {
			if current := recoveryUnitMainPid(scope, unit); current > 0 && current != previousPid {
				return true
			}
		}
	}
	return false
}

// Scope label and base unit of a skip entry in its mapping or scope slash unit shape
func splitSkipEntry(entry any) (string, string) {
	var scope, unit string
	if obj, ok := pyjson.AsObject(entry); ok {
		if v := obj.GetOr("scope", nil); pyrepr.Truthy(v) {
			scope = pyrepr.Str(v)
		}
		if v := obj.GetOr("unit", nil); pyrepr.Truthy(v) {
			unit = pyrepr.Str(v)
		}
	} else {
		var sep bool
		scope, unit, sep = strings.Cut(pyrepr.Str(entry), "/")
		if !sep {
			scope, unit = "", scope
		}
	}
	return scope, strings.TrimSuffix(unit, ".service")
}

// Settled units split into scope qualified and legacy bare entries
func normalizedSkips(skipUnits []any) (map[[2]string]bool, map[string]bool) {
	qualified, legacy := map[[2]string]bool{}, map[string]bool{}
	for _, entry := range skipUnits {
		scope, base := splitSkipEntry(entry)
		if base == "" {
			continue
		}
		if slices.Contains(recoveryScopeLabels, scope) {
			qualified[[2]string{scope, base}] = true
		} else {
			legacy[base] = true
		}
	}
	return qualified, legacy
}

// Restarts every active serve and dashboard unit systemd lists and proves each came back on a new pid
func restartServeUnits(skipUnits []any) *pyjson.Object {
	skippedQualified, skippedLegacy := normalizedSkips(skipUnits)
	outcomes := map[[2]string]bool{}
	seen := map[[2]string]bool{}
	for _, pair := range systemctlScopes() {
		label, scope := pair[0].(string), pair[1].([]string)
		for _, unit := range listedServeUnits(scope) {
			base := strings.TrimSuffix(unit, ".service")
			target := [2]string{label, base}
			if seen[target] || skippedQualified[target] || skippedLegacy[base] {
				continue
			}
			seen[target] = true
			if !recoveryUnitIsActive(scope, unit, false) {
				continue
			}
			previous := recoveryUnitMainPid(scope, unit)
			outcomes[target] = previous > 0 &&
				recoverySucceeded(recoveryRun(append(slices.Clone(scope), "--no-ask-password", "restart", unit), recoveryUnitRestartTimeout, recoveryRunOptions{})) &&
				serveUnitReplaced(scope, unit, previous)
		}
	}
	var verified, failed []string
	for target, ok := range outcomes {
		name := target[0] + "/" + target[1]
		if ok {
			verified = append(verified, name)
		} else {
			failed = append(failed, name)
		}
	}
	sort.Strings(verified)
	sort.Strings(failed)
	return pyjson.ObjectOf("verified", stringsAny(verified), "failed", stringsAny(failed))
}

func stringsAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

// Validated profiles, supervisors, serve flag and skip units of a recovery payload, a ValueError text when malformed
func parseRecoveryPayload(text string) ([]string, map[string]string, bool, []any, error) {
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil, false, nil, err
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		payload = pyjson.NewObject()
	}
	rawProfiles, ok := payload.GetOr("profiles", nil).([]any)
	if !ok {
		return nil, nil, false, nil, &pm.PyError{Class: "ValueError", Message: "recovery payload must contain a profiles list"}
	}
	profiles := make([]string, 0, len(rawProfiles))
	for _, p := range rawProfiles {
		name, isStr := p.(string)
		if !isStr || !home.ProfileIDRe.MatchString(name) {
			return nil, nil, false, nil, &pm.PyError{Class: "ValueError", Message: "recovery profiles contain an invalid profile id"}
		}
		profiles = append(profiles, name)
	}
	supervisors := map[string]string{}
	if raw := payload.GetOr("supervisors", nil); raw != nil {
		obj, isObj := raw.(*pyjson.Object)
		if !isObj {
			return nil, nil, false, nil, &pm.PyError{Class: "ValueError", Message: "recovery supervisors map is invalid"}
		}
		for _, profile := range obj.Keys {
			supervisor, isStr := obj.Vals[profile].(string)
			if !isStr || !home.ProfileIDRe.MatchString(profile) || !recoverySupervisorRe.MatchString(supervisor) {
				return nil, nil, false, nil, &pm.PyError{Class: "ValueError", Message: "recovery supervisors map is invalid"}
			}
			supervisors[profile] = supervisor
		}
	}
	recoverServe := false
	skipUnits := []any{}
	if raw := payload.GetOr("serve_units", nil); raw != nil {
		block, isObj := raw.(*pyjson.Object)
		if !isObj {
			return nil, nil, false, nil, &pm.PyError{Class: "ValueError", Message: "recovery serve_units block is invalid"}
		}
		recoverServe = pyrepr.Truthy(block.GetOr("recover", nil))
		rawSkip := block.GetOr("skip", nil)
		if !pyrepr.Truthy(rawSkip) {
			rawSkip = []any{}
		}
		skips, isList := rawSkip.([]any)
		if !isList {
			return nil, nil, false, nil, &pm.PyError{Class: "ValueError", Message: "recovery serve_units skip list is invalid"}
		}
		for _, entry := range skips {
			if _, isStr := entry.(string); !isStr {
				if _, isObj := entry.(*pyjson.Object); !isObj {
					return nil, nil, false, nil, &pm.PyError{Class: "ValueError", Message: "recovery serve_units skip list is invalid"}
				}
			}
		}
		for _, entry := range skips {
			if obj, isObj := entry.(*pyjson.Object); isObj {
				unit, unitIsStr := obj.GetOr("unit", nil).(string)
				if !unitIsStr {
					return nil, nil, false, nil, &pm.PyError{Class: "ValueError", Message: "recovery serve_units skip list is invalid"}
				}
				if _, scopeIsStr := obj.GetOr("scope", nil).(string); !scopeIsStr {
					entry = pyjson.ObjectOf("unit", unit)
				}
			}
			scope, base := splitSkipEntry(entry)
			if (scope != "" && !slices.Contains(recoveryScopeLabels, scope)) || !recoveryUnitRe.MatchString(base+".service") {
				continue
			}
			if scope != "" {
				skipUnits = append(skipUnits, scope+"/"+base)
			} else {
				skipUnits = append(skipUnits, base)
			}
		}
	}
	return profiles, supervisors, recoverServe, skipUnits, nil
}

// Restarts the payload's profiles and serve units and prints the outcome JSON, the child exit status
func UpdateRestartRecoveryMain(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	prog := "icebath " + UpdateRestartRecoveryEntry
	usage := "usage: " + prog + " [-h]\n"
	stdinFlag := false
	var unknown []string
	for _, arg := range args {
		switch arg {
		case "--stdin":
			stdinFlag = true
		case "-h", "--help":
			fmt.Fprint(stdout, usage)
			return 0
		default:
			unknown = append(unknown, arg)
		}
	}
	if len(unknown) > 0 {
		fmt.Fprintf(stderr, "%s%s: error: unrecognized arguments: %s\n", usage, prog, strings.Join(unknown, " "))
		return 2
	}
	if !stdinFlag {
		fmt.Fprintf(stderr, "%s%s: error: this command is an internal update-recovery entry point\n", usage, prog)
		return 2
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", launchErrorClass(err), launchErrorText(err))
		return 1
	}
	text, err := pystr.DecodeUTF8(raw)
	var result *pyjson.Object
	var profiles []string
	var supervisors map[string]string
	var recoverServe bool
	var skipUnits []any
	if err == nil {
		profiles, supervisors, recoverServe, skipUnits, err = parseRecoveryPayload(text)
	}
	if err == nil {
		result, err = restartProfiles(ctx, profiles, supervisors)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", launchErrorClass(err), launchErrorText(err))
			return 1
		}
		if recoverServe {
			result.Set("serve_units", restartServeUnits(skipUnits))
		} else {
			result.Set("serve_units", pyjson.ObjectOf("verified", []any{}, "failed", []any{}))
		}
	}
	if err != nil {
		body, _ := pyjson.Dumps(pyjson.ObjectOf("error", err.Error(), "verified", []any{}, "relaunch_attempted", []any{},
			"failed", []any{}, "serve_units", pyjson.ObjectOf("verified", []any{}, "failed", []any{})))
		fmt.Fprintln(stdout, body)
		return 2
	}
	body, _ := pyjson.Dumps(result, pyjson.SortKeys(true))
	fmt.Fprintln(stdout, body)
	serveFailed := rowValue(result.GetOr("serve_units", nil), "failed")
	if pyrepr.Truthy(result.GetOr("failed", nil)) || pyrepr.Truthy(serveFailed) {
		return 1
	}
	return 0
}
