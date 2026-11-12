package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Install root with an ops profile carrying its identity marker, set as the process home
func relayHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	ops := filepath.Join(h, "profiles", "ops")
	mkdirAll(t, ops)
	if err := os.WriteFile(filepath.Join(ops, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

// Result of a relay door response, failing the case on an error
func relayResult(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if _, failed := response["error"]; failed {
		t.Fatalf("relay error %v", response)
	}
	return pyjson.Plain(response["result"]).(map[string]any)
}

// Calls a relay door the way server._methods does
func relayCall(ctx context.Context, method string, rid any, params map[string]any) map[string]any {
	return lookupMethod(method)(ctx, rid, params)
}

// Target authority double recording each forwarded delivery
type relayForward struct {
	home   string
	params map[string]any
}

func swapRelayDeliver(t *testing.T, fn func(ctx context.Context, home string, params map[string]any) (map[string]any, error)) {
	t.Helper()
	saved := relayDeliver
	relayDeliver = fn
	t.Cleanup(func() { relayDeliver = saved })
}

func relayTarget() botmode.RemoteRosterRow {
	return botmode.RemoteRosterRow{Profile: "scout", Handle: "scout", ConnectionID: "cloud-1"}
}

func relayEnvelopeIDs(t *testing.T, result map[string]any) []string {
	t.Helper()
	ids := []string{}
	for _, envelope := range result["envelopes"].([]any) {
		ids = append(ids, envelope.(map[string]any)["id"].(string))
	}
	return ids
}

func relayErrorData(response map[string]any) map[string]any {
	failure, _ := response["error"].(map[string]any)
	data, _ := failure["data"].(map[string]any)
	return data
}

// test_bot_relay_methods.py test_roster_sync_persists_and_counts
func TestRosterSyncPersistsAndCounts(t *testing.T) {
	h := relayHome(t)
	out := relayResult(t, relayCall(context.Background(), "bot_relay.roster.sync", 1, map[string]any{"agents": []any{
		map[string]any{"profile": "scout", "handle": "scout", "connection_id": "cloud-1"},
		map[string]any{"profile": "", "connection_id": "cloud-1"},
	}}))
	if out["count"] != 1 {
		t.Fatalf("count %v", out)
	}
	rows := botmode.ReadRemoteRoster(h)
	if len(rows) != 1 || rows[0].Profile != "scout" {
		t.Fatalf("roster %v", rows)
	}
}

// test_bot_relay_methods.py test_outbox_drain_replays_canonical_envelope_until_reply_acknowledged
func TestOutboxDrainReplaysCanonicalEnvelopeUntilReplyAcknowledged(t *testing.T) {
	h := relayHome(t)
	ctx := context.Background()
	env, err := botmode.EnqueueEnvelope(ctx, h, relayTarget(), "m", "default", "icebath")
	if err != nil {
		t.Fatal(err)
	}
	id := env.GetOr("id", nil).(string)
	first := relayResult(t, relayCall(ctx, "bot_relay.outbox.drain", 1, map[string]any{}))
	if ids := relayEnvelopeIDs(t, first); !reflect.DeepEqual(ids, []string{id}) {
		t.Fatalf("first drain %v", ids)
	}
	second := relayResult(t, relayCall(ctx, "bot_relay.outbox.drain", 2, map[string]any{}))
	if ids := relayEnvelopeIDs(t, second); !reflect.DeepEqual(ids, []string{id}) {
		t.Fatalf("second drain %v", ids)
	}
	if second["envelopes"].([]any)[0].(map[string]any)["message"] != "m" {
		t.Fatalf("message %v", second)
	}
	relayResult(t, relayCall(ctx, "bot_relay.reply", 3, map[string]any{"id": id, "reply": "done"}))
	if ids := relayEnvelopeIDs(t, relayResult(t, relayCall(ctx, "bot_relay.outbox.drain", 4, map[string]any{}))); len(ids) != 0 {
		t.Fatalf("answered envelope drained again %v", ids)
	}
}

// test_bot_relay_methods.py test_outbox_drain_settles_a_claimed_envelope_nobody_answered_with_a_typed_timeout
func TestOutboxDrainSettlesAClaimedEnvelopeNobodyAnsweredWithATypedTimeout(t *testing.T) {
	h := relayHome(t)
	ctx := context.Background()
	lost, err := botmode.EnqueueEnvelope(ctx, h, relayTarget(), "lost", "w", "w")
	if err != nil {
		t.Fatal(err)
	}
	done, err := botmode.EnqueueEnvelope(ctx, h, relayTarget(), "done", "w", "w")
	if err != nil {
		t.Fatal(err)
	}
	lostID, doneID := lost.GetOr("id", nil).(string), done.GetOr("id", nil).(string)
	ids := relayEnvelopeIDs(t, relayResult(t, relayCall(ctx, "bot_relay.outbox.drain", 1, map[string]any{})))
	want := []string{lostID, doneID}
	sort.Strings(ids)
	sort.Strings(want)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("first drain %v", ids)
	}
	if _, err := botmode.WriteReply(ctx, h, doneID, "answered", "", ""); err != nil {
		t.Fatal(err)
	}
	if ids := relayEnvelopeIDs(t, relayResult(t, relayCall(ctx, "bot_relay.outbox.drain", 2, map[string]any{}))); !reflect.DeepEqual(ids, []string{lostID}) {
		t.Fatalf("second drain %v", ids)
	}
	lostPath := filepath.Join(botmode.RelayRoot(h), botmode.ClaimedDir, lostID+".json")
	raw, err := os.ReadFile(lostPath)
	if err != nil {
		t.Fatal(err)
	}
	var stale map[string]any
	if err := json.Unmarshal(raw, &stale); err != nil {
		t.Fatal(err)
	}
	stale["created_at"] = time.Now().Unix() - botmode.ReplyWaitSeconds - 1
	raw, _ = json.Marshal(stale)
	if err := os.WriteFile(lostPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if ids := relayEnvelopeIDs(t, relayResult(t, relayCall(ctx, "bot_relay.outbox.drain", 3, map[string]any{}))); len(ids) != 0 {
		t.Fatalf("stale drain %v", ids)
	}
	raw, err = os.ReadFile(filepath.Join(botmode.RelayRoot(h), botmode.RepliesDir, lostID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["reason"] != "delivery_timeout" || reply["error"] == "" {
		t.Fatalf("timeout reply %v", reply)
	}
	if ids := relayEnvelopeIDs(t, relayResult(t, relayCall(ctx, "bot_relay.outbox.drain", 4, map[string]any{}))); len(ids) != 0 {
		t.Fatalf("settled envelope re-offered %v", ids)
	}
}

// test_bot_relay_methods.py test_deliver_forwards_stable_id_to_target_profile_authority
func TestDeliverForwardsStableIdToTargetProfileAuthority(t *testing.T) {
	h := relayHome(t)
	var forwarded []relayForward
	swapRelayDeliver(t, func(_ context.Context, target string, params map[string]any) (map[string]any, error) {
		forwarded = append(forwarded, relayForward{target, params})
		return map[string]any{"status": "queued", "delivery_id": params["id"], "reply": ""}, nil
	})
	ctx := context.Background()
	envelopeID := strings.Repeat("b", 32)
	out := relayResult(t, relayCall(ctx, "bot_relay.deliver", 1, map[string]any{"id": envelopeID, "profile": "ops", "message": "ping"}))
	if out["status"] != "queued" || out["delivery_id"] != envelopeID {
		t.Fatalf("out %v", out)
	}
	last := forwarded[len(forwarded)-1]
	if last.home != filepath.Join(h, "profiles", "ops") || last.params["id"] != envelopeID || last.params["profile"] != "ops" {
		t.Fatalf("forwarded %v", last)
	}
	relayResult(t, relayCall(ctx, "bot_relay.deliver", 2, map[string]any{"id": envelopeID, "profile": "ops", "message": "ping"}))
	if len(forwarded) != 2 || forwarded[0].params["id"] != envelopeID || forwarded[1].params["id"] != envelopeID {
		t.Fatalf("retry %v", forwarded)
	}
	relayResult(t, relayCall(ctx, "bot_relay.deliver", 3, map[string]any{"id": strings.Repeat("c", 32), "profile": "icebath", "message": "x"}))
	last = forwarded[len(forwarded)-1]
	if last.home != h || last.params["profile"] != "default" {
		t.Fatalf("default alias %v", last)
	}
}

// test_bot_relay_methods.py test_deliver_unreachable_authority_is_a_typed_refusal
func TestDeliverUnreachableAuthorityIsATypedRefusal(t *testing.T) {
	h := relayHome(t)
	swapRelayDeliver(t, func(context.Context, string, map[string]any) (map[string]any, error) {
		return nil, errors.New("profile authority is not ready")
	})
	ctx := context.Background()
	refused := relayCall(ctx, "bot_relay.deliver", 1, map[string]any{"id": strings.Repeat("d", 32), "profile": "ops", "message": "x"})
	if relayErrorData(refused)["reason"] != "runtime_unavailable" || !strings.Contains(refused["error"].(map[string]any)["message"].(string), "not ready") {
		t.Fatalf("refused %v", refused)
	}
	mkdirAll(t, filepath.Join(h, "profiles", "sessions"))
	for _, ghost := range []string{"ghost", "sessions"} {
		refused := relayCall(ctx, "bot_relay.deliver", 2, map[string]any{"id": strings.Repeat("e", 32), "profile": ghost, "message": "x"})
		if relayErrorData(refused)["reason"] != "unknown_profile" {
			t.Fatalf("%s %v", ghost, refused)
		}
	}
}

// test_bot_relay_methods.py test_deliver_requires_stable_id_and_valid_profile
func TestDeliverRequiresStableIdAndValidProfile(t *testing.T) {
	for i, params := range []map[string]any{
		{"profile": "", "message": ""},
		{"profile": "ops", "message": "no envelope id"},
		{"id": "../evil", "profile": "ops", "message": "x"},
		{"id": strings.Repeat("e", 32), "profile": "../ops", "message": "x"},
	} {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			relayHome(t)
			swapRelayDeliver(t, func(context.Context, string, map[string]any) (map[string]any, error) {
				t.Fatal("malformed requests never reach an authority")
				return nil, nil
			})
			if reason := relayErrorData(relayCall(context.Background(), "bot_relay.deliver", 1, params))["reason"]; reason != "invalid_params" {
				t.Fatalf("reason %v", reason)
			}
		})
	}
}

// test_bot_relay_methods.py test_reply_roundtrip_and_id_validation
func TestReplyRoundtripAndIdValidation(t *testing.T) {
	h := relayHome(t)
	ctx := context.Background()
	envelopeID := strings.Repeat("c", 32)
	relayResult(t, relayCall(ctx, "bot_relay.reply", 1, map[string]any{"id": envelopeID, "reply": "hi"}))
	raw, err := os.ReadFile(filepath.Join(botmode.RelayRoot(h), botmode.RepliesDir, envelopeID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil || reply["reply"] != "hi" {
		t.Fatalf("reply %v %v", reply, err)
	}
	if _, failed := relayCall(ctx, "bot_relay.reply", 2, map[string]any{"id": "../evil"})["error"]; !failed {
		t.Fatal("malformed envelope id accepted")
	}
}

// Calling transport carrying an auth identity
type relayClient struct{ identity map[string]any }

func (c *relayClient) Write(*pyjson.Object) bool    { return true }
func (c *relayClient) Close()                       {}
func (c *relayClient) AuthIdentity() map[string]any { return c.identity }
func relaySenderAuthor() map[string]any {
	return map[string]any{"id": "bot:cloud-1/scout", "name": "scout", "is_bot": true}
}
func relaySender() map[string]any {
	return map[string]any{"from_profile": "scout", "from_handle": "scout", "from_connection": "cloud-1"}
}

// test_bot_relay_methods.py test_deliver_accepts_a_sender_from_an_admitted_non_login_client
func TestDeliverAcceptsASenderFromAnAdmittedNonLoginClient(t *testing.T) {
	for name, identity := range map[string]map[string]any{"no identity": nil,
		"server-internal identity": {"user_id": "server-internal", "provider": "server-internal"}} {
		t.Run(name, func(t *testing.T) {
			relayHome(t)
			var forwarded []map[string]any
			swapRelayDeliver(t, func(_ context.Context, _ string, params map[string]any) (map[string]any, error) {
				forwarded = append(forwarded, params)
				return map[string]any{"status": "queued"}, nil
			})
			ctx := tuigateway.BindTransport(context.Background(), &relayClient{identity: identity})
			params := map[string]any{"id": strings.Repeat("f", 32), "profile": "ops", "message": "ping"}
			for key, value := range relaySender() {
				params[key] = value
			}
			if out := relayResult(t, relayCall(ctx, "bot_relay.deliver", 1, params)); out["status"] != "queued" {
				t.Fatalf("out %v", out)
			}
			if !reflect.DeepEqual(forwarded[0]["author"], relaySenderAuthor()) {
				t.Fatalf("author %v", forwarded[0]["author"])
			}
			for key := range relaySender() {
				if _, present := forwarded[0][key]; present {
					t.Fatalf("sender field %s forwarded", key)
				}
			}
		})
	}
}

// test_bot_relay_methods.py test_deliver_from_a_logged_in_client_is_attributed_to_its_principal_never_to_the_claimed_sender
func TestDeliverFromALoggedInClientIsAttributedToItsPrincipalNeverToTheClaimedSender(t *testing.T) {
	relayHome(t)
	var forwarded []map[string]any
	swapRelayDeliver(t, func(_ context.Context, _ string, params map[string]any) (map[string]any, error) {
		forwarded = append(forwarded, params)
		return map[string]any{"status": "queued"}, nil
	})
	client := &relayClient{identity: map[string]any{"user_id": "alice", "provider": "google"}}
	ctx := tuigateway.BindTransport(context.Background(), client)
	shapes := []map[string]any{{"from_profile": "scout"}, {"from_connection": "cloud-1"}, relaySender(), {}}
	for rid, sender := range shapes {
		params := map[string]any{"id": strings.Repeat("f", 32), "profile": "ops", "message": "ping"}
		for key, value := range sender {
			params[key] = value
		}
		relayResult(t, relayCall(ctx, "bot_relay.deliver", rid, params))
	}
	if len(forwarded) != len(shapes) {
		t.Fatalf("admitted %d of %d", len(forwarded), len(shapes))
	}
	ids := map[any]bool{}
	for _, params := range forwarded {
		author := params["author"].(map[string]any)
		id := author["id"].(string)
		if author["is_bot"] != true || !strings.HasPrefix(id, "bot:principal:dashboard:") || !strings.HasSuffix(id, "/relay") || author["name"] != "relayed teammate" {
			t.Fatalf("author %v", author)
		}
		if reflect.DeepEqual(author, relaySenderAuthor()) {
			t.Fatal("the claimed sender became the author")
		}
		for key := range relaySender() {
			if _, present := params[key]; present {
				t.Fatalf("sender field %s forwarded", key)
			}
		}
		ids[id] = true
	}
	if len(ids) != 1 {
		t.Fatalf("authors %v", ids)
	}
	client.identity = map[string]any{"user_id": "bob", "provider": "google"}
	params := map[string]any{"id": strings.Repeat("f", 32), "profile": "ops", "message": "ping"}
	for key, value := range relaySender() {
		params[key] = value
	}
	relayResult(t, relayCall(ctx, "bot_relay.deliver", 9, params))
	if ids[forwarded[len(forwarded)-1]["author"].(map[string]any)["id"]] {
		t.Fatal("a different principal got the same author")
	}
}

// test_bot_relay_methods.py test_deliver_restamps_relayed_sender_with_a_reply_safe_handle
func TestDeliverRestampsRelayedSenderWithAReplySafeHandle(t *testing.T) {
	h := relayHome(t)
	var seen []any
	swapRelayDeliver(t, func(_ context.Context, _ string, params map[string]any) (map[string]any, error) {
		seen = append(seen, params["message"])
		return map[string]any{"status": "queued"}, nil
	})
	ctx := context.Background()
	if _, err := botmode.WriteRemoteRoster(ctx, h, []any{map[string]any{"profile": "default", "handle": "icebath", "connection_id": "vps-1", "title": "CoS Bot"}}); err != nil {
		t.Fatal(err)
	}
	stamp := "Message from \U0001F916 CoS Bot (@icebath): are we done?"
	sender := map[string]any{"from_profile": "default", "from_handle": "icebath", "from_connection": "vps-1"}
	deliver := func(rid int, id, message string, overrides map[string]any) {
		params := map[string]any{"id": strings.Repeat(id, 32), "profile": "ops", "message": message}
		for key, value := range sender {
			params[key] = value
		}
		for key, value := range overrides {
			params[key] = value
		}
		relayResult(t, relayCall(ctx, "bot_relay.deliver", rid, params))
	}
	deliver(1, "a", stamp, nil)
	deliver(2, "b", stamp, map[string]any{"from_connection": "lan-2"})
	deliver(3, "c", "plain text (@icebath): x", nil)
	want := []any{"Message from \U0001F916 CoS Bot (@cos-bot): are we done?", "Message from \U0001F916 CoS Bot (@icebath@lan-2): are we done?",
		"plain text (@icebath): x"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen %q", seen)
	}
}

// test_bot_relay_methods.py test_gateway_drains_the_mailbox_the_tools_write_to
func TestGatewayDrainsTheMailboxTheToolsWriteTo(t *testing.T) {
	for _, subdir := range []string{"profiles/ops", "dev"} {
		t.Run(subdir, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			h := filepath.Join(tmp, ".icebath", subdir)
			mkdirAll(t, h)
			t.Setenv("ICEBATH_HOME", h)
			writerRoot := botmode.InstallRoot(h)
			env, err := botmode.EnqueueEnvelope(context.Background(), writerRoot, relayTarget(), "m", "default", "icebath")
			if err != nil {
				t.Fatal(err)
			}
			if relayRoot() != writerRoot {
				t.Fatalf("relay root %s writer root %s", relayRoot(), writerRoot)
			}
			drained := relayResult(t, relayCall(context.Background(), "bot_relay.outbox.drain", 1, map[string]any{}))
			if ids := relayEnvelopeIDs(t, drained); !reflect.DeepEqual(ids, []string{env.GetOr("id", nil).(string)}) {
				t.Fatalf("drained %v", ids)
			}
		})
	}
}

// test_relay_live_author.py test_live_relay_forwards_sender_to_canonical_authority
func TestLiveRelayForwardsSenderToCanonicalAuthority(t *testing.T) {
	h := relayHome(t)
	var submitted []relayForward
	receipt := map[string]any{"status": "queued", "delivery_id": strings.Repeat("a", 32), "admission_id": "admission"}
	swapRelayDeliver(t, func(_ context.Context, target string, params map[string]any) (map[string]any, error) {
		submitted = append(submitted, relayForward{target, params})
		return receipt, nil
	})
	swapMethod(t, "prompt.submit", func(context.Context, any, map[string]any) map[string]any {
		t.Fatal("relay must not execute through the UI")
		return nil
	})
	response := relayCall(context.Background(), "bot_relay.deliver", 1, map[string]any{"id": strings.Repeat("a", 32), "profile": "ops",
		"message": "ping", "from_profile": "coder", "from_handle": "coder"})
	if got := relayResult(t, response); !reflect.DeepEqual(got, receipt) {
		t.Fatalf("result %v", got)
	}
	want := []relayForward{{filepath.Join(h, "profiles", "ops"), map[string]any{"id": strings.Repeat("a", 32), "profile": "ops", "message": "ping",
		"author": map[string]any{"id": "bot:coder", "name": "coder", "is_bot": true}}}}
	if !reflect.DeepEqual(submitted, want) {
		t.Fatalf("submitted %v", submitted)
	}
}
