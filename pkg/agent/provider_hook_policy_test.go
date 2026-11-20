package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

var _ provider.ProviderHookFailureScope = (*AIAgent)(nil)

func TestAFailedHookWarnsPerSessionAndIsAskedAgainAfterAReset(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	var calls []string
	profile := &provider.Profile{Name: "hookpolicy", BaseURL: "https://api.hookpolicy.test/v1",
		RequestHeadersHook: func(baseURL, model string, userInitiated bool) map[string]string {
			calls = append(calls, model)
			panic("plugin bug")
		}}
	first, second := &AIAgent{}, &AIAgent{}
	ctx := context.Background()
	kw := map[string]any{"base_url": "https://api.hookpolicy.test/v1", "model": "m"}
	ask := func(a *AIAgent) {
		got, ok := provider.AskProfile(ctx, profile, "request_headers", map[string]string{}, a, nil, kw).(map[string]string)
		if !ok || len(got) != 0 {
			t.Fatalf("failing hook answered %v", got)
		}
	}
	for _, a := range []*AIAgent{first, first, second} {
		ask(a)
	}
	first.providerHookFailures = map[string]bool{}
	ask(first)
	if len(calls) != 3 {
		t.Fatalf("hook asked %d times, want first once, second once, first again after the reset", len(calls))
	}
	if n := strings.Count(logs.String(), "request_headers hook failed"); n != 3 {
		t.Fatalf("%d warnings, want one per session:\n%s", n, logs.String())
	}
}
