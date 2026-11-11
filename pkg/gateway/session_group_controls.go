package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
	"weak"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/installidentity"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Capability each room verb requires
var groupMethods = map[string]string{
	"groups.capabilities":        "session:read",
	"groups.list":                "session:read",
	"groups.state":               "session:read",
	"groups.log":                 "session:read",
	"groups.create":              "session:control",
	"groups.rename":              "session:control",
	"groups.disband":             "session:control",
	"groups.send":                "session:submit",
	"groups.attachment.upload":   "session:submit",
	"groups.attachment.download": "session:read",
	"groups.stop":                "session:control",
	"groups.retry":               "session:control",
	"groups.discard":             "session:control",
	"groups.approve":             "session:approve",
}

// Room verbs in their declaration order
var groupMethodOrder = []string{"groups.capabilities", "groups.list", "groups.state", "groups.log", "groups.create",
	"groups.rename", "groups.disband", "groups.send", "groups.attachment.upload", "groups.attachment.download",
	"groups.stop", "groups.retry", "groups.discard", "groups.approve"}

// Params each room verb and profiles.list accept besides profile
var groupFields = map[string][]string{
	"groups.capabilities":        {},
	"groups.list":                {"limit", "offset", "include_disbanded"},
	"groups.state":               {"room_id", "include_disbanded"},
	"groups.log":                 {"room_id", "since_seq", "limit", "include_disbanded"},
	"groups.create":              {"room_id", "name", "members"},
	"groups.rename":              {"room_id", "event_id", "name"},
	"groups.disband":             {"room_id", "cancel_id"},
	"groups.send":                {"room_id", "event_id", "payload"},
	"groups.attachment.upload":   {"room_id", "upload_id", "kind", "name", "mime", "data_base64"},
	"groups.attachment.download": {"room_id", "event_id", "attachment_id"},
	"groups.stop":                {"room_id", "cancel_id"},
	"groups.retry":               {"room_id", "member_id", "task_id", "execution_generation"},
	"groups.discard":             {"room_id", "member_id", "task_id", "execution_generation"},
	"groups.approve":             {"room_id", "member_id", "task_id", "execution_generation", "choice", "request_id"},
	"profiles.list":              {"include_sessions"},
}

// Profile facts a Bots roster row shows, read from the profile directory
type RosterProfile struct {
	IsDefault   bool
	Model       any
	Provider    any
	Description string
	DisplayName string
	SkillCount  int
}

// Profile facts of one profile home, assigned in wiring from the profiles listing
var RosterProfileInfo func(ctx context.Context, name, path string, isDefault bool) (RosterProfile, error)

// One sibling bot of the roster and the principal its session fields are read as, nil for metadata only
type rosterSibling struct {
	bot    LocalBot
	reader *Principal
}

