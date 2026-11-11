package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Ledger rows and the fresh recovery process an aborted restart phase consults, replaced by hermes tests
var (
	abortLedgerEntries    = func() []map[string]any { return tools.LedgerEntries(tools.LedgerOptions{}) }
	abortRunFreshRecovery = runFreshRecoveryProcess
	abortServeRecovery    = serveUnitRecoveryAvailable
)

// True when a fresh process can restart serve and dashboard units on this host
func serveUnitRecoveryAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	path, err := exec.LookPath("systemctl")
	return err == nil && path != ""
}

// Number of a JSON value an int or float stands for, false for anything else
func abortNumeric(value any) (float64, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case float64:
		return v, true
	}
	if n, ok := pyIntValue(value); ok {
		return float64(n), true
	}
	return 0, false
}

// Planned serve and dashboard runtimes still on their pre update incarnation, every missing evidence counting as surviving
func survivingPreUpdateServeRuntimes(ctx context.Context, plan *UpdatePlan) ([]any, error) {
	type plannedRow struct {
		row        *pyjson.Object
		created    float64
		hasCreated bool
	}
	planned := map[int]*plannedRow{}
	if plan != nil {
		for _, rt := range plan.Runtimes {
			if rt == nil || (rt.Kind != "serve" && rt.Kind != "dashboard") || rt.PID == nil || *rt.PID <= 0 {
				continue
			}
			pid := *rt.PID
			detail := rt.Detail
			if detail == nil {
				detail = pyjson.NewObject()
			}
			created, has := abortNumeric(detail.GetOr("create_time", nil))
			row := pyjson.ObjectOf("pid", pid, "kind", rt.Kind, "profile", rt.Profile, "supervisor", rt.Supervisor)
			if unit := detail.GetOr("systemd_unit", nil); pyrepr.Truthy(unit) {
				row.Set("systemd_unit", pyrepr.Str(unit))
				row.Set("systemd_scope", detail.GetOr("systemd_scope", nil))
			}
			planned[pid] = &plannedRow{row: row, created: created, hasCreated: has}
		}
	}
	if len(planned) == 0 {
		return []any{}, nil
	}
	type liveRow struct {
		created    float64
		hasCreated bool
	}
	live := map[int]liveRow{}
	for _, entry := range abortLedgerEntries() {
		purpose := entry["purpose"]
		if purpose != "serve" && purpose != "dashboard" {
			continue
		}
		if _, isBool := entry["pid"].(bool); isBool {
			continue
		}
		pid, ok := pyIntValue(entry["pid"])
		if !ok {
			continue
		}
		created, has := abortNumeric(entry["create_time"])
		live[int(pid)] = liveRow{created: created, hasCreated: has}
	}
	pids := make([]int, 0, len(planned))
	for pid := range planned {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	survivors := []any{}
	for _, pid := range pids {
		row := planned[pid]
		current, present := live[pid]
		if !present {
			continue
		}
		if row.hasCreated && current.hasCreated && math.Abs(current.created-row.created) >= 2.0 {
			continue
		}
		survivors = append(survivors, row.row)
	}
	return survivors, nil
}

// Settled units qualified by scope so a unit of one manager never suppresses the other
func qualifiedServeSkips(skipUnits map[string]bool) []any {
	entries := make([]string, 0, len(skipUnits))
	for entry := range skipUnits {
		entries = append(entries, entry)
	}
	sort.Strings(entries)
	rows := []any{}
	for _, entry := range entries {
		scope, unit, sep := strings.Cut(entry, "/")
		switch {
		case sep && (scope == "user" || scope == "system") && unit != "":
			rows = append(rows, pyjson.ObjectOf("scope", scope, "unit", unit))
		case entry != "":
			rows = append(rows, pyjson.ObjectOf("unit", entry))
		}
	}
	return rows
}

// Runs the restart recovery entry detached from the updater, nil when it could not run or outlived its budget
func runFreshRecoveryProcess(ctx context.Context, profiles []string, candidates map[string]string, gatewayMode, recoverServe bool, skipUnits map[string]bool) *recoveryCompleted {
	exe, err := runningExecutable()
	if err != nil {
		slog.Warn(fmt.Sprintf("Fresh gateway restart recovery failed: %s", launchErrorText(err)))
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
			slog.Warn("Cannot isolate fresh gateway recovery from the gateway cgroup")
			return nil
		}
		command = append([]string{systemdRun, "--user", "--scope", "--quiet", "--collect", "--"}, command...)
	}
	supervisors := pyjson.NewObject()
	for _, profile := range profiles {
		supervisors.Set(profile, candidates[profile])
	}
	payload, err := pyjson.Dumps(pyjson.ObjectOf("profiles", stringsAny(profiles), "supervisors", supervisors,
		"serve_units", pyjson.ObjectOf("recover", recoverServe, "skip", qualifiedServeSkips(skipUnits))))
	if err != nil {
		return nil
	}
	budget := 30 + 90*len(profiles)
	if recoverServe {
		budget += 150
	}
	timeout := time.Duration(max(180, budget)) * time.Second
	child := exec.CommandContext(ctx, command[0], command[1:]...)
	child.Env = env
	child.SysProcAttr = recoveryDetachedAttr()
	child.Stdin = strings.NewReader(payload)
	result, err := recoveryCapture(child, timeout)
	if err != nil {
		slog.Warn(fmt.Sprintf("Fresh gateway restart recovery failed: %s", launchErrorText(err)))
		return nil
	}
	return result
}

