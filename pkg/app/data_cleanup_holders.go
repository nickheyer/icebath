package app

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Stop advice per supervisor that would relaunch a gateway right after a drain
var ownerStopHints = map[string]string{
	"systemd":  "stop its systemd unit with systemctl before retrying",
	"launchd":  "stop its LaunchAgent with launchctl before retrying",
	"desktop":  "quit the owning desktop app before retrying",
	"external": "stop the gateway through its external supervisor before retrying",
}

// Control socket probes the holder checks reach, swapped by the cases that answer for a gateway
var (
	holdersIdentifyGateway = func(h string) *pyjson.Object {
		return gateway.IdentifyGateway(h, gateway.DefaultControlClientTimeout)
	}
	holdersPauseGateway = func(h string) *pyjson.Object {
		return gateway.PauseGatewayForUpdate(h, gateway.DefaultControlClientTimeout)
	}
	holdersProcessStartTime = gateway.GetProcessStartTime
)

// RuntimeError a holder check raises when a live writer still owns the home
type holderError struct{ msg string }

func (e *holderError) Error() string { return e.msg }

func holderErrorf(format string, args ...any) error {
	return &holderError{fmt.Sprintf(format, args...)}
}

// Whether the strict gateway identity behind pidPath names a live gateway, its ambiguity error otherwise
func strictGatewayRunning(pidPath string) (bool, error) {
	_, _, ok, err := gateway.GetRunningPidIdentityStrict(pidPath)
	return ok, err
}

// Python int value of a decoded JSON field, false for bool, float, big or missing values
func jsonPyInt(v any) (int, bool) {
	n, ok := v.(int64)
	return int(n), ok
}

// Identify answer of the manually launched gateway owning the home, nil when none runs, an error for a supervised one
func manualGatewayIdentity(h string) (*pyjson.Object, error) {
	identity := holdersIdentifyGateway(h)
	if identity == nil {
		running, err := strictGatewayRunning(filepath.Join(h, "gateway.pid"))
		if err != nil {
			return nil, err
		}
		if running {
			return nil, holderErrorf("gateway has no usable control socket for %s; stop it through its owner and retry", h)
		}
		return nil, nil
	}
	claimed, _ := identity.Get("hermes_home")
	if !pyrepr.Truthy(claimed) {
		return nil, holderErrorf("gateway control identity does not match %s", h)
	}
	resolved, err := pypath.Resolve(pyrepr.Str(claimed))
	if err != nil {
		return nil, err
	}
	if !pathEq(resolved, h) {
		return nil, holderErrorf("gateway control identity does not match %s", h)
	}
	owner := gateway.LaunchSupervisor(identity)
	if owner != "manual" {
		hint, known := ownerStopHints[owner]
		if !known {
			hint = "stop it through its supervisor before retrying"
		}
		named := owner
		if named == "" {
			named = "unknown"
		}
		return nil, holderErrorf("%s gateway still owns %s; %s", named, h, hint)
	}
	return identity, nil
}

// Whether the identified gateway itself accepted the drain or is already stopping
func pauseAcked(response, identity *pyjson.Object) bool {
	if response == nil || response.Len() == 0 || identity == nil {
		return false
	}
	respPid, _ := response.Get("pid")
	idPid, _ := identity.Get("pid")
	if !pyrepr.Equal(respPid, idPid) {
		return false
	}
	pausing, _ := response.Get("pausing")
	stopping, _ := response.Get("already_stopping")
	return pyrepr.Truthy(pausing) || pyrepr.Truthy(stopping)
}

// Python float of a drain timeout field, the TypeError or ValueError text for anything else
func drainTimeoutSeconds(v any) (float64, error) {
	switch x := v.(type) {
	case int64:
		return float64(x), nil
	case float64:
		return x, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, fmt.Errorf("could not convert string to float: %s", pyrepr.StrRepr(x))
		}
		return f, nil
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", pyrepr.TypeName(v))
}

