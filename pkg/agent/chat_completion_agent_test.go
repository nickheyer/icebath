package agent

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestDisableStreamingLandsOnTheRetrySession(t *testing.T) {
	a := &AIAgent{}
	if a.RetrySession() != a.RetrySession() || a.RetrySession().DisableStreaming {
		t.Fatal("retry session not stable or streaming already off")
	}
	a.DisableStreaming()
	if !a.RetrySession().DisableStreaming || shouldStream(t.Context(), a) {
		t.Fatal("disable_streaming not honoured by the stream gate")
	}
}

func TestActiveRequestAbortHookRunsOnInterruptAbort(t *testing.T) {
	a := &AIAgent{}
	a.icAbortActiveRequest("idle", "abort failed")
	var reasons []string
	hook := &provider.RequestAbortHook{Abort: func(reason string) bool { reasons = append(reasons, reason); return true }}
	a.SetActiveRequestAbort(hook)
	if a.ActiveRequestAbort() != hook {
		t.Fatal("hook identity lost")
	}
	a.icAbortActiveRequest("interrupt", "abort failed")
	a.SetActiveRequestAbort(&provider.RequestAbortHook{Abort: func(string) bool { panic("boom") }})
	a.icAbortActiveRequest("again", "abort failed")
	if len(reasons) != 1 || reasons[0] != "interrupt" {
		t.Fatalf("reasons %v", reasons)
	}
}

func TestOllamaThinkingProbeIsCachedPerModelAndBaseURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"capabilities":["completion","thinking"]}`))
	}))
	t.Cleanup(srv.Close)
	a := &AIAgent{model: "qwen3:8b", baseURL: srv.URL + "/v1"}
	if !a.OllamaSupportsThinkingCached() || !a.OllamaSupportsThinkingCached() {
		t.Fatal("thinking capability not reported")
	}
	if hits.Load() != 1 {
		t.Fatalf("definitive answer probed %d times", hits.Load())
	}
	a.model = "llama3"
	a.OllamaSupportsThinkingCached()
	if hits.Load() != 2 {
		t.Fatalf("new model key probed %d times", hits.Load())
	}
}

func TestStreamConsumersAndReasoningDeltaWants(t *testing.T) {
	a := &AIAgent{env: promptTestEnv(t)}
	if a.HasStreamConsumers() || a.WantsReasoningDeltas() {
		t.Fatal("consumers reported without callbacks")
	}
	a.streamDeltaCallback = func(string) {}
	if !a.HasStreamConsumers() || !a.WantsReasoningDeltas() {
		t.Fatal("stream delta callback not a consumer")
	}
	b := &AIAgent{env: a.env, streamCallback: func(string) {}}
	if !b.HasStreamConsumers() || b.WantsReasoningDeltas() {
		t.Fatal("stream callback alone is a consumer without reasoning wants")
	}
}

func TestToolGenStartedCallbackFailureIsSwallowed(t *testing.T) {
	var got string
	(&AIAgent{toolGenCallback: func(name string) { got = name }}).FireToolGenStarted("write_file")
	(&AIAgent{toolGenCallback: func(string) { panic("display gone") }}).FireToolGenStarted("x")
	(&AIAgent{}).FireToolGenStarted("y")
	if got != "write_file" {
		t.Fatalf("callback saw %q", got)
	}
}

func TestRequestShapeAccessors(t *testing.T) {
	a := &AIAgent{prefillMessages: []any{map[string]any{"role": "user", "content": "hi"}}, credentialPoolEntryID: "e1"}
	if a.MaxTokens() != nil {
		t.Fatal("unset max_tokens is not None")
	}
	a.maxTokens = iptr(512)
	if mt := a.MaxTokens(); mt == nil || *mt != 512 {
		t.Fatalf("max tokens %v", mt)
	}
	if pf := a.PrefillMessages(); len(pf) != 1 || pf[0]["content"] != "hi" {
		t.Fatalf("prefill %v", pf)
	}
	if a.CredentialPool() != nil {
		t.Fatal("nil pool surfaced as a typed nil")
	}
	a.ClearCredentialPool()
	if a.credentialPoolEntryID != "" {
		t.Fatal("entry id kept after clearing the pool")
	}
	a.SetActiveCodexStreamRequestToken("tok")
	if a.ActiveCodexStreamRequestToken() != "tok" {
		t.Fatal("codex request token lost")
	}
	if hint, err := a.CodexSilentHangHint(nil); err != nil || hint != "" {
		t.Fatalf("hint %q %v", hint, err)
	}
}
