package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	hostedRoomIdleFallbackSeconds  = 5.0
	hostedRoomActivePollSeconds    = 0.25
	hostedRoomTerminalGraceSeconds = 30.0
)

var (
	hostedServiceTerminalStatuses  = []string{"deferred", "settled", "failed", "cancelled"}
	hostedServiceLiveStatuses      = []string{"queued", "running", "stopping"}
	hostedServiceStoppableStatuses = []string{"queued", "running", "indeterminate", "deferred", "stopping"}
	hostedServiceRetryableStatuses = []string{"indeterminate", "deferred"}
)

func hostedServiceLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.hosted_room_service")
}

// Member id of a task, its target profile when no member id is set
func TaskMemberID(task map[string]any) string {
	payload, _ := hostedMapping(task["payload"])
	if v := payload["target_member_id"]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return pyrepr.Str(payload["target_profile"])
}

// Pending action naming the exact attempt retry and discard fence on
func AttemptAction(kind string, task map[string]any) map[string]any {
	return map[string]any{"kind": kind, "member_id": TaskMemberID(task), "task_id": hrTaskIdentity(task).TaskID,
		"execution_generation": task["execution_generation"]}
}

// Room owner's discussion ceilings, defaults kept and logged for a malformed value
func hostedDiscussionLimits(root string) (DiscussionLimits, error) {
	cfg, err := config.LoadUserEffective(filepath.Join(root, "config.yaml"), config.NewSecrets(root).Lookup())
	if err != nil {
		return DiscussionLimits{}, err
	}
	limits, err := hostedDiscussionLimitsFrom(cfg)
	var invalid *DiscussionValidationError
	if errors.As(err, &invalid) {
		hostedServiceLog().Warn(fmt.Sprintf("ignoring hosted_rooms.discussion in %s/config.yaml: %s", root, err.Error()))
		return DefaultDiscussionLimits(), nil
	}
	return limits, err
}

func hostedDiscussionLimitsFrom(cfg map[string]any) (DiscussionLimits, error) {
	roomsValue, ok := cfg["hosted_rooms"]
	if !ok {
		roomsValue = map[string]any{}
	}
	rooms, isMap := hostedMapping(roomsValue)
	if !isMap {
		return DiscussionLimits{}, &DiscussionValidationError{&DiscussionPolicyError{"hosted_rooms must be a mapping"}}
	}
	settingsValue, ok := rooms["discussion"]
	if !ok {
		settingsValue = map[string]any{}
	}
	settings, isMap := hostedMapping(settingsValue)
	if !isMap {
		return DiscussionLimits{}, &DiscussionValidationError{&DiscussionPolicyError{"hosted_rooms.discussion must be a mapping"}}
	}
	maxRounds, ok := settings["max_rounds"]
	if !ok {
		maxRounds = int64(MaxDiscussionRounds)
	}
	maxMessages, ok := settings["max_messages"]
	if !ok {
		maxMessages = int64(MaxDiscussionMessages)
	}
	return NewDiscussionLimits(maxRounds, maxMessages)
}

// Agent turn timeout plus the terminal grace a room turn waits
func hostedRoomTurnTimeoutSeconds() float64 {
	raw, ok := os.LookupEnv("ICEBATH_AGENT_TIMEOUT")
	if !ok {
		raw = "1800"
	}
	agentTimeout, err := pyint.ParseFloat(raw)
	if err != nil {
		agentTimeout = 0
	}
	if !(agentTimeout > 0) {
		agentTimeout = 1800
	}
	return agentTimeout + hostedRoomTerminalGraceSeconds
}

// Whether the peer proves the scoped grant is already unusable
func grantRevokeIsTerminal(err *PeerRunsHTTPError) bool {
	return (err.StatusCode == 401 || err.StatusCode == 403) && (err.ErrorCode == "invalid_room_grant" || err.ErrorCode == "room_reauthorization_required")
}

func hostedRoomAuthority(room map[string]any) (string, int64, error) {
	epoch, err := pyint.Int64(room["authority_epoch"])
	return pyrepr.Str(room["authority_gateway_id"]), epoch, err
}

// Session operations the service resolves approvals through
type HostedRoomRPC interface {
	Approve(ctx context.Context, sessionID, requestID, choice string) (map[string]any, error)
}

// Behaviour a gateway subclass of the service replaces
type hostedRoomServiceHooks interface {
	LocalProfiles() ([]string, error)
	RoomProfiles(room map[string]any) ([]string, error)
	Bindings() ([]HostedRoomBinding, error)
	TurnLock(profile string) (func(), error)
	ResolveMemberTransport(binding HostedRoomBinding, task map[string]any) (InternalSessionRPC, error)
}

// Construction arguments of a hosted room service
type HostedRoomServiceOptions struct {
	DBPath      string
	MakeRPC     func(dbPath string) (HostedRoomRPC, error)
	PeerRoutes  map[[2]string]PeerMemberRoute
	PeerClients map[any]HostedRoomPeerClient
}

// Owns the hosted Discussion policy and its transport free worker
type HostedRoomService struct {
	ctx              context.Context
	self             hostedRoomServiceHooks
	DBPath           string
	DiscussionLimits DiscussionLimits
	policyLock       pythread.RLock
	pendingActions   map[[2]string]map[string]any
	pendingOrder     [][2]string
	PolicyCheckpoint *HostedRoomPolicyCheckpoint
	RPC              HostedRoomRPC
	linkLoadError    string
	routesMu         sync.Mutex
	peerRouteStatus  map[[2]string]string
	peerRoutes       map[[2]string]PeerMemberRoute
	peerClients      map[[2]string]HostedRoomPeerClient
	prepareStalls    *PrepareStallTracker
	runtime          *HostedRoomRuntime
}

