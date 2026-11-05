package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Server srv answering ok with the fixture's costs recorded under the given trust
func costedServer(t *testing.T, trust string, limit int) *handlerTransport {
	t.Helper()
	_, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("ok"), nil })
	cfg := costedConfig()
	cfg["trust"] = trust
	if limit > 0 {
		cfg["spend_limit"] = limit
	}
	list := []*Tool{{Name: "paid_render"}, {Name: "batch"}, {Name: "free_status"}}
	if err := recordToolTrustMetadata(context.Background(), "srv", cfg, list, nil); err != nil {
		t.Fatal(err)
	}
	return tr
}

// Env whose approval session key names one chat
func chatEnv(chat string) *tools.Env {
	env := &tools.Env{Values: &sync.Map{}}
	env.Values.Store("approval.session_key", chat)
	return env
}

// Runs one costed handler call in a chat and returns the decoded result and the prompts it raised
func costCall(t *testing.T, ctx context.Context, tool, chat, answer string, args map[string]any) (map[string]any, []string) {
	t.Helper()
	prev := requestTrustConsent
	asked := fakeConsent(t, answer)
	raw := makeToolHandler("srv", tool, 30, false)(ctx, &tools.Call{Name: "mcp_srv_" + tool, Args: args, Env: chatEnv(chat)})
	requestTrustConsent = prev
	var titles []string
	for _, o := range *asked {
		titles = append(titles, o.Title)
	}
	return decodeResult(t, raw), titles
}

func TestCostsUntrustedPromptNamesCostAndRunningTotalAndDeclinesAddNothing(t *testing.T) {
	tr := costedServer(t, "untrusted", 0)
	ctx := context.Background()
	_, asked := costCall(t, ctx, "paid_render", "chat-a", "decline", nil)
	if !strings.Contains(asked[0], "Costs about 40 credits.") || strings.Contains(asked[0], "used about") {
		t.Fatalf("first prompt %q", asked[0])
	}
	result, asked := costCall(t, ctx, "paid_render", "chat-a", "accept", nil)
	if len(result) != 1 || result["result"] != "ok" || strings.Contains(asked[0], "used about") {
		t.Fatalf("declined call added spend %v %q", result, asked[0])
	}
	_, asked = costCall(t, ctx, "batch", "chat-a", "accept", nil)
	if !strings.Contains(asked[0], "Costs 5-20 credits (per item). This chat has used about 40 credits on 'srv' (limit 100).") {
		t.Fatalf("batch prompt %q", asked[0])
	}
	_, asked = costCall(t, ctx, "free_status", "chat-a", "accept", nil)
	want := "MCP tool 'free_status' on UNTRUSTED server 'srv' wants to run. This tool is write-capable (no readOnlyHint=true annotation) and may modify external state."
	if len(asked) != 1 || asked[0] != want {
		t.Fatalf("undeclared tool prompt %q", asked)
	}
	if n := tr.count(methodToolsCall); n != 3 {
		t.Fatalf("dispatched %d want 3", n)
	}
}

func TestCostsFullTrustAsksOnlyPastTheLimitAndUnattendedRefusesWithoutDispatch(t *testing.T) {
	tr := costedServer(t, "full", 0)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		result, asked := costCall(t, ctx, "paid_render", "chat-a", "accept", nil)
		if result["result"] != "ok" || len(asked) != 0 {
			t.Fatalf("below the limit %v %q", result, asked)
		}
	}
	if _, asked := costCall(t, ctx, "free_status", "chat-a", "accept", nil); len(asked) != 0 {
		t.Fatalf("free tool asked %q", asked)
	}
	result, asked := costCall(t, ctx, "paid_render", "chat-a", "decline", nil)
	if !strings.Contains(asked[0], "would take this chat past its spend limit") ||
		!strings.Contains(asked[0], "This chat has used about 80 credits on 'srv' (limit 100).") {
		t.Fatalf("over limit prompt %q", asked[0])
	}
	errText, _ := result["error"].(string)
	if !strings.Contains(errText, "spend limit is 100 credits") || !strings.Contains(errText, "Do not retry") {
		t.Fatalf("refusal %q", errText)
	}
	if result, _ = costCall(t, ctx, "paid_render", "chat-a", "accept", nil); result["result"] != "ok" {
		t.Fatalf("approved past the limit %v", result)
	}
	if _, asked = costCall(t, ctx, "batch", "chat-a", "accept", nil); !strings.Contains(asked[0], "used about 120 credits") {
		t.Fatalf("next paid call %q", asked)
	}
	dispatched := tr.count(methodToolsCall)
	env := chatEnv("chat-a")
	env.Values.Store("session.cron", true)
	raw := makeToolHandler("srv", "paid_render", 30, false)(ctx, &tools.Call{Name: "mcp_srv_paid_render", Env: env})
	errText, _ = decodeResult(t, raw)["error"].(string)
	if !strings.Contains(errText, "used about 140 credits") || tr.count(methodToolsCall) != dispatched {
		t.Fatalf("unattended %q dispatched %d of %d", errText, tr.count(methodToolsCall), dispatched)
	}
}

