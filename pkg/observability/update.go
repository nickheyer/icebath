package observability

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	PendingUpdatesDirname  = "pending_updates"
	RecordedUpdatesDirname = "recorded_updates"
	recordedUpdatesKeep    = 64
)

var (
	updateIDPattern      = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	runOutcomes          = map[string]string{"success": "success", "refused": "refused", "noop": "noop"}
	fleetBadStates       = Set("stale", "down")
	desktopApplyModes    = map[string]string{"electron-updater": "package", "app-installer": "package", "microsoft-store": "package", "windows-handoff": "git", "posix-handoff": "git"}
	exceptionStopReason  = regexp.MustCompile(`^([A-Z][A-Za-z0-9_]*):(?: |\n?\z)`)
	pmErrorTypes         = Set("BuildFailure", "DownloadError", "DownloadPaused", "DownloadTransportError", "FeatureProbeError", "HashError", "InstallError", "PinMismatch", "ResolutionConflict", "StaleLockRow")
	osErrorTypes         = builtinOSErrorNames()
	subprocessErrorTypes = Set("CalledProcessError", "SubprocessError", "TimeoutExpired")
	stopReasonPrefixes   = [][2]string{{"historical takeover preparation failed", "deps_failed"}, {"Windows gateway recovery failed", "restart_failed"}}
	diskFullErrno        = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*: \[(?:Errno 28|WinError (?:39|112))\]`)
	exceptionTypeClasses = []struct {
		types ValueSet
		class string
	}{{Set("PermissionError"), "permission_denied"}, {pmErrorTypes, "deps_failed"}, {osErrorTypes, "os_error"}, {subprocessErrorTypes, "subprocess_failed"}}
)

// Names of the builtin OSError subclasses, WindowsError joining them on Windows
func builtinOSErrorNames() ValueSet {
	names := Set("OSError", "EnvironmentError", "IOError", "BlockingIOError", "ChildProcessError", "ConnectionError",
		"BrokenPipeError", "ConnectionAbortedError", "ConnectionRefusedError", "ConnectionResetError", "FileExistsError",
		"FileNotFoundError", "InterruptedError", "IsADirectoryError", "NotADirectoryError", "PermissionError",
		"ProcessLookupError", "TimeoutError")
	if runtime.GOOS == "windows" {
		return names.Union(Set("WindowsError"))
	}
	return names
}

// pyTypeError is a Python TypeError raised while reading a malformed receipt
type pyTypeError string

func (e pyTypeError) Error() string { return string(e) }

// Python membership of a value in a string set, unhashable lists and dicts raising TypeError
func pyMember(v any, s ValueSet) bool {
	switch t := v.(type) {
	case string:
		return s.Has(t)
	case []any, map[string]any, *pyjson.Object:
		panic(pyTypeError(fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(t))))
	}
	return false
}

// Items Python iterates for value or (), a truthy non iterable raising TypeError
func iterOrEmpty(v any) []any {
	if !pyrepr.Truthy(v) {
		return nil
	}
	switch t := v.(type) {
	case []any:
		return t
	case *pyjson.Object:
		out := make([]any, len(t.Keys))
		for i, k := range t.Keys {
			out[i] = k
		}
		return out
	case map[string]any:
		out := make([]any, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	}
	panic(pyTypeError(fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))))
}

// Dict get on a receipt value, None for anything that is not a dict
func dictGet(v any, key string) any {
	d, _ := asDict(v)
	return d[key]
}

// Python value == number for ints and floats
func numberEquals(v any, n float64) bool {
	f, ok := pyNumber(v)
	return ok && f == n
}

// Whether a fleet row is stale or down
func fleetBad(receipt map[string]any) bool {
	fleet, ok := receipt["fleet"].([]any)
	if !ok {
		return false
	}
	for _, row := range fleet {
		if d, ok := asDict(row); ok && pyMember(d["state"], fleetBadStates) {
			return true
		}
	}
	return false
}

// Whether the gateway restart or a runtime reported it did not finish
func restartIncomplete(receipt map[string]any) bool {
	if restart, ok := asDict(receipt["gateway_restart"]); ok {
		if restart["incomplete"] == true || pyrepr.Truthy(restart["phase_error"]) || pyrepr.Truthy(restart["failed_units"]) {
			return true
		}
	}
	outcomes, ok := receipt["runtime_outcomes"].([]any)
	if !ok {
		return false
	}
	for _, row := range outcomes {
		if d, ok := asDict(row); ok && d["outcome"] == "failed" {
			return true
		}
	}
	return false
}

// updateStage is one stage mark with its duration since the previous mark
type updateStage struct {
	fields   map[string]any
	duration any
}

func (s updateStage) name() string { return s.fields["name"].(string) }

// Class the run named itself, the parked user changes or a pre apply stop class
func namedStop(receipt map[string]any, stages []updateStage) string {
	if receipt["outcome"] == "partial" && pyrepr.Truthy(receipt["user_action"]) {
		return "local_changes_parked"
	}
	stopClass := receipt["stop_class"]
	if pyMember(stopClass, UpdateStopClasses) {
		for _, s := range stages {
			if s.name() == "apply" {
				return ""
			}
		}
		return stopClass.(string)
	}
	return ""
}

// UpdateFailureClass is why a failed or refused run stopped, read from the final receipt
func UpdateFailureClass(receipt map[string]any, stages []updateStage, outcome string) string {
	if outcome != "failed" && outcome != "refused" {
		return "none"
	}
	steps, _ := receipt["steps"].([]any)
	for _, s := range steps {
		if d, ok := asDict(s); ok && d["name"] == "admission" && !pyrepr.Truthy(d["ok"]) {
			return "managed_install"
		}
	}
	reason, _ := receipt["stop_reason"].(string)
	exitCode := receipt["exit_code"]
	excType := ""
	if m := exceptionStopReason.FindStringSubmatch(reason); m != nil {
		excType = m[1]
	}
	if excType == "KeyboardInterrupt" || numberEquals(exitCode, 130) {
		return "interrupted"
	}
	if named := namedStop(receipt, stages); named != "" {
		return named
	}
	if outcome == "refused" {
		if numberEquals(exitCode, 2) {
			return "lock_held"
		}
		return "other"
	}
	failedMarks, marked := map[string]bool{}, map[string]bool{}
	for _, s := range stages {
		marked[s.name()] = true
		if s.fields["outcome"] == "failed" {
			failedMarks[s.name()] = true
		}
	}
	switch {
	case failedMarks["build"]:
		return "build_failed"
	case fleetBad(receipt):
		return "fleet_stale"
	case failedMarks["restart"] || restartIncomplete(receipt):
		return "restart_failed"
	case receipt["outcome"] == "partial":
		return "fleet_unverified"
	case len(stages) > 0 && stages[len(stages)-1].name() == "restart" && stages[len(stages)-1].fields["outcome"] == "skipped":
		return "restart_failed"
	}
	for _, p := range stopReasonPrefixes {
		if strings.HasPrefix(reason, p[0]) {
			return p[1]
		}
	}
	if diskFullErrno.MatchString(reason) {
		return "disk_full"
	}
	if excType != "" {
		for _, c := range exceptionTypeClasses {
			if c.types.Has(excType) {
				return c.class
			}
		}
		return "exception"
	}
	if marked["apply"] {
		if !marked["deps"] {
			return "deps_failed"
		}
		if !marked["build"] {
			return "build_failed"
		}
		return "other"
	}
	if _, ok := receipt["schema"]; !ok {
		return "other"
	}
	if reason == "" && exitCode == nil {
		return "git_failed"
	}
	return "aborted_before_apply"
}

// Seconds since the epoch of a number or an ISO timestamp, naive ones in local time, false when unreadable
func epoch(value any) (float64, bool) {
	if _, isBool := value.(bool); !isBool {
		if _, isInt := pyInt(value); isInt {
			return pyNumber(value)
		}
		if f, ok := pyNumber(value); ok {
			return f, true
		}
	}
	t, aware, err := pydatetime.ParseISOFormat(pyrepr.Str(value))
	if err != nil {
		return 0, false
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	}
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9, true
}

// Milliseconds between two timestamps, nil when either is unreadable or the end precedes the start
func elapsedMs(start, end any) any {
	first, ok1 := epoch(start)
	last, ok2 := epoch(end)
	if !ok1 || !ok2 || last < first {
		return nil
	}
	return (last - first) * 1000
}

// Stage marks in order with durations, plus a verify stage when the fleet matrix was passed
func receiptStages(receipt map[string]any) []updateStage {
	var stages []updateStage
	previous := receipt["started_at"]
	for _, raw := range iterOrEmpty(receipt["stages"]) {
		mark, ok := asDict(raw)
		if !ok || !pyMember(mark["name"], UpdateStages) {
			continue
		}
		stages = append(stages, updateStage{mark, elapsedMs(previous, mark["at"])})
		if pyrepr.Truthy(mark["at"]) {
			previous = mark["at"]
		}
	}
	fleet, isList := receipt["fleet"].([]any)
	if !isList || len(fleet) == 0 {
		return stages
	}
	anyFailed := false
	for _, s := range stages {
		if s.name() == "verify" {
			return stages
		}
		if s.fields["outcome"] == "failed" {
			anyFailed = true
		}
	}
	outcome := "success"
	if fleetBad(receipt) || (receipt["outcome"] == "partial" && !anyFailed) {
		outcome = "failed"
	}
	return append(stages, updateStage{map[string]any{"name": "verify", "outcome": outcome}, elapsedMs(previous, receipt["finished_at"])})
}

// Where a failed or refused run stopped, the failing stage or the stage it never reached
func failedStage(stages []updateStage) string {
	if len(stages) == 0 {
		return "other"
	}
	last := stages[len(stages)-1]
	terminal := last.name() == "verify" || (last.name() == "restart" && last.fields["outcome"] == "skipped")
	if last.fields["outcome"] == "failed" || terminal {
		failed := ""
		for _, s := range stages {
			if s.fields["outcome"] == "failed" {
				failed = s.name()
			}
		}
		if failed != "" {
			return failed
		}
		return last.name()
	}
	for i, name := range UpdateStageOrder {
		if name == last.name() && i+1 < len(UpdateStageOrder) {
			return UpdateStageOrder[i+1]
		}
	}
	return "other"
}

// How the run applied its code, external when an admission step refused it
func applyMode(receipt map[string]any, stages []updateStage) string {
	for _, s := range stages {
		if s.name() == "apply" && pyMember(s.fields["mode"], Set("git", "zip")) {
			return s.fields["mode"].(string)
		}
	}
	for _, raw := range iterOrEmpty(receipt["steps"]) {
		if d, ok := asDict(raw); ok && d["name"] == "admission" && !pyrepr.Truthy(d["ok"]) {
			return "external"
		}
	}
	return "unknown"
}

// Whether the code moved and the run closed owing work rather than failing
func committedWithWorkOwed(receipt map[string]any) bool {
	applied := false
	for _, raw := range iterOrEmpty(receipt["stages"]) {
		if d, ok := asDict(raw); ok && d["name"] == "apply" && d["outcome"] != "failed" {
			applied = true
		}
	}
	owed := receipt["outcome"] == "interrupted" || (receipt["outcome"] == "partial" && pyrepr.Truthy(receipt["user_action"]))
	return applied && owed
}

// UpdateFields holds the bounded update run row and its stage rows
type UpdateFields struct {
	Run    map[string]string
	Stages []map[string]string
}

// UpdateReceiptFields derives the run and stage rows of a final receipt, nil when it is not one
func UpdateReceiptFields(receipt any) (fields *UpdateFields, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(pyTypeError)
			if !ok {
				panic(r)
			}
			fields, err = nil, e
		}
	}()
	rec, ok := asDict(receipt)
	if !ok || !pyrepr.Truthy(rec["finished_at"]) {
		return nil, nil
	}
	stages := receiptStages(rec)
	outcome, known := runOutcomes[runOutcomeText(rec["outcome"])]
	if !known {
		outcome = "failed"
	}
	if outcome == "success" {
		for _, s := range stages {
			if s.name() == "apply" && s.fields["outcome"] == "skipped" {
				outcome = "noop"
				break
			}
		}
	}
	if committedWithWorkOwed(rec) {
		outcome = "partial"
	}
	var ageMS any
	started, startedOK := epoch(rec["started_at"])
	if committed, ok := epoch(dictGetOrRaise(rec["pre_update"], "commit_date")); ok && startedOK {
		ageMS = (started - committed) * 1000
	}
	classOutcome := outcome
	if outcome == "partial" {
		classOutcome = "failed"
	}
	failureClass := UpdateFailureClass(rec, stages, classOutcome)
	failed := "none"
	if failureClass == "local_changes_parked" {
		failed = "apply"
	} else if outcome == "failed" || outcome == "refused" || outcome == "partial" {
		failed = failedStage(stages)
	}
	kind := "cli"
	if rec["initiator"] == "desktop" {
		kind = "desktop"
	}
	run := map[string]string{
		"apply_mode": applyMode(rec, stages), "duration_bucket": UpdateDurationBucket(elapsedMs(rec["started_at"], rec["finished_at"])),
		"failed_stage": failed, "from_version_age_bucket": VersionAgeBucket(ageMS), "kind": kind, "outcome": outcome,
		"failure_class": failureClass,
	}
	if outcome == "refused" && len(stages) == 0 {
		run["failed_stage"] = "none"
	}
	rows := make([]map[string]string, 0, len(stages)+1)
	for _, s := range stages {
		out := "failed"
		if pyMember(s.fields["outcome"], Set("success", "failed", "skipped")) {
			out = s.fields["outcome"].(string)
		}
		rows = append(rows, map[string]string{"duration_bucket": UpdateDurationBucket(s.duration), "outcome": out, "stage": s.name()})
	}
	if diedIn := unmarkedFailedStage(run, stages); diedIn != "" {
		lastMark := rec["started_at"]
		for i := len(stages) - 1; i >= 0; i-- {
			if pyrepr.Truthy(stages[i].fields["at"]) {
				lastMark = stages[i].fields["at"]
				break
			}
		}
		rows = append(rows, map[string]string{"duration_bucket": UpdateDurationBucket(elapsedMs(lastMark, rec["finished_at"])), "outcome": "failed", "stage": diedIn})
	}
	return &UpdateFields{Run: run, Stages: rows}, nil
}

// Python str(value or "") of a receipt outcome
func runOutcomeText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Python (value or {}).get(key), a truthy non dict raising AttributeError
func dictGetOrRaise(v any, key string) any {
	if !pyrepr.Truthy(v) {
		return nil
	}
	d, ok := asDict(v)
	if !ok {
		panic(pyTypeError(fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))))
	}
	return d[key]
}

// Stage a failed run stopped in when the receipt holds no mark for it, empty otherwise
func unmarkedFailedStage(run map[string]string, stages []updateStage) string {
	diedIn := run["failed_stage"]
	if run["outcome"] != "failed" || !UpdateStages.Has(diedIn) {
		return ""
	}
	for _, s := range stages {
		if s.name() == diedIn {
			return ""
		}
	}
	return diedIn
}

// Cheap gate reading telemetry.shared_metrics.enabled as exactly true, panicking when the config is unreadable
var collectionOn = func(ctx context.Context) bool {
	raw, err := config.ReadRawConfigReadonly(ctx, home.Dir(ctx))
	if err != nil {
		panic(err)
	}
	var node any = raw
	for _, key := range []string{"telemetry", "shared_metrics"} {
		node = dictGet(node, key)
	}
	d, ok := asDict(node)
	return ok && d["enabled"] == true
}

// Latches an update id once per profile, false when already counted, pruning to the newest latches
func claimUpdateID(ctx context.Context, updateID any) (first bool, latch string) {
	id, ok := updateID.(string)
	if !ok || !updateIDPattern.MatchString(id) {
		return true, ""
	}
	dir := filepath.Join(home.Dir(ctx), "telemetry", "shared_metrics", RecordedUpdatesDirname)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		panic(err)
	}
	latch = filepath.Join(dir, id)
	f, err := os.OpenFile(latch, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o777)
	if errors.Is(err, fs.ErrExist) {
		return false, ""
	}
	if err != nil {
		panic(err)
	}
	f.Close()
	pruneRecordedUpdates(dir)
	return true, latch
}

// Deletes every update latch past the newest kept ones, stopping on the first filesystem error
func pruneRecordedUpdates(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type latchFile struct {
		path  string
		mtime time.Time
	}
	files := make([]latchFile, 0, len(entries))
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			return
		}
		files = append(files, latchFile{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	for i := recordedUpdatesKeep; i < len(files); i++ {
		if err := os.Remove(files[i].path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return
		}
	}
}

// RecordUpdateReceipt records a final receipt's run and stage rows once per update id, false to keep it for a retry
func RecordUpdateReceipt(ctx context.Context, receipt any, waitSaved bool) (settled bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Update shared metrics not recorded", "error", r)
			settled = false
		}
	}()
	if !collectionOn(ctx) {
		return true
	}
	derived, err := UpdateReceiptFields(receipt)
	if err != nil {
		panic(err)
	}
	if derived == nil {
		return true
	}
	rec, _ := asDict(receipt)
	first, latch := claimUpdateID(ctx, rec["update_id"])
	if !first {
		return true
	}
	rows := []Mark{{UpdateRunMark, derived.Run}}
	for _, row := range derived.Stages {
		rows = append(rows, Mark{UpdateStageMark, row})
	}
	if !waitSaved {
		for _, row := range rows {
			emit(ctx, row.Name, func() map[string]string { return row.Data })
		}
		return true
	}
	if EmitSaved(ctx, rows) > 0 {
		return true
	}
	if latch != "" {
		if err := os.Remove(latch); err != nil && !errors.Is(err, fs.ErrNotExist) {
			panic(err)
		}
	}
	return false
}

// PendingUpdatesDir is where a pre pull interpreter parks receipts for the next start
func PendingUpdatesDir(h string) string {
	return filepath.Join(h, "telemetry", "shared_metrics", PendingUpdatesDirname)
}

// Removes every parked update receipt of a home, ignoring errors
var purgePendingUpdates = func(h string) { _ = os.RemoveAll(PendingUpdatesDir(h)) }

// Records each receipt a pre pull interpreter parked, claimed so one start counts it
var reportPendingUpdates = func(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Pending update shared metrics not reported", "error", r)
		}
	}()
	dir := PendingUpdatesDir(home.Dir(ctx))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !strings.Contains(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		claimed := claim(path)
		if claimed == "" {
			continue
		}
		receipt, isDict := readRecord(claimed)
		saved := !isDict || RecordUpdateReceipt(ctx, receipt, true)
		SettleClaim(claimed, path, saved)
	}
}

// DesktopUpdateFields is the update run row of a Desktop packaged self update
func DesktopUpdateFields(outcome, failedStage, durationMS, mechanism, fromCommitDate any) map[string]string {
	outcomeValue := pystr.Lower(pystr.Strip(runOutcomeText(outcome)))
	if !UpdateOutcomes.Has(outcomeValue) {
		outcomeValue = "failed"
	}
	stage := pystr.Lower(pystr.Strip(runOutcomeText(failedStage)))
	var ageMS any
	if commit, ok := epoch(fromCommitDate); ok {
		ageMS = (float64(time.Now().UnixNano())/1e9 - commit) * 1000
	}
	mode, ok := desktopApplyModes[pystr.Lower(pystr.Strip(runOutcomeText(mechanism)))]
	if !ok {
		mode = "unknown"
	}
	failed := "none"
	if outcomeValue == "failed" {
		failed = "other"
		if DesktopUpdateStages.Has(stage) {
			failed = stage
		}
	}
	failureClass := "none"
	if outcomeValue == "failed" || outcomeValue == "refused" {
		failureClass = "unknown"
	}
	return map[string]string{
		"apply_mode": mode, "duration_bucket": UpdateDurationBucket(durationMS), "failed_stage": failed,
		"from_version_age_bucket": VersionAgeBucket(ageMS), "kind": "desktop", "outcome": outcomeValue,
		"failure_class": failureClass,
	}
}

// RecordDesktopUpdate emits one Desktop self update run from its keyword arguments, nothing when they do not fit
func RecordDesktopUpdate(ctx context.Context, raw map[string]any) {
	emit(ctx, UpdateRunMark, func() map[string]string {
		for key := range raw {
			if !Set("outcome", "failed_stage", "duration_ms", "mechanism", "from_commit_date").Has(key) {
				panic(pyTypeError(fmt.Sprintf("desktop_update_fields() got an unexpected keyword argument '%s'", key)))
			}
		}
		for _, key := range []string{"outcome", "failed_stage", "duration_ms", "mechanism"} {
			if _, ok := raw[key]; !ok {
				panic(pyTypeError(fmt.Sprintf("desktop_update_fields() missing 1 required keyword-only argument: '%s'", key)))
			}
		}
		return DesktopUpdateFields(raw["outcome"], raw["failed_stage"], raw["duration_ms"], raw["mechanism"], raw["from_commit_date"])
	})
}
