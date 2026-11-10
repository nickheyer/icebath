package kanban

const (
	descTaskIDDefault = "Task id. If omitted, defaults to ICEBATH_KANBAN_TASK from the env — the task the dispatcher spawned you to work on. That default only exists for a dispatcher-spawned worker; any other caller has no default and must pass an explicit task_id (use kanban_list to discover ids)."
	descBoard         = "Kanban board slug to target. When omitted, the call resolves the active board the usual way: ICEBATH_KANBAN_DB env → ICEBATH_KANBAN_BOARD env → the 'current' symlink under the kanban home → 'default'. Pass an explicit slug only when the caller (e.g. a Telegram routing layer) needs to override the env-pinned active board for this one call."
)

// One typed schema property with its model facing description
func kanbanSchemaProp(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

// Schema fragment of the optional board parameter every kanban tool takes
func kanbanBoardSchemaProp() map[string]any { return kanbanSchemaProp("string", descBoard) }

// Tool schema carrying the given properties followed by the optional board parameter
func kanbanToolSchema(name, description string, properties map[string]any, required []any) map[string]any {
	props := make(map[string]any, len(properties)+1)
	for k, v := range properties {
		props[k] = v
	}
	props["board"] = kanbanBoardSchemaProp()
	return map[string]any{"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": props, "required": required}}
}

// Status filter values of kanban_list, every workflow column in board order then archived
func kanbanStatusEnum() []any {
	keys := DefaultWorkflow.Keys()
	out := make([]any, 0, len(keys)+1)
	for _, k := range keys {
		out = append(out, k)
	}
	return append(out, Archived)
}

