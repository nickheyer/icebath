package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// One captured x_search request
type xsCaptured struct {
	url     string
	headers http.Header
	body    map[string]any
}

// Round tripper backed by a function
type xsRoundTrip func(*http.Request) (*http.Response, error)

func (f xsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// HTTP response with a body for a request
func xsResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: req}
}

// Routes x_search HTTP through a scripted handler and records every request
func stubXSearchHTTP(t *testing.T, fn func(n int, req *http.Request) (*http.Response, error)) *[]xsCaptured {
	t.Helper()
	var captured []xsCaptured
	prevClient, prevSleep, prevNow := xSearchHTTPClient, xSearchSleep, xSearchNow
	xSearchHTTPClient = func(timeout time.Duration) *http.Client {
		return &http.Client{Timeout: timeout, Transport: xsRoundTrip(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			var body map[string]any
			json.Unmarshal(raw, &body)
			captured = append(captured, xsCaptured{url: req.URL.String(), headers: req.Header.Clone(), body: body})
			return fn(len(captured), req)
		})}
	}
	xSearchSleep = func(ctx context.Context, d time.Duration) error { return nil }
	xSearchNow = func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { xSearchHTTPClient, xSearchSleep, xSearchNow = prevClient, prevSleep, prevNow })
	return &captured
}

// Env with no xAI credentials leaking in from the host
func xsEnv(t *testing.T) *Env {
	t.Helper()
	for _, v := range []string{"XAI_API_KEY", "XAI_BASE_URL", "ICEBATH_XAI_BASE_URL"} {
		t.Setenv(v, "")
	}
	prev := CredentialPools
	CredentialPools = nil
	t.Cleanup(func() { CredentialPools = prev })
	return isrcEnv(t, "local")
}

// Pool double selecting one xai-oauth entry
type fakePools struct {
	token, base string
	asked       []string
	refreshed   string
}

func (f *fakePools) SelectPooled(env *Env, provider string) (string, string, bool) {
	f.asked = append(f.asked, provider)
	if provider != "xai-oauth" {
		return "", "", false
	}
	return f.token, f.base, f.token != ""
}

func (f *fakePools) RefreshMatching(env *Env, provider, apiKeyHint string) (string, string, bool) {
	f.asked = append(f.asked, "refresh:"+apiKeyHint)
	if f.refreshed == "" {
		return "", "", false
	}
	return f.refreshed, f.base, true
}

func TestXSearchPostsResponsesRequest(t *testing.T) {
	env := xsEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	captured := stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 200, `{"output_text":"People on X are discussing xAI's latest launch.","citations":[{"url":"https://x.com/example/status/1","title":"Example post"}]}`), nil
	})
	r := decodeResult(t, xSearchTool(context.Background(), env, "What are people saying about xAI on X?", []any{"xai", "@grok"}, nil, "2026-04-01", "2026-04-10", true, false))
	c := (*captured)[0]
	tool := c.body["tools"].([]any)[0].(map[string]any)
	if c.url != "https://api.x.ai/v1/responses" || c.headers.Get("User-Agent") != "Icebath-Agent/"+icebathVersion() || c.headers.Get("Authorization") != "Bearer xai-test-key" {
		t.Fatalf("request %+v", c)
	}
	if c.body["model"] != "grok-4.5" || c.body["store"] != false || c.body["reasoning"] != nil {
		t.Fatalf("payload %v", c.body)
	}
	if tool["type"] != "x_search" || tool["from_date"] != "2026-04-01" || tool["to_date"] != "2026-04-10" || tool["enable_image_understanding"] != true || tool["enable_video_understanding"] != nil {
		t.Fatalf("tool def %v", tool)
	}
	if h := tool["allowed_x_handles"].([]any); len(h) != 2 || h[0] != "xai" || h[1] != "grok" {
		t.Fatalf("handles %v", h)
	}
	input := c.body["input"].([]any)[0].(map[string]any)
	if input["role"] != "user" || input["content"] != "What are people saying about xAI on X?" {
		t.Fatalf("input %v", input)
	}
	if r["success"] != true || r["answer"] != "People on X are discussing xAI's latest launch." || r["credential_source"] != "xai" || r["degraded"] != false {
		t.Fatalf("result %v", r)
	}
}

