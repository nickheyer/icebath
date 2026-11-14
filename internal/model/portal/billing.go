package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	billingTimeout       = 15 * time.Second
	billingTokenCacheTTL = 30 * time.Second
)

type BillingKind int

const (
	BillingDenied BillingKind = iota
	BillingScopeRequired
	BillingAuthRequired
	BillingRemoteSpendingRevoked
	BillingSessionRevoked
	BillingRateLimited
	BillingStripeUnavailable
	BillingUpgradeCapExceeded
)

type BillingError struct {
	Message    string
	Kind       BillingKind
	Status     int
	Code       string
	PortalURL  string
	RetryAfter *int
	Payload    json.RawMessage
	Actor      string
	Machine    string
	Recovery   string
	Cause      error
}

func (e *BillingError) Error() string { return e.Message }

func (e *BillingError) Unwrap() error { return e.Cause }

func (e *BillingError) NotLoggedIn() bool {
	return e.Kind == BillingAuthRequired || e.Kind == BillingSessionRevoked
}

func (e *BillingError) Transient() bool {
	return e.Kind == BillingRateLimited || e.Kind == BillingStripeUnavailable || e.Kind == BillingUpgradeCapExceeded
}

type billingMemo struct {
	at    time.Time
	token string
	base  string
	set   bool
}

func (a *Account) InvalidateBillingToken() {
	a.mu.Lock()
	a.billingMemo = billingMemo{}
	a.mu.Unlock()
}

func (a *Account) resolvePortalBaseURL(ctx context.Context, state *object) string {
	for _, candidate := range []string{a.portalOverride(ctx), state.str("portal_base_url")} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return strings.TrimRight(trimmed, "/")
		}
	}
	return auth.DefaultPortalURL
}

func (a *Account) absolutizePortalURL(ctx context.Context, raw any) string {
	portalURL, ok := raw.(string)
	if !ok {
		return ""
	}
	if strings.TrimSpace(portalURL) == "" {
		return portalURL
	}
	base, err := url.Parse(strings.TrimRight(a.resolvePortalBaseURL(ctx, nil), "/") + "/")
	if err != nil {
		return portalURL
	}
	ref, err := url.Parse(portalURL)
	if err != nil {
		return portalURL
	}
	return base.ResolveReference(ref).String()
}

func billingNotLoggedIn(cause error) *BillingError {
	return &BillingError{Message: "Not logged into Icebath Portal — run `icebath portal` to log in.", Kind: BillingAuthRequired,
		Status: http.StatusUnauthorized, Code: "invalid_token", Cause: cause}
}

func (a *Account) resolveTokenAndBase(ctx context.Context, useCache bool) (string, string, error) {
	if useCache {
		a.mu.Lock()
		memo := a.billingMemo
		a.mu.Unlock()
		if memo.set && time.Since(memo.at) < billingTokenCacheTTL {
			return memo.token, memo.base, nil
		}
	}
	base := a.resolvePortalBaseURL(ctx, a.quietState(ctx))
	secret, err := a.AccessToken(ctx, AccessTokenOptions{})
	if err != nil {
		if _, ok := authErrorOf(err); ok {
			return "", "", billingNotLoggedIn(err)
		}
		return "", "", err
	}
	token := strings.TrimSpace(secret.Reveal())
	a.mu.Lock()
	a.billingMemo = billingMemo{at: time.Now(), token: token, base: base, set: true}
	a.mu.Unlock()
	return token, base, nil
}

type billingRoute struct {
	kind     BillingKind
	fallback string
}

