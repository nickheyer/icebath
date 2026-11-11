package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// State meta prefix naming the durable owner subject of a hosted room
const hostedRoomOwnerKey = "gateway.hosted.owner.v1:"

// Capabilities of the room owner principal a local member producer runs as
var hostedProducerCaps = map[string]bool{"session:create": true, "session:read": true, "session:submit": true,
	"session:control": true, "session:approve": true}

// Authority bound hosted room service whose member turns run through the canonical session FIFO
type CanonicalHostedRoomService struct {
	*HostedRoomService
	Authority          *SessionAuthority
	loop               context.Context
	memberMu           sync.Mutex
	memberRPCs         *pydict.Dict[[5]string, InternalSessionRPC]
	transportInstalled bool
	readyImportStarted bool
	flagsMu            sync.Mutex
}

// Hosted room service over the authority's own room store, producers dispatching onto the owner loop
func NewCanonicalHostedRoomService(ctx context.Context, authority *SessionAuthority, loop context.Context) (*CanonicalHostedRoomService, error) {
	c := &CanonicalHostedRoomService{HostedRoomService: &HostedRoomService{}, Authority: authority, loop: loop,
		memberRPCs: pydict.New[[5]string, InternalSessionRPC]()}
	err := c.HostedRoomService.init(ctx, c, HostedRoomServiceOptions{DBPath: authority.DB.Path(),
		MakeRPC: func(string) (HostedRoomRPC, error) { return c, nil }})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Seatable local profile homes by name for this authority's groups
func (c *CanonicalHostedRoomService) ProfileHomes() (map[string]string, []string, error) {
	homes, _, err := MemberRoster(c.ctx, c.Authority, true)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]string, homes.Len())
	for name, profileHome := range homes.All() {
		out[name] = profileHome
	}
	return out, homes.Keys(), nil
}

// Profile names a new group may seat, in roster order
func (c *CanonicalHostedRoomService) LocalProfiles() ([]string, error) {
	_, names, err := c.ProfileHomes()
	return names, err
}

// Local profiles plus every local member a saved room already seats, sorted
func (c *CanonicalHostedRoomService) RoomProfiles(room map[string]any) ([]string, error) {
	names, err := c.LocalProfiles()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	members, _ := pyjson.Plain(room["members"]).([]any)
	for _, raw := range members {
		member, ok := hostedMapping(raw)
		if !ok {
			continue
		}
		profile, isText := member["profile"].(string)
		if !isText {
			continue
		}
		target := map[string]any{}
		if pyrepr.Truthy(member["target"]) {
			if target, ok = hostedMapping(member["target"]); !ok {
				return nil, errors.New("'" + pyrepr.TypeName(member["target"]) + "' object has no attribute 'get'")
			}
		}
		kind, has := target["kind"]
		if !has {
			kind = "local"
		}
		if pyrepr.Equal(kind, "local") {
			seen[profile] = true
		}
	}
	return slices.Sorted(mapsKeys(seen)), nil
}

