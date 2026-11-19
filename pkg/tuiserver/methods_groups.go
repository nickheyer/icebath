package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Wire order of groups.capabilities methods, every one running on the RPC pool
var groupsMethods = []string{"groups.capabilities", "groups.list", "groups.create", "groups.state", "groups.send",
	"groups.rename", "groups.log", "groups.disband", "groups.replica_state", "groups.promote", "groups.stop",
	"groups.retry", "groups.approve", "groups.attachment.upload", "groups.attachment.download"}

// Refusal texts of a missing hosted room worker and driver
const (
	groupsWorkerUnavailable = "Group Chat worker is unavailable. Restart the Icebath gateway and try again."
	groupsDriverUnavailable = "hosted room driver is unavailable"
)

// Shared run idempotency store whose durability decides whether peer run replay survives the process
var (
	groupsRunStoreMu sync.Mutex
	groupsRunStore   *gateway.RunIdempotencyStore
)

// Shared hosted room error envelope of one room method
type roomMethodSpec struct {
	code, roomCode, serviceCode int
	replicaOnly, noReason, db   bool
	serviceMessage              string
}

// Handler of one room method, handed the live service when the spec requires it and the store path when asked
type roomBody func(ctx context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, dbPath string) (map[string]any, error)

var groupsRegistry = &handlerRegistry{}

func init() {
	groupsRegistry.method("groups.capabilities", groupsCapabilities)
	roomMethod("groups.list", roomMethodSpec{code: 5110, db: true}, groupsListLocal)
	roomMethod("groups.create", roomMethodSpec{code: 5111, roomCode: 4110, serviceCode: 4123, serviceMessage: groupsWorkerUnavailable}, groupsCreate)
	roomMethod("groups.state", roomMethodSpec{code: 5115, roomCode: 4114, db: true}, groupsState)
	roomMethod("groups.send", roomMethodSpec{code: 5112, roomCode: 4111, serviceCode: 4123, serviceMessage: groupsWorkerUnavailable}, groupsSend)
	roomMethod("groups.attachment.upload", roomMethodSpec{code: 5121, roomCode: 4125, serviceCode: 4123, serviceMessage: groupsWorkerUnavailable}, groupsAttachmentUpload)
	roomMethod("groups.attachment.download", roomMethodSpec{code: 5123, roomCode: 4126, serviceCode: 4115}, groupsAttachmentDownload)
	roomMethod("groups.disband", roomMethodSpec{code: 5114, roomCode: 4113, serviceCode: 4123, serviceMessage: groupsWorkerUnavailable}, groupsDisband)
	roomMethod("groups.stop", roomMethodSpec{code: 5116, serviceCode: 4115}, groupsStop)
	roomMethod("groups.approve", roomMethodSpec{code: 5119, serviceCode: 4115}, groupsApprove)
	roomMethod("groups.retry", roomMethodSpec{code: 5118, serviceCode: 4115}, groupsRetry)
	roomMethod("groups.discard", roomMethodSpec{code: 5125}, groupsDiscard)
	roomMethod("groups.rename", roomMethodSpec{code: 5117, roomCode: 4117, db: true}, func(ctx context.Context, rid any, p map[string]any, _ *gateway.HostedRoomService, dbPath string) (map[string]any, error) {
		room, err := gateway.RenameRoom(ctx, dbPath, p["room_id"], p["event_id"], p["name"], nil)
		if err != nil {
			return nil, err
		}
		return ok(rid, map[string]any{"room": room}), nil
	})
	roomMethod("groups.log", roomMethodSpec{code: 5113, roomCode: 4112, db: true}, func(ctx context.Context, rid any, p map[string]any, _ *gateway.HostedRoomService, dbPath string) (map[string]any, error) {
		result, err := gateway.ReadEvents(ctx, dbPath, p["room_id"], paramOr(p, "since_seq", 0), paramOr(p, "limit", 100), p["include_disbanded"] == true)
		if err != nil {
			return nil, err
		}
		return ok(rid, result), nil
	})
	roomMethod("groups.replica_state", roomMethodSpec{code: 5117, roomCode: 4117, replicaOnly: true, noReason: true, db: true}, func(ctx context.Context, rid any, p map[string]any, _ *gateway.HostedRoomService, dbPath string) (map[string]any, error) {
		result, err := gateway.ReplicaState(ctx, dbPath, p["room_id"])
		if err != nil {
			return nil, err
		}
		return ok(rid, result), nil
	})
	roomMethod("groups.promote", roomMethodSpec{code: 5118, roomCode: 4118, noReason: true, db: true}, groupsPromote)
	groupsRegistry.install()
	addLongHandlers(append(append([]string(nil), groupsMethods...), "groups.discard")...)
}