// Hosted room service over the server's session handlers
func NewHostedRoomService(ctx context.Context, o HostedRoomServiceOptions) (*HostedRoomService, error) {
	s := &HostedRoomService{}
	if err := s.init(ctx, s, o); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *HostedRoomService) init(ctx context.Context, self hostedRoomServiceHooks, o HostedRoomServiceOptions) error {
	s.ctx, s.self, s.DBPath = ctx, self, o.DBPath
	if s.DBPath == "" {
		s.DBPath = HostedRoomsDefaultDBPath(ctx)
	}
	limits, err := hostedDiscussionLimits(s.Root())
	if err != nil {
		return err
	}
	s.DiscussionLimits = limits
	if _, err := PruneDisbandedRooms(ctx, s.DBPath, nil); err != nil {
		return err
	}
	s.pendingActions = map[[2]string]map[string]any{}
	if s.PolicyCheckpoint, err = NewHostedRoomPolicyCheckpoint(ctx, s.DBPath, 1+2*MaxDiscussionMembers*limits.MaxRounds); err != nil {
		return err
	}
	if s.RPC, err = o.MakeRPC(s.DBPath); err != nil {
		return err
	}
	s.peerRouteStatus, s.peerRoutes, s.peerClients = map[[2]string]string{}, map[[2]string]PeerMemberRoute{}, map[[2]string]HostedRoomPeerClient{}
	s.prepareStalls = NewPrepareStallTracker()
	if err := s.loadStoredLinks(); err != nil {
		s.linkLoadError = err.Error()
	}
	for key, route := range o.PeerRoutes {
		s.peerRoutes[key] = route
		client, ok := o.PeerClients[key]
		if !ok {
			client = o.PeerClients[route.TargetInstallID]
		}
		if client != nil {
			s.peerClients[key] = client
		}
	}
	opts := DefaultHostedRoomRuntimeOptions()
	opts.DBPath, opts.Rooms, opts.TransportResolver, opts.TurnLock = s.DBPath, self.Bindings, self.ResolveMemberTransport, self.TurnLock
	if rpc, ok := s.RPC.(InternalSessionRPC); ok {
		opts.RPC = rpc
	}
	opts.PrepareRoom = func(b HostedRoomBinding) error { return s.prepareTracked(b, false) }
	opts.PublishTerminal = s.PublishTerminal
	opts.PendingAction = s.setPendingAction
	opts.PollIntervalSeconds, opts.ActivePollIntervalSeconds = hostedRoomIdleFallbackSeconds, hostedRoomActivePollSeconds
	opts.TurnTimeoutSeconds = hostedRoomTurnTimeoutSeconds()
	s.runtime, err = NewHostedRoomRuntime(ctx, opts)
	return err
}

// Room worker of the service
func (s *HostedRoomService) Runtime() *HostedRoomRuntime { return s.runtime }

// Room state database the service drives
func (s *HostedRoomService) StorePath() string { return s.DBPath }

// Status snapshot of the room worker
func (s *HostedRoomService) RuntimeStatus() map[string]any { return s.runtime.Status() }

// Rehydrates persisted peer routes, per link errors collected into one string
func (s *HostedRoomService) loadStoredLinks() error {
	stored, loadErrors, err := LoadRoomLinksTolerant(s.ctx, s.DBPath)
	if err != nil {
		return err
	}
	problems := append([]string{}, loadErrors...)
	for _, link := range stored {
		key := [2]string{link.RoomID, link.MemberID}
		supported := false
		for _, v := range link.Catalog.ProtocolVersions {
			supported = supported || (v.IsInt64() && v.Int64() == HostedProtocolVersion)
		}
		if !supported {
			problems = append(problems, link.RoomID+":"+link.MemberID+":protocol-upgrade-required")
			continue
		}
		homeInstall, err := LocalAuthorityGatewayID(s.ctx)
		if err != nil {
			return err
		}
		client, err := NewDefaultPeerRunsHTTPClient(s.ctx, link.TargetURL, "", s.DBPath)
		if err != nil {
			return err
		}
		s.peerRoutes[key] = PeerMemberRoute{HomeInstallID: homeInstall, MemberID: link.MemberID, TargetInstallID: link.Catalog.InstallationID,
			TargetProfile: link.TargetProfile, CapabilityDigest: link.Catalog.CatalogDigest, ExecutionPolicyDigest: link.Catalog.ExecutionPolicy.PolicyDigest,
			CancellationScopeID: link.CancellationScopeID, TraceID: link.TraceID, Grant: link.Grant}
		s.peerClients[key] = client
		s.peerRouteStatus[key] = link.Status
	}
	if len(problems) > 0 {
		s.linkLoadError = strings.Join(problems, ",")
	}
	return nil
}

// Directory holding the room store
func (s *HostedRoomService) Root() string { return filepath.Dir(s.DBPath) }

