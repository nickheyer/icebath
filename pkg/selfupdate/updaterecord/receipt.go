package updaterecord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

const (
	CommandBoundaryStopReason = "completed at command boundary"
	ExternalState             = "external"
	RestartPendingState       = "restart_pending"
	FollowupRetryDefault      = "the next launch or `icebath update` retries it"
)

var (
	receiptKeep = 20
	// Follow-up steps whose failure leaves the source update tail owed
	TailFollowups = map[string]bool{"dependencies": true, "launchers": true, "build": true, "maintenance": true,
		"profile_sync": true, "config_migration": true, "completion": true}
	// Follow-ups meaning the build stage did not succeed
	BuildFollowups = map[string]bool{"launchers": true, "build": true, "completion": true}

	gitFilePermission = mustPyre(`Permission denied(?! *\()|\bEACCES\b`)
	parkedReasonLabel = regexp.MustCompile(`^(?:[A-Z][A-Za-z0-9_]{0,63}:(?: \[(?:Errno|WinError) \d+\])?` +
		`|historical takeover preparation failed|Windows gateway recovery failed)`)
	stopClassToken = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	nonDigit       = regexp.MustCompile(`[^0-9]`)

	receiptStdout io.Writer = os.Stdout
	receiptLog              = slog.Default

	consentReaderImportable = func() bool { return true }
	pmLastForUpdate         = pm.LastForUpdateObject

	finalized struct {
		sync.Mutex
		byID map[string]*pyjson.Object
	}

	receiptHooks struct {
		sync.Mutex
		serve    ServeObligationHooks
		recorder func(ctx context.Context, receipt map[string]any)
	}
)

func mustPyre(pattern string) func(string) bool {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return func(s string) bool {
		m, err := re.MatchString(s)
		return err == nil && m
	}
}

// Serve obligation calls receipts carry forward rows through, installed by the update serve obligations owner
type ServeObligationHooks struct {
	IsReminderRow             func(row any) bool
	RetainReceiptManualServes func(ctx context.Context, receipt map[string]any) []any
}

// Installs the reminder row predicate and the manual serve retention finalize applies
func SetServeObligationHooks(hooks ServeObligationHooks) {
	receiptHooks.Lock()
	defer receiptHooks.Unlock()
	receiptHooks.serve = hooks
}

// Installs the shared metrics recorder a final receipt is published through
func SetUpdateReceiptRecorder(record func(ctx context.Context, receipt map[string]any)) {
	receiptHooks.Lock()
	defer receiptHooks.Unlock()
	receiptHooks.recorder = record
}

func currentReceiptHooks() (ServeObligationHooks, func(context.Context, map[string]any)) {
	receiptHooks.Lock()
	defer receiptHooks.Unlock()
	return receiptHooks.serve, receiptHooks.recorder
}

func init() {
	pm.SetUpdateReceiptHooks(pm.UpdateReceiptHooks{
		RecordStopReason: func(reason string) { RecordStopReason(context.Background(), reason) },
		Current: func() map[string]any {
			st := stateOf(nil)
			st.mu.Lock()
			defer st.mu.Unlock()
			if st.current == nil {
				return nil
			}
			plain, _ := pyjson.Plain(st.current.Data).(map[string]any)
			return plain
		},
		Clear: func() {
			st := stateOf(nil)
			st.mu.Lock()
			st.current = nil
			st.mu.Unlock()
		},
	})
}

// Per context receipt state, the analogue of the current receipt and scope terminal ContextVars
type receiptState struct {
	mu       sync.Mutex
	current  *UpdateReceipt
	terminal map[string]any
}

type receiptStateKey struct{}

var defaultReceiptState = &receiptState{}

func stateOf(ctx context.Context) *receiptState {
	if ctx != nil {
		if st, ok := ctx.Value(receiptStateKey{}).(*receiptState); ok {
			return st
		}
	}
	return defaultReceiptState
}

// Open receipt of the context's scope, nil when none has begun
func CurrentReceipt(ctx context.Context) *UpdateReceipt {
	st := stateOf(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.current
}

// Forgets the open receipt of the context's scope the way the hermes tests reset _current
func ClearCurrentReceipt(ctx context.Context) {
	st := stateOf(ctx)
	st.mu.Lock()
	st.current = nil
	st.mu.Unlock()
}

func withState(ctx context.Context, st *receiptState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, receiptStateKey{}, st)
	return pm.WithUpdateCorrelation(ctx, func() string {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.current == nil {
			return ""
		}
		return st.current.CorrelationID
	})
}

// Context with no open receipt and a fresh terminal cell, keeping the command guard off an enclosing update
func UpdateReceiptScope(ctx context.Context) context.Context {
	return withState(ctx, &receiptState{terminal: map[string]any{}})
}

// Context inheriting the open receipt and terminal cell of ctx whose later records stay local to it
func CopyReceiptContext(ctx context.Context) context.Context {
	parent := stateOf(ctx)
	parent.mu.Lock()
	st := &receiptState{current: parent.current, terminal: parent.terminal}
	parent.mu.Unlock()
	return withState(ctx, st)
}

