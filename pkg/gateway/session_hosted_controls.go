package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Current task and binding of an exact local attempt an operator control fences on
func (c *CanonicalHostedRoomService) controlTask(roomID string, memberID, taskID, executionGeneration any) (map[string]any, HostedRoomBinding, error) {
	generation, isInt := hostedStrictInt(executionGeneration)
	member, isMember := memberID.(string)
	task, isTask := taskID.(string)
	if !isInt || generation < 1 || !isMember || member == "" || !isTask || task == "" {
		return nil, HostedRoomBinding{}, runtimeRefusal("invalid_params")
	}
	gatewayID, epoch, err := c.ownedAuthority(roomID)
	if err != nil {
		return nil, HostedRoomBinding{}, err
	}
	tasks, err := DriverListTasks(c.ctx, c.DBPath, roomID, nil)
	if err != nil {
		return nil, HostedRoomBinding{}, err
	}
	var current map[string]any
	for _, candidate := range tasks {
		if hrTaskIdentity(candidate).TaskID == task {
			current = candidate
			break
		}
	}
	if current == nil {
		return nil, HostedRoomBinding{}, runtimeRefusal("stale_generation")
	}
	if execution, _ := hostedStrictInt(current["execution_generation"]); execution != generation || TaskMemberID(current) != member {
		return nil, HostedRoomBinding{}, runtimeRefusal("stale_generation")
	}
	room, err := c.room(roomID)
	if err != nil {
		return nil, HostedRoomBinding{}, err
	}
	payload, _ := hostedMapping(current["payload"])
	seated := false
	members, _ := pyjson.Plain(room["members"]).([]any)
	for _, raw := range members {
		m, _ := hostedMapping(raw)
		if pyrepr.Equal(m["member_id"], member) && pyrepr.Equal(m["profile"], payload["target_profile"]) {
			seated = true
			break
		}
	}
	if !seated {
		return nil, HostedRoomBinding{}, runtimeRefusal("permission_denied")
	}
	peer, err := c.memberIsPeer(roomID, member)
	if err != nil {
		return nil, HostedRoomBinding{}, err
	}
	if peer {
		return nil, HostedRoomBinding{}, runtimeRefusal("unsupported_operation")
	}
	return current, HostedRoomBinding{RoomID: roomID, GatewayID: gatewayID, AuthorityEpoch: epoch}, nil
}

// Settles an indeterminate local attempt as cancelled, the canonical receipt committing before the driver write
func (c *CanonicalHostedRoomService) DiscardRoomTask(roomID string, memberID, taskID, executionGeneration any) (map[string]any, error) {
	c.policyLock.Lock()
	defer c.policyLock.Unlock()
	task, binding, err := c.controlTask(roomID, memberID, taskID, executionGeneration)
	if err != nil {
		return nil, err
	}
	generation, _ := hostedStrictInt(executionGeneration)
	cancelID := "discard:" + pyrepr.Str(generation)
	if pyrepr.Equal(task["status"], "cancelled") && pyrepr.Equal(task["cancel_id"], cancelID) {
		return task, nil
	}
	if !pyrepr.Equal(task["status"], "indeterminate") {
		return nil, runtimeRefusal("stale_generation")
	}
	transport, err := c.self.ResolveMemberTransport(binding, task)
	if err != nil {
		return nil, err
	}
	payload, _ := hostedMapping(task["payload"])
	if err := hostedDiscard(c.ctx, transport, pyrepr.Str(payload["target_profile"]), taskID.(string), generation); err != nil {
		return nil, err
	}
	lease, err := c.runtime.ensureLease(binding)
	if err != nil {
		return nil, err
	}
	result, err := c.runtime.fenced(c.runtime.opResolveCancellation(cancelID), &binding, task, lease, true, nil)
	if err != nil {
		return nil, err
	}
	c.runtime.setBlocked(roomID, false)
	c.runtime.Wakeup()
	return result, nil
}

