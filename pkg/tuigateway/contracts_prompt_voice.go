package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ClientSurface = pydantic.Enum("ClientSurface", "hud", "hud", "voice_live", "voice-live")

var PromptSubmitParams = pydantic.Extend(SessionParams, "PromptSubmitParams", ParamsConfig,
	pydantic.Def("text", pydantic.Any, ""),
	pydantic.Def("display_kind", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("interrupted", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("queued", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("voice_context", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("voice_turn", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("title_preview", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("truncate_before_user_ordinal", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("truncate_before_row_id", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("truncate_before_message_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("confirm_truncate", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("confirm_empty_truncate", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("confirm_deep_truncate", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("rebind_survivor_row_ids", pydantic.Nullable(pydantic.List(pydantic.Int)), nil),
	pydantic.Def("hosted_task", pydantic.Nullable(pydantic.Any), nil).Alias("_hosted_task").Exclude(),
	pydantic.Def("turn_author", pydantic.Nullable(pydantic.Any), nil).Alias("_turn_author").Exclude(),
	pydantic.Def("hosted_terminal_callback", pydantic.Nullable(pydantic.Any), nil).Alias("_hosted_terminal_callback").Exclude(),
	pydantic.Def("hosted_images", pydantic.Nullable(pydantic.Any), nil).Alias("_hosted_images").Exclude(),
).Described("``text`` is normally a string; the relay / hosted paths may hand a structured (parts list)\npayload, and the busy path renders it. Truncation (rewind / edit / regenerate) needs explicit\nconsent: ``confirm_truncate`` plus one durable target (``truncate_before_row_id`` preferred,\n``truncate_before_message_id``, or the legacy ``truncate_before_user_ordinal``).")

var PromptSubmitStatus = pydantic.Enum("PromptSubmitStatus", "streaming", "streaming", "queued", "queued", "steered", "steered", "redirected", "redirected")

var PromptSubmitResult = pydantic.NewModel("PromptSubmitResult", ClosedConfig,
	pydantic.Def("status", pydantic.Nullable(PromptSubmitStatus), nil),
	pydantic.Def("voice_stopped", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("user_row_id", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("survivor_user_row_ids", pydantic.Nullable(pydantic.List(pydantic.Nullable(pydantic.Int))), nil),
	pydantic.Def("survivor_row_id_map", pydantic.Nullable(pydantic.Dict(pydantic.Nullable(pydantic.Int))), nil),
	pydantic.Def("turn_isolation", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("admission_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("forwarded", pydantic.Nullable(pydantic.Bool), nil),
).Described("``status`` is absent only on the typed-stop-phrase reply (``voice_stopped``). After a\ntruncation the survivor row ids let the client rebind its cached ``rowId``s (``None`` map\nentries: drop the cached id). ``turn_isolation`` marks a compute-host dispatch.")

var _ = Method("prompt.submit", PromptSubmitParams, PromptSubmitResult, "Send a user turn to a live session; busy sessions queue / steer / redirect instead of refusing.")

var ImageMeta = pydantic.NewModel("ImageMeta", ClosedConfig,
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("width", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("height", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("token_estimate", pydantic.Nullable(pydantic.Int), nil),
)

var AttachedImageResult = pydantic.Extend(ImageMeta, "AttachedImageResult", ClosedConfig,
	pydantic.Req("attached", pydantic.Bool),
	pydantic.Def("path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("remainder", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("bytes", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_prompt.py::_attached_image_result``: the image is queued for the next turn.")

var ClipboardPasteParams = pydantic.Extend(SessionParams, "ClipboardPasteParams", ParamsConfig)

var _ = Method("clipboard.paste", ClipboardPasteParams, AttachedImageResult, "Save the host clipboard image into the session and queue it for the next turn.")

var ImageAttachParams = pydantic.Extend(SessionParams, "ImageAttachParams", ParamsConfig,
	pydantic.Req("path", pydantic.Str),
)

var _ = Method("image.attach", ImageAttachParams, AttachedImageResult, "Queue a gateway-visible image file for the next turn.")

var ImageAttachBytesParams = pydantic.Extend(SessionParams, "ImageAttachBytesParams", ParamsConfig,
	pydantic.Def("content_base64", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("data", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("filename", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("ext", pydantic.Nullable(pydantic.Str), nil),
).Described("``content_base64`` (or the ``data`` alias) carries the bytes; ``filename`` / ``ext`` only hint\nthe extension — magic bytes decide.")

var _ = Method("image.attach_bytes", ImageAttachBytesParams, AttachedImageResult, "Queue an image uploaded as base64 (remote client); reply mirrors image.attach.")

var PdfAttachParams = pydantic.Extend(SessionParams, "PdfAttachParams", ParamsConfig,
	pydantic.Def("path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("content_base64", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("data", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("filename", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("first_page", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("last_page", pydantic.Nullable(pydantic.Int), nil),
).Described("Host ``path`` or base64 ``content_base64`` / ``data``; ``first_page`` / ``last_page`` bound the\nrender (per-call page cap enforced server-side).")

var PdfPage = pydantic.Extend(ImageMeta, "PdfPage", ClosedConfig,
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("page", pydantic.Int),
)

var PdfAttachResult = pydantic.NewModel("PdfAttachResult", ClosedConfig,
	pydantic.Req("attached", pydantic.Bool),
	pydantic.Req("filename", pydantic.Str),
	pydantic.Req("pages_attached", pydantic.Int),
	pydantic.Req("pages", pydantic.List(PdfPage)),
	pydantic.Req("count", pydantic.Int),
	pydantic.Req("text", pydantic.Str),
)

var _ = Method("pdf.attach", PdfAttachParams, PdfAttachResult, "Render a PDF's pages to PNG and queue them as images for the next turn.")

var StagedFileUpload = pydantic.NewModel("StagedFileUpload", ParamsConfig,
	pydantic.Req("install_id", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("profile_home", pydantic.Str),
).Described("Source identity returned by the browser upload route, checked against the live profile.")

var FileAttachParams = pydantic.Extend(SessionParams, "FileAttachParams", ParamsConfig,
	pydantic.Def("path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("data_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("staged_upload", pydantic.Nullable(StagedFileUpload), nil),
).Described("``path`` when the file is gateway-visible, else ``data_url`` carries the bytes; ``name`` labels\nan uploaded file.")

var FileAttachResult = pydantic.NewModel("FileAttachResult", ClosedConfig,
	pydantic.Req("attached", pydantic.Bool),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("path", pydantic.Str),
	pydantic.Req("ref_path", pydantic.Str),
	pydantic.Req("ref_text", pydantic.Str),
	pydantic.Req("uploaded", pydantic.Bool),
)

var _ = Method("file.attach", FileAttachParams, FileAttachResult, "Stage a non-image file into the session workspace and hand back its @file: ref.")

var ImageDetachParams = pydantic.Extend(SessionParams, "ImageDetachParams", ParamsConfig,
	pydantic.Req("path", pydantic.Str),
)

var ImageDetachResult = pydantic.NewModel("ImageDetachResult", ClosedConfig,
	pydantic.Req("detached", pydantic.Bool),
	pydantic.Req("count", pydantic.Int),
)

var _ = Method("image.detach", ImageDetachParams, ImageDetachResult, "Drop a queued image before the turn is sent.")

var InputDetectDropParams = pydantic.Extend(SessionParams, "InputDetectDropParams", ParamsConfig,
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
)

var InputDetectDropResult = pydantic.Extend(ImageMeta, "InputDetectDropResult", ClosedConfig,
	pydantic.Req("matched", pydantic.Bool),
	pydantic.Def("is_image", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("path", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("text", pydantic.Nullable(pydantic.Str), nil),
).Described("``matched: false`` alone when the text is not a drop; an image drop is queued immediately\n(``is_image`` + ``count``), a file drop only yields the ``text`` to insert.")

var _ = Method("input.detect_drop", InputDetectDropParams, InputDetectDropResult, "Recognise a terminal file drop pasted into the composer and turn it into an attachment.")

var SideAgentParams = pydantic.Extend(SessionParams, "SideAgentParams", ParamsConfig,
	pydantic.Req("text", pydantic.Str),
)

var TaskIdResult = pydantic.NewModel("TaskIdResult", ClosedConfig,
	pydantic.Req("task_id", pydantic.Str),
).Described("The side agent runs detached; its answer lands on the parent session as an event carrying\nthis ``task_id``.")

var _ = Method("prompt.background", SideAgentParams, TaskIdResult, "Run a task on a fresh agent in the background; the answer arrives as background.complete.")

var _ = Method("prompt.btw", SideAgentParams, TaskIdResult, "Side question over a snapshot of the live conversation; the answer arrives as btw.complete.")

var PreviewRestartParams = pydantic.Extend(SessionParams, "PreviewRestartParams", ParamsConfig,
	pydantic.Req("url", pydantic.Str),
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("context", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("preview.restart", PreviewRestartParams, TaskIdResult, "Spawn a hidden agent that brings the desktop preview's dev server back up.")

var ClarifyLockParams = pydantic.NewModel("ClarifyLockParams", ParamsConfig,
	pydantic.Req("request_id", pydantic.Str),
	pydantic.Req("question_id", pydantic.Str),
	pydantic.Def("answer", pydantic.Any, ""),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var ClarifyLockStatus = pydantic.Enum("ClarifyLockStatus", "ok", "ok", "expired", "expired")

var ClarifyLockResult = pydantic.NewModel("ClarifyLockResult", ClosedConfig,
	pydantic.Req("status", ClarifyLockStatus),
	pydantic.Def("remaining", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
).Described("``remaining`` lists the qids still unanswered; the lock that empties it resolves the request.\n``expired``: the wait already ended (timeout / cancel) — not an error.")

var _ = Method("clarify.lock", ClarifyLockParams, ClarifyLockResult, "Lock one answer of a batch clarify request (editable until every question is locked).")

var RequestAnswerParams = pydantic.NewModel("RequestAnswerParams", ParamsConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("result", pydantic.Dict(pydantic.Any)),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var RequestAnswerResult = pydantic.NewModel("RequestAnswerResult", ClosedConfig,
	pydantic.Req("status", ClarifyLockStatus),
)

var _ = Method("request.answer", RequestAnswerParams, RequestAnswerResult, "Answer an open server→client request from a client that never received the frame.")

var ApprovalPendingParams = pydantic.Extend(SessionParams, "ApprovalPendingParams", ParamsConfig)

var ApprovalPendingResult = pydantic.NewModel("ApprovalPendingResult", ClosedConfig,
	pydantic.Req("approvals", pydantic.List(PendingApproval)),
)

var _ = Method("approval.pending", ApprovalPendingParams, ApprovalPendingResult, "Replay the approvals still waiting on this session (reconnect / polling).")

var ApprovalReceivedParams = pydantic.Extend(SessionParams, "ApprovalReceivedParams", ParamsConfig,
	pydantic.Req("request_id", pydantic.Str),
)

var ApprovalReceivedResult = pydantic.NewModel("ApprovalReceivedResult", ClosedConfig,
	pydantic.Req("acknowledged", pydantic.Bool),
)

var _ = Method("approval.received", ApprovalReceivedParams, ApprovalReceivedResult, "Tell the backend the card is on screen, so its timeout clock starts.")

var ApprovalRespondParams = pydantic.Extend(SessionParams, "ApprovalRespondParams", ParamsConfig,
	pydantic.Def("choice", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("all", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("request_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``choice`` is one of the offered ``approval`` choices (once / session / always / deny); ``all``\nresolves every pending approval, ``request_id`` a specific one, neither the oldest.")

var ApprovalRespondResult = pydantic.NewModel("ApprovalRespondResult", ClosedConfig,
	pydantic.Req("resolved", pydantic.Int),
)

var _ = Method("approval.respond", ApprovalRespondParams, ApprovalRespondResult, "Deliver the user's decision on a dangerous command (falls back to durable identity on a stale sid).")

var VoiceToggleAction = pydantic.Enum("VoiceToggleAction", "status", "status", "on", "on", "off", "off", "tts", "tts")

var VoiceToggleParams = pydantic.NewModel("VoiceToggleParams", ParamsConfig,
	pydantic.Def("action", VoiceToggleAction, "status"),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var VoiceToggleResult = pydantic.NewModel("VoiceToggleResult", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Req("record_key", pydantic.Str),
	pydantic.Req("tts", pydantic.Bool),
	pydantic.Def("stop_hint", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("available", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("audio_available", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("stt_available", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("details", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_voice.py::_voice_status_payload`` (+ the requirements probe on ``status``, the spoken\nstop hint on ``on``).")

var _ = Method("voice.toggle", VoiceToggleParams, VoiceToggleResult, "/voice parity: report, flip voice mode on/off, or toggle speech output.")

var VoiceRecordAction = pydantic.Enum("VoiceRecordAction", "start", "start", "stop", "stop")

var VoiceRecordParams = pydantic.NewModel("VoiceRecordParams", ParamsConfig,
	pydantic.Def("action", VoiceRecordAction, "start"),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var VoiceRecordStatus = pydantic.Enum("VoiceRecordStatus", "recording", "recording", "stopped", "stopped", "busy", "busy")

var VoiceRecordResult = pydantic.NewModel("VoiceRecordResult", ClosedConfig,
	pydantic.Req("status", VoiceRecordStatus),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("voice.record", VoiceRecordParams, VoiceRecordResult, "VAD-bounded push-to-talk; the transcript arrives as a voice.transcript event.")

var VoiceTtsParams = pydantic.NewModel("VoiceTtsParams", ParamsConfig,
	pydantic.Req("text", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var VoiceTtsResult = pydantic.NewModel("VoiceTtsResult", ClosedConfig,
	pydantic.Req("status", pydantic.Str),
)

var _ = Method("voice.tts", VoiceTtsParams, VoiceTtsResult, "Speak text through the backend TTS engine (barge-in aware).")

var WakeStartParams = pydantic.NewModel("WakeStartParams", ParamsConfig,
	pydantic.Def("surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("persist", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("client_capture", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``surface`` names the caller (\"tui\" | \"gui\"); ``persist`` is the explicit gesture that also\nflips ``wake_word.enabled`` on; ``client_capture`` asks for PCM streamed via wake.feed.")

var WakeStartResult = pydantic.NewModel("WakeStartResult", ClosedConfig,
	pydantic.Req("started", pydantic.Bool),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("hint", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("phrase", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("owner_surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("enabled_persisted", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("capture", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("sample_rate", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("frame_length", pydantic.Nullable(pydantic.Int), nil),
).Described("``started: false`` carries ``reason`` (unavailable / disabled / disabled_for_surface / owned);\n``sample_rate`` / ``frame_length`` describe the PCM frames the armed detector expects.")

var _ = Method("wake.start", WakeStartParams, WakeStartResult, "Arm the wake-word listener for the calling surface; refusals explain why.")

var WakeStopParams = pydantic.NewModel("WakeStopParams", ParamsConfig,
	pydantic.Def("persist", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var WakeOwnerResult = pydantic.NewModel("WakeOwnerResult", ClosedConfig,
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
)

var WakeStopResult = pydantic.Extend(WakeOwnerResult, "WakeStopResult", ClosedConfig,
	pydantic.Req("stopped", pydantic.Bool),
	pydantic.Req("disabled_persisted", pydantic.Bool),
)

var _ = Method("wake.stop", WakeStopParams, WakeStopResult, "Stop this surface's listener; persist also writes wake_word.enabled: false.")

var WakeControlParams = pydantic.NewModel("WakeControlParams", ParamsConfig,
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var WakePauseResult = pydantic.Extend(WakeOwnerResult, "WakePauseResult", ClosedConfig,
	pydantic.Req("paused", pydantic.Bool),
)

var _ = Method("wake.pause", WakeControlParams, WakePauseResult, "Release the mic (e.g. while the desktop's browser captures audio).")

var WakeResumeResult = pydantic.Extend(WakeOwnerResult, "WakeResumeResult", ClosedConfig,
	pydantic.Req("resumed", pydantic.Bool),
)

var _ = Method("wake.resume", WakeControlParams, WakeResumeResult, "Reclaim the mic after a pause; no-op if the listener isn't armed.")

var WakeStatusParams = pydantic.NewModel("WakeStatusParams", ParamsConfig,
	pydantic.Def("surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("client_capture", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var WakeInputDevice = pydantic.NewModel("WakeInputDevice", OpenConfig,
	pydantic.Def("selector", pydantic.Nullable(pydantic.Union(pydantic.Int, pydantic.Str)), nil),
	pydantic.Def("name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("max_input_channels", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("default_samplerate", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("hostapi_index", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("hostapi", pydantic.Nullable(pydantic.Str), nil),
).Described("``tools/wake_word.py::_describe_input_device`` — PortAudio diagnostics for the configured mic.")

var WakeStatusResult = pydantic.NewModel("WakeStatusResult", ClosedConfig,
	pydantic.Req("listening", pydantic.Bool),
	pydantic.Req("owned_by_caller", pydantic.Bool),
	pydantic.Def("owner_surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("phrase", pydantic.Str),
	pydantic.Req("provider", pydantic.Str),
	pydantic.Req("configured_surface", pydantic.Str),
	pydantic.Req("input_device", WakeInputDevice),
	pydantic.Req("available", pydantic.Bool),
	pydantic.Req("hint", pydantic.Str),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Req("audio_silent", pydantic.Bool),
	pydantic.Req("capture", pydantic.Str),
	pydantic.Req("local_input_available", pydantic.Bool),
	pydantic.Req("sample_rate", pydantic.Int),
	pydantic.Req("frame_length", pydantic.Int),
).Described("``enabled`` is config truth; ``listening`` is this caller's armed detector; ``audio_silent``\nmeans armed but deaf (see ``hint``).")

var _ = Method("wake.status", WakeStatusParams, WakeStatusResult, "Everything a client needs to draw the wake-word state and decide whether to (re)arm.")

var WakeFeedParams = pydantic.NewModel("WakeFeedParams", ParamsConfig,
	pydantic.Def("pcm", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pcm_b64", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("sample_rate", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``pcm`` (or the ``pcm_b64`` alias): base64 int16 mono little-endian, 16 kHz only.")

var WakeFeedResult = pydantic.Extend(WakeOwnerResult, "WakeFeedResult", ClosedConfig,
	pydantic.Req("fed", pydantic.Bool),
)

var _ = Method("wake.feed", WakeFeedParams, WakeFeedResult, "Push client-captured PCM into the armed detector (mic-less remote backends).")
