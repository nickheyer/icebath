package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch key slots and config secret refs the local policies outside the retired sessions still name
func secretRefs(ctx context.Context, a *SessionAuthority, retired map[string]bool) (map[string]bool, error) {
	chats := map[string]bool{}
	a.mu.Lock()
	for sid, live := range a.Sessions {
		if !retired[sid] && live.Source != nil && live.Source.Platform == PlatformLocal {
			chats[live.Source.ChatID] = true
		}
	}
	for sid := range a.retiredLocal {
		if !retired[sid] {
			chats[sid] = true
		}
	}
	a.mu.Unlock()
	refs := map[string]bool{}
	if len(chats) == 0 {
		return refs, nil
	}
	adapters, err := localAdapterMap(ctx, a)
	if err != nil {
		return nil, err
	}
	held, _ := adapters.Get(PlatformLocal)
	local, _ := held.(*LocalSessionAdapter)
	if local == nil {
		return refs, nil
	}
	for chat := range chats {
		policy := local.policy(chat)
		if policy == nil {
			continue
		}
		if policy.CredentialRef != nil && *policy.CredentialRef != "" {
			refs[keySlot(*policy.CredentialRef)] = true
		}
		if policy.ConfigSecretRef != nil && *policy.ConfigSecretRef != "" {
			refs[*policy.ConfigSecretRef] = true
		}
	}
	return refs, nil
}

// Drops the retired sessions' launch keys, frozen config secrets and borrowed editor servers no kept policy names
func releaseLaunchSecrets(ctx context.Context, a *SessionAuthority, sessionIDs []string) error {
	retired := map[string]bool{}
	for _, sid := range sessionIDs {
		retired[sid] = true
	}
	if len(retired) == 0 {
		return nil
	}
	kept, err := secretRefs(ctx, a, retired)
	if err != nil {
		return err
	}
	prefix := fmt.Sprintf("%s:%d:", a.InstanceID, a.Epoch)
	if slots := a.launchSecretSlots(); slots != nil {
		slots.mu.Lock()
		for ref := range slots.keys {
			parts := strings.SplitN(ref, ":", 3)
			if retired[parts[len(parts)-1]] && !kept[ref] {
				delete(slots.keys, ref)
				a.mu.Lock()
				delete(a.localOwnerKeyRequests, ref)
				a.mu.Unlock()
			}
		}
		for ref := range slots.configs {
			owner, owned := "", false
			if strings.HasPrefix(ref, configSecretPrefix) {
				loaded, err := pyjson.Loads(ref[len(configSecretPrefix):])
				if err != nil {
					slots.mu.Unlock()
					return err
				}
				fields, ok := loaded.(*pyjson.Object)
				if !ok {
					slots.mu.Unlock()
					return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(loaded))}
				}
				if session, has := fields.Get("session"); has {
					owner, owned = session.(string)
				}
			} else if strings.HasPrefix(ref, prefix) {
				owner, owned = ref[len(prefix):], true
			}
			if owned && retired[owner] && !kept[ref] {
				delete(slots.configs, ref)
			}
		}
		slots.mu.Unlock()
	}
	for sid := range retired {
		setBorrowedEditorMCP(a, sessionScope(a, sid), nil, true)
	}
	return nil
}

// Tools env of the active home with the terminal scope bound to ctx
func terminalEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Drops each session's workspace binding under its owning profile's scope
func releaseSessionWorkspaces(ctx context.Context, a *SessionAuthority, sessionIDs []string) {
	env := terminalEnv(ownerScope(ctx, a, false))
	for _, sid := range sessionIDs {
		tools.UnbindTurnWorkspace(env, sid)
	}
}

