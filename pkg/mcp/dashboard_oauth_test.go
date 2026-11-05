package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDashboardFlow(flowID, serverName string) *DashboardOAuthFlow {
	return NewDashboardOAuthFlow(flowID, serverName, "", "/tmp/icebath-test", "https://agent.example/mcp/oauth/callback/"+flowID, false)
}

func TestDashboardFlowExposesAuthorizationURLAndAcceptsCallback(t *testing.T) {
	ctx := context.Background()
	flow := NewDashboardOAuthFlow("flow-1", "reports", "", "/tmp/icebath-test", "https://agent.example/mcp/oauth/callback/flow-1", false)
	if err := flow.PublishAuthorizationURL(ctx, "https://idp.example/authorize?state=s1"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"flow_id": "flow-1", "server_name": "reports", "status": "authorization_required",
		"authorization_url": "https://idp.example/authorize?state=s1", "error": nil}
	if got := flow.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot %v", got)
	}
	if err := flow.DeliverCallback("code-1", "s1", "", ""); err != nil {
		t.Fatal(err)
	}
	code, state, iss, err := flow.WaitForCallback(ctx, 300*time.Second)
	if err != nil || code != "code-1" || state != "s1" || iss != "" {
		t.Fatalf("callback %q %q %q %v", code, state, iss, err)
	}
}

func TestDashboardFlowPreservesRFC9207Iss(t *testing.T) {
	ctx := context.Background()
	flow := NewDashboardOAuthFlow("flow-iss", "cloudflare", "", "/tmp/icebath-test", "https://agent.example/mcp/oauth/callback/flow-iss", false)
	if err := flow.PublishAuthorizationURL(ctx, "https://idp.example/authorize?state=s1"); err != nil {
		t.Fatal(err)
	}
	if err := flow.DeliverCallback("code-1", "s1", "", "https://mcp.cloudflare.com"); err != nil {
		t.Fatal(err)
	}
	code, state, iss, err := flow.WaitForCallback(ctx, 300*time.Second)
	if err != nil || code != "code-1" || state != "s1" || iss != "https://mcp.cloudflare.com" {
		t.Fatalf("callback %q %q %q %v", code, state, iss, err)
	}
}

func TestDashboardFlowAcceptsOnlyOneConcurrentCallback(t *testing.T) {
	flow := NewDashboardOAuthFlow("flow-race", "reports", "", "/tmp/icebath-test", "https://agent.example/mcp/oauth/callback/flow-race", false)
	if err := flow.PublishAuthorizationURL(context.Background(), "https://idp.example/authorize?state=state"); err != nil {
		t.Fatal(err)
	}
	var start, done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	var outcomes []string
	for _, code := range []string{"one", "two"} {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			err := flow.DeliverCallback(code, "state", "", "")
			var verr *DashboardOAuthValueError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				outcomes = append(outcomes, "accepted")
			case errors.As(err, &verr):
				outcomes = append(outcomes, "rejected")
			default:
				outcomes = append(outcomes, err.Error())
			}
		}()
	}
	start.Done()
	done.Wait()
	sort.Strings(outcomes)
	if !reflect.DeepEqual(outcomes, []string{"accepted", "rejected"}) {
		t.Fatalf("outcomes %v", outcomes)
	}
}

func TestDashboardMCPOAuthHelpersUseDashboardFlowWithoutLoopbackPort(t *testing.T) {
	oauthHome(t)
	flow := NewDashboardOAuthFlow("flow-4", "reports", "", "/tmp/icebath-test", "https://agent.example/mcp/oauth/callback/flow-4", false)
	ctx := withDashboardOAuthFlow(context.Background(), flow)
	cfg := map[string]any{}
	port, err := configureCallbackPort(ctx, cfg, NewTokenStorage(ctx, "reports", ""))
	if err != nil || port != 0 {
		t.Fatalf("port %d %v", port, err)
	}
	md, err := buildClientMetadata(cfg)
	if err != nil || md.RedirectURIs[0] != flow.RedirectURI {
		t.Fatalf("metadata %v %v", md, err)
	}
	if err := makeRedirectHandler(0, "", "")(ctx, "https://idp.example/authorize?state=state-4"); err != nil {
		t.Fatal(err)
	}
	if err := flow.DeliverCallback("code-4", "state-4", "", ""); err != nil {
		t.Fatal(err)
	}
	res, err := makeCallbackWaiter(0, "", 300*time.Second)(ctx)
	if err != nil || res.Code != "code-4" || res.State != "state-4" {
		t.Fatalf("result %v %v", res, err)
	}
	if flow.AuthorizationURL() != "https://idp.example/authorize?state=state-4" {
		t.Fatalf("authorization url %q", flow.AuthorizationURL())
	}
}