// Source owner attestation of a hosted operation against current membership and durable task state
func (c *CanonicalHostedRoomService) Attest(ctx context.Context, selector map[string]any, operation string, params map[string]any) (map[string]any, error) {
	if len(selector) != 3 {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, key := range []string{"room_id", "member_id", "profile"} {
		if _, has := selector[key]; !has {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	roomID, _ := selector["room_id"].(string)
	member, profile := selector["member_id"], selector["profile"]
	owner, err := c.owner(roomID)
	if err != nil {
		return nil, err
	}
	if _, _, err := c.ownedAuthority(roomID); err != nil {
		return nil, err
	}
	room, err := c.room(roomID)
	if err != nil {
		return nil, err
	}
	seated := false
	members, _ := pyjson.Plain(room["members"]).([]any)
	for _, raw := range members {
		m, _ := hostedMapping(raw)
		target, _ := hostedMapping(m["target"])
		kind, has := target["kind"]
		if !has {
			kind = "local"
		}
		if pyrepr.Equal(m["member_id"], member) && pyrepr.Equal(m["profile"], profile) && pyrepr.Equal(kind, "local") {
			seated = true
			break
		}
	}
	if !seated {
		return nil, runtimeRefusal("permission_denied")
	}
	homes, _, err := c.ProfileHomes()
	if err != nil {
		return nil, err
	}
	profileName, _ := profile.(string)
	targetHome, found := homes[profileName]
	if !found || !pyrepr.Equal(params["_target_home"], targetHome) {
		return nil, runtimeRefusal("permission_denied")
	}
	result := map[string]any{"owner": owner, "target_home": targetHome}
	if operation != "submit" && operation != "execute" && operation != "attachment" {
		return result, nil
	}
	tasks, err := DriverListTasks(ctx, c.DBPath, roomID, nil)
	if err != nil {
		return nil, err
	}
	var matches []map[string]any
	for _, task := range tasks {
		if c.attestMatches(task, member, profile, operation, params) {
			matches = append(matches, task)
		}
	}
	if len(matches) != 1 {
		return nil, runtimeRefusal("permission_denied")
	}
	payload, _ := hostedMapping(matches[0]["payload"])
	manifestValue, has := payload["attachments"]
	if !has {
		manifestValue = []any{}
	}
	memberID := pyrepr.Str(member)
	if operation == "attachment" {
		manifest, err := hostedManifestItems(manifestValue)
		if err != nil {
			return nil, err
		}
		chunk, err := sourceAttachmentChunk(ctx, c.DBPath, memberID, roomID, manifest, params)
		if err != nil {
			return nil, err
		}
		for key, value := range chunk {
			result[key] = value
		}
		return result, nil
	}
	manifest, err := hostedManifestItems(manifestValue)
	if err != nil {
		return nil, err
	}
	digests, err := sourceAttachmentDigests(ctx, c.DBPath, memberID, roomID, manifest)
	if err != nil {
		return nil, err
	}
	result["prompt"], result["attachments"], result["attachment_digests"] = payload["prompt"], manifestValue, digests
	return result, nil
}

// Stored manifest items of a running task payload as mappings
func hostedManifestItems(value any) ([]map[string]any, error) {
	list, _ := pyjson.Plain(value).([]any)
	out := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		item, ok := hostedMapping(raw)
		if !ok {
			return nil, errors.New("'" + pyrepr.TypeName(raw) + "' object is not subscriptable")
		}
		out = append(out, item)
	}
	return out, nil
}

// Whether a task is the exact running attempt the attested operation names
func (c *CanonicalHostedRoomService) attestMatches(task map[string]any, member, profile any, operation string, params map[string]any) bool {
	payload, _ := hostedMapping(task["payload"])
	execution, _ := hostedStrictInt(task["execution_generation"])
	wantedGeneration, isInt := hostedStrictInt(params["execution_generation"])
	if !pyrepr.Equal(hrTaskIdentity(task).AsDict(), pyjson.Plain(params["task"])) || !isInt || execution != wantedGeneration ||
		!pyrepr.Equal(task["status"], "running") {
		return false
	}
	targetMember, has := payload["target_member_id"]
	if !has {
		targetMember = payload["target_profile"]
	}
	if !pyrepr.Equal(targetMember, member) || !pyrepr.Equal(payload["target_profile"], profile) {
		return false
	}
	if operation == "execute" {
		return true
	}
	prompt, _ := payload["prompt"].(string)
	if wantedDigest, has := params["prompt_sha256"]; has {
		digest := sha256.Sum256([]byte(prompt))
		if !pyrepr.Equal(hex.EncodeToString(digest[:]), wantedDigest) {
			return false
		}
	} else if !pyrepr.Equal(payload["prompt"], params["prompt"]) {
		return false
	}
	stored, has := payload["attachments"]
	if !has {
		stored = []any{}
	}
	sent := params["attachments"]
	if !pyrepr.Truthy(sent) {
		sent = []any{}
	}
	return pyrepr.Equal(pyjson.Plain(stored), pyjson.Plain(sent))
}

// Room bindings of the base service whose rooms this authority owns
func (c *CanonicalHostedRoomService) Bindings() ([]HostedRoomBinding, error) {
	owned := map[string]bool{}
	err := c.Authority.DB.ReadCtx(c.ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(c.ctx, "SELECT key FROM state_meta WHERE key LIKE ?", hostedRoomOwnerKey+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			owned[key[len(hostedRoomOwnerKey):]] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	bindings, err := c.HostedRoomService.Bindings()
	if err != nil {
		return nil, err
	}
	out := []HostedRoomBinding{}
	for _, binding := range bindings {
		if owned[binding.RoomID] {
			out = append(out, binding)
		}
	}
	return out, nil
}

// Member turns serialize on the canonical FIFO, so no cross process lock is held
func (c *CanonicalHostedRoomService) TurnLock(string) (func(), error) { return func() {}, nil }

// Checks, or with create claims, room ownership, inside the caller's transaction when a conn is given
func (c *CanonicalHostedRoomService) AuthorizeRoom(ctx context.Context, actorSubject, roomID any, create bool, conn state.SQLiteConn) (bool, error) {
	subject, isSubject := actorSubject.(string)
	room, isRoom := roomID.(string)
	if !isSubject || subject == "" || !isRoom || len([]rune(room)) > 128 || !roomIdentifierRe.MatchString(room) {
		return false, runtimeRefusal("invalid_params")
	}
	write := func(conn state.SQLiteConn) error {
		if err := state.RuntimeEpoch(ctx, conn, c.Authority.Epoch); err != nil {
			return err
		}
		key := hostedRoomOwnerKey + room
		row, err := state.ConnRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", key)
		if err != nil {
			return err
		}
		if row == nil && create {
			historical, err := state.ConnRow(ctx, conn, "SELECT 1 FROM hosted_rooms WHERE room_id=? UNION ALL "+
				"SELECT 1 FROM hosted_room_retired_ids WHERE room_id=?", room, room)
			if err != nil {
				return err
			}
			if historical == nil {
				_, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, subject)
				return err
			}
		}
		if row == nil || !SameOwner(row["value"], subject) {
			return runtimeRefusal("permission_denied")
		}
		return nil
	}
	if conn != nil {
		return true, write(conn)
	}
	return true, c.Authority.DB.ExecuteWrite(ctx, write)
}

// Durable owner subject of a room this authority claimed, refused when it claimed none
func (c *CanonicalHostedRoomService) owner(roomID string) (string, error) {
	var row map[string]any
	err := c.Authority.DB.ReadCtx(c.ctx, func(conn state.SQLiteConn) error {
		var err error
		row, err = state.ConnRow(c.ctx, conn, "SELECT value FROM state_meta WHERE key=?", hostedRoomOwnerKey+roomID)
		return err
	})
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", runtimeRefusal("permission_denied")
	}
	return pyrepr.Str(row["value"]), nil
}

// Member turn producer, a peer route when remote, an owner relay for another home, else the local FIFO
func (c *CanonicalHostedRoomService) ResolveMemberTransport(binding HostedRoomBinding, task map[string]any) (InternalSessionRPC, error) {
	payload, _ := hostedMapping(task["payload"])
	memberValue := payload["target_member_id"]
	if !pyrepr.Truthy(memberValue) {
		memberValue = payload["target_profile"]
	}
	member := pyrepr.Str(memberValue)
	peer, err := c.memberIsPeer(binding.RoomID, member)
	if err != nil {
		return nil, err
	}
	if peer {
		return c.HostedRoomService.ResolveMemberTransport(binding, task)
	}
	profileValue, has := payload["target_profile"]
	if !has {
		return nil, &pyKeyError{"target_profile"}
	}
	profile := pyrepr.Str(profileValue)
	owner, err := c.owner(binding.RoomID)
	if err != nil {
		return nil, err
	}
	homes, _, err := c.ProfileHomes()
	if err != nil {
		return nil, err
	}
	memberHome, found := homes[profile]
	if !found {
		return nil, runtimeRefusal("permission_denied")
	}
	key := [5]string{binding.RoomID, member, profile, owner, memberHome}
	c.memberMu.Lock()
	defer c.memberMu.Unlock()
	if rpc, ok := c.memberRPCs.Get(key); ok {
		return rpc, nil
	}
	if !hostedSamePath(memberHome, c.Authority.ProfileID) {
		rpc := NewHostedRoomOwnerRPC(memberHome, c.Authority.ProfileID, binding.RoomID, member, profile)
		c.memberRPCs.Set(key, rpc)
		return rpc, nil
	}
	authorized := func(ctx context.Context, conn state.SQLiteConn, operation string, identity *TaskIdentity, generation *int64) (bool, error) {
		return c.producerAuthorized(ctx, conn, binding, owner, member, profile, memberHome, operation, identity, generation)
	}
	authorize := func(ctx context.Context, operation string, identity *TaskIdentity, generation *int64) (bool, error) {
		var allowed bool
		err := c.Authority.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			var err error
			allowed, err = authorized(ctx, conn, operation, identity, generation)
			return err
		})
		return allowed, err
	}
	authorizeWrite := func(ctx context.Context, conn state.SQLiteConn, identity TaskIdentity, generation int64) error {
		allowed, err := authorized(ctx, conn, "submit", &identity, &generation)
		if err != nil {
			return err
		}
		if !allowed {
			return runtimeRefusal("permission_denied")
		}
		return nil
	}
	principal := Principal{Subject: owner, ProfileID: c.Authority.ProfileID, Capabilities: hostedProducerCaps,
		TransportID: "hosted:" + binding.RoomID + ":" + member}
	rpc, err := NewHostedRoomAuthorityRPC(c.Authority, c.loop, binding.RoomID, member, profile, principal, authorize, authorizeWrite)
	if err != nil {
		return nil, err
	}
	c.memberRPCs.Set(key, rpc)
	return rpc, nil
}

