//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Fresh recovery process an aborted restart phase consults, replaced by tests
var (
	abortRunFreshRecovery = runFreshRecoveryProcess
	abortRecoveryCapture  = recoveryCapture
)

// Runs the restart recovery entry detached from the updater, nil when it could not run or outlived its budget
func runFreshRecoveryProcess(ctx context.Context, profiles []string, candidates map[string]string, gatewayMode bool) *recoveryCompleted {
	exe, err := runningExecutable()
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Fresh gateway restart recovery failed: %s", launchErrorText(err)))
		return nil
	}
	command := []string{exe, UpdateRestartRecoveryEntry, "--stdin"}
	env := []string{}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(recoveryGatewayMarkers, key) || key == restartRecoveryEnv {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, restartRecoveryEnv+"=1")
	if gatewayMode && runtime.GOOS == "linux" {
		systemdRun, err := exec.LookPath("systemd-run")
		if err != nil || systemdRun == "" {
			pkgLog().Warn("Cannot isolate fresh gateway recovery from the gateway cgroup")
			return nil
		}
		command = append([]string{systemdRun, "--user", "--scope", "--quiet", "--collect", "--"}, command...)
	}
	supervisors := pyjson.NewObject()
	for _, profile := range profiles {
		supervisors.Set(profile, candidates[profile])
	}
	payload, err := pyjson.Dumps(pyjson.ObjectOf("profiles", stringsAny(profiles), "supervisors", supervisors))
	if err != nil {
		return nil
	}
	timeout := time.Duration(max(180, 30+90*len(profiles))) * time.Second
	child := exec.CommandContext(ctx, command[0], command[1:]...)
	child.Env = env
	child.SysProcAttr = recoveryDetachedAttr()
	child.Stdin = strings.NewReader(payload)
	result, err := abortRecoveryCapture(child, timeout)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Fresh gateway restart recovery failed: %s", launchErrorText(err)))
		return nil
	}
	return result
}

// Result of a fresh recovery that requested profiles, the per bucket profiles and the skips
func recoveryOutcome(requested, verified, attempted, failed []string, skipped []*pyjson.Object) map[string]any {
	skippedRows := make([]any, 0, len(skipped))
	for _, row := range skipped {
		skippedRows = append(skippedRows, row)
	}
	return map[string]any{"requested": requested, "verified": verified, "relaunch_attempted": attempted,
		"failed": failed, "skipped": skippedRows}
}

// Retries supervised gateway restarts in a fresh process and returns its fail closed result
func recoverGatewayRestartAfterAbort(ctx context.Context, w io.Writer, plan *UpdatePlan, gatewayMode bool, skipProfiles map[string]bool) (map[string]any, error) {
	candidates, skipped := gatewayRecoveryPartition(plan, skipProfiles)
	profiles := slices.Clone(candidates.Profiles)
	sort.Strings(profiles)
	if len(profiles) == 0 {
		return recoveryOutcome([]string{}, []string{}, []string{}, []string{}, skipped), nil
	}
	allFailed := func() map[string]any {
		return recoveryOutcome(profiles, []string{}, []string{}, slices.Clone(profiles), skipped)
	}
	result := abortRunFreshRecovery(ctx, profiles, candidates.Supervisors, gatewayMode)
	if result == nil {
		return allFailed(), nil
	}
	if result.ReturnCode != 0 {
		pkgLog().Warn(fmt.Sprintf("Fresh gateway restart recovery exited %d", result.ReturnCode))
		return allFailed(), nil
	}
	decoded, err := pyjson.Loads(result.Stdout)
	recovery, isObj := decoded.(*pyjson.Object)
	if err != nil || !isObj {
		pkgLog().Warn("Fresh gateway restart recovery returned invalid JSON")
		return allFailed(), nil
	}
	buckets := [3][]string{}
	valid := true
	for i, key := range []string{"verified", "relaunch_attempted", "failed"} {
		list, ok := recovery.GetOr(key, nil).([]any)
		if !ok {
			valid = false
			break
		}
		for _, profile := range list {
			text, isStr := profile.(string)
			if !isStr {
				valid = false
				break
			}
			buckets[i] = append(buckets[i], text)
		}
	}
	if valid {
		reported := append(append(slices.Clone(buckets[0]), buckets[1]...), buckets[2]...)
		unique := map[string]bool{}
		for _, profile := range reported {
			unique[profile] = true
		}
		want := map[string]bool{}
		for _, profile := range profiles {
			want[profile] = true
		}
		valid = len(unique) == len(reported) && len(unique) == len(want)
		for profile := range unique {
			valid = valid && want[profile]
		}
	}
	if !valid {
		pkgLog().Warn("Fresh gateway restart recovery returned incomplete profiles")
		return allFailed(), nil
	}
	for i := range buckets {
		if buckets[i] == nil {
			buckets[i] = []string{}
		}
		sort.Strings(buckets[i])
	}
	if covered, ok := recovery.GetOr("covered", nil).(*pyjson.Object); ok {
		for _, owner := range covered.Keys {
			others, isList := covered.Vals[owner].([]any)
			if !isList || len(others) == 0 {
				continue
			}
			names := make([]string, 0, len(others))
			for _, other := range others {
				names = append(names, pyrepr.Str(other))
			}
			fmt.Fprintf(w, "  • One host gateway serves %s and %s — restarted once through %s; that restart is their outcome.\n",
				owner, strings.Join(names, ", "), owner)
		}
	}
	for _, line := range []struct {
		names []string
		text  string
	}{
		{buckets[0], "  ✓ Restarted supervised gateway(s) in a fresh process (systemd-verified active): "},
		{buckets[1], "  ⚠ Relaunch attempted in a fresh process but not supervisor-verified (check these gateways manually): "},
	} {
		if len(line.names) > 0 {
			fmt.Fprintln(w, line.text+strings.Join(line.names, ", "))
		}
	}
	return recoveryOutcome(profiles, buckets[0], buckets[1], buckets[2], skipped), nil
}

// True only when every planned gateway is covered and no failed or unverified recovery bucket remains
func abortRecoveryIsComplete(plannedGatewayProfiles, coveredGatewayProfiles map[string]bool, recoveryResult map[string]any) bool {
	if len(plannedGatewayProfiles) == 0 {
		return false
	}
	for profile := range plannedGatewayProfiles {
		if !coveredGatewayProfiles[profile] {
			return false
		}
	}
	return !pyrepr.Truthy(recoveryResult["failed"]) && !pyrepr.Truthy(recoveryResult["relaunch_attempted"])
}

// Error a recovery child that outlived its budget stands for, the subprocess TimeoutExpired
type recoveryTimeout struct {
	argv    []string
	seconds float64
}

func (e *recoveryTimeout) Error() string {
	return fmt.Sprintf("%s timed out after %s seconds", strings.Join(e.argv, " "), errchain.Show(e.seconds))
}

// Runs a prepared child capturing stdout as replaced text, killing it past the timeout
func recoveryCapture(child *exec.Cmd, timeout time.Duration) (*recoveryCompleted, error) {
	var stdout strings.Builder
	child.Stdout, child.Stderr = &stdout, io.Discard
	if err := child.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			return nil, err
		}
	case <-time.After(timeout):
		_ = child.Process.Kill()
		<-done
		return nil, &recoveryTimeout{argv: child.Args, seconds: timeout.Seconds()}
	}
	return &recoveryCompleted{ReturnCode: child.ProcessState.ExitCode(), Stdout: pystr.DecodeUTF8Replace([]byte(stdout.String()))}, nil
}