// Runs a room verb or profiles.list for the connection's own profile under that profile's runtime scope
func dispatchGroupControl(ctx context.Context, c *AuthorityConnection, method string, params *pyjson.Object) (any, error) {
	a, actor := c.authority, c.actor
	capability, ok := groupMethods[method]
	if !ok {
		capability = "session:read"
	}
	if !actor.Capabilities[capability] {
		return nil, runtimeRefusal("permission_denied")
	}
	if actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("profile_mismatch")
	}
	allowed, known := groupFields[method]
	if !known || params == nil {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, key := range params.Keys {
		if key != "profile" && !slices.Contains(allowed, key) {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	profileHome := a.ProfileID
	dbPath, err := pypath.Resolve(a.DB.Path())
	if err != nil {
		return nil, err
	}
	resolvedHome, err := pypath.Resolve(profileHome)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(dbPath) != resolvedHome {
		return nil, runtimeRefusal("profile_mismatch")
	}
	profile := params.GetOr("profile", nil)
	profileName, isText := profile.(string)
	if profile != nil && !isText {
		return nil, runtimeRefusal("invalid_params")
	}
	if profileName != "" && !profiles.ProfileMatchesHome(ctx, profileName, profileHome) {
		return nil, runtimeRefusal("profile_mismatch")
	}
	supplied := map[string]any{}
	for _, key := range params.Keys {
		if key != "profile" {
			supplied[key] = params.GetOr(key, nil)
		}
	}
	var siblings []rosterSibling
	if method == "profiles.list" {
		siblings = rosterSiblings(c, supplied)
	}
	scoped := profileRuntimeScope(ctx, profileHome, true)
	if method == "profiles.list" {
		return groupProfiles(scoped, a, actor, profileHome, supplied, siblings)
	}
	result, err := groupControl(scoped, a, actor, method, supplied, c.HostScoped())
	if err == nil {
		return result, nil
	}
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return nil, err
	}
	var hostedErr *HostedRoomError
	if errors.As(err, &hostedErr) {
		reason := "invalid_params"
		var reasoned interface{ Reason() string }
		if errors.As(err, &reasoned) && reasoned.Reason() != "" {
			reason = reasoned.Reason()
		}
		return nil, &state.RuntimeStoreError{Reason: reason, Cause: err}
	}
	if groupValueOrTypeError(err) {
		return nil, &state.RuntimeStoreError{Reason: "invalid_params", Cause: err}
	}
	return nil, err
}

// Errors of the Python ValueError and TypeError families a room verb folds into invalid_params
func groupValueOrTypeError(err error) bool {
	if errAs[*AttachmentError](err) || errAs[*HostedRoomPeerError](err) || errAs[*DiscussionPolicyError](err) ||
		errAs[*DriverStateError](err) || errAs[*pyjson.DecodeError](err) {
		return true
	}
	family := map[string]bool{"ValueError": true, "TypeError": true, "UnicodeDecodeError": true, "JSONDecodeError": true}
	var pyErr *PyError
	if errors.As(err, &pyErr) && family[pyErr.Kind] {
		return true
	}
	var intErr *pyint.Error
	if errors.As(err, &intErr) && family[intErr.Kind] {
		return true
	}
	var typed interface{ PyTypeName() string }
	return errors.As(err, &typed) && family[typed.PyTypeName()]
}

// Python int of an include_disbanded flag read as the SQL truth value
func groupIntFlag(params map[string]any) (bool, error) {
	value, ok := params["include_disbanded"]
	if !ok {
		return false, nil
	}
	n, err := pyint.Of(value)
	if err != nil {
		return false, err
	}
	return n.Sign() != 0, nil
}

// TypeError for a keyword argument the room function requires and params lack
func groupRequire(params map[string]any, keys ...string) error {
	for _, key := range keys {
		if _, ok := params[key]; !ok {
			return &PyError{Kind: "TypeError", Msg: fmt.Sprintf("missing a required keyword-only argument: '%s'", key)}
		}
	}
	return nil
}

// Room id a service method takes, the room id validation error for a value that is not text
func groupRoomID(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	return hostedRoomID(value)
}

// Value of params under key when truthy, else the fallback
func groupOr(params map[string]any, key string, fallback any) any {
	if value := params[key]; pyTruthy(value) {
		return value
	}
	return fallback
}

// Stops a room's work through the service, validating a cancel id that is not text where the store would
func groupStopRoom(ctx context.Context, service *CanonicalHostedRoomService, roomValue, cancelValue any, requireAcknowledged bool) (int, error) {
	roomID, err := groupRoomID(roomValue)
	if err != nil {
		return 0, err
	}
	cancelID, ok := cancelValue.(string)
	if !ok {
		gatewayID, epoch, err := service.ownedAuthority(roomID)
		if err != nil {
			return 0, err
		}
		_, err = RequestRoomStop(ctx, service.DBPath, roomID, cancelValue, gatewayID, epoch)
		return 0, err
	}
	return service.StopRoom(roomID, cancelID, requireAcknowledged)
}

// Room verb over the hosted store, run by the canonical service while its driver runs
func groupControl(ctx context.Context, a *SessionAuthority, actor Principal, method string, params map[string]any, hostScope bool) (any, error) {
	dbPath := a.DB.Path()
	gatewayID, err := LocalAuthorityGatewayID(ctx)
	if err != nil {
		return nil, err
	}
	hosted := a.HostedRoomService
	running := false
	authorizer, hasAuthorizer := hosted.(interface {
		AuthorizeRoom(ctx context.Context, actorSubject, roomID any, create bool, conn state.SQLiteConn) (bool, error)
	})
	if hosted != nil {
		store, ok := hosted.(interface {
			StorePath() string
			RuntimeStatus() map[string]any
		})
		if !ok {
			return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'db_path'", hosted)}
		}
		serviceDB, err := pypath.Resolve(store.StorePath())
		if err != nil {
			return nil, err
		}
		ownDB, err := pypath.Resolve(dbPath)
		if err != nil {
			return nil, err
		}
		if serviceDB != ownDB {
			return nil, runtimeRefusal("profile_mismatch")
		}
		status := store.RuntimeStatus()
		running = pyTruthy(status["running"]) && !pyTruthy(status["stopping"])
	}
	driver := func(attribute string) (*CanonicalHostedRoomService, error) {
		if service, ok := hosted.(*CanonicalHostedRoomService); ok {
			return service, nil
		}
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '%s'", hosted, attribute)}
	}
	var admit func(conn state.SQLiteConn) error
	if roomID, hasRoom := params["room_id"]; hosted != nil && hasRoom {
		if !hasAuthorizer {
			return nil, runtimeRefusal("permission_denied")
		}
		if method == "groups.create" {
			admit = func(conn state.SQLiteConn) error {
				_, err := authorizer.AuthorizeRoom(ctx, actor.Subject, roomID, true, conn)
				return err
			}
		} else if _, err := authorizer.AuthorizeRoom(ctx, actor.Subject, roomID, false, nil); err != nil {
			return nil, err
		}
	}
	switch method {
	case "groups.attachment.upload", "groups.attachment.download":
		if !running {
			return nil, runtimeRefusal("runtime_coordination_required")
		}
		service, err := driver("db_path")
		if err != nil {
			return nil, err
		}
		if method == "groups.attachment.upload" {
			return Upload(ctx, service, actor, params)
		}
		return Download(ctx, service, actor, params)
	case "groups.send", "groups.stop", "groups.retry", "groups.discard", "groups.approve":
		if !running {
			return nil, runtimeRefusal("runtime_coordination_required")
		}
		if !pyTruthy(params["room_id"]) {
			return nil, runtimeRefusal("invalid_params")
		}
		service, err := driver(map[string]string{"groups.send": "send", "groups.stop": "stop_room", "groups.retry": "retry_room_task",
			"groups.discard": "discard_room_task", "groups.approve": "approve_room_task"}[method])
		if err != nil {
			return nil, err
		}
		return groupExecutionControl(ctx, service, method, params)
	case "groups.capabilities":
		homes, refused, err := MemberRoster(ctx, a, hostScope)
		if err != nil {
			return nil, err
		}
		seatable := []any{}
		names := homes.Keys()
		slices.Sort(names)
		for _, name := range names {
			if _, gone := refused[name]; !gone {
				seatable = append(seatable, name)
			}
		}
		methods := make([]any, len(groupMethodOrder))
		for i, name := range groupMethodOrder {
			methods[i] = name
		}
		return pyjson.ObjectOf("protocol_version", HostedProtocolVersion, "driver", running,
			"persistent_process", true, "authority_gateway_id", gatewayID,
			"room_link", pyjson.ObjectOf("enabled", false, "reason", "canonical_driver_required"),
			"features", []any{"room_identity", "monotonic_log", "replayable_disband"}, "methods", methods,
			"max_log_limit", MaxRoomLogLimit, "seatable_profiles", seatable), nil
	case "groups.list":
		return groupListing(ctx, a, actor, dbPath, params, hosted != nil, authorizer, hasAuthorizer)
	case "groups.create":
		var service *CanonicalHostedRoomService
		if running {
			if service, err = driver("create_room"); err != nil {
				return nil, err
			}
		}
		return groupCreate(ctx, a, service, dbPath, gatewayID, params, admit, hostScope)
	case "groups.state":
		if err := groupRequire(params, "room_id"); err != nil {
			return nil, err
		}
		include, err := groupIntFlag(params)
		if err != nil {
			return nil, err
		}
		room, err := RoomState(ctx, dbPath, params["room_id"], include)
		if err != nil {
			return nil, err
		}
		result := pyjson.ObjectOf("room", room)
		if running && room["disbanded_at"] == nil {
			service, err := driver("status")
			if err != nil {
				return nil, err
			}
			roomID, _ := room["room_id"].(string)
			status, err := service.Status(&roomID)
			if err != nil {
				return nil, err
			}
			result.Set("driver_status", status)
		}
		return result, nil
	case "groups.log":
		if err := groupRequire(params, "room_id"); err != nil {
			return nil, err
		}
		include, err := groupIntFlag(params)
		if err != nil {
			return nil, err
		}
		sinceSeq, ok := params["since_seq"]
		if !ok {
			sinceSeq = int64(0)
		}
		limit, ok := params["limit"]
		if !ok {
			limit = int64(100)
		}
		return ReadEvents(ctx, dbPath, params["room_id"], sinceSeq, limit, include)
	case "groups.rename":
		if err := groupRequire(params, "room_id", "event_id", "name"); err != nil {
			return nil, err
		}
		room, err := RenameRoom(ctx, dbPath, params["room_id"], params["event_id"], params["name"], nil)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("room", room), nil
	case "groups.disband":
		return groupDisband(ctx, running, driver, dbPath, gatewayID, params)
	}
	return nil, &pyKeyError{key: method}
}

// One page of rooms, paged over the rooms the actor may see when the hosted service authorizes them
func groupListing(ctx context.Context, a *SessionAuthority, actor Principal, dbPath string, params map[string]any, hosted bool,
	authorizer interface {
		AuthorizeRoom(ctx context.Context, actorSubject, roomID any, create bool, conn state.SQLiteConn) (bool, error)
	}, hasAuthorizer bool) (any, error) {
	limitValue, ok := params["limit"]
	if !ok {
		limitValue = int64(MaxRoomListLimit)
	}
	offsetValue, ok := params["offset"]
	if !ok {
		offsetValue = int64(0)
	}
	include, err := groupIntFlag(params)
	if err != nil {
		return nil, err
	}
	if !hosted {
		result, err := ListRooms(ctx, dbPath, include, limitValue, offsetValue)
		if err != nil {
			return nil, err
		}
		rooms := make([]any, len(result))
		for i, room := range result {
			rooms[i] = room
		}
		var next any
		limit, _ := pyint.Instance(limitValue)
		offset, _ := pyint.Instance(offsetValue)
		if limit != nil && int64(len(result)) == limit.Int64() {
			next = pyint.Native(new(big.Int).Add(offset, limit))
		}
		return pyjson.ObjectOf("rooms", rooms, "next_offset", next), nil
	}
	if !hasAuthorizer {
		return nil, runtimeRefusal("permission_denied")
	}
	visible := func(room map[string]any) (bool, error) {
		_, err := authorizer.AuthorizeRoom(ctx, actor.Subject, room["room_id"], false, nil)
		if err == nil {
			return true, nil
		}
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) && refusal.Reason == "permission_denied" {
			return false, nil
		}
		return false, err
	}
	found := []any{}
	var cursorValue any = offsetValue
	var next any
	for next == nil {
		batch, err := ListRooms(ctx, dbPath, include, limitValue, cursorValue)
		if err != nil {
			return nil, err
		}
		limit, _ := pyint.Instance(limitValue)
		cursor, _ := pyint.Instance(cursorValue)
		cursor = new(big.Int).Set(cursor)
		for _, room := range batch {
			shown, err := visible(room)
			if err != nil {
				return nil, err
			}
			if shown {
				if int64(len(found)) == limit.Int64() {
					next = pyint.Native(new(big.Int).Set(cursor))
					break
				}
				found = append(found, room)
			}
			cursor.Add(cursor, big.NewInt(1))
		}
		cursorValue = pyint.Native(cursor)
		if int64(len(batch)) < limit.Int64() {
			break
		}
	}
	return pyjson.ObjectOf("rooms", found, "next_offset", next), nil
}