func billingRouteFor(status int, code string, known bool) (billingRoute, bool) {
	if known {
		switch code {
		case "stripe_unavailable":
			return billingRoute{BillingStripeUnavailable, "Stripe is temporarily unavailable — try again shortly."}, true
		case "upgrade_cap_exceeded":
			return billingRoute{BillingUpgradeCapExceeded, "Daily plan-change limit reached — try again tomorrow."}, true
		}
		switch {
		case status == 401 && code == "session_revoked":
			return billingRoute{BillingSessionRevoked, "Your session was logged out — log in again."}, true
		case status == 403 && code == "remote_spending_revoked":
			return billingRoute{BillingRemoteSpendingRevoked, "Remote spending was stopped for this terminal."}, true
		case status == 403 && code == "insufficient_scope":
			return billingRoute{BillingScopeRequired, "This action needs the billing:manage scope."}, true
		}
	}
	switch status {
	case 401:
		return billingRoute{BillingAuthRequired, "Authentication required."}, true
	case 403:
		return billingRoute{BillingDenied, "Billing request denied."}, true
	case 429, 503:
		return billingRoute{BillingRateLimited, "Rate limited — try again shortly."}, true
	}
	return billingRoute{}, false
}

func (a *Account) billingFailure(ctx context.Context, status int, payload *object, header http.Header) *BillingError {
	rawCode := payload.get("error")
	code, known := rawCode.(string)
	route, ok := billingRouteFor(status, code, known)
	if !ok {
		route = billingRoute{BillingDenied, fmt.Sprintf("Billing request failed (%d).", status)}
	}
	message := route.fallback
	if v := payload.get("message"); truthy(v) {
		message = textual(v)
	} else if route.kind == BillingDenied && truthy(rawCode) {
		message = textual(rawCode)
	}
	out := &BillingError{
		Message: message, Kind: route.kind, Status: status, Code: textualOrEmpty(rawCode),
		PortalURL: a.absolutizePortalURL(ctx, payload.get("portalUrl")), Payload: payload.raw(),
		Actor: textualOrEmpty(payload.get("actor")), Machine: textualOrEmpty(payload.get("code")), Recovery: textualOrEmpty(payload.get("recovery")),
	}
	if wait, ok := httpclient.RetryAfter(header, time.Now()); ok {
		seconds := int(wait.Seconds())
		out.RetryAfter = &seconds
	}
	return out
}

func (a *Account) billingRequest(ctx context.Context, method, path string, body *object, extra map[string]string, timeout time.Duration) (json.RawMessage, error) {
	ctx = a.bind(ctx)
	if timeout <= 0 {
		timeout = billingTimeout
	}
	client, _, err := a.verifiedClient(ctx, nil, "", nil)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		token, base, err := a.resolveTokenAndBase(ctx, attempt == 0)
		if err != nil {
			return nil, err
		}
		headers := map[string]string{"Authorization": "Bearer " + token}
		var data []byte
		if body != nil {
			headers["Content-Type"] = "application/json"
			data = asciiSpacedJSON(body)
		}
		for k, v := range extra {
			headers[k] = v
		}
		r, err := exchange(ctx, client, method, base+path, headers, data, timeout)
		if err != nil {
			reason := httpclient.Message(err)
			if errors.Is(err, httpclient.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				reason = "timed out"
			}
			return nil, &BillingError{Message: "Could not reach Icebath Portal: " + reason, Code: "network_error", Cause: err}
		}
		if r.status >= 200 && r.status < 300 {
			if strings.TrimSpace(string(r.body)) == "" {
				return json.RawMessage("{}"), nil
			}
			if _, err := parseValue(r.body); err != nil {
				return nil, &BillingError{Message: "Billing endpoint returned a non-JSON response (it may not be available on this deployment).",
					Code: "endpoint_unavailable", Status: r.status, Cause: err}
			}
			return json.RawMessage(r.body), nil
		}
		if r.status == http.StatusUnauthorized && attempt == 0 {
			a.InvalidateBillingToken()
			continue
		}
		return nil, a.billingFailure(ctx, r.status, r.object(), r.header)
	}
}

func requireString(value, message, code string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", &BillingError{Message: message, Code: code}
	}
	return trimmed, nil
}

func (a *Account) postIdempotent(ctx context.Context, path string, body *object, idempotencyKey, what string, timeout time.Duration) (json.RawMessage, error) {
	key, err := requireString(idempotencyKey, fmt.Sprintf("Idempotency-Key is required for %s.", what), "idempotency_key_required")
	if err != nil {
		return nil, err
	}
	return a.billingRequest(ctx, http.MethodPost, path, body, map[string]string{"Idempotency-Key": key}, timeout)
}

