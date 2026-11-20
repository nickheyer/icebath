package gateway

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Validated prune filter keywords, scalar values of known names only
func pruneFilters(raw any) (map[string]any, error) {
	plain := pyjson.Plain(raw)
	filters, ok := plain.(map[string]any)
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	for key, value := range filters {
		if !state.PruneFilterNames[key] && key != "older_than_days" {
			return nil, runtimeRefusal("invalid_params")
		}
		switch value.(type) {
		case nil, bool, int, int64, float64, string:
		default:
			return nil, runtimeRefusal("invalid_params")
		}
	}
	out := make(map[string]any, len(filters))
	for key, value := range filters {
		out[key] = value
	}
	return out, nil
}

// Owner run bulk prune of its own store for the local operator, the delete tracked as a mutation
func prune(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	a, actor := c.authority, c.actor
	if !c.nativeOwner || !actor.Capabilities["session:operator"] {
		return nil, runtimeRefusal("permission_denied")
	}
	if actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("profile_mismatch")
	}
	neverActive := params.GetOr("never_active_days", nil)
	_, hasFilters := params.Get("filters")
	if hasFilters == (neverActive != nil) {
		return nil, runtimeRefusal("invalid_params")
	}
	var days float64
	if neverActive != nil {
		switch n := neverActive.(type) {
		case int64:
			days = float64(n)
		case int:
			days = float64(n)
		case float64:
			days = n
		default:
			return nil, runtimeRefusal("invalid_params")
		}
		if days < 0 {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	var filters map[string]any
	if hasFilters {
		raw, _ := params.Get("filters")
		var err error
		if filters, err = pruneFilters(raw); err != nil {
			return nil, err
		}
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	scoped := context.WithoutCancel(ctx)
	task := trackMutation(ctx, a, func(context.Context) (any, error) {
		return applyPrune(scoped, a, filters, days)
	})
	select {
	case <-task.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return task.wait()
}

// Deletes the matching rows through the housekeeping prune path and retires their live sessions
func applyPrune(ctx context.Context, a *SessionAuthority, filters map[string]any, neverActiveDays float64) (map[string]any, error) {
	sessionsDir := filepath.Join(home.Dir(ctx), "sessions")
	var deletedIDs []string
	var result map[string]any
	if filters == nil {
		deleted, routing, skipped, err := a.DB.PruneNeverActiveKeyedSessions(ctx, state.NeverActivePruneOptions{
			OlderThanDays: neverActiveDays, SessionsDir: sessionsDir, DeletedIDs: &deletedIDs})
		if err != nil {
			return nil, err
		}
		result = map[string]any{"deleted": deleted, "routing_deleted": routing, "skipped": skipped}
	} else {
		options := state.NewPruneSessionsOptions()
		older, source, parsed := state.PruneKwargs(filters)
		if _, given := filters["older_than_days"]; given {
			options.OlderThanDays = older
		}
		options.Source, options.Filters = source, parsed
		options.SessionsDir, options.ExcludeActiveWriteGuards, options.DeletedIDs = sessionsDir, true, &deletedIDs
		deleted, err := a.DB.PruneSessions(ctx, options)
		if err != nil {
			return nil, err
		}
		result = map[string]any{"deleted": deleted}
	}
	if err := retireLiveSessions(ctx, a, deletedIDs, false); err != nil {
		return nil, err
	}
	return result, nil
}