// Default profile plus every named profile with an identity and no tombstone
func (s *HostedRoomService) LocalProfiles() ([]string, error) {
	profiles := map[string]bool{"default": true}
	profilesDir := filepath.Join(s.Root(), "profiles")
	if info, err := os.Stat(profilesDir); err == nil && info.IsDir() {
		entries, err := os.ReadDir(profilesDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			path := filepath.Join(profilesDir, entry.Name())
			if info, err := os.Stat(path); err != nil || !info.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if home.ProfileHasIdentity(path) && !home.ProfileDeleted(path) {
				profiles[entry.Name()] = true
			}
		}
	}
	out := make([]string, 0, len(profiles))
	for p := range profiles {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// Profiles a saved room's roster is checked against
func (s *HostedRoomService) RoomProfiles(map[string]any) ([]string, error) {
	return s.self.LocalProfiles()
}

// Rooms this gateway holds authority over
func (s *HostedRoomService) Bindings() ([]HostedRoomBinding, error) {
	local, err := LocalAuthorityGatewayID(s.ctx)
	if err != nil {
		return nil, err
	}
	rooms, err := ListRooms(s.ctx, s.DBPath, false, int64(MaxRoomListLimit), int64(0))
	if err != nil {
		return nil, err
	}
	out := []HostedRoomBinding{}
	for _, room := range rooms {
		gatewayID, epoch, err := hostedRoomAuthority(room)
		if err != nil {
			return nil, err
		}
		if gatewayID == local {
			out = append(out, HostedRoomBinding{RoomID: pyrepr.Str(room["room_id"]), GatewayID: gatewayID, AuthorityEpoch: epoch})
		}
	}
	return out, nil
}

func (s *HostedRoomService) room(roomID string) (map[string]any, error) {
	return RoomState(s.ctx, s.DBPath, roomID, false)
}

// Authority of a room this gateway owns, a conflict error otherwise
func (s *HostedRoomService) ownedAuthority(roomID string) (string, int64, error) {
	room, err := s.room(roomID)
	if err != nil {
		return "", 0, err
	}
	gatewayID, epoch, err := hostedRoomAuthority(room)
	if err != nil {
		return "", 0, err
	}
	local, err := LocalAuthorityGatewayID(s.ctx)
	if err != nil {
		return "", 0, err
	}
	if gatewayID != local {
		return "", 0, authorityConflict("This Group Chat is managed by another gateway.")
	}
	return gatewayID, epoch, nil
}

// Cross process turn lock of the profile under the room root
func (s *HostedRoomService) TurnLock(profile string) (func(), error) {
	return tools.AcquireTurnLock(s.ctx, s.Root(), profile, nil)
}

func (s *HostedRoomService) Start() error { s.runtime.Start(); return nil }

func (s *HostedRoomService) Stop(timeout float64) bool { return s.runtime.Stop(timeout) }

func (s *HostedRoomService) Wakeup() { s.runtime.Wakeup() }

func (s *HostedRoomService) listTasks(roomID string, statuses []string) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, status := range statuses {
		tasks, err := DriverListTasks(s.ctx, s.DBPath, roomID, &status)
		if err != nil {
			return nil, err
		}
		out = append(out, tasks...)
	}
	return out, nil
}

func (s *HostedRoomService) saveLink(roomID, memberID, targetURL, targetProfile, grant string, catalog *GatewayRoomCatalog, cancellationScopeID, traceID string) error {
	link, err := MakeStoredRoomLink(roomID, memberID, targetURL, targetProfile, grant, catalog, cancellationScopeID, traceID)
	if err != nil {
		return err
	}
	return SaveRoomLink(s.ctx, s.DBPath, link)
}

// Registers one verified route, persisting its scoped grant when a catalog and URL are given
func (s *HostedRoomService) RegisterPeerRoute(roomID, memberID string, route PeerMemberRoute, client HostedRoomPeerClient, targetURL *string, catalog *GatewayRoomCatalog) error {
	if binder, ok := peerHook[peerReceiptStoreBinder](client); ok {
		if err := binder.BindReceiptStore(s.DBPath); err != nil {
			return err
		}
	}
	if catalog != nil {
		if route.ExecutionPolicyDigest == "" {
			route.ExecutionPolicyDigest = catalog.ExecutionPolicy.PolicyDigest
		}
		if route.CapabilityDigest != catalog.CatalogDigest || route.ExecutionPolicyDigest != catalog.ExecutionPolicy.PolicyDigest {
			return &RoomValueError{Msg: "peer route does not match its target catalog"}
		}
		if targetURL != nil {
			if err := s.saveLink(roomID, memberID, *targetURL, route.TargetProfile, route.Grant, catalog, route.CancellationScopeID, route.TraceID); err != nil {
				return err
			}
		}
	}
	s.publishRoute([2]string{roomID, memberID}, route, client)
	s.runtime.Wakeup()
	return nil
}

// Makes a persisted route live as ready, binding its client when given
func (s *HostedRoomService) publishRoute(key [2]string, route PeerMemberRoute, client HostedRoomPeerClient) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	s.peerRoutes[key], s.peerRouteStatus[key] = route, "ready"
	if client != nil {
		s.peerClients[key] = client
	}
}

func (s *HostedRoomService) peerRoute(key [2]string) (PeerMemberRoute, bool, HostedRoomPeerClient) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	route, ok := s.peerRoutes[key]
	return route, ok, s.peerClients[key]
}

// Revokes and forgets every scoped peer route of one room, an unreachable target leaving it for retry
func (s *HostedRoomService) RevokeRoomRoutes(roomID string) (int, error) {
	type keyed struct {
		key   [2]string
		route PeerMemberRoute
	}
	s.routesMu.Lock()
	routes := []keyed{}
	for key, route := range s.peerRoutes {
		if key[0] == roomID {
			routes = append(routes, keyed{key, route})
		}
	}
	s.routesMu.Unlock()
	sort.Slice(routes, func(i, j int) bool { return routes[i].key[1] < routes[j].key[1] })
	for _, r := range routes {
		_, _, client := s.peerRoute(r.key)
		revoker, ok := peerHook[peerGrantRevoker](client)
		if !ok {
			return 0, errors.New("peer room grant cannot be revoked safely")
		}
		if _, err := revoker.RevokeGrant(r.route.Grant); err != nil {
			var peerErr *PeerRunsHTTPError
			if !errors.As(err, &peerErr) || !grantRevokeIsTerminal(peerErr) {
				return 0, err
			}
		}
	}
	if _, err := DeleteRoomLinkRecords(s.ctx, s.DBPath, roomID); err != nil {
		return 0, err
	}
	s.routesMu.Lock()
	for _, r := range routes {
		delete(s.peerRoutes, r.key)
		delete(s.peerRouteStatus, r.key)
		delete(s.peerClients, r.key)
	}
	s.routesMu.Unlock()
	return len(routes), nil
}

