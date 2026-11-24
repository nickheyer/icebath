package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var hptBinding = HostedRoomBinding{RoomID: "room-1", GatewayID: "gateway-home", AuthorityEpoch: 2}

var hptRoute = PeerMemberRoute{HomeInstallID: "install-home", MemberID: "member-reviewer", TargetInstallID: "install-peer",
	TargetProfile: "reviewer", CapabilityDigest: strings.Repeat("a", 64), ExecutionPolicyDigest: strings.Repeat("b", 64),
	CancellationScopeID: "cancel-1", TraceID: "trace-1", Grant: "signed-room-grant"}

type hptCall struct {
	method string
	params map[string]any
}

// Peer client that records every call and answers like an accepting peer
type hptFakePeerClient struct {
	calls   []hptCall
	session map[string]any
	active  bool
	taskID  any
}

func newHptFakePeerClient() *hptFakePeerClient {
	return &hptFakePeerClient{session: map[string]any{"session_id": "group-session"}}
}

func (c *hptFakePeerClient) Prepare(_ context.Context, req PeerPrepare) (map[string]any, error) {
	params := map[string]any{"room_id": req.RoomID, "profile": req.Profile, "source": req.Source, "grant": req.Grant, "create": req.Create}
	if req.ExpectedSessionID != nil {
		params["expected_session_id"] = *req.ExpectedSessionID
	}
	c.calls = append(c.calls, hptCall{"prepare", params})
	if req.Create || (req.ExpectedSessionID != nil && *req.ExpectedSessionID != "") {
		return c.session, nil
	}
	return nil, nil
}

func (c *hptFakePeerClient) Dispatch(_ context.Context, dispatch *jsonutil.Object, grant string) (map[string]any, error) {
	c.calls = append(c.calls, hptCall{"dispatch", map[string]any{"dispatch": dispatch, "grant": grant}})
	c.active = true
	c.taskID, _ = dispatch.Get("task_id")
	return map[string]any{"status": "accepted", "task_id": c.taskID}, nil
}

func (c *hptFakePeerClient) History(_ context.Context, roomID, profile, sessionID, grant string) ([]map[string]any, error) {
	c.calls = append(c.calls, hptCall{"history", map[string]any{"room_id": roomID, "profile": profile, "session_id": sessionID}})
	return []map[string]any{}, nil
}

func (c *hptFakePeerClient) Status(_ context.Context, roomID, profile, sessionID, grant string) (map[string]any, error) {
	c.calls = append(c.calls, hptCall{"status", map[string]any{"room_id": roomID, "profile": profile, "session_id": sessionID}})
	return map[string]any{"active": c.active, "task_id": c.taskID}, nil
}

func (c *hptFakePeerClient) Stop(_ context.Context, dispatch *jsonutil.Object, grant string) (map[string]any, error) {
	c.calls = append(c.calls, hptCall{"stop", map[string]any{"dispatch": dispatch, "grant": grant}})
	c.active = false
	return map[string]any{"status": "cancelled", "task_id": c.taskID}, nil
}

func (c *hptFakePeerClient) callsOf(method string) []map[string]any {
	var out []map[string]any
	for _, call := range c.calls {
		if call.method == method {
			out = append(out, call.params)
		}
	}
	return out
}

