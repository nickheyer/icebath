package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type rawChatClient struct {
	t       *testing.T
	parsed  any
	headers http.Header
	raws    int
}

func (c *rawChatClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	c.t.Fatal("plain create must not be used when raw is available")
	return nil, nil
}

func (c *rawChatClient) CreateChatCompletionRaw(context.Context, map[string]any) (any, http.Header, error) {
	c.raws++
	return c.parsed, c.headers, nil
}

type plainChatClient struct{ out any }

type offNousChatClient struct{ t *testing.T }

func (c offNousChatClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return "plain", nil
}

func (c offNousChatClient) CreateChatCompletionRaw(context.Context, map[string]any) (any, http.Header, error) {
	c.t.Fatal("raw must not be used off Nous")
	return nil, nil, nil
}

func (c plainChatClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return c.out, nil
}

func windowCapture(state *CreditsState) *CreditsCapture {
	capture := &CreditsCapture{}
	capture.SetRoute("nous", "m", "https://inference-api.nousresearch.com/v1")
	if state != nil {
		capture.adoptCredits(state)
	}
	return capture
}

func TestChatCompletionCapturingHeadersTakesTheRawCallOnlyForAWindowSession(t *testing.T) {
	seeded := cwState(cwDefaults())
	agent := &fakeChatAgent{provider: "nous", credits: windowCapture(seeded)}
	parsed := map[string]any{"choices": []any{}}
	raw := &rawChatClient{t: t, parsed: parsed, headers: cwHeaders(cwDefaultH())}
	got, err := ChatCompletionCapturingHeaders(context.Background(), agent, raw, map[string]any{"model": "m"})
	if err != nil || got == nil || got.(map[string]any)["choices"] == nil || raw.raws != 1 {
		t.Fatalf("raw call result %v %v raws %d", got, err, raw.raws)
	}
	if st := agent.credits.Credits(); st == nil || st == seeded || !st.FromHeader {
		t.Fatalf("window response headers not captured: %+v", st)
	}
	plain := plainChatClient{out: "plain"}
	if got, _ := ChatCompletionCapturingHeaders(context.Background(), &fakeChatAgent{provider: "example", credits: windowCapture(cwState(cwDefaults()))}, offNousChatClient{t: t}, nil); got != "plain" {
		t.Fatalf("another provider used the raw surface: %v", got)
	}
	if got, _ := ChatCompletionCapturingHeaders(context.Background(), agent, plain, nil); got != "plain" {
		t.Fatalf("bare client result %v", got)
	}
	for _, state := range []*CreditsState{nil, cwState(cwDefaults(), func(s *CreditsState) { s.DenominatorKind = "subscription_cap" })} {
		if got, _ := ChatCompletionCapturingHeaders(context.Background(), &fakeChatAgent{provider: "nous", credits: windowCapture(state)}, plain, nil); got != "plain" {
			t.Fatalf("non window session result %v", got)
		}
	}
	if _, err := ChatCompletionCapturingHeaders(context.Background(), agent, struct{}{}, nil); err == nil {
		t.Fatal("client without chat completions accepted")
	}
}

func TestChatCompletionCapturingHeadersLeavesANonWindowResponseUncaptured(t *testing.T) {
	grant := cwWith(cwHeaders(cwDefaultH()), "x-nous-credits-denominator-kind", "subscription_cap")
	remaining := http.Header{}
	remaining.Set("x-nous-credits-remaining-micros", "1")
	for i, h := range []http.Header{{}, grant, remaining} {
		seeded := cwState(cwDefaults())
		agent := &fakeChatAgent{provider: "nous", credits: windowCapture(seeded)}
		raw := &rawChatClient{t: t, parsed: "parsed", headers: h}
		got, err := ChatCompletionCapturingHeaders(context.Background(), agent, raw, nil)
		if err != nil || got != "parsed" || raw.raws != 1 {
			t.Fatalf("response %d result %v %v", i, got, err)
		}
		if agent.credits.Credits() != seeded || agent.credits.RateLimit() != nil {
			t.Fatalf("non window response %d captured", i)
		}
	}
}

func TestOpenAIClientRawCreateReturnsTheResponseHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-nous-credits-remaining-micros", "7")
		_, _ = w.Write([]byte(`{"id":"c","choices":[]}`))
	}))
	defer srv.Close()
	retries := 0
	c := NewOpenAIClient("k", srv.URL, OpenAIClientOptions{MaxRetries: &retries}).(*auxOpenAIClient)
	out, h, err := c.CreateChatCompletionRaw(context.Background(), map[string]any{"model": "m", "messages": []any{}})
	if err != nil || out == nil || h.Get("x-nous-credits-remaining-micros") != "7" {
		t.Fatalf("raw create %v %v %v", out, h, err)
	}
}
