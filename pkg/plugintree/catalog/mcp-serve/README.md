<p align="center">
  <img src="../assets/mcp-serve.jpg" alt="MCP Serve plugin for Icebath Agent" width="100%">
</p>

# MCP Serve

Compiled into icebath; `icebath plugins install mcp-serve` publishes it into a profile. Formerly built into icebath core as `icebath mcp serve`.

Run Icebath as an MCP server. Any MCP client (Claude Code, Claude Desktop, Cursor, Codex, your own harness) launches `icebath mcp-serve` over stdio and gets your Icebath messaging conversations as tools: list and read conversations across Telegram, Discord, Slack, WhatsApp, Signal, Matrix and the rest, poll or wait for new messages, send messages, and see approval requests. With `--tools` it also gets Icebath's own web, browser, vision, image generation, skills and text-to-speech tools.

## Install

```bash
icebath plugins install mcp-serve
```

## Connect an MCP client

```json
{
  "mcpServers": {
    "icebath": {
      "command": "icebath",
      "args": ["mcp-serve"]
    }
  }
}
```

Use `["-p", "<profile>", "mcp-serve"]` to serve another profile's conversations; the plugin must be installed in that profile too (`icebath -p <profile> plugins install mcp-serve`).

**Coming from `icebath mcp serve`?** Change the client's `args` from `["mcp", "serve"]` to `["mcp-serve"]`. The ten tool names, their parameters and descriptions are unchanged. On a Icebath without the old command, `icebath mcp serve` prints this new spelling and the install line.

## Tools

| Tool | What it does |
|---|---|
| `conversations_list` | Active conversations with their session keys, platform, chat type and last activity |
| `conversation_get` | One conversation's details |
| `messages_read` | Recent messages of a conversation |
| `attachments_fetch` | Images, media and other non-text parts of one message |
| `events_poll` / `events_wait` | New messages since a cursor; `events_wait` long-polls |
| `messages_send` | Send to `platform:chat_id` (or a channel name), through Icebath's own send path |
| `channels_list` | Targets you can send to |
| `permissions_list_open` / `permissions_respond` | Approval requests seen during this session |

Messages come from the profile's `state.db`. Events start when the server starts; older history is read with `messages_read`. Reading needs no running gateway; `messages_send` does, since the platform connections live there.

## `--tools`: Icebath's tools for other agents

```json
{ "command": "icebath", "args": ["mcp-serve", "--tools"] }
```

Adds these Icebath tools next to the bridge, using this profile's configured providers and credentials:

`web_search` · `web_extract` · `browser_navigate` · `browser_click` · `browser_type` · `browser_press` · `browser_snapshot` · `browser_scroll` · `browser_back` · `browser_get_images` · `browser_console` · `browser_vision` · `vision_analyze` · `image_generate` · `skill_view` · `skills_list` · `text_to_speech`

- **Off by default.** Without `--tools` the server exposes only the conversation bridge.
- **A fixed allowlist.** Terminal, file, code execution, process, kanban and delegation tools are never exposed, whatever the profile enables: they would hand the client a shell on your machine outside Icebath's approval prompts (kanban tasks run as full agents).
- **No local files.** `browser_navigate` and `web_extract` take only web URLs (no `file:`), `vision_analyze` only web or `data:` images (no local paths), and `text_to_speech` writes only into Icebath's own cache (its `output_path` is not offered). Web URLs include local-network addresses such as `localhost:3000`, as they do for the Icebath model.
- **Your profile's choices apply.** Only toolsets enabled for the CLI in `icebath tools` are offered, and a tool whose backend is not configured (no web provider, no image key, no local browser) is left out, the same availability rules the Icebath model sees.
- **Your hooks still apply.** Shell hooks and plugin `pre_tool_call` hooks run around every call; `--accept-hooks` skips the first-use consent check for shell hooks, as it does for the gateway.
- Tool output is secret-redacted (`ICEBATH_REDACT_SECRETS`), and calls cost whatever the configured provider charges.

## Options

```bash
icebath mcp-serve                 # conversation bridge only
icebath mcp-serve --tools         # plus Icebath's tools (see above)
icebath mcp-serve --verbose       # debug logging on stderr
```

The server is stdio only. Logs go to stderr; stdout is the MCP wire.

## License

MIT, see LICENSE and THIRD_PARTY_NOTICES of icebath.
