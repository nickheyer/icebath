package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Writes the shared metrics collection and send consent into the home config
func wireConsent(t *testing.T, dir string, send bool) {
	t.Helper()
	text := "telemetry:\n  shared_metrics:\n    enabled: true\n    send: " + map[bool]string{true: "true", false: "false"}[send] + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

type wireRoundTrip struct {
	mu   sync.Mutex
	sent []http.Header
}

// Answers every attempt with an empty schemas body and keeps the headers it carried
func (w *wireRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	w.mu.Lock()
	w.sent = append(w.sent, req.Header.Clone())
	w.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"gw-req-1"}},
		Body: io.NopCloser(bytes.NewReader([]byte(`{"schemas": {}}`))), Request: req}, nil
}

func TestWireCorrelationIDsGoOnlyWithSendConsent(t *testing.T) {
	for _, send := range []bool{false, true} {
		dir := isolateHome(t)
		wireConsent(t, dir, send)
		rt := &wireRoundTrip{}
		env := &tools.Env{Home: dir}
		client := tools.NewConnectorClientWith(env, &tools.CallRef{TurnID: "turn-1", ToolCallID: "call-1"}, "", &http.Client{Transport: rt},
			func() (string, error) { return "https://gw.test", nil },
			func(string) map[string]string { return map[string]string{"Authorization": "Bearer t"} })
		for range 2 {
			if _, err := client.Schemas(context.Background(), []string{"GMAIL_SEND_EMAIL"}); err != nil {
				t.Fatal(err)
			}
		}
		first, second := rt.sent[0], rt.sent[1]
		if !strings.HasPrefix(first.Get("User-Agent"), "icebath-agent/") {
			t.Fatalf("user agent %q", first.Get("User-Agent"))
		}
		var correlation []string
		for k := range first {
			if l := strings.ToLower(k); strings.HasPrefix(l, "x-nous-hermes-") || l == "x-request-id" {
				correlation = append(correlation, l)
			}
			if strings.HasPrefix(strings.ToLower(k), "x-hermes-") {
				t.Fatalf("unstripped header %s", k)
			}
		}
		if !send {
			if len(correlation) != 0 {
				t.Fatalf("correlation without consent %v", correlation)
			}
			continue
		}
		if first.Get("x-nous-hermes-turn") != "turn-1" || first.Get("x-nous-hermes-tool-call") != "call-1" {
			t.Fatalf("headers %v", first)
		}
		if first.Get("x-request-id") == "" || first.Get("x-request-id") == second.Get("x-request-id") {
			t.Fatalf("request ids %q %q", first.Get("x-request-id"), second.Get("x-request-id"))
		}
	}
}

func TestWireSessionHeaderIsTheConversationRoot(t *testing.T) {
	dir := isolateHome(t)
	wireConsent(t, dir, true)
	t.Setenv("ICEBATH_SESSION_ID", "child-session")
	ctx := provider.WithPortalConversationContext(context.Background(), "root-session")
	headers, err := tools.CorrelationHeaders(ctx, &tools.Env{Home: dir}, nil)
	if err != nil || headers["x-nous-hermes-session"] != "root-session" {
		t.Fatalf("headers %v %v", headers, err)
	}
	if headers, _ := tools.CorrelationHeaders(context.Background(), &tools.Env{Home: dir}, nil); headers["x-nous-hermes-session"] != "child-session" {
		t.Fatalf("session id fallback %v", headers)
	}
}

type wireMint struct {
	mu   sync.Mutex
	sent []map[string]any
	hdrs []http.Header
}

// Gateway at CONNECTOR_GATEWAY_URL minting every requested connector, guest setup skipped and a fixed user token
func wireMintGateway(t *testing.T) *wireMint {
	g := &wireMint{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.sent = append(g.sent, body)
		g.hdrs = append(g.hdrs, r.Header.Clone())
		g.mu.Unlock()
		results := []any{}
		for _, n := range wireSlice(body["connectors"]) {
			results = append(results, map[string]any{"connector": n, "status": "initiated", "connectUrl": "https://l"})
		}
		w.Header().Set("X-Request-Id", "mint-id")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CONNECTOR_GATEWAY_URL", srv.URL)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "t")
	saved := tools.EnsureGuestIdentity
	tools.EnsureGuestIdentity = func(*tools.Env) string { return "" }
	t.Cleanup(func() { tools.EnsureGuestIdentity = saved })
	return g
}

func wireSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func TestWireEachConnectPathNamesItsInitiator(t *testing.T) {
	dir := isolateHome(t)
	g := wireMintGateway(t)
	env := &tools.Env{Home: dir}
	ctx := context.Background()
	var op *tools.ConnectionOperation
	for _, initiator := range []string{tools.InitiatorAgentTool, tools.InitiatorDesktopPage} {
		op = tools.NewConnectionOperation([]*tools.ConnTarget{tools.NewConnTarget("gmail", "connector", "connect")}, "", "", nil)
		client, err := tools.NewManagedClient(env, true, initiator, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := tools.Mint(ctx, env, client, op, []string{"gmail"}, false, tools.ActorBackendWatcher, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := op.Transition("gmail", tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{}); err != nil {
		t.Fatal(err)
	}
	if refusal, err := Reissue(ctx, env, op, []string{"gmail"}, ""); refusal != "" || err != nil {
		t.Fatalf("reissue %q %v", refusal, err)
	}
	var initiators []any
	for _, body := range g.sent {
		initiators = append(initiators, body["initiator"])
	}
	if !reflect.DeepEqual(initiators, []any{"agent_tool", "desktop_page", "card_retry"}) {
		t.Fatalf("initiators %v", initiators)
	}
}

type wireDispatch struct {
	mu   sync.Mutex
	recs []string
}

func (h *wireDispatch) Enabled(context.Context, slog.Level) bool { return true }
func (h *wireDispatch) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *wireDispatch) WithGroup(string) slog.Handler            { return h }

// Keeps the dispatch id of each connector outcome line
func (h *wireDispatch) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "dispatch" {
			h.mu.Lock()
			h.recs = append(h.recs, "dispatch="+a.Value.String())
			h.mu.Unlock()
		}
		return true
	})
	return nil
}

func TestWireTryAgainKeepsTheOpeningCallsIDs(t *testing.T) {
	dir := isolateHome(t)
	wireConsent(t, dir, true)
	g := wireMintGateway(t)
	logs := &wireDispatch{}
	env := &tools.Env{Home: dir, Log: slog.New(logs)}
	op := tools.NewConnectionOperation([]*tools.ConnTarget{tools.NewConnTarget("gmail", "connector", "connect")}, "", "",
		&tools.CallRef{TurnID: "turn-7", ToolCallID: "call-7"})
	for _, to := range []tools.TargetState{tools.TargetInitiated, tools.TargetFailed} {
		if _, err := op.Transition("gmail", to, tools.ActorBackendWatcher, tools.TransitionFields{}); err != nil {
			t.Fatal(err)
		}
	}
	if refusal, err := Reissue(context.Background(), env, op, []string{"gmail"}, ""); refusal != "" || err != nil {
		t.Fatalf("reissue %q %v", refusal, err)
	}
	headers := g.hdrs[0]
	if headers.Get("x-nous-hermes-turn") != "turn-7" || headers.Get("x-nous-hermes-tool-call") != "call-7" {
		t.Fatalf("headers %v", headers)
	}
	if len(logs.recs) == 0 || logs.recs[len(logs.recs)-1] != "dispatch=call-7" {
		t.Fatalf("outcomes %q", logs.recs)
	}
}
