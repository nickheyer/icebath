package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

type plainBlockModel struct{ fields map[string]any }

func (m plainBlockModel) ModelDump(warnings bool) any { return m.fields }

type plainBareModel struct{ fields map[string]any }

func (m plainBareModel) ModelDump() any { return m.fields }

type plainFailingModel struct{}

func (plainFailingModel) ModelDump() (any, error) { return nil, errors.New("dump refused") }

type plainThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Hidden    string `json:"_request_id"`
	private   string
}

type plainResponseObject struct {
	Content     []any          `json:"content"`
	StopReason  string         `json:"stop_reason"`
	StopDetails plainBareModel `json:"stop_details"`
}

func TestAnthropicNormalizeResponseConvertsSDKShapedBlocksToPlainData(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	response := plainResponseObject{
		Content: []any{
			&plainThinkingBlock{Type: "thinking", Thinking: "weigh it", Signature: "sig-1", Hidden: "req", private: "p"},
			plainBlockModel{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": map[string]any{"path": "a.txt"}}},
			plainBareModel{map[string]any{"type": "text", "text": "done"}},
		},
		StopReason:  "tool_use",
		StopDetails: plainBareModel{map[string]any{"type": "refusal", "category": "cyber"}},
	}
	resp, err := GetTransport(string(ModeAnthropicMessages)).NormalizeResponse(response, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "done" || resp.Message.Reasoning != "weigh it" || len(resp.Message.ToolCalls) != 1 ||
		resp.Message.ToolCalls[0].Function.Name != "read_file" || resp.Message.ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("message %+v", resp.Message)
	}
	var details []map[string]any
	if err := json.Unmarshal(resp.Message.ReasoningDetails, &details); err != nil || len(details) != 1 ||
		details[0]["signature"] != "sig-1" || details[0]["_request_id"] != nil || details[0]["private"] != nil {
		t.Fatalf("reasoning details %s %v", resp.Message.ReasoningDetails, err)
	}
	blocks, live := AnthropicLiveBlocks(resp.Message)
	if !live || len(blocks) != 3 {
		t.Fatalf("ordered blocks %v %v", blocks, live)
	}
	stop, _ := ResponseStopDetails(resp).(map[string]any)
	if stop["type"] != "refusal" || stop["category"] != "cyber" {
		t.Fatalf("stop details %v", ResponseStopDetails(resp))
	}
}

func TestToPlainDataStringifiesCyclesAndDepthPastTwenty(t *testing.T) {
	shared := map[string]any{"k": "v"}
	twice := toPlainData(map[string]any{"a": shared, "b": []any{shared}}, 0, nil).(map[string]any)
	if twice["a"].(map[string]any)["k"] != "v" || twice["b"].([]any)[0].(map[string]any)["k"] != "v" {
		t.Fatalf("shared value %v", twice)
	}
	loop := map[string]any{"name": "root"}
	loop["self"] = []any{loop}
	plain := toPlainData(loop, 0, nil).(map[string]any)
	if text := plain["self"].([]any)[0]; text != "{'name': 'root', 'self': [{...}]}" {
		t.Fatalf("cycle text %v", text)
	}
	var deep any = "leaf"
	for i := 0; i < 25; i++ {
		deep = []any{deep}
	}
	level := toPlainData(deep, 0, nil)
	for i := 0; i <= 20; i++ {
		level = level.([]any)[0]
	}
	if level != "[[[['leaf']]]]" {
		t.Fatalf("depth text %v", level)
	}
	if raw := toPlainData([]byte("ab"), 0, nil); string(raw.([]byte)) != "ab" {
		t.Fatalf("bytes %v", raw)
	}
	if at := toPlainData(time.Unix(5, 0), 0, nil); at != time.Unix(5, 0) {
		t.Fatalf("opaque value %v", at)
	}
}

func TestHookPayloadDumpsModelsBeforeOtherShapes(t *testing.T) {
	payload := HookJSONable(map[string]any{
		"full":    plainBlockModel{map[string]any{"api_key": "sk-secret", "text": "kept"}},
		"bare":    &plainBareModel{map[string]any{"n": 1}},
		"failing": plainFailingModel{},
	}, DefaultHookJSONCaps).(map[string]any)
	full, _ := payload["full"].(map[string]any)
	if full["api_key"] != "<redacted>" || full["text"] != "kept" {
		t.Fatalf("model dump %v", payload["full"])
	}
	if bare, _ := payload["bare"].(map[string]any); bare["n"] != 1 {
		t.Fatalf("bare dump %v", payload["bare"])
	}
	if _, dumped := payload["failing"].(map[string]any); !dumped {
		t.Fatalf("a failed dump did not fall to the object fields %v", payload["failing"])
	}
}

func TestBearerHookClientSendsMintedTokensAndClaudeCodeIdentity(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("HTTPS_PROXY", "")
	var seen []http.Header
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()
	minted := 0
	source := func(context.Context) (string, error) {
		minted++
		return "entra-jwt-" + strings.Repeat("x", minted), nil
	}
	client, err := newAnthropicClient(Route{Provider: "custom", Mode: ModeAnthropicMessages, BaseURL: server.URL + "/anthropic/v1/", Model: "claude-sonnet-4-5", TokenSource: source})
	if err != nil {
		t.Fatal(err)
	}
	hooked := client.(*anthropicClient)
	if hooked.bearerHook == nil || hooked.bearerHook.SendsAsAPIKey != nil || hooked.hookStyle(context.Background()) != "bearer" {
		t.Fatalf("third party hook %+v", hooked.bearerHook)
	}
	request := &Request{Model: "claude-sonnet-4-5", Messages: []*state.Message{{Role: "user", Content: "hi"}}, MaxTokens: 8}
	for i := 0; i < 2; i++ {
		if _, err := client.Complete(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0].Get("Authorization") != "Bearer entra-jwt-x" || seen[1].Get("Authorization") != "Bearer entra-jwt-xx" ||
		seen[0].Get("X-Api-Key") != "" || seen[0].Get("X-App") != "" || queries[0] != "/anthropic/v1/messages?" {
		t.Fatalf("requests %v %v", seen, queries)
	}

	native, err := newAnthropicClient(Route{Provider: "anthropic", Mode: ModeAnthropicMessages, Model: "claude-sonnet-4-5",
		TokenSource: func(context.Context) (string, error) { return "sk-ant-oat01-subscription", nil }})
	if err != nil {
		t.Fatal(err)
	}
	nativeHook := native.(*anthropicClient)
	if nativeHook.hookStyle(context.Background()) != "oauth" || nativeHook.bearerHook.SendsAsAPIKey("sk-ant-oat01-subscription") ||
		!nativeHook.bearerHook.SendsAsAPIKey("sk-ant-api03-console") {
		t.Fatal("the native host hook did not tell an OAuth token from a Console key")
	}
	headers, oauth, err := nativeHook.requestHeaders(context.Background(), anthCommonBetasForBaseURL("", false), nil)
	if err != nil || !oauth || headers["x-app"] != "cli" || !strings.HasPrefix(headers["User-Agent"], "claude-code/") ||
		!strings.Contains(headers["anthropic-beta"], anthropicOAuthOnlyBetas[0]) {
		t.Fatalf("native OAuth headers %v %v %v", headers, oauth, err)
	}
}

func TestBaseClientKwargsStripV1AndAddTheAzureQuery(t *testing.T) {
	normalized, kwargs := anthBaseClientKwargs(" https://res.services.ai.azure.com/anthropic/v1/ ", 0)
	if normalized != "https://res.services.ai.azure.com/anthropic" || kwargs.baseURL != normalized ||
		kwargs.defaultQuery["api-version"] != "2025-04-15" || kwargs.timeout != 900*time.Second || kwargs.connect != 10*time.Second {
		t.Fatalf("azure kwargs %q %+v", normalized, kwargs)
	}
	if _, pinned := anthBaseClientKwargs("https://res.services.ai.azure.com/anthropic?api-version=2026-01-01", 30*time.Second); pinned.defaultQuery != nil || pinned.timeout != 30*time.Second {
		t.Fatalf("pinned api version %+v", pinned)
	}
	if empty, kwargs := anthBaseClientKwargs("", -1); empty != "" || kwargs.baseURL != "" || kwargs.defaultQuery != nil || kwargs.timeout != 900*time.Second {
		t.Fatalf("default host kwargs %q %+v", empty, kwargs)
	}
	transport := newAnthDirectTransport(Route{BaseURL: "https://res.services.ai.azure.com/anthropic/v1"})
	req, err := transport.Request(context.Background(), anthOpMessages, map[string]any{"model": "m"}, nil)
	if err != nil || req.URL.String() != "https://res.services.ai.azure.com/anthropic/v1/messages?api-version=2025-04-15" {
		t.Fatalf("azure request %v %v", req, err)
	}
	if query, _ := AnthropicBaseClientQuery("https://api.anthropic.com/v1"); query != "https://api.anthropic.com" {
		t.Fatalf("native base %q", query)
	}
}
