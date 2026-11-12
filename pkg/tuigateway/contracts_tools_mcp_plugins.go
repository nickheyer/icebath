package tuigateway

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

var ToolsetsListResult = pydantic.NewModel("ToolsetsListResult", ClosedConfig,
	pydantic.Req("toolsets", pydantic.List(ToolsetRow)),
)

var _ = Method("tools.list", sessionScoped, ToolsetsListResult, "Every toolset with its resolved tool names, flagged against the session's (or config's) enabled set.")

var _ = Method("toolsets.list", sessionScoped, ToolsetsListResult, "Toolset summaries (no tool names) for the desktop Toolsets tab.")

var ToolShowRow = pydantic.NewModel("ToolShowRow", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
)

var ToolShowSection = pydantic.NewModel("ToolShowSection", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("tools", pydantic.List(ToolShowRow)),
)

var ToolsShowResult = pydantic.NewModel("ToolsShowResult", ClosedConfig,
	pydantic.Req("sections", pydantic.List(ToolShowSection)),
	pydantic.Req("total", pydantic.Int),
)

var _ = Method("tools.show", sessionScoped, ToolsShowResult, "The /tools listing grouped by toolset, including tools deferred behind the tool_search bridge.")

var ToolsAction = pydantic.Enum("ToolsAction", "enable", "enable", "disable", "disable")

