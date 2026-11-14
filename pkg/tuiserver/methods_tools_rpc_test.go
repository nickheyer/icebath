package tuiserver

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Call counters and revision box of the reload fakes, the module state restored after the case
func reloadFakes(t *testing.T) (*map[string]int, *string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	calls := &map[string]int{"discover": 0, "shutdown": 0}
	rev := "rev-a"
	savedShutdown, savedDiscover, savedRev := reloadShutdownMCP, reloadDiscoverMCP, reloadMCPRev
	reloadShutdownMCP = func(context.Context) { (*calls)["shutdown"]++ }
	reloadDiscoverMCP = func(context.Context) error { (*calls)["discover"]++; return nil }
	reloadMCPRev = func(context.Context) string { return rev }
	savedGen, savedLoaded := mcpReloadGen, mcpReloadLoadedRev
	mcpReloadGen, mcpReloadLoadedRev = 0, ""
	t.Cleanup(func() {
		reloadShutdownMCP, reloadDiscoverMCP, reloadMCPRev = savedShutdown, savedDiscover, savedRev
		mcpReloadGen, mcpReloadLoadedRev = savedGen, savedLoaded
	})
	return calls, &rev
}

// Calls reload.mcp for a session that does not exist with the confirm gate passed
func callReload(rev *string, rid any) map[string]any {
	params := map[string]any{"session_id": "no-such-session", "confirm": true}
	if rev != nil {
		params["rev"] = *rev
	}
	return lookupMethod("reload.mcp")(context.Background(), rid, params)
}

func TestSuccessReportsLoadedRev(t *testing.T) {
	calls, rev := reloadFakes(t)
	*rev = "rev-a"
	want := "rev-a"
	envelope := callReload(&want, 1)
	result, _ := envelope["result"].(map[string]any)
	if result["status"] != "reloaded" || result["loaded_rev"] != "rev-a" {
		t.Fatalf("envelope = %v", envelope)
	}
	if (*calls)["discover"] != 1 || mcpReloadGen != 1 {
		t.Fatalf("discover = %d gen = %d", (*calls)["discover"], mcpReloadGen)
	}
}

func TestFailedReloadIsAnErrorAndNoGenerationAdvance(t *testing.T) {
	reloadFakes(t)
	reloadDiscoverMCP = func(context.Context) error { return errors.New("flapping server") }
	want := "rev-b"
	envelope := callReload(&want, 1)
	if _, failed := envelope["error"]; !failed {
		t.Fatalf("envelope = %v", envelope)
	}
	if mcpReloadGen != 0 || mcpReloadLoadedRev != "" {
		t.Fatalf("gen = %d loaded = %q", mcpReloadGen, mcpReloadLoadedRev)
	}
}

func TestLeaderRehashesUntilStableWhenConfigChangesMidReload(t *testing.T) {
	calls, _ := reloadFakes(t)
	hashes := []string{"rev-a", "rev-b", "rev-b"}
	reloadMCPRev = func(context.Context) string {
		next := hashes[0]
		hashes = hashes[1:]
		return next
	}
	want := "rev-a"
	envelope := callReload(&want, 1)
	result, _ := envelope["result"].(map[string]any)
	if result["loaded_rev"] != "rev-b" {
		t.Fatalf("envelope = %v", envelope)
	}
	if (*calls)["discover"] != 2 || mcpReloadGen != 1 {
		t.Fatalf("discover = %d gen = %d", (*calls)["discover"], mcpReloadGen)
	}
}

func TestShellExecScrubsChildEnvAndForceRedactsRpcOutput(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	secret := "sk-shell-exec-secret-1234567890"
	t.Setenv("OPENROUTER_API_KEY", secret)
	command := "echo visible; echo " + secret + "; if [ -n \"$OPENROUTER_API_KEY\" ]; then echo inherited; else echo scrubbed; fi; echo diagnostic " + secret + " >&2"
	response := handleRequest(context.Background(), map[string]any{"id": "security", "method": "shell.exec",
		"params": map[string]any{"command": command}})
	result, _ := response["result"].(map[string]any)
	stdout, _ := result["stdout"].(string)
	stderr, _ := result["stderr"].(string)
	if !strings.Contains(stdout, "scrubbed") || strings.Contains(stdout, "inherited") {
		t.Fatalf("child env kept OPENROUTER_API_KEY: %v", response)
	}
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatalf("secret survived redaction: %v", response)
	}
	if !strings.Contains(stdout, "visible") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestRollbackRestoreRefusesANonBooleanAllBeforeTouchingAnything(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	history := []any{map[string]any{"role": "user", "content": "hi"}, map[string]any{"role": "assistant", "content": "ok"}}
	for _, value := range []any{"false", "true", 1, 0, []any{}, map[string]any{}} {
		for _, extra := range []map[string]any{{}, {"file_path": "a.py"}} {
			session := liveSession(t, "sid", map[string]any{"agent": testAgent(t, agent.AIAgentOptions{}), "session_key": "session-key",
				"history": append([]any{}, history...), "history_version": 0, "running": false, "attached_images": []any{},
				"image_counter": 0, "cols": 80, "slash_worker": nil, "show_reasoning": false})
			params := map[string]any{"session_id": "sid", "hash": "1", "all": value}
			for k, v := range extra {
				params[k] = v
			}
			resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "rollback.restore", "params": params})
			errObj, _ := resp["error"].(map[string]any)
			want := map[string]any{"reason": "invalid_params", "field": "all", "detail": "must be a boolean"}
			if !reflect.DeepEqual(errObj["data"], want) {
				t.Fatalf("value %v extra %v: resp = %v", value, extra, resp)
			}
			if !reflect.DeepEqual(session.Get("history"), history) {
				t.Fatalf("history rewound: %v", session.Get("history"))
			}
		}
	}
}
