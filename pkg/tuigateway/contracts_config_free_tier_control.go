package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ConfigGetParams = pydantic.Extend(ProfileParams, "ConfigGetParams", ParamsConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``key`` selects one getter from ``_CONFIG_GETTERS``; ``cwd`` feeds the ``project`` getter,\n``session_id`` lets ``reasoning`` / ``fast`` answer with the session's live pin.")

var ConfigProviderRef = pydantic.NewModel("ConfigProviderRef", OpenConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Fac("aliases", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("authenticated", pydantic.Bool, false),
).Described("``hermes_cli/models.py::list_available_providers`` row.")

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
).Described("Union of every getter's payload: ``value`` for the simple words, ``config`` for ``full``,\n``mtime`` / ``mcp_rev`` for the poller, ``model`` / ``provider`` / ``providers`` for ``provider``,\n``home`` / ``display`` for ``profile``, ``cwd`` / ``branch`` for ``project``, ``prompt``.")

var _ = Method("config.get", ConfigGetParams, ConfigGetResult, "Read one normalised config value (or the whole effective config) the way the UIs render it.")

var ConfigSetParams = pydantic.Extend(ProfileParams, "ConfigSetParams", ParamsConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("value", pydantic.Any, ""),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("confirm_expensive_model", pydantic.Bool, false),
).Described("``key`` picks the setter (``_CONFIG_SETTERS``, ``details_mode.<section>``, display toggles);\n``value`` is the raw word/string the setter normalises (falsy non-strings are reported back in\nthe error). ``scope`` applies to ``yolo`` / ``reasoning``; ``confirm_expensive_model`` to ``model``.")

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
).Described("``{key, value}`` plus the setter's extras: model switches add ``warning`` /\n``confirm_required`` / ``confirm_message`` / ``scope`` / ``deferred``; ``focus`` adds\n``tool_progress``; ``cwd`` adds ``cwd`` / ``branch``; ``personality`` adds ``history_reset`` /\n``info``; ``yolo`` reports its ``scope``. ``value`` is a bool only for the display toggles.")

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
).Described("``provider_configured`` is the loose answer; the boot record's fields (``ready``,\n``free_tier_account``, ``free_tier_route``, ``other_providers``, ``inference_provider``) ride along\non the launch profile. An unknown ``profile`` answers ``ok=False`` + ``error``.")

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
).Described("``ok=False`` + ``error`` when the resolved model can't be served; ``free_tier_route`` says the\nselected route is the welcome host.")

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
).Described("Structured envelope: ``ok=False`` + ``error`` renders inline instead of failing the RPC.")

var _ = Method("diagnostics.share_nous", DiagnosticsShareNousParams, DiagnosticsShareNousResult, "Upload a force-redacted debug bundle to Nous-internal diagnostics storage.")

var FreeTierChallengePayload = pydantic.NewModel("FreeTierChallengePayload", OpenConfig,
	pydantic.Req("type", pydantic.Literal("browser")),
	pydantic.Req("url", pydantic.Str),
	pydantic.Req("required", pydantic.Bool),
	pydantic.Req("expires_in", pydantic.Int),
	pydantic.Req("message", pydantic.Str),
	pydantic.Def("attempt", pydantic.Int, 0),
).Described("``hermes_cli/anon_challenge.py::BrowserChallenge.as_payload``: the ``free_tier.challenge``\nevent, and ``free_tier.status``'s ``challenge`` field for a client that connected after it.")

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
).Described("``available`` = an identity exists AND the tier is on; whether inference runs on it is\n``setup.runtime_check.free_tier_route``'s question.")

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
).Described("The focused profile's ``telemetry.shared_metrics`` opt-ins. ``send`` is never true while\n``enabled`` is false; ``decided`` = either key is written in config.yaml (the shipped defaults\nare not an answer) and it is not a ``reask``: an \"off\" from before the type-ahead fix, offered\nonce more with the reason.")

var _ = Method("shared_metrics.status", ProfileParams, SharedMetricsConsentResult, "Pure read of the focused profile's shared-metrics opt-ins (collection, upload, answered).")