func hptTransport(t *testing.T, client HostedRoomPeerClient, sourceEventSeq int64) *PeerHostedRoomTransport {
	t.Helper()
	transport, err := NewPeerHostedRoomTransport(hptBinding, hptRoute, client, sourceEventSeq, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

func hptSubmit(transport *PeerHostedRoomTransport, prompt string, task TaskIdentity, generation int64, onTerminal func(map[string]any) error, member *string) (map[string]any, error) {
	return transport.Submit(context.Background(), HostedSubmit{Profile: "reviewer", SessionID: "group-session", Prompt: prompt,
		Source: RoomSessionSource, Task: task, ExecutionGeneration: generation, OnTerminal: onTerminal, MemberID: member})
}

func TestPeerTransportPreparesGroupSessionNotCanonicalBotChat(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	client := newHptFakePeerClient()
	transport := hptTransport(t, client, 1)
	resolved, err := transport.ResolveExact(ctx, "reviewer", "Group: room-1", RoomSessionSource)
	if err != nil || resolved != nil {
		t.Fatalf("resolve_exact = %v, %v", resolved, err)
	}
	created, err := transport.Create(ctx, "reviewer", "Group: room-1", RoomSessionSource)
	if err != nil || created["session_id"] != "group-session" {
		t.Fatalf("create = %v, %v", created, err)
	}
	for _, params := range client.callsOf("prepare") {
		if params["room_id"] != "room-1" || params["source"] != "bot_room" {
			t.Fatalf("prepare params = %v", params)
		}
	}
}

func TestPeerTransportDispatchesFullFencedCoordinatesAndExactStop(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	client := newHptFakePeerClient()
	transport := hptTransport(t, client, 1)
	if _, err := transport.Create(ctx, "reviewer", "Group: room-1", RoomSessionSource); err != nil {
		t.Fatal(err)
	}
	var terminal []map[string]any
	result, err := hptSubmit(transport, "Review this change.", TaskIdentity{"room-1", "task-1", "thread-1", "turn-1"}, 3,
		func(receipt map[string]any) error { terminal = append(terminal, receipt); return nil }, nil)
	if err != nil || result["status"] != "accepted" {
		t.Fatalf("submit = %v, %v", result, err)
	}
	dispatch := client.callsOf("dispatch")[0]["dispatch"].(*jsonutil.Object)
	for key, want := range map[string]any{"authority_epoch": int64(2), "execution_generation": int64(3), "target_profile": "reviewer",
		"capability_digest": strings.Repeat("a", 64)} {
		if got, _ := dispatch.Get(key); !anyutil.Equal(got, want) {
			t.Fatalf("dispatch %s = %v", key, got)
		}
	}
	if len(terminal) != 0 {
		t.Fatalf("terminal = %v", terminal)
	}
	other, err := transport.Interrupt(ctx, "reviewer", "group-session", RoomSessionSource, "other-task")
	if err != nil || other != nil {
		t.Fatalf("interrupt of another task = %v, %v", other, err)
	}
	stopped, err := transport.Interrupt(ctx, "reviewer", "group-session", RoomSessionSource, "task-1")
	if err != nil || stopped["status"] != "cancelled" {
		t.Fatalf("interrupt = %v, %v", stopped, err)
	}
	if stops := client.callsOf("stop"); len(stops) != 1 {
		t.Fatalf("stop calls = %d", len(stops))
	}
}

func TestPeerTransportRefusesASubmitForAnotherMember(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := newHptFakePeerClient()
	transport := hptTransport(t, client, 1)
	if _, err := transport.Create(context.Background(), "reviewer", "Group: room-1", RoomSessionSource); err != nil {
		t.Fatal(err)
	}
	task := TaskIdentity{"room-1", "task-1", "thread-1", "turn-1"}
	noop := func(map[string]any) error { return nil }
	other := "member-other"
	_, err := hptSubmit(transport, "Review this change.", task, 1, noop, &other)
	var refusal *RoomValueError
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Msg, "member does not match") {
		t.Fatalf("submit for another member = %v", err)
	}
	if len(client.callsOf("dispatch")) != 0 {
		t.Fatal("a dispatch reached the peer")
	}
	own := "member-reviewer"
	result, err := hptSubmit(transport, "Review this change.", task, 1, noop, &own)
	if err != nil || result["status"] != "accepted" {
		t.Fatalf("submit for the member = %v, %v", result, err)
	}
}

func TestPeerTransportCarriesEachTurnsRealSourceEventSequence(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var observed []any
	for index, seq := range []int64{7, 42} {
		client := newHptFakePeerClient()
		transport := hptTransport(t, client, seq)
		if _, err := transport.Create(context.Background(), "reviewer", "Group: room-1", RoomSessionSource); err != nil {
			t.Fatal(err)
		}
		turn := index + 1
		if _, err := hptSubmit(transport, "Turn "+string(rune('0'+turn)), TaskIdentity{"room-1", "task-" + string(rune('0'+turn)), "thread-1",
			"turn-" + string(rune('0'+turn))}, 1, func(map[string]any) error { return nil }, nil); err != nil {
			t.Fatal(err)
		}
		value, _ := client.callsOf("dispatch")[0]["dispatch"].(*jsonutil.Object).Get("source_event_seq")
		observed = append(observed, value)
	}
	if !anyutil.Equal(observed, []any{int64(7), int64(42)}) {
		t.Fatalf("source_event_seq = %v", observed)
	}
}

func TestPeerTransportRejectsProfileSourceAndRoomTitleMismatch(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	transport := hptTransport(t, newHptFakePeerClient(), 1)
	for _, args := range [][3]string{{"other", "Group: room-1", "bot_room"}, {"reviewer", "Bot Chat", "bot_room"}, {"reviewer", "Group: room-1", "cli"}} {
		_, err := transport.ResolveExact(context.Background(), args[0], args[1], args[2])
		var refusal *RoomValueError
		if !errors.As(err, &refusal) {
			t.Fatalf("mismatch was accepted: %v (%v)", args, err)
		}
	}
}
