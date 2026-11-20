# Projects and folders (`icebath project`)

A **Project** is a named workspace that can span several folders — a repo, a monorepo, a
scratch directory, a sibling repo you always open together. Projects are named
workspaces a chat can move into, and a kanban task linked to a project with a
primary folder defaults to a worktree and branch under that repo.

Projects are **not** git concepts and not config keys: they live in their own SQLite store,
one per profile (`$ICEBATH_HOME/projects.db`; `~/.icebath` by default, `%LOCALAPPDATA%\icebath`
on Windows), created on first use. `icebath -p <profile> project list` shows another
profile's projects.

## Command surface

`icebath project <sub> --help` is authoritative for flags.

| Subcommand | What it does |
|---|---|
| `create <name> [folders...]` | New project. First folder becomes the primary; `--slug`, `--primary PATH`, `--description`, `--icon`, `--color`, `--board SLUG`, `--use` (make it active). |
| `list` (alias `ls`) | Projects, oldest first; `*` marks the active one; `--all` includes archived. |
| `show <project>` | Slug, id, name, about/board/primary, folder list (`*` = primary). |
| `add-folder <project> <path>` | Add a folder; `--label`, `--primary` (make it primary). |
| `remove-folder <project> <path>` | Drop a folder (repoints primary if it was primary). |
| `rename <project> <name>` | Change the display name. The slug does **not** change. |
| `set-primary <project> <path>` | Primary folder; the path must already be in the project. |
| `use [<project>]` | Set the active project; with no argument, clear it. |
| `archive <project>` / `restore <project>` | Hide / unhide a project (reversible; nothing on disk moves). |
| `bind-board <project> [<board>]` | Record a kanban board on the project; omit the board to unbind (see Kanban below). |

`<project>` is an id (`p_1a2b3c4d`, exact) or a slug (any case). The CLI does **not**
accept the display name; get the slug from `icebath project list`.

## Semantics worth knowing

- **Slug** — derived from the name (lowercased, runs of non-alphanumerics collapse to `-`,
  capped at 64 chars) unless `--slug` is given. An invalid `--slug` exits **2**. A slug that
  is already taken gets a suffix (`team-docs-2`, …), an explicit `--slug` included, so read
  the slug back from the `Created project <slug>` line.
- **Folders** — stored as absolute paths; `~`, relative paths and trailing slashes are
  normalized on every command. Paths are not checked or created. The primary is the first
  folder given, or `--primary`; it anchors kanban worktrees and the folder a TUI chat
  moves to.
- **One project per primary folder** — `create` refuses a primary path that is already the
  primary of another (non-archived) project (`folder already belongs to project '<slug>'
  …`).
- **Active project** — one per-profile pointer that `use` sets and clears: the `*` in
  `icebath project list`. It is a marker,
  not a scope: kanban tasks and chat workspaces do not follow it.
- **Archived projects** are hidden from `list` (use `--all`) and stop claiming their folders
  for path → project resolution; they still resolve by id or slug.
- **Path → project resolution** — a folder owns itself and everything under it, and the
  longest matching folder wins, so a nested project resolves to the innermost one.
- **Deleting** — the CLI has no delete; `archive` is the reversible way to retire a project.
- **Exit codes** — `0` ok; `1` no such project, or the path is not a folder of that project
  (`remove-folder`, `set-primary`); `2` invalid input (bad slug or board slug, empty name,
  duplicate primary folder, bad arguments).

## What projects actually drive

- **Status line naming.** A chat's workspace (its cwd) resolves to the owning project
  by the path rule above, and the TUI status line names it.
- **The `desktop_project` tool** (toolset `project`, folded in by the TUI gateway;
  not in the default CLI or messaging toolsets). `action: create|switch|list` with `name` /
  `path`; unlike the CLI it also matches display names, and archived projects. In a live
  TUI chat, `create` and `switch` move *that chat's* workspace to the project's
  primary folder — the intended way to move a chat between projects,
  not `cd`. The tool itself leaves the active-project pointer alone; a project with no folders cannot move the chat, so it sets the pointer instead. A
  primary folder that does not exist on disk is skipped silently: the call succeeds but the
  chat stays put. `create` with a `path` that is already another project's primary reuses
  that project instead of failing.
- **Kanban.** A task is linked to a project when it is created with
  `icebath kanban create … --project <id-or-slug>`, or when its board carries a project binding;
  tasks on such a board inherit the
  project unless created with `--workspace scratch`. A linked task whose project has a
  primary folder defaults to a worktree at `<primary>/.worktrees/<task-id>` on branch
  `<project-slug>/<task-id>[-<title-slug>]` instead of the generic `wt/<task-id>`; the
  primary must be a git repo when the worker starts. An unknown `--project` value does not
  fail: the task is created unlinked.
  `icebath project bind-board` does not set that board binding. It records the board on the
  project and, when the board exists and the project has a primary folder, sets the board's
  default workdir to it (only a bare `--workspace worktree` task uses it);
  an existing board binding is left as it was. Unbinding (`bind-board <project>` with
  no board) clears only the project's record; the board keeps its default workdir and any
  board binding.

## Common tasks

```bash
# Make a project for a repo and start using it
icebath project create "Icebath Agent" ~/Work/icebath-agent --use

# A workspace spanning two repos (first given = primary)
icebath project create "Payments" ~/Work/payments-api ~/Work/payments-web

# Add a folder later / move the primary
icebath project add-folder payments ~/Work/payments-infra --label infra
icebath project set-primary payments ~/Work/payments-web

# What is active (archived included), and what a project contains
icebath project list --all
icebath project show payments

# Retire one without losing it
icebath project archive payments        # hidden from list / path resolution
icebath project restore payments

# A project-linked kanban task (worktree + deterministic branch)
icebath kanban create "Wire the webhook" --project payments
```

## Troubleshooting

- **`no such project: <Name>`** — the CLI takes an id or slug, not the display name.
- **Kanban task has no `project_id` / branch** — `--project` did not match an id or slug in
  this profile (no error is raised), or the board was only bound with
  `icebath project bind-board`. Pass a valid `--project <slug>`. A `project_id` with no branch on a non-`dir` task means the
  project has no primary folder
  (`icebath project add-folder <project> <path> --primary`).
- **"switched project but this chat didn't move"** — `icebath project use` only moves the
  pointer. Switch from inside the chat (the `desktop_project` tool), or start the chat in the
  project's folder; check that the primary folder exists.
- **`create` refused: folder already belongs to project X** — the folder is already X's
  primary (possibly an automatic sidebar group that was given a color or icon). Use X
  (`icebath project show X`, `rename` it if the name is wrong) rather than adding the folder
  to a second project.
- **A project's folders look "unprojected"** — the project is archived;
  `icebath project restore <project>`.
- **Two profiles show different projects** — expected: `projects.db` is per profile.

## See also

- `references/cli-reference.md` — `icebath project` in the CLI command map.
- `references/background-systems.md` — kanban overview.
- `references/project-context-files.md` — a different feature: `AGENTS.md` / `.icebath.md` /
  `CLAUDE.md` instruction files that load into a session.
- `icebath project --help`
