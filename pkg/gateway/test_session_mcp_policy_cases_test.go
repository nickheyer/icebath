package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// First argument that turns the test binary into the stdio MCP peer of the editor cases
const ownedMCPPeerArg = "-icebath-owned-mcp-peer"

// Serves as the MCP peer before any test setup when the binary was launched as one
func init() {
	if len(os.Args) > 1 && os.Args[1] == ownedMCPPeerArg {
		os.Exit(runOwnedMCPPeer())
	}
}

// Answers initialize, tools/list and tools/call with the borrowed credential, line by line until stdin closes
func runOwnedMCPPeer() int {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		ident, present := request["id"]
		if !present || ident == nil {
			continue
		}
		params, _ := request["params"].(map[string]any)
		var result map[string]any
		switch request["method"] {
		case "initialize":
			result = map[string]any{"protocolVersion": params["protocolVersion"], "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "owned-peer", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "owned echo",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": os.Getenv("BORROWED_CREDENTIAL")}}}
		default:
			result = map[string]any{}
		}
		if out.Encode(map[string]any{"jsonrpc": "2.0", "id": ident, "result": result}) != nil {
			return 1
		}
	}
	return 0
}

// Stdio MCP server spec whose tool echoes a borrowed credential
func ownedServerSpec(t *testing.T) map[string]any {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"name": "same-editor-name", "command": binary, "args": []any{ownedMCPPeerArg},
		"env": []any{map[string]any{"name": "BORROWED_CREDENTIAL", "value": "PRIVATE_A"},
			map[string]any{"name": "ICEBATH_HOME", "value": os.Getenv("ICEBATH_HOME")}}}
}

// Owner with no database and no launch slots, as the namespace owner of the editor cases
func editorOwner() *SessionAuthority {
	return &SessionAuthority{InstanceID: "owned", Epoch: 1, ProfileID: "owned-profile"}
}

// Editor policy of an acp session bound under the owner with its borrowed secrets in memory
func editorPolicy(t *testing.T, ctx context.Context, a *SessionAuthority, sid string, spec map[string]any, cwd string) *LocalSessionPolicy {
	t.Helper()
	private := &RecoveredSecrets{}
	result, err := buildPolicy(ctx, map[string]any{"source": "acp", "cwd": cwd, "model": "local", "toolsets": []any{},
		"editor": map[string]any{"mcp_servers": []any{spec}}}, map[string]any{}, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindLaunchKey(ctx, a, sid, result, nil, private)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func refusedReason(err error) string {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

func TestColdEditorRebindRequiresExactBorrowedConfig(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	cwd := t.TempDir()
	spec := ownedServerSpec(t)
	original := editorPolicy(t, ctx, editorOwner(), "cold", spec, cwd)
	text, err := pyjson.Dumps(original.asDict())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	cold, err := restorePolicy(stored)
	if err != nil {
		t.Fatal(err)
	}
	authority := editorOwner()
	t.Cleanup(func() { releaseEditorMCP(ctx, authority, []string{"cold"}) })
	err = policyScope(ctx, cold, authority, func(context.Context) error { return nil })
	if reason := refusedReason(err); reason != "launch_credentials_unavailable" {
		t.Fatalf("cold scope %v", err)
	}
	changed := pyjson.Plain(pyjson.Clone(spec)).(map[string]any)
	changed["env"].([]any)[0].(map[string]any)["value"] = "CHANGED_SECRET"
	if reason := refusedReason(attachEditorMCP(ctx, authority, cold, []any{changed})); reason != "acp_mcp_policy_conflict" {
		t.Fatalf("changed secret attach gave %q", reason)
	}
	if err := attachEditorMCP(ctx, authority, cold, []any{spec}); err != nil {
		t.Fatal(err)
	}
	err = policyScope(ctx, cold, authority, func(scoped context.Context) error {
		scope := tools.SessionToolScope(scoped)
		var names []string
		for _, name := range tools.Default.AllToolNames(scoped, scope) {
			if strings.HasPrefix(name, "mcp__") {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			t.Fatal("no editor tool in the session scope")
		}
		entry := tools.Default.Lookup(scoped, scope, names[0])
		if entry == nil {
			t.Fatalf("tool %s has no entry", names[0])
		}
		if result := entry.Handler(scoped, &tools.Call{Name: names[0], Args: map[string]any{}}); !strings.Contains(result, "PRIVATE_A") {
			t.Fatalf("dispatch result %q", result)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cold.Toolsets, original.Toolsets) {
		t.Fatalf("toolsets %v, want %v", cold.Toolsets, original.Toolsets)
	}
}
