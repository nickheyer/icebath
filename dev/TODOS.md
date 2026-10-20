All six package readers have reported and I've verified the key claims directly. Here is the full stock-take.

Build, vet, and all 44 package test suites pass. The framework is complete as a library, but nothing composes it: there is no `cmd/`, no package imports agent, session, or tool from outside its own tree, and the README snippet is the only wiring. The UI package is a finished window manager with a demo and zero harness-aware cards. Everything below is what stands between that and icebath.

## Where the framework stands

**Quality is high.** No TODOs, no stubs, no missing symbols, one intentional panic in `session.New`. Coverage is above 90% everywhere except these packages:

| Package          | Coverage |
| ---------------- | -------- |
| session/jsonfile | 79.4%    |
| session/jsonl    | 84.4%    |
| profiles/bedrock | 84.1%    |
| tool/files       | 88.3%    |
| provider/sigv4   | 88.3%    |

**Four structural gaps cut across every package and the UI will hit all of them first:**

- **The event stream is thin.** `agent.Event` has five types and no timestamp, sequence number, run or turn boundary, approval request, compaction start, or subagent start and end. All `tool_end` events for a round are withheld until the slowest tool finishes at `pkg/agent/agent.go:336`. Hook stdout, gate decisions, and agent events go to three unrelated sinks, and the provider layer emits nothing at all about retries, backoff, or failover.
- **Nothing records time or authorship below the session.** `session.Message` carries role and content only. Without per-message timestamp, agent name, model, and usage, the history deck has no axis to fly along.
- **Every tool result is a flat text blob.** Read, Glob, Grep, Edit, WebFetch, and MCP render to strings with ad hoc footers, and the structured types that hold the real data are unexported. A UI either parses those strings or captures pre-images itself for diffs.
- **No cost, no capability metadata, no facts.** No pricing exists anywhere, `profiles.Model` cannot say whether a model supports tools or vision, and nothing in any package stores a recorded finding.

**Two README bugs** will bite the first assembly: `files.Options{Root: "."}` at `README.md:47` rejects every path because root must be absolute after `EvalSymlinks`, and the snippet names a `Profile` as `prov`.

## The UI: surfaces to build

The deck is a strong engine. Springs, z-order, lift and shadow, deal-in, fold-to-strip, solo chips, gutter drag, hint bar, content caching, cursor translation, and a stale-frame-rejecting clock are all done and tested. The netrun demo already sketches the right five shapes: TRACE, UPLINK, DAEMONS, PROBE, and ICE. What is missing is everything above the engine and a handful of engine capabilities the harness needs.

**Deck engine additions in `pkg/ui/deck`:**

- **A timeline layout mode.** The history deck needs a third `Mode` beyond Split and Solo: cards fanned along an axis, overlapping like a hand, with the focused card lifted and expanded and neighbors offset and dimmed. Scrub with arrow keys, wheel, and drag. The existing `entry`, `anim.Rect`, lift, and shadow carry over unchanged.
- **Narrow and ultrawide layouts.** `splitWidth` falls to half width below the minimums, which is unusable under roughly 40 columns. Add a stacked column mode for narrow terminals and multiple stack columns for ultrawide ones.
- **An overlay layer.** Approvals, the command palette, and confirmations need a modal card drawn above everything with a dimmed backdrop and first claim on input. Nothing in the compositor supports it yet.
- **Toasts.** Transient strip for hook output, failover transitions, finished jobs, and errors, with auto-expiry.
- **Card-to-deck intents.** A card currently cannot ask the deck for anything. The netrun roster uses an app-level message to spawn. Define messages a card returns to request focus, promotion, a sibling, closure, or attention with an urgency that drives the glow color, Pulse for a pending approval and Alarm for a failure.
- **Per-key claims.** Capture is all or nothing, and the deck steals j, k, h, l, x, q, digits, enter, and esc. A transcript card needs j and k for scrolling without swallowing tab. Add an interface a card uses to claim specific keys.
- **Clock rest and motion scale.** At idle the clock ticks at 20 FPS forever because the focus ring flows continuously. Add a rest state at zero FPS when nothing moves and the terminal is blurred, and a motion scale option where zero snaps, for reduced motion and high-latency SSH.
- **Layout persistence.** Order, ratio, mode, and hints saved per session.
- **Min-size and pin hints.** A card should be able to declare that it must not fold, the composer for example, or that it stays in the stack.
- **Groups within a slot.** Several agents' traces sharing one slot behind a tab strip.
- **Stack overflow indicator.** When strips scroll off the top, show how many are hidden.
- **Theme variants and runtime switching.** Only Blackwall exists. Add light, high contrast, and a 16-color plus ASCII-border fallback. A user style is palette, border set, motion scale, and density.
- **Terminal integration already offered by `tea.View`.** Program status reporting for idle, working, waiting, done, and failed mapped to agent state. Progress bar for mission progress. Window title with the mission name. Focus reporting. Bracketed paste for the composer. Key release events.
- **Hyperlinks, selection, clipboard.** `uv.Link` cells for file and line references. Text selection in cards with `tea.SetClipboard`.
- **Golden render tests** via `x/exp/golden` so visuals are locked as the deck evolves.