func TestCostsOverLimitUntrustedPromptPutsNoticeAndCostBeforeArguments(t *testing.T) {
	tr := costedServer(t, "untrusted", 30)
	asked := fakeConsent(t, "decline")
	notes := strings.Repeat("y", 3000)
	args := map[string]any{"scene": "castle", "notes": notes}
	raw := `{"scene": "castle", "notes": "` + notes + `"}`
	makeToolHandler("srv", "paid_render", 30, false)(context.Background(), &tools.Call{Name: "mcp_srv_paid_render", Args: args, RawArgs: raw, Env: chatEnv("chat-a")})
	if len(*asked) != 1 {
		t.Fatalf("prompts %d", len(*asked))
	}
	text := (*asked)[0].Title
	notice := strings.Index(text, "goes past the chat's spend limit")
	cost := strings.Index(text, "Costs about 40 credits.")
	arg := strings.Index(text, `"scene": "castle"`)
	if !(-1 < notice && notice < arg && -1 < cost && cost < arg) {
		t.Fatalf("order notice %d cost %d arg %d in %q", notice, cost, arg, text)
	}
	if tr.count(methodToolsCall) != 0 {
		t.Fatal("declined call dispatched")
	}
}

func TestCostsChatsNeverShareATallyThroughTheHandler(t *testing.T) {
	costedServer(t, "full", 0)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		costCall(t, ctx, "paid_render", "chat-a", "accept", nil)
	}
	if _, asked := costCall(t, ctx, "paid_render", "chat-a", "accept", nil); len(asked) == 0 {
		t.Fatal("chat-a at its limit did not ask")
	}
	if _, asked := costCall(t, ctx, "paid_render", "chat-b", "accept", nil); len(asked) != 0 {
		t.Fatalf("chat-b asked %q", asked)
	}
}

func TestCostsChatKeepsItsTotalAcrossARotationAndEachCronRunHasItsOwn(t *testing.T) {
	costedServer(t, "full", 0)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		costCall(t, ctx, "paid_render", "parent", "accept", nil)
	}
	tools.TransferSessionYolo("parent", "child")
	if _, asked := costCall(t, ctx, "paid_render", "child", "decline", nil); len(asked) == 0 || !strings.Contains(asked[0], "used about 80 credits") {
		t.Fatalf("rotated chat prompt %q", asked)
	}
	for _, job := range []string{"job-a", "job-b", "job-c"} {
		runCtx := BindRun(ctx, job+":run-1")
		env := chatEnv("")
		env.Values.Store("session.cron", true)
		for i := 0; i < 2; i++ {
			raw := makeToolHandler("srv", "paid_render", 30, false)(runCtx, &tools.Call{Name: "mcp_srv_paid_render", Env: env})
			if got := decodeResult(t, raw); len(got) != 1 || got["result"] != "ok" {
				t.Fatalf("%s call %d %v", job, i, got)
			}
		}
	}
}

func TestCostsParallelOverLimitCallsAreAskedOneAtATime(t *testing.T) {
	costedServer(t, "full", 0)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		costCall(t, ctx, "paid_render", "chat-a", "accept", nil)
	}
	var mu sync.Mutex
	open, peak := 0, 0
	var prompts []string
	arrived := make(chan struct{}, 2)
	prev := requestTrustConsent
	requestTrustConsent = func(_ context.Context, _ *tools.Env, message, _ string, _ tools.ElicitationOptions) string {
		mu.Lock()
		open++
		peak = max(peak, open)
		prompts = append(prompts, message)
		mu.Unlock()
		arrived <- struct{}{}
		select {
		case <-arrived:
		case <-time.After(200 * time.Millisecond):
		}
		mu.Lock()
		open--
		mu.Unlock()
		return "accept"
	}
	t.Cleanup(func() { requestTrustConsent = prev })
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			makeToolHandler("srv", "paid_render", 30, false)(ctx, &tools.Call{Name: "mcp_srv_paid_render", Env: chatEnv("chat-a")})
		}()
	}
	wg.Wait()
	if peak != 1 || len(prompts) != 2 {
		t.Fatalf("peak %d prompts %d", peak, len(prompts))
	}
	first, second := strings.Contains(prompts[0], "used about 80 credits"), strings.Contains(prompts[1], "used about 80 credits")
	if first == second {
		t.Fatalf("the second prompt must see the first booking %q", prompts)
	}
}
