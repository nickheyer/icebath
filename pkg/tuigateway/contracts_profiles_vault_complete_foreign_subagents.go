package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var CompletionItem = pydantic.NewModel("CompletionItem", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("display", pydantic.Str, ""),
	pydantic.Def("meta", pydantic.Str, ""),
	pydantic.Def("kind", pydantic.Nullable(pydantic.Str), nil),
).Described("One popover row; ``kind`` rides only on slash completions (command vs skill).")

var CompletionItemsResult = pydantic.NewModel("CompletionItemsResult", ClosedConfig,
	pydantic.Fac("items", pydantic.List(CompletionItem), pydantic.NewList),
)

var CompletePathParams = pydantic.Extend(ProfileParams, "CompletePathParams", ParamsConfig,
	pydantic.Def("word", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``word`` is the token under the cursor (``@`` prefix = context reference); ``cwd`` /\n``session_id`` pick the directory the listing resolves against.")

var _ = Method("complete.path", CompletePathParams, CompletionItemsResult, "Path / @-reference completions for the composer (files, folders, profiles, plugin providers).")

var CompleteSlashParams = pydantic.NewModel("CompleteSlashParams", ParamsConfig,
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``session_id`` binds skill completions to that session's profile and workspace (project skills);\n``profile`` scopes a session-less request (a new-chat draft).")

var CompleteSlashResult = pydantic.NewModel("CompleteSlashResult", ClosedConfig,
	pydantic.Fac("items", pydantic.List(CompletionItem), pydantic.NewList),
	pydantic.Def("replace_from", pydantic.Nullable(pydantic.Int), nil),
).Described("``replace_from`` is the column the accepted item replaces from.")

var _ = Method("complete.slash", CompleteSlashParams, CompleteSlashResult, "Ranked slash-command / skill completions for a ``/`` token.")

var PasteCollapseParams = pydantic.NewModel("PasteCollapseParams", ParamsConfig,
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
)

var PasteCollapseResult = pydantic.NewModel("PasteCollapseResult", ClosedConfig,
	pydantic.Req("placeholder", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("lines", pydantic.Int),
)

var _ = Method("paste.collapse", PasteCollapseParams, PasteCollapseResult, "Spill a large paste to a file and hand back the inline placeholder.")

var ModelSaveKeyParams = pydantic.NewModel("ModelSaveKeyParams", ParamsConfig,
	pydantic.Req("slug", pydantic.Str),
	pydantic.Req("api_key", pydantic.Str),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
)

var ModelSaveKeyResult = pydantic.NewModel("ModelSaveKeyResult", ClosedConfig,
	pydantic.Req("provider", ModelOptionProvider),
)

var _ = Method("model.save_key", ModelSaveKeyParams, ModelSaveKeyResult, "Save an API key for a provider and return its refreshed inventory row.")

var ModelProbeParams = pydantic.NewModel("ModelProbeParams", ParamsConfig,
	pydantic.Req("base_url", pydantic.Str),
	pydantic.Def("api_key", pydantic.Str, ""),
)

var ModelProbeResult = pydantic.NewModel("ModelProbeResult", ClosedConfig,
	pydantic.Req("base_url", pydantic.Str),
	pydantic.Req("found", pydantic.Bool),
	pydantic.Req("models", pydantic.List(pydantic.Str)),
	pydantic.Req("probed_url", pydantic.Str),
	pydantic.Req("suggested_base_url", pydantic.Str),
)

var _ = Method("model.probe", ModelProbeParams, ModelProbeResult, "List the models an OpenAI compatible endpoint serves, with the base URL that answered.")

var ModelAssignParams = pydantic.NewModel("ModelAssignParams", ParamsConfig,
	pydantic.Req("provider", pydantic.Str),
	pydantic.Req("model", pydantic.Str),
	pydantic.Def("base_url", pydantic.Str, ""),
	pydantic.Def("api_key", pydantic.Str, ""),
	pydantic.Def("confirm_expensive_model", pydantic.Bool, false),
)

var ModelAssignResult = pydantic.NewModel("ModelAssignResult", ClosedConfig,
	pydantic.Req("provider", pydantic.Str),
	pydantic.Req("model", pydantic.Str),
	pydantic.Req("base_url", pydantic.Str),
	pydantic.Req("confirm_required", pydantic.Bool),
	pydantic.Req("confirm_message", pydantic.Str),
).Described("``confirm_required`` mirrors ``config.set``: a guarded model pick wrote nothing yet.")

var _ = Method("model.assign", ModelAssignParams, ModelAssignResult, "Write the profile default model and provider, registering a new custom endpoint with its key.")

var ModelDisconnectParams = pydantic.NewModel("ModelDisconnectParams", ParamsConfig,
	pydantic.Req("slug", pydantic.Str),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
)

var ModelDisconnectResult = pydantic.NewModel("ModelDisconnectResult", ClosedConfig,
	pydantic.Req("slug", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("disconnected", pydantic.Bool),
)

var _ = Method("model.disconnect", ModelDisconnectParams, ModelDisconnectResult, "Remove every credential (env keys and OAuth state) for a provider.")

var ProfileSessionPreview = pydantic.NewModel("ProfileSessionPreview", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("title", pydantic.Str, ""),
	pydantic.Def("preview", pydantic.Str, ""),
	pydantic.Def("started_at", pydantic.Union(pydantic.Float, pydantic.Int), 0),
	pydantic.Def("last_active", pydantic.Union(pydantic.Float, pydantic.Int), 0),
	pydantic.Def("message_count", pydantic.Int, 0),
	pydantic.Def("live_message_count", pydantic.Nullable(pydantic.Int), nil),
).Described("Newest human-facing session of a profile (``_latest_profile_session_rows``).")

var ProfileWorkerSession = pydantic.NewModel("ProfileWorkerSession", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("source", pydantic.Str, ""),
	pydantic.Def("title", pydantic.Str, ""),
	pydantic.Def("last_active", pydantic.Union(pydantic.Float, pydantic.Int), 0),
).Described("Newest kanban/tool worker row, so rosters can show a profile as working.")

var ProfileCanonicalSession = pydantic.NewModel("ProfileCanonicalSession", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("resolved_id", pydantic.Str),
	pydantic.Def("root_title", pydantic.Str, ""),
	pydantic.Def("title", pydantic.Str, ""),
	pydantic.Def("preview", pydantic.Str, ""),
	pydantic.Def("started_at", pydantic.Union(pydantic.Float, pydantic.Int), 0),
	pydantic.Def("last_active", pydantic.Union(pydantic.Float, pydantic.Int), 0),
	pydantic.Def("message_count", pydantic.Int, 0),
	pydantic.Def("live_message_count", pydantic.Nullable(pydantic.Int), nil),
).Described("The profile's \"Bot Chat\" registry row; ``resolved_id`` is the live compression tip.")

var ProfileRow = pydantic.NewModel("ProfileRow", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Def("is_default", pydantic.Bool, false),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Str, ""),
	pydantic.Def("display_name", pydantic.Str, ""),
	pydantic.Def("skill_count", pydantic.Int, 0),
	pydantic.Fac("previous_names", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("last_session", pydantic.Nullable(ProfileSessionPreview), nil),
	pydantic.Def("worker_session", pydantic.Nullable(ProfileWorkerSession), nil),
	pydantic.Def("canonical_session", pydantic.Nullable(ProfileCanonicalSession), nil),
	pydantic.Fac("ui_meta_revisions", pydantic.Dict(pydantic.Int), pydantic.NewDict),
	pydantic.Def("ui_meta", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("has_avatar", pydantic.Bool, false),
).Described("One roster row; the session fields are present only with ``include_sessions``.")

var ProfilesListParams = pydantic.Extend(ProfileParams, "ProfilesListParams", ParamsConfig,
	pydantic.Def("include_sessions", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
)

var ProfilesListResult = pydantic.NewModel("ProfilesListResult", ClosedConfig,
	pydantic.Fac("profiles", pydantic.List(ProfileRow), pydantic.NewList),
	pydantic.Def("bot_mode_protocol", pydantic.Bool, true),
	pydantic.Def("install_id", pydantic.Str, ""),
).Described("``bot_mode_protocol`` tells clients this backend injects the teammate protocol itself;\n``install_id`` (as on ``/api/status``) names the machine that answered.")

var _ = Method("profiles.list", ProfilesListParams, ProfilesListResult, "Roster of profiles with previews so a client paints without N follow-up calls.")

var ProfilesCreateParams = pydantic.Extend(ProfileParams, "ProfilesCreateParams", ParamsConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("clone_from", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("clone_all", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
	pydantic.Def("clone_channels", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
	pydantic.Def("no_skills", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
	pydantic.Def("no_alias", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
	pydantic.Def("soul", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("share_auth", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
	pydantic.Def("mirror_credentials", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
).Described("``clone_from`` omitted = fresh profile + bundled skills; ``mirror_credentials`` defaults on\nso a headless bot has a provider.")

var ProfileMirrored = pydantic.NewModel("ProfileMirrored", ClosedConfig,
	pydantic.Def("env", pydantic.Bool, false),
	pydantic.Def("auth", pydantic.Union(pydantic.Bool, pydantic.Literal("shared")), false),
	pydantic.Def("model_inherited", pydantic.Bool, false),
	pydantic.Def("voice", pydantic.Bool, false),
).Described("What was copied from the launch profile; ``auth`` is ``\"shared\"`` under ``share_auth``.")

var ProfilesCreateResult = pydantic.NewModel("ProfilesCreateResult", ClosedConfig,
	pydantic.Def("ok", pydantic.Bool, true),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Def("soul_written", pydantic.Bool, false),
	pydantic.Def("model_set", pydantic.Bool, false),
	pydantic.Req("mirrored", ProfileMirrored),
)

var _ = Method("profiles.create", ProfilesCreateParams, ProfilesCreateResult, "Create a profile (ws twin of POST /api/profiles), mirroring launch credentials by default.")

var ProfileNameParams = pydantic.Extend(ProfileParams, "ProfileNameParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
)

var CapabilityEntry = pydantic.NewModel("CapabilityEntry", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("enabled", pydantic.Bool, true),
)

var ToolsetEntry = pydantic.Extend(CapabilityEntry, "ToolsetEntry", ClosedConfig,
	pydantic.Def("label", pydantic.Str, ""),
	pydantic.Def("description", pydantic.Str, ""),
	pydantic.Def("tool_count", pydantic.Int, 0),
)

var McpServerEntry = pydantic.Extend(CapabilityEntry, "McpServerEntry", ClosedConfig,
	pydantic.Def("transport", pydantic.Str, "stdio"),
)

var ProfileModelPin = pydantic.NewModel("ProfileModelPin", ClosedConfig,
	pydantic.Def("provider", pydantic.Str, ""),
	pydantic.Def("default", pydantic.Str, ""),
)

var ProfilesDescribeResult = pydantic.NewModel("ProfilesDescribeResult", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("description", pydantic.Str, ""),
	pydantic.Def("soul", pydantic.Str, ""),
	pydantic.Req("model", ProfileModelPin),
	pydantic.Fac("skills", pydantic.List(CapabilityEntry), pydantic.NewList),
	pydantic.Fac("toolsets", pydantic.List(ToolsetEntry), pydantic.NewList),
	pydantic.Def("toolsets_pinned", pydantic.Bool, false),
	pydantic.Fac("mcp_servers", pydantic.List(McpServerEntry), pydantic.NewList),
).Described("Editor snapshot; ``toolsets_pinned`` says whether ``tools.enabled_toolsets`` is explicit.")

var _ = Method("profiles.describe", ProfileNameParams, ProfilesDescribeResult, "Everything the profile editor shows: soul, model pin, skills, toolsets, MCP servers.")

var ProfilesConfigureParams = pydantic.Extend(ProfileParams, "ProfilesConfigureParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("ui_meta", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("ui_meta_expected_revisions", pydantic.Nullable(pydantic.Dict(pydantic.Int)), nil),
	pydantic.Def("soul", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("confirm_expensive_model", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
	pydantic.Def("disabled_skills", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("enabled_toolsets", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("enabled_mcp_servers", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("Sections are independent; ``ui_meta_expected_revisions`` is a per-key compare-and-swap.")

var UiMetaConflict = pydantic.NewModel("UiMetaConflict", ClosedConfig,
	pydantic.Def("expected", pydantic.Any, nil),
	pydantic.Def("actual", pydantic.Int, 0),
)

var ProfilesConfigureApplied = pydantic.NewModel("ProfilesConfigureApplied", ClosedConfig,
	pydantic.Def("ui_meta", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("ui_meta_revisions", pydantic.Nullable(pydantic.Dict(pydantic.Int)), nil),
	pydantic.Def("ui_meta_conflicts", pydantic.Nullable(pydantic.Dict(UiMetaConflict)), nil),
	pydantic.Def("soul", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("skills", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("toolsets", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("mcp_servers", pydantic.Nullable(pydantic.Bool), nil),
).Described("Per-section outcome; only the sections the request carried are present.")

var ProfilesConfigureResult = pydantic.NewModel("ProfilesConfigureResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("applied", ProfilesConfigureApplied),
	pydantic.Def("confirm_required", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("confirm_message", pydantic.Nullable(pydantic.Str), nil),
).Described("``confirm_required`` mirrors ``config.set``: a guarded model pick wrote nothing yet.")

var _ = Method("profiles.configure", ProfilesConfigureParams, ProfilesConfigureResult, "Editor Save: apply any subset of a profile's sections and report each one.")

var ProfilesSetAssetParams = pydantic.Extend(ProfileParams, "ProfilesSetAssetParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("asset", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("data", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("clear", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
).Described("``data`` is a data URL or bare base64 (PNG/JPEG/WebP, sniffed); ``clear`` deletes instead.")

var ProfilesSetAssetResult = pydantic.NewModel("ProfilesSetAssetResult", ClosedConfig,
	pydantic.Def("ok", pydantic.Bool, true),
	pydantic.Req("asset", pydantic.Str),
	pydantic.Def("size", pydantic.Int, 0),
	pydantic.Def("removed", pydantic.Nullable(pydantic.Int), nil),
)

var _ = Method("profiles.set_asset", ProfilesSetAssetParams, ProfilesSetAssetResult, "Store or clear a profile asset (avatar) atomically.")

var ProfilesGetAssetParams = pydantic.Extend(ProfileParams, "ProfilesGetAssetParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("asset", pydantic.Nullable(pydantic.Str), nil),
)

var ProfilesGetAssetResult = pydantic.NewModel("ProfilesGetAssetResult", ClosedConfig,
	pydantic.Req("found", pydantic.Bool),
	pydantic.Def("mime", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("size", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("data", pydantic.Nullable(pydantic.Str), nil),
).Described("Absent is ``found: false``, not an error.")

var _ = Method("profiles.get_asset", ProfilesGetAssetParams, ProfilesGetAssetResult, "A profile asset as a data URL.")

var ForeignSource = pydantic.Enum("ForeignSource", "claude", "claude", "codex", "codex")

var ForeignSessionRow = pydantic.NewModel("ForeignSessionRow", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("source", ForeignSource),
	pydantic.Req("label", pydantic.Str),
	pydantic.Def("title", pydantic.Str, ""),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("mtime", pydantic.Float),
	pydantic.Def("turn_count", pydantic.Int, 0),
	pydantic.Def("excerpt", pydantic.Str, ""),
).Described("``hermes_cli/foreign_sessions_browser.py::list_foreign_sessions`` — ``id`` is an opaque\nhandle, never a path.")

var SessionForeignListParams = pydantic.Extend(ProfileParams, "SessionForeignListParams", ParamsConfig,
	pydantic.Def("source", pydantic.Nullable(ForeignSource), nil),
	pydantic.Def("offset", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("limit", pydantic.Nullable(pydantic.Int), nil),
)

var SessionForeignListResult = pydantic.NewModel("SessionForeignListResult", ClosedConfig,
	pydantic.Fac("sessions", pydantic.List(ForeignSessionRow), pydantic.NewList),
	pydantic.Def("next_offset", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Req("host", pydantic.Str),
	pydantic.Def("unreadable", pydantic.Int, 0),
).Described("``unreadable`` counts logs on this page that failed to parse.")

var _ = Method("session.foreign.list", SessionForeignListParams, SessionForeignListResult, "One page of Claude Code / Codex sessions found on the serving backend.")

var ForeignTurn = pydantic.NewModel("ForeignTurn", ClosedConfig,
	pydantic.Req("role", pydantic.Str),
	pydantic.Req("content", pydantic.Str),
)

var SessionForeignIdParams = pydantic.Extend(ProfileParams, "SessionForeignIdParams", ParamsConfig,
	pydantic.Def("id", pydantic.Nullable(pydantic.Str), nil),
)

var SessionForeignPreviewResult = pydantic.NewModel("SessionForeignPreviewResult", ClosedConfig,
	pydantic.Fac("messages", pydantic.List(ForeignTurn), pydantic.NewList),
	pydantic.Def("total", pydantic.Int, 0),
	pydantic.Def("truncated", pydantic.Bool, false),
	pydantic.Def("already_imported", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
).Described("Bounded to the last 40 turns / 8000 chars each; ``already_imported`` is the local id.")

var _ = Method("session.foreign.preview", SessionForeignIdParams, SessionForeignPreviewResult, "Preview a foreign session's tail before importing it.")

var SessionForeignImportResult = pydantic.NewModel("SessionForeignImportResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("already_imported", pydantic.Bool, false),
)

var _ = Method("session.foreign.import", SessionForeignIdParams, SessionForeignImportResult, "Import a foreign session into this profile's history (idempotent per origin).")

var SubagentSnapshot = pydantic.NewModel("SubagentSnapshot", ClosedConfig,
	pydantic.Req("subagent_id", pydantic.Str),
	pydantic.Def("parent_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("depth", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("goal", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("delegation_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("status", pydantic.Nullable(SubagentStatus), nil),
	pydantic.Def("tool_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("last_tool", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("accepting_steer", pydantic.Nullable(pydantic.Bool), nil),
).Described("``methods_subagents._SUBAGENT_SNAPSHOT_FIELDS`` projection of one live child record.")

var FailedDelegation = pydantic.NewModel("FailedDelegation", ClosedConfig,
	pydantic.Req("delegation_id", pydantic.Str),
	pydantic.Def("task_index", pydantic.Int, 0),
	pydantic.Req("status", pydantic.Str),
	pydantic.Def("goal", pydantic.Str, ""),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("dispatched_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("completed_at", pydantic.Nullable(pydantic.Float), nil),
).Described("``async_delegation.failed_delegations_for_session`` row: one failed task of an async delegation.")

var SubagentListResult = pydantic.NewModel("SubagentListResult", ClosedConfig,
	pydantic.Fac("subagents", pydantic.List(SubagentSnapshot), pydantic.NewList),
	pydantic.Fac("delegations", pydantic.List(FailedDelegation), pydantic.NewList),
).Described("``delegations``: recently failed async delegation tasks for the session (durable store), newest first.")

var _ = Method("subagent.list", SessionParams, SubagentListResult, "Live children owned by this session (other sessions' children never leak).")

var SubagentRosterEntry = pydantic.NewModel("SubagentRosterEntry", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("subagents", pydantic.Int, 0),
	pydantic.Def("background", pydantic.Int, 0),
)

var SubagentRosterResult = pydantic.NewModel("SubagentRosterResult", ClosedConfig,
	pydantic.Fac("sessions", pydantic.List(SubagentRosterEntry), pydantic.NewList),
)

var _ = Method("subagent.roster", ProfileParams, SubagentRosterResult, "Per-conversation counts of live delegated children and running background processes, limited to conversations stored in the named profile (launch profile when omitted). Needs no attached session.")

var SubagentIdParams = pydantic.Extend(SessionParams, "SubagentIdParams", ParamsConfig,
	pydantic.Req("subagent_id", pydantic.Str),
)

var SubagentInterruptResult = pydantic.NewModel("SubagentInterruptResult", ClosedConfig,
	pydantic.Req("found", pydantic.Bool),
	pydantic.Req("subagent_id", pydantic.Str),
)

var _ = Method("subagent.interrupt", SubagentIdParams, SubagentInterruptResult, "Hard-interrupt one owned child; ``found`` is false when it already finished.")

var SubagentTailResult = pydantic.NewModel("SubagentTailResult", ClosedConfig,
	pydantic.Req("subagent_id", pydantic.Str),
	pydantic.Def("available", pydantic.Bool, false),
	pydantic.Def("text", pydantic.Str, ""),
	pydantic.Def("truncated", pydantic.Bool, false),
).Described("``available`` is false while the child has no live transcript yet (or it was cleaned up).")

var _ = Method("subagent.tail", SubagentIdParams, SubagentTailResult, "Last 16KB of an owned child's live transcript.")
