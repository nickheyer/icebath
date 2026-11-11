package agent

import (
	"fmt"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Constructor options of AIAgent keyword arguments, the TypeError of an unknown keyword or a value its parameter cannot take
func AIAgentOptionsFromKwargs(kwargs map[string]any) (AIAgentOptions, error) {
	var o AIAgentOptions
	for name, value := range kwargs {
		if err := applyAgentKwarg(&o, name, pyjson.Plain(value)); err != nil {
			return AIAgentOptions{}, err
		}
	}
	return o, nil
}

// Keyword value as text, None reading as empty
func agentKwargText(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return pyrepr.Str(value)
}

// Keyword value as a list of texts, None reading as nil
func agentKwargTexts(name string, value any) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string(nil), v...), nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, agentKwargText(item))
		}
		return out, nil
	}
	return nil, agentKwargTypeError(name, value)
}

// Keyword value as an int, None reading as zero
func agentKwargInt(name string, value any) (int, error) {
	if value == nil {
		return 0, nil
	}
	n, err := pyint.Int64(value)
	if err != nil {
		return 0, agentKwargTypeError(name, value)
	}
	if n > math.MaxInt {
		return math.MaxInt, nil
	}
	return int(n), nil
}

// Keyword value as an optional float
func agentKwargFloat(name string, value any) (*float64, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case float64:
		return &v, nil
	}
	n, err := pyint.Int64(value)
	if err != nil {
		return nil, agentKwargTypeError(name, value)
	}
	f := float64(n)
	return &f, nil
}

// Keyword value as a dict, None reading as nil
func agentKwargDict(name string, value any) (map[string]any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return v, nil
	}
	return nil, agentKwargTypeError(name, value)
}

// Keyword value as a list of dicts, a single dict standing for a one entry list
func agentKwargDictList(name string, value any) ([]map[string]any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return []map[string]any{v}, nil
	case []map[string]any:
		return v, nil
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if entry, ok := item.(map[string]any); ok {
				out = append(out, entry)
			}
		}
		return out, nil
	}
	return nil, agentKwargTypeError(name, value)
}

func agentKwargTypeError(name string, value any) error {
	return &workerStoreError{"TypeError", fmt.Sprintf("AIAgent() argument '%s' cannot take a %s", name, pyrepr.TypeName(value))}
}

