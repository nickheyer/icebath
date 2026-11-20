package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// API adapter double answering the store, routes and run aliases the admission reads
type apiTurnAdapterDouble struct {
	*BasePlatformAdapter
	db      *state.SessionDB
	routes  map[string]map[string]any
	aliases map[string]string
}

func (d *apiTurnAdapterDouble) Connect(context.Context, bool) (bool, error) { return true, nil }
func (d *apiTurnAdapterDouble) Disconnect(context.Context) error            { return nil }
func (d *apiTurnAdapterDouble) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": "API Server", "type": "api", "chat_id": chatID}, nil
}
func (d *apiTurnAdapterDouble) Send(context.Context, string, string, string, map[string]any) (SendResult, error) {
	return SendResult{Success: false, Error: "API server uses HTTP request/response, not send()"}, nil
}
func (d *apiTurnAdapterDouble) EnsureSessionDB(context.Context) (*state.SessionDB, error) {
	return d.db, nil
}
func (d *apiTurnAdapterDouble) ConfiguredModelRoute(alias any) map[string]any {
	name, _ := alias.(string)
	return d.routes[name]
}
func (d *apiTurnAdapterDouble) BindRunAdmissionAlias(key, admissionID string) {
	d.aliases[key] = admissionID
}
func (d *apiTurnAdapterDouble) ReleaseRunAdmissionAlias(key, admissionID string) {
	if d.aliases[key] == admissionID {
		delete(d.aliases, key)
	}
}

// Runner double of the API admission cases, holding the canonical authority and resolving every source to the API adapter
type apiOwnerRunner struct {
	authorityRunnerDouble
	authority *SessionAuthority
	handle    func(ctx context.Context, event *MessageEvent) (any, error)
}

func (r *apiOwnerRunner) SessionAuthority() *SessionAuthority { return r.authority }

func (r *apiOwnerRunner) HandleMessage(ctx context.Context, event *MessageEvent) (any, error) {
	return r.handle(ctx, event)
}

// Canonical authority over a fresh profile store with the API adapter bound to its runner and database
func newAPITurnOwner(t *testing.T) (context.Context, *SessionAuthority, *apiTurnAdapterDouble) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	runner := &apiOwnerRunner{}
	runner.store = NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), &GatewayConfig{}, nil)
	runner.store.PinDB(db)
	epoch, err := db.BeginRuntimeEpoch(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	authority := NewSessionAuthority(runner, "default", "first", db, epoch)
	runner.authority = authority
	adapter := &apiTurnAdapterDouble{db: db, routes: map[string]map[string]any{}, aliases: map[string]string{}}
	adapter.BasePlatformAdapter = NewBasePlatformAdapter(&PlatformConfig{Enabled: true}, PlatformAPIServer, adapter)
	adapter.GatewayRunner = runner
	runner.adapter = adapter
	return ctx, authority, adapter
}

// Policy the reviewer profile advertises for the room policy cases
func apiRoomPolicy(t *testing.T) *pyjson.Object {
	t.Helper()
	return roomTestPolicy(t, "manual", 12)
}

// Asserts err is the runtime refusal with reason
func requireRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) || refusal.Reason != reason {
		t.Fatalf("err = %v, want refusal %s", err, reason)
	}
}

func TestAdmissionWithAPolicyButNoDispatchIsRefused(t *testing.T) {
	ctx, owner, api := newAPITurnOwner(t)
	_, err := AdmitAPITurn(ctx, api, &APITurn{SessionID: "policy-only", UserMessage: "hello", ConversationHistory: []any{},
		Settings: map[string]any{"room_execution_policy": apiRoomPolicy(t)}})
	requireRefusal(t, err, "permission_denied")
	rows, err := owner.DB.ListSessionAdmissions(ctx, "policy-only", true)
	if err != nil || len(rows) != 0 {
		t.Fatalf("admissions = %v, %v", rows, err)
	}
}