func TestDashboardFirstMarkErrorReasonReachesCallbackWaiterAndIsNeverClobbered(t *testing.T) {
	ctx := context.Background()
	flow := testDashboardFlow("flow-err", "asana")
	flow.MarkError("OAuth cancelled by user", false)
	if _, _, _, err := flow.WaitForCallback(ctx, 300*time.Second); err == nil || !strings.Contains(err.Error(), "OAuth cancelled by user") {
		t.Fatalf("waiter error %v", err)
	}
	flow.MarkError("OAuth authorization failed: OAuth cancelled by user", false)
	if got := flow.Snapshot()["error"]; got != "OAuth cancelled by user" {
		t.Fatalf("error clobbered %v", got)
	}
	delivered := testDashboardFlow("flow-late", "asana")
	if err := delivered.PublishAuthorizationURL(ctx, "https://idp.example/authorize?state=s9"); err != nil {
		t.Fatal(err)
	}
	if err := delivered.DeliverCallback("code-9", "s9", "", ""); err != nil {
		t.Fatal(err)
	}
	delivered.MarkError("worker crashed after the browser redirected", false)
	code, state, _, err := delivered.WaitForCallback(ctx, 300*time.Second)
	if err != nil || code != "code-9" || state != "s9" {
		t.Fatalf("delivered callback %q %q %v", code, state, err)
	}
}

type emptyRuntimeError struct{}

func (emptyRuntimeError) Error() string { return "" }

func TestDashboardEmptyExceptionTextStaysDiagnosable(t *testing.T) {
	if got := ExceptionMessage(&emptyRuntimeError{}); got != "emptyRuntimeError" {
		t.Fatalf("empty message %q", got)
	}
	if got := ExceptionMessage(errors.New("boom")); got != "boom" {
		t.Fatalf("message %q", got)
	}
	flow := testDashboardFlow("flow-empty", "asana")
	flow.MarkError("", false)
	if flow.Snapshot()["error"] == nil {
		t.Fatal("blank cause exposed")
	}
	if _, _, _, err := flow.WaitForCallback(context.Background(), 300*time.Second); err == nil || !strings.Contains(err.Error(), "empty error message") {
		t.Fatalf("waiter error %v", err)
	}
}