// Transport of a member turn, the in process RPC for local members and a tracked peer route otherwise
func (s *HostedRoomService) ResolveMemberTransport(binding HostedRoomBinding, task map[string]any) (InternalSessionRPC, error) {
	payload, _ := hostedMapping(task["payload"])
	memberID := hostedStrOrEmpty(payload["target_member_id"])
	if memberID == "" {
		memberID = hostedStrOrEmpty(payload["target_profile"])
	}
	key := [2]string{binding.RoomID, memberID}
	route, ok, client := s.peerRoute(key)
	if !ok {
		peer, err := s.memberIsPeer(binding.RoomID, memberID)
		if err != nil {
			return nil, err
		}
		if peer {
			return nil, errors.New("peer room route is unavailable")
		}
		rpc, _ := s.RPC.(InternalSessionRPC)
		return rpc, nil
	}
	if client == nil {
		return nil, errors.New("peer room client is unavailable")
	}
	identity, isIdentity := task["identity"].(TaskIdentity)
	executionGeneration, err := hostedIntOrZero(task["execution_generation"])
	if err != nil {
		return nil, err
	}
	if binder, ok := peerHook[peerObservationBinder](client); ok && isIdentity && executionGeneration > 0 {
		if err := binder.BindObservation(identity.TaskID, executionGeneration); err != nil {
			return nil, err
		}
	}
	setStatus := func(status string) func() error {
		return func() error { return s.setRouteStatus(key[0], key[1], status) }
	}
	tracked := &routeStatusPeerClient{client: client, onReady: setStatus("ready"), onReauthorization: setStatus("needs_reauthorization"),
		onUnavailable: setStatus("unavailable"),
		onRefreshed: func(grant string, catalog *GatewayRoomCatalog) error {
			return s.rotateRouteGrant(key[0], key[1], grant, catalog)
		}}
	if err := s.recoverPeerAdmission(binding, task, route, tracked); err != nil {
		return nil, err
	}
	sourceEventSeq, err := hostedIntOrZero(payload["source_event_seq"])
	if err != nil {
		return nil, err
	}
	var taskID *string
	if isIdentity {
		taskID = &identity.TaskID
	}
	return NewPeerHostedRoomTransport(binding, route, tracked, sourceEventSeq, taskID, &executionGeneration)
}

// Rediscovers an admitted peer run without advancing its generation
func (s *HostedRoomService) recoverPeerAdmission(binding HostedRoomBinding, task map[string]any, route PeerMemberRoute, client HostedRoomPeerClient) error {
	recoverer, hasRecover := peerHook[peerDispatchRecoverer](client)
	identity, isIdentity := task["identity"].(TaskIdentity)
	payload, isMap := hostedMapping(task["payload"])
	executionGeneration, err := hostedIntOrZero(task["execution_generation"])
	if err != nil {
		return err
	}
	status, _ := task["status"].(string)
	if !hasRecover || !isIdentity || !isMap || executionGeneration < 1 || (status != "running" && status != "indeterminate" && status != "stopping") {
		return nil
	}
	prompt, isStr := payload["prompt"].(string)
	sourceEventSeq, err := hostedIntOrZero(payload["source_event_seq"])
	if err != nil {
		return err
	}
	if !isStr || sourceEventSeq < 1 || route.TraceID == "" {
		return errors.New("peer room admission identity is unavailable for recovery")
	}
	dispatch, err := BuildMemberDispatch(binding, route, identity.RoomID, identity.TaskID, route.TargetProfile, executionGeneration, sourceEventSeq, prompt, route.TraceID)
	if err != nil {
		return err
	}
	_, err = recoverer.RecoverDispatch(dispatch.AsMapping(), route.Grant)
	return err
}

func (s *HostedRoomService) memberIsPeer(roomID, memberID string) (bool, error) {
	room, err := s.room(roomID)
	if err != nil {
		return false, err
	}
	members, _ := room["members"].([]any)
	for _, raw := range members {
		m, ok := hostedMapping(raw)
		if !ok {
			continue
		}
		id := hostedStrOrEmpty(m["member_id"])
		if id == "" {
			id = hostedStrOrEmpty(m["profile"])
		}
		if id == memberID {
			target, isMap := hostedMapping(m["target"])
			return isMap && target["kind"] == "peer", nil
		}
	}
	return false, nil
}

func (s *HostedRoomService) setRouteStatus(roomID, memberID, status string) error {
	key := [2]string{roomID, memberID}
	s.routesMu.Lock()
	if current, ok := s.peerRouteStatus[key]; ok && current == status {
		s.routesMu.Unlock()
		return nil
	}
	s.peerRouteStatus[key] = status
	s.routesMu.Unlock()
	_, err := MarkRoomLinkStatus(s.ctx, s.DBPath, roomID, memberID, status)
	return err
}

func (s *HostedRoomService) setPendingAction(roomID, memberID string, action map[string]any) error {
	s.policyLock.Lock()
	defer s.policyLock.Unlock()
	key := [2]string{roomID, memberID}
	if action == nil {
		if _, ok := s.pendingActions[key]; ok {
			delete(s.pendingActions, key)
			s.pendingOrder = hostedDropKey(s.pendingOrder, key)
		}
		return nil
	}
	copied := make(map[string]any, len(action)+1)
	for k, v := range action {
		copied[k] = v
	}
	copied["member_id"] = memberID
	if _, ok := s.pendingActions[key]; !ok {
		s.pendingOrder = append(s.pendingOrder, key)
	}
	s.pendingActions[key] = copied
	return nil
}

func hostedDropKey(order [][2]string, key [2]string) [][2]string {
	for i, k := range order {
		if k == key {
			return append(order[:i:i], order[i+1:]...)
		}
	}
	return order
}

