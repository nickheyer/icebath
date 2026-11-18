package tuihost

import (
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)



var CanonicalAttachedImageResult = pydantic.Extend(AttachedImageResult, "CanonicalAttachedImageResult", ClosedConfig,
	pydantic.Req("mime", pydantic.Str),
)

var CanonicalBotDeliverParams = pydantic.NewModel("CanonicalBotDeliverParams", ParamsConfig,
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("message", pydantic.Str),
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("author", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("notification_category", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalBotDeliverResult = pydantic.NewModel("CanonicalBotDeliverResult", ClosedConfig,
	pydantic.Req("status", pydantic.Str),
	pydantic.Req("delivery_id", pydantic.Str),
	pydantic.Req("profile_home", pydantic.Str),
	pydantic.Req("session_id", pydantic.Str),
	pydantic.Def("admission_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("refused", pydantic.Nullable(pydantic.Literal(true)), nil),
	pydantic.Req("message", pydantic.Str),
	pydantic.Def("reply", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("retry_admission_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
).Described("A delivery's receipt. It names its admission, unless ``refused``: the FIFO refused the\ndelivery before anything was committed (``gateway/session_bot.py``), a terminal ``failed``\nreceipt with no admission that the relay settles as the sender's answer.").WithAfter(admittedOrRefused)



var _ = CanonicalMethod("image.attach_bytes", ImageAttachBytesParams, CanonicalAttachedImageResult, "")

var _ = CanonicalMethod("bot_relay.deliver", CanonicalBotDeliverParams, CanonicalBotDeliverResult, "")

var _ = CanonicalMethod("bot_relay.settle", CanonicalBotDeliverParams, CanonicalBotDeliverResult, "")

// Refuses a deliver result that is neither admitted nor a refused failed receipt without an admission
func admittedOrRefused(r *pydantic.Instance) error {
	refused := r.Get("refused") == true
	if (r.Get("admission_id") == nil) != refused || (refused && r.Get("status") != "failed") {
		return errors.New("a deliver result names its admission, or is a refused failed receipt without one")
	}
	return nil
}
