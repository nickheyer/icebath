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
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Natural human delay range in milliseconds
var defaultHumanDelayRangeMS = [2]int{800, 2500}

// Busy input modes a profile config may name
var busyInputModes = map[string]bool{"interrupt": true, "queue": true, "steer": true}

// Ephemeral system prompt of a config from its personality or system prompt, assigned by the command package
var ResolveEphemeralSystemPromptFromConfig func(cfg map[string]any) string

// Persisted /fast preference parsed like the CLI, empty when unset or unknown, assigned by the app package
var ParseServiceTierConfig func(raw string) string

// Value at section.key of a config, nil when absent
func cfgValue(cfg map[string]any, keys ...string) any {
	value, _ := config.CfgGet(cfg, keys...)
	return value
}

// Python str of a value, empty for a falsy one
func truthyText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Stripped section.key string of the gateway config, empty when unset
func cfgStr(ctx context.Context, section, key string) string {
	return pystr.Strip(truthyText(cfgValue(gateway.LoadGatewayConfigMap(ctx, ""), section, key)))
}

// Non empty stripped env var, else the config string
func envOrCfgStr(ctx context.Context, envVar, section, key string) string {
	if v := pystr.Strip(os.Getenv(envVar)); v != "" {
		return v
	}
	return cfgStr(ctx, section, key)
}

// Ephemeral prefill messages from the env file, the config file, or the legacy agent key
func loadPrefillMessages(ctx context.Context) []any {
	filePath := os.Getenv("ICEBATH_PREFILL_MESSAGES_FILE")
	if filePath == "" {
		cfg := gateway.LoadGatewayConfigMap(ctx, "")
		filePath = truthyText(cfg["prefill_messages_file"])
		if filePath == "" {
			filePath = truthyText(cfgValue(cfg, "agent", "prefill_messages_file"))
		}
	}
	if filePath == "" {
		return nil
	}
	path := expandUser(filePath)
	if !filepath.IsAbs(path) {
		path = filepath.Join(home.Dir(ctx), path)
	}
	if _, err := os.Stat(path); err != nil {
		runLog().Warn(fmt.Sprintf("Prefill messages file not found: %s", path))
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Failed to load prefill messages from %s: %v", path, err))
		return nil
	}
	data, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		runLog().Warn(fmt.Sprintf("Failed to load prefill messages from %s: %v", path, err))
		return nil
	}
	list, ok := data.([]any)
	if !ok {
		runLog().Warn(fmt.Sprintf("Prefill messages file must contain a JSON array: %s", path))
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
		runLog().Warn(fmt.Sprintf("Room allow_silence lookup failed for %s; keeping the reply rule: %v", source.ChatID, err))
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
	r.sessionState(sessionKey).conversation.reasoningOverride = cloneReasoning(reasoningConfig)
}

// Sets the /fast override to a tier or explicit normal, or clears it
func (r *GatewayRunner) setSessionServiceTierOverride(sessionKey string, serviceTier *string, clear bool) {
	if sessionKey == "" {
		return
	}
	if clear {
		r.sessionState(sessionKey).conversation.serviceTierOverride = serviceTierUnset
		return
	}
	r.sessionState(sessionKey).conversation.serviceTierOverride = serviceTierOverride{set: true, tier: serviceTier}
}

// Configured agent.service_tier parsed like the CLI, nil when unset or unknown
func loadServiceTier(ctx context.Context) *string {
	return optString(ParseServiceTierConfig(cfgStr(ctx, "agent", "service_tier")))
}

// The display.show_reasoning toggle
func loadShowReasoning(ctx context.Context) bool {
	return provider.IsTruthyValue(cfgValue(gateway.LoadGatewayConfigMap(ctx, ""), "display", "show_reasoning"))
}

// Busy input behavior from env or config, interrupt unless queue or steer
func loadBusyInputMode(ctx context.Context) string {
	mode := strings.ToLower(envOrCfgStr(ctx, "ICEBATH_GATEWAY_BUSY_INPUT_MODE", "display", "busy_input_mode"))
	if mode == "queue" || mode == "steer" {
		return mode
	}
	return "interrupt"
}

// Busy text behavior, an explicit legacy busy_text_mode first, else queue only for a queue input mode
func loadBusyTextMode(ctx context.Context) string {
	legacy := strings.ToLower(envOrCfgStr(ctx, "ICEBATH_GATEWAY_BUSY_TEXT_MODE", "display", "busy_text_mode"))
	if legacy == "interrupt" || legacy == "queue" {
		return legacy
	}
	if loadBusyInputMode(ctx) == "queue" {
		return "queue"
	}
	return "interrupt"
}

