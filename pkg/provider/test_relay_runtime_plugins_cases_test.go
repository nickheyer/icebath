package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestRealBindingKeepsTwoProfileTrajectoriesSeparateInSharedExporters(t *testing.T) {
	tmp := t.TempDir()
	workingDirectory := filepath.Join(tmp, "project", "workspace")
	configDirectory := filepath.Join(tmp, "selected-config")
	atofDir, atifDir := filepath.Join(tmp, "atof"), filepath.Join(tmp, "atif")
	for _, d := range []string{workingDirectory, configDirectory} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(configDirectory, "plugins.toml")
	config := strings.TrimSpace(`
version = 1

[[components]]
kind = "observability"
enabled = true

[components.config]
version = 4

[components.config.atof]
enabled = true

[[components.config.atof.sinks]]
type = "file"
output_directory = "` + filepath.ToSlash(atofDir) + `"
filename = "events.jsonl"
mode = "overwrite"

[components.config.atif]
enabled = true
output_directory = "` + filepath.ToSlash(atifDir) + `"
filename_template = "trajectory-{session_id}.json"
agent_name = "Icebath Native Test"
agent_version = "test"
`)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	xdgConfigHome := filepath.Join(tmp, "xdg")
	if err := os.MkdirAll(xdgConfigHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workingDirectory)
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	t.Setenv(tools.RelayPluginsConfigEnv, configPath)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "profile-a"))
	relaytest.Use(t)
	tools.ResetRelayRuntimeForTests()

	runtimeIDs := map[string]string{}
	func() {
		defer tools.ResetRelayRuntimeForTests()
		for _, profile := range []string{"profile-a", "profile-b"} {
			sessionID := "native-export-" + profile
			t.Setenv("ICEBATH_HOME", filepath.Join(tmp, profile))
			ctx := pyctx.With(context.Background(), pyctx.New())
			profileKey := tools.CurrentRelayProfileKey(ctx)
			lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: profileKey, SessionID: sessionID,
				Platform: "cli", Model: "test-model"})
			host, ok := lease.Host.(*tools.RelayRuntime)
			if !ok {
				t.Fatalf("lease host %T is not a Relay runtime", lease.Host)
			}
			runtimeIDs[profile] = host.RuntimeID
			turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, "turn-"+profile, "task-"+profile, nil)
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					tools.RelayCoordinator.EndTurn(ctx, turn, "success")
					tools.RelayCoordinator.ReleaseConversation(lease)
					tools.RelayCoordinator.FinalizeConversation(ctx, profileKey, sessionID)
				}()
				if !host.ManagedExecutionEnabled() {
					t.Fatal("managed execution is not enabled")
				}
				session := sessionID
				if _, err := RelayExecute(ctx, map[string]any{"model": "test-model", "messages": []any{}}, func(context.Context, map[string]any) (any, error) {
					return map[string]any{"id": "response-" + profile, "model": "test-model", "choices": []any{map[string]any{
						"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}}, nil
				}, RelayExecuteOptions{Name: "test-provider", ModelName: "test-model", SessionID: &session,
					Metadata: map[string]any{"api_mode": "chat_completions", "api_request_id": "request-" + profile}}); err != nil {
					t.Fatal(err)
				}
				if _, _, err := tools.RelayExecuteTool(ctx, "terminal", map[string]any{"command": "true"}, func(context.Context, map[string]any) (any, error) {
					return map[string]any{"output": "ok"}, nil
				}, sessionID, "", map[string]any{"tool_call_id": "tool-" + profile}); err != nil {
					t.Fatal(err)
				}
			}()
		}
	}()

	atofPayload, err := os.ReadFile(filepath.Join(atofDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range runtimeIDs {
		if !strings.Contains(string(atofPayload), id) {
			t.Fatalf("ATOF events lack runtime %s", id)
		}
	}
	trajectories, _ := filepath.Glob(filepath.Join(atifDir, "trajectory-*.json"))
	if len(trajectories) != 2 {
		t.Fatalf("trajectories %v", trajectories)
	}
	observed := map[string]bool{}
	for _, path := range trajectories {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var trajectory map[string]any
		if err := json.Unmarshal(raw, &trajectory); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(trajectory)
		matching := 0
		for _, id := range runtimeIDs {
			if strings.Contains(string(payload), id) {
				matching++
				observed[id] = true
			}
		}
		if matching != 1 {
			t.Fatalf("%s matches %d runtimes", path, matching)
		}
		extra, _ := trajectory["extra"].(map[string]any)
		events, _ := extra["observed_events"].([]any)
		categories := map[any]bool{}
		for _, item := range events {
			if ev, _ := item.(map[string]any); ev["kind"] == "scope" {
				categories[ev["category"]] = true
			}
		}
		for _, want := range []string{"agent", "llm", "tool"} {
			if !categories[want] {
				t.Fatalf("%s observed categories %v lack %s", path, categories, want)
			}
		}
	}
	if len(observed) != len(runtimeIDs) {
		t.Fatalf("observed runtimes %v of %v", observed, runtimeIDs)
	}
}
