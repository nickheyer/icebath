# TODOS

Goal: icebath's terminal UI is a daily driver. One consistent interface, every capability reachable from inside it, no surprises.

Rules from Nick:
- The one surface is the window manager TUI in `pkg/tui`.
- Parity with hermes-agent is no longer a target anywhere.
- No new tests. Tests that only pin replaced internals are deleted; tests of behaviour that still exists are updated. Only the lead runs tests, subagents never do.
- No commits; Nick commits.

Close an item with `[x]` and the capture that proves it. Captures come from the PTY harness with the scripted model (`~/.cache/icebath-redesign/scripts/drv.py`, `scripts/parity/fakellm.py`; setup in `~/.cache/icebath-redesign/audit/hands-on.md`).

Plan: `/home/nick/.claude/plans/snuggly-sparking-feather.md`. Audit reports with steps and file:line for every ID below: `~/.cache/icebath-redesign/audit/`:

| IDs | Report |
|---|---|
| `F*` | `hands-on.md` |
| `R*` `C*` `K*` | `input-nav.md` |
| `M*` | `multisession.md` §10 |
| `H*` | `settings-docs.md` §5.5 |
| gap table | `cli-tui-gap.md` |
| logins | `logins.md` |

Words in the UI: "background service", never "gateway", "tier" or "lane"; one name per feature (`settings-docs.md` §7).

## Done

- [x] Crash-looping `icebath-gateway.service` removed. The service pointed at a `go run` binary deleted on exit; systemd had retried it 289 times.
- [x] Service definitions that would run a binary from a Go build scratch dir or the `go run` cache (`go-build/xx/<hash>-d/icebath`) are refused: systemd and launchd install and refresh, and the Windows launcher (`pkg/gateway/service_launchd.go`, `service_schtasks.go`).
- [x] systemd stops restarting when the binary is missing (`RestartPreventExitStatus=78 203`, `pkg/gateway/service_systemd_unit.go`).
- [x] Quick setup no longer offers the background service after "Skip" for messaging (`pkg/app/setup_quick.go`).

## Phase 0: safety and breakage

- [x] F02 Esc in a setup picker cancels the flow. Nothing preselected is applied and the TUI says "Setup stopped" (`pkg/tuiserver/setup_flow.go`, `pkg/provider/setup_tui_flows.go`, `setup_host.go`; `~/.cache/icebath-redesign/captures/p0/10_checklist.txt`, `11_after_esc.txt`, `14_draft_kept.txt` "Provider setup skipped").
- [x] F10 / K6 / K7 Approval dialog fixes:
  - Deny is the default row.
  - Answer keys are ignored for 600 ms after the dialog appears and during a burst of typing.
  - "Always allow" has no single key.
  - Dialogs are sticky overlays (`wm.Sticky`): an outside click does nothing, and the palette or another overlay opens on top and gives the dialog back when it closes (`pkg/ui/wm/input.go`, `pkg/tui/dialogs.go`).
  - Captures: `~/.cache/icebath-redesign/captures/p0/03_approval.txt`, `04_click_outside.txt`, `05_palette_over.txt`, `06_back_to_dialog.txt`, `07_denied.txt`.
- [x] F09 The session's YOLO state is read when the session binds and after each toggle. "yolo on" shows in red in the bar (never dropped for room) and on the session frame (`~/.cache/icebath-redesign/captures/p0/02_yolo.txt`).
- [x] F03 The title notch is drawn only on the focused card, so focus shows without colour (`pkg/ui/wm/render.go`; `~/.cache/icebath-redesign/captures/p0/01_start.txt`, `03_approval.txt`). A toast when focus moves to a new card comes with the Phase 3 keymap.
- [x] F04 `make dev` and a background service of another build:
  - An idle owner is replaced silently (existing).
  - A busy one gets an in-TUI dialog "Background service runs another build · Restart it / Keep it" (`Host.ReplaceOwner`, `pkg/tui/gatewayrecovery.go`).
  - A service started by a `go run` build exits 5 s after its last client instead of 600 s (`gateway.GoRunExecutable`, `pkg/gatewayrun/run_idle_exit.go`).
  - Stale translations that named `icebath gateway restart` are removed.
  - Capture of two builds side by side still to take.
