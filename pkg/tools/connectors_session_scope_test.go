package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Status runs against the gateway only when the session grants connections, else it refuses before any client exists
func TestManagedStatusFollowsTheSessionConnectionsGrant(t *testing.T) {
	cases := []struct {
		enabled, disabled []string
		allowed           bool
	}{
		{[]string{}, nil, false}, {[]string{"safe"}, nil, false}, {[]string{"icebath-webhook"}, nil, false},
		{[]string{"connections"}, nil, true}, {[]string{"icebath-cli"}, []string{"connections"}, false},
		{[]string{"safe"}, []string{"connections"}, false}, {nil, []string{"connections"}, false}, {nil, nil, true},
	}
	for _, c := range cases {
		env := managedTestSetup(t)
		t.Setenv("ICEBATH_HOME", env.Home)
		oldPortal := NousPortalBaseURL
		NousPortalBaseURL = func(context.Context) (string, error) { return "https://portal.test", nil }
		var remote []string
		client := &toolCasesClient{}
		out := ManageConnections(context.Background(), env, map[string]any{"action": "status"}, ManageOptions{
			ClientFactory: func() (ManagedClient, error) {
				remote = append(remote, "status")
				return client, nil
			},
			ConnectorsAvailable: func() bool { return c.allowed },
		})
		NousPortalBaseURL = oldPortal
		var status map[string]any
		if err := json.Unmarshal([]byte(out), &status); err != nil {
			t.Fatalf("%v in %s", err, out)
		}
		if c.allowed {
			if _, has := status["error"]; has || !reflect.DeepEqual(remote, []string{"status"}) || len(client.calls) != 1 {
				t.Fatalf("%v/%v: status %s remote %v calls %v", c.enabled, c.disabled, out, remote, client.calls)
			}
			continue
		}
		if !strings.Contains(out, "not available in this session") || remote != nil || client.calls != nil {
			t.Fatalf("%v/%v: status %s remote %v", c.enabled, c.disabled, out, remote)
		}
	}
}
