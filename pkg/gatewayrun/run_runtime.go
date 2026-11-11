package gatewayrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Records of the session runtime lifecycle carry the run_runtime module name
func runtimeLog() *slog.Logger { return slog.Default().With("logger", "gateway.run_runtime") }

// Realpath of a home the way Path.resolve settles it
func resolveHome(h string) string {
	resolved, err := pypath.Realpath(h, pypath.Lax)
	if err != nil {
		return h
	}
	return resolved
}

// Name and home pairs this process reserved, the launch home first then served secondaries
func (r *GatewayRunner) reservedProfileHomes(ctx context.Context) []profiles.ServedProfile {
	launch := resolveHome(home.Dir(ctx))
	name := r.primaryProfileName
	if name == "" {
		name = "default"
	}
	homes := []profiles.ServedProfile{{Name: name, Home: launch}}
	if !r.multiplexing() {
		return homes
	}
	reserved, _ := r.config.RuntimeProfileHomes()
	seen := map[string]bool{launch: true}
	for _, p := range reserved {
		canonical := resolveHome(p.Home)
		if !seen[canonical] {
			seen[canonical] = true
			homes = append(homes, profiles.ServedProfile{Name: p.Name, Home: canonical})
		}
	}
	return homes
}

// State database of profileHome through the runner's scoped handle cache, opened under that profile's scope
func (r *GatewayRunner) openProfileStore(ctx context.Context, profileHome string) *state.SessionDB {
	return r.SessionDB(gateway.ProfileRuntimeScope(ctx, profileHome, false))
}

// Opens every secondary's store concurrently before the authorities are built in order
func (r *GatewayRunner) prewarmProfileStores(ctx context.Context, homes []profiles.ServedProfile) {
	var wg sync.WaitGroup
	for _, p := range homes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			r.openProfileStore(ctx, p.Home)
		}()
	}
	wg.Wait()
}

// One registry reference to the launch state.db so the store open in the constructor is a hit, nil on failure
func pinLaunchStore(ctx context.Context) *state.DB {
	db, err := state.Acquire(ctx, "")
	if err != nil {
		runtimeLog().Debug("launch state.db prewarm failed; the store open reports it", "error", err)
		return nil
	}
	return db
}

// Drops the launch store reference pinLaunchStore took
func unpinLaunchStore(db *state.DB) {
	if db != nil {
		state.Release(db)
	}
}

// Why profileHome's state.db has no handle, the error its failed open recorded
func (r *GatewayRunner) storeOpenFailure(profileHome string) string {
	path := filepath.Join(profileHome, "state.db")
	caches := []*gateway.RecoverableHandleCache{}
	if r.sessionStore != nil {
		if c := r.sessionStore.DBHandleCache(); c != nil {
			caches = append(caches, c)
		}
	}
	if r.sessionDBHandleCache != nil {
		caches = append(caches, r.sessionDBHandleCache)
	}
	for _, c := range caches {
		if e := c.LastError(path); e != "" {
			return fmt.Sprintf("%s did not open: %s", path, e)
		}
	}
	return fmt.Sprintf("%s is unavailable (its last open failed; retry pending)", path)
}

// Builds one profile's authority against its own state.db, an error when that store is unusable
func (r *GatewayRunner) buildProfileAuthority(ctx context.Context, name, profileHome string, register bool) (*gateway.SessionAuthority, error) {
	registry := r.sessionAuthorities
	instanceID := pyrepr.Str(r.sessionRuntimeDescriptor["instance_id"])
	db := r.openProfileStore(ctx, profileHome)
	scoped := gateway.ProfileRuntimeScope(ctx, profileHome, false)
	if db == nil {
		return nil, &pyRuntimeError{r.storeOpenFailure(profileHome)}
	}
	if filepath.Dir(resolveHome(db.Path())) != profileHome {
		return nil, &pyRuntimeError{fmt.Sprintf("session authority database does not belong to the reserved profile %s", profileHome)}
	}
	if err := registry.Add(profileHome, nil, &name); err != nil {
		return nil, err
	}
	authority, err := gateway.InitializeSessionAuthority(scoped, r, profileHome, instanceID, db, register)
	if err != nil {
		_, _ = registry.Remove(profileHome)
		return nil, err
	}
	if err := registry.Replace(profileHome, authority); err != nil {
		return nil, err
	}
	return authority, nil
}

