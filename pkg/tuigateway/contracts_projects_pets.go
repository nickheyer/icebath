package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ProjectFolder = pydantic.NewModel("ProjectFolder", ClosedConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("is_primary", pydantic.Bool, false),
	pydantic.Def("added_at", pydantic.Nullable(pydantic.Int), nil),
).Described("``hermes_cli/projects_db.py::ProjectFolder.to_dict``.")

var ProjectInfo = pydantic.NewModel("ProjectInfo", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("slug", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("icon", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("color", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("board_slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("primary_path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("archived", pydantic.Bool, false),
	pydantic.Req("created_at", pydantic.Int),
	pydantic.Fac("folders", pydantic.List(ProjectFolder), pydantic.NewList),
).Described("``hermes_cli/projects_db.py::Project.to_dict`` — one stored project with its folders.")

var ProjectsPayload = pydantic.NewModel("ProjectsPayload", ClosedConfig,
	pydantic.Req("projects", pydantic.List(ProjectInfo)),
	pydantic.Def("active_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_projects._projects_payload``: every project (archived included) + the active id.")

var ProjectResult = pydantic.NewModel("ProjectResult", ClosedConfig,
	pydantic.Req("project", ProjectInfo),
)

var OptionalProjectResult = pydantic.NewModel("OptionalProjectResult", ClosedConfig,
	pydantic.Def("project", pydantic.Nullable(ProjectInfo), nil),
)

var ProjectIdParams = pydantic.Extend(ProfileParams, "ProjectIdParams", ParamsConfig,
	pydantic.Req("id", pydantic.Str),
).Described("Any method addressed at one stored project (``5062`` when the id resolves to nothing).")

var _ = Method("projects.list", ProfileParams, ProjectsPayload, "Every project of the profile (archived included) plus which one is active.")

var _ = Method("projects.get", ProjectIdParams, ProjectResult, "One stored project with its folders.")

var ProjectsCreateParams = pydantic.Extend(ProfileParams, "ProjectsCreateParams", ParamsConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("folders", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("primary_path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("icon", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("color", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("board_slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("use", pydantic.Bool, false),
).Described("``use`` also activates the new project.")

var _ = Method("projects.create", ProjectsCreateParams, OptionalProjectResult, "Create a project from a name + folders; duplicate primary paths are refused (5063).")

var ProjectsUpdateParams = pydantic.Extend(ProjectIdParams, "ProjectsUpdateParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("icon", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("color", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("board_slug", pydantic.Nullable(pydantic.Str), nil),
).Described("Absent keys are left untouched; ``''`` clears ``color`` / ``icon``.")

var _ = Method("projects.update", ProjectsUpdateParams, ProjectResult, "Patch a project's display fields; answers the refreshed project.")

var ProjectsAddFolderParams = pydantic.Extend(ProjectIdParams, "ProjectsAddFolderParams", ParamsConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("is_primary", pydantic.Bool, false),
)

var _ = Method("projects.add_folder", ProjectsAddFolderParams, ProjectResult, "Attach a folder to a project (optionally as its primary path).")

var ProjectFolderParams = pydantic.Extend(ProjectIdParams, "ProjectFolderParams", ParamsConfig,
	pydantic.Req("path", pydantic.Str),
)

var _ = Method("projects.remove_folder", ProjectFolderParams, ProjectResult, "Detach a folder from a project.")

var _ = Method("projects.set_primary", ProjectFolderParams, ProjectResult, "Make one attached folder the project's primary path.")

var ProjectsArchiveParams = pydantic.Extend(ProjectIdParams, "ProjectsArchiveParams", ParamsConfig,
	pydantic.Def("restore", pydantic.Bool, false),
)

var _ = Method("projects.archive", ProjectsArchiveParams, ProjectsPayload, "Archive (or with ``restore`` un-archive) a project; answers the full listing.")

var _ = Method("projects.delete", ProjectIdParams, ProjectsPayload, "Delete a project and its folders; answers the full listing.")

var ProjectsSetActiveParams = pydantic.Extend(ProfileParams, "ProjectsSetActiveParams", ParamsConfig,
	pydantic.Def("id", pydantic.Nullable(pydantic.Str), nil),
).Described("No ``id`` (or null) clears the active project.")

var ActiveIdResult = pydantic.NewModel("ActiveIdResult", ClosedConfig,
	pydantic.Def("active_id", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("projects.set_active", ProjectsSetActiveParams, ActiveIdResult, "Switch (or clear) the active project for the profile.")

var ProjectsForCwdParams = pydantic.Extend(ProfileParams, "ProjectsForCwdParams", ParamsConfig,
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
).Described("Absent ``cwd`` resolves the gateway's default completion cwd.")

var ProjectsForCwdResult = pydantic.NewModel("ProjectsForCwdResult", ClosedConfig,
	pydantic.Def("project", pydantic.Nullable(ProjectInfo), nil),
	pydantic.Req("cwd", pydantic.Str),
	pydantic.Def("branch", pydantic.Str, ""),
)

var _ = Method("projects.for_cwd", ProjectsForCwdParams, ProjectsForCwdResult, "Which project (if any) owns a directory, plus the resolved cwd and its git branch.")

var RepoDiscoveryPolicy = pydantic.NewModel("RepoDiscoveryPolicy", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Req("roots", pydantic.List(pydantic.Str)),
	pydantic.Req("exclude_paths", pydantic.List(pydantic.Str)),
).Described("``methods_projects._repo_discovery_policy`` — the effective ``desktop.repo_scan_*`` config.")

var RepoDiscoveryPolicyParams = pydantic.NewModel("RepoDiscoveryPolicyParams", ParamsConfig,
	pydantic.Def("enabled", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("roots", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("exclude_paths", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("repo_scan_enabled", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("repo_scan_roots", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("repo_scan_exclude_paths", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("The policy the desktop scanned under (short or ``repo_scan_*`` long keys both accepted).")

var DiscoveredRepo = pydantic.NewModel("DiscoveredRepo", ClosedConfig,
	pydantic.Req("root", pydantic.Str),
	pydantic.Def("label", pydantic.Str, ""),
	pydantic.Def("sessions", pydantic.Int, 0),
	pydantic.Def("last_active", pydantic.Float, 0.0),
).Described("``methods_projects._discover_repos_payload`` row: a git root with session totals.")

var ProjectsDiscoverReposParams = pydantic.Extend(ProfileParams, "ProjectsDiscoverReposParams", ParamsConfig,
	pydantic.Def("scan", pydantic.Bool, false),
).Described("``scan`` asks the host to walk the policy roots itself (remote-gateway desktop).")

var ProjectsDiscoverReposResult = pydantic.NewModel("ProjectsDiscoverReposResult", ClosedConfig,
	pydantic.Req("repos", pydantic.List(DiscoveredRepo)),
	pydantic.Def("discovery_policy", pydantic.Nullable(RepoDiscoveryPolicy), nil),
)

var _ = Method("projects.discover_repos", ProjectsDiscoverReposParams, ProjectsDiscoverReposResult, "Repos for the desktop overview: scanned-from-disk (cached) ∪ session-derived.")

var RecordRepoItem = pydantic.NewModel("RecordRepoItem", ParamsConfig,
	pydantic.Req("root", pydantic.Str),
	pydantic.Def("label", pydantic.Nullable(pydantic.Str), nil),
)

var ProjectsRecordReposParams = pydantic.Extend(ProfileParams, "ProjectsRecordReposParams", ParamsConfig,
	pydantic.Def("repos", pydantic.Nullable(pydantic.List(pydantic.Union(RecordRepoItem, pydantic.Str))), nil),
	pydantic.Def("discovery_policy", pydantic.Nullable(RepoDiscoveryPolicyParams), nil),
).Described("Repos as ``{root, label}`` objects or bare root strings; entries without a root are skipped.")

var ProjectsRecordReposResult = pydantic.Extend(ProjectsDiscoverReposResult, "ProjectsRecordReposResult", ClosedConfig,
	pydantic.Req("accepted", pydantic.Bool),
)

var _ = Method("projects.record_repos", ProjectsRecordReposParams, ProjectsRecordReposResult, "Persist repo roots found by the client's (desktop-side) scan; return the merged list.")

var ProjectTreeSession = pydantic.Extend(StoredSessionRow, "ProjectTreeSession", OpenConfig,
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_projects._project_tree_row`` + ``project_tree.stamp_profile``: the minimal row the\nsidebar renders, stamped with the profile it belongs to.")

var ProjectTreeLane = pydantic.NewModel("ProjectTreeLane", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Def("path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("isMain", pydantic.Bool, false),
	pydantic.Def("isKanban", pydantic.Bool, false),
	pydantic.Def("isGit", pydantic.Bool, true),
	pydantic.Fac("sessions", pydantic.List(ProjectTreeSession), pydantic.NewList),
).Described("One branch / worktree / kanban lane inside a repo; ``sessions`` is empty unless hydrated.")

var ProjectTreeRepo = pydantic.NewModel("ProjectTreeRepo", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Def("path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Fac("groups", pydantic.List(ProjectTreeLane), pydantic.NewList),
	pydantic.Def("sessionCount", pydantic.Int, 0),
)

var ProjectTreeNode = pydantic.NewModel("ProjectTreeNode", ClosedConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Def("path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("color", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("icon", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("isAuto", pydantic.Bool, false),
	pydantic.Def("isNoProject", pydantic.Bool, false),
	pydantic.Def("sessionCount", pydantic.Int, 0),
	pydantic.Def("lastActive", pydantic.Float, 0.0),
	pydantic.Def("totalTokens", pydantic.Int, 0),
	pydantic.Def("totalCostUsd", pydantic.Float, 0.0),
	pydantic.Fac("repos", pydantic.List(ProjectTreeRepo), pydantic.NewList),
	pydantic.Fac("previewSessions", pydantic.List(ProjectTreeSession), pydantic.NewList),
	pydantic.Fac("sessionIds", pydantic.List(pydantic.Str), pydantic.NewList),
).Described("``project_tree._project_node`` — explicit, auto (git root) or the synthetic Home bucket.")

var ProjectsTreeParams = pydantic.Extend(ProfileParams, "ProjectsTreeParams", ParamsConfig,
	pydantic.Def("preview_limit", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("session_limit", pydantic.Nullable(pydantic.Int), nil),
)

var ProjectsTreeResult = pydantic.NewModel("ProjectsTreeResult", ClosedConfig,
	pydantic.Req("projects", pydantic.List(ProjectTreeNode)),
	pydantic.Def("active_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Fac("scoped_session_ids", pydantic.List(pydantic.Str), pydantic.NewList),
)

var _ = Method("projects.tree", ProjectsTreeParams, ProjectsTreeResult, "Project → repo → lane overview with counts and a few preview sessions per project.")

var ProjectsProjectSessionsParams = pydantic.Extend(ProfileParams, "ProjectsProjectSessionsParams", ParamsConfig,
	pydantic.Req("project_id", pydantic.Str),
	pydantic.Def("session_limit", pydantic.Nullable(pydantic.Int), nil),
)

var ProjectsProjectSessionsResult = pydantic.NewModel("ProjectsProjectSessionsResult", ClosedConfig,
	pydantic.Def("project", pydantic.Nullable(ProjectTreeNode), nil),
)

var _ = Method("projects.project_sessions", ProjectsProjectSessionsParams, ProjectsProjectSessionsResult, "Fully hydrated lanes for one project, from the same grouping as projects.tree.")

var PetInfoParams = pydantic.Extend(ProfileParams, "PetInfoParams", ParamsConfig,
	pydantic.Def("knownRevision", pydantic.Nullable(pydantic.Str), nil),
).Described("``knownRevision``: the spritesheet revision the caller already holds (send-once bytes).")

var PetInfoResult = pydantic.NewModel("PetInfoResult", OpenConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("displayName", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mime", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("spritesheetBase64", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("spritesheetRevision", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("spritesheetUnchanged", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("frameW", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("frameH", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("framesPerState", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("framesByState", pydantic.Nullable(pydantic.Dict(pydantic.Int)), nil),
	pydantic.Def("framesByRow", pydantic.Nullable(pydantic.Dict(pydantic.Int)), nil),
	pydantic.Def("loopMs", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("scale", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("stateRows", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("``server._pet_sprite_payload`` behind ``enabled``; every sprite field is absent when the pet\ndisplay is off, ``spritesheetBase64`` is elided when ``spritesheetUnchanged``.")

var _ = Method("pet.info", PetInfoParams, PetInfoResult, "Active pet for sprite renderers: spritesheet (base64) + frame geometry + state-row taxonomy.")

var PetInfoMetaResult = pydantic.NewModel("PetInfoMetaResult", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("displayName", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scale", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("spritesheetRevision", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("pet.info.meta", ProfileParams, PetInfoMetaResult, "Cheap active-pet metadata used to avoid full payload refreshes.")

var PetCellsParams = pydantic.Extend(ProfileParams, "PetCellsParams", ParamsConfig,
	pydantic.Def("state", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("graphics", pydantic.Bool, false),
).Described("``graphics`` opts into the kitty payload when the TTY speaks it; ``cols`` overrides the width.")

var PetCellsResult = pydantic.NewModel("PetCellsResult", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("displayName", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("state", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("frameMs", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("frames", pydantic.Nullable(pydantic.Union(pydantic.List(pydantic.List(pydantic.List(pydantic.List(pydantic.Int)))), pydantic.List(pydantic.Str))), nil),
	pydantic.Def("scale", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("graphics", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("imageId", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("color", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("rows", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("placeholder", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("Unicode: ``frames`` is frame → row → cell ``[tr,tg,tb,ta, br,bg,bb,ba]``; kitty (``graphics``\nset): ``frames`` are transmit escapes and ``placeholder`` the text grid.")

var _ = Method("pet.cells", PetCellsParams, PetCellsResult, "Half-block cell frames (or a kitty placement) for one pet state.")

var PetGalleryParams = pydantic.Extend(ProfileParams, "PetGalleryParams", ParamsConfig,
	pydantic.Def("localOnly", pydantic.Bool, false),
)

var PetGalleryEntry = pydantic.NewModel("PetGalleryEntry", ClosedConfig,
	pydantic.Req("slug", pydantic.Str),
	pydantic.Req("displayName", pydantic.Str),
	pydantic.Req("installed", pydantic.Bool),
	pydantic.Def("spritesheetUrl", pydantic.Str, ""),
	pydantic.Def("curated", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("generated", pydantic.Bool, false),
)

var PetGalleryResult = pydantic.NewModel("PetGalleryResult", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("active", pydantic.Str, ""),
	pydantic.Fac("pets", pydantic.List(PetGalleryEntry), pydantic.NewList),
)

var _ = Method("pet.gallery", PetGalleryParams, PetGalleryResult, "Petdex gallery + local install state (installed-only offline); localOnly skips the remote manifest.")

var PetSlugParams = pydantic.Extend(ProfileParams, "PetSlugParams", ParamsConfig,
	pydantic.Req("slug", pydantic.Str),
)

var PetSlugResult = pydantic.NewModel("PetSlugResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("slug", pydantic.Str),
	pydantic.Def("displayName", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("pet.select", PetSlugParams, PetSlugResult, "Adopt a pet: install (if needed) + activate; writes display.pet.* to config.")

var _ = Method("pet.remove", PetSlugParams, PetSlugResult, "Uninstall a pet (delete its directory); if it was active, turn the display off.")

var PetRenameParams = pydantic.Extend(PetSlugParams, "PetRenameParams", ParamsConfig,
	pydantic.Req("name", pydantic.Str),
)

var _ = Method("pet.rename", PetRenameParams, PetSlugResult, "Rename a pet's display name + realign its slug/dir; follows the active slug in config.")

var PetExportResult = pydantic.NewModel("PetExportResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("filename", pydantic.Str),
	pydantic.Req("zipBase64", pydantic.Str),
)

var _ = Method("pet.export", PetSlugParams, PetExportResult, "Export an installed pet as a re-importable .zip.")

var PetThumbParams = pydantic.Extend(PetSlugParams, "PetThumbParams", ParamsConfig,
	pydantic.Def("url", pydantic.Nullable(pydantic.Str), nil),
).Described("``url``: spritesheet source for a not-yet-installed pet.")

var PetThumbResult = pydantic.NewModel("PetThumbResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("slug", pydantic.Str),
	pydantic.Def("dataUri", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("pet.thumb", PetThumbParams, PetThumbResult, "Idle-frame PNG data URI for the picker (desktop CSP breaks CDN <img>).")

var _ = Method("pet.disable", ProfileParams, OkResult, "Turn the pet display off from the desktop picker.")

var PetScaleParams = pydantic.Extend(ProfileParams, "PetScaleParams", ParamsConfig,
	pydantic.Def("scale", pydantic.Any, nil),
)

var PetScaleResult = pydantic.NewModel("PetScaleResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("scale", pydantic.Float),
)

var _ = Method("pet.scale", PetScaleParams, PetScaleResult, "Persist display.pet.scale (clamped to engine bounds) from the desktop slider.")
