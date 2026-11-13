package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Brings up adapters for every non launch profile under multiplexing and returns the connected count
func (r *GatewayRunner) startSecondaryProfileAdapters(ctx context.Context) (int, error) {
	if !r.multiplexOn() {
		if _, err := gateway.PublishRuntimeStatus(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{"served_profiles": []any{}}}); err != nil {
			runLog().Debug("could not clear served_profiles", "exc_info", err)
		}
		return 0, nil
	}
	active := r.primaryProfileName
	if active == "" {
		active = "default"
	}
	served, err := profiles.ProfilesToServe(ctx, true, false, true)
	if err != nil {
		return 0, err
	}
	for _, p := range served {
		if p.Name != "default" && profiles.ProfileIsParked(p.Home) {
			runLog().Info(fmt.Sprintf("profile '%s' is parked (gateway.parked); not served by this gateway", p.Name))
		}
	}
	connected := 0
	claimed := r.primaryResourceClaims(active)
	profileHomes, err := gateway.MultiplexProfileHomes(ctx, r.config)
	if err != nil {
		return 0, err
	}
	r.servedProfileSignatures = map[string][]any{}
	transientFailed := map[string]bool{}
	for _, p := range profileHomes {
		if p.Name == active {
			continue
		}
		scanSignature, err := profileServeSignature(ctx, p.Home)
		if err != nil {
			return 0, err
		}
		count, err := r.startOneProfileAdapters(ctx, p.Name, p.Home, claimed)
		var configErr *MultiplexConfigError
		if errors.As(err, &configErr) {
			return 0, err
		}
		if err != nil {
			if isCancellation(err) || ctx.Err() != nil {
				return 0, err
			}
			runLog().Error(fmt.Sprintf("Failed to start adapters for profile '%s': %s", p.Name, err.Error()), "exc_info", err)
			transientFailed[p.Name] = true
			continue
		}
		connected += count
		r.servedProfileSignatures[p.Name] = scanSignature
	}
	r.recordServedProfiles(ctx, active, profileHomes)
	for name := range transientFailed {
		delete(r.servedProfileSignatures, name)
	}
	for _, name := range r.profileConfigs.Keys() {
		if _, served := r.servedProfileSignatures[name]; !served {
			r.profileConfigs.Delete(name)
		}
	}
	r.restoreSecondaryCompletionLedgers(ctx, profileHomes)
	return connected, nil
}

// Records the served profile set for status and the host record and seeds each profile's pairing store
func (r *GatewayRunner) recordServedProfiles(ctx context.Context, active string, profileHomes []profiles.ServedProfile) {
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		var others []string
		for _, p := range profileHomes {
			if p.Name != active {
				others = append(others, p.Name)
			}
		}
		slices.Sort(others)
		served := append([]string{active}, others...)
		if err := r.noteServedProfiles(ctx, profileHomes); err != nil {
			return err
		}
		if r.pairingStores == nil {
			r.pairingStores = map[string]*gateway.PairingStore{}
		}
		for _, name := range served {
			if _, ok := r.pairingStores[name]; name == "" || ok {
				continue
			}
			if name == active {
				r.pairingStores[name] = r.pairingStore
				continue
			}
			store, err := gateway.NewPairingStore(ctx, name)
			if err != nil {
				return err
			}
			r.pairingStores[name] = store
		}
		names := make([]any, len(served))
		for i, name := range served {
			names[i] = name
		}
		if _, err := gateway.PublishRuntimeStatus(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{"served_profiles": names}}); err != nil {
			return err
		}
		if gateway.OwnsHostLock(gateway.RoleGateway) {
			gateway.PublishHostRecord(ctx, gateway.RoleGateway, gateway.HostPublishOptions{Profiles: served, Home: home.Dir(ctx)})
		}
		return nil
	}()
	if err != nil {
		runLog().Debug("could not record served_profiles", "exc_info", err)
	}
}

