package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane/remoteplane"
)

// Result of one profiles.configure of the default profile
func configureDefaultProfile(t *testing.T, rid int, params map[string]any) map[string]any {
	t.Helper()
	params["name"] = "default"
	reply, _ := pyjson.Plain(lookupMethod("profiles.configure")(context.Background(), rid, params)).(map[string]any)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("configure answered %v", reply)
	}
	return result
}

// Whether the example server reads enabled in the user config as the profile editor reads it
func exampleMCPEnabled(t *testing.T) bool {
	t.Helper()
	raw, err := config.ReadUserConfigRaw(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := raw["mcp_servers"].(map[string]any)
	entry, _ := servers["example"].(map[string]any)
	return mcpEntryEnabled(entry)
}

func TestProfileEditorReportsMcpUnappliedWhenLegacyDisabledCleanupIsLocked(t *testing.T) {
	p := remoteplane.Setup(t)
	p.Mu.Lock()
	p.Upper = map[string]any{"mcp_servers": map[string]any{"example": map[string]any{"command": "unused", "enabled": false}}}
	p.Profile("default").Values = map[string]any{"mcp_servers": map[string]any{"example": map[string]any{"disabled": true}}}
	p.UpperLocks = []map[string]any{{"path": "mcp_servers.example.disabled", "level": "tenant"}}
	p.Mu.Unlock()
	setLaunchHome(t, p.Home)
	locked := configureDefaultProfile(t, 1, map[string]any{"enabled_mcp_servers": []any{"example"}})
	applied, _ := locked["applied"].(map[string]any)
	if applied["mcp_servers"] != false || locked["ok"] != false {
		t.Fatalf("locked edit answered %v", locked)
	}
	if exampleMCPEnabled(t) {
		t.Fatal("the editor shows the locked server on")
	}
	p.Mu.Lock()
	p.UpperLocks = nil
	p.Mu.Unlock()
	remote.GetRemoteBackend().(interface{ PollAll() }).PollAll()
	unlocked := configureDefaultProfile(t, 2, map[string]any{"enabled_mcp_servers": []any{"example"}})
	applied, _ = unlocked["applied"].(map[string]any)
	if applied["mcp_servers"] != true || unlocked["ok"] != true {
		t.Fatalf("unlocked edit answered %v", unlocked)
	}
	if !exampleMCPEnabled(t) {
		t.Fatal("the editor shows the unlocked server off")
	}
}