// Live hosted room service of this process, nil unless its lifecycle owner started it and it is running
func hostedRoomService() *gateway.HostedRoomService {
	service, _ := tuihost.GetHostedRoomService().(*gateway.HostedRoomService)
	return service
}

// Profile this process runs as, ICEBATH_PROFILE else default
func groupsProfileName() string {
	if name := strings.TrimSpace(os.Getenv("ICEBATH_PROFILE")); name != "" {
		return name
	}
	return "default"
}

// Home of a routed profile other than the process's own, a ValueError for the launch profile
func groupsForeignProfileHome(profile string) (string, error) {
	profileDir, err := profileHome(profile)
	if err != nil {
		return "", err
	}
	if profileDir == "" {
		return "", fmt.Errorf("profile '%s' is unavailable", profile)
	}
	return profileDir, nil
}

// Profile a room call names, the process profile without one, a routed one checked to exist here
func groupsRequestedProfile(ctx context.Context, params map[string]any) (string, error) {
	requested := strings.TrimSpace(relayString(params["profile"]))
	if requested == "" {
		return groupsProfileName(), nil
	}
	if requested == strings.TrimSpace(currentProfileName(ctx)) {
		return requested, nil
	}
	if _, err := groupsForeignProfileHome(requested); err != nil {
		return "", err
	}
	if name := responseProfileName(ctx, requested); name != "" {
		return name, nil
	}
	return requested, nil
}

// Execution policy of a profile resolved under that profile's full runtime scope
func groupsExecutionPolicy(ctx context.Context, profile string) (*pyjson.Object, error) {
	scopeHome := ""
	if profile != strings.TrimSpace(currentProfileName(ctx)) && profile != groupsProfileName() {
		foreign, err := groupsForeignProfileHome(profile)
		if err != nil {
			return nil, err
		}
		scopeHome = foreign
	}
	return gateway.ExecutionPolicyMapping(profileRuntimeScope(ctx, scopeHome, true), profile, nil)
}

// Whether peer run replay survives this gateway process, the shared run store opened on first use
func roomLinkRunStorageDurable(ctx context.Context) bool {
	groupsRunStoreMu.Lock()
	defer groupsRunStoreMu.Unlock()
	if groupsRunStore == nil {
		store, err := gateway.NewRunIdempotencyStore(ctx, "")
		if err != nil {
			panic(err)
		}
		groupsRunStore = store
	}
	return groupsRunStore.Durable()
}

// This gateway's direct only, text only RoomLink catalog
func groupsLocalCatalog(ctx context.Context, installationID, profile string, policy *pyjson.Object) (*pyjson.Object, error) {
	text := true
	return gateway.LocalCatalogMapping(ctx, gateway.CatalogOptions{InstallationID: installationID, ProtocolVersions: []any{gateway.HostedProtocolVersion},
		LinkModes: []string{"direct"}, Text: &text, Attachments: false, TargetProfile: profile, ExecutionPolicy: policy})
}