var SharedMetricsSetParams = pydantic.Extend(ProfileParams, "SharedMetricsSetParams", ParamsConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("send", pydantic.Bool, false),
	pydantic.Def("first_run", pydantic.Bool, false),
).Described("``send`` is ignored unless ``enabled``; ``first_run`` marks the Desktop first-run answer.")

var _ = Method("shared_metrics.set", SharedMetricsSetParams, SharedMetricsConsentResult, "Write both shared-metrics opt-ins at once (send requires collection) and reconcile consent windows.")

var SharedMetricsSlashCommandParams = pydantic.Extend(ProfileParams, "SharedMetricsSlashCommandParams", ParamsConfig,
	pydantic.Req("command", pydantic.Str),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``command`` is the raw typed name (no leading ``/``, no args); the backend canonicalizes it\nagainst the published registry. ``session_id`` scopes the count to that session's profile.")

var SharedMetricsSlashCommandResult = pydantic.NewModel("SharedMetricsSlashCommandResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
)

var _ = Method("shared_metrics.slash_command", SharedMetricsSlashCommandParams, SharedMetricsSlashCommandResult, "Count one user-typed slash command (fire-and-forget; a no-op unless shared metrics are on).")

var SharedMetricsStartupLatencyParams = pydantic.Extend(ProfileParams, "SharedMetricsStartupLatencyParams", ParamsConfig,
	pydantic.Req("elapsed_ms", pydantic.Float),
	pydantic.Def("surface", pydantic.Nullable(pydantic.Literal("desktop_attach", "tui")), nil),
	pydantic.Def("launch_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``elapsed_ms`` = the client's own launch (TUI process start / Desktop app start) to ready\n(TUI gateway ready / Desktop backend attached), measured once per launch by the client. The\nclient names its surface because a Desktop may attach to a URL/cloud backend where\n``ICEBATH_DESKTOP`` is unset; without it the backend falls back to its own client detection.\n``launch_id`` is an opaque per-launch token the backend latches on (never recorded), so a\nreconnect re-sending the same launch counts once while a new launch counts again.")

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
).Described("One Desktop PACKAGED self-update (electron-updater / App Installer / Store). Source-checkout\nhand-offs run ``icebath update`` and are counted from its receipt, never here. Raw words; the\nbackend buckets them: ``outcome`` success|failed|noop|refused, ``failed_stage``\ndownload|verify|apply|restart, ``mechanism`` the updater strategy kind, ``duration_ms`` wall\ntime, ``from_commit_date`` the updated-from build's commit time (epoch seconds) when known.")

var SharedMetricsUpdateRunResult = pydantic.NewModel("SharedMetricsUpdateRunResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
)

var _ = Method("shared_metrics.update_run", SharedMetricsUpdateRunParams, SharedMetricsUpdateRunResult, "Count one Desktop packaged self-update outcome (fire-and-forget; a no-op unless shared metrics are on).")

var SharedMetricsDesktopFeatureUseParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopFeatureUseParams", ParamsConfig,
	pydantic.Req("area", pydantic.Str),
).Described("``area`` is a Desktop surface id (``command_palette``, ``terminal_pane``, ``settings_<view>`` …);\nthe backend collapses anything outside its closed set to ``other``.")

var _ = Method("shared_metrics.desktop_feature_use", SharedMetricsDesktopFeatureUseParams, OkResult, "Count one Desktop area used today (fire-and-forget; once per area per UTC day; a no-op unless on).")

var SharedMetricsDesktopFrictionParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopFrictionParams", ParamsConfig,
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("detail", pydantic.Str),
).Described("``kind`` notice_dismissed|error_toast|renderer_crash|backend_disconnect|slow_frame; ``detail`` a\nclosed code-defined word for that kind (notice id, error category, crash reason, drop reason, frame\nduration bucket), never message text.")

var _ = Method("shared_metrics.desktop_friction", SharedMetricsDesktopFrictionParams, OkResult, "Count one Desktop friction event (fire-and-forget; capped per day; a no-op unless on).")

var SharedMetricsDesktopOnboardingParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopOnboardingParams", ParamsConfig,
	pydantic.Req("step", pydantic.Str),
	pydantic.Req("event", pydantic.Str),
).Described("``step`` a Desktop first-run step id; ``event`` reached|completed|abandoned.")

var _ = Method("shared_metrics.desktop_onboarding", SharedMetricsDesktopOnboardingParams, OkResult, "Count one Desktop first-run step transition (fire-and-forget; once per step+event; a no-op unless on).")

var SharedMetricsDesktopDislikeParams = pydantic.Extend(ProfileParams, "SharedMetricsDesktopDislikeParams", ParamsConfig,
	pydantic.Req("signal", pydantic.Str),
	pydantic.Def("target", pydantic.Str, ""),
	pydantic.Def("setting", pydantic.Nullable(pydantic.Str), nil),
).Described("``signal`` quick_close|cancelled|setting_off_default|rage_click|undo|feature_disabled; ``target`` a\nclosed code-defined id for that signal (area, flow, action, undo path, feature toggle); ``setting`` a\nconfig key for setting_off_default only (the value is never sent — the backend compares it to the\ndefault).")

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
).Described("One finished UTC day of Desktop use, aggregated on the client. ``day`` (YYYY-MM-DD) only latches\na resend and is never recorded; the raw counts are bucketed by the backend.")

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
).Described("``hermes_cli/inventory.py::_apply_pricing`` — formatted $/Mtok strings (``\"\"`` unknown,\n``\"free\"``); the sale fields are Nous Portal-only.")

var ModelCapabilities = pydantic.NewModel("ModelCapabilities", ClosedConfig,
	pydantic.Req("fast", pydantic.Bool),
	pydantic.Def("ultrafast", pydantic.Bool, false),
	pydantic.Def("daybreak", pydantic.Bool, false),
	pydantic.Req("reasoning", pydantic.Bool),
	pydantic.Def("can_disable_reasoning", pydantic.Nullable(pydantic.Bool), nil),
).Described("``hermes_cli/inventory.py::_apply_capabilities``.")

var ProviderLimit = pydantic.NewModel("ProviderLimit", ClosedConfig,
	pydantic.Req("scope", pydantic.Literal("account", "models")),
	pydantic.Def("resets_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("models", pydantic.Nullable(pydantic.Dict(pydantic.Str)), nil),
).Described("``hermes_cli/inventory.py::_apply_limits`` — ``account``: the whole login is rate-limited until\n``resets_at`` (ISO, absent when unknown); ``models``: only these models are, each until its time.")

var ProviderUsageWindow = pydantic.NewModel("ProviderUsageWindow", ClosedConfig,
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("used_percent", pydantic.Float),
	pydantic.Def("resets_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Literal("account", "model"), "account"),
).Described("One subscription usage window (``agent/account_usage.py::AccountUsageWindow``): e.g. the 5-hour\nsession or the weekly cap, with how much of it is spent and when it rolls over (ISO).\n\n``scope``: ``account`` — exhausting the window exhausts the whole login (Codex session/weekly,\nso a limited account's resets_at must wait for it); ``model`` — the window caps only one model\nfamily (Anthropic Opus/Sonnet weekly) and can never imply the account itself is out of quota.")

var ProviderUsageAccount = pydantic.NewModel("ProviderUsageAccount", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("label", pydantic.Str, ""),
	pydantic.Fac("windows", pydantic.List(ProviderUsageWindow), pydantic.NewList),
	pydantic.Req("state", pydantic.Literal("ready", "limited", "unknown", "unavailable")),
	pydantic.Def("resets_at", pydantic.Nullable(pydantic.Str), nil),
).Described("One account of a provider's credential pool (``hermes_cli/inventory.py::_pool_usage_accounts``).\n``id`` is a stable non-secret account identity (never a key or URL); ``label`` may be empty (UI\nfalls back to a localized \"Account N\"). ``windows`` is empty while the account's usage is not\nyet known — state carries the meaning, never a fabricated gauge.\n\n``state``: ``ready`` — live quota below the cap (numeric windows present); ``limited`` — a live\ncredential-wide cooldown or exhausted account-scoped quota windows; ``unknown`` — no live\nnumeric windows (failed/empty fetch, stale snapshot, provider without a usage API);\n``unavailable`` — DEAD auth row (kept visible, never a quota row).\n\n``resets_at``: for a limited account, the LATEST of its exhausted account-scoped windows (or a\nlive cooldown when later); ``None`` when unknown (the frontend renders its own advisory, e.g.\nthe earliest limited sibling).")

var ProviderUsage = pydantic.NewModel("ProviderUsage", ClosedConfig,
	pydantic.Fac("windows", pydantic.List(ProviderUsageWindow), pydantic.NewList),
	pydantic.Def("accounts", pydantic.Nullable(pydantic.List(ProviderUsageAccount)), nil),
).Described("``hermes_cli/inventory.py::_apply_usage`` — the provider's subscription usage, from cache.\n\nMulti-entry credential pools carry ``accounts`` (one row per account; the legacy ``windows``\nstays EMPTY there — a provider-wide percentage across different logins would be fabricated).\nSingle-account providers keep the legacy ``windows`` gauge.")

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
).Described("One ``hermes_cli/inventory.py::build_models_payload`` provider row (the union of every field\nthe builder sets; ``pricing_pending`` / ``free_tier_pending`` mark the cached-only path).")

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
).Described("``probe`` answers ``{available}`` alone; ``image_data`` (data URL) is omitted when the\ndownload failed or exceeded ``max_bytes`` so callers fall back to ``image``.")

