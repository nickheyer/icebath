package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestGeminiBuildRequestPreservesExplicitMaxTokensWithoutThinking(t *testing.T) {
	req := buildGeminiRequest(geminiRequestInput{Messages: gemList(`[{"role": "user", "content": "hi"}]`), MaxTokens: 4096})
	gen := req["generationConfig"].(map[string]any)
	if gen["maxOutputTokens"] != 4096 {
		t.Fatalf("max %v", gen["maxOutputTokens"])
	}
	if _, ok := gen["thinkingConfig"]; ok {
		t.Fatal("thinkingConfig present")
	}
}

func TestGeminiBuildRequestRaisesMaxOutputWhenThinkingIsEnabled(t *testing.T) {
	req := buildGeminiRequest(geminiRequestInput{Messages: gemList(`[{"role": "user", "content": "hi"}]`), MaxTokens: 4096,
		ThinkingConfig: gemMap(`{"includeThoughts": true, "thinkingLevel": "high"}`)})
	gen := req["generationConfig"].(map[string]any)
	if gen["maxOutputTokens"] != geminiDefaultMaxOutputTokens || gemAt(gen, "thinkingConfig", "thinkingLevel") != "high" {
		t.Fatalf("gen %v", gen)
	}
}

func TestGeminiBuildRequestDoesNotRaiseWhenThinkingIsDisabled(t *testing.T) {
	req := buildGeminiRequest(geminiRequestInput{Messages: gemList(`[{"role": "user", "content": "hi"}]`), MaxTokens: 4096,
		ThinkingConfig: gemMap(`{"includeThoughts": false}`)})
	gen := req["generationConfig"].(map[string]any)
	if gen["maxOutputTokens"] != 4096 || gemAt(gen, "thinkingConfig", "includeThoughts") != false {
		t.Fatalf("gen %v", gen)
	}
}

const gemParallelHistory = `[
	{"role": "user", "content": "Read a.txt and b.txt"},
	{"role": "assistant", "content": "", "tool_calls": [
		{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"a.txt\"}"}},
		{"id": "call_2", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"b.txt\"}"}}]},
	{"role": "tool", "tool_call_id": "call_1", "content": "AAA"},
	{"role": "tool", "tool_call_id": "call_2", "content": "BBB"}]`

// Values of one key under every part holding the given part kind
func gemPartIDs(contents []map[string]any, kind string) []any {
	var out []any
	for _, c := range contents {
		for _, p := range c["parts"].([]any) {
			if inner, ok := p.(map[string]any)[kind].(map[string]any); ok {
				out = append(out, inner["id"])
			}
		}
	}
	return out
}

func TestGemini3ToolCallIDsRequiresIDsGate(t *testing.T) {
	for _, m := range []string{"gemini-3.6-flash", "google/gemini-3.6-pro", "gemini-3-flash-preview"} {
		if !geminiRequiresToolCallIDs(m) {
			t.Errorf("%s should require ids", m)
		}
	}
	for _, m := range []string{"gemini-2.5-flash", "gemini-1.5-pro", "claude-opus-4.6", ""} {
		if geminiRequiresToolCallIDs(m) {
			t.Errorf("%s should not require ids", m)
		}
	}
}

func TestGemini3ToolCallIDsPreservedForGemini3(t *testing.T) {
	contents, _ := buildGeminiContents(gemList(gemParallelHistory), true, false)
	gemEq(t, gemPartIDs(contents, "functionCall"), `["call_1","call_2"]`)
	gemEq(t, gemPartIDs(contents, "functionResponse"), `["call_1","call_2"]`)
}

func TestGemini3ToolCallIDsOmittedForOlderGemini(t *testing.T) {
	contents, _ := buildGeminiContents(gemList(gemParallelHistory), false, false)
	for _, id := range append(gemPartIDs(contents, "functionCall"), gemPartIDs(contents, "functionResponse")...) {
		if id != nil {
			t.Fatalf("id leaked: %v", id)
		}
	}
}

func TestGemini3ToolCallIDsBuildRequestThreadsModelGate(t *testing.T) {
	req := buildGeminiRequest(geminiRequestInput{Messages: gemList(gemParallelHistory), Model: "gemini-3.6-flash"})
	ids := gemPartIDs(req["contents"].([]map[string]any), "functionCall")
	if len(ids) == 0 || ids[0] != "call_1" {
		t.Fatalf("ids %v", ids)
	}
	old := buildGeminiRequest(geminiRequestInput{Messages: gemList(gemParallelHistory), Model: "gemini-2.5-flash"})
	for _, id := range gemPartIDs(old["contents"].([]map[string]any), "functionCall") {
		if id != nil {
			t.Fatalf("old model id %v", id)
		}
	}
}

