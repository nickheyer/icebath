package gateway

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner that revives a persisted routing entry's source pinned to its receiving bot
type restoredSourceRunner interface {
	RestoredSource(ctx context.Context, entry *SessionEntry) (*SessionSource, error)
}

// Routing entry that still names the session under the route, nil otherwise
func routedEntry(ctx context.Context, a *SessionAuthority, sid, route string) (*SessionEntry, error) {
	store := a.Runner.SessionStore()
	store.mu.Lock()
	entry := store.entryLocked(ctx, route)
	store.mu.Unlock()
	if entry == nil || entry.Origin == nil || entry.SessionKey != route {
		return nil, nil
	}
	owner, err := a.LogicalOwner(ctx, entry.SessionID)
	if err != nil || owner != sid {
		return nil, err
	}
	return entry, nil
}

// True when the lane's current routing entry names a conversation other than the session
func namesAnotherConversation(ctx context.Context, a *SessionAuthority, sid, route string) (bool, error) {
	store := a.Runner.SessionStore()
	store.mu.Lock()
	entry := store.entryLocked(ctx, route)
	store.mu.Unlock()
	if entry == nil {
		return false, nil
	}
	owner, err := a.LogicalOwner(ctx, entry.SessionID)
	return err == nil && owner != sid, err
}

// Routing entry rebuilt from the row alone once the route no longer vouches for it, nil without a matching source
func rowEntry(ctx context.Context, row *pyjson.Object, platform Platform, route, profile string) (*SessionEntry, error) {
	source := rowSource(ctx, row, platform, profile)
	if source == nil || source.Platform != platform {
		return nil, nil
	}
	started := sessionNow()
	if stamp, _ := row.Get("started_at"); pyTruthy(stamp) {
		seconds, err := pyint.Float(stamp)
		if err != nil {
			return nil, err
		}
		whole, frac := math.Modf(seconds)
		started = pyDatetime{T: time.Unix(int64(whole), int64(math.Round(frac*1e6))*1000)}
	}
	id, has := row.Get("id")
	if !has {
		return nil, &pyKeyError{key: "id"}
	}
	entry := NewSessionEntry(route, pyStrOf(id), started, started)
	entry.Origin, entry.Platform, entry.ChatType = source, platform, source.ChatType
	entry.TransportProfile = rowOptStr(row, "transport_profile")
	return entry, nil
}

// Route, entry and still routed entry of a row a route or a messaging platform row can stand for
type mountPlanResult struct {
	route  string
	entry  *SessionEntry
	routed *SessionEntry
}

// Mount plan of a session this profile's messaging lineage root can stand for, nil otherwise
func mountPlan(ctx context.Context, a *SessionAuthority, sid string) (*mountPlanResult, error) {
	row, err := a.DB.GetSession(ctx, sid)
	if err != nil {
		return nil, err
	}
	route := ""
	if row != nil {
		if key, _ := row.Get("session_key"); pyTruthy(key) {
			route = pyStrOf(key)
		}
	}
	if route == "" {
		return nil, nil
	}
	label, has := row.Get("source")
	if !has {
		return nil, &pyKeyError{key: "source"}
	}
	platform, messaging := messagingPlatform(ctx, label)
	if !messaging {
		return nil, nil
	}
	owner, err := a.LogicalOwner(ctx, sid)
	if err != nil || owner != sid {
		return nil, err
	}
	store := a.Runner.SessionStore()
	profile := store.namedProfileForKey(route)
	served, _ := servedProfile(a)
	if profile != served {
		return nil, nil
	}
	routed, err := routedEntry(ctx, a, sid, route)
	if err != nil {
		return nil, err
	}
	entry := routed
	if entry == nil {
		if entry, err = rowEntry(ctx, row, platform, route, profile); err != nil || entry == nil {
			return nil, err
		}
	}
	return &mountPlanResult{route: route, entry: entry, routed: routed}, nil
}

