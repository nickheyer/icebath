package provider

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type billingOutcome struct {
	status  int
	body    string
	headers map[string]string
}

type billingSeen struct {
	method, url string
	data        any
	headers     http.Header
}

type billingTransport struct {
	outcomes []billingOutcome
	seen     []billingSeen
}

func (b *billingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var data any
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &data)
		}
	}
	b.seen = append(b.seen, billingSeen{method: req.Method, url: req.URL.String(), data: data, headers: req.Header.Clone()})
	out := b.outcomes[len(b.seen)-1]
	header := http.Header{}
	for k, v := range out.headers {
		header.Set(k, v)
	}
	return &http.Response{StatusCode: out.status, Status: http.StatusText(out.status), Header: header,
		Body: io.NopCloser(strings.NewReader(out.body)), Request: req}, nil
}

// Scripted billing responses with a resolver double, the env carrying the recording transport
func billingSequence(t *testing.T, resolver func(PoolEnv, bool) (string, string, error), outcomes ...billingOutcome) (PoolEnv, *billingTransport) {
	t.Helper()
	prev := nousBillingResolveFn
	t.Cleanup(func() { nousBillingResolveFn = prev; nousBillingInvalidateCachedToken() })
	nousBillingInvalidateCachedToken()
	if resolver == nil {
		resolver = func(PoolEnv, bool) (string, string, error) { return "tok", "https://portal.example", nil }
	}
	nousBillingResolveFn = resolver
	rt := &billingTransport{outcomes: outcomes}
	env := testEnv(t, newTestHome(t))
	env.HTTP = &http.Client{Transport: rt}
	return env, rt
}

func billingErr(t *testing.T, err error) *BillingError {
	t.Helper()
	var be *BillingError
	if !errors.As(err, &be) {
		t.Fatalf("error %v (%T) is not a BillingError", err, err)
	}
	return be
}

// ---- test_nous_billing_request.py ----

func TestNousBilling401RefreshesTokenAndRetriesSuccessfully(t *testing.T) {
	env, rt := billingSequence(t, func(_ PoolEnv, useCache bool) (string, string, error) {
		if !useCache {
			return "tok-fresh", "https://portal.example", nil
		}
		return "tok-stale", "https://portal.example", nil
	}, billingOutcome{status: 401, body: "{}"}, billingOutcome{status: 200, body: `{"ok": true}`})
	got, err := GetNousBillingState(env, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := got.(map[string]any); len(m) != 1 || m["ok"] != true {
		t.Fatalf("state = %v", got)
	}
	if len(rt.seen) != 2 || rt.seen[0].headers.Get("Authorization") != "Bearer tok-stale" || rt.seen[1].headers.Get("Authorization") != "Bearer tok-fresh" {
		t.Fatalf("seen = %+v", rt.seen)
	}
}

func TestNousBilling403RemoteSpendingRevokedMapsThroughRequest(t *testing.T) {
	env, _ := billingSequence(t, nil, billingOutcome{status: 403, body: `{"error": "remote_spending_revoked", "actor": "self", "recovery": "reconnect"}`})
	_, err := GetNousBillingState(env, 0)
	be := billingErr(t, err)
	if be.Kind != BillingErrorRemoteSpendingRevoked || be.Actor != "self" || be.Recovery != "reconnect" {
		t.Fatalf("error = %+v", be)
	}
}

func TestNousBilling429RetryAfterHeaderMapsToRateLimited(t *testing.T) {
	env, _ := billingSequence(t, nil, billingOutcome{status: 429, body: "{}", headers: map[string]string{"Retry-After": "15"}})
	_, err := GetNousBillingState(env, 0)
	be := billingErr(t, err)
	if be.Kind != BillingErrorRateLimited || be.RetryAfter == nil || *be.RetryAfter != 15 {
		t.Fatalf("error = %+v", be)
	}
}

func TestNousBillingNonJSONSecond401MapsToAuthErrorNotSessionRevoked(t *testing.T) {
	env, _ := billingSequence(t, nil, billingOutcome{status: 401, body: "{}"}, billingOutcome{status: 401, body: "<html>Unauthorized</html>"})
	_, err := GetNousBillingState(env, 0)
	be := billingErr(t, err)
	if !be.Kind.IsAuth() || be.Kind == BillingErrorSessionRevoked {
		t.Fatalf("error = %+v", be)
	}
}

func TestNousBilling404GetChargeStatusMapsToGenericBillingError(t *testing.T) {
	env, rt := billingSequence(t, nil, billingOutcome{status: 404, body: "{}"})
	_, err := GetNousChargeStatus(env, "ch_404", 0)
	be := billingErr(t, err)
	if be.Kind != BillingErrorGeneric || be.Status != 404 {
		t.Fatalf("error = %+v", be)
	}
	if rt.seen[0].url != "https://portal.example/api/billing/charge/ch_404" {
		t.Fatalf("url = %s", rt.seen[0].url)
	}
}

func TestNousBillingTokenCacheIsScopedPerProfileHome(t *testing.T) {
	nousBillingInvalidateCachedToken()
	prev := nousBillingTokenFn
	t.Cleanup(func() { nousBillingTokenFn = prev; nousBillingInvalidateCachedToken() })
	nousBillingTokenFn = func(env PoolEnv) (string, error) { return "tok-" + filepath.Base(env.Store.Home()), nil }
	root := t.TempDir()
	var seen []string
	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		token, _, err := nousBillingResolveTokenAndBase(testEnv(t, dir), true)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, token)
	}
	if strings.Join(seen, ",") != "tok-a,tok-b" {
		t.Fatalf("seen = %v", seen)
	}
}

