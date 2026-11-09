package provider

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeConnErr struct{}

func (*fakeConnErr) Error() string { return "Connection error." }

func (a *fakeChatAgent) CreateRequestAnthropicClient(reason string) (any, error) {
	return a.anthropicCreate(reason)
}
func (a *fakeChatAgent) AbortRequestAnthropicClient(client any, reason string) error {
	a.mu.Lock()
	a.aAborts = append(a.aAborts, clientCall{client, reason})
	a.mu.Unlock()
	if a.onAbort != nil {
		a.onAbort(client, reason)
	}
	return nil
}
func (a *fakeChatAgent) CloseRequestAnthropicClient(client any, reason string) error {
	a.mu.Lock()
	a.aCloses = append(a.aCloses, clientCall{client, reason})
	a.mu.Unlock()
	return nil
}
func (a *fakeChatAgent) ActiveCodexStreamRequestToken() any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.codexToken
}
func (a *fakeChatAgent) SetActiveCodexStreamRequestToken(token any) {
	a.mu.Lock()
	a.codexToken = token
	a.mu.Unlock()
}
func (a *fakeChatAgent) EmitWaitNotice(text string) {
	a.mu.Lock()
	a.notices = append(a.notices, text)
	a.mu.Unlock()
}
func (a *fakeChatAgent) EmitDiagnosticWait(text string) {
	a.mu.Lock()
	a.diagWaits = append(a.diagWaits, text)
	a.mu.Unlock()
}
func (a *fakeChatAgent) ClientLogContext() string { return "client=fake" }

func (a *fakeChatAgent) reasons(calls *[]clientCall) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for _, c := range *calls {
		out = append(out, c.reason)
	}
	return out
}

func (a *fakeChatAgent) snapshotStrings(list *[]string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), *list...)
}

func (a *fakeChatAgent) setInterrupted(on bool) {
	a.mu.Lock()
	a.interrupted = on
	a.mu.Unlock()
}

// Codex agent on the hosted codex backend with a 60s stale budget and a fresh home
func agentFormCodexAgent(t *testing.T, provider, baseURL string) *fakeChatAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	unsetEnvForTest(t, CodexTTFBTimeoutEnv, CodexTTFBMaxEnv, CodexTTFBDisableAboveEnv, CodexTTFBStrictEnv, CodexEventStaleTimeoutEnv, LocalStreamStaleTimeoutEnv)
	client := &fakeChatClient{}
	return &fakeChatAgent{provider: provider, apiMode: "codex_responses", baseURL: baseURL, model: "gpt-5.5", plat: "cli",
		resolve: func(map[string]any) (any, error) { return 60.0, nil },
		create:  func(string, map[string]any) (any, error) { return client, nil }}
}

func anyContains(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// Wraps the resolver so the implicit policy keeps its branches while the test scales time
func shortenAgentWatchdogs(t *testing.T, edit func(*agentNonstreamWatchdogs)) {
	prev := resolveAgentNonstreamWatchdogs
	resolveAgentNonstreamWatchdogs = func(ctx context.Context, agent ChatCompletionAgent, kw map[string]any) (agentNonstreamWatchdogs, error) {
		w, err := prev(ctx, agent, kw)
		edit(&w)
		return w, err
	}
	t.Cleanup(func() { resolveAgentNonstreamWatchdogs = prev })
}

func TestAgentNonStreamStaleCloseIsSingleOwner(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	aborted := make(chan struct{})
	var once sync.Once
	client := &fakeChatClient{create: func(ctx context.Context, kw map[string]any) (any, error) {
		select {
		case <-aborted:
		case <-time.After(5 * time.Second):
			t.Error("stale watchdog never aborted the request")
		}
		return nil, &fakeConnErr{}
	}}
	agent := &fakeChatAgent{provider: "openrouter", apiMode: "chat_completions", baseURL: "https://openrouter.ai/api/v1", model: "m",
		resolve: func(map[string]any) (any, error) { return 0.5, nil },
		create:  func(string, map[string]any) (any, error) { return client, nil },
		onAbort: func(any, string) { once.Do(func() { close(aborted) }) }}
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "m", "messages": []any{}})
	var ce *fakeConnErr
	if !errors.As(err, &ce) {
		t.Fatalf("the worker's own error must surface, got %v", err)
	}
	if closes := agent.reasons(&agent.closes); len(closes) != 1 || closes[0] != "request_error_cleanup" {
		t.Fatalf("closes %v", closes)
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 1 || aborts[0] != "stale_call_kill" {
		t.Fatalf("aborts %v", aborts)
	}
}