// Pathlib equality of two paths, case folded where the platform folds case
func hostedSamePath(a, b string) bool {
	return pypath.Normcase(pypath.PurePathStr(a)) == pypath.Normcase(pypath.PurePathStr(b))
}

// Whether a producer operation still matches room ownership, authority, seating, home and task state in one snapshot
func (c *CanonicalHostedRoomService) producerAuthorized(ctx context.Context, conn state.SQLiteConn, binding HostedRoomBinding, owner, member, profile, memberHome string,
	operation string, identity *TaskIdentity, generation *int64) (bool, error) {
	if err := state.RuntimeEpoch(ctx, conn, c.Authority.Epoch); err != nil {
		return false, err
	}
	owned, err := state.ConnRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", hostedRoomOwnerKey+binding.RoomID)
	if err != nil {
		return false, err
	}
	if owned == nil || !pyrepr.Equal(owned["value"], owner) {
		return false, nil
	}
	storedRooms, err := queryHostedRows(ctx, conn, "SELECT * FROM hosted_rooms WHERE room_id=?", binding.RoomID)
	if err != nil {
		return false, err
	}
	if len(storedRooms) == 0 {
		return false, nil
	}
	if disbanded, _ := storedRooms[0].Get("disbanded_at"); disbanded != nil {
		return false, nil
	}
	room, err := hostedRoomFromRow(storedRooms[0], false)
	if err != nil {
		return false, err
	}
	if !pyrepr.Equal(room["authority_gateway_id"], binding.GatewayID) || !pyrepr.Equal(room["authority_epoch"], binding.AuthorityEpoch) {
		return false, nil
	}
	seated := false
	members, _ := pyjson.Plain(room["members"]).([]any)
	for _, raw := range members {
		m, _ := hostedMapping(raw)
		if !pyrepr.Equal(m["member_id"], member) || !pyrepr.Equal(m["profile"], profile) {
			continue
		}
		target, present := m["target"]
		if target == nil {
			seated = true
			break
		}
		if targetMap, ok := hostedMapping(target); present && ok {
			kind, has := targetMap["kind"]
			if !has {
				kind = "local"
			}
			if pyrepr.Equal(kind, "local") {
				seated = true
				break
			}
		}
	}
	if !seated {
		return false, nil
	}
	homes, _, err := c.ProfileHomes()
	if err != nil {
		return false, err
	}
	if current, found := homes[profile]; !found || !hostedSamePath(current, memberHome) {
		return false, nil
	}
	if identity == nil {
		return true, nil
	}
	storedTasks, err := queryHostedRows(ctx, conn, "SELECT * FROM hosted_room_driver_tasks WHERE room_id=? AND task_id=?", binding.RoomID, identity.TaskID)
	if err != nil {
		return false, err
	}
	if len(storedTasks) == 0 {
		return false, nil
	}
	current, err := driverTaskFromRow(storedTasks[0], false)
	if err != nil {
		return false, err
	}
	payload, _ := hostedMapping(current["payload"])
	targetMember, has := payload["target_member_id"]
	if !has {
		targetMember = profile
	}
	statuses := map[string]bool{"running": true, "stopping": true}
	if operation == "submit" || operation == "execute" {
		statuses = map[string]bool{"running": true}
	}
	execution, _ := hostedStrictInt(current["execution_generation"])
	status, _ := current["status"].(string)
	return hrTaskIdentity(current) == *identity && generation != nil && execution == *generation &&
		pyrepr.Equal(payload["target_profile"], profile) && pyrepr.Equal(targetMember, member) && statuses[status], nil
}

