package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Startup watchdog lease of the unclean exit integrity check, renewed while SQLite makes progress
const (
	integrityCheckLeasePhase  = "state_db_unclean_integrity_check"
	integrityCheckLeaseS      = 900.0
	StateDBIntegrityDeadlineS = 1800.0
)

var (
	integrityCheckProgressOps   = 100_000
	integrityCheckLeaseRenewS   = 60.0
	ledgerProcessCreateTime     = processCreateTimeLive
	ledgerOnDiskJournalMode     = state.OnDiskJournalMode
	ledgerReportStartupProgress = state.ReportStartupProgress
	checkStateDBIntegrity       = CheckStateDBIntegrity
)

// Epoch birth of pid from the process identity prober, nil when it cannot say
func processCreateTimeLive(pid int) *float64 {
	birth, err := tools.ProcessBirth(pid)
	if err != nil {
		return nil
	}
	return &birth
}

// Path under the given home or the process home
func ledgerHomePath(ctx context.Context, h string, relative ...string) string {
	return filepath.Join(append([]string{watchdogHome(ctx, h)}, relative...)...)
}

// state/gateway.lifecycle.json under the given home or the process home
func GetLifecycleSentinelPath(ctx context.Context, h string) string {
	return ledgerHomePath(ctx, h, "state", "gateway.lifecycle.json")
}

// Duration of a non negative seconds count, a century standing in for any span past it
func secondsDuration(s float64) time.Duration {
	const century = 100 * 365 * 24 * time.Hour
	if !(s < century.Seconds()) {
		return century
	}
	return time.Duration(s * float64(time.Second))
}

func ledgerNowISO() string { return pydatetime.ISOFormatUTC(time.Now()) }

// Integer fields of a Key value proc file renamed per wanted, empty when unreadable or malformed
func procFields(path string, wanted map[string]string) *pyjson.Object {
	found := pyjson.NewObject()
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return pyjson.NewObject()
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.SplitAfter(text, "\n") {
		key, _, _ := strings.Cut(line, ":")
		dst, ok := wanted[key]
		if !ok {
			continue
		}
		fields := pystr.SplitWhitespace(line, -1)
		if len(fields) < 2 {
			return pyjson.NewObject()
		}
		n, ok := pyint.Parse(fields[1])
		if !ok {
			return pyjson.NewObject()
		}
		found.Set(dst, pyint.Value(n))
		if found.Len() == len(wanted) {
			break
		}
	}
	return found
}

// Own RSS, MemTotal, MemAvailable and swap used in KiB from proc, empty off Linux
func SampleMemory() *pyjson.Object {
	sample := procFields("/proc/self/status", map[string]string{"VmRSS": "rss_kib"})
	mem := procFields("/proc/meminfo", map[string]string{"MemTotal": "mem_total_kib", "MemAvailable": "mem_available_kib",
		"SwapTotal": "SwapTotal", "SwapFree": "SwapFree"})
	swapTotal, hasTotal := mem.Get("SwapTotal")
	swapFree, hasFree := mem.Get("SwapFree")
	mem.Delete("SwapTotal")
	mem.Delete("SwapFree")
	for _, k := range mem.Keys {
		sample.Set(k, mem.Vals[k])
	}
	if hasTotal && hasFree {
		total, _ := pyint.Of(swapTotal)
		free, _ := pyint.Of(swapFree)
		sample.Set("swap_used_kib", pyint.Value(new(big.Int).Sub(total, free)))
	}
	return sample
}

// JSON object of a UTF-8 file with an optional BOM, nil when unreadable, malformed or not an object
func ledgerReadJSON(path string) *pyjson.Object {
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return nil
	}
	v, err := pyjson.Loads(strings.TrimPrefix(string(data), "\ufeff"))
	if err != nil {
		return nil
	}
	obj, _ := v.(*pyjson.Object)
	return obj
}

// Writes the lifecycle sentinel atomically as compact JSON, logging failures at debug
func writeSentinel(ctx context.Context, payload *pyjson.Object, h string) {
	path := GetLifecycleSentinelPath(ctx, h)
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		slog.Debug(fmt.Sprintf("Failed to write lifecycle sentinel: %v", err))
		return
	}
	if err := config.AtomicJSONWriteWith(ctx, path, payload); err != nil {
		slog.Debug(fmt.Sprintf("Failed to write lifecycle sentinel: %v", err))
	}
}

