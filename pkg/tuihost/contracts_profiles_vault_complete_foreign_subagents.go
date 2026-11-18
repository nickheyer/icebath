package tuihost

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