func TestGemini3ToolCallIDsResponsePreservesProviderToolCallID(t *testing.T) {
	resp := translateGeminiResponse(gemMap(`{"candidates": [{"content": {"parts": [{"functionCall": {"id": "call_native_7", "name": "read_file", "args": {"path": "a.txt"}}}]}, "finishReason": "STOP"}]}`), "gemini-3.6-flash")
	if id := gemAt(resp, "choices", 0, "message", "tool_calls", 0, "id"); id != "call_native_7" {
		t.Fatalf("id %v", id)
	}
}

func TestGemini3ToolCallIDsResponseGeneratesIDWhenAbsent(t *testing.T) {
	resp := translateGeminiResponse(gemMap(`{"candidates": [{"content": {"parts": [{"functionCall": {"name": "read_file", "args": {}}}]}, "finishReason": "STOP"}]}`), "gemini-2.5-flash")
	if id, _ := gemAt(resp, "choices", 0, "message", "tool_calls", 0, "id").(string); !strings.HasPrefix(id, "call_") || len(id) != 17 {
		t.Fatalf("id %q", id)
	}
}

const gemPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func gemVisionToolMessages() []any {
	return gemList(`[{"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "vision_analyze", "arguments": "{}"}}]},
		{"role": "tool", "tool_call_id": "call_1", "name": "vision_analyze", "content": [{"type": "text", "text": "a red pixel"},
		{"type": "image_url", "image_url": {"url": "` + gemPNGDataURL + `"}}]}]`)
}

func TestGemini3xEmbedsImageInFunctionResponseParts(t *testing.T) {
	for _, model := range []string{"gemini-3.5-flash", "gemini-3-flash-preview", "gemini-3-pro-preview", "gemini-3.1-flash-lite-preview"} {
		req := buildGeminiRequest(geminiRequestInput{Messages: gemVisionToolMessages(), Model: model, Tools: []any{}})
		fr := gemAt(req["contents"], 1, "parts", 0, "functionResponse").(map[string]any)
		if gemAt(fr, "parts", 0, "inlineData", "mimeType") != "image/png" || gemAt(fr, "parts", 0, "inlineData", "data") == "" {
			t.Fatalf("%s: %v", model, fr)
		}
	}
}

func TestGemini2xDoesNotEmbedImageParts(t *testing.T) {
	req := buildGeminiRequest(geminiRequestInput{Messages: gemVisionToolMessages(), Model: "gemini-2.5-flash", Tools: []any{}})
	if _, ok := gemAt(req["contents"], 1, "parts", 0, "functionResponse").(map[string]any)["parts"]; ok {
		t.Fatal("2.x embedded image parts")
	}
}

func TestGeminiTextOnlyToolResultHasNoParts(t *testing.T) {
	msgs := gemList(`[{"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{}"}}]},
		{"role": "tool", "tool_call_id": "call_1", "name": "read_file", "content": "file contents here"}]`)
	req := buildGeminiRequest(geminiRequestInput{Messages: msgs, Model: "gemini-3.6-flash", Tools: []any{}})
	if _, ok := gemAt(req["contents"], 1, "parts", 0, "functionResponse").(map[string]any)["parts"]; ok {
		t.Fatal("text result carries parts")
	}
}

// Every payload a Gemini SSE body yields
func gemReadSSE(body string) []any {
	r := newGeminiSSEReader(strings.NewReader(body))
	out := []any{}
	for {
		ev, err := r.Next()
		if err != nil {
			return out
		}
		out = append(out, ev)
	}
}

func TestGeminiIterSSEEventsFlushesResidualBufferAfterEOF(t *testing.T) {
	gemEq(t, gemReadSSE("data: {\"candidates\": [1]}\ndata: {\"candidates\": [2]}"), `[{"candidates": [1]}, {"candidates": [2]}]`)
}

func TestGeminiIterSSEEventsStopsAtDoneAndIgnoresTrailingFrames(t *testing.T) {
	gemEq(t, gemReadSSE("data: {\"candidates\": [1]}\ndata: [DONE]\ndata: {\"candidates\": [3]}\n"), `[{"candidates": [1]}]`)
	gemEq(t, gemReadSSE("data: {\"candidates\": [1]}\ndata: [DONE]"), `[{"candidates": [1]}]`)
}

