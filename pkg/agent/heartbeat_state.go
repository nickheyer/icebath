package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/nickheyer/icebath/pkg/goals"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const heartbeatMetaPrefix = "heartbeat:"

func heartbeatLogger() *slog.Logger { return slog.Default().With("logger", "hermes_cli.heartbeat") }

// Serializable per session heartbeat
type HeartbeatState struct {
	Prompt          string
	IntervalSeconds *big.Int
	Status          string
	CreatedAt       float64
	LastFiredAt     float64
	FireCount       int64
}

// Active heartbeat of prompt every intervalSeconds with the dataclass defaults
func NewHeartbeatState(prompt string, intervalSeconds int64) *HeartbeatState {
	return &HeartbeatState{Prompt: prompt, IntervalSeconds: big.NewInt(intervalSeconds), Status: "active"}
}

// JSON of every field in declaration order with non ASCII kept
func (s *HeartbeatState) ToJSON() (string, error) {
	return pyjson.Dumps(pyjson.ObjectOf("prompt", s.Prompt, "interval_seconds", pyint.Value(s.IntervalSeconds), "status", s.Status,
		"created_at", s.CreatedAt, "last_fired_at", s.LastFiredAt, "fire_count", s.FireCount), pyjson.EnsureASCII(false))
}

// Heartbeat of a stored row with each field coerced and a falsy value taking its default
func HeartbeatStateFromJSON(raw string) (*HeartbeatState, error) {
	d, err := pyjson.LoadsDict(raw)
	if err != nil {
		return nil, err
	}
	get := func(k string) any { return d.GetOr(k, nil) }
	s := &HeartbeatState{Prompt: pyrepr.Str(pyOr(get("prompt"), ""))}
	if s.IntervalSeconds, err = pyint.Of(pyOr(get("interval_seconds"), int64(0))); err != nil {
		return nil, err
	}
	s.Status = pyrepr.Str(pyOr(get("status"), "active"))
	if s.CreatedAt, err = pyint.Float(pyOr(get("created_at"), 0.0)); err != nil {
		return nil, err
	}
	if s.LastFiredAt, err = pyint.Float(pyOr(get("last_fired_at"), 0.0)); err != nil {
		return nil, err
	}
	if s.FireCount, err = pyint.Int64(pyOr(get("fire_count"), int64(0))); err != nil {
		return nil, err
	}
	return s, nil
}

// Shared goals session database the heartbeat rows persist through
func heartbeatSessionDB(ctx context.Context) *state.DB { return goals.SessionDB(ctx) }

// Stored heartbeat of a session, nil when absent, cleared, unreadable or unparseable
func LoadHeartbeat(ctx context.Context, sessionID string) *HeartbeatState {
	if sessionID == "" {
		return nil
	}
	db := heartbeatSessionDB(ctx)
	if db == nil {
		return nil
	}
	raw, _, err := db.GetMeta(ctx, heartbeatMetaPrefix+sessionID)
	if err != nil {
		heartbeatLogger().Debug(fmt.Sprintf("HeartbeatManager: get_meta failed: %s", err))
		return nil
	}
	if raw == "" {
		return nil
	}
	st, err := HeartbeatStateFromJSON(raw)
	if err != nil {
		heartbeatLogger().Warn(fmt.Sprintf("HeartbeatManager: could not parse stored heartbeat for %s: %s", sessionID, err))
		return nil
	}
	if st.Status == "cleared" {
		return nil
	}
	return st
}

// Persists a heartbeat to the session database, warning when the database is unavailable
func SaveHeartbeat(ctx context.Context, sessionID string, st *HeartbeatState) {
	if sessionID == "" {
		return
	}
	db := heartbeatSessionDB(ctx)
	if db == nil {
		goals.WarnDroppedWrite("HeartbeatManager", "heartbeat", sessionID)
		return
	}
	raw, err := st.ToJSON()
	if err == nil {
		err = db.SetMeta(ctx, heartbeatMetaPrefix+sessionID, raw)
	}
	if err != nil {
		heartbeatLogger().Debug(fmt.Sprintf("HeartbeatManager: set_meta failed: %s", err))
	}
}

// Copies a heartbeat onto the continuation session and archives the parent row, true when migrated
func migrateHeartbeatToSession(ctx context.Context, oldSessionID, newSessionID string) bool {
	if oldSessionID == "" || newSessionID == "" || oldSessionID == newSessionID {
		return false
	}
	st := LoadHeartbeat(ctx, oldSessionID)
	if st == nil || LoadHeartbeat(ctx, newSessionID) != nil {
		return false
	}
	SaveHeartbeat(ctx, newSessionID, st)
	st.Status = "cleared"
	SaveHeartbeat(ctx, oldSessionID, st)
	return true
}