// Sets the option one keyword names
func applyAgentKwarg(o *AIAgentOptions, name string, value any) error {
	var err error
	switch name {
	case "base_url":
		o.BaseURL = agentKwargText(value)
	case "api_key":
		o.APIKey = agentKwargText(value)
	case "provider":
		o.Provider = agentKwargText(value)
	case "api_mode":
		o.APIMode = agentKwargText(value)
	case "acp_command":
		o.ACPCommand = agentKwargText(value)
	case "acp_args":
		o.ACPArgs, err = agentKwargTexts(name, value)
	case "command":
		o.Command = agentKwargText(value)
	case "args":
		o.Args, err = agentKwargTexts(name, value)
	case "model":
		o.Model = agentKwargText(value)
	case "max_iterations":
		o.MaxIterations, err = agentKwargInt(name, value)
	case "tool_delay":
		o.ToolDelay, err = agentKwargFloat(name, value)
	case "enabled_toolsets":
		o.EnabledToolsets, err = agentKwargTexts(name, value)
	case "disabled_toolsets":
		o.DisabledToolsets, err = agentKwargTexts(name, value)
	case "save_trajectories":
		o.SaveTrajectories = pyrepr.Truthy(value)
	case "verbose_logging":
		o.VerboseLogging = pyrepr.Truthy(value)
	case "quiet_mode":
		o.QuietMode = pyrepr.Truthy(value)
	case "tool_progress_mode":
		o.ToolProgressMode = agentKwargText(value)
	case "ephemeral_system_prompt":
		o.EphemeralSystemPrompt = agentKwargText(value)
	case "log_prefix_chars":
		o.LogPrefixChars, err = agentKwargInt(name, value)
	case "log_prefix":
		o.LogPrefix = agentKwargText(value)
	case "providers_allowed":
		o.ProvidersAllowed = value
	case "providers_ignored":
		o.ProvidersIgnored = value
	case "providers_order":
		o.ProvidersOrder = value
	case "provider_sort":
		o.ProviderSort = value
	case "provider_require_parameters":
		o.ProviderRequireParameters = pyrepr.Truthy(value)
	case "provider_data_collection":
		o.ProviderDataCollection = value
	case "openrouter_min_coding_score":
		o.OpenRouterMinCodingScore = value
	case "session_id":
		o.SessionID = agentKwargText(value)
	case "tool_progress_callback":
		o.ToolProgressCallback = value
	case "tool_start_callback":
		o.ToolStartCallback = value
	case "tool_complete_callback":
		o.ToolCompleteCallback = value
	case "thinking_callback":
		o.ThinkingCallback = value
	case "reasoning_callback":
		o.ReasoningCallback = value
	case "clarify_callback":
		o.ClarifyCallback = value
	case "read_terminal_callback":
		o.ReadTerminalCallback = value
	case "read_preview_callback":
		o.ReadPreviewCallback = value
	case "drive_preview_callback":
		o.DrivePreviewCallback = value
	case "read_window_below_callback":
		o.ReadWindowBelowCallback = value
	case "connection_callback":
		o.ConnectionCallback = value
	case "tour_callback":
		o.TourCallback = value
	case "theme_callback":
		o.ThemeCallback = value
	case "setup_beat_callback":
		o.SetupBeatCallback = value
	case "step_callback":
		o.StepCallback = value
	case "stream_delta_callback":
		o.StreamDeltaCallback = value
	case "interim_assistant_callback":
		o.InterimAssistantCallback = value
	case "tool_gen_callback":
		o.ToolGenCallback = value
	case "status_callback":
		o.StatusCallback = value
	case "notice_callback":
		o.NoticeCallback = value
	case "notice_clear_callback":
		o.NoticeClearCallback = value
	case "event_callback":
		if value != nil {
			cb, ok := value.(func(string, map[string]any))
			if !ok {
				return agentKwargTypeError(name, value)
			}
			o.EventCallback = cb
		}
	case "reaction_callback":
		if value != nil {
			cb, ok := value.(func(string))
			if !ok {
				return agentKwargTypeError(name, value)
			}
			o.ReactionCallback = cb
		}
	case "max_tokens":
		o.MaxTokens, err = agentKwargInt(name, value)
	case "reasoning_config":
		o.ReasoningConfig, err = agentKwargDict(name, value)
	case "service_tier":
		o.ServiceTier = agentKwargText(value)
	case "request_overrides":
		o.RequestOverrides, err = agentKwargDict(name, value)
	case "prefill_messages":
		if value != nil {
			items, ok := value.([]any)
			if !ok {
				return agentKwargTypeError(name, value)
			}
			o.PrefillMessages = items
		}
	case "platform":
		o.Platform = agentKwargText(value)
	case "user_id":
		o.UserID = agentKwargText(value)
	case "user_id_alt":
		o.UserIDAlt = agentKwargText(value)
	case "user_name":
		o.UserName = agentKwargText(value)
	case "chat_id":
		o.ChatID = agentKwargText(value)
	case "chat_name":
		o.ChatName = agentKwargText(value)
	case "chat_type":
		o.ChatType = agentKwargText(value)
	case "thread_id":
		o.ThreadID = agentKwargText(value)
	case "gateway_session_key":
		o.GatewaySessionKey = agentKwargText(value)
	case "skip_context_files":
		o.SkipContextFiles = pyrepr.Truthy(value)
	case "load_soul_identity":
		o.LoadSoulIdentity = pyrepr.Truthy(value)
	case "skip_memory":
		o.SkipMemory = pyrepr.Truthy(value)
	case "skip_background_review":
		o.SkipBackgroundReview = pyrepr.Truthy(value)
	case "session_db":
		if value != nil {
			store, ok := value.(SessionStore)
			if !ok {
				return agentKwargTypeError(name, value)
			}
			o.SessionDB = sessionStoreOrNil(store)
		}
	case "parent_session_id":
		o.ParentSessionID = agentKwargText(value)
	case "iteration_budget":
		if value != nil {
			budget, ok := value.(*IterationBudget)
			if !ok {
				return agentKwargTypeError(name, value)
			}
			o.IterationBudget = budget
		}
	case "run_budget_seconds":
		o.RunBudgetSeconds, err = agentKwargFloat(name, value)
	case "fallback_model":
		o.FallbackModel, err = agentKwargDictList(name, value)
	case "credential_pool":
		if value != nil {
			pool, ok := value.(*provider.Pool)
			if !ok {
				return agentKwargTypeError(name, value)
			}
			o.CredentialPool = pool
		}
	case "checkpoints_enabled":
		o.CheckpointsEnabled = pyrepr.Truthy(value)
	case "checkpoint_max_snapshots":
		o.CheckpointMaxSnapshots, err = agentKwargInt(name, value)
	case "checkpoint_max_total_size_mb":
		o.CheckpointMaxTotalSizeMB, err = agentKwargInt(name, value)
	case "checkpoint_max_file_size_mb":
		o.CheckpointMaxFileSizeMB, err = agentKwargInt(name, value)
	case "pass_session_id":
		o.PassSessionID = pyrepr.Truthy(value)
	case "requested_provider":
		o.RequestedProvider = agentKwargText(value)
	case "capabilities":
		var caps map[string]any
		caps, err = agentKwargDict(name, value)
		if caps != nil {
			o.Capabilities = make(map[string]bool, len(caps))
			for k, v := range caps {
				o.Capabilities[k] = pyrepr.Truthy(v)
			}
		}
	case "cwd":
		o.Cwd = agentKwargText(value)
	case "side_agent":
		o.SideAgent = pyrepr.Truthy(value)
	case "memory_manager":
		if value != nil {
			manager, ok := value.(*memory.MemoryManager)
			if !ok {
				return agentKwargTypeError(name, value)
			}
			o.MemoryManager = manager
		}
	case "tool_result_metadata_callback":
		o.ToolResultMetadataCallback = value
	default:
		return &workerStoreError{"TypeError", fmt.Sprintf("AIAgent.__init__() got an unexpected keyword argument '%s'", name)}
	}
	return err
}