// Persists a target refreshed scoped grant before publishing it live
func (s *HostedRoomService) rotateRouteGrant(roomID, memberID, grant string, catalog *GatewayRoomCatalog) error {
	key := [2]string{roomID, memberID}
	route, ok, _ := s.peerRoute(key)
	if !ok {
		return errors.New("peer room route is unavailable")
	}
	links, err := LoadRoomLinks(s.ctx, s.DBPath)
	if err != nil {
		return err
	}
	var stored *StoredRoomLink
	for i := range links {
		if links[i].RoomID == roomID && links[i].MemberID == memberID {
			stored = &links[i]
			break
		}
	}
	if stored == nil {
		return errors.New("peer room route cannot be renewed before persistence")
	}
	if catalog != nil {
		supported, direct := false, false
		for _, v := range catalog.ProtocolVersions {
			supported = supported || (v.IsInt64() && v.Int64() == HostedProtocolVersion)
		}
		for _, mode := range catalog.LinkModes {
			direct = direct || mode == "direct"
		}
		if catalog.InstallationID != route.TargetInstallID || catalog.ExecutionPolicy.TargetProfile != route.TargetProfile || !supported ||
			!direct || !catalog.Text || catalog.ExecutionPolicy.PolicyDigest != route.ExecutionPolicyDigest {
			if err := s.setRouteStatus(roomID, memberID, "needs_reauthorization"); err != nil {
				return err
			}
			return errors.New("peer room execution policy changed; reauthorization is required")
		}
		route.CapabilityDigest, route.ExecutionPolicyDigest = catalog.CatalogDigest, catalog.ExecutionPolicy.PolicyDigest
	}
	saved := catalog
	if saved == nil {
		saved = stored.Catalog
	}
	if err := s.saveLink(roomID, memberID, stored.TargetURL, stored.TargetProfile, grant, saved, stored.CancellationScopeID, stored.TraceID); err != nil {
		return err
	}
	route.Grant = grant
	s.publishRoute(key, route, nil)
	return nil
}

func (s *HostedRoomService) routeStatuses(roomID *string) []any {
	s.routesMu.Lock()
	keys := make([][2]string, 0, len(s.peerRouteStatus))
	for key := range s.peerRouteStatus {
		keys = append(keys, key)
	}
	statuses := make(map[[2]string]string, len(keys))
	for _, key := range keys {
		statuses[key] = s.peerRouteStatus[key]
	}
	s.routesMu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		if keys[i][1] != keys[j][1] {
			return keys[i][1] < keys[j][1]
		}
		return statuses[keys[i]] < statuses[keys[j]]
	})
	out := []any{}
	for _, key := range keys {
		if roomID == nil || key[0] == *roomID {
			out = append(out, map[string]any{"room_id": key[0], "member_id": key[1], "status": statuses[key]})
		}
	}
	return out
}

// Every event of the room replayed page by page
func (s *HostedRoomService) events(roomID string) ([]map[string]any, error) {
	events := []map[string]any{}
	var cursor int64
	for {
		page, err := ReadEvents(s.ctx, s.DBPath, roomID, cursor, int64(MaxRoomLogLimit), false)
		if err != nil {
			return nil, err
		}
		if rows, ok := page["events"].([]any); ok {
			for _, row := range rows {
				if m, ok := row.(map[string]any); ok {
					events = append(events, m)
				}
			}
		}
		next := cursor
		if v := page["cursor"]; pyrepr.Truthy(v) {
			if next, err = pyint.Int64(v); err != nil {
				return nil, err
			}
		}
		if !pyrepr.Truthy(page["has_more"]) {
			return events, nil
		}
		if next <= cursor {
			return nil, errors.New("hosted room replay cursor did not advance")
		}
		cursor = next
	}
}

func (s *HostedRoomService) policySnapshot(room map[string]any) (PolicySnapshot, error) {
	latest, err := pyint.Int64(room["latest_seq"])
	if err != nil {
		return PolicySnapshot{}, err
	}
	return s.PolicyCheckpoint.Snapshot(s.ctx, pyrepr.Str(room["room_id"]), latest)
}

func mapsToAny(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

func (s *HostedRoomService) publishTerminalTasks(room map[string]any) (bool, error) {
	changed, roomID := false, pyrepr.Str(room["room_id"])
	localProfiles, err := s.self.RoomProfiles(room)
	if err != nil {
		return false, err
	}
	cursor, err := pyint.Int64(room["latest_seq"])
	if err != nil {
		return false, err
	}
	tasks, err := s.listTasks(roomID, hostedServiceTerminalStatuses)
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		status, _ := task["status"].(string)
		generation := hrTaskInt(task, "execution_generation")
		identity := hrTaskIdentity(task)
		exists, err := s.PolicyCheckpoint.PublicationExists(s.ctx, roomID, identity.TaskID, status, generation)
		if err != nil {
			return false, err
		}
		if exists {
			continue
		}
		payload, _ := hostedMapping(task["payload"])
		sourceSeq, err := pyint.Int64(payload["source_event_seq"])
		if err != nil {
			return false, err
		}
		taskEvents, err := s.PolicyCheckpoint.EventsForTask(s.ctx, roomID, sourceSeq, payload["input_context"], identity.TaskID)
		if err != nil {
			return false, err
		}
		plan, err := ReconstructTaskPlan(room, mapsToAny(taskEvents), task, localProfiles)
		if err != nil {
			return false, err
		}
		messageID := "dmessage:" + strings.TrimPrefix(identity.TaskID, "dtask:")
		visible := false
		for _, event := range taskEvents {
			visible = visible || (pyrepr.Equal(event["event_id"], messageID) && event["kind"] == "message.member")
		}
		if visible {
			kept := []map[string]any{}
			for _, event := range taskEvents {
				seq, _ := pyint.Int64(event["seq"])
				if event["kind"] != "message.user" || seq <= sourceSeq {
					kept = append(kept, event)
				}
			}
			taskEvents = kept
		}
		var deferredGeneration *int64
		if status == "deferred" {
			deferredGeneration = &generation
		}
		publication, err := PlanPublication(room, mapsToAny(taskEvents), plan, status, task["result"], deferredGeneration, localProfiles)
		if err != nil {
			return false, err
		}
		for _, event := range publication.Events {
			in := event.AppendInput(roomID)
			in.ExpectedLatestSeq = cursor
			appended, err := AppendEvent(s.ctx, s.DBPath, in)
			if err != nil {
				return false, err
			}
			seq, err := pyint.Int64(appended["seq"])
			if err != nil {
				return false, err
			}
			cursor = max(cursor, seq)
		}
		changed = true
	}
	return changed, nil
}