func TestXSearchConfigSection(t *testing.T) {
	env := xsEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	env.Config.Set("x_search", map[string]any{"model": "grok-x", "reasoning_effort": "HIGH", "timeout_seconds": 5, "retries": -3})
	captured := stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 503, `upstream down`), nil
	})
	r := decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "", "", false, false))
	if len(*captured) != 1 || (*captured)[0].body["model"] != "grok-x" || (*captured)[0].body["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("captured %+v", *captured)
	}
	if r["error_type"] != "HTTPError" || r["error"] != "upstream down" {
		t.Fatalf("result %v", r)
	}
	if xSearchInt(env, "timeout_seconds", defaultXSearchTimeoutSeconds, 30) != 30 || xSearchInt(env, "retries", defaultXSearchRetries, 0) != 0 {
		t.Fatal("floors not applied")
	}
	env.Config.Set("x_search.timeout_seconds", "abc")
	if xSearchInt(env, "timeout_seconds", defaultXSearchTimeoutSeconds, 30) != 180 {
		t.Fatal("unparseable value must fall back to the default")
	}
	env.Config.Set("x_search.reasoning_effort", "turbo")
	r = decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "", "", false, false))
	if r["error"] != "x_search.reasoning_effort must be one of: low, medium, high, xhigh (got 'turbo')" {
		t.Fatalf("effort error %v", r)
	}
}

func TestXSearchFilterValidation(t *testing.T) {
	env := xsEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	captured := stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 200, `{"output_text":"x"}`), nil
	})
	cases := []struct {
		allowed, excluded any
		from, to          string
		want              string
	}{
		{[]any{"xai"}, []any{"grok"}, "", "", "allowed_x_handles and excluded_x_handles cannot be used together"},
		{[]any{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}, nil, "", "", "allowed_x_handles supports at most 10 handles"},
		{nil, nil, "2026/04/01", "", "from_date must be YYYY-MM-DD (got '2026/04/01')"},
		{nil, nil, "", "2026-02-30", "to_date must be YYYY-MM-DD (got '2026-02-30')"},
		{nil, nil, "2026-04-10", "2026-04-01", "from_date (2026-04-10) must be on or before to_date (2026-04-01)"},
		{nil, nil, "2026-12-01", "", "from_date (2026-12-01) is in the future; X Search only indexes past posts (today UTC is 2026-11-04)"},
	}
	for _, c := range cases {
		r := decodeResult(t, xSearchTool(context.Background(), env, "q", c.allowed, c.excluded, c.from, c.to, false, false))
		if r["error"] != c.want || r["success"] != nil {
			t.Errorf("%v %v %q %q: %v", c.allowed, c.excluded, c.from, c.to, r)
		}
	}
	if len(*captured) != 0 {
		t.Fatal("invalid filters must fail before any request")
	}
	if r := decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "2026-4-1", "2026-12-31", false, false)); r["success"] != true {
		t.Fatalf("single digit month and day and a future to_date are valid: %v", r)
	}
	if r := decodeResult(t, xSearchTool(context.Background(), env, "   ", nil, nil, "", "", false, false)); r["error"] != "query is required for x_search" {
		t.Fatalf("blank query %v", r)
	}
}

func TestXSearchExtractsInlineURLCitations(t *testing.T) {
	env := xsEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 200, `{"output":[{"type":"message","content":[{"type":"output_text","text":"xAI posted an update on X.","annotations":[{"type":"url_citation","url":"https://x.com/xai/status/123","title":"xAI update","start_index":0,"end_index":3},{"type":"file_citation","url":"ignored"}]}]},{"type":"reasoning","content":[{"type":"text","text":"hidden"}]}]}`), nil
	})
	r := decodeResult(t, xSearchTool(context.Background(), env, "latest post from xai", nil, nil, "", "", false, false))
	inline := r["inline_citations"].([]any)
	want := map[string]any{"url": "https://x.com/xai/status/123", "title": "xAI update", "start_index": 0.0, "end_index": 3.0}
	if r["success"] != true || r["answer"] != "xAI posted an update on X." || len(inline) != 1 {
		t.Fatalf("%v", r)
	}
	for k, v := range want {
		if inline[0].(map[string]any)[k] != v {
			t.Fatalf("citation %v", inline[0])
		}
	}
}

