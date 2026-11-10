package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"gopkg.in/yaml.v3"
)

// Terminal config keys a left core plugin still reads mapped to the env copy core also wrote
var LeftCoreTerminalKeys = map[string]string{"terminal.singularity_image": "TERMINAL_SINGULARITY_IMAGE"}

// LeftCoreFeature is one feature that moved out of core into a catalog plugin
type LeftCoreFeature struct {
	Plugin             string
	Label              string
	InUse              func(h string, processEnv bool) bool
	Unchanged          string
	SecretEnv          []string
	PrivateEnv         []string
	Toolsets           []string
	OffPlatforms       []string
	ReplacesToolsets   []string
	Platform           string
	EnableEnv          []string
	ChannelEnvPrefixes []string
	CLI                string
	Command            string
	OldCommand         string
	OldCLI             []string
	PluginKey          string
	TerminalBackend    string
	LeftAt             int
	Providers          []string
	ProfileEnv         []string
	LaunchEnv          []string
	Hosts              []string
	Notice             func(h string, processEnv bool) string
}

var copilotAPIProviders = []string{"copilot", "github-copilot", "github-models", "github-model", "github"}

var copilotACPProviders = []string{"copilot-acp", "github-copilot-acp", "copilot-acp-agent"}

var leftCoreTable []LeftCoreFeature

func init() { leftCoreTable = buildLeftCoreTable() }

func noLeftCoreNotice(string, bool) string { return "" }

// Every feature that left core with its plugin, predicates, env names, toolsets, commands, providers and hosts
func buildLeftCoreTable() []LeftCoreFeature {
	rows := []LeftCoreFeature{
		{
			Plugin: "homeassistant", Label: "Home Assistant", InUse: homeassistantInUse,
			Unchanged: "HASS_TOKEN/HASS_URL, platforms.homeassistant and the ha_* tool names are unchanged",
			SecretEnv: []string{"HASS_TOKEN"}, PrivateEnv: []string{"HASS_URL"},
			Toolsets: []string{"homeassistant"}, OffPlatforms: []string{"acp", "webhook"}, Platform: "homeassistant",
			EnableEnv: []string{"HASS_TOKEN"}, ChannelEnvPrefixes: []string{"HASS_"},
		},
		{
			Plugin: "spotify", Label: "Spotify", InUse: spotifyInUse,
			Unchanged: "your Spotify login, the spotify toolset and the spotify_* tools are unchanged; " +
				"`icebath auth spotify` is now `icebath spotify login`",
			CLI: "spotify",
		},
		{
			Plugin: "langfuse", Label: "Langfuse", InUse: langfuseInUse,
			Unchanged: "your Langfuse keys and ICEBATH_LANGFUSE_* settings are unchanged; the plugin is now " +
				"`langfuse` (was observability/langfuse)",
			SecretEnv:  []string{"ICEBATH_LANGFUSE_SECRET_KEY"},
			PrivateEnv: []string{"ICEBATH_LANGFUSE_PUBLIC_KEY", "ICEBATH_LANGFUSE_BASE_URL"},
			PluginKey:  "observability/langfuse",
		},
		{
			Plugin: "mcp-serve", Label: "icebath as an MCP server", InUse: func(string, bool) bool { return false },
			Unchanged: "the conversation tools keep their names; MCP clients change their args to [\"mcp-serve\"]",
			Command:   "mcp-serve", OldCommand: "icebath mcp serve",
		},
		{
			Plugin: "camofox", Label: "Camofox browser backend", InUse: camofoxInUse,
			Unchanged: "CAMOFOX_URL, the browser.camofox settings and the browser_navigate/browser_click/... tool " +
				"names are unchanged; they are the `camofox` toolset now, and core's `browser` toolset " +
				"(browser_exec) is turned off in this profile",
			SecretEnv: []string{"CAMOFOX_API_KEY"}, PrivateEnv: []string{"CAMOFOX_URL"},
			Toolsets: []string{"camofox"}, OffPlatforms: []string{"webhook"}, ReplacesToolsets: []string{"browser"}, LeftAt: 52,
		},
		{
			Plugin: "singularity", Label: "Singularity/Apptainer terminal backend", InUse: singularityInUse,
			Unchanged: "terminal.backend: singularity, terminal.singularity_image (TERMINAL_SINGULARITY_IMAGE) " +
				"and the container_* limits are unchanged",
			TerminalBackend: "singularity",
		},
		{
			Plugin: "agent-importers", Label: "`icebath import-agent`", InUse: agentImportsInUse,
			Unchanged: "`icebath import-agent`, its flags and your import-sync.json sync sources are unchanged",
			Command:   "import-agent", OldCLI: []string{"claw"},
		},
		{
			Plugin: "github-copilot", Label: "GitHub Copilot", InUse: copilotInUse,
			Unchanged: "your COPILOT_GITHUB_TOKEN / GH_TOKEN / GITHUB_TOKEN and `icebath auth add copilot` logins, model " +
				"names and copilot-acp settings are unchanged",
			PrivateEnv: []string{"COPILOT_GITHUB_TOKEN", "COPILOT_API_BASE_URL", "COPILOT_ACP_BASE_URL"},
			Providers:  append(append([]string{}, copilotAPIProviders...), copilotACPProviders...),
			Hosts:      []string{"githubcopilot.com", "models.github.ai"},
			ProfileEnv: []string{"ICEBATH_COPILOT_ACP_COMMAND", "ICEBATH_COPILOT_ACP_ARGS", "COPILOT_CLI_PATH", "COPILOT_ACP_BASE_URL"},
			LaunchEnv:  []string{"ICEBATH_COPILOT_ACP_COMMAND", "ICEBATH_COPILOT_ACP_ARGS", "COPILOT_CLI_PATH"},
			Notice:     copilotReloginNotice,
		},
	}
	for i := range rows {
		if rows[i].Notice == nil {
			rows[i].Notice = noLeftCoreNotice
		}
	}
	return rows
}

// LeftCore returns a copy of every feature row that left core
func LeftCore() []LeftCoreFeature { return append([]LeftCoreFeature(nil), leftCoreTable...) }

func leftCoreFeatureByPlugin(plugin string) *LeftCoreFeature {
	for i := range leftCoreTable {
		if leftCoreTable[i].Plugin == plugin {
			return &leftCoreTable[i]
		}
	}
	panic(fmt.Sprintf("no left core row for plugin %q", plugin))
}

