package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/goals"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"

	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	loopMetaPrefix                    = "loop:"
	loopDefaultPauseReason            = "user-paused"
	loopResumeMaxDelaySeconds float64 = 5.0
)

// Whole seconds of a loops duration setting in effect for ctx, never below floor
func loopsSeconds(ctx context.Context, d *settings.Duration, floor int64) int64 {
	return max(floor, int64(d.Get(ctx)/time.Second))
}

func loopMinIntervalSeconds(ctx context.Context) int64 {
	return loopsSeconds(ctx, settings.LoopsMinIntervalSeconds, 5)
}

func loopMaxTicksDefault(ctx context.Context) int64 {
	return max(0, int64(settings.LoopsMaxTicks.Get(ctx)))
}

func loopSelfPacedFloorSeconds(ctx context.Context) int64 {
	return loopsSeconds(ctx, settings.LoopsSelfPacedFloorSeconds, 10)
}

// Serializable loop state stored per session, uncoerced fields keeping their stored JSON value
type SessionLoopState struct {
	Prompt             any
	Status             any
	Mode               any
	IntervalSeconds    float64
	CurrentDelay       float64
	Times              int64
	Until              string
	MaxTicks           int64
	TicksFired         int64
	CreatedAt          float64
	LastFiredAt        float64
	NextDueAt          float64
	AwaitingResponse   bool
	LastResponseDigest string
	PausedReason       any
	LastStopReason     any
	Route              *jsonutil.Object
}

// Active interval loop of prompt with the dataclass defaults
func NewSessionLoopState(prompt any) *SessionLoopState {
	return &SessionLoopState{Prompt: prompt, Status: "active", Mode: "interval", MaxTicks: int64(settings.LoopsMaxTicks.Default), Route: jsonutil.NewObject()}
}

// JSON of every field in declaration order with non ASCII kept
func (s *SessionLoopState) ToJSON() (string, error) {
	route := s.Route
	if route == nil {
		route = jsonutil.NewObject()
	}
	return jsonutil.Encode(jsonutil.ObjectOf(
		"prompt", s.Prompt, "status", s.Status, "mode", s.Mode, "interval_seconds", s.IntervalSeconds,
		"current_delay", s.CurrentDelay, "times", s.Times, "until", s.Until, "max_ticks", s.MaxTicks,
		"ticks_fired", s.TicksFired, "created_at", s.CreatedAt, "last_fired_at", s.LastFiredAt,
		"next_due_at", s.NextDueAt, "awaiting_response", s.AwaitingResponse,
		"last_response_digest", s.LastResponseDigest, "paused_reason", s.PausedReason,
		"last_stop_reason", s.LastStopReason, "route", route,
	), jsonutil.Spaced())

}

// Loop state of a stored row, a missing key taking its default and a falsy one its type zero
func SessionLoopStateFromJSON(raw string) (*SessionLoopState, error) {
	return sessionLoopStateFrom(raw)
}

// Loop state of a stored str or bytes value, the json.loads TypeError for anything else
func sessionLoopStateFrom(raw any) (*SessionLoopState, error) {
	var v any
	var err error
	switch t := raw.(type) {
	case string:
		v, err = jsonutil.Decode(t)
	case []byte:
		v, err = jsonutil.Decode(t)
	default:
		return nil, &errorutil.Error{Kind: "TypeError", Msg: "stored loop state is " + errchain.Kind(raw) + " where JSON text was expected"}
	}
	if err != nil {
		return nil, errchain.Reword(err, "stored loop state is not valid JSON")
	}
	d, ok := v.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("stored loop state is %s where a mapping was expected", errchain.Kind(v))
	}
	get := func(k string) any { x, _ := d.Get(k); return x }
	s := &SessionLoopState{
		Prompt: d.ValueOr("prompt", ""), Status: d.ValueOr("status", "active"), Mode: d.ValueOr("mode", "interval"),
		PausedReason: get("paused_reason"), LastStopReason: get("last_stop_reason"), Route: jsonutil.NewObject(),
	}
	if r, ok := get("route").(*jsonutil.Object); ok {
		s.Route = r
	}
	float := func(key string) (float64, error) { return anyutil.ToFloat(pyOr(d.ValueOr(key, 0.0), 0.0)) }
	integer := func(key string, def int64) (int64, error) { return anyutil.ToInt(pyOr(d.ValueOr(key, def), int64(0))) }
	if s.IntervalSeconds, err = float("interval_seconds"); err != nil {
		return nil, err
	}
	if s.CurrentDelay, err = float("current_delay"); err != nil {
		return nil, err
	}
	if s.Times, err = integer("times", 0); err != nil {
		return nil, err
	}
	s.Until = anyutil.String(pyOr(d.ValueOr("until", ""), ""))
	if s.MaxTicks, err = integer("max_ticks", int64(settings.LoopsMaxTicks.Default)); err != nil {
		return nil, err
	}
	if s.TicksFired, err = integer("ticks_fired", 0); err != nil {
		return nil, err
	}
	if s.CreatedAt, err = float("created_at"); err != nil {
		return nil, err
	}
	if s.LastFiredAt, err = float("last_fired_at"); err != nil {
		return nil, err
	}
	if s.NextDueAt, err = float("next_due_at"); err != nil {
		return nil, err
	}
	s.AwaitingResponse = pyTruthy(d.ValueOr("awaiting_response", false))
	s.LastResponseDigest = anyutil.String(pyOr(d.ValueOr("last_response_digest", ""), ""))
	return s, nil
}