var ToolsConfigureParams = pydantic.NewModel("ToolsConfigureParams", ParamsConfig,
	pydantic.Req("action", ToolsAction),
	pydantic.Req("names", pydantic.List(pydantic.Str)),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``names`` are toolset keys or ``server:tool`` MCP targets; with ``session_id`` the live session's\nprofile is authoritative and its agent is rebuilt.")

var ToolsConfigureResult = pydantic.NewModel("ToolsConfigureResult", ClosedConfig,
	pydantic.Req("changed", pydantic.List(pydantic.Str)),
	pydantic.Req("enabled_toolsets", pydantic.List(pydantic.Str)),
	pydantic.Def("info", pydantic.Nullable(SessionLiveInfo), nil),
	pydantic.Req("missing_servers", pydantic.List(pydantic.Str)),
	pydantic.Req("reset", pydantic.Bool),
	pydantic.Req("unknown", pydantic.List(pydantic.Str)),
)

var _ = Method("tools.configure", ToolsConfigureParams, ToolsConfigureResult, "Persist a toolset / MCP enable-disable change and rebuild the session agent so it takes effect now.")

var ReloadEnvParams = pydantic.NewModel("ReloadEnvParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("The shared owner serves ``/reload`` for one session the caller controls (``session_id``).")

var ReloadEnvResult = pydantic.NewModel("ReloadEnvResult", ClosedConfig,
	pydantic.Req("updated", pydantic.Int),
)

var _ = Method("reload.env", ReloadEnvParams, ReloadEnvResult, "Re-read ~/.icebath/.env (CLI /reload parity); built agents keep their pool until /new.")

var ReloadMcpParams = pydantic.NewModel("ReloadMcpParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("confirm", pydantic.Bool, false),
	pydantic.Def("always", pydantic.Bool, false),
	pydantic.Def("rev", pydantic.Nullable(pydantic.Str), nil),
).Described("Without ``confirm`` the handler may answer ``confirm_required`` (per ``approvals.mcp_reload_confirm``);\n``always`` persists the opt-out; ``rev`` is the config revision the caller wants loaded (coalescing).")

var ReloadMcpStatus = pydantic.Enum("ReloadMcpStatus", "confirm_required", "confirm_required", "reloaded", "reloaded")

var ReloadMcpSession = pydantic.Enum("ReloadMcpSession", "applied", "applied", "unchanged", "unchanged", "busy", "busy")

var ReloadMcpResult = pydantic.NewModel("ReloadMcpResult", ClosedConfig,
	pydantic.Req("status", ReloadMcpStatus),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("loaded_rev", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("coalesced", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("turn_isolation", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("host_ack", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Def("session", pydantic.Nullable(ReloadMcpSession), nil),
	pydantic.Def("unavailable", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("session_servers", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("``session``: what the calling local session's MCP servers did (a new policy generation, nothing to\nchange, or deferred while a turn runs or waits); ``unavailable``: configured servers that did not connect\nbeside what did apply; ``session_servers``: the MCP servers that session's committed policy runs.")

var _ = Method("reload.mcp", ReloadMcpParams, ReloadMcpResult, "Tear down and rediscover MCP servers for every live session (prompt cache is invalidated).")

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

var LearningFramesParams = pydantic.NewModel("LearningFramesParams", ParamsConfig,
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("rows", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("frames", pydantic.Nullable(pydantic.Int), nil),
)

var LearningFrame = pydantic.NewModel("LearningFrame", ClosedConfig,
	pydantic.Req("reveal", pydantic.Float),
	pydantic.Req("date", pydantic.Str),
	pydantic.Req("visible", pydantic.Int),
	pydantic.Req("grid", pydantic.List(pydantic.List(pydantic.Any))),
	pydantic.Fac("labels", pydantic.List(pydantic.Dict(pydantic.Any)), pydantic.NewList),
).Described("``agent.learning_graph_render.render_graph`` projection; ``grid`` rows are lists of\n``[text, styleKey, alpha?, hexOverride?]`` runs.")

var LearningLegendItem = pydantic.NewModel("LearningLegendItem", ClosedConfig,
	pydantic.Req("glyph", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Def("style", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("color", pydantic.Nullable(pydantic.Str), nil),
)

var LearningNodeRow = pydantic.NewModel("LearningNodeRow", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("glyph", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("fullLabel", pydantic.Str),
	pydantic.Req("meta", pydantic.Str),
	pydantic.Req("body", pydantic.Str),
	pydantic.Req("style", pydantic.Str),
)

var LearningBucketRow = pydantic.NewModel("LearningBucketRow", ClosedConfig,
	pydantic.Req("index", pydantic.Int),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("date", pydantic.Str),
	pydantic.Req("skills", pydantic.Int),
	pydantic.Req("memories", pydantic.Int),
	pydantic.Req("total", pydantic.Int),
	pydantic.Def("category", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("color", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("nodes", pydantic.List(LearningNodeRow)),
)

var LearningAxis = pydantic.NewModel("LearningAxis", ClosedConfig,
	pydantic.Req("start", pydantic.Str),
	pydantic.Req("end", pydantic.Str),
)

var LearningFramesResult = pydantic.NewModel("LearningFramesResult", ClosedConfig,
	pydantic.Req("frames", pydantic.List(LearningFrame)),
	pydantic.Req("legend", pydantic.List(LearningLegendItem)),
	pydantic.Req("categories", pydantic.List(LearningLegendItem)),
	pydantic.Req("buckets", pydantic.List(LearningBucketRow)),
	pydantic.Req("summary", pydantic.List(pydantic.Str)),
	pydantic.Req("axis", LearningAxis),
	pydantic.Req("count", pydantic.Int),
	pydantic.Req("cols", pydantic.Int),
	pydantic.Req("rows", pydantic.Int),
)

var _ = Method("learning.frames", LearningFramesParams, LearningFramesResult, "Pre-render the /journey timeline (frames + legend/summary) so the TUI walks it locally.")

var LearningNodeParams = pydantic.NewModel("LearningNodeParams", ParamsConfig,
	pydantic.Def("id", pydantic.Nullable(pydantic.Str), nil),
)

var LearningEditParams = pydantic.Extend(LearningNodeParams, "LearningEditParams", ParamsConfig,
	pydantic.Def("content", pydantic.Nullable(pydantic.Str), nil),
)

var LearningMutationResult = pydantic.NewModel("LearningMutationResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
).Described("``agent.learning_mutations`` — ``ok: false`` carries the reason in ``message``.")

var LearningDetailResult = pydantic.Extend(LearningMutationResult, "LearningDetailResult", ClosedConfig,
	pydantic.Def("kind", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("content", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("learning.detail", LearningNodeParams, LearningDetailResult, "Node content (SKILL.md or memory chunk) for an edit prefill.")

var _ = Method("learning.delete", LearningNodeParams, LearningMutationResult, "Archive a skill (restorable via curator) or remove a memory chunk.")

var _ = Method("learning.edit", LearningEditParams, LearningMutationResult, "Rewrite a node's content (SKILL.md or memory chunk).")

var McpCatalogEntry = pydantic.NewModel("McpCatalogEntry", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
	pydantic.Def("connector_slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("installed", pydantic.Bool),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Req("requires", pydantic.List(pydantic.Str)),
	pydantic.Req("transport", pydantic.Str),
)

var McpCatalogResult = pydantic.NewModel("McpCatalogResult", ClosedConfig,
	pydantic.Req("servers", pydantic.List(McpCatalogEntry)),
)

var _ = Method("mcp.catalog", ProfileParams, McpCatalogResult, "Curated MCP presets with per-profile installed/enabled state and the env keys each needs.")

var McpServerSource = pydantic.Enum("McpServerSource", "config", "config", "plugin", "plugin")

var McpServerSummary = pydantic.NewModel("McpServerSummary", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("transport", pydantic.Str),
	pydantic.Def("url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("args", pydantic.List(pydantic.Str)),
	pydantic.Req("env", pydantic.List(pydantic.Str)),
	pydantic.Def("auth", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("oauth_tokens_present", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("tools", pydantic.Nullable(pydantic.Any), nil),
	pydantic.Req("source", McpServerSource),
	pydantic.Def("plugin", pydantic.Nullable(pydantic.Str), nil),
).Described("``tui_gateway/mcp_rpc_helpers.summarize_server`` — a server's config without secret values.")

var McpServersListResult = pydantic.NewModel("McpServersListResult", ClosedConfig,
	pydantic.Req("servers", pydantic.List(McpServerSummary)),
)

var _ = Method("mcp.servers.list", ProfileParams, McpServersListResult, "Configured MCP servers for the (scoped) profile, secrets redacted to env-key names.")

var McpRuntimeStatus = pydantic.Enum("McpRuntimeStatus", "connected", "connected", "disabled", "disabled", "connecting", "connecting", "failed", "failed", "lazy", "lazy", "configured", "configured")

var McpServerRuntimeRow = pydantic.NewModel("McpServerRuntimeRow", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("transport", pydantic.Str),
	pydantic.Req("tools", pydantic.Int),
	pydantic.Req("connected", pydantic.Bool),
	pydantic.Req("disabled", pydantic.Bool),
	pydantic.Req("status", McpRuntimeStatus),
	pydantic.Req("source", McpServerSource),
	pydantic.Def("plugin", pydantic.Nullable(pydantic.Str), nil),
).Described("Safe projection of ``tools.mcp_tool_discovery.get_mcp_status`` rows.")

var McpServersStatusResult = pydantic.NewModel("McpServersStatusResult", ClosedConfig,
	pydantic.Req("servers", pydantic.List(McpServerRuntimeRow)),
	pydantic.Req("checked_at", pydantic.Int),
)

var _ = Method("mcp.servers.status", ProfileParams, McpServersStatusResult, "Cached runtime state per configured server; never connects, probes, or starts auth.")

var McpServerNameParams = pydantic.Extend(ProfileParams, "McpServerNameParams", ParamsConfig,
	pydantic.Req("name", pydantic.Str),
)

var McpServersAddParams = pydantic.Extend(McpServerNameParams, "McpServersAddParams", ParamsConfig,
	pydantic.Def("preset", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("config", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("bearer_token", pydantic.Nullable(pydantic.Str), nil),
).Described("``preset`` (catalog id) and/or ``config`` (url/command/args/env/headers/auth/tools); a\n``bearer_token`` is written to the profile's .env, only the header template persists.")

var McpServersAddResult = pydantic.NewModel("McpServersAddResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("server", McpServerSummary),
)

var _ = Method("mcp.servers.add", McpServersAddParams, McpServersAddResult, "Add a server to the profile's config from a catalog preset and/or an explicit config.")

var McpServersSetApiKeyParams = pydantic.Extend(McpServerNameParams, "McpServersSetApiKeyParams", ParamsConfig,
	pydantic.Req("value", pydantic.Str),
	pydantic.Def("env_var", pydantic.Nullable(pydantic.Str), nil),
)

var McpServersSetApiKeyResult = pydantic.NewModel("McpServersSetApiKeyResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("env_var", pydantic.Str),
	pydantic.Req("server", McpServerSummary),
)

var _ = Method("mcp.servers.set_api_key", McpServersSetApiKeyParams, McpServersSetApiKeyResult, "Store a credential in the profile's .env and reference it from the server config (header or env).")

var McpProbeTool = pydantic.NewModel("McpProbeTool", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
)

var McpServersTestResult = pydantic.NewModel("McpServersTestResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("tools", pydantic.List(McpProbeTool)),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("prompts", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("resources", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Req("oauth_needed", pydantic.Bool),
	pydantic.Def("oauth_tokens_present", pydantic.Nullable(pydantic.Bool), nil),
).Described("``ok: false`` carries ``error``; ``prompts`` / ``resources`` are only counted on success.")

var _ = Method("mcp.servers.test", McpServerNameParams, McpServersTestResult, "Connect, list tools, disconnect — an OAuth server with no token on disk is reported as not ok.")

var McpServersRemoveResult = pydantic.NewModel("McpServersRemoveResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("removed", pydantic.Bool),
)

var _ = Method("mcp.servers.remove", McpServerNameParams, McpServersRemoveResult, "Drop a server from the profile's config.yaml.")

var McpOauthStartParams = pydantic.Extend(McpServerNameParams, "McpOauthStartParams", ParamsConfig,
	pydantic.Def("client_redirect_uri", pydantic.Nullable(pydantic.Str), nil),
).Described("With ``client_redirect_uri`` the CLIENT hosts the loopback and relays the code via\n``mcp.servers.oauth.callback``.")

var McpOauthStartResult = pydantic.NewModel("McpOauthStartResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("auth_url", pydantic.Str),
	pydantic.Req("flow", pydantic.Str),
)

var _ = Method("mcp.servers.oauth.start", McpOauthStartParams, McpOauthStartResult, "Begin a PKCE OAuth flow; the client opens auth_url and polls mcp.servers.oauth.poll.")

var McpOauthFlowParams = pydantic.Extend(McpServerNameParams, "McpOauthFlowParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
).Described("``session_id`` is the OAuth flow id returned by ``oauth.start`` (not a gateway session).")

var McpOauthPollStatus = pydantic.Enum("McpOauthPollStatus", "pending", "pending", "approved", "approved", "error", "error")

var McpOauthPollResult = pydantic.NewModel("McpOauthPollResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("status", McpOauthPollStatus),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error_message", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("auth_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tools", pydantic.Nullable(pydantic.List(McpProbeTool)), nil),
)

var _ = Method("mcp.servers.oauth.poll", McpOauthFlowParams, McpOauthPollResult, "Poll a flow; approved persists tokens for the profile and returns the probed tools.")

var McpOauthCancelResult = pydantic.NewModel("McpOauthCancelResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error_message", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("mcp.servers.oauth.cancel", McpOauthFlowParams, McpOauthCancelResult, "Cancel a flow owned by the resolved profile, waking its callback worker.")

var McpOauthCallbackParams = pydantic.Extend(McpOauthFlowParams, "McpOauthCallbackParams", ParamsConfig,
	pydantic.Def("code", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("state", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("iss", pydantic.Nullable(pydantic.Str), nil),
)

var McpOauthCallbackResult = pydantic.NewModel("McpOauthCallbackResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error_message", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("mcp.servers.oauth.callback", McpOauthCallbackParams, McpOauthCallbackResult, "Relay a client-captured redirect into a client_redirect_uri flow.")

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

var PluginsAction = pydantic.Enum("PluginsAction", "list", "list", "toggle", "toggle", "install", "install", "inspect", "inspect", "install_desktop", "install_desktop", "remove_desktop", "remove_desktop", "update", "update", "remove", "remove", "settings", "settings", "onboarding", "onboarding", "presence", "presence")

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
).Described("``toggle``: ``key``/``name`` + ``enable``; ``install``: ``identifier``/``repo`` or ``catalog_name``\n(+ ``force``, ``enable``, ``ref``, ``inspected_sha`` = the commit ``inspect`` read: a custom source with no\n``ref`` whose branch moved since is refused); ``inspect``: ``identifier``/``repo`` or ``catalog_name``;\n``install_desktop``: ``identifier``/``repo`` (any source ``install`` takes) or ``catalog_name``, always\nwith ``ref`` (the commit ``inspect`` read and the dialog showed; another commit is refused) (+ ``force``) — the desktop half onto\nthe host that serves this client; ``remove_desktop``: ``name``\n(+ ``force`` for a package's own half); ``update``: ``name`` (+ ``accept_capabilities`` to apply a re-pin\nthat widened the plugin after the user confirmed the ``delta``, + ``force`` to apply an update whose\nsecurity scan returned caution after the user saw the findings); ``remove``: ``name`` (user installs only);\n``settings``: ``key`` + ``values`` (``{setting_key: value}``, non-secret schema keys only);\n``presence``: ``names`` (catalog names, answered in that order).")

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
	pydantic.Req("has_desktop_half", pydantic.Bool),
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
).Described("What a plugin loaded mid-run does NOW vs later (``hermes_cli.plugins_activation``). ``activated_now``\nkinds (``{kind: [names]}``): ``gateway_commands`` (slash names), ``locales`` (``<lang>.<surface>``\nlanguage-pack layers), ``gateway_transforms`` / ``hooks`` (hook\nnames), ``callbacks`` (platforms / ``slack:<action_id>``) — live in the running gateway once it reloaded\n(``gateway_reloaded``). ``live_now``: the plugin's MCP servers (connected, with their tools, or the\nerror) and skills, usable in every open chat of the profile from its next turn — the chats also get a\nnote listing them. ``deferred`` kinds: ``tools`` (Python tool names) and ``prompt`` (section ids)\napply from the next session. ``missing_dependencies``: the plugin's declared Python requirements a\nrunning process cannot import (installed after it booted, or never); ``missing_in`` names which\nprocesses (``gateway``, ``backend`` = the Desktop / dashboard process) — only restarting them loads them.")

var OnboardingCatalogPlugin = pydantic.NewModel("OnboardingCatalogPlugin", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("title", pydantic.Str),
	pydantic.Req("description", pydantic.Str),
	pydantic.Req("tier", CatalogTier),
	pydantic.Req("platforms", pydantic.List(pydantic.Str)),
	pydantic.Req("app_state", CatalogAppState),
	pydantic.Req("sentence", pydantic.Str),
).Described("A catalog plugin curated for the onboarding card (``onboarding: true``) that this OS runs.\n``app_state`` is the pinned ``plugin.json`` declaration judged on this host; ``sentence`` names what\nis missing (empty when present or unknown).")

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
	pydantic.Def("onboarding", pydantic.Nullable(pydantic.List(OnboardingCatalogPlugin)), nil),
	pydantic.Def("presence", pydantic.Nullable(pydantic.List(CatalogPluginPresence)), nil),
	pydantic.Def("agent", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("desktop", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("agent_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("desktop_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("insecure", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("paired_with_agent", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("unpaired_from", pydantic.Nullable(pydantic.Str), nil),
).Described("``list`` → ``plugins`` + counts; ``toggle`` → ``ok``/``unchanged``/``restart_required``/``name``\n(the canonical key written)/``plugin``; ``install`` → ``hermes_cli.plugins_cmd.dashboard_install_plugin``'s\nok payload; ``inspect`` → ``ok``/``agent``/``desktop``/``agent_name``/``desktop_name``/``sha`` (the commit\nread)/``warnings``/``insecure`` or ``ok=false`` + ``error``; ``toggle``/``install``/``update`` that loaded a plugin also carry ``gateway_reloaded`` (the\nrunning gateway picked it up and re-wired its handlers) and ``activation`` — the honest split of what is\nlive now vs deferred, so ``restart_required`` is True only when no gateway answered or the running\ngateway/backend cannot import the plugin's Python dependencies (``activation.missing_dependencies``); ``update`` → ``ok``/``unchanged``/``sha``, or ``ok=false`` + ``consent_required`` with the\n``delta`` (``{surface: [added...]}``) / ``delta_lines`` a widened pin adds — nothing changed until the\nclient retries with ``accept_capabilities`` — or ``ok=false`` + ``scan_blocked`` with ``scan_verdict`` /\n``scan_findings`` (retry a caution verdict with ``force``); ``remove`` → ``ok``/``name`` plus\n``cleared_memory_provider`` when the removed plugin was the live ``memory.provider``.")

var _ = Method("plugins.manage", PluginsManageParams, PluginsManageResult, "Plugins Hub backend: list installed plugins, toggle, git-install, re-pin a catalog install, or remove a user install.")

var PluginCatalogLookupStatus = pydantic.Enum("PluginCatalogLookupStatus", "found", "found", "invalid_name", "invalid_name", "unknown", "unknown", "unavailable", "unavailable")

var PluginCatalogLookupParams = pydantic.Extend(ProfileParams, "PluginCatalogLookupParams", ParamsConfig,
	pydantic.Req("name", pydantic.Str),
)

var PluginCatalogLookupEntry = pydantic.NewModel("PluginCatalogLookupEntry", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("repo", pydantic.Str),
	pydantic.Req("sha", pydantic.Str),
	pydantic.Req("subdir", pydantic.Str),
	pydantic.Def("tier", pydantic.Str, "community"),
).Described("The fields an install needs: the same ones the embedded catalog picker posts on a pick.")

var PluginCatalogLookupResult = pydantic.NewModel("PluginCatalogLookupResult", ClosedConfig,
	pydantic.Req("status", PluginCatalogLookupStatus),
	pydantic.Def("entry", pydantic.Nullable(PluginCatalogLookupEntry), nil),
).Described("``found`` carries ``entry``. ``unknown`` covers a name the catalog doesn't list and one it removed;\n``unavailable`` means neither the live catalog nor the in-tree fallback could be read.")

var _ = Method("plugins.catalog.lookup", PluginCatalogLookupParams, PluginCatalogLookupResult, "One curated plugin-catalog entry by name, from the backend's live catalog (cached, with the in-tree fallback and kill list); backs icebath://plugin/install?catalog= deep links.")
