package goals

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	DefaultGoalMaxTurns       = 20
	DefaultGateTimeoutSeconds = 300
	DefaultGateMaxRetries     = 3
)

var goalContractFields = [...]string{"outcome", "verification", "constraints", "boundaries", "stop_when"}

var goalContractLabels = map[string]string{
	"outcome": "Outcome", "verification": "Verification", "constraints": "Constraints",
	"boundaries": "Boundaries", "stop_when": "Stop when blocked",
}

func goalsLogger() *slog.Logger { return slog.Default().With("logger", "hermes_cli.goals") }

// Optional structured completion contract whose empty fields are omitted everywhere
type GoalContract struct {
	Outcome      string
	Verification string
	Constraints  string
	Boundaries   string
	StopWhen     string
}

// Field values in display order
func (c GoalContract) values() [5]string {
	return [5]string{c.Outcome, c.Verification, c.Constraints, c.Boundaries, c.StopWhen}
}

// True when every field is blank after stripping
func (c GoalContract) IsEmpty() bool {
	for _, v := range c.values() {
		if pystr.Strip(v) != "" {
			return false
		}
	}
	return true
}

// Ordered dict of every field keyed by its name
func (c GoalContract) ToDict() *pyjson.Object {
	o := pyjson.NewObject()
	for i, v := range c.values() {
		o.Set(goalContractFields[i], v)
	}
	return o
}

// Contract of a stored dict with each field stringified and stripped, empty for a non dict
func GoalContractFromDict(data any) GoalContract {
	d, ok := data.(*pyjson.Object)
	if !ok {
		return GoalContract{}
	}
	var v [5]string
	for i, f := range goalContractFields {
		raw, _ := d.Get(f)
		v[i] = pystr.Strip(pyrepr.Str(pyrepr.Or(raw, "")))
	}
	return GoalContract{Outcome: v[0], Verification: v[1], Constraints: v[2], Boundaries: v[3], StopWhen: v[4]}
}

// Labelled lines of the non empty fields, empty for an empty contract
func (c GoalContract) RenderBlock() string {
	var lines []string
	for i, v := range c.values() {
		if s := pystr.Strip(v); s != "" {
			lines = append(lines, "- "+goalContractLabels[goalContractFields[i]]+": "+s)
		}
	}
	return strings.Join(lines, "\n")
}

// Deterministic shell command that must pass before a goal can be done
type GoalGate struct {
	Command        string
	TimeoutSeconds int64
	MaxRetries     int64
	Attempts       int64
	LastExitCode   *int64
	LastOutputTail string
}

// Gate for command with the default timeout and retry budget
func NewGoalGate(command string) GoalGate {
	return GoalGate{Command: command, TimeoutSeconds: DefaultGateTimeoutSeconds, MaxRetries: DefaultGateMaxRetries}
}

// Ordered dict of every gate field
func (g GoalGate) ToDict() *pyjson.Object {
	var exit any
	if g.LastExitCode != nil {
		exit = *g.LastExitCode
	}
	return pyjson.ObjectOf("command", g.Command, "timeout_seconds", g.TimeoutSeconds, "max_retries", g.MaxRetries,
		"attempts", g.Attempts, "last_exit_code", exit, "last_output_tail", g.LastOutputTail)
}

// Gate of a stored dict with Python int and str coercion, a blank gate for a non dict
func GoalGateFromDict(data any) (GoalGate, error) {
	d, ok := data.(*pyjson.Object)
	if !ok {
		return NewGoalGate(""), nil
	}
	get := func(k string) any { v, _ := d.Get(k); return v }
	g := GoalGate{Command: pyrepr.Str(pyrepr.Or(get("command"), ""))}
	var err error
	if g.TimeoutSeconds, err = pyint.Int64(pyrepr.Or(get("timeout_seconds"), int64(DefaultGateTimeoutSeconds))); err != nil {
		return GoalGate{}, err
	}
	if g.MaxRetries, err = pyint.Int64(pyrepr.Or(get("max_retries"), int64(DefaultGateMaxRetries))); err != nil {
		return GoalGate{}, err
	}
	if g.Attempts, err = pyint.Int64(pyrepr.Or(get("attempts"), int64(0))); err != nil {
		return GoalGate{}, err
	}
	if raw := get("last_exit_code"); raw != nil {
		n, err := pyint.Int64(raw)
		if err != nil {
			return GoalGate{}, err
		}
		g.LastExitCode = &n
	}
	g.LastOutputTail = pyrepr.Str(pyrepr.Or(get("last_output_tail"), ""))
	return g, nil
}

