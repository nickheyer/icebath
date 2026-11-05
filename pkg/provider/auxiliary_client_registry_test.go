package provider

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type auxLogRecord struct {
	level slog.Level
	msg   string
	attrs int
}

// Records every default logger record with its level for the aux log assertions
type auxLogCapture struct {
	mu      sync.Mutex
	records []auxLogRecord
}

func (c *auxLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *auxLogCapture) Handle(_ context.Context, rec slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, auxLogRecord{rec.Level, rec.Message, rec.NumAttrs()})
	return nil
}
func (c *auxLogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *auxLogCapture) WithGroup(string) slog.Handler      { return c }

func (c *auxLogCapture) matching(sub string) []auxLogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []auxLogRecord
	for _, r := range c.records {
		if strings.Contains(r.msg, sub) {
			out = append(out, r)
		}
	}
	return out
}

func auxCaptureLogs(t *testing.T) *auxLogCapture {
	t.Helper()
	c := &auxLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

const auxMinimaxInferenceBaseURL = "https://api.minimax.io/anthropic"

func auxMinimaxSeams(t *testing.T) {
	t.Helper()
	auxLoggedMinimaxAbsentKeys.clear()
	auxLoggedMinimaxUnexpectedKeys.clear()
	t.Cleanup(func() {
		auxMinimaxRuntimeCredentialsFn = resolveMinimaxOAuthRuntimeCredentialsWith
		auxProviderAuthStateFn = func(env PoolEnv, provider string) (map[string]any, error) {
			return env.Store.GetProviderAuthState(provider)
		}
		auxLoggedMinimaxAbsentKeys.clear()
		auxLoggedMinimaxUnexpectedKeys.clear()
	})
}

func TestResolveMinimaxOAuthMissingCredentialsLogOnceDebug(t *testing.T) {
	ctx := modelsTestHome(t)
	auxMinimaxSeams(t)
	auxMinimaxRuntimeCredentialsFn = func(PoolEnv, bool) (map[string]any, error) {
		return nil, &AuthError{Message: "Not logged into MiniMax OAuth.", Provider: "minimax-oauth", Code: "not_logged_in", ReloginRequired: true}
	}
	logs := auxCaptureLogs(t)
	var resolved auxResolveResult
	for i := 0; i < 5; i++ {
		var err error
		resolved, err = RegistryAuthTypeArms["oauth_minimax"](ctx, auxResolveRequest{Provider: "minimax-oauth", Model: "MiniMax-M3"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if resolved.Client != nil || resolved.Model != "" {
		t.Fatalf("absent credentials resolve to nothing, got %+v", resolved)
	}
	recs := logs.matching("minimax-oauth")
	if len(recs) != 1 || recs[0].level != slog.LevelDebug {
		t.Fatalf("expected exactly one deduped debug record, got %+v", recs)
	}
	for _, r := range logs.records {
		if r.level >= slog.LevelWarn || r.attrs > 0 {
			t.Fatalf("logged out minimax-oauth must not warn or carry a traceback: %+v", r)
		}
	}
}

func TestResolveMinimaxOAuthProbeReadsRawStateNoRefresh(t *testing.T) {
	ctx := modelsTestHome(t)
	auxMinimaxSeams(t)
	var asked []string
	auxProviderAuthStateFn = func(_ PoolEnv, provider string) (map[string]any, error) {
		asked = append(asked, provider)
		return map[string]any{"access_token": "expired-but-present", "refresh_token": "rt",
			"expires_at": "2001-01-01T00:00:00+00:00", "inference_base_url": auxMinimaxInferenceBaseURL}, nil
	}
	resolveCalled := false
	auxMinimaxRuntimeCredentialsFn = func(PoolEnv, bool) (map[string]any, error) {
		resolveCalled = true
		return nil, nil
	}
	res, err := RegistryAuthTypeArms["oauth_minimax"](AuxProbeMode(ctx), auxResolveRequest{Provider: "minimax-oauth", Model: "MiniMax-M3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "MiniMax-M3" {
		t.Fatalf("model = %q", res.Model)
	}
	probe, ok := res.Client.(*auxProbeClient)
	if !ok || probe.BaseURL() != auxMinimaxInferenceBaseURL {
		t.Fatalf("probe mode must answer minimax-oauth with a probe client from the raw persisted state, got %#v", res.Client)
	}
	if resolveCalled || len(asked) != 1 || asked[0] != "minimax-oauth" {
		t.Fatalf("resolver called %v, state asked %v", resolveCalled, asked)
	}
}

func TestResolveMinimaxOAuthProbeWithoutTokenIsAbsent(t *testing.T) {
	ctx := modelsTestHome(t)
	auxMinimaxSeams(t)
	auxProviderAuthStateFn = func(PoolEnv, string) (map[string]any, error) {
		return map[string]any{"inference_base_url": auxMinimaxInferenceBaseURL}, nil
	}
	logs := auxCaptureLogs(t)
	res, err := RegistryAuthTypeArms["oauth_minimax"](AuxProbeMode(ctx), auxResolveRequest{Provider: "minimax-oauth"})
	if err != nil || res.Client != nil {
		t.Fatalf("got %+v, %v", res, err)
	}
	if recs := logs.matching("minimax-oauth not logged in"); len(recs) != 1 || recs[0].level != slog.LevelDebug {
		t.Fatalf("records %+v", recs)
	}
}