func TestQueuedPolicyOnlyAdmissionFromAnOlderBuildIsNotRecovered(t *testing.T) {
	ctx, owner, api := newAPITurnOwner(t)
	admitted, err := AdmitAPITurn(ctx, api, &APITurn{SessionID: "legacy-policy", RequestID: "queued-turn", UserMessage: "hello",
		ConversationHistory: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	stored := pyjson.Clone(admitted.Row.GetOr("payload", nil)).(*pyjson.Object)
	turn := stored.GetOr("api_turn_v1", nil).(*pyjson.Object)
	turn.GetOr("settings", nil).(*pyjson.Object).Set("room_execution_policy", apiRoomPolicy(t))
	encoded, err := pyjson.Dumps(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE session_admissions SET payload_json=? WHERE admission_id=?", encoded,
			admitted.Row.GetOr("admission_id", nil))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owner.mu.Lock()
	clear(owner.Sessions)
	owner.mu.Unlock()
	var scheduled []string
	saved := scheduleSession
	scheduleSession = func(_ context.Context, _ *SessionAuthority, ref SessionRef) error {
		scheduled = append(scheduled, ref.SessionID)
		return nil
	}
	t.Cleanup(func() { scheduleSession = saved })
	if err := RecoverAPITurns(ctx, api); err != nil {
		t.Fatal(err)
	}
	if len(scheduled) != 0 {
		t.Fatalf("scheduled = %v, want none", scheduled)
	}
	_, err = checkAPITurn(ctx, owner, admitted.Ref, stored)
	requireRefusal(t, err, "permission_denied")
}

func TestDispatchWithItsCatalogPolicyIsAdmittedAndBound(t *testing.T) {
	ctx, owner, api := newAPITurnOwner(t)
	install, err := LocalAuthorityGatewayID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy, catalogMap, err := localRoomCatalog(ctx, api, "reviewer", install)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := GatewayRoomCatalogFromMapping(catalogMap)
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Review the user's patch."
	sum := sha256.Sum256([]byte(prompt))
	dispatch, err := HostedMemberDispatchFromMapping(map[string]any{
		"protocol_version": int64(2), "room_id": "room-1", "home_install_id": "install-home",
		"authority_gateway_id": "gateway-home", "authority_epoch": int64(1), "member_id": "member-reviewer",
		"target_install_id": install, "target_profile": "reviewer", "task_id": "task-1",
		"execution_generation": int64(1), "source_event_seq": int64(1), "cancellation_scope_id": "cancel-1",
		"prompt": prompt, "prompt_digest": hex.EncodeToString(sum[:]), "capability_digest": catalog.CatalogDigest,
		"execution_policy_digest": policy.GetOr("policy_digest", nil), "trace_id": "trace-1"})
	if err != nil {
		t.Fatal(err)
	}
	mapping := dispatch.AsMapping()
	seed := strings.Join([]string{dispatch.HomeInstallID, dispatch.RoomID, dispatch.MemberID, dispatch.TargetProfile}, "\x00")
	digest := sha256.Sum256([]byte(seed))
	sid := "room_" + hex.EncodeToString(digest[:])[:32]
	admitted, err := AdmitAPITurn(ctx, api, &APITurn{SessionID: sid, UserMessage: prompt, ConversationHistory: []any{},
		Settings: map[string]any{"room_dispatch": mapping, "room_execution_policy": policy}})
	if err != nil {
		t.Fatal(err)
	}
	if rowString(admitted.Row, "status") != AdmissionQueued {
		t.Fatalf("status = %v", admitted.Row.GetOr("status", nil))
	}
	payload := admitted.Row.GetOr("payload", nil).(*pyjson.Object)
	if _, err := checkAPITurn(ctx, owner, admitted.Ref, payload); err != nil {
		t.Fatal(err)
	}
	settings := payload.GetOr("api_turn_v1", nil).(*pyjson.Object).GetOr("settings", nil)
	scoped, err := APIPolicyScope(BindAPIExecution(ctx, pyjson.ObjectOf("settings", settings)))
	if err != nil {
		t.Fatal(err)
	}
	bound := CurrentRoomExecutionPolicy(scoped)
	if bound == nil || !pyjson.Equal(bound.AsMapping(), policy) {
		t.Fatalf("bound policy = %v, want %v", bound, policy)
	}
	if CurrentRoomExecutionPolicy(ctx) != nil {
		t.Fatal("policy leaked outside the admission scope")
	}
}

func TestRequestRelayMetadataReachesTheExecutingTurn(t *testing.T) {
	ctx, owner, api := newAPITurnOwner(t)
	metadata := pyjson.ObjectOf("relay.trace", "trace-7", "tenant", "acme")
	admitted, err := AdmitAPITurn(ctx, api, &APITurn{SessionID: "relay-meta", UserMessage: "hello", ConversationHistory: []any{},
		RelayMetadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	var seen any
	owner.Runner.(*apiOwnerRunner).handle = func(ctx context.Context, _ *MessageEvent) (any, error) {
		seen = APIExecution(ctx).GetOr("relay_metadata", nil)
		executionResultOf(ctx).Set("result", pyjson.ObjectOf("final_response", "ok"))
		return "ok", nil
	}
	if _, err := executeAdmission(ctx, owner, admitted.Ref, admitted.Row); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(seen, metadata) {
		t.Fatalf("relay_metadata = %v, want %v", seen, metadata)
	}
}
