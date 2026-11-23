package gatewayrun

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
)

// Ephemeral system prompt of a config from its personality or system prompt, assigned by the command package
var ResolveEphemeralSystemPromptFromConfig func(cfg map[string]any) string

// Persisted /fast preference parsed like the CLI, empty when unset or unknown, assigned by the app package
var ParseServiceTierConfig func(raw string) string

// Python str of a value, empty for a falsy one
func truthyText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Ephemeral prefill messages from the configured prefill file
func loadPrefillMessages(ctx context.Context) []any {
	filePath := pystr.Strip(settings.PrefillMessagesFile.Get(ctx))
	if filePath == "" {
		return nil
	}
	path := expandUser(filePath)
	if !filepath.IsAbs(path) {
		path = filepath.Join(home.Dir(ctx), path)
	}
	if _, err := os.Stat(path); err != nil {
		pkgLog().Warn(fmt.Sprintf("Prefill messages file not found: %s", path))
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to load prefill messages from %s: %v", path, err))
		return nil
	}
	data, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to load prefill messages from %s: %v", path, err))
		return nil
	}
	list, ok := data.([]any)
	if !ok {
		pkgLog().Warn(fmt.Sprintf("Prefill messages file must contain a JSON array: %s", path))
		return nil
	}
	return list
}

