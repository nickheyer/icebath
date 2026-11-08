package agent

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Python exception double carrying its class name and message
type pyClassErr struct{ class, msg string }

func (e *pyClassErr) Error() string      { return e.msg }
func (e *pyClassErr) PyTypeName() string { return e.class }

// Status error double with response headers, the SDK APIStatusError shape
type statusHeaderErr struct {
	msg     string
	status  int
	headers http.Header
}

func (e *statusHeaderErr) Error() string                { return e.msg }
func (e *statusHeaderErr) StatusCode() int              { return e.status }
func (e *statusHeaderErr) ResponseHeaders() http.Header { return e.headers }
func (e *statusHeaderErr) PyTypeName() string           { return "APIStatusError" }

func asciiCodecErr() error {
	return &pyClassErr{"UnicodeEncodeError", "'ascii' codec can't encode character '\\u2615' in position 0: ordinal not in range"}
}

func rowsOf(dicts ...map[string]any) (out []*state.Message) {
	for _, d := range dicts {
		out = append(out, state.MessageFromDict(d))
	}
	return out
}

func dictsOf(rows []*state.Message) (out []map[string]any) {
	for _, m := range rows {
		out = append(out, state.MessageToDict(m))
	}
	return out
}

func setASCIILocale(t *testing.T, ascii bool) {
	prev := recoveryLocaleEnv
	recoveryLocaleEnv = func(key string) string {
		if ascii && key == "LANG" {
			return "C.ascii"
		}
		return ""
	}
	t.Cleanup(func() { recoveryLocaleEnv = prev })
}

func quietAgent() (*AIAgent, *[]string) {
	printed := &[]string{}
	a := &AIAgent{suppressStatusOutput: false, printFn: func(s string) { *printed = append(*printed, s) }}
	a.notificationConfig = map[string]any{}
	return a, printed
}

func TestStripNonAsciiAsciiOnly(t *testing.T) {
	if state.StripNonASCII("hello world") != "hello world" {
		t.Fatal("ascii text changed")
	}
}

func TestSanitizeMessagesNonAsciiNoChangeAsciiOnly(t *testing.T) {
	messages := []any{map[string]any{"role": "user", "content": "hello"}}
	if state.SanitizeMessagesNonASCII(messages) || messages[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("ascii message changed %v", messages)
	}
}

func TestSanitizeMessagesNonAsciiEmptyMessages(t *testing.T) {
	if state.SanitizeMessagesNonASCII([]any{}) {
		t.Fatal("empty list reported a change")
	}
}

func TestSurrogateVsAsciiSanitization(t *testing.T) {
	t.Run("test_surrogates_still_handled", func(t *testing.T) {
		messages := []any{map[string]any{"role": "user", "content": "test \xed\xa0\x80 end"}}
		if !state.SanitizeMessagesSurrogates(messages) {
			t.Fatal("surrogate not found")
		}
		content := messages[0].(map[string]any)["content"].(string)
		if strings.Contains(content, "\xed\xa0\x80") || !strings.Contains(content, "�") {
			t.Fatalf("content %q", content)
		}
	})
	t.Run("test_surrogates_in_name_and_tool_calls_are_sanitized", func(t *testing.T) {
		fn := map[string]any{"name": "read\xed\xa0\x80_file", "arguments": `{"path": "bad` + "\xed\xa0\x80" + `.txt"}`}
		tc := map[string]any{"id": "call_\xed\xa0\x80", "type": "function", "function": fn}
		msg := map[string]any{"role": "assistant", "name": "bad\xed\xa0\x80name", "content": nil, "tool_calls": []any{tc}}
		if !state.SanitizeMessagesSurrogates([]any{msg}) {
			t.Fatal("surrogates not found")
		}
		for _, v := range []any{msg["name"], tc["id"], fn["name"], fn["arguments"]} {
			if strings.Contains(v.(string), "\xed\xa0\x80") {
				t.Fatalf("surrogate left in %q", v)
			}
		}
	})
	t.Run("test_ascii_codec_strips_all_non_ascii", func(t *testing.T) {
		messages := []any{map[string]any{"role": "user", "content": "test ☤🤖你好 end"}}
		if !state.SanitizeMessagesNonASCII(messages) || messages[0].(map[string]any)["content"] != "test  end" {
			t.Fatalf("messages %v", messages)
		}
	})
	t.Run("test_no_surrogates_returns_false", func(t *testing.T) {
		if state.SanitizeMessagesSurrogates([]any{map[string]any{"role": "user", "content": "hello ☤ world"}}) {
			t.Fatal("non surrogate text reported")
		}
	})
}

