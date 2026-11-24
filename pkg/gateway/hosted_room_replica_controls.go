package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Whether a room id names a replica here rather than a room of this gateway's own store
func groupIsReplica(ctx context.Context, dbPath, roomID string) (bool, error) {
	if !roomIdentifierRe.MatchString(roomID) {
		return false, nil
	}
	if _, err := RoomState(ctx, dbPath, roomID, true); err == nil {
		return false, nil
	}
	link, err := ReplicaLink(ctx, dbPath, roomID)
	return link != nil, err
}

// A listing page with the replicated rooms added to its first page
func groupWithReplicas(ctx context.Context, dbPath string, params map[string]any, listed any) (any, error) {
	page, ok := listed.(*jsonutil.Object)
	if !ok {
		return listed, nil
	}
	if offset, present := params["offset"]; present {
		if n, err := anyutil.ToInt(offset); err != nil || n != 0 {
			return listed, err
		}
	}
	include, err := groupIntFlag(params)
	if err != nil {
		return nil, err
	}
	replicas, err := ReplicaRooms(ctx, dbPath, include)
	if err != nil {
		return nil, err
	}
	rooms, _ := page.ValueOr("rooms", []any{}).([]any)
	page.Set("rooms", append(append([]any{}, rooms...), replicas...))
	return page, nil
}

// Room verb on a replica, reads answered here and changes queued for the room's home
func groupReplicaControl(ctx context.Context, actor Principal, service *CanonicalHostedRoomService, method string, params map[string]any, dbPath, roomID string) (any, error) {
	link, err := ReplicaLink(ctx, dbPath, roomID)
	if err != nil {
		return nil, err
	}
	include, err := groupIntFlag(params)
	if err != nil {
		return nil, err
	}
	queue := func(op string, fields map[string]any) error {
		_, err := QueueReplicaOp(ctx, dbPath, roomID, op, fields)
		return err
	}
	attempt := func() map[string]any {
		return map[string]any{"member_id": params["member_id"], "task_id": params["task_id"], "execution_generation": params["execution_generation"]}
	}
	switch method {
	case "groups.state":
		room, err := ReplicaRoom(ctx, dbPath, roomID, include)
		if err != nil {
			return nil, err
		}
		result := jsonutil.ObjectOf("room", room)
		if link["status"] != nil && room["disbanded_at"] == nil {
			result.Set("driver_status", link["status"])
		}
		return result, nil
	case "groups.log":
		since, ok := params["since_seq"]
		if !ok {
			since = int64(0)
		}
		limit, ok := params["limit"]
		if !ok {
			limit = int64(100)
		}
		return ReplicaReadEvents(ctx, dbPath, roomID, since, limit)
	case "groups.replica_state":
		return ReplicaStateView(ctx, dbPath, roomID)
	case "groups.peers":
		peers, err := RoomPeerRegistry(ctx)
		if err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("peers", peers, "shares", []any{}), nil
	case "groups.share", "groups.unshare":
		return nil, roomRefusal("invalid_params", "only the room's home "+anyutil.String(link["home"])+" shares it")
	case "groups.attachment.upload":
		return ReplicaUpload(ctx, dbPath, roomID, params)
	case "groups.attachment.download":
		return ReplicaDownload(ctx, dbPath, roomID, params)
	}
	if link["disbanded_at"] != nil {
		return nil, roomRefusal("invalid_params", "this room was disbanded")
	}
	switch method {
	case "groups.send":
		payload, err := ValidateUserPayload(params["payload"])
		if err != nil {
			return nil, err
		}
		eventID, err := UserEventID(params["event_id"])
		if err != nil {
			return nil, err
		}
		if err := replicaUploadsPresent(ctx, dbPath, roomID, payload["attachments"]); err != nil {
			return nil, err
		}
		if err := queue("send", map[string]any{"event_id": eventID, "payload": payload}); err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("event", nil, "client_event_id", params["event_id"], "accepted", true, "driver_started", false, "queued", true), nil
	case "groups.rename":
		if err := groupRequire(params, "event_id", "name"); err != nil {
			return nil, err
		}
		name, err := hostedRoomName(params["name"])
		if err != nil {
			return nil, err
		}
		if err := queue("rename", map[string]any{"event_id": params["event_id"], "name": name}); err != nil {
			return nil, err
		}
		room, err := ReplicaRoom(ctx, dbPath, roomID, false)
		if err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("room", room, "queued", true), nil
	case "groups.stop":
		if err := queue("stop", map[string]any{"cancel_id": groupOr(params, "cancel_id", "group-stop")}); err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("cancelled", int64(0), "queued", true), nil
	case "groups.continue":
		if err := queue("continue", map[string]any{"discussion_event_id": anyutil.String(groupOr(params, "discussion_event_id", ""))}); err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("continued", true, "event", nil, "queued", true), nil
	case "groups.disband":
		if err := queue("disband", map[string]any{"cancel_id": groupOr(params, "cancel_id", "room-disbanded")}); err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("tombstone", nil, "queued", true), nil
	case "groups.approve":
		fields := attempt()
		fields["choice"], fields["request_id"] = params["choice"], params["request_id"]
		if err := queue("approve", fields); err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("approved", true, "result", map[string]any{}, "queued", true), nil
	case "groups.retry", "groups.discard":
		op := method[len("groups."):]
		if err := queue(op, attempt()); err != nil {
			return nil, err
		}
		verb := "retried"
		if op == "discard" {
			verb = "discarded"
		}
		return jsonutil.ObjectOf(verb, true, "task", nil, "queued", true), nil
	case "groups.promote":
		if params["confirm"] != true {
			return nil, roomRefusal("invalid_params", "continuing a room here needs confirm")
		}
		if service == nil {
			return nil, runtimeRefusal("runtime_coordination_required")
		}
		reason := groupOr(params, "reason", "the home gateway "+anyutil.String(link["home"])+" is gone")
		return service.PromoteReplicaRoom(actor, roomID, reason)
	}
	return nil, &pyKeyError{key: method}
}

// Refusal unless every manifest item names a file uploaded to this replica
func replicaUploadsPresent(ctx context.Context, dbPath, roomID string, manifest any) error {
	items, _ := manifest.([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		var row *jsonutil.Object
		err := replicaViewTransaction(ctx, dbPath, false, func(conn state.SQLiteConn) (err error) {
			row, err = state.QueryRowObject(ctx, conn, "SELECT 1 AS hit FROM hosted_room_replica_uploads WHERE room_id=? AND attachment_id=?", roomID, item["attachment_id"])
			return err
		})
		if err != nil {
			return err
		}
		if row == nil {
			return roomRefusal("invalid_params", "an attachment of the message was not uploaded to this room")
		}
	}
	return nil
}