func TestGeminiNativeClientNeverReroutesAQKeysOffTheConfiguredSurface(t *testing.T) {
	for _, tc := range [][3]string{
		{"AQ.studio-key", "", "https://generativelanguage.googleapis.com/v1beta/models/"},
		{"AIza-test", "", "https://generativelanguage.googleapis.com/v1beta/models/"},
		{"AQ.express-key", "https://aiplatform.googleapis.com", "https://aiplatform.googleapis.com/v1beta1/publishers/google/models/"},
		{"AIza-test", "https://aiplatform.googleapis.com/v1beta1", "https://aiplatform.googleapis.com/v1beta1/publishers/google/models/"},
		{"AQ.studio-key", "http://localhost:4000/gemini", "http://localhost:4000/gemini/v1beta/models/"},
	} {
		f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
			gemWriteJSON(w, 200, `{"candidates": [{"content": {"role": "model", "parts": [{"text": "ok"}]}, "finishReason": "STOP"}]}`)
		})
		c := gemClient(t, f, tc[1], tc[0], "gemini-3.7-flash")
		if _, err := c.Complete(context.Background(), gemUserRequest("gemini-3.7-flash", "hi")); err != nil {
			t.Fatal(err)
		}
		if reqs := f.all(); len(reqs) != 1 || reqs[0].URL != tc[2]+"gemini-3.7-flash:generateContent" {
			t.Fatalf("%v: %v", tc, reqs)
		}
	}
}

func TestGeminiNativeDetectionCoversExpressButNotVertexOAuthOpenAPI(t *testing.T) {
	if !isNativeGeminiBaseURL("https://aiplatform.googleapis.com/v1beta1/publishers/google") {
		t.Fatal("express base not native")
	}
	if isNativeGeminiBaseURL("https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi") {
		t.Fatal("vertex openapi captured")
	}
}

func TestGeminiThinkingTokensAreCountedAsOutputAndSurfacedAsReasoning(t *testing.T) {
	prompt, visible, thoughts := 10, 200, 5000
	payload := gemMap(fmt.Sprintf(`{"candidates": [{"content": {"parts": [{"text": "done"}]}, "finishReason": "STOP"}],
		"usageMetadata": {"promptTokenCount": %d, "candidatesTokenCount": %d, "thoughtsTokenCount": %d, "totalTokenCount": %d}}`, prompt, visible, thoughts, prompt+visible+thoughts))
	usage := translateGeminiResponse(payload, "gemini-2.5-flash")["usage"].(map[string]any)
	if usage["prompt_tokens"].(int)+usage["completion_tokens"].(int) != usage["total_tokens"].(int) || gemAt(usage, "completion_tokens_details", "reasoning_tokens") != thoughts {
		t.Fatalf("usage %v", usage)
	}
	canonical := ccUsageFromMap(usage)
	if canonical.OutputTokens != visible+thoughts || canonical.ReasoningTokens != thoughts {
		t.Fatalf("canonical %+v", canonical)
	}
}

func TestGeminiResponseWithoutThinkingTokensKeepsItsOutputCount(t *testing.T) {
	usage := translateGeminiResponse(gemMap(`{"candidates": [{"content": {"parts": [{"text": "hi"}]}, "finishReason": "STOP"}],
		"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}}`), "gemini-2.5-flash")["usage"].(map[string]any)
	if usage["completion_tokens"] != 5 || usage["prompt_tokens"].(int)+usage["completion_tokens"].(int) != usage["total_tokens"] {
		t.Fatalf("usage %v", usage)
	}
	if ccUsageFromMap(usage).ReasoningTokens != 0 {
		t.Fatal("reasoning tokens without thoughts")
	}
}

func TestGeminiBuildRequestTranslatesResponseFormatOnBothSchemaSurfaces(t *testing.T) {
	schema := `{"type": "object", "properties": {"ok": {"type": "boolean"}}, "required": ["ok"], "additionalProperties": false}`
	for _, tc := range []struct {
		surface bool
		key     string
	}{{true, "responseJsonSchema"}, {false, "responseSchema"}} {
		gen := buildGeminiRequest(geminiRequestInput{Messages: gemList(`[{"role": "user", "content": "hi"}]`),
			ResponseFormat: gemMap(`{"type": "json_schema", "json_schema": {"name": "out", "schema": ` + schema + `}}`), ToolsAsJSONSchema: tc.surface})["generationConfig"].(map[string]any)
		if gen["responseMimeType"] != "application/json" {
			t.Fatalf("mime %v", gen)
		}
		out := gen[tc.key].(map[string]any)
		gemEq(t, out["properties"], `{"ok": {"type": "boolean"}}`)
		gemEq(t, out["required"], `["ok"]`)
		if _, has := out["additionalProperties"]; has != tc.surface {
			t.Fatalf("%s additionalProperties %v", tc.key, out)
		}
	}
}