// Appends one JSON line to logs/gateway-exit-diag.log, unknown values rendered through str
func appendExitDiag(ctx context.Context, record *pyjson.Object, h string) {
	path := ledgerHomePath(ctx, h, "logs", "gateway-exit-diag.log")
	text, err := pyjson.Dumps(record, pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to append unclean-exit record: %v", err))
		return
	}
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		slog.Debug(fmt.Sprintf("Failed to append unclean-exit record: %v", err))
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err == nil {
		_, err = f.WriteString(text + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to append unclean-exit record: %v", err))
	}
}

// Python int of a value whose type is int or float exactly, false for bool and every other type
func exactNumber(v any) (float64, bool, error) {
	switch v.(type) {
	case bool, nil, string:
		return 0, false, nil
	case float64:
		return v.(float64), true, nil
	}
	if _, ok := pyrepr.Int(v); !ok {
		return 0, false, nil
	}
	f, err := pyint.Float(v)
	return f, true, err
}

// Whether pid is the live incarnation that claimed the sentinel, a handover rather than a death
func pidIsSentinelOwner(pid, startTime, createTime any) (bool, error) {
	n, err := pyint.Of(pid)
	if err != nil || n.Sign() <= 0 || !n.IsInt64() || n.Int64() > math.MaxInt32 || !statusPidExists(int(n.Int64())) {
		return false, nil
	}
	actual := ledgerProcessCreateTime(int(n.Int64()))
	if actual == nil {
		return true, nil
	}
	if f, ok, err := exactNumber(createTime); ok {
		return err == nil && math.Abs(*actual-f) <= 2.0, err
	}
	if f, ok, err := exactNumber(startTime); ok {
		return err == nil && *actual <= f+2.0, err
	}
	return true, nil
}

// Python int of an isinstance int value, bools included
func isinstanceInt(v any) (*big.Int, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return pyrepr.Int(v)
}

// Whether a heartbeat memory sample sits in the critical tier of memory status
func suspectedOOM(mem *pyjson.Object) (bool, error) {
	avail, ok := isinstanceInt(recGet(mem, "mem_available_kib"))
	if !ok {
		return false, nil
	}
	if avail.Cmp(big.NewInt(criticalAvailableKiB)) < 0 {
		return true, nil
	}
	total, ok := isinstanceInt(recGet(mem, "mem_total_kib"))
	if !ok || total.Sign() <= 0 {
		return false, nil
	}
	fraction, _ := new(big.Rat).SetFrac(avail, total).Float64()
	if math.IsInf(fraction, 0) {
		return false, errors.New("integer division result too large for a float")
	}
	return fraction < criticalAvailableFraction, nil
}

// Evidence of an unclean previous life, nil when the sentinel is clean, absent or held by a live owner
func DetectUncleanExit(ctx context.Context, h string) (*pyjson.Object, error) {
	sentinel := ledgerReadJSON(GetLifecycleSentinelPath(ctx, h))
	if sentinel == nil || sentinel.Len() == 0 || !pyEq(recGet(sentinel, "phase"), "running") {
		return nil, nil
	}
	owner, err := pidIsSentinelOwner(recGet(sentinel, "pid"), recGet(sentinel, "start_time"), recGet(sentinel, "create_time"))
	if err != nil {
		return nil, err
	}
	if owner {
		return nil, nil
	}
	evidence := pyjson.NewObject()
	evidence.Set("prior_pid", recGet(sentinel, "pid"))
	evidence.Set("prior_started_at", recGet(sentinel, "started_at"))
	evidence.Set("prior_start_time", recGet(sentinel, "start_time"))
	hb := ledgerReadJSON(GetLoopHeartbeatPath(ctx, h))
	if hb != nil && hb.Len() > 0 {
		evidence.Set("last_heartbeat_at", recGet(hb, "updated_at"))
	}
	if mem, ok := recGet(hb, "mem").(*pyjson.Object); ok {
		evidence.Set("last_heartbeat_mem", mem)
		oom, err := suspectedOOM(mem)
		if err != nil {
			return nil, err
		}
		if oom {
			evidence.Set("suspected_oom", true)
		}
	}
	return evidence, nil
}

// Claims the startup lease and renews it from a SQLite progress handler, aborting once deadline passes
func installIntegrityCheckLease(conn state.SQLiteConn, deadline *time.Time) error {
	ledgerReportStartupProgress(integrityCheckLeaseS, integrityCheckLeasePhase)
	lastRenew := time.Now()
	renewEvery := secondsDuration(integrityCheckLeaseRenewS)
	raw, ok := conn.(pysqlite.RawConn)
	if !ok {
		return fmt.Errorf("connection %T exposes no driver connection for a progress handler", conn)
	}
	return pysqlite.SetProgressHandler(raw, func() int {
		now := time.Now()
		if now.Sub(lastRenew) >= renewEvery {
			lastRenew = now
			ledgerReportStartupProgress(integrityCheckLeaseS, integrityCheckLeasePhase)
		}
		if deadline != nil && !now.Before(*deadline) {
			return 1
		}
		return 0
	}, integrityCheckProgressOps)
}

// ok, absent, or the first quick_check complaint of state.db, check-incomplete when the deadline aborts it
func CheckStateDBIntegrity(ctx context.Context, h string, deadlineS *float64) string {
	path := ledgerHomePath(ctx, h, "state.db")
	if _, err := os.Stat(path); err != nil {
		return "absent"
	}
	var deadline *time.Time
	if deadlineS != nil {
		at := time.Now().Add(secondsDuration(pyMaxFloat(0.0, *deadlineS)))
		deadline = &at
	}
	row, err := integrityQuickCheck(path, deadline)
	if err != nil {
		class, _ := state.SQLiteClassOf(err)
		if deadline != nil && class == state.SQLiteOperationalError && strings.Contains(strings.ToLower(state.ErrText(err)), "interrupt") {
			limit, _ := pyrepr.Format(pyMaxFloat(0.0, *deadlineS), "g")
			return "check-incomplete: exceeded " + limit + "s"
		}
		return "check-failed: " + state.ErrText(err)
	}
	if row == nil {
		return "check-failed: no result"
	}
	return pyrepr.Str(row)
}

// First column of PRAGMA quick_check(1) on a fresh connection under the integrity lease, nil without a row
func integrityQuickCheck(path string, deadline *time.Time) (any, error) {
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := installIntegrityCheckLease(conn, deadline); err != nil {
		return nil, err
	}
	if raw, ok := conn.(pysqlite.RawConn); ok {
		defer pysqlite.SetProgressHandler(raw, nil, 0)
	}
	rows, err := conn.QueryContext(context.Background(), "PRAGMA quick_check(1)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var value any
	if err := rows.Scan(&value); err != nil {
		return nil, err
	}
	if b, ok := value.([]byte); ok {
		value = string(b)
	}
	return value, rows.Close()
}

// On disk journal mode of state.db through one short lived connection, empty when undeterminable
func stateDBJournalMode(path string) string {
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 1.0})
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read state.db journal mode: %s", state.ErrText(err)))
		return ""
	}
	defer conn.Close()
	mode, ok, err := ledgerOnDiskJournalMode(context.Background(), conn)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read state.db journal mode: %s", state.ErrText(err)))
		return ""
	}
	if !ok {
		return ""
	}
	return mode
}