// Python str of a config value
func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return leftCorePyRepr(v)
}

// Python str of value or else, the value counting only when truthy
func pyOrStr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pyStr(v)
}

func anyMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func isTruthyValue(v any) bool {
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyTruthy(v)
}

// Reports whether a config or env value spells a truthy flag the way is_truthy_value does
func IsTruthyValue(v any) bool { return isTruthyValue(v) }

// Raw config.yaml of a home, empty when missing, unreadable or not a mapping
func leftCoreReadConfig(h string) map[string]any {
	doc, err := config.ReadDocument(home.ConfigPath(h))
	if err != nil || doc == nil {
		return map[string]any{}
	}
	return doc
}

// Raw config.yaml for a write back, empty when missing or not a mapping, failing on malformed YAML
func leftCoreReadUserConfigRaw(path string) (map[string]any, error) {
	doc, err := config.ReadDocument(path)
	if err == nil {
		return doc, nil
	}
	var invalid *config.InvalidUserConfigError
	if errors.As(err, &invalid) {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		var probe any
		if yaml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &probe) == nil {
			return map[string]any{}, nil
		}
	}
	return nil, err
}

// User config.yaml with env references expanded and the managed overlay applied, as the gateway loader reads it
func leftCoreYAMLLayers(h string) (map[string]any, error) {
	path := home.ConfigPath(h)
	doc := map[string]any{}
	if _, err := os.Stat(path); err == nil {
		read, err := config.ReadDocument(path)
		if err != nil {
			return nil, err
		}
		doc = read
	}
	expanded, _ := config.Expand(doc, os.LookupEnv).(map[string]any)
	if expanded == nil {
		expanded = map[string]any{}
	}
	return config.ApplyManagedOverlay(expanded), nil
}

// PlatformConfiguredOn reports a platform switched on, or holding a token without an enabled key, in the merged gateway config
func PlatformConfiguredOn(h, name string) bool {
	gwData := gatewayLegacyJSON(h)
	if gwData == nil {
		gwData = map[string]any{}
	}
	yamlCfg, err := leftCoreYAMLLayers(h)
	if err != nil {
		yamlCfg = map[string]any{}
	}
	block, ok := gatewayMergePlatformSections(yamlCfg, yamlCfg["gateway"], gwData, []string{name})[name].(map[string]any)
	if !ok {
		return false
	}
	enabled, token := gatewayPlatformEnabled(block)
	_, explicit := block["enabled"]
	return enabled || (!explicit && strings.TrimSpace(token) != "")
}

// Reports a toolset of names selected for another platform or in the top level toolsets list
func toolsetListed(cfg map[string]any, names map[string]bool, ownPlatform string) bool {
	var selections []any
	if rows, ok := cfg["platform_toolsets"].(map[string]any); ok {
		for platform, sel := range rows {
			if platform != ownPlatform {
				selections = append(selections, sel)
			}
		}
	}
	selections = append(selections, cfg["toolsets"])
	for _, sel := range selections {
		if list, ok := sel.([]any); ok {
			for _, item := range list {
				if names[pyStr(item)] {
					return true
				}
			}
		}
	}
	return false
}

// Home Assistant ran in core for a dotenv token, a process token, the platform switched on or its toolset selected
func homeassistantInUse(h string, processEnv bool) bool {
	if strings.TrimSpace(config.LoadEnvFile(home.EnvPath(h))["HASS_TOKEN"]) != "" {
		return true
	}
	if processEnv {
		if v, _, err := GetSecret(context.Background(), "HASS_TOKEN"); err == nil && strings.TrimSpace(v) != "" {
			return true
		}
	}
	if PlatformConfiguredOn(h, "homeassistant") {
		return true
	}
	return toolsetListed(leftCoreReadConfig(h), map[string]bool{"homeassistant": true, "icebath-homeassistant": true}, "homeassistant")
}

// auth.json of a home, empty when missing, unreadable or invalid
func leftCoreAuthStore(h string) map[string]any {
	data, err := os.ReadFile(home.AuthPath(h))
	if err != nil {
		return map[string]any{}
	}
	var store any
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &store) != nil {
		return map[string]any{}
	}
	return anyMap(store)
}

// Spotify ran in core for a stored login or the spotify toolset selected
func spotifyInUse(h string, _ bool) bool {
	if providers, ok := leftCoreAuthStore(h)["providers"].(map[string]any); ok {
		if state, ok := providers["spotify"].(map[string]any); ok && (pyTruthy(state["access_token"]) || pyTruthy(state["refresh_token"])) {
			return true
		}
	}
	return toolsetListed(leftCoreReadConfig(h), map[string]bool{"spotify": true}, "")
}

// Whether the plugin key or name sits in plugins.enabled and plugins.disabled with the managed overlay applied
func leftCorePluginLists(h, plugin string) (bool, bool) {
	feature := leftCoreFeatureByPlugin(plugin)
	cfg, err := leftCoreYAMLLayers(h)
	if err != nil {
		slog.Default().Debug("left-core: unreadable config", "home", h, "error", err)
		return false, false
	}
	plugins := anyMap(cfg["plugins"])
	listed := func(key string) bool {
		names, ok := plugins[key].([]any)
		if !ok {
			return false
		}
		for _, n := range names {
			if s := pyStr(n); s == feature.PluginKey || s == feature.Plugin {
				return true
			}
		}
		return false
	}
	return listed("enabled"), listed("disabled")
}

func bundledPluginSelected(h, plugin string) bool {
	enabled, disabled := leftCorePluginLists(h, plugin)
	return enabled && !disabled
}

func langfuseInUse(h string, _ bool) bool { return bundledPluginSelected(h, "langfuse") }

func isLaunchHome(h string) bool { return home.Key(h) == home.Key(home.Process()) }

// Singularity ran in core for terminal.backend, the dotenv TERMINAL_ENV or the launch home process env
func singularityInUse(h string, processEnv bool) bool {
	terminal := map[string]any{}
	if cfg, err := leftCoreYAMLLayers(h); err == nil {
		terminal = anyMap(cfg["terminal"])
	} else {
		slog.Default().Debug("left-core: unreadable config", "home", h, "error", err)
	}
	raw := terminal["backend"]
	if !pyTruthy(raw) {
		raw = terminal["env_type"]
	}
	configured := strings.TrimSpace(pyOrStr(raw))
	if configured == "" {
		configured = strings.TrimSpace(resolveHomeDotenv(h)["TERMINAL_ENV"])
	}
	if configured == "" && processEnv && isLaunchHome(h) {
		configured = strings.TrimSpace(os.Getenv("TERMINAL_ENV"))
	}
	return strings.ToLower(configured) == "singularity"
}

