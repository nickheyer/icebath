# Contributor Quick Reference

For occasional contributors and PR authors. Full developer docs: the repository at https://github.com/nickheyer/icebath

### Project Layout

```
icebath/
├── cmd/icebath/          # Binary entry point (main.go)
├── pkg/agent/            # AIAgent — conversation loop (conversation_loop.go), system prompt, compression, routing, delegation
├── pkg/tools/            # One file per tool, registry (def.go), dispatch (dispatch.go), toolsets (toolsets.go)
├── pkg/commands/         # CLI command tree (cli_tree_*.go) and the slash command registry (table.go)
├── pkg/config/           # Defaults (defaults.go), env var definitions (env.go), skins
├── pkg/state/            # SQLite session store (db.go)
├── pkg/tui/              # Terminal UI cards and commands
├── pkg/ui/               # UI kit: renderer, window manager, keys, themes
├── pkg/tuigateway/       # Wire contract between the terminal UI and the session host
├── pkg/gateway/          # Messaging gateway
│   └── platforms/        # Platform adapters (telegram, discord, etc.)
├── pkg/gatewayrun/       # Gateway runner and its slash command handlers
├── pkg/cron/             # Job store (store.go) and scheduler (scheduler.go)
├── pkg/skills/           # Skills system; bundled/ and optional/ skill trees
├── pkg/plugins/          # Plugin registry; pkg/pluginsdk is the plugin author library
├── site/                 # icebath.app site bundle
└── Makefile              # make build, make test
```

Config: `~/.icebath/config.yaml` (settings), `~/.icebath/.env` (API keys) — both under `$ICEBATH_HOME` when it is set.

### Adding a Tool

Two edits. A tool file registers itself from its `init()` through
`tools.Register`, but a tool is only *exposed* to an agent once its name
appears in a toolset.

**1. Create `pkg/tools/your_tool.go`:**
```go
package tools

import (
	"context"
	"os"
)

var exampleToolSchema = Dict(
	"name", "example_tool",
	"description", "...",
	"parameters", Dict("type", "object", "properties", Dict(
		"param", Dict("type", "string", "description", "..."),
	)),
)

func handleExampleTool(ctx context.Context, call *Call) string {
	param, _ := call.Args["param"].(string)
	return JSON(Dict("success", true, "data", "...", "param", param))
}

func init() {
	Register(Def{Name: "example_tool", Toolset: "example", Schema: exampleToolSchema,
		Handler: handleExampleTool, Check: func(env *Env) bool { return os.Getenv("EXAMPLE_API_KEY") != "" },
		RequiresEnv: []string{"EXAMPLE_API_KEY"}, Description: "Example tool"})
}
```

**2. Wire it into a toolset in `pkg/tools/toolsets.go`** — add the name to
`CoreTools` (every platform) or to a specific entry of `Toolsets`.

All handlers return JSON strings (`JSON`, `ErrorResult`). Use `home.Dir(ctx)`
from `pkg/home` for paths, never hardcode `~/.icebath`. For custom/local-only
tools, write a plugin (next section) instead of editing core.

### Writing a Plugin

A third-party plugin is a directory in `~/.icebath/plugins/<name>/` holding a
`plugin.yaml` and a guest built from Go that imports
`github.com/nickheyer/icebath/pkg/pluginsdk`. The guest runs apart from
icebath's own code, a program in its own process and a WebAssembly module in
a sandbox, and talks to icebath over a framed wire. Python plugin source is
not loaded. `icebath plugins --help` lists the validate, doctor and pack verbs;
`icebath plugins enable <name>` activates a plugin.

**Two forms**, picked by `runtime` in the manifest:

| `runtime` | Guest | Build | Reach |
|---|---|---|---|
| `program` | native executable | `go build -o <entry> .` | an operating system process started with the profile environment, free to do what any program can; what it asks of the host still follows `grants` |
| `wasm` | WebAssembly module | `GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o <entry>.wasm .` | only what `grants` allow, memory and run time capped by `limits`; runs only while icebath calls it |

`main` is `func main() { pluginsdk.Main() }` in both forms.

