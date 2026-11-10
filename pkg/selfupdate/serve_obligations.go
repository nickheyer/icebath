package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	serveObligationsLog = slog.Default().With("logger", "hermes_cli.update_serve_obligations")
	// Liveness of a pid against a recorded creation time, known false when the prober cannot say
	servePidAliveMatches = func(pid int64, createTime *float64) (alive, known bool) {
		if pid > math.MaxInt32 {
			return false, true
		}
		return tools.PidAliveMatches(int(pid), createTime, false, nil)
	}
	serveStdout io.Writer = os.Stdout
	serveStderr io.Writer = os.Stderr
)

// Value under key of a decoded JSON object, with key presence and whether v is an object
func rowLookup(v any, key string) (value any, present, isDict bool) {
	switch row := v.(type) {
	case map[string]any:
		if row == nil {
			return nil, false, false
		}
		value, present = row[key]
		return value, present, true
	case *pyjson.Object:
		if row == nil {
			return nil, false, false
		}
		value, present = row.Get(key)
		return value, present, true
	}
	return nil, false, false
}

// Value under key of a row, nil when the row is not an object or lacks the key
func rowValue(v any, key string) any {
	value, _, _ := rowLookup(v, key)
	return value
}

// Value under key of a row, the fallback when the key is absent
func rowValueOr(v any, key string, fallback any) any {
	if value, present, _ := rowLookup(v, key); present {
		return value
	}
	return fallback
}

// True for a decoded JSON object
func isRowDict(v any) bool {
	_, _, ok := rowLookup(v, "")
	return ok
}

// True when the row's kind is serve or dashboard
func isServeKind(row any) bool {
	kind := rowValue(row, "kind")
	return kind == "serve" || kind == "dashboard"
}

// Integer of a value whose Python type is exactly int
func exactInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// Number of a value whose Python type is exactly int or float
func exactNumber(v any) (float64, bool) {
	if n, ok := exactInt(v); ok {
		return float64(n), true
	}
	f, ok := v.(float64)
	return f, ok
}

// Files a durable restart reminder for an identified manual serve, true when discharged or filed
func DeferManualServe(ctx context.Context, runtime any, requireAlive bool) bool {
	if !isServeKind(runtime) || rowValue(runtime, "supervisor") != "manual-serve" || rowValue(runtime, "restart_via") != "respawn-argv" {
		return false
	}
	if !isRowDict(rowValue(runtime, "detail")) {
		return false
	}
	return deferIdentified(ctx, runtime, nil, requireAlive)
}

// True when the incarnation is gone or its reminder was filed, false while the restart is still owed
func deferIdentified(ctx context.Context, runtime any, extra *pyjson.Object, requireAlive bool) bool {
	pid, isInt := exactInt(rowValue(runtime, "pid"))
	if !isInt || pid <= 0 {
		return false
	}
	rawCreated := rowValue(rowValue(runtime, "detail"), "create_time")
	created, isNumber := exactNumber(rawCreated)
	if !isNumber || math.IsInf(created, 0) || math.IsNaN(created) || created <= 0 {
		if requireAlive {
			return false
		}
		alive, known := servePidAliveMatches(pid, nil)
		return known && !alive
	}
	alive, known := servePidAliveMatches(pid, &created)
	if requireAlive && !(known && alive) {
		return false
	}
	if known && !alive {
		return true
	}
	row := pyjson.NewObject()
	row.Set("kind", rowValue(runtime, "kind"))
	row.Set("profile", rowValueOr(runtime, "profile", "unknown"))
	row.Set("pid", pid)
	row.Set("create_time", rawCreated)
	if extra != nil {
		for _, key := range extra.Keys {
			row.Set(key, extra.Vals[key])
		}
	}
	if err := writeServeReminder(ctx, row, pid, created); err != nil {
		serveObligationsLog.Debug(fmt.Sprintf("Could not preserve serve restart obligation: %s", pypath.OSErrorText(err)))
		return false
	}
	return true
}

// Directory holding one reminder file per serve incarnation
func serveRestartPendingDir(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "serve_restart_pending")
}

