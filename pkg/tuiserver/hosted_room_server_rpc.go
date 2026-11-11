package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// An in process session operation the installed handler rejected
type HostedRoomSessionError struct {
	Method      string
	Code        int64
	Msg         string
	NotAdmitted bool
}

func (e *HostedRoomSessionError) Error() string { return e.Method + " failed: " + e.Msg }

// Lookup of a method name the server never registered, a KeyError
type hostedRoomMissingMethodError struct{ method string }

func (e *hostedRoomMissingMethodError) Error() string { return pyrepr.Repr(e.method) }

// Server surface the adapter drives, its installed handlers, live session records, pending approvals and profile homes
type HostedRoomServer struct {
	Method          func(name string) Handler
	SessionRecord   func(sessionID string) *tuigateway.Session
	PendingApproval func(ctx context.Context, sessionKey string) map[string]any
	ProfileHome     func(profile string) (string, error)
}

// The fully initialized server of this process
func InstalledHostedRoomServer() HostedRoomServer {
	return HostedRoomServer{Method: lookupMethod, SessionRecord: tuigateway.SessionRecordByIDOrKey,
		PendingApproval: pendingApprovalRequestPayload, ProfileHome: profileHome}
}

// Installed server handlers normalized for the hosted room runtime
type HostedRoomServerRPC struct {
	server             HostedRoomServer
	ids                atomic.Int64
	DBPath             *string
	acceptsAttachments bool
}

// Binds the hosted room service over these installed handlers into the process slot without starting it
func BindHostedRoomServer() {
	tuigateway.BindHostedRoomServer(newHostedRoomService, gateway.HostedRoomsDefaultDBPath)
}

// Hosted room service at the database whose session operations run through the installed handlers
func newHostedRoomService(ctx context.Context, dbPath string) (tuigateway.HostedRoomService, error) {
	service, err := gateway.NewHostedRoomService(ctx, gateway.HostedRoomServiceOptions{DBPath: dbPath,
		MakeRPC: func(path string) (gateway.HostedRoomRPC, error) {
			return NewHostedRoomServerRPC(InstalledHostedRoomServer(), &path), nil
		}})
	if err != nil {
		return nil, err
	}
	return service, nil
}

// Server RPC whose event bound files resolve from the room store at the path, none without a store
func NewHostedRoomServerRPC(server HostedRoomServer, dbPath *string) *HostedRoomServerRPC {
	return &HostedRoomServerRPC{server: server, DBPath: dbPath, acceptsAttachments: dbPath != nil}
}

// Whether event bound files can be resolved for a turn on this route
func (r *HostedRoomServerRPC) AcceptsAttachments() bool { return r.acceptsAttachments }

// Result of one installed handler, its error envelope raised as a session error
func (r *HostedRoomServerRPC) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	handler := r.server.Method(method)
	if handler == nil {
		return nil, &hostedRoomMissingMethodError{method}
	}
	envelope := handler(ctx, fmt.Sprintf("hosted-room-%d", r.ids.Add(1)), params)
	if errorObj, ok := hostedRPCDict(envelope["error"]); ok {
		code := any(5000)
		if pyrepr.Truthy(errorObj["code"]) {
			code = errorObj["code"]
		}
		number, err := pyint.Int64(code)
		if err != nil {
			return nil, err
		}
		message := errorObj["message"]
		if !pyrepr.Truthy(message) {
			message = "gateway rejected the request"
		}
		return nil, &HostedRoomSessionError{Method: method, Code: number, Msg: pyrepr.Str(message)}
	}
	result, ok := hostedRPCDict(envelope["result"])
	if !ok {
		return nil, &HostedRoomSessionError{Method: method, Code: 5000, Msg: "gateway returned no result"}
	}
	return result, nil
}

// Plain dict of a handler value held as a map or an ordered object
func hostedRPCDict(value any) (map[string]any, bool) {
	switch x := value.(type) {
	case map[string]any:
		return x, x != nil
	case *pyjson.Object:
		if x == nil {
			return nil, false
		}
		out := make(map[string]any, len(x.Keys))
		for _, key := range x.Keys {
			out[key] = x.Vals[key]
		}
		return out, true
	}
	return nil, false
}

// Items of a handler list value, false when it is not a list
func hostedRPCList(value any) ([]any, bool) {
	switch x := value.(type) {
	case []any:
		return x, x != nil
	case []map[string]any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = item
		}
		return out, x != nil
	case []*pyjson.Object:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = item
		}
		return out, x != nil
	}
	return nil, false
}

// Newest hidden session with exactly the title, nil when the profile holds none
func (r *HostedRoomServerRPC) ResolveExact(ctx context.Context, profile, title, source string) (map[string]any, error) {
	result, err := r.call(ctx, "session.list", map[string]any{"profile": profile, "title": title, "include_hidden": true})
	if err != nil {
		return nil, err
	}
	rows, ok := hostedRPCList(result["sessions"])
	if !ok || len(rows) == 0 {
		return nil, nil
	}
	row, ok := hostedRPCDict(rows[0])
	if !ok {
		return nil, nil
	}
	sessionID := row["resolved_id"]
	if !pyrepr.Truthy(sessionID) {
		sessionID = row["id"]
	}
	resolvedTitle := row["title"]
	if !pyrepr.Truthy(resolvedTitle) {
		resolvedTitle = title
	}
	return map[string]any{"session_id": sessionID, "title": resolvedTitle}, nil
}