- [x] The several-homes log line is plain words at info level: "Another icebath home already runs a background service on this computer; this home gets its own." (`pkg/gatewayrun/run_host_gateway.go`).
- [x] F11 / K17 The Ctrl+C "press again" toast lasts exactly the 2 s window. Ctrl+D quits only on an empty prompt with no turn running (`pkg/tui/app.go`).
- [x] F08 No sticky header while a prompt sits on the top row, so the previous prompt is never painted over it (`pkg/tui/transcriptview.go` `stickyPrompt`). Capture still to take.
- [x] F05 Slash names that start with what was typed lead the list, so `/se` shows `/setup` (`pkg/tui/slash_args.go`; `~/.cache/icebath-redesign/captures/p0/12_slash_se.txt`).
- [x] F19 "quit" is the last palette command, so a stray Enter never quits. A title that equals or starts with the query now leads every looser match (`pkg/ui/wm/palette.go`).
- [x] F16 (part) Stored sessions carry their preview (`pkg/gateway/session_listing.go` `listingPreview`), and stored sessions with no messages are hidden. Readability at 80 cols comes with the Sessions hub in Phase 4.
- [x] F22 A prompt typed before any model is set up says so, stays in the prompt and opens model setup (`ui.SetupRequired`; `~/.cache/icebath-redesign/captures/p0/13_prompt_before_setup.txt`, `14_draft_kept.txt`).

## Phase 1: foundations

### 1a. Settings registry, `pkg/settings` (`settings-docs.md` §6)

- [x] `pkg/settings`: 956 typed entries in 12 groups (55 everyday, 901 advanced), resolving default < global < session < env < managed with a source for each value (`~/.cache/icebath-redesign/handoff/settings.md`).
- [x] `settings.Set` is the only writer. Both `config.set` implementations, the whitelist, `icebath config set`, the background service's per-session controls and `/voice` go through it. `Was` migrations run in `config migrate` and lazily.
- [x] (part) Voice state left the process env and `gateway_voice_mode.json`. TTS, STT and image generation have one provider key each, and image generation has defaults. The other duplicates of `settings-docs.md` §3 remain.
- [x] (backend) "Detail level" is one control over tool progress and fold state; the TUI commands still to move onto it.
- [x] (part) The busy mode defaults to `queue` everywhere.
- [x] (part) `ICEBATH_VOICE*`, `FAL_IMAGE_MODEL`, `OPENROUTER_IMAGE_*` and `STT_*_MODEL` folded into keys. Env readers in `pkg/tui`, `pkg/app`, `pkg/i18n` and `pkg/gatewayrun` remain (listed in the handoff). Nothing calls `settings.EndSession` yet.
- [ ] Typed readers replace raw map reads, group by group.

### 1b. Named logins (`logins.md` §3)

- [x] Pool rows are logins with name, account and rotate; origin owned/linked comes from the source. auth.json is v2. Config: `logins.<p>.default` (id or `@rotate`), `on_exhausted`, `strategy` (`~/.cache/icebath-redesign/handoff/logins.md`).
- [x] Resolution order: `--api-key`, session pin, the parent session's login at `session.create`, profile default, today's behaviour. All of it lives in `LoadSelectedPool`, which every runtime pool load uses.
- [x] (backend) A pinned login never falls through. Headless runs fail with `login_exhausted` unless `on_exhausted: rotate`; the TUI gets a `login.exhausted` event. The TUI dialog for it is still to do.
- [x] `model.save_key` no longer writes `.env`, and `model.disconnect` removes one login (all with `all`). The OAuth sign-ins and Copilot setup in `icebath model` still write the old stores.
- [x] auth.json v1→v2 migration:
  - Codex, xAI and Anthropic PKCE tokens move into their rows and the old copies are tombstoned.
  - Root store first, with a backup; a second run does nothing.
  - `icebath auth migrate --dry-run` exists. It runs at background service and TUI server start (`EnsureLoginsMigrated`).
  - Dry run on Nick's real home: one `anthropic-token` login linked to `env:ANTHROPIC_TOKEN`, made the default; auth.json unchanged.