**Harness cards, each a `deck.Card`:**

- **Composer, the UPLINK.** Multi-line textarea, paste, history, slash commands, path completion, image attachment, steer queue while a run is active, mode and model indicators, draft persistence.
- **Transcript, the TRACE.** Streaming turns with markdown via glamour v2 and syntax via chroma, both reachable from the module proxy. Thinking collapsible with live deltas. Tool rows with glyph, name, argument summary, spinner, duration, and expandable result. Diffs for Edit and Write. Refusal rows with the gate reason. Steer and compaction markers. Follow-tail with a paused state as in the demo. Search using viewport highlights. Jump by turn. Per-agent color.
- **Approval.** Modal plus a row in the transcript. Shows the full call, the exact rule Always would create from `rules.Policy.Key`, subjects with unsafe flags, tool description and annotations, judge verdict and reason when configured, and a pending queue. Keys y, n, a, v. Auto-focus with Pulse glow.
- **Daemons, the DAEMONS.** Subagents and shell background jobs with state, elapsed, tokens, last tool, kill, and click to open the child transcript.
- **Mission, the PROBE.** Objective, plan checklist, progress, assigned agents, findings count, cost. One card per mission, several in parallel.
- **Findings.** The facts store rendered as table and tree with filter, evidence links, and markdown export.
- **Telemetry, the ICE.** Context fill against the compaction threshold, tokens per second, cache ratio, cost, latency, retries and failover, rate limits.
- **Workspace.** Files touched this run with diffs and the shell's current directory.
- **Jobs.** Live tail of background job output.
- **Config.** Profile picker with vendor, endpoint, credential status, and model list. Knobs, rules editor, mode switch, hooks list, MCP servers with status, tools, and log.
- **Sessions.** List, resume, fork, delete, search.
- **Palette and help.** Fuzzy command palette and keymap.

**The history deck design.** The unit is a moment: a user turn, an assistant turn with its tool round, an approval, a compaction, a fork point, or a mission milestone. Each moment is a card with title, glyph, timestamp, and a frozen render of the transcript as it stood then. Flying through them is the timeline mode above. Forking from a moment is `Session.Truncate` followed by `Fork`, both of which exist. Comparing two moments is two cards in Split. This needs three things that do not exist: per-message metadata in the session, a persisted event log so renders replay exactly, and a deterministic renderer from a session prefix to card content. The jsonl store is the natural place to append event records next to messages.

**A bridge package between agent and Bubble Tea.** This is the largest new surface and every card depends on it:

- **Event pump.** Agent observer to `tea.Msg` through `Program.Send`, goroutine-safe because notes arrive from tool goroutines, stamping sequence and time, and coalescing text deltas per frame to hold 60 FPS.
- **Approver.** Implements `policy.Asker`, posts a request message, waits on a channel honoring context, and enriches the request with rule key, subjects, tool definition, and judge verdict.
- **Run controller.** Start, cancel, steer, one agent per mission, a registry of running agents, child sessions captured by the subagent factory.
- **Persistence.** Save from the tool-start, turn, and stop hooks since the agent never saves.
- **Pre-image capture.** Read the file before Write and Edit run so the transcript can show a diff.
- **Shell streaming.** A `shell.Runner` wrapper that tees stdout and stderr to the UI with call correlation.
- **MCP manager.** Reconnect, status, and tool refresh on a separate goroutine to avoid the callback deadlock described below.
- **Provider observability.** A `Model` wrapper timing requests and surfacing failover.
- **Cost.** A pricing table applied to usage with per-protocol input semantics.
- **Findings store.** A new package with a fact record, a tool so agents can record one, and persistence in the session store.

