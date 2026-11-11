package gateway

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Names of the signals a shutdown snapshot recognises by number
var forensicsSignalNames = map[int]string{
	int(syscall.SIGTERM): "SIGTERM", int(syscall.SIGINT): "SIGINT", int(syscall.SIGHUP): "SIGHUP",
	int(syscall.SIGQUIT): "SIGQUIT",
}

// Readable signal name, UNKNOWN without a signal and signal#n for an unnamed number
func forensicsSignalName(sig *int) string {
	if sig == nil {
		return "UNKNOWN"
	}
	if name, ok := forensicsSignalNames[*sig]; ok {
		return name
	}
	return fmt.Sprintf("signal#%d", *sig)
}

// One field of /proc/pid/status, false off Linux or when unreadable
func readProcField(pid int, key string) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "", false
	}
	for _, line := range pystr.Splitlines(string(data), false) {
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(line[len(key)+1:]), true
		}
	}
	return "", false
}

// Compact proc identity of pid, never its command line
func procSummary(pid int) *pyjson.Object {
	summary := pyjson.NewObject()
	summary.Set("pid", int64(pid))
	if pid <= 0 {
		return summary
	}
	for _, pair := range [][2]string{{"name", "Name"}, {"state", "State"}} {
		if value, ok := readProcField(pid, pair[1]); ok {
			summary.Set(pair[0], value)
		}
	}
	if ppid, ok := readProcField(pid, "PPid"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(ppid)); err == nil {
			summary.Set("ppid", int64(n))
		}
	}
	if uid, ok := readProcField(pid, "Uid"); ok {
		if fields := strings.Fields(uid); uid != "" && len(fields) > 0 {
			uid = fields[0]
		}
		summary.Set("uid", uid)
	}
	return summary
}

// Text of a marker file, false when absent or unreadable
func readForensicsMarker(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimPrefix(strings.ToValidUTF8(string(data), "\ufffd"), "\ufeff"), true
}

// Fast snapshot of who asks this process to shut down, never blocking and never failing
func SnapshotShutdownContext(receivedSignal *int) *pyjson.Object {
	pid, ppid := os.Getpid(), os.Getppid()
	ctx := pyjson.NewObject()
	ctx.Set("ts", float64(time.Now().UnixNano())/1e9)
	ctx.Set("ts_monotonic", monotonicSeconds())
	ctx.Set("signal", forensicsSignalName(receivedSignal))
	if receivedSignal != nil {
		ctx.Set("signal_num", int64(*receivedSignal))
	} else {
		ctx.Set("signal_num", nil)
	}
	ctx.Set("pid", int64(pid))
	ctx.Set("ppid", int64(ppid))
	ctx.Set("parent", procSummary(ppid))
	ctx.Set("self", procSummary(pid))
	for _, pair := range [][2]string{{"systemd_invocation_id", "INVOCATION_ID"}, {"systemd_journal_stream", "JOURNAL_STREAM"}} {
		if value := os.Getenv(pair[1]); value != "" {
			ctx.Set(pair[0], value)
		}
	}
	ctx.Set("under_systemd", os.Getenv("INVOCATION_ID") != "" || ppid == 1)
	if load, err := loadAverage1m(); err == nil {
		ctx.Set("loadavg_1m", load)
	}
	if tracer, ok := readProcField(pid, "TracerPid"); ok && tracer != "0" {
		if n, err := strconv.Atoi(tracer); err == nil && pystr.StrIsDigit(tracer) {
			ctx.Set("tracer_pid", int64(n))
			ctx.Set("tracer", procSummary(n))
		} else {
			ctx.Set("tracer_pid", tracer)
			ctx.Set("tracer", nil)
		}
	}
	func() {
		defer func() { _ = recover() }()
		homeText, err := pypath.PathExpanduser(os.Getenv("ICEBATH_HOME"))
		if err != nil || homeText == "" {
			return
		}
		if raw, ok := readForensicsMarker(filepath.Join(homeText, takeoverMarkerFilename)); ok {
			ctx.Set("takeover_marker", firstRunes(raw, 300))
			ctx.Set("takeover_marker_for_self", strings.Contains(raw, fmt.Sprintf(`"target_pid": %d`, pid)) ||
				strings.Contains(raw, fmt.Sprintf(`'target_pid': %d`, pid)))
		}
		if raw, ok := readForensicsMarker(filepath.Join(homeText, plannedStopMarkerFilename)); ok {
			ctx.Set("planned_stop_marker", firstRunes(raw, 300))
		}
	}()
	return ctx
}

var monotonicStart = time.Now()

// Leading n code points of s
func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// Seconds on a clock that never goes backwards
func monotonicSeconds() float64 { return time.Since(monotonicStart).Seconds() }

// POSIX listing the detached diagnostic runs, identity and resource columns only
func asyncDiagnosticScript(signalName string, selfPid int) string {
	return fmt.Sprintf("echo '=== shutdown diagnostic @ %s ==='; ", signalName) +
		"echo '--- date ---'; date -u +%Y-%m-%dT%H:%M:%SZ; " +
		"echo '--- ps (top 60 by cpu, comm only) ---'; " +
		"ps -eo pid,ppid,user,pcpu,pmem,stat,comm 2>/dev/null | { IFS= read -r h; echo \"$h\"; sort -nrk4; } | head -60; " +
		fmt.Sprintf("echo '--- pstree of self ---'; pstree -pl %d 2>/dev/null | head -40 || true; ", selfPid) +
		"echo '--- loadavg ---'; cat /proc/loadavg 2>/dev/null || sysctl -n vm.loadavg 2>/dev/null || true; " +
		"echo '--- recent dmesg (oom/killed) ---'; " +
		"dmesg -T 2>/dev/null | tail -20 || journalctl --user -n 20 --no-pager 2>/dev/null | tail -20 || true; " +
		"echo '=== end ==='"
}

