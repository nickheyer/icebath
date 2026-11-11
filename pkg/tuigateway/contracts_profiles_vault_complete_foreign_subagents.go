package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var CompletionItem = pydantic.NewModel("CompletionItem", ClosedConfig,
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("display", pydantic.Str, ""),
	pydantic.Def("meta", pydantic.Str, ""),
	pydantic.Def("kind", pydantic.Nullable(pydantic.Str), nil),
)

var CompletionItemsResult = pydantic.NewModel("CompletionItemsResult", ClosedConfig,
	pydantic.Fac("items", pydantic.List(CompletionItem), pydantic.NewList),
)

var CompletePathParams = pydantic.Extend(ProfileParams, "CompletePathParams", ParamsConfig,
	pydantic.Def("word", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("complete.path", CompletePathParams, CompletionItemsResult, "Path / @-reference completions for the composer (files, folders, profiles, plugin providers).")

var CompleteSlashParams = pydantic.NewModel("CompleteSlashParams", ParamsConfig,
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var CompleteSlashResult = pydantic.NewModel("CompleteSlashResult", ClosedConfig,
	pydantic.Fac("items", pydantic.List(CompletionItem), pydantic.NewList),
	pydantic.Def("replace_from", pydantic.Nullable(pydantic.Int), nil),
)

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
)

var ProfileWorkerSession = pydantic.NewModel("ProfileWorkerSession", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("source", pydantic.Str, ""),
	pydantic.Def("title", pydantic.Str, ""),
	pydantic.Def("last_active", pydantic.Union(pydantic.Float, pydantic.Int), 0),
)

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
)

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
)

var ProfilesListParams = pydantic.Extend(ProfileParams, "ProfilesListParams", ParamsConfig,
	pydantic.Def("include_sessions", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
)

var ProfilesListResult = pydantic.NewModel("ProfilesListResult", ClosedConfig,
	pydantic.Fac("profiles", pydantic.List(ProfileRow), pydantic.NewList),
	pydantic.Def("bot_mode_protocol", pydantic.Bool, true),
	pydantic.Def("install_id", pydantic.Str, ""),
)

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
)

var ProfileMirrored = pydantic.NewModel("ProfileMirrored", ClosedConfig,
	pydantic.Def("env", pydantic.Bool, false),
	pydantic.Def("auth", pydantic.Union(pydantic.Bool, pydantic.Literal("shared")), false),
	pydantic.Def("model_inherited", pydantic.Bool, false),
	pydantic.Def("voice", pydantic.Bool, false),
)

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
)

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
)

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
)