// ---- test_billing_portal_url.py ----

func TestNousBillingAbsolutizeResolvesRelative(t *testing.T) {
	t.Setenv("ICEBATH_PORTAL_BASE_URL", "https://nas-pr-412.nousresearch.wtf")
	got, err := AbsolutizeNousBillingPortalURL(testEnv(t, newTestHome(t)), "/billing?topup=open")
	if err != nil || got != "https://nas-pr-412.nousresearch.wtf/billing?topup=open" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestNousBillingRaiseForErrorAttachesAbsolutePortalURL(t *testing.T) {
	t.Setenv("ICEBATH_PORTAL_BASE_URL", "https://nas-pr-412.nousresearch.wtf")
	err := nousBillingErrorFor(testEnv(t, newTestHome(t)), 403, map[string]any{"error": "no_payment_method", "portalUrl": "/billing?topup=open"}, nil)
	if be := billingErr(t, err); be.PortalURL != "https://nas-pr-412.nousresearch.wtf/billing?topup=open" {
		t.Fatalf("portal = %v", be.PortalURL)
	}
}

// ---- test_remote_spending_gate_contract.py ----

func billingRaise(t *testing.T, status int, payload map[string]any, headers http.Header) *BillingError {
	t.Helper()
	return billingErr(t, nousBillingErrorFor(testEnv(t, newTestHome(t)), status, payload, headers))
}

func TestNousBilling403RemoteSpendingRevokedMapsToTypedErrWithActor(t *testing.T) {
	be := billingRaise(t, 403, map[string]any{"error": "remote_spending_revoked", "recovery": "reconnect", "actor": "admin"}, nil)
	if be.Kind != BillingErrorRemoteSpendingRevoked || be.Actor != "admin" || be.Recovery != "reconnect" {
		t.Fatalf("error = %+v", be)
	}
}

func TestNousBilling401SessionRevokedIsDistinctFromPlain401(t *testing.T) {
	revoked := billingRaise(t, 401, map[string]any{"error": "session_revoked", "recovery": "login"}, nil)
	if revoked.Kind != BillingErrorSessionRevoked || revoked.Recovery != "login" {
		t.Fatalf("revoked = %+v", revoked)
	}
	if plain := billingRaise(t, 401, map[string]any{"error": "invalid_token"}, nil); plain.Kind == BillingErrorSessionRevoked {
		t.Fatalf("plain = %+v", plain)
	}
}

func TestNousBilling503IsRateLimitedNotRevokedAndCarriesRetryAfter(t *testing.T) {
	be := billingRaise(t, 503, map[string]any{"error": "temporarily_unavailable"}, http.Header{"Retry-After": {"30"}})
	if be.Kind != BillingErrorRateLimited || be.RetryAfter == nil || *be.RetryAfter != 30 {
		t.Fatalf("error = %+v", be)
	}
}

func TestNousBilling409IdempotencyConflictPassesThrough(t *testing.T) {
	be := billingRaise(t, 409, map[string]any{"error": "idempotency_conflict", "message": "same key, different amount"}, nil)
	if be.ErrorCode != "idempotency_conflict" {
		t.Fatalf("error = %+v", be)
	}
}