func TestXSearchStructuredHTTPError(t *testing.T) {
	env := xsEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 403, `{"code":"forbidden","error":"x_search is not enabled for this model"}`), nil
	})
	raw := xSearchTool(context.Background(), env, "latest xai discussion", nil, nil, "", "", false, false)
	r := decodeResult(t, raw)
	if r["success"] != false || r["provider"] != "xai" || r["tool"] != "x_search" || r["error_type"] != "HTTPError" || r["error"] != "forbidden: x_search is not enabled for this model" {
		t.Fatalf("%v", r)
	}
	if !strings.HasPrefix(raw, `{"success":false,"provider":"xai","tool":"x_search","error":`) {
		t.Fatalf("key order %q", raw)
	}
	stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 400, `{"code":"bad","error":{"detail":"nested"}}`), nil
	})
	r = decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "", "", false, false))
	if r["error"] != "bad: {'detail': 'nested'}" {
		t.Fatalf("nested error repr %v", r["error"])
	}
	stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 404, ``), nil
	})
	r = decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "", "", false, false))
	if r["error"] != "404 Client Error: Not Found for url: https://api.x.ai/v1/responses" {
		t.Fatalf("empty body error %v", r["error"])
	}
}

func TestXSearchRetries(t *testing.T) {
	env := xsEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	captured := stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		if n == 1 {
			return xsResponse(req, 502, `{"error":"bad gateway"}`), nil
		}
		if n == 2 {
			return nil, errors.New("connection reset by peer")
		}
		return xsResponse(req, 200, `{"output_text":"third time"}`), nil
	})
	var sleeps []time.Duration
	xSearchSleep = func(ctx context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	r := decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "", "", false, false))
	if r["answer"] != "third time" || len(*captured) != 3 || len(sleeps) != 2 || sleeps[0] != 1500*time.Millisecond || sleeps[1] != 3*time.Second {
		t.Fatalf("%v requests %d sleeps %v", r, len(*captured), sleeps)
	}
	captured = stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 401, `{"error":"bad key"}`), nil
	})
	if r := decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "", "", false, false)); r["error"] != "bad key" || len(*captured) != 1 {
		t.Fatalf("4xx must not retry: %v %d", r, len(*captured))
	}
	stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return nil, &timeoutErr{}
	})
	if r := decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "", "", false, false)); r["error"] != "xAI x_search timed out after 180 seconds" || r["error_type"] != "ReadTimeout" {
		t.Fatalf("timeout %v", r)
	}
	stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 200, `not json`), nil
	})
	if r := decodeResult(t, xSearchTool(context.Background(), env, "q", nil, nil, "", "", false, false)); r["error_type"] != "JSONDecodeError" || r["error"] != "Expecting value: line 1 column 1 (char 0)" {
		t.Fatalf("decode error %v", r)
	}
}

// Network error reporting a timeout
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "read tcp: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestXSearchDegradedFlag(t *testing.T) {
	env := xsEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 200, `{"output_text":"broad answer","citations":[]}`), nil
	})
	r := decodeResult(t, xSearchTool(context.Background(), env, "anything", nil, nil, "", "", false, false))
	if r["degraded"] != false || r["degraded_reason"] != nil {
		t.Fatalf("no filters %v", r)
	}
	r = decodeResult(t, xSearchTool(context.Background(), env, "anything", []any{"xai"}, nil, "2026-01-01", "", false, false))
	if r["degraded"] != true || r["degraded_reason"] != "no citations returned despite filters: allowed_x_handles, from_date" {
		t.Fatalf("filters %v", r)
	}
}