func agentImportsInUse(h string, _ bool) bool {
	info, err := os.Stat(filepath.Join(h, "import-sync.json"))
	return err == nil && info.Mode().IsRegular()
}

// Env name from the home dotenv layers, else the launch home process env when asked
func leftCoreHomeEnv(h, name string, processEnv bool) string {
	value := strings.TrimSpace(resolveHomeDotenv(h)[name])
	if value == "" && processEnv && isLaunchHome(h) {
		value = strings.TrimSpace(os.Getenv(name))
	}
	return value
}

// Camofox ran in core when no CDP endpoint or managed gateway won and the stored selection or CAMOFOX_URL chose it
func camofoxInUse(h string, processEnv bool) bool {
	browser := anyMap(leftCoreReadConfig(h)["browser"])
	if strings.TrimSpace(pyOrStr(browser["cdp_url"])) != "" || leftCoreHomeEnv(h, "BROWSER_CDP_URL", processEnv) != "" {
		return false
	}
	if isTruthyValue(browser["use_gateway"]) {
		return false
	}
	selected := ""
	if provider, ok := browser["cloud_provider"]; ok && provider != nil {
		selected = strings.ToLower(strings.TrimSpace(pyStr(provider)))
	}
	if selected != "" {
		return selected == "camofox"
	}
	return leftCoreHomeEnv(h, "CAMOFOX_URL", processEnv) != ""
}

// Every inference route a home configures across model, fallbacks, auxiliary, delegation, moa, cron and custom endpoints
func leftCoreRouteSlots(h string) []map[string]any {
	cfg, err := leftCoreYAMLLayers(h)
	if err != nil {
		slog.Default().Debug("left-core: unreadable config", "home", h, "error", err)
		cfg = map[string]any{}
	}
	withChain := func(slot map[string]any) []map[string]any {
		return append([]map[string]any{slot}, GetFallbackChain(map[string]any{"fallback_providers": slot["fallback_providers"]})...)
	}
	delegation := anyMap(cfg["delegation"])
	slots := []map[string]any{anyMap(cfg["model"])}
	slots = append(slots, GetFallbackChain(cfg)...)
	for _, task := range anyMap(cfg["auxiliary"]) {
		slots = append(slots, anyMap(task))
	}
	slots = append(slots, withChain(delegation)...)
	slots = append(slots, map[string]any{"provider": cronDefaultProvider(cfg)})
	for _, preset := range anyMap(normalizeMoAConfig(cfg["moa"])["presets"]) {
		p := anyMap(preset)
		if refs, ok := p["reference_models"].([]any); ok {
			for _, ref := range refs {
				slots = append(slots, anyMap(ref))
			}
		}
		slots = append(slots, anyMap(p["aggregator"]))
	}
	for _, route := range delegationBackendRoutes(delegation) {
		slots = append(slots, withChain(route)...)
	}
	for _, entry := range CompatibleCustomProviders(cfg) {
		slots = append(slots, map[string]any{"base_url": entry["base_url"]})
	}
	if data, err := os.ReadFile(filepath.Join(home.CronDir(h), "jobs.json")); err == nil {
		var store any
		if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &store) == nil {
			if doc, ok := store.(map[string]any); ok {
				if jobs, ok := doc["jobs"].([]any); ok {
					for _, job := range jobs {
						slots = append(slots, anyMap(job))
					}
				}
			}
		}
	}
	return slots
}

// Routes of every delegation backend options list
func delegationBackendRoutes(delegation map[string]any) []map[string]any {
	var out []map[string]any
	backends, ok := delegation["backends"].(map[string]any)
	if !ok {
		return nil
	}
	for _, b := range backends {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		options, ok := block["options"].([]any)
		if !ok {
			continue
		}
		for _, o := range options {
			if route, ok := o.(map[string]any); ok {
				out = append(out, route)
			}
		}
	}
	return out
}

// Every provider a home routes through plus the auth.json active provider
func leftCoreProviderChoices(h string) map[string]bool {
	chosen := map[string]bool{}
	for _, slot := range leftCoreRouteSlots(h) {
		chosen[strings.ToLower(strings.TrimSpace(pyOrStr(slot["provider"])))] = true
	}
	chosen[strings.ToLower(strings.TrimSpace(pyOrStr(leftCoreAuthStore(h)["active_provider"])))] = true
	delete(chosen, "")
	return chosen
}

// RoutesAtHosts reports a configured route whose base url is at one of the hosts or a subdomain
func RoutesAtHosts(h string, hosts []string) bool {
	if len(hosts) == 0 {
		return false
	}
	urls := map[string]bool{}
	for _, slot := range leftCoreRouteSlots(h) {
		urls[strings.TrimSpace(pyOrStr(slot["base_url"]))] = true
	}
	delete(urls, "")
	for u := range urls {
		for _, host := range hosts {
			if leftCoreAtHost(u, host) {
				return true
			}
		}
	}
	return false
}

// ProvidersSelected returns the names a home routes through or names in ICEBATH_INFERENCE_PROVIDER, minus its own custom endpoints
func ProvidersSelected(h string, names []string, processEnv bool) map[string]bool {
	chosen := leftCoreProviderChoices(h)
	chosen[strings.ToLower(leftCoreHomeEnv(h, "ICEBATH_INFERENCE_PROVIDER", processEnv))] = true
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[strings.ToLower(n)] = true
	}
	out := map[string]bool{}
	for name := range chosen {
		if wanted[name] && !namesCustomEndpoint(h, name) {
			out[name] = true
		}
	}
	return out
}

// Reports a name resolving to one of the home's own providers or custom_providers entries
func namesCustomEndpoint(h, name string) bool {
	cfg, err := leftCoreYAMLLayers(h)
	if err != nil {
		slog.Default().Debug("left-core: unreadable config", "home", h, "error", err)
		return false
	}
	r := newResolver(context.Background(), config.Tree(cfg), config.NewSecrets(h), h, nil)
	return r.NamedCustomProviderConfigPath(name, cfg) != nil
}

func copilotInUse(h string, processEnv bool) bool {
	return len(ProvidersSelected(h, append(append([]string{}, copilotAPIProviders...), copilotACPProviders...), processEnv)) > 0
}

