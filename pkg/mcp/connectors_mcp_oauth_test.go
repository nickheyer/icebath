package mcp

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Waits up to five seconds for the flow's worker to return
func waitWorkerDone(flow *DashboardOAuthFlow) {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !flow.WorkerDone(); {
		time.Sleep(10 * time.Millisecond)
	}
}

func resetOAuthSessions(t *testing.T) {
	t.Helper()
	clearSessions := func() {
		oauthSessionsMu.Lock()
		clear(oauthSessions)
		oauthSessionsMu.Unlock()
	}
	clearSessions()
	t.Cleanup(clearSessions)
}

// A worker that publishes an authorize URL carrying state and waits for the callback like the SDK does
func publishingWorker(state string, finished chan struct{}) func(context.Context, *tools.Env, string, string, map[string]any, bool, OAuthWorkerOptions) {
	return func(ctx context.Context, env *tools.Env, homeDir, serverName string, cfg map[string]any, reconnectLive bool, o OAuthWorkerOptions) {
		flow := o.Flow
		defer func() {
			flow.MarkWorkerDone()
			if o.OnDone != nil {
				o.OnDone()
			}
			if finished != nil {
				finished <- struct{}{}
			}
		}()
		if err := flow.PublishAuthorizationURL(ctx, "https://as.example.com/authorize?client_id=x&state="+state); err != nil {
			flow.MarkError(err.Error(), false)
			return
		}
		if _, _, _, err := flow.WaitForCallback(ctx, 10*time.Second); err != nil {
			flow.MarkError(err.Error(), false)
			return
		}
		_ = flow.MarkApproved()
	}
}

func swapSessionWorker(t *testing.T, w func(context.Context, *tools.Env, string, string, map[string]any, bool, OAuthWorkerOptions)) {
	saved := oauthSessionWorker
	oauthSessionWorker = w
	t.Cleanup(func() { oauthSessionWorker = saved })
}

func waitFinished(t *testing.T, finished chan struct{}) {
	t.Helper()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel must wake the worker, not leave an occupied slot")
	}
}

func TestOAuthCancelIsScopedIdempotentAndReleasesWorker(t *testing.T) {
	for _, clientRedirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "listener", true: "client_redirect"}[clientRedirect], func(t *testing.T) {
			resetOAuthSessions(t)
			root := t.TempDir()
			finished := make(chan struct{}, 4)
			swapSessionWorker(t, publishingWorker("test", finished))
			h := filepath.Join(root, "origin")
			t.Setenv("ICEBATH_HOME", h)
			ctx := context.Background()
			var redirect *string
			if clientRedirect {
				redirect = strp("http://127.0.0.1:49152/callback")
			}
			result, err := StartOAuthFlow(ctx, &tools.Env{}, h, "reports", map[string]any{"url": "https://mcp.example"}, false, 30*time.Second, redirect)
			if err != nil {
				t.Fatal(err)
			}
			sid, _ := result.Get("session_id")
			rec := oauthSessions[sid.(string)]
			if ok, _ := CancelOAuthFlow(ctx, sid.(string), "reports", filepath.Join(root, "other")).Get("ok"); ok != false {
				t.Fatal("another profile canceled the flow")
			}
			if ok, _ := CancelOAuthFlow(ctx, sid.(string), "other", h).Get("ok"); ok != false {
				t.Fatal("another server canceled the flow")
			}
			if rec.flow.Snapshot()["status"] != "authorization_required" {
				t.Fatalf("status %v", rec.flow.Snapshot()["status"])
			}
			if ok, _ := CancelOAuthFlow(ctx, sid.(string), "reports", h).Get("ok"); ok != true {
				t.Fatal("owner cancel refused")
			}
			waitFinished(t, finished)
			if st, _ := PollOAuthFlow(ctx, sid.(string), "reports").Get("status"); st != "error" {
				t.Fatalf("poll %v", st)
			}
			rec.mu.Lock()
			if rec.httpd != nil {
				t.Fatal("listener still bound")
			}
			rec.mu.Unlock()
			if ok, _ := CancelOAuthFlow(ctx, sid.(string), "reports", h).Get("ok"); ok != true {
				t.Fatal("repeat cancel refused")
			}
			if ok, _ := DeliverOAuthCallbackFlow(ctx, sid.(string), "reports", "late", "test", "", "").Get("ok"); ok != false {
				t.Fatal("late callback accepted")
			}
			retry, err := StartOAuthFlow(ctx, &tools.Env{}, h, "reports", map[string]any{"url": "https://mcp.example"}, false, 30*time.Second,
				strp("http://127.0.0.1:49152/callback"))
			if err != nil {
				t.Fatal(err)
			}
			rid, _ := retry.Get("session_id")
			if ok, _ := CancelOAuthFlow(ctx, rid.(string), "reports", h).Get("ok"); ok != true {
				t.Fatal("retry cancel refused")
			}
			waitFinished(t, finished)
		})
	}
}

