package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Authority serving a profile home, nil when this runtime does not serve it
func AuthorityForHome(runner any, profileHome string) (*SessionAuthority, error) {
	return authorityForHome(runner, profileHome)
}

// Binds a verified native HTTP actor as a native legacy transport of the authority
func BindNativeTransport(a *SessionAuthority, actor Principal, identity map[string]any) {
	bindNativeTransport(a, actor, identity)
}

// Forgets the legacy transport binding of an actor
func UnbindNativeTransport(a *SessionAuthority, actor Principal) { unbindNativeTransport(a, actor) }

// Action specific session mutation shared by the socket and authenticated HTTP
func MutateSession(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error) {
	return mutateSession(ctx, a, actor, ref, params)
}

// History control actor of a principal, its held capabilities sorted
func HistoryActorOf(actor Principal) state.HistoryActor { return historyActor(actor) }