// Path with a leading tilde expanded to the user home
func expandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if userHome, err := os.UserHomeDir(); err == nil {
			return filepath.Join(userHome, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// Ephemeral system prompt from the session env, else the configured personality or system prompt
func loadEphemeralSystemPrompt(ctx context.Context) string {
	if prompt := state.SessionEnv(ctx, "ICEBATH_EPHEMERAL_SYSTEM_PROMPT", ""); prompt != "" {
		return prompt
	}
	return ResolveEphemeralSystemPromptFromConfig(gateway.LoadGatewayConfigMap(ctx, ""))
}

// Channel override entry of a channel or thread, nil when absent or no config is bound
func (r *GatewayRunner) channelOverride(platform gateway.Platform, chatID string, threadID, parentID *string) *gateway.ChannelOverride {
	if r.config == nil {
		return nil
	}
	return getChannelOverride(r.config, platform, chatID, threadID, parentID)
}

// Existing silence exemptions, then the optional room permission of the serving profile
func (r *GatewayRunner) silenceAllowedForSource(ctx context.Context, source *gateway.SessionSource, displayKind any, replyExpected *bool, agentResult map[string]any) bool {
	return gateway.SilenceAllowed(displayKind, replyExpected, nil, nil) ||
		gateway.SilenceAllowed(displayKind, replyExpected, r.roomAllowSilence(ctx, source), agentResult)
}

// Room allow_silence policy read in the serving profile, nil when unset or the lookup fails
func (r *GatewayRunner) roomAllowSilence(ctx context.Context, source *gateway.SessionSource) *bool {
	cfg := r.config
	runtime := ""
	if identity := gateway.IdentityOf(source); identity != nil {
		runtime = identity.RuntimeProfile
	} else {
		runtime = optText(source.Profile)
	}
	primary := r.primaryProfileName
	if primary == "" {
		primary = "default"
	}
	routed := runtime != "" && runtime != primary
	override, err := func() (*gateway.ChannelOverride, error) {
		if cfg == nil || routed {
			profileHome, err := r.resolveProfileHomeForSource(ctx, source)
			if err != nil {
				return nil, err
			}
			raw := gateway.LoadGatewayConfigMap(ctx, filepath.Join(profileHome, "config.yaml"))
			name, gatewaySection := string(source.Platform), raw["gateway"]
			merged, err := gateway.MergePlatformSections(ctx, raw, gatewaySection, map[string]any{}, nil)
			if err != nil {
				return nil, err
			}
			block, _ := merged[name].(map[string]any)
			overrides := block["channel_overrides"]
			var gatewayPlatforms any
			if section, ok := gatewaySection.(map[string]any); ok {
				gatewayPlatforms = section["platforms"]
			}
			section, _ := gateway.PlatformSection(raw, name, gatewayPlatforms)
			if sectionMap, ok := section.(map[string]any); ok {
				if own, ok := sectionMap["channel_overrides"].(map[string]any); ok {
					overrides = own
				}
			}
			if _, ok := overrides.(map[string]any); !ok {
				overrides = map[string]any{}
			}
			built, err := gateway.GatewayConfigFromDict(ctx, map[string]any{"platforms": map[string]any{
				name: map[string]any{"channel_overrides": overrides}}})
			if err != nil {
				return nil, err
			}
			cfg = built
		}
		return getChannelOverride(cfg, source.Platform, source.ChatID, source.ThreadID, source.ParentChatID), nil
	}()
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Room allow_silence lookup failed for %s; keeping the reply rule: %v", source.ChatID, err))
		return nil
	}
	if override == nil {
		return nil
	}
	if allow, ok := override.AllowSilence.(bool); ok {
		return &allow
	}
	return nil
}

// Model of a channel, its channel override else the global default
func (r *GatewayRunner) resolveModelForChannel(ctx context.Context, platform gateway.Platform, chatID string, userConfig map[string]any, threadID, parentID *string) string {
	var channel any
	if override := r.channelOverride(platform, chatID, threadID, parentID); override != nil {
		channel = override
	}
	return provider.ResolveEffectiveModel(nil, channel, gateway.ResolveGatewayModel(ctx, userConfig))
}

// Ephemeral system prompt of a channel or thread, its override else the current profile prompt
func (r *GatewayRunner) getSystemPromptForChannel(ctx context.Context, platform gateway.Platform, chatID string, threadID, parentID *string) string {
	override := r.channelOverride(platform, chatID, threadID, parentID)
	if override != nil && pyrepr.Truthy(override.SystemPrompt) {
		return pystr.Strip(pyrepr.Str(override.SystemPrompt))
	}
	return loadEphemeralSystemPrompt(ctx)
}

// Reasoning effort from the gateway config, the per model override before the global one
func loadReasoningConfig(ctx context.Context, model string) map[string]any {
	return config.ResolveReasoningConfig(gateway.LoadGatewayConfigMap(ctx, ""), model)
}

// Value and global flag of /reasoning arguments, a dash pair of the em dash reading as two dashes
func parseReasoningCommandArgs(rawArgs string) (string, bool) {
	text := strings.ReplaceAll(pystr.Strip(rawArgs), "—", "--")
	if text == "" {
		return "", false
	}
	tokens, err := pyshlex.SplitPOSIX(text, false)
	if err != nil {
		tokens = strings.Fields(text)
	}
	var kept []string
	global := false
	for _, token := range tokens {
		if token == "--global" {
			global = true
			continue
		}
		kept = append(kept, token)
	}
	return strings.ToLower(pystr.Strip(strings.Join(kept, " "))), global
}

// Sets or clears the session scoped reasoning override
func (r *GatewayRunner) setSessionReasoningOverride(sessionKey string, reasoningConfig map[string]any) {
	if sessionKey == "" {
		return
	}
	r.sessionReasoningOverrides().Set(sessionKey, cloneReasoning(reasoningConfig))
}

// Sets the /fast override to a tier or explicit normal, or clears it
func (r *GatewayRunner) setSessionServiceTierOverride(sessionKey string, serviceTier *string, clear bool) {
	if sessionKey == "" {
		return
	}
	if clear {
		r.sessionServiceTierOverrides().Set(sessionKey, serviceTierUnset)
		return
	}
	r.sessionServiceTierOverrides().Set(sessionKey, serviceTierOverride{set: true, tier: serviceTier})
}

// Configured /fast preference parsed like the CLI, nil for normal or an unknown tier
func loadServiceTier(ctx context.Context) *string {
	return optString(ParseServiceTierConfig(settings.FastMode.Get(ctx)))
}

// Whether the profile in ctx shows the model's thinking
func loadShowReasoning(ctx context.Context) bool {
	return settings.ShowThinking.Get(ctx)
}

// Busy input behavior of the profile in ctx
func loadBusyInputMode(ctx context.Context) string {
	return settings.BusyMode.Get(ctx)
}

// Busy text behavior, an explicit busy_text_mode first, else queue only for a queue input mode
func loadBusyTextMode(ctx context.Context) string {
	if text := settings.DisplayBusyTextMode.Get(ctx); text != "" {
		return text
	}
	if loadBusyInputMode(ctx) == "queue" {
		return "queue"
	}
	return "interrupt"
}

// Busy text debounce and hard cap seconds of the profile in ctx
func busyTextTiming(ctx context.Context) [2]float64 {
	return [2]float64{settings.DisplayBusyTextDebounceSeconds.Get(ctx).Seconds(), settings.DisplayBusyTextHardCapSeconds.Get(ctx).Seconds()}
}

// Human delay range of the profile in ctx in milliseconds, nil when off, the default range for natural or a backwards custom range
func humanDelay(ctx context.Context) *[2]int {
	natural := [2]int{int(settings.HumanDelayMinMs.Default.Milliseconds()), int(settings.HumanDelayMaxMs.Default.Milliseconds())}
	switch settings.HumanDelay.Get(ctx) {
	case "off":
		return nil
	case "natural":
		return &natural
	}
	bounds := [2]int{int(settings.HumanDelayMinMs.Get(ctx).Milliseconds()), int(settings.HumanDelayMaxMs.Get(ctx).Milliseconds())}
	if bounds[0] > bounds[1] {
		pkgLog().Warn(fmt.Sprintf("Human-like delay minimum %d ms exceeds the maximum %d ms; using the natural range", bounds[0], bounds[1]))
		return &natural
	}
	return &bounds
}

// Caches a routed profile busy policy and pacing for this gateway lifetime, read in the profile scope of ctx
func (r *GatewayRunner) snapshotProfileBusyModes(ctx context.Context, profileName string) {
	inputMode, textMode := loadBusyInputMode(ctx), loadBusyTextMode(ctx)
	if r.busyInputModesByProfile == nil {
		r.busyInputModesByProfile = map[string]string{}
	}
	if r.busyTextModesByProfile == nil {
		r.busyTextModesByProfile = map[string]string{}
	}
	if r.busyTextTimingByProfile == nil {
		r.busyTextTimingByProfile = map[string][2]float64{}
	}
	if r.humanDelayByProfile == nil {
		r.humanDelayByProfile = map[string]*[2]int{}
	}
	r.busyInputModesByProfile[profileName] = inputMode
	r.busyTextModesByProfile[profileName] = textMode
	r.busyTextTimingByProfile[profileName] = busyTextTiming(ctx)
	r.humanDelayByProfile[profileName] = humanDelay(ctx)
}

// Routed profile whose busy policy applies, empty for none
func (r *GatewayRunner) busyProfileNameForSource(ctx context.Context, source *gateway.SessionSource) string {
	if !r.multiplexing() {
		return ""
	}
	if name := pystr.Strip(optText(source.Profile)); name != "" {
		return name
	}
	routed, err := r.profileNameForSource(ctx, source, nil)
	if err != nil {
		return ""
	}
	return pystr.Strip(optText(routed))
}

// Busy mode of the routed profile snapshot, else the runner mode
func (r *GatewayRunner) effectiveBusyMode(ctx context.Context, source *gateway.SessionSource, fallback string, byProfile map[string]string) string {
	profileName := r.busyProfileNameForSource(ctx, source)
	if profileName == "" || byProfile == nil {
		return fallback
	}
	if mode, ok := byProfile[profileName]; ok {
		return mode
	}
	return fallback
}

// Busy input mode of the routed profile startup snapshot
func (r *GatewayRunner) effectiveBusyInputMode(ctx context.Context, source *gateway.SessionSource) string {
	return r.effectiveBusyMode(ctx, source, r.busyInputMode, r.busyInputModesByProfile)
}

// Legacy busy text mode of the routed profile startup snapshot
func (r *GatewayRunner) effectiveBusyTextMode(ctx context.Context, source *gateway.SessionSource) string {
	return r.effectiveBusyMode(ctx, source, r.busyTextMode, r.busyTextModesByProfile)
}

// Graceful restart and stop drain timeout in seconds
func loadRestartDrainTimeout(ctx context.Context) float64 {
	return max(0, settings.AgentRestartDrainTimeout.Get(ctx).Seconds())
}

// Live launchd ExitTimeOut of this job, nil when not launchd owned or unreadable, warning when the drain exceeds it
func loadLaunchdExitTimeout(drainTimeout float64) *float64 {
	label := gateway.LaunchdServiceLabel(nil, runtime.GOOS)
	if label == "" {
		return nil
	}
	exitTimeout := gateway.ReadLaunchdExitTimeoutS(gateway.LaunchdExitTimeoutQuery{Label: label})
	if exitTimeout == nil {
		return nil
	}
	effective := gateway.ResolveLaunchdCappedDrain(drainTimeout, *exitTimeout, nil)
	if effective < drainTimeout {
		pkgLog().Warn(fmt.Sprintf("restart_drain_timeout=%.0fs exceeds the live launchd exit timeout (%.0fs) for %s; "+
			"signal-driven stops will drain at most %.0fs so teardown finishes before launchd "+
			"SIGKILLs (launchd clamps ExitTimeOut in the per-user domain).", drainTimeout, *exitTimeout, label, effective))
	} else {
		pkgLog().Info(fmt.Sprintf("launchd exit timeout for %s is %.0fs (drain %.0fs fits)", label, *exitTimeout, drainTimeout))
	}
	return exitTimeout
}

// In band restart wait for idle timeout in seconds
func loadRestartAfterTurnTimeout(ctx context.Context) float64 {
	return max(0, settings.AgentRestartAfterTurnTimeout.Get(ctx).Seconds())
}

// Cron and api server floor under the stop drain wait in seconds
func loadCronDrainTimeout(ctx context.Context) float64 {
	return max(0, settings.AgentCronDrainTimeout.Get(ctx).Seconds())
}

// Unexpected signal post interrupt grace in seconds
func loadSignalInterruptGraceTimeout(ctx context.Context) float64 {
	return max(0, settings.GatewaySignalInterruptGraceTimeout.Get(ctx).Seconds())
}

// Grace before teardown after interrupting agents, longer on an unexpected signal
func (r *GatewayRunner) postInterruptGraceTimeout() float64 {
	if r.signalInitiatedShutdown && !r.restartRequested {
		return math.Max(0, r.signalInterruptGraceTimeout)
	}
	return gateway.DefaultGatewayPostInterruptGraceTimeout
}

// Background process notification mode of the ambient profile
func loadBackgroundNotificationsMode(ctx context.Context) string {
	return settings.DisplayBackgroundProcessNotifications.Get(ctx)
}

// OpenRouter provider routing preferences of the profile, empty when unset
func loadProviderRouting(ctx context.Context) map[string]any {
	return settings.ProviderRouting.Get(ctx)
}

// Fallback chain of fallback_providers merged with the legacy fallback_model, nil when empty
func loadFallbackModel(ctx context.Context) []map[string]any {
	chain := provider.GetFallbackChain(gateway.LoadGatewayConfigMap(ctx, ""))
	if len(chain) == 0 {
		return nil
	}
	return chain
}

// Rereads the fallback chain of the active home, a failed read keeping that home's last known good chain
func (r *GatewayRunner) refreshFallbackModel(ctx context.Context) []map[string]any {
	activeHome := home.Dir(ctx)
	if r.fallbackModelByHome == nil {
		r.fallbackModelByHome = map[string][]map[string]any{}
	}
	homeKey := home.Key(activeHome)
	cfgPath := filepath.Join(activeHome, "config.yaml")
	exists, err := config.ConfigExists(ctx, cfgPath)
	if err == nil && !exists {
		r.fallbackModel = nil
		r.fallbackModelByHome[homeKey] = nil
		return nil
	}
	var cfg map[string]any
	if err == nil {
		cfg, err = config.LoadUserEffectiveFailClosed(cfgPath, os.LookupEnv)
	}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("fallback_providers refresh: config.yaml read failed; keeping last known-good chain: %v", err))
		if known, ok := r.fallbackModelByHome[homeKey]; ok {
			r.fallbackModel = known
		}
		return r.fallbackModel
	}
	chain := provider.GetFallbackChain(cfg)
	if len(chain) == 0 {
		chain = nil
	}
	r.fallbackModel = chain
	r.fallbackModelByHome[homeKey] = chain
	return chain
}