// Persists one reminder row as an immutable file named by pid and creation time through a synced temp file
func writeServeReminder(ctx context.Context, row *pyjson.Object, pid int64, created float64) error {
	directory := serveRestartPendingDir(ctx)
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return err
	}
	target := filepath.Join(directory, fmt.Sprintf("%d-%s.json", pid, pyint.FloatHex(created)))
	handle, err := os.CreateTemp(directory, "tmp")
	if err != nil {
		return err
	}
	temporary := handle.Name()
	defer func() {
		if rmErr := os.Remove(temporary); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			serveObligationsLog.Debug(fmt.Sprintf("Could not remove %s: %s", temporary, pypath.OSErrorText(rmErr)))
		}
	}()
	text, err := pyjson.Dumps(row)
	if err != nil {
		handle.Close()
		return err
	}
	if _, err := handle.WriteString(text); err != nil {
		handle.Close()
		return err
	}
	if err := handle.Sync(); err != nil {
		handle.Close()
		return err
	}
	if err := handle.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

// True for a serve or dashboard row a systemd unit owns with the unit recorded
func IsUnitServeRow(row any) bool {
	if !isRowDict(row) || !isServeKind(row) || rowValue(row, "supervisor") != "systemd" {
		return false
	}
	detail := rowValue(row, "detail")
	return isRowDict(detail) && pyrepr.Truthy(rowValue(detail, "systemd_unit"))
}

// True for the rows the receipt backed reminder flow carries, manual serves and unit owned serves
func IsReminderRow(row any) bool {
	return IsUnitServeRow(row) || (isRowDict(row) && isServeKind(row) && rowValue(row, "supervisor") == "manual-serve")
}

// Files a durable reminder carrying the unit's restart command for a unit owned serve still alive
func DeferUnitServe(ctx context.Context, runtime any) (bool, error) {
	if !IsUnitServeRow(runtime) {
		return false, nil
	}
	detail := rowValue(runtime, "detail")
	commands, err := gateway.SystemdRestartCommands(pyrepr.Str(rowValue(detail, "systemd_unit")), rowValue(detail, "systemd_scope"))
	if err != nil {
		return false, err
	}
	restart := make([]any, len(commands))
	for i, c := range commands {
		restart[i] = c
	}
	extra := pyjson.NewObject()
	extra.Set("restart", restart)
	return deferIdentified(ctx, runtime, extra, false), nil
}

// Elements of a truthy list, none for a falsy value, an error for any other value
func rowsOf(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("TypeError: '%s' object is not a list of rows", pyrepr.TypeName(v))
	}
	return items, nil
}

// Reminder rows of the receipt whose transfer is still owed, so receipt rotation cannot discard failed writes
func RetainReceiptManualServes(ctx context.Context, receipt any) ([]any, error) {
	receipt = pyjson.Plain(receipt)
	var plan any = map[string]any{}
	if p := rowValue(receipt, "plan"); pyrepr.Truthy(p) {
		if !isRowDict(p) {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(p))
		}
		plan = p
	}
	var rows []any
	for _, source := range []any{rowValue(plan, "runtimes"), rowValue(receipt, "pending_manual_serves"), rowValue(receipt, "carried_manual_serves")} {
		items, err := rowsOf(source)
		if err != nil {
			return nil, err
		}
		rows = append(rows, items...)
	}
	pending := []any{}
	for _, row := range rows {
		if !IsReminderRow(row) {
			continue
		}
		deferred := false
		if IsUnitServeRow(row) {
			var err error
			if deferred, err = DeferUnitServe(ctx, row); err != nil {
				return nil, err
			}
		} else {
			deferred = DeferManualServe(ctx, row, false)
		}
		if deferred {
			continue
		}
		seen := false
		for _, have := range pending {
			seen = seen || pyrepr.Equal(have, row)
		}
		if !seen {
			pending = append(pending, row)
		}
	}
	return pending, nil
}

// Warns about serve restart debt, the unsaved transfers of the snapshot first then every reminder still filed
func WarnPendingManualServes(ctx context.Context, startup bool, pendingManual []any, havePending bool) error {
	stream := serveStdout
	if startup {
		stream = serveStderr
	}
	return warnPendingManualServesTo(ctx, stream, pendingManual, havePending)
}

