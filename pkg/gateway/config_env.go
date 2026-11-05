package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Core platform rows of the env names whose presence drives its env enable, in table order
var EnvEnableCredentials = []struct {
	Platform Platform
	Names    []string
}{{PlatformRelay, []string{"GATEWAY_RELAY_URL"}}}

// Platforms already warned that an explicit disable beats env credentials
var explicitDisableWarned = struct {
	sync.Mutex
	set map[Platform]bool
}{set: map[Platform]bool{}}

// Context key marking a status read whose logs and one time warnings are held back
type statusReadKey struct{}

func statusRead(ctx context.Context) bool {
	v, _ := ctx.Value(statusReadKey{}).(bool)
	return v
}

// Credential names a disable warning cites, the core row else the entry's or the bundled declaration
func enableCredentialNames(ctx context.Context, platform Platform) []string {
	for _, row := range EnvEnableCredentials {
		if row.Platform == platform {
			return row.Names
		}
	}
	var names []string
	if entry := Registry.Get(ctx, string(platform)); entry != nil {
		names = entry.EnvEnableCredentials
	}
	if len(names) == 0 {
		_, names, _ = bundledEnvRows(string(platform))
	}
	return names
}

// One time warning that platforms.x.enabled false wins over env credentials
func warnExplicitDisableBeatsEnv(ctx context.Context, platform Platform) {
	explicitDisableWarned.Lock()
	if explicitDisableWarned.set[platform] || statusRead(ctx) {
		explicitDisableWarned.Unlock()
		return
	}
	explicitDisableWarned.set[platform] = true
	explicitDisableWarned.Unlock()
	names := enableCredentialNames(ctx, platform)
	present := []string{}
	for _, n := range names {
		if pystr.Strip(os.Getenv(n)) != "" {
			present = append(present, n)
		}
	}
	if len(present) == 0 {
		present = names
	}
	creds := strings.Join(present, ", ")
	if creds == "" {
		creds = "its credentials"
	}
	p := string(platform)
	slog.Warn(fmt.Sprintf("Platform '%s' is explicitly disabled by platforms.%s.enabled: false in "+
		"config.yaml, so the credentials found in the environment (%s) will NOT "+
		"start its adapter. Environment credentials no longer override an "+
		"explicit disable. Remove the key or set platforms.%s.enabled: true to "+
		"turn it back on.", p, p, creds, p))
}

// True while a multiplexer loads a non default profile's config
func loadingSecondaryUnderMultiplexer(ctx context.Context) bool {
	override := home.Override(ctx)
	return override != "" && provider.IsMultiplexActive(ctx) && home.ProfileNameFor(override) != "default"
}

// Enables a platform on env credentials unless config.yaml explicitly disabled it
func enableFromEnv(ctx context.Context, cfg *GatewayConfig, platform Platform, popMarker, warn bool) *PlatformConfig {
	pc := platformSetDefault(cfg, platform)
	explicit, ok := pc.Extra["_enabled_explicit"]
	if !ok {
		explicit = false
	}
	if popMarker {
		delete(pc.Extra, "_enabled_explicit")
	}
	if pc.Enabled {
		return pc
	}
	if !pyrepr.Truthy(explicit) && !(SharedListenerMirrorPlatforms[string(platform)] && loadingSecondaryUnderMultiplexer(ctx)) {
		pc.Enabled = true
	} else if warn {
		warnExplicitDisableBeatsEnv(ctx, platform)
	}
	return pc
}

// API server enable on a usable key with its listener extras
func applyAPIServerEnv(ctx context.Context, cfg *GatewayConfig) {
	key := getenvStr(ctx, "API_SERVER_KEY", "")
	if !hasUsableAPIServerKey(key) {
		return
	}
	extra := enableFromEnv(ctx, cfg, PlatformAPIServer, true, false).Extra
	extra["key"] = key
	CSVExtras(extra, []CSVSpec{{"cors_origins", getenvStr(ctx, "API_SERVER_CORS_ORIGINS", "")}})
	EnvExtras(ctx, extra, []EnvSpec{{Key: "port", Env: "API_SERVER_PORT", Int: true}, {Key: "host", Env: "API_SERVER_HOST"},
		{Key: "model_name", Env: "API_SERVER_MODEL_NAME"}}, false)
}

