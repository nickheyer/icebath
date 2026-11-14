package provider

import (
	"context"
	"sync"
	"testing"
)

func TestPurePerRequestHookIsAskedOncePerArgumentSet(t *testing.T) {
	calls := 0
	p := &Profile{Name: "memo-probe", RequestHeadersHook: func(baseURL, model string, userInitiated bool) map[string]string {
		calls++
		return map[string]string{"X-Model": model}
	}}
	ctx := context.Background()
	kw := map[string]any{"base_url": "https://memo.example", "model": "m1", "user_initiated": true}
	first := AskProfile(ctx, p, "request_headers", nil, nil, nil, kw).(map[string]string)
	second := AskProfile(ctx, p, "request_headers", nil, nil, nil, kw).(map[string]string)
	if calls != 1 || first["X-Model"] != "m1" || second["X-Model"] != "m1" {
		t.Fatalf("calls %d answers %v %v", calls, first, second)
	}
	first["X-Model"] = "mutated"
	if again := AskProfile(ctx, p, "request_headers", nil, nil, nil, kw).(map[string]string); again["X-Model"] != "m1" || calls != 1 {
		t.Fatalf("memoized answer leaked a caller mutation: %v", again)
	}
	kw2 := map[string]any{"base_url": "https://memo.example", "model": "m2", "user_initiated": true}
	if AskProfile(ctx, p, "request_headers", nil, nil, nil, kw2).(map[string]string)["X-Model"] != "m2" || calls != 2 {
		t.Fatalf("a new argument set must ask again, calls %d", calls)
	}
}

func TestPureHookAnswerSurvivesAConcurrentMemoClear(t *testing.T) {
	p := &Profile{Name: "clear-probe", MaxTokensParamHook: func(baseURL, model string) string { return "max_completion_tokens" }}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			kw := map[string]any{"base_url": "https://clear.example", "model": string(rune('a' + i%26))}
			if got := AskProfile(ctx, p, "max_tokens_param", nil, nil, nil, kw); got != "max_completion_tokens" {
				t.Errorf("answer %v", got)
			}
		}()
		go func() {
			defer wg.Done()
			routePureMemoMu.Lock()
			clear(routePureMemo)
			routePureMemoMu.Unlock()
		}()
	}
	wg.Wait()
}

type hookScope struct{ failures map[string]bool }

func (s *hookScope) ProviderHookFailures() map[string]bool { return s.failures }

func TestFailedHookAnswersFallbackForTheRestOfTheScopeSession(t *testing.T) {
	calls := 0
	p := &Profile{Name: "broken", ClientHeadersHook: func(baseURL, apiKey string, hasImages bool) map[string]string {
		calls++
		panic("plugin host lost")
	}}
	ctx := context.Background()
	scope := &hookScope{failures: map[string]bool{}}
	kw := map[string]any{"base_url": "https://broken.example"}
	if got := AskProfile(ctx, p, "client_headers", "generic", scope, nil, kw); got != "generic" || calls != 1 {
		t.Fatalf("got %v calls %d", got, calls)
	}
	if !HookFailed(p, "client_headers", scope) {
		t.Fatal("failure must stick to the scope")
	}
	if got := AskProfile(ctx, p, "client_headers", "generic", scope, nil, kw); got != "generic" || calls != 1 {
		t.Fatalf("a failed hook is not asked again in the same scope, calls %d", calls)
	}
	if AskProfile(ctx, p, "client_headers", "generic", nil, nil, kw); calls != 2 {
		t.Fatalf("a scopeless caller asks again, calls %d", calls)
	}
	if HookFailed(p, "client_headers", nil) {
		t.Fatal("a scopeless caller keeps no failures")
	}
}

func TestErrorPathHookFailureAnswersFallback(t *testing.T) {
	p := &Profile{Name: "err-path", CredentialRefreshNoticeHook: func(reason string) string { panic("boom") }}
	if got := AskOnErrorPath(context.Background(), p, "credential_refresh_notice", "", map[string]any{"reason": "stale"}); got != "" {
		t.Fatalf("got %v", got)
	}
	ok := &Profile{Name: "err-ok", CredentialRefreshNoticeHook: func(reason string) string { return "refreshed " + reason }}
	if got := AskOnErrorPath(context.Background(), ok, "credential_refresh_notice", "", map[string]any{"reason": "stale"}); got != "refreshed stale" {
		t.Fatalf("got %v", got)
	}
}

func TestRouteAnswerWithoutClaimingProfileIsNil(t *testing.T) {
	if got := routeAnswer(context.Background(), "client_headers", "", nil, map[string]any{"base_url": ""}); got != nil {
		t.Fatalf("got %v", got)
	}
}
