package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	gatewayKind                       = "icebath-gateway"
	runtimeStatusFile                 = "gateway_state.json"
	locksDirname                      = "gateway-locks"
	gatewayLockFilename               = "gateway.lock"
	windowsLockOffset                 = 1024 * 1024
	gatewayRunningPidCacheTTL         = time.Second
	epochMinPlausible         float64 = 946684800.0
	// Same host start time drift tolerance, 2 s on either fingerprint scale
	StartTimeDriftTolerance = 200
)

// Exit reasons the out of loop watchdogs stamp with a degraded state before a hard exit
var WatchdogExitReasons = map[string]bool{"loop_liveness_watchdog": true, "shutdown_watchdog": true}

// Seams over the process facts status reads, replaced by tests the way hermes tests monkeypatch the module
var (
	statusIsWindows               = runtime.GOOS == "windows"
	statusGetpid                  = os.Getpid
	statusPidExists               = pidExistsLive
	statusProcessStartTime        = processStartTimeLive
	statusReadProcessCmdline      = readProcessCmdlineLive
	statusLooksLikeGatewayProcess = looksLikeGatewayProcessLive
	statusBuildPidRecord          = buildPidRecordLive
	statusProfileLabelForHome     = profileLabelForHomeLive
	statusReplace                 = os.Rename
	statusRecordArgv              = recordArgvLive
)

// Python value of a record key, nil when the record or the key is missing
func recGet(r *pyjson.Object, key string) any {
	if r == nil {
		return nil
	}
	v, _ := r.Get(key)
	return v
}

// Python equality of two decoded JSON values, numbers compared by value across int, float and bool
func pyEq(a, b any) bool {
	ra, aNum := pyNumber(a)
	rb, bNum := pyNumber(b)
	if aNum || bNum {
		return aNum && bNum && ra != nil && rb != nil && ra.Cmp(rb) == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	}
	return pyrepr.Repr(a) == pyrepr.Repr(b)
}

// Exact rational of a Python number, nil for NaN and infinities which equal nothing finite
func pyNumber(v any) (*big.Rat, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewRat(1, 1), true
		}
		return big.NewRat(0, 1), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, true
		}
		return new(big.Rat).SetFloat64(t), true
	}
	if n, ok := pyrepr.Int(v); ok {
		return new(big.Rat).SetInt(n), true
	}
	return nil, false
}

// Whether v is a Python int that is not a bool
func pyIsInt(v any) bool {
	if _, ok := v.(bool); ok {
		return false
	}
	_, ok := pyrepr.Int(v)
	return ok
}

// Launch home that owns the identity files, never the per session override
func processHermesHome() string { return home.Process() }

// Stable absolute home path for persisted identity data
func canonicalHermesHome(p string) (string, error) {
	return pypath.Realpath(pypath.Expanduser(p), pypath.Lax)
}

// Compares two home paths with the host platform case rules for callers outside this package
func SameHermesHome(left, right string) (bool, error) { return sameHermesHome(left, right) }

// Compares two home paths with the host platform case rules
func sameHermesHome(left, right string) (bool, error) {
	l, err := canonicalHermesHome(left)
	if err != nil {
		return false, err
	}
	r, err := canonicalHermesHome(right)
	if err != nil {
		return false, err
	}
	return pypath.Normcase(l) == pypath.Normcase(r), nil
}

// Whether a persisted record names a different home than expected, failing closed when the comparison errors
func RecordedGatewayHomeConflicts(record *pyjson.Object, expectedHome string) bool {
	recorded, ok := recGet(record, "hermes_home").(string)
	if !ok || pystr.Strip(recorded) == "" {
		return false
	}
	base := expectedHome
	if base == "" {
		base = processHermesHome()
	}
	same, err := sameHermesHome(recorded, base)
	return err != nil || !same
}

var profileLabelRe = regexp.MustCompile(`\A[a-z0-9][a-z0-9_-]{0,63}\n?\z`)

// Profile label of a home for callers outside this package, empty when unknown
func ProfileLabelForHome(h string) string { return profileLabelForHomeLive(h) }

// Profile label of a home, the profiles child name, default for the root home, empty when unknown
func profileLabelForHomeLive(h string) string {
	canonical, err := canonicalHermesHome(h)
	if err != nil {
		return ""
	}
	if pypath.PathName(pypath.PathParent(canonical)) == "profiles" && profileLabelRe.MatchString(pypath.PathName(canonical)) {
		return pypath.PathName(canonical)
	}
	for _, defaultHome := range []func() string{home.Root, home.PlatformDefault} {
		if same, err := sameHermesHome(canonical, defaultHome()); err == nil && same {
			return "default"
		}
	}
	return ""
}