// Parks a secondary whose store is unusable, releasing its reservation so boot serves every other profile
func (r *GatewayRunner) parkReservedProfile(ctx context.Context, name, profileHome string, cause error) error {
	runtimeLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s' not served: its session store is unusable (%s): %s",
		name, profileHome, pyrepr.ExceptionMessage(cause)))
	if err := r.releaseProfileHome(ctx, profileHome); err != nil {
		return err
	}
	r.parkProfile(ctx, name, "session store unusable: "+pyrepr.ExceptionMessage(cause))
	return nil
}

// Publishes name under parked_profiles in the descriptor and the runtime status
func (r *GatewayRunner) parkProfile(ctx context.Context, name, reason string) {
	parked := r.parkedProfileMap()
	parked[name] = firstRunesText(reason, 500)
	recordParkedProfiles(ctx, parked)
}

// Leading n code points of s
func firstRunesText(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// Drops name from parked_profiles when it was parked
func (r *GatewayRunner) unparkProfile(ctx context.Context, name string) {
	parked := r.parkedProfileMap()
	if _, ok := parked[name]; ok {
		delete(parked, name)
		recordParkedProfiles(ctx, parked)
	}
}

// Whether name is parked, unserved now but served again on a rescan
func (r *GatewayRunner) profileParked(name string) bool {
	if name == "" || r.sessionRuntimeDescriptor == nil {
		return false
	}
	parked, _ := r.sessionRuntimeDescriptor["parked_profiles"].(map[string]any)
	_, ok := parked[name]
	return ok
}

// Retryable refusal of a request under a parked profile's prefix
func (r *GatewayRunner) refuseParkedScope(ctx context.Context) error {
	name := gateway.ServedProfileName(home.Dir(ctx))
	if r.profileParked(name) {
		return gateway.NewParkedProfileRefusal(name)
	}
	return nil
}

// Live parked_profiles map of the descriptor, created on first use
func (r *GatewayRunner) parkedProfileMap() map[string]any {
	if r.sessionRuntimeDescriptor == nil {
		r.sessionRuntimeDescriptor = map[string]any{}
	}
	parked, ok := r.sessionRuntimeDescriptor["parked_profiles"].(map[string]any)
	if !ok {
		parked = map[string]any{}
		r.sessionRuntimeDescriptor["parked_profiles"] = parked
	}
	return parked
}

// Persists the parked set, swallowing a failed write
func recordParkedProfiles(ctx context.Context, parked map[string]any) {
	snapshot := make(map[string]any, len(parked))
	for k, v := range parked {
		snapshot[k] = v
	}
	if _, err := gateway.WriteRuntimeStatus(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{"parked_profiles": snapshot}}, false, nil); err != nil {
		runtimeLog().Debug("could not record parked_profiles", "error", err)
	}
}