// Busy input and text modes of one profile config, never consulting the process env
func busyModesFromConfig(cfg map[string]any, fallbackInput, fallbackText string) (string, string) {
	rawInput := strings.ToLower(pystr.Strip(truthyText(cfgValue(cfg, "display", "busy_input_mode"))))
	inputMode := fallbackInput
	if busyInputModes[rawInput] {
		inputMode = rawInput
	}
	rawText := strings.ToLower(pystr.Strip(truthyText(cfgValue(cfg, "display", "busy_text_mode"))))
	switch {
	case rawText == "interrupt" || rawText == "queue":
		return inputMode, rawText
	case busyInputModes[rawInput]:
		if inputMode == "queue" {
			return inputMode, "queue"
		}
		return inputMode, "interrupt"
	}
	return inputMode, fallbackText
}

// Busy text debounce and hard cap seconds of one profile, a bad value warned about and defaulted
func busyTextTimingFromConfig(cfg map[string]any) [2]float64 {
	var out [2]float64
	for i, spec := range []struct {
		key string
		def float64
	}{{"busy_text_debounce_seconds", gateway.DefaultBusyTextDebounceSeconds}, {"busy_text_hard_cap_seconds", gateway.DefaultBusyTextHardCapSeconds}} {
		raw := cfgValue(cfg, "display", spec.key)
		if raw == nil || raw == "" {
			out[i] = spec.def
			continue
		}
		value, err := pyint.Float(raw)
		if err != nil || value < 0 || math.IsNaN(value) {
			runLog().Warn(fmt.Sprintf("display.%s=%s is not a non-negative number; using %s", spec.key, pyrepr.Repr(raw), pyrepr.Str(spec.def)))
			value = spec.def
		}
		out[i] = value
	}
	return out
}

// Human delay range of one profile in milliseconds, nil when off, natural for a bad custom range
func humanDelayFromConfig(cfg map[string]any) *[2]int {
	modeRaw := cfgValue(cfg, "human_delay", "mode")
	if modeRaw == nil {
		modeRaw = "off"
	}
	mode := strings.ToLower(pystr.Strip(truthyText(modeRaw)))
	if mode == "" {
		mode = "off"
	}
	if mode == "off" {
		return nil
	}
	bounds := defaultHumanDelayRangeMS
	if mode != "natural" {
		if mode != "custom" {
			runLog().Warn(fmt.Sprintf("human_delay.mode=%s is not off/natural/custom; using natural", pyrepr.Repr(mode)))
			natural := defaultHumanDelayRangeMS
			return &natural
		}
		for i, key := range []string{"min_ms", "max_ms"} {
			def := defaultHumanDelayRangeMS[i]
			raw := cfgValue(cfg, "human_delay", key)
			value := def
			if raw != nil && raw != "" {
				parsed, err := pyint.Int64(raw)
				if err != nil || parsed < 0 {
					runLog().Warn(fmt.Sprintf("human_delay.%s=%s is not a non-negative integer; using %d", key, pyrepr.Repr(raw), def))
				} else {
					value = int(parsed)
				}
			}
			bounds[i] = value
		}
		if bounds[0] > bounds[1] {
			runLog().Warn(fmt.Sprintf("human_delay.min_ms=%d exceeds human_delay.max_ms=%d; using natural range", bounds[0], bounds[1]))
			bounds = defaultHumanDelayRangeMS
		}
	}
	return &bounds
}

// Caches a routed profile busy policy and pacing for this gateway lifetime
func (r *GatewayRunner) snapshotProfileBusyModes(profileName string, cfg map[string]any) {
	inputMode, textMode := busyModesFromConfig(cfg, r.busyInputMode, r.busyTextMode)
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
	r.busyTextTimingByProfile[profileName] = busyTextTimingFromConfig(cfg)
	r.humanDelayByProfile[profileName] = humanDelayFromConfig(cfg)
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

// Warns when a supplied timeout is not a number the parser could read
func warnUnparsableTimeout(cfgKey string, raw any, def float64) {
	if _, err := pyint.Float(raw); err != nil {
		runLog().Warn(fmt.Sprintf("Invalid %s '%s', using default %.0fs", cfgKey, pyrepr.Str(raw), def))
	}
}

// Graceful restart and stop drain timeout in seconds
func loadRestartDrainTimeout(ctx context.Context) float64 {
	raw := envOrCfgStr(ctx, "ICEBATH_RESTART_DRAIN_TIMEOUT", "agent", "restart_drain_timeout")
	value := gateway.ParseRestartDrainTimeout(raw)
	if raw != "" && value == gateway.DefaultGatewayRestartDrainTimeout {
		warnUnparsableTimeout("restart_drain_timeout", raw, gateway.DefaultGatewayRestartDrainTimeout)
	}
	return value
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
		runLog().Warn(fmt.Sprintf("restart_drain_timeout=%.0fs exceeds the live launchd exit timeout (%.0fs) for %s; "+
			"signal-driven stops will drain at most %.0fs so teardown finishes before launchd "+
			"SIGKILLs (launchd clamps ExitTimeOut in the per-user domain).", drainTimeout, *exitTimeout, label, effective))
	} else {
		runLog().Info(fmt.Sprintf("launchd exit timeout for %s is %.0fs (drain %.0fs fits)", label, *exitTimeout, drainTimeout))
	}
	return exitTimeout
}

