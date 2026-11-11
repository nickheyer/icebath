package tuigateway

import (
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var CanonicalConfigGetResult = pydantic.Extend(ConfigGetResult, "CanonicalConfigGetResult", ClosedConfig,
	pydantic.Def("key", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("scope", pydantic.Nullable(pydantic.Str), nil),
)

var CanonicalConfigSetParams = pydantic.Extend(ProfileParams, "CanonicalConfigSetParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("key", pydantic.Str),
	pydantic.Def("value", pydantic.Any, nil),
	pydantic.Def("confirm_expensive_model", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("scope", pydantic.Nullable(pydantic.Literal("session", "global")), nil),
)

var CanonicalAttachedImageResult = pydantic.Extend(AttachedImageResult, "CanonicalAttachedImageResult", ClosedConfig,
	pydantic.Req("mime", pydantic.Str),
)

var CanonicalProfileRow = pydantic.Extend(ProfileRow, "CanonicalProfileRow", ClosedConfig,
	pydantic.Def("last_session", pydantic.Nullable(ProfileCanonicalSession), nil),
)

var CanonicalProfilesResult = pydantic.Extend(ProfilesListResult, "CanonicalProfilesResult", ClosedConfig,
	pydantic.Req("profiles", pydantic.List(CanonicalProfileRow)),
)

var CanonicalProfilesParams = pydantic.Extend(ProfileParams, "CanonicalProfilesParams", ParamsConfig,
	pydantic.Def("include_sessions", pydantic.Bool, true),
)

var CanonicalRoomCreateParams = pydantic.Extend(RoomParams, "CanonicalRoomCreateParams", ParamsConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("members", pydantic.List(RoomMemberInput)),
)

var CanonicalRoomAttemptParams = pydantic.Extend(RoomParams, "CanonicalRoomAttemptParams", ParamsConfig,
	pydantic.Req("member_id", pydantic.Str),
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
)

var CanonicalRoomApproveParams = pydantic.Extend(CanonicalRoomAttemptParams, "CanonicalRoomApproveParams", ParamsConfig,
	pydantic.Req("choice", pydantic.Literal("once", "deny")),
	pydantic.Req("request_id", pydantic.Str),
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
).WithAfter(admittedOrRefused)

var _ = CanonicalMethod("config.get", ConfigGetParams, CanonicalConfigGetResult, "")

var _ = CanonicalMethod("config.set", CanonicalConfigSetParams, ConfigSetResult, "")

var _ = CanonicalMethod("profiles.list", CanonicalProfilesParams, CanonicalProfilesResult, "")

var _ = CanonicalMethod("image.attach_bytes", ImageAttachBytesParams, CanonicalAttachedImageResult, "")

var _ = CanonicalMethod("groups.retry", CanonicalRoomAttemptParams, GroupsRetryResult, "")

var _ = CanonicalMethod("groups.create", CanonicalRoomCreateParams, GroupsCreateResult, "")

var _ = CanonicalMethod("groups.approve", CanonicalRoomApproveParams, GroupsApproveResult, "")

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
