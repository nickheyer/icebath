package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Records of the session runtime lifecycle carry the run_runtime module name
func runtimeLog() *slog.Logger { return slog.Default().With("logger", "gateway.run_runtime") }

// Context scoped to the authority's owning profile, its secrets hydrated when asked
func OwnerScope(ctx context.Context, a *SessionAuthority, hydrateSecrets bool) context.Context {
	return ownerScope(ctx, a, hydrateSecrets)
}

// Every authority the runner holds, retiring ones included
func AllAuthorities(runner any) []*SessionAuthority { return allAuthorities(runner) }

// Authorities that accept work, no unbuilt slot and none retiring
func ServingAuthorities(runner any) []*SessionAuthority { return servingAuthorities(runner) }

// Canonical profile id of a served home, default for the root and any custom home
func ServedProfileName(profileHome string) string { return servedProfileName(profileHome) }

// Admission identity a store row answers to, its compression root then its local creation id
func AdmissionOwner(ctx context.Context, a *SessionAuthority, sessionID string) (string, error) {
	sid, err := a.LogicalOwner(ctx, sessionID)
	if err != nil || sid == "" {
		return sid, err
	}
	var owner string
	err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var lineageErr error
		owner, lineageErr = state.LocalLineageOwner(ctx, conn, sid)
		return lineageErr
	})
	return owner, err
}

// Clears crash left turn markers whose canonical admission recovered unknown, so no reply is owed
func ReleaseUnknownTurnMarkers(ctx context.Context, runner any, store *SessionStore) error {
	if store == nil {
		return nil
	}
	for _, authority := range allAuthorities(runner) {
		scoped := ownerScope(ctx, authority, false)
		rows, err := authority.DB.ReadAll(scoped, "SELECT DISTINCT target_session_id FROM session_admissions WHERE status='unknown'")
		if err != nil {
			return err
		}
		unknown := map[string]bool{}
		for _, row := range rows {
			v, _ := row.Get("target_session_id")
			unknown[pyrepr.Str(v)] = true
		}
		if len(unknown) == 0 {
			continue
		}
		for _, entry := range store.ListSessions(scoped, nil) {
			if entry.ActiveTurnToken == nil || *entry.ActiveTurnToken == "" {
				continue
			}
			owner, err := AdmissionOwner(scoped, authority, entry.SessionID)
			if err != nil {
				return err
			}
			if unknown[owner] {
				if _, err := store.ClearTurnActive(scoped, entry.SessionKey, *entry.ActiveTurnToken); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Runner that resolves the adapter a stored origin delivers through
type nativeRecoveryRunner interface {
	SessionStore() *SessionStore
	AdapterForSource(ctx context.Context, source *SessionSource) (PlatformAdapter, error)
}

// Binds one authority's queued native rows to the adapters connected now, returning each session's verdict
func RecoverProfileNativeSessions(ctx context.Context, runner any, authority *SessionAuthority) (*pyjson.Object, error) {
	r, ok := runner.(nativeRecoveryRunner)
	if !ok {
		return nil, fmt.Errorf("'%T' object has no attribute 'session_store'", runner)
	}
	scoped := ownerScope(ctx, authority, false)
	rows, err := authority.DB.ReadAll(scoped, "SELECT DISTINCT target_session_id FROM session_admissions WHERE status IN ('queued','unknown')")
	if err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	for _, row := range rows {
		v, _ := row.Get("target_session_id")
		pending[pyrepr.Str(v)] = true
	}
	var bindings []NativeBinding
	for _, entry := range r.SessionStore().ListSessions(scoped, nil) {
		if entry.Origin == nil {
			continue
		}
		owner, err := authority.LogicalOwner(scoped, entry.SessionID)
		if err != nil {
			return nil, err
		}
		if !pending[owner] {
			continue
		}
		adapter, err := r.AdapterForSource(scoped, entry.Origin)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, NativeBinding{SessionID: owner, Source: entry.Origin, Adapter: adapter})
	}
	outcome, err := authority.RecoverNativeSessions(scoped, bindings)
	if err != nil {
		return nil, err
	}
	rest := map[string]bool{}
	for sid := range pending {
		if _, ok := outcome.Get(sid); !ok {
			rest[sid] = true
		}
	}
	detached, err := RecoverDetachedSessions(scoped, authority, rest)
	if err != nil {
		return nil, err
	}
	for _, sid := range detached {
		outcome.Set(sid, "detached")
	}
	for _, sid := range outcome.Keys {
		verdict, _ := outcome.Get(sid)
		runtimeLog().Info(fmt.Sprintf("Native session startup recovery %s (%s): %s", sid, authority.ProfileID, pyrepr.Str(verdict)))
	}
	return outcome, nil
}