// Webhook enable on WEBHOOK_ENABLED, its port and secret also reaching a configured webhook
func applyWebhookEnv(ctx context.Context, cfg *GatewayConfig) {
	enabled, _ := pystr.BoolToken(getenvStr(ctx, "WEBHOOK_ENABLED", ""))
	existing, configured := cfg.Platforms[PlatformWebhook]
	if !enabled && !configured {
		return
	}
	webhook := existing
	if enabled {
		webhook = enableFromEnv(ctx, cfg, PlatformWebhook, true, false)
	}
	EnvExtras(ctx, webhook.Extra, []EnvSpec{{Key: "port", Env: "WEBHOOK_PORT", Int: true}, {Key: "secret", Env: "WEBHOOK_SECRET"}}, false)
}

// Microsoft Graph webhook config from env, the explicit marker read and kept for the relay pass
func applyMSGraphWebhookEnv(ctx context.Context, cfg *GatewayConfig) {
	enabled, _ := pystr.BoolToken(getenvStr(ctx, "MSGRAPH_WEBHOOK_ENABLED", ""))
	clientState := getenvStr(ctx, "MSGRAPH_WEBHOOK_CLIENT_STATE", "")
	resources := getenvStr(ctx, "MSGRAPH_WEBHOOK_ACCEPTED_RESOURCES", "")
	allowedCIDRs := getenvStr(ctx, "MSGRAPH_WEBHOOK_ALLOWED_SOURCE_CIDRS", "")
	_, configured := cfg.Platforms[PlatformMSGraphWebhook]
	if !enabled && !configured && getenvStr(ctx, "MSGRAPH_WEBHOOK_PORT", "") == "" && clientState == "" && resources == "" && allowedCIDRs == "" {
		return
	}
	msgraph := platformSetDefault(cfg, PlatformMSGraphWebhook)
	if enabled {
		enableFromEnv(ctx, cfg, PlatformMSGraphWebhook, false, false)
	}
	EnvExtras(ctx, msgraph.Extra, []EnvSpec{{Key: "port", Env: "MSGRAPH_WEBHOOK_PORT", Int: true}}, false)
	if clientState != "" {
		msgraph.Extra["client_state"] = clientState
	}
	CSVExtras(msgraph.Extra, []CSVSpec{{"accepted_resources", resources}, {"allowed_source_cidrs", allowedCIDRs}})
}

// Env enablement probe result of an entry as a non empty mapping, nil otherwise or when it panics
func pluginProbeSeed(ctx context.Context, entry *PlatformEntry) (seed map[string]any) {
	if entry.EnvEnablementFn == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("env_enablement_fn for %s raised: %v", entry.Name, r))
			seed = nil
		}
	}()
	if seed = entry.EnvEnablementFn(ctx); len(seed) == 0 {
		return nil
	}
	return seed
}

// Connected check of an entry on a transient enabled view seeded with the env extras
func pluginIsConfigured(entry *PlatformEntry, existingExtra, seed map[string]any) (configured bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("is_connected for %s raised: %v — skipping enablement", entry.Name, r))
			configured = false
		}
	}()
	for k, v := range seed {
		if _, ok := existingExtra[k]; !ok && k != "home_channel" {
			existingExtra[k] = v
		}
	}
	view := NewPlatformConfig()
	view.Enabled, view.Extra = true, existingExtra
	ok, err := entry.IsConnected(view)
	if err != nil {
		slog.Debug(fmt.Sprintf("is_connected for %s raised: %s — skipping enablement", entry.Name, err))
		return false
	}
	if !ok {
		slog.Debug(fmt.Sprintf("Plugin platform '%s' available but not configured (is_connected returned False) — skipping enable", entry.Name))
	}
	return ok
}