// A completion child finalized this command's run so its terminal outcome becomes this scope's
func AdoptTerminalReceipt(ctx context.Context, data any) {
	st := stateOf(ctx)
	m, ok := pyjson.AsObject(data)
	if !ok || !truthy(m.Vals["finished_at"]) {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.terminal != nil {
		st.terminal["outcome"] = m.Vals["outcome"]
	}
}

// True when this command's run is closed and its receipt says success
func CommittedSuccess(ctx context.Context) bool {
	st := stateOf(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.current == nil && len(st.terminal) > 0 && st.terminal["outcome"] == "success"
}

// Update correlation id of the open receipt in this context, nil when none is open
func CurrentCorrelationID(ctx context.Context) *string {
	st := stateOf(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.current == nil {
		return nil
	}
	id := st.current.CorrelationID
	return &id
}

func utcNowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// Running code identity as an ordered mapping, empty when the probe fails
func codeIdentity(refresh bool) *pyjson.Object {
	identity, err := GetCodeIdentity(refresh)
	if err != nil || identity == nil {
		return pyjson.NewObject()
	}
	return pyjson.Clone(identity).(*pyjson.Object)
}

// Mapping entries reduced to stringified keys, with an int pid first when requested
func strRecords(entries any, keys []string, pid bool) ([]any, error) {
	items, err := pyList(entries)
	if err != nil {
		return nil, err
	}
	records := []any{}
	for _, entry := range items {
		m, ok := pyjson.AsObject(entry)
		if !ok {
			continue
		}
		record := pyjson.NewObject()
		if pid {
			raw, present := m.Vals["pid"]
			if !present || !truthy(raw) {
				raw = 0
			}
			n, err := pyIntConvert(raw)
			if err != nil {
				return nil, err
			}
			record.Set("pid", n)
		}
		for _, key := range keys {
			value, present := m.Vals[key]
			if !present {
				value = ""
			}
			record.Set(key, pyrepr.Str(value))
		}
		records = append(records, record)
	}
	return records, nil
}

// Correlation id an external launcher exported, nil when absent
func launcherCorrelationID() any {
	if id := pystr.Strip(os.Getenv("ICEBATH_UPDATE_CORRELATION_ID")); id != "" {
		return id
	}
	return nil
}

// Observable facts of one icebath update run
type UpdateReceipt struct {
	Data          *pyjson.Object
	CorrelationID string
	begun         bool
	prev          *UpdateReceipt
	owner         *receiptState
}

// Fresh running receipt carrying argv, pid, pre update identity and a new correlation id
func NewUpdateReceipt() *UpdateReceipt {
	argv := make([]any, len(os.Args))
	for i, a := range os.Args {
		argv[i] = a
	}
	r := &UpdateReceipt{Data: pyjson.ObjectOf(
		"schema", 1, "started_at", utcNowISO(), "finished_at", nil,
		"argv", argv, "pid", os.Getpid(),
		"outcome", "running", "pre_update", codeIdentity(false), "post_update", pyjson.NewObject(),
		"steps", []any{}, "skips", []any{}, "gateway_restart", pyjson.NewObject(), "fleet", []any{},
	)}
	r.CorrelationID = uuid4Hex()
	r.Data.Set("update_id", r.CorrelationID)
	r.Data.Set("correlation_id", launcherCorrelationID())
	if action := pystr.Strip(os.Getenv("ICEBATH_ACTION_ID")); action != "" {
		r.Data.Set("action_id", action)
	} else {
		r.Data.Set("action_id", nil)
	}
	return r
}

func (r *UpdateReceipt) appendTo(key string, entry *pyjson.Object) error {
	existing, present := r.Data.Vals[key]
	if !present {
		r.Data.Set(key, []any{entry})
		return nil
	}
	list, ok := existing.([]any)
	if !ok {
		return fmt.Errorf("'%s' object has no attribute 'append'", pyrepr.TypeName(existing))
	}
	r.Data.Set(key, append(list, entry))
	return nil
}

// Records one step outcome
func (r *UpdateReceipt) Step(name string, ok bool, detail string) error {
	return r.appendTo("steps", pyjson.ObjectOf("name", name, "ok", ok, "detail", detail, "at", utcNowISO()))
}

// Records a skipped step with its reason
func (r *UpdateReceipt) Skip(name, reason string) error {
	return r.appendTo("skips", pyjson.ObjectOf("name", name, "reason", reason, "at", utcNowISO()))
}

// Marks a stage end whose duration is the gap since the previous mark, facts following in call order
func (r *UpdateReceipt) Stage(name, outcome string, facts *pyjson.Object) error {
	mark := pyjson.ObjectOf("name", name, "outcome", outcome, "at", utcNowISO())
	mark.Update(facts)
	return r.appendTo("stages", mark)
}

// Sets one top level field
func (r *UpdateReceipt) Fact(key string, value any) error {
	r.Data.Set(key, value)
	return nil
}

// Records a post commit step the run still owes
func (r *UpdateReceipt) Followup(step, reason string) error {
	return r.appendTo("followups", pyjson.ObjectOf("step", step, "reason", reason, "at", utcNowISO()))
}

// Gateway restart phase outcome, FreshRecovery nil when the fresh recovery pass did not run
type GatewayRestart struct {
	RestartedServices            []string
	RelaunchedProfiles           []string
	ExternallySupervisedProfiles []string
	KilledPIDs                   []int
	FailedUnits                  []string
	Incomplete                   bool
	PhaseError                   string
	FreshRecovery                map[string]any
}

func stringList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Records the gateway restart phase with the conservative fresh recovery buckets
func (r *UpdateReceipt) GatewayRestartResult(g GatewayRestart) error {
	killed := make([]any, len(g.KilledPIDs))
	for i, p := range g.KilledPIDs {
		killed[i] = p
	}
	result := pyjson.ObjectOf(
		"restarted_services", stringList(g.RestartedServices),
		"relaunched_profiles", stringList(g.RelaunchedProfiles),
		"externally_supervised_profiles", stringList(g.ExternallySupervisedProfiles),
		"killed_pids", killed,
		"failed_units", stringList(g.FailedUnits),
		"incomplete", g.Incomplete,
		"phase_error", g.PhaseError,
	)
	if g.FreshRecovery != nil {
		persisted := pyjson.NewObject()
		for _, key := range []string{"requested", "verified", "relaunch_attempted", "failed"} {
			raw, present := g.FreshRecovery[key]
			if !present {
				raw = []any{}
			}
			items, err := pyList(raw)
			if err != nil {
				return err
			}
			out := make([]any, len(items))
			for i, p := range items {
				out[i] = pyrepr.Str(p)
			}
			persisted.Set(key, out)
		}
		skippedRaw, present := g.FreshRecovery["skipped"]
		if !present {
			skippedRaw = []any{}
		}
		skipped, err := strRecords(skippedRaw, []string{"profile", "kind", "supervisor", "reason"}, false)
		if err != nil {
			return err
		}
		persisted.Set("skipped", skipped)
		serveUnits := pyjson.NewObject()
		if raw := g.FreshRecovery["serve_units"]; truthy(raw) {
			m, ok := pyjson.AsObject(raw)
			if !ok {
				return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))
			}
			serveUnits = m
		}
		units := pyjson.NewObject()
		for _, key := range []string{"verified", "failed"} {
			var raw any = []any{}
			if v := serveUnits.Vals[key]; truthy(v) {
				raw = v
			}
			items, err := pyList(raw)
			if err != nil {
				return err
			}
			out := make([]any, len(items))
			for i, u := range items {
				out[i] = pyrepr.Str(u)
			}
			units.Set(key, out)
		}
		persisted.Set("serve_units", units)
		staleRaw, present := g.FreshRecovery["stale_runtimes"]
		if !present {
			staleRaw = []any{}
		}
		stale, err := strRecords(staleRaw, []string{"kind", "profile", "supervisor"}, true)
		if err != nil {
			return err
		}
		persisted.Set("stale_runtimes", stale)
		result.Set("fresh_recovery", persisted)
	}
	r.Data.Set("gateway_restart", result)
	return nil
}

// Closes the receipt, partial when a committed run still owes a user action
func (r *UpdateReceipt) Finalize(outcome string) {
	if outcome == "success" && truthy(r.Data.Vals["user_action"]) {
		outcome = "partial"
	}
	r.Data.Set("outcome", outcome)
	r.Data.Set("finished_at", utcNowISO())
	r.Data.Set("post_update", codeIdentity(true))
}

func (r *UpdateReceipt) clone() *UpdateReceipt {
	c := *r
	c.Data = pyjson.Clone(r.Data).(*pyjson.Object)
	return &c
}

// Root home receipt store
func receiptDir(ctx context.Context) string {
	return filepath.Join(home.LogsDir(home.RootOf(home.Dir(ctx))), "update_receipts")
}

// Exported receiptDir
func ReceiptDir(ctx context.Context) string { return receiptDir(ctx) }

// Root store then the profile's own when a named profile makes them differ
func receiptDirs(ctx context.Context) []string {
	root := receiptDir(ctx)
	profile := filepath.Join(home.LogsDir(home.Dir(ctx)), "update_receipts")
	rr, err1 := pypath.Resolve(root)
	pr, err2 := pypath.Resolve(profile)
	if err1 == nil && err2 == nil && rr == pr {
		return []string{root}
	}
	return []string{root, profile}
}

// Exported receiptDirs
func ReceiptDirs(ctx context.Context) []string { return receiptDirs(ctx) }

// Replaces latest.json in every store a reader of this home consults
func writeLatest(ctx context.Context, payload []byte) error {
	for _, directory := range receiptDirs(ctx) {
		if err := os.MkdirAll(directory, 0o777); err != nil {
			return err
		}
		if err := pm.DurableWriteBytes(filepath.Join(directory, "latest.json"), payload); err != nil {
			return err
		}
	}
	return nil
}

// Exported writeLatest
func WriteLatest(ctx context.Context, payload []byte) error { return writeLatest(ctx, payload) }

// Archive file of one run, named at begin so the running and terminal records coincide
func runFile(directory string, data *pyjson.Object) string {
	started := data.Vals["started_at"]
	text := ""
	if truthy(started) {
		text = pyrepr.Str(started)
	}
	stamp := pystr.Slice(nonDigit.ReplaceAllString(text, ""), 0, 14)
	if stamp == "" {
		stamp = time.Now().Format("20060102150405")
	}
	var pid any = os.Getpid()
	if truthy(data.Vals["pid"]) {
		pid = data.Vals["pid"]
	}
	name := fmt.Sprintf("update_%s_%s_%s_%s.json", pystr.Slice(stamp, 0, 8), pystr.Slice(stamp, 8, len(stamp)),
		pyrepr.Str(pid), pyrepr.Str(data.Vals["update_id"]))
	return filepath.Join(directory, name)
}

// Exported runFile
func RunFile(directory string, data *pyjson.Object) string { return runFile(directory, data) }

// Directory entries matching a glob pattern in directory order, regular files and others alike
func globDir(directory, pattern string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if fnmatch.FnmatchCase(e.Name(), pattern) {
			out = append(out, filepath.Join(directory, e.Name()))
		}
	}
	return out, nil
}