// Publishes a session as its lane would after a restart, nil when no route can stand for it
func restoreRoutedSession(ctx context.Context, a *SessionAuthority, sid string) (*SessionRef, error) {
	plan, err := mountPlan(ctx, a, sid)
	if err != nil || plan == nil {
		return nil, err
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	runner, ok := a.Runner.(restoredSourceRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_restored_source'", a.Runner)}
	}
	source, err := runner.RestoredSource(ctx, plan.entry)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	_, held := a.Sessions[sid]
	var live *LiveSession
	if !held {
		live = NewLiveSession(source, plan.route)
		a.Sessions[sid] = live
	}
	a.mu.Unlock()
	if live != nil && plan.routed == nil {
		moved, err := namesAnotherConversation(ctx, a, sid, plan.route)
		if err != nil {
			return nil, err
		}
		if moved {
			if err := detach(ctx, a, live, sid); err != nil {
				return nil, err
			}
		}
	}
	return &SessionRef{ProfileID: a.ProfileID, SessionID: sid}, nil
}

// Moves a messaging conversation its chat no longer names off the chat's route onto its own detached key
func detach(ctx context.Context, a *SessionAuthority, live *LiveSession, sessionID string) error {
	if live.Source == nil {
		return nil
	}
	if route := detachedRouteOf(live.Source); route != nil && *route != "" {
		return nil
	}
	adapter, err := localAdapter(ctx, a)
	if err != nil {
		return err
	}
	local, ok := adapter.(*LocalSessionAdapter)
	if !ok {
		return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'register_source'", adapter)}
	}
	route := detachedKey(live.Route, sessionID)
	source := ReplaceSource(live.Source, func(s *SessionSource) {
		s.DetachedRoute = &route
		s.MessageID = nil
	})
	a.mu.Lock()
	live.Route, live.Source = route, source
	a.mu.Unlock()
	local.registerSource(source)
	return nil
}

// True when a routed restore would mount the session, this profile's messaging lineage root
func mountable(ctx context.Context, a *SessionAuthority, sid string) (bool, error) {
	plan, err := mountPlan(ctx, a, sid)
	return plan != nil, err
}

// Mount a messaging row resumes on, its compression lineage root, any other row keeping its own ref
func messagingOwnerRef(ctx context.Context, a *SessionAuthority, ref SessionRef) (SessionRef, error) {
	row, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		return SessionRef{}, err
	}
	if row == nil {
		return ref, nil
	}
	if key, _ := row.Get("session_key"); !pyTruthy(key) {
		return ref, nil
	}
	label, has := row.Get("source")
	if !has {
		return SessionRef{}, &pyKeyError{key: "source"}
	}
	if _, messaging := messagingPlatform(ctx, label); !messaging {
		return ref, nil
	}
	owner, err := a.LogicalOwner(ctx, ref.SessionID)
	if err != nil || owner == ref.SessionID {
		return ref, err
	}
	return SessionRef{ProfileID: ref.ProfileID, SessionID: owner}, nil
}

// Mount of a local conversation a messaging lane took over, nil for any other row
func handedOffRef(ctx context.Context, a *SessionAuthority, sessionID string) (*SessionRef, error) {
	ref, err := messagingOwnerRef(ctx, a, SessionRef{ProfileID: a.ProfileID, SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	row, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		return nil, err
	}
	var created any
	if row != nil {
		created, _ = row.Get("created_source")
	}
	if !state.LegacyLocalSource(created) && !state.ClientLabel(created) {
		return nil, nil
	}
	mounts, err := mountable(ctx, a, ref.SessionID)
	if err != nil || !mounts {
		return nil, err
	}
	return &ref, nil
}

// True once the lane that took over a handed off conversation moved on to another one
func supersededHandOff(ctx context.Context, a *SessionAuthority, ref SessionRef) (bool, error) {
	if live, ok := a.live(ref.SessionID); ok && liveDetached(live) {
		return true, nil
	}
	row, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		return false, err
	}
	route := ""
	if row != nil {
		if key, _ := row.Get("session_key"); key != nil {
			route = pyStrOf(key)
		}
	}
	return namesAnotherConversation(ctx, a, ref.SessionID, route)
}

// True for a messaging conversation its chat moved past, pinned to a detached route of its own
func liveDetached(live *LiveSession) bool {
	return detachedRouteOf(live.Source) != nil
}

// Authority, local owner and row id a repoint of a session onto a messaging lane carries, the conversation's current head
func conversationHead(ctx context.Context, runner any, sessionID string) (*SessionAuthority, *string, string, error) {
	authority, err := activeAuthority(ctx, runner)
	if err != nil {
		return nil, nil, "", err
	}
	var db *state.SessionDB
	if authority != nil {
		db = authority.DB
	} else if held, ok := runner.(sessionDBRunner); ok {
		db = held.SessionDB(ctx)
	} else {
		return nil, nil, "", &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_session_db'", runner)}
	}
	if db == nil {
		return authority, nil, sessionID, nil
	}
	owner, head, err := db.HandoffHead(ctx, sessionID)
	if err != nil {
		return nil, nil, "", err
	}
	if owner == "" {
		return authority, nil, head, nil
	}
	return authority, &owner, head, nil
}

// Moves the local mount a viewer still holds onto the lane a handoff repointed, then publishes the route
func followHandOff(ctx context.Context, a *SessionAuthority, source *SessionSource, route, owner string, localOwner *string) error {
	a.mu.Lock()
	var held *LiveSession
	if localOwner != nil && *localOwner != "" {
		held = a.Sessions[*localOwner]
	}
	if held == nil || held.Source == nil || held.Source.Platform != PlatformLocal {
		held = nil
	} else if *localOwner != owner {
		held.Source, held.Route = source, route
	}
	_, live := a.Sessions[owner]
	a.mu.Unlock()
	if held != nil || live {
		_, err := a.publishRoute(ctx, source, owner, route)
		return err
	}
	return nil
}