// Whether the authority is one of its own runner's authorities, so one process coordinates both writes of a delete
func runtimeOwnsAuthority(a *SessionAuthority) bool {
	if runner, ok := a.Runner.(authorityRegistryRunner); ok {
		if registry := runner.SessionAuthorities(); registry != nil {
			for _, owned := range registry.Authorities() {
				if owned == a {
					return true
				}
			}
			return false
		}
	}
	if runner, ok := a.Runner.(launchAuthorityRunner); ok {
		return runner.SessionAuthority() == a
	}
	return false
}

// Database path of a routing store, empty when it names none
func routingDBPath(db SessionStoreDB) string {
	if pathed, ok := db.(interface{ Path() string }); ok {
		return pathed.Path()
	}
	return ""
}

// Drops committed deletions from owner memory, refusing storage_unavailable when a required routing fence failed
func retireLiveSessions(ctx context.Context, a *SessionAuthority, sessionIDs []string, requireFence bool) error {
	var fenceErr error
	var store *SessionStore
	if a.Runner != nil {
		store = a.Runner.SessionStore()
	}
	if store != nil {
		store.RetireRuntimeSessions(sessionIDs)
		routing := store.routingDB(ctx)
		if len(sessionIDs) > 0 && routing != nil && routingDBPath(routing) != a.DB.Path() {
			fenceErr = fenceRetiredRoutes(ctx, a, store, sessionIDs)
			if fenceErr != nil {
				slog.Warn("could not fence shared routing rows for deleted sessions: " + pyExceptionRepr(fenceErr))
			}
		}
		if len(sessionIDs) > 0 {
			if err := store.StripRetiredMirrorRoutes(ctx, sessionIDs); err != nil {
				slog.Warn("could not strip retired routes from sessions.json: " + pyExceptionRepr(err))
			}
		}
	}
	if err := releaseLaunchSecrets(ctx, a, sessionIDs); err != nil {
		return err
	}
	releaseSessionWorkspaces(ctx, a, sessionIDs)
	for _, sid := range sessionIDs {
		allowForSession(a.ProfileID, sid)
		releaseUnlocks(sid)
		a.mu.Lock()
		candidate := a.Sessions[sid]
		delete(a.Sessions, sid)
		_, retiredRoute := a.retiredRoutes[sid]
		if candidate != nil {
			delete(a.retiredRoutes, sid)
		}
		a.mu.Unlock()
		if candidate == nil && retiredRoute {
			if err := forgetRetired(ctx, a, sid); err != nil {
				return err
			}
		}
		if candidate == nil {
			continue
		}
		if evicting, ok := a.Runner.(agentEvictingRunner); ok {
			evicting.EvictCachedAgent(candidate.Route)
		}
		if err := forgetLiveSession(ctx, a, sid, candidate); err != nil {
			return err
		}
		if liveDetached(candidate) {
			dropOwnerHints(a.Runner.SessionStore(), candidate.Route)
		}
	}
	releaseEditorMCP(ctx, a, sessionIDs)
	releaseSessionPrefill(a, sessionIDs)
	if fenceErr != nil && requireFence {
		refusal := state.NewRuntimeStoreError("storage_unavailable", nil, nil)
		refusal.Cause = fenceErr
		return refusal
	}
	return nil
}

// Fences the deleted ids in the launch home's shared routing index under this profile's namespace
func fenceRetiredRoutes(ctx context.Context, a *SessionAuthority, store *SessionStore, sessionIDs []string) error {
	profile := ""
	if runner, ok := a.Runner.(authorityRegistryRunner); ok {
		if registry := runner.SessionAuthorities(); registry != nil {
			profile, _ = registry.ProfileName(a)
		}
	}
	expiring, err := a.DB.ExpiringRetirements(ctx, sessionIDs)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(expiring))
	for _, sid := range sessionIDs {
		if expiring[sid] {
			ids = append(ids, sid)
		}
	}
	namespace := strings.SplitN(SessionKeyNamespace(profile), ":", 2)[1]
	return store.FenceRetiredRoutes(ctx, sessionIDs, namespace, ids)
}