const stateDBFailedIntegrity = "state.db FAILED integrity check after an unclean gateway exit: %s — sessions may read as " +
	"missing until it is repaired. Run `icebath doctor`."

// Persists the unclean exit record and warns, scanning state.db inline unless the scan is deferred
func reportUncleanExit(ctx context.Context, evidence *pyjson.Object, h string, deferIntegrityCheck bool) {
	if deferIntegrityCheck {
		evidence.Set("state_db_integrity", "deferred")
	} else {
		verdict := checkStateDBIntegrity(ctx, h, nil)
		evidence.Set("state_db_integrity", verdict)
		if verdict != "ok" && verdict != "absent" {
			slog.Error(fmt.Sprintf(stateDBFailedIntegrity, verdict))
		}
	}
	record := pyjson.NewObject()
	record.Set("ts", ledgerNowISO())
	record.Set("tag", "gateway.previous_unclean_exit")
	record.Set("pid", int64(os.Getpid()))
	for _, k := range evidence.Keys {
		record.Set(k, evidence.Vals[k])
	}
	appendExitDiag(ctx, record, h)
	oom := recGet(evidence, "suspected_oom")
	if _, ok := evidence.Get("suspected_oom"); !ok {
		oom = false
	}
	slog.Warn(fmt.Sprintf("Previous gateway life (pid=%s, started_at=%s) exited UNCLEANLY (no exit path ran — SIGKILL / OOM / "+
		"VM death, or a process kill issued by the agent or one of its descendants, e.g. a pkill/taskkill of "+
		"the host interpreter image; see #113667). last_heartbeat_at=%s last_mem=%s suspected_oom=%s",
		pyrepr.Str(recGet(evidence, "prior_pid")), pyrepr.Str(recGet(evidence, "prior_started_at")),
		pyrepr.Str(recGet(evidence, "last_heartbeat_at")), pyrepr.Str(recGet(evidence, "last_heartbeat_mem")), pyrepr.Str(oom)))
}

