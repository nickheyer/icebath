package agent

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

const wireFakeHost = "https://api.wirefake.test/v1"

// Makes the fake profile's request headers hook raise the way the patched plugin method does
var wireFakeRequestHeadersBroken bool

// Registers the fake plugin profile whose wire seams answer only for its own host
func wireFakeProfile(t *testing.T) *[]string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	seenKeys := &[]string{}
	onHost := func(baseURL string) bool {
		ok, _ := pyurl.BaseURLHostMatches(baseURL, "wirefake.test")
		return ok
	}
	provider.RegisterProvider(provider.Profile{Name: "wirefake", BaseURL: wireFakeHost, Hostnames: []string{"wirefake.test"},
		APIModeForModelHook: func(model, apiKey string) provider.APIMode {
			*seenKeys = append(*seenKeys, apiKey)
			if strings.HasPrefix(model, "gpt-5-lite") {
				return "chat_completions"
			}
			return ""
		},
		ClientHeadersHook: func(baseURL, apiKey string, hasImages bool) map[string]string {
			if !onHost(baseURL) {
				return nil
			}
			headers := map[string]string{"X-Wire": "1"}
			if hasImages {
				headers["X-Wire-Images"] = "1"
			}
			return headers
		},
		RequestHeadersHook: func(baseURL, model string, userInitiated bool) map[string]string {
			if wireFakeRequestHeadersBroken {
				panic("plugin bug")
			}
			if userInitiated && onHost(baseURL) {
				return map[string]string{"X-Attr": "user"}
			}
			return map[string]string{}
		},
		MaxTokensParamHook: func(baseURL, model string) string {
			if onHost(baseURL) {
				return "max_completion_tokens"
			}
			return ""
		},
	})
	return seenKeys
}

