package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var RoomMember = pydantic.NewModel("RoomMember", OpenConfig,
	pydantic.Def("member_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("handle", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("target", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
)

var RoomActor = pydantic.NewModel("RoomActor", ClosedConfig,
	pydantic.Req("kind", pydantic.Str),
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("display_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("connection_id", pydantic.Nullable(pydantic.Str), nil),
)

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
)

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
)

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
)

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
)

var RoomLinkEndpoint = pydantic.NewModel("RoomLinkEndpoint", ClosedConfig,
	pydantic.Req("available", pydantic.Bool),
	pydantic.Def("url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("transport_security", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
)

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
)

var RoomLinkStatus = pydantic.NewModel("RoomLinkStatus", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("catalog", pydantic.Nullable(RoomLinkCatalog), nil),
	pydantic.Def("endpoint", pydantic.Nullable(RoomLinkEndpoint), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
)

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
)

var RoomDriverStatus = pydantic.NewModel("RoomDriverStatus", ClosedConfig,
	pydantic.Req("running", pydantic.Bool),
	pydantic.Req("working", pydantic.Bool),
	pydantic.Req("blocked", pydantic.Bool),
	pydantic.Req("counts", pydantic.Dict(pydantic.Int)),
	pydantic.Req("pending_actions", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Fac("member_activity", pydantic.List(RoomMemberActivity), pydantic.NewList),
	pydantic.Def("last_error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("peer_routes", pydantic.List(PeerRouteStatus)),
)

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
	pydantic.Req("event", RoomEvent),
	pydantic.Def("client_event_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("accepted", pydantic.Bool, true),
	pydantic.Def("driver_started", pydantic.Bool, true),
)

var _ = Method("groups.send", GroupsSendParams, GroupsSendResult, "Append one inert message.user event idempotently; the actor is server-owned.")

var GroupsRenameParams = pydantic.Extend(RoomParams, "GroupsRenameParams", ParamsConfig,
	pydantic.Req("event_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
)

var GroupsRenameResult = pydantic.NewModel("GroupsRenameResult", ClosedConfig,
	pydantic.Req("room", Room),
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
)

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
	pydantic.Req("tombstone", RoomTombstone),
)

var _ = Method("groups.disband", GroupsDisbandParams, GroupsDisbandResult, "Permanently tombstone a hosted room id after stopping its work and revoking peer routes.")

var GroupsStopParams = pydantic.Extend(RoomParams, "GroupsStopParams", ParamsConfig,
	pydantic.Def("cancel_id", pydantic.Nullable(pydantic.Str), nil),
)

var GroupsStopResult = pydantic.NewModel("GroupsStopResult", ClosedConfig,
	pydantic.Req("cancelled", pydantic.Int),
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
)

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
	pydantic.Req("task", RoomTaskReceipt),
)

var _ = Method("groups.retry", GroupsRetryParams, GroupsRetryResult, "Retry one indeterminate room task after explicit user confirmation.")

var GroupsReplicateParams = pydantic.Extend(RoomParams, "GroupsReplicateParams", ParamsConfig,
	pydantic.Req("room_name", pydantic.Str),
	pydantic.Req("members", pydantic.List(RoomMemberInput)),
	pydantic.Req("page", pydantic.Dict(pydantic.Any)),
)

var GroupsReplicateResult = pydantic.NewModel("GroupsReplicateResult", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("stored_seq", pydantic.Int),
	pydantic.Req("ingested", pydantic.Int),
	pydantic.Req("authority", RoomAuthority),
	pydantic.Req("caught_up", pydantic.Bool),
)

var _ = Method("groups.replicate", GroupsReplicateParams, GroupsReplicateResult, "Persist one authority-stamped replay page into the local replica store; idempotent.")

var GroupsReplicaStateParams = pydantic.Extend(RoomParams, "GroupsReplicaStateParams", ParamsConfig)

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
)

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

var GroupsDemoteParams = pydantic.Extend(RoomParams, "GroupsDemoteParams", ParamsConfig,
	pydantic.Req("observed_gateway_id", pydantic.Str),
	pydantic.Req("observed_epoch", pydantic.Int),
)

var GroupsDemoteResult = pydantic.NewModel("GroupsDemoteResult", ClosedConfig,
	pydantic.Req("room_id", pydantic.Str),
	pydantic.Req("authority_gateway_id", pydantic.Str),
	pydantic.Req("authority_epoch", pydantic.Int),
	pydantic.Req("idempotent", pydantic.Bool),
)

var _ = Method("groups.demote", GroupsDemoteParams, GroupsDemoteResult, "Fence this gateway's stale room authority against a proven newer epoch.")

