package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// MinHeartbeatIntervalSeconds is the floor below which a heartbeat would be a busy loop
const MinHeartbeatIntervalSeconds = 60

// HeartbeatPollSeconds is how often drivers poll for due heartbeats
const HeartbeatPollSeconds = 5.0

const heartbeatPromptTemplate = "[Heartbeat — recurring instruction, fires every %s]\n%s\n\n" +
	"If there is nothing meaningful to do or report for this instruction " +
	"right now, reply briefly that nothing has changed and stop — do not invent work."

const heartbeatSpaceClass = `[\s\v\x1c-\x1f\x{85}\pZ]`

var heartbeatIntervalRE = regexp.MustCompile(`(?i)^` + heartbeatSpaceClass + `*(?:every` + heartbeatSpaceClass + `+)?(\p{Nd}+(?:\.\p{Nd}+)?)` +
	heartbeatSpaceClass + "*(s|sec|secs|seconds?|m|m[i\u0130\u0131]n|m[i\u0130\u0131]ns|m[i\u0130\u0131]nutes?|h|hr|hrs|hours?|d|days?)" + heartbeatSpaceClass + `*$`)

var heartbeatUnitSeconds = map[string]float64{
	"s": 1, "sec": 1, "secs": 1, "second": 1, "seconds": 1,
	"m": 60, "min": 60, "mins": 60, "minute": 60, "minutes": 60,
	"h": 3600, "hr": 3600, "hrs": 3600, "hour": 3600, "hours": 3600,
	"d": 86400, "day": 86400, "days": 86400,
}

var heartbeatClaimLock sync.Mutex

// Seconds of an interval like 10m or every 2h, nil when not one and minus one below the floor
func ParseHeartbeatInterval(text string) (*big.Int, error) {
	if text == "" {
		return nil, nil
	}
	m := heartbeatIntervalRE.FindStringSubmatch(text)
	if m == nil {
		return nil, nil
	}
	value, err := pyint.ParseFloat(m[1])
	if err != nil {
		return nil, err
	}
	unit := pystr.Lower(m[2])
	factor, known := heartbeatUnitSeconds[unit]
	if !known {
		return nil, &pyint.Error{Kind: "KeyError", Msg: pyrepr.StrRepr(unit)}
	}
	seconds, err := pyint.Of(value * factor)
	if err != nil {
		return nil, err
	}
	if seconds.Cmp(big.NewInt(MinHeartbeatIntervalSeconds)) < 0 {
		return big.NewInt(-1), nil
	}
	return seconds, nil
}

// Human readable interval, the largest whole unit of days, hours or minutes, else seconds
func FormatHeartbeatInterval(seconds *big.Int) string {
	for _, u := range []struct {
		size   int64
		suffix string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}} {
		quotient, remainder := new(big.Int).DivMod(seconds, big.NewInt(u.size), new(big.Int))
		if remainder.Sign() == 0 {
			return quotient.String() + u.suffix
		}
	}
	return seconds.String() + "s"
}

// Reports a float at or above an integer of any size, compared exactly
func floatAtLeastInt(f float64, n *big.Int) bool {
	if math.IsNaN(f) {
		return false
	}
	if math.IsInf(f, 0) {
		return f > 0
	}
	return new(big.Float).SetFloat64(f).Cmp(new(big.Float).SetInt(n)) >= 0
}

// Float of an integer of any size, the OverflowError float raises beyond its range
func intAsFloat(n *big.Int) (float64, error) {
	f, _ := new(big.Float).SetInt(n).Float64()
	if math.IsInf(f, 0) {
		return 0, &pyint.Error{Kind: "OverflowError", Msg: "int too large to convert to float"}
	}
	return f, nil
}

// Reports an active heartbeat whose interval elapsed since the last fire or creation
func (s *HeartbeatState) IsDue(now float64) bool {
	if s.Status != "active" || s.Prompt == "" || s.IntervalSeconds.Sign() <= 0 {
		return false
	}
	anchor := s.LastFiredAt
	if anchor == 0 {
		anchor = s.CreatedAt
	}
	return floatAtLeastInt(now-anchor, s.IntervalSeconds)
}

// User turn text a due heartbeat injects
func (s *HeartbeatState) RenderPrompt() string {
	return fmt.Sprintf(heartbeatPromptTemplate, FormatHeartbeatInterval(s.IntervalSeconds), s.Prompt)
}

// Reports an active or unparseable heartbeat row in the store, read errors returned to the caller
func StoreHasActiveHeartbeat(ctx context.Context, db *state.SessionDB) (bool, error) {
	rows, err := db.ListMetaPrefix(ctx, heartbeatMetaPrefix)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		raw, ok := row.Value.(string)
		if !ok {
			return true, nil
		}
		st, err := HeartbeatStateFromJSON(raw)
		if err != nil || st.Status == "active" {
			return true, nil
		}
	}
	return false, nil
}

// HeartbeatManager holds one session's heartbeat and decides its due ticks
type HeartbeatManager struct {
	SessionID string
	ctx       context.Context
	state     *HeartbeatState
	lastClaim *[2]float64
}

// Manager over the stored heartbeat of a session
func NewHeartbeatManager(ctx context.Context, sessionID string) *HeartbeatManager {
	return &HeartbeatManager{SessionID: sessionID, ctx: ctx, state: LoadHeartbeat(ctx, sessionID)}
}

// The current heartbeat, nil when none is stored
func (m *HeartbeatManager) State() *HeartbeatState { return m.state }