// Installs the catalog plugin of every feature that left core and that the home uses, in the home's runtime scope
func recoverLeftCoreIn(ctx context.Context, profileHome string, hydrateSecrets bool) {
	provider.RecoverLeftCoreAtStartup(gateway.ProfileRuntimeScope(ctx, profileHome, hydrateSecrets), nil, true)
}

// Left core migration of the launch home, then of the default root when the launch home is another one
func recoverLeftCoreAtGatewayStart(ctx context.Context) error {
	provider.RecoverLeftCoreAtStartup(ctx, nil, true)
	root, err := pypath.Resolve(home.Root())
	if err != nil {
		return err
	}
	launch, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return err
	}
	if root != launch {
		recoverLeftCoreIn(ctx, home.Root(), true)
	}
	return nil
}

// Hydrates a secondary profile, enters its scope once and returns its gateway config, refusing an open policy
func (r *GatewayRunner) loadSecondaryProfileConfig(ctx context.Context, profileName, profileHome string) (*gateway.GatewayConfig, error) {
	provider.HydrateProfileSecretSources(profileHome)
	recoverLeftCoreIn(ctx, profileHome, false)
	scoped := gateway.ProfileRuntimeScope(ctx, profileHome, false)
	profileRuntimeCfg := gateway.LoadGatewayConfigMap(scoped, "")
	plugins.DiscoverPlugins(scoped, false)
	if err := r.subscribePluginRewire(plugins.GetPluginManager(scoped), profileName, profileHome); err != nil {
		return nil, err
	}
	gateway.RegisterWorkerConfigHooks(scoped, fmt.Sprintf("shell-hook/webhook registration failed for profile '%s'", profileName), slog.LevelWarn)
	profileCfg, err := gateway.LoadGatewayConfig(scoped)
	if err != nil {
		return nil, err
	}
	violation := ownPolicyOpenStartupViolation(scoped, profileCfg)
	r.snapshotProfileBusyModes(profileName, profileRuntimeCfg)
	if violation != "" {
		return nil, newMultiplexConfigError(fmt.Sprintf("Profile '%s' enables %s. "+
			"Enable GATEWAY_ALLOW_ALL_USERS or the platform allow-all flag "+
			"for that profile, or change dm_policy/group_policy away from 'open'.", profileName, violation))
	}
	return profileCfg, nil
}

// Whether a secondary platform is already live, queued for reconnect or held across a teardown
func (r *GatewayRunner) secondaryPlatformBusy(profileName string, platform gateway.Platform, profileMap interface{ Has(gateway.Platform) bool }) bool {
	if profileMap.Has(platform) {
		return true
	}
	r.profileFailedMu.Lock()
	_, queued := r.profileFailedPlatforms[profileName][platform]
	r.profileFailedMu.Unlock()
	if queued {
		return true
	}
	r.adapterClaimsMu.Lock()
	defer r.adapterClaimsMu.Unlock()
	return r.secondaryHeldClaims.Has(secondaryKey{profile: profileName, platform: platform})
}

