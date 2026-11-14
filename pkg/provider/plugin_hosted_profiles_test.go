package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/plugins"
)

type hostedCallRecord struct {
	attr   string
	args   []any
	kwargs map[string]any
}

// Routes every hosted profile call to the canned answers, recording what crossed the host boundary
func fakeHostedCalls(t *testing.T, answers map[string]any) *[]hostedCallRecord {
	t.Helper()
	calls := &[]hostedCallRecord{}
	prev := hostedProfileCallFn
	hostedProfileCallFn = func(_ context.Context, _ *plugins.HostedProfile, attr string, args []any, kwargs map[string]any) (any, error) {
		*calls = append(*calls, hostedCallRecord{attr, args, kwargs})
		return answers[attr], nil
	}
	t.Cleanup(func() { hostedProfileCallFn = prev })
	return calls
}

func TestProfilesFromHostedCopiesFieldsByPythonAttributeName(t *testing.T) {
	h := &plugins.HostedProfile{Name: "acme", Fields: map[string]any{
		"name": "acme", "api_mode": "codex_responses", "aliases": []any{"acme-ai"}, "env_vars": []any{"ACME_API_KEY"},
		"base_url": "https://api.acme.test/v1", "models_url": "", "auth_type": "oauth_external", "supports_streaming": nil,
		"supports_health_check": false, "supports_model_listing": true, "supports_vision": true, "fallback_models": []any{"a-1", "a-2"},
		"default_headers": map[string]any{"X-Acme": "1"}, "fixed_temperature": &plugins.Opaque{TypeName: "object", Text: "<object object>"},
		"default_max_tokens": int64(8192), "responses_replay_item_ids": false, "responses_native_compaction": true,
		"provider_overlay": map[string]any{"transport": "openai_chat", "is_aggregator": true}, "base_url_env_var": "ACME_BASE_URL",
		"vendor_flag": "kept",
	}}
	p := profilesFromHosted(context.Background(), []*plugins.HostedProfile{h})[0]
	if p.Name != "acme" || p.Mode != ModeCodexResponses || !reflect.DeepEqual(p.Aliases, []string{"acme-ai"}) || !reflect.DeepEqual(p.KeyEnv, []string{"ACME_API_KEY"}) {
		t.Fatalf("identity fields = %+v", p)
	}
	if p.AuthType != "oauth_external" || p.Auth != AuthKindOf("oauth_external") || p.SupportsStreaming != nil || p.HealthCheck || !p.Vision {
		t.Fatalf("auth and capability fields = %+v", p)
	}
	if p.ModelsURL != "https://api.acme.test/v1/models" || p.ModelsDevID != "acme" || p.DefaultMaxTokens != 8192 || !p.OmitTemperature || p.FixedTemperature != nil {
		t.Fatalf("derived fields = %q %q %d %v", p.ModelsURL, p.ModelsDevID, p.DefaultMaxTokens, p.OmitTemperature)
	}
	if !p.ResponsesDropReplayItemIDs || p.ResponsesNoNativeCompaction || p.Overlay == nil || p.Overlay.Transport != "openai_chat" || !p.Overlay.IsAggregator || p.BaseURLEnv != "ACME_BASE_URL" {
		t.Fatalf("responses and overlay fields = %+v %+v", p, p.Overlay)
	}
	if p.Headers["X-Acme"] != "1" || !reflect.DeepEqual(p.FallbackModels, []string{"a-1", "a-2"}) || p.Extra["vendor_flag"] != "kept" {
		t.Fatalf("headers fallbacks extra = %v %v %v", p.Headers, p.FallbackModels, p.Extra)
	}
}