// Registers a room method under the shared envelope, forwarding to an owning gateway first
func roomMethod(name string, spec roomMethodSpec, body roomBody) {
	if spec.serviceMessage == "" {
		spec.serviceMessage = groupsDriverUnavailable
	}
	groupsRegistry.method(name, func(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
		if forwarded := forwardGroups(ctx, rid, name, params); forwarded != nil {
			return forwarded
		}
		var service *gateway.HostedRoomService
		if spec.serviceCode != 0 {
			if service = hostedRoomService(); service == nil {
				return rpcErr(rid, spec.serviceCode, spec.serviceMessage, nil)
			}
		}
		dbPath := ""
		if spec.db {
			dbPath = gateway.HostedRoomsDefaultDBPath(ctx)
		}
		fail := func(err error) map[string]any {
			if spec.roomCode != 0 && roomErrorClass(err, spec.replicaOnly) {
				var data any
				var reasoned interface{ Reason() string }
				if !spec.noReason && errors.As(err, &reasoned) && reasoned.Reason() != "" {
					data = map[string]any{"reason": reasoned.Reason()}
				}
				return rpcErr(rid, spec.roomCode, err.Error(), data)
			}
			return rpcErr(rid, spec.code, err.Error(), nil)
		}
		defer func() {
			if failure := recover(); failure != nil {
				if configOutage(failure) {
					panic(failure)
				}
				err, isError := failure.(error)
				if !isError {
					err = errors.New(failureText(failure))
				}
				response = fail(err)
			}
		}()
		out, err := body(ctx, rid, params, service, dbPath)
		if err != nil {
			return fail(err)
		}
		return out
	})
}

// Whether an error is a hosted room error, only a replica error when the method is replica only
func roomErrorClass(err error, replicaOnly bool) bool {
	if replicaOnly {
		var replica *gateway.ReplicaError
		return errors.As(err, &replica)
	}
	var room *gateway.HostedRoomError
	return errors.As(err, &room)
}

// The hosted room protocol this gateway implements and its RoomLink readiness
func groupsCapabilities(ctx context.Context, rid any, params map[string]any) map[string]any {
	if forwarded := forwardGroups(ctx, rid, "groups.capabilities", params); forwarded != nil {
		return forwarded
	}
	service := hostedRoomService()
	driverReady := service != nil && truthy(service.RuntimeStatus()["running"])
	authorityID, err := gateway.LocalAuthorityGatewayID(ctx)
	if err != nil {
		panic(err)
	}
	roomLink := func() (link map[string]any) {
		defer func() {
			if failure := recover(); failure != nil {
				if configOutage(failure) {
					panic(failure)
				}
				link = nil
			}
		}()
		profile, err := groupsRequestedProfile(ctx, params)
		if err != nil || !roomLinkRunStorageDurable(ctx) {
			return nil
		}
		if _, err := gateway.GatewayRoomGrantSecret(ctx, ""); err != nil {
			return nil
		}
		policy, err := groupsExecutionPolicy(ctx, profile)
		if err != nil {
			return nil
		}
		catalog, err := groupsLocalCatalog(ctx, authorityID, profile, policy)
		if err != nil {
			return nil
		}
		plain := pyjson.Plain(catalog).(map[string]any)
		return map[string]any{"enabled": true, "profile": profile, "catalog": plain, "endpoint": plain["endpoint"]}
	}()
	if roomLink == nil {
		reason := "gateway_roomlink_secret_unavailable"
		if !roomLinkRunStorageDurable(ctx) {
			reason = "durable_run_storage_required"
		}
		roomLink = map[string]any{"enabled": false, "reason": reason}
	}
	persistent := false
	if catalog, isDict := roomLink["catalog"].(map[string]any); isDict {
		persistent = truthy(catalog["persistent_process"])
	}
	methods := make([]any, len(groupsMethods))
	for i, name := range groupsMethods {
		methods[i] = name
	}
	return ok(rid, map[string]any{"protocol_version": gateway.HostedRoomsProtocolVersion, "driver": driverReady, "persistent_process": persistent,
		"authority_gateway_id": authorityID, "room_link": roomLink,
		"features": []any{"authority_epoch", "coordinator_fencing", "room_identity", "monotonic_log", "idempotent_send",
			"replayable_disband", "typed_events", "actor_identity", "log_replication", "authority_takeover"},
		"methods": methods, "max_log_limit": gateway.MaxRoomLogLimit})
}

// Python int() of a param, zero for a falsy one
func groupsInt(value any) (int64, error) {
	if !truthy(value) {
		return 0, nil
	}
	n, err := pyint.Of(value)
	if err != nil {
		return 0, errchain.Reword(err, "%s is not a whole number", errchain.Show(value))
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("the number %s is larger than a 64 bit integer holds", n.String())
	}
	return n.Int64(), nil
}

