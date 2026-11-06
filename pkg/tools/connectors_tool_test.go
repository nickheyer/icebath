package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Gateway double of the manage_connections handler cases, recording every call
type toolCasesClient struct{ calls [][]any }

func (c *toolCasesClient) ListConnectors(context.Context, time.Duration) ([]map[string]any, error) {
	c.calls = append(c.calls, []any{"list"})
	return []map[string]any{{"connector": "gmail", "enabled": true, "connected": false},
		{"connector": "linear", "enabled": true, "connected": true}}, nil
}

func (c *toolCasesClient) Connections(_ context.Context, req ConnectionsRequest) (map[string]any, error) {
	c.calls = append(c.calls, []any{"connections", req.Connectors, req.Reinitiate})
	var results []any
	for _, name := range req.Connectors {
		results = append(results, map[string]any{"connector": name, "status": "initiated", "connect_url": "https://connect.example/" + name,
			"instruction": "finish authorizing " + name + " in the browser", "reinitiated": req.Reinitiate})
	}
	return map[string]any{"results": results, "summary": map[string]any{"total": len(req.Connectors), "initiated": len(req.Connectors)}}, nil
}

func (c *toolCasesClient) AccountStatus(context.Context, string, time.Duration) (map[string]any, error) {
	return nil, nil
}

func toolCasesRun(t *testing.T, args map[string]any, factory func() (ManagedClient, error)) map[string]any {
	t.Helper()
	env := managedTestSetup(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	oldPortal := NousPortalBaseURL
	NousPortalBaseURL = func(context.Context) (string, error) { return "https://portal.test", nil }
	t.Cleanup(func() { NousPortalBaseURL = oldPortal })
	out := ManageConnections(context.Background(), env, args, ManageOptions{ClientFactory: factory})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("%v in %s", err, out)
	}
	return parsed
}

func TestManageConnectionsStatusListsAndFiltersConnectors(t *testing.T) {
	client := &toolCasesClient{}
	out := toolCasesRun(t, map[string]any{"action": "status", "connectors": []any{"GMAIL"}},
		func() (ManagedClient, error) { return client, nil })
	want := []any{map[string]any{"connector": "gmail", "enabled": true, "connected": false}}
	if !reflect.DeepEqual(out["connectors"], want) {
		t.Fatalf("out %v", out)
	}
}

func TestManageConnectionsConnectWithoutConnectorsIsAUsageError(t *testing.T) {
	out := toolCasesRun(t, map[string]any{"action": "connect"}, func() (ManagedClient, error) { return &toolCasesClient{}, nil })
	if msg, _ := out["error"].(string); !strings.Contains(msg, "requires 'connectors'") {
		t.Fatalf("out %v", out)
	}
}

func TestManageConnectionsDisconnectIsRefusedBeforeAnyGatewayCall(t *testing.T) {
	client := &toolCasesClient{}
	out := toolCasesRun(t, map[string]any{"action": "disconnect", "connectors": []any{"gmail"}},
		func() (ManagedClient, error) { return client, nil })
	if _, ok := out["error"]; !ok || len(client.calls) != 0 {
		t.Fatalf("out %v calls %v", out, client.calls)
	}
}

func TestManageConnectionsGatewayFailureIsAModelActionableError(t *testing.T) {
	out := toolCasesRun(t, map[string]any{"action": "status"}, func() (ManagedClient, error) { return nil, errors.New("gateway on fire") })
	if msg, _ := out["error"].(string); !strings.Contains(msg, "connector gateway request failed") {
		t.Fatalf("out %v", out)
	}
}

func TestManageConnectionsMCPActionsBelongToMCPTargetsOnly(t *testing.T) {
	props := ManageConnectionsSchema["parameters"].(map[string]any)["properties"].(map[string]any)
	enum := props["action"].(map[string]any)["enum"].([]any)
	for _, verb := range []string{"install", "enable", "authorize"} {
		if !slices.Contains(enum, any(verb)) {
			t.Fatalf("enum %v lacks %s", enum, verb)
		}
	}
	none := func() (ManagedClient, error) { return &toolCasesClient{}, nil }
	for _, c := range []struct {
		args map[string]any
		want []string
	}{
		{map[string]any{"action": "install", "connectors": []any{"linear"}}, []string{"mcp", "install"}},
		{map[string]any{"action": "connect", "connectors": []any{map[string]any{"name": "linear", "mcp": true}}}, []string{"managed-connector action"}},
		{map[string]any{"action": "uninstall", "connectors": []any{"gmail"}}, []string{"action must be one of"}},
	} {
		msg, _ := toolCasesRun(t, c.args, none)["error"].(string)
		for _, w := range c.want {
			if !strings.Contains(msg, w) {
				t.Fatalf("%v -> %q lacks %q", c.args, msg, w)
			}
		}
	}
}