// Writes the serve restart debt warnings to stream, reading the receipt when the caller holds no retained transfers
func warnPendingManualServesTo(ctx context.Context, stream io.Writer, pendingManual []any, havePending bool) error {
	if !havePending {
		var receipt any = map[string]any{}
		if latest := updaterecord.ReadLatestReceipt(ctx); latest != nil {
			receipt = latest
		}
		var err error
		if pendingManual, err = RetainReceiptManualServes(ctx, receipt); err != nil {
			return err
		}
	}
	for _, row := range pendingManual {
		unitRow := IsUnitServeRow(row)
		label := "manual"
		if unitRow {
			label = "unit"
		}
		kind, present, _ := rowLookup(row, "kind")
		if !present {
			return fmt.Errorf("KeyError: 'kind'")
		}
		fmt.Fprintf(stream, "  ⚠ %s [%s] pid %s: %s restart reminder could not be saved; restart remains pending in the update receipt.\n",
			pyrepr.Str(kind), pyrepr.Str(rowValueOr(row, "profile", "unknown")), pyrepr.Str(rowValueOr(row, "pid", "unknown")), label)
		var detail any = map[string]any{}
		if d := rowValue(row, "detail"); isRowDict(d) {
			detail = d
		}
		if unitRow {
			commands, err := gateway.SystemdRestartCommands(pyrepr.Str(rowValue(detail, "systemd_unit")), rowValue(detail, "systemd_scope"))
			if err != nil {
				return err
			}
			fmt.Fprintf(stream, "    Restart its unit: %s\n", gateway.FormatRestartCommands(commands))
		} else if _, isNumber := exactNumber(rowValue(detail, "create_time")); isNumber {
			fmt.Fprintln(stream, "    Ask its owner to relaunch `icebath serve` / `icebath dashboard`; check reminder storage permissions and free space.")
		} else {
			fmt.Fprintln(stream, "    This host could not read the process creation time, so no durable reminder could be filed; ask its owner to relaunch `icebath serve` / `icebath dashboard`, and the warning clears once the pid is confirmed gone.")
		}
	}
	paths, _ := filepath.Glob(filepath.Join(pypathGlobEscape(serveRestartPendingDir(ctx)), "*.json"))
	sort.Strings(paths)
	for _, path := range paths {
		if err := reconcileServeReminder(stream, path); err != nil {
			serveObligationsLog.Debug(fmt.Sprintf("Could not reconcile manual serve obligation %s: %s", path, pypath.OSErrorText(err)))
			fmt.Fprintf(stream, "  ⚠ Manual serve restart reminder could not be verified: %s\n", filepath.Base(path))
		}
	}
	return nil
}

// Directory path with glob metacharacters escaped so only the file pattern matches
func pypathGlobEscape(dir string) string {
	var b bytes.Buffer
	for _, r := range dir {
		if r == '*' || r == '?' || r == '[' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Drops the reminder of a dead incarnation or prints the restart it still owes
func reconcileServeReminder(stream io.Writer, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return fmt.Errorf("UnicodeDecodeError: reminder is not UTF-8")
	}
	doc, err := pyjson.Loads(string(raw))
	if err != nil {
		return err
	}
	row, isDict := pyjson.Plain(doc).(map[string]any)
	if !isDict {
		return fmt.Errorf("TypeError: reminder is not an object")
	}
	for _, key := range []string{"pid", "create_time"} {
		if _, present := row[key]; !present {
			return fmt.Errorf("KeyError: '%s'", key)
		}
	}
	if alive, known := reminderLiveness(row["pid"], row["create_time"]); known && !alive {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return rmErr
		}
		return nil
	}
	for _, key := range []string{"kind", "profile"} {
		if _, present := row[key]; !present {
			return fmt.Errorf("KeyError: '%s'", key)
		}
	}
	head := fmt.Sprintf("  ⚠ %s [%s] pid %s: ", pyrepr.Str(row["kind"]), pyrepr.Str(row["profile"]), pyrepr.Str(row["pid"]))
	if restart, isList := row["restart"].([]any); isList && len(restart) > 0 {
		commands := make([]string, len(restart))
		for i, c := range restart {
			commands[i] = pyrepr.Str(c)
		}
		fmt.Fprintln(stream, head+"unit restart still pending; this process may still serve pre-update code.")
		fmt.Fprintf(stream, "    Restart its unit: %s\n", gateway.FormatRestartCommands(commands))
		return nil
	}
	fmt.Fprintln(stream, head+"manual restart still pending; this process may still serve pre-update code.")
	fmt.Fprintln(stream, "    Ask its owner to relaunch `icebath serve` / `icebath dashboard` (reconnect Desktop for an SSH backend).")
	return nil
}

// Liveness of a filed reminder's pid, unknown when the pid or creation time is not a usable number
func reminderLiveness(rawPid, rawCreated any) (alive, known bool) {
	pid, err := pyint.Of(rawPid)
	if err != nil || !pid.IsInt64() {
		return false, false
	}
	if rawCreated == nil {
		return servePidAliveMatches(pid.Int64(), nil)
	}
	created, isNumber := exactNumber(rawCreated)
	if !isNumber {
		return false, false
	}
	return servePidAliveMatches(pid.Int64(), &created)
}