func TestProfilesFromHostedRunsEachOverrideInTheHost(t *testing.T) {
	calls := fakeHostedCalls(t, map[string]any{
		"get_max_tokens":          int64(4096),
		"build_api_kwargs_extras": []any{map[string]any{"a": int64(1)}, map[string]any{"b": "x"}},
		"fetch_models":            []any{"m-1", "m-2"},
		"classify_api_error":      map[string]any{"reason": "Billing", "should_fallback": true, "error_context": map[string]any{"k": "v"}},
		"probe_models":            []any{nil, "https://api.acme.test/v1/models", "https://api.acme.test/v1"},
		"fetch_account_usage": plugins.Record{"provider": "acme", "source": "api", "title": "Account limits",
			"fetched_at": &plugins.Opaque{TypeName: "datetime", Text: "datetime.datetime(2026, 11, 6, 12, 30, 5, tzinfo=datetime.timezone.utc)"},
			"windows":    []any{plugins.Record{"label": "Daily", "used_percent": 42.5, "scope": "account"}}},
	})
	h := &plugins.HostedProfile{Name: "acme", Fields: map[string]any{"name": "acme"}, Calls: map[string]plugins.HostedProfileCall{
		"get_max_tokens": {}, "build_api_kwargs_extras": {}, "fetch_models": {}, "classify_api_error": {Field: true},
		"probe_models": {}, "fetch_account_usage": {}, "custom_helper": {},
	}}
	p := profilesFromHosted(context.Background(), []*plugins.HostedProfile{h})[0]
	if got := p.GetMaxTokensHook("m"); got != 4096 {
		t.Fatalf("get_max_tokens = %d", got)
	}
	a, b := p.BuildAPIKwargsExtrasHook(&Reasoning{Effort: "high"}, map[string]any{"model": "m"})
	if a["a"] != int64(1) || b["b"] != "x" {
		t.Fatalf("build_api_kwargs_extras = %v %v", a, b)
	}
	ids, listed := p.FetchModelsHook(context.Background(), "", "https://api.acme.test/v1", 8*time.Second)
	if !listed || !reflect.DeepEqual(ids, []string{"m-1", "m-2"}) {
		t.Fatalf("fetch_models = %v %v", ids, listed)
	}
	vd := p.ClassifyAPIError(ClassifyInput{Model: "m"}, 403, "", "quota", "denied", nil)
	if vd == nil || vd.Reason != ReasonBilling || vd.ShouldFallback == nil || !*vd.ShouldFallback || vd.Context["k"] != "v" {
		t.Fatalf("classify verdict = %+v", vd)
	}
	probe := p.ProbeModelsHook(context.Background(), "https://api.acme.test/v1", "", 5*time.Second)
	if probe == nil || probe.Listed || probe.ProbedURL != "https://api.acme.test/v1/models" || probe.ResolvedBaseURL != "https://api.acme.test/v1" {
		t.Fatalf("probe = %+v", probe)
	}
	snap, err := p.FetchAccountUsageHook(context.Background(), "https://api.acme.test/v1", "k")
	if err != nil || snap.Provider != "acme" || !snap.FetchedAt.Equal(time.Date(2026, 11, 6, 12, 30, 5, 0, time.UTC)) || len(snap.Windows) != 1 || *snap.Windows[0].UsedPercent != 42.5 {
		t.Fatalf("usage snapshot = %+v %v", snap, err)
	}
	if _, ok := p.Extra["custom_helper"]; !ok {
		t.Fatal("an override with no Go hook is not reachable from the profile")
	}
	byAttr := map[string]hostedCallRecord{}
	for _, c := range *calls {
		byAttr[c.attr] = c
	}
	if c := byAttr["build_api_kwargs_extras"]; c.kwargs["model"] != "m" || c.kwargs["reasoning_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("build_api_kwargs_extras kwargs = %v", c.kwargs)
	}
	if c := byAttr["classify_api_error"]; c.kwargs["status_code"] != 403 || c.kwargs["error_code"] != "quota" || c.kwargs["message"] != "denied" || c.kwargs["model"] != "m" {
		t.Fatalf("classify kwargs = %v", c.kwargs)
	}
	if c := byAttr["fetch_models"]; c.kwargs["api_key"] != nil || c.kwargs["timeout"] != 8.0 {
		t.Fatalf("fetch_models kwargs = %v", c.kwargs)
	}
}

func TestHostedVerdictDeclinesUnknownReasonsLikeTheProfileClassifier(t *testing.T) {
	if hostedVerdict(map[string]any{"reason": "not-a-reason"}) != nil || hostedVerdict(nil) != nil || hostedVerdict("billing") != nil {
		t.Fatal("an unknown or missing reason produced a verdict")
	}
}

type fakeHostedObject struct {
	attrs  map[string]any
	result any
	kwargs *map[string]any
}

func (o *fakeHostedObject) Get(_ context.Context, name string) (any, error) {
	return o.attrs[name], nil
}

func (o *fakeHostedObject) Call(_ context.Context, method string, _ []any, kwargs map[string]any) (any, error) {
	if method == "create" {
		*o.kwargs = kwargs
	}
	return o.result, nil
}

func TestCreateClientProfileServesCompletionsThroughTheLiveHostClient(t *testing.T) {
	var sent map[string]any
	completion := plugins.Record{"id": "c1", "model": "acme-1", "object": "chat.completion",
		"choices": []any{map[string]any{"index": int64(0), "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "hello from the host"}}},
		"usage":   map[string]any{"prompt_tokens": int64(3), "completion_tokens": int64(4), "total_tokens": int64(7)}}
	completions := &fakeHostedObject{result: completion, kwargs: &sent}
	root := &fakeHostedObject{attrs: map[string]any{"chat": &fakeHostedObject{attrs: map[string]any{"completions": completions}, kwargs: &sent}}, kwargs: &sent}
	var gotKwargs map[string]any
	prev := hostedProfileCreateClientFn
	hostedProfileCreateClientFn = func(_ context.Context, h *plugins.HostedProfile, clientKwargs map[string]any) (hostedClientObject, error) {
		gotKwargs = clientKwargs
		return root, nil
	}
	t.Cleanup(func() { hostedProfileCreateClientFn = prev })
	h := &plugins.HostedProfile{Name: "ownclient", Fields: map[string]any{"name": "ownclient"}, InProcess: "it overrides create_client(), which returns a live SDK client"}
	p := profilesFromHosted(context.Background(), []*plugins.HostedProfile{h})[0]
	client, err := p.CreateClient(context.Background(), map[string]any{"api_key": "sk-test", "base_url": "https://own.test/v1"})
	if err != nil || client == nil || gotKwargs["api_key"] != "sk-test" || client.Route().BaseURL != "https://own.test/v1" {
		t.Fatalf("client %v err %v kwargs %v", client, err, gotKwargs)
	}
	req, _, err := auxRequestFromChatKwargs(map[string]any{"model": "acme-1", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Complete(context.Background(), req)
	if err != nil || resp.Message.Text() != "hello from the host" || resp.Usage.InputTokens != 3 {
		t.Fatalf("complete = %+v %v", resp, err)
	}
	if sent["model"] != "acme-1" || sent["stream"] != nil {
		t.Fatalf("create kwargs = %v", sent)
	}
	var texts []string
	resp, err = client.Stream(context.Background(), req, func(ev StreamEvent) error {
		if ev.Kind == EventTextDelta {
			texts = append(texts, ev.Text)
		}
		return nil
	})
	if err != nil || resp.Message.Text() != "hello from the host" || strings.Join(texts, "") != "hello from the host" || sent["stream"] != true {
		t.Fatalf("stream over a final object = %+v %v %v %v", resp, err, texts, sent)
	}
	if !client.(*hostedSDKClient).cc.nonStreaming {
		t.Fatal("a final completion object for a streamed call did not switch the client to non streaming")
	}
}