- [ ] Restart the background service automatically after a migration that changed something.
- [x] Bugs fixed:
  - Removing one Codex login no longer signs out another.
  - `ANTHROPIC_TOKEN` no longer shadows a pasted key.
  - Media tools use the profile default login instead of `Peek()`.
  - The tests that pinned the old behaviour are updated.
- [x] RPCs `login.list/add_key/oauth_start/oauth_poll/link/rename/remove/set_default/set_rotation/convert` and `session.set_login`. `session.create` and `session.info` carry the login. `icebath auth add --name/--rotate/--default`, `use`, `rename`, `rotation`, `migrate`; `logout` removes the default login only.
- [ ] A settings registry entry for `logins.*`, so `config.set logins.*` works.

### 1c. Commands

- [x] `commands.NewCLI().Tree()` plus the `commands.tree` RPC: 443 paths, of which 400 run inside, 21 after exit (`profile rename` and `profile delete` can move the live home) and 22 are internal (`chat`, `sessions browse` and `acp` open a nested terminal UI) (`~/.cache/icebath-redesign/handoff/commands.md`).
- [x] The local owner (native transport plus `session:operator`, decided by the server) runs `/pause`, `/restart`, `/platform`, `/agents`, `/plugins enable|disable`, kanban writes, `/usage reset`, `/suggestions` and `/memory` writes.
  - Cron writes are allowed for everyone; the "while subscribed" refusal was a port leftover.
  - Refusals that remain have plain hints.

## Phase 2: several sessions per space (`multisession.md` §6-7)

- [x] Step 0, state bugs fixed:
  - M1: `renewOnRedraw` is gone. The config cache is cleared when the session or config.yaml changes.
  - M2/M3: one queue edit index lives on the queue; a deleted item reads as nil.
  - M4: a session switch keeps the draft's chips with its text.
  - M5: the turn clock restarts on a switch.
  - M7: the profile home is resolved once.
  - M8: `slashFlight` is gone; a slash answer is dropped only when its session leaves the view.
  - M12: queue journal failures no longer idle the session in view.
  - M14: the argument cache is keyed per session.
- [ ] Step 1: explicit transitions replace `afterUpdate` diffing; delete `seen*`, `refreshGen` (M23) and the arm flags.
- [x] Step 2, done differently from the audit's plan:
  - `app` is now the per-session object. It embeds a shared `*shell` (window manager, connection, keys, terminal, theme, exit state, the session list), so `a.wm` / `a.gw` keep compiling (`pkg/tui/shell.go`).
  - The shell is the Bubble Tea model.
  - Commands a session issues are tagged (`sessionMsg`), so their messages come back to it.
- [ ] Step 3: registries lifted to the shell (M15 attach hold, M16 create slot, M18 subscriptions on transport loss).
- [x] Step 4: routing.
  - Gateway events go by session id; `gateway.*`, skin and sessions-changed events go to every session.
  - Server requests go by `session_id`, submits by the composer's session.
  - Each session recovers its own session on reconnect (M24).
  - M6, M17, M19 remain.
- [x] Step 5: spaces.
  - `workspace.new` (`+`, alt+shift+n) opens a space with a fresh session; `session.beside` and `workspace.close` are added (F01, K3).
  - Session cards have unique keys and can be closed while more than one is open; closing detaches the session (M26).
  - The bar shows every space's chip, and a question waiting in another session is named in the bar notice.
  - One pet (in the active card), one wake listener.
  - Background-session notices skip sessions open in a card.
  - Captures: `~/.cache/icebath-redesign/captures/p2/01_plus.txt`, `02_space2_reply.txt`, `03_back_space1.txt`.
- [ ] Dialogs drawn inside the card that asked (today they share the sticky overlay stack) (M20). The voice device gets an owner session.
- [x] Step 6: several session cards per space stream at once (F17) (`~/.cache/icebath-redesign/captures/p2/04_beside.txt`, `05_both_streaming.txt`, `06_both_done.txt`). Widgets following the active card is still to do.
- [x] Step 7: the layout and each card's session are saved to `<home>/tui-layout.json` on exit and restored at start; each card resumes its own session (F18; `~/.cache/icebath-redesign/captures/p2/07_restored.txt` before the rekey fix, `08_restored_fixed.txt`). M25 (active-session file follows focus) remains.
- [ ] Server cleanups:
  - sid on `session.reclaimed` and `approval.cancelled`
  - a side-effect-free revision read (M22)
  - re-attach every session on reconnect
  - session limits