// Builds the descriptor, the authority registry and one authority per reserved home, parking unusable secondaries
func (r *GatewayRunner) initializeGatewayRuntime(ctx context.Context) error {
	homes := r.reservedProfileHomes(ctx)
	for _, p := range homes {
		if !gateway.ProcessOwnership.Owns(p.Home) {
			return &pyRuntimeError{"session authority requires reserved profile ownership: " + p.Home}
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	instanceID := hex.EncodeToString(raw)
	descriptor := map[string]any{
		"instance_id": instanceID, "runtime_protocol": 1, "state": "starting",
		"capabilities": []any{}, "served_profiles": []any{},
	}
	r.sessionRuntimeDescriptor = descriptor
	registry := gateway.NewSessionAuthorities(homes[0].Home, r.multiplexing())
	r.sessionAuthorities = registry
	r.prewarmProfileStores(ctx, homes[1:])
	for index, p := range homes {
		if _, err := r.buildProfileAuthority(ctx, p.Name, p.Home, index == 0); err != nil {
			if index == 0 {
				return err
			}
			if err := r.parkReservedProfile(ctx, p.Name, p.Home, err); err != nil {
				return err
			}
		}
	}
	launch, err := registry.Launch()
	if err != nil {
		return err
	}
	descriptor["authority_epoch"] = launch.Epoch
	descriptor["served_profiles"] = registry.ServedProfiles()
	recordParkedProfiles(ctx, r.parkedProfileMap())
	store, err := gateway.NewTicketStore(instanceID, registry.ProfileIDs())
	if err != nil {
		return err
	}
	r.sessionTicketStore = store
	return gateway.ReleaseUnknownTurnMarkers(ctx, r, r.sessionStore)
}

// Republishes the served set to the descriptor and the ticket store
func (r *GatewayRunner) publishServedSet() {
	r.sessionRuntimeDescriptor["served_profiles"] = r.sessionAuthorities.ServedProfiles()
	r.sessionTicketStore.SetProfileIDs(r.sessionAuthorities.ProfileIDs())
}

// Grows the reservation by one profile, waiting out a transient holder, nothing reserved once cancel closes
func (r *GatewayRunner) reserveProfileHome(ctx context.Context, name, profileHome string, cancel <-chan struct{}) error {
	profileHome = resolveHome(profileHome)
	if err := gateway.ReserveWaitingOutTransientHolders(ctx, gateway.ProcessOwnership, []string{profileHome}, browseReconcileReserveWait(), cancel); err != nil {
		return err
	}
	reserved, ok := r.config.RuntimeProfileHomes()
	if !ok {
		return nil
	}
	for _, p := range reserved {
		if resolveHome(p.Home) == profileHome {
			return nil
		}
	}
	r.config.SetRuntimeProfileHomes(append(reserved, profiles.ServedProfile{Name: name, Home: profileHome}))
	return nil
}

// Reserves off the caller's goroutine, and on cancellation stops the worker and undoes what it completed
func (r *GatewayRunner) reserveProfileHomeInWorker(ctx context.Context, name, profileHome string) error {
	profileHome = resolveHome(profileHome)
	ownedBefore := gateway.ProcessOwnership.Owns(profileHome)
	cancel := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- r.reserveProfileHome(context.WithoutCancel(ctx), name, profileHome, cancel) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		close(cancel)
		err := <-done
		if err == nil && !ownedBefore {
			if releaseErr := r.releaseProfileHome(context.WithoutCancel(ctx), profileHome); releaseErr != nil {
				return releaseErr
			}
		}
		return ctx.Err()
	}
}

// Shrinks the reservation by one profile, refused while its authority has not retired
func (r *GatewayRunner) releaseProfileHome(ctx context.Context, profileHome string) error {
	profileHome = resolveHome(profileHome)
	if r.sessionAuthorities != nil && r.sessionAuthorities.ForHome(profileHome) != nil {
		return &pyRuntimeError{"Profile authority must retire before ownership is released"}
	}
	r.withdrawProfileHome(profileHome)
	gateway.ProcessOwnership.Release(profileHome)
	return nil
}

// Drops profileHome from the served set every startup consumer reads, keeping its gateway.lock
func (r *GatewayRunner) withdrawProfileHome(profileHome string) {
	reserved, ok := r.config.RuntimeProfileHomes()
	if !ok {
		return
	}
	kept := []profiles.ServedProfile{}
	for _, p := range reserved {
		if resolveHome(p.Home) != profileHome {
			kept = append(kept, p)
		}
	}
	r.config.SetRuntimeProfileHomes(kept)
}

// A retired attempt's writer is still live, so the profile keeps its reservation and authority
type ProfileOwnershipRetained struct{ Msg string }

func (e *ProfileOwnershipRetained) Error() string { return e.Msg }

// Python exception class of the retained ownership refusal
func (e *ProfileOwnershipRetained) PyTypeName() string { return "ProfileOwnershipRetained" }

// Recovers every serving ledger's native rows once hot served adapters connected, logging a failed store
func (r *GatewayRunner) recoverServedNativeRows(ctx context.Context) {
	if r.sessionAuthorities == nil {
		return
	}
	for _, authority := range gateway.ServingAuthorities(r) {
		if _, err := gateway.RecoverProfileNativeSessions(ctx, r, authority); err != nil {
			runtimeLog().Error(fmt.Sprintf("Native session recovery failed for %s", authority.ProfileID), "error", err)
		}
	}
}
