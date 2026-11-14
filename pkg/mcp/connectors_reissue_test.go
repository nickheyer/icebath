package mcp

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Connections route recording every mint the card retry sends
type reissueGateway struct{ mints []map[string]any }

func (g *reissueGateway) ListConnectors(context.Context, time.Duration) ([]map[string]any, error) {
	return []map[string]any{{"connector": "gmail", "enabled": true, "connected": false}}, nil
}

func (g *reissueGateway) Connections(_ context.Context, req tools.ConnectionsRequest) (map[string]any, error) {
	g.mints = append(g.mints, map[string]any{"connectors": req.Connectors, "reinitiate": req.Reinitiate, "alias": req.Alias,
		"connection_id": req.ConnectionID})
	results := []any{}
	for _, c := range req.Connectors {
		results = append(results, map[string]any{"connector": c, "status": "initiated", "connection_id": "ca_new",
			"connect_url": "https://connect.example/" + c})
	}
	return map[string]any{"results": results}, nil
}

func (g *reissueGateway) AccountStatus(_ context.Context, id string, _ time.Duration) (map[string]any, error) {
	return map[string]any{"connectionId": id, "connector": "gmail", "status": "active", "active": true}, nil
}

func TestTryAgainOnANamedAccountRepairsTheAccountItMintedNotTheName(t *testing.T) {
	isolateHome(t)
	gateway := &reissueGateway{}
	saved := reissueManagedClient
	var initiators []string
	reissueManagedClient = func(_ *tools.Env, createsGuest bool, initiator string, _ *tools.CallRef) (tools.ManagedClient, error) {
		initiators = append(initiators, initiator)
		return gateway, nil
	}
	t.Cleanup(func() { reissueManagedClient = saved })
	target := tools.NewConnTarget("gmail", "connector", "reconnect")
	alias := "work"
	target.Alias = &alias
	op := tools.NewConnectionOperation([]*tools.ConnTarget{target}, "s1", "", nil)
	id, detail := "ca_work", "expired"
	if _, err := op.Transition("gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{ConnectionID: &id}); err != nil {
		t.Fatal(err)
	}
	if _, err := op.Transition("gmail", tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{Detail: &detail}); err != nil {
		t.Fatal(err)
	}
	refusal, err := Reissue(context.Background(), &tools.Env{Platform: "desktop"}, op, []string{"gmail"}, "")
	if err != nil || refusal != "" {
		t.Fatalf("refusal %q err %v", refusal, err)
	}
	want := []map[string]any{{"connectors": []string{"gmail"}, "reinitiate": true, "alias": "", "connection_id": "ca_work"}}
	if !reflect.DeepEqual(gateway.mints, want) || !reflect.DeepEqual(initiators, []string{tools.InitiatorCardRetry}) {
		t.Fatalf("mints %v initiators %v", gateway.mints, initiators)
	}
}

func TestReissueRefusesUnknownMixedLiveAndSettledRows(t *testing.T) {
	isolateHome(t)
	a, b := tools.NewConnTarget("gmail", "connector", "connect"), tools.NewConnTarget("linear", "mcp", "install")
	c := tools.NewConnTarget("blender", "plugin", "install")
	op := tools.NewConnectionOperation([]*tools.ConnTarget{a, b, c}, "s1", "", nil)
	ctx := context.Background()
	for _, c := range []struct {
		names []string
		want  string
	}{{[]string{"nope"}, tools.ConnUnknownTarget}, {[]string{"gmail", "linear"}, tools.ConnMixedKinds},
		{[]string{"gmail"}, tools.ConnLinkStillValid}} {
		if got, err := Reissue(ctx, nil, op, c.names, ""); err != nil || got != c.want {
			t.Fatalf("%v: %q %v", c.names, got, err)
		}
	}
	if _, err := op.Transition("blender", tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{}); err != nil {
		t.Fatal(err)
	}
	op.Settle(tools.SettleContinue)
	if got, _ := Reissue(ctx, nil, op, []string{"blender"}, ""); got != tools.ConnSettled {
		t.Fatalf("settled %q", got)
	}
}