func TestStripNonAsciiFromApiKey(t *testing.T) {
	if got := state.StripNonASCII("sk-proj-abc" + "ʋ" + "def"); got != "sk-proj-abcdef" {
		t.Fatalf("key %q", got)
	}
}

func TestSanitizesToolDescriptionAndParameterDescriptions(t *testing.T) {
	path := map[string]any{"type": "string", "description": "File path │ with unicode"}
	fn := map[string]any{"name": "read_file", "description": "Print structured output │ with emoji 🤖",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"path": path}}}
	tools := []any{map[string]any{"type": "function", "function": fn}}
	if !state.SanitizeStructureNonASCII(tools) {
		t.Fatal("no change reported")
	}
	if fn["description"] != "Print structured output  with emoji " || path["description"] != "File path  with unicode" {
		t.Fatalf("tools %v", tools)
	}
}

func TestSanitizesNestedDictStructure(t *testing.T) {
	headers := map[string]any{"X-Title": "Hermes │ Agent", "User-Agent": "Hermes/1.0 🤖"}
	if !state.SanitizeStructureNonASCII(map[string]any{"default_headers": headers}) {
		t.Fatal("no change reported")
	}
	if headers["X-Title"] != "Hermes  Agent" || headers["User-Agent"] != "Hermes/1.0 " {
		t.Fatalf("headers %v", headers)
	}
}

func TestApiMessagesAndApiKwargsSanitized(t *testing.T) {
	t.Run("test_api_messages_with_reasoning_content_is_sanitized", func(t *testing.T) {
		assistant := map[string]any{"role": "assistant", "content": "Sure!", "reasoning_content": "Let me think « step by step »"}
		apiMessages := []any{map[string]any{"role": "system", "content": "You are helpful."}, map[string]any{"role": "user", "content": "hi"}, assistant}
		if !state.SanitizeMessagesNonASCII(apiMessages) || strings.ContainsAny(assistant["reasoning_content"].(string), "«»") {
			t.Fatalf("assistant %v", assistant)
		}
	})
	t.Run("test_api_kwargs_with_non_ascii_extra_body_is_sanitized", func(t *testing.T) {
		extra := map[string]any{"system": "Think carefully → answer"}
		kwargs := map[string]any{"model": "glm-5.1", "messages": []any{map[string]any{"role": "user", "content": "ok"}}, "extra_body": extra}
		if !state.SanitizeStructureNonASCII(kwargs) || strings.Contains(extra["system"].(string), "→") {
			t.Fatalf("extra %v", extra)
		}
	})
	t.Run("test_reasoning_field_in_canonical_messages_is_sanitized", func(t *testing.T) {
		assistant := map[string]any{"role": "assistant", "content": "ok", "reasoning": "Let me think « carefully »"}
		if !state.SanitizeMessagesNonASCII([]any{map[string]any{"role": "user", "content": "hello"}, assistant}) ||
			strings.ContainsAny(assistant["reasoning"].(string), "«»") {
			t.Fatalf("assistant %v", assistant)
		}
	})
}