// Sentence telling a Copilot API home with no token the plugin reads to sign in again
func copilotReloginNotice(h string, processEnv bool) string {
	if len(ProvidersSelected(h, copilotAPIProviders, processEnv)) == 0 {
		return ""
	}
	for _, name := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if leftCoreHomeEnv(h, name, processEnv) != "" {
			return ""
		}
	}
	store := leftCoreAuthStore(h)
	if pool, ok := store["credential_pool"].(map[string]any); ok {
		if rows, ok := pool["copilot"].([]any); ok {
			for _, row := range rows {
				if m, ok := row.(map[string]any); ok && strings.SplitN(pyOrStr(m["source"]), ":", 2)[0] == "manual" {
					return ""
				}
			}
		}
	}
	if providers, ok := store["providers"].(map[string]any); ok {
		if entry, ok := providers["copilot"].(map[string]any); ok && len(entry) > 0 {
			return ""
		}
	}
	return "This profile's Copilot login came from `gh auth token` or the GitHub/Copilot CLI, which the plugin " +
		"no longer reads: run `icebath auth add copilot` to sign in."
}

func leftCoreInstallSentence(ctx context.Context, f *LeftCoreFeature) string {
	return fmt.Sprintf("%s moved out of core into the '%s' plugin: install it with `%s`", f.Label, f.Plugin,
		PluginInstallCommand(f.Plugin, home.Dir(ctx)))
}

// PlatformInstallHint names the install command for an unknown platform that left core, else empty
func PlatformInstallHint(ctx context.Context, platform string) string {
	for i := range leftCoreTable {
		if f := &leftCoreTable[i]; f.Platform != "" && f.Platform == platform {
			return ". " + leftCoreInstallSentence(ctx, f)
		}
	}
	return ""
}

// TerminalBackendInstallHint names the install command for a terminal backend that left core, else empty
func TerminalBackendInstallHint(ctx context.Context, backend string) string {
	for i := range leftCoreTable {
		if f := &leftCoreTable[i]; f.TerminalBackend != "" && f.TerminalBackend == backend {
			return ". " + leftCoreInstallSentence(ctx, f)
		}
	}
	return ""
}

// ProviderInstallHint names the install, enable or update command for a provider a left core plugin supplies
func ProviderInstallHint(ctx context.Context, name string) string {
	f, ok := leftCoreProviderFeature(name)
	if !ok {
		return ""
	}
	h := home.Dir(ctx)
	if !PluginPresent(f.Plugin, h) {
		return leftCoreInstallSentence(ctx, f) + "."
	}
	command := PluginInstallCommand(f.Plugin, h)
	if _, disabled := leftCorePluginLists(h, f.Plugin); disabled {
		return fmt.Sprintf("%s moved out of core into the '%s' plugin, which is installed but disabled: run `%s`.",
			f.Label, f.Plugin, strings.ReplaceAll(command, " install ", " enable "))
	}
	cause := ""
	if loadErr := leftCoreLoadError(f); loadErr != "" {
		cause = " (" + loadErr + ")"
	}
	return fmt.Sprintf("%s moved out of core into the '%s' plugin, which is installed but failed to load%s: run `%s`, "+
		"or see `icebath logs` for the cause.", f.Label, f.Plugin, cause, strings.ReplaceAll(command, " install ", " update "))
}

// Error the feature's provider plugin raised on import in the active home, else empty
func leftCoreLoadError(f *LeftCoreFeature) string { return leftCoreHooks().LoadError(f.Plugin) }

func leftCoreProviderInstallHint(name string) string {
	return ProviderInstallHint(context.Background(), name)
}

// FeatureInUse reports the row predicate or a configured route at one of its hosts
func FeatureInUse(f *LeftCoreFeature, h string, processEnv bool) bool {
	return f.InUse(h, processEnv) || (len(f.Hosts) > 0 && RoutesAtHosts(h, f.Hosts))
}

type leftCoreHeldKey struct{}

// WithLeftCoreInstallsHeld marks a context whose resolutions never attempt a left core migration
func WithLeftCoreInstallsHeld(ctx context.Context) context.Context {
	return context.WithValue(ctx, leftCoreHeldKey{}, true)
}

func leftCoreInstallsHeld(ctx context.Context) bool {
	held, _ := ctx.Value(leftCoreHeldKey{}).(bool)
	return held
}

var (
	leftCoreTerminalFrontEnd atomic.Bool
	leftCoreAttemptedMu      sync.Mutex
	leftCoreAttempted        = map[string]bool{}
)

// ServeNoticesOnStderr declares a terminal front end whose provider recovery outcomes print to stderr
func ServeNoticesOnStderr() { leftCoreTerminalFrontEnd.Store(true) }

func leftCoreWasAttempted(key string) bool {
	leftCoreAttemptedMu.Lock()
	defer leftCoreAttemptedMu.Unlock()
	return leftCoreAttempted[key]
}

func stderrSay(message string) { fmt.Fprintln(os.Stderr, message) }

// Installs the plugin of a left core provider a home uses and lacks before resolution reports it unknown
func recoverLeftCoreProvider(ctx context.Context, h, name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "auto" {
		name = strings.ToLower(strings.TrimSpace(pyOrStr(leftCoreAuthStore(h)["active_provider"])))
	}
	f, ok := leftCoreProviderFeature(name)
	if !ok || leftCoreInstallsHeld(ctx) {
		return
	}
	var say func(string)
	if leftCoreTerminalFrontEnd.Load() {
		say = stderrSay
		for _, message := range claimLeftCoreNotices(h) {
			say(message)
		}
	}
	if leftCoreWasAttempted(home.Key(h)) {
		return
	}
	if PluginPresent(f.Plugin, h) {
		return
	}
	if _, registered := ProfileFor(name); registered {
		return
	}
	if FeatureInUse(f, h, true) {
		RecoverLeftCoreAtStartup(home.WithOverride(ctx, h), say, false)
	}
}