// Serializable goal state stored per session, uncoerced fields keeping their stored JSON value
type GoalState struct {
	Goal                         any
	Status                       any
	TurnsUsed                    int64
	MaxTurns                     int64
	CreatedAt                    float64
	LastTurnAt                   float64
	LastVerdict                  any
	LastReason                   any
	PausedReason                 any
	ConsecutiveParseFailures     int64
	ConsecutiveTransportFailures int64
	Subgoals                     []string
	WaitingOnPID                 *int64
	WaitingOnSession             *string
	WaitingUntil                 float64
	WaitingOnDelegations         int64
	WaitingReason                any
	WaitingSince                 float64
	Contract                     GoalContract
	Gates                        []GoalGate
}

// Active goal with the dataclass defaults
func NewGoalState(goal any) *GoalState {
	return &GoalState{Goal: goal, Status: "active", MaxTurns: DefaultGoalMaxTurns}
}

// JSON of every field in declaration order with non ASCII kept
func (s *GoalState) ToJSON() (string, error) {
	subgoals := make([]any, 0, len(s.Subgoals))
	for _, g := range s.Subgoals {
		subgoals = append(subgoals, g)
	}
	gates := make([]any, 0, len(s.Gates))
	for _, g := range s.Gates {
		gates = append(gates, g.ToDict())
	}
	var pid, waitSession any
	if s.WaitingOnPID != nil {
		pid = *s.WaitingOnPID
	}
	if s.WaitingOnSession != nil {
		waitSession = *s.WaitingOnSession
	}
	o := pyjson.ObjectOf(
		"goal", s.Goal, "status", s.Status, "turns_used", s.TurnsUsed, "max_turns", s.MaxTurns,
		"created_at", s.CreatedAt, "last_turn_at", s.LastTurnAt, "last_verdict", s.LastVerdict,
		"last_reason", s.LastReason, "paused_reason", s.PausedReason,
		"consecutive_parse_failures", s.ConsecutiveParseFailures,
		"consecutive_transport_failures", s.ConsecutiveTransportFailures, "subgoals", subgoals,
		"waiting_on_pid", pid, "waiting_on_session", waitSession, "waiting_until", s.WaitingUntil,
		"waiting_on_delegations", s.WaitingOnDelegations, "waiting_reason", s.WaitingReason,
		"waiting_since", s.WaitingSince, "contract", s.Contract.ToDict(), "gates", gates,
	)
	return pyjson.Dumps(o, pyjson.EnsureASCII(false))
}

// Goal state of a stored row with the Python coercions, legacy rows filling defaults
func GoalStateFromJSON(raw string) (*GoalState, error) {
	d, err := pyjson.LoadsDict(raw)
	if err != nil {
		return nil, err
	}
	get := func(k string) any { return d.GetOr(k, nil) }
	rawSubgoals := pyrepr.Or(get("subgoals"), []any{})
	s := &GoalState{}
	for _, f := range []struct {
		key string
		dst *int64
	}{{"turns_used", &s.TurnsUsed}, {"consecutive_parse_failures", &s.ConsecutiveParseFailures},
		{"consecutive_transport_failures", &s.ConsecutiveTransportFailures}, {"waiting_on_delegations", &s.WaitingOnDelegations}} {
		if *f.dst, err = pyint.Int64(pyrepr.Or(get(f.key), int64(0))); err != nil {
			return nil, err
		}
	}
	for _, f := range []struct {
		key string
		dst *float64
	}{{"created_at", &s.CreatedAt}, {"last_turn_at", &s.LastTurnAt}, {"waiting_until", &s.WaitingUntil}, {"waiting_since", &s.WaitingSince}} {
		if *f.dst, err = pyint.Float(pyrepr.Or(get(f.key), 0.0)); err != nil {
			return nil, err
		}
	}
	s.Goal = d.GetOr("goal", "")
	s.Status = d.GetOr("status", "active")
	if s.MaxTurns, err = pyint.Int64(pyrepr.Or(get("max_turns"), int64(DefaultGoalMaxTurns))); err != nil {
		return nil, err
	}
	s.LastVerdict, s.LastReason, s.PausedReason = get("last_verdict"), get("last_reason"), get("paused_reason")
	s.Subgoals = []string{}
	if list, ok := rawSubgoals.([]any); ok {
		for _, item := range list {
			if t := pystr.Strip(pyrepr.Str(item)); t != "" {
				s.Subgoals = append(s.Subgoals, t)
			}
		}
	}
	if pid := get("waiting_on_pid"); pyrepr.Truthy(pid) {
		n, err := pyint.Int64(pid)
		if err != nil {
			return nil, err
		}
		s.WaitingOnPID = &n
	}
	if ws := get("waiting_on_session"); pyrepr.Truthy(ws) {
		v := pyrepr.Str(ws)
		s.WaitingOnSession = &v
	}
	s.WaitingReason = get("waiting_reason")
	s.Contract = GoalContractFromDict(get("contract"))
	items, err := pyIterate(pyrepr.Or(get("gates"), []any{}))
	if err != nil {
		return nil, err
	}
	s.Gates = []GoalGate{}
	for _, item := range items {
		g, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		cmd, _ := g.Get("command")
		if pystr.Strip(pyrepr.Str(pyrepr.Or(cmd, ""))) == "" {
			continue
		}
		gate, err := GoalGateFromDict(g)
		if err != nil {
			return nil, err
		}
		s.Gates = append(s.Gates, gate)
	}
	return s, nil
}

