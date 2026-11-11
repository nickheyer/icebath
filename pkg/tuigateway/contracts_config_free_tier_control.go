package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ConfigGetParams = pydantic.Extend(ProfileParams, "ConfigGetParams", ParamsConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
)

var ConfigProviderRef = pydantic.NewModel("ConfigProviderRef", OpenConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Fac("aliases", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("authenticated", pydantic.Bool, false),
)

var ConfigGetResult = pydantic.NewModel("ConfigGetResult", ClosedConfig,
	pydantic.Def("value", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_progress", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("providers", pydantic.Nullable(pydantic.List(ConfigProviderRef)), nil),
	pydantic.Def("home", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("config", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mtime", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("mcp_rev", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("config.get", ConfigGetParams, ConfigGetResult, "Read one normalised config value (or the whole effective config) the way the UIs render it.")

var ConfigSetParams = pydantic.Extend(ProfileParams, "ConfigSetParams", ParamsConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("value", pydantic.Any, ""),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("confirm_expensive_model", pydantic.Bool, false),
)

var ConfigSetResult = pydantic.NewModel("ConfigSetResult", ClosedConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("value", pydantic.Nullable(pydantic.Union(pydantic.Str, pydantic.Bool)), nil),
	pydantic.Def("warning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("confirm_required", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("confirm_message", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("deferred", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("tool_progress", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("branch", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("history_reset", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("info", pydantic.Nullable(SessionLiveInfo), nil),
)

var _ = Method("config.set", ConfigSetParams, ConfigSetResult, "Change one config key (persisted or session-scoped) and read back the normalised value.")

var SetupStatusResult = pydantic.NewModel("SetupStatusResult", ClosedConfig,
	pydantic.Def("provider_configured", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("ready", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("free_tier_account", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("free_tier_route", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("other_providers", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("inference_provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("ok", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error_code", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("retryable", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("retry_after", pydantic.Nullable(pydantic.Int), nil),
)

var _ = Method("setup.status", ProfileParams, SetupStatusResult, "Loose provider check: is ANY provider auth state discoverable for the (launch or named) profile.")

var SetupRuntimeCheckParams = pydantic.Extend(ProfileParams, "SetupRuntimeCheckParams", ParamsConfig,
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
)

var SetupRuntimeCheckResult = pydantic.NewModel("SetupRuntimeCheckResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error_code", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("free_tier_route", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("setup.runtime_check", SetupRuntimeCheckParams, SetupRuntimeCheckResult, "Strict provider check through the same runtime resolution the agent uses on session creation.")

var DiagnosticsShareNousParams = pydantic.NewModel("DiagnosticsShareNousParams", ParamsConfig,
	pydantic.Def("error_context", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("extra_files", pydantic.Nullable(pydantic.Dict(pydantic.Str)), nil),
	pydantic.Def("log_lines", pydantic.Nullable(pydantic.Int), nil),
)

var DiagnosticsShareNousResult = pydantic.NewModel("DiagnosticsShareNousResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Def("view_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("upload_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("expires_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("diagnostics.share_nous", DiagnosticsShareNousParams, DiagnosticsShareNousResult, "Upload a force-redacted debug bundle to Nous-internal diagnostics storage.")

var FreeTierChallengePayload = pydantic.NewModel("FreeTierChallengePayload", OpenConfig,
	pydantic.Req("type", pydantic.Literal("browser")),
	pydantic.Req("url", pydantic.Str),
	pydantic.Req("required", pydantic.Bool),
	pydantic.Req("expires_in", pydantic.Int),
	pydantic.Req("message", pydantic.Str),
	pydantic.Def("attempt", pydantic.Int, 0),
)

var FreeTierStatusResult = pydantic.NewModel("FreeTierStatusResult", ClosedConfig,
	pydantic.Req("has_guest", pydantic.Bool),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Req("available", pydantic.Bool),
	pydantic.Req("notice_pending", pydantic.Bool),
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error_code", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("retryable", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("retry_after", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("challenge", pydantic.Nullable(FreeTierChallengePayload), nil),
	pydantic.Def("nudge_due_in", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("connector_nudge_due_in", pydantic.Nullable(pydantic.Int), nil),
)

var _ = Method("free_tier.status", ProfileParams, FreeTierStatusResult, "Pure read of the focused profile's free-tier identity state (no network, no side effects).")

var FreeTierChallengeResultParams = pydantic.Extend(ProfileParams, "FreeTierChallengeResultParams", ParamsConfig,
	pydantic.Req("url", pydantic.Str),
	pydantic.Def("attempt", pydantic.Int, 0),
	pydantic.Req("outcome", pydantic.Literal("done", "failed", "closed", "timeout", "refused", "error", "unsupported")),
)

var FreeTierChallengeResult = pydantic.NewModel("FreeTierChallengeResult", ClosedConfig,
	pydantic.Req("accepted", pydantic.Bool),
)

var _ = Method("free_tier.challenge_result", FreeTierChallengeResultParams, FreeTierChallengeResult, "Report a browser window outcome for the matching pending attempt; mint remains authoritative.")

var FreeTierProvisionResult = pydantic.NewModel("FreeTierProvisionResult", ClosedConfig,
	pydantic.Req("has_guest", pydantic.Bool),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error_code", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("retryable", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("retry_after", pydantic.Nullable(pydantic.Int), nil),
)

var _ = Method("free_tier.provision", ProfileParams, FreeTierProvisionResult, "Explicit retry of the free-tier identity mint when the boot bootstrap could not create it.")

var FreeTierAckNoticeResult = pydantic.NewModel("FreeTierAckNoticeResult", ClosedConfig,
	pydantic.Req("acked", pydantic.Bool),
)

var _ = Method("free_tier.ack_notice", ProfileParams, FreeTierAckNoticeResult, "Mark the one-time availability notice as shown on the free-tier identity.")

var FreeTierClaimNudgeParams = pydantic.Extend(ProfileParams, "FreeTierClaimNudgeParams", ParamsConfig,
	pydantic.Def("kind", pydantic.Literal("trial", "connector"), "trial"),
)

var FreeTierClaimNudgeResult = pydantic.NewModel("FreeTierClaimNudgeResult", ClosedConfig,
	pydantic.Req("claimed", pydantic.Bool),
)

var _ = Method("free_tier.claim_nudge", FreeTierClaimNudgeParams, FreeTierClaimNudgeResult, "Claim the due sign-in offer; true for exactly one caller each time an offer comes due.")

var SharedMetricsConsentResult = pydantic.NewModel("SharedMetricsConsentResult", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Req("send", pydantic.Bool),
	pydantic.Req("decided", pydantic.Bool),
	pydantic.Def("reask", pydantic.Bool, false),
)

var _ = Method("shared_metrics.status", ProfileParams, SharedMetricsConsentResult, "Pure read of the focused profile's shared-metrics opt-ins (collection, upload, answered).")

var SharedMetricsSetParams = pydantic.Extend(ProfileParams, "SharedMetricsSetParams", ParamsConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("send", pydantic.Bool, false),
	pydantic.Def("first_run", pydantic.Bool, false),
)

var _ = Method("shared_metrics.set", SharedMetricsSetParams, SharedMetricsConsentResult, "Write both shared-metrics opt-ins at once (send requires collection) and reconcile consent windows.")

var SharedMetricsSlashCommandParams = pydantic.Extend(ProfileParams, "SharedMetricsSlashCommandParams", ParamsConfig,
	pydantic.Req("command", pydantic.Str),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
)

var SharedMetricsSlashCommandResult = pydantic.NewModel("SharedMetricsSlashCommandResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
)

var _ = Method("shared_metrics.slash_command", SharedMetricsSlashCommandParams, SharedMetricsSlashCommandResult, "Count one user-typed slash command (fire-and-forget; a no-op unless shared metrics are on).")

var SharedMetricsStartupLatencyParams = pydantic.Extend(ProfileParams, "SharedMetricsStartupLatencyParams", ParamsConfig,
	pydantic.Req("elapsed_ms", pydantic.Float),
	pydantic.Def("surface", pydantic.Nullable(pydantic.Literal("desktop_attach", "tui")), nil),
	pydantic.Def("launch_id", pydantic.Nullable(pydantic.Str), nil),
)

var SharedMetricsStartupLatencyResult = pydantic.NewModel("SharedMetricsStartupLatencyResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
)

var _ = Method("shared_metrics.startup_latency", SharedMetricsStartupLatencyParams, SharedMetricsStartupLatencyResult, "Record one client launch-to-ready latency (fire-and-forget; a no-op unless shared metrics are on).")

var SharedMetricsUpdateRunParams = pydantic.Extend(ProfileParams, "SharedMetricsUpdateRunParams", ParamsConfig,
	pydantic.Req("outcome", pydantic.Str),
	pydantic.Def("failed_stage", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mechanism", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("duration_ms", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("from_commit_date", pydantic.Nullable(pydantic.Float), nil),
)

var SharedMetricsUpdateRunResult = pydantic.NewModel("SharedMetricsUpdateRunResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
)

var _ = Method("shared_metrics.update_run", SharedMetricsUpdateRunParams, SharedMetricsUpdateRunResult, "Count one Desktop packaged self-update outcome (fire-and-forget; a no-op unless shared metrics are on).")

var SharedMetricsDesktopFeatureUseParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopFeatureUseParams", ParamsConfig,
	pydantic.Req("area", pydantic.Str),
)

var _ = Method("shared_metrics.desktop_feature_use", SharedMetricsDesktopFeatureUseParams, OkResult, "Count one Desktop area used today (fire-and-forget; once per area per UTC day; a no-op unless on).")

var SharedMetricsDesktopFrictionParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopFrictionParams", ParamsConfig,
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("detail", pydantic.Str),
)

var _ = Method("shared_metrics.desktop_friction", SharedMetricsDesktopFrictionParams, OkResult, "Count one Desktop friction event (fire-and-forget; capped per day; a no-op unless on).")

var SharedMetricsDesktopOnboardingParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopOnboardingParams", ParamsConfig,
	pydantic.Req("step", pydantic.Str),
	pydantic.Req("event", pydantic.Str),
)

var _ = Method("shared_metrics.desktop_onboarding", SharedMetricsDesktopOnboardingParams, OkResult, "Count one Desktop first-run step transition (fire-and-forget; once per step+event; a no-op unless on).")

var SharedMetricsDesktopDislikeParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopDislikeParams", ParamsConfig,
	pydantic.Req("signal", pydantic.Str),
	pydantic.Def("target", pydantic.Str, ""),
	pydantic.Def("setting", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("shared_metrics.desktop_dislike", SharedMetricsDesktopDislikeParams, OkResult, "Count one Desktop dislike signal (fire-and-forget; capped per signal per day; a no-op unless on).")

var SharedMetricsDesktopModeDay = pydantic.NewModel("SharedMetricsDesktopModeDay", ParamsConfig,
	pydantic.Req("mode", pydantic.Literal("bots", "sessions")),
	pydantic.Def("active_ms", pydantic.Float, 0),
	pydantic.Def("messages_sent", pydantic.Int, 0),
)

var SharedMetricsDesktopActionDay = pydantic.NewModel("SharedMetricsDesktopActionDay", ParamsConfig,
	pydantic.Req("action", pydantic.Str),
	pydantic.Req("via", pydantic.Literal("click", "menu", "palette", "shortcut")),
	pydantic.Req("count", pydantic.Int),
)

var SharedMetricsDesktopDailyParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopDailyParams", ParamsConfig,
	pydantic.Req("day", pydantic.Str),
	pydantic.Def("bot_count", pydantic.Int, 0),
	pydantic.Fac("modes", pydantic.List(SharedMetricsDesktopModeDay), pydantic.NewList),
	pydantic.Fac("actions", pydantic.List(SharedMetricsDesktopActionDay), pydantic.NewList),
)

var SharedMetricsDesktopDailyResult = pydantic.NewModel("SharedMetricsDesktopDailyResult", ClosedConfig,
	pydantic.Req("recorded", pydantic.Bool),
)

var _ = Method("shared_metrics.desktop_daily", SharedMetricsDesktopDailyParams, SharedMetricsDesktopDailyResult, "Record one finished Desktop day (mode use + button presses); recorded=false keeps it for a retry.")

var ModelOptionsParams = pydantic.Extend(ProfileParams, "ModelOptionsParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("explicit_only", pydantic.Bool, false),
	pydantic.Def("include_unconfigured", pydantic.Bool, false),
	pydantic.Def("refresh", pydantic.Bool, false),
)

var ModelPricing = pydantic.NewModel("ModelPricing", ClosedConfig,
	pydantic.Req("input", pydantic.Str),
	pydantic.Req("output", pydantic.Str),
	pydantic.Def("cache", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("free", pydantic.Bool),
	pydantic.Def("discount_percent", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("was_input", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("was_output", pydantic.Nullable(pydantic.Str), nil),
)

var ModelCapabilities = pydantic.NewModel("ModelCapabilities", ClosedConfig,
	pydantic.Req("fast", pydantic.Bool),
	pydantic.Def("ultrafast", pydantic.Bool, false),
	pydantic.Def("daybreak", pydantic.Bool, false),
	pydantic.Req("reasoning", pydantic.Bool),
	pydantic.Def("can_disable_reasoning", pydantic.Nullable(pydantic.Bool), nil),
)

var ProviderLimit = pydantic.NewModel("ProviderLimit", ClosedConfig,
	pydantic.Req("scope", pydantic.Literal("account", "models")),
	pydantic.Def("resets_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("models", pydantic.Nullable(pydantic.Dict(pydantic.Str)), nil),
)

var ProviderUsageWindow = pydantic.NewModel("ProviderUsageWindow", ClosedConfig,
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("used_percent", pydantic.Float),
	pydantic.Def("resets_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Literal("account", "model"), "account"),
)

var ProviderUsageAccount = pydantic.NewModel("ProviderUsageAccount", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("label", pydantic.Str, ""),
	pydantic.Fac("windows", pydantic.List(ProviderUsageWindow), pydantic.NewList),
	pydantic.Req("state", pydantic.Literal("ready", "limited", "unknown", "unavailable")),
	pydantic.Def("resets_at", pydantic.Nullable(pydantic.Str), nil),
)

var ProviderUsage = pydantic.NewModel("ProviderUsage", ClosedConfig,
	pydantic.Fac("windows", pydantic.List(ProviderUsageWindow), pydantic.NewList),
	pydantic.Def("accounts", pydantic.Nullable(pydantic.List(ProviderUsageAccount)), nil),
)

var ModelOptionProvider = pydantic.NewModel("ModelOptionProvider", OpenConfig,
	pydantic.Req("slug", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Fac("models", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("total_models", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("is_current", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("is_user_defined", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("aliases", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("api_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("auth_type", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("authenticated", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("key_env", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("warning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("featured_models", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("capabilities", pydantic.Nullable(pydantic.Dict(ModelCapabilities)), nil),
	pydantic.Def("pricing", pydantic.Nullable(pydantic.Dict(ModelPricing)), nil),
	pydantic.Def("pricing_pending", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("free_tier", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("free_tier_pending", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("free_tier_row", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("unavailable_models", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("plan_usage", pydantic.Nullable(PlanUsage), nil),
	pydantic.Def("usage_levels", pydantic.Nullable(pydantic.Dict(pydantic.Str)), nil),
	pydantic.Def("limit", pydantic.Nullable(ProviderLimit), nil),
	pydantic.Def("usage", pydantic.Nullable(ProviderUsage), nil),
)

var ModelOptionsResult = pydantic.NewModel("ModelOptionsResult", ClosedConfig,
	pydantic.Req("providers", pydantic.List(ModelOptionProvider)),
	pydantic.Def("model", pydantic.Str, ""),
	pydantic.Def("provider", pydantic.Str, ""),
)

var _ = Method("model.options", ModelOptionsParams, ModelOptionsResult, "Provider/model inventory for the picker, layered over the session's live provider when given.")

var ImageGenerateParams = pydantic.NewModel("ImageGenerateParams", ParamsConfig,
	pydantic.Def("prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("aspect_ratio", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("probe", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("max_bytes", pydantic.Nullable(pydantic.Int), nil),
)

var ImageGenerateResult = pydantic.NewModel("ImageGenerateResult", ClosedConfig,
	pydantic.Req("available", pydantic.Bool),
	pydantic.Def("success", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("image", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("image_data", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("image.generate", ImageGenerateParams, ImageGenerateResult, "Generate an image through the tool's provider dispatcher and hand the renderer a data URL.")

var GoalContractSnapshot = pydantic.NewModel("GoalContractSnapshot", ClosedConfig,
	pydantic.Def("outcome", pydantic.Str, ""),
	pydantic.Def("verification", pydantic.Str, ""),
	pydantic.Def("constraints", pydantic.Str, ""),
	pydantic.Def("boundaries", pydantic.Str, ""),
	pydantic.Def("stop_when", pydantic.Str, ""),
)

var GoalGateSnapshot = pydantic.NewModel("GoalGateSnapshot", ClosedConfig,
	pydantic.Req("command", pydantic.Str),
	pydantic.Req("timeout_seconds", pydantic.Int),
	pydantic.Req("max_retries", pydantic.Int),
	pydantic.Req("attempts", pydantic.Int),
	pydantic.Def("last_exit_code", pydantic.Nullable(pydantic.Int), nil),
)

var WaitBarrierUntil = pydantic.NewModel("WaitBarrierUntil", ClosedConfig,
	pydantic.Req("type", pydantic.Literal("until")),
	pydantic.Req("until_at", pydantic.Float),
	pydantic.Def("reason", pydantic.Str, ""),
)

var WaitBarrierTarget = pydantic.NewModel("WaitBarrierTarget", ClosedConfig,
	pydantic.Req("type", pydantic.Literal("session", "pid")),
	pydantic.Req("target", pydantic.Union(pydantic.Str, pydantic.Int)),
	pydantic.Def("reason", pydantic.Str, ""),
)

var GoalSnapshot = pydantic.NewModel("GoalSnapshot", ClosedConfig,
	pydantic.Req("title", pydantic.Str),
	pydantic.Req("status", pydantic.Str),
	pydantic.Req("turns_used", pydantic.Int),
	pydantic.Req("max_turns", pydantic.Int),
	pydantic.Req("contract", GoalContractSnapshot),
	pydantic.Req("subgoals", pydantic.List(pydantic.Str)),
	pydantic.Req("gates", pydantic.List(GoalGateSnapshot)),
	pydantic.Def("created_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("updated_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("paused_reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("last_verdict", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("last_reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("wait_barrier", pydantic.Nullable(pydantic.Tagged("type", WaitBarrierUntil, WaitBarrierTarget)), nil),
)

var LoopSnapshot = pydantic.NewModel("LoopSnapshot", ClosedConfig,
	pydantic.Req("prompt", pydantic.Str),
	pydantic.Req("status", pydantic.Str),
	pydantic.Req("mode", pydantic.Str),
	pydantic.Req("interval_seconds", pydantic.Float),
	pydantic.Req("current_delay", pydantic.Float),
	pydantic.Req("times", pydantic.Int),
	pydantic.Req("until", pydantic.Str),
	pydantic.Req("max_ticks", pydantic.Int),
	pydantic.Req("ticks_fired", pydantic.Int),
	pydantic.Req("created_at", pydantic.Float),
	pydantic.Req("last_fired_at", pydantic.Float),
	pydantic.Req("next_due_at", pydantic.Float),
	pydantic.Req("awaiting_response", pydantic.Bool),
	pydantic.Req("deferred_by_goal", pydantic.Bool),
	pydantic.Def("paused_reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("last_stop_reason", pydantic.Nullable(pydantic.Str), nil),
)

var HeartbeatSnapshot = pydantic.NewModel("HeartbeatSnapshot", ClosedConfig,
	pydantic.Req("prompt", pydantic.Str),
	pydantic.Req("status", pydantic.Str),
	pydantic.Req("interval_seconds", pydantic.Int),
	pydantic.Req("created_at", pydantic.Float),
	pydantic.Req("last_fired_at", pydantic.Float),
	pydantic.Req("fire_count", pydantic.Int),
)

var SessionControlSnapshot = pydantic.NewModel("SessionControlSnapshot", ClosedConfig,
	pydantic.Req("goal", pydantic.Nullable(GoalSnapshot)),
	pydantic.Req("loop", pydantic.Nullable(LoopSnapshot)),
	pydantic.Req("heartbeat", pydantic.Nullable(HeartbeatSnapshot)),
	pydantic.Req("revision", pydantic.Str),
	pydantic.Req("updated_at", pydantic.Float),
)

var SessionControlReadParams = pydantic.Extend(ProfileParams, "SessionControlReadParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
)

var SessionControlReadResult = pydantic.NewModel("SessionControlReadResult", ClosedConfig,
	pydantic.Req("control", SessionControlSnapshot),
)

var _ = Method("session.control.read", SessionControlReadParams, SessionControlReadResult, "Stable, allowlisted snapshot of one live session's goal / loop / heartbeat state.")

var SessionControlAction = pydantic.Enum("SessionControlAction", "goal_pause", "goal.pause", "goal_resume", "goal.resume", "goal_clear", "goal.clear", "goal_unwait", "goal.unwait", "loop_pause", "loop.pause", "loop_resume", "loop.resume", "loop_stop", "loop.stop", "subgoal_add", "subgoal.add", "subgoal_remove", "subgoal.remove", "subgoal_clear", "subgoal.clear", "heartbeat_pause", "heartbeat.pause", "heartbeat_resume", "heartbeat.resume", "heartbeat_clear", "heartbeat.clear")

var SessionControlArgs = pydantic.NewModel("SessionControlArgs", ParamsConfig,
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("index", pydantic.Nullable(pydantic.Int), nil),
)

var SessionControlParams = pydantic.Extend(ProfileParams, "SessionControlParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("action", pydantic.Str),
	pydantic.Def("args", pydantic.Nullable(SessionControlArgs), nil),
)

var SessionControlDispatch = pydantic.NewModel("SessionControlDispatch", ClosedConfig,
	pydantic.Req("type", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("output", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("notice", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("message", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("display", pydantic.Nullable(pydantic.Str)),
)

var SessionControlResult = pydantic.NewModel("SessionControlResult", ClosedConfig,
	pydantic.Req("control", SessionControlSnapshot),
	pydantic.Req("dispatch", SessionControlDispatch),
)

var _ = Method("session.control", SessionControlParams, SessionControlResult, "Run one allowlisted goal / loop / subgoal / heartbeat action and return the exact resulting snapshot.")

var VerificationStatusParams = pydantic.Extend(ProfileParams, "VerificationStatusParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
)

var VerificationEvidenceRow = pydantic.NewModel("VerificationEvidenceRow", OpenConfig,
	pydantic.Def("id", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("created_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("root", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("canonical_command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("kind", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("exit_code", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("output_summary", pydantic.Nullable(pydantic.Str), nil),
)

var VerificationStatusInfo = pydantic.NewModel("VerificationStatusInfo", ClosedConfig,
	pydantic.Req("status", pydantic.Str),
	pydantic.Def("evidence", pydantic.Nullable(VerificationEvidenceRow), nil),
	pydantic.Def("root", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("changed_paths", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
)

var VerificationStatusResult = pydantic.NewModel("VerificationStatusResult", ClosedConfig,
	pydantic.Req("verification", VerificationStatusInfo),
)

var _ = Method("verification.status", VerificationStatusParams, VerificationStatusResult, "Best known verification evidence for a cwd/session; read-only, never runs checks.")
