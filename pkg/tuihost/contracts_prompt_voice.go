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