// JSON document of a file read as utf-8-sig
func loadJSONFile(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Exported loadJSONFile
func LoadReceiptFile(path string) (any, error) { return loadJSONFile(path) }

// Indented receipt payload with a trailing newline, str for anything JSON cannot encode
func receiptPayload(data any, indent bool) ([]byte, error) {
	opts := []pyjson.Option{pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil })}
	if indent {
		opts = append(opts, pyjson.Indent(2))
	}
	text, err := pyjson.Dumps(data, opts...)
	if err != nil {
		return nil, err
	}
	if indent {
		text += "\n"
	}
	return []byte(text), nil
}

// Exported receiptPayload
func ReceiptPayload(data any, indent bool) ([]byte, error) { return receiptPayload(data, indent) }

// The run's own archive path and record in the root store in key order, never latest.json
func ReadRunRecord(ctx context.Context, updateID string) (string, *pyjson.Object, bool) {
	paths, err := globDir(receiptDir(ctx), "update_*_"+updateID+".json")
	if err != nil {
		return "", nil, false
	}
	for _, path := range paths {
		v, err := loadJSONFile(path)
		if err != nil {
			continue
		}
		if record, ok := v.(*pyjson.Object); ok && pyrepr.Equal(record.Vals["update_id"], updateID) {
			return path, record, true
		}
	}
	return "", nil, false
}

// Writes the open run to disk unless its archive already holds the terminal record
func persistRunning(ctx context.Context, data *pyjson.Object) {
	directory := receiptDir(ctx)
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return
	}
	path := runFile(directory, data)
	if _, stored, ok := ReadRunRecord(ctx, pyrepr.Str(data.Vals["update_id"])); ok && truthy(stored.Vals["finished_at"]) {
		return
	}
	record := data.Copy()
	record.Set("writer_pid", os.Getpid())
	if ct := ProcessCreateTime(os.Getpid()); ct != nil {
		record.Set("writer_create_time", *ct)
	} else {
		record.Set("writer_create_time", nil)
	}
	payload, err := receiptPayload(record, true)
	if err != nil {
		return
	}
	if pm.DurableWriteBytes(path, payload) != nil {
		return
	}
	_ = writeLatest(ctx, payload)
}

// Liveness of pid against a recorded creation time, nil when the probe cannot say
func pidAliveMatches(pid int64, createTime any) *bool {
	if pid > math.MaxInt32 || !proc.Exists(int(pid)) {
		return boolPtr(false)
	}
	birth, err := proc.Birth(int(pid))
	if err != nil {
		if !proc.Exists(int(pid)) {
			return boolPtr(false)
		}
		return nil
	}
	same := true
	if createTime != nil {
		ct, ok := pyFloat(createTime)
		if !ok {
			return nil
		}
		same = math.Abs(birth-ct) < 2.0
	}
	return boolPtr(same && (runtime.GOOS == "windows" || !proc.Zombie(int(pid))))
}

// Whether the run's owner or last writer may still be alive
func ownerAlive(record *pyjson.Object) bool {
	var order []int64
	identities := map[int64]any{}
	for _, pair := range [][2]string{{"pid", "pid_create_time"}, {"writer_pid", "writer_create_time"}} {
		pid, ok := pyIntValue(record.Vals[pair[0]])
		if !ok || pid <= 0 {
			continue
		}
		existing, seen := identities[pid]
		if !seen {
			order = append(order, pid)
		}
		if !seen || existing == nil {
			identities[pid] = record.Vals[pair[1]]
		}
	}
	for _, pid := range order {
		if alive := pidAliveMatches(pid, identities[pid]); alive == nil || *alive {
			return true
		}
	}
	return false
}

