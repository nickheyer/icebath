<p align="center"><img src="../assets/langfuse.jpg" alt="Langfuse: official Icebath Agent plugin" width="100%"></p>

# Langfuse for Icebath Agent

> Compiled into icebath; `icebath plugins install langfuse` publishes it into a profile.

icebath plugin that traces Icebath turns,
LLM calls and tool invocations to [Langfuse](https://langfuse.com), the open-source LLM observability
platform. One span per turn, one generation per API call, one observation per tool call and subagent.
Usage totals, per-type token counts and cost estimates come from Icebath's own pricing, so Langfuse sees
the same breakdown (input / output / cache read / cache write / reasoning tokens) as `icebath logs`.
Reasoning is reported as `output_reasoning_tokens` and left out of `output`, so the per-type counts
add up to the total.

The plugin fails open: no credentials or a transient Langfuse error turn into a no-op in the hook
(missing or placeholder keys log one warning). The agent loop is never affected.

**If you already used Langfuse with Icebath, you don't need to do anything.** `icebath update` (or the
first start after updating) installs and enables this plugin for every profile that had
`observability/langfuse` enabled. Your keys and every `ICEBATH_LANGFUSE_*` setting stay exactly as they
were.

## Install

From the Icebath plugin catalog:

```bash
icebath plugins install langfuse
```

Install asks for your project keys and saves them to the profile's `.env`. For a self-hosted Langfuse,
also set the server URL:

```bash
ICEBATH_LANGFUSE_PUBLIC_KEY=pk-lf-...
ICEBATH_LANGFUSE_SECRET_KEY=sk-lf-...
ICEBATH_LANGFUSE_BASE_URL=https://cloud.langfuse.com   # or your self-hosted URL
```

Keys pasted straight from a template (`pk-lf-...`, `your-key`, ...) are refused with a warning. The
`pk-lf-` / `sk-lf-` prefix is enforced only for Langfuse Cloud (`*.langfuse.com`); on any other
host a key without it logs a one-time warning and is still used, since a custom
Langfuse-compatible endpoint may issue its own keys and authenticates them itself.

Restart Icebath and the next turn ships a trace. Plugins are installed per profile: use
`icebath -p <profile> plugins install langfuse` for another profile. The plugin is compiled into icebath,
so there is nothing else to install.

## Verify

```bash
icebath plugins list                 # langfuse should show "enabled"
icebath chat -q "hello"              # then check Langfuse for a "Icebath turn" trace
```

Generation observations include the Icebath system prompt when the provider uses a separate `system`
param (Anthropic Messages API). Open an **LLM call** child span to inspect `role: system` (truncated
via `ICEBATH_LANGFUSE_MAX_CHARS`).

Root traces carry `platform:<name>` and `trigger:cron|interactive` tags (cron runs also
`cron-job:<job-id>`), and their metadata names the Icebath `profile` the turn ran for, so scheduled
runs and profiles sharing one project can be filtered apart. Kanban worker turns also carry
`kanban_task_id` / `kanban_run_id`, so a trace joins back to its board card.

### End-user attribution (opt-in)

Langfuse can group traces by the person who sent the message (its **Users** view). That needs a
third-party identifier, so it is off until you turn it on for a profile:

```bash
icebath config set plugins.entries.langfuse.settings.end_user_attribution true
```

In the Desktop app it is a switch on the plugin's page under **Settings → Plugins**. With it on, a
gateway turn's trace gets the sender's platform ID as its `userId`, exactly as the platform reports it
(for Matrix, the full `@user:server` ID), and subagents carry the user of the turn that spawned them.
Turns with no sender, such as CLI and cron runs, stay without a `userId`. The setting is read every
turn, so no restart is needed. The ID goes to your Langfuse project unhashed, even when
`privacy.redact_pii` is on.

## How it works