func (s *HostedRoomService) appendRoomStatus(room map[string]any, decision DiscussionDecision) error {
	if decision.DiscussionEventID == nil {
		return nil
	}
	gatewayID, epoch, err := hostedRoomAuthority(room)
	if err != nil {
		return err
	}
	var threadID any
	if decision.ThreadID != nil {
		threadID = *decision.ThreadID
	}
	_, err = AppendEvent(s.ctx, s.DBPath, AppendEventInput{
		RoomID: pyrepr.Str(room["room_id"]), EventID: fmt.Sprintf("dactivity:%s:%s", *decision.DiscussionEventID, decision.Reason),
		Kind: "room.activity", Actor: map[string]any{"kind": "gateway", "id": gatewayID},
		Payload: map[string]any{"status": decision.Status, "reason_code": decision.Reason, "thread_id": threadID,
			"discussion_event_id": *decision.DiscussionEventID},
		AuthorityGatewayID: gatewayID, AuthorityEpoch: epoch,
	})
	return err
}

// Publishes terminal tasks, then admits the next Discussion turn or records the room's settled status
func (s *HostedRoomService) PrepareRoom(binding HostedRoomBinding) error {
	s.policyLock.Lock()
	defer s.policyLock.Unlock()
	room, err := s.room(binding.RoomID)
	if err != nil {
		return err
	}
	snapshot, err := s.policySnapshot(room)
	if err != nil {
		return err
	}
	changed, err := s.publishTerminalTasks(room)
	if err != nil {
		if errAs[*EventCursorConflictError](err) {
			return nil
		}
		return err
	}
	if changed {
		if room, err = s.room(binding.RoomID); err != nil {
			return err
		}
		if snapshot, err = s.policySnapshot(room); err != nil {
			return err
		}
	}
	if err := s.PolicyCheckpoint.CompactCompleted(s.ctx, binding.RoomID); err != nil {
		return err
	}
	if _, err := DriverPrunePublishedTerminalTasks(s.ctx, s.DBPath, binding.RoomID, s.runtime.driverClock(), DriverTerminalTaskRetentionSeconds, int64(DriverMaxRetainedTerminalTasks)); err != nil {
		return err
	}
	live, err := s.listTasks(binding.RoomID, hostedServiceLiveStatuses)
	if err != nil || len(live) > 0 {
		return err
	}
	profiles, err := s.self.RoomProfiles(room)
	if err != nil {
		return err
	}
	decision, err := PlanNextTask(room, mapsToAny(snapshot.Events), profiles, snapshot.Watermarks, true, s.DiscussionLimits)
	if err != nil {
		return err
	}
	switch {
	case decision.Status == "task" && decision.Task != nil:
		return s.admitPlannedTask(binding, room, snapshot, decision)
	case decision.Status == "settled" || decision.Status == "bounded":
		return s.appendRoomStatus(room, decision)
	}
	return nil
}

func (s *HostedRoomService) admitPlannedTask(binding HostedRoomBinding, room map[string]any, snapshot PolicySnapshot, decision DiscussionDecision) error {
	existing, err := DriverGetTaskForTurn(s.ctx, s.DBPath, decision.Task.Identity)
	if err != nil {
		return err
	}
	legacy := make(map[string]any, len(decision.Task.Payload))
	for k, v := range decision.Task.Payload {
		if k != "input_context" {
			legacy[k] = v
		}
	}
	profiles, err := s.self.RoomProfiles(room)
	if err != nil {
		return err
	}
	var admitted map[string]any
	existingPayload, _ := hostedMapping(existing["payload"])
	_, frozen := existingPayload["input_context"]
	switch {
	case existing != nil && frozen:
		seq, err := pyint.Int64(existingPayload["source_event_seq"])
		if err != nil {
			return err
		}
		prior, err := s.PolicyCheckpoint.EventsForTask(s.ctx, binding.RoomID, seq, existingPayload["input_context"], hrTaskIdentity(existing).TaskID)
		if err != nil {
			return err
		}
		if _, err := ReconstructTaskPlan(room, mapsToAny(prior), existing, profiles); err != nil {
			return err
		}
		admitted = existing
	case existing != nil && pyjson.Equal(existingPayload, legacy):
		if _, err := ReconstructTaskPlan(room, mapsToAny(snapshot.Events), existing, profiles); err != nil {
			return err
		}
		admitted = existing
	default:
		if admitted, err = DriverAdmitTask(s.ctx, s.DBPath, decision.Task.Identity, decision.Task.Payload, func() any { return hostedWallClock() }); err != nil {
			return err
		}
	}
	current, err := s.room(binding.RoomID)
	if err != nil {
		return err
	}
	fenceSnapshot, err := s.policySnapshot(current)
	if err != nil {
		return err
	}
	fence := fenceSnapshot.StoppedThroughSeq
	if decision.SourceEventSeq != nil && *decision.SourceEventSeq < fence {
		if _, err := s.runtime.Cancel(hrTaskIdentity(admitted), fmt.Sprintf("stop-fence:%d", fence)); err != nil {
			return err
		}
	}
	return nil
}