func TestAgentNonStreamBreakerShortCircuitsAndSuccessResets(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "3")
	client := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) { return "resp", nil }}
	agent := &fakeChatAgent{provider: "openrouter", apiMode: "chat_completions", baseURL: "https://openrouter.ai/api/v1", model: "m",
		resolve: func(map[string]any) (any, error) { return 60.0, nil },
		create:  func(string, map[string]any) (any, error) { return client, nil }}
	agent.retry.staleStreak.Store(3)
	if _, err := InterruptibleAPICall(t.Context(), agent, map[string]any{}); err == nil || !strings.Contains(err.Error(), "unresponsive") {
		t.Fatalf("err %v", err)
	}
	if client.calls.Load() != 0 || agent.retry.StaleStreak() != 3 {
		t.Fatalf("calls %d streak %d", client.calls.Load(), agent.retry.StaleStreak())
	}
	agent.retry.staleStreak.Store(2)
	if resp, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "m", "messages": []any{}}); err != nil || resp != "resp" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if agent.retry.StaleStreak() != 0 {
		t.Fatalf("streak %d", agent.retry.StaleStreak())
	}
}

func TestNonStreamingCancelDoesNotSurfaceNetworkError(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	started := make(chan struct{})
	release := make(chan struct{})
	client := &fakeChatClient{create: func(ctx context.Context, kw map[string]any) (any, error) {
		close(started)
		<-release
		return nil, &fakeConnErr{}
	}}
	var agent *fakeChatAgent
	agent = &fakeChatAgent{provider: "openrouter", apiMode: "chat_completions", baseURL: "https://openrouter.ai/api/v1", model: "m",
		resolve: func(map[string]any) (any, error) { return 60.0, nil },
		create:  func(string, map[string]any) (any, error) { return client, nil },
		onAbort: func(any, string) { close(release) }}
	go func() {
		<-started
		agent.setInterrupted(true)
	}()
	_, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "m", "messages": []any{}})
	var ie *InterruptedError
	if !errors.As(err, &ie) || err.Error() != "Agent interrupted during API call" {
		t.Fatalf("err %v", err)
	}
	if aborts := agent.reasons(&agent.aborts); len(aborts) != 1 || aborts[0] != "interrupt_abort" {
		t.Fatalf("aborts %v", aborts)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(agent.reasons(&agent.closes)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closes := agent.reasons(&agent.closes); len(closes) != 1 || closes[0] != "request_error_cleanup" {
		t.Fatalf("closes %v", closes)
	}
}

// Steps the poll clock 300ms per poll and releases the worker on the two hundredth poll
func moaHeartbeatAgent(t *testing.T) (*fakeChatAgent, any) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	response := &struct{ ok bool }{true}
	release := make(chan struct{})
	client := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		<-release
		return response, nil
	}}
	agent := &fakeChatAgent{provider: "moa", apiMode: "chat_completions", plat: "desktop", client: client,
		resolve: func(map[string]any) (any, error) { return math.Inf(1), nil }}
	base := time.Unix(1000, 0)
	var polls atomic.Int64
	prevClock, prevWait := agentNonStreamClock, agentNonStreamPollWait
	agentNonStreamClock = func() time.Time { return base.Add(time.Duration(polls.Load()) * nonStreamPollInterval) }
	agentNonStreamPollWait = func(time.Duration) <-chan time.Time {
		n := polls.Add(1)
		if n > 200 {
			return nil
		}
		if n == 200 {
			close(release)
		}
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	t.Cleanup(func() { agentNonStreamClock, agentNonStreamPollWait = prevClock, prevWait })
	return agent, response
}

type panickingNoticeAgent struct{ *fakeChatAgent }

func (panickingNoticeAgent) EmitWaitNotice(string) { panic("format failure") }

type fakeStreamHandle struct{ closed atomic.Int32 }

func (h *fakeStreamHandle) Close() error { h.closed.Add(1); return nil }

type fakeStreamWithResponse struct{ resp *fakeStreamHandle }

func (s fakeStreamWithResponse) StreamResponse() any { return s.resp }