| Hook | Behaviour |
|---|---|
| `pre_api_request` / `pre_llm_call` | Open (or reuse) a per-turn root span "Icebath turn". Start a `generation` child observation for this API call with serialized recent messages as input. |
| `post_api_request` / `post_llm_call` | Close the generation, attach `usage_details`, `cost_details`, `finish_reason`, assistant output + tool calls. If no tool calls and non-empty content, close the turn. |
| `api_request_error` | Close the failed generation with `level=ERROR`, status code, retry counters and a capture-mode-scrubbed error message. Non-retryable failures also finish the turn. |
| `pre_tool_call` | Start a `tool` child observation with sanitized `args`. |
| `post_tool_call` | Close the tool observation with sanitized `result`. `read_file` payloads get summarized (head + tail + omitted-line count) so a huge file read stays under `ICEBATH_LANGFUSE_MAX_CHARS`. |
| `subagent_start` / `subagent_stop` | One observation per delegated subagent under the parent turn. |
| `on_session_end` / `on_session_finalize` | Close any still-open traces for the session, so interrupted or tool-only turns don't dangle. Only process exit flushes. |

Ended observations export through the SDK's bounded background queue: turn completion, API errors
and session rotation (`/new`, `/reset`, expiry) never wait for `flush()`, so a slow or unavailable
Langfuse backend cannot hold up a finished turn (or a cron job past its watchdog). Process exit
still flushes and can wait for the SDK. Traces may appear after the SDK's next batch; a full queue,
failed export or abrupt kill can lose telemetry (there is no offline spool).

Session grouping keys off the Icebath session ID (or task ID for subagents) via
`langfuse.propagate_attributes`, so everything in one `icebath chat` session lives under one Langfuse
session. Under a multiplexed gateway each profile traces with its own keys.
Each turn is its own trace, so `ICEBATH_LANGFUSE_SAMPLE_RATE` samples individual turns, not whole
conversations.

## Optional tuning

```bash
ICEBATH_LANGFUSE_ENV=production       # environment tag
ICEBATH_LANGFUSE_RELEASE=v1.0.0       # release tag
ICEBATH_LANGFUSE_SAMPLE_RATE=0.5      # sample 50% of traces
ICEBATH_LANGFUSE_MAX_CHARS=12000      # max chars per field (default: 12000)
ICEBATH_LANGFUSE_MAX_DEPTH=4          # max payload depth (default: 4)
ICEBATH_LANGFUSE_CAPTURE=sanitized    # content capture mode (see below)
ICEBATH_LANGFUSE_DEBUG=true           # verbose plugin logging to agent.log
OTEL_SERVICE_NAME=my-agent           # OTel service.name on traces (default: icebath-agent)
```

The standard SDK names (`LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY`, `LANGFUSE_BASE_URL`) are
accepted too; the `ICEBATH_`-prefixed ones win when both are set.

`ICEBATH_LANGFUSE_MAX_DEPTH` controls nested payload capture in both `sanitized` and `full` modes,
including tool arguments and JSON tool results. The root is depth 0; each dictionary value or array
element adds one level. Values beyond the limit become `<max-depth>`, including scalars. For deeper MCP
responses, set it to a higher non-negative integer (for example, `10`). Unset or blank values default
to `4`; invalid or negative values log a warning and fall back to `4`. `0` keeps only the root level.
Increasing the depth exports more content and may produce larger traces; secret redaction,
string-length limits and the 50-item collection limit remain unchanged. `metadata` mode still omits
content.

## Capture modes

`ICEBATH_LANGFUSE_CAPTURE` controls how much *content* (prompts, responses, tool arguments/results) is
exported. Structural metadata (IDs, roles, tool names, token usage, cost, timing) is captured in every
mode.

| mode | behavior |
|------|----------|
| `metadata` | No content. Each content field is replaced by a shape/size stub (`{"omitted": true, "type": "text", "chars": N}`). |
| `sanitized` | **(default)** Content is exported after secret-pattern redaction (API keys, tokens, JWTs, private keys, `password=`-style assignments) and truncation. Redaction runs *before* truncation. |
| `full` | Raw content, truncated only. Explicit opt-in: traces will contain whatever passed through the conversation, including injected memory and file contents. |

The active mode is recorded on every trace as `metadata.capture_mode`. `sanitized` is pattern-based
defense in depth, not a DLP guarantee. For personal sessions or shared Langfuse projects, prefer
`metadata`.

## Disable or remove

```bash
icebath plugins disable langfuse     # keep it installed, stop tracing
icebath plugins remove langfuse      # uninstall
```

The Langfuse client is cached after the first hook call. If credentials or the SDK are missing, that
decision is cached too, so later hooks return immediately.
