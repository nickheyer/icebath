package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Asker recording every question the CLI approval panel would show and selecting answer
type recordingAsker struct {
	mu     sync.Mutex
	asked  [][]tools.Question
	answer string
}

func (a *recordingAsker) Ask(_ context.Context, q []tools.Question) (tools.FormResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, q)
	return tools.FormResult{Outcome: "answered", Answers: []tools.Answer{{Selected: []string{a.answer}}}}, nil
}

// Env carrying gateway session values the approval context reads
func sessionEnv(values map[string]any) *tools.Env {
	env := &tools.Env{Values: &sync.Map{}}
	for k, v := range values {
		env.Values.Store("session."+k, v)
	}
	return env
}

func TestTrustGateApprovalCardRedactsWithRedactionOff(t *testing.T) {
	_, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("ok"), nil })
	serverTrustLevels[ServerKey{Name: "srv"}] = TrustUntrusted
	asked := fakeConsent(t, "decline")
	call := toolCall(map[string]any{
		"job":         "release",
		"api_key":     "sk-supersecretcredential123456",
		"db_password": "hunter2-correct-horse-battery",
		"headers": map[string]any{"Authorization": "Bearer eyJhbGciOiJIUzI1NiJ9.cGF5bG9hZA.c2lnbmF0dXJl",
			"x-api-key": "opaqueheadervalue77"},
		"password":    float64(987654321),
		"credentials": []any{"opaque-credential-value"},
		"target":      "prod‮",
		"notes":       strings.Repeat("x", 5000),
	})
	call.Env.Config = config.Tree{"security": map[string]any{"redact_secrets": false}}
	makeToolHandler("srv", "trigger_build", 30, false)(context.Background(), call)
	if len(*asked) != 1 {
		t.Fatalf("asked %d", len(*asked))
	}
	message := (*asked)[0].Title
	if !strings.Contains(message, `"job": "release"`) || !strings.Contains(message, "truncated") || len([]rune(message)) >= 2000 {
		t.Fatalf("message %d runes %q", len([]rune(message)), message)
	}
	for _, secret := range []string{"sk-supersecretcredential123456", "hunter2-correct-horse-battery",
		"eyJhbGciOiJIUzI1NiJ9.cGF5bG9hZA.c2lnbmF0dXJl", "opaqueheadervalue77", "987654321", "opaque-credential-value", "‮"} {
		if strings.Contains(message, secret) {
			t.Fatalf("secret %q leaked into %q", secret, message)
		}
	}
	if tr.count(methodToolsCall) != 0 {
		t.Fatal("declined call reached the server")
	}
}

func TestTrustGateUnconfiguredServerDefaultsToFullTrust(t *testing.T) {
	handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("ok"), nil })
	asked := fakeConsent(t, "decline")
	out := makeToolHandler("srv", "delete_repo", 30, false)(context.Background(), toolCall(map[string]any{"repo": "x"}))
	if got := decodeResult(t, out); len(got) != 1 || got["result"] != "ok" || len(*asked) != 0 {
		t.Fatalf("out %s asked %d", out, len(*asked))
	}
}

func TestTrustGateReadOnlyFalseHintIsGated(t *testing.T) {
	_, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("ok"), nil })
	key := ServerKey{Name: "srv"}
	serverTrustLevels[key] = TrustUntrusted
	toolReadOnlyHints[key] = map[string]bool{"write_file": false}
	asked := fakeConsent(t, "decline")
	makeToolHandler("srv", "write_file", 30, false)(context.Background(), toolCall(map[string]any{"path": "/etc/passwd"}))
	if len(*asked) != 1 || tr.count(methodToolsCall) != 0 {
		t.Fatalf("asked %d calls %d", len(*asked), tr.count(methodToolsCall))
	}
}