// Profile label of a scoped lock owner from its profile field or its recorded home, empty when unknown
func ScopedLockOwnerLabel(record any) string {
	r, ok := record.(*pyjson.Object)
	if !ok {
		return ""
	}
	if profile, ok := recGet(r, "profile").(string); ok && profileLabelRe.MatchString(pystr.Strip(profile)) {
		return pystr.Strip(profile)
	}
	if h, ok := recGet(r, "hermes_home").(string); ok && pystr.Strip(h) != "" {
		return statusProfileLabelForHome(h)
	}
	return ""
}

// Sibling of path named name, Path.with_name
func pathWithName(path, name string) string { return pypath.PathJoin(pypath.PathParent(path), name) }

func pidPath() string { return filepath.Join(processHermesHome(), "gateway.pid") }

// Lock path beside the pid file, the process pid file when pidPath is empty
func gatewayLockPathLive(p string) string {
	if p == "" {
		p = pidPath()
	}
	return pypath.PathJoin(pypath.PathParent(p), gatewayLockFilename)
}

var statusGatewayLockPath = gatewayLockPathLive

func runtimeStatusPath() string { return filepath.Join(processHermesHome(), runtimeStatusFile) }

// Per user rendezvous directory for machine local locks, ICEBATH_GATEWAY_LOCK_DIR overriding
func GatewayLockDir() string {
	if override := os.Getenv("ICEBATH_GATEWAY_LOCK_DIR"); override != "" {
		return pypath.PathStr(override)
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if !pypath.IsAbs(stateHome) {
		stateHome = pypath.PathJoin(pypath.Expanduser("~"), ".local", "state")
	}
	return pypath.PathJoin(stateHome, "icebath", locksDirname)
}

func utcNowISO() string { return pydatetime.ISOFormat(time.Now().UTC().Truncate(time.Microsecond)) }

// Persisted updated_at coerced to an RFC3339 string, false for garbage, an error for an int too large for float
func NormalizeUpdatedAt(value any) (string, bool, error) {
	switch t := value.(type) {
	case string:
		raw := pystr.Strip(t)
		if strings.HasSuffix(raw, "Z") || strings.HasSuffix(raw, "z") {
			raw = raw[:len(raw)-1] + "+00:00"
		}
		parsed, aware, err := pydatetime.ParseISOFormat(raw)
		if err != nil {
			return "", false, nil
		}
		if !aware {
			parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.UTC)
		}
		return pydatetime.ISOFormat(parsed), true, nil
	case bool:
		return "", false, nil
	}
	if _, isNum := pyNumber(value); !isNum {
		return "", false, nil
	}
	seconds, err := pyint.Float(value)
	if err != nil {
		return "", false, err
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < epochMinPlausible || seconds > now+86400 {
		return "", false, nil
	}
	whole, frac := math.Modf(seconds)
	us := math.RoundToEven(frac * 1e6)
	if us >= 1e6 {
		whole++
		us -= 1e6
	}
	return pydatetime.ISOFormat(time.Unix(int64(whole), int64(us)*1000).UTC()), true, nil
}

// State a not running gateway's retained record reports, startup_failed or watchdog degraded while still wanted
func RetainedGatewayState(rt any) string {
	r, _ := rt.(*pyjson.Object)
	if !pyEq(recGet(r, "desired_state"), "stopped") {
		state := recGet(r, "gateway_state")
		if pyEq(state, "startup_failed") {
			return "startup_failed"
		}
		if reason, ok := recGet(r, "exit_reason").(string); ok && pyEq(state, "degraded") && WatchdogExitReasons[reason] {
			return "degraded"
		}
	}
	return "stopped"
}

// Error kinds terminate_pid raises, OSError refusals and the os.kill failures
var (
	ErrRefusedKill = errors.New("refused kill")
)

// Terminates pid with SIGTERM or a guarded force kill, taskkill tree kill for a Windows force
func TerminatePid(pid int, force bool, expectedStartTime any) error {
	if force && (statusIsWindows || expectedStartTime != nil) {
		if expectedStartTime == nil {
			return fmt.Errorf("%w: refusing to force-kill PID %d without a process start-time guard", ErrRefusedKill, pid)
		}
		current := statusProcessStartTime(pid)
		if current == nil {
			return fmt.Errorf("%w: refusing to force-kill PID %d; process start time is unavailable", ErrRefusedKill, pid)
		}
		agree, err := startTimesAgree(current, expectedStartTime)
		if err != nil {
			return fmt.Errorf("%w: refusing to force-kill PID %d; malformed start time", ErrRefusedKill, pid)
		}
		if !agree {
			return fmt.Errorf("%w: refusing to force-kill PID %d; process identity changed", ErrRefusedKill, pid)
		}
	}
	if !(force && statusIsWindows) {
		return killPid(pid, force)
	}
	code, stdout, stderr, err := statusTaskkill(pid)
	if errors.Is(err, fs.ErrNotExist) {
		return killPid(pid, false)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		details := pystr.Strip(stderr)
		if stderr == "" {
			details = pystr.Strip(stdout)
		}
		if details == "" {
			details = fmt.Sprintf("taskkill failed for PID %d", pid)
		}
		return errors.New(details)
	}
	return nil
}

// Whether every recorded fingerprint is positive and within a millisecond of current, an error for junk
func startTimesAgree(current any, recorded ...any) (bool, error) {
	cur, err := pyint.Float(current)
	if err != nil {
		return false, err
	}
	if !(cur > 0) {
		return false, nil
	}
	for _, r := range recorded {
		f, err := pyint.Float(r)
		if err != nil {
			return false, err
		}
		if !(f > 0 && math.Abs(f-cur) <= 0.001) {
			return false, nil
		}
	}
	return true, nil
}

// Whether two start time fingerprints are the same incarnation within tolerance, an error for junk
func StartTimeFingerprintsMatch(recorded, current any, tolerance int) (bool, error) {
	c, err := pyint.Of(current)
	if err != nil {
		return false, err
	}
	r, err := pyint.Of(recorded)
	if err != nil {
		return false, err
	}
	diff := new(big.Int).Sub(c, r)
	return diff.Abs(diff).Cmp(big.NewInt(int64(tolerance))) <= 0, nil
}

func scopeHash(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])[:16]
}

