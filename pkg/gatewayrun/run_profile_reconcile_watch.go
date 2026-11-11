package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func reconcileLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.run_profile_reconcile")
}

// Why a single profile boot must stay standalone now that a second profile exists, empty when nothing blocks it
func addedProfileBlocker(ctx context.Context, launch string) (string, error) {
	optedOut, err := AutoMigrationOptedOut(ctx, launch)
	if err != nil {
		return "", err
	}
	if optedOut {
		return gateway.AutoMigrationOptedOutReason, nil
	}
	return ImplicitMultiplexBlocker(ctx)
}

// Error of a panic the Python broad except would have caught
func recoveredError(rec any) error {
	if err, ok := rec.(error); ok {
		return err
	}
	return fmt.Errorf("%v", rec)
}

// Every interval seconds re-syncs consent changed shell hooks, then rescans profiles when multiplexing
func (r *GatewayRunner) profileReconcileWatcher(ctx context.Context, interval float64) error {
	for r.running.Load() {
		select {
		case <-time.After(time.Duration(interval * float64(time.Second))):
		case <-ctx.Done():
			return ctx.Err()
		}
		if !r.running.Load() {
			return nil
		}
		err := func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = recoveredError(rec)
				}
			}()
			if err := r.syncHooksOnConsentChange(ctx); err != nil {
				return err
			}
			on := r.multiplexOn()
			if !on {
				if on, err = r.multiplexForAddedProfile(ctx); err != nil {
					return err
				}
			}
			if on {
				_, err = r.reconcileServedProfiles(ctx, "watcher")
			}
			return err
		}()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			reconcileLog().Warn("Served-profile reconcile failed; retrying next cycle", "exc_info", err)
		}
	}
	return nil
}

// Re-syncs the shell hooks of the launch and served homes whose consent or config changed since the last tick
func (r *GatewayRunner) syncHooksOnConsentChange(ctx context.Context) error {
	homes := []string{home.Process()}
	if r.servedProfileHomes != nil {
		homes = append(homes, r.servedProfileHomes.Values()...)
	}
	seen := r.hookConsentSignatures
	if seen == nil {
		seen = map[string][]any{}
	}
	r.hookConsentSignatures = r.resyncChangedHookConsent(ctx, homes, seen)
	return nil
}

// Blocking half of the consent sync, returning the signatures it applied and leaving failed homes unrecorded
func (r *GatewayRunner) resyncChangedHookConsent(ctx context.Context, homes []string, seen map[string][]any) map[string][]any {
	current := map[string][]any{}
	for _, h := range homes {
		resolved, err := pypath.Resolve(h)
		if err != nil {
			resolved = h
		}
		sig := []any{statSignature(filepath.Join(resolved, plugins.ShellHookAllowlistFilename)),
			statSignature(filepath.Join(resolved, "config.yaml"))}
		if prev, ok := seen[resolved]; ok && reflect.DeepEqual(prev, sig) {
			current[resolved] = sig
			continue
		}
		cfgPath := filepath.Join(resolved, "config.yaml")
		var scoped context.Context
		if r.multiplexOn() {
			scoped = gateway.ProfileRuntimeScope(ctx, resolved, true)
		} else {
			scoped = standaloneLaunchScope(ctx)
		}
		failed := func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = recoveredError(rec)
				}
			}()
			plugins.SyncShellHooksFromConfig(scoped, func() any { return gateway.LoadGatewayConfigMap(scoped, cfgPath) })
			return nil
		}()
		if failed != nil {
			reconcileLog().Warn(fmt.Sprintf("shell-hook re-sync failed for %s; retrying next cycle", resolved), "exc_info", failed)
			continue
		}
		current[resolved] = sig
	}
	return current
}

// Name lists of a reconcile result in their Python key order
func newReconcileResult(reason string) *pyjson.Object {
	return pyjson.ObjectOf("added", []any{}, "removed", []any{}, "rescanned", []any{}, "parked", []any{}, "reason", reason)
}

// Appends name to the list result holds under key
func resultAppend(result *pyjson.Object, key, name string) {
	list, _ := result.Get(key)
	items, _ := list.([]any)
	result.Set(key, append(items, name))
}