func TestSanitizeMessagesPersistMarker(t *testing.T) {
	t.Run("test_surrogate_repair_pops_marker", func(t *testing.T) {
		msg := map[string]any{"role": "user", "content": "test \xed\xa0\x80 end", state.DBPersistedMarker: true}
		if !state.SanitizeMessagesSurrogates([]any{msg}) || strings.Contains(msg["content"].(string), "\xed\xa0\x80") {
			t.Fatalf("msg %v", msg)
		}
		if _, ok := msg[state.DBPersistedMarker]; ok {
			t.Fatal("persisted marker kept")
		}
	})
	t.Run("test_unchanged_dict_keeps_marker", func(t *testing.T) {
		msg := map[string]any{"role": "user", "content": "clean ascii", state.DBPersistedMarker: true}
		if state.SanitizeMessagesNonASCII([]any{msg}) || msg[state.DBPersistedMarker] != true {
			t.Fatalf("msg %v", msg)
		}
	})
	t.Run("test_ascii_recovery_sanitizes_detached_request_not_canonical_history", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		setASCIILocale(t, true)
		a, _ := quietAgent()
		a.apiKey, a.clientKwargs = "ascii-key", map[string]any{}
		a.prefillMessages = []any{map[string]any{"role": "assistant", "content": "prefill ☕"}}
		a.tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "description": "desc ☕"}}}
		a.cachedSystemPrompt, a.ephemeralSystemPrompt = "cached ☕", "ephemeral ☕"
		canonical := rowsOf(map[string]any{"role": "user", "content": "olá ☕", state.DBPersistedMarker: true})
		apiMessages := []any{map[string]any{"role": "user", "content": "olá ☕"}}
		apiKwargs := map[string]any{"tools": a.tools, "extra_body": map[string]any{"note": "request ☕"}}
		canonicalBefore := fmt.Sprint(dictsOf(canonical))
		prefillBefore, toolsBefore := fmt.Sprint(a.prefillMessages), fmt.Sprint(a.tools)
		prompt := "active ☕"
		if !recoverUnicodeEncodeError(a, asciiCodecErr(), canonical, apiMessages, apiKwargs, &prompt) {
			t.Fatal("ascii codec error not recovered")
		}
		if prompt != "active " {
			t.Fatalf("prompt %q", prompt)
		}
		if fmt.Sprint(dictsOf(canonical)) != canonicalBefore || fmt.Sprint(a.prefillMessages) != prefillBefore || fmt.Sprint(a.tools) != toolsBefore {
			t.Fatal("canonical state rewritten")
		}
		if a.cachedSystemPrompt != "cached ☕" || a.ephemeralSystemPrompt != "ephemeral ☕" || !canonical[0].Persisted {
			t.Fatal("cached prompts or persisted mark changed")
		}
		if c := apiMessages[0].(map[string]any)["content"].(string); state.StripNonASCII(c) != c {
			t.Fatalf("request copy kept non ascii %q", c)
		}
		if !a.forceASCIIPayload {
			t.Fatal("ascii payload not forced")
		}
		retry := map[string]any{"tools": a.tools, "extra_body": map[string]any{"note": "retry ☕"}}
		a.sanitizeOutboundKwargs(retry)
		if sameList(retry["tools"], a.tools) || fmt.Sprint(a.tools) != toolsBefore {
			t.Fatal("canonical tools stripped in place")
		}
		desc := retry["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["description"].(string)
		note := retry["extra_body"].(map[string]any)["note"].(string)
		if state.StripNonASCII(desc) != desc || state.StripNonASCII(note) != note {
			t.Fatalf("retry kwargs kept non ascii %q %q", desc, note)
		}
	})
	t.Run("test_ascii_word_in_error_does_not_strip_utf8_request_copy", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		setASCIILocale(t, false)
		a, _ := quietAgent()
		a.apiKey, a.clientKwargs, a.tools, a.cachedSystemPrompt = "ascii-key", map[string]any{}, []any{}, "system ☕"
		canonical := rowsOf(map[string]any{"role": "user", "content": "olá ☕"})
		apiMessages := []any{map[string]any{"role": "user", "content": "olá ☕"}}
		prompt := "system ☕"
		if recoverUnicodeEncodeError(a, asciiCodecErr(), canonical, apiMessages, map[string]any{}, &prompt) {
			t.Fatal("unrepaired request retried")
		}
		if a.unicodeSanitizationPasses != 0 || canonical[0].Content != "olá ☕" || apiMessages[0].(map[string]any)["content"] != "olá ☕" || a.cachedSystemPrompt != "system ☕" {
			t.Fatal("utf-8 request copy rewritten")
		}
	})
}

func TestRecoveryRetriesAtStandardSpeedBeforeClassification(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, printed := quietAgent()
	a.model, a.provider = "claude-opus-5", "anthropic"
	err := &statusHeaderErr{msg: "This request would exceed your organization's rate limit of 0 fast mode input tokens per minute.", status: 429,
		headers: http.Header{"Anthropic-Fast-Input-Tokens-Limit": {"0"}, "Anthropic-Fast-Output-Tokens-Limit": {"0"}}}
	s := &LoopState{APIMessages: []any{}, APIKwargs: map[string]any{"model": "claude-opus-5", "extra_body": map[string]any{"speed": "fast"}}, ActiveSystemPrompt: "sys"}
	retry, rerr := recoverBeforeClassification(t.Context(), a, s, err)
	if rerr != nil || !retry || s.ActiveSystemPrompt != "sys" {
		t.Fatalf("retry %v err %v prompt %q", retry, rerr, s.ActiveSystemPrompt)
	}
	if a.markFastModeUnavailable() {
		t.Fatal("model not marked fast unavailable")
	}
	if !strings.Contains(strings.Join(*printed, "\n"), "standard speed") {
		t.Fatalf("printed %q", *printed)
	}
}