func TestRequestClientRegistryRoutesByKindAndOwner(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	anthropicClient := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) { return "msg", nil }}
	agent := &fakeChatAgent{provider: "anthropic", apiMode: "anthropic_messages", baseURL: "https://api.anthropic.com", model: "claude",
		resolve:         func(map[string]any) (any, error) { return 60.0, nil },
		anthropicCreate: func(reason string) (any, error) { return anthropicClient, nil }}
	if resp, err := InterruptibleAPICall(t.Context(), agent, map[string]any{"model": "claude"}); err != nil || resp != "msg" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if closes := agent.reasons(&agent.aCloses); len(closes) != 1 || closes[0] != "request_complete" || len(agent.closes) != 0 {
		t.Fatalf("anthropic closes %v openai closes %v", closes, agent.closes)
	}
	reg := newRequestClientRegistry(agent)
	worker, stranger := &registryCaller{}, &registryCaller{}
	reg.setClient(anthropicClient, "anthropic_messages", worker)
	if err := reg.closeOnce("interrupt_abort", stranger); err != nil {
		t.Fatal(err)
	}
	if aborts := agent.reasons(&agent.aAborts); len(aborts) != 1 || aborts[0] != "interrupt_abort" || len(agent.aCloses) != 1 {
		t.Fatalf("stranger must only abort, aborts %v closes %v", aborts, agent.aCloses)
	}
	if err := reg.closeOnce("request_error_cleanup", worker); err != nil || len(agent.aCloses) != 2 {
		t.Fatalf("owner close %v %v", err, agent.aCloses)
	}
	if err := reg.closeOnce("request_error_cleanup", worker); err != nil || len(agent.aCloses) != 2 {
		t.Fatal("a popped client closes once")
	}
	plain := "not closable"
	if got := reg.setStreamHandle(plain, worker); got != plain || reg.client != nil {
		t.Fatal("an unclosable stream stays unregistered")
	}
	inner := &fakeStreamHandle{}
	reg.setStreamHandle(fakeStreamWithResponse{inner}, worker)
	if err := reg.closeOnce("interrupt_abort", stranger); err != nil || inner.closed.Load() != 1 || reg.client != nil {
		t.Fatalf("a stream handle closes from any goroutine, closed %d", inner.closed.Load())
	}
}

func (a *fakeChatAgent) SessionID() string { return a.model + "-session" }

func TestMergeNousPortalMessagesExtraBodyOnlyForNous(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	kwargs := map[string]any{"model": "claude-opus-4-8", "messages": []any{}}
	other := &fakeChatAgent{provider: "anthropic", model: "s"}
	if got := MergeNousPortalMessagesExtraBody(t.Context(), other, kwargs, nil); got["extra_body"] != nil || len(got) != 2 {
		t.Fatalf("non nous %v", got)
	}
	nous := &fakeChatAgent{provider: "nous", model: "sess-abc123"}
	kwargs = map[string]any{"extra_body": map[string]any{"keep": 1}}
	got := MergeNousPortalMessagesExtraBody(t.Context(), nous, kwargs, []map[string]any{{"role": "user", "content": "hi"}})
	extra := got["extra_body"].(map[string]any)
	if extra["keep"] != 1 || extra["session_id"] == nil || extra["provider_preferences"] != nil {
		t.Fatalf("extra %v", extra)
	}
	tags, _ := extra["tags"].([]any)
	if !slicesContainsAny(tags, "product=icebath-agent") || !slicesContainsAny(tags, "conversation=sess-abc123-session") {
		t.Fatalf("tags %v", tags)
	}
	bad := map[string]any{"extra_body": "x"}
	if got := MergeNousPortalMessagesExtraBody(t.Context(), nous, bad, nil); got["extra_body"] != "x" {
		t.Fatalf("a non dict extra_body stays %v", got)
	}
}

func slicesContainsAny(list []any, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestResolveNonstreamWatchdogsExportsTheResolvedFuses(t *testing.T) {
	agent := agentFormCodexAgent(t, "openai-codex", "https://chatgpt.com/backend-api/codex")
	got, err := ResolveNonstreamWatchdogs(context.Background(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := resolveAgentNonstreamWatchdogsOf(context.Background(), agent, map[string]any{"model": "gpt-5.5", "input": "hi"})
	want := NonStreamWatchdogs{IdleEnabled: inner.idleEnabled, TTFBEnabled: inner.ttfbEnabled, IdleTimeout: inner.idle, TTFBTimeout: inner.ttfb, StaleTimeout: inner.stale}
	if got != want || !got.TTFBEnabled || !got.IdleEnabled {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
