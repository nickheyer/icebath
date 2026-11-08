package agent

import (
	"context"
	"errors"
	"testing"
)

func nousWireResp(kv ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func nousWireAgent() *AIAgent {
	a := &AIAgent{provider: "nous", model: "anthropic/claude-fable-5.1", apiMode: "chat_completions", apiKey: "k", sessionID: "s"}
	a.setBaseURL("https://inference-api.nousresearch.com/v1")
	return a
}

// Installs the auto wire mode, the clearance flag and a recording switch double
func installNousWireHook(t *testing.T, mode string, cleared bool, fail bool) *[]string {
	t.Helper()
	prevMode, prevCleared, prevSwitch := nousAnthropicWireMode, gmiNativeWireCleared, switchAgentModel
	t.Cleanup(func() {
		nousAnthropicWireMode, gmiNativeWireCleared, switchAgentModel = prevMode, prevCleared, prevSwitch
	})
	var switches []string
	nousAnthropicWireMode = func(context.Context) string { return mode }
	gmiNativeWireCleared = cleared
	switchAgentModel = func(a *AIAgent, _ context.Context, _, _, _, _, apiMode string, _ map[string]bool) error {
		if fail {
			return errors.New("no client")
		}
		switches = append(switches, apiMode)
		a.apiMode = apiMode
		return nil
	}
	return &switches
}

func TestClassifierOpenrouterByProviderFieldOrGenID(t *testing.T) {
	for _, r := range []map[string]any{
		nousWireResp("provider", "Anthropic", "id", "gen-1788708403-xXjDsFw"),
		nousWireResp("provider", "Claude Platform on AWS", "id", "x"),
		nousWireResp("id", "gen-1788680469-hyks5Nj9n7rmdAbMyQ5x"),
	} {
		if got := classifyUpstream(r); got != "openrouter" {
			t.Fatalf("%v classified %q", r, got)
		}
	}
}

func TestClassifierGmiByAnthropicNativeIDWithoutProvider(t *testing.T) {
	if got := classifyUpstream(nousWireResp("id", "msg_01XrffvmxsRUWwCbVpBsADHo")); got != "gmi" {
		t.Fatalf("classified %q", got)
	}
}

func TestClassifierUnknownNeverClassifies(t *testing.T) {
	for _, r := range []any{nil, nousWireResp(), nousWireResp("id", nil), nousWireResp("id", "chatcmpl-abc"), nousWireResp("id", "", "provider", "")} {
		if got := classifyUpstream(r); got != "" {
			t.Fatalf("%v classified %q", r, got)
		}
	}
}

func TestWireChoiceChatForOpenrouterAndUnknown(t *testing.T) {
	if wireForUpstream("openrouter") != "chat_completions" || wireForUpstream("") != "chat_completions" {
		t.Fatal("chat expected")
	}
}

func TestWireChoiceGmiIsChatUntilClearedThenNative(t *testing.T) {
	installNousWireHook(t, "auto", false, false)
	if wireForUpstream("gmi") != "chat_completions" {
		t.Fatal("uncleared gmi must stay on chat")
	}
	gmiNativeWireCleared = true
	if wireForUpstream("gmi") != "anthropic_messages" {
		t.Fatal("cleared gmi goes native")
	}
}

func TestHookGmiClearedSchedulesOnceAndAppliesAtNextIteration(t *testing.T) {
	switches := installNousWireHook(t, "auto", true, false)
	ctx := context.Background()
	a := nousWireAgent()
	if !a.maybeSwitchWireAfterFirstResponse(ctx, nousWireResp("id", "msg_01abc"), 1) {
		t.Fatal("switch not scheduled")
	}
	if a.apiMode != "chat_completions" || len(*switches) != 0 {
		t.Fatal("switched while the response is consumed")
	}
	if !applyPendingWireSwitch(ctx, a) || a.apiMode != "anthropic_messages" || len(*switches) != 1 {
		t.Fatalf("apply %v %v", a.apiMode, *switches)
	}
	if applyPendingWireSwitch(ctx, a) {
		t.Fatal("nothing pending twice")
	}
	later := nousWireResp("provider", "Anthropic", "id", "gen-1-x")
	if a.maybeSwitchWireAfterFirstResponse(ctx, later, 2) || a.maybeSwitchWireAfterFirstResponse(ctx, later, 1) {
		t.Fatal("decision flipped after call one")
	}
	if len(*switches) != 1 || (*switches)[0] != "anthropic_messages" {
		t.Fatalf("switches %v", *switches)
	}
}

func TestHookOpenrouterStaysOnChat(t *testing.T) {
	switches := installNousWireHook(t, "auto", false, false)
	a := nousWireAgent()
	if a.maybeSwitchWireAfterFirstResponse(context.Background(), nousWireResp("provider", "Anthropic", "id", "gen-1-x"), 1) {
		t.Fatal("openrouter scheduled a switch")
	}
	if a.apiMode != "chat_completions" || len(*switches) != 0 {
		t.Fatal("wire moved")
	}
}

func TestHookGmiUnclearedStaysOnChat(t *testing.T) {
	switches := installNousWireHook(t, "auto", false, false)
	a := nousWireAgent()
	if a.maybeSwitchWireAfterFirstResponse(context.Background(), nousWireResp("id", "msg_01abc"), 1) {
		t.Fatal("uncleared gmi scheduled a switch")
	}
	if a.apiMode != "chat_completions" || len(*switches) != 0 {
		t.Fatal("wire moved")
	}
}

func TestHookExplicitModesNeverAutoSwitch(t *testing.T) {
	for _, mode := range []string{"chat", "native"} {
		t.Run(mode, func(t *testing.T) {
			switches := installNousWireHook(t, mode, true, false)
			a := nousWireAgent()
			if mode == "native" {
				a.apiMode = "anthropic_messages"
			}
			if a.maybeSwitchWireAfterFirstResponse(context.Background(), nousWireResp("id", "msg_01abc"), 1) || len(*switches) != 0 {
				t.Fatal("explicit mode switched")
			}
		})
	}
}

func TestHookOtherProvidersAndModelsUntouched(t *testing.T) {
	switches := installNousWireHook(t, "auto", true, false)
	ctx := context.Background()
	other := nousWireAgent()
	other.provider = "openrouter"
	gpt := nousWireAgent()
	gpt.model = "openai/gpt-5.6-sol"
	if other.maybeSwitchWireAfterFirstResponse(ctx, nousWireResp("id", "msg_01abc"), 1) || gpt.maybeSwitchWireAfterFirstResponse(ctx, nousWireResp("id", "msg_01abc"), 1) {
		t.Fatal("non nous anthropic route scheduled a switch")
	}
	if len(*switches) != 0 {
		t.Fatal("switched")
	}
}

func TestHookSwitchFailureIsSwallowedAndDecisionIsFinal(t *testing.T) {
	installNousWireHook(t, "auto", true, true)
	ctx := context.Background()
	a := nousWireAgent()
	if !a.maybeSwitchWireAfterFirstResponse(ctx, nousWireResp("id", "msg_01abc"), 1) {
		t.Fatal("switch not scheduled")
	}
	if applyPendingWireSwitch(ctx, a) {
		t.Fatal("failed switch reported success")
	}
	if a.apiMode != "chat_completions" || !a.nousWireDecided || a.nousWirePending != nil {
		t.Fatalf("state %v %v %v", a.apiMode, a.nousWireDecided, a.nousWirePending)
	}
}
