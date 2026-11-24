package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	Dispatch(ctx context.Context, dispatch *jsonutil.Object, grant string) (map[string]any, error)
	History(ctx context.Context, roomID, profile, sessionID, grant string) ([]map[string]any, error)
	Status(ctx context.Context, roomID, profile, sessionID, grant string) (map[string]any, error)
	Stop(ctx context.Context, dispatch *jsonutil.Object, grant string) (map[string]any, error)
}

// Peer client that binds its room scope before calls
type peerRoomScopeBinder interface {
	BindRoomScope(scope PeerRoomScope) error
}

// Peer client that stops an attempt from its durable receipt alone
type peerStopReceipter interface {
	StopReceipt(ctx context.Context, taskID string, executionGeneration int64, grant string) (map[string]any, error)
}

// Peer client that hands a member's turn files to the peer before the turn, resuming from what the peer holds
type peerFileSender interface {
	SendFile(ctx context.Context, grant string, file map[string]any, data []byte) (map[string]any, error)
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
	Attachments           bool
}

// Fully fenced member dispatch shared by submit and recovery, naming the files the peer holds for the turn
func BuildMemberDispatch(binding HostedRoomBinding, route PeerMemberRoute, roomID, taskID, targetProfile string, executionGeneration, sourceEventSeq int64, prompt, traceID string, files []map[string]any) (*HostedMemberDispatch, error) {
	digest := sha256.Sum256([]byte(prompt))
	mapping := map[string]any{
		"protocol_version": int64(HostedProtocolVersion), "room_id": roomID, "home_install_id": route.HomeInstallID,
		"authority_gateway_id": binding.GatewayID, "authority_epoch": binding.AuthorityEpoch, "member_id": route.MemberID,
		"target_install_id": route.TargetInstallID, "target_profile": targetProfile, "task_id": taskID,
		"execution_generation": executionGeneration, "source_event_seq": sourceEventSeq,
		"cancellation_scope_id": route.CancellationScopeID, "prompt": prompt, "prompt_digest": hex.EncodeToString(digest[:]),
		"capability_digest": route.CapabilityDigest, "execution_policy_digest": route.ExecutionPolicyDigest, "trace_id": traceID,
	}
	if len(files) > 0 {
		items := make([]any, len(files))
		for i, file := range files {
			items[i] = file
		}
		mapping["attachments"] = items
	}
	return HostedMemberDispatchFromMapping(mapping)
}

// Dispatch items of a turn's bound files with the digests the home store verified, their bytes when wanted
func peerTurnFiles(ctx context.Context, dbPath, roomID, memberID string, attachments []any, withBytes bool) ([]map[string]any, [][]byte, error) {
	manifest, err := validateBoundTaskManifest(attachments)
	if err != nil {
		return nil, nil, err
	}
	store, err := NewHostedRoomAttachmentStore(ctx, dbPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, nil, err
	}
	files := make([]map[string]any, 0, len(manifest))
	var data [][]byte
	for _, item := range manifest {
		var saved map[string]any
		if withBytes {
			read, err := store.Read(roomID, item["attachment_id"], memberID, item["event_id"], false)
			if err != nil {
				return nil, nil, err
			}
			saved = read.Attachment
			data = append(data, read.Data)
		} else if saved, err = store.Describe(roomID, item["attachment_id"], memberID, item["event_id"]); err != nil {
			return nil, nil, err
		}
		if !hostedItemMatches(saved, item) {
			return nil, nil, runtimeRefusal("permission_denied")
		}
		files = append(files, map[string]any{"attachment_id": item["attachment_id"], "kind": item["kind"], "mime": item["mime"],
			"name": item["name"], "size": item["size"], "sha256": saved["sha256"]})
	}
	return files, data, nil
}

// Translates runtime session operations into recipient validated peer RPC
type PeerHostedRoomTransport struct {
	Binding             HostedRoomBinding
	Route               PeerMemberRoute
	Client              HostedRoomPeerClient
	SourceEventSeq      int64
	TaskID              *string
	ExecutionGeneration *int64
	StorePath           string
	AdoptCatalog        func(grant string, catalog *GatewayRoomCatalog) error
	sessionID           *string
	dispatch            *HostedMemberDispatch
}

// Peer client that reads the target's live catalog under a room grant
type peerProber interface {
	Probe(grant string) (map[string]any, error)
}

