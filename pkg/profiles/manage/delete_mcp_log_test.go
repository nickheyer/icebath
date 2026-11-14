package manage

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

// First argument that turns this test binary into the stdio MCP fixture server
const mcpStdioFixtureArg = "icebath-mcp-stdio-fixture"

// Answers initialize and tools/list line by line with one echo tool, skipping notifications
func runMCPStdioFixture(in io.Reader, out io.Writer) int {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var request map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return 1
		}
		id, ok := request["id"]
		if !ok {
			continue
		}
		var result any = map[string]any{}
		switch request["method"] {
		case "initialize":
			params, _ := request["params"].(map[string]any)
			result = map[string]any{"protocolVersion": params["protocolVersion"], "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "probe-fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Echo", "inputSchema": map[string]any{"type": "object"}}}}
		}
		line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		if err != nil {
			return 1
		}
		if _, err := out.Write(append(line, '\n')); err != nil {
			return 1
		}
	}
	return 0
}

func TestDeleteProfileAfterStdioProbe(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	oldClean, oldBackends, oldNotify := cleanupGatewayService, stopProfileBackends, notifyMultiplexer
	t.Cleanup(func() {
		cleanupGatewayService, stopProfileBackends, notifyMultiplexer = oldClean, oldBackends, oldNotify
	})
	cleanupGatewayService = func(context.Context, string, string) bool { return false }
	stopProfileBackends = func(string, string) {}
	notifyMultiplexer = func(string) {}
	ctx := context.Background()
	profile, err := CreateProfile(ctx, "mcp-probe-delete", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		key := home.Key(profile)
		mcp.ShutdownMCPServers(ctx, &key, nil, 5*time.Second)
	})
	found, err := mcp.ProbeSingleServer(home.WithOverride(ctx, profile), &tools.Env{}, "fixture",
		map[string]any{"command": os.Args[0], "args": []any{mcpStdioFixtureArg}})
	if err != nil || !reflect.DeepEqual(found, []mcp.ToolSummary{{Name: "echo", Description: "Echo"}}) {
		t.Fatalf("%v %v", found, err)
	}
	if _, err := DeleteProfile(ctx, "mcp-probe-delete", true); err != nil {
		t.Fatal(err)
	}
	if pathExists(profile) {
		t.Fatal("profile directory survives the delete")
	}
}
