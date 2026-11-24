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
| `H*` | `settings-docs.md` §5.5 |
| gap table | `cli-tui-gap.md`, re-run in `~/.cache/icebath-redesign/handoff2/gap-table.md` |

Words in the UI: "background service", never "gateway", "tier" or "lane"; one name per feature (`settings-docs.md` §7).

Every item below was checked against the code on 2026-11-23. Items that were open in the list before and are done in the code were taken out with the closed ones.

## Settings

- [ ] Typed readers replace raw map reads, group by group. Every setting has a typed handle in `pkg/settings`; readers outside it still take values from a loaded config map, a passed section or the process environment.
  - `pkg/provider` (model, providers, auxiliary and mixes reads in the auth, switch and inventory code).
  - `pkg/media`: the speech generators take a request map; they get a typed request built from the handles.
  - `pkg/tools`, `pkg/agent`, `pkg/cron`, `pkg/mcp`, `pkg/gatewayrun`, `pkg/kanban`, `pkg/plugins`, `pkg/commands`, `pkg/tuiserver`, `pkg/skills`, `pkg/lsp`, `cmd/icebath`.
  - Chat app adapters: their env knobs and their raw `settings.Resolve` list readers.
  - Env readers in `pkg/tui` (37 `os.Getenv` calls), `pkg/app` (48), `pkg/gatewayrun` (12), `pkg/i18n` (1): each is a setting, a real process fact, or dead.
  - Writers of registered keys outside `settings.Set`.
  - Config pushed into the process environment for a reader elsewhere (`pkg/config/facade_env.go` `TerminalConfigEnvMap`, `pkg/provider/env_loader.go` `applyTerminalConfigToEnv`, 125 `os.Setenv` calls outside tests): the reader takes the handle and the push goes. Then the rows of the env table in `pkg/config/env.go` that nothing reads.
- [ ] `pkg/config/defaults.go` is written by `pkg/settings/internal/docgen` from the registry instead of by hand.
- [ ] The duplicates of `settings-docs.md` §3 that are left: one setting and one place per feature.

## Terminal UI

- [ ] `pkg/tui` split into subpackages: session, composer, transcript, dialogs, hubs, commands, setup, media, extras. It holds 151 source files in one package today.
- [ ] Commands whose only home in the TUI is the run-a-command screen get a screen or an action of their own (`handoff2/gap-table.md`, 35 rows marked PARTIAL):
  - `approvals suggest`, `approvals test`
  - `auth priority`, `auth reset`
  - `backup`
  - `config edit`, `config path`, `config env-path`
  - `kanban tail`, `kanban watch`
  - `plugins adopt`, `plugins pack` (show, install, export), `plugins validate`
  - `sessions recover`
  - `skills publish`
  - `uninstall`
  - `verify`
  - chat app tools: `whatsapp`, `whatsapp-cloud`, `photon` (setup, status, install-sidecar, telemetry), `feishu comment-rules`, `google_chat oauth`
  - session flags of `icebath` and `icebath chat` that have no control inside a running session
- [ ] Duplicate entry points collapse (`cli-tui-gap.md` §2). Shadowed options come back. Name collisions get one meaning each.
  - `/verbose` stands beside `/detail` in `pkg/commands/table.go`.
  - `/fast`: the command table takes `normal|fast|auto|cold|ultrafast|status` and `--global`; the TUI takes `normal|fast|ultrafast|status|on|off|toggle` (H3).
  - 35 alias lists in `pkg/commands/table.go` (`clear`/`reset`, `compose`, `fork`, `compact`, …), which the TUI lists and follows (`commandAliases` in `pkg/tui/slash.go`).

## Help and words

- [ ] Wrong help texts that are left:
  - H9: `pkg/i18n/locales/en.yaml` says "raise `max_iterations`"; the setting is `agent.max_turns`.
  - H10: the bundled `icebath-agent` skill's `slash-commands.md` lists a `/indicator` look and a `/verbose` step that do not exist. The fix needs the packed skill tree rebuilt (`go generate`), which the no-build rule keeps from agents.
  - H11: tips t180 and t181 call Camofox "built in"; it is a plugin.
- [ ] Ten strings of the core catalog are still English in 13 to 16 of the 16 languages: `gateway.model.err_once_local`, `gateway.model.local_next_turn`, and eight under `gateway.kanban.wake.*`.

## Project-wide cleanup

- [ ] Package splits: `pkg/gateway` (692 source files at the top level), `pkg/provider` (468), `pkg/tools` (395), `pkg/gatewayrun` (352), `pkg/agent` (330), `pkg/state` (218), `pkg/tuiserver` (188), `pkg/config` (79).
- [ ] Python-port leftovers are removed: `py*` shims where the stdlib does the job (`pkg/internal` holds 66 packages), `pythonPath` naming (25 files), reference-mirroring compatibility code, `*_export.go` shims with no caller.
- [ ] Code that only serves older installs, seen while checking this list: `gateway migrate-legacy` (`pkg/commands/cli_tree_a.go`, `pkg/gatewaycli/gateway_cmd_run.go`).
- [ ] Files that do not belong in the source tree: `pkg/gatewayrun/default/` (a profile home written by a test run), `pkg/pm/.lock.json.lock`, `pkg/mcp/TODOS.md`.
- [ ] Performance: cold start, time to first token, streaming throughput, idle CPU, RSS and background service start time were measured (`~/.cache/icebath-redesign/handoff2/lead.md`). Still to fix: 131 ms and 69 MB spent in package init (1,329 `regexp.MustCompile` at package level, 288 of them in `pkg/tools`), and the size of the binary (dead-code elimination is off for exported methods because `reflect.Value.MethodByName` is reachable through `text/template`). The idle CPU fix is in and not yet measured.
- [ ] Agent loop sweep with the scripted model (`fakellm.py` `SCENARIOS`): retries, fallback, compression, context overflow, tool errors, parallel tool calls, reasoning, delegation, memory, scheduled jobs.
- [ ] Drive each chat-app adapter with a scripted inbound message and check its replies.

## Proof still to take

These are done in the code and have no capture. Each needs a built binary and the PTY harness.

- [ ] F04: two builds side by side on one home.
- [ ] F08: a prompt on the top row with no sticky header over it.
- [ ] Setup parts (`/setup agent`, `voice`, `terminal`, `tools`, `chat-apps`, `quick`, `blank`, `import`, `helpers`) at 120x40 and 80x24.
- [ ] Get started opening by itself on an empty home; your own endpoint with a key.
- [ ] The `login.exhausted` question; a real browser sign in.
- [ ] Fallback models, model mixes, memory provider, projects, hooks, secret stores and the autofill vault screens.
- [ ] Every System section at 80x24; a profile switch through the after-exit queue; quit-and-run from the after-exit screen.
- [ ] Chat apps with a configured platform and a pairing request.

Closed items are in this file's git history.