- [ ] `pkg/tui` split into subpackages: session, composer, transcript, dialogs, hubs, commands, setup, media, extras.
- [ ] Remaining: M3, M9, M10, M11, M13, M14, M21.

## Phase 3: input and keys (`input-nav.md` §5-6)

- [x] ctrl+x prefix key (`pkg/ui/wm/input.go`, the `leader` context in `pkg/ui/keys/actions.go`). It works in every window and sends ctrl+x itself on a second press. A strip lists the next keys, one line per group (sessions, windows, layout, spaces, prompt), each set of four directions as one item (`p3/01_leader_strip.txt`, `02_leader_n.txt`, `03_leader_c.txt`, `04_leader_1.txt`).
- [x] Conflicts removed:
  - The ctrl+x triple; `tui.cut`, `tui.queue.delete` and `tui.sessions` move to the leader.
  - alt+g, alt+k, alt+v, ctrl+k, ctrl+p and ctrl+r on the TUI.
  - alt+enter on `terminal.new`.
  - shift+up/down on composer select.
  - alt+f and alt+d on the window manager.
  - The prompt gets alt+b/f/d and alt+backspace (K1, K14, F14).
- [ ] Still to do: K2 (voice key), K4 (Option-as-alt setting), K5 (room newline), K15 (zoom focus), terminal cards passing only the remaining alt keys.
- [x] Composer history ring (`pkg/ui/cards/composer.go`):
  - Recall clamps at both ends.
  - Edits to recalled entries are kept until the next send (R4).
  - Up and Down use visual rows and enter entries at the matching end (R5, R6).
  - A recalled entry never opens the completion popup (R7, F07).
  - ctrl+p / ctrl+n always recall.
  - ctrl+r is a reverse search: enter keeps the match, esc restores (R13).
  - Up and Down reach the queue only from an empty prompt or while editing a queued input; it clamps, edits are kept while moving, and the history continues past it (R1-R3).
  - Captures: `~/.cache/icebath-redesign/captures/p3/10_up1.txt` … `16_search_kept.txt`.
- [ ] Ring: R8-R12, R14, F30, one shared history store for every card.
- [x] Completion: Tab accepts (F06, C2). Enter takes a suggestion only after moving the highlight or while typing a command name, otherwise it sends what is shown (C1). shift+tab no longer belongs to completion (C4).
- [ ] Completion: C3 stale rows, C5, C6 invalidation on session events, C7 room cards, F31 `@file:` directories.
- [x] Esc removes one layer per press, in this order: selection, then a queued or recalled prompt (back to the draft), then a running turn (stopped). There is a 300 ms guard after an Esc that closed something.
  - On an idle empty prompt, a first Esc arms the rewind for one second.
  - Esc never clears the draft.
  - The double-Enter stop and the double-Esc draft discard are gone (K9, K10, K22, F25, R10).
- [x] ctrl+c with a typed draft puts it in the stash ("Draft put aside; ctrl+s brings it back") instead of discarding it (K11). Typing closes the stash panel and goes into the prompt; delete is ctrl+d / delete, not `d` (K8).
- [ ] View mode for the transcript and terminal scrollback: search, jumping between prompts, copy (K12, K18, K19).
- [ ] Dialogs:
  - field typing survives dismissal (K20)
  - one line-editor keymap for every field (K21)
- [x] The session footer shows the keys for the current state (idle, busy, popup, search, history, queued), read from the keymap (K13). Captures: `p3/24_legend_idle.txt`, `25_legend_history.txt`, `26_legend_popup.txt`.
- [ ] About 39 older locale strings still name keys in their text (K16).

## Phase 4: hubs

