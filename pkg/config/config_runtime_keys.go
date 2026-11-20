package config

import (
	"fmt"
	"strings"
)

// Core owned keys read at runtime but absent from the defaults on purpose, a default changing what their reader falls back to, mapped to that reader
var CoreRuntimeKeys = map[string]string{
	"agent.prefill_messages_file":              "pkg/gatewayrun/run_config_loaders.go loadPrefillMessages (older place of prefill_messages_file)",
	"cron.inflight_max_minutes":                "pkg/cron/scheduler_inflight.go inflightMinAllowanceMinutes (unset = ICEBATH_CRON_INFLIGHT_MAX_MINUTES, then 30)",
	"display.arena":                            "pkg/vxl/arena/config.go Parse (unset = the arena defaults)",
	"display.busy_text_mode":                   "pkg/gatewayrun/run_config_loaders.go loadBusyTextMode (unset = follows display.busy_input_mode)",
	"display.message_reactions":                "pkg/tuiserver/methods_prompt_history.go (unset = off)",
	"display.thinking_mode":                    "pkg/tuigateway/config.go (older name of display.details_mode)",
	"display.tool_progress_overrides":          "pkg/config/display_config.go (older form of display.platforms.<name>.tool_progress)",
	"display.tui_mouse":                        "pkg/tuigateway/config.go MouseTrackingOf (older name of display.mouse_tracking)",
	"gateway.max_concurrent_sessions":          "pkg/activesessions/max_concurrent.go (nested spelling of max_concurrent_sessions)",
	"gateway.max_live_sessions":                "pkg/tuiserver/session_reaper_cap.go maxLiveSessions (nested spelling of max_live_sessions)",
	"gateway.systemd_watchdog_seconds":         "pkg/gateway/config_types.go CoerceSystemdWatchdogSeconds (unset = off)",
	"kanban.diagnostics":                       "pkg/kanban/diagnostics.go (unset = the built-in thresholds)",
	"stt.elevenlabs.base_url":                  "pkg/media/stt_cloud.go (unset = the provider endpoint)",
	"stt.openai.api_key":                       "pkg/media/stt_cloud.go (unset = the provider key from the environment)",
	"stt.openai.base_url":                      "pkg/media/stt_cloud.go (unset = the provider endpoint)",
	"stt.prompt":                               "pkg/media/transcription_tools.go (unset = no static prompt)",
	"stt.xai.base_url":                         "pkg/media/stt_cloud.go (unset = the provider endpoint)",
	"terminal.docker_orphan_reaper":            "pkg/gatewayrun/run_config_bridge.go bridgeTerminalConfigToEnv (unset = TERMINAL_DOCKER_ORPHAN_REAPER, then on)",
	"terminal.docker_persist_across_processes": "pkg/gatewayrun/run_config_bridge.go bridgeTerminalConfigToEnv (unset = TERMINAL_DOCKER_PERSIST_ACROSS_PROCESSES, then on)",
	"terminal.sandbox_dir":                     "pkg/gatewayrun/run_config_bridge.go bridgeTerminalConfigToEnv (unset = TERMINAL_SANDBOX_DIR, then the tool default)",
	"tts.deepinfra.speed":                      "pkg/media/tts_tool_openai.go (unset = tts.speed, then 1.0)",
	"tts.edge.speed":                           "pkg/media/tts_tool_providers.go (unset = tts.speed, then 1.0)",
	"tts.elevenlabs.base_url":                  "pkg/media/tts_elevenlabs_voices.go (unset = the provider endpoint)",
	"tts.elevenlabs.streaming_model_id":        "pkg/media/tts_streaming_providers.go (unset = tts.elevenlabs.model_id)",
	"tts.gemini.base_url":                      "pkg/media/tts_tool_providers.go (unset = the provider endpoint)",
	"tts.minimax.base_url":                     "pkg/media/tts_tool_providers.go (unset = the endpoint of the region)",
	"tts.minimax.group_id":                     "pkg/media/tts_tool_providers.go (unset = no group id sent)",
	"tts.minimax.region":                       "pkg/media/tts_tool_providers.go (unset = global, or cn when only the cn key is set)",
	"tts.mistral.base_url":                     "pkg/media/tts_tool_providers.go (unset = the provider endpoint)",
	"tts.model":                                "pkg/media/tts_tool_plugins.go pluginVoiceKwargs (unset = the plugin provider's model)",
	"tts.openai.api_key":                       "pkg/media/tts_tool_openai.go (unset = the provider key from the environment)",
	"tts.openai.base_url":                      "pkg/media/tts_tool_openai.go (unset = the provider endpoint)",
	"tts.openai.language":                      "pkg/media/tts_tool_openai.go (unset = no language hint)",
	"tts.openai.speed":                         "pkg/media/tts_tool_openai.go (unset = tts.speed, then 1.0)",
	"tts.optimize_streaming_latency":           "pkg/media/tts_tool_providers.go (read for xai when tts.xai.optimize_streaming_latency is absent)",
	"tts.output_format":                        "pkg/media/tts_tool_plugins.go (unset = the default format of command providers)",
	"tts.piper.length_scale":                   "pkg/media/tts_tool_local.go (unset = the value of the voice model)",
	"tts.piper.noise_scale":                    "pkg/media/tts_tool_local.go (unset = the value of the voice model)",
	"tts.piper.noise_w_scale":                  "pkg/media/tts_tool_local.go (unset = the value of the voice model)",
	"tts.piper.voices_dir":                     "pkg/media/tts_tool_local.go (unset = the voice cache of the home)",
	"tts.speed":                                "pkg/media/tts_tool_openai.go (unset = 1.0)",
	"tts.streaming.provider":                   "pkg/media/tts_streaming_providers.go (unset = tts.provider)",
	"tts.voice":                                "pkg/media/tts_tool_plugins.go pluginVoiceKwargs (unset = the plugin provider's voice)",
	"tts.xai.base_url":                         "pkg/media/tts_tool_providers.go (unset = the provider endpoint)",
	"tts.xai.speech_tags":                      "pkg/media/tts_tool_providers.go (older name of tts.xai.auto_speech_tags)",
	"tts.xai.streaming_url":                    "pkg/media/tts_streaming_xai.go (unset = the provider streaming endpoint)",
	"tts.xai.text_normalization":               "pkg/media/tts_tool_providers.go (unset = the built-in setting)",
}