// Rows of a hosted query as ordered objects
func queryHostedRows(ctx context.Context, conn state.SQLiteConn, query string, args ...any) ([]*pyjson.Object, error) {
	return state.QueryObjects(ctx, conn, query, args...)
}

// Reconstructs the private producer of a queued hosted admission from durable task state before its claim
func (c *CanonicalHostedRoomService) CheckAdmission(ctx context.Context, ref SessionRef, row *pyjson.Object) error {
	err := c.checkAdmission(ctx, ref, row)
	if err == nil || !hostedValueFamily(err) {
		return err
	}
	return &state.RuntimeStoreError{Reason: "permission_denied", Cause: err}
}

// Errors of the Python ValueError, TypeError, KeyError and StopIteration families the admission check folds into a refusal
func hostedValueFamily(err error) bool {
	return errAs[*state.RuntimeStoreError](err) || errAs[*HostedRoomError](err) || errAs[*AttachmentError](err) ||
		errAs[*DriverValidationError](err) || errAs[*pyKeyError](err) || errAs[*pyjson.DecodeError](err) || errAs[*hostedAdmissionMismatch](err)
}

// A hosted admission whose durable binding no longer matches its producer, a ValueError
type hostedAdmissionMismatch struct{ msg string }

func (e *hostedAdmissionMismatch) Error() string { return e.msg }