var GroupsPeerInviteParams = pydantic.Extend(ProfileParams, "GroupsPeerInviteParams", ParamsConfig,
	pydantic.Def("room_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("home_install_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("authority_gateway_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("authority_epoch", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("member_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("grant_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("ttl_seconds", pydantic.Nullable(pydantic.Float), nil),
)

var GroupsPeerInviteResult = pydantic.NewModel("GroupsPeerInviteResult", ClosedConfig,
	pydantic.Req("grant", pydantic.Str),
	pydantic.Req("target_profile", pydantic.Str),
	pydantic.Req("catalog", RoomLinkCatalog),
	pydantic.Req("endpoint", RoomLinkEndpoint),
)

var _ = Method("groups.peer.invite", GroupsPeerInviteParams, GroupsPeerInviteResult, "Mint one target-issued room/profile grant for a prospective room home.")

var GroupsPeerRevokeParams = pydantic.Extend(ProfileParams, "GroupsPeerRevokeParams", ParamsConfig,
	pydantic.Req("grant", pydantic.Str),
)

var GroupsPeerRevokeResult = pydantic.NewModel("GroupsPeerRevokeResult", ClosedConfig,
	pydantic.Def("revoked", pydantic.Bool, true),
)

var _ = Method("groups.peer.revoke", GroupsPeerRevokeParams, GroupsPeerRevokeResult, "Revoke one target-issued grant using its exact profile scope.")

var GroupsPeerRegisterParams = pydantic.Extend(RoomParams, "GroupsPeerRegisterParams", ParamsConfig,
	pydantic.Req("member_id", pydantic.Str),
	pydantic.Req("target_url", pydantic.Str),
	pydantic.Req("target_profile", pydantic.Str),
	pydantic.Req("grant", pydantic.Str),
	pydantic.Req("catalog", pydantic.Dict(pydantic.Any)),
	pydantic.Def("cancellation_scope_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("trace_id", pydantic.Nullable(pydantic.Str), nil),
)

var GroupsPeerRegisterResult = pydantic.NewModel("GroupsPeerRegisterResult", ClosedConfig,
	pydantic.Def("registered", pydantic.Bool, true),
	pydantic.Req("mode", pydantic.Str),
	pydantic.Req("transport_security", pydantic.Str),
	pydantic.Req("target_install_id", pydantic.Str),
	pydantic.Req("target_profile", pydantic.Str),
)

var _ = Method("groups.peer.register", GroupsPeerRegisterParams, GroupsPeerRegisterResult, "Register and probe one scoped peer route on the room home.")

var RelayAgentRow = pydantic.NewModel("RelayAgentRow", OpenParamsConfig,
	pydantic.Def("profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("handle", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("connection_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("connection_label", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("title", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("online", pydantic.Nullable(pydantic.Bool), nil),
)

var BotRelayRosterSyncParams = pydantic.Extend(ProfileParams, "BotRelayRosterSyncParams", ParamsConfig,
	pydantic.Def("agents", pydantic.Nullable(pydantic.List(RelayAgentRow)), nil),
)

var BotRelayRosterSyncResult = pydantic.NewModel("BotRelayRosterSyncResult", ClosedConfig,
	pydantic.Req("count", pydantic.Int),
)

var _ = Method("bot_relay.roster.sync", BotRelayRosterSyncParams, BotRelayRosterSyncResult, "Replace this gateway's view of agents on other connections; answers the accepted row count.")

var BotRelayOutboxDrainParams = pydantic.Extend(ProfileParams, "BotRelayOutboxDrainParams", ParamsConfig)

var RelayEnvelope = pydantic.NewModel("RelayEnvelope", OpenConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Req("created_at", pydantic.Union(pydantic.Int, pydantic.Float)),
	pydantic.Req("from_profile", pydantic.Str),
	pydantic.Req("from_handle", pydantic.Str),
	pydantic.Req("target_connection", pydantic.Str),
	pydantic.Req("target_profile", pydantic.Str),
	pydantic.Req("target_handle", pydantic.Str),
	pydantic.Req("message", pydantic.Str),
)

var BotRelayOutboxDrainResult = pydantic.NewModel("BotRelayOutboxDrainResult", ClosedConfig,
	pydantic.Req("envelopes", pydantic.List(RelayEnvelope)),
)

var _ = Method("bot_relay.outbox.drain", BotRelayOutboxDrainParams, BotRelayOutboxDrainResult, "Atomically claim every pending cross-connection envelope queued on this gateway.")

var BotRelayDeliverParams = pydantic.NewModel("BotRelayDeliverParams", ParamsConfig,
	pydantic.Req("profile", pydantic.Str),
	pydantic.Req("message", pydantic.Str),
	pydantic.Def("from_profile", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("from_handle", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("from_connection", pydantic.Nullable(pydantic.Str), nil),
)

var BotRelayDeliverResult = pydantic.NewModel("BotRelayDeliverResult", ClosedConfig,
	pydantic.Req("reply", pydantic.Str),
)

var _ = Method("bot_relay.deliver", BotRelayDeliverParams, BotRelayDeliverResult, "Deliver a relayed DM into a Bot Chat on this gateway and return the one-turn reply (blocking).")

var BotRelayReplyParams = pydantic.Extend(ProfileParams, "BotRelayReplyParams", ParamsConfig,
	pydantic.Req("id", pydantic.Str),
	pydantic.Def("reply", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("bot_relay.reply", BotRelayReplyParams, OkResult, "Write a relayed reply and/or typed error for an envelope so the sender-side waiter resolves.")