// Pid of a detached process listing appended to logPath, false on failure or Windows
func SpawnAsyncDiagnostic(logPath, signalName string, timeoutSeconds float64) (int, bool) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o777); err != nil {
		return 0, false
	}
	return spawnAsyncDiagnostic(logPath, signalName, timeoutSeconds)
}

// Bound argv of the diagnostic, the GNU timeout prefix when one is installed
func diagnosticTimeoutPrefix(timeoutSeconds float64) []string {
	bin, err := exec.LookPath("timeout")
	if err != nil {
		bin, err = exec.LookPath("gtimeout")
	}
	if err != nil {
		return nil
	}
	return []string{bin, strconv.FormatFloat(timeoutSeconds, 'f', 0, 64)}
}

// One scannable log line of a shutdown context, parent identity but never argv
func FormatContextForLog(ctx *pyjson.Object) string {
	parent, _ := recGet(ctx, "parent").(*pyjson.Object)
	loadStr := "?"
	switch load := recGet(ctx, "loadavg_1m").(type) {
	case float64:
		loadStr = strconv.FormatFloat(load, 'f', 2, 64)
	case int64:
		loadStr = strconv.FormatFloat(float64(load), 'f', 2, 64)
	}
	var extras []string
	if recGet(ctx, "takeover_marker") != nil {
		who := "other"
		if pyrepr.Truthy(recGet(ctx, "takeover_marker_for_self")) {
			who = "self"
		}
		extras = append(extras, "takeover_marker_present="+who)
	}
	if recGet(ctx, "planned_stop_marker") != nil {
		extras = append(extras, "planned_stop_marker_present=yes")
	}
	if tracer := recGet(ctx, "tracer_pid"); pyrepr.Truthy(tracer) {
		extras = append(extras, "tracer_pid="+pyrepr.Str(tracer))
	}
	extrasStr := ""
	if len(extras) > 0 {
		extrasStr = " " + strings.Join(extras, " ")
	}
	orUnknown := func(v any) string {
		if pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
		return "?"
	}
	signal := "?"
	if v, ok := ctx.Get("signal"); ok {
		signal = pyrepr.Str(v)
	}
	underSystemd := "no"
	if pyrepr.Truthy(recGet(ctx, "under_systemd")) {
		underSystemd = "yes"
	}
	return fmt.Sprintf("signal=%s under_systemd=%s parent_pid=%s parent_name=%s loadavg_1m=%s%s",
		signal, underSystemd, orUnknown(recGet(parent, "pid")), orUnknown(recGet(parent, "name")), loadStr, extrasStr)
}

// Sorted JSON of a shutdown context, values JSON cannot carry rendered as text
func ContextAsJSON(ctx any) string {
	out, err := pyjson.Dumps(ctx, pyjson.SortKeys(true), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return "{}"
	}
	return out
}

// Whether systemd's TimeoutStopSec covers the stop budget, nil when aligned facts cannot be read
func CheckSystemdTimingAlignment(ctx context.Context, drainTimeout, cronDrainTimeout float64) (*pyjson.Object, error) {
	if os.Getenv("INVOCATION_ID") == "" {
		return nil, nil
	}
	unitName := ""
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range pystr.Splitlines(string(data), true) {
			parts := strings.Split(strings.TrimSpace(line), "/")
			for i := len(parts) - 1; i >= 0; i-- {
				if strings.HasSuffix(parts[i], ".service") {
					unitName = parts[i]
					break
				}
			}
			if unitName != "" {
				break
			}
		}
	}
	if unitName == "" {
		return nil, nil
	}
	timeoutUS := systemdTimeoutStopUS(ctx, unitName)
	if timeoutUS == nil {
		return nil, nil
	}
	timeoutStopSec, _ := new(big.Float).Quo(new(big.Float).SetInt(timeoutUS), big.NewFloat(1_000_000.0)).Float64()
	expectedInt, err := ResolveSystemdTimeoutStopSec(drainTimeout, cronDrainTimeout, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	expected, _ := new(big.Float).SetInt(expectedInt).Float64()
	out := pyjson.NewObject()
	out.Set("unit", unitName)
	out.Set("timeout_stop_sec", timeoutStopSec)
	out.Set("drain_timeout", drainTimeout)
	out.Set("cron_drain_timeout", cronDrainTimeout)
	out.Set("expected_min", expected)
	out.Set("mismatch", timeoutStopSec < expected)
	return out, nil
}

// TimeoutStopUSec of the unit in microseconds, the user manager asked first
func systemdTimeoutStopUS(ctx context.Context, unitName string) *big.Int {
	for _, flag := range [][]string{{"--user"}, {}} {
		argv := append(append([]string{"systemctl"}, flag...), "show", unitName, "--property=TimeoutStopUSec")
		result, err := pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Timeout: 2 * time.Second})
		if err != nil || result.Returncode != 0 {
			continue
		}
		for _, line := range pystr.Splitlines(strings.ToValidUTF8(string(result.Stdout), "\ufffd"), false) {
			if !strings.HasPrefix(line, "TimeoutStopUSec=") {
				continue
			}
			value := strings.TrimSpace(line[len("TimeoutStopUSec="):])
			var timeoutUS *big.Int
			if pystr.StrIsDigit(value) {
				timeoutUS, _ = new(big.Int).SetString(value, 10)
			} else {
				timeoutUS = ParseSystemdDurationToUS(value)
			}
			if timeoutUS != nil {
				return timeoutUS
			}
		}
	}
	return nil
}
