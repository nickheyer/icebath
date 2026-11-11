package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Coordinates of one scoped prepare call on a target gateway's room member API
type PeerPrepare struct {
	RoomID            string
	Profile           string
	Source            string
	Grant             string
	Create            bool
	ExpectedSessionID *string
}

// Room scope bound into a peer client before any call
type PeerRoomScope struct {
	RoomID             string
	HomeInstallID      string
	AuthorityGatewayID string
	AuthorityEpoch     int64
	MemberID           string
	TargetInstallID    string
	TargetProfile      string
}

// Authenticated client of a target gateway's narrow room member API
type HostedRoomPeerClient interface {
	Prepare(ctx context.Context, req PeerPrepare) (map[string]any, error)
	Dispatch(ctx context.Context, dispatch *pyjson.Object, grant string) (map[string]any, error)
	History(ctx context.Context, roomID, profile, sessionID, grant string) ([]map[string]any, error)
	Status(ctx context.Context, roomID, profile, sessionID, grant string) (map[string]any, error)
	Stop(ctx context.Context, dispatch *pyjson.Object, grant string) (map[string]any, error)
}

// Peer client that binds its room scope before calls
type peerRoomScopeBinder interface {
	BindRoomScope(scope PeerRoomScope) error
}

// Peer client that stops an attempt from its durable receipt alone
type peerStopReceipter interface {
	StopReceipt(ctx context.Context, taskID string, executionGeneration int64, grant string) (map[string]any, error)
}

// Peer failure classified for failover, retryable on another link unless ambiguous
type peerFailoverError interface {
	PeerRetryable() bool
	PeerAmbiguous() bool
}

// One address or provider of the same authenticated target gateway
type RoomLinkCandidate struct {
	Name            string
	Mode            string
	TargetInstallID string
	Client          HostedRoomPeerClient
}

// Tries alternate links without changing target or logical task identity
type FailoverHostedRoomPeerClient struct {
	Candidates             []RoomLinkCandidate
	ReprobeIntervalSeconds float64
	clock                  func() float64
	active                 int
	lastPrimaryProbe       float64
}

// Failover client over candidates of one installation, monotonic clock when nil
func NewFailoverHostedRoomPeerClient(candidates []RoomLinkCandidate, reprobeIntervalSeconds float64, clock func() float64) (*FailoverHostedRoomPeerClient, error) {
	if len(candidates) == 0 {
		return nil, &RoomValueError{Msg: "at least one RoomLink candidate is required"}
	}
	installs := map[string]bool{}
	for _, c := range candidates {
		installs[c.TargetInstallID] = true
	}
	if len(installs) != 1 {
		return nil, &RoomValueError{Msg: "RoomLink candidates must target one installation"}
	}
	if reprobeIntervalSeconds <= 0 {
		return nil, &RoomValueError{Msg: "reprobe_interval_seconds must be positive"}
	}
	if clock == nil {
		clock = pythread.Monotonic
	}
	return &FailoverHostedRoomPeerClient{Candidates: append([]RoomLinkCandidate(nil), candidates...), ReprobeIntervalSeconds: reprobeIntervalSeconds, clock: clock}, nil
}

// Link calls go to first
func (f *FailoverHostedRoomPeerClient) ActiveLink() RoomLinkCandidate { return f.Candidates[f.active] }

// Tries the active link, the primary after its cooldown, then the rest, ambiguous or final failures propagating
func failoverCall[T any](f *FailoverHostedRoomPeerClient, call func(HostedRoomPeerClient) (T, error)) (T, error) {
	now := f.clock()
	order := []int{f.active}
	if f.active != 0 && now-f.lastPrimaryProbe >= f.ReprobeIntervalSeconds {
		f.lastPrimaryProbe = now
		order = []int{0, f.active}
	}
	for index := range f.Candidates {
		seen := false
		for _, o := range order {
			seen = seen || o == index
		}
		if !seen {
			order = append(order, index)
		}
	}
	var lastErr error
	var zero T
	for _, index := range order {
		result, err := call(f.Candidates[index].Client)
		if err != nil {
			var classified peerFailoverError
			if !errors.As(err, &classified) || classified.PeerAmbiguous() || !classified.PeerRetryable() {
				return zero, err
			}
			lastErr = err
			continue
		}
		f.active = index
		return result, nil
	}
	if lastErr != nil {
		return zero, lastErr
	}
	return zero, errors.New("no RoomLink candidate was attempted")
}

func (f *FailoverHostedRoomPeerClient) Prepare(ctx context.Context, req PeerPrepare) (map[string]any, error) {
	return failoverCall(f, func(c HostedRoomPeerClient) (map[string]any, error) { return c.Prepare(ctx, req) })
}

