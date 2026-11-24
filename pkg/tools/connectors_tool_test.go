package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestManageConnectionsValidatesMCPActions(t *testing.T) {
	props := jsonutil.Plain(ManageConnectionsSchema).(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	enum := props["action"].(map[string]any)["enum"].([]any)
	for _, verb := range []string{"install", "enable", "authorize"} {
		if !slices.Contains(enum, any(verb)) {
			t.Fatalf("enum %v lacks %s", enum, verb)
		}
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "install"}, "requires 'connectors'"},
		{map[string]any{"action": "install", "connectors": []any{map[string]any{"name": "linear", "mcp": true}}}, "unknown target field(s) mcp"},
		{map[string]any{"action": "uninstall", "connectors": []any{"linear"}}, "action must be one of"},
	} {
		var out map[string]any
		if err := json.Unmarshal([]byte(ManageConnections(context.Background(), &Env{Home: t.TempDir()}, c.args, ManageOptions{})), &out); err != nil {
			t.Fatal(err)
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, c.want) {
			t.Fatalf("%v -> %q lacks %q", c.args, msg, c.want)
		}
	}
}
