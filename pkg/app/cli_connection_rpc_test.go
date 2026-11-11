package app

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CLI with an offline agent whose session owns a fresh two row connector operation
func connectionRPCCLI(t *testing.T) (*CLI, *tools.ConnectionOperation) {
	t.Helper()
	tempHome(t)
	tools.LiveResetForTests()
	t.Cleanup(tools.LiveResetForTests)
	c := &CLI{ctx: context.Background(), sessionID: "sid-connections"}
	c.agent = offlineAgent(t, c)
	op := tools.NewConnectionOperation([]*tools.ConnTarget{tools.NewConnTarget("gmail", "connector", "connect"),
		tools.NewConnTarget("notion", "connector", "connect")}, connectionSessionKey(c), "", nil)
	if err := tools.LiveOpen(c.ctx, op); err != nil {
		t.Fatal(err)
	}
	return c, op
}

// connectionGateway records every mint a card retry sends
type connectionGateway struct{ mints [][2]any }

func (g *connectionGateway) ListConnectors(context.Context, time.Duration) ([]map[string]any, error) {
	return []map[string]any{{"connector": "notion", "enabled": true, "connected": false}}, nil
}

func (g *connectionGateway) Connections(_ context.Context, req tools.ConnectionsRequest) (map[string]any, error) {
	g.mints = append(g.mints, [2]any{append([]string{}, req.Connectors...), req.Reinitiate})
	results := []any{}
	for _, n := range req.Connectors {
		results = append(results, map[string]any{"connector": n, "status": "initiated", "connect_url": "https://l/" + n + "/2"})
	}
	return map[string]any{"results": results}, nil
}

func (g *connectionGateway) AccountStatus(context.Context, string, time.Duration) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestConnectionRespondDrivesTheLiveOperation(t *testing.T) {
	c, op := connectionRPCCLI(t)
	if _, err := op.Transition("gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{}); err != nil {
		t.Fatal(err)
	}
	if err := agentConnectionRespond(c.ctx, c, op.OpID, map[string]any{"targets": []any{map[string]any{"name": "notion", "status": "skipped"}}}); err != nil {
		t.Fatal(err)
	}
	if op.StateOf(op.Target("notion")) != tools.TargetSkipped || !op.Wake.IsSet() {
		t.Fatalf("notion %v wake %v", op.StateOf(op.Target("notion")), op.Wake.IsSet())
	}
	if err := agentConnectionRespond(c.ctx, c, "op-unknown", map[string]any{"settled_by": "continue"}); err != connectorUnknownOperation {
		t.Fatalf("unknown op %v", err)
	}
}

func TestConnectionRespondCannotClaimAnOutcomeForAnyTarget(t *testing.T) {
	c, op := connectionRPCCLI(t)
	if _, err := op.Transition("gmail", tools.TargetInitiated, tools.ActorBackendWatcher, tools.TransitionFields{}); err != nil {
		t.Fatal(err)
	}
	err := agentConnectionRespond(c.ctx, c, op.OpID, map[string]any{"targets": []any{
		map[string]any{"name": "gmail", "status": "connected"}, map[string]any{"name": "notion", "status": "failed"}}})
	if err != connectorInvalidAnswer || err.Error() != "Connection answer is invalid." {
		t.Fatalf("err %v", err)
	}
	if op.StateOf(op.Target("gmail")) != tools.TargetInitiated || op.StateOf(op.Target("notion")) != tools.TargetPending {
		t.Fatalf("states moved")
	}
	if err := agentConnectionRespond(c.ctx, c, op.OpID, map[string]any{"settled_by": "continue", "extra": true}); err != connectorInvalidAnswer {
		t.Fatalf("extra key %v", err)
	}
}

func TestConnectionRespondContinueSettles(t *testing.T) {
	c, op := connectionRPCCLI(t)
	if err := agentConnectionRespond(c.ctx, c, op.OpID, map[string]any{"settled_by": "continue"}); err != nil {
		t.Fatal(err)
	}
	if !op.Settled() || op.SettledBy() != tools.SettleContinue {
		t.Fatalf("settled %v by %v", op.Settled(), op.SettledBy())
	}
	if tools.LiveCurrent(connectionSessionKey(c), "") != nil {
		t.Fatal("settled operation still open")
	}
}

func TestConnectionRetryReissuesOnlyADeadLink(t *testing.T) {
	c, op := connectionRPCCLI(t)
	gmailURL, notionURL, detail := "https://l/gmail/1", "https://l/notion/1", "vendor: nope"
	for _, step := range []struct {
		name string
		to   tools.TargetState
		f    tools.TransitionFields
	}{{"gmail", tools.TargetInitiated, tools.TransitionFields{ConnectURL: &gmailURL}},
		{"notion", tools.TargetInitiated, tools.TransitionFields{ConnectURL: &notionURL}},
		{"notion", tools.TargetFailed, tools.TransitionFields{Detail: &detail}}} {
		if _, err := op.Transition(step.name, step.to, tools.ActorBackendWatcher, step.f); err != nil {
			t.Fatal(err)
		}
	}
	savedSelect, savedAvailable := connectionSelectToolNames, connectionConnectorsAvailable
	connectionConnectorsAvailable = func(*tools.Env) bool { return false }
	connectionSelectToolNames = func(context.Context, *tools.Env, []string, []string) map[string]bool {
		return map[string]bool{"manage_connections": true}
	}
	t.Cleanup(func() { connectionSelectToolNames, connectionConnectorsAvailable = savedSelect, savedAvailable })
	if err := agentConnectionRetry(c.ctx, c, "notion"); err != connectorsUnavailableRefusal {
		t.Fatalf("unavailable %v", err)
	}
	connectionConnectorsAvailable = func(*tools.Env) bool { return true }
	gateway := &connectionGateway{}
	t.Cleanup(mcp.SwapReissueManagedClient(func(*tools.Env, bool, string, *tools.CallRef) (tools.ManagedClient, error) {
		return gateway, nil
	}))
	if err := agentConnectionRetry(c.ctx, c, "gmail"); err == nil || err.Error() != "Reopen the stored link." || len(gateway.mints) != 0 {
		t.Fatalf("live link %v mints %v", err, gateway.mints)
	}
	if got := op.Target("gmail").ConnectURL; got != gmailURL {
		t.Fatalf("gmail link %v", got)
	}
	if err := agentConnectionRetry(c.ctx, c, "notion"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gateway.mints, [][2]any{{[]string{"notion"}, true}}) {
		t.Fatalf("mints %v", gateway.mints)
	}
	notion := op.Target("notion")
	if op.StateOf(notion) != tools.TargetInitiated || notion.ConnectURL != "https://l/notion/2" {
		t.Fatalf("notion %v %v", op.StateOf(notion), notion.ConnectURL)
	}
}
