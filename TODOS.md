# TODOS

Goal: icebath's terminal UI is a daily driver. One consistent interface, every capability reachable from inside it, no surprises.

Rules from Nick:
- The one surface is the window manager TUI in `pkg/tui`.
- Parity with hermes-agent is no longer a target anywhere.
- No new tests. Tests that only pin replaced internals are deleted; tests of behaviour that still exists are updated. Only the lead runs tests, subagents never do.
- No commits; Nick commits.

Close an item with `[x]` 

## Settings

- [ ] Typed readers replace raw map reads, group by group. Every setting has a typed handle in `pkg/settings`; readers outside it still take values from a loaded config map, a passed section or the process environment.
  - `pkg/provider`: the resolver (`newResolver`) and the readers behind `LoadConfigSafe`, the model switch code, and the setup flows that save a whole config document.
  - `pkg/media`: wake word, voice, image and video generation.
  - `pkg/tools`, `pkg/agent`, `pkg/mcp`, `pkg/kanban`, `pkg/plugins`, `pkg/commands`, `pkg/tuiserver`, `pkg/skills`, `pkg/memory`, `pkg/codeexec`, `pkg/browser`, `pkg/gateway`, `cmd/icebath`; in `pkg/cron`, the readers that still load the config.
  - Chat app adapters whatsapp, whatsappcloud, photon, matrix, yuanbao, and the home channel of every platform: their env knobs and their raw `settings.Resolve` list readers.
  - Env readers in `pkg/tui` (14 `os.Getenv` calls), `pkg/app` (32): each is a setting, a real process fact, or dead.
  - Writers of registered keys outside `settings.Set`.
  - Config pushed into the process environment for a reader elsewhere (`pkg/config/facade_env.go` `TerminalConfigEnvMap`, `pkg/provider/env_loader.go` `applyTerminalConfigToEnv`, 88 `os.Setenv` calls outside tests): the reader takes the handle and the push goes. Then the rows of the env table in `pkg/config/env.go` that nothing reads.
- [ ] `pkg/config/defaults.go` is written by `pkg/settings/internal/docgen` from the registry instead of by hand.
- [ ] The duplicates of `settings-docs.md` §3 that are left: one setting and one place per feature.

## Terminal UI

- [ ] `pkg/tui` split into subpackages: session, composer, transcript, dialogs, hubs, commands, setup, media, extras. It holds 151 source files in one package today.
- [ ] Launch flags of `icebath` and `icebath chat` with no control inside a running session. `/options` covers checkpoints, safe mode, ignore rules, ignore user config, pass session id, the turn limit, the run budget, skills, the folder and the endpoint; the other flags are not checked against it yet.
- [ ] Duplicate entry points collapse (`cli-tui-gap.md` §2): two ways in that write different state for one thing write the same state. Shadowed options come back. Name collisions get one meaning each. No command, alias or spelling is removed; aliases are features.

## Help and words

- [ ] Wrong help texts that are left:
  - H10: the packed skill tree (`pkg/skills/bundled.tar.zst`) still holds the old `slash-commands.md` of the `icebath-agent` skill. It needs the rebuild (`go generate`), which the no-build rule keeps from agents.

## Project-wide cleanup

- [ ] Package splits: `pkg/gateway` (692 source files at the top level), `pkg/provider` (468), `pkg/tools` (395), `pkg/gatewayrun` (352), `pkg/agent` (330), `pkg/state` (218), `pkg/tuiserver` (188), `pkg/config` (79).
- [ ] Python-port leftovers are removed: `py*` shims where the stdlib does the job (`pkg/internal` holds 66 packages), `pythonPath` naming (25 files), reference-mirroring compatibility code, `*_export.go` shims with no caller.
- [ ] Delete everything intended to support legacy versions of icebath or migrate functionality to current version, including but not limited to: `pkg/gateway/service_retired.go` and its callers, the `ALTER TABLE` steps of the hosted rooms stores, the pickle migration in `pkg/gateway/platforms/matrix/e2ee_goolm.go`, `legacyCustomPoolMatches` in `pkg/provider/pool_persist.go`, `fallback_model` read beside `fallback_providers` (`pkg/provider/fallback_routes.go`), and the flat `moa` block (`legacyMoAKeys`, `pkg/provider/inventory.go`). No migration is ever added.
- [ ] Performance: cold start, time to first token, streaming throughput, idle CPU, RSS and background service start time were measured (`~/.cache/icebath-redesign/handoff2/lead.md`). Still to fix: 131 ms and 69 MB spent in package init (1,329 `regexp.MustCompile` at package level, 288 of them in `pkg/tools`), and the size of the binary (dead-code elimination is off for exported methods because `reflect.Value.MethodByName` is reachable through `text/template`). The idle CPU fix is in and not yet measured.
- [ ] Drive each chat-app adapter with a scripted inbound message and check its replies.