// Exported ownerAlive
func OwnerAlive(record *pyjson.Object) bool { return ownerAlive(record) }

// Paths under directory matching pattern ordered by modification time, oldest first
func byMtime(paths []string) ([]string, error) {
	type stamped struct {
		path  string
		mtime time.Time
	}
	items := make([]stamped, 0, len(paths))
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		items = append(items, stamped{p, st.ModTime()})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].mtime.Before(items[j].mtime) })
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.path
	}
	return out, nil
}

// Marks running records whose processes are gone as interrupted and returns them
func ReconcileInterruptedRuns(ctx context.Context) []map[string]any {
	var interrupted []*pyjson.Object
	func() {
		directory := receiptDir(ctx)
		latest := readLatestReceipt(ctx)
		paths, err := globDir(directory, "update_*.json")
		if err != nil {
			return
		}
		ordered, err := byMtime(paths)
		if err != nil {
			return
		}
		if len(ordered) > receiptKeep {
			ordered = ordered[len(ordered)-receiptKeep:]
		}
		for _, path := range ordered {
			v, err := loadJSONFile(path)
			if err != nil {
				continue
			}
			record, ok := v.(*pyjson.Object)
			if !ok || !pyrepr.Equal(record.Vals["outcome"], "running") || ownerAlive(record) {
				continue
			}
			if stages, isMap := record.Vals["stages"].(*pyjson.Object); isMap && stages.Len() > 0 {
				continue
			}
			var last any
			if stages, err := pyList(orEmptyList(record.Vals["stages"])); err == nil && len(stages) > 0 {
				last = mapGet(stages[len(stages)-1], "name")
			} else if err != nil {
				continue
			}
			lastText := "start"
			if truthy(last) {
				lastText = pyrepr.Str(last)
			}
			record.Set("outcome", "interrupted")
			record.Set("interrupted_detected_at", utcNowISO())
			record.Set("stop_reason", fmt.Sprintf("process exited during the update (last stage: %s)", lastText))
			payload, err := receiptPayload(record, true)
			if err != nil || pm.DurableWriteBytes(path, payload) != nil {
				continue
			}
			if pyrepr.Equal(mapGet(latest, "update_id"), record.Vals["update_id"]) {
				if writeLatest(ctx, payload) != nil {
					continue
				}
			}
			interrupted = append(interrupted, record)
		}
	}()
	var out []map[string]any
	for _, record := range interrupted {
		fmt.Fprintf(receiptStdout, "⚠ The previous update (%s) was interrupted before it finished (%s); this update completes the work it still owed.\n",
			pyrepr.Str(record.Vals["started_at"]), pyrepr.Str(record.Vals["stop_reason"]))
		out = append(out, plainReceipt(record))
	}
	return out
}

// Starts a durably running receipt, restoring any enclosing one when it finalizes
func BeginUpdateReceipt(ctx context.Context, previous *pyjson.Object, correlationID string) {
	receipt := NewUpdateReceipt()
	hasPrevious := previous != nil && previous.Len() > 0
	if hasPrevious {
		receipt.Data.Update(pyjson.Clone(previous).(*pyjson.Object))
	}
	if correlationID != "" {
		receipt.CorrelationID = correlationID
	}
	receipt.Data.Set("update_id", receipt.CorrelationID)
	receipt.Data.Set("outcome", "running")
	receipt.Data.Set("finished_at", nil)
	if id := receipt.Data.Vals["correlation_id"]; truthy(id) {
		receipt.Data.Set("correlation_id", id)
	} else {
		receipt.Data.Set("correlation_id", launcherCorrelationID())
	}
	if _, present := receipt.Data.Vals["pid_create_time"]; !present {
		if pid, ok := pyIntValue(receipt.Data.Vals["pid"]); ok && pid > 0 && pid <= math.MaxInt32 {
			if ct := ProcessCreateTime(int(pid)); ct != nil {
				receipt.Data.Set("pid_create_time", *ct)
			} else {
				receipt.Data.Set("pid_create_time", nil)
			}
		}
	}
	if !hasPrevious {
		ReconcileInterruptedRuns(ctx)
	}
	carried := false
	if hasPrevious {
		_, carried = previous.Vals["carried_manual_serves"]
	}
	if !carried {
		if rows, err := carriedManualServes(ctx); err == nil {
			receipt.Data.Set("carried_manual_serves", rows)
		}
	}
	st := stateOf(ctx)
	st.mu.Lock()
	receipt.begun, receipt.prev, receipt.owner = true, st.current, st
	st.current = receipt
	st.mu.Unlock()
	persistRunning(ctx, receipt.Data)
}

// Previous record's reminder rows snapshotted for finalize to carry forward
func carriedManualServes(ctx context.Context) ([]any, error) {
	serve, _ := currentReceiptHooks()
	prior := readLatestReceipt(ctx)
	plan := pyjson.NewObject()
	if raw := mapGet(prior, "plan"); truthy(raw) {
		m, ok := pyjson.AsObject(raw)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		plan = m
	}
	var rows []any
	for _, raw := range []any{plan.Vals["runtimes"], mapGet(prior, "pending_manual_serves"), mapGet(prior, "carried_manual_serves")} {
		items, err := pyList(orEmptyList(raw))
		if err != nil {
			return nil, err
		}
		rows = append(rows, items...)
	}
	if serve.IsReminderRow == nil {
		return nil, errors.New("No module named 'update_serve_obligations'")
	}
	out := []any{}
	for _, row := range rows {
		if serve.IsReminderRow(pyjson.Plain(row)) {
			out = append(out, row)
		}
	}
	return out, nil
}

// Re-asserts the open running run on disk
func PersistRunningReceipt(ctx context.Context) {
	st := stateOf(ctx)
	st.mu.Lock()
	current := st.current
	st.mu.Unlock()
	if current != nil && pyrepr.Equal(current.Data.Vals["outcome"], "running") {
		persistRunning(ctx, current.Data)
	}
}

// Marks the build stage failed only when a build follow-up is owed
func RecordBuildStage(ctx context.Context, followups [][2]string) {
	outcome := "success"
	for _, f := range followups {
		if BuildFollowups[f[0]] {
			outcome = "failed"
			break
		}
	}
	RecordStage(ctx, "build", outcome, nil)
}