// Reports an unclean previous exit then claims the sentinel for this life, returning the evidence
func RecordStartup(ctx context.Context, h string, deferIntegrityCheck bool) *pyjson.Object {
	evidence, err := DetectUncleanExit(ctx, h)
	if err != nil {
		slog.Debug(fmt.Sprintf("Unclean-exit detection failed: %v", err))
		evidence = nil
	} else if evidence != nil {
		reportUncleanExit(ctx, evidence, h, deferIntegrityCheck)
	}
	claim := pyjson.NewObject()
	claim.Set("phase", "running")
	claim.Set("pid", int64(os.Getpid()))
	claim.Set("start_time", float64(time.Now().UnixNano())/1e9)
	claim.Set("started_at", ledgerNowISO())
	if createTime := ledgerProcessCreateTime(os.Getpid()); createTime != nil {
		claim.Set("create_time", *createTime)
	}
	if evidence != nil {
		claim.Set("prior_unclean_exit", true)
		if pyrepr.Truthy(recGet(evidence, "suspected_oom")) {
			claim.Set("prior_suspected_oom", true)
		}
	}
	writeSentinel(ctx, claim, h)
	return evidence
}

// Post connect state.db scan of an unclean exit appending a second diag record, never panics
func RunDeferredIntegrityCheck(ctx context.Context, evidence *pyjson.Object, h string, deadlineS float64) (verdict string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("Deferred state.db integrity check failed: %v", r))
			verdict = "check-failed: deferred scan raised"
		}
	}()
	path := ledgerHomePath(ctx, h, "state.db")
	mode := "wal"
	if _, err := os.Stat(path); err == nil {
		mode = stateDBJournalMode(path)
	}
	if mode != "wal" {
		shown := mode
		if shown == "" {
			shown = "unknown"
		}
		verdict = "skipped: rollback-journal store (journal_mode=" + shown + "), scan would block live writes"
		slog.Warn(fmt.Sprintf("Previous gateway exit was unclean, but the state.db integrity check was skipped: %s is a "+
			"rollback-journal store (journal_mode=%s) and a background scan would block live writes. "+
			"To check it, stop the gateway and run: sqlite3 %s 'PRAGMA quick_check'", path, shown, path))
	} else {
		verdict = checkStateDBIntegrity(ctx, h, &deadlineS)
	}
	if verdict != "ok" && verdict != "absent" && !strings.HasPrefix(verdict, "skipped:") {
		slog.Error(fmt.Sprintf(stateDBFailedIntegrity, verdict))
	}
	record := pyjson.NewObject()
	record.Set("ts", ledgerNowISO())
	record.Set("tag", "gateway.state_db_integrity")
	record.Set("deferred", true)
	record.Set("state_db_integrity", verdict)
	if prior := recGet(evidence, "prior_pid"); prior != nil {
		record.Set("prior_pid", prior)
	}
	appendExitDiag(ctx, record, h)
	if evidence != nil {
		evidence.Set("state_db_integrity", verdict)
	}
	return verdict
}

// Marks this life cleanly exited unless another process owns the sentinel, keeping its incarnation identity
func MarkExited(ctx context.Context, exitCode *int, reason string, h string) {
	sentinel := ledgerReadJSON(GetLifecycleSentinelPath(ctx, h))
	if sentinel != nil && !pyEq(recGet(sentinel, "pid"), int64(os.Getpid())) {
		return
	}
	exited := pyjson.NewObject()
	exited.Set("phase", "exited")
	exited.Set("pid", int64(os.Getpid()))
	if exitCode != nil {
		exited.Set("exit_code", int64(*exitCode))
	} else {
		exited.Set("exit_code", nil)
	}
	exited.Set("exit_reason", reason)
	exited.Set("exited_at", ledgerNowISO())
	for _, key := range []string{"start_time", "create_time"} {
		if v := recGet(sentinel, key); v != nil {
			exited.Set(key, v)
		}
	}
	writeSentinel(ctx, exited, h)
}

// clean, unclean or unknown for the last gateway life of a profile home
func ReadPriorExitLabel(profileHome string) string {
	sentinel := ledgerReadJSON(filepath.Join(profileHome, "state", "gateway.lifecycle.json"))
	switch phase := recGet(sentinel, "phase"); {
	case pyEq(phase, "exited"):
		return "clean"
	case pyEq(phase, "running"):
		return "unclean"
	}
	return "unknown"
}