// Room preparation with failures counted per room, a stalled room skipped until the next Send or Retry
func (s *HostedRoomService) prepareTracked(binding HostedRoomBinding, retryStalled bool) error {
	if s.prepareStalls.Stalled(binding.RoomID) && !retryStalled {
		return nil
	}
	if err := s.PrepareRoom(binding); err != nil {
		if s.prepareStalls.Failed(binding.RoomID, err) {
			ReportPrepareStall(s.ctx, s.DBPath, binding.RoomID, err)
		}
		return err
	}
	s.prepareStalls.Clear(binding.RoomID)
	return nil
}

// Publishes a terminal task by preparing its room again, then wakes the worker
func (s *HostedRoomService) PublishTerminal(binding HostedRoomBinding, _ map[string]any) error {
	if err := s.prepareTracked(binding, false); err != nil {
		return err
	}
	s.runtime.Wakeup()
	return nil
}

// Creates a room over a validated roster under this gateway's authority
func (s *HostedRoomService) CreateRoom(roomID, name string, members any, admit func(conn state.SQLiteConn) error) (map[string]any, error) {
	profiles, err := s.self.LocalProfiles()
	if err != nil {
		return nil, err
	}
	normalized, err := ValidateRoster(members, profiles)
	if err != nil {
		return nil, err
	}
	rows := make([]any, len(normalized))
	for i, m := range normalized {
		target := map[string]any{}
		for k, v := range m.Target {
			target[k] = v
		}
		row := map[string]any{"member_id": m.MemberID, "profile": m.Profile, "handle": m.Handle, "target": target}
		if m.DisplayName != "" {
			row["display_name"] = m.DisplayName
		}
		rows[i] = row
	}
	local, err := LocalAuthorityGatewayID(s.ctx)
	if err != nil {
		return nil, err
	}
	room, err := CreateRoom(s.ctx, s.DBPath, CreateRoomInput{RoomID: roomID, Name: name, Members: rows, AuthorityGatewayID: local, Admit: admit})
	if err != nil {
		return nil, err
	}
	s.runtime.Wakeup()
	return room, nil
}

// Stores one user message, then prepares the room, a later preparation failure left to the driver
func (s *HostedRoomService) Send(roomID, eventID string, payload any) (map[string]any, error) {
	normalized, err := ValidateUserPayload(payload)
	if err != nil {
		return nil, err
	}
	gatewayID, epoch, err := s.ownedAuthority(roomID)
	if err != nil {
		return nil, err
	}
	event, err := AppendUserEvent(s.ctx, s, roomID, eventID, normalized, gatewayID, epoch)
	if err != nil {
		return nil, err
	}
	s.prepareStalls.Clear(roomID)
	if err := s.prepareAfterSend(roomID); err != nil {
		hostedServiceLog().Warn(fmt.Sprintf("hosted room %s: preparing the room after a stored Send failed; the room driver retries it", roomID), "exc_info", err)
	}
	s.runtime.Wakeup()
	return event, nil
}

func (s *HostedRoomService) prepareAfterSend(roomID string) error {
	binding, err := s.bindingFor(roomID)
	if err != nil || binding == nil {
		return err
	}
	return s.prepareTracked(*binding, false)
}

func (s *HostedRoomService) bindingFor(roomID string) (*HostedRoomBinding, error) {
	bindings, err := s.self.Bindings()
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if b.RoomID == roomID {
			found := b
			return &found, nil
		}
	}
	return nil, nil
}

// Fences the room's earlier turns and cancels every stoppable task, the count of tasks it touched
func (s *HostedRoomService) StopRoom(roomID, cancelID string, requireAcknowledged bool) (int, error) {
	gatewayID, epoch, err := s.ownedAuthority(roomID)
	if err != nil {
		return 0, err
	}
	if _, err := RequestRoomStop(s.ctx, s.DBPath, roomID, cancelID, gatewayID, epoch); err != nil {
		return 0, err
	}
	pending, count, err := s.cancelStoppable(roomID, cancelID)
	if err != nil {
		return 0, err
	}
	if requireAcknowledged && pending > 0 {
		return 0, errors.New("room work is still stopping; retry deletion after Stop completes")
	}
	s.runtime.Wakeup()
	return count, nil
}

func (s *HostedRoomService) cancelStoppable(roomID, cancelID string) (int, int, error) {
	s.policyLock.Lock()
	defer s.policyLock.Unlock()
	listed, err := s.listTasks(roomID, hostedServiceStoppableStatuses)
	if err != nil {
		return 0, 0, err
	}
	seen, tasks := map[[2]string]int{}, []map[string]any{}
	for _, task := range listed {
		identity := hrTaskIdentity(task)
		key := [2]string{identity.RoomID, identity.TaskID}
		if i, ok := seen[key]; ok {
			tasks[i] = task
			continue
		}
		seen[key] = len(tasks)
		tasks = append(tasks, task)
	}
	pending := 0
	for _, task := range tasks {
		ownCancelID := ""
		if task["status"] == "stopping" {
			ownCancelID = hostedStrOrEmpty(task["cancel_id"])
		}
		if ownCancelID == "" {
			ownCancelID = cancelID
		}
		result, err := s.runtime.Cancel(hrTaskIdentity(task), ownCancelID)
		if err != nil {
			return 0, 0, err
		}
		if result["status"] == "stopping" {
			pending++
		}
	}
	return pending, len(tasks), nil
}