- [x] Shared hub keys, the same in every screen of things to manage (`hub` context, `pkg/tui/hub_logins.go` `registerHubKeys`): enter use, a add, e edit, r rename, x remove, d default, t toggle. Built on `cards.List`.
- [ ] Move the older screens to the hub keys: skills (x install), plugins (enter toggle), MCP (space/e toggle, x remove), and the session switcher (d delete).
- [x] Settings screen (`/settings [group]`, aliases `/config` and `/preferences`, palette "settings", `pkg/tui/hub_settings.go`):
  - The 12 groups lead to each group's settings, with label, value, source mark and what it does.
  - Tab shows the advanced settings; the detail pane has the default, source, when it applies, env and shortcut.
  - Enter changes a setting by kind: a switch flips, a choice comes from a list with meanings, a secret is masked, anything else is a prompt with the current value. x resets to the default. A toast says when the change takes effect.
  - Contracts `settings.*` are in `pkg/tuigateway/settings.go`; the background service answers in plain JSON.
  - Captures: `p4/22_settings_groups.txt`, `23_settings_voice.txt` (one place for voice and TTS), `24_settings_toggle.txt` (writes `voice.speak_replies`).
- [ ] Settings: setup sections run on the same engine (F20, F21, F34). Map settings are edited in config.yaml for now. Rows for session scope.
- [x] Logins screen (`/logins`, aliases `/login`, `/accounts`, palette "logins"):
  - Every provider's logins with marks for this session, default and rotation, plus kind, where the secret lives, account and status.
  - A detail pane shows usage windows, fingerprint, sharing and the selection.
  - Keys: d default, t rotation, r rename, x remove (confirm, hints shown).
  - a add: pick a provider, then paste a key (masked, optional name), sign in by browser or device code (link and code shown, input prompted), or link an env var.
  - Enter pins the login for this session through `config.set model … --provider --login`, since the background service has no `session.set_login`.
  - Captures: `~/.cache/icebath-redesign/captures/p4/01_logins.txt` … `09_use_personal.txt` (two Anthropic logins, work and personal).
- [ ] `session.set_login` on the background service, so a login can be pinned without touching the model. Pinning a login of another provider opens the model picker today.
- [ ] Model picker: "via ‹login›", a "+ add login" row, ctrl+d opens Logins. The `login.exhausted` dialog (switch / wait / rotate).
- [x] (part) Session switcher:
  - b opens a session beside this one and c opens it in a new space; a card that already shows it gets the focus instead (`shell.openSession`).
  - ctrl+n opens a new session card beside this one.
  - Live sessions carry their preview (`state.PreviewSQL`).
  - Capture: `p4/31_open_beside.txt`.
- [ ] Sessions: rename any session, pin, archive, prune, export; hub keys (x remove instead of d); readable at 80 cols (F15, F16).
- [ ] System:
  - background service
  - doctor
  - log files
  - storage
  - packages
  - updates
  - security audit
- [ ] Scheduled jobs.
- [ ] Plugins & skills, MCP.
- [ ] Profiles.
- [ ] Kanban.
- [ ] Chat apps.

## Phase 5: every CLI command in the TUI (`cli-tui-gap.md`)

- [ ] The palette lists backend commands and commands that take arguments (`pkg/tui/slash.go:314-322`).
- [x] Palette → "run an icebath command" (`pkg/tui/runcommand.go`):
  - It lists every user-facing CLI command from `commands.tree` with its help.
  - Picking one opens a prompt with the command line to finish.
  - The command runs in a terminal window; when it ends its output opens in a pager.
  - Captures: `~/.cache/icebath-redesign/captures/p5/03_run_pick_status.txt`, `08_status_output.txt`.
- [ ] Run command: a form built from the flags (types, choices, required) instead of a free command line; show the usage line, not only the command.
- [x] After-exit queue: commands of class `after_exit` are held in memory by the TUI (`tui.TakeAfterExit`, the TUI runs in the launcher process) with the reason shown. On exit code 0 or 42 the launcher runs them in the terminal and then opens icebath again with the layout restored, except after `uninstall` and `gateway stop`; the background service is stopped first for `gateway`, `sessions` and `uninstall` commands. After a crash or ctrl+c the commands that did not run are printed (`pkg/app/tui_launch.go` `runAfterExit`).
- [ ] Show the queued commands in the TUI and let them be removed before quitting.
- [ ] Duplicate entry points collapse (`cli-tui-gap.md` §2). Shadowed options come back. Name collisions get one meaning each.
- [ ] The gap table re-run shows zero NONE rows for user-facing commands.

## Phase 6: help and first run