// Passive dependency probe, false when the check is missing or panics
func entryDepsOK(entry *PlatformEntry) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("check_fn for %s raised: %v", entry.Name, r))
			ok = false
		}
	}()
	if entry.CheckFn == nil {
		slog.Debug(fmt.Sprintf("check_fn for %s raised: 'NoneType' object is not callable", entry.Name))
		return false
	}
	return entry.CheckFn()
}

// Enables a configured plugin platform whose deps are present or installable later
func enablePluginPlatform(ctx context.Context, cfg *GatewayConfig, entry *PlatformEntry) {
	platform, err := ParsePlatform(ctx, entry.Name)
	if err != nil {
		slog.Debug(fmt.Sprintf("unknown platform name %s: %s", pyrepr.Repr(entry.Name), err))
		return
	}
	existing, has := cfg.Platforms[platform]
	alreadyEnabled := has && existing.Enabled
	existingExtra := map[string]any{}
	if has {
		maps.Copy(existingExtra, existing.Extra)
	}
	if has && !alreadyEnabled && pyrepr.Truthy(existingExtra["_enabled_explicit"]) {
		return
	}
	seed := pluginProbeSeed(ctx, entry)
	if !alreadyEnabled && entry.IsConnected != nil && !pluginIsConfigured(entry, existingExtra, seed) {
		return
	}
	if !entryDepsOK(entry) && entry.EnsureDepsFn == nil {
		return
	}
	pc := platformSetDefault(cfg, platform)
	pc.Enabled = true
	if len(seed) == 0 {
		return
	}
	commit := maps.Clone(seed)
	homeRaw := commit["home_channel"]
	delete(commit, "home_channel")
	maps.Copy(pc.Extra, commit)
	if h, ok := homeRaw.(map[string]any); ok && pyrepr.Truthy(h["chat_id"]) {
		name := "Home"
		if pyrepr.Truthy(h["name"]) {
			name = pyrepr.Str(h["name"])
		}
		hc := &HomeChannel{Platform: platform, ChatID: pyrepr.Str(h["chat_id"]), Name: name}
		if pyrepr.Truthy(h["thread_id"]) {
			t := pyrepr.Str(h["thread_id"])
			hc.ThreadID = &t
		}
		hc.PostInit()
		pc.HomeChannel = hc
	}
}

// Platform names of a config, sorted for a stable evidence pass
func configPlatformNames(cfg *GatewayConfig) []string {
	names := make([]string, 0, len(cfg.Platforms))
	for p := range cfg.Platforms {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return names
}

// Plugin entries with activation evidence after idempotent discovery, empty when discovery panics
func configuredPluginEntriesFor(ctx context.Context, cfg *GatewayConfig) (out []*PlatformEntry) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("Plugin platform discovery for env overrides failed: %v", r))
			out = []*PlatformEntry{}
		}
	}()
	plugins.DiscoverPlugins(ctx, false)
	return ConfiguredPluginEntries(ctx, Registry, map[string]any{}, configPlatformNames(cfg), "")
}

// Env rows of one platform the env pass runs, a registry entry or a bundled declaration without one
type envRowsEntry struct {
	name      string
	overrides []EnvStep
}

// Bundled platforms with activation evidence but no registry entry, read from their declarations
func bundledEnvFallbacks(ctx context.Context, cfg *GatewayConfig, entries []*PlatformEntry, only Platform) []envRowsEntry {
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Name] = true
	}
	names := map[string]bool{}
	if only != "" {
		names[string(only)] = true
	} else {
		bundled, _ := bundledPlatformScanCached()
		maps.Copy(names, bundled)
		for n := range bundledEnvSiblings {
			names[n] = true
		}
	}
	for n := range have {
		delete(names, n)
	}
	if len(names) == 0 {
		return nil
	}
	wanted := ActivationPredicate(ctx, map[string]any{}, configPlatformNames(cfg), "", nil)
	out := []envRowsEntry{}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if !wanted(name) {
			continue
		}
		if overrides, _, ok := bundledEnvRows(name); ok && len(overrides) > 0 {
			out = append(out, envRowsEntry{name, overrides})
		}
	}
	return out
}