// Whitespace collapsed text cut to 500 characters
func collapseReason(reason string) string {
	return pystr.Slice(strings.Join(pystr.SplitWhitespace(reason, -1), " "), 0, 500)
}

// Prints the owed follow-up protocol line and records it on the open run
func RecordFollowup(ctx context.Context, step, reason, retry string) {
	reason = collapseReason(reason)
	if reason == "" {
		reason = "failed"
	}
	fmt.Fprintf(receiptStdout, "  ⚠ Update follow-up '%s' did not finish: %s (%s)\n", step, reason, retry)
	st := stateOf(ctx)
	st.mu.Lock()
	open := st.current != nil
	st.mu.Unlock()
	if !open {
		return
	}
	record(ctx, "update followup "+step, func(r *UpdateReceipt) error { return r.Followup(step, reason) })
	st.mu.Lock()
	current := st.current
	st.mu.Unlock()
	if current != nil {
		persistRunning(ctx, current.Data)
	}
}

// Records a committed run's owed user action so it finalizes partial
func RecordUserAction(ctx context.Context, step, reason string) {
	action := pyjson.ObjectOf("step", step, "reason", collapseReason(reason))
	record(ctx, "update user action "+step, func(r *UpdateReceipt) error { return r.Fact("user_action", action) })
	PersistRunningReceipt(ctx)
}

// Appends a follow-up that failed after finalize to the run's closed receipt
func AmendTerminalFollowup(ctx context.Context, updateID, step, reason string) error {
	path, rec, ok := ReadRunRecord(ctx, updateID)
	if !ok {
		return nil
	}
	r := &UpdateReceipt{Data: rec}
	if err := r.Followup(step, reason); err != nil {
		return err
	}
	payload, err := receiptPayload(rec, true)
	if err != nil {
		return err
	}
	if pm.DurableWriteBytes(path, payload) != nil {
		return nil
	}
	if pyrepr.Equal(mapGet(ReadLatestReceipt(ctx), "update_id"), updateID) {
		_ = writeLatest(ctx, payload)
	}
	return nil
}

// Records a failed post commit step on the open run, else amends the run's closed receipt
func OweFollowup(ctx context.Context, updateID *string, step, reason, retry string) error {
	RecordFollowup(ctx, step, reason, retry)
	st := stateOf(ctx)
	st.mu.Lock()
	open := st.current != nil
	st.mu.Unlock()
	if !open && updateID != nil && *updateID != "" {
		return AmendTerminalFollowup(ctx, *updateID, step, reason)
	}
	return nil
}

// Applies mutate to a copy of the open receipt and makes the copy current in this context only
func record(ctx context.Context, what string, mutate func(*UpdateReceipt) error) {
	st := stateOf(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.current == nil {
		return
	}
	c := st.current.clone()
	if err := mutate(c); err != nil {
		receiptLog().Debug(fmt.Sprintf("Could not record %s: %s", what, err))
		return
	}
	st.current = c
}

// Records one update step outcome
func RecordStep(ctx context.Context, name string, ok bool, detail string) {
	record(ctx, "update step "+name, func(r *UpdateReceipt) error { return r.Step(name, ok, detail) })
}

// Records a skipped step with the reason it was skipped
func RecordSkip(ctx context.Context, name, reason string) {
	record(ctx, "update skip "+name, func(r *UpdateReceipt) error { return r.Skip(name, reason) })
}

// Marks the end of a pipeline stage and refreshes the durable running record
func RecordStage(ctx context.Context, name, outcome string, facts *pyjson.Object) {
	record(ctx, "update stage "+name, func(r *UpdateReceipt) error { return r.Stage(name, outcome, facts) })
	PersistRunningReceipt(ctx)
}

// Sets one top level receipt field
func RecordFact(ctx context.Context, key string, value any) {
	record(ctx, "update fact "+key, func(r *UpdateReceipt) error { return r.Fact(key, value) })
}

// Records the gateway restart phase outcome
func RecordGatewayRestart(ctx context.Context, g GatewayRestart) {
	record(ctx, "gateway restart result", func(r *UpdateReceipt) error { return r.GatewayRestartResult(g) })
}

// Names the closed stop token of the exit about to stop this run
func RecordStopReason(ctx context.Context, reason string) {
	record(ctx, "update stop "+reason, func(r *UpdateReceipt) error { return r.Fact("stop_class", reason) })
}

// Closed stop token for a failed git command about to end the run, nil for anything else
func GitErrorStopClass(err error) *string {
	var called *pysubprocess.CalledProcessError
	if !errors.As(err, &called) {
		return nil
	}
	raw := called.Stderr
	if len(raw) == 0 {
		raw = called.Stdout
	}
	code := called.Returncode
	return GitOutputStopClass(pystr.DecodeUTF8Replace(raw), called.Cmd, &code)
}

// Stop token for a git call that returned instead of raising
func GitOutputStopClass(output string, argv []string, returncode *int) *string {
	token := func(s string) *string { return &s }
	switch {
	case strings.Contains(output, "index.lock") && strings.Contains(output, "File exists"):
		return token("git_index_locked")
	case gitFilePermission(output):
		return token("permission_denied")
	case returncode != nil && *returncode == 124 && strings.Contains(output, "timed out after"):
		return token("git_timeout")
	case strings.Contains(output, "No space left on device"):
		return token("disk_full")
	}
	for _, a := range argv {
		if a == "stash" {
			return token("local_changes_blocked")
		}
	}
	for _, a := range argv {
		if a == "checkout" || a == "merge" || a == "reset" {
			return token("checkout_move_failed")
		}
	}
	return nil
}

// Delegate or partner when the update marker names this process's handoff, else empty
func startedByHandoffPartner() (string, error) {
	raw, err := os.ReadFile(UpdateMarkerPath())
	if err != nil {
		return "", err
	}
	m := parseMarker(raw, nil)
	if m.startedAt == nil {
		return "", nil
	}
	pid, ppid := os.Getpid(), os.Getppid()
	if m.delegatePID != nil && (*m.delegatePID == pid || *m.delegatePID == ppid) {
		return "delegate", nil
	}
	partners := map[int]bool{}
	for _, p := range []int{handoffPID(), ppid} {
		if p != 0 && p != 1 && p != pid {
			partners[p] = true
		}
	}
	if partners[m.pid] || (m.delegatePID != nil && partners[*m.delegatePID]) {
		return "partner", nil
	}
	return "", nil
}

// Counts an exit that fires before this run's receipt opens in shared metrics only
func RecordStopWithoutReceipt(ctx context.Context, reason, outcome string) {
	now := utcNowISO()
	final := "failed"
	if outcome == "refused" {
		final = "refused"
	}
	data := pyjson.ObjectOf(
		"schema", 1, "update_id", uuid4Hex(), "started_at", now, "finished_at", now,
		"outcome", final, "stop_class", reason, "pre_update", pyjson.NewObject(),
		"stages", []any{}, "steps", []any{}, "fleet", []any{},
	)
	if claim, err := startedByHandoffPartner(); err == nil {
		if claim != "" {
			data.Set("initiator", "desktop")
		}
		if claim == "delegate" {
			data.Set("outcome", "failed")
		}
	}
	if _, recorder := currentReceiptHooks(); recorder != nil {
		recorder(ctx, plainReceipt(data))
	}
}

// Finalizes and persists the open receipt exactly once, returning its archive path or empty
func FinalizeUpdateReceipt(ctx context.Context, outcome string, fleet []any, stopReason string) string {
	st := stateOf(ctx)
	st.mu.Lock()
	current := st.current
	if current == nil {
		st.mu.Unlock()
		return ""
	}
	receipt := current.clone()
	if receipt.begun && receipt.owner == st {
		st.current = receipt.prev
	} else {
		st.current = nil
	}
	st.mu.Unlock()
	path, err := finalizeReceipt(ctx, receipt, outcome, fleet, stopReason)
	if err != nil {
		receiptLog().Warn(fmt.Sprintf("Could not write update receipt (%s): %s", outcome, pypath.OSErrorText(err)))
		fmt.Fprintf(receiptStdout, "  ⚠ Update receipt not written: %s\n", pypath.OSErrorText(err))
		return ""
	}
	return path
}

func finalizeReceipt(ctx context.Context, receipt *UpdateReceipt, outcome string, fleet []any, stopReason string) (string, error) {
	receipt.Finalize(outcome)
	data := receipt.Data
	if stopReason != "" {
		data.Set("stop_reason", stopReason)
	}
	finalized.Lock()
	if finalized.byID == nil {
		finalized.byID = map[string]*pyjson.Object{}
	}
	finalized.byID[pyrepr.Str(data.Vals["update_id"])] = data
	finalized.Unlock()
	AdoptTerminalReceipt(ctx, data)
	if fleet != nil {
		data.Set("fleet", fleet)
	}
	serve, _ := currentReceiptHooks()
	if serve.RetainReceiptManualServes == nil {
		return "", errors.New("No module named 'update_serve_obligations'")
	}
	var prior *pyjson.Object
	if carried, present := data.Pop("carried_manual_serves"); present {
		prior = pyjson.ObjectOf("pending_manual_serves", orEmptyList(carried))
	} else if prior = readLatestReceipt(ctx); prior == nil {
		prior = pyjson.NewObject()
	}
	if pending := serve.RetainReceiptManualServes(ctx, plainReceipt(prior)); len(pending) > 0 {
		data.Set("pending_manual_serves", pending)
	}
	embedPMSync(ctx, data, receipt.CorrelationID)
	directory := receiptDir(ctx)
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return "", err
	}
	path := runFile(directory, data)
	payload, err := receiptPayload(data, true)
	if err != nil {
		return "", err
	}
	if err := pm.DurableWriteBytes(path, payload); err != nil {
		return "", err
	}
	_ = writeLatest(ctx, payload)
	pruneOldReceipts(directory)
	publishSharedMetrics(ctx, data)
	return path, nil
}

