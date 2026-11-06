package cron

var cronjobSchema = map[string]any{
	"name":        "cronjob_manage",
	"description": "Schedule recurring or one-off agent tasks (cron jobs): create, list, edit, run. action='create' schedules a job from a prompt and/or skills; 'list' inspects jobs; 'update'/'pause'/'resume'/'remove' manage one by job_id (always list first \u2014 never guess job IDs); 'run' fires a job immediately in the BACKGROUND (returns a handle at once, outcome re-enters the conversation when done \u2014 do not wait or poll; optional 'prompt' adds transient context for that fire only); 'cancel' stops a job's LIVE run without touching its schedule (a no-op when nothing is in flight).\n\nJobs run in a fresh session with no current-chat context, so prompts must be self-contained, and the agent's FINAL RESPONSE is what gets delivered \u2014 cron runs are autonomous and cannot ask questions. Jobs run on the main agent model (whatever `icebath model` is set to when they fire) unless the user pins one. Prefer updating an existing job over creating near-duplicates.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"paused": map[string]any{
				"type":        "boolean",
				"description": "Create only: persist disabled atomically. Resume to schedule; explicit run remains available. Default false.",
			},
			"paused_reason": map[string]any{
				"type":        "string",
				"description": "Create only: auditable reason; requires paused=true.",
			},
			"action": map[string]any{
				"type":        "string",
				"description": "One of: create, list, update, pause, resume, remove, run, cancel. When action=create, the 'schedule' and 'prompt' fields are REQUIRED.",
			},
			"job_id": map[string]any{
				"type":        "string",
				"description": "Required for update/pause/resume/remove/run/cancel.",
			},
			"pinned": map[string]any{
				"type":        "boolean",
				"description": "For create/update. ONLY set when the user explicitly asks to pin (or unpin) a job's model. pinned=true locks the CURRENT main agent model (and its provider) onto the job so later `icebath model` / `/model` changes never touch it; pinned=false releases the lock so the job follows the main agent model again. Never set it on your own initiative: by default jobs follow the main model.",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "For create: the full self-contained prompt (paired with any skills as the task instruction). For run: optional transient context for that single fire (never persisted).",
			},
			"schedule": map[string]any{
				"type":        "string",
				"description": "REQUIRED for create. Schedule forms: (1) recurring interval \u2014 '30m', 'every 2h', 'every hour' (EVERY 30 minutes / 2 hours / hour, forever by default); (2) explicit one-shot by duration \u2014 'in 30m', 'in 2h' (fires ONCE that far from now; use this for 'remind me in N minutes' \u2014 do NOT hand-compute an absolute timestamp); (3) natural day/time \u2014 'every monday 9am', 'weekdays at 9am', 'every day at 9am' (recurring weekly/daily); (4) cron syntax \u2014 '0 9 * * *' (daily 9am); (5) absolute one-shot \u2014 ISO timestamp '2026-06-01T09:00:00'.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Optional human-friendly name",
			},
			"repeat": map[string]any{
				"type":        "integer",
				"description": "Optional repeat count. Omit for defaults (once for one-shot, forever for recurring).",
			},
			"deliver": map[string]any{
				"type":        "string",
				"description": "Where the job's output is POSTED as a one-way message (the job itself always runs in a fresh session with no chat context). Omit to address the chat/topic this job was created from. Otherwise: 'local' (save only, no delivery), 'all' (every connected home channel, resolved at fire time), 'bot-chat' or 'bot-chat:<profile>' (inject into a Bot Chat as a real message), or platform:chat_id:thread_id (e.g. 'telegram:-1001234567890:17585'). Comma-combine like 'origin,all'. From a local chat (CLI, TUI or Desktop) the default writes the result into that chat's history as a note, seen when the chat is reopened and by its next turn, not as a live message.",
			},
			"failure_deliver": map[string]any{
				"type":        "string",
				"description": "Optional override target for FAILURE notices only (same grammar as deliver). When set, engine failure/interruption notices go here instead of the deliver target; 'local' suppresses them entirely (state still recorded in cron list/run history). Use for jobs delivering into shared channels where failure noise is unwanted. Omit = failures follow deliver (default). On update, '' clears.",
			},
			"skills": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
				},
				"description": "Optional ordered skill names loaded before the cron prompt. On update, [] clears.",
			},
			"catch_up": map[string]any{
				"type":        "boolean",
				"description": "Runs missed while the machine was off: true = run once on return, false = skip. Omit for the default (recurring run once, one-shots skip).",
			},
			"script": map[string]any{
				"type":        "string",
				"description": scriptDescription("the profile ICEBATH_HOME"),
			},
			"monitor": map[string]any{
				"type":        "string",
				"description": "Optional change-detector that gates the agent: an http(s) URL (fetched each tick) or a script path (same rules as `script`, run each tick) \u2014 cheap, no LLM. Output identical to the previous tick skips the agent run entirely; changed output wakes the agent with a diff injected into the prompt. First tick always runs (baseline). Output must be deterministic (no timestamps) or every tick looks changed. Incompatible with no_agent. On update, '' clears.",
			},
			"no_agent": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "True = no LLM: the scheduler runs `script` (required) on schedule and delivers its stdout verbatim; empty stdout sends nothing (watchdog pattern). Use for script-only pings with fixed output; keep False for anything needing reasoning.",
			},
			"context_from": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
				},
				"description": "Optional job ID(s) whose most recent completed output is injected as context each run \u2014 chains jobs (A collects, B processes). For a job's OWN previous output prefer `continuity`. On update, [] clears.",
			},
			"continuity": map[string]any{
				"type":        "boolean",
				"description": "True = each run sees the job's own previous output, so it can dedupe and continue where it left off (scouts, monitors, incremental digests). Default false. On update, false turns it off.",
			},
			"enabled_toolsets": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
				},
				"description": "Optional toolset names to restrict the job's agent to (e.g. [\"web\", \"terminal\"]) \u2014 cuts token overhead. Infer from the prompt. Omit for all default tools. [] = an explicit zero-tool allowlist (the job's agent gets no toolsets, MCP servers included unless named); it is preserved as [], never widened back to the default.",
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Optional absolute existing path to run the job from: injects that directory's AGENTS.md/context files and anchors terminal/file tools there. On update, '' clears.",
			},
			"timezone": map[string]any{
				"type":        "string",
				"description": "Optional IANA zone (e.g. 'America/New_York') the cron expression is read in, instead of the profile's. On update, '' clears.",
			},
			"attach_to_session": map[string]any{
				"type":        "boolean",
				"description": "True = the job's delivery is CONTINUABLE \u2014 the user can reply and the agent has the brief in context (threads on thread-capable platforms, mirrored into the DM elsewhere). Use for conversational recurring jobs (briefings); leave unset for fire-and-forget alerts. Scope: the job's own conversation only \u2014 the origin chat, the home-channel fallback when deliver='origin' captured no origin (script-created jobs), a user-written bare platform target (deliver='slack' \u2014 that platform's home channel), or the job's single explicit platform:chat target (this flag is the only way to attach an explicit target). Broadcast targets are never attached; no effect when deliver='local'.",
			},
		},
		"required": []any{"action"},
	},
}