var ProfilesConfigureResult = pydantic.NewModel("ProfilesConfigureResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("applied", ProfilesConfigureApplied),
	pydantic.Def("confirm_required", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("confirm_message", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("profiles.configure", ProfilesConfigureParams, ProfilesConfigureResult, "Editor Save: apply any subset of a profile's sections and report each one.")

var ProfilesSetAssetParams = pydantic.Extend(ProfileParams, "ProfilesSetAssetParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("asset", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("data", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("clear", pydantic.Nullable(pydantic.Union(pydantic.Bool, pydantic.Str)), nil),
)

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
)

var _ = Method("profiles.get_asset", ProfilesGetAssetParams, ProfilesGetAssetResult, "A profile asset as a data URL.")

var OnboardingStateParams = pydantic.NewModel("OnboardingStateParams", ParamsConfig,
	pydantic.Def("assume_fresh", pydantic.Bool, false),
)

var OnboardingRunStateResult = pydantic.NewModel("OnboardingRunStateResult", ClosedConfig,
	pydantic.Req("run", pydantic.Bool),
	pydantic.Req("eligible", pydantic.Bool),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("run_id", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("onboarding.state", OnboardingStateParams, OnboardingRunStateResult, "Whether desktop first-run onboarding is due, and the chat in progress; reads the root profile's config.")

var OnboardingSetRunParams = pydantic.NewModel("OnboardingSetRunParams", ParamsConfig,
	pydantic.Req("run", pydantic.Bool),
	pydantic.Def("mark_profile_offered", pydantic.Bool, false),
)

var _ = Method("onboarding.set_run", OnboardingSetRunParams, OnboardingRunStateResult, "Write onboarding.run in the root profile's config.yaml; answers the new state.")

var OnboardingFactsSource = pydantic.Enum("OnboardingFactsSource", "local", "local", "remote", "remote")

var OnboardingStartParams = pydantic.NewModel("OnboardingStartParams", ParamsConfig,
	pydantic.Def("rerun", pydantic.Bool, false),
	pydantic.Def("fresh_launch", pydantic.Bool, false),
	pydantic.Def("assume_fresh", pydantic.Bool, false),
	pydantic.Def("facts_source", OnboardingFactsSource, "local"),
	pydantic.Def("locale", pydantic.Nullable(pydantic.Str), nil),
)

var OnboardingStartResult = pydantic.NewModel("OnboardingStartResult", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("resumed", pydantic.Bool),
	pydantic.Req("run_id", pydantic.Str),
)

var _ = Method("onboarding.start", OnboardingStartParams, OnboardingStartResult, "Open the onboarding chat in the default profile: create it, write its run record and play the opening.")

var OnboardingSkipParams = pydantic.Extend(SessionParams, "OnboardingSkipParams", ParamsConfig)

var OnboardingSkipResult = pydantic.NewModel("OnboardingSkipResult", ClosedConfig,
	pydantic.Req("skipped", pydantic.Bool),
)

var _ = Method("onboarding.skip", OnboardingSkipParams, OnboardingSkipResult, "Skip setup: end the session's onboarding run and close the card that waits.")

var VaultKind = pydantic.Enum("VaultKind", "login", "login", "payment", "payment", "address", "address")

var VaultItem = pydantic.NewModel("VaultItem", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Def("origin", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("created_at", pydantic.Str, ""),
	pydantic.Def("identifier", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("identifier_type", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("has_otp", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Req("backend", pydantic.Str),
)

var VaultListResult = pydantic.NewModel("VaultListResult", ClosedConfig,
	pydantic.Fac("items", pydantic.List(VaultItem), pydantic.NewList),
)

var _ = Method("vault.list", ProfileParams, VaultListResult, "Metadata-only listing across the local vault and every unlocked password manager.")

var VaultSource = pydantic.NewModel("VaultSource", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("display_name", pydantic.Str),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Req("needs_unlock", pydantic.Bool),
	pydantic.Req("unlocked", pydantic.Bool),
	pydantic.Req("installed", pydantic.Bool),
)

var VaultSourcesResult = pydantic.NewModel("VaultSourcesResult", ClosedConfig,
	pydantic.Fac("sources", pydantic.List(VaultSource), pydantic.NewList),
)

var _ = Method("vault.sources", ProfileParams, VaultSourcesResult, "Status of every login source (local vault + detected password managers).")

var VaultSourceSetParams = pydantic.Extend(ProfileParams, "VaultSourceSetParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("enabled", pydantic.Nullable(pydantic.Bool), nil),
)

var VaultSourceSetResult = pydantic.NewModel("VaultSourceSetResult", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("enabled", pydantic.Bool),
)

var _ = Method("vault.source.set", VaultSourceSetParams, VaultSourceSetResult, "Enable or disable an external password manager (disabling also locks it).")

var VaultUnlockParams = pydantic.Extend(ProfileParams, "VaultUnlockParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("password", pydantic.Nullable(pydantic.Str), nil),
)

var VaultUnlockResult = pydantic.NewModel("VaultUnlockResult", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("unlocked", pydantic.Bool, true),
)

var _ = Method("vault.unlock", VaultUnlockParams, VaultUnlockResult, "Unlock a password manager for this session with its master password.")

var VaultLockParams = pydantic.Extend(ProfileParams, "VaultLockParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
)

var VaultLockResult = pydantic.NewModel("VaultLockResult", ClosedConfig,
	pydantic.Def("locked", pydantic.Bool, true),
)

var _ = Method("vault.lock", VaultLockParams, VaultLockResult, "Forget a manager's session token (every manager when no name is given).")

var VaultAddParams = pydantic.Extend(ProfileParams, "VaultAddParams", ParamsConfig,
	pydantic.Def("kind", pydantic.Nullable(VaultKind), nil),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("origin", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("secret", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
)

var VaultAddResult = pydantic.NewModel("VaultAddResult", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
)

var _ = Method("vault.add", VaultAddParams, VaultAddResult, "Add a login / payment / address item to the local vault.")

var VaultRemoveParams = pydantic.Extend(ProfileParams, "VaultRemoveParams", ParamsConfig,
	pydantic.Def("id", pydantic.Nullable(pydantic.Str), nil),
)

var VaultRemoveResult = pydantic.NewModel("VaultRemoveResult", ClosedConfig,
	pydantic.Req("removed", pydantic.Bool),
)

var _ = Method("vault.remove", VaultRemoveParams, VaultRemoveResult, "Remove a local vault item by id.")

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
)

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
)

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
)

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
)

var FailedDelegation = pydantic.NewModel("FailedDelegation", ClosedConfig,
	pydantic.Req("delegation_id", pydantic.Str),
	pydantic.Def("task_index", pydantic.Int, 0),
	pydantic.Req("status", pydantic.Str),
	pydantic.Def("goal", pydantic.Str, ""),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("dispatched_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("completed_at", pydantic.Nullable(pydantic.Float), nil),
)

var SubagentListResult = pydantic.NewModel("SubagentListResult", ClosedConfig,
	pydantic.Fac("subagents", pydantic.List(SubagentSnapshot), pydantic.NewList),
	pydantic.Fac("delegations", pydantic.List(FailedDelegation), pydantic.NewList),
)

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
)

var _ = Method("subagent.tail", SubagentIdParams, SubagentTailResult, "Last 16KB of an owned child's live transcript.")