// Creates a room after the roster check, through the service while its driver runs
func groupCreate(ctx context.Context, a *SessionAuthority, service *CanonicalHostedRoomService, dbPath, gatewayID string,
	params map[string]any, admit func(conn state.SQLiteConn) error, hostScope bool) (any, error) {
	homes, refused, err := MemberRoster(ctx, a, hostScope)
	if err != nil {
		return nil, err
	}
	if err := RefuseUnseatable(params["members"], refused); err != nil {
		return nil, err
	}
	membersRefusal := func(err error) error {
		var invalid *DiscussionValidationError
		if errors.As(err, &invalid) {
			field, detail := "members", err.Error()
			refusal := state.NewRuntimeStoreError("invalid_params", &field, &detail)
			refusal.Cause = err
			return refusal
		}
		return err
	}
	members, err := ValidateRoster(params["members"], homes.Keys())
	if err != nil {
		return nil, membersRefusal(err)
	}
	if service != nil {
		if err := groupRequire(params, "room_id", "name", "members"); err != nil {
			return nil, err
		}
		roomID, err := groupRoomID(params["room_id"])
		if err != nil {
			return nil, err
		}
		name, ok := params["name"].(string)
		if !ok {
			if _, err := hostedRoomName(params["name"]); err != nil {
				return nil, err
			}
		}
		room, err := service.CreateRoom(roomID, name, params["members"], admit)
		if err != nil {
			return nil, membersRefusal(err)
		}
		return pyjson.ObjectOf("room", room), nil
	}
	normalized := make([]any, len(members))
	for i, m := range members {
		target := map[string]any{}
		for k, v := range m.Target {
			target[k] = v
		}
		row := pyjson.ObjectOf("member_id", m.MemberID, "profile", m.Profile, "handle", m.Handle, "target", target)
		if m.DisplayName != "" {
			row.Set("display_name", m.DisplayName)
		}
		normalized[i] = row
	}
	room, err := CreateRoom(ctx, dbPath, CreateRoomInput{RoomID: params["room_id"], Name: params["name"], Members: normalized,
		AuthorityGatewayID: gatewayID, Admit: admit})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("room", room), nil
}