// Folds in only the pm sync sections filed under this update's correlation id, an embed failure changes nothing
func embedPMSync(ctx context.Context, data *pyjson.Object, updateID string) {
	defer func() {
		if r := recover(); r != nil {
			receiptLog().Debug(fmt.Sprintf("pm sync-section embed skipped: %v", r))
		}
	}()
	sync := pmLastForUpdate(ctx, updateID, true)
	if sync != nil {
		for _, key := range []string{"venv_rebuild", "plugin_bisect", "feature_list", "steps", "exit_code"} {
			if sync.Vals[key] != nil {
				data.Set("pm_"+key, sync.Vals[key])
			}
		}
		if sync.Vals["outcome"] != nil {
			data.Set("pm_sync_outcome", sync.Vals["outcome"])
		}
		if truthy(sync.Vals["warnings"]) {
			data.Set("pm_warnings", sync.Vals["warnings"])
		}
		if sync.Vals["refusal"] != nil {
			data.Set("pm_refusal", sync.Vals["refusal"])
		}
	}
}

// Shared metrics consent from the config reader, nil when it cannot tell
func collectionEnabledNow(ctx context.Context) *bool {
	cfg, err := config.ReadRawConfigReadonly(ctx, home.Dir(ctx))
	if err != nil || cfg == nil || config.FailedReadError(cfg) != nil {
		return nil
	}
	var node any = cfg
	for _, key := range []string{"telemetry", "shared_metrics"} {
		if m, ok := node.(map[string]any); ok {
			node = m[key]
		} else {
			node = nil
		}
	}
	m, ok := node.(map[string]any)
	return boolPtr(ok && m["enabled"] == true)
}