// Aligns a cached agent fallback chain with the current config unless a cooldown holds it on a fallback
func applyFallbackChainToAgent(a *agent.AIAgent, chain []map[string]any) {
	if a == nil {
		return
	}
	a.ApplyFallbackChain(chain)
}

// Reasoning of a session, its /reasoning override, a local policy level, else the per model or global config
func (r *GatewayRunner) resolveSessionReasoningConfig(ctx context.Context, source *gateway.SessionSource, sessionKey, model string) (map[string]any, error) {
	resolved := r.resolveSessionKeyOrNone(ctx, source, sessionKey)
	if resolved != "" {
		if override, ok := r.sessionReasoningOverrides().Get(resolved); ok {
			return override, nil
		}
	}
	policy, err := gateway.LocalPolicy(ctx, r, source)
	if err != nil {
		return nil, err
	}
	if policy != nil {
		return policy.CurrentReasoningConfig(ctx, r, resolved)
	}
	return loadReasoningConfig(ctx, model), nil
}

// Effective service tier, a set /fast override, a local policy tier, else the config default
func (r *GatewayRunner) resolveSessionServiceTier(ctx context.Context, source *gateway.SessionSource, sessionKey string) (*string, error) {
	resolved := r.resolveSessionKeyOrNone(ctx, source, sessionKey)
	if resolved != "" {
		if override, ok := r.sessionServiceTierOverrides().Get(resolved); ok {
			return override.tier, nil
		}
	}
	if live := gateway.LocalLiveForRoute(r, resolved); live != nil {
		policy, err := gateway.LocalPolicy(ctx, r, live.Source)
		if err != nil {
			return nil, err
		}
		if policy != nil {
			return policy.CurrentServiceTier(ctx, r, resolved)
		}
	}
	return loadServiceTier(ctx), nil
}

// Whether a local owner turn runs with agent.daybreak explicitly true
func (r *GatewayRunner) loadDaybreak(ctx context.Context, source *gateway.SessionSource) bool {
	if source == nil || source.Platform != gateway.PlatformLocal {
		return false
	}
	policy, err := gateway.LocalPolicyForSource(ctx, r, source)
	if err != nil || policy == nil || !gateway.DaybreakLabels[policy.Source] {
		return false
	}
	return settings.Daybreak.Get(ctx)
}