// Reports an active or paused heartbeat
func (m *HeartbeatManager) HasHeartbeat() bool {
	return m.state != nil && (m.state.Status == "active" || m.state.Status == "paused")
}

// Reports an active heartbeat
func (m *HeartbeatManager) IsActive() bool { return m.state != nil && m.state.Status == "active" }

// One line status of the heartbeat with its interval, next fire and prompt
func (m *HeartbeatManager) StatusLine() (string, error) {
	s := m.state
	if s == nil {
		return "No heartbeat. Set one with /heartbeat every <interval> <prompt>.", nil
	}
	every := FormatHeartbeatInterval(s.IntervalSeconds)
	fired := ""
	if s.FireCount != 0 {
		fired = fmt.Sprintf(", fired %d×", s.FireCount)
	}
	if s.Status == "active" {
		anchor := s.LastFiredAt
		if anchor == 0 {
			anchor = s.CreatedAt
		}
		interval, err := intAsFloat(s.IntervalSeconds)
		if err != nil {
			return "", err
		}
		nextIn, err := pyint.Of(anchor + interval - nowSeconds())
		if err != nil {
			return "", err
		}
		if nextIn.Sign() < 0 {
			nextIn = new(big.Int)
		}
		return fmt.Sprintf("♥ Heartbeat (every %s, next in ~%ss%s): %s", every, nextIn, fired, s.Prompt), nil
	}
	icon := ""
	if s.Status == "paused" {
		icon = "⏸ "
	}
	return fmt.Sprintf("%sHeartbeat (%s, every %s%s): %s", icon, s.Status, every, fired, s.Prompt), nil
}

// HeartbeatValueError is the ValueError a refused heartbeat set raises
type HeartbeatValueError struct{ Msg string }

func (e *HeartbeatValueError) Error() string      { return e.Msg }
func (e *HeartbeatValueError) PyTypeName() string { return "ValueError" }

// Stores a fresh active heartbeat, refusing an empty prompt or an interval under the floor
func (m *HeartbeatManager) Set(prompt string, intervalSeconds any) (*HeartbeatState, error) {
	prompt = pystr.Strip(prompt)
	if prompt == "" {
		return nil, &HeartbeatValueError{"heartbeat prompt is empty"}
	}
	interval, err := pyint.Of(intervalSeconds)
	if err != nil {
		return nil, err
	}
	if interval.Cmp(big.NewInt(MinHeartbeatIntervalSeconds)) < 0 {
		return nil, &HeartbeatValueError{fmt.Sprintf("interval must be at least %ds", MinHeartbeatIntervalSeconds)}
	}
	m.state = &HeartbeatState{Prompt: prompt, IntervalSeconds: interval, Status: "active", CreatedAt: nowSeconds()}
	SaveHeartbeat(m.ctx, m.SessionID, m.state)
	return m.state, nil
}

func (m *HeartbeatManager) setStatus(status string, reanchor bool) *HeartbeatState {
	if m.state == nil {
		return nil
	}
	heartbeatClaimLock.Lock()
	defer heartbeatClaimLock.Unlock()
	m.state.Status = status
	if reanchor {
		m.state.LastFiredAt = nowSeconds()
	}
	SaveHeartbeat(m.ctx, m.SessionID, m.state)
	return m.state
}

// Pauses the heartbeat, nil without one
func (m *HeartbeatManager) Pause() *HeartbeatState { return m.setStatus("paused", false) }

// Resumes the heartbeat re anchored so a stale tick never fires at once
func (m *HeartbeatManager) Resume() *HeartbeatState { return m.setStatus("active", true) }

// Clears the heartbeat, true when one existed
func (m *HeartbeatManager) Clear() bool {
	cleared := m.setStatus("cleared", false) != nil
	m.state = nil
	return cleared
}

// Claims a due tick and returns its prompt, false when not due or another manager took it
func (m *HeartbeatManager) DuePrompt(now *float64) (string, bool) {
	heartbeatClaimLock.Lock()
	defer heartbeatClaimLock.Unlock()
	at := nowSeconds()
	if now != nil {
		at = *now
	}
	s := m.state
	if s == nil || !s.IsDue(at) {
		return "", false
	}
	stored := LoadHeartbeat(m.ctx, m.SessionID)
	if stored == nil || stored.Status != "active" || stored.LastFiredAt != s.LastFiredAt || stored.FireCount != s.FireCount {
		m.state = stored
		return "", false
	}
	m.lastClaim = &[2]float64{s.LastFiredAt, float64(s.FireCount)}
	s.LastFiredAt = at
	s.FireCount++
	SaveHeartbeat(m.ctx, m.SessionID, s)
	return s.RenderPrompt(), true
}

// Rewinds the last claimed fire whose turn never started, false when the stored state moved on
func (m *HeartbeatManager) AbandonFire() bool {
	claim, s := m.lastClaim, m.state
	if claim == nil || s == nil {
		return false
	}
	current := LoadHeartbeat(m.ctx, m.SessionID)
	if current == nil || current.Status != "active" || current.LastFiredAt != s.LastFiredAt || current.FireCount != s.FireCount {
		return false
	}
	s.LastFiredAt, s.FireCount = claim[0], int64(claim[1])
	m.lastClaim = nil
	SaveHeartbeat(m.ctx, m.SessionID, s)
	return true
}

// Reports a refused heartbeat set
func IsHeartbeatValueError(err error) bool {
	var hv *HeartbeatValueError
	return errors.As(err, &hv)
}