**Entry point.** An `init` function names the register function; icebath runs
it once per load with the `Host` it registers on:

```go
func init() {
	pluginsdk.Register(func(h pluginsdk.Host) error {
		_, err := h.RegisterTool(pluginsdk.Tool{Name: "hello", Toolset: "hello", Description: "Say hello",
			Schema:  map[string]any{"name": "hello", "description": "Say hello", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			Handler: func(ctx context.Context, call pluginsdk.ToolCall) (string, error) { return `{"ok": true}`, nil }})
		return err
	})
}

func main() { pluginsdk.Main() }
```

**Manifest (`plugin.yaml`).**

| Field | Meaning |
|---|---|
| `name`, `version`, `description`, `author`, `license`, `homepage`, `tags` | identity shown in listings |
| `runtime`, `entry` | `program` or `wasm`, and the program (plus arguments) or the module file inside the plugin directory |
| `kind` | `standalone` (default, loaded when enabled), `backend`, `platform`, `model-provider`, `exclusive` (loaded only by the category that selects it) |
| `provides` | categories the plugin is found under: `memory_provider` (`memory.provider`), `context_engine` (`context.engine`), `cron_scheduler` (`cron.provider`), `computer_use_provider` (`computer_use.backend`); a `kind: exclusive` plugin that lists nothing is offered to every category |
| `grants.read`, `grants.write` | path roots the host reads and writes for the plugin, absolute or starting with `~`, `$ICEBATH_HOME`, `$PLUGIN_DIR`, `$PLUGIN_DATA` |
| `grants.net` | host names `Host.HTTP` may reach, exact, `*.suffix` or `*` |
| `grants.env` | environment variable names `Host.Env` may read |
| `limits.memory_mb`, `limits.call_seconds` | wasm memory cap (16 to 4096, default 256) and run time of one call (default 120) |
| `requires_env`, `optional_env`, `config_schema`, `requires_icebath`, `requires_plugins`, `capabilities` | credentials asked at install, typed settings, version floor, advisory dependencies, gated host powers |
| `provides_tools`, `provides_hooks`, `provides_middleware`, `provides_locales` | what the plugin declares it registers, checked by `plugins doctor` |
| `requires_auth`, `oauth` | sign in the host attaches to `Host.ProviderRequest` |

A value the plugin was not granted is refused by the host: `Host.Env` of an
ungranted name, `Host.ReadFile` outside the granted roots and `Host.HTTP` to
an ungranted host all answer an error. A plugin never reads the credential
store; a provider call that needs the user's sign in goes through
`Host.ProviderRequest`.

**Host methods** (`pkg/pluginsdk/sdk.go`).

| Group | Methods |
|---|---|
| Identity | `PluginID`, `PluginKey`, `Name`, `Dir`, `ProfileName`, `Manifest` |
| Settings | `Config`, `SetConfig`, `HasPlugin`, `HasCapability` |
| Registration | `RegisterTool`, `RegisterHook`, `RegisterMiddleware`, `RegisterCommand`, `RegisterCLICommand`, `RegisterProvider`, `RegisterLoginBackend`, `RegisterTerminalBackend`, `RegisterCronScheduler`, `RegisterPlatform`, `RegisterSlackActionHandler`, `RegisterApprovalTransport`, `RegisterWidget`, `RegisterSkill`, `RegisterLocale`, `RegisterLocaleDir`, `RegisterSystemPromptSection`, `RegisterAutomationBlueprint`, `RegisterAuxiliaryTask`, `RegisterRedactionPatterns`, `Subscribe`, `Emit`, `OnUnload` |
| Session | `DispatchTool`, `CallMCP`, `InjectMessage`, `InjectMessageAt`, `PostMessage`, `AskChoice`, `SideQuestion`, `SessionCwd`, `SessionSurface`, `CurrentCronExecution`, `Facade`, `Call` |
| Granted input and output | `ReadFile`, `WriteFile`, `ListDir`, `Remove`, `HTTP`, `ProviderRequest`, `Env` |

A registration returns a `*Handle`; `Handle.Dispose` removes it before unload,
and unloading the plugin removes every registration it made. Model provider
profiles are named with `pluginsdk.RegisterProfile` from `init`.

