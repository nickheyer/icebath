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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hidden entry restarting supervised gateway profiles from a fresh process
const UpdateRestartRecoveryEntry = "update-restart-recovery"

const (
	restartRecoveryEnv     = "ICEBATH_UPDATE_RESTART_RECOVERY"
	recoveryProfileTimeout = 90 * time.Second
	recoveryVerifyTimeout  = 15 * time.Second
)

var (
	recoveryGatewayMarkers = []string{"_ICEBATH_GATEWAY", "ICEBATH_GATEWAY", "ICEBATH_GATEWAY_MODE"}
	recoverySupervisorRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
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

// Recovery command runner, replaced by tests
var recoveryRun = runRecoveryCommand

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
		if recoveryUnitIsActive([]string{systemctl, "--user"}, unit) {
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

// True when systemd exits zero reporting the unit active
func recoveryUnitIsActive(scope []string, unit string) bool {
	result := recoveryRun(append(slices.Clone(scope), "is-active", unit), recoveryVerifyTimeout, recoveryRunOptions{})
	return recoverySucceeded(result) && recoveryStdout(result) == "active"
}

func stringsAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

// Validated profiles and supervisors of a recovery payload, a ValueError text when malformed
func parseRecoveryPayload(text string) ([]string, map[string]string, error) {
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, nil, err
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		payload = pyjson.NewObject()
	}
	rawProfiles, ok := payload.GetOr("profiles", nil).([]any)
	if !ok {
		return nil, nil, &pm.PyError{Class: "ValueError", Message: "recovery payload must contain a profiles list"}
	}
	profiles := make([]string, 0, len(rawProfiles))
	for _, p := range rawProfiles {
		name, isStr := p.(string)
		if !isStr || !home.ProfileIDRe.MatchString(name) {
			return nil, nil, &pm.PyError{Class: "ValueError", Message: "recovery profiles contain an invalid profile id"}
		}
		profiles = append(profiles, name)
	}
	supervisors := map[string]string{}
	if raw := payload.GetOr("supervisors", nil); raw != nil {
		obj, isObj := raw.(*pyjson.Object)
		if !isObj {
			return nil, nil, &pm.PyError{Class: "ValueError", Message: "recovery supervisors map is invalid"}
		}
		for _, profile := range obj.Keys {
			supervisor, isStr := obj.Vals[profile].(string)
			if !isStr || !home.ProfileIDRe.MatchString(profile) || !recoverySupervisorRe.MatchString(supervisor) {
				return nil, nil, &pm.PyError{Class: "ValueError", Message: "recovery supervisors map is invalid"}
			}
			supervisors[profile] = supervisor
		}
	}
	return profiles, supervisors, nil
}

// Restarts the payload's profiles and prints the outcome JSON, the child exit status
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
	if err == nil {
		profiles, supervisors, err = parseRecoveryPayload(text)
	}
	if err == nil {
		result, err = restartProfiles(ctx, profiles, supervisors)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", launchErrorClass(err), launchErrorText(err))
			return 1
		}
	}
	if err != nil {
		body, _ := pyjson.Dumps(pyjson.ObjectOf("error", err.Error(), "verified", []any{}, "relaunch_attempted", []any{}, "failed", []any{}))
		fmt.Fprintln(stdout, body)
		return 2
	}
	body, _ := pyjson.Dumps(result, pyjson.SortKeys(true))
	fmt.Fprintln(stdout, body)
	if pyrepr.Truthy(result.GetOr("failed", nil)) {
		return 1
	}
	return 0
}