func (c *CanonicalHostedRoomService) checkAdmission(ctx context.Context, ref SessionRef, row *pyjson.Object) error {
	requestID := rowString(row, "request_id")
	if !strings.HasPrefix(requestID, "hosted:") {
		return &hostedAdmissionMismatch{"not a hosted admission"}
	}
	identity, generation, err := hostedRequestIdentity(requestID)
	if err != nil {
		return err
	}
	if generation < 1 {
		return &hostedAdmissionMismatch{"invalid generation"}
	}
	owner, err := c.owner(identity.RoomID)
	if err != nil {
		return err
	}
	principal, _ := row.Get("principal_id")
	if !SameOwner(owner, principal) {
		return &hostedAdmissionMismatch{"foreign owner"}
	}
	room, err := c.room(identity.RoomID)
	if err != nil {
		return err
	}
	tasks, err := DriverListTasks(ctx, c.DBPath, identity.RoomID, nil)
	if err != nil {
		return err
	}
	var task map[string]any
	for _, candidate := range tasks {
		execution, _ := hostedStrictInt(candidate["execution_generation"])
		if hrTaskIdentity(candidate) == identity && execution == generation {
			task = candidate
			break
		}
	}
	if task == nil {
		return &hostedAdmissionMismatch{"no task for the hosted admission"}
	}
	gatewayID, epoch, err := hostedRoomAuthority(room)
	if err != nil {
		return err
	}
	rpc, err := c.self.ResolveMemberTransport(HostedRoomBinding{RoomID: identity.RoomID, GatewayID: gatewayID, AuthorityEpoch: epoch}, task)
	if err != nil {
		return err
	}
	var rpcRef SessionRef
	hasRef := false
	switch producer := rpc.(type) {
	case *HostedRoomAuthorityRPC:
		rpcRef, hasRef = producer.Ref, true
	case *HostedRoomOwnerRPC:
		rpcRef, hasRef = producer.currentRef()
	}
	if !hasRef || rpcRef != ref || !pyrepr.Equal(task["status"], "running") {
		return &hostedAdmissionMismatch{"changed hosted binding"}
	}
	producer, ok := rpc.(*HostedRoomAuthorityRPC)
	if !ok {
		return errors.New("'HostedRoomOwnerRPC' object has no attribute 'authority'")
	}
	payload, _ := hostedMapping(task["payload"])
	prompt, _ := payload["prompt"].(string)
	committed, err := CommittedSubmissionPayload(ctx, producer, prompt, payload["attachments"])
	if err != nil {
		return err
	}
	stored, _ := row.Get("payload")
	if !pyjson.Equal(stored, committed) {
		return &hostedAdmissionMismatch{"changed hosted binding"}
	}
	allowed, err := producer.Authorizer(ctx, "execute", &identity, &generation)
	if err != nil {
		return err
	}
	if !allowed {
		return &hostedAdmissionMismatch{"changed hosted binding"}
	}
	return nil
}