// Copy of result with the given keys set after its own, the {**result, ...} of the Python
func resultWith(result *pyjson.Object, kv ...any) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range result.Keys {
		v, _ := result.Get(k)
		out.Set(k, v)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out.Set(kv[i].(string), kv[i+1])
	}
	return out
}

// Served profile names as a JSON list
func namesList(names []string) []any {
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// Launch profile name of this runner, default when none was recorded
func (r *GatewayRunner) primaryProfileOrDefault() string {
	if r.primaryProfileName != "" {
		return r.primaryProfileName
	}
	return "default"
}

// Ordered copy of a name to home map
func copyHomes(d *pydict.Dict[string, string]) *pydict.Dict[string, string] {
	out := pydict.New[string, string]()
	if d != nil {
		for k, v := range d.All() {
			out.Set(k, v)
		}
	}
	return out
}

// Ordered name to home map of the profiles a multiplexer serves now
func liveServedProfiles(ctx context.Context) (*pydict.Dict[string, string], error) {
	served, err := profiles.ProfilesToServe(ctx, true, false, false)
	if err != nil {
		return nil, err
	}
	live := pydict.New[string, string]()
	for _, p := range served {
		live.Set(p.Name, p.Home)
	}
	return live, nil
}

// Diffs profiles against the served set, serving new ones, unserving deleted ones and rebuilding changed ones
func (r *GatewayRunner) reconcileServedProfiles(ctx context.Context, reason string) (*pyjson.Object, error) {
	result := newReconcileResult(reason)
	if !r.multiplexOn() {
		armed, err := r.multiplexForAddedProfile(ctx)
		if err != nil {
			return nil, err
		}
		if !armed {
			return resultWith(result, "multiplex", false, "served_profiles", namesList(r.servedProfileNames())), nil
		}
	}
	if !r.running.Load() || r.servedProfileHomes == nil {
		return resultWith(result, "pending", true, "served_profiles", namesList(r.servedProfileNames())), nil
	}
	lock := r.reconcileLock()
	lock.Lock()
	defer lock.Unlock()
	active := r.primaryProfileOrDefault()
	live, err := liveServedProfiles(ctx)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, name := range live.Keys() {
		listed[name] = true
	}
	known := copyHomes(r.servedProfileHomes)
	blocked, timedOut := map[string]bool{}, map[string]bool{}
	warned, timeoutWarned := r.profileOwnGatewayWarned, r.profileProbeTimeoutWarned
	for _, name := range live.Keys() {
		if name == active || known.Has(name) {
			continue
		}
		profileHome, _ := live.Get(name)
		probeCtx, cancel := context.WithTimeout(ctx, ownGatewayProbeTimeout)
		res, err := r.runHousekeepingInExecutor(probeCtx, func(context.Context) (any, error) {
			pid, found, err := gateway.LiveGatewayPidForHome(profileHome)
			if err != nil || !found {
				return nil, err
			}
			return pid, nil
		})
		probeTimedOut := errors.Is(err, context.DeadlineExceeded) && probeCtx.Err() != nil && ctx.Err() == nil
		cancel()
		if probeTimedOut {
			timedOut[name] = true
			msg := fmt.Sprintf("[MULTIPLEX] Own-gateway probe for profile '%s' timed out; not serving it this cycle", name)
			if timeoutWarned[name] {
				reconcileLog().Debug(msg)
			} else {
				reconcileLog().Warn(msg)
			}
			live.Delete(name)
			continue
		}
		if err != nil {
			return nil, err
		}
		if res != nil {
			blocked[name] = true
			if !warned[name] {
				reconcileLog().Warn(fmt.Sprintf("[MULTIPLEX] Profile '%s' still runs its own gateway; "+
					"stop it before the host can serve this profile", name))
			}
			live.Delete(name)
		}
	}
	r.profileOwnGatewayWarned = blocked
	r.profileProbeTimeoutWarned = timedOut
	sigs := r.servedProfileSignatures
	if sigs == nil {
		sigs = map[string][]any{}
	}
	for _, name := range r.parkedProfileNames() {
		if !listed[name] {
			r.unparkProfile(ctx, name)
		}
	}
	parked := map[string]bool{}
	for _, name := range r.parkedProfileNames() {
		parked[name] = true
	}
	retryParked := reason == "control-socket"
	var added, removed, changed []string
	for _, name := range live.Keys() {
		if !known.Has(name) && name != active && (retryParked || !parked[name]) {
			added = append(added, name)
		}
	}
	for _, name := range known.Keys() {
		if !live.Has(name) && name != active {
			removed = append(removed, name)
		}
	}
	for _, name := range live.Keys() {
		if !known.Has(name) || name == active || slices.Contains(added, name) {
			continue
		}
		profileHome, _ := live.Get(name)
		sig, err := profileServeSignature(ctx, profileHome)
		if err != nil {
			return nil, err
		}
		if !signaturesEqual(sig, sigs[name]) {
			changed = append(changed, name)
		}
	}
	current := pydict.New[string, string]()
	for name, profileHome := range live.All() {
		if known.Has(name) || name == active {
			current.Set(name, profileHome)
		}
	}
	return r.applyProfileChanges(ctx, current, added, removed, changed, reason, live, result)
}

// Applies a selected diff under the reconcile lock, parking new profiles that cannot be served
func (r *GatewayRunner) applyProfileChanges(ctx context.Context, current *pydict.Dict[string, string], added, removed, changed []string,
	reason string, live *pydict.Dict[string, string], result *pyjson.Object) (*pyjson.Object, error) {
	active := r.primaryProfileOrDefault()
	if result == nil {
		result = newReconcileResult(reason)
	} else {
		result = resultWith(result)
	}
	if _, ok := result.Get("parked"); !ok {
		result.Set("parked", []any{})
	}
	if live == nil {
		live = copyHomes(current)
	}
	known := copyHomes(r.servedProfileHomes)
	sigs := r.servedProfileSignatures
	if len(sigs) == 0 {
		sigs = map[string][]any{}
	}
	if len(added) == 0 && len(removed) == 0 && len(changed) == 0 {
		return resultWith(result, "served_profiles", namesList(r.servedProfileNames())), nil
	}
	for _, name := range removed {
		profileHome, _ := known.Get(name)
		if err := r.unserveProfile(ctx, name, profileHome); err != nil {
			return nil, err
		}
		resultAppend(result, "removed", name)
	}
	for _, name := range slices.Clone(added) {
		profileHome, _ := live.Get(name)
		parkReason, err := r.serveReservedProfile(ctx, name, profileHome)
		if err != nil {
			return nil, err
		}
		if parkReason != "" {
			added = slices.DeleteFunc(added, func(n string) bool { return n == name })
			r.parkProfile(ctx, name, parkReason)
			resultAppend(result, "parked", name)
			continue
		}
		r.unparkProfile(ctx, name)
		current.Set(name, profileHome)
	}
	claimed := r.liveResourceClaims(active)
	transientFailed := map[string]bool{}
	configParked := map[string]bool{}
	for _, name := range append(slices.Clone(added), changed...) {
		profileHome, _ := current.Get(name)
		scanSignature, err := profileServeSignature(ctx, profileHome)
		if err != nil {
			return nil, err
		}
		if slices.Contains(changed, name) {
			r.dropStalePlatformVerdicts(ctx, name)
		}
		connected, err := r.startOneProfileAdapters(ctx, name, profileHome, claimed)
		var configErr *MultiplexConfigError
		switch {
		case errors.As(err, &configErr):
			reconcileLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s' not served: %s", name, configErr))
			current.Delete(name)
			if err := r.unserveProfile(ctx, name, profileHome); err != nil {
				return nil, err
			}
			r.parkProfile(ctx, name, configErr.Error())
			resultAppend(result, "parked", name)
			configParked[name] = true
			continue
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			reconcileLog().Error(fmt.Sprintf("[MULTIPLEX] Failed to start adapters for profile '%s'", name), "exc_info", err)
			connected = 0
			transientFailed[name] = true
		default:
			sigs[name] = scanSignature
		}
		if slices.Contains(added, name) {
			reconcileLog().Info(fmt.Sprintf("[MULTIPLEX] Now serving profile '%s' (%d adapter(s) connected; %s)", name, connected, reason))
			resultAppend(result, "added", name)
		} else {
			reconcileLog().Info(fmt.Sprintf("[MULTIPLEX] Re-scanned profile '%s' after config/.env change (%d adapter(s) connected)", name, connected))
			resultAppend(result, "rescanned", name)
		}
	}
	if len(configParked) > 0 {
		added = slices.DeleteFunc(added, func(n string) bool { return configParked[n] })
	}
	r.servedProfileSignatures = sigs
	liveNow, err := liveServedProfiles(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range current.Keys() {
		if liveNow.Has(name) || name == active {
			continue
		}
		profileHome, _ := current.Get(name)
		current.Delete(name)
		if err := r.unserveProfile(ctx, name, profileHome); err != nil {
			return nil, err
		}
		resultAppend(result, "removed", name)
		added = slices.DeleteFunc(added, func(n string) bool { return n == name })
	}
	homes := make([]profiles.ServedProfile, 0, current.Len())
	for name, profileHome := range current.All() {
		homes = append(homes, profiles.ServedProfile{Name: name, Home: profileHome})
	}
	r.recordServedProfiles(ctx, active, homes)
	for name := range transientFailed {
		delete(r.servedProfileSignatures, name)
		r.profileConfigs.Delete(name)
	}
	if len(added) > 0 {
		r.recoverServedNativeRows(ctx)
		addedHomes := make([]profiles.ServedProfile, 0, len(added))
		for _, name := range added {
			profileHome, _ := current.Get(name)
			addedHomes = append(addedHomes, profiles.ServedProfile{Name: name, Home: profileHome})
		}
		r.afterProfilesAdded(ctx, addedHomes)
	}
	result.Set("served_profiles", namesList(r.servedProfileNames()))
	return result, nil
}

// Drops a changed profile's runtime status entries for platforms with no live or queued adapter
func (r *GatewayRunner) dropStalePlatformVerdicts(ctx context.Context, name string) {
	live := map[gateway.Platform]bool{}
	if r.profileAdapters != nil {
		if adapters, ok := r.profileAdapters.Get(name); ok && adapters != nil {
			for _, p := range adapters.Keys() {
				live[p] = true
			}
		}
	}
	for p := range r.profileFailedPlatforms[name] {
		live[p] = true
	}
	if r.unservedSecondaryPlatforms != nil {
		for _, key := range r.unservedSecondaryPlatforms.Keys() {
			if key.profile == name {
				r.unservedSecondaryPlatforms.Delete(key)
			}
		}
	}
	keep := make([]string, 0, len(live))
	for p := range live {
		keep = append(keep, fmt.Sprintf("%s:%s", name, p))
	}
	slices.Sort(keep)
	writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{DropProfilePlatforms: name, KeepPlatforms: keep})
}

// Profiles that exist but could not be served, named in the published parked map
func (r *GatewayRunner) parkedProfileNames() []string {
	parked := r.parkedProfiles()
	names := make([]string, 0, len(parked))
	for name := range parked {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Starts multiplexing in place when a single profile boot now has a second profile, true when armed
func (r *GatewayRunner) multiplexForAddedProfile(ctx context.Context) (bool, error) {
	lock := r.reconcileLock()
	lock.Lock()
	defer lock.Unlock()
	if r.multiplexOn() {
		return true, nil
	}
	if !r.running.Load() || r.draining.Load() {
		return false, nil
	}
	recorded := gateway.RecordedStandaloneReason("")
	if recorded != gateway.SingleProfileReason && recorded != gateway.AutoMigrationOptedOutReason {
		return false, nil
	}
	launch, err := pypath.Resolve(home.Process())
	if err != nil {
		return false, err
	}
	root, err := pypath.Resolve(home.Root())
	if err != nil {
		return false, err
	}
	if launch != root {
		return false, nil
	}
	all, err := profiles.ProfilesToServe(ctx, true, false, true)
	if err != nil {
		return false, err
	}
	if len(all) < 2 {
		return false, nil
	}
	found, err := r.runHousekeepingInExecutor(ctx, func(c context.Context) (any, error) { return addedProfileBlocker(c, launch) })
	if err != nil {
		return false, err
	}
	if blocker, _ := found.(string); blocker != "" {
		if r.singleProfileBlockerLogged == nil || blocker != *r.singleProfileBlockerLogged {
			reconcileLog().Warn(fmt.Sprintf("[MULTIPLEX] A new profile exists but this gateway stays standalone: %s", blocker))
			r.singleProfileBlockerLogged = &blocker
		}
		if blocker == gateway.AutoMigrationOptedOutReason {
			gateway.RecordMultiplexDecision(ctx, gateway.MultiplexDecision{Enabled: false, Source: "guard", Reason: blocker})
		}
		return false, nil
	}
	if !r.running.Load() || r.draining.Load() {
		return false, nil
	}
	provider.ActivateMultiProfileHosting()
	mcp.RekeyUnscopedConnections(home.Key(launch))
	multiplex := true
	r.config.MultiplexProfiles = &multiplex
	launchHomes := []profiles.ServedProfile{{Name: "default", Home: launch}}
	r.config.SetRuntimeProfileHomes(launchHomes)
	if r.sessionAuthorities != nil {
		r.sessionAuthorities.Multiplexed = true
	}
	if r.adapters != nil {
		for _, adapter := range r.adapters.Values() {
			base := adapter.Base()
			handler := r.primaryMessageHandler(ctx)
			base.SetBoundMessageHandler(handler.owner, handler.fn)
			base.SetBusySessionHandler(r.primaryBusySessionHandler(ctx))
			base.SetPlatformEventHandler(r.primaryPlatformEventHandler(ctx))
		}
	}
	r.recordServedProfiles(ctx, "default", launchHomes)
	gateway.LogMultiplexDecision(ctx, gateway.MultiplexDecision{Enabled: true, Source: "default", Reason: "a second profile was created"})
	reconcileLog().Info("[MULTIPLEX] A second profile exists; serving every profile from this process (no restart)")
	return true, nil
}

// Grows the reservation by the home and builds its authority, the park reason when it cannot be served
func (r *GatewayRunner) serveReservedProfile(ctx context.Context, name, profileHome string) (string, error) {
	if err := r.reserveProfileHomeInWorker(ctx, name, profileHome); err != nil {
		var conflict *gateway.OwnershipConflict
		if !errors.As(err, &conflict) && !pypath.IsOSError(err) {
			return "", err
		}
		reconcileLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s' not served: %s", name, err))
		return fmt.Sprintf("home owned by another gateway: %s", err), nil
	}
	if r.sessionAuthorities == nil {
		return "", nil
	}
	if _, err := serveProfileRuntime(ctx, r, name, profileHome); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if r.sessionAuthorities.ForHome(profileHome) != nil {
			reconcileLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s' not served (%s); a turn outlived its Stop, "+
				"ownership retained", name, err), "exc_info", err)
			return fmt.Sprintf("stopping after a failed serve; ownership retained: %s", err), nil
		}
		reconcileLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s' not served: its session store is unusable (%s): %s",
			name, profileHome, err), "exc_info", err)
		if releaseErr := r.releaseProfileHome(ctx, profileHome); releaseErr != nil {
			return "", releaseErr
		}
		return fmt.Sprintf("session store unusable: %s", err), nil
	}
	return "", nil
}

