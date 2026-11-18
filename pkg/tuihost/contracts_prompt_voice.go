package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)





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

var WakeStartParams = pydantic.NewModel("WakeStartParams", ParamsConfig,
	pydantic.Def("surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("persist", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
).Described("``surface`` names the caller (\"tui\"); ``persist`` is the explicit gesture that also\nflips ``wake_word.enabled`` on.")

var WakeStartResult = pydantic.NewModel("WakeStartResult", ClosedConfig,
	pydantic.Req("started", pydantic.Bool),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("hint", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("phrase", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("owner_surface", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("enabled_persisted", pydantic.Nullable(pydantic.Bool), nil),
).Described("``started: false`` carries ``reason`` (unavailable / disabled / disabled_for_surface / owned).")

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
	pydantic.Def("phrase", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("wake.stop", WakeStopParams, WakeStopResult, "Stop this surface's listener; persist also writes wake_word.enabled: false.")

var WakeControlParams = pydantic.NewModel("WakeControlParams", ParamsConfig,
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
)

var WakeResumeResult = pydantic.Extend(WakeOwnerResult, "WakeResumeResult", ClosedConfig,
	pydantic.Req("resumed", pydantic.Bool),
)

var _ = Method("wake.resume", WakeControlParams, WakeResumeResult, "Reclaim the mic after a pause; no-op if the listener isn't armed.")

var WakeStatusParams = pydantic.NewModel("WakeStatusParams", ParamsConfig,
	pydantic.Def("surface", pydantic.Nullable(pydantic.Str), nil),
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
	pydantic.Req("local_input_available", pydantic.Bool),
).Described("``enabled`` is config truth; ``listening`` is this caller's armed detector; ``audio_silent``\nmeans armed but deaf (see ``hint``).")

var _ = Method("wake.status", WakeStatusParams, WakeStatusResult, "Everything a client needs to draw the wake-word state and decide whether to (re)arm.")