// True when a non empty contract is attached
func (s *GoalState) HasContract() bool { return !s.Contract.IsEmpty() }

// Numbered subgoal lines, empty when there are no subgoals
func (s *GoalState) RenderSubgoalsBlock() string {
	lines := make([]string, len(s.Subgoals))
	for i, t := range s.Subgoals {
		lines[i] = fmt.Sprintf("- %d. %s", i+1, t)
	}
	return strings.Join(lines, "\n")
}

// Lifts every wait barrier
func (s *GoalState) ClearWait() {
	s.WaitingOnPID, s.WaitingOnSession, s.WaitingReason = nil, nil, nil
	s.WaitingUntil, s.WaitingSince, s.WaitingOnDelegations = 0, 0, 0
}

// Elements Python iteration yields for a JSON value, the TypeError for a scalar
func pyIterate(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		out := make([]any, 0, t.Len())
		for _, k := range t.Keys {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

func GoalMetaKey(sessionID string) string { return "goal:" + sessionID }

// Session database cache per home with its off loop bootstrap bookkeeping
var (
	goalsDBMu       sync.Mutex
	goalsDBCache    = map[string]*state.DB{}
	goalsDBInflight = map[string]chan struct{}{}
	goalsDBLoopWait = 250 * time.Millisecond
	goalsDBInitWait = 1500 * time.Millisecond
)

// Empties the per home session database cache the way tests reset the module level dict
func ClearSessionDBCache() {
	goalsDBMu.Lock()
	goalsDBCache = map[string]*state.DB{}
	goalsDBMu.Unlock()
}

// Acquires the home database off the caller and caches it unless another caller won the race
func goalsBootstrapSessionDB(h string, done chan struct{}) {
	db, err := goalsAcquireSessionDB(context.Background(), h)
	if err != nil {
		goalsLogger().Debug(fmt.Sprintf("GoalManager: background SessionDB() raised (%s)", err))
		db = nil
	}
	goalsDBMu.Lock()
	if db != nil {
		if _, ok := goalsDBCache[h]; !ok {
			goalsDBCache[h] = db
			db = nil
		}
	}
	delete(goalsDBInflight, h)
	goalsDBMu.Unlock()
	if db != nil {
		goalsReleaseSessionDB(db)
	}
	close(done)
}

// Cached registry database of the home in effect, never opened inline on a running loop, nil on failure
func SessionDB(ctx context.Context) *state.DB {
	h := home.Dir(ctx)
	goalsDBMu.Lock()
	cached := goalsDBCache[h]
	goalsDBMu.Unlock()
	if cached != nil && goalsRegistryToreDown(cached) {
		goalsDBMu.Lock()
		if goalsDBCache[h] == cached {
			delete(goalsDBCache, h)
		}
		goalsDBMu.Unlock()
		cached = nil
	}
	if cached != nil {
		return cached
	}
	if pyctx.RunningLoop(ctx) {
		goalsDBMu.Lock()
		if c := goalsDBCache[h]; c != nil {
			goalsDBMu.Unlock()
			return c
		}
		done := goalsDBInflight[h]
		wait := goalsDBLoopWait
		if done == nil {
			done = make(chan struct{})
			goalsDBInflight[h] = done
			go goalsBootstrapSessionDB(h, done)
			wait = goalsDBInitWait
		}
		goalsDBMu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-done:
		case <-timer.C:
		}
		timer.Stop()
		goalsDBMu.Lock()
		defer goalsDBMu.Unlock()
		return goalsDBCache[h]
	}
	db, err := goalsAcquireSessionDB(ctx, h)
	if err != nil {
		goalsLogger().Debug(fmt.Sprintf("GoalManager: SessionDB() raised (%s)", err))
		return nil
	}
	goalsDBMu.Lock()
	defer goalsDBMu.Unlock()
	if existing := goalsDBCache[h]; existing != nil {
		goalsReleaseSessionDB(db)
		return existing
	}
	goalsDBCache[h] = db
	return db
}

// Shared registry handle of the home state database, a package var so tests substitute the open
var goalsAcquireSessionDB = func(ctx context.Context, h string) (*state.DB, error) {
	return state.Acquire(ctx, home.StatePath(h))
}

func goalsReleaseSessionDB(db *state.DB) { state.ReleaseOrClose(db) }

// True once the registry force closed the handle so the cache entry is stale
func goalsRegistryToreDown(db *state.DB) bool { return !db.SharedRegistryOwned() }

// Warns that a state write was dropped because the session database is unavailable
func WarnDroppedWrite(manager, kind, sessionID string) {
	goalsLogger().Warn(fmt.Sprintf("%s: %s for %s not persisted — session DB unavailable "+
		"(bootstrap window exceeded, in-memory state still active)", manager, kind, sessionID))
}

// Stored goal of a session, nil when absent, unreadable or unparseable
func LoadGoal(ctx context.Context, sessionID string) *GoalState {
	if sessionID == "" {
		return nil
	}
	db := SessionDB(ctx)
	if db == nil {
		return nil
	}
	raw, _, err := db.GetMeta(ctx, GoalMetaKey(sessionID))
	if err != nil {
		goalsLogger().Debug(fmt.Sprintf("GoalManager: get_meta failed: %s", err))
		return nil
	}
	if raw == "" {
		return nil
	}
	st, err := GoalStateFromJSON(raw)
	if err != nil {
		goalsLogger().Warn(fmt.Sprintf("GoalManager: could not parse stored goal for %s: %s", sessionID, err))
		return nil
	}
	return st
}

// Persists a goal to the session database, warning when the database is unavailable
func SaveGoal(ctx context.Context, sessionID string, st *GoalState) {
	if sessionID == "" {
		return
	}
	db := SessionDB(ctx)
	if db == nil {
		WarnDroppedWrite("GoalManager", "goal", sessionID)
		return
	}
	raw, err := st.ToJSON()
	if err == nil {
		err = db.SetMeta(ctx, GoalMetaKey(sessionID), raw)
	}
	if err != nil {
		goalsLogger().Debug(fmt.Sprintf("GoalManager: set_meta failed: %s", err))
	}
}

// Marks the stored goal cleared, keeping the row for audit
func ClearGoal(ctx context.Context, sessionID string) {
	st := LoadGoal(ctx, sessionID)
	if st == nil {
		return
	}
	st.Status = "cleared"
	SaveGoal(ctx, sessionID, st)
}

// Copies a live goal onto the continuation session and archives the parent row, true when migrated
func MigrateGoalToSession(ctx context.Context, oldSessionID, newSessionID, reason string) bool {
	if oldSessionID == "" || newSessionID == "" || oldSessionID == newSessionID {
		return false
	}
	st := LoadGoal(ctx, oldSessionID)
	if st == nil || st.Status == "cleared" {
		return false
	}
	if LoadGoal(ctx, newSessionID) != nil {
		return false
	}
	SaveGoal(ctx, newSessionID, st)
	ClearGoal(ctx, oldSessionID)
	if reason == "" {
		reason = "rotation"
	}
	goalsLogger().Debug(fmt.Sprintf("GoalManager: migrated goal %s -> %s (%s)", oldSessionID, newSessionID, reason))
	return true
}