// Log routing refresh and scoped MCP discovery for hot added profiles
func (r *GatewayRunner) afterProfilesAdded(ctx context.Context, profileHomes []profiles.ServedProfile) {
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				reconcileLog().Debug("log routing refresh failed", "exc_info", recoveredError(rec))
			}
		}()
		enableMultiplexLogRouting(ctx, r.config)
	}()
	for _, p := range profileHomes {
		scoped := mcp.SuppressInteractiveOAuth(gateway.ProfileRuntimeScope(ctx, p.Home, true))
		env := &tools.Env{Home: home.Dir(scoped), TerminalScope: tools.GetTerminalScope(scoped)}
		if _, err := mcp.ConnectAll(scoped, env, nil); err != nil {
			reconcileLog().Warn(fmt.Sprintf("MCP tool discovery failed for profile '%s'", p.Name), "exc_info", err)
		}
	}
}

// Stops and unroutes one profile, releasing this process's handles into its home before its reservation
func (r *GatewayRunner) unserveProfile(ctx context.Context, name, profileHome string) error {
	retired := true
	if r.sessionAuthorities != nil {
		ok, err := unserveProfileRuntime(ctx, r, profileHome)
		if err != nil {
			return err
		}
		retired = ok
	}
	pending := r.profileFailedPlatforms[name]
	delete(r.profileFailedPlatforms, name)
	var tasks []*gatewayTask
	for _, task := range pending {
		if task != nil && !task.isDone() {
			tasks = append(tasks, task)
		}
	}
	if r.retiringProfiles == nil {
		r.retiringProfiles = map[string]struct{}{}
	}
	r.retiringProfiles[name] = struct{}{}
	func() {
		defer delete(r.retiringProfiles, name)
		for _, task := range tasks {
			task.Cancel()
		}
		if len(tasks) > 0 {
			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(r.adapterDisconnectTimeoutSecs()*float64(time.Second)))
			defer cancel()
			for _, task := range tasks {
				task.wait(waitCtx)
			}
		}
	}()
	scoped := gateway.ProfileRuntimeScope(ctx, profileHome, false)
	var adapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
	if r.profileAdapters != nil {
		adapters, _ = r.profileAdapters.Get(name)
		r.profileAdapters.Delete(name)
	}
	if adapters == nil {
		adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	}
	for platform, adapter := range adapters.All() {
		r.holdSecondaryClaims(name, platform, adapter, r.adapterClaims(platform, adapter))
	}
	func() {
		defer func() {
			if r.secondaryHeldClaims == nil {
				return
			}
			for _, key := range r.secondaryHeldClaims.Keys() {
				if key.profile == name {
					r.secondaryHeldClaims.Delete(key)
				}
			}
		}()
		for platform, adapter := range adapters.All() {
			r.boundedAdapterTeardown(scoped, adapter, platform, name)
			r.releaseSecondaryClaims(name, platform, adapter)
		}
	}()
	writeRuntimeStatusQuiet(scoped, gateway.RuntimeStatusUpdate{DropProfilePlatforms: name})
	delete(r.pairingStores, name)
	delete(r.busyTextModesByProfile, name)
	delete(r.busyInputModesByProfile, name)
	delete(r.busyTextTimingByProfile, name)
	delete(r.humanDelayByProfile, name)
	r.profileConfigs.Delete(name)
	delete(r.profileBotPlatforms, name)
	if r.servedProfileHomes != nil {
		r.servedProfileHomes.Delete(name)
	}
	delete(r.servedProfileSignatures, name)
	prefix := gateway.SessionKeyNamespace(name) + ":"
	var keys []string
	if r.agentCache != nil {
		if r.agentCacheLock != nil {
			r.agentCacheLock.Lock()
		}
		keys = r.agentCache.Keys()
		if r.agentCacheLock != nil {
			r.agentCacheLock.Unlock()
		}
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					reconcileLog().Debug(fmt.Sprintf("agent eviction failed for %s", key), "exc_info", recoveredError(rec))
				}
			}()
			r.evictCachedAgent(scoped, key, false, false)
		}()
	}
	if !retired {
		reconcileLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s' unrouted, but a turn outlived its Stop; ownership retained", name))
		return nil
	}
	state.CloseAllUnder(profileHome)
	if _, err := manage.MemoryStoreReleaseAllUnder(profileHome); err != nil {
		reconcileLog().Debug("memory-store release failed", "exc_info", err)
	}
	scope := home.Key(profileHome)
	mcp.ShutdownMCPServers(scoped, &scope, nil, 15*time.Second)
	if _, err := logging.ReleaseProfileLogHandlers(profileHome); err != nil {
		reconcileLog().Debug("profile log handler release failed", "exc_info", err)
	}
	if err := r.releaseProfileHome(scoped, profileHome); err != nil {
		return err
	}
	reconcileLog().Info(fmt.Sprintf("[MULTIPLEX] Profile '%s' unserved — %d adapter(s) stopped and unrouted", name, adapters.Len()))
	return nil
}