func TestTrustGateAPIRunCallbackResolvesConsent(t *testing.T) {
	const sessionKey = "api-run-mcp-trust"
	env := sessionEnv(map[string]any{"platform": "api_server", "key": sessionKey})
	asker := &recordingAsker{answer: "once"}
	env.Ask = asker
	var mu sync.Mutex
	var seen []map[string]any
	tools.RegisterGatewayNotify(sessionKey, func(data map[string]any) error {
		mu.Lock()
		seen = append(seen, data)
		mu.Unlock()
		if n := tools.ResolveGatewayApproval(sessionKey, "once", false, "", data["request_id"].(string)); n != 1 {
			t.Errorf("resolved %d approvals", n)
		}
		return nil
	})
	t.Cleanup(func() { tools.UnregisterGatewayNotify(sessionKey) })
	got := requestTrustConsent(context.Background(), env, "MCP tool 'write' on UNTRUSTED server 'srv' wants to run.",
		"Approve once or deny.", tools.ElicitationOptions{Surface: "mcp-trust/srv"})
	if got != "accept" {
		t.Fatalf("consent %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0]["pattern_key"] != "mcp_elicitation" || seen[0]["request_id"] == "" || len(asker.asked) != 0 {
		t.Fatalf("seen %v cli asks %d", seen, len(asker.asked))
	}
}

func TestTrustGateUnattendedContextsSkipCallback(t *testing.T) {
	for _, tc := range []struct {
		platform    string
		cron        bool
		singleQuery bool
	}{{"webhook", false, false}, {"telegram", true, false}, {"api_server", false, true}, {"local", false, true}} {
		t.Run(tc.platform, func(t *testing.T) {
			sessionKey := tc.platform + "-mcp-trust"
			env := sessionEnv(map[string]any{"platform": tc.platform, "key": sessionKey, "cron": tc.cron, "single_query": tc.singleQuery})
			asker := &recordingAsker{answer: "once"}
			env.Ask = asker
			var mu sync.Mutex
			notified := 0
			tools.RegisterGatewayNotify(sessionKey, func(map[string]any) error {
				mu.Lock()
				notified++
				mu.Unlock()
				return nil
			})
			t.Cleanup(func() { tools.UnregisterGatewayNotify(sessionKey) })
			if got := requestTrustConsent(context.Background(), env, "write", "Approve once or deny.", tools.ElicitationOptions{}); got != "decline" {
				t.Fatalf("consent %q", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if notified != 0 || len(asker.asked) != 0 {
				t.Fatalf("notified %d cli asks %d", notified, len(asker.asked))
			}
		})
	}
}

func TestTrustGateCLIPanelNeverAskedWithoutUser(t *testing.T) {
	for _, context_ := range []string{"single_query", "cron", "webhook"} {
		t.Run(context_, func(t *testing.T) {
			env := sessionEnv(nil)
			switch context_ {
			case "single_query":
				t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
			case "cron":
				env.Values.Store("session.cron", true)
			default:
				env.Values.Store("session.platform", "webhook")
			}
			asker := &recordingAsker{answer: "once"}
			env.Ask = asker
			if got := requestTrustConsent(context.Background(), env, "write", "Approve once or deny.", tools.ElicitationOptions{}); got != "decline" {
				t.Fatalf("consent %q", got)
			}
			if len(asker.asked) != 0 {
				t.Fatalf("cli panel asked %d times", len(asker.asked))
			}
		})
	}
}

func TestTrustGateArgumentsFollowTheEmittedKeyOrder(t *testing.T) {
	args := map[string]any{"zeta": map[string]any{"y": json.Number("3"), "x": json.Number("4")}, "alpha": []any{map[string]any{"d": json.Number("1"), "c": json.Number("2")}}, "added": true}
	raw := `{"zeta": {"x": 1, "y": 2}, "alpha": [{"d": 1, "c": 2}], "zeta": {"y": 3, "x": 4}}`
	want := "{\n  \"zeta\": {\n    \"y\": 3,\n    \"x\": 4\n  },\n  \"alpha\": [\n    {\n      \"d\": 1,\n      \"c\": 2\n    }\n  ],\n  \"added\": true\n}"
	if got := formatTrustGateArguments(&tools.Env{}, args, raw); got != want {
		t.Fatalf("got %q", got)
	}
	if got := formatTrustGateArguments(&tools.Env{}, map[string]any{"b": true, "a": false}, "not json"); got != "{\n  \"a\": false,\n  \"b\": true\n}" {
		t.Fatalf("undecodable raw text %q", got)
	}
}
