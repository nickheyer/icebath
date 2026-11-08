package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"gopkg.in/yaml.v3"
)

type sqCall struct {
	pluginID, prompt string
	timeout          float64
	capture          *sideQuestionCapture
}

// Temp home with the recorded executor in place of the side request and the redeem service installed
func sqExecutor(t *testing.T) *[]sqCall {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	calls := &[]sqCall{}
	prevAsk := askSideQuestion
	askSideQuestion = func(_ context.Context, capture *sideQuestionCapture, prompt, pluginID string, timeout float64) *string {
		*calls = append(*calls, sqCall{pluginID, prompt, timeout, capture})
		answer := "answer for " + pluginID
		return &answer
	}
	plugins.InstallServices(&plugins.Services{RedeemSideQuestion: RedeemSideQuestion})
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	restoreInterpreter := plugins.SwapHostInterpreter(func(context.Context) (string, error) { return python, nil })
	t.Cleanup(func() {
		plugins.ResetPluginManagersForTests()
		restoreInterpreter()
		askSideQuestion = prevAsk
		plugins.InstallServices(nil)
		sideQuestionLeaseMu.Lock()
		clear(sideQuestionLeases)
		sideQuestionLeaseMu.Unlock()
	})
	return calls
}

func sqWriteYAML(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Plugin directory whose manifest declares llm.side_question or nothing
func sqInstallPlugin(t *testing.T, name string, declares bool) {
	manifest := map[string]any{"name": name}
	if declares {
		manifest["capabilities"] = []string{sideQuestionCapabilityID}
	}
	dir := filepath.Join(os.Getenv("ICEBATH_HOME"), "plugins", name)
	sqWriteYAML(t, filepath.Join(dir, "plugin.yaml"), manifest)
	body := "import json, os, pathlib\n\ndef register(ctx):\n    def _on_post_llm_call(**kwargs):\n        pass\n    ctx.register_hook('post_llm_call', _on_post_llm_call)\n"
	if err := os.WriteFile(filepath.Join(dir, "__init__.py"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Writes the enabled and granted plugin config and rediscovers the active manager
func sqLoad(t *testing.T, enabled, granted []string) *plugins.Manager {
	entries := map[string]any{}
	for _, name := range granted {
		entries[name] = map[string]any{"granted_capabilities": []string{sideQuestionCapabilityID}}
	}
	sqWriteYAML(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"),
		map[string]any{"plugins": map[string]any{"enabled": enabled, "entries": entries}})
	m := plugins.GetPluginManager(context.Background())
	m.DiscoverAndLoad(context.Background(), true)
	return m
}

func sqConsented(t *testing.T) *plugins.Manager {
	sqInstallPlugin(t, "next-prompt", true)
	sqInstallPlugin(t, "bystander", false)
	return sqLoad(t, []string{"next-prompt", "bystander"}, []string{"next-prompt"})
}

func sqCtx(m *plugins.Manager, name string) *plugins.Context {
	return plugins.NewContext(&plugins.Manifest{Name: name, Key: name}, m)
}

func sqAgent(turnID string) *AIAgent {
	return &AIAgent{cachedSystemPrompt: "sys", tools: []any{}, sessionID: "sess", currentTurnID: turnID}
}

func sqIssue(t *testing.T, a *AIAgent, messages []map[string]any, turnID string) any {
	t.Helper()
	token, err := issueSideQuestionToken(context.Background(), a, messages, turnID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func sqRedeem(t *testing.T, token, prompt any, pluginID string, timeout any) *string {
	t.Helper()
	answer, err := RedeemSideQuestion(context.Background(), token, prompt, pluginID, "", timeout)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

func sqAsk(t *testing.T, c *plugins.Context, token any) *string {
	t.Helper()
	answer, err := c.SideQuestion(context.Background(), token, "q", 15.0)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

func sqIsAnswer(got *string, want string) bool { return got != nil && *got == want }

func TestTokenOnlyForADeclaredAndGrantedPlugin(t *testing.T) {
	sqExecutor(t)
	sqInstallPlugin(t, "declares-only", true)
	sqInstallPlugin(t, "granted-only", false)
	sqLoad(t, []string{"declares-only", "granted-only"}, []string{"granted-only"})
	if tok := sqIssue(t, sqAgent("turn-1"), nil, "turn-1"); tok != nil {
		t.Fatalf("token %v without a declared and granted plugin", tok)
	}
	sqLoad(t, []string{"declares-only", "granted-only"}, []string{"declares-only", "granted-only"})
	if _, ok := sqIssue(t, sqAgent("turn-1"), nil, "turn-1").(string); !ok {
		t.Fatal("no token for a declared and granted plugin")
	}
}

func TestNoTokenWhenTheConsentedPluginIsDisabled(t *testing.T) {
	sqExecutor(t)
	sqInstallPlugin(t, "next-prompt", true)
	sqLoad(t, []string{}, []string{"next-prompt"})
	if tok := sqIssue(t, sqAgent("turn-1"), nil, "turn-1"); tok != nil {
		t.Fatalf("token %v", tok)
	}
}

func TestNoTokenForAPersistDisabledFork(t *testing.T) {
	sqExecutor(t)
	sqConsented(t)
	a := sqAgent("turn-1")
	a.persistDisabled = true
	if tok := sqIssue(t, a, nil, "turn-1"); tok != nil {
		t.Fatalf("token %v", tok)
	}
}

func TestNoTokenForACronRun(t *testing.T) {
	sqExecutor(t)
	sqConsented(t)
	cron, telegram := sqAgent("turn-1"), sqAgent("turn-1")
	cron.platform, telegram.platform = "cron", "telegram"
	if tok := sqIssue(t, cron, nil, "turn-1"); tok != nil {
		t.Fatalf("cron token %v", tok)
	}
	if _, ok := sqIssue(t, telegram, nil, "turn-1").(string); !ok {
		t.Fatal("no telegram token")
	}
}

func TestNoTokenForASubagentTurn(t *testing.T) {
	sqExecutor(t)
	sqConsented(t)
	child, top := sqAgent("turn-1"), sqAgent("turn-1")
	child.delegateDepth = 1
	if tok := sqIssue(t, child, nil, "turn-1"); tok != nil {
		t.Fatalf("subagent token %v", tok)
	}
	if _, ok := sqIssue(t, top, nil, "turn-1").(string); !ok {
		t.Fatal("no top level token")
	}
}

func TestCaptureIsFrozenAtIssue(t *testing.T) {
	calls := sqExecutor(t)
	sqConsented(t)
	messages := []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello"}}
	a := sqAgent("turn-1")
	token := sqIssue(t, a, messages, "turn-1")
	messages[0]["content"] = "edited"
	messages = append(messages, map[string]any{"role": "user", "content": "later"})
	sqRedeem(t, token, "q", "next-prompt", 15.0)
	want := []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "hello"}}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0].capture.messages, want) {
		t.Fatalf("calls %+v", *calls)
	}
	runtime.KeepAlive(a)
}

func TestDeniedPluginGetsNoneAndLeavesTheLease(t *testing.T) {
	calls := sqExecutor(t)
	m := sqConsented(t)
	a := sqAgent("turn-1")
	token := sqIssue(t, a, nil, "turn-1")
	if got := sqAsk(t, sqCtx(m, "bystander"), token); got != nil || len(*calls) != 0 {
		t.Fatalf("bystander got %v, calls %d", got, len(*calls))
	}
	if got := sqAsk(t, sqCtx(m, "next-prompt"), token); !sqIsAnswer(got, "answer for next-prompt") {
		t.Fatalf("consented plugin got %v", got)
	}
	runtime.KeepAlive(a)
}

func TestOneAttemptPerTurn(t *testing.T) {
	calls := sqExecutor(t)
	sqConsented(t)
	a := sqAgent("turn-1")
	token := sqIssue(t, a, nil, "turn-1")
	if got := sqRedeem(t, token, "q", "next-prompt", 15.0); !sqIsAnswer(got, "answer for next-prompt") {
		t.Fatalf("first %v", got)
	}
	if got := sqRedeem(t, token, "q", "other", 15.0); got != nil || len(*calls) != 1 {
		t.Fatalf("second %v, calls %d", got, len(*calls))
	}
	runtime.KeepAlive(a)
}

func TestANewLeaseForTheSameAgentDropsTheOldOne(t *testing.T) {
	sqExecutor(t)
	sqConsented(t)
	a := sqAgent("turn-1")
	old := sqIssue(t, a, nil, "turn-1")
	a.currentTurnID = "turn-2"
	fresh := sqIssue(t, a, nil, "turn-2")
	if got := sqRedeem(t, old, "q", "next-prompt", 15.0); got != nil {
		t.Fatalf("old lease answered %v", *got)
	}
	if got := sqRedeem(t, fresh, "q", "next-prompt", 15.0); !sqIsAnswer(got, "answer for next-prompt") {
		t.Fatalf("new lease %v", got)
	}
}

func TestManyLiveSessionsKeepTheirTokens(t *testing.T) {
	sqExecutor(t)
	m := sqConsented(t)
	parents := make([]*AIAgent, 40)
	tokens := make([]any, 40)
	for i := range parents {
		parents[i] = sqAgent("turn-1")
		parents[i].sessionID = fmt.Sprintf("sess-%d", i)
		tokens[i] = sqIssue(t, parents[i], nil, "turn-1")
	}
	if got := sqAsk(t, sqCtx(m, "next-prompt"), tokens[0]); !sqIsAnswer(got, "answer for next-prompt") {
		t.Fatalf("first session lost its token, got %v", got)
	}
	runtime.KeepAlive(parents)
}

func TestExpiredLeaseIsNone(t *testing.T) {
	calls := sqExecutor(t)
	sqConsented(t)
	a := sqAgent("turn-1")
	token := sqIssue(t, a, nil, "turn-1")
	real := sideQuestionNow
	sideQuestionNow = func() float64 { return real() + sideQuestionLeaseTTLSeconds + 1 }
	t.Cleanup(func() { sideQuestionNow = real })
	if got := sqRedeem(t, token, "q", "next-prompt", 15.0); got != nil || len(*calls) != 0 {
		t.Fatalf("expired lease answered %v, calls %d", got, len(*calls))
	}
	runtime.KeepAlive(a)
}

func TestLeaseDiesWhenTheParentStartsItsNextTurn(t *testing.T) {
	calls := sqExecutor(t)
	sqConsented(t)
	a := sqAgent("turn-1")
	token := sqIssue(t, a, nil, "turn-1")
	a.currentTurnID = "turn-2"
	if got := sqRedeem(t, token, "q", "next-prompt", 15.0); got != nil || len(*calls) != 0 {
		t.Fatalf("moved on lease answered %v, calls %d", got, len(*calls))
	}
}

func TestLeaseDiesWithItsParent(t *testing.T) {
	calls := sqExecutor(t)
	sqConsented(t)
	token := func() any { return sqIssue(t, sqAgent("turn-1"), nil, "turn-1") }()
	runtime.GC()
	runtime.GC()
	if got := sqRedeem(t, token, "q", "next-prompt", 15.0); got != nil || len(*calls) != 0 {
		t.Fatalf("orphaned lease answered %v, calls %d", got, len(*calls))
	}
}

func TestBadPromptIsNoneAndConsumesTheLease(t *testing.T) {
	for _, prompt := range []any{strings.Repeat("x", sideQuestionMaxPromptChars+1), "", "   ", nil, 42} {
		calls := sqExecutor(t)
		sqConsented(t)
		a := sqAgent("turn-1")
		token := sqIssue(t, a, nil, "turn-1")
		if got := sqRedeem(t, token, prompt, "next-prompt", 15.0); got != nil {
			t.Fatalf("prompt %v answered %v", prompt, *got)
		}
		if got := sqRedeem(t, token, "fine", "next-prompt", 15.0); got != nil || len(*calls) != 0 {
			t.Fatalf("prompt %v left the lease, calls %d", prompt, len(*calls))
		}
		runtime.KeepAlive(a)
	}
}

func TestAnotherProfileCannotRedeemTheLease(t *testing.T) {
	calls := sqExecutor(t)
	m := sqConsented(t)
	other := filepath.Join(t.TempDir(), "profiles", "other")
	sqWriteYAML(t, filepath.Join(other, "config.yaml"), map[string]any{"plugins": map[string]any{
		"enabled": []string{"next-prompt"}, "entries": map[string]any{"next-prompt": map[string]any{"granted_capabilities": []string{sideQuestionCapabilityID}}}}})
	foreign := plugins.NewContext(&plugins.Manifest{Name: "next-prompt", Key: "next-prompt"}, plugins.NewManager(other))
	a := sqAgent("turn-1")
	token := sqIssue(t, a, nil, "turn-1")
	if got := sqAsk(t, foreign, token); got != nil {
		t.Fatalf("foreign profile got %v", *got)
	}
	if got, err := RedeemSideQuestion(context.Background(), token, "q", "next-prompt", other, 15.0); err != nil || got != nil || len(*calls) != 0 {
		t.Fatalf("foreign home redeemed %v %v, calls %d", got, err, len(*calls))
	}
	if got := sqAsk(t, sqCtx(m, "next-prompt"), token); !sqIsAnswer(got, "answer for next-prompt") {
		t.Fatalf("owner lost the lease, got %v", got)
	}
	runtime.KeepAlive(a)
}

func TestTimeoutIsClamped(t *testing.T) {
	for _, tc := range []struct {
		given any
		used  float64
	}{{0.5, 0.5}, {600, 60.0}, {0, 15.0}, {-3, 15.0}, {math.NaN(), 15.0}, {"x", 15.0}} {
		calls := sqExecutor(t)
		sqConsented(t)
		a := sqAgent("turn-1")
		token := sqIssue(t, a, nil, "turn-1")
		sqRedeem(t, token, "q", "next-prompt", tc.given)
		if len(*calls) != 1 || (*calls)[0].timeout != tc.used {
			t.Fatalf("timeout %v used %+v", tc.given, *calls)
		}
		runtime.KeepAlive(a)
	}
}