**The shell app in `cmd/icebath`.** Flags, config discovery, profile and model selection, workspace root, session resume, a print mode that emits the event stream as JSON for scripting, a headless mission runner, and a non-TTY fallback.

## Package by package: surfaces to implement

**agent**
- Add run, turn, round, approval, compaction, subagent, and hook events with timestamp and sequence. Emit `tool_end` per call as it finishes rather than after the round at `pkg/agent/agent.go:336`.
- Emit events for stop-hook continuations and before-run rewrites, which are appended silently at `pkg/agent/agent.go:232`.
- Hitting the round limit on a tool turn leaves an assistant message with unanswered tool calls at `pkg/agent/agent.go:239`, which makes the next request invalid for most providers.
- A mid-stream failure leaves a dangling user message. Set `Session.Model` from `ModelID`. Copy `Extra` per turn. Add a soft interrupt that stops after the current tool.

**agent/compact**
- No start event, so the UI shows nothing while the summary model runs.
- The summary request is one message holding the whole markdown transcript including thinking, and the threshold measures the previous request rather than the pending one.

**agent/hooks**
- `additionalContext` from PreToolUse and Stop hooks is parsed and discarded at `pkg/agent/hooks/hooks.go:193` and `pkg/agent/hooks/hooks.go:229`.
- A PostToolUse block erases the real tool output. No `stop_hook_active`, so a blocking stop hook loops to the round limit. Hook output goes to a writer, not the event stream. Only four events exist, no PreCompact, SessionStart, SessionEnd, SubagentStop, or Notification.

**agent/subagent**
- No start or end events, no parent, depth, or call id on events, no handle to the child session, no steer or cancel handle, and children run serially because the tool is not read-only.

**session**
- Per-message metadata and an event log. Encapsulate fields, since every one is exported next to a mutex and a UI reading usage mid-run is a race. `Truncate` leaves `Last` stale. `Labels` aliases through `Info` and `Header`. Usage serializes PascalCase inside snake_case documents. `Store` has no context, `Exists`, `Path`, or paged list. Both file stores fail the whole listing on one corrupt file and skip fsync.

**policy**
- `Asker` receives only the call. Pass why it is asking, the rule key, subjects, and tool definition. `Approver` says nothing about who answered. `Memory` is volatile and keys by exact string while rules key by glob, so there are two allow stores with different semantics. Reason strings never say which layer decided. Provide a race-free mutable policy holder and an approver-to-asker adapter so memory can sit before the judge.

**policy/rules**
- No save, remove, or clear. Which rule matched is unexported. `DefaultSubjects` lacks BashOutput, KillShell, and MCP tools. `Dir` is static while the shell's directory moves. Tool-wide deny and ask rules are untested at `pkg/policy/rules/rules.go:378`.

**policy/mode**
- Bypass returns Allow before consulting the base at `pkg/policy/mode/mode.go:65`, so deny rules are not enforced in bypass. Composition order is undocumented and `Guard` above rules turns every argument-level Bash, Write, Edit, and WebFetch allow rule into Ask. Plan-mode denials are indistinguishable from deny rules.

**policy/judge**
- The reason is discarded on allow and on escalation. The judge sees only name and raw args, no cwd, task, or tool description.

**policy/tty**
- Owns stdin for life with no `Close`, so it cannot coexist with a TUI. The UI asker replaces it entirely.

**tool**
- `Registry.Add` is non-atomic on failure. Schema validation is `json.Valid` only. `RunAll` ignores context between calls. No progress or streaming interface, no structured result payload, no change notification, no per-call timeout.

**tool/files**
- Relative `Root` rejects everything. Structured results for Read, Glob, Grep, and Edit. Pre-image or diff output for Write and Edit. Edit shows a phantom empty trailing line at `pkg/tool/files/edit.go:102`. Grep `head_limit` counts context lines and can drop every match at `pkg/tool/files/grep.go:273`. Multiline mode never sets the multi-line anchor flag. Glob cannot list directories and has no brace expansion. Write and Edit replace symlinks with regular files. Read, Write, and Edit ignore context. Default ignore is only `.git`.