// Resolves an approval through the local producer that owns the session, refused when none does
func (c *CanonicalHostedRoomService) Approve(ctx context.Context, sessionID, requestID, choice string) (map[string]any, error) {
	c.memberMu.Lock()
	var producer InternalSessionRPC
	for _, rpc := range c.memberRPCs.Values() {
		var ref SessionRef
		switch r := rpc.(type) {
		case *HostedRoomAuthorityRPC:
			ref = r.Ref
		case *HostedRoomOwnerRPC:
			var known bool
			if ref, known = r.currentRef(); !known {
				c.memberMu.Unlock()
				return nil, errors.New("'HostedRoomOwnerRPC' object has no attribute 'ref'")
			}
		default:
			c.memberMu.Unlock()
			return nil, errors.New("'" + pyrepr.TypeName(rpc) + "' object has no attribute 'ref'")
		}
		if ref.SessionID == sessionID {
			producer = rpc
			break
		}
	}
	c.memberMu.Unlock()
	approver, ok := producer.(interface {
		Approve(ctx context.Context, sessionID, requestID, choice string) (map[string]any, error)
	})
	if producer == nil || !ok {
		return nil, runtimeRefusal("permission_denied")
	}
	return approver.Approve(ctx, sessionID, requestID, choice)
}

// Whether the private transport routes of this service are installed
func (c *CanonicalHostedRoomService) TransportInstalled() bool {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	return c.transportInstalled
}

// Records the private transport routes of this service as installed
func (c *CanonicalHostedRoomService) MarkTransportInstalled() {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	c.transportInstalled = true
}

// Marks the ready import begun, false when it began before
func (c *CanonicalHostedRoomService) BeginReadyImport() bool {
	c.flagsMu.Lock()
	defer c.flagsMu.Unlock()
	if c.readyImportStarted {
		return false
	}
	c.readyImportStarted = true
	return true
}