func TestOAuthSessionOperationsRequireResolvedOwner(t *testing.T) {
	for _, operation := range []string{"poll", "callback", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			resetOAuthSessions(t)
			root := t.TempDir()
			owner := filepath.Join(root, "owner")
			flow := NewDashboardOAuthFlow("owned", "reports", "", owner, "http://127.0.0.1:49152/callback", false)
			if err := flow.PublishAuthorizationURL(context.Background(), "https://idp.example/authorize?state=test"); err != nil {
				t.Fatal(err)
			}
			oauthSessions["owned"] = &oauthSession{flow: flow, serverName: "reports", homeDir: owner, createdAt: time.Now()}
			invoke := func(ctx context.Context) map[string]any {
				switch operation {
				case "poll":
					return plainJSON(PollOAuthFlow(ctx, "owned", "reports"))
				case "callback":
					return plainJSON(DeliverOAuthCallbackFlow(ctx, "owned", "reports", "valid", "test", "", ""))
				}
				return plainJSON(CancelOAuthFlow(ctx, "owned", "reports", home.Dir(ctx)))
			}
			rejected := invoke(home.WithOverride(context.Background(), filepath.Join(root, "other")))
			if msg, _ := rejected["error_message"].(string); !strings.Contains(msg, "profile mismatch") {
				t.Fatalf("rejected %v", rejected)
			}
			if _, ok := rejected["auth_url"]; ok {
				t.Fatal("auth_url leaked")
			}
			if flow.Snapshot()["status"] != "authorization_required" {
				t.Fatalf("status %v", flow.Snapshot()["status"])
			}
			accepted := invoke(home.WithOverride(context.Background(), owner))
			ok, has := accepted["ok"]
			if !has {
				ok = accepted["status"] == "pending"
			}
			if ok != true {
				t.Fatalf("accepted %v", accepted)
			}
		})
	}
}

func TestOAuthCancelDoesNotRevokeAnApprovedFlow(t *testing.T) {
	resetOAuthSessions(t)
	h := t.TempDir()
	flow := NewDashboardOAuthFlow("approved", "reports", "", h, "http://127.0.0.1:49152/callback", false)
	flow.DiscoveryError = "tools/list unavailable"
	if err := flow.MarkApproved(); err != nil {
		t.Fatal(err)
	}
	poll := (&OAuthAttempt{authURL: "https://idp.example", flow: flow}).Poll()
	if poll.Status != "approved" || poll.Error != "" || len(poll.Tools) != 0 || poll.DiscoveryError != "tools/list unavailable" {
		t.Fatalf("poll %+v", poll)
	}
	flow.MarkWorkerDone()
	oauthSessions["approved"] = &oauthSession{flow: flow, serverName: "reports", homeDir: h, createdAt: time.Now()}
	if got := connJSON(CancelOAuthFlow(context.Background(), "approved", "reports", h)); got != `{"ok":true,"status":"approved"}` {
		t.Fatalf("cancel %s", got)
	}
	if ok, _ := CancelOAuthFlow(context.Background(), "missing", "reports", h).Get("ok"); ok != false {
		t.Fatal("missing session canceled")
	}
}

func TestOAuthValidateAcceptsLoopbackHTTP(t *testing.T) {
	for uri, want := range map[string]string{
		"http://127.0.0.1:8412/callback":     "http://127.0.0.1:8412/callback",
		"http://localhost:60000/callback":    "http://localhost:60000/callback",
		"http://127.0.0.1:9999":              "http://127.0.0.1:9999/callback",
		"  http://127.0.0.1:8412/callback  ": "http://127.0.0.1:8412/callback",
	} {
		if got, err := ValidateClientRedirectURI(uri); err != nil || got != want {
			t.Fatalf("%q -> %q %v", uri, got, err)
		}
	}
}

func TestOAuthValidateRejectsNonLoopback(t *testing.T) {
	for _, uri := range []string{"https://127.0.0.1:8412/callback", "http://evil.example.com:8412/callback",
		"http://192.168.1.10:8412/callback", "http://127.0.0.1/callback", "http://user:pass@127.0.0.1:8412/callback",
		"javascript:alert(1)", "", "not a url"} {
		if _, err := ValidateClientRedirectURI(uri); err == nil {
			t.Fatalf("%q accepted", uri)
		}
	}
}