// Non empty env var else agent.cfgKey parsed, warning once when a supplied value fails to parse
func loadEnvOrAgentCfgTimeout(ctx context.Context, envVar, cfgKey string, parse func(any) float64, def float64) float64 {
	var raw any
	if envRaw, ok := os.LookupEnv(envVar); ok && pystr.Strip(envRaw) != "" {
		raw = envRaw
	} else {
		raw = cfgValue(gateway.LoadGatewayConfigMap(ctx, ""), "agent", cfgKey)
	}
	value := parse(raw)
	if raw != nil && pystr.Strip(pyrepr.Str(raw)) != "" {
		warnUnparsableTimeout(cfgKey, raw, def)
	}
	return value
}

// In band restart wait for idle timeout in seconds
func loadRestartAfterTurnTimeout(ctx context.Context) float64 {
	return loadEnvOrAgentCfgTimeout(ctx, "ICEBATH_RESTART_AFTER_TURN_TIMEOUT", "restart_after_turn_timeout",
		gateway.ParseRestartAfterTurnTimeout, gateway.DefaultGatewayRestartAfterTurnTimeout)
}

// Cron and api server floor under the stop drain wait in seconds
func loadCronDrainTimeout(ctx context.Context) float64 {
	return loadEnvOrAgentCfgTimeout(ctx, "ICEBATH_CRON_DRAIN_TIMEOUT", "cron_drain_timeout",
		gateway.ParseCronDrainTimeout, gateway.DefaultGatewayCronDrainTimeout)
}

// Unexpected signal post interrupt grace in seconds from the gateway config
func loadSignalInterruptGraceTimeout(ctx context.Context) float64 {
	raw := cfgValue(gateway.LoadGatewayConfigMap(ctx, ""), "gateway", "signal_interrupt_grace_timeout")
	value := gateway.ParseSignalInterruptGraceTimeout(raw)
	if raw != nil && raw != "" {
		warnUnparsableTimeout("signal_interrupt_grace_timeout", raw, gateway.DefaultGatewaySignalInterruptGraceTimeout)
	}
	return value
}

// Grace before teardown after interrupting agents, longer on an unexpected signal
func (r *GatewayRunner) postInterruptGraceTimeout() float64 {
	if r.signalInitiatedShutdown && !r.restartRequested {
		return math.Max(0, r.signalInterruptGraceTimeout)
	}
	return gateway.DefaultGatewayPostInterruptGraceTimeout
}

// Background process notification mode of the ambient profile, concise unless a known mode is set
func loadBackgroundNotificationsMode(ctx context.Context) string {
	mode := gateway.PlatformGateEnv(ctx, "ICEBATH_BACKGROUND_NOTIFICATIONS", "")
	if mode == "" {
		raw := cfgValue(gateway.LoadGatewayConfigMap(ctx, ""), "display", "background_process_notifications")
		if raw == false {
			mode = "off"
		} else if raw != nil && raw != "" {
			mode = pyrepr.Str(raw)
		}
	}
	if mode == "" {
		mode = "concise"
	}
	mode = strings.ToLower(pystr.Strip(mode))
	switch mode {
	case "concise", "all", "result", "error", "off":
		return mode
	}
	runLog().Warn(fmt.Sprintf("Unknown background_process_notifications '%s', defaulting to 'concise'", mode))
	return "concise"
}

// OpenRouter provider routing preferences of the gateway config, empty when unset
func loadProviderRouting(ctx context.Context) map[string]any {
	routing, ok := gateway.LoadGatewayConfigMap(ctx, "")["provider_routing"].(map[string]any)
	if !ok || len(routing) == 0 {
		return map[string]any{}
	}
	return routing
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
		runLog().Debug(fmt.Sprintf("fallback_providers refresh: config.yaml read failed; keeping last known-good chain: %v", err))
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
		if state := r.peekSessionState(resolved); state != nil && state.conversation.reasoningOverride != nil {
			return state.conversation.reasoningOverride, nil
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
		if state := r.peekSessionState(resolved); state != nil && state.conversation.serviceTierOverride.set {
			return state.conversation.serviceTierOverride.tier, nil
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
	daybreak, ok := cfgValue(gateway.LoadGatewayConfigMap(ctx, ""), "agent", "daybreak").(bool)
	return ok && daybreak
}