// Creates and connects one profile's adapters under its runtime scope and returns the connected count
func (r *GatewayRunner) startOneProfileAdapters(ctx context.Context, profileName, profileHome string, claimed map[claimKey]string) (int, error) {
	profileCfg, err := r.loadSecondaryProfileConfig(ctx, profileName, profileHome)
	if err != nil {
		return 0, err
	}
	if r.profileConfigs == nil {
		r.profileConfigs = pydict.New[string, *gateway.GatewayConfig]()
	}
	r.profileConfigs.Set(profileName, profileCfg)
	multiplex := r.multiplexOn()
	profileMap := r.profileAdapterMap(profileName)
	scoped := profileScopeNoHydrate(ctx, profileHome)
	connected := 0
	for _, platform := range profileCfg.PlatformKeys() {
		platformConfig := profileCfg.Platforms[platform]
		if platformConfig == nil || !platformConfig.Enabled {
			continue
		}
		if r.secondaryPlatformBusy(profileName, platform, profileMap) {
			continue
		}
		hasCredential := platformHasBotCredential(scoped, platform, platformConfig)
		if multiplex && !hasCredential {
			if platform == gateway.PlatformWhatsApp {
				r.noteUnservedSecondaryPlatform(ctx, profileName, platform)
			}
			runLog().Info(fmt.Sprintf("[MULTIPLEX] Profile '%s': skipping %s - no bot credential "+
				"in this profile's secrets", profileName, platform))
			continue
		}
		if multiplex && platform == gateway.PlatformRelay {
			r.noteUnservedSecondaryPlatform(ctx, profileName, platform)
			continue
		}
		if multiplex && gateway.SharedListenerMirrorPlatforms[string(platform)] {
			runLog().Info(fmt.Sprintf("[MULTIPLEX] Profile '%s': %s is served by the default profile's listener at /p/%s/ — "+
				"not starting a second listener", profileName, platform, profileName))
			continue
		}
		adapter, err := r.createAdapter(scoped, platform, platformConfig)
		if err != nil {
			runLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s': _create_adapter('%s') raised", profileName, platform), "exc_info", err)
			continue
		}
		heals := false
		if adapter == nil {
			heals = r.adapterMayHeal(scoped, platform, platformConfig)
			r.noteUnbuiltSecondaryPlatform(ctx, profileName, platform, heals)
			continue
		}
		owners := r.heldClaimOwners(nil)
		if r.running.Load() {
			owners = r.currentClaimOwners(nil)
		}
		merged := make(map[claimKey]string, len(claimed)+len(owners))
		for k, v := range claimed {
			merged[k] = v
		}
		for k, v := range owners {
			merged[k] = v
		}
		claims, ok := r.claimSecondaryPlatform(ctx, profileName, profileHome, platform, adapter, merged)
		if !ok {
			continue
		}
		r.configureProfileAdapter(ctx, adapter, profileName, platform)
		r.holdSecondaryClaims(profileName, platform, adapter, claims)
		installed, err := func() (bool, error) {
			defer r.releaseSecondaryClaims(profileName, platform, adapter)
			success, err := r.connectInitialAdapterWithTimeout(scoped, adapter, platform)
			if err != nil {
				if isCancellation(err) || ctx.Err() != nil {
					return false, err
				}
				runLog().Error(fmt.Sprintf("✗ %s error (profile: %s): %s", platform, profileName, err.Error()))
				success = false
			} else if !success {
				runLog().Warn(fmt.Sprintf("✗ %s failed to connect (profile: %s)", platform, profileName))
			}
			if !success {
				r.safeAdapterDisconnect(ctx, adapter, platform)
				r.scheduleSecondaryProfileStartupReconnect(ctx, profileName, platform, adapter)
				return false, nil
			}
			profileMap.Set(platform, adapter)
			return true, nil
		}()
		if err != nil {
			return connected, err
		}
		if !installed {
			continue
		}
		r.syncVoiceModeStateToAdapter(ctx, adapter)
		for _, claim := range claims {
			if claim != nil {
				claimed[*claim] = profileName
			}
		}
		connected++
		runLog().Info(fmt.Sprintf("✓ %s connected (profile: %s)", platform, profileName))
	}
	return connected, nil
}

// Re-queues undelivered async completions from every secondary profile's ledger
func (r *GatewayRunner) restoreSecondaryCompletionLedgers(ctx context.Context, profileHomes []profiles.ServedProfile) {
	queue := tools.DefaultProcessRegistry().CompletionQueue
	r.eachSecondaryLedger(ctx, profileHomes, func(profileHome string) (int, error) {
		return tools.RestoreUndeliveredCompletions(&tools.Env{Home: profileHome}, queue)
	}, "Restored")
}