func (f *FailoverHostedRoomPeerClient) Dispatch(ctx context.Context, dispatch *pyjson.Object, grant string) (map[string]any, error) {
	return failoverCall(f, func(c HostedRoomPeerClient) (map[string]any, error) { return c.Dispatch(ctx, dispatch, grant) })
}

func (f *FailoverHostedRoomPeerClient) History(ctx context.Context, roomID, profile, sessionID, grant string) ([]map[string]any, error) {
	return failoverCall(f, func(c HostedRoomPeerClient) ([]map[string]any, error) {
		return c.History(ctx, roomID, profile, sessionID, grant)
	})
}

func (f *FailoverHostedRoomPeerClient) Status(ctx context.Context, roomID, profile, sessionID, grant string) (map[string]any, error) {
	return failoverCall(f, func(c HostedRoomPeerClient) (map[string]any, error) {
		return c.Status(ctx, roomID, profile, sessionID, grant)
	})
}

func (f *FailoverHostedRoomPeerClient) Stop(ctx context.Context, dispatch *pyjson.Object, grant string) (map[string]any, error) {
	return failoverCall(f, func(c HostedRoomPeerClient) (map[string]any, error) { return c.Stop(ctx, dispatch, grant) })
}

// Binds the room scope into every candidate that takes one
func (f *FailoverHostedRoomPeerClient) BindRoomScope(scope PeerRoomScope) error {
	for _, c := range f.Candidates {
		if binder, ok := peerHook[peerRoomScopeBinder](c.Client); ok {
			if err := binder.BindRoomScope(scope); err != nil {
				return err
			}
		}
	}
	return nil
}

// Secret free target coordinates plus a separately stored room grant
type PeerMemberRoute struct {
	HomeInstallID         string
	MemberID              string
	TargetInstallID       string
	TargetProfile         string
	CapabilityDigest      string
	CancellationScopeID   string
	TraceID               string
	Grant                 string
	ExecutionPolicyDigest string
}

// Fully fenced member dispatch shared by submit and recovery
func BuildMemberDispatch(binding HostedRoomBinding, route PeerMemberRoute, roomID, taskID, targetProfile string, executionGeneration, sourceEventSeq int64, prompt, traceID string) (*HostedMemberDispatch, error) {
	digest := sha256.Sum256([]byte(prompt))
	return HostedMemberDispatchFromMapping(map[string]any{
		"protocol_version": int64(HostedProtocolVersion), "room_id": roomID, "home_install_id": route.HomeInstallID,
		"authority_gateway_id": binding.GatewayID, "authority_epoch": binding.AuthorityEpoch, "member_id": route.MemberID,
		"target_install_id": route.TargetInstallID, "target_profile": targetProfile, "task_id": taskID,
		"execution_generation": executionGeneration, "source_event_seq": sourceEventSeq,
		"cancellation_scope_id": route.CancellationScopeID, "prompt": prompt, "prompt_digest": hex.EncodeToString(digest[:]),
		"capability_digest": route.CapabilityDigest, "execution_policy_digest": route.ExecutionPolicyDigest, "trace_id": traceID,
	})
}

// Translates runtime session operations into recipient validated peer RPC
type PeerHostedRoomTransport struct {
	Binding             HostedRoomBinding
	Route               PeerMemberRoute
	Client              HostedRoomPeerClient
	SourceEventSeq      int64
	TaskID              *string
	ExecutionGeneration *int64
	sessionID           *string
	dispatch            *HostedMemberDispatch
}