func TestXSearchCredentials(t *testing.T) {
	t.Run("oauth only", func(t *testing.T) {
		env := xsEnv(t)
		CredentialPools = &fakePools{token: "oauth-bearer-token", base: "https://api.x.ai/v1"}
		if !checkXSearchRequirements(env) {
			t.Fatal("oauth credentials must satisfy the check")
		}
		captured := stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
			return xsResponse(req, 200, `{"output_text":"Found posts via OAuth."}`), nil
		})
		r := decodeResult(t, xSearchTool(context.Background(), env, "anything about xai", nil, nil, "", "", false, false))
		if r["credential_source"] != "xai-oauth" || (*captured)[0].headers.Get("Authorization") != "Bearer oauth-bearer-token" {
			t.Fatalf("%v", r)
		}
	})
	t.Run("no credentials", func(t *testing.T) {
		env := xsEnv(t)
		if checkXSearchRequirements(env) {
			t.Fatal("no credentials must fail the check")
		}
		r := decodeResult(t, xSearchTool(context.Background(), env, "anything", nil, nil, "", "", false, false))
		if !strings.Contains(r["error"].(string), "No xAI credentials available") {
			t.Fatalf("%v", r)
		}
	})
	t.Run("explicit api key preferred over oauth", func(t *testing.T) {
		env := xsEnv(t)
		t.Setenv("XAI_API_KEY", "paid-key-x1")
		CredentialPools = &fakePools{token: "oauth-key-x1", base: "https://api.x.ai/v1"}
		captured := stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
			return xsResponse(req, 200, `{"output_text":"Real posts with citations."}`), nil
		})
		r := decodeResult(t, xSearchTool(context.Background(), env, "from:elon latest", nil, nil, "", "", false, false))
		if r["credential_source"] != "xai" || (*captured)[0].headers.Get("Authorization") != "Bearer paid-key-x1" {
			t.Fatalf("%v", r)
		}
		if creds := resolveXAIHTTPCredentials(env, false); creds.Provider != "xai-oauth" {
			t.Fatalf("default order prefers oauth: %+v", creds)
		}
	})
	t.Run("bearer falls back to oauth without an api key", func(t *testing.T) {
		env := xsEnv(t)
		CredentialPools = &fakePools{token: "oauth-key-x1", base: ""}
		key, base, source, err := resolveXAIBearer(env)
		if err != nil || key != "oauth-key-x1" || base != "https://api.x.ai/v1" || source != "xai-oauth" {
			t.Fatalf("%q %q %q %v", key, base, source, err)
		}
	})
	t.Run("base url override pinned to the xai origin", func(t *testing.T) {
		env := xsEnv(t)
		t.Setenv("XAI_API_KEY", "k")
		t.Setenv("ICEBATH_XAI_BASE_URL", "https://evil.example.com/v1")
		if _, base, _, _ := resolveXAIBearer(env); base != DefaultXAIBaseURL {
			t.Fatalf("foreign override accepted %q", base)
		}
		t.Setenv("ICEBATH_XAI_BASE_URL", "http://api.x.ai/v1")
		if _, base, _, _ := resolveXAIBearer(env); base != DefaultXAIBaseURL {
			t.Fatalf("cleartext override accepted %q", base)
		}
		t.Setenv("ICEBATH_XAI_BASE_URL", "https://eu.api.x.ai/v1/")
		if _, base, _, _ := resolveXAIBearer(env); base != "https://eu.api.x.ai/v1" {
			t.Fatalf("xai subdomain override rejected %q", base)
		}
	})
}

func TestXSearchRegistered(t *testing.T) {
	registerXSearchTool()
	d := Default.Get("x_search")
	if d == nil || d.Toolset != "x_search" || d.Emoji != "\U0001F426" || d.MaxResultChars != 100_000 || len(d.RequiresEnv) != 1 || d.RequiresEnv[0] != "XAI_API_KEY" {
		t.Fatalf("%+v", d)
	}
	env := xsEnv(t)
	t.Setenv("XAI_API_KEY", "xai-test-key")
	captured := stubXSearchHTTP(t, func(n int, req *http.Request) (*http.Response, error) {
		return xsResponse(req, 200, `{"output_text":"ok"}`), nil
	})
	d.Handler(context.Background(), &Call{Env: env, Args: map[string]any{"query": "q", "excluded_x_handles": []any{"@spam"}, "enable_video_understanding": 1.0}})
	tool := (*captured)[0].body["tools"].([]any)[0].(map[string]any)
	if tool["excluded_x_handles"].([]any)[0] != "spam" || tool["enable_video_understanding"] != true {
		t.Fatalf("handler args %v", tool)
	}
}
