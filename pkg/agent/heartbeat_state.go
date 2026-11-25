package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const heartbeatMetaPrefix = "heartbeat:"

// Serializable per session heartbeat
type HeartbeatState struct {
	Prompt          string
	IntervalSeconds int64
	Status          string
	CreatedAt       float64
	LastFiredAt     float64
	FireCount       int64
}

// Active heartbeat of prompt every intervalSeconds with the dataclass defaults
func NewHeartbeatState(prompt string, intervalSeconds int64) *HeartbeatState {
	return &HeartbeatState{Prompt: prompt, IntervalSeconds: intervalSeconds, Status: "active"}
}

// JSON of every field in declaration order with non ASCII kept
func (s *HeartbeatState) ToJSON() (string, error) {
	return jsonutil.Encode(jsonutil.ObjectOf("prompt", s.Prompt, "interval_seconds", s.IntervalSeconds, "status", s.Status,
		"created_at", s.CreatedAt, "last_fired_at", s.LastFiredAt, "fire_count", s.FireCount), jsonutil.Spaced())
}

// Heartbeat of a stored row with each field coerced and a falsy value taking its default
func HeartbeatStateFromJSON(raw string) (*HeartbeatState, error) {
	d, err := jsonutil.DecodeObject(raw)
	if err != nil {
		return nil, errchain.Reword(err, "stored heartbeat state is not a JSON mapping")
	}
	get := d.Value
	s := &HeartbeatState{Prompt: anyutil.String(anyutil.Or(get("prompt"), ""))}
	if s.IntervalSeconds, err = anyutil.ToInt(anyutil.Or(get("interval_seconds"), int64(0))); err != nil {
		return nil, err
	}
	s.Status = anyutil.String(anyutil.Or(get("status"), "active"))
	if s.CreatedAt, err = anyutil.ToFloat(anyutil.Or(get("created_at"), 0.0)); err != nil {
		return nil, err
	}
	if s.LastFiredAt, err = anyutil.ToFloat(anyutil.Or(get("last_fired_at"), 0.0)); err != nil {
		return nil, err
	}
	if s.FireCount, err = anyutil.ToInt(anyutil.Or(get("fire_count"), int64(0))); err != nil {
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
		pkgLog().Debug(fmt.Sprintf("HeartbeatManager: get_meta failed: %s", err))
		return nil
	}
	if raw == "" {
		return nil
	}
	st, err := HeartbeatStateFromJSON(raw)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("HeartbeatManager: could not parse stored heartbeat for %s: %s", sessionID, err))
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
		pkgLog().Debug(fmt.Sprintf("HeartbeatManager: set_meta failed: %s", err))
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