**Provider kinds** of `Host.RegisterProvider(kind, *pluginsdk.Object)`. An
`Object` carries `Attrs` icebath reads and `Methods` it calls by name; a
member a kind leaves optional gets its base behaviour, a missing required one
fails the registration. A method reads the object icebath addressed with
`pluginsdk.Self(ctx)`.

| Kind | Required members | Selected by |
|---|---|---|
| `image_gen`, `video_gen` | `name`, `generate` | `image_gen.provider`, `video_gen.provider` |
| `web_search` | `name`, `is_available` (`search`, `extract` with `supports_search`, `supports_extract`) | web backend settings |
| `browser` | `name`, `is_available`, `create_session`, `close_session`, `emergency_cleanup` | `browser.cloud_provider` |
| `browser_page_backend` | `name`, `has_page`, `evaluate` | the browser vault |
| `terminal_environment` | `name`, `is_available`, `create_environment` | `terminal.backend` |
| `secret_source` | `name`, `fetch` (answers `pluginsdk.Record{Type: "FetchResult"}` or a mapping with its fields) | `secrets.<name>.enabled` |
| `tts`, `transcription` | `name`, `synthesize` / `transcribe` | `tts.provider`, `stt.provider` |
| `delegation` | `name`, `route_task` | `delegation.backend` |
| `context_engine` | `name`, `update_from_response`, `should_compress`, `compress` | `context.engine` |
| `context_reference` | `prefix`, `autocomplete`, `expand` | `@prefix:` in a message, for the profile that loaded the plugin |
| `memory` | `name`, `is_available`, `initialize`, `get_tool_schemas` | `memory.provider` |
| `login_backend` | `name`, `prefix`, `list_items`, `get_meta`, `resolve_password` | `vault.<name>.enabled` |
| `cron_scheduler` | `name`, `start` | `cron.provider` |
| `computer_use` | `name`, `is_available`, `create_backend` | `computer_use.backend` |

**Typed surfaces** build the provider object for you:

- `pluginsdk.Platform` (`RegisterPlatform`): a messaging platform. `Adapter`
  builds one `pluginsdk.Adapter` per connection (`Connect`, `Disconnect`,
  `Send` required, every other hook optional); the `pluginsdk.Gateway` it
  receives takes inbound messages (`HandleMessage`) and connection state
  (`MarkConnected`, `MarkDisconnected`, `SetFatalError`). Setup is declarative:
  `SetupInstructions` and `SetupVars` are the questions icebath asks in the
  terminal UI and saves as profile environment variables; a plugin has no
  terminal, so a setup function is refused. Platforms need the plugins tier
  (the full and noextras builds).
- `pluginsdk.LoginBackend` (`RegisterLoginBackend`): a browser vault backend
  listing `VaultItem`s and resolving passwords, one time codes and secrets by
  handle.
- `pluginsdk.TerminalBackend` (`RegisterTerminalBackend`): `CreateEnvironment`
  answers a `pluginsdk.TerminalEnvironment` whose `Execute` runs one command
  (`TerminalCommand` in, `TerminalResult` out) and whose `Cleanup` releases it.
  Optional: `ExecPrefix` (an argv on the icebath machine that runs its
  remainder inside the environment, with `SupportsStreams` on the backend),
  `FetchFile`, `FetchRealpath`. The one thing that does not cross: a running
  foreground command cannot be handed to the background, since icebath holds
  no process of it; it runs to its end or its timeout.
- `pluginsdk.CronScheduler` (`RegisterCronScheduler`): `Start` receives the
  stop `*pluginsdk.Event` and a `pluginsdk.Cron` whose `ClaimFire`,
  `FireClaimed`, `FireDue` and `RecoverInterrupted` run a job through the
  run and delivery path of icebath. Only the plugin `cron.provider` names
  gets these operations.
- `pluginsdk.CLICommand` (`RegisterCLICommand`): an `icebath <name>`
  subcommand whose `Run` answers its output and exit code.