// Every row's profile managed env names
func leftCoreProfileEnv() []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range leftCoreTable {
		for _, name := range f.ProfileEnv {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// Every row's launch env names the generic env writers refuse
func leftCoreLaunchEnvNames() map[string]bool {
	out := map[string]bool{}
	for _, f := range leftCoreTable {
		for _, name := range f.LaunchEnv {
			out[name] = true
		}
	}
	return out
}

// LegacyPluginAlias returns the catalog plugin a bundled key that left core now names, else empty
func LegacyPluginAlias(name string) string {
	for _, f := range leftCoreTable {
		if f.PluginKey != "" && f.PluginKey == name {
			return f.Plugin
		}
	}
	return ""
}

// LegacyPluginKeys returns the bundled keys that now name a catalog plugin
func LegacyPluginKeys(plugin string) map[string]bool {
	out := map[string]bool{}
	for _, f := range leftCoreTable {
		if f.PluginKey != "" && f.Plugin == plugin {
			out[f.PluginKey] = true
		}
	}
	return out
}

// MovedCommandHint says where a command a left core plugin took over went and how to get it, else empty
func MovedCommandHint(ctx context.Context, prog, value, action string) string {
	h := home.Dir(ctx)
	for i := range leftCoreTable {
		moved := &leftCoreTable[i]
		if moved.OldCommand == "" || moved.OldCommand != prog+" "+value {
			continue
		}
		get := ""
		if !PluginPresent(moved.Plugin, h) {
			get = fmt.Sprintf(" Install it with: `%s`.", PluginInstallCommand(moved.Plugin, h))
		}
		return fmt.Sprintf("%s moved out of core into the '%s' plugin: `%s` is now `icebath %s`.%s",
			moved.Label, moved.Plugin, moved.OldCommand, moved.Command, get)
	}
	var feature *LeftCoreFeature
	for i := range leftCoreTable {
		f := &leftCoreTable[i]
		switch prog {
		case "icebath auth":
			if f.CLI != "" && f.CLI == value {
				feature = f
			}
		case "icebath":
			if value != "" && (value == f.CLI || value == f.Command || containsString(f.OldCLI, value)) {
				feature = f
			}
		}
		if feature != nil {
			break
		}
	}
	if feature == nil {
		return ""
	}
	install := PluginInstallCommand(feature.Plugin, h)
	get := ""
	if !PluginPresent(feature.Plugin, h) {
		get = fmt.Sprintf(" Install it with: `%s`.", install)
	} else if prog == "icebath" {
		get = fmt.Sprintf(" Enable it with: `%s`.", strings.ReplaceAll(install, " install ", " enable "))
	}
	if prog == "icebath auth" {
		var parts []string
		for _, p := range []string{"icebath auth", action, value} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		verb := action
		if verb == "" {
			verb = "login"
		}
		return fmt.Sprintf("%s moved out of core into the '%s' plugin: `%s` is now `icebath %s %s`.%s",
			feature.Label, feature.Plugin, strings.Join(parts, " "), value, verb, get)
	}
	label := feature.Label
	if containsString(feature.OldCLI, value) {
		label = fmt.Sprintf("`icebath %s`", value)
	}
	return fmt.Sprintf("%s moved out of core into the '%s' plugin, which is not loaded.%s", label, feature.Plugin, get)
}

// MovedPluginHint names the install command for a bundled key or catalog name of a plugin that left core, else empty
func MovedPluginHint(ctx context.Context, name string) string {
	for i := range leftCoreTable {
		if f := &leftCoreTable[i]; f.PluginKey != "" && (name == f.PluginKey || name == f.Plugin) {
			return leftCoreInstallSentence(ctx, f) + "."
		}
	}
	return ""
}

// DeferStartupNotice logs a message and keeps it for the active home's next watched turn
func DeferStartupNotice(ctx context.Context, message string) {
	slog.Default().Warn(message)
	queueLeftCoreNotice(home.Dir(ctx), message)
}

func leftCoreNoticeFile(h string) string {
	return filepath.Join(home.CacheDir(h), "left-core-notices.jsonl")
}

// Runs fn under the cross process lock guarding a home's kept notices, failing after five seconds
func withLeftCoreNoticesLocked(h string, fn func(path string) error) error {
	path := leftCoreNoticeFile(h)
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	f, err := openLockFile(lockPath)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := kernelLock(f, true)
		if err == nil {
			break
		}
		if !isLockContention(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Timed out waiting for the left-core notice lock (%s)", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer kernelLock(f, false)
	return fn(path)
}

// JSON string literal escaping every non ASCII rune as Python json.dumps does
func pyJSONQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || (r >= 0x7f && r < 0x10000):
				if r == 0x7f {
					b.WriteRune(r)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			case r >= 0x10000:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Opens the kept notice file for one append
var leftCoreOpenAppend = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// Keeps a message once for the home's next watched turn in any process
func queueLeftCoreNotice(h, message string) {
	line := pyJSONQuote(message)
	err := withLeftCoreNoticesLocked(h, func(path string) error {
		if data, err := os.ReadFile(path); err == nil {
			for _, existing := range pySplitLines(strings.TrimPrefix(string(data), "\ufeff")) {
				if existing == line {
					return nil
				}
			}
		}
		fh, err := leftCoreOpenAppend(path)
		if err != nil {
			return err
		}
		if _, err := fh.WriteString(line + "\n"); err != nil {
			fh.Close()
			return err
		}
		return fh.Close()
	})
	if err != nil {
		slog.Default().Debug("left-core notice not kept", "home", h, "error", err)
	}
}

// Takes every notice kept for a home exactly once across processes
func claimLeftCoreNotices(h string) []string {
	if _, err := os.Stat(leftCoreNoticeFile(h)); err != nil {
		return nil
	}
	var lines []string
	err := withLeftCoreNoticesLocked(h, func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines = pySplitLines(strings.TrimPrefix(string(data), "\ufeff"))
		return os.Remove(path)
	})
	if err != nil {
		return nil
	}
	var messages []string
	seen := map[string]bool{}
	for _, line := range lines {
		if seen[line] {
			continue
		}
		seen[line] = true
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil || dec.More() {
			continue
		}
		messages = append(messages, pyStr(v))
	}
	return messages
}

// ClaimKeptNotices takes every notice kept for the active home, for a turn someone sees
func ClaimKeptNotices(ctx context.Context) []string { return claimLeftCoreNotices(home.Dir(ctx)) }

// PluginPresent reports the plugin installed in the home, enabled or not
func PluginPresent(plugin, h string) bool {
	info, err := os.Stat(filepath.Join(home.PluginsDir(h), plugin))
	return err == nil && info.IsDir()
}

// Reports core resolving the feature toolsets on for a platform selecting the list
func leftCoreCarried(f *LeftCoreFeature, selection []any) bool {
	for _, item := range selection {
		name := pyStr(item)
		if containsString(f.Toolsets, name) || containsString(f.ReplacesToolsets, name) || name == "icebath" {
			return true
		}
		if rest, ok := strings.CutPrefix(name, "icebath-"); ok && !containsString(f.OffPlatforms, strings.ReplaceAll(rest, "-", "_")) {
			return true
		}
	}
	return false
}

const (
	leftCoreScopedKey    = "_left_core_scoped"
	leftCoreInstalledKey = "_left_core_installed"
	leftCoreUsedKey      = "_left_core_used"
)

// RecordCoreUse marks every row leaving core at the config version that the home uses, reporting a change
func RecordCoreUse(h string, cfg map[string]any, version int) bool {
	var used []*LeftCoreFeature
	for i := range leftCoreTable {
		f := &leftCoreTable[i]
		if f.LeftAt == version && !leftCoreMarked(cfg, leftCoreUsedKey, f) && FeatureInUse(f, h, true) {
			used = append(used, f)
		}
	}
	for _, f := range used {
		leftCoreMark(cfg, leftCoreUsedKey, f)
	}
	return len(used) > 0
}

// LeftCoreVersions lists every config version a row left core at, ascending
func LeftCoreVersions() []int {
	seen := map[int]bool{}
	var out []int
	for _, f := range leftCoreTable {
		if f.LeftAt != 0 && !seen[f.LeftAt] {
			seen[f.LeftAt] = true
			out = append(out, f.LeftAt)
		}
	}
	sort.Ints(out)
	return out
}

// Python int of a config scalar
func pyInt(v any) (int, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return t, true
	case int64:
		return int(t), true
	case uint64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		n, err := strconv.Atoi(t.String())
		return n, err == nil
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(t), "_", ""))
		return n, err == nil
	}
	return 0, false
}

