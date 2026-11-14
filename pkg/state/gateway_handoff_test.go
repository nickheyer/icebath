package state

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestHeartbeatPruneDropsOnlyStaleRows(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	old, fresh := now()-600, now()
	for id, ts := range map[string]*float64{"dead@h:1": &old, "live@h:2": &fresh} {
		if err := db.RegisterBackendHeartbeat(ctx, BackendHeartbeat{BackendID: id, PID: 1, StartedAt: 1, LastHeartbeat: ts}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := db.PruneStaleHeartbeats(ctx, 0); err != nil || len(got) != 0 {
		t.Fatalf("zero window pruned %v %v", got, err)
	}
	if got, err := db.PruneStaleHeartbeats(ctx, 300); err != nil || !reflect.DeepEqual(got, []string{"dead@h:1"}) {
		t.Fatalf("pruned %v %v", got, err)
	}
	rows, err := db.ListBackendHeartbeats(ctx)
	if err != nil || len(rows) != 1 || rowValue(rows[0], "backend_id") != "live@h:2" {
		t.Fatalf("listed %v %v", rows, err)
	}
}

func TestHandoffStateMachine(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "s", Source: "cli"})
	if db.GetHandoffState(ctx, "missing") != nil {
		t.Fatal("missing session has a handoff record")
	}
	if ok, err := db.RequestHandoff(ctx, "s", "telegram"); err != nil || !ok {
		t.Fatalf("request %v %v", ok, err)
	}
	if ok, _ := db.RequestHandoff(ctx, "s", "discord"); ok {
		t.Fatal("second request accepted while pending")
	}
	if pending, err := db.HasPendingHandoffs(ctx); err != nil || !pending || len(db.ListPendingHandoffs(ctx)) != 1 {
		t.Fatalf("pending %v %v", pending, err)
	}
	if ok, _ := db.FailHandoff(ctx, "s", "gave up", []string{"running"}); ok {
		t.Fatal("compare and swap failed a pending row")
	}
	if ok, err := db.ClaimHandoff(ctx, "s"); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if ok, _ := db.ClaimHandoff(ctx, "s"); ok {
		t.Fatal("claimed twice")
	}
	if got := db.ReclaimStaleRunningHandoffs(ctx, strings.Repeat("x", 600)); !reflect.DeepEqual(got, []string{"s"}) {
		t.Fatalf("reclaimed %v", got)
	}
	state := db.GetHandoffState(ctx, "s")
	if rowValue(state, "state") != "failed" || rowValue(state, "platform") != "telegram" || len(pyStr(rowValue(state, "error"))) != 500 {
		t.Fatalf("state %v", state)
	}
	if ok, _ := db.RequestHandoff(ctx, "s", "telegram"); !ok {
		t.Fatal("failed handoff could not be retried")
	}
	if err := db.CompleteHandoff(ctx, "s"); err != nil || rowValue(db.GetHandoffState(ctx, "s"), "state") != "completed" {
		t.Fatalf("complete %v", err)
	}
}

func TestHygieneFailureStreakCountsAndResets(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	for want := 1; want <= 3; want++ {
		if got, err := db.IncrementHygieneFailureStreak(ctx, "k"); err != nil || got != want {
			t.Fatalf("streak %d %v want %d", got, err, want)
		}
	}
	if err := db.ResetHygieneFailureStreak(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.IncrementHygieneFailureStreak(ctx, "k"); got != 1 {
		t.Fatalf("streak after reset %d", got)
	}
	if got, _ := db.IncrementHygieneFailureStreak(ctx, ""); got != 1 {
		t.Fatalf("empty key streak %d", got)
	}
}

func TestRoutingReplaceSkipsFencedRoutes(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "live", Source: "telegram"})
	if err := db.RetireGatewayRoutes(ctx, []string{"gone", ""}, "bot2", nil); err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{
		"agent:main:telegram:dm:1": `{"session_id": "live"}`,
		"agent:bot2:telegram:dm:2": `{"session_id": "gone"}`,
		"agent:main:telegram:dm:3": `{"session_id": "gone"}`,
	}
	skipped, err := db.ReplaceGatewayRoutingEntries(ctx, entries, "s1")
	if err != nil || !reflect.DeepEqual(skipped, map[string]bool{"agent:bot2:telegram:dm:2": true, "agent:main:telegram:dm:3": true}) {
		t.Fatalf("skipped %v %v", skipped, err)
	}
	loaded, err := db.LoadGatewayRoutingEntries(ctx, "s1")
	if err != nil || !reflect.DeepEqual(loaded, map[string]string{"agent:main:telegram:dm:1": `{"session_id": "live"}`}) {
		t.Fatalf("loaded %v %v", loaded, err)
	}
	if retired, err := db.RouteRetired(ctx, "agent:bot2:x", `{"session_id": "gone"}`); err != nil || !retired {
		t.Fatalf("route retired %v %v", retired, err)
	}
	if retired, err := db.RouteRetired(ctx, "agent:bot3:x", `{"session_id": "gone"}`); err != nil || retired {
		t.Fatalf("other profile fenced %v %v", retired, err)
	}
	if entry, err := db.GatewayRoutingEntryForSession(ctx, "live"); err != nil || rowValue(entry, "session_id") != "live" {
		t.Fatalf("entry %v %v", entry, err)
	}
	if got := RouteNamespace("cli:x"); got != "main" {
		t.Fatalf("namespace %q", got)
	}
}

func TestSessionGatewayRuntimePrecedence(t *testing.T) {
	meta := func(pairs ...any) *pyjson.Object {
		o := pyjson.NewObject()
		for i := 0; i < len(pairs); i += 2 {
			o.Set(pairs[i].(string), pairs[i+1])
		}
		return o
	}
	cases := []struct {
		row  *pyjson.Object
		want string
	}{
		{meta("model_config", `{"gateway_runtime": {"provider": "openrouter", "api_mode": null}, "provider": "x"}`), `{"provider": "openrouter"}`},
		{meta("model_config", `{"provider": "anthropic", "base_url": "", "api_mode": "messages"}`), `{"provider": "anthropic", "api_mode": "messages"}`},
		{meta("model_config", "{}", "billing_provider", " nous "), `{"provider": "nous"}`},
		{meta("model_config", "{}", "billing_provider", "custom", "billing_base_url", "http://h/v1"), `{"base_url": "http://h/v1", "provider": "custom"}`},
		{meta("model_config", "not json", "billing_provider", "auto"), `{}`},
		{meta("model_config", `{"gateway_runtime": {"model": "m", "x": null}}`), `{"model": "m"}`},
		{nil, `{}`},
	}
	for i, c := range cases {
		got, err := pyjson.Dumps(SessionGatewayRuntime(c.row))
		if err != nil || got != c.want {
			t.Fatalf("case %d got %s %v want %s", i, got, err, c.want)
		}
	}
}

func TestLocalReceiptReadsTheStoredPolicy(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+"ok", `{"entry": {"session_id": "ok"}, "route": "cli:ok"}`)
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+"list", `[1]`)
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+"bad", `{`)
	receipt, err := db.LocalReceipt(ctx, "ok")
	if err != nil || rowValue(receipt, "route") != "cli:ok" {
		t.Fatalf("receipt %v %v", receipt, err)
	}
	for _, sid := range []string{"missing", "list", "bad"} {
		var refusal *RuntimeStoreError
		if _, err := db.LocalReceipt(ctx, sid); !errors.As(err, &refusal) || refusal.Reason != "storage_unavailable" {
			t.Fatalf("%s refusal %v", sid, err)
		}
	}
}