// Config keys one active plugin manifest declares
type PluginConfigKeys struct {
	Name       string
	ConfigKeys []string
}

// Owners of runtime read keys outside the schema, supplied by the moa, tools and plugin packages
type RuntimeKeyOwners struct {
	IsPresetFieldPath   func(parts []string) bool
	SelectionConfigKeys func() map[string]bool
	ActiveManifests     func() []PluginConfigKeys
}

// Reports whether key equals a path or sits below one
func coveredBy[V any](key string, paths map[string]V) bool {
	for path := range paths {
		if key == path || strings.HasPrefix(key, path+".") {
			return true
		}
	}
	return false
}

// Reports whether a dotted path exists in the defaults through mappings
func InDefaultConfig(path string) bool {
	_, ok := CfgGet(Defaults(), strings.Split(path, ".")...)
	return ok
}

// Config keys active plugins declare minus paths the defaults already define
func PluginDeclaredConfigKeys(manifests []PluginConfigKeys) map[string]bool {
	keys := map[string]bool{}
	for _, m := range manifests {
		for _, path := range m.ConfigKeys {
			if InDefaultConfig(path) {
				pkgLog().Debug(fmt.Sprintf("config_keys: ignoring %s from plugin %s (a DEFAULT_CONFIG key)", path, m.Name))
				continue
			}
			keys[path] = true
		}
	}
	return keys
}

// Reports whether a schema refused key is a moa preset field, a selection or core runtime key, or plugin declared
func IsRuntimeReadKey(parts []string, owners RuntimeKeyOwners) bool {
	if owners.IsPresetFieldPath(parts) {
		return true
	}
	dotted := strings.Join(parts, ".")
	if owners.SelectionConfigKeys()[dotted] || coveredBy(dotted, CoreRuntimeKeys) {
		return true
	}
	return coveredBy(dotted, PluginDeclaredConfigKeys(owners.ActiveManifests()))
}