// Rank of an entry, static members first in enum order so the env rows run deterministically
func platformRank(ctx context.Context, name string) int {
	members := PlatformMemberMap()
	p, err := ParsePlatform(ctx, name)
	if err != nil {
		return len(members)
	}
	if i := slices.Index(members, p); i >= 0 {
		return i
	}
	return len(members)
}

// Runs one env step, an error or panic logged and swallowed so a broken row never breaks the load
func runEnvStep(ctx context.Context, cfg *GatewayConfig, name string, step EnvStep) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("env override for %s raised: %v", name, r))
		}
	}()
	if err := step.Apply(ctx, cfg); err != nil {
		slog.Debug(fmt.Sprintf("env override for %s raised: %s", name, err))
	}
}

// Runs each platform's declared env steps in platform rank order
func applyPluginEnvOverrides(ctx context.Context, cfg *GatewayConfig, entries []envRowsEntry) {
	ranked := slices.Clone(entries)
	ranks := make(map[string]int, len(ranked))
	for _, e := range ranked {
		ranks[e.name] = platformRank(ctx, e.name)
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranks[ranked[i].name] < ranks[ranked[j].name] })
	for _, e := range ranked {
		for _, step := range e.overrides {
			runEnvStep(ctx, cfg, e.name, step)
		}
	}
}

// Registry driven enable of plugin platforms their env rows did not already enable
func enablePluginPlatformsFromEnv(ctx context.Context, cfg *GatewayConfig, entries []*PlatformEntry) {
	for _, entry := range entries {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug(fmt.Sprintf("Plugin platform enable pass failed for %s: %v", entry.Name, r))
				}
			}()
			enablePluginPlatform(ctx, cfg, entry)
		}()
	}
}

// Relay enable from env or YAML, the env stamp disabling every directly connected messaging platform
func applyRelayEnv(ctx context.Context, cfg *GatewayConfig) {
	relayURLEnv := pystr.Strip(getenvStr(ctx, "GATEWAY_RELAY_URL", ""))
	relayURLYAML := ""
	if existing, ok := cfg.Platforms[PlatformRelay]; ok {
		v := existing.Extra["relay_url"]
		if !pyrepr.Truthy(v) {
			v = ""
		}
		relayURLYAML = pystr.Strip(pyrepr.Str(v))
	}
	relayURL := relayURLEnv
	if relayURL == "" {
		relayURL = relayURLYAML
	}
	if relayURL != "" {
		relay := enableFromEnv(ctx, cfg, PlatformRelay, false, true)
		relay.Extra["relay_url"] = strings.TrimRight(relayURL, "/")
		if !relay.Enabled {
			return
		}
	}
	if allow, _ := pystr.BoolToken(getenvStr(ctx, "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS", "")); relayURLEnv == "" || allow {
		return
	}
	skip := map[Platform]bool{PlatformRelay: true, PlatformLocal: true, PlatformAPIServer: true, PlatformWebhook: true}
	for _, platform := range slices.Sorted(maps.Keys(cfg.Platforms)) {
		pc := cfg.Platforms[platform]
		if skip[platform] || !pc.Enabled {
			continue
		}
		switch {
		case statusRead(ctx):
		case pyrepr.Truthy(pc.Extra["_enabled_explicit"]):
			slog.Warn(fmt.Sprintf("Relay connector is configured via GATEWAY_RELAY_URL; "+
				"disabling directly-connected platform '%s' even though "+
				"it is explicitly enabled in this profile's configuration. "+
				"All messaging goes through the connector on this "+
				"deployment. Set GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true "+
				"to keep direct platforms alongside the relay.", platform))
		default:
			slog.Info(fmt.Sprintf("Relay connector is configured via GATEWAY_RELAY_URL; disabling directly-connected platform '%s'.", platform))
		}
		pc.Enabled = false
	}
}

