package app

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Gateway answering each method from a fixed table and recording every call
func rollbackGateway(t *testing.T, answers map[string]any) (*scriptedClient, *gatewaycli.GatewayConnection) {
	t.Helper()
	client := &scriptedClient{}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		answer, known := answers[method]
		if !known {
			return nil, false, nil
		}
		client.record(method, params)
		return wireValue(t, answer), true, nil
	}
	return client, scriptedConnection(t, client)
}

// CLI on session local-rb holding one old user row
func rollbackCLI() *CLI {
	c := bareCLI()
	c.sessionID = "local-rb"
	c.conversationHistory = []map[string]any{{"role": "user", "content": "old"}}
	return c
}

// Last call the client recorded for a method
func lastCallOf(client *scriptedClient, method string) map[string]any {
	calls := client.callsOf(method)
	if len(calls) == 0 {
		return nil
	}
	return calls[len(calls)-1]
}

func TestRollbackListsAndRestoresThroughTheGateway(t *testing.T) {
	rec := recordPrints(t)
	t.Setenv("TERMINAL_CWD", t.TempDir())
	client, conn := rollbackGateway(t, map[string]any{
		"rollback.list": map[string]any{"enabled": true, "checkpoints": []any{
			map[string]any{"hash": "3101637abcdef", "timestamp": "2026-10-13T11:25:00+00:00", "message": "before write_file"}}},
		"rollback.restore": map[string]any{"success": true, "restored_to": "3101637", "reason": "before write_file", "history_removed": 2},
		"session.resume":   map[string]any{"revision": 3, "execution_generation": 1, "messages": []any{}}})
	c := rollbackCLI()
	gatewayRollback(c, conn, "/rollback")
	if out := printedText(rec); !strings.Contains(out, "1. 3101637") || !strings.Contains(out, "before write_file") {
		t.Fatalf("list %q", out)
	}
	gatewayRollback(c, conn, "/rollback 1")
	if call := lastCallOf(client, "rollback.restore"); !sameJSON(call, map[string]any{"session_id": "local-rb", "hash": "1"}) {
		t.Fatalf("restore %v", call)
	}
	if len(c.conversationHistory) != 0 {
		t.Fatalf("history %v", c.conversationHistory)
	}
}

func TestRollbackAllReachesTheGatewayAndKeptEditsAreShown(t *testing.T) {
	rec := recordPrints(t)
	t.Setenv("TERMINAL_CWD", t.TempDir())
	client, conn := rollbackGateway(t, map[string]any{
		"rollback.restore": map[string]any{"success": true, "restored_to": "3101637", "reason": "before write_file",
			"skipped_user_edits": []any{"notes.md"}}})
	c := rollbackCLI()
	gatewayRollback(c, conn, "/rollback 1")
	if call := lastCallOf(client, "rollback.restore"); !sameJSON(call, map[string]any{"session_id": "local-rb", "hash": "1"}) {
		t.Fatalf("restore %v", call)
	}
	if out := printedText(rec); !strings.Contains(out, "notes.md") || !strings.Contains(out, "--all") {
		t.Fatalf("output %q", out)
	}
	gatewayRollback(c, conn, "/rollback 1 --all")
	if call := lastCallOf(client, "rollback.restore"); !sameJSON(call, map[string]any{"session_id": "local-rb", "hash": "1", "all": true}) {
		t.Fatalf("restore all %v", call)
	}
	gatewayRollback(c, conn, "/rollback 1 a.txt --force")
	if call := lastCallOf(client, "rollback.restore"); !sameJSON(call, map[string]any{"session_id": "local-rb", "hash": "1", "file_path": "a.txt"}) {
		t.Fatalf("restore file %v", call)
	}
}

func TestClassicCliMoaTurnRunsOnTheMoaRoute(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := &scriptedClient{script: []any{scriptFrame("message.complete", map[string]any{"text": "ok", "status": "complete"})}}
	conn := scriptedConnection(t, client)
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{Model: "moa-default", Provider: "moa", BaseURL: "moa://local", APIMode: "chat_completions"})
	if result := runTurn(t, a, "one prompt through MoA"); result["failed"] == true {
		t.Fatalf("result %v", result)
	}
	var routed []any
	for _, params := range client.callsOf("session.mutate") {
		if params["operation"] == "model" {
			if payload, _ := params["payload"].(map[string]any); payload["model"] == "moa-default" {
				routed = append(routed, payload)
			}
		}
	}
	for _, params := range client.callsOf("prompt.submit") {
		if override, _ := params["model_override"].(map[string]any); override != nil && override["model"] == "moa-default" {
			routed = append(routed, override)
		}
	}
	if len(routed) == 0 {
		t.Fatalf("the /moa turn ran the session model, not the MoA preset: %v", client.calls)
	}
}

func TestClassicCliModelSwitchReachesTheGatewaySession(t *testing.T) {
	rec := recordPrints(t)
	client := &scriptedClient{script: []any{scriptFrame("message.complete", map[string]any{"text": "ok", "status": "complete"})}}
	info := map[string]any{"model": "old/model", "provider": "openrouter"}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "session.resume" {
			return nil, false, nil
		}
		client.record(method, params)
		return map[string]any{"revision": 7, "execution_generation": 3, "info": info, "messages": []any{},
			"stored_session_id": scriptSID, "session_id": scriptSID, "prompts": []any{}, "pending": []any{}}, true, nil
	}
	conn := scriptedConnection(t, client)
	c := bareCLI()
	c.model, c.provider, c.requestedProvider, c.apiKey, c.explicitAPIKey = "old/model", "openrouter", "openrouter", "sk-old", "sk-old"
	c.baseURL, c.explicitBaseURL, c.apiMode = "https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1", "chat_completions"
	c.sessionID = scriptSID
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{Model: "old/model", Provider: "openrouter"})
	c.agent = a
	prev := switchModelResolve
	t.Cleanup(func() { switchModelResolve = prev })
	switchModelResolve = func(context.Context, provider.SwitchModelOptions) (provider.ModelSwitchResult, error) {
		return provider.ModelSwitchResult{Success: true, NewModel: "claude-sonnet-4.6", TargetProvider: "anthropic", APIKey: "sk-ant",
			BaseURL: "https://api.anthropic.com", APIMode: "anthropic_messages", ProviderLabel: "Anthropic"}, nil
	}
	c.handleModelSwitch("/model claude-sonnet-4.6 --provider anthropic")
	var failed []string
	for _, line := range rec.plain() {
		if strings.Contains(strings.ToLower(line), "fail") {
			failed = append(failed, line)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("/model printed a failure %v", failed)
	}
	if c.model != "claude-sonnet-4.6" {
		t.Fatalf("model %q", c.model)
	}
	runTurn(t, a, "next turn")
	var routes []any
	for _, params := range client.callsOf("session.mutate") {
		if params["operation"] == "model" {
			routes = append(routes, params["payload"])
		}
	}
	for _, params := range client.callsOf("prompt.submit") {
		if override, present := params["model_override"]; present {
			routes = append(routes, override)
		}
	}
	if len(routes) == 0 {
		t.Fatalf("the next turn runs the old model: nothing reached the gateway %v", client.calls)
	}
}