// Peer transport of one member route, its client bound to the room scope
func NewPeerHostedRoomTransport(binding HostedRoomBinding, route PeerMemberRoute, client HostedRoomPeerClient, sourceEventSeq int64, taskID *string, executionGeneration *int64) (*PeerHostedRoomTransport, error) {
	if sourceEventSeq < 1 {
		return nil, &RoomValueError{Msg: "peer room source_event_seq must be positive"}
	}
	t := &PeerHostedRoomTransport{Binding: binding, Route: route, Client: client, SourceEventSeq: sourceEventSeq, TaskID: taskID, ExecutionGeneration: executionGeneration}
	if binder, ok := peerHook[peerRoomScopeBinder](client); ok {
		if err := binder.BindRoomScope(PeerRoomScope{
			RoomID: binding.RoomID, HomeInstallID: route.HomeInstallID, AuthorityGatewayID: binding.GatewayID,
			AuthorityEpoch: binding.AuthorityEpoch, MemberID: route.MemberID, TargetInstallID: route.TargetInstallID,
			TargetProfile: route.TargetProfile,
		}); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// A member dispatch has no field for files
func (t *PeerHostedRoomTransport) AcceptsAttachments() bool { return false }

func (t *PeerHostedRoomTransport) validateCoordinates(profile, source string, title *string) error {
	if source != RoomSessionSource {
		return &RoomValueError{Msg: "peer room transport requires source=bot_room"}
	}
	if profile != t.Route.TargetProfile {
		return &RoomValueError{Msg: "peer room transport profile does not match its grant"}
	}
	if title != nil && *title != RoomSessionTitle(t.Binding.RoomID) {
		return &RoomValueError{Msg: "peer room transport title does not match room identity"}
	}
	return nil
}

func (t *PeerHostedRoomTransport) prepare(ctx context.Context, profile, source string, create bool, title, expectedSessionID *string) (map[string]any, error) {
	if err := t.validateCoordinates(profile, source, title); err != nil {
		return nil, err
	}
	return t.Client.Prepare(ctx, PeerPrepare{RoomID: t.Binding.RoomID, Grant: t.Route.Grant, Profile: profile, Source: source, Create: create, ExpectedSessionID: expectedSessionID})
}

func (t *PeerHostedRoomTransport) ResolveExact(ctx context.Context, profile, title, source string) (map[string]any, error) {
	return t.prepare(ctx, profile, source, false, &title, nil)
}

func (t *PeerHostedRoomTransport) Create(ctx context.Context, profile, title, source string) (map[string]any, error) {
	session, err := t.prepare(ctx, profile, source, true, &title, nil)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, errors.New("peer did not create the room session")
	}
	id := hostedStrOrEmpty(session["session_id"])
	if id == "" {
		id = hostedStrOrEmpty(session["id"])
	}
	t.sessionID = &id
	return session, nil
}

func (t *PeerHostedRoomTransport) Resume(ctx context.Context, profile, sessionID, source string) (map[string]any, error) {
	session, err := t.prepare(ctx, profile, source, false, nil, &sessionID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, errors.New("peer room session is unavailable")
	}
	t.sessionID = &sessionID
	return session, nil
}

// Dispatches the fenced member turn, reporting an immediately terminal result
func (t *PeerHostedRoomTransport) Submit(ctx context.Context, req HostedSubmit) (map[string]any, error) {
	if len(req.Attachments) > 0 {
		return nil, &RoomValueError{Msg: "attachments_unsupported_on_route: peer route cannot carry files"}
	}
	if req.MemberID != nil && *req.MemberID != t.Route.MemberID {
		return nil, &RoomValueError{Msg: "peer room transport member does not match its grant"}
	}
	if err := t.validateCoordinates(req.Profile, req.Source, nil); err != nil {
		return nil, err
	}
	if t.sessionID != nil && *t.sessionID != req.SessionID {
		return nil, &RoomValueError{Msg: "peer room session changed during admission"}
	}
	traceID := t.Route.TraceID
	if traceID == "" {
		traceID = "trace-" + uuid4HexString()
	}
	dispatch, err := BuildMemberDispatch(t.Binding, t.Route, req.Task.RoomID, req.Task.TaskID, req.Profile, req.ExecutionGeneration, t.SourceEventSeq, req.Prompt, traceID)
	if err != nil {
		return nil, err
	}
	t.dispatch = dispatch
	sessionID := req.SessionID
	t.sessionID = &sessionID
	result, err := t.Client.Dispatch(ctx, dispatch.AsMapping(), t.Route.Grant)
	if err != nil {
		return nil, err
	}
	switch result["status"] {
	case "settled", "failed", "cancelled":
		if err := req.OnTerminal(result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (t *PeerHostedRoomTransport) History(ctx context.Context, profile, sessionID, source string) ([]map[string]any, error) {
	if err := t.validateCoordinates(profile, source, nil); err != nil {
		return nil, err
	}
	return t.Client.History(ctx, t.Binding.RoomID, profile, sessionID, t.Route.Grant)
}

func (t *PeerHostedRoomTransport) Info(ctx context.Context, profile, sessionID, source string) (map[string]any, error) {
	if err := t.validateCoordinates(profile, source, nil); err != nil {
		return nil, err
	}
	return t.Client.Status(ctx, t.Binding.RoomID, profile, sessionID, t.Route.Grant)
}

// Stops the dispatched attempt, or the recovered one from its durable receipt
func (t *PeerHostedRoomTransport) Interrupt(ctx context.Context, profile, sessionID, source, expectedTaskID string) (map[string]any, error) {
	if err := t.validateCoordinates(profile, source, nil); err != nil {
		return nil, err
	}
	if t.dispatch != nil {
		if t.dispatch.TaskID != expectedTaskID {
			return nil, nil
		}
		return t.Client.Stop(ctx, t.dispatch.AsMapping(), t.Route.Grant)
	}
	receipter, ok := peerHook[peerStopReceipter](t.Client)
	if t.TaskID == nil || *t.TaskID != expectedTaskID || t.ExecutionGeneration == nil || *t.ExecutionGeneration == 0 || !ok {
		return nil, nil
	}
	return receipter.StopReceipt(ctx, expectedTaskID, *t.ExecutionGeneration, t.Route.Grant)
}