// Drops the explicit enable markers once every env pass has read them
func scrubExplicitMarkers(cfg *GatewayConfig) {
	for _, pc := range cfg.Platforms {
		delete(pc.Extra, "_enabled_explicit")
	}
}

// Env wins over config.yaml and gateway.json, only limiting the plugin passes to one built in platform
func applyEnvOverrides(ctx context.Context, cfg *GatewayConfig, only Platform) {
	var entries []*PlatformEntry
	if only == "" {
		entries = configuredPluginEntriesFor(ctx, cfg)
	} else if entry := discoveredRegistryEntry(ctx, only); entry != nil {
		entries = []*PlatformEntry{entry}
	}
	rows := make([]envRowsEntry, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, envRowsEntry{e.Name, e.EnvOverrides})
	}
	applyPluginEnvOverrides(ctx, cfg, append(rows, bundledEnvFallbacks(ctx, cfg, entries, only)...))
	applyAPIServerEnv(ctx, cfg)
	applyWebhookEnv(ctx, cfg)
	applyMSGraphWebhookEnv(ctx, cfg)
	enablePluginPlatformsFromEnv(ctx, cfg, entries)
	applyRelayEnv(ctx, cfg)
	scrubExplicitMarkers(cfg)
}

// Enabled and connected as the gateway decides them for one built in platform of the profile yamlCfg configures
func BuiltinPlatformStatus(ctx context.Context, platformID string, yamlCfg map[string]any) (enabled, connected, ok bool, err error) {
	if !BuiltinPlatformValues[platformID] {
		return false, false, false, nil
	}
	platform := Platform(platformID)
	names := map[string]bool{platformID: true, string(PlatformRelay): true}
	top, nested := platformLayers(yamlCfg, names)
	platforms, err := MergePlatformSections(ctx, top, nested, map[string]any{}, nil)
	if err != nil {
		return false, false, true, err
	}
	authored := snapshotAuthoredExtra(platforms)
	managedTop, managedNested := platformLayers(config.ApplyManagedOverlay(map[string]any{}), names)
	managed := maps.Clone(managedTop)
	managed["gateway"] = managedNested
	if _, err := applyManagedExtra(ctx, authored, managed, platforms); err != nil {
		return false, false, true, err
	}
	BridgePlatformSharedKeys(top, nested["platforms"], map[string]any{}, platforms, []Platform{platform, PlatformRelay}, nil, authored)
	for _, block := range platforms {
		if m, isMap := block.(map[string]any); isMap {
			delete(m, "home_channel")
		}
	}
	cfg, err := GatewayConfigFromDict(ctx, map[string]any{"platforms": platforms})
	if err != nil {
		return false, false, true, err
	}
	ctx = context.WithValue(ctx, statusReadKey{}, true)
	applyEnvOverrides(ctx, cfg, platform)
	pc, has := cfg.Platforms[platform]
	if !has {
		return false, false, true, nil
	}
	return pc.Enabled, cfg.isPlatformConnected(ctx, platform, pc), true, nil
}

// The root layer and the gateway layer of yamlCfg holding only the named platform blocks
func platformLayers(yamlCfg map[string]any, names map[string]bool) (map[string]any, map[string]any) {
	gatewaySection, _ := yamlCfg["gateway"].(map[string]any)
	top := map[string]any{"platforms": onlyKeys(yamlCfg["platforms"], names)}
	maps.Copy(top, onlyKeys(yamlCfg, names))
	nested := map[string]any{"platforms": onlyKeys(gatewaySection["platforms"], names)}
	maps.Copy(nested, onlyKeys(gatewaySection, names))
	return top, nested
}

// Deep copies of a mapping's entries named in names, empty for a non mapping
func onlyKeys(section any, names map[string]bool) map[string]any {
	out := map[string]any{}
	m, ok := section.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range m {
		if names[k] {
			out[k] = pycopy.DeepCopy(v)
		}
	}
	return out
}
