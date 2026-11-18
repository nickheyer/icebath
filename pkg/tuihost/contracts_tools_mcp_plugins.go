package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var sessionScoped = pydantic.NewModel("_SessionScoped", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
).Described("Handlers that look a live session up with ``_sessions.get(params.get(\"session_id\"))``: an\nabsent / unknown id falls back to the launch profile's config, so it is never required.")

var ToolsetRow = pydantic.NewModel("ToolsetRow", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
	pydantic.Req("tool_count", pydantic.Int),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("tools", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("One row of ``methods_tools._toolset_rows``; ``tools`` only when the caller asked for them\n(``tools.list``).")

var ToolsAction = pydantic.Enum("ToolsAction", "enable", "enable", "disable", "disable")

var ReloadEnvParams = pydantic.NewModel("ReloadEnvParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("The shared owner serves ``/reload`` for one session the caller controls (``session_id``).")

var ReloadEnvResult = pydantic.NewModel("ReloadEnvResult", ClosedConfig,
	pydantic.Req("updated", pydantic.Int),
)

var _ = Method("reload.env", ReloadEnvParams, ReloadEnvResult, "Re-read ~/.icebath/.env (CLI /reload parity); built agents keep their pool until /new.")

var SkillsAction = pydantic.Enum("SkillsAction", "list", "list", "search", "search", "install", "install", "browse", "browse", "inspect", "inspect")

var SkillsManageParams = pydantic.Extend(ProfileParams, "SkillsManageParams", ParamsConfig,
	pydantic.Def("action", SkillsAction, "list"),
	pydantic.Def("query", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("page", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("page_size", pydantic.Nullable(pydantic.Int), nil),
).Described("``query`` is the search text / hub identifier / browse page (digits); ``page`` / ``page_size``\napply to ``browse``.")

var SkillHubHit = pydantic.NewModel("SkillHubHit", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
)

var SkillBrowseItem = pydantic.NewModel("SkillBrowseItem", OpenConfig,
	pydantic.Def("name", pydantic.Str, ""),
	pydantic.Def("description", pydantic.Str, ""),
	pydantic.Def("source", pydantic.Str, ""),
	pydantic.Def("trust", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("identifier", pydantic.Nullable(pydantic.Str), nil),
).Described("``hermes_cli.skills_hub.browse_skills`` row.")

var SkillInspectInfo = pydantic.NewModel("SkillInspectInfo", OpenConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("identifier", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tags", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("skill_md_preview", pydantic.Nullable(pydantic.Str), nil),
).Described("``hermes_cli.skills_hub.inspect_skill``; ``{}`` when the identifier resolves nowhere.")

var SkillsManageResult = pydantic.NewModel("SkillsManageResult", ClosedConfig,
	pydantic.Def("skills", pydantic.Nullable(pydantic.Dict(pydantic.List(pydantic.Str))), nil),
	pydantic.Def("results", pydantic.Nullable(pydantic.List(SkillHubHit)), nil),
	pydantic.Def("installed", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("items", pydantic.Nullable(pydantic.List(SkillBrowseItem)), nil),
	pydantic.Def("page", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("total_pages", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("total", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("info", pydantic.Nullable(SkillInspectInfo), nil),
).Described("Shape follows the action: ``list`` → ``skills`` (category → names); ``search`` → ``results``;\n``install`` → ``installed`` + ``name``; ``browse`` → ``items`` + paging; ``inspect`` → ``info``.")

var _ = Method("skills.manage", SkillsManageParams, SkillsManageResult, "Skills hub backend: list the profile's skills or search / browse / inspect / install from the hub.")

var SkillsReloadParams = pydantic.NewModel("SkillsReloadParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``session_id`` binds the rescan to that session's profile and workspace (project skills);\n``profile`` scopes a session-less rescan.")

var SkillCommandRef = pydantic.NewModel("SkillCommandRef", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("description", pydantic.Str, ""),
)

var SkillsReloadDiff = pydantic.NewModel("SkillsReloadDiff", OpenConfig,
	pydantic.Fac("added", pydantic.List(SkillCommandRef), pydantic.NewList),
	pydantic.Fac("removed", pydantic.List(SkillCommandRef), pydantic.NewList),
	pydantic.Fac("unchanged", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("total", pydantic.Int, 0),
	pydantic.Def("commands", pydantic.Int, 0),
).Described("``agent.skill_commands.reload_skills``.")

var SkillsReloadResult = pydantic.NewModel("SkillsReloadResult", ClosedConfig,
	pydantic.Req("output", pydantic.Str),
	pydantic.Req("result", SkillsReloadDiff),
)

var _ = Method("skills.reload", SkillsReloadParams, SkillsReloadResult, "Re-scan skill dirs; the pre-rendered ``output`` is what /reload-skills prints.")

var PluginsListParams = pydantic.NewModel("PluginsListParams", ParamsConfig)

var LegacyPluginRow = pydantic.NewModel("LegacyPluginRow", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("version", pydantic.Str),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tools", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("hooks", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("commands", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
)

var PluginToolsetRow = pydantic.NewModel("PluginToolsetRow", ClosedConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
)

var PluginsListResult = pydantic.NewModel("PluginsListResult", ClosedConfig,
	pydantic.Req("plugins", pydantic.List(LegacyPluginRow)),
	pydantic.Def("toolsets", pydantic.Nullable(pydantic.List(PluginToolsetRow)), nil),
	pydantic.Def("portable_mcp_servers", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
)

var _ = Method("plugins.list", PluginsListParams, PluginsListResult, "Loaded plugin manager entries (legacy flat view); the Plugins Hub uses plugins.manage list.")

var PluginsAction = pydantic.Enum("PluginsAction", "list", "list", "toggle", "toggle", "install", "install", "inspect", "inspect", "update", "update", "remove", "remove", "settings", "settings", "presence", "presence")

var PluginsManageParams = pydantic.Extend(ProfileParams, "PluginsManageParams", ParamsConfig,
	pydantic.Def("action", PluginsAction, "list"),
	pydantic.Def("key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("enable", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("identifier", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("repo", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("catalog_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("force", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("ref", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("inspected_sha", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("accept_capabilities", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("values", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("names", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("``toggle``: ``key``/``name`` + ``enable``; ``install``: ``identifier``/``repo`` or ``catalog_name``\n(+ ``force``, ``enable``, ``ref``, ``inspected_sha`` = the commit ``inspect`` read: a custom source with no\n``ref`` whose branch moved since is refused); ``inspect``: ``identifier``/``repo`` or ``catalog_name``;\n``update``: ``name`` (+ ``accept_capabilities`` to apply a re-pin\nthat widened the plugin after the user confirmed the ``delta``, + ``force`` to apply an update whose\nsecurity scan returned caution after the user saw the findings); ``remove``: ``name`` (user installs only);\n``settings``: ``key`` + ``values`` (``{setting_key: value}``, non-secret schema keys only);\n``presence``: ``names`` (catalog names, answered in that order).")

var PluginSettingFieldType = pydantic.Enum("PluginSettingFieldType", "string", "string", "number", "number", "boolean", "boolean", "enum", "enum", "secret", "secret", "json", "json")

var PluginSettingField = pydantic.NewModel("PluginSettingField", ClosedConfig,
	pydantic.Req("key", pydantic.Str),
	pydantic.Req("type", PluginSettingFieldType),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
	pydantic.Req("required", pydantic.Bool),
	pydantic.Def("value", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("default", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("choices", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("env", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("has_value", pydantic.Nullable(pydantic.Bool), nil),
).Described("One ``config_schema`` key of a plugin manifest, rendered by the Plugins hub\n(``hermes_cli.plugins_settings.plugin_settings_fields``). ``secret`` fields carry no value: ``env``\nnames the ``.env`` variable and ``has_value`` whether it is set.")

var PluginServerState = pydantic.Enum("PluginServerState", "connected", "connected", "app_not_running", "app_not_running", "icebath_not_connected", "icebath_not_connected", "endpoint_unavailable", "endpoint_unavailable", "no_interactive_session", "no_interactive_session", "version_too_old", "version_too_old", "missing_app", "missing_app", "unsupported_gpu", "unsupported_gpu", "unknown", "unknown")

var PluginServerRow = pydantic.NewModel("PluginServerRow", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("state", PluginServerState),
	pydantic.Req("sentence", pydantic.Str),
)

var AgentPluginRow = pydantic.NewModel("AgentPluginRow", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("key", pydantic.Str),
	pydantic.Req("version", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
	pydantic.Req("source", pydantic.Str),
	pydantic.Req("status", pydantic.Str),
	pydantic.Req("portable", pydantic.Bool),
	pydantic.Req("install_dir", pydantic.Str),
	pydantic.Req("servers", pydantic.List(PluginServerRow)),
	pydantic.Def("catalog_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("catalog_tier", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("installed_sha", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("catalog_sha", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("catalog_version", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("update_available", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("update_target", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pinned_sha", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("settings_schema", pydantic.Nullable(pydantic.List(PluginSettingField)), nil),
	pydantic.Fac("tools", pydantic.List(pydantic.Str), pydantic.NewList),
).Described("``methods_tools._plugin_rows`` + ``plugins_cmd_catalog.catalog_row_fields`` provenance.")

var PluginLiveServer = pydantic.NewModel("PluginLiveServer", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("connected", pydantic.Bool),
	pydantic.Fac("tools", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
).Described("One plugin MCP server connected at activation: its callable tool names, or the reason it did not connect.")

var PluginLiveSkill = pydantic.NewModel("PluginLiveSkill", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("description", pydantic.Str, ""),
).Described("One plugin skill usable now through ``skill_view`` (qualified ``<plugin>:<skill>``).")

var PluginLiveNow = pydantic.NewModel("PluginLiveNow", ClosedConfig,
	pydantic.Fac("mcp_servers", pydantic.List(PluginLiveServer), pydantic.NewList),
	pydantic.Fac("skills", pydantic.List(PluginLiveSkill), pydantic.NewList),
)

var PluginRestartTarget = pydantic.Enum("PluginRestartTarget", "gateway", "gateway", "backend", "backend")

var PluginActivation = pydantic.NewModel("PluginActivation", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("key", pydantic.Str),
	pydantic.Fac("activated_now", pydantic.Dict(pydantic.List(pydantic.Str)), pydantic.NewDict),
	pydantic.Def("live_now", pydantic.Nullable(PluginLiveNow), nil),
	pydantic.Fac("deferred", pydantic.Dict(pydantic.List(pydantic.Str)), pydantic.NewDict),
	pydantic.Fac("missing_dependencies", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("missing_in", pydantic.List(PluginRestartTarget), pydantic.NewList),
).Described("What a plugin loaded mid-run does NOW vs later (``hermes_cli.plugins_activation``). ``activated_now``\nkinds (``{kind: [names]}``): ``gateway_commands`` (slash names), ``locales`` (``<lang>.<surface>``\nlanguage-pack layers), ``gateway_transforms`` / ``hooks`` (hook\nnames), ``callbacks`` (platforms / ``slack:<action_id>``) — live in the running gateway once it reloaded\n(``gateway_reloaded``). ``live_now``: the plugin's MCP servers (connected, with their tools, or the\nerror) and skills, usable in every open chat of the profile from its next turn — the chats also get a\nnote listing them. ``deferred`` kinds: ``tools`` (Python tool names) and ``prompt`` (section ids)\napply from the next session. ``missing_dependencies``: the plugin's declared Python requirements a\nrunning process cannot import (installed after it booted, or never); ``missing_in`` names which\nprocesses (``gateway``, ``backend`` = the session server process) — only restarting them loads them.")

var CatalogAppState = pydantic.Enum("CatalogAppState", "present", "present", "missing_app", "missing_app", "app_not_running", "app_not_running", "unknown", "unknown").Described("Whether the native app a catalog plugin drives is installed and running, from the platform its plugin.json declares.")

var CatalogPluginPresence = pydantic.NewModel("CatalogPluginPresence", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("title", pydantic.Str),
	pydantic.Req("state", CatalogAppState),
	pydantic.Req("sentence", pydantic.Str),
	pydantic.Req("disclosure", pydantic.Str),
).Described("A named catalog plugin this OS runs. ``state`` is the pinned ``plugin.json`` app declaration judged\non this host; ``sentence`` names what is missing (empty when present or unknown); ``disclosure`` is the\ncatalog description's ``Disclosure:`` sentence (empty when it has none).")

var PluginScanFinding = pydantic.NewModel("PluginScanFinding", ClosedConfig,
	pydantic.Req("pattern_id", pydantic.Str),
	pydantic.Req("severity", pydantic.Str),
	pydantic.Req("category", pydantic.Str),
	pydantic.Req("file", pydantic.Str),
	pydantic.Req("line", pydantic.Int),
	pydantic.Req("description", pydantic.Str),
).Described("One security-scan finding (``tools.plugin_guard``) on a plugin tree the scan blocked.")

var PluginsManageResult = pydantic.NewModel("PluginsManageResult", ClosedConfig,
	pydantic.Def("plugins", pydantic.Nullable(pydantic.List(AgentPluginRow)), nil),
	pydantic.Def("user_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("bundled_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("ok", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("unchanged", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("restart_required", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("gateway_reloaded", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("activation", pydantic.Nullable(PluginActivation), nil),
	pydantic.Def("cleared_memory_provider", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("plugin", pydantic.Nullable(AgentPluginRow), nil),
	pydantic.Def("plugin_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("warnings", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("missing_env", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("python_dependencies", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("known_issues", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("after_install_path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("enabled", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("sha", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("consent_required", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("delta", pydantic.Nullable(pydantic.Dict(pydantic.List(pydantic.Str))), nil),
	pydantic.Def("delta_lines", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("scan_blocked", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("scan_verdict", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scan_findings", pydantic.Nullable(pydantic.List(PluginScanFinding)), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("written", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("presence", pydantic.Nullable(pydantic.List(CatalogPluginPresence)), nil),
	pydantic.Def("agent", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("agent_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("insecure", pydantic.Nullable(pydantic.Bool), nil),
).Described("``list`` → ``plugins`` + counts; ``toggle`` → ``ok``/``unchanged``/``restart_required``/``name``\n(the canonical key written)/``plugin``; ``install`` → ``hermes_cli.plugins_cmd.unattended_install_plugin``'s\nok payload; ``inspect`` → ``ok``/``agent``/``agent_name``/``sha`` (the commit\nread)/``warnings``/``insecure`` or ``ok=false`` + ``error``; ``toggle``/``install``/``update`` that loaded a plugin also carry ``gateway_reloaded`` (the\nrunning gateway picked it up and re-wired its handlers) and ``activation`` — the honest split of what is\nlive now vs deferred, so ``restart_required`` is True only when no gateway answered or the running\ngateway/backend cannot import the plugin's Python dependencies (``activation.missing_dependencies``); ``update`` → ``ok``/``unchanged``/``sha``, or ``ok=false`` + ``consent_required`` with the\n``delta`` (``{surface: [added...]}``) / ``delta_lines`` a widened pin adds — nothing changed until the\nclient retries with ``accept_capabilities`` — or ``ok=false`` + ``scan_blocked`` with ``scan_verdict`` /\n``scan_findings`` (retry a caution verdict with ``force``); ``remove`` → ``ok``/``name`` plus\n``cleared_memory_provider`` when the removed plugin was the live ``memory.provider``.")

var _ = Method("plugins.manage", PluginsManageParams, PluginsManageResult, "Plugins Hub backend: list installed plugins, toggle, git-install, re-pin a catalog install, or remove a user install.")
