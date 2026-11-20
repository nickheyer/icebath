# delegate_task: diagnosing "my batch was capped"

When a user reports `delegate_task` ran fewer subagents than they asked for
(e.g. "I set max_concurrent_children: 15 but only 9 ran"), there are exactly
**two** code paths in Icebath that cap a batch, and two more that only look
like one. If neither cap fired, the cap came from the **model itself** — not
from Icebath — and the user's narration of "the runtime caps at N" is the
model rationalising its own choice.

## The real caps in Icebath

All resolved through `tools.MaxConcurrentChildren()` in
`pkg/tools/delegate_tool_config.go`, which reads
`delegation.max_concurrent_children` from `config.yaml`
(env fallback `DELEGATION_MAX_CONCURRENT_CHILDREN`, default 10). Floor of 1.
**No hard ceiling.**

1. **Per-call hard reject** — `pkg/tools/delegate_tool_tasks.go`.
   If `len(tasks) > max_children`, the call returns a tool error with the
   exact message: `"Too many tasks: {N} provided, but
   max_concurrent_children is {M}. ..."` The model sees this as a failed
   tool call and usually retries with fewer tasks.

2. **Per-turn truncator** — `capDelegateTaskCalls` in
   `pkg/agent/run_agent_tools.go`. If the model emits *multiple separate*
   `delegate_task` tool_calls in a single assistant turn, the count of those
   calls is truncated to `max_children`. Logs as
   `Truncated N excess delegate_task call(s) to enforce
   max_concurrent_children=M limit` at WARNING.

## Two paths that cap nothing

3. **Cost-warning** — same `MaxConcurrentChildren()`. When the value set
   in `config.yaml` is `> 10`, logs once per process at WARNING:
   `delegation.max_concurrent_children=N: each child consumes API tokens
   independently. High values multiply cost linearly.` This is **just a
   log line** — it does not cap anything. Easy to mis-read as "Icebath is
   refusing my value."

4. **Background pool full** — a `background` call whose units the
   background delegation pool (`tools.MaxAsyncChildren()`, the same limit,
   counted across the whole process) cannot take runs them inline instead:
   `delegate_task: async pool at capacity (...); running the whole batch
   synchronously instead.` at INFO, or `delegate_task: unit K/N not
   accepted (...); running it inline.` at WARNING
   (`pkg/agent/delegate_tool_dispatch.go`). Every task still runs; only the
   timing changes.

## Diagnostic recipe

When a user says "delegate is capped at N":

```bash
# 1. What does the loaded config actually say?
icebath config get delegation.max_concurrent_children

# 2. Is the env fallback set? (it only applies when config.yaml has no value)
printenv DELEGATION_MAX_CONCURRENT_CHILDREN; grep DELEGATION_MAX_CONCURRENT_CHILDREN "${ICEBATH_HOME:-$HOME/.icebath}/.env"

# 3. Did Icebath's truncator fire, or did the background pool run units inline?
grep -E "Truncated.*delegate_task|async pool at capacity|not accepted" "${ICEBATH_HOME:-$HOME/.icebath}/logs/agent.log" | tail
# If no Truncated line appears, the truncator never executed.

# 4. Did the per-call reject fire? It is a tool result, so it sits in the session transcript.
icebath sessions export - --session-id <id> | grep "Too many tasks"
```

If config, env and the transcript agree, and neither the `Truncated` line
nor the `Too many tasks` result appears, **the cap is the model**, not Icebath.

## Why models self-limit batches

Reasoning models (Claude Opus/Sonnet, GPT-5, Grok-4) routinely trim a
13- or 15-task batch to a "rounder" number (5, 8, 9, 10) when their
internal reasoning says the coordination cost outweighs parallelism. The
cost-warning log line printed at startup *reinforces* this — the model
reads its own reasoning trace and sees "each child consumes API tokens
independently" and concludes a smaller batch is "more responsible."

The model will then narrate the choice as "the runtime caps at 9" or
"despite the config saying 15, max parallel is 9," which is **not true**
— it's post-hoc rationalisation. Calling this out to the user is fine;
it is a real, well-known reasoning-model failure mode (face-saving
attribution to the system rather than admitting a self-imposed limit).

## How to actually force N parallel children

Tell the model explicitly in the prompt:

> "Send all 13 tasks in **one** `delegate_task` call with a `tasks` array
> of 13 items. Do not split into multiple calls. The runtime supports
> this; `delegation.max_concurrent_children` is set to 15."

If the model still trims, use `execute_code` to construct the `tasks`
list deterministically and call the tool with that exact list — the
model is then merely a courier and is far less likely to second-guess
the count. Or use a different model: smaller / less-reasoning-heavy
models trim less aggressively in practice.

## Pitfalls / gotchas

- **`max_concurrent_children` caps each call and each turn of one parent,
  not every agent at once.** The reject and the truncator count only the
  calls of the turn in front of them (`pkg/tools/delegate_tool_tasks.go`,
  `pkg/agent/run_agent_tools.go`). Two different parents can each spawn
  `max_children` workers concurrently; only background units share one
  process-wide pool, and a full pool runs them inline.
- **`subagent_auto_approve: false` does not cap concurrency.** It only
  controls whether children inherit yolo / approval bypass. Don't mistake
  it for a throttle.
- **The cost-warning log fires once per process** when the configured value
  is > 10. Don't take its presence as evidence that anything was capped — only
  the `Truncated...` and `Too many tasks` lines indicate actual capping.
- **Don't suggest reverting `max_concurrent_children` to fix this.** The
  user set it deliberately; the fix is to push back on the model, not
  the config.