func wireSeamAgent(t *testing.T, providerName, baseURL, model string) *AIAgent {
	t.Helper()
	a, err := NewAIAgent(context.Background(), AIAgentOptions{APIKey: "k", BaseURL: baseURL, Provider: providerName, Model: model,
		QuietMode: true, SkipContextFiles: true, SkipMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAPIModeForModelDecidesResponsesRoutingPerModel(t *testing.T) {
	seenKeys := wireFakeProfile(t)
	ctx := context.Background()
	if providerModelRequiresResponsesAPI(ctx, "gpt-5-lite", "wirefake") {
		t.Fatal("the profile keeps gpt-5-lite on chat completions")
	}
	if !providerModelRequiresResponsesAPI(ctx, "gpt-5.4", "wirefake") {
		t.Fatal("an empty answer defers to the generic gpt-5 rule")
	}
	if mode, ok := provider.ModelDerivedAPIMode(ctx, "wirefake", "gpt-5-lite", "tok"); !ok || mode != "chat_completions" {
		t.Fatalf("model derived api mode %q %v", mode, ok)
	}
	keys := *seenKeys
	if len(keys) == 0 || keys[len(keys)-1] != "tok" {
		t.Fatalf("seen keys %q", keys)
	}
	if !slices.Contains(keys, "") {
		t.Fatalf("routing never asked the credential free form %q", keys)
	}
}

func TestCustomRouteToProfileHostGetsClientHeadersAndMaxTokensParam(t *testing.T) {
	wireFakeProfile(t)
	ctx := context.Background()
	a := wireSeamAgent(t, "custom", wireFakeHost, "llama-3.1-8b")
	if got := stringHeaders(a.clientKwargs["default_headers"]); got["X-Wire"] != "1" {
		t.Fatalf("default headers %v", a.clientKwargs["default_headers"])
	}
	if got := a.maxTokensParam(ctx, 7); !reflect.DeepEqual(got, map[string]any{"max_completion_tokens": 7}) {
		t.Fatalf("max tokens param %v", got)
	}
	kwargs := map[string]any{"base_url": wireFakeHost, "default_headers": map[string]string{"x-wire": "user-set"}}
	ensureRouteClientHeaders(ctx, kwargs, "custom")
	if got := stringHeaders(kwargs["default_headers"]); !reflect.DeepEqual(got, map[string]string{"x-wire": "user-set"}) {
		t.Fatalf("user header overridden %v", got)
	}
	kwargs = map[string]any{"base_url": wireFakeHost}
	ensureRouteClientHeaders(ctx, kwargs, "custom")
	if got := stringHeaders(kwargs["default_headers"]); !reflect.DeepEqual(got, map[string]string{"X-Wire": "1"}) {
		t.Fatalf("missing endpoint header not added %v", got)
	}
}

func TestOtherEndpointsKeepGenericBehaviour(t *testing.T) {
	wireFakeProfile(t)
	ctx := context.Background()
	a := wireSeamAgent(t, "custom", "https://llm.example.test/v1", "llama-3.1-8b")
	if _, has := stringHeaders(a.clientKwargs["default_headers"])["X-Wire"]; has {
		t.Fatalf("foreign endpoint got the profile header %v", a.clientKwargs["default_headers"])
	}
	if got := a.maxTokensParam(ctx, 7); !reflect.DeepEqual(got, map[string]any{"max_tokens": 7}) {
		t.Fatalf("max tokens param %v", got)
	}
	kwargs := map[string]any{"base_url": "https://llm.example.test/v1"}
	ensureRouteClientHeaders(ctx, kwargs, "custom")
	if _, has := kwargs["default_headers"]; has {
		t.Fatalf("default headers added %v", kwargs)
	}
}

// Builds the request payload of one call carrying a single user row and returns its kwargs
func wirePayload(t *testing.T, a *AIAgent, apiCallCount int) map[string]any {
	t.Helper()
	a.emptyContentRetries = 0
	system := ""
	s := &LoopState{APIMessages: []any{map[string]any{"role": "user", "content": "hi"}}, ToolsForAPI: toolSurface(a.tools), SystemMessage: &system,
		APICallCount: apiCallCount, APIRequestID: "r", EffectiveTaskID: "t", TurnID: "u"}
	if err := buildRequestPayload(context.Background(), a, s); err != nil {
		t.Fatal(err)
	}
	return s.APIKwargs
}

func wireAttr(kwargs map[string]any) (any, bool) {
	switch headers := kwargs["extra_headers"].(type) {
	case map[string]any:
		v, ok := headers["X-Attr"]
		return v, ok
	case map[string]string:
		v, ok := headers["X-Attr"]
		return v, ok
	}
	return nil, false
}

func TestProviderWireSeamsUserAttribution(t *testing.T) {
	t.Run("test_user_turn_attribution_is_consumed_by_the_first_call_only", func(t *testing.T) {
		wireFakeProfile(t)
		a := wireSeamAgent(t, "wirefake", wireFakeHost, "gpt-4.1")
		a.isUserInitiatedTurn = true
		if got := wirePayload(t, a, 1)["extra_headers"]; !reflect.DeepEqual(stringHeaders(got), map[string]string{"X-Attr": "user"}) {
			t.Fatalf("first call headers %v", got)
		}
		if a.isUserInitiatedTurn {
			t.Fatal("turn flag not consumed")
		}
		if _, has := wireAttr(wirePayload(t, a, 2)); has {
			t.Fatal("tool loop follow up billed as the user")
		}
	})
	t.Run("test_only_the_first_attempt_of_a_user_turn_carries_user_attribution", func(t *testing.T) {
		wireFakeProfile(t)
		a := wireSeamAgent(t, "wirefake", wireFakeHost, "gpt-4.1")
		a.isUserInitiatedTurn = true
		if got := wirePayload(t, a, 1)["extra_headers"]; !reflect.DeepEqual(stringHeaders(got), map[string]string{"X-Attr": "user"}) {
			t.Fatalf("first attempt headers %v", got)
		}
		if _, has := wireAttr(wirePayload(t, a, 1)); has {
			t.Fatal("retried attempt billed as the user")
		}
		if _, has := wireAttr(wirePayload(t, a, 2)); has {
			t.Fatal("follow up billed as the user")
		}
	})
	t.Run("test_route_without_attribution_still_consumes_the_turn_flag", func(t *testing.T) {
		wireFakeProfile(t)
		a := wireSeamAgent(t, "custom", "https://llm.example.test/v1", "gpt-4.1")
		a.isUserInitiatedTurn = true
		if _, has := wireAttr(wirePayload(t, a, 1)); has {
			t.Fatal("foreign route got the attribution header")
		}
		if a.isUserInitiatedTurn {
			t.Fatal("turn flag not consumed")
		}
	})
	t.Run("test_failing_request_headers_hook_never_bills_a_follow_up_as_the_user", func(t *testing.T) {
		wireFakeProfile(t)
		a := wireSeamAgent(t, "wirefake", wireFakeHost, "gpt-4.1")
		a.isUserInitiatedTurn = true
		wireFakeRequestHeadersBroken = true
		_, has := wireAttr(wirePayload(t, a, 1))
		wireFakeRequestHeadersBroken = false
		if has {
			t.Fatal("failing hook produced the attribution header")
		}
		if a.isUserInitiatedTurn {
			t.Fatal("turn flag not consumed")
		}
		if _, has := wireAttr(wirePayload(t, a, 2)); has {
			t.Fatal("tool loop follow up billed as the user")
		}
	})
}