// Tombstones a room after stopping its work and revoking its routes, refusing active work without a driver
func groupDisband(ctx context.Context, running bool, driver func(attribute string) (*CanonicalHostedRoomService, error),
	dbPath, gatewayID string, params map[string]any) (any, error) {
	roomValue := params["room_id"]
	current, err := RoomState(ctx, dbPath, roomValue, true)
	if err != nil {
		return nil, err
	}
	if running && current["disbanded_at"] == nil {
		service, err := driver("stop_room")
		if err != nil {
			return nil, err
		}
		if _, err := groupStopRoom(ctx, service, roomValue, groupOr(params, "cancel_id", "room-disbanded"), true); err != nil {
			return nil, err
		}
		roomID, err := groupRoomID(roomValue)
		if err != nil {
			return nil, err
		}
		if _, err := service.RevokeRoomRoutes(roomID); err != nil {
			return nil, err
		}
	}
	if !running {
		for _, status := range []string{"queued", "running", "stopping", "indeterminate", "deferred"} {
			tasks, err := DriverListTasks(ctx, dbPath, roomValue, &status)
			if err != nil {
				return nil, err
			}
			if len(tasks) > 0 {
				return nil, runtimeRefusal("runtime_coordination_required")
			}
		}
	}
	current, err = RoomState(ctx, dbPath, roomValue, true)
	if err != nil {
		return nil, err
	}
	tombstone, err := DisbandRoom(ctx, dbPath, roomValue, gatewayID, current["authority_epoch"], nil)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("tombstone", tombstone), nil
}