// Reports a home that ran the feature in core from its config version, use marker or missing stamp
func leftCorePredatesSplit(h string, f *LeftCoreFeature) bool {
	if f.LeftAt == 0 {
		return true
	}
	cfg := leftCoreReadConfig(h)
	stamp, stamped := cfg["_config_version"]
	if leftCoreMarked(cfg, leftCoreUsedKey, f) || !stamped {
		return true
	}
	version, ok := pyInt(stamp)
	return ok && version < f.LeftAt
}

func leftCoreMarked(cfg map[string]any, key string, f *LeftCoreFeature) bool {
	done, ok := cfg[key].([]any)
	if !ok {
		return false
	}
	for _, item := range done {
		if pyStr(item) == f.Plugin {
			return true
		}
	}
	return false
}

func leftCoreScopeKept(cfg map[string]any, f *LeftCoreFeature) bool {
	return len(f.Toolsets) == 0 || leftCoreMarked(cfg, leftCoreScopedKey, f)
}

func leftCoreMark(cfg map[string]any, key string, f *LeftCoreFeature) {
	set := map[string]bool{f.Plugin: true}
	if done, ok := cfg[key].([]any); ok {
		for _, item := range done {
			set[pyStr(item)] = true
		}
	}
	cfg[key] = sortedAnyStrings(set)
}

func sortedAnyStrings(set map[string]bool) []any {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

func pyStrList(items []any) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = pyStr(item)
	}
	return out
}