func scopeLockPath(scope, identity string) string {
	return pypath.PathJoin(GatewayLockDir(), scope+"-"+scopeHash(identity)+".lock")
}

// Start time fingerprint of pid, /proc ticks or psutil birth in centiseconds, nil when unavailable
func GetProcessStartTime(pid int) any { return statusProcessStartTime(pid) }

// Gateway lifecycle subcommand a live pid runs, read from its command line
func looksLikeGatewayProcessLive(pid int) bool {
	cmdline, ok := statusReadProcessCmdline(pid)
	return ok && cmdline != "" && LooksLikeGatewayCommandLine(cmdline)
}

// Whether persisted record metadata names a gateway runtime when the live command line is unreadable
func recordLooksLikeGateway(record *pyjson.Object) bool {
	argv, ok := recGet(record, "argv").([]any)
	if !pyEq(recGet(record, "kind"), gatewayKind) || !ok || len(argv) == 0 {
		return false
	}
	parts := make([]string, len(argv))
	for i, part := range argv {
		parts[i] = pyrepr.Str(part)
	}
	return LooksLikeGatewayRuntimeCommandLine(strings.Join(parts, " "))
}

// Process argv with an inline source placeholder replaced by the running executable
func recordArgvLive() []any {
	argv := make([]any, len(os.Args))
	for i, a := range os.Args {
		argv[i] = a
	}
	if len(argv) > 0 && argv[0] == "-c" {
		if exe, err := os.Executable(); err == nil {
			argv[0] = exe
		}
	}
	return argv
}

func buildPidRecordLive() *pyjson.Object {
	pid := statusGetpid()
	h, err := canonicalHermesHome(processHermesHome())
	if err != nil {
		panic(fmt.Sprintf("gateway: process home %q cannot be resolved: %v", processHermesHome(), err))
	}
	r := pyjson.NewObject()
	r.Set("pid", int64(pid))
	r.Set("kind", gatewayKind)
	r.Set("argv", statusRecordArgv())
	r.Set("start_time", statusProcessStartTime(pid))
	r.Set("hermes_home", h)
	return r
}

// Code identity of this process for gateway_state.json, empty when the identity is unavailable
func codeIdentityFields() [][2]any {
	identity, err := updaterecord.GetCodeIdentity(false)
	if err != nil {
		return nil
	}
	return [][2]any{{"code_sha", identity.Vals["sha"]}, {"code_version", identity.Vals["version"]}}
}