- `pluginsdk.Widget` (`RegisterWidget`): see `references/tui-widgets.md`.
- `pluginsdk.Message` (`PostMessage`): a posted message with `Edit`,
  `Refresh` and `Result`. `pluginsdk.HostObject` is any other object living
  in icebath, read with `Get` and called with `Call`; `pluginsdk.HostFunc` is
  a callback icebath handed over.

`icebath plugins doctor <dir>` loads the plugin under a throwaway home, with
`Host.HTTP` denied, and reports what it registered.

### Adding a Slash Command

1. Add a `def(...)` entry to `Builtin` in `pkg/commands/table.go` (description key `slash.<name>.description` in the locale catalogs)
2. Add its terminal UI handler to the command tables in `pkg/tui/slash.go` and `pkg/tui/slash_*.go`; a command the UI does not answer itself goes to the gateway as `slash.exec`, which the session authority serves in `pkg/gateway/session_commands_exec.go`
3. (Optional) Add a gateway handler: a `case` in `commandHandlerFor` and the name in `plainCommands` (or `idleCommands`) in `pkg/gatewayrun/run_busy_dispatch.go`

All consumers (help text, autocomplete, Telegram menu, Slack mapping) derive from the central registry automatically.

### Agent Loop (High Level)

```
(*AIAgent).RunConversation():          # pkg/agent/turn_facade.go
  1. Build system prompt               # BuildSystemPrompt, pkg/agent/system_prompt.go
  2. Loop while iterations < max:      # pkg/agent/conversation_loop.go
     a. Call LLM (OpenAI-format messages + tool schemas)
     b. If tool_calls → dispatch each via tools.HandleFunctionCall() → append results → continue
     c. If text response → return
  3. Context compression triggers automatically near token limit
```

### Testing

Go tests sit next to the code they test (`*_test.go`) and need the
`goolm` build tag. Run every build and test under a memory limit.

```bash
make test                                                     # full suite: go test -tags goolm ./...
GOMEMLIMIT=6GiB go test -tags goolm ./pkg/tools/              # one package
GOMEMLIMIT=6GiB go test -tags goolm ./pkg/tools/ -run TestX   # one test
GOMEMLIMIT=6GiB go test -tags goolm ./pkg/tools/ -run TestX -v -count=1   # verbose, no cached result
```

- Every test binary gets a private user home under the temp dir
  (`pkg/internal/homeguard`; packages outside `pkg` import `pkg/testguard`) —
  tests never touch the real `~/.icebath/`.
- `./setup-icebath.sh --runtime-only` builds the binary from a checkout and
  downloads the modules the tests need (`go mod download`).
- The Go version comes from `go.mod`; `make build` writes `dist/icebath`.
- **Windows:** run the same `go test` commands in PowerShell. See `references/windows-quirks.md`.
- After adding or upgrading a dependency (`go get`), run `go mod tidy` and
  commit `go.mod` with `go.sum`.
  Do not edit `go.sum` by hand.

Host-specific tests run on the real host. Put them in a file whose name
carries the target (`_windows_test.go`, `_linux_test.go`, `_darwin_test.go`)
or a `//go:build` line, such as `//go:build windows && arm64`.
Do not fake the host by swapping `runtime.GOOS` checks or platform probes.

### System prompt's execution-environment block

Factual host/backend guidance (OS, `$HOME`, cwd, terminal backend, shell)
is emitted by `BuildEnvironmentHints` in `pkg/agent/prompt_builder.go`. The key
invariant for prompt authors: with a **remote** terminal backend
(`docker, modal, daytona, ssh, vercel_sandbox`, plus plugin backends whose terminal provider flag `is_remote` is true), host info is
suppressed and *every* file tool runs inside the backend container — the
prompt must never describe the host the agent can't touch.

### Commit Conventions

```
type: concise subject line

Optional body.
```

Types: `fix:`, `feat:`, `refactor:`, `docs:`, `chore:`

### Key Rules

- **Never break prompt caching** — don't change context, tools, or system prompt mid-conversation
- **Message role alternation** — never two assistant or two user messages in a row
- Use `home.Dir(ctx)` from `pkg/home` for all paths (profile-safe)
- Config values go in `config.yaml`, secrets go in `.env`
- New tools need a `Check` func so they only appear when requirements are met