// Shared goals session database the loop rows persist through
func loopsSessionDB(ctx context.Context) *state.DB { return goals.SessionDB(ctx) }

// Parsed loop row, nil on corrupt data with a warning when the session is named
func parseLoopState(raw any, sessionID string) *SessionLoopState {
	st, err := sessionLoopStateFrom(raw)
	if err != nil {
		if sessionID != "" {
			pkgLog().Warn(fmt.Sprintf("LoopManager: could not parse stored loop for %s: %s", sessionID, err))
		}
		return nil
	}
	return st
}

// Session meta key the loop row of the session is stored under
func loopMetaKey(sessionID string) string { return loopMetaPrefix + sessionID }

// Stored loop of a session, nil when absent, unreadable or unparseable
func LoadLoop(ctx context.Context, sessionID string) *SessionLoopState {
	if sessionID == "" {
		return nil
	}
	db := loopsSessionDB(ctx)
	if db == nil {
		return nil
	}
	raw, _, err := db.GetMeta(ctx, loopMetaKey(sessionID))
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("LoopManager: get_meta failed: %s", err))
		return nil
	}
	if raw == "" {
		return nil
	}
	return parseLoopState(raw, sessionID)
}

// Persists a loop to the session database, warning when the database is unavailable
func SaveLoop(ctx context.Context, sessionID string, st *SessionLoopState) {
	if sessionID == "" {
		return
	}
	db := loopsSessionDB(ctx)
	if db == nil {
		goals.WarnDroppedWrite("LoopManager", "loop", sessionID)
		return
	}
	raw, err := st.ToJSON()
	if err == nil {
		err = db.SetMeta(ctx, loopMetaKey(sessionID), raw)
	}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("LoopManager: set_meta failed: %s", err))
	}
}

// Marks the stored loop cleared, keeping the row for audit
func ClearLoop(ctx context.Context, sessionID string) {
	if st := LoadLoop(ctx, sessionID); st != nil {
		st.Status = "cleared"
		SaveLoop(ctx, sessionID, st)
	}
}

// Session and state of one active loop row
type ActiveLoop struct {
	SessionID string
	State     *SessionLoopState
}

// Every active loop in the session database, empty on any database error
func ListActiveLoops(ctx context.Context) []ActiveLoop {
	db := loopsSessionDB(ctx)
	out := []ActiveLoop{}
	if db == nil {
		return out
	}
	entries, err := db.ListMetaPrefix(ctx, loopMetaPrefix)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("LoopManager: list_meta_prefix failed: %s", err))
		return out
	}
	for _, e := range entries {
		sessionID := e.Key[len(loopMetaPrefix):]
		if sessionID == "" || !pyTruthy(e.Value) {
			continue
		}
		if st := parseLoopState(e.Value, ""); st != nil && st.Status == "active" {
			out = append(out, ActiveLoop{SessionID: sessionID, State: st})
		}
	}
	return out
}