// Rooms hosted by this gateway with the next page offset
func groupsListLocal(ctx context.Context, rid any, params map[string]any, _ *gateway.HostedRoomService, dbPath string) (map[string]any, error) {
	limit, offset := paramOr(params, "limit", gateway.MaxRoomListLimit), paramOr(params, "offset", 0)
	rooms, err := gateway.ListRooms(ctx, dbPath, params["include_disbanded"] == true, limit, offset)
	if err != nil {
		return nil, err
	}
	var next any
	if limitInt, isInt := limit.(int); isInt && len(rooms) == limitInt {
		offsetInt, _ := offset.(int)
		next = offsetInt + limitInt
	}
	local := localRooms(ctx, rooms)
	out := make([]any, len(local))
	for i, room := range local {
		out[i] = room
	}
	return ok(rid, map[string]any{"rooms": out, "next_offset": next}), nil
}

// Creates a hosted room idempotently under this gateway's stable install identity
func groupsCreate(_ context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, _ string) (map[string]any, error) {
	room, err := service.CreateRoom(relayRoomText(params["room_id"]), relayRoomText(params["name"]), params["members"], nil)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"room": room}), nil
}

// String a room service call takes for a param, empty for a missing one
func relayRoomText(value any) string {
	text, _ := value.(string)
	return text
}

// One room's replay cursor and fenced authority state with its driver status while live
func groupsState(ctx context.Context, rid any, params map[string]any, _ *gateway.HostedRoomService, dbPath string) (map[string]any, error) {
	room, err := gateway.RoomState(ctx, dbPath, params["room_id"], params["include_disbanded"] == true)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"room": room}
	if service := hostedRoomService(); service != nil && room["disbanded_at"] == nil {
		roomID := pyrepr.Str(room["room_id"])
		status, err := service.Status(&roomID)
		if err != nil {
			return nil, err
		}
		result["driver_status"] = status
	}
	return ok(rid, result), nil
}

// Appends one user message event idempotently, the actor server owned
func groupsSend(_ context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, _ string) (map[string]any, error) {
	clientEventID := params["event_id"]
	eventID, err := gateway.UserEventID(clientEventID)
	if err != nil {
		return nil, err
	}
	event, err := service.Send(relayRoomText(params["room_id"]), eventID, params["payload"])
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"event": event, "client_event_id": clientEventID, "accepted": true, "driver_started": true}), nil
}

// Retains one composer file for a later groups.send manifest
func groupsAttachmentUpload(ctx context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, _ string) (map[string]any, error) {
	roomID := relayRoomText(params["room_id"])
	if _, _, err := service.OwnedAuthority(roomID); err != nil {
		return nil, err
	}
	if _, err := service.RoomRecord(roomID); err != nil {
		return nil, err
	}
	result, err := gateway.StoreUpload(ctx, service, roomID, params)
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Reads one retained attachment of a room event
func groupsAttachmentDownload(ctx context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, _ string) (map[string]any, error) {
	roomID := relayRoomText(params["room_id"])
	if _, err := service.RoomRecord(roomID); err != nil {
		return nil, err
	}
	result, err := gateway.ReadDownload(ctx, service, roomID, params)
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Stops and revokes a live room then writes its tombstone, a replayed disband answering the stored tombstone
func groupsDisband(ctx context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, _ string) (map[string]any, error) {
	roomID := relayString(params["room_id"])
	disbandWith := func(existing map[string]any) (map[string]any, error) {
		localID, err := gateway.LocalAuthorityGatewayID(ctx)
		if err != nil {
			return nil, err
		}
		if existing != nil && pyrepr.Str(existing["authority_gateway_id"]) != localID {
			return nil, &gateway.AuthorityConflictError{HostedRoomError: &gateway.HostedRoomError{Msg: "This Group Chat is managed by another gateway."}}
		}
		var epoch any = 1
		if existing != nil {
			epoch = existing["authority_epoch"]
		}
		epochInt, err := groupsInt(epoch)
		if err != nil {
			return nil, err
		}
		tombstone, err := gateway.DisbandRoom(ctx, service.StorePath(), params["room_id"], localID, epochInt, nil)
		if err != nil {
			return nil, err
		}
		return ok(rid, map[string]any{"tombstone": tombstone}), nil
	}
	existing, err := gateway.RoomState(ctx, service.StorePath(), params["room_id"], true)
	var expired *gateway.RoomHistoryExpiredError
	if errors.As(err, &expired) {
		return disbandWith(nil)
	}
	if err != nil {
		return nil, err
	}
	if existing["disbanded_at"] != nil {
		return disbandWith(existing)
	}
	cancelID := relayString(params["cancel_id"])
	if cancelID == "" {
		cancelID = "room-disbanded"
	}
	if _, err := service.StopRoom(roomID, cancelID, true); err != nil {
		return nil, err
	}
	if _, err := service.RevokeRoomRoutes(roomID); err != nil {
		return nil, err
	}
	return disbandWith(existing)
}

// Cancels a room's running work
func groupsStop(_ context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, _ string) (map[string]any, error) {
	cancelID := relayString(params["cancel_id"])
	if cancelID == "" {
		cancelID = "groups-stop"
	}
	count, err := service.StopRoom(relayString(params["room_id"]), cancelID, false)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"cancelled": count}), nil
}

