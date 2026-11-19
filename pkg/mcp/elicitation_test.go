package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

type consentCall struct {
	env                  *tools.Env
	message, description string
	opts                 tools.ElicitationOptions
}

func setConsent(t *testing.T, fn func(ctx context.Context, env *tools.Env) string) *[]consentCall {
	t.Helper()
	var mu sync.Mutex
	calls := &[]consentCall{}
	old := elicitationConsent
	elicitationConsent = func(ctx context.Context, env *tools.Env, message, description string, o tools.ElicitationOptions) string {
		mu.Lock()
		*calls = append(*calls, consentCall{env, message, description, o})
		mu.Unlock()
		return fn(ctx, env)
	}
	t.Cleanup(func() { elicitationConsent = old })
	return calls
}

func answer(a string) func(context.Context, *tools.Env) string {
	return func(context.Context, *tools.Env) string { return a }
}

func formParams(message string, schema string) *ElicitParams {
	var p ElicitParams
	raw := `{"mode":"form","message":` + mustQuote(message) + `,"requestedSchema":` + schema + `}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		panic(err)
	}
	return &p
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func newTestElicitor(cfg map[string]any, callContext func() context.Context) *elicitationHandler {
	return newElicitationHandler("pay", &tools.Env{Platform: "cli"}, cfg, callContext).(*elicitationHandler)
}

// Asker answering every question batch with the scripted answers in order
type scriptedAsker struct {
	answers []tools.Answer
	asked   [][]tools.Question
}

func (a *scriptedAsker) Ask(ctx context.Context, qs []tools.Question) (tools.FormResult, error) {
	a.asked = append(a.asked, qs)
	return tools.FormResult{Outcome: "submitted", Answers: a.answers[:len(qs)]}, nil
}

func TestElicitFormModeAcceptAndCancel(t *testing.T) {
	setConsent(t, answer("accept"))
	h := newTestElicitor(map[string]any{"timeout": 5}, nil)
	h.env.Ask = &scriptedAsker{answers: []tools.Answer{{Status: "answered", Selected: []string{"yes"}}}}
	res, rpcErr := h.Elicit(context.Background(), formParams("authorize a payment of $0.50", `{"properties":{"approved":{"type":"boolean"}}}`))
	if rpcErr != nil || res.Action != "accept" || mustJSON(t, res) != `{"action":"accept","content":{"approved":true}}` {
		t.Fatalf("accept = %+v %v", res, rpcErr)
	}
	if m := h.Metrics(); m["accepted"] != 1 || m["declined"] != 0 {
		t.Fatalf("metrics = %v", m)
	}
	setConsent(t, answer("cancel"))
	h = newTestElicitor(map[string]any{"timeout": 5}, nil)
	if res, _ := h.Elicit(context.Background(), formParams("please confirm", `{}`)); res.Action != "cancel" || h.Metrics()["errors"] != 1 {
		t.Fatalf("cancel = %+v %v", res, h.Metrics())
	}
}

func TestElicitFailureModes(t *testing.T) {
	calls := setConsent(t, answer("accept"))
	h := newTestElicitor(map[string]any{"timeout": 5}, nil)
	res, _ := h.Elicit(context.Background(), &ElicitParams{Mode: "url", Message: "open this url", URL: "https://example.com/auth", ElicitationID: "e1"})
	if res.Action != "accept" || res.Content != nil || h.Metrics()["accepted"] != 1 || len(*calls) != 1 || (*calls)[0].opts.URL != "https://example.com/auth" ||
		!strings.Contains((*calls)[0].description, "https://example.com/auth") || h.ElicitationDone("e1") == nil {
		t.Fatalf("url mode = %+v %v %+v", res, h.Metrics(), *calls)
	}
	if _, rpcErr := h.Elicit(context.Background(), &ElicitParams{Mode: "url", Message: "open", URL: "javascript:alert(1)"}); rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("unusable url = %v", rpcErr)
	}
	setConsent(t, func(context.Context, *tools.Env) string { panic("approval system blew up") })
	h = newTestElicitor(map[string]any{"timeout": 5}, nil)
	if res, _ := h.Elicit(context.Background(), formParams("please confirm", `{}`)); res.Action != "decline" || h.Metrics()["errors"] != 1 {
		t.Fatalf("panic = %+v %v", res, h.Metrics())
	}
	oldGrace := elicitationOuterGrace
	elicitationOuterGrace = 0
	t.Cleanup(func() { elicitationOuterGrace = oldGrace })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	setConsent(t, func(context.Context, *tools.Env) string {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		return "accept"
	})
	h = newTestElicitor(map[string]any{"timeout": 0.05}, nil)
	start := time.Now()
	res, _ = h.Elicit(context.Background(), formParams("please confirm", `{}`))
	if res.Action != "cancel" || h.Metrics()["errors"] != 1 || h.timeout != 1 || time.Since(start) >= 2*time.Second {
		t.Fatalf("timeout = %+v %v timeout=%v", res, h.Metrics(), h.timeout)
	}
}

func TestElicitContextBridge(t *testing.T) {
	calls := setConsent(t, answer("accept"))
	gateway := &tools.Env{Platform: "telegram"}
	captured := withCallEnv(context.Background(), gateway)
	h := newTestElicitor(map[string]any{"timeout": 5}, func() context.Context { return captured })
	if res, _ := h.Elicit(context.Background(), formParams("please confirm", `{}`)); res.Action != "accept" {
		t.Fatalf("captured = %+v", res)
	}
	if len(*calls) != 1 || (*calls)[0].env != gateway || (*calls)[0].opts.Surface != "mcp-elicitation/pay" || (*calls)[0].opts.Timeout != 5*time.Second {
		t.Fatalf("consent calls = %+v", *calls)
	}
	calls = setConsent(t, answer("accept"))
	h = newTestElicitor(map[string]any{"timeout": 5}, nil)
	if res, _ := h.Elicit(context.Background(), formParams("please confirm", `{}`)); res.Action != "accept" || len(*calls) != 1 || (*calls)[0].env != h.env {
		t.Fatalf("direct = %+v %+v", res, *calls)
	}
	setConsent(t, answer("decline"))
	h = newTestElicitor(map[string]any{"timeout": 5}, func() context.Context { return nil })
	if res, _ := h.Elicit(context.Background(), formParams("please confirm", `{}`)); res.Action != "decline" {
		t.Fatalf("nil context = %+v", res)
	}
}

func TestElicitRequestedSchemaReachesConsentDescription(t *testing.T) {
	calls := setConsent(t, answer("decline"))
	h := newTestElicitor(map[string]any{"timeout": 5}, nil)
	asker := &scriptedAsker{answers: []tools.Answer{{Status: "answered", Text: "12345"}, {Status: "answered", Text: "4242"}}}
	h.env.Ask = asker
	res, _ := h.Elicit(context.Background(), formParams("authorize a payment of $0.50",
		`{"type":"object","properties":{"zip":{"type":"string"},"card_number":{"type":"string","description":"card to charge"}}}`))
	want := "Answers for MCP server 'pay':\n  zip: \"12345\"\n  card_number: \"4242\""
	if res.Action != "decline" || len(*calls) != 1 || (*calls)[0].description != want || (*calls)[0].message != "Send these answers?" {
		t.Fatalf("consent calls = %+v", *calls)
	}
	if len(asker.asked) != 1 || !strings.Contains(asker.asked[0][0].Text, "authorize a payment of $0.50") || !strings.Contains(asker.asked[0][1].Text, "card to charge") {
		t.Fatalf("questions = %+v", asker.asked)
	}
}