// Hidden room plumbing session that follows the profile config and survives disconnects
func (r *HostedRoomServerRPC) Create(ctx context.Context, profile, title, source string) (map[string]any, error) {
	return r.call(ctx, "session.create", map[string]any{
		"profile": profile, "title": title, "source": source, "hidden": true,
		"room_plumbing": true, "follow_profile_config": true, "close_on_disconnect": false})
}

// Resumed session without its message list
func (r *HostedRoomServerRPC) Resume(ctx context.Context, profile, sessionID, source string) (map[string]any, error) {
	return r.call(ctx, "session.resume", map[string]any{
		"profile": profile, "session_id": sessionID, "omit_messages": true, "source": source})
}

// Submits one fenced member turn, its bound files staged in the member's own home under a capture lease
func (r *HostedRoomServerRPC) Submit(ctx context.Context, req gateway.HostedSubmit) (map[string]any, error) {
	if len(req.Attachments) > 0 && r.DBPath == nil {
		return nil, errors.New("attachments_unsupported_on_route: in-process route cannot carry files")
	}
	var memberID any
	if req.MemberID != nil {
		memberID = *req.MemberID
	}
	if len(req.Attachments) == 0 {
		return r.submit(ctx, req, memberID, nil)
	}
	profileDir, err := r.server.ProfileHome(req.Profile)
	if err != nil {
		return nil, err
	}
	leased, release := gateway.CaptureLease(ctx)
	defer release()
	prepared := leased
	if profileDir != "" {
		prepared = home.WithOverride(leased, profileDir)
	}
	inputs, err := gateway.PrepareBeforeSubmit(func() (map[string]any, error) {
		return gateway.BoundSubmissionPayload(prepared, *r.DBPath, req.Task.RoomID, memberID, req.Prompt, append([]any(nil), req.Attachments...), nil)
	})
	if err != nil {
		return nil, err
	}
	var images []any
	staged, _ := inputs["attachments"].([]any)
	for _, item := range staged {
		images = append(images, item.(map[string]any)["path"])
	}
	req.Prompt = inputs["text"].(string)
	return r.submit(leased, req, memberID, images)
}

// The prompt.submit call of a turn, a handler rejection marked not admitted
func (r *HostedRoomServerRPC) submit(ctx context.Context, req gateway.HostedSubmit, memberID any, images []any) (map[string]any, error) {
	params := map[string]any{"profile": req.Profile, "session_id": req.SessionID, "text": req.Prompt, "source": req.Source}
	if len(images) > 0 {
		params["_hosted_images"] = images
	}
	params["_hosted_task"] = map[string]any{
		"room_id": req.Task.RoomID, "task_id": req.Task.TaskID, "thread_id": req.Task.ThreadID,
		"turn_id": req.Task.TurnID, "execution_generation": req.ExecutionGeneration, "member_id": memberID}
	params["_hosted_terminal_callback"] = req.OnTerminal
	result, err := r.call(ctx, "prompt.submit", params)
	var rejected *HostedRoomSessionError
	if errors.As(err, &rejected) {
		rejected.NotAdmitted = true
		return nil, &gateway.HostedAttemptError{Err: rejected, NotAdmitted: true}
	}
	return result, err
}

// Message rows of the session, non dict rows dropped
func (r *HostedRoomServerRPC) History(ctx context.Context, profile, sessionID, source string) ([]map[string]any, error) {
	result, err := r.call(ctx, "session.history", map[string]any{"profile": profile, "session_id": sessionID})
	if err != nil {
		return nil, err
	}
	rows, ok := hostedRPCList(result["messages"])
	out := []map[string]any{}
	if !ok {
		return out, nil
	}
	for _, raw := range rows {
		if row, isDict := hostedRPCDict(raw); isDict {
			out = append(out, row)
		}
	}
	return out, nil
}

// Whether the live record runs, its hosted task and any approval it waits on
func (r *HostedRoomServerRPC) Info(ctx context.Context, profile, sessionID, source string) (map[string]any, error) {
	record := r.server.SessionRecord(sessionID)
	if record == nil {
		return map[string]any{"active": false, "task_id": nil}, nil
	}
	lock, ok := record.Get("history_lock").(*sync.Mutex)
	if !ok {
		return map[string]any{"active": pyrepr.Truthy(record.Get("running")), "task_id": nil}, nil
	}
	lock.Lock()
	defer lock.Unlock()
	var taskID any
	if task, isDict := hostedRPCDict(record.Get("_hosted_room_task")); isDict {
		taskID = task["task_id"]
	}
	result := map[string]any{"active": pyrepr.Truthy(record.Get("running")), "task_id": taskID}
	key := record.Get("session_key")
	if !pyrepr.Truthy(key) {
		key = ""
	}
	if r.server.PendingApproval == nil {
		return result, nil
	}
	if pending := r.server.PendingApproval(ctx, pyrepr.Str(key)); len(pending) > 0 {
		result["status"] = "waiting_for_approval"
		result["pending_approval"] = pending
	}
	return result, nil
}

// Resolves one exact local room approval without broad policy changes
func (r *HostedRoomServerRPC) Approve(ctx context.Context, sessionID, requestID, choice string) (map[string]any, error) {
	return r.call(ctx, "approval.respond", map[string]any{
		"session_id": sessionID, "request_id": requestID, "choice": choice, "all": false})
}

// Interrupts the session only while the expected hosted task is the running one
func (r *HostedRoomServerRPC) Interrupt(ctx context.Context, profile, sessionID, source, expectedTaskID string) (map[string]any, error) {
	return r.call(ctx, "session.interrupt", map[string]any{
		"profile": profile, "session_id": sessionID, "expected_hosted_task_id": expectedTaskID})
}