// Only the bounded fields the update metric classifier reads, never argv or step text
func metricReceipt(data *pyjson.Object) *pyjson.Object {
	pre, _ := pyjson.AsObject(data.Vals["pre_update"])
	restart, _ := pyjson.AsObject(data.Vals["gateway_restart"])
	reason, _ := data.Vals["stop_reason"].(string)
	stages := []any{}
	for _, raw := range listItems(data.Vals["stages"]) {
		mark, ok := pyjson.AsObject(raw)
		if !ok {
			continue
		}
		kept := pyjson.NewObject()
		for _, key := range []string{"name", "outcome", "at", "mode"} {
			if v, present := mark.Vals[key]; present {
				kept.Set(key, v)
			}
		}
		stages = append(stages, kept)
	}
	steps := []any{}
	for _, raw := range listItems(data.Vals["steps"]) {
		if step, ok := pyjson.AsObject(raw); ok && pyrepr.Equal(step.Vals["name"], "admission") {
			steps = append(steps, pyjson.ObjectOf("name", "admission", "ok", truthy(step.Vals["ok"])))
		}
	}
	fleet := []any{}
	for _, raw := range listItems(data.Vals["fleet"]) {
		if row, ok := pyjson.AsObject(raw); ok {
			fleet = append(fleet, pyjson.ObjectOf("state", row.Vals["state"]))
		}
	}
	outcomes := []any{}
	for _, raw := range listItems(data.Vals["runtime_outcomes"]) {
		if row, ok := pyjson.AsObject(raw); ok && pyrepr.Equal(row.Vals["outcome"], "failed") {
			outcomes = append(outcomes, pyjson.ObjectOf("outcome", "failed"))
		}
	}
	var stopClass any
	if s, ok := data.Vals["stop_class"].(string); ok && stopClassToken.MatchString(s) {
		stopClass = s
	}
	var exitCode any
	if n, ok := pyIntValue(data.Vals["exit_code"]); ok {
		if _, isBool := data.Vals["exit_code"].(bool); !isBool {
			exitCode = n
		}
	}
	stopReason := ""
	if label := parkedReasonLabel.FindString(reason); label != "" {
		stopReason = label
	} else if reason != "" {
		stopReason = "-"
	}
	var initiator any
	if pyrepr.Equal(data.Vals["initiator"], "desktop") {
		initiator = "desktop"
	}
	gateway := pyjson.NewObject()
	for _, key := range []string{"incomplete", "phase_error", "failed_units"} {
		gateway.Set(key, truthy(mapGet(restart, key)))
	}
	return pyjson.ObjectOf(
		"schema", data.Vals["schema"], "update_id", data.Vals["update_id"], "started_at", data.Vals["started_at"],
		"finished_at", data.Vals["finished_at"], "outcome", data.Vals["outcome"], "initiator", initiator,
		"pre_update", pyjson.ObjectOf("commit_date", mapGet(pre, "commit_date")), "stages", stages, "steps", steps,
		"fleet", fleet, "stop_class", stopClass, "exit_code", exitCode, "stop_reason", stopReason,
		"user_action", truthy(data.Vals["user_action"]), "gateway_restart", gateway, "runtime_outcomes", outcomes,
	)
}

// Consent as far as a finalizer without the config reader can tell
func bareCollectionEnabled(ctx context.Context) *bool {
	if enabled := collectionEnabledNow(ctx); enabled != nil {
		return enabled
	}
	_, err := os.Stat(filepath.Join(home.Dir(ctx), "config.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return boolPtr(false)
	}
	return nil
}

// Parks the bounded metric fields for the next start, or purges parked ones when collection is off
func parkMetricReceipt(ctx context.Context, data *pyjson.Object, enabled *bool) error {
	pending := filepath.Join(home.Dir(ctx), "telemetry", "shared_metrics", "pending_updates")
	if enabled != nil && !*enabled {
		_ = os.RemoveAll(pending)
		return nil
	}
	if err := os.MkdirAll(pending, 0o777); err != nil {
		return err
	}
	payload, err := receiptPayload(metricReceipt(data), false)
	if err != nil {
		return err
	}
	return pm.DurableWriteBytes(filepath.Join(pending, pyrepr.Str(data.Vals["update_id"])+".json"), payload)
}

// Records the final receipt's run and stage rows, parking them when this finalizer predates the swap
func publishSharedMetrics(ctx context.Context, data *pyjson.Object) {
	pre, post := pyjson.NewObject(), pyjson.NewObject()
	for i, key := range []string{"pre_update", "post_update"} {
		raw := data.Vals[key]
		if !truthy(raw) {
			continue
		}
		m, ok := pyjson.AsObject(raw)
		if !ok {
			return
		}
		if i == 0 {
			pre = m
		} else {
			post = m
		}
	}
	if pid, ok := pyIntValue(data.Vals["pid"]); ok && pid == int64(os.Getpid()) &&
		!(truthy(pre.Vals["sha"]) && pyrepr.Equal(pre.Vals["sha"], post.Vals["sha"])) {
		_ = parkMetricReceipt(ctx, data, collectionEnabledNow(ctx))
		return
	}
	if !consentReaderImportable() {
		_ = parkMetricReceipt(ctx, data, bareCollectionEnabled(ctx))
		return
	}
	if _, recorder := currentReceiptHooks(); recorder != nil {
		recorder(ctx, plainReceipt(data))
	}
}

// Deep copy of the terminal record this process finalized for an update id, nil when none
func FinalizedReceipt(updateID string) *pyjson.Object {
	finalized.Lock()
	defer finalized.Unlock()
	data, ok := finalized.byID[updateID]
	if !ok {
		return nil
	}
	return pyjson.Clone(data).(*pyjson.Object)
}

// Command boundary safety net that persists a still open receipt with the exit's outcome
func FinalizePendingUpdateReceipt(ctx context.Context, exitCode *int, stopReason string) string {
	st := stateOf(ctx)
	st.mu.Lock()
	current := st.current
	if current == nil {
		st.mu.Unlock()
		return ""
	}
	outcome := "failed"
	switch {
	case exitCode == nil || *exitCode == 0:
		outcome = "success"
	case *exitCode == 2:
		outcome = "refused"
	case truthy(current.Data.Vals["user_action"]):
		outcome = "partial"
	}
	if exitCode != nil {
		c := current.clone()
		c.Data.Set("exit_code", *exitCode)
		st.current = c
	}
	st.mu.Unlock()
	return FinalizeUpdateReceipt(ctx, outcome, nil, stopReason)
}

// Carries the open run forward from its archive, returning the archive when the run is already closed
func ResumeRunRecord(ctx context.Context) string {
	st := stateOf(ctx)
	st.mu.Lock()
	current := st.current
	st.mu.Unlock()
	if current == nil {
		return ""
	}
	path, rec, ok := ReadRunRecord(ctx, pyrepr.Str(current.Data.Vals["update_id"]))
	if !ok {
		return ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if truthy(rec.Vals["finished_at"]) {
		if current.begun && current.owner == st {
			st.current = current.prev
		} else {
			st.current = nil
		}
		return path
	}
	rec.Delete("writer_pid")
	c := *current
	c.Data = rec
	st.current = &c
	return ""
}

// Closes a run interrupted after the commit point as interrupted
func FinalizeInterruptedUpdateReceipt(ctx context.Context, stopReason string, exitCode int) string {
	fmt.Fprintln(receiptStdout, "⚠ Interrupted after the code was updated: the new code is in place; its remaining steps are owed "+
		"and the next launch or `icebath update` finishes them.")
	st := stateOf(ctx)
	st.mu.Lock()
	open := st.current != nil
	st.mu.Unlock()
	if !open {
		return ""
	}
	if closed := ResumeRunRecord(ctx); closed != "" {
		return closed
	}
	st.mu.Lock()
	if st.current != nil {
		c := st.current.clone()
		c.Data.Set("exit_code", exitCode)
		st.current = c
	}
	st.mu.Unlock()
	return FinalizeUpdateReceipt(ctx, "interrupted", nil, stopReason)
}

// Keeps the newest archived run receipts
func pruneOldReceipts(directory string) {
	paths, err := globDir(directory, "update_*.json")
	if err != nil {
		return
	}
	var files []string
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			files = append(files, p)
		}
	}
	ordered, err := byMtime(files)
	if err != nil {
		return
	}
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	if len(ordered) <= receiptKeep {
		return
	}
	for _, stale := range ordered[receiptKeep:] {
		_ = os.Remove(stale)
	}
}