// Drains the manual gateway of the home over its control socket and waits until it is gone
func drainManualGateway(h string) error {
	identity, err := manualGatewayIdentity(h)
	if err != nil || identity == nil {
		return err
	}
	if served, _ := identity.Get("served_profiles"); pyrepr.Truthy(served) {
		return holderErrorf("gateway serves multiple profiles; stop it explicitly before deleting one profile's data")
	}
	rawPid, _ := identity.Get("pid")
	start, _ := identity.Get("start_time")
	pid, isInt := jsonPyInt(rawPid)
	if !isInt || pid <= 0 || start == nil || !pyrepr.Equal(holdersProcessStartTime(pid), start) {
		return holderErrorf("gateway process identity is unverified for %s", h)
	}
	response := holdersPauseGateway(h)
	if !pauseAcked(response, identity) {
		return holderErrorf("gateway refused to drain for %s", h)
	}
	var probeErr error
	gone := func() (bool, bool) {
		current := holdersProcessStartTime(pid)
		originalGone := !gateway.PidExists(pid) || (current != nil && !pyrepr.Equal(current, start))
		if !originalGone || holdersIdentifyGateway(h) != nil {
			return false, false
		}
		running, err := strictGatewayRunning(filepath.Join(h, "gateway.pid"))
		if err != nil {
			probeErr = err
			return false, true
		}
		return !running, !running
	}
	drain := response.GetOr("drain_timeout", 30.0)
	seconds, err := drainTimeoutSeconds(drain)
	if err != nil {
		return err
	}
	timeout := min(120.0, max(5.0, seconds+5.0))
	exited := pydeadline.PollUntil(gone, time.Duration(timeout*float64(time.Second)), 50*time.Millisecond)
	if probeErr != nil {
		return probeErr
	}
	if !exited {
		return holderErrorf("gateway did not exit for %s; no data removed", h)
	}
	return nil
}

// Refuses while a serve or dashboard backend recorded in the spawn ledger is still running
func refuseBackendWriters(h string) error {
	root, err := pypath.Resolve(home.RootOf(h))
	if err != nil {
		return err
	}
	ledger := filepath.Join(root, tools.LedgerFilename)
	rows, ok := tools.ReadSpawnLedger(ledger)
	if !ok {
		return holderErrorf("cannot read backend ownership: %s", ledger)
	}
	for _, row := range rows {
		purpose, _ := row.Get("purpose")
		if purpose != "serve" && purpose != "dashboard" {
			continue
		}
		rawPid, _ := row.Get("pid")
		pid, isInt := jsonPyInt(rawPid)
		if !isInt || pid <= 0 {
			return holderErrorf("backend ownership contains an invalid PID")
		}
		if tools.EntryStillRunning(row.Vals) {
			return holderErrorf("backend PID %d still owns %s; close its desktop/dashboard before retrying", pid, h)
		}
	}
	return nil
}

// Python value of one sqlite column the way sqlite3.Row hands it back
func sqliteColumnValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case int:
		return int64(x)
	}
	return v
}

// Refuses while a claimed or running cron execution of the home is still owned by a live process
func refuseCronWriters(h string) error {
	path := filepath.Join(h, "cron", "executions.db")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return err
	}
	uri, err := pypath.AsURI(resolved)
	if err != nil {
		return err
	}
	inspectErr := func(err error) error {
		return holderErrorf("cannot inspect cron owners in %s: %s", path, state.ErrText(err))
	}
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=busy_timeout(250)")
	if err != nil {
		return inspectErr(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT * FROM executions WHERE status IN ('claimed','running')")
	if err != nil {
		return inspectErr(err)
	}
	columns, err := rows.Columns()
	if err != nil {
		rows.Close()
		return inspectErr(err)
	}
	var records []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			rows.Close()
			return inspectErr(err)
		}
		record := map[string]any{}
		for i, name := range columns {
			record[name] = sqliteColumnValue(values[i])
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return inspectErr(err)
	}
	rows.Close()
	for _, row := range records {
		pid, isInt := jsonPyInt(row["pid"])
		if pyrepr.Truthy(row["handoff_pending"]) {
			return holderErrorf("cron execution %s is being handed to a worker; wait for it to finish", pyrepr.Str(row["id"]))
		}
		if !isInt {
			return fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(row["pid"]))
		}
		if !gateway.PidExists(pid) {
			continue
		}
		current := holdersProcessStartTime(pid)
		recorded := row["process_started_at"]
		if current == nil || recorded == nil || pyrepr.Equal(current, recorded) {
			return holderErrorf("cron execution %s still owns %s (PID %d); stop it before retrying", pyrepr.Str(row["id"]), h, pid)
		}
	}
	return nil
}

