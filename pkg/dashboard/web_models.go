package dashboard

import (
	"errors"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var (
	optStr   = Optional(Str)
	optInt   = Optional(Int)
	optFloat = Optional(Float)
	optBool  = Optional(Bool)
)

// Field profile defaulting to None, the selector most request models carry
func profileField() ModelField { return FieldDefault("profile", optStr, nil) }

var configUpdateModel = &Model{ClassName: "ConfigUpdate", Fields: []ModelField{
	Field("config", DictOf(Any)),
	profileField(),
}}

var envVarUpdateModel = &Model{ClassName: "EnvVarUpdate", Fields: []ModelField{
	Field("key", Str),
	Field("value", Str),
	profileField(),
	FieldDefault("api_key", Str, ""),
	FieldDefault("provider_setup", Bool, false),
}}

var envVarDeleteModel = &Model{ClassName: "EnvVarDelete", Fields: []ModelField{
	Field("key", Str),
	profileField(),
}}

var envVarRevealModel = &Model{ClassName: "EnvVarReveal", Fields: envVarDeleteModel.Fields}

var memoryProviderConfigUpdateModel = &Model{ClassName: "MemoryProviderConfigUpdate", Fields: []ModelField{
	FieldDefault("values", DictOf(Any), pyjson.NewObject()),
}}

var memoryProviderSetupRequestModel = &Model{ClassName: "MemoryProviderSetupRequest", Fields: []ModelField{
	FieldDefault("values", DictOf(Any), pyjson.NewObject()),
}}

var customEndpointModelDetailModel = &Model{ClassName: "CustomEndpointModelDetail",
	Doc: "One ``/v1/models`` row with the routing metadata a gateway may advertise on a\n    reasoning alias (``gpt-5.6-sol-high`` → ``gpt-5.6-sol`` @ ``high``). See #93622.",
	Fields: []ModelField{
		Field("id", Str),
		FieldDefault("canonical_model", optStr, nil),
		FieldDefault("reasoning_effort", optStr, nil),
	}}

var customEndpointUpdateModel = &Model{ClassName: "CustomEndpointUpdate", Fields: []ModelField{
	FieldDefault("id", Str, ""),
	Field("name", Str),
	Field("base_url", Str),
	Field("model", Str),
	FieldDefault("api_key", optStr, nil),
	FieldDefault("api_mode", Optional(Literal("", "chat_completions", "codex_responses", "anthropic_messages")), nil),
	FieldDefault("context_length", optInt, nil),
	FieldDefault("discover_models", Bool, true),
	FieldDefault("make_default", Bool, false),
	FieldDefault("models", Optional(ListOf(Str)), nil),
	FieldDefault("model_details", Optional(ListOf(customEndpointModelDetailModel)), nil),
}}

var messagingPlatformUpdateModel = &Model{ClassName: "MessagingPlatformUpdate", Fields: []ModelField{
	FieldDefault("enabled", optBool, nil),
	FieldDefault("env", DictOf(Str), pyjson.NewObject()),
	FieldDefault("clear_env", ListOf(Str), []any{}),
	profileField(),
}}

var telegramOnboardingStartModel = &Model{ClassName: "TelegramOnboardingStart", Fields: []ModelField{
	FieldDefault("bot_name", optStr, nil),
}}

var telegramOnboardingApplyModel = &Model{ClassName: "TelegramOnboardingApply", Fields: []ModelField{
	Field("allowed_user_ids", ListOf(Str)),
	profileField(),
}}

var whatsAppOnboardingStartModel = &Model{ClassName: "WhatsAppOnboardingStart", Fields: []ModelField{
	FieldDefault("mode", optStr, "bot"),
	FieldDefault("allowed_users", optStr, ""),
	profileField(),
	FieldDefault("reset_session", Bool, false),
}}

var whatsAppOnboardingApplyModel = &Model{ClassName: "WhatsAppOnboardingApply", Fields: []ModelField{
	FieldDefault("mode", optStr, nil),
	FieldDefault("allowed_users", optStr, nil),
	profileField(),
}}

var audioTranscriptionRequestModel = &Model{ClassName: "AudioTranscriptionRequest", Fields: []ModelField{
	Field("data_url", Str),
	FieldDefault("mime_type", optStr, nil),
}}

var managedFileUploadModel = &Model{ClassName: "ManagedFileUpload", Fields: []ModelField{
	Field("path", Str),
	Field("data_url", Str),
	FieldDefault("overwrite", Bool, true),
}}

var chatImageUploadModel = &Model{ClassName: "ChatImageUpload", Fields: []ModelField{
	Field("data_url", Str),
	FieldDefault("filename", optStr, nil),
}}

var managedDirectoryCreateModel = &Model{ClassName: "ManagedDirectoryCreate", Fields: []ModelField{
	Field("path", Str),
}}

var managedFileDeleteModel = &Model{ClassName: "ManagedFileDelete", Fields: []ModelField{
	Field("path", Str),
	FieldDefault("recursive", Bool, false),
}}

var fileTicketRequestModel = &Model{ClassName: "FileTicketRequest",
	Doc: "POST /api/files/ticket — the exact query the ticketed download/stream URL carries.",
	Fields: []ModelField{
		Field("route", Literal("download", "stream")),
		Field("path", Str),
		profileField(),
		FieldDefault("session_id", optStr, nil),
	}}

var modelAssignmentModel = &Model{ClassName: "ModelAssignment",
	Doc: `POST /api/model/set — assign a provider/model to a slot.

    scope="main" → model.provider + model.default; scope="auxiliary" → auxiliary.<task>.*
    (task="" = every auxiliary slot, task="__reset__" = reset every slot to provider="auto").
    `,
	Fields: []ModelField{
		Field("scope", Str),
		Field("provider", Str),
		Field("model", Str),
		FieldDefault("task", Str, ""),
		FieldDefault("reasoning_effort", optStr, nil),
		FieldDefault("base_url", Str, ""),
		FieldDefault("api_key", Str, ""),
		FieldDefault("confirm_expensive_model", Bool, false),
		profileField(),
	}}

var moaModelSlotModel = &Model{ClassName: "MoaModelSlot", Fields: []ModelField{
	FieldDefault("provider", Str, ""),
	FieldDefault("model", Str, ""),
	FieldDefault("reasoning_effort", optStr, nil),
	FieldDefault("enabled", Bool, true),
}}

// Instance of MoaModelSlot with every default, the aggregator default of the MoA payloads
func moaModelSlotDefault() *pyjson.Object {
	return pyjson.ObjectOf("provider", "", "model", "", "reasoning_effort", nil, "enabled", true)
}

// Rejects JSON booleans and non finite or non positive timeouts before float coercion, empty meaning None
func validateReferenceTimeout(value any) (any, error) {
	if value == nil || value == "" {
		return nil, nil
	}
	timeout := math.NaN()
	if _, isBool := value.(bool); !isBool {
		f, err := pyint.Float(value)
		if err != nil {
			return nil, errors.New("reference_timeout must be a finite positive number")
		}
		timeout = f
	}
	if math.IsInf(timeout, 0) || math.IsNaN(timeout) || timeout <= 0 {
		return nil, errors.New("reference_timeout must be a finite positive number")
	}
	return timeout, nil
}

var moaReferenceControlsFields = []ModelField{
	FieldDefault("reference_timeout", BeforeValidator(validateReferenceTimeout, optFloat), nil),
	FieldDefault("degraded_reference_policy", Literal("loud", "silent"), "loud"),
}

var moaReferenceControlsModel = &Model{ClassName: "_MoaReferenceControls", Fields: moaReferenceControlsFields}

var moaPresetPayloadModel = &Model{ClassName: "MoaPresetPayload", Fields: append(append([]ModelField{}, moaReferenceControlsFields...),
	FieldDefault("reference_models", ListOf(moaModelSlotModel), []any{}),
	FieldDefault("aggregator", moaModelSlotModel, moaModelSlotDefault()),
	FieldDefault("reference_temperature", optFloat, nil),
	FieldDefault("aggregator_temperature", optFloat, nil),
	FieldDefault("fanout", optStr, nil),
	FieldDefault("enabled", Bool, true),
)}

var moaConfigPayloadModel = &Model{ClassName: "MoaConfigPayload", Fields: append(append([]ModelField{}, moaReferenceControlsFields...),
	FieldDefault("default_preset", Str, "default"),
	FieldDefault("active_preset", Str, ""),
	FieldDefault("presets", DictOf(moaPresetPayloadModel), pyjson.NewObject()),
	FieldDefault("reference_models", ListOf(moaModelSlotModel), []any{}),
	FieldDefault("aggregator", moaModelSlotModel, moaModelSlotDefault()),
	FieldDefault("reference_temperature", optFloat, nil),
	FieldDefault("aggregator_temperature", optFloat, nil),
	FieldDefault("fanout", optStr, nil),
	FieldDefault("enabled", Bool, true),
	profileField(),
)}

var fsWriteTextModel = &Model{ClassName: "FsWriteText", Fields: []ModelField{
	Field("path", Str),
	Field("content", Str),
	FieldDefault("session_id", optStr, nil),
}}

var gitPathBodyModel = &Model{ClassName: "GitPathBody", Fields: []ModelField{Field("path", Str)}}

var gitFileBodyModel = &Model{ClassName: "GitFileBody", Fields: []ModelField{
	Field("path", Str),
	FieldDefault("file", optStr, nil),
}}

var gitPrListBodyModel = &Model{ClassName: "GitPrListBody", Fields: []ModelField{
	Field("path", Str),
	FieldDefault("branches", ListOf(Str), []any{}),
	FieldDefault("numbers", ListOf(Int), []any{}),
}}

var sessionPrScanBodyModel = &Model{ClassName: "SessionPrScanBody", Fields: []ModelField{
	FieldDefault("ids", ListOf(Str), []any{}),
}}

var gitCommitBodyModel = &Model{ClassName: "GitCommitBody", Fields: []ModelField{
	Field("path", Str),
	Field("message", Str),
	FieldDefault("push", Bool, false),
}}

var gitWorktreeAddBodyModel = &Model{ClassName: "GitWorktreeAddBody", Fields: []ModelField{
	Field("path", Str),
	FieldDefault("name", optStr, nil),
	FieldDefault("branch", optStr, nil),
	FieldDefault("base", optStr, nil),
	FieldDefault("existingBranch", optStr, nil),
}}

var gitWorktreeRemoveBodyModel = &Model{ClassName: "GitWorktreeRemoveBody", Fields: []ModelField{
	Field("path", Str),
	Field("worktreePath", Str),
	FieldDefault("force", Bool, false),
}}

var gitBranchSwitchBodyModel = &Model{ClassName: "GitBranchSwitchBody", Fields: []ModelField{
	Field("path", Str),
	Field("branch", Str),
}}

var curatorPauseModel = &Model{ClassName: "CuratorPause", Fields: []ModelField{Field("paused", Bool)}}

var learningNodeRefModel = &Model{ClassName: "LearningNodeRef", Fields: []ModelField{
	Field("id", Str),
	profileField(),
}}

var learningNodeEditModel = &Model{ClassName: "LearningNodeEdit", Fields: []ModelField{
	Field("id", Str),
	Field("content", Str),
	profileField(),
}}

var debugShareRequestModel = &Model{ClassName: "DebugShareRequest", Fields: []ModelField{
	FieldDefault("redact", Bool, true),
	FieldDefault("lines", Int, int64(200)),
}}

var ttsSpeakRequestModel = &Model{ClassName: "TTSSpeakRequest", Fields: []ModelField{Field("text", Str)}}

var voiceLiveSessionRequestModel = &Model{ClassName: "VoiceLiveSessionRequest",
	Doc: "POST /api/audio/voice-live/session: the renderer's WebRTC SDP offer plus optional prior\n    text turns (``{\"type\":\"message\",\"role\":..,\"content\":[..]}``) to seed the live voice model.",
	Fields: []ModelField{
		Field("sdp", Str),
		FieldDefault("history", Optional(ListOf(DictOf(Any))), nil),
	}}

var ttsLeaseRequestModel = &Model{ClassName: "TTSLeaseRequest",
	Doc: "POST /api/audio/tts-lease: ``lease`` names the toggle/surface holding the lease\n    (``desktop:read-aloud``, ``desktop:conversation``); ``active`` True acquires + warms, False releases.",
	Fields: []ModelField{
		Field("lease", Str),
		FieldDefault("active", Bool, true),
	}}

var sttLeaseRequestModel = &Model{ClassName: "STTLeaseRequest",
	Doc: "POST /api/audio/stt-lease: ``lease`` names the voice-input session holding the lease\n    (``desktop:voice-input:<renderer>``); ``active`` True acquires + pre-loads the local\n    STT model, False releases. Unlike TTS, release never unloads (shared engine).",
	Fields: []ModelField{
		Field("lease", Str),
		FieldDefault("active", Bool, true),
	}}

var oauthSubmitBodyModel = &Model{ClassName: "OAuthSubmitBody", Fields: []ModelField{
	Field("session_id", Str),
	Field("code", Str),
}}

var bulkDeleteSessionsModel = &Model{ClassName: "BulkDeleteSessions", Fields: []ModelField{
	Field("ids", ListOf(Str)),
	profileField(),
}}

var sessionImportModel = &Model{ClassName: "SessionImport", Fields: []ModelField{
	FieldDefault("request_id", optStr, nil),
	FieldDefault("expected_revision", Optional(StrictIntRange(Bound(0), nil)), nil),
	Field("sessions", ListOf(DictOf(Any))),
	profileField(),
}}

var sessionRenameModel = &Model{ClassName: "SessionRename", Fields: []ModelField{
	FieldDefault("request_id", optStr, nil),
	FieldDefault("expected_revision", Optional(StrictIntRange(Bound(0), nil)), nil),
	FieldDefault("expected_generation", Optional(StrictIntRange(Bound(0), nil)), nil),
	FieldDefault("title", optStr, nil),
	FieldDefault("archived", Optional(StrictBool), nil),
	FieldDefault("hidden", Optional(StrictBool), nil),
	FieldDefault("pinned", Optional(StrictBool), nil),
	FieldDefault("unread", Optional(StrictBool), nil),
	profileField(),
}}

var sessionOwnerBackfillModel = &Model{ClassName: "SessionOwnerBackfill",
	Doc:    "POST /api/sessions/owner-backfill (legacy migration). ``profile`` scopes WHICH state.db is\n    stamped; the stamped value is always that store's own serving-profile identity — the caller\n    cannot inject an arbitrary owner.",
	Fields: []ModelField{profileField()}}

var sessionPruneModel = &Model{ClassName: "SessionPrune", Fields: []ModelField{
	FieldDefault("older_than_days", optFloat, int64(90)),
	FieldDefault("source", optStr, nil),
	profileField(),
	FieldDefault("started_before", optFloat, nil),
	FieldDefault("started_after", optFloat, nil),
	FieldDefault("title_like", optStr, nil),
	FieldDefault("end_reason", optStr, nil),
	FieldDefault("cwd_prefix", optStr, nil),
	FieldDefault("min_messages", optInt, nil),
	FieldDefault("max_messages", optInt, nil),
	FieldDefault("model_like", optStr, nil),
	FieldDefault("provider", optStr, nil),
	FieldDefault("user_id", optStr, nil),
	FieldDefault("chat_id", optStr, nil),
	FieldDefault("chat_type", optStr, nil),
	FieldDefault("branch_like", optStr, nil),
	FieldDefault("min_tokens", optInt, nil),
	FieldDefault("max_tokens", optInt, nil),
	FieldDefault("min_cost", optFloat, nil),
	FieldDefault("max_cost", optFloat, nil),
	FieldDefault("min_tool_calls", optInt, nil),
	FieldDefault("max_tool_calls", optInt, nil),
	FieldDefault("include_archived", Bool, false),
	FieldDefault("dry_run", Bool, false),
}}

var cronJobCreateModel = &Model{ClassName: "CronJobCreate", Fields: []ModelField{
	FieldDefault("paused", StrictBool, false),
	FieldDefault("paused_reason", optStr, nil),
	FieldDefault("prompt", Str, ""),
	Field("schedule", Str),
	FieldDefault("name", Str, ""),
	FieldDefault("deliver", Str, "local"),
	FieldDefault("repeat", Optional(SmartUnion(Int, Str)), nil),
	FieldDefault("skills", Optional(ListOf(Str)), nil),
	FieldDefault("model", optStr, nil),
	FieldDefault("provider", optStr, nil),
	FieldDefault("base_url", optStr, nil),
	FieldDefault("script", optStr, nil),
	FieldDefault("context_from", Optional(Any), nil),
	FieldDefault("enabled_toolsets", Optional(ListOf(Str)), nil),
	FieldDefault("workdir", optStr, nil),
	FieldDefault("no_agent", Bool, false),
	FieldDefault("timezone", optStr, nil),
	FieldDefault("grace_minutes", Optional(SmartUnion(Int, Str)), nil),
	FieldDefault("catch_up", Optional(StrictBool), nil),
}}

var cronJobUpdateModel = &Model{ClassName: "CronJobUpdate", Fields: []ModelField{Field("updates", DictOf(Any))}}

var automationBlueprintInstantiateModel = &Model{ClassName: "AutomationBlueprintInstantiate", Fields: []ModelField{
	Field("blueprint", Str),
	FieldDefault("values", DictOf(Any), pyjson.NewObject()),
}}

var mcpServerCreateModel = &Model{ClassName: "MCPServerCreate", Fields: []ModelField{
	Field("name", Str),
	FieldDefault("url", optStr, nil),
	FieldDefault("command", optStr, nil),
	FieldDefault("args", ListOf(Str), []any{}),
	FieldDefault("env", DictOf(Str), pyjson.NewObject()),
	FieldDefault("auth", optStr, nil),
	FieldDefault("bearer_token", Optional(Secret), nil),
	profileField(),
}}

var mcpServersReplaceModel = &Model{ClassName: "MCPServersReplace", Fields: []ModelField{
	FieldDefault("servers", DictOf(DictOf(Any)), pyjson.NewObject()),
	profileField(),
}}

var mcpEnabledToggleModel = &Model{ClassName: "MCPEnabledToggle", Fields: []ModelField{
	Field("enabled", Bool),
	profileField(),
}}

var mcpCatalogInstallModel = &Model{ClassName: "MCPCatalogInstall", Fields: []ModelField{
	Field("name", Str),
	FieldDefault("env", DictOf(Str), pyjson.NewObject()),
	FieldDefault("enable", Bool, true),
	profileField(),
}}

var pairingApproveModel = &Model{ClassName: "PairingApprove", Fields: []ModelField{
	Field("platform", Str),
	FieldDefault("code", Str, ""),
	FieldDefault("request_id", Str, ""),
	profileField(),
}}

var pairingRevokeModel = &Model{ClassName: "PairingRevoke", Fields: []ModelField{
	Field("platform", Str),
	Field("user_id", Str),
	profileField(),
}}

var webhookCreateModel = &Model{ClassName: "WebhookCreate", Fields: []ModelField{
	Field("name", Str),
	FieldDefault("description", optStr, nil),
	FieldDefault("events", ListOf(Str), []any{}),
	FieldDefault("prompt", optStr, nil),
	FieldDefault("script", optStr, nil),
	FieldDefault("skills", ListOf(Str), []any{}),
	FieldDefault("deliver", Str, "log"),
	FieldDefault("deliver_only", Bool, false),
	FieldDefault("deliver_chat_id", optStr, nil),
	FieldDefault("secret", optStr, nil),
}}

var webhookEnabledToggleModel = &Model{ClassName: "WebhookEnabledToggle", Fields: []ModelField{Field("enabled", Bool)}}

var credentialPoolAddModel = &Model{ClassName: "CredentialPoolAdd", Fields: []ModelField{
	Field("provider", Str),
	Field("api_key", Str),
	FieldDefault("label", optStr, nil),
}}

var memoryProviderSelectModel = &Model{ClassName: "MemoryProviderSelect", Fields: []ModelField{Field("provider", Str)}}

var memoryResetModel = &Model{ClassName: "MemoryReset", Fields: []ModelField{FieldDefault("target", Str, "all")}}

var backupRequestModel = &Model{ClassName: "BackupRequest", Fields: []ModelField{FieldDefault("output", optStr, nil)}}

var importRequestModel = &Model{ClassName: "ImportRequest", Fields: []ModelField{
	Field("archive", Str),
	FieldDefault("force", Bool, false),
}}

var hookCreateModel = &Model{ClassName: "HookCreate", Fields: []ModelField{
	Field("event", Str),
	Field("command", Str),
	FieldDefault("matcher", optStr, nil),
	FieldDefault("timeout", optInt, nil),
	FieldDefault("approve", Bool, true),
}}

var hookDeleteModel = &Model{ClassName: "HookDelete", Fields: []ModelField{
	Field("event", Str),
	Field("command", Str),
}}

var skillInstallRequestModel = &Model{ClassName: "SkillInstallRequest", Fields: []ModelField{
	Field("identifier", Str),
	profileField(),
}}

var skillUninstallRequestModel = &Model{ClassName: "SkillUninstallRequest", Fields: []ModelField{
	Field("name", Str),
	profileField(),
}}

var skillsUpdateRequestModel = &Model{ClassName: "SkillsUpdateRequest", Fields: []ModelField{profileField()}}

var profileCreateModel = &Model{ClassName: "ProfileCreate", Fields: []ModelField{
	Field("name", Str),
	FieldDefault("soul", optStr, nil),
	FieldDefault("clone_from", optStr, nil),
	FieldDefault("clone_from_default", Bool, false),
	FieldDefault("clone_all", Bool, false),
	FieldDefault("clone_channels", Bool, false),
	FieldDefault("no_skills", Bool, false),
	FieldDefault("description", optStr, nil),
	FieldDefault("provider", optStr, nil),
	FieldDefault("model", optStr, nil),
	FieldDefault("mcp_servers", ListOf(mcpServerCreateModel), []any{}),
	FieldDefault("keep_skills", ListOf(Str), []any{}),
	FieldDefault("hub_skills", ListOf(Str), []any{}),
}}

var profileRenameModel = &Model{ClassName: "ProfileRename", Fields: []ModelField{Field("new_name", Str)}}

var profileExportModel = &Model{ClassName: "ProfileExport", Fields: []ModelField{
	FieldDefault("extra_files", DictOf(Str), pyjson.NewObject()),
	FieldDefault("output", Str, ""),
}}

var profileImportModel = &Model{ClassName: "ProfileImport", Fields: []ModelField{
	Field("archive", Str),
	FieldDefault("name", optStr, nil),
}}

var profileSoulUpdateModel = &Model{ClassName: "ProfileSoulUpdate", Fields: []ModelField{Field("content", Str)}}

var profileActiveUpdateModel = &Model{ClassName: "ProfileActiveUpdate", Fields: []ModelField{Field("name", Str)}}

var profileDescriptionUpdateModel = &Model{ClassName: "ProfileDescriptionUpdate", Fields: []ModelField{
	FieldDefault("description", Str, ""),
}}

var profileModelUpdateModel = &Model{ClassName: "ProfileModelUpdate", Fields: []ModelField{
	Field("provider", Str),
	Field("model", Str),
}}

var profileDescribeAutoModel = &Model{ClassName: "ProfileDescribeAuto", Fields: []ModelField{
	FieldDefault("overwrite", Bool, false),
}}

var skillToggleModel = &Model{ClassName: "SkillToggle", Fields: []ModelField{
	Field("name", Str),
	Field("enabled", Bool),
	profileField(),
}}

var skillLockModel = &Model{ClassName: "SkillLock", Fields: []ModelField{
	Field("name", Str),
	Field("locked", Bool),
	profileField(),
}}

var skillCreateModel = &Model{ClassName: "SkillCreate", Fields: []ModelField{
	Field("name", Str),
	Field("content", Str),
	FieldDefault("category", optStr, nil),
	profileField(),
}}

var skillContentUpdateModel = &Model{ClassName: "SkillContentUpdate", Fields: []ModelField{
	Field("name", Str),
	Field("content", Str),
	profileField(),
}}

var toolsetToggleModel = &Model{ClassName: "ToolsetToggle", Fields: []ModelField{
	Field("enabled", Bool),
	profileField(),
}}

var toolsetProviderSelectModel = &Model{ClassName: "ToolsetProviderSelect", Fields: []ModelField{
	Field("provider", Str),
	FieldDefault("capability", optStr, nil),
	profileField(),
}}

var toolsetModelSelectModel = &Model{ClassName: "ToolsetModelSelect", Fields: []ModelField{
	Field("model", Str),
	FieldDefault("provider", optStr, nil),
	profileField(),
}}

var toolsetEnvUpdateModel = &Model{ClassName: "ToolsetEnvUpdate", Fields: []ModelField{
	Field("env", DictOf(Str)),
	profileField(),
}}

var toolsetPostSetupModel = &Model{ClassName: "ToolsetPostSetup", Fields: []ModelField{
	Field("key", Str),
	profileField(),
}}

var terminalBackendSelectModel = &Model{ClassName: "TerminalBackendSelect", Fields: []ModelField{
	Field("backend", Str),
	profileField(),
}}

var rawConfigUpdateModel = &Model{ClassName: "RawConfigUpdate", Fields: []ModelField{
	Field("yaml_text", Str),
	profileField(),
}}

var themeSetBodyModel = &Model{ClassName: "ThemeSetBody", Fields: []ModelField{Field("name", Str)}}

var fontSetBodyModel = &Model{ClassName: "FontSetBody", Fields: []ModelField{Field("font", Str)}}

var agentPluginInstallBodyModel = &Model{ClassName: "_AgentPluginInstallBody", Fields: []ModelField{
	Field("identifier", Str),
	FieldDefault("force", Bool, false),
	FieldDefault("enable", Bool, true),
	FieldDefault("catalog_name", optStr, nil),
	FieldDefault("ref", optStr, nil),
}}

var pluginProvidersPutBodyModel = &Model{ClassName: "_PluginProvidersPutBody", Fields: []ModelField{
	FieldDefault("memory_provider", optStr, nil),
	FieldDefault("context_engine", optStr, nil),
}}

var pluginVisibilityBodyModel = &Model{ClassName: "_PluginVisibilityBody", Fields: []ModelField{Field("hidden", Bool)}}