- [x] `/help`, F1 and `ctrl+x ?` open one searchable overlay (`pkg/tui/hub_help.go`) listing every key for the session, every ctrl+x key and every command once (local and backend merged), each with what it does, two columns, chords comma separated. Enter puts a command in the prompt ("enter insert" shows on command rows). It replaces the ~270-line transcript dump (F13). The stale Esc and ctrl+c texts are rewritten. Capture: `~/.cache/icebath-redesign/captures/p6/01_help.txt`.
- [ ] The wrong help texts H1-H17 are fixed, help text is rewritten to the rules, and jargon is removed (F28).
  - [x] Key descriptions shown in `/help` fit one line at 120 columns; "space" replaces "workspace" in every key and palette text; the welcome line, "Ask me anything…" and the `/help` placeholder are gone; the banner shows the short session id; the redaction notice is one line; the stash toast names the bound key.
  - [x] Registry Short texts are one clause of at most 70 characters (about 500 entries in `pkg/settings`).
- [ ] Translations for strings added during the redesign (they fall back to English until then).
- [ ] First run: one screen to add a login and pick a model; nothing installs without an explicit choice.
- [ ] README is trimmed. The settings reference and `cli-config.yaml.example` are generated from the registry.
- [ ] Rendering and chrome:
  - F24 spinner verb in the reasoning text
  - F26 misleading footer status
  - F27 top bar jitter
  - F29 title scramble animation
  - F32 tool rows
  - F33 `##` headings render raw
  - F35 heavy chrome on small terminals

## Open bugs found in the cleanup review

- [ ] A pool write keeps every in-memory row, so a login removed while a session holds its pool comes back on the next write (`pkg/provider/store.go` pool merge; needs tombstones).
- [ ] `refreshDefaultFollowers` checks that a session is idle, then swaps its pool from the RPC goroutine; a turn that starts in between races (`pkg/tuiserver/methods_logins.go`).
- [ ] Only one session login pin is saved in `model_config`, so pins for other providers are lost on resume.
- [ ] The Logins screen compares the provider name with the pool key, so custom providers always open the model picker (`pkg/tui/hub_logins.go`).
- [ ] Browser sign-in from the Logins screen opens the browser on the background service's machine.
- [ ] Session-scope `settings.set` on the local backend stores a value nothing reads; a session-scope reset of busy, verbose, show_reasoning or service_tier leaves the live value in place.
- [ ] Migration lets an existing config key win over a `.env` value that used to win at runtime (for example `ICEBATH_LANGUAGE`).
- [ ] On managed installs `settings.Set` refuses global writes, so per-chat `/voice` choices are not saved.
- [ ] `a.post` messages from inline images, placed images and widgets are not tagged with their session, so a background card's errors print in the active transcript.
- [ ] A queue journal change from any session reloads the queue and drops an edit on one of the background service's own rows (`pkg/tui/queue.go`).
- [ ] The exit summary names the first session even when its card was closed.
- [ ] `CachedTree` serialises only its own builds; other `NewCLI` calls in the service process can race with it.
- [ ] `login.add_key` failures reuse error code 4002, which also means "unknown provider" (`pkg/tuiserver/methods_complete.go`).

## Phase 7: project-wide cleanup

- [ ] Package splits: `pkg/gateway` (691 files at the top level), `pkg/provider` (467), `pkg/tools` (395), `pkg/agent` (329), `pkg/state`, `pkg/config`.
- [ ] Python-port leftovers are removed: `py*` shims where the stdlib does the job, `pythonPath` naming, reference-mirroring compatibility code.
- [ ] Every CLI command prints "⚠ install out of sync (ffmpeg…; node…; npm…; ripgrep…) — this unknown-managed install must rebuild the artifact to fix" on a dev or unmanaged install. Say it once, in plain words, where it matters.
- [ ] Performance: measure cold start, time to first token, streaming throughput, idle CPU, RSS and background service start time, then fix the worst. The binary is 331 MB.
- [ ] Agent loop sweep with the scripted model (`fakellm.py` `SCENARIOS`): retries, fallback, compression, context overflow, tool errors, parallel tool calls, reasoning, delegation, memory, scheduled jobs.
- [ ] Drive each chat-app adapter with a scripted inbound message and check its replies.

Closed items from the parity era are in this file's git history (before 2026-11-21).
