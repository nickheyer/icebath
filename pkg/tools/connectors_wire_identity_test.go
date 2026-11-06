package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
)

type wireReply struct {
	status    int
	body      string
	requestID string
}

type wireSent struct {
	url     string
	headers http.Header
	body    map[string]any
}

type wireTransport struct {
	mu      sync.Mutex
	replies []wireReply
	sent    []wireSent
}

// Answers each request with the next scripted reply and records what was sent
func (w *wireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(data, &body)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, wireSent{req.URL.String(), req.Header.Clone(), body})
	r := w.replies[0]
	w.replies = w.replies[1:]
	return &http.Response{StatusCode: r.status, Header: http.Header{"X-Request-Id": {r.requestID}},
		Body: io.NopCloser(bytes.NewReader([]byte(r.body))), Request: req}, nil
}

// Gateway client over a scripted transport at https://gw.test with a fixed bearer token
func wireClient(env *Env, call *CallRef, replies ...wireReply) (*ConnectorClient, *wireTransport) {
	transport := &wireTransport{replies: replies}
	return NewConnectorClientWith(env, call, "", &http.Client{Transport: transport},
		func() (string, error) { return "https://gw.test", nil },
		func(string) map[string]string { return map[string]string{"Authorization": "Bearer t"} }), transport
}

func wireEnv(t *testing.T) (*Env, *outcomeLog) {
	for _, k := range []string{"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_SOURCE", "ICEBATH_CRON_SESSION", "ICEBATH_SESSION_ID"} {
		t.Setenv(k, "")
	}
	logs := &outcomeLog{}
	return &Env{Home: t.TempDir(), Log: slog.New(logs), Values: &sync.Map{}}, logs
}

func TestWireUserAgentNamesTheSessionSurface(t *testing.T) {
	for _, c := range []struct{ bound, surface string }{{"", "cli"}, {"tui", "tui"}, {"desktop", "desktop"}, {"telegram", "gateway"}, {"api_server", "api"}} {
		env, _ := wireEnv(t)
		if c.bound != "" {
			env.Values.Store("session.platform", c.bound)
		}
		agent, err := ConnectorUserAgent(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		product, rest, _ := strings.Cut(agent, " ")
		if !strings.HasPrefix(product, "icebath-agent/") || len(product) <= len("icebath-agent/") {
			t.Fatalf("product %q", product)
		}
		if !strings.HasPrefix(rest, "("+c.surface+"; ") || !strings.HasSuffix(rest, ")") {
			t.Fatalf("%s: rest %q", c.bound, rest)
		}
		if osName := rest[len(c.surface)+3 : len(rest)-1]; !map[string]bool{"linux": true, "macos": true, "windows": true, "unknown": true}[osName] {
			t.Fatalf("os %q", osName)
		}
	}
}

func TestWireRunningTurnPlatformNamesTheSurfaceBeforeTheSessionVars(t *testing.T) {
	env, _ := wireEnv(t)
	env.Values.Store("session.platform", "tui")
	ctx := pyctx.With(context.Background(), pyctx.CopyContext(context.Background()))
	relayCurrentTurnVar.Set(ctx, &RelayTurnContext{Lease: &ConversationLease{Platform: "telegram"}})
	agent, err := ConnectorUserAgent(ctx, env)
	if err != nil || !strings.Contains(agent, " (gateway; ") {
		t.Fatalf("agent %q %v", agent, err)
	}
}

func TestWireGatewayRequestIDIsLoggedOnFailureAndSuccess(t *testing.T) {
	env, logs := wireEnv(t)
	client, _ := wireClient(env, &CallRef{ToolCallID: "call-9"},
		wireReply{200, `{"schemas": {}}`, "ok-id"},
		wireReply{403, `{"error": {"code": "TOOL_NOT_ALLOWED", "message": "no"}}`, "bad-id"})
	if _, err := client.Schemas(context.Background(), []string{"X"}); err != nil {
		t.Fatal(err)
	}
	_, err := client.Schemas(context.Background(), []string{"X"})
	var gerr *ToolGatewayError
	if !errors.As(err, &gerr) || gerr.RequestID != "bad-id" {
		t.Fatalf("error %#v", err)
	}
	want := []string{"DEBUG connector connectors.schemas 200 - request_id=ok-id dispatch=call-9",
		"WARN connector connectors.schemas 403 TOOL_NOT_ALLOWED request_id=bad-id dispatch=call-9"}
	if !reflect.DeepEqual(logs.recs, want) {
		t.Fatalf("outcomes %q", logs.recs)
	}
}

func TestWireANotYetCreatedAccountStaysDebug(t *testing.T) {
	env, logs := wireEnv(t)
	client, _ := wireClient(env, nil, wireReply{404, `{"error": "connection_not_found"}`, "gw-req-1"})
	got, err := client.AccountStatus(context.Background(), "acct-1", 0)
	if got != nil || err != nil {
		t.Fatalf("account %v %v", got, err)
	}
	if len(logs.recs) != 1 || !strings.HasPrefix(logs.recs[0], "DEBUG ") || !strings.Contains(logs.recs[0], "connectors.accounts 404") {
		t.Fatalf("outcomes %q", logs.recs)
	}
}

type opMessages struct {
	mu   sync.Mutex
	opID string
	msgs []string
}

func (h *opMessages) Enabled(context.Context, slog.Level) bool { return true }
func (h *opMessages) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *opMessages) WithGroup(string) slog.Handler            { return h }