// Refuses a named profile wipe while the default multiplexer serves it or its scope cannot be proven
func refuseMultiplexer(h string) error {
	root, err := pypath.Resolve(home.RootOf(h))
	if err != nil {
		return err
	}
	if pathEq(h, root) {
		return nil
	}
	identity := holdersIdentifyGateway(root)
	if identity != nil {
		served := identity.GetOr("served_profiles", []any{})
		name := pypath.PathName(h)
		listed := false
		switch x := served.(type) {
		case []any:
			listed = slices.ContainsFunc(x, func(v any) bool { return pyrepr.Equal(v, name) })
		case string:
			listed = strings.Contains(x, name)
		default:
			return fmt.Errorf("argument of type '%s' is not iterable", pyrepr.TypeName(served))
		}
		if listed {
			return holderErrorf("the default gateway serves %s; stop the multiplexer explicitly before retrying", h)
		}
		return nil
	}
	running, err := strictGatewayRunning(filepath.Join(root, "gateway.pid"))
	if err != nil {
		return err
	}
	if running {
		return holderErrorf("the default gateway's profile scope is unverified; stop it explicitly before retrying")
	}
	return nil
}

// Runs body while every writer lock of the home is held, refusing when any live writer still owns it
func quiescentHome(h string, body func() error) error {
	h, err := pypath.Resolve(h)
	if err != nil {
		return err
	}
	for _, check := range []func(string) error{refuseBackendWriters, refuseMultiplexer, drainManualGateway} {
		if err := check(h); err != nil {
			return err
		}
	}
	var releases []func()
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	releaseStore, err := tools.CheckpointStoreLock(filepath.Join(h, "checkpoints"))
	if err != nil {
		return err
	}
	releases = append(releases, releaseStore)
	gatewayLock, err := os.OpenFile(filepath.Join(h, "gateway.lock"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	releases = append(releases, func() { gatewayLock.Close() })
	if !gateway.TryAcquireFileLock(gatewayLock) {
		return holderErrorf("gateway is still active for %s", h)
	}
	releases = append(releases, func() { gateway.ReleaseFileLock(gatewayLock) })
	releaseSessions, err := activesessions.FileLock(filepath.Join(h, "runtime", "active_sessions.lock"), activesessions.NoLockTimeout)
	if err != nil {
		var unavailable *activesessions.LockUnavailableError
		if errors.As(err, &unavailable) {
			return holderErrorf("%s", unavailable.Error())
		}
		return err
	}
	releases = append(releases, releaseSessions)
	entries, err := activesessions.ReadEntries(filepath.Join(h, "runtime", "active_sessions.json"), true)
	if err != nil {
		return err
	}
	sessions, err := activesessions.PruneDead(entries, true)
	if err != nil {
		return err
	}
	if len(sessions) > 0 {
		seen := map[int]bool{}
		var pids []int
		for _, entry := range sessions {
			pid, _ := jsonPyInt(entry.GetOr("pid", nil))
			if !seen[pid] {
				seen[pid] = true
				pids = append(pids, pid)
			}
		}
		slices.Sort(pids)
		text := make([]string, len(pids))
		for i, pid := range pids {
			text[i] = strconv.Itoa(pid)
		}
		return holderErrorf("live chat sessions still own %s (PIDs [%s]); close them before retrying", h, strings.Join(text, ", "))
	}
	for _, path := range []string{filepath.Join(h, ".backup.lock"), filepath.Join(h, "cron", ".tick.lock")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return err
		}
		handle, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
		if err != nil {
			return err
		}
		releases = append(releases, func() { handle.Close() })
		locked, err := pm.LockFD(handle, false, 0)
		if err != nil {
			return err
		}
		if !locked {
			return holderErrorf("an active writer still owns %s", path)
		}
	}
	if err := refuseCronWriters(h); err != nil {
		return err
	}
	return body()
}