// Retries one uncertain or deferred task, or a stalled room's preparation, after explicit user action
func (s *HostedRoomService) RetryRoomTask(roomID, taskID string, memberID *string, executionGeneration *int64) (map[string]any, error) {
	if strings.HasPrefix(taskID, PrepareRetryTaskPrefix) {
		return s.retryPreparation(roomID, taskID)
	}
	candidates, err := s.listTasks(roomID, hostedServiceRetryableStatuses)
	if err != nil {
		return nil, err
	}
	for _, c := range candidates {
		if hrTaskIdentity(c).TaskID != taskID || (memberID != nil && *memberID != TaskMemberID(c)) ||
			(executionGeneration != nil && *executionGeneration != hrTaskInt(c, "execution_generation")) {
			continue
		}
		generation := hrTaskInt(c, "execution_generation")
		return s.runtime.RetryIndeterminate(hrTaskIdentity(c), &generation)
	}
	return nil, invalidTransition("no retryable room task matches task_id")
}

// Prepares a stalled room once now, a failure staying stalled and returned
func (s *HostedRoomService) retryPreparation(roomID, taskID string) (map[string]any, error) {
	binding, err := s.bindingFor(roomID)
	if err != nil {
		return nil, err
	}
	if binding == nil || taskID != PrepareRetryTaskPrefix+roomID || !s.prepareStalls.Stalled(roomID) {
		return nil, invalidTransition("no retryable room task matches task_id")
	}
	if err := s.prepareTracked(*binding, true); err != nil {
		s.prepareStalls.Stall(roomID, err)
		return nil, err
	}
	s.runtime.Wakeup()
	return map[string]any{"identity": TaskIdentity{RoomID: roomID, TaskID: taskID, ThreadID: taskID, TurnID: taskID},
		"status": "prepared", "execution_generation": int64(1), "cancel_generation": int64(0)}, nil
}

// Resolves one exact local or peer approval and wakes room observation
func (s *HostedRoomService) ApproveRoomTask(roomID, memberID, taskID string, executionGeneration int64, choice string, requestID *string) (map[string]any, error) {
	key := [2]string{roomID, memberID}
	route, hasRoute, client := s.peerRoute(key)
	s.policyLock.Lock()
	action := s.pendingActions[key]
	s.policyLock.Unlock()
	requested := ""
	if requestID != nil {
		requested = *requestID
	}
	matches := func(pending map[string]any) bool {
		if pending == nil {
			return false
		}
		generation, err := hostedIntOrZero(pending["execution_generation"])
		return err == nil && hostedStrOrEmpty(pending["request_id"]) == requested && pyrepr.Equal(pending["task_id"], taskID) && generation == executionGeneration
	}
	if requested == "" || !matches(action) {
		return nil, errors.New("room approval is no longer pending")
	}
	if choice != "once" && choice != "deny" {
		return nil, errors.New("room approval choice must be once or deny")
	}
	var result map[string]any
	var err error
	if approver, ok := peerHook[peerApprover](client); hasRoute && ok {
		result, err = approver.ApproveReceipt(taskID, executionGeneration, requested, choice, route.Grant)
	} else {
		sessionID := hostedStrOrEmpty(action["session_id"])
		if sessionID == "" {
			return nil, errors.New("local room approval identity is unavailable")
		}
		result, err = s.RPC.Approve(s.ctx, sessionID, requested, choice)
	}
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("room approval target is unavailable")
	}
	s.policyLock.Lock()
	if matches(s.pendingActions[key]) {
		delete(s.pendingActions, key)
		s.pendingOrder = hostedDropKey(s.pendingOrder, key)
	}
	s.policyLock.Unlock()
	s.runtime.Wakeup()
	return result, nil
}

// Runtime health with peer routes, or one room's working, blocked and pending action view
func (s *HostedRoomService) Status(roomID *string) (map[string]any, error) {
	runtime := s.runtime.Status()
	runtime["peer_routes"] = s.routeStatuses(roomID)
	if s.linkLoadError != "" {
		runtime["link_load_error"] = s.linkLoadError
	}
	if roomID == nil {
		return runtime, nil
	}
	tasks, err := DriverListTasks(s.ctx, s.DBPath, *roomID, nil)
	if err != nil {
		return nil, err
	}
	counts := map[string]any{}
	pendingActions := []any{}
	for _, task := range tasks {
		status := pyrepr.Str(task["status"])
		n, _ := counts[status].(int64)
		counts[status] = n + 1
		if status == "indeterminate" || status == "deferred" {
			pendingActions = append(pendingActions, AttemptAction("retry", task))
		}
	}
	if s.prepareStalls.Stalled(*roomID) {
		room, err := s.room(*roomID)
		if err != nil {
			return nil, err
		}
		pendingActions = append(pendingActions, PrepareRetryAction(*roomID, room))
	}
	s.policyLock.Lock()
	for _, key := range s.pendingOrder {
		if key[0] == *roomID {
			copied := map[string]any{}
			for k, v := range s.pendingActions[key] {
				copied[k] = v
			}
			pendingActions = append(pendingActions, copied)
		}
	}
	s.policyLock.Unlock()
	activity := []any{}
	for _, task := range tasks {
		status := pyrepr.Str(task["status"])
		if status == "queued" || status == "running" || status == "stopping" {
			activity = append(activity, map[string]any{"member_id": TaskMemberID(task), "status": status})
		}
	}
	blocked := false
	for _, r := range runtime["blocked_rooms"].([]any) {
		blocked = blocked || r == *roomID
	}
	var lastError any
	if text := s.prepareStalls.LastError(*roomID); text != nil {
		lastError = *text
	}
	return map[string]any{
		"running": runtime["running"], "working": len(activity) > 0,
		"blocked": blocked || pyrepr.Truthy(counts["indeterminate"]) || pyrepr.Truthy(counts["stopping"]),
		"counts":  counts, "pending_actions": pendingActions, "member_activity": activity,
		"last_error": lastError, "peer_routes": s.routeStatuses(roomID),
	}, nil
}