func TestGeminiBuildRequestDropsJSONOutputWhenToolChoiceForcesCalls(t *testing.T) {
	for _, choice := range []any{"required", gemMap(`{"type": "function", "function": {"name": "lookup"}}`)} {
		gen := buildGeminiRequest(geminiRequestInput{Messages: gemList(`[{"role": "user", "content": "hi"}]`),
			Tools: gemList(`[{"type": "function", "function": {"name": "lookup", "parameters": {"type": "object"}}}]`), ToolChoice: choice, Model: "gemini-3-flash",
			ResponseFormat: gemMap(`{"type": "json_schema", "json_schema": {"name": "out", "schema": {"type": "object"}}}`), ToolsAsJSONSchema: true})["generationConfig"].(map[string]any)
		for _, k := range []string{"responseMimeType", "responseJsonSchema", "responseSchema"} {
			if _, ok := gen[k]; ok {
				t.Fatalf("%v kept %s", choice, k)
			}
		}
	}
}

func TestGeminiBuildRequestToolsPlusJSONOutputOnlyOnGemini3(t *testing.T) {
	for model, keeps := range map[string]bool{"gemini-2.5-flash": false, "gemini-3-flash": true} {
		gen := buildGeminiRequest(geminiRequestInput{Messages: gemList(`[{"role": "user", "content": "hi"}]`),
			Tools: gemList(`[{"type": "function", "function": {"name": "lookup", "parameters": {"type": "object"}}}]`), ToolChoice: "auto", Model: model,
			ResponseFormat: gemMap(`{"type": "json_object"}`), ToolsAsJSONSchema: true})["generationConfig"].(map[string]any)
		if _, ok := gen["responseMimeType"]; ok != keeps {
			t.Fatalf("%s keeps json %v", model, ok)
		}
	}
}

// One long decode hop body
func gemHop(text, finish, token string, prompt, out int) string {
	cand := map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}, "finishReason": finish}
	if token != "" {
		cand["continuationToken"] = token
	}
	data, _ := json.Marshal(map[string]any{"candidates": []any{cand}, "usageMetadata": map[string]any{"promptTokenCount": prompt, "candidatesTokenCount": out, "totalTokenCount": prompt + out}})
	return string(data)
}

func TestGeminiLongDecodeContinuesAcrossHopsAndReportsFirstHopPrompt(t *testing.T) {
	hops := []string{gemHop("part one ", "CONTINUATION", "tok-1", 100, 32768), gemHop("part two", "STOP", "", 32868, 500)}
	checkpoint := `{"candidates": [{"content": {"parts": [{"text": ""}]}, "continuationToken": "ckpt"}]}`
	f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			gemWriteSSE(w, checkpoint, hops[n%2])
			return
		}
		gemWriteJSON(w, 200, hops[n%2])
	})
	c := gemClient(t, f, "https://generativelanguage.googleapis.com/v1beta", "AQ.test", "gemini-4-argon")
	req := gemUserRequest("gemini-4-argon", "prove it")
	req.MaxTokens = 262_144
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	sent := f.all()
	want := copyAnyMap(sent[0].Body)
	want["continuationToken"] = "tok-1"
	gemEq(t, sent[1].Body, gemCanon(t, want))
	if gemAt(sent[0].Body, "generationConfig", "maxOutputTokens") != json.Number("262144") {
		t.Fatalf("max %v", gemAt(sent[0].Body, "generationConfig"))
	}
	if resp.Message.Content != "part one part two" || resp.FinishReason != "stop" {
		t.Fatalf("resp %v %s", resp.Message.Content, resp.FinishReason)
	}
	if prompt, _, ok := ResponseInitialPromptTokens(resp); !ok || resp.Usage.InputTokens != 32968 || resp.Usage.OutputTokens != 33268 || prompt != 100 {
		t.Fatalf("usage %+v initial %d", resp.Usage, prompt)
	}
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
	var text strings.Builder
	sresp, err := c.Stream(context.Background(), req, func(ev StreamEvent) error {
		if ev.Kind == EventTextDelta {
			text.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sent = f.all()
	want = copyAnyMap(sent[0].Body)
	want["continuationToken"] = "tok-1"
	gemEq(t, sent[1].Body, gemCanon(t, want))
	if text.String() != "part one part two" || sresp.FinishReason != "stop" {
		t.Fatalf("stream %q %s", text.String(), sresp.FinishReason)
	}
	if prompt, _, ok := ResponseInitialPromptTokens(sresp); !ok || sresp.Usage.InputTokens != 32968 || prompt != 100 {
		t.Fatalf("stream usage %+v initial %d", sresp.Usage, prompt)
	}
}