var (
	kanbanShowSchema = kanbanToolSchema("kanban_show", "Read a task's full state: body, assignee, parent handoffs, attempts, comments. It also carries the title and recent events. Use this to (re)orient yourself before starting work, especially on retries. The response includes a pre-formatted ``worker_context`` string suitable for inclusion verbatim in your reasoning. Outside a dispatcher-spawned worker there is no default task: a bare call returns a pointer to ``kanban_list`` instead of task state.", map[string]any{
		"task_id": kanbanSchemaProp("string", descTaskIDDefault),
	}, []any{})
	kanbanListSchema = kanbanToolSchema("kanban_list", "List Kanban task summaries so an orchestrator can discover work to route. Supports the same core filters as the CLI: assignee, status, tenant, include_archived, and limit. Returns compact rows with ids, title, status, assignee, priority, parent/child ids, and counts. Bounded to 50 rows by default, 200 max, with truncation metadata. Also recomputes ready tasks before listing, matching the CLI. Orchestrator-only — dispatcher-spawned task workers never see this tool.", map[string]any{
		"assignee": kanbanSchemaProp("string", "Optional assignee/profile filter."),
		"status": map[string]any{
			"type":        "string",
			"enum":        kanbanStatusEnum(),
			"description": "Optional task status filter.",
		},
		"tenant":           kanbanSchemaProp("string", "Optional tenant/project namespace filter."),
		"include_archived": kanbanSchemaProp("boolean", "Include archived tasks. Defaults to false."),
		"limit":            kanbanSchemaProp("integer", "Optional maximum rows to return (default 50, max 200)."),
	}, []any{})
	kanbanCompleteSchema = kanbanToolSchema("kanban_complete", "Mark your current task done with a structured handoff for others. Prefer ``summary`` for a human-readable 1-3 sentence description of what you did; put machine-readable facts in ``metadata`` (changed_files, tests_run, decisions, findings, etc). At least one of ``summary`` or ``result`` is required. If you created new tasks via ``kanban_create`` during this run, list their ids in ``created_cards`` — the kernel verifies them so phantom references are caught before they leak into downstream automation. If you produced deliverable files (charts, PDFs, spreadsheets, generated images), list their absolute paths in ``artifacts`` — the gateway notifier will upload them as native attachments to the human who subscribed to the task, so the deliverable lands in their chat alongside the summary instead of being a path they have to fetch by hand.", map[string]any{
		"task_id":  kanbanSchemaProp("string", descTaskIDDefault),
		"summary":  kanbanSchemaProp("string", "Human-readable handoff, 1-3 sentences. Appears in Run History on the dashboard and in downstream workers' context."),
		"metadata": kanbanSchemaProp("object", "Free-form dict of structured facts about this attempt — {\"changed_files\": [...], \"tests_run\": 12, \"findings\": [...]}. Surfaced to downstream workers alongside ``summary``."),
		"result":   kanbanSchemaProp("string", "Short result log line (legacy field, maps to task.result). Use ``summary`` instead when possible; this exists for compatibility with callers that still set --result on the CLI."),
		"created_cards": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
			"description": "Optional structured manifest of task ids you created via ``kanban_create`` during this run. The kernel verifies each id exists and was created by this worker's profile; any phantom id blocks the completion with an error listing what went wrong (auditable in the task's events). Only list ids you got back from a successful ``kanban_create`` call — do not invent or remember ids from prose. Omit the field if you did not create any cards.",
		},
		"artifacts": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
			"description": "Optional list of absolute paths to deliverable files you produced during this run — generated charts, PDFs, spreadsheets, images, archives. Examples: [\"~/.icebath/cache/scratch/q3-revenue.png\", \"~/.icebath/cache/scratch/report.pdf\"]. The gateway notifier uploads each path as a native attachment to the subscribed chat (images embed inline, everything else uploads as a file) so the deliverable lands with the completion notification. Skip intermediate scratch files and references that are not the deliverable. The path must exist on disk at completion. Files inside a managed scratch workspace are copied to durable task attachments before cleanup; a missing declared scratch artifact keeps the task in-flight so you can fix the path and retry.",
		},
	}, []any{})
	kanbanBlockSchema = kanbanToolSchema("kanban_block", "Stop work on this task and route it according to WHY you're stuck. Set ``kind`` to say which: 'dependency' (waiting on another task — goes to todo and auto-resumes when that task finishes, no human needed), 'needs_input' (you need a human decision/answer), 'capability' (a hard wall: no access, missing credentials, an action no agent can do), or 'transient' (a flaky failure that may clear). ``reason`` is shown to the human on the board. If a task keeps getting unblocked and re-blocked for the same reason, it is auto-escalated to triage. Use for genuine blockers only — don't block on things you can resolve yourself.", map[string]any{
		"task_id": kanbanSchemaProp("string", descTaskIDDefault),
		"reason":  kanbanSchemaProp("string", "What you need answered or what stopped you, in one or two sentences. Don't paste the whole conversation; the human has the board and can ask follow-ups via comments."),
		"kind": map[string]any{
			"type":        "string",
			"enum":        []any{"dependency", "needs_input", "capability", "transient"},
			"description": "Why you're blocked. 'dependency' waits in todo and resumes automatically when an incomplete parent finishes; if no parent is open it is recorded as needs_input instead. The others surface to a human. Omit only if none apply.",
		},
	}, []any{"reason"})
	kanbanScheduleSchema = kanbanToolSchema("kanban_schedule", "Park your task as 'scheduled' while it waits for a time or external event. This ends the current run and makes the task non-dispatchable until an orchestrator unblocks it; it does not create a timer. Put any wake-up marker such as ``SCHEDULED_UNTIL=<ISO8601>`` in ``reason``.", map[string]any{
		"task_id": kanbanSchemaProp("string", descTaskIDDefault),
		"reason":  kanbanSchemaProp("string", "Optional reason or machine-readable wake-up marker recorded on the completed run and scheduled event."),
	}, []any{})
	kanbanRequestReviewSchema = kanbanToolSchema("kanban_request_review", "Hand a finished, self-verified task to a human or reviewer before it is done. Moves the task to the 'review' column and notifies the subscriber. Unlike ``kanban_block`` this is NOT a blocker — it never counts toward unblock-loop detection, so a task can cycle through review across follow-ups without ever being falsely escalated to triage. Use this instead of blocking with a free-form 'review-required:' reason.", map[string]any{
		"task_id":  kanbanSchemaProp("string", descTaskIDDefault),
		"summary":  kanbanSchemaProp("string", "What was implemented and how it was verified, in one or two sentences — shown to the reviewer. Don't paste the whole diff; the reviewer has the board and the PR."),
		"reviewer": kanbanSchemaProp("string", "Optional reviewer profile. When provided, the task is reassigned to that profile before review dispatch."),
		"metadata": map[string]any{
			"type":                 "object",
			"description":          "Optional structured handoff facts for the reviewer, such as changed_files, tests_run, commit, or decisions.",
			"additionalProperties": true,
		},
		"artifacts": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
			"description": "Optional list of absolute paths to deliverable files this handoff names — generated charts, PDFs, spreadsheets, images, archives. Examples: ['~/.icebath/cache/scratch/q3-revenue.png', '~/.icebath/cache/scratch/report.pdf']. A review handoff is the last implementer transition, so the kernel copies these into the task's durable attachments before the reviewer's completion cleans the scratch workspace up, and the gateway notifier uploads them as native attachments to the subscribed chat. A missing declared scratch artifact keeps the task in place so you can fix the path and retry.",
		},
	}, []any{"summary"})
	kanbanRequestChangesSchema = kanbanToolSchema("kanban_request_changes", "Reviewer verdict: send the task back to its implementer with required changes. This closes the review run, reapplies parent dependency gating, and requeues the task without using block-loop accounting. Only use from a task claimed from the review column; use kanban_block only for a genuine external blocker.", map[string]any{
		"task_id": kanbanSchemaProp("string", descTaskIDDefault),
		"reason":  kanbanSchemaProp("string", "Specific, actionable changes the implementer must make before requesting another review."),
	}, []any{"reason"})
	kanbanHeartbeatSchema = kanbanToolSchema("kanban_heartbeat", "Signal you're still alive during a long operation (training, encoding, crawls). Call every few minutes so humans see liveness separately from PID checks. Pure side effect — no work changes.", map[string]any{
		"task_id": kanbanSchemaProp("string", descTaskIDDefault),
		"note":    kanbanSchemaProp("string", "Optional short note describing current progress. Shown in the event log."),
	}, []any{})
	kanbanCommentSchema = kanbanToolSchema("kanban_comment", "Append a comment to a task's thread. Use for durable notes that should outlive this run (questions for the next worker, partial findings, rationale). Ephemeral reasoning doesn't belong here — use your normal response instead.", map[string]any{
		"task_id": kanbanSchemaProp("string", "Task id. Required (may be your own task or another's — comment threads are per-task). Outside a dispatcher-spawned worker there is no default; use kanban_list to discover ids."),
		"body":    kanbanSchemaProp("string", "Markdown-supported comment body."),
	}, []any{"task_id", "body"})
	kanbanAttachSchema = kanbanToolSchema("kanban_attach", "Attach a file to a task by passing its bytes inline (base64). Use for genuine file artifacts the next worker or a human should be able to download — generated reports, images, exports. The file is stored as a real attachment (not a comment link) under the task's attachments dir, capped at 25 MB. Prefer kanban_attach_url when you only have a URL.", map[string]any{
		"task_id":  kanbanSchemaProp("string", descTaskIDDefault),
		"filename": kanbanSchemaProp("string", "File name to store it under (e.g. 'report.pdf'). Directory components are stripped; only the leaf is kept."),
		"content_base64": map[string]any{
			"type":        "string",
			"description": "The file contents, base64-encoded. Max 25 MB decoded.",
		},
		"content_type": kanbanSchemaProp("string", "Optional MIME type (e.g. 'application/pdf')."),
	}, []any{"filename", "content_base64"})
	kanbanAttachURLSchema = kanbanToolSchema("kanban_attach_url", "Attach a file to a task by URL (Icebath downloads it server-side, max 25 MB). Use when you have a link rather than the bytes. Only http/https URLs are accepted.", map[string]any{
		"task_id":      kanbanSchemaProp("string", descTaskIDDefault),
		"url":          kanbanSchemaProp("string", "http(s) URL to fetch and store."),
		"filename":     kanbanSchemaProp("string", "Optional name to store it under. Defaults to the URL path's leaf component."),
		"content_type": kanbanSchemaProp("string", "Optional MIME type override. Defaults to the Content-Type the server returns."),
	}, []any{"url"})
	kanbanAttachmentsSchema = kanbanToolSchema("kanban_attachments", "List a task's attached files, with the absolute on-disk path you can read. Rows carry id, filename, content_type, size and uploader.", map[string]any{
		"task_id": kanbanSchemaProp("string", descTaskIDDefault),
	}, []any{})
	kanbanCreateSchema = kanbanToolSchema("kanban_create", "Create a kanban task, optionally a child of the current one (via ``parents``). Used by orchestrator workers to fan out — decompose work into child tasks with specific assignees, link them into a pipeline, then complete your own task. The dispatcher picks up the new tasks on its next tick and spawns the assigned profiles.", map[string]any{
		"title":    kanbanSchemaProp("string", "Short task title (required)."),
		"assignee": kanbanSchemaProp("string", "Profile name that should execute this task (e.g. 'researcher-a', 'reviewer', 'writer'). Required — tasks without an assignee are never dispatched."),
		"body":     kanbanSchemaProp("string", "Opening post: full spec, acceptance criteria, links. The assigned worker reads this as part of its context."),
		"parents": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
			"description": "Parent task ids. The new task stays in 'todo' until every parent reaches 'done'; then it auto-promotes to 'ready'. Typical fan-in: list all the researcher task ids when creating a synthesizer task.",
		},
		"tenant":   kanbanSchemaProp("string", "Optional namespace for multi-project isolation. Defaults to ICEBATH_TENANT env if set."),
		"priority": kanbanSchemaProp("integer", "Dispatcher tiebreaker. Higher = picked sooner when multiple ready tasks share an assignee."),
		"workspace_kind": map[string]any{
			"type":        "string",
			"enum":        []any{"scratch", "dir", "worktree"},
			"description": "Workspace flavor: 'scratch' (fresh tmp dir, default), 'dir' (shared directory, requires absolute workspace_path), 'worktree' (git worktree).",
		},
		"workspace_path":      kanbanSchemaProp("string", "Absolute path for 'dir' or 'worktree' workspace. Relative paths are rejected at dispatch."),
		"project":             kanbanSchemaProp("string", "Optional project id or slug to link the task to. When set, the task becomes a git worktree under the project's primary repo with a deterministic branch (project slug + task id), instead of a random branch."),
		"triage":              kanbanSchemaProp("boolean", "If true, task lands in 'triage' instead of 'todo' — a specifier profile is expected to flesh out the body before work starts."),
		"idempotency_key":     kanbanSchemaProp("string", "If a non-archived task with this key already exists, return that task's id instead of creating a duplicate. Useful for retry-safe automation."),
		"max_runtime_seconds": kanbanSchemaProp("integer", "Per-task runtime cap. When exceeded, the dispatcher SIGTERMs the worker and re-queues the task with outcome='timed_out'."),
		"initial_status": map[string]any{
			"type":        "string",
			"enum":        []any{"running", "blocked"},
			"description": "Initial card status. Use 'blocked' for tasks that require immediate human ops (R3 gate) to skip the brief running-to-blocked transition. Defaults to 'running', which preserves the usual dispatch path.",
		},
		"skills": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
			"description": "Skill names to force-load into the dispatched worker. The kanban lifecycle is already injected automatically; use this to pin a task to a specialist context — e.g. ['translation'] for a translation task, ['github-code-review'] for a reviewer task. The names must match skills installed on the assignee's profile.",
		},
		"goal_mode":           kanbanSchemaProp("boolean", "Run the dispatched worker in a goal loop. When true, after each turn an auxiliary judge checks the worker's response against this card's title/body; if the work isn't done and budget remains, the worker keeps going in the same session until the judge agrees it's complete (or the goal-turn budget is exhausted, which blocks the task for human review). Use this for open-ended cards where one shot rarely finishes the work. Defaults to false (classic single-shot worker)."),
		"completion_contract": kanbanSchemaProp("string", "Declare at creation: local-only (default), OWNER/REPO for PR publication, or an exact GitHub PR URL. PR tasks cannot complete until repository-required exact-head CI passes. On publication pass metadata.published_pr."),
		"goal_max_turns":      kanbanSchemaProp("integer", "Turn budget for goal_mode workers. Caps how many continuation turns the worker may take before the task is blocked for review. Ignored unless goal_mode is true. Defaults to the goal-engine default (20)."),
		"model":               kanbanSchemaProp("string", "Pin the dispatched worker to this model instead of the assignee profile's configured model. Use the exact model name the target provider expects. Omit to use the profile default."),
		"provider":            kanbanSchemaProp("string", "Provider the 'model' belongs to (e.g. 'openrouter', 'anthropic', 'nous'). Set this whenever the model is not from the assignee profile's configured provider — a model name alone is resolved against the profile's provider and will fail if it belongs to a different one. Requires 'model'."),
	}, []any{"title", "assignee"})
	kanbanUnblockSchema = kanbanToolSchema("kanban_unblock", "Unblock a Kanban task. It moves to ready when all parents are done, or todo while any parent remains open. Orchestrator-only — only profiles with the kanban toolset can unblock routed work; dispatcher-spawned task workers never see this tool.", map[string]any{
		"task_id": kanbanSchemaProp("string", "Blocked task id to move to ready or parent-gated todo."),
	}, []any{"task_id"})
	kanbanLinkSchema = kanbanToolSchema("kanban_link", "Add a parent→child dependency edge after both tasks already exist. The child won't promote to 'ready' until all parents are 'done'. Cycles and self-links are rejected. A running child is rejected unless the active owning worker is linking its own card for a dependency handoff.", map[string]any{
		"parent_id": map[string]any{
			"type":        "string",
			"description": "Parent task id.",
		},
		"child_id": map[string]any{
			"type":        "string",
			"description": "Child task id.",
		},
	}, []any{"parent_id", "child_id"})
)