// Whether a record's home is this process home, a legacy record without one counting as ours
func pidRecordBelongsToCurrentProfile(record *pyjson.Object) (bool, error) {
	if record == nil {
		return false, nil
	}
	recordHome := recGet(record, "hermes_home")
	if !pyrepr.Truthy(recordHome) {
		return true, nil
	}
	s, ok := recordHome.(string)
	if !ok {
		return false, fmt.Errorf("TypeError: expected str, bytes or os.PathLike object, not %s", pyrepr.TypeName(recordHome))
	}
	return sameHermesHome(s, processHermesHome())
}

func buildRuntimeStatusRecord() *pyjson.Object {
	r := pyjson.NewObject()
	pidRecord := statusBuildPidRecord()
	for _, k := range pidRecord.Keys {
		r.Set(k, recGet(pidRecord, k))
	}
	r.Set("gateway_state", "starting")
	r.Set("exit_reason", nil)
	r.Set("restart_requested", false)
	r.Set("active_agents", int64(0))
	r.Set("platforms", pyjson.NewObject())
	store := pyjson.NewObject()
	store.Set("status", "unknown")
	r.Set("session_store", store)
	r.Set("updated_at", utcNowISO())
	for _, kv := range codeIdentityFields() {
		r.Set(kv[0].(string), kv[1])
	}
	return r
}

// JSON object stored at path, nil when absent, empty, undecodable or not an object, bare pids accepted on request
func readJSONFile(path string, barePidOK bool) *pyjson.Object {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return nil
	}
	raw := pystr.Strip(string(data))
	if raw == "" {
		return nil
	}
	payload, err := pyjson.Loads(raw)
	if err != nil {
		payload = nil
		if barePidOK {
			if n, ok := pyint.Parse(raw); ok {
				payload = pyint.Native(n)
			}
		}
	}
	if barePidOK {
		if _, isBool := payload.(bool); isBool || pyIsInt(payload) {
			r := pyjson.NewObject()
			r.Set("pid", payload)
			return r
		}
	}
	obj, _ := payload.(*pyjson.Object)
	return obj
}

// Writes a compact JSON record atomically
func writeJSONFile(path string, payload any) error {
	return config.AtomicJSONWriteWith(path, payload, pyjson.Separators(",", ":"))
}

func unlinkQuietly(path string) { _ = os.Remove(path) }

func readPidRecord(p string) *pyjson.Object {
	if p == "" {
		p = pidPath()
	}
	return readJSONFile(p, true)
}

func readGatewayLockRecord(p string) *pyjson.Object {
	if p == "" {
		p = statusGatewayLockPath("")
	}
	return readJSONFile(p, true)
}

// Integer pid stored under key, false when missing or not convertible like int()
func pidFromRecord(record *pyjson.Object, key string) (int, bool) {
	if record == nil {
		return 0, false
	}
	v, ok := record.Get(key)
	if !ok {
		return 0, false
	}
	n, err := pyint.Of(v)
	if err != nil || !n.IsInt64() || n.Int64() != int64(int(n.Int64())) {
		return 0, false
	}
	return int(n.Int64()), true
}

// Whether both start times are known and differ, the pid reuse guard
func startTimesConflict(recorded, current any) bool {
	return recorded != nil && current != nil && !pyEq(current, recorded)
}

// Record pid when it is alive and passes the start time reuse guard
func liveFromRecord(record *pyjson.Object) (int, bool) {
	pid, ok := pidFromRecord(record, "pid")
	if !ok || !statusPidExists(pid) {
		return 0, false
	}
	if startTimesConflict(recGet(record, "start_time"), statusProcessStartTime(pid)) {
		return 0, false
	}
	return pid, true
}

var (
	runningPidCacheMu sync.Mutex
	runningPidCache   = map[runningPidCacheKey]runningPidCacheEntry{}
)

type runningPidCacheKey struct {
	path          string
	cleanupStale  bool
	includeStatus bool
}

type fileSig struct {
	exists bool
	mtime  int64
	size   int64
}

type runningPidCacheEntry struct {
	at  time.Time
	sig []fileSig
	pid int
	ok  bool
}

func clearRunningPidCache() {
	runningPidCacheMu.Lock()
	defer runningPidCacheMu.Unlock()
	clear(runningPidCache)
}

func fileCacheSignature(p string) fileSig {
	st, err := os.Stat(p)
	if err != nil {
		return fileSig{}
	}
	return fileSig{true, st.ModTime().UnixNano(), st.Size()}
}

// Unlinks a stale pid file when cleanup is requested, the sibling lock inode always kept
func cleanupInvalidPidPath(p string, cleanupStale bool) {
	if !cleanupStale {
		return
	}
	clearRunningPidCache()
	_ = os.Remove(p)
}