// Turns the replaced core toolsets off in saved platform lists and agent.disabled_toolsets
func leftCoreTurnOffReplaced(cfg map[string]any, f *LeftCoreFeature) {
	replaced := map[string]bool{}
	for _, ts := range f.ReplacesToolsets {
		replaced[ts] = true
	}
	if saved, ok := cfg["platform_toolsets"].(map[string]any); ok {
		for platform, value := range saved {
			selection, ok := parsePlatformToolsetsValue(value)
			if !ok {
				continue
			}
			hit := false
			for _, item := range selection {
				if replaced[pyStr(item)] {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			kept := []any{}
			for _, item := range selection {
				if !replaced[pyStr(item)] {
					kept = append(kept, item)
				}
			}
			keptNames := pyStrList(kept)
			for _, ts := range f.Toolsets {
				if !containsString(keptNames, ts) {
					kept = append(kept, ts)
				}
			}
			saved[platform] = kept
		}
	}
	agent, ok := cfg["agent"].(map[string]any)
	if !ok {
		agent = map[string]any{}
	}
	var disabled []string
	if pyTruthy(agent["disabled_toolsets"]) {
		disabled = parseConfigStringList(agent["disabled_toolsets"])
	}
	turnedOff := map[string]bool{}
	disjoint := true
	for ts := range replaced {
		turnedOff[ts] = true
		if containsString(disabled, ts) {
			disjoint = false
		}
	}
	if !disjoint {
		for _, ts := range f.Toolsets {
			turnedOff[ts] = true
		}
	}
	for _, ts := range disabled {
		delete(turnedOff, ts)
	}
	out := make([]any, 0, len(disabled)+len(turnedOff))
	for _, ts := range disabled {
		out = append(out, ts)
	}
	out = append(out, sortedAnyStrings(turnedOff)...)
	agent["disabled_toolsets"] = out
	cfg["agent"] = agent
}

// Keeps the core era toolset scope once and marks the plugin installed once present, in one config write
func leftCoreRecordMigration(ctx context.Context, h string, f *LeftCoreFeature) error {
	path := home.ConfigPath(h)
	_, statErr := os.Stat(path)
	created := statErr != nil
	cfg, err := leftCoreReadUserConfigRaw(path)
	if err != nil {
		return err
	}
	installed := PluginPresent(f.Plugin, h) && !leftCoreMarked(cfg, leftCoreInstalledKey, f)
	if leftCoreScopeKept(cfg, f) && !installed {
		return nil
	}
	if installed {
		leftCoreMark(cfg, leftCoreInstalledKey, f)
	}
	if !leftCoreScopeKept(cfg, f) {
		type platformSelection struct {
			list []any
			ok   bool
		}
		selections := map[string]platformSelection{}
		if saved, ok := cfg["platform_toolsets"].(map[string]any); ok {
			for platform, value := range saved {
				list, ok := parsePlatformToolsetsValue(value)
				selections[platform] = platformSelection{list, ok}
			}
		}
		for _, platform := range f.OffPlatforms {
			if _, ok := selections[platform]; !ok {
				selections[platform] = platformSelection{}
			}
		}
		known, ok := cfg["known_plugin_toolsets"].(map[string]any)
		if !ok {
			known = map[string]any{}
		}
		for platform, selection := range selections {
			if !selection.ok && !containsString(f.OffPlatforms, platform) {
				continue
			}
			if selection.ok && leftCoreCarried(f, selection.list) {
				continue
			}
			current, _ := known[platform].([]any)
			currentNames := pyStrList(current)
			var missing []string
			for _, ts := range f.Toolsets {
				if !containsString(currentNames, ts) {
					missing = append(missing, ts)
				}
			}
			if len(missing) > 0 {
				set := map[string]bool{}
				for _, name := range append(currentNames, missing...) {
					set[name] = true
				}
				known[platform] = sortedAnyStrings(set)
			}
		}
		if len(known) > 0 {
			cfg["known_plugin_toolsets"] = known
		}
		if len(f.ReplacesToolsets) > 0 {
			leftCoreTurnOffReplaced(cfg, f)
		}
		leftCoreMark(cfg, leftCoreScopedKey, f)
	}
	if created {
		leftCoreStampCreated(h, cfg)
	}
	return config.SaveUser(ctx, h, cfg)
}

// Rows the home uses and never had installed that the catalog ships, converting each row's toolset scope once
func leftCorePending(ctx context.Context, h string, say func(string), processEnv, backoff bool) []*LeftCoreFeature {
	var out []*LeftCoreFeature
	for i := range leftCoreTable {
		f := &leftCoreTable[i]
		if leftCoreMarked(leftCoreReadConfig(h), leftCoreInstalledKey, f) {
			continue
		}
		if !FeatureInUse(f, h, processEnv) {
			continue
		}
		if !leftCorePredatesSplit(h, f) {
			leftCoreSayPostSplit(h, f, say, backoff, processEnv)
			continue
		}
		present := PluginPresent(f.Plugin, h)
		if err := leftCoreRecordMigration(ctx, h, f); err != nil {
			tail := "."
			if !present {
				tail = fmt.Sprintf(" and run `%s`.", PluginInstallCommand(f.Plugin, h))
			}
			say(fmt.Sprintf("  \u26a0 %s moved out of core into the '%s' plugin: its per-platform toolset selection could "+
				"not be kept (%v). Check `icebath tools`%s", f.Label, f.Plugin, err, tail))
			continue
		}
		if present {
			continue
		}
		if backoff && LeftCoreFailedRecently(h, f.Plugin) {
			slog.Default().Info(fmt.Sprintf("%s plugin install failed recently for %s; retrying later or on `icebath update`", f.Label, h))
			continue
		}
		if _, listed := leftCoreHooks().CatalogEntry(f.Plugin); !listed {
			NoteLeftCoreFailure(h, f.Plugin)
			say(leftCoreNoticed(fmt.Sprintf("  \u26a0 %s moved out of core into the '%s' plugin, which this icebath cannot "+
				"find in the plugin catalog yet. Run `%s` once it is listed.", f.Label, f.Plugin, PluginInstallCommand(f.Plugin, h)), h, f, processEnv))
			continue
		}
		out = append(out, f)
	}
	return out
}

// Stamps the config.yaml the conversion creates as current with its rows in use noted
func leftCoreStampCreated(h string, cfg map[string]any) {
	cfg["_config_version"] = config.Version
	noteFirstStamp(h, map[string]any{}, cfg)
}

// Tells a home that uses a feature its stamp says it never ran in core where the feature went
func leftCoreSayPostSplit(h string, f *LeftCoreFeature, say func(string), backoff, processEnv bool) {
	if PluginPresent(f.Plugin, h) || (backoff && LeftCoreFailedRecently(h, f.Plugin)) {
		return
	}
	NoteLeftCoreFailure(h, f.Plugin)
	say(leftCoreNoticed(fmt.Sprintf("  \u26a0 %s moved out of core into the '%s' plugin, which this profile does not have, "+
		"so it is off. Run `%s`.", f.Label, f.Plugin, PluginInstallCommand(f.Plugin, h)), h, f, processEnv))
}

func leftCoreNoticed(message, h string, f *LeftCoreFeature, processEnv bool) string {
	if extra := f.Notice(h, processEnv); extra != "" {
		return message + " " + extra
	}
	return message
}

// Catalog installer bound to a home, consent defaulting to the home's unattended consent
var leftCoreInstallInto = func(parent context.Context, h string, consent *bool) func(name string) (map[string]any, error) {
	return func(name string) (map[string]any, error) {
		ctx := home.WithOverride(parent, h)
		agreed := false
		if consent == nil {
			agreed = unattendedConsent(ctx)
		} else {
			agreed = *consent
		}
		return leftCoreHooks().Install(ctx, name, agreed)
	}
}

// One line cause of an install error, git's own fatal or error line when present
func leftCoreFirstCause(errText string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(errText, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	cause := ""
	found := false
	for _, line := range lines {
		if strings.HasPrefix(line, "fatal:") || strings.HasPrefix(line, "error:") {
			cause = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			found = true
			break
		}
	}
	if !found {
		if len(lines) > 0 {
			cause = lines[0]
		} else {
			cause = "unknown error"
		}
	}
	return strings.TrimRight(cause, ". ")
}

// Installs one row's plugin into a home and reports the outcome
func leftCoreInstallOne(ctx context.Context, h string, f *LeftCoreFeature, install func(string) (map[string]any, error), say func(string), processEnv bool) bool {
	result, err := install(f.Plugin)
	if err != nil {
		result = map[string]any{"ok": false, "error": err.Error()}
	}
	if pyTruthy(result["ok"]) {
		RefreshBoundHome(home.WithOverride(context.Background(), h))
		if err := leftCoreRecordMigration(ctx, h, f); err != nil {
			slog.Default().Debug("left-core install marker not written", "home", h, "error", err)
		}
		say(leftCoreNoticed(fmt.Sprintf("  \u2713 %s moved out of core \u2014 installed the '%s' plugin from the catalog (%s).",
			f.Label, f.Plugin, f.Unchanged), h, f, processEnv))
		return true
	}
	NoteLeftCoreFailure(h, f.Plugin)
	cause := leftCoreFirstCause(pyOrStr(result["error"]))
	say(leftCoreNoticed(fmt.Sprintf("  \u26a0 %s moved out of core and its '%s' plugin could not be installed automatically: %s. Run `%s`.",
		f.Label, f.Plugin, cause, PluginInstallCommand(f.Plugin, h)), h, f, processEnv))
	return false
}

// MigrateLeftCoreHome installs every left core plugin the home uses and lacks, returning the installed names
func MigrateLeftCoreHome(ctx context.Context, h string, install func(string) (map[string]any, error), say func(string), processEnv bool) []string {
	var installed []string
	for _, f := range leftCorePending(ctx, h, say, processEnv, false) {
		if leftCoreInstallOne(ctx, h, f, install, say, processEnv) {
			installed = append(installed, f.Plugin)
		}
	}
	return installed
}

// MigrateAllLeftCoreHomes migrates every profile home sharing the install, grouped by plugin and consent, the enumeration error unhandled like Python
func MigrateAllLeftCoreHomes(ctx context.Context, say func(string)) ([]string, error) {
	labelled := func(h string) func(string) {
		return func(message string) {
			say(fmt.Sprintf("  [%s] %s", PluginHomeLabel(h), strings.TrimLeft(message, " \t\n\r\v\f")))
		}
	}
	type group struct {
		feature *LeftCoreFeature
		homes   []string
	}
	type groupKey struct {
		plugin  string
		consent bool
	}
	index := map[groupKey]int{}
	var groups []*group
	homes, err := leftCoreHooks().DependencyHomes()
	if err != nil {
		return nil, err
	}
	for _, h := range homes {
		features := leftCorePending(ctx, h, labelled(h), isLaunchHome(h), false)
		consent := len(features) > 0 && homeConsent(h)
		for _, f := range features {
			key := groupKey{f.Plugin, consent}
			i, ok := index[key]
			if !ok {
				i = len(groups)
				index[key] = i
				groups = append(groups, &group{feature: f})
			}
			groups[i].homes = append(groups[i].homes, h)
		}
	}
	var installed []string
	cancelled := false
	for _, g := range groups {
		f := g.feature
		if len(g.homes) > 1 && interactiveTerminal() {
			labels := make([]string, len(g.homes))
			for i, h := range g.homes {
				labels[i] = PluginHomeLabel(h)
			}
			say(fmt.Sprintf("  %s is used in %d profiles (%s); your answers to its dependency questions apply to all of them.",
				f.Label, len(g.homes), strings.Join(labels, ", ")))
		}
		gctx := plugins.WithSharedDependencyAnswers(ctx)
		for i, h := range g.homes {
			ok := leftCoreInstallOne(gctx, h, f, leftCoreInstallInto(gctx, h, nil), labelled(h), isLaunchHome(h))
			if ctx.Err() != nil {
				cancelled = true
				break
			}
			if ok {
				installed = append(installed, f.Plugin)
				continue
			}
			rest := g.homes[i+1:]
			if len(rest) > 0 {
				labels := make([]string, len(rest))
				commands := make([]string, len(rest))
				for j, r := range rest {
					labels[j] = PluginHomeLabel(r)
					commands[j] = "`" + PluginInstallCommand(f.Plugin, r) + "`"
				}
				say(fmt.Sprintf("  \u26a0 The '%s' plugin was not installed for %s either. Run %s.",
					f.Plugin, strings.Join(labels, ", "), strings.Join(commands, ", ")))
			}
			break
		}
		if cancelled {
			say("  \u26a0 Plugin migration cancelled. Profiles already migrated keep their plugin; run " +
				"`icebath plugins install <name>` (with `-p <profile>`) for the rest.")
			break
		}
	}
	return installed, nil
}

// Live gateway serving the home, which loaded its platforms before this install
var leftCoreGatewayServes = func(h string) bool { return gatewayServesHome(h) }

// Lazy install policy of the home in ctx
var leftCoreLazyInstallsAllowed = pm.LazyInstallsAllowedFor

// RecoverLeftCoreAtStartup makes one migration attempt per process per home, delivering or keeping every outcome
func RecoverLeftCoreAtStartup(ctx context.Context, say func(string), gatewayStart bool) []string {
	if leftCoreInstallsHeld(ctx) {
		return nil
	}
	if workerConfigSnapshotActive() {
		return nil
	}
	h := home.Dir(ctx)
	key := home.Key(h)
	delivered := map[string]bool{}
	deliver := func(message string) {
		if say == nil {
			queueLeftCoreNotice(h, message)
			return
		}
		if delivered[message] {
			return
		}
		delivered[message] = true
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Default().Debug("left-core migration notification failed", "error", r)
				}
			}()
			say(message)
		}()
	}
	if say != nil {
		for _, message := range claimLeftCoreNotices(h) {
			deliver(message)
		}
	}
	leftCoreAttemptedMu.Lock()
	if leftCoreAttempted[key] {
		leftCoreAttemptedMu.Unlock()
		return nil
	}
	leftCoreAttempted[key] = true
	leftCoreAttemptedMu.Unlock()
	report := func(message string) {
		message = strings.TrimSpace(message)
		slog.Default().Warn(message)
		deliver(message)
	}
	features := leftCorePending(ctx, h, report, true, true)
	if len(features) == 0 {
		return nil
	}
	if !leftCoreLazyInstallsAllowed(ctx) {
		for _, f := range features {
			report(leftCoreNoticed(fmt.Sprintf("\u26a0 %s moved out of core and its '%s' plugin is not installed, so it is off. "+
				"security.allow_lazy_installs is off, so icebath did not fetch it: run `%s`.", f.Label, f.Plugin,
				PluginInstallCommand(f.Plugin, h)), h, f, true))
		}
		return nil
	}
	var installed []string
	consent := true
	for _, f := range features {
		if leftCoreInstallOne(ctx, h, f, leftCoreInstallInto(ctx, h, &consent), report, true) {
			installed = append(installed, f.Plugin)
			if !gatewayStart && f.Platform != "" && leftCoreGatewayServes(h) {
				report(fmt.Sprintf("Restart the gateway (`icebath gateway restart`) so it serves %s.", f.Label))
			}
		}
	}
	return installed
}

// RecoverLeftCoreForCommand migrates the active home first when a command names a left core plugin it uses and lacks
func RecoverLeftCoreForCommand(ctx context.Context, name string) {
	h := home.Dir(ctx)
	for i := range leftCoreTable {
		f := &leftCoreTable[i]
		if name != "" && (name == f.CLI || name == f.Command || containsString(f.OldCLI, name)) &&
			!PluginPresent(f.Plugin, h) && FeatureInUse(f, h, false) {
			RecoverLeftCoreAtStartup(ctx, stderrSay, false)
			return
		}
	}
}