// Authority hosted service whose private transport the lifecycle installs once and starts behind readiness
type hostedLifecycleService interface {
	TransportInstalled() bool
	MarkTransportInstalled()
	Attest(ctx context.Context, selector map[string]any, operation string, params map[string]any) (map[string]any, error)
	Start() error
	BeginReadyImport() bool
	Wakeup()
}

// Runner surface the hosted service lifecycle reads
type hostedServiceRunner interface {
	SessionControlServer() *GatewayControlServer
	SessionRuntimeDescriptor() map[string]any
	Draining() bool
}

// Prepares every served transport before readiness releases a coordinator and returns the active service
func EnsureHostedService(ctx context.Context, runner any) (*CanonicalHostedRoomService, error) {
	active, err := activeAuthority(ctx, runner)
	if err != nil {
		return nil, err
	}
	if active == nil {
		return nil, runtimeRefusal("profile_mismatch")
	}
	for _, authority := range servingAuthorities(runner) {
		if err := ensureHostedService(ctx, runner, authority); err != nil {
			return nil, err
		}
	}
	if err := StartReadyHostedServices(runner); err != nil {
		return nil, err
	}
	service, _ := active.HostedRoomService.(*CanonicalHostedRoomService)
	return service, nil
}

// Builds the authority's service after the standalone import and installs its private transport once
func ensureHostedService(ctx context.Context, runner any, authority *SessionAuthority) error {
	scoped := ownerScope(ctx, authority, false)
	if authority.HostedRoomService == nil {
		epoch := authority.Epoch
		if _, err := ImportStandaloneRooms(scoped, authority.DB.Path(), &epoch); err != nil {
			return err
		}
		created, err := NewCanonicalHostedRoomService(scoped, authority, ctx)
		if err != nil {
			return err
		}
		authority.HostedRoomService = created
	}
	service, ok := authority.HostedRoomService.(hostedLifecycleService)
	if !ok {
		return errors.New("'" + pyrepr.TypeName(authority.HostedRoomService) + "' object has no attribute 'attest'")
	}
	if service.TransportInstalled() {
		return nil
	}
	server, ok := runner.(hostedServiceRunner)
	if !ok || server.SessionControlServer() == nil {
		return errors.New("'NoneType' object has no attribute 'private_handlers'")
	}
	InstallHostedTransport(server.SessionControlServer(), authority, ctx, service.Attest)
	service.MarkTransportInstalled()
	return nil
}

// Starts a fully prepared served set behind the published ready gate, then imports once more
func StartReadyHostedServices(runner any) error {
	lifecycle, ok := runner.(hostedServiceRunner)
	if !ok || !pyrepr.Equal(lifecycle.SessionRuntimeDescriptor()["state"], "ready") || lifecycle.Draining() {
		return nil
	}
	authorities := servingAuthorities(runner)
	services := make([]hostedLifecycleService, 0, len(authorities))
	for _, authority := range authorities {
		service, ok := authority.HostedRoomService.(hostedLifecycleService)
		if !ok || !service.TransportInstalled() {
			return nil
		}
		services = append(services, service)
	}
	for _, service := range services {
		if err := service.Start(); err != nil {
			return err
		}
	}
	ImportAfterReady(authorities)
	return nil
}

// Imports standalone rooms once more after the gateway published ready, off the owner loop, waking claimed rooms
func ImportAfterReady(authorities []*SessionAuthority) {
	for _, authority := range authorities {
		service, ok := authority.HostedRoomService.(hostedLifecycleService)
		if !ok || !service.BeginReadyImport() {
			continue
		}
		target, epoch := authority.DB.Path(), authority.Epoch
		go func(authority *SessionAuthority, service hostedLifecycleService) {
			imported, err := ImportStandaloneRooms(ownerScope(context.Background(), authority, false), target, &epoch)
			if err != nil {
				slog.Default().With("logger", "threading").Error("Exception in thread hosted-rooms-ready-import", "exc_info", err)
				return
			}
			if imported > 0 {
				service.Wakeup()
			}
		}(authority, service)
	}
}