// Result of a fresh recovery that requested profiles, the per bucket profiles, skips and serve units
func recoveryOutcome(requested, verified, attempted, failed []string, skipped []*pyjson.Object, serveUnits map[string]any) map[string]any {
	if serveUnits == nil {
		serveUnits = map[string]any{"verified": []string{}, "failed": []string{}}
	}
	skippedRows := make([]any, 0, len(skipped))
	for _, row := range skipped {
		skippedRows = append(skippedRows, row)
	}
	return map[string]any{"requested": requested, "verified": verified, "relaunch_attempted": attempted,
		"failed": failed, "skipped": skippedRows, "serve_units": serveUnits}
}

// Verified and failed serve units of the child result, an unreadable block failing when serve recovery ran
func parseServeUnits(raw any, recoverServe bool) map[string]any {
	if block, ok := raw.(*pyjson.Object); ok {
		verified, vok := block.GetOr("verified", nil).([]any)
		failed, fok := block.GetOr("failed", nil).([]any)
		if vok && fok {
			names := map[string][]string{}
			valid := true
			for key, list := range map[string][]any{"verified": verified, "failed": failed} {
				for _, unit := range list {
					text, isStr := unit.(string)
					if !isStr {
						valid = false
						break
					}
					names[key] = append(names[key], text)
				}
			}
			if valid {
				v, f := append([]string{}, names["verified"]...), append([]string{}, names["failed"]...)
				sort.Strings(v)
				sort.Strings(f)
				return map[string]any{"verified": v, "failed": f}
			}
		}
	}
	if recoverServe {
		slog.Warn("Fresh recovery returned an invalid serve-unit result")
		return map[string]any{"verified": []string{}, "failed": []string{"<unreadable>"}}
	}
	return map[string]any{"verified": []string{}, "failed": []string{}}
}

// Retries supervised gateway and serve unit restarts in a fresh process and returns its fail closed result
func recoverGatewayRestartAfterAbort(ctx context.Context, w io.Writer, plan *UpdatePlan, gatewayMode bool, skipProfiles, skipUnits map[string]bool) (map[string]any, error) {
	candidates, skipped := gatewayRecoveryPartition(plan, skipProfiles)
	profiles := slices.Clone(candidates.Profiles)
	sort.Strings(profiles)
	recoverServe := abortServeRecovery()
	if len(profiles) == 0 && !recoverServe {
		return recoveryOutcome([]string{}, []string{}, []string{}, []string{}, skipped, nil), nil
	}
	allFailed := func() map[string]any {
		return recoveryOutcome(profiles, []string{}, []string{}, slices.Clone(profiles), skipped, nil)
	}
	result := abortRunFreshRecovery(ctx, profiles, candidates.Supervisors, gatewayMode, recoverServe, skipUnits)
	if result == nil {
		return allFailed(), nil
	}
	if result.ReturnCode != 0 {
		slog.Warn(fmt.Sprintf("Fresh gateway restart recovery exited %d", result.ReturnCode))
		return allFailed(), nil
	}
	decoded, err := pyjson.Loads(result.Stdout)
	recovery, isObj := decoded.(*pyjson.Object)
	if err != nil || !isObj {
		slog.Warn("Fresh gateway restart recovery returned invalid JSON")
		return allFailed(), nil
	}
	rawServe := recovery.GetOr("serve_units", nil)
	if !pyrepr.Truthy(rawServe) {
		rawServe = pyjson.ObjectOf("verified", []any{}, "failed", []any{})
	}
	serveUnits := parseServeUnits(rawServe, recoverServe)
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
		slog.Warn("Fresh gateway restart recovery returned incomplete profiles")
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
		{serveUnits["verified"].([]string), "  ✓ Restarted serve unit(s) in a fresh process (new main PID observed): "},
		{serveUnits["failed"].([]string), "  ⚠ Could not verify a replacement for serve unit(s): "},
	} {
		if len(line.names) > 0 {
			fmt.Fprintln(w, line.text+strings.Join(line.names, ", "))
		}
	}
	return recoveryOutcome(profiles, buckets[0], buckets[1], buckets[2], skipped, serveUnits), nil
}

