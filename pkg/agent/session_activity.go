package agent

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	mathutil "github.com/nickheyer/icebath/pkg/utils/math"
)

// SessionActivityHeartbeatMinIntervalSeconds is the floor between durable session activity heartbeat writes
const SessionActivityHeartbeatMinIntervalSeconds = 60.0

// TerminalCompressionProvenances are the stamps that end a compression phase and persist immediately
var TerminalCompressionProvenances = map[state.ActivityProvenance]bool{
	state.ActivityProvenanceAgentCompressionTimeout:  true,
	state.ActivityProvenanceAgentCompressionCooldown: true,
	state.ActivityProvenanceAgentCompressionTurnhold: true,
}

// Whether the stamp ends a compression phase and bypasses the persist rate limit
func IsTerminalCompressionProvenance(provenance string) bool {
	return TerminalCompressionProvenances[state.NormalizeActivityProvenance(provenance)]
}

// Iteration N of M for status lines, just N when the cap is unbounded or not an integer
func FormatIterationProgress(ctx context.Context, apiCallCount, maxIterations any) (string, error) {
	limit, err := anyutil.ToInt(maxIterations)
	if err != nil {
		if !errors.Is(err, anyutil.ErrNotNumber) {
			return "", err
		}
		limit = math.MaxInt64
	}
	if limit == math.MaxInt64 {
		return i18n.T(ctx, "display.iteration_progress.unbounded", "", map[string]any{"n": apiCallCount}), nil
	}
	return i18n.T(ctx, "display.iteration_progress.bounded", "", map[string]any{"n": apiCallCount, "max": limit}), nil
}

// Clears the persist rate limit so the next activity stamp writes through
func ResetSessionActivityPersistWindow(a *AIAgent) {
	if a != nil {
		a.sessionActivityLastPersistMono = 0.0
	}
}

// Shared activity snapshot with the short reader aliases, caller extras merged last
func BuildActivitySnapshot(lastActivityAt *float64, lastActivityDescription, lastActivityProvenance string, now *float64, extra *jsonutil.Object) *jsonutil.Object {
	clock := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		clock = *now
	}
	desc := state.BoundActivityDescription(lastActivityDescription)
	prov := string(state.NormalizeActivityProvenance(lastActivityProvenance))
	var when, since any
	if lastActivityAt != nil {
		when = *lastActivityAt
		since = mathutil.Round(clock-*lastActivityAt, 1)
	}
	snap := jsonutil.NewObject()
	snap.Set("last_activity_at", when)
	snap.Set("last_activity_description", desc)
	snap.Set("last_activity_provenance", prov)
	snap.Set("seconds_since_activity", since)
	snap.Set("last_activity_ts", when)
	snap.Set("last_activity_desc", desc)
	snap.Set("description", desc)
	snap.Set("provenance", prov)
	if extra != nil {
		for _, k := range extra.Keys() {
			snap.Set(k, extra.Value(k))
		}
	}
	return snap
}