var _ = Method("image.generate", ImageGenerateParams, ImageGenerateResult, "Generate an image through the tool's provider dispatcher and hand the renderer a data URL.")

var GoalContractSnapshot = pydantic.NewModel("GoalContractSnapshot", ClosedConfig,
	pydantic.Def("outcome", pydantic.Str, ""),
	pydantic.Def("verification", pydantic.Str, ""),
	pydantic.Def("constraints", pydantic.Str, ""),
	pydantic.Def("boundaries", pydantic.Str, ""),
	pydantic.Def("stop_when", pydantic.Str, ""),
).Described("``hermes_cli/goals.py::GoalContract.to_dict``.")

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
).Described("``methods_session_control.py::_safe_goal_snapshot`` — the frontend-safe GoalState subset.")

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
).Described("``_safe_loop_snapshot`` — persisted LoopState fields, never its route.")

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
).Described("``_snapshot_control`` — ``revision`` is a hash of the visible state (``\"\"`` when empty);\n``updated_at`` is the newest persisted timestamp (``0`` when none).")

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
).Described("``subgoal.add`` reads ``text``; ``subgoal.remove`` reads the 1-based ``index``.")

var SessionControlParams = pydantic.Extend(ProfileParams, "SessionControlParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("action", pydantic.Str),
	pydantic.Def("args", pydantic.Nullable(SessionControlArgs), nil),
).Described("``action`` is validated by the handler (unknown / gate actions answer ``4004``), so it stays a\nstring on the wire; ``SessionControlAction`` lists the accepted set.")

var SessionControlDispatch = pydantic.NewModel("SessionControlDispatch", ClosedConfig,
	pydantic.Req("type", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("output", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("notice", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("message", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("display", pydantic.Nullable(pydantic.Str)),
).Described("``_dispatch_envelope`` — the command result's user-visible envelope, every key always present.")

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
).Described("One ``verification_events`` row (``agent/verification_evidence.py``).")

var VerificationStatusInfo = pydantic.NewModel("VerificationStatusInfo", ClosedConfig,
	pydantic.Req("status", pydantic.Str),
	pydantic.Def("evidence", pydantic.Nullable(VerificationEvidenceRow), nil),
	pydantic.Def("root", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("changed_paths", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("``verification_status()``: ``disabled`` / ``not_applicable`` / ``unverified`` / ``stale`` or the\nlatest event's own status; ``root`` and friends only once a workspace was identified.")

var VerificationStatusResult = pydantic.NewModel("VerificationStatusResult", ClosedConfig,
	pydantic.Req("verification", VerificationStatusInfo),
)

var _ = Method("verification.status", VerificationStatusParams, VerificationStatusResult, "Best known verification evidence for a cwd/session; read-only, never runs checks.")