func (a *Account) BillingState(ctx context.Context, timeout time.Duration) (json.RawMessage, error) {
	return a.billingRequest(ctx, http.MethodGet, "/api/billing/state", nil, nil, timeout)
}

func (a *Account) PatchAutoTopUp(ctx context.Context, enabled bool, threshold, topUpAmount float64, timeout time.Duration) (json.RawMessage, error) {
	body := pairs("enabled", enabled, "threshold", jsonFloat(threshold), "topUpAmount", jsonFloat(topUpAmount))
	return a.billingRequest(ctx, http.MethodPatch, "/api/billing/auto-top-up", body, nil, timeout)
}

func (a *Account) PatchKeepGoingOnCredits(ctx context.Context, enabled bool, timeout time.Duration) (json.RawMessage, error) {
	return a.billingRequest(ctx, http.MethodPatch, "/api/billing/keep-going-on-credits", pairs("enabled", enabled), nil, timeout)
}

func (a *Account) PostCharge(ctx context.Context, amountUSD float64, idempotencyKey string, timeout time.Duration) (json.RawMessage, error) {
	return a.postIdempotent(ctx, "/api/billing/charge", pairs("amountUsd", jsonFloat(amountUSD)), idempotencyKey, "a charge", timeout)
}

func (a *Account) ChargeStatus(ctx context.Context, chargeID string, timeout time.Duration) (json.RawMessage, error) {
	id, err := requireString(chargeID, "A charge id is required.", "invalid_charge_id")
	if err != nil {
		return nil, err
	}
	return a.billingRequest(ctx, http.MethodGet, "/api/billing/charge/"+quotePathSegment(id), nil, nil, timeout)
}

func (a *Account) SubscriptionState(ctx context.Context, timeout time.Duration) (json.RawMessage, error) {
	return a.billingRequest(ctx, http.MethodGet, "/api/billing/subscription", nil, nil, timeout)
}

func (a *Account) SubscriptionPreview(ctx context.Context, subscriptionTypeID string, timeout time.Duration) (json.RawMessage, error) {
	return a.billingRequest(ctx, http.MethodPost, "/api/billing/subscription/preview", pairs("subscriptionTypeId", subscriptionTypeID), nil, timeout)
}

func (a *Account) PutSubscriptionPendingChange(ctx context.Context, subscriptionTypeID string, cancel bool, timeout time.Duration) (json.RawMessage, error) {
	body := pairs("type", "cancellation")
	if !cancel {
		tier, err := requireString(subscriptionTypeID, "A subscription tier is required to schedule a plan change.", "invalid_subscription_type")
		if err != nil {
			return nil, err
		}
		body = pairs("type", "tier_change", "subscriptionTypeId", tier)
	}
	return a.billingRequest(ctx, http.MethodPut, "/api/billing/subscription/pending-change", body, nil, timeout)
}

func (a *Account) DeleteSubscriptionPendingChange(ctx context.Context, timeout time.Duration) (json.RawMessage, error) {
	return a.billingRequest(ctx, http.MethodDelete, "/api/billing/subscription/pending-change", nil, nil, timeout)
}

func (a *Account) SubscriptionUpgrade(ctx context.Context, subscriptionTypeID, idempotencyKey string, timeout time.Duration) (json.RawMessage, error) {
	return a.postIdempotent(ctx, "/api/billing/subscription/upgrade", pairs("subscriptionTypeId", subscriptionTypeID), idempotencyKey, "an upgrade", timeout)
}

func jsonFloat(f float64) json.Number {
	return json.Number(floatText(f))
}

func asciiSpacedJSON(v any) []byte {
	var b strings.Builder
	for _, r := range spacedJSON(v) {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			high, low := utf16.EncodeRune(r)
			fmt.Fprintf(&b, "\\u%04x\\u%04x", high, low)
		}
	}
	return []byte(b.String())
}

func quotePathSegment(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(unreserved, s[i]) >= 0 {
			b.WriteByte(s[i])
		} else {
			fmt.Fprintf(&b, "%%%02X", s[i])
		}
	}
	return b.String()
}