// Retries a deferred local attempt whose session is idle, a stalled preparation through the shared service
func (c *CanonicalHostedRoomService) RetryRoomTask(roomID string, memberID, taskID, executionGeneration any) (map[string]any, error) {
	if text, ok := taskID.(string); ok && strings.HasPrefix(text, PrepareRetryTaskPrefix) {
		var member *string
		if value, isText := memberID.(string); isText {
			member = &value
		}
		var generation *int64
		if value, isInt := hostedStrictInt(executionGeneration); isInt {
			generation = &value
		}
		return c.HostedRoomService.RetryRoomTask(roomID, text, member, generation)
	}
	c.policyLock.Lock()
	defer c.policyLock.Unlock()
	task, binding, err := c.controlTask(roomID, memberID, taskID, executionGeneration)
	if err != nil {
		return nil, err
	}
	if pyrepr.Equal(task["status"], "indeterminate") {
		return nil, runtimeRefusal("unknown_execution")
	}
	if !pyrepr.Equal(task["status"], "deferred") {
		return nil, runtimeRefusal("stale_generation")
	}
	transport, err := c.self.ResolveMemberTransport(binding, task)
	if err != nil {
		return nil, err
	}
	payload, _ := hostedMapping(task["payload"])
	ref, err := hostedTransportRef(transport)
	if err != nil {
		return nil, err
	}
	info, err := transport.Info(c.ctx, pyrepr.Str(payload["target_profile"]), ref.SessionID, RoomSessionSource)
	if err != nil {
		return nil, err
	}
	if pyrepr.Equal(info["status"], "unknown") {
		return nil, runtimeRefusal("unknown_execution")
	}
	if pyrepr.Truthy(info["active"]) {
		return nil, runtimeRefusal("session_busy")
	}
	lease, err := c.runtime.ensureLease(binding)
	if err != nil {
		return nil, err
	}
	return c.runtime.requeue(c.runtime.opRequeueDeferred(), task, lease, roomID)
}

// Room status whose pending actions name the exact local attempts discard and retry fence on
func (c *CanonicalHostedRoomService) Status(roomID *string) (map[string]any, error) {
	result, err := c.HostedRoomService.Status(roomID)
	if err != nil || roomID == nil {
		return result, err
	}
	actions := []any{}
	existing, _ := result["pending_actions"].([]any)
	for _, raw := range existing {
		action, _ := hostedMapping(raw)
		taskID, has := action["task_id"]
		if !has {
			taskID = ""
		}
		if !pyrepr.Equal(action["kind"], "retry") || strings.HasPrefix(pyrepr.Str(taskID), PrepareRetryTaskPrefix) {
			actions = append(actions, raw)
		}
	}
	tasks, err := DriverListTasks(c.ctx, c.DBPath, *roomID, nil)
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		status := pyrepr.Str(task["status"])
		if status != "indeterminate" && status != "deferred" {
			continue
		}
		peer, err := c.memberIsPeer(*roomID, TaskMemberID(task))
		if err != nil {
			return nil, err
		}
		if peer {
			continue
		}
		kind := "retry"
		if status == "indeterminate" {
			kind = "discard"
		}
		actions = append(actions, AttemptAction(kind, task))
	}
	out := make(map[string]any, len(result))
	for key, value := range result {
		out[key] = value
	}
	out["pending_actions"] = actions
	return out, nil
}

// Session reference of a member producer, refused for a transport that names none
func hostedTransportRef(transport InternalSessionRPC) (SessionRef, error) {
	switch producer := transport.(type) {
	case *HostedRoomAuthorityRPC:
		return producer.Ref, nil
	case *HostedRoomOwnerRPC:
		if ref, known := producer.currentRef(); known {
			return ref, nil
		}
		return SessionRef{}, &hostedAttributeError{"'HostedRoomOwnerRPC' object has no attribute 'ref'"}
	}
	return SessionRef{}, &hostedAttributeError{"'" + pyrepr.TypeName(transport) + "' object has no attribute 'ref'"}
}

// Member producer that discards an attempt fenced on its task and hosted generation
type hostedDiscarder interface {
	Discard(ctx context.Context, profile, sessionID, source, expectedTaskID string, executionGeneration int64) (map[string]any, error)
}

var (
	_ hostedDiscarder = (*HostedRoomAuthorityRPC)(nil)
	_ hostedDiscarder = (*HostedRoomOwnerRPC)(nil)
)

// Discards the attempt through the member producer that fences on its hosted generation
func hostedDiscard(ctx context.Context, transport InternalSessionRPC, profile, taskID string, generation int64) error {
	ref, err := hostedTransportRef(transport)
	if err != nil {
		return err
	}
	discarder, ok := transport.(hostedDiscarder)
	if !ok {
		return &hostedAttributeError{"'" + pyrepr.TypeName(transport) + "' object has no attribute 'discard'"}
	}
	_, err = discarder.Discard(ctx, profile, ref.SessionID, RoomSessionSource, taskID, generation)
	return err
}

// Attribute lookup failure on a transport lacking a producer capability
type hostedAttributeError struct{ msg string }

func (e *hostedAttributeError) Error() string { return e.msg }