var replayDetails = []any{map[string]any{"type": "reasoning.text", "text": "thought"}}

func replayTurn() map[string]any {
	return map[string]any{"role": "assistant", "content": "", "reasoning": "thought", "reasoning_content": "thought",
		"reasoning_details": cloneMessageForSend(replayDetails)}
}

func nousLane(model string, db *state.DB) *AIAgent {
	a, _ := quietAgent()
	a.provider, a.model, a.baseURL, a.apiMode = "nous", model, "https://inference-api.nousresearch.com/v1", "chat_completions"
	if db != nil {
		a.sessionID, a.sessionDB = "s1", db
	}
	return a
}

func recoverRejection(t *testing.T, a *AIAgent, body string, sent []any) bool {
	s := &LoopState{APIMessages: sent, APIKwargs: map[string]any{"messages": sent}}
	retry, err := recoverBeforeClassification(t.Context(), a, s, &provider.APIError{StatusCode: 400, Message: body, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return retry
}

func TestNamedFieldRejectionStripsOncePerModelAndNeverLoops(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	model := "z-ai/glm-5.3-flash:US"
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli", Model: model}); err != nil {
		t.Fatal(err)
	}
	lane := nousLane(model, db)
	sent := []any{map[string]any{"role": "user", "content": "hi"}, replayTurn()}
	body := "Extra inputs are not permitted, field: 'messages[1].reasoning_details'"
	if recoverRejection(t, lane, "Provider returned error", sent) {
		t.Fatal("generic upstream error counted as a rejection")
	}
	if got, _ := lane.rejectedReasoningCarriers(); len(got) != 0 {
		t.Fatalf("carriers recorded %v", got)
	}
	if !recoverRejection(t, lane, body, sent) {
		t.Fatal("named rejection not retried")
	}
	rejected := lane.reasoningRejectingRoutes[[3]string{"nous", "inference-api.nousresearch.com", model}]
	if !reflect.DeepEqual(rejected, map[string]bool{"reasoning": true, "reasoning_details": true}) {
		t.Fatalf("rejected %v", rejected)
	}
	route := provider.ReasoningReplayRouteFor("chat_completions", "nous", lane.model, lane.baseURL, false, rejected)
	reapplyReasoningEcho(sent, route.Pad, route.Carriers)
	turn := sent[1].(map[string]any)
	_, hasR := turn["reasoning"]
	_, hasRD := turn["reasoning_details"]
	if turn["reasoning_content"] != "thought" || hasR || hasRD {
		t.Fatalf("reshaped turn %v", turn)
	}
	if recoverRejection(t, lane, body, []any{map[string]any{"role": "assistant", "reasoning_details": cloneMessageForSend(replayDetails)}}) {
		t.Fatal("known rejection looped")
	}
	resumed, err := nousLane(model, db).rejectedReasoningCarriers()
	if err != nil || !reflect.DeepEqual(resumed, map[string]bool{"reasoning": true, "reasoning_details": true}) {
		t.Fatalf("resumed carriers %v %v", resumed, err)
	}
	if !provider.ReasoningReplayRouteFor("chat_completions", "nous", "moonshotai/kimi-k3", lane.baseURL, false, nil).Carriers["reasoning_details"] {
		t.Fatal("sibling model lost a carrier")
	}
}

func imageHistory() []any {
	return []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is in this?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,BBBB"}},
		}},
	}
}

func imageAgent(providerName, model string) *AIAgent {
	a, _ := quietAgent()
	a.provider, a.model, a.imageRejectingModels, a.dbFlushScanPrefix = providerName, model, map[[2]string]bool{}, 7
	return a
}

func recoverImages(t *testing.T, a *AIAgent, err error, history, apiMessages []any) (bool, *LoopState) {
	var dicts []map[string]any
	for _, m := range history {
		dicts = append(dicts, m.(map[string]any))
	}
	s := &LoopState{Messages: rowsOf(dicts...), APIMessages: apiMessages, APIKwargs: map[string]any{}, ActiveSystemPrompt: "sys"}
	retry, rerr := recoverBeforeClassification(t.Context(), a, s, err)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return retry, s
}