// Answers one member task's pending approval
func groupsApprove(_ context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, _ string) (map[string]any, error) {
	generation, err := groupsInt(params["execution_generation"])
	if err != nil {
		return nil, err
	}
	requestID := relayString(params["request_id"])
	result, err := service.ApproveRoomTask(relayString(params["room_id"]), relayString(params["member_id"]), relayString(params["task_id"]),
		generation, relayString(params["choice"]), &requestID)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"approved": true, "result": result}), nil
}

// Retries one failed member task and answers its receipt
func groupsRetry(_ context.Context, rid any, params map[string]any, service *gateway.HostedRoomService, _ string) (map[string]any, error) {
	var memberID *string
	if text, isText := params["member_id"].(string); isText {
		memberID = &text
	}
	var generation *int64
	if raw, present := params["execution_generation"]; present && raw != nil {
		n, err := pyint.Of(raw)
		if err != nil {
			return nil, err
		}
		value := n.Int64()
		generation = &value
	}
	task, err := service.RetryRoomTask(relayString(params["room_id"]), relayString(params["task_id"]), memberID, generation)
	if err != nil {
		return nil, err
	}
	if task == nil {
		task = map[string]any{}
	}
	receipt := map[string]any{}
	identity, _ := task["identity"].(map[string]any)
	for _, field := range []string{"room_id", "task_id", "thread_id", "turn_id"} {
		receipt[field] = relayString(identity[field])
	}
	receipt["status"] = relayString(task["status"])
	for _, field := range []string{"execution_generation", "cancel_generation"} {
		value, err := groupsInt(task[field])
		if err != nil {
			return nil, err
		}
		receipt[field] = value
	}
	return ok(rid, map[string]any{"retried": true, "task": receipt}), nil
}

// Discard belongs to the gateway that owns the room, a room driven here never offers it
func groupsDiscard(_ context.Context, rid any, _ map[string]any, _ *gateway.HostedRoomService, _ string) (map[string]any, error) {
	return rpcErr(rid, 4127, "Only the Icebath gateway that owns this Group Chat can discard its work.", nil), nil
}

// Promotes the local replica once the caller confirms the previous authority can no longer commit
func groupsPromote(ctx context.Context, rid any, params map[string]any, _ *gateway.HostedRoomService, dbPath string) (map[string]any, error) {
	if params["confirm"] != true {
		return rpcErr(rid, 4118, "promotion requires confirm=true acknowledging the previous authority can no longer commit", nil), nil
	}
	result, err := gateway.PromoteReplica(ctx, dbPath, params["room_id"], paramOr(params, "reason", "authority-unreachable"), nil)
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Value of a param or its default when the key is absent
func paramOr(params map[string]any, key string, def any) any {
	if v, present := params[key]; present {
		return v
	}
	return def
}
