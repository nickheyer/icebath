# Hermes CLI Reference

Live sources when anything looks stale: `icebath --help`, `icebath <command> --help`,
https://hermes-agent.nousresearch.com/docs/reference/cli-commands

### Global Flags

```
icebath [flags] [command]        (no subcommand = interactive chat)

  --version, -V             Show version
  -z, --oneshot PROMPT      One-shot: print ONLY the final response (for scripts/pipes)
  -m MODEL  --provider P    Model/provider override for this invocation
  -t, --toolsets LIST       Comma-separated toolsets for this invocation
  --resume, -r SESSION      Resume session by ID or title
  --continue, -c [NAME]     Resume by name, or most recent session
  --worktree, -w            Isolated git worktree mode (parallel agents)
  --skills, -s SKILL        Preload skills (comma-separate or repeat)
  --profile, -p NAME        Use a named profile
  --yolo                    Skip dangerous command approval
  --tui / --cli             Force the Ink TUI / classic REPL
  --ignore-rules            Skip AGENTS.md/SOUL.md/memory/skill injection
  --safe-mode               Disable ALL customizations (troubleshooting)
  --pass-session-id         Include session ID in system prompt
```

### Chat

```
icebath chat [flags]
  -q, --query TEXT          Single query, non-interactive
  --image PATH              Attach a local image to a single query
  -Q, --quiet               Suppress banner, spinner, tool previews
  --checkpoints             Enable filesystem checkpoints (/rollback)
  --max-turns N             Cap tool-calling iterations
  --source TAG              Session source tag (default: cli)
```
(plus the global flags above)

### Configuration

```
icebath setup [section]      Wizard (model|tts|terminal|gateway|tools|agent)
icebath model                Interactive model/provider picker
icebath fallback [add|remove|list]  Fallback provider chain
icebath config [show|edit|get|set|unset|path|env-path|check|migrate]
icebath login / logout       OAuth sign-in / clear stored auth
icebath doctor [--fix]       Check dependencies and config
icebath status [--full]      Component summary (--full: every section)
```

### Tools & Skills

```
icebath tools [list|enable NAME|disable NAME]   Per-platform toolsets (curses UI with no args)

icebath skills list|browse|search QUERY|inspect ID
icebath skills install ID    Hub identifier OR a direct https://…/SKILL.md URL
icebath skills config        Enable/disable skills per platform
icebath skills check|update|uninstall|publish PATH
icebath skills tap add REPO  Add a GitHub repo as a skill source
icebath bundles              Skill bundles (one /<name> alias loads several skills)
```

### MCP Servers

```
icebath mcp add NAME (--url or --command) | remove | list | test NAME
icebath mcp catalog | install NAME     Curated catalog install
icebath mcp configure NAME             Toggle tool selection
icebath mcp-serve                      Run Hermes as an MCP server (mcp-serve plugin)
```
Details (transport, tool discovery, catalog): `references/native-mcp.md`.

### Gateway (Messaging Platforms)

```
icebath gateway run|install|start|stop|restart|status|setup
```

20+ platforms: Telegram, Discord, Slack, WhatsApp (Baileys + Business Cloud API), iMessage (Photon — `icebath photon setup`), Signal, Email, SMS, Matrix, Mattermost, Teams, LINE, SimpleX, ntfy, Google Chat, Home Assistant, DingTalk, Feishu, WeCom, Weixin, API Server, Webhooks. Open WebUI connects via the API Server adapter. Most adapters ship under `plugins/platforms/`.
Docs: https://hermes-agent.nousresearch.com/docs/user-guide/messaging/

### Sessions

```
icebath sessions list|browse|rename ID TITLE|delete ID|export OUT|prune|stats
```

### Cron / Webhooks

```
icebath cron list|create SCHED|edit ID|pause|resume|run ID|remove|status
    Schedules: '30m', 'every 2h', '0 9 * * *', ISO timestamp
icebath webhook subscribe NAME|list|remove NAME|test NAME
```
Webhook payloads/routes: `references/webhooks.md`.

### Profiles

```
icebath profile list|create NAME (--clone|--clone-all|--clone-from)|use|show|delete
icebath profile rename A B | alias NAME | export NAME | import FILE
icebath profile migrate-identity A B   Retry a completed rename's session/routing identity migration
```

### Credentials & Pools

```
icebath auth                 Interactive credential manager
icebath auth add [PROVIDER]  Add OAuth or API-key credential (nous, openai-codex, qwen-oauth, …)
icebath auth list|remove P IDX|reset PROVIDER|status
```
Multiple credentials per provider form a pool that rotates automatically and skips exhausted keys.

### Other

```
icebath desktop / gui        Native desktop app
icebath dashboard            Web admin panel + embedded chat (--stop / --status)
icebath proxy                OpenAI-compatible local proxy backed by an OAuth provider
icebath portal               Quick setup / sign in via Nous Portal
icebath kanban <verb>        Multi-agent work-queue board
icebath project              Named multi-folder workspaces (see references/projects-and-folders.md)
icebath skin list|use|set    Switch/tweak skins (see references/themes.md)
icebath pets <verb>          Pet mascots (see references/petdex.md)
icebath memory setup|status|off|reset   Memory provider
icebath secrets bitwarden|onepassword   External secret stores
icebath moa                  Mixture-of-Agents slots
icebath hooks / security / backup / import / checkpoints / console
icebath logs [-f] [errors]   View agent/error logs
icebath send                 One-off message through a gateway platform
icebath pairing / plugins / insights / journey / computer-use
icebath acp                  ACP server (IDE integration)
icebath completion bash|zsh|fish
icebath update / uninstall
```

Plugin- and provider-supplied subcommands (e.g. `icebath photon setup`) only appear once their plugin is installed/active. Importing from OpenClaw, Claude Code, Codex CLI or Cursor (`icebath import-agent <agent>`, plus the older `icebath claw migrate|cleanup`) comes from the `agent-importers` plugin; a fresh `icebath setup` that finds one of those agents offers to install it.

### Where to Find Things

| Looking for... | Location |
|---|---|
| Config options | `icebath config edit` · [Configuration docs](https://hermes-agent.nousresearch.com/docs/user-guide/configuration) |
| Tools / toolsets | `icebath tools list` · [Tools reference](https://hermes-agent.nousresearch.com/docs/reference/tools-reference) |
| Skills catalog | `icebath skills browse` · [Skills catalog](https://hermes-agent.nousresearch.com/docs/reference/skills-catalog) |
| Provider setup | `icebath model` · [Providers guide](https://hermes-agent.nousresearch.com/docs/integrations/providers) |
| Env variables | `icebath config env-path` · [Env vars reference](https://hermes-agent.nousresearch.com/docs/reference/environment-variables) |
| Gateway logs | `~/.icebath/logs/gateway.log` (or `icebath logs`) |
| Sessions | `icebath sessions browse` (reads state.db) |