func TestOAuthStartFlowClientRedirectSkipsGatewayListener(t *testing.T) {
	resetOAuthSessions(t)
	h := isolateHome(t)
	swapSessionWorker(t, publishingWorker("teststate123", nil))
	var bound []*DashboardOAuthFlow
	savedRecv, savedCard := loopbackReceiver, cardOAuthWorker
	loopbackReceiver = func(flow *DashboardOAuthFlow) (*http.Server, int, error) {
		bound = append(bound, flow)
		return startLoopbackReceiver(flow)
	}
	t.Cleanup(func() { loopbackReceiver, cardOAuthWorker = savedRecv, savedCard })
	ctx := context.Background()
	result, err := StartOAuthFlow(ctx, &tools.Env{}, h, "clicky", map[string]any{"url": "https://mcp.example.com/mcp", "auth": "oauth"},
		false, 30*time.Second, strp("http://127.0.0.1:8412/callback"))
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := result.Get("session_id")
	authURL, _ := result.Get("auth_url")
	if sid == "" || !strings.HasPrefix(authURL.(string), "https://as.example.com/authorize") || len(bound) != 0 {
		t.Fatalf("result %v bound %d", result, len(bound))
	}
	rec := oauthSessions[sid.(string)]
	if rec.httpd != nil || rec.flow.RedirectURI != "http://127.0.0.1:8412/callback" {
		t.Fatalf("rec %+v", rec)
	}
	DeliverOAuthCallbackFlow(ctx, sid.(string), "clicky", "authcode", "teststate123", "", "")
	waitWorkerDone(rec.flow)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("mcp_servers:\n  cardy:\n    url: https://mcp.example.com/mcp\n    auth: oauth\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cardOAuthWorker = publishingWorker("teststate123", nil)
	t.Setenv("SSH_CLIENT", "192.0.2.1 12345 22")
	attempt, err := StartCardOAuth(ctx, &tools.Env{}, "cardy", CardOAuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(attempt.flow.RedirectURI, "http://127.0.0.1:") {
		t.Fatalf("redirect %s", attempt.flow.RedirectURI)
	}
	oauthSessionsMu.Lock()
	_, registered := oauthSessions[attempt.flow.FlowID]
	oauthSessionsMu.Unlock()
	if !registered {
		t.Fatal("card flow not registered")
	}
	DeliverOAuthCallbackFlow(ctx, attempt.flow.FlowID, "cardy", "authcode", "teststate123", "", "")
	waitWorkerDone(attempt.flow)
	pinned := NewDashboardOAuthFlow("pinned", "asana", "", home.Dir(ctx), "", false)
	var pinnedBound []*DashboardOAuthFlow
	loopbackReceiver = func(flow *DashboardOAuthFlow) (*http.Server, int, error) {
		pinnedBound = append(pinnedBound, flow)
		return nil, 0, nil
	}
	srv, err := ChooseCallbackReceiver(pinned, map[string]any{"oauth": map[string]any{"client_id": "asana-client",
		"redirect_host": "localhost", "redirect_port": 27890}}, nil)
	if err != nil || srv != nil || pinned.RedirectURI != "http://localhost:27890/callback" || len(pinnedBound) != 0 {
		t.Fatalf("pinned %v %v %s %d", srv, err, pinned.RedirectURI, len(pinnedBound))
	}
}

func TestOAuthStartFlowRejectsBadClientRedirect(t *testing.T) {
	resetOAuthSessions(t)
	h := isolateHome(t)
	swapSessionWorker(t, publishingWorker("teststate123", nil))
	if _, err := StartOAuthFlow(context.Background(), &tools.Env{}, h, "clicky2", map[string]any{"url": "https://mcp.example.com/mcp"},
		false, 30*time.Second, strp("https://evil.example.com/callback")); err == nil {
		t.Fatal("bad redirect accepted")
	}
	for _, rec := range oauthSessions {
		if rec.serverName == "clicky2" {
			t.Fatal("a session was registered")
		}
	}
}

// A relay session whose flow published an authorize URL carrying state
func makeRelaySession(t *testing.T, sessionID, server, state string) *DashboardOAuthFlow {
	flow := NewDashboardOAuthFlow(sessionID, server, "", home.Dir(context.Background()), "http://127.0.0.1:9000/callback", false)
	if err := flow.PublishAuthorizationURL(context.Background(), "https://as.example.com/authorize?state="+state); err != nil {
		t.Fatal(err)
	}
	oauthSessionsMu.Lock()
	oauthSessions[sessionID] = &oauthSession{sessionID: sessionID, serverName: server, homeDir: home.Dir(context.Background()), flow: flow, createdAt: time.Now()}
	oauthSessionsMu.Unlock()
	return flow
}

func flowCallback(flow *DashboardOAuthFlow) [3]string {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.callback == nil {
		return [3]string{}
	}
	return *flow.callback
}

func TestOAuthDeliverCallbackAcceptsMatchingState(t *testing.T) {
	resetOAuthSessions(t)
	isolateHome(t)
	flow := makeRelaySession(t, "sess-relay-1", "hosp", "s3cr3tstate")
	out := DeliverOAuthCallbackFlow(context.Background(), "sess-relay-1", "hosp", "abc", "s3cr3tstate", "", "")
	if connJSON(out) != `{"ok":true,"session_id":"sess-relay-1"}` || flowCallback(flow) != [3]string{"abc", "s3cr3tstate", ""} {
		t.Fatalf("out %s callback %v", connJSON(out), flowCallback(flow))
	}
}

func TestOAuthLoopbackListenerForwardsIss(t *testing.T) {
	resetOAuthSessions(t)
	isolateHome(t)
	flow := makeRelaySession(t, "sess-relay-loop", "loopy", "loopstate")
	srv, port, err := startLoopbackReceiver(flow)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + itoa(port) + "/callback?code=abc&state=loopstate&iss=https%3A%2F%2Fas.example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if flowCallback(flow) != [3]string{"abc", "loopstate", "https://as.example.com"} {
		t.Fatalf("callback %v", flowCallback(flow))
	}
}

func TestOAuthDeliverCallbackRejectsStateMismatch(t *testing.T) {
	resetOAuthSessions(t)
	isolateHome(t)
	makeRelaySession(t, "sess-relay-1", "hosp", "s3cr3tstate")
	out := plainJSON(DeliverOAuthCallbackFlow(context.Background(), "sess-relay-1", "hosp", "abc", "WRONG", "", ""))
	if out["ok"] != false || !strings.Contains(strings.ToLower(out["error_message"].(string)), "state") {
		t.Fatalf("out %v", out)
	}
}

func TestOAuthDeliverCallbackRejectsReplay(t *testing.T) {
	resetOAuthSessions(t)
	isolateHome(t)
	makeRelaySession(t, "sess-relay-1", "hosp", "s3cr3tstate")
	ctx := context.Background()
	if ok, _ := DeliverOAuthCallbackFlow(ctx, "sess-relay-1", "hosp", "abc", "s3cr3tstate", "", "").Get("ok"); ok != true {
		t.Fatal("first delivery refused")
	}
	if ok, _ := DeliverOAuthCallbackFlow(ctx, "sess-relay-1", "hosp", "abc", "s3cr3tstate", "", "").Get("ok"); ok != false {
		t.Fatal("replay accepted")
	}
}

func TestOAuthDeliverCallbackUnknownSessionAndNameMismatch(t *testing.T) {
	resetOAuthSessions(t)
	isolateHome(t)
	makeRelaySession(t, "sess-relay-1", "hosp", "s3cr3tstate")
	ctx := context.Background()
	if ok, _ := DeliverOAuthCallbackFlow(ctx, "nope", "hosp", "a", "s", "", "").Get("ok"); ok != false {
		t.Fatal("unknown session accepted")
	}
	out := plainJSON(DeliverOAuthCallbackFlow(ctx, "sess-relay-1", "other-server", "a", "s", "", ""))
	if out["ok"] != false || !strings.Contains(out["error_message"].(string), "mismatch") {
		t.Fatalf("out %v", out)
	}
}

func TestOAuthDeliverCallbackPropagatesProviderError(t *testing.T) {
	resetOAuthSessions(t)
	isolateHome(t)
	flow := makeRelaySession(t, "sess-relay-1", "hosp", "s3cr3tstate")
	if ok, _ := DeliverOAuthCallbackFlow(context.Background(), "sess-relay-1", "hosp", "", "s3cr3tstate", "access_denied", "").Get("ok"); ok != true {
		t.Fatal("provider error refused")
	}
	if _, _, _, err := flow.WaitForCallback(context.Background(), time.Second); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("wait %v", err)
	}
}
