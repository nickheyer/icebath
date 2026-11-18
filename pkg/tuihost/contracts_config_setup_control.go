package tuihost

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
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("ok", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
).Described("``provider_configured`` is the loose answer. An unknown ``profile`` answers ``ok=False`` + ``error``.")




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
).Described("``hermes_cli/inventory.py::_apply_pricing`` — formatted $/Mtok strings (``\"\"`` unknown,\n``\"free\"``).")

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
	pydantic.Def("limit", pydantic.Nullable(ProviderLimit), nil),
	pydantic.Def("usage", pydantic.Nullable(ProviderUsage), nil),
).Described("One ``hermes_cli/inventory.py::build_models_payload`` provider row (the union of every field\nthe builder sets; ``pricing_pending`` marks the cached-only path).")

var ModelOptionsResult = pydantic.NewModel("ModelOptionsResult", ClosedConfig,
	pydantic.Req("providers", pydantic.List(ModelOptionProvider)),
	pydantic.Def("model", pydantic.Str, ""),
	pydantic.Def("provider", pydantic.Str, ""),
)

var _ = Method("model.options", ModelOptionsParams, ModelOptionsResult, "Provider/model inventory for the picker, layered over the session's live provider when given.")















