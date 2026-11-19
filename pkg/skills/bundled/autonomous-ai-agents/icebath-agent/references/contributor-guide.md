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
tools, write a plugin with `pkg/pluginsdk` (`Host.RegisterTool`) and drop it
with its `plugin.yaml` in `~/.icebath/plugins/` instead of editing core —
`icebath plugins --help` lists the validate, doctor and pack verbs.

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