func TestRejectionNeverReachesPersistedHistory(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	textOnly := &provider.APIError{StatusCode: 400, Message: "This model does not support images.", Body: "This model does not support images."}
	t.Run("test_canonical_history_keeps_its_images", func(t *testing.T) {
		a := imageAgent("text-only-provider", "text-model")
		before := dictsOf(rowsOf(imageHistory()[0].(map[string]any), imageHistory()[1].(map[string]any)))
		wire := imageHistory()
		retry, s := recoverImages(t, a, textOnly, imageHistory(), wire)
		if !retry || !reflect.DeepEqual(dictsOf(s.Messages), before) || a.dbFlushScanPrefix != 7 {
			t.Fatalf("retry %v prefix %v", retry, a.dbFlushScanPrefix)
		}
		if !a.stripImagesForRejectingModel(&wire) || strings.Contains(fmt.Sprint(wire), "image_url") {
			t.Fatalf("wire %v", wire)
		}
	})
	t.Run("test_every_model_in_a_fallback_chain_is_tracked", func(t *testing.T) {
		a := imageAgent("p", "model-a")
		retryA, _ := recoverImages(t, a, textOnly, imageHistory(), []any{})
		a.model = "model-b"
		rebuilt := imageHistory()
		if a.stripImagesForRejectingModel(&rebuilt) || !strings.Contains(fmt.Sprint(rebuilt), "image_url") {
			t.Fatal("model-b stripped before it rejected")
		}
		retryB, _ := recoverImages(t, a, textOnly, imageHistory(), []any{})
		if !retryA || !retryB || !reflect.DeepEqual(a.imageRejectingModels, map[[2]string]bool{{"p", "model-a"}: true, {"p", "model-b"}: true}) {
			t.Fatalf("retry %v %v models %v", retryA, retryB, a.imageRejectingModels)
		}
		if again, _ := recoverImages(t, a, textOnly, imageHistory(), []any{}); again {
			t.Fatal("known rejecting model looped")
		}
		for _, model := range []string{"model-a", "model-b"} {
			a.model = model
			api := imageHistory()
			if !a.stripImagesForRejectingModel(&api) || strings.Contains(fmt.Sprint(api), "image_url") {
				t.Fatalf("%s kept images", model)
			}
		}
		a.model = "model-c"
		api := imageHistory()
		if a.stripImagesForRejectingModel(&api) || strings.Count(fmt.Sprint(api), "data:image/png") != 2 {
			t.Fatal("model-c lost images")
		}
	})
	t.Run("test_a_corrupt_image_does_not_mark_the_model_image_rejecting", func(t *testing.T) {
		body := "Invalid request: prepare image failed: failed to decode image: invalid or unsupported image format"
		a := imageAgent("text-only-provider", "text-model")
		before := dictsOf(rowsOf(imageHistory()[0].(map[string]any), imageHistory()[1].(map[string]any)))
		wire := imageHistory()
		retry, s := recoverImages(t, a, &provider.APIError{StatusCode: 400, Message: body, Body: body}, imageHistory(), wire)
		if !retry || strings.Contains(fmt.Sprint(s.APIMessages), "image_url") || !reflect.DeepEqual(dictsOf(s.Messages), before) || len(a.imageRejectingModels) != 0 {
			t.Fatalf("retry %v wire %v models %v", retry, s.APIMessages, a.imageRejectingModels)
		}
		api := imageHistory()
		if a.stripImagesForRejectingModel(&api) || strings.Count(fmt.Sprint(api), "data:image/png") != 2 {
			t.Fatal("corrupt image marked the model")
		}
	})
}

func TestBedrockEventOrderFallsBackToConverseOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, _ := quietAgent()
	a.provider, a.apiMode, a.client, a.clientKwargs = "bedrock", "anthropic_messages", struct{}{}, map[string]any{"k": 1}
	err := &pyClassErr{"RuntimeError", `Unexpected event order, got content_block_start before "message_start"`}
	s := &LoopState{APIKwargs: map[string]any{}}
	if retry, rerr := recoverBeforeClassification(t.Context(), a, s, err); rerr != nil || !retry {
		t.Fatalf("retry %v err %v", retry, rerr)
	}
	if a.apiMode != "bedrock_converse" || a.bedrockRegion != "us-east-1" || a.client != nil || len(a.clientKwargs) != 0 || !a.bedrockConverseFallbackAttempted {
		t.Fatal("converse fallback not armed")
	}
	a.apiMode = "anthropic_messages"
	if retry, _ := recoverBeforeClassification(t.Context(), a, s, err); retry {
		t.Fatal("converse fallback armed twice")
	}
}
