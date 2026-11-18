package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var RoomMember = pydantic.NewModel("RoomMember", OpenConfig,
	pydantic.Def("member_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("handle", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("target", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
).Described("One roster row (``hosted_room_discussion.validate_roster``); legacy rooms may carry\npre-normalisation rows, so the set stays open.")

var RoomActor = pydantic.NewModel("RoomActor", ClosedConfig,
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("connection_id", pydantic.Nullable(pydantic.Str), nil),
).Described("``hosted_rooms._ACTOR_FIELDS``; member replies carry ``profile`` plus, when known,\n``display_name`` and the peer ``connection_id`` (``hosted_room_discussion._member_actor``).")

var RoomEvent = pydantic.NewModel("RoomEvent", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("seq", pydantic.Int),
	pydantic.Req("event_id", pydantic.Str),
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("actor", RoomActor),
	pydantic.Def("authority_epoch", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Req("payload", pydantic.Dict(pydantic.Any)),
	pydantic.Req("created_at", pydantic.Float),
	pydantic.Def("idempotent", pydantic.Bool, false),
).Described("``gateway/hosted_rooms.py::_event_from_row``.")

var Room = pydantic.NewModel("Room", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("members", pydantic.List(RoomMember)),
	pydantic.Req("authority_gateway_id", pydantic.Str),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Req("revision", pydantic.Int),
	pydantic.Req("created_at", pydantic.Float),
	pydantic.Req("updated_at", pydantic.Float),
	pydantic.Def("idempotent", pydantic.Bool, false),
	pydantic.Def("disbanded_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("latest_seq", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("adopted", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("claim_event", pydantic.Nullable(RoomEvent), nil),
	pydantic.Def("authority_claim", pydantic.Nullable(RoomEvent), nil),
	pydantic.Def("event", pydantic.Nullable(RoomEvent), nil),
	pydantic.Def("replica", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("home", pydantic.Nullable(pydantic.Str), nil),
).Described("``gateway/hosted_rooms.py::_room_from_row`` plus the branch-only keys ``create`` (legacy\nadoption), ``state`` (``authority_claim``) and ``rename`` (``event``) add.\n``replica`` and ``home`` mark a room another gateway hosts and shares with this one.")

var RoomAuthority = pydantic.NewModel("RoomAuthority", ClosedConfig,
	pydantic.Req("gateway_id", pydantic.Str),
	pydantic.Req("epoch", pydantic.Int),
)

var RoomMemberInput = pydantic.NewModel("RoomMemberInput", OpenParamsConfig,
	pydantic.Def("member_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("handle", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("target", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
).Described("A roster row as the client proposes it; ``validate_roster`` owns the exact rules.")

var RoomParams = pydantic.Extend(ProfileParams, "RoomParams", ParamsConfig,
	pydantic.Req("room_id", pydantic.Str),
)

var RoomExecutionPolicy = pydantic.NewModel("RoomExecutionPolicy", ClosedConfig,
	pydantic.Req("version", pydantic.Int),
	pydantic.Req("target_profile", pydantic.Str),
	pydantic.Req("enabled_toolsets", pydantic.List(pydantic.Str)),
	pydantic.Req("approval_mode", pydantic.Str),
	pydantic.Req("max_iterations", pydantic.Int),
	pydantic.Req("policy_digest", pydantic.Str),
).Described("``gateway/hosted_room_execution_policy.py::execution_policy_mapping``.")

var RoomLinkEndpoint = pydantic.NewModel("RoomLinkEndpoint", ClosedConfig,
	pydantic.Req("available", pydantic.Bool),
	pydantic.Def("url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("transport_security", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
).Described("``GatewayRoomCatalog.endpoint_mapping``: ``url``/``transport_security`` when available,\n``reason`` when not.")

var RoomLinkCatalog = pydantic.NewModel("RoomLinkCatalog", ClosedConfig,
	pydantic.Req("installation_id", pydantic.Str),
	pydantic.Req("protocol_versions", pydantic.List(pydantic.Int)),
	pydantic.Req("link_modes", pydantic.List(pydantic.Str)),
	pydantic.Req("persistent_process", pydantic.Bool),
	pydantic.Req("text", pydantic.Bool),
	pydantic.Req("attachments", pydantic.Bool),
	pydantic.Req("execution_policy", RoomExecutionPolicy),
	pydantic.Req("catalog_digest", pydantic.Str),
	pydantic.Def("endpoint", pydantic.Nullable(RoomLinkEndpoint), nil),
).Described("``gateway/hosted_room_peer.py::GatewayRoomCatalog.as_mapping``.")

var RoomLinkStatus = pydantic.NewModel("RoomLinkStatus", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("catalog", pydantic.Nullable(RoomLinkCatalog), nil),
	pydantic.Def("endpoint", pydantic.Nullable(RoomLinkEndpoint), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
).Described("``enabled`` with ``profile``/``catalog``/``endpoint``, or disabled with a ``reason``.")

var GroupsCapabilitiesParams = pydantic.Extend(ProfileParams, "GroupsCapabilitiesParams", ParamsConfig)

var GroupsCapabilitiesResult = pydantic.NewModel("GroupsCapabilitiesResult", ClosedConfig,
	pydantic.Req("protocol_version", pydantic.Int),
	pydantic.Req("driver", pydantic.Bool),
	pydantic.Req("persistent_process", pydantic.Bool),
	pydantic.Req("authority_gateway_id", pydantic.Str),
	pydantic.Req("room_link", RoomLinkStatus),
	pydantic.Req("features", pydantic.List(pydantic.Str)),
	pydantic.Req("methods", pydantic.List(pydantic.Str)),
	pydantic.Req("max_log_limit", pydantic.Int),
	pydantic.Def("seatable_profiles", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
)

var _ = Method("groups.capabilities", GroupsCapabilitiesParams, GroupsCapabilitiesResult, "Describe the hosted-room protocol implemented by this gateway.")

var GroupsListParams = pydantic.Extend(ProfileParams, "GroupsListParams", ParamsConfig,
	pydantic.Def("include_disbanded", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("limit", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("offset", pydantic.Nullable(pydantic.Int), nil),
)

var GroupsListResult = pydantic.NewModel("GroupsListResult", ClosedConfig,
	pydantic.Req("rooms", pydantic.List(Room)),
	pydantic.Def("next_offset", pydantic.Nullable(pydantic.Int), nil),
)

var _ = Method("groups.list", GroupsListParams, GroupsListResult, "List rooms hosted by this gateway, most recently changed first.")

var GroupsCreateParams = pydantic.Extend(ProfileParams, "GroupsCreateParams", ParamsConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("members", pydantic.List(RoomMemberInput)),
	pydantic.Def("authority_gateway_id", pydantic.Nullable(pydantic.Str), nil),
)

var GroupsCreateResult = pydantic.NewModel("GroupsCreateResult", ClosedConfig,
	pydantic.Req("room", Room),
)

var _ = Method("groups.create", GroupsCreateParams, GroupsCreateResult, "Create a hosted room idempotently; authority is this gateway's stable install identity.")

var GroupsStateParams = pydantic.Extend(RoomParams, "GroupsStateParams", ParamsConfig,
	pydantic.Def("include_disbanded", pydantic.Nullable(pydantic.Bool), nil),
)

var PeerRouteStatus = pydantic.NewModel("PeerRouteStatus", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("member_id", pydantic.Str),
	pydantic.Req("status", pydantic.Str),
)

var RoomMemberActivity = pydantic.NewModel("RoomMemberActivity", ClosedConfig,
	pydantic.Req("member_id", pydantic.Str),
	pydantic.Req("status", pydantic.Str),
).Described("One live room task: ``status`` is ``queued``, ``running`` or ``stopping``.")

var RoomDriverStatus = pydantic.NewModel("RoomDriverStatus", ClosedConfig,
	pydantic.Req("running", pydantic.Bool),
	pydantic.Req("working", pydantic.Bool),
	pydantic.Req("blocked", pydantic.Bool),
	pydantic.Req("counts", pydantic.Dict(pydantic.Int)),
	pydantic.Req("pending_actions", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Fac("member_activity", pydantic.List(RoomMemberActivity), pydantic.NewList),
	pydantic.Def("last_error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("peer_routes", pydantic.List(PeerRouteStatus)),
).Described("``HostedRoomService.status(room_id)``; ``pending_actions`` rows are the exact attempt\n(``hosted_room_service.attempt_action``: ``kind`` + ``member_id``/``task_id``/``execution_generation``)\nor the driver's approval action (``kind: approval`` + run/session/approval context), or a stalled\nroom preparation's Retry (``task_id`` ``prepare:<room_id>``, ``member_id`` the members not woken).\n``member_activity`` lists live tasks in admission order (absent from older gateways).\n``last_error`` is this room's last preparation failure, null once one succeeds.")

var GroupsStateResult = pydantic.NewModel("GroupsStateResult", ClosedConfig,
	pydantic.Req("room", Room),
	pydantic.Def("driver_status", pydantic.Nullable(RoomDriverStatus), nil),
)

var _ = Method("groups.state", GroupsStateParams, GroupsStateResult, "One hosted room's replay cursor and fenced authority state, plus live driver status.")

var GroupsSendParams = pydantic.Extend(RoomParams, "GroupsSendParams", ParamsConfig,
	pydantic.Def("event_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("payload", pydantic.Dict(pydantic.Any)),
)

var GroupsSendResult = pydantic.NewModel("GroupsSendResult", ClosedConfig,
	pydantic.Def("event", pydantic.Nullable(RoomEvent), nil),
	pydantic.Def("client_event_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("accepted", pydantic.Bool, true),
	pydantic.Def("driver_started", pydantic.Bool, true),
	pydantic.Def("queued", pydantic.Nullable(pydantic.Bool), nil),
).Described("``queued`` without an ``event``: the room is a replica and its home appends the message on its next sync.")

var _ = Method("groups.send", GroupsSendParams, GroupsSendResult, "Append one inert message.user event idempotently; the actor is server-owned.")

var GroupsRenameParams = pydantic.Extend(RoomParams, "GroupsRenameParams", ParamsConfig,
	pydantic.Req("event_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
)

var GroupsRenameResult = pydantic.NewModel("GroupsRenameResult", ClosedConfig,
	pydantic.Req("room", Room),
	pydantic.Def("queued", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("groups.rename", GroupsRenameParams, GroupsRenameResult, "Rename one hosted room atomically with its replay event.")

var GroupsLogParams = pydantic.Extend(RoomParams, "GroupsLogParams", ParamsConfig,
	pydantic.Def("since_seq", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("limit", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("include_disbanded", pydantic.Nullable(pydantic.Bool), nil),
)

var GroupsLogResult = pydantic.NewModel("GroupsLogResult", ClosedConfig,
	pydantic.Req("events", pydantic.List(RoomEvent)),
	pydantic.Req("cursor", pydantic.Int),
	pydantic.Req("latest_seq", pydantic.Int),
	pydantic.Req("has_more", pydantic.Bool),
	pydantic.Req("authority", RoomAuthority),
).Described("``gateway/hosted_rooms.py::read_events`` page — also the ``page`` a room home pushes to the peers it shares the room with.")

var _ = Method("groups.log", GroupsLogParams, GroupsLogResult, "A monotonic room-log delta after since_seq, bounded by count and page bytes.")

var GroupsDisbandParams = pydantic.Extend(RoomParams, "GroupsDisbandParams", ParamsConfig,
	pydantic.Def("cancel_id", pydantic.Nullable(pydantic.Str), nil),
)

var RoomTombstone = pydantic.NewModel("RoomTombstone", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("disbanded_at", pydantic.Float),
	pydantic.Req("idempotent", pydantic.Bool),
	pydantic.Def("history_expired", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("event", pydantic.Nullable(RoomEvent), nil),
)

var GroupsDisbandResult = pydantic.NewModel("GroupsDisbandResult", ClosedConfig,
	pydantic.Def("tombstone", pydantic.Nullable(RoomTombstone), nil),
	pydantic.Def("queued", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("groups.disband", GroupsDisbandParams, GroupsDisbandResult, "Permanently tombstone a hosted room id after stopping its work and revoking peer routes.")

var GroupsStopParams = pydantic.Extend(RoomParams, "GroupsStopParams", ParamsConfig,
	pydantic.Def("cancel_id", pydantic.Nullable(pydantic.Str), nil),
)

var GroupsStopResult = pydantic.NewModel("GroupsStopResult", ClosedConfig,
	pydantic.Req("cancelled", pydantic.Int),
	pydantic.Def("queued", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("groups.stop", GroupsStopParams, GroupsStopResult, "Durably cancel queued or running work for one hosted room.")

var GroupsApproveParams = pydantic.Extend(RoomParams, "GroupsApproveParams", ParamsConfig,
	pydantic.Req("member_id", pydantic.Str),
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("choice", ApprovalChoice),
	pydantic.Req("request_id", pydantic.Str),
)

var GroupsApproveResult = pydantic.NewModel("GroupsApproveResult", ClosedConfig,
	pydantic.Def("approved", pydantic.Bool, true),
	pydantic.Req("result", pydantic.Dict(pydantic.Any)),
	pydantic.Def("queued", pydantic.Nullable(pydantic.Bool), nil),
).Described("``result`` is the local ``approval.respond`` answer or the peer's run-action receipt.")

var _ = Method("groups.approve", GroupsApproveParams, GroupsApproveResult, "Resolve one exact pending approval raised by a local or peer room member.")

var GroupsRetryParams = pydantic.Extend(RoomParams, "GroupsRetryParams", ParamsConfig,
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Def("member_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("execution_generation", pydantic.Nullable(pydantic.Int), nil),
)

var RoomTaskReceipt = pydantic.NewModel("RoomTaskReceipt", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("task_id", pydantic.Str),
	pydantic.Req("thread_id", pydantic.Str),
	pydantic.Req("turn_id", pydantic.Str),
	pydantic.Req("status", pydantic.Str),
	pydantic.Req("execution_generation", pydantic.Int),
	pydantic.Req("cancel_generation", pydantic.Int),
)

var GroupsRetryResult = pydantic.NewModel("GroupsRetryResult", ClosedConfig,
	pydantic.Def("retried", pydantic.Bool, true),
	pydantic.Def("task", pydantic.Nullable(RoomTaskReceipt), nil),
	pydantic.Def("queued", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("groups.retry", GroupsRetryParams, GroupsRetryResult, "Retry one indeterminate room task after explicit user confirmation.")

var GroupsReplicaStateParams = pydantic.Extend(RoomParams, "GroupsReplicaStateParams", ParamsConfig)

var RoomQueuedError = pydantic.NewModel("RoomQueuedError", ClosedConfig,
	pydantic.Req("outbox_id", pydantic.Int),
	pydantic.Req("op", pydantic.Str),
	pydantic.Req("error", pydantic.Str),
)

var GroupsReplicaStateResult = pydantic.NewModel("GroupsReplicaStateResult", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("members", pydantic.List(RoomMember)),
	pydantic.Req("authority", RoomAuthority),
	pydantic.Req("last_seq", pydantic.Int),
	pydantic.Req("latest_seq", pydantic.Int),
	pydantic.Req("event_bytes", pydantic.Int),
	pydantic.Req("created_at", pydantic.Float),
	pydantic.Req("updated_at", pydantic.Float),
	pydantic.Def("home", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("synced_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("disbanded_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("pending", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("errors", pydantic.Nullable(pydantic.List(RoomQueuedError)), nil),
).Described("``home``, ``synced_at``, ``pending`` (operations queued for the home) and ``errors`` (ones it refused) on the canonical route.")

var _ = Method("groups.replica_state", GroupsReplicaStateParams, GroupsReplicaStateResult, "The local replica's coverage and authority lineage for one room.")

var GroupsPromoteParams = pydantic.Extend(RoomParams, "GroupsPromoteParams", ParamsConfig,
	pydantic.Def("confirm", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
)

var GroupsPromoteResult = pydantic.NewModel("GroupsPromoteResult", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("authority_gateway_id", pydantic.Str),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Req("previous_gateway_id", pydantic.Str),
	pydantic.Req("previous_epoch", pydantic.Int),
	pydantic.Req("claim_seq", pydantic.Int),
	pydantic.Req("latest_seq", pydantic.Int),
)

var _ = Method("groups.promote", GroupsPromoteParams, GroupsPromoteResult, "Continue a replicated room on this gateway at epoch + 1; requires confirm=true.")