// Send, stop, retry, discard and approve of a room through the canonical service
func groupExecutionControl(ctx context.Context, service *CanonicalHostedRoomService, method string, params map[string]any) (any, error) {
	generation := func() (int64, bool) {
		value := params["execution_generation"]
		if _, isBool := value.(bool); isBool {
			return 0, false
		}
		n, ok := pyint.Instance(value)
		if !ok || n.Sign() < 1 {
			return 0, false
		}
		if !n.IsInt64() {
			return -1, true
		}
		return n.Int64(), true
	}
	switch method {
	case "groups.send":
		eventID, err := UserEventID(params["event_id"])
		if err != nil {
			return nil, err
		}
		roomID, isText := params["room_id"].(string)
		if !isText {
			if _, err := ValidateUserPayload(params["payload"]); err != nil {
				return nil, err
			}
			_, err := hostedRoomID(params["room_id"])
			return nil, err
		}
		event, err := service.Send(roomID, eventID, params["payload"])
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("event", event, "client_event_id", params["event_id"], "accepted", true, "driver_started", true), nil
	case "groups.stop":
		cancelled, err := groupStopRoom(ctx, service, params["room_id"], groupOr(params, "cancel_id", "desktop-stop"), false)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("cancelled", cancelled), nil
	case "groups.retry", "groups.discard":
		if _, ok := generation(); !ok || !pyTruthy(params["member_id"]) || !pyTruthy(params["task_id"]) {
			return nil, runtimeRefusal("invalid_params")
		}
		roomID, err := groupRoomID(params["room_id"])
		if err != nil {
			return nil, err
		}
		var task map[string]any
		if method == "groups.discard" {
			task, err = service.DiscardRoomTask(roomID, params["member_id"], params["task_id"], params["execution_generation"])
		} else {
			task, err = service.RetryRoomTask(roomID, params["member_id"], params["task_id"], params["execution_generation"])
		}
		if err != nil {
			return nil, err
		}
		identity := hrTaskIdentity(task)
		receipt := pyjson.ObjectOf("room_id", identity.RoomID, "task_id", identity.TaskID, "thread_id", identity.ThreadID,
			"turn_id", identity.TurnID, "status", task["status"], "execution_generation", task["execution_generation"],
			"cancel_generation", task["cancel_generation"])
		verb := "retried"
		if method == "groups.discard" {
			verb = "discarded"
		}
		return pyjson.ObjectOf(verb, true, "task", receipt), nil
	case "groups.approve":
		executionGeneration, ok := generation()
		choice := params["choice"]
		requestID, isText := params["request_id"].(string)
		if !ok || (choice != "once" && choice != "deny") || !isText || requestID == "" {
			return nil, runtimeRefusal("invalid_params")
		}
		roomID, roomText := params["room_id"].(string)
		memberID, memberText := params["member_id"].(string)
		taskID, taskText := params["task_id"].(string)
		if !roomText || !memberText || !taskText || executionGeneration < 0 {
			return nil, errors.New("room approval is no longer pending")
		}
		result, err := service.ApproveRoomTask(roomID, memberID, taskID, executionGeneration, choice.(string), &requestID)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("approved", true, "result", result), nil
	}
	return nil, &pyKeyError{key: method}
}

// Other local bots this process serves with the principal a host scoped socket reads their sessions as
func rosterSiblings(c *AuthorityConnection, params map[string]any) []rosterSibling {
	a, actor := c.authority, c.actor
	if registryOf(a.Runner) == nil {
		return []rosterSibling{}
	}
	include, present := params["include_sessions"]
	readable := c.HostScoped() && (!present || include == true)
	siblings := []rosterSibling{}
	for _, bot := range LocalBots(a.Runner) {
		if bot.Authority == a {
			continue
		}
		sibling := rosterSibling{bot: bot}
		if readable && bot.Problem == nil {
			sibling.reader = &Principal{Subject: actor.Subject, ProfileID: bot.Authority.ProfileID,
				Capabilities: actor.Capabilities, TransportID: actor.TransportID}
		}
		siblings = append(siblings, sibling)
	}
	return siblings
}

// Comparable reader key of a roster session memo
type rosterReader struct {
	subject  string
	operator bool
}

// Session fields of a roster row read from the authority's own store as actor, reused while the store has not moved
func botSessions(ctx context.Context, a *SessionAuthority, actor Principal) (map[string]any, error) {
	operator := actor.Capabilities["session:operator"]
	return tuigateway.CachedReaderSessionFields(filepath.Dir(a.DB.Path()), rosterReader{actor.Subject, operator},
		func() (map[string]any, error) { return readBotSessions(ctx, a.DB, actor.Subject, operator) })
}

// SQL predicate of a local chat, minted here or adopted under a legacy binding
const groupLocalChat = "(chat_id LIKE 'local-%' OR EXISTS (SELECT 1 FROM state_meta WHERE key = ? || sessions.chat_id))"

// Value when truthy, else the fallback
func groupTruthyOr(value, fallback any) any {
	if pyTruthy(value) {
		return value
	}
	return fallback
}