// Keeps the info lines that name the watched operation
func (h *opMessages) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Level == slog.LevelInfo && h.opID != "" && strings.HasPrefix(r.Message, "connection op "+h.opID) {
		h.msgs = append(h.msgs, r.Message)
	}
	return nil
}

func TestWireOperationLogsOpenTransitionsAndSettle(t *testing.T) {
	op := NewConnectionOperation([]*ConnTarget{NewConnTarget("gmail", "connector", "connect")}, "s-log", "", nil)
	logs := &opMessages{opID: op.OpID}
	saved := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(saved) })
	if err := LiveOpen(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	for _, to := range []TargetState{TargetInitiated, TargetConnected} {
		if _, err := op.Transition("gmail", to, ActorBackendWatcher, TransitionFields{}); err != nil {
			t.Fatal(err)
		}
	}
	op.Settle(SettleAllResolved)
	LiveClose(op)
	id := op.OpID
	want := []string{"connection op " + id + " opened: connector:gmail:connect",
		"connection op " + id + ": gmail pending -> initiated by backend_watcher",
		"connection op " + id + ": gmail initiated -> connected by backend_watcher",
		"connection op " + id + " settled by all_resolved: gmail=connected"}
	if !reflect.DeepEqual(logs.msgs, want) {
		t.Fatalf("lines %q", logs.msgs)
	}
	if _, err := json.Marshal(op.Snapshot(false)); err != nil {
		t.Fatal(err)
	}
}

func TestWireA200WhoseBodyFailedIsAWarningWithTheRequestID(t *testing.T) {
	env, logs := wireEnv(t)
	client, _ := wireClient(env, &CallRef{ToolCallID: "call-3"},
		wireReply{200, `{"results": [{"error": {"code": "CONNECTION_REQUIRED", "message": "connect gmail"}}]}`, "exec-id"},
		wireReply{200, "not json", "junk-id"},
		wireReply{200, `{"unexpected": true}`, "one-id"})
	plan := PlannedCall{Position: 0, Name: "gmail_send_email", Connector: "gmail", Tool: "GMAIL_SEND_EMAIL", Arguments: map[string]any{}}
	legacyExecuteOrigins.Lock()
	legacyExecuteOrigins.set["https://gw.test"] = true
	legacyExecuteOrigins.Unlock()
	rows, err := client.Execute(context.Background(), []PlannedCall{plan}, "", "")
	legacyExecuteOrigins.Lock()
	delete(legacyExecuteOrigins.set, "https://gw.test")
	legacyExecuteOrigins.Unlock()
	if err != nil || asStr(asMap(rows[0]["error"])["code"]) != "CONNECTION_REQUIRED" {
		t.Fatalf("execute %v %v", rows, err)
	}
	_, err = client.Schemas(context.Background(), []string{"X"})
	var gerr *ToolGatewayError
	if !errors.As(err, &gerr) || gerr.RequestID != "junk-id" || gerr.Code != "INVALID_RESPONSE" {
		t.Fatalf("schemas error %#v", err)
	}
	if _, err := client.Execute(context.Background(), []PlannedCall{plan}, "", ""); !errors.As(err, &gerr) {
		t.Fatalf("execute error %#v", err)
	}
	want := []string{"WARN connector connectors.execute 200 CONNECTION_REQUIRED request_id=exec-id dispatch=call-3",
		"WARN connector connectors.schemas 200 INVALID_RESPONSE request_id=junk-id dispatch=call-3",
		"WARN connector connectors.execute 200 INVALID_RESPONSE request_id=one-id dispatch=call-3"}
	if !reflect.DeepEqual(logs.recs, want) {
		t.Fatalf("outcomes %q", logs.recs)
	}
}