**tool/shell**
- Unix-only with no build tags. No foreground output streaming and no call correlation on `Command`. No non-consuming job tail, no per-job kill, no completion signal. Background jobs ignore the timeout argument at `pkg/tool/shell/bash.go:57`. The Bash description argument is decoded and discarded at `pkg/tool/shell/bash.go:43`. `Output.Duration` is never surfaced. A duplicate map key at `pkg/tool/shell/shell_test.go:197` hides the unknown-job test.

**tool/web**
- Supplying a `Client` silently disables the private-address guard and timeout. `MaxText` cuts bytes but reports characters. Cache never evicts. No charset decoding. Preformatted blocks lose indentation. Relative links are not resolved. Same-host redirects on a different port are refused. No status, headers, timing, or cache-hit in the result.

**tool/mcp**
- No reconnect, health loop, or exported connection state. Callbacks run on the read goroutine, so calling `Tools` inside `OnToolsChanged` deadlocks. Resources and prompts are absent. Progress tokens are never sent and progress notifications are dropped. Sampling, elicitation, and roots are refused. No cancelled notification on context cancel. Protocol version is not validated. Tool names are not length-capped against the registry's 64-character rule. The default 30-second HTTP client bounds every tool call. The GET stream is never reopened. Unix-only stdio.

**profiles**
- Invalid `OLLAMA_HOST` or `AWS_REGION` panics at import at `pkg/profiles/ollama/ollama.go:44` and `pkg/profiles/bedrock/bedrock.go:48`. No default model on any built-in profile. `profiles.Model` has no capability flags or pricing, and OpenRouter's pricing is dropped. No config file path convention. The registry returns aliased maps, so mutating a looked-up profile mutates the registry. No base URL environment overrides except Ollama. Bedrock does not list inference profiles.

**provider**
- No observer for attempts, backoff, Retry-After, or response headers, so rate-limit headers and request ids are unobservable. Cancellation during backoff returns the previous HTTP error at `pkg/provider/provider.go:183`. A zero `Retry.Base` with positive attempts spins. Retry-After is uncapped. Failover fires only on fall-through, never on success. Recording buffers the entire streamed body before delivering it at `pkg/provider/record/record.go:179`, which removes streaming latency under record. Sigv4 exposes neither the winning source nor expiry.

**protocol core**
- No Document, Citation, or server-tool part. Usage arrives only on `done`, with no partial usage event. The snapshot's `ToolCall.Args` stays nil during deltas, so the UI must concatenate. `start` is not guaranteed first. `Usage.Input` means different things per protocol. In-stream vendor errors carry status 200, so `Retryable` and failover never advance on them. `Response` has no request id or headers. `ParseArgs` accepts the literal null.

**protocol codecs**
- OpenAI completions loses thinking in both directions. Anthropic sends foreign or empty signatures verbatim, so a vendor switch mid-session breaks. Server tools hard-fail decode on Anthropic, Responses, Google, and Mistral. Citations are dropped everywhere. Redacted thinking is supported by Bedrock, rejected by Google, dropped by Ollama, and mangled by Mistral. Mistral has no finish reason and skips argument validation. Ollama drops the duration metrics the ICE card wants. Bedrock drops latency metrics, cannot request cache points through `Extra`, and has no eventstream frame size cap. Google turns a blocked prompt into an error and uses its own SSE splitter. `ToolResult.Name` is required only by Google and `IsError` is dropped by Mistral and Ollama.

**Recommended order.** Start with the event stream and session metadata, because every card keys off them and the history deck is impossible without them. In parallel, build the bridge package and the shell app with the Transcript, Composer, and Approval cards on the deck as it stands, so there is a usable harness early. Then the deck additions, overlay, intents, key claims, narrow layout, and rest clock. Then Daemons, Telemetry, Workspace, and Jobs, which need the shell runner and pre-image work. Then the findings store, Mission cards, and the timeline mode. Config, MCP, and provider observability after that. Codec gaps as the chosen vendors demand them.
