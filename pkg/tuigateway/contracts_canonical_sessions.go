package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var CanonicalSessionParams = pydantic.Extend(ProfileParams, "CanonicalSessionParams", ParamsConfig,
	pydantic.Req("session_id", pydantic.Str),
)

var CanonicalCreateParams = pydantic.Extend(ProfileParams, "CanonicalCreateParams", ParamsConfig,
	pydantic.Def("request_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("source", pydantic.Literal("cli", "tui", "gui", "acp", "tool", "oneshot"), "cli"),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("row_cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("base_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("api_key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("toolsets", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("reasoning", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("max_turns", pydantic.Nullable(pydantic.Union(pydantic.Int, pydantic.Str)), nil),
	pydantic.Def("ignore_rules", pydantic.Bool, false),
	pydantic.Def("yolo", pydantic.Bool, false),
	pydantic.Def("safe_mode", pydantic.Bool, false),
	pydantic.Def("ignore_user_config", pydantic.Bool, false),
	pydantic.Def("skills", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("checkpoints", pydantic.Bool, false),
	pydantic.Def("accept_hooks", pydantic.Bool, false),
	pydantic.Def("pass_session_id", pydantic.Bool, false),
	pydantic.Def("editor", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("hidden", pydantic.Bool, false),
	pydantic.Def("follow_profile_config", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("service_tier", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd_explicit", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("run_budget", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("verbose", pydantic.Bool, false),
	pydantic.Def("client_env", pydantic.Nullable(pydantic.Dict(pydantic.Str)), nil),
)

var CanonicalResumeParams = pydantic.Extend(ProfileParams, "CanonicalResumeParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Str, ""),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("latest", pydantic.Nullable(pydantic.Literal("cli", "tui")), nil),
	pydantic.Def("workspace", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("editor", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("api_key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("defer_history", pydantic.Bool, false),
	pydantic.Def("omit_messages", pydantic.Bool, false),
	pydantic.Def("cols", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("display_projection", pydantic.Bool, false),
)

var CanonicalSessionHandle = pydantic.NewModel("CanonicalSessionHandle", ClosedConfig,
	pydantic.Req("ref", CanonicalSessionRef),
	pydantic.Req("instance_id", pydantic.Str),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Req("revision", pydantic.Int),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("execution_state", pydantic.Literal("idle", "running", "waiting", "unknown", "terminal")),
)

var CanonicalPendingAdmission = pydantic.Extend(AdmissionStatus, "CanonicalPendingAdmission", ClosedConfig,
	pydantic.Req("input_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("controls", pydantic.Nullable(pydantic.List(pydantic.Literal("steer", "replace"))), nil),
	pydantic.Def("steering", pydantic.Nullable(pydantic.Bool), nil),
)

var CanonicalPendingPrompt = pydantic.NewModel("CanonicalPendingPrompt", ClosedConfig,
	pydantic.Req("kind", pydantic.Literal("approval", "clarify")),
	pydantic.Req("prompt_id", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("choices", pydantic.List(pydantic.Str)),
	pydantic.Def("command", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("edit", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("question", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("multi_select", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("timeout_s", pydantic.Nullable(pydantic.Int), nil),
)

var CanonicalSnapshotInfo = pydantic.Extend(SessionInventoryFields, "CanonicalSnapshotInfo", ClosedConfig,
	pydantic.Req("desktop_protocol", pydantic.Literal("icebath-gateway-v1")),
	pydantic.Def("source", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("lazy", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("profile_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("edit_approval_policy", pydantic.Nullable(pydantic.Literal("ask", "workspace_session", "session")), nil),
	pydantic.Def("launch_request", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("reasoning_effort", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reasoning_effort_wire", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("service_tier", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("fast", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("last_active_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("yolo", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("api_mode", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("credential_hint", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("config_warning", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalSnapshot = pydantic.NewModel("CanonicalSnapshot", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("stored_session_id", pydantic.Str),
	pydantic.Req("messages", pydantic.List(TranscriptMessage)),
	pydantic.Def("messages_omitted", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("display_messages", pydantic.Nullable(pydantic.List(TranscriptMessage)), nil),
	pydantic.Req("message_count", pydantic.Int),
	pydantic.Req("running", pydantic.Bool),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Req("replay_epoch", pydantic.Str),
	pydantic.Req("last_sequence", pydantic.Int),
	pydantic.Req("subscription_id", pydantic.Str),
	pydantic.Req("revision", pydantic.Int),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("pending", pydantic.List(CanonicalPendingAdmission)),
	pydantic.Req("prompts", pydantic.List(CanonicalPendingPrompt)),
	pydantic.Req("info", CanonicalSnapshotInfo),
	pydantic.Def("pending_connection", pydantic.Nullable(ConnectionRequestPayload), nil),
	pydantic.Def("todo_state", pydantic.Nullable(TodoState), nil),
	pydantic.Def("open_requests", pydantic.Nullable(pydantic.List(OpenRequestEntry)), nil),
)

var CanonicalListParams = pydantic.Extend(ProfileParams, "CanonicalListParams", ParamsConfig,
	pydantic.Def("scope", pydantic.Literal("live", "stored"), "live"),
	pydantic.Def("limit", pydantic.Int, 200),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("include_hidden", pydantic.Bool, false),
)

var CanonicalSessionListRow = pydantic.NewModel("CanonicalSessionListRow", ClosedConfig,
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("title", pydantic.Str),
	pydantic.Req("source", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("started_at", pydantic.Nullable(pydantic.Float)),
	pydantic.Req("message_count", pydantic.Int),
	pydantic.Req("running", pydantic.Bool),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("resolved_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("root_title", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalListResult = pydantic.NewModel("CanonicalListResult", ClosedConfig,
	pydantic.Req("sessions", pydantic.List(CanonicalSessionListRow)),
	pydantic.Req("scope", pydantic.Literal("stored", "live")),
)

var CanonicalAttachment = pydantic.NewModel("CanonicalAttachment", ParamsConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("mime", pydantic.Str),
)

var CanonicalTurnAuthor = pydantic.NewModel("CanonicalTurnAuthor", ParamsConfig,
	pydantic.Def("id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("is_bot", pydantic.Bool, false),
	pydantic.Def("origin", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalModelOverride = pydantic.NewModel("CanonicalModelOverride", ParamsConfig,
	pydantic.Req("model", pydantic.Str),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reasoning", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalSubmitParams = pydantic.Extend(CanonicalSessionParams, "CanonicalSubmitParams", ParamsConfig,
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("submission_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("input_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("queued", pydantic.Bool, false),
	pydantic.Def("attachments", pydantic.Nullable(pydantic.List(CanonicalAttachment)), nil),
	pydantic.Def("finite", pydantic.Bool, false),
	pydantic.Def("unattended", pydantic.Bool, false),
	pydantic.Def("surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("voice_context", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("interrupted", pydantic.Bool, false),
	pydantic.Def("voice_turn", pydantic.Bool, false),
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Literal("hidden")), nil),
	pydantic.Def("title_preview", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("turn_author", pydantic.Nullable(CanonicalTurnAuthor), nil),
	pydantic.Def("api_text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model_override", pydantic.Nullable(CanonicalModelOverride), nil),
	pydantic.Def("clarify_batch", pydantic.Bool, false),
)

var CanonicalVoiceStopped = pydantic.NewModel("CanonicalVoiceStopped", ClosedConfig,
	pydantic.Req("voice_stopped", pydantic.Literal(true)),
).Described("A bare stop phrase typed while the session's voice chat is on: the chat ended, nothing was\nadmitted (TUI-26).")

var CanonicalSubmitResult = pydantic.NewRootModel("CanonicalSubmitResult", pydantic.Union(AdmissionStatus, CanonicalVoiceStopped)).Described("``prompt.submit``: the admission receipt, or ``{voice_stopped: true}``.")

var CanonicalSteerQueuedParams = pydantic.Extend(CanonicalSessionParams, "CanonicalSteerQueuedParams", ParamsConfig,
	pydantic.Req("admission_id", pydantic.Str),
)

var CanonicalReplaceQueuedParams = pydantic.Extend(CanonicalSessionParams, "CanonicalReplaceQueuedParams", ParamsConfig,
	pydantic.Req("admission_id", pydantic.Str),
	pydantic.Req("submission_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("attachments", pydantic.Nullable(pydantic.List(CanonicalAttachment)), nil),
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Literal("hidden")), nil),
	pydantic.Def("title_preview", pydantic.Nullable(pydantic.Str), nil),
).Described("The edited prompt: admitted as a NEW admission (``submission_id``) at the FIFO tail.")

var CanonicalControlParams = pydantic.Extend(CanonicalSessionParams, "CanonicalControlParams", ParamsConfig,
	pydantic.Req("execution_generation", pydantic.Int),
)

var CanonicalCorrectionParams = pydantic.Extend(CanonicalControlParams, "CanonicalCorrectionParams", ParamsConfig,
	pydantic.Req("text", pydantic.Str),
)

var CanonicalCorrectionAck = pydantic.NewModel("CanonicalCorrectionAck", ClosedConfig,
	pydantic.Req("status", pydantic.Literal("queued", "redirected", "rejected")),
	pydantic.Req("text", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Def("delivered", pydantic.Nullable(pydantic.Literal(false)), nil),
)

var CanonicalCorrectionResult = pydantic.NewRootModel("CanonicalCorrectionResult", pydantic.Union(CanonicalCorrectionAck, CanonicalVoiceStopped)).Described("``session.steer`` / ``session.redirect``: the correction verdict, or ``{voice_stopped: true}``\nwhen the busy-time text was a bare stop phrase that ended the LOCAL voice chat.")

var CanonicalApprovalParams = pydantic.Extend(CanonicalControlParams, "CanonicalApprovalParams", ParamsConfig,
	pydantic.Req("prompt_id", pydantic.Str),
	pydantic.Req("choice", pydantic.Str),
)

var CanonicalEventsParams = pydantic.Extend(CanonicalSessionParams, "CanonicalEventsParams", ParamsConfig,
	pydantic.Def("last_sequence", pydantic.Int, 0),
	pydantic.Def("last_seen", pydantic.Int, 0),
	pydantic.Def("replay_epoch", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalReplayEvent = pydantic.NewModel("CanonicalReplayEvent", ClosedConfig,
	pydantic.Req("type", pydantic.Str),
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Req("payload", pydantic.Dict(pydantic.Any)),
	pydantic.Req("replay_epoch", pydantic.Str),
	pydantic.Req("seq", pydantic.Int),
	pydantic.Def("authority_epoch", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("execution_generation", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("admission_id", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalEventsResult = pydantic.NewModel("CanonicalEventsResult", ClosedConfig,
	pydantic.Req("events", pydantic.List(CanonicalReplayEvent)),
	pydantic.Req("latest_seq", pydantic.Int),
	pydantic.Req("last_sequence", pydantic.Int),
	pydantic.Req("epoch", pydantic.Str),
	pydantic.Req("replay_epoch", pydantic.Str),
	pydantic.Req("truncated", pydantic.Bool),
	pydantic.Req("snapshot_required", pydantic.Bool),
	pydantic.Req("count", pydantic.Int),
	pydantic.Def("open_requests", pydantic.Nullable(pydantic.List(OpenRequestEntry)), nil),
)

var _ = CanonicalMethod("session.create", CanonicalCreateParams, CanonicalSnapshot, "")

var _ = CanonicalMethod("session.resume", CanonicalResumeParams, CanonicalSnapshot, "")

var _ = CanonicalMethod("session.list", CanonicalListParams, CanonicalListResult, "")

var _ = CanonicalMethod("session.interrupt", CanonicalControlParams, CanonicalSessionHandle, "")

var _ = CanonicalMethod("session.steer", CanonicalCorrectionParams, CanonicalCorrectionResult, "")

var _ = CanonicalMethod("session.redirect", CanonicalCorrectionParams, CanonicalCorrectionResult, "")

var _ = CanonicalMethod("session.events.since", CanonicalEventsParams, CanonicalEventsResult, "")

var _ = CanonicalMethod("prompt.submit", CanonicalSubmitParams, CanonicalSubmitResult, "")

var _ = CanonicalMethod("approval.respond", CanonicalApprovalParams, PromptResponseResult, "")

var _ = CanonicalMethod("prompt.steer_queued", CanonicalSteerQueuedParams, AdmissionStatus, "Hand a still-queued admission's text to the running turn's agent as a steer. The admission reads 'started' with that turn (pending snapshots mark it 'steering') and never runs on its own; it settles terminal 'steered' when the turn completes with its text, else 'interrupted' (its completion frame says 'cancelled', as an interrupted turn's does); a text the turn never consumed goes back to its queue place. Refusals: admission_started, session_idle, steer_unavailable (no agent to take it: the admission stays queued), unsupported_control, not_found.")

var _ = CanonicalMethod("prompt.replace_queued", CanonicalReplaceQueuedParams, AdmissionStatus, "Cancel a still-queued admission and admit the edited prompt as a new admission at the back of the FIFO, in one write bound to the replaced admission. Refusals: admission_started, admission_conflict (the submission_id replaced another admission), queue_full, unsupported_control, not_found.")
