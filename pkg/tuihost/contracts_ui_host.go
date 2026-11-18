package tuihost

import "github.com/nickheyer/icebath/pkg/internal/pydantic"

var NoParams = pydantic.NewModel("NoParams", ParamsConfig)

var LinesResult = pydantic.NewModel("LinesResult", ClosedConfig,
	pydantic.Req("lines", pydantic.List(pydantic.Str)),
).Described("Text lines in the profile's language, shown as they come.")

var SnapshotArgsParams = pydantic.NewModel("SnapshotArgsParams", ParamsConfig,
	pydantic.Fac("args", pydantic.List(pydantic.Str), pydantic.NewList),
).Described("``args`` are the words after the /snapshot subcommand.")

var _ = Method("snapshot.list", NoParams, LinesResult, "The profile's quick state snapshots as a numbered table.")

var _ = Method("snapshot.create", SnapshotArgsParams, LinesResult, "Take a quick state snapshot, the args joined as its label.")

var _ = Method("snapshot.prune", SnapshotArgsParams, LinesResult, "Keep the newest N quick snapshots, twenty when no count is given.")

var SnapshotTargetResult = pydantic.NewModel("SnapshotTargetResult", ClosedConfig,
	pydantic.Req("id", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("lines", pydantic.List(pydantic.Str)),
).Described("``id`` is the snapshot a restore names by id or list number; null with ``lines`` when none is named or the number is off.")

var _ = Method("snapshot.target", SnapshotArgsParams, SnapshotTargetResult, "Resolve the snapshot a /snapshot restore names.")

var ProfilesExportParams = pydantic.NewModel("ProfilesExportParams", ParamsConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("output", pydantic.Nullable(pydantic.Str), nil),
).Described("``name`` defaults to the connection's profile; ``output`` is an absolute archive path, the export directory when null.")

var ProfilesExportResult = pydantic.NewModel("ProfilesExportResult", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
)

var _ = Method("profiles.export", ProfilesExportParams, ProfilesExportResult, "Write a profile (config, skills, theme) to a shareable archive.")

var ProfilesImportParams = pydantic.NewModel("ProfilesImportParams", ParamsConfig,
	pydantic.Req("archive", pydantic.Str),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
).Described("``archive`` is an absolute path; ``name`` overrides the profile name the archive carries.")

var ProfilesImportResult = pydantic.NewModel("ProfilesImportResult", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("wrapper", pydantic.Nullable(pydantic.Str)),
).Described("``wrapper`` is the alias script created for the new profile, null when none was made because the name collides with a command or the script could not be written.")

var _ = Method("profiles.import", ProfilesImportParams, ProfilesImportResult, "Import a shared profile archive as a new profile with its wrapper alias.")

var ToolsListParams = pydantic.NewModel("ToolsListParams", ParamsConfig,
	pydantic.Def("platform", pydantic.Str, "cli"),
)

var _ = Method("tools.list", ToolsListParams, LinesResult, "Every toolset and MCP tool of a platform with its enabled or disabled status.")

var WorktreeRef = pydantic.NewModel("WorktreeRef", ClosedConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("branch", pydantic.Str),
	pydantic.Req("repo_root", pydantic.Str),
)

var WorktreeLaunch = pydantic.NewModel("WorktreeLaunch", ClosedConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("branch", pydantic.Str),
)

var WorktreeStatusParams = pydantic.NewModel("WorktreeStatusParams", ParamsConfig,
	pydantic.Req("cwd", pydantic.Str),
	pydantic.Req("launch_cwd", pydantic.Str),
	pydantic.Req("pid", pydantic.Int),
).Described("``launch_cwd`` and ``pid`` find the tree a -w launch of that process locked.")

var WorktreeStatusResult = pydantic.NewModel("WorktreeStatusResult", ClosedConfig,
	pydantic.Req("repo_root", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("launch", pydantic.Nullable(WorktreeLaunch)),
)

var _ = Method("worktree.status", WorktreeStatusParams, WorktreeStatusResult, "Repository of a folder and the tree a -w launch locked.")

var WorktreeCwdParams = pydantic.NewModel("WorktreeCwdParams", ParamsConfig,
	pydantic.Req("cwd", pydantic.Str),
)

var WorktreeListResult = pydantic.NewModel("WorktreeListResult", ClosedConfig,
	pydantic.Req("repo_root", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("lines", pydantic.List(pydantic.Str)),
).Described("``lines`` are git worktree list, empty when git failed.")

var _ = Method("worktree.list", WorktreeCwdParams, WorktreeListResult, "The git worktrees of the folder's repository.")

var WorktreePruneParams = pydantic.NewModel("WorktreePruneParams", ParamsConfig,
	pydantic.Req("cwd", pydantic.Str),
	pydantic.Def("dry_run", pydantic.Bool, false),
	pydantic.Def("keep", pydantic.Nullable(pydantic.Str), nil),
).Described("``keep`` is the path of the tree in use, never reclaimed.")

var WorktreeKept = pydantic.NewModel("WorktreeKept", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("reason", pydantic.Str),
)

var WorktreePruneResult = pydantic.NewModel("WorktreePruneResult", ClosedConfig,
	pydantic.Req("repo_root", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("actions", pydantic.List(pydantic.Str)),
	pydantic.Req("preserved", pydantic.List(WorktreeKept)),
	pydantic.Req("error", pydantic.Nullable(pydantic.Str)),
)

var _ = Method("worktree.prune", WorktreePruneParams, WorktreePruneResult, "Reclaim stale trees and merged branches, or name what a dry run would reclaim.")

var WorktreeCreateParams = pydantic.NewModel("WorktreeCreateParams", ParamsConfig,
	pydantic.Req("cwd", pydantic.Str),
	pydantic.Def("name", pydantic.Str, ""),
	pydantic.Req("pid", pydantic.Int),
).Described("``pid`` is the process the tree is locked for, the client that settles it.")

var WorktreeCreateResult = pydantic.NewModel("WorktreeCreateResult", ClosedConfig,
	pydantic.Req("repo_root", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("tree", pydantic.Nullable(WorktreeRef)),
	pydantic.Req("lines", pydantic.List(pydantic.Str)),
).Described("``lines`` are the creation report; ``tree`` is null when the creation failed.")

var _ = Method("worktree.create", WorktreeCreateParams, WorktreeCreateResult, "Create an isolated git worktree on a new branch.")

var _ = Method("worktree.record", WorktreeRef, pydantic.NewModel("WorktreeRecordResult", ClosedConfig, pydantic.Req("ok", pydantic.Bool)),
	"Remember a tree a session moved into, so its resume finds it.")

var _ = Method("worktree.settle", WorktreeRef, LinesResult, "Remove a clean tree and its branch, or say why it is kept.")