// Records on latest.json that the fleet it reports as owed now serves the install
func SettleLatestReceiptFleet(ctx context.Context, fleet []any, discharges func(map[string]any) bool) bool {
	v, err := loadJSONFile(filepath.Join(receiptDir(ctx), "latest.json"))
	if err != nil {
		receiptLog().Debug(fmt.Sprintf("Could not settle latest update receipt from the live fleet: %s", pypath.OSErrorText(err)))
		return false
	}
	receipt, ok := v.(*pyjson.Object)
	if !ok {
		return false
	}
	receipt.Set("fleet", append([]any{}, fleet...))
	gateway, ok := receipt.Vals["gateway_restart"].(*pyjson.Object)
	if !ok {
		gateway = pyjson.NewObject()
	}
	gateway.Set("incomplete", false)
	gateway.Set("phase_error", "")
	gateway.Set("settled_from_live_fleet_at", utcNowISO())
	receipt.Set("gateway_restart", gateway)
	if !discharges(plainReceipt(receipt)) {
		return false
	}
	payload, err := receiptPayload(receipt, true)
	if err == nil {
		err = writeLatest(ctx, payload)
	}
	if err != nil {
		receiptLog().Debug(fmt.Sprintf("Could not settle latest update receipt from the live fleet: %s", pypath.OSErrorText(err)))
		return false
	}
	return true
}

// Most recent update receipt across the stores of this home in key order, nil when none parses
func readLatestReceipt(ctx context.Context) *pyjson.Object {
	var best string
	var bestTime time.Time
	for _, d := range receiptDirs(ctx) {
		p := filepath.Join(d, "latest.json")
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if best == "" || st.ModTime().After(bestTime) {
			best, bestTime = p, st.ModTime()
		}
	}
	if best == "" {
		return nil
	}
	v, err := loadJSONFile(best)
	if err != nil {
		return nil
	}
	m, _ := v.(*pyjson.Object)
	return m
}

// Exported readLatestReceipt keeping the ordered object
func LatestReceiptObject(ctx context.Context) *pyjson.Object { return readLatestReceipt(ctx) }

// Most recent update receipt across the stores of this home, nil when none parses
func ReadLatestReceipt(ctx context.Context) map[string]any {
	if latest := readLatestReceipt(ctx); latest != nil {
		return plainReceipt(latest)
	}
	return nil
}

// Newest receipt written by a dashboard action, latest.json first then the retained archive
func ReadReceiptForAction(ctx context.Context, actionID string) map[string]any {
	if receipt := ReceiptObjectForAction(ctx, actionID); receipt != nil {
		return plainReceipt(receipt)
	}
	return nil
}

// Newest receipt of a dashboard action keeping the ordered object, nil when none matches
func ReceiptObjectForAction(ctx context.Context, actionID string) *pyjson.Object {
	if latest := readLatestReceipt(ctx); latest != nil && latest.Len() > 0 && pyrepr.Equal(latest.Vals["action_id"], actionID) {
		return latest
	}
	var archives []string
	for _, d := range receiptDirs(ctx) {
		paths, err := globDir(d, "update_*.json")
		if err == nil {
			archives = append(archives, paths...)
		}
	}
	ordered, err := byMtime(archives)
	if err != nil {
		return nil
	}
	for i := len(ordered) - 1; i >= 0; i-- {
		v, err := loadJSONFile(ordered[i])
		if err != nil {
			continue
		}
		if payload, ok := v.(*pyjson.Object); ok && pyrepr.Equal(payload.Vals["action_id"], actionID) {
			return payload
		}
	}
	return nil
}

// Whether a fleet row serves an install this update did not touch
func RowIsExternal(row any) bool {
	return pyrepr.Equal(mapGet(row, "state"), ExternalState)
}

// Random version 4 uuid as 32 hex digits
func uuid4Hex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// Python truthiness of a decoded JSON value
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case *pyjson.Object:
		return t != nil && t.Len() > 0
	case map[string]any:
		return len(t) > 0
	}
	if n, ok := pyrepr.Int(v); ok {
		return n.Sign() != 0
	}
	return true
}

// Value under key of a decoded dict, nil when the key is absent or the value is no dict
func mapGet(v any, key string) any {
	switch t := v.(type) {
	case *pyjson.Object:
		if t != nil {
			return t.Vals[key]
		}
	case map[string]any:
		return t[key]
	}
	return nil
}

// Plain map form of an ordered receipt for readers that only look up keys
func plainReceipt(o *pyjson.Object) map[string]any {
	m, _ := pyjson.Plain(o).(map[string]any)
	return m
}

func orEmptyList(v any) any {
	if !truthy(v) {
		return []any{}
	}
	return v
}

// Elements of a list value, empty for anything else
func listItems(v any) []any {
	items, _ := v.([]any)
	return items
}

// Python list() of a decoded JSON value
func pyList(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return append([]any{}, t...), nil
	case []string:
		return stringList(t), nil
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		return stringList(append([]string(nil), t.Keys...)), nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return stringList(keys), nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
}

// Integer value of a JSON int, bool counting as Python int, false for anything else
func pyIntValue(v any) (int64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return 0, false
	}
	if n, ok := pyrepr.Int(v); ok && n.IsInt64() {
		return n.Int64(), true
	}
	return 0, false
}

// Python int() of a value
func pyIntConvert(v any) (int64, error) {
	if n, ok := pyIntValue(v); ok {
		return n, nil
	}
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, fmt.Errorf("cannot convert float %s to integer", pyrepr.Float(t))
		}
		return int64(t), nil
	case string:
		n, err := strconv.ParseInt(strings.ReplaceAll(pystr.Strip(t), "_", ""), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(t))
		}
		return n, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

// Python float() of a value, false when it raises
func pyFloat(v any) (float64, bool) {
	if n, ok := pyIntValue(v); ok {
		return float64(n), true
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(pystr.Strip(t), "_", ""), 64)
		return f, err == nil
	}
	return 0, false
}