// Takes the peer's live catalog when it changed under the same execution policy, the route following it
func (t *PeerHostedRoomTransport) refreshCatalog() error {
	prober, ok := peerHook[peerProber](t.Client)
	if !ok || t.AdoptCatalog == nil {
		return nil
	}
	probe, err := prober.Probe(t.Route.Grant)
	if err != nil {
		return err
	}
	live, err := GatewayRoomCatalogFromMapping(probe["catalog"])
	if err != nil {
		return err
	}
	if live.CatalogDigest == t.Route.CapabilityDigest {
		return nil
	}
	if err := t.AdoptCatalog(t.Route.Grant, live); err != nil {
		return err
	}
	pkgLog().Info(fmt.Sprintf("hosted room %s: member %s took the peer's changed catalog, files %v", t.Binding.RoomID, t.Route.MemberID, live.Attachments))
	t.Route.CapabilityDigest, t.Route.Attachments = live.CatalogDigest, live.Attachments
	return nil
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

// Whether the peer's catalog takes room files and the home store is there to send them from
func (t *PeerHostedRoomTransport) AcceptsAttachments() bool {
	return t.Route.Attachments && t.StorePath != ""
}

// Bound files of a turn as dispatch items with their bytes from the home store
type peerTurnBytes struct {
	items []map[string]any
	data  [][]byte
}

// Hands each bound file to the peer under a fresh grant, a refusal naming the file and the peer's rule
func (t *PeerHostedRoomTransport) sendFiles(ctx context.Context, dispatch *HostedMemberDispatch, files peerTurnBytes) error {
	sender, ok := peerHook[peerFileSender](t.Client)
	if !ok {
		return &HostedAttemptError{Err: errors.New("this member's RoomLink cannot carry files"), RefusedBeforeSubmit: true}
	}
	if tracked, ok := t.Client.(*routeStatusPeerClient); ok {
		grant, err := tracked.refreshGrant(dispatch.AsMapping(), t.Route.Grant)
		if err != nil {
			return &HostedAttemptError{Err: err, NotAdmitted: true}
		}
		t.Route.Grant = grant
	}
	for i, file := range files.items {
		if _, err := sender.SendFile(ctx, t.Route.Grant, file, files.data[i]); err != nil {
			var refused *PeerFileRefusedError
			var transfer *PeerFileTransferError
			switch {
			case errors.As(err, &refused):
				return &HostedAttemptError{Err: err, RefusedBeforeSubmit: true}
			case errors.As(err, &transfer):
				return err
			}
			return &HostedAttemptError{Err: err, NotAdmitted: true}
		}
	}
	return nil
}

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

// Dispatches the fenced member turn after its files reached the peer, reporting an immediately terminal result
func (t *PeerHostedRoomTransport) Submit(ctx context.Context, req HostedSubmit) (map[string]any, error) {
	if len(req.Attachments) > 0 && !t.AcceptsAttachments() {
		return nil, &HostedAttemptError{Err: &AttachmentsUnsupportedError{Msg: hostedFilesRefusal(req.Attachments, t)}, RefusedBeforeSubmit: true}
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
	var files peerTurnBytes
	if len(req.Attachments) > 0 {
		var err error
		if files, err = PrepareBeforeSubmit(func() (peerTurnBytes, error) {
			items, data, err := peerTurnFiles(ctx, t.StorePath, t.Binding.RoomID, t.Route.MemberID, req.Attachments, true)
			return peerTurnBytes{items, data}, err
		}); err != nil {
			return nil, err
		}
	}
	dispatch, err := BuildMemberDispatch(t.Binding, t.Route, req.Task.RoomID, req.Task.TaskID, req.Profile, req.ExecutionGeneration, t.SourceEventSeq, req.Prompt, traceID, files.items)
	if err != nil {
		return nil, err
	}
	if len(files.items) > 0 {
		if err := t.sendFiles(ctx, dispatch, files); err != nil {
			return nil, err
		}
	}
	t.dispatch = dispatch
	sessionID := req.SessionID
	t.sessionID = &sessionID
	result, err := t.Client.Dispatch(ctx, dispatch.AsMapping(), t.Route.Grant)
	var drift *PeerRunsHTTPError
	if errors.As(err, &drift) && drift.ErrorCode == peerCapabilityChange[0] {
		if err := t.refreshCatalog(); err != nil {
			return nil, err
		}
		if dispatch, err = BuildMemberDispatch(t.Binding, t.Route, req.Task.RoomID, req.Task.TaskID, req.Profile, req.ExecutionGeneration, t.SourceEventSeq, req.Prompt, traceID, files.items); err != nil {
			return nil, err
		}
		t.dispatch = dispatch
		result, err = t.Client.Dispatch(ctx, dispatch.AsMapping(), t.Route.Grant)
	}
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