// Names each serve or dashboard process still on pre update code with how its owner restarts it
func warnStaleServeRuntimes(w io.Writer, rows []any) error {
	if len(rows) == 0 {
		return nil
	}
	fmt.Fprintln(w, "  ⚠ These serve/dashboard processes still run pre-update code (they started before the checkout changed):")
	text := func(row any, key, fallback string) string {
		if v := rowValue(row, key); pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
		return fallback
	}
	var unitRows []any
	for _, row := range rows {
		fmt.Fprintf(w, "      pid %s — %s (profile %s, %s)\n", pyrepr.Str(rowValue(row, "pid")), pyrepr.Str(rowValue(row, "kind")),
			text(row, "profile", "default"), text(row, "supervisor", "unknown"))
		if rowValue(row, "supervisor") == "systemd" {
			unitRows = append(unitRows, row)
		}
	}
	if len(unitRows) < len(rows) {
		fmt.Fprintln(w, "    Ask their owner to relaunch `icebath serve` / `icebath dashboard`, or reconnect Desktop for an SSH backend.")
	}
	for _, row := range unitRows {
		commands, err := gateway.SystemdRestartCommands(text(row, "systemd_unit", "<unit>"), pyjson.Plain(rowValue(row, "systemd_scope")))
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "    pid %s is managed by systemd: %s\n", pyrepr.Str(rowValue(row, "pid")), gateway.FormatRestartCommands(commands))
	}
	if runtime.GOOS == "darwin" {
		for _, row := range rows {
			if rowValue(row, "supervisor") == "launchd" {
				fmt.Fprintln(w, "    For launchd-managed backends: `launchctl kickstart -k gui/$UID/<label>`.")
				break
			}
		}
	}
	return nil
}

// Stale serve rows the updater owes a restart for, never a client owned serve
func owedStaleServeRows(rows []any) []any {
	out := []any{}
	for _, row := range rows {
		if supervisor, ok := rowValue(row, "supervisor").(string); ok && ClientOwnedServeSupervisors[supervisor] {
			continue
		}
		out = append(out, row)
	}
	return out
}

// True only when every planned gateway is covered and no recovery bucket or owed stale serve remains
func abortRecoveryIsComplete(plannedGatewayProfiles, coveredGatewayProfiles map[string]bool, recoveryResult map[string]any, staleRuntimeRows []any) bool {
	if len(plannedGatewayProfiles) == 0 {
		return false
	}
	for profile := range plannedGatewayProfiles {
		if !coveredGatewayProfiles[profile] {
			return false
		}
	}
	if pyrepr.Truthy(recoveryResult["failed"]) || pyrepr.Truthy(recoveryResult["relaunch_attempted"]) {
		return false
	}
	if pyrepr.Truthy(rowValue(recoveryResult["serve_units"], "failed")) {
		return false
	}
	return len(owedStaleServeRows(staleRuntimeRows)) == 0
}

// Error a recovery child that outlived its budget stands for, the subprocess TimeoutExpired
type recoveryTimeout struct {
	argv    []string
	seconds float64
}

func (e *recoveryTimeout) Error() string {
	return fmt.Sprintf("Command %s timed out after %s seconds", pyrepr.Repr(stringsAny(e.argv)), pyrepr.Str(e.seconds))
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