// Copies a live loop onto the continuation session and archives the parent row, true when migrated
func migrateLoopToSession(ctx context.Context, oldSessionID, newSessionID, reason string) bool {
	if oldSessionID == "" || newSessionID == "" || oldSessionID == newSessionID {
		return false
	}
	st := LoadLoop(ctx, oldSessionID)
	if st == nil || st.Status == "cleared" || LoadLoop(ctx, newSessionID) != nil {
		return false
	}
	SaveLoop(ctx, newSessionID, st)
	ClearLoop(ctx, oldSessionID)
	if reason == "" {
		reason = "rotation"
	}
	pkgLog().Debug(fmt.Sprintf("LoopManager: migrated loop %s -> %s (%s)", oldSessionID, newSessionID, reason))
	return true
}

// Per session loop state with the user controls that persist it
type LoopManager struct {
	SessionID string
	state     *SessionLoopState
}

// Manager of a session with its stored loop loaded
func NewLoopManager(ctx context.Context, sessionID string) *LoopManager {
	return &LoopManager{SessionID: sessionID, state: LoadLoop(ctx, sessionID)}
}

func (m *LoopManager) State() *SessionLoopState { return m.state }

// Rereads the loop from the session database
func (m *LoopManager) Refresh(ctx context.Context) { m.state = LoadLoop(ctx, m.SessionID) }

func (m *LoopManager) IsActive() bool { return m.state != nil && m.state.Status == "active" }

func (m *LoopManager) save(ctx context.Context) *SessionLoopState {
	SaveLoop(ctx, m.SessionID, m.state)
	return m.state
}

// Starts a loop replacing any existing one, self paced when intervalSeconds is nil
func (m *LoopManager) Set(ctx context.Context, prompt string, intervalSeconds *int64, times int64, until string, route *jsonutil.Object) (*SessionLoopState, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, &errorutil.Error{Kind: "ValueError", Msg: "loop prompt is empty"}
	}
	now := nowSeconds()
	selfPaced := intervalSeconds == nil
	interval := 0.0
	if !selfPaced {
		interval = float64(max(*intervalSeconds, loopMinIntervalSeconds(ctx)))
	}
	st := NewSessionLoopState(prompt)
	st.Mode = "interval"
	st.CurrentDelay = interval
	if selfPaced {
		st.Mode = "self_paced"
		st.CurrentDelay = float64(loopSelfPacedFloorSeconds(ctx))
	}
	st.IntervalSeconds = interval
	st.Times = max(0, times)
	st.Until = strings.TrimSpace(until)
	st.MaxTicks = loopMaxTicksDefault(ctx)
	st.CreatedAt, st.NextDueAt = now, now
	if route != nil {
		st.Route = route.Clone()
	}
	m.state = st
	return m.save(ctx), nil
}

// Pauses an active or paused loop, the TypeError Python raises for an unhashable stored status
func (m *LoopManager) Pause(ctx context.Context, reason string) (*SessionLoopState, error) {
	s := m.state
	if s == nil {
		return nil, nil
	}
	in, err := pyStatusIn(s.Status, "active", "paused")
	if err != nil {
		return nil, err
	}
	if !in {
		return nil, nil
	}
	s.Status, s.PausedReason, s.AwaitingResponse = "paused", reason, false
	return m.save(ctx), nil
}

// Reactivates a loop that is not cleared, rearming at most five seconds out
func (m *LoopManager) Resume(ctx context.Context) *SessionLoopState {
	s := m.state
	if s == nil || s.Status == "cleared" {
		return nil
	}
	s.Status, s.PausedReason, s.AwaitingResponse = "active", nil, false
	delay := s.CurrentDelay
	if delay == 0 {
		delay = s.IntervalSeconds
	}
	if delay == 0 {
		delay = float64(loopSelfPacedFloorSeconds(ctx))
	}
	s.NextDueAt = nowSeconds() + min(delay, loopResumeMaxDelaySeconds)
	return m.save(ctx)
}

// Marks the loop cleared and forgets it, false when there was no live loop
func (m *LoopManager) Clear(ctx context.Context) bool {
	if m.state == nil || m.state.Status == "cleared" {
		return false
	}
	m.state.Status = "cleared"
	m.save(ctx)
	m.state = nil
	return true
}

// Python membership of a stored value in a set of strings, the TypeError for an unhashable value
func pyStatusIn(v any, members ...string) (bool, error) {
	switch v.(type) {
	case []any, *jsonutil.Object:
		return false, &errorutil.Error{Kind: "TypeError", Msg: "stored loop status is " + errchain.Kind(v) + " where text was expected"}
	}
	for _, m := range members {
		if v == m {
			return true, nil
		}
	}
	return false, nil
}