// Bot Chat, newest visible local chat and freshest worker of one store, each only when the reader may see it
func readBotSessions(ctx context.Context, db *state.SessionDB, subject string, operator bool) (map[string]any, error) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(InternalListingSources)), ",")
	ownerFilter, ownerParams := "user_id=? AND ", []any{subject}
	if operator {
		ownerFilter, ownerParams = "", nil
	}
	sources := make([]any, len(InternalListingSources))
	for i, source := range InternalListingSources {
		sources[i] = source
	}
	fields := map[string]any{"last_session": nil, "worker_session": nil, "canonical_session": nil}
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		summary := func(root *pyjson.Object) (*pyjson.Object, error) {
			rootID := pyStrOf(root.GetOr("id", nil))
			tip, err := state.CompressionTipOn(ctx, conn, rootID)
			if err != nil {
				return nil, err
			}
			target, err := state.QueryRowObject(ctx, conn, "SELECT * FROM sessions WHERE id=?", tip)
			if err != nil {
				return nil, err
			}
			if target == nil {
				target = root
			}
			message, err := state.QueryRowObject(ctx, conn, "SELECT content FROM messages WHERE session_id=? AND active=1 "+
				"AND role IN ('user','assistant') AND TRIM(COALESCE(content,''))!='' ORDER BY id DESC LIMIT 1", tip)
			if err != nil {
				return nil, err
			}
			text := ""
			if message != nil {
				text = strings.Join(pystr.SplitWhitespace(pyStrOf(groupTruthyOr(message.GetOr("content", nil), "")), -1), " ")
			}
			preview := text
			if runes := []rune(text); len(runes) > 80 {
				preview = string(runes[:80]) + "..."
			}
			return pyjson.ObjectOf("id", root.GetOr("id", nil), "resolved_id", tip,
				"title", groupTruthyOr(target.GetOr("title", nil), ""), "root_title", groupTruthyOr(root.GetOr("title", nil), ""),
				"preview", preview, "started_at", groupTruthyOr(target.GetOr("started_at", nil), int64(0)),
				"last_active", groupTruthyOr(target.GetOr("last_activity_at", nil), groupTruthyOr(target.GetOr("started_at", nil), int64(0))),
				"message_count", groupTruthyOr(target.GetOr("message_count", nil), int64(0))), nil
		}
		canonical, err := state.QueryRowObject(ctx, conn, "SELECT * FROM sessions WHERE title=? AND "+groupLocalChat,
			"Bot Chat", state.LocalLegacyPrefix)
		if err != nil {
			return err
		}
		if canonical != nil && (SameOwner(canonical.GetOr("user_id", nil), subject) || operator) && !pyTruthy(canonical.GetOr("archived", nil)) {
			if fields["canonical_session"], err = summary(canonical); err != nil {
				return err
			}
		}
		args := append(append(append([]any{}, ownerParams...), state.LocalLegacyPrefix), sources...)
		latest, err := state.QueryRowObject(ctx, conn, "SELECT * FROM sessions WHERE "+ownerFilter+groupLocalChat+
			" AND archived=0 AND hidden=0 AND COALESCE(source,'') NOT IN ("+marks+") "+
			"ORDER BY COALESCE(last_activity_at,started_at) DESC LIMIT 1", args...)
		if err != nil {
			return err
		}
		canonicalSummary, _ := fields["canonical_session"].(*pyjson.Object)
		switch {
		case latest != nil && canonicalSummary != nil && pyjson.Equal(latest.GetOr("id", nil), canonical.GetOr("id", nil)):
			fields["last_session"] = canonicalSummary.Copy()
		case latest != nil:
			if fields["last_session"], err = summary(latest); err != nil {
				return err
			}
		}
		worker, err := state.QueryRowObject(ctx, conn, "SELECT id, source, title, COALESCE(last_activity_at, started_at) AS last_active FROM sessions "+
			"WHERE COALESCE(source,'') IN ("+marks+") AND archived=0 AND hidden=0 ORDER BY last_active DESC LIMIT 1", sources...)
		if err != nil {
			return err
		}
		if worker != nil {
			fields["worker_session"] = pyjson.ObjectOf("id", worker.GetOr("id", nil),
				"source", strings.ToLower(pystr.Strip(pyStrOf(groupTruthyOr(worker.GetOr("source", nil), "")))),
				"title", groupTruthyOr(worker.GetOr("title", nil), ""), "last_active", groupTruthyOr(worker.GetOr("last_active", nil), int64(0)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return fields, nil
}

// Sibling authorities whose last roster read failed, held weakly so a dropped authority leaves the set
var groupDegraded = struct {
	mu  sync.Mutex
	set map[weak.Pointer[SessionAuthority]]struct{}
}{set: map[weak.Pointer[SessionAuthority]]struct{}{}}

// Marks or clears an authority as degraded, reporting whether it already was
func groupMarkDegraded(a *SessionAuthority, degraded bool) bool {
	key := weak.Make(a)
	groupDegraded.mu.Lock()
	defer groupDegraded.mu.Unlock()
	_, was := groupDegraded.set[key]
	if !degraded {
		delete(groupDegraded.set, key)
		return was
	}
	if !was {
		groupDegraded.set[key] = struct{}{}
		runtime.AddCleanup(a, func(k weak.Pointer[SessionAuthority]) {
			groupDegraded.mu.Lock()
			delete(groupDegraded.set, k)
			groupDegraded.mu.Unlock()
		}, key)
	}
	return was
}

// Session fields of a sibling row, empty for a retiring sibling or one whose store did not answer
func siblingSessions(ctx context.Context, a *SessionAuthority, reader Principal) map[string]any {
	a.mu.Lock()
	gone := a.Retiring || a.Retired
	a.mu.Unlock()
	if gone {
		return map[string]any{}
	}
	fields, err := botSessions(ctx, a, reader)
	if err != nil {
		message := fmt.Sprintf("Bots roster: %s is metadata-only, its session store did not answer (%s)", a.ProfileID, err)
		if groupMarkDegraded(a, true) {
			slog.Debug(message)
		} else {
			slog.Warn(message)
		}
		return map[string]any{}
	}
	groupMarkDegraded(a, false)
	return fields
}

// Session field keys in the order a roster row carries them
var rosterSessionKeys = []string{"last_session", "worker_session", "canonical_session"}

// Copies session fields onto a roster row in their row order
func rosterUpdate(row *pyjson.Object, fields map[string]any) {
	for _, key := range rosterSessionKeys {
		if value, ok := fields[key]; ok {
			row.Set(key, value)
		}
	}
}

// Whether path names a regular file, symlinks followed
func groupIsFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// Whether path names a directory, symlinks followed
func groupIsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// The Bots roster, this profile's row then every profile it can name, with the install id
func groupProfiles(ctx context.Context, a *SessionAuthority, actor Principal, profileHome string, params map[string]any,
	siblings []rosterSibling) (any, error) {
	includeSessions := true
	if value, ok := params["include_sessions"]; ok {
		flag, isBool := value.(bool)
		if !isBool {
			return nil, runtimeRefusal("invalid_params")
		}
		includeSessions = flag
	}
	name := servedProfileName(profileHome)
	info, err := RosterProfileInfo(ctx, name, profileHome, name == "default")
	if err != nil {
		return nil, err
	}
	row := pyjson.ObjectOf("name", name, "path", profileHome, "is_default", info.IsDefault, "model", info.Model,
		"provider", info.Provider, "description", info.Description, "display_name", info.DisplayName, "skill_count", info.SkillCount)
	meta := map[string]any{}
	if metaPath := filepath.Join(profileHome, "profile.yaml"); groupIsFile(metaPath) {
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(raw) {
			return nil, &PyError{Kind: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode " + metaPath}
		}
		loaded, err := config.SafeLoad(string(raw))
		if err != nil {
			return nil, err
		}
		if mapping, ok := loaded.(map[string]any); ok {
			meta = mapping
		}
	}
	revisions := pyjson.NewObject()
	if mapping, ok := meta["_ui_meta_revisions"].(map[string]any); ok {
		for _, key := range config.MappingKeys(mapping) {
			value := mapping[key]
			if _, isBool := value.(bool); isBool {
				continue
			}
			n, ok := pyint.Instance(value)
			if !ok {
				continue
			}
			if n.Sign() < 0 {
				revisions.Set(key, int64(0))
			} else {
				revisions.Set(key, value)
			}
		}
	}
	row.Set("ui_meta_revisions", revisions)
	if uiMeta, ok := meta["ui_meta"].(map[string]any); ok {
		row.Set("ui_meta", uiMeta)
	}
	hasAvatar := false
	for _, ext := range []string{"png", "jpg", "webp"} {
		if groupIsFile(filepath.Join(profileHome, "assets", "avatar."+ext)) {
			hasAvatar = true
			break
		}
	}
	row.Set("has_avatar", hasAvatar)
	if includeSessions {
		fields, err := botSessions(ctx, a, actor)
		if err != nil {
			return nil, err
		}
		rosterUpdate(row, fields)
	}
	rows := []any{row}
	ownResolved, err := pypath.Resolve(profileHome)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{ownResolved: true}
	discovered := func(configured, target string, reachAuthority *SessionAuthority, reader *Principal) error {
		resolved, err := pypath.Resolve(target)
		if err != nil {
			return err
		}
		if seen[resolved] {
			return nil
		}
		seen[resolved] = true
		entry := pyjson.ObjectOf("name", configured, "path", target, "is_default", configured == "default", "model", "",
			"provider", "", "description", "", "display_name", configured, "skill_count", 0)
		if includeSessions && reachAuthority != nil {
			rosterUpdate(entry, siblingSessions(ctx, reachAuthority, *reader))
		}
		if meta, ok := discoveredProfileMeta(configured, filepath.Join(target, "profile.yaml")).(map[string]any); ok {
			entry.Set("display_name", pyStrOf(groupTruthyOr(meta["display_name"], configured)))
			if uiMeta, ok := meta["ui_meta"].(map[string]any); ok {
				entry.Set("ui_meta", uiMeta)
			}
		}
		rows = append(rows, entry)
		return nil
	}
	for _, sibling := range siblings {
		var reach *SessionAuthority
		if sibling.reader != nil {
			reach = sibling.bot.Authority
		}
		if err := discovered(servedProfileName(sibling.bot.Home), sibling.bot.Home, reach, sibling.reader); err != nil {
			return nil, err
		}
	}
	if a.HostedRoomService != nil {
		service, ok := a.HostedRoomService.(*CanonicalHostedRoomService)
		if !ok {
			return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'profile_homes'", a.HostedRoomService)}
		}
		homes, names, err := service.ProfileHomes()
		if err != nil {
			return nil, err
		}
		for _, configured := range names {
			if err := discovered(configured, homes[configured], nil, nil); err != nil {
				return nil, err
			}
		}
	}
	if root := profiles.DefaultHome(); groupIsDir(root) {
		if err := discovered("default", root, nil, nil); err != nil {
			return nil, err
		}
	}
	for _, target := range namedProfileDirs() {
		if err := discovered(filepath.Base(target), target, nil, nil); err != nil {
			return nil, err
		}
	}
	installID, err := installidentity.GetInstallID(ctx, nil)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("profiles", rows, "bot_mode_protocol", true, "install_id", installID), nil
}

// Cached named profile listing keyed on the profiles root and every entry's identity markers and tombstone
var namedDirsCache = struct {
	mu      sync.Mutex
	entries map[string]namedDirsEntry
}{entries: map[string]namedDirsEntry{}}

type namedDirsEntry struct {
	signature string
	targets   []string
}

// Mode, mtime, ctime, size and inode of a path not following a final link, empty when it cannot be stat'ed
func groupLstatKey(path string) string {
	fi, err := os.Lstat(path)
	if err != nil {
		return "None"
	}
	return fmt.Sprintf("%d:%v", fi.Mode(), config.FileSignature(fi))
}

// Named profile directories, relisted only when an entry's identity markers, tombstone or stat moved
func namedProfileDirs() []string {
	root := profiles.ProfilesRoot()
	signature, ok := namedDirsSignature(root)
	namedDirsCache.mu.Lock()
	cached, hit := namedDirsCache.entries[root]
	namedDirsCache.mu.Unlock()
	if ok && hit && cached.signature == signature {
		return cached.targets
	}
	targets, err := profiles.IterNamedProfileDirs(true)
	if err != nil {
		slog.Warn(fmt.Sprintf("profiles.list: cannot list %s: %s", root, err))
		return []string{}
	}
	if ok {
		namedDirsCache.mu.Lock()
		namedDirsCache.entries[root] = namedDirsEntry{signature: signature, targets: targets}
		namedDirsCache.mu.Unlock()
	}
	return targets
}

// Change signature of the profiles root and its entries, false when any stat fails
func namedDirsSignature(root string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	markers := home.ProfileIdentityMarkers()
	rows := make([]string, 0, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(root, entry.Name())
		parts := make([]string, len(markers))
		for i, marker := range markers {
			parts[i] = groupLstatKey(filepath.Join(entryPath, marker))
		}
		_, tombErr := os.Stat(home.TombstonePath(entryPath))
		fi, err := os.Stat(entryPath)
		if err != nil {
			return "", false
		}
		rows = append(rows, fmt.Sprintf("%q|%v|%s|%t", entry.Name(), config.FileSignature(fi), strings.Join(parts, ","), tombErr == nil))
	}
	slices.Sort(rows)
	fi, err := os.Stat(root)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%v/%s", config.FileSignature(fi), strings.Join(rows, "\n")), true
}

// Parsed profile.yaml memo keyed on the file's bytes
var profileMetaCache = struct {
	mu      sync.Mutex
	entries map[string]profileMetaEntry
}{entries: map[string]profileMetaEntry{}}

type profileMetaEntry struct {
	raw  string
	meta any
}

// A sibling's parsed profile.yaml, an empty mapping when it is absent or unreadable
func discoveredProfileMeta(name, metaPath string) any {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR) {
			return map[string]any{}
		}
		slog.Warn(fmt.Sprintf("profiles.list: skipping unreadable profile.yaml for %s (%s): %s", name, metaPath, err))
		return map[string]any{}
	}
	profileMetaCache.mu.Lock()
	cached, hit := profileMetaCache.entries[metaPath]
	profileMetaCache.mu.Unlock()
	if hit && cached.raw == string(raw) {
		return cached.meta
	}
	var meta any
	if !utf8.Valid(raw) {
		slog.Warn(fmt.Sprintf("profiles.list: skipping unreadable profile.yaml for %s (%s): %s", name, metaPath, "'utf-8' codec can't decode"))
		meta = map[string]any{}
	} else if meta, err = config.SafeLoad(string(raw)); err != nil {
		slog.Warn(fmt.Sprintf("profiles.list: skipping unreadable profile.yaml for %s (%s): %s", name, metaPath, err))
		meta = map[string]any{}
	}
	profileMetaCache.mu.Lock()
	profileMetaCache.entries[metaPath] = profileMetaEntry{raw: string(raw), meta: meta}
	profileMetaCache.mu.Unlock()
	return meta
}