func TestDashboardFailedReauthRollbackPreservesNewerOAuthState(t *testing.T) {
	oauthHome(t)
	storage := NewTokenStorage(context.Background(), "reports", "")
	writeRaw := func(text string) {
		if err := os.WriteFile(storage.TokensPath(), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(oauthTokenDir(storage.homeDir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRaw("OLD")
	backup := storage.Snapshot()
	if err := storage.Remove(false); err != nil {
		t.Fatal(err)
	}
	writeRaw("FRESH")
	if err := storage.Restore(backup, true); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(storage.TokensPath()); string(raw) != "FRESH" {
		t.Fatalf("tokens %q", raw)
	}
}

func TestDashboardPreregisteredPinnedRedirectPortKeepsLoopbackListenerUnderDashboardFlow(t *testing.T) {
	dir := oauthHome(t)
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	flow := NewDashboardOAuthFlow("flow-5", "asana", "", dir, "https://agent.example/mcp/oauth/callback/flow-5", false)
	ctx := ForceInteractiveOAuth(withDashboardOAuthFlow(context.Background(), flow))
	cfg := map[string]any{"client_id": "pre-registered", "client_secret": "s", "redirect_host": "localhost", "redirect_port": port}
	got, err := configureCallbackPort(ctx, cfg, NewTokenStorage(ctx, "asana", ""))
	if err != nil || got != port {
		t.Fatalf("port %d %v", got, err)
	}
	md, err := buildClientMetadata(cfg)
	if err != nil || md.RedirectURIs[0] != fmt.Sprintf("http://localhost:%d/callback", port) {
		t.Fatalf("metadata %v %v", md, err)
	}
	if err := makeRedirectHandler(port, "", "")(ctx, "https://idp.example/authorize?state=state-5"); err != nil {
		t.Fatal(err)
	}
	if flow.AuthorizationURL() != "https://idp.example/authorize?state=state-5" {
		t.Fatalf("dashboard url %q", flow.AuthorizationURL())
	}
	ch := runWaiter(ctx, makeCallbackWaiter(port, "", 10*time.Second))
	if err := hitCallbackWhenReady(fmt.Sprintf("http://127.0.0.1:%d/callback?code=code-5&state=state-5", port), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	out := awaitWaiter(t, ch, 10*time.Second)
	if out.err != nil || out.res.Code != "code-5" || out.res.State != "state-5" {
		t.Fatalf("result %v %v", out.res, out.err)
	}
}

func TestDashboardRedirectHandlerRemintsAnEndedDashboardFlow(t *testing.T) {
	for _, endedBy := range []string{"approved", "error"} {
		t.Run(endedBy, func(t *testing.T) {
			oauthHome(t)
			flow := testDashboardFlow("flow-stale", "asana")
			if err := flow.PublishAuthorizationURL(context.Background(), "https://idp.example/authorize?state=spent"); err != nil {
				t.Fatal(err)
			}
			if endedBy == "approved" {
				if err := flow.MarkApproved(); err != nil {
					t.Fatal(err)
				}
			} else {
				flow.MarkError("worker gave up on the earlier attempt", false)
			}
			flow.MarkWorkerDone()
			ctx := withDashboardOAuthFlow(context.Background(), flow)
			if err := makeRedirectHandler(0, "", "")(ctx, "https://idp.example/authorize?state=fresh"); err != nil {
				t.Fatalf("an ended dashboard flow handle reached the connect ladder: %v", err)
			}
			if flow.Snapshot()["status"] != "authorization_required" || flow.ExpectedState() != "fresh" || !flow.WorkerDone() {
				t.Fatalf("not re-minted %v %q", flow.Snapshot(), flow.ExpectedState())
			}
			if err := flow.DeliverCallback("late-code", "fresh", "", ""); err != nil {
				t.Fatal(err)
			}
			if code, _, _, err := flow.WaitForCallback(context.Background(), 300*time.Second); err != nil || code != "late-code" {
				t.Fatalf("late callback %q %v", code, err)
			}
		})
	}
}

func TestDashboardFlowStatusDoesNotExposeAuthorizationCode(t *testing.T) {
	flow := NewDashboardOAuthFlow("flow-status", "reports", "", "/tmp/icebath-test", "https://agent.example/api/mcp/oauth/callback/flow-status", false)
	flow.authorizationURL = "https://idp.example/authorize"
	flow.status = "approved"
	flow.callback = &[3]string{"secret-code", "secret-state", ""}
	snap := flow.Snapshot()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if snap["status"] != "approved" || strings.Contains(string(raw), "secret-code") || strings.Contains(string(raw), "secret-state") {
		t.Fatalf("snapshot %s", raw)
	}
}

func TestDashboardCancelMarksTheFlowTerminalForARetryingWorker(t *testing.T) {
	ctx := context.Background()
	flow := NewDashboardOAuthFlow("flow-cancel-retry", "reports", "", "/tmp/icebath-test", "https://agent.example/api/mcp/oauth/callback/reports", false)
	if err := flow.PublishAuthorizationURL(ctx, "https://idp.example/authorize?state=first"); err != nil {
		t.Fatal(err)
	}
	flow.MarkError("OAuth cancelled by user", true)
	if err := flow.PublishAuthorizationURL(ctx, "https://idp.example/authorize?state=second"); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled flow re-minted %v", err)
	}
	if flow.Snapshot()["status"] != "error" || !flow.Cancelled() {
		t.Fatalf("snapshot %v", flow.Snapshot())
	}
}

func TestDashboardFlowRejectsURLWithoutStateAndTimesOut(t *testing.T) {
	ctx := context.Background()
	flow := testDashboardFlow("flow-nostate", "reports")
	var verr *DashboardOAuthValueError
	if err := flow.PublishAuthorizationURL(ctx, "https://idp.example/authorize?state="); !errors.As(err, &verr) {
		t.Fatalf("blank state accepted %v", err)
	}
	var terr *DashboardOAuthTimeoutError
	if _, err := flow.WaitForAuthorizationURL(ctx, 20*time.Millisecond); !errors.As(err, &terr) {
		t.Fatalf("authorization url wait %v", err)
	}
	if _, _, _, err := flow.WaitForCallback(ctx, 20*time.Millisecond); !errors.As(err, &terr) {
		t.Fatalf("callback wait %v", err)
	}
	if err := flow.DeliverCallback("c", "s", "", ""); !errors.As(err, &verr) || err.Error() != "OAuth callback state mismatch" {
		t.Fatalf("callback before publish %v", err)
	}
	flow.MarkError("worker crashed", false)
	if _, err := flow.WaitForAuthorizationURL(ctx, time.Second); err == nil || err.Error() != "worker crashed" {
		t.Fatalf("ended before authorization %v", err)
	}
	if err := flow.MarkApproved(); err == nil || err.Error() != "OAuth flow already ended" {
		t.Fatalf("approve after error %v", err)
	}
}
