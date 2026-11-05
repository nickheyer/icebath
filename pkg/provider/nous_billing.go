package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// NousBillingDefaultTimeout keeps a hung portal from freezing the TUI on charge and poll calls
const NousBillingDefaultTimeout = 15 * time.Second

const nousBillingTokenCacheTTL = 30 * time.Second

// BillingErrorKind is the typed outcome of a failed billing call
type BillingErrorKind int

const (
	BillingErrorGeneric BillingErrorKind = iota
	BillingErrorScopeRequired
	BillingErrorAuth
	BillingErrorSessionRevoked
	BillingErrorRemoteSpendingRevoked
	BillingErrorRateLimited
	BillingErrorStripeUnavailable
	BillingErrorUpgradeCapExceeded
)

// IsAuth reports the 401 family, a logged out session included
func (k BillingErrorKind) IsAuth() bool {
	return k == BillingErrorAuth || k == BillingErrorSessionRevoked
}

// IsTransient reports outcomes that never reached Stripe so a retry after backoff is safe
func (k BillingErrorKind) IsTransient() bool {
	return k == BillingErrorRateLimited || k == BillingErrorStripeUnavailable || k == BillingErrorUpgradeCapExceeded
}

// BillingError is a failed billing call carrying the server code, deep link, backoff and payload
type BillingError struct {
	Kind       BillingErrorKind
	Message    string
	Status     int
	ErrorCode  any
	PortalURL  any
	RetryAfter *int
	Payload    map[string]any
	Actor      any
	Code       any
	Recovery   any
	Cause      error
}

func (e *BillingError) Error() string { return e.Message }

func (e *BillingError) Unwrap() error { return e.Cause }

// ResolveNousBillingPortalBaseURL picks the operator override, then the stored portal, then the default
func ResolveNousBillingPortalBaseURL(env PoolEnv, state map[string]any) string {
	for _, candidate := range []any{nousPortalEnvOverride(env), state["portal_base_url"]} {
		if s, ok := candidate.(string); ok && pyStripSpace(s) != "" {
			return strings.TrimRight(pyStripSpace(s), "/")
		}
	}
	return DefaultNousPortalURL
}

// AbsolutizeNousBillingPortalURL joins a relative server portalUrl onto the portal base, other values pass through
func AbsolutizeNousBillingPortalURL(env PoolEnv, portalURL any) (any, error) {
	s, ok := portalURL.(string)
	if !ok || pyStripSpace(s) == "" {
		return portalURL, nil
	}
	joined, err := pyURLJoin(strings.TrimRight(ResolveNousBillingPortalBaseURL(env, nil), "/")+"/", s)
	if err != nil {
		return nil, err
	}
	return joined, nil
}

type nousBillingCachedToken struct {
	at          time.Time
	token, base string
}

var (
	nousBillingTokenMu    sync.Mutex
	nousBillingTokenCache = map[string]nousBillingCachedToken{}
	nousBillingResolveFn  = nousBillingResolveTokenAndBase
	nousBillingTokenFn    = func(env PoolEnv) (string, error) { return ResolveNousAccessToken(env, NousAccessTokenOptions{}) }
)

// Drops every cached billing bearer so a replay after a scope grant uses the newly scoped token
func nousBillingInvalidateCachedToken() {
	nousBillingTokenMu.Lock()
	nousBillingTokenCache = map[string]nousBillingCachedToken{}
	nousBillingTokenMu.Unlock()
}

// Canonical not logged in auth error wrapping the resolver failure when there was one
func nousBillingNotLoggedIn(cause error) *BillingError {
	return &BillingError{Kind: BillingErrorAuth, Message: "Not logged into Nous Portal — run `icebath portal` to log in.",
		Status: 401, ErrorCode: "invalid_token", Payload: map[string]any{}, Cause: cause}
}

// Portal bearer and base for the env's home, served from a 30 second cache unless useCache is false
func nousBillingResolveTokenAndBase(env PoolEnv, useCache bool) (string, string, error) {
	key := nousAccountHomeKey(env)
	if useCache {
		nousBillingTokenMu.Lock()
		cached, ok := nousBillingTokenCache[key]
		nousBillingTokenMu.Unlock()
		if ok && time.Since(cached.at) < nousBillingTokenCacheTTL {
			return cached.token, cached.base, nil
		}
	}
	var state map[string]any
	if env.Store != nil {
		if loaded, err := env.Store.GetProviderAuthState("nous"); err == nil {
			state = loaded
		}
	}
	base := ResolveNousBillingPortalBaseURL(env, state)
	token, err := nousBillingTokenFn(env)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return "", "", nousBillingNotLoggedIn(err)
		}
		return "", "", err
	}
	token = pyStripSpace(token)
	nousBillingTokenMu.Lock()
	nousBillingTokenCache[key] = nousBillingCachedToken{at: time.Now(), token: token, base: base}
	nousBillingTokenMu.Unlock()
	return token, base, nil
}

// Whole Retry-After seconds, nil when absent or unreadable
func nousBillingRetryAfter(h http.Header) *int {
	if h == nil {
		return nil
	}
	seconds, ok := parseRetryAfterSeconds(h)
	if !ok {
		return nil
	}
	return &seconds
}

type nousBillingRoute struct {
	kind     BillingErrorKind
	fallback string
}

type nousBillingStatusCode struct {
	status int
	code   string
}

var (
	nousBillingErrorsByCode = map[string]nousBillingRoute{
		"stripe_unavailable":   {BillingErrorStripeUnavailable, "Stripe is temporarily unavailable — try again shortly."},
		"upgrade_cap_exceeded": {BillingErrorUpgradeCapExceeded, "Daily plan-change limit reached — try again tomorrow."},
	}
	nousBillingErrorsByStatusCode = map[nousBillingStatusCode]nousBillingRoute{
		{401, "session_revoked"}:         {BillingErrorSessionRevoked, "Your session was logged out — log in again."},
		{403, "remote_spending_revoked"}: {BillingErrorRemoteSpendingRevoked, "Remote spending was stopped for this terminal."},
		{403, "insufficient_scope"}:      {BillingErrorScopeRequired, "This action needs the billing:manage scope."},
	}
	nousBillingErrorsByStatus = map[int]nousBillingRoute{
		401: {BillingErrorAuth, "Authentication required."},
		403: {BillingErrorGeneric, "Billing request denied."},
		429: {BillingErrorRateLimited, "Rate limited — try again shortly."},
		503: {BillingErrorRateLimited, "Rate limited — try again shortly."},
	}
)

// Typed billing error of an HTTP error response, routed by code, then status and code, then status
func nousBillingErrorFor(env PoolEnv, status int, payload any, headers http.Header) error {
	p, _ := payload.(map[string]any)
	if p == nil {
		p = map[string]any{}
	}
	errVal := p["error"]
	key, keyed := errVal.(string)
	route, ok := nousBillingRoute{}, false
	if keyed {
		if route, ok = nousBillingErrorsByCode[key]; !ok {
			route, ok = nousBillingErrorsByStatusCode[nousBillingStatusCode{status, key}]
		}
	}
	if !ok {
		if route, ok = nousBillingErrorsByStatus[status]; !ok {
			route = nousBillingRoute{BillingErrorGeneric, fmt.Sprintf("Billing request failed (%d).", status)}
		}
	}
	portal, err := AbsolutizeNousBillingPortalURL(env, p["portalUrl"])
	if err != nil {
		return err
	}
	message := route.fallback
	switch {
	case pyTruthy(p["message"]):
		message = nousBillingStr(p["message"])
	case route.kind == BillingErrorGeneric && pyTruthy(errVal):
		message = nousBillingStr(errVal)
	}
	return &BillingError{Kind: route.kind, Message: message, Status: status, ErrorCode: errVal, PortalURL: portal,
		RetryAfter: nousBillingRetryAfter(headers), Payload: p, Actor: p["actor"], Code: p["code"], Recovery: p["recovery"]}
}

// Python str of a decoded JSON value with numbers printed as Python prints int and float
func nousBillingStr(v any) string {
	if n, ok := v.(json.Number); ok {
		return nousBillingNumberText(n)
	}
	return pyStr(v)
}

// Python text of a decoded JSON number, integers exact and floats in repr form
func nousBillingNumberText(n json.Number) string {
	text := n.String()
	if !strings.ContainsAny(text, ".eE") {
		if i, ok := new(big.Int).SetString(text, 10); ok {
			return i.String()
		}
	}
	f, _ := strconv.ParseFloat(text, 64)
	return nousBillingFloatJSON(f)
}

// Float as Python json.dumps writes it, Infinity and NaN spelled the JavaScript way
func nousBillingFloatJSON(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	}
	return pyFloatRepr(f)
}

type nousBillingField struct {
	key   string
	value any
}

// Python json.dumps text of a request body with default separators and ASCII escaping
func nousBillingDumpsFields(fields []nousBillingField) (string, error) {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		value, err := nousBillingDumps(f.value)
		if err != nil {
			return "", err
		}
		parts = append(parts, pyJSONQuote(f.key)+": "+value)
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

// Python json.dumps text of one value with mapping keys written in sorted order
func nousBillingDumps(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "null", nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case string:
		return pyJSONQuote(t), nil
	case json.Number:
		return nousBillingNumberText(t), nil
	case float64:
		return nousBillingFloatJSON(t), nil
	case float32:
		return nousBillingFloatJSON(float64(t)), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			s, err := nousBillingDumps(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]nousBillingField, 0, len(keys))
		for _, k := range keys {
			fields = append(fields, nousBillingField{k, t[k]})
		}
		return nousBillingDumpsFields(fields)
	}
	return "", fmt.Errorf("Object of type %T is not JSON serializable", v)
}

// Python float of a money argument, a ValueError or TypeError text when it does not convert
func nousBillingFloat(v any) (float64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		if f, ok := nousFloatText(t); ok {
			return f, nil
		}
		return 0, fmt.Errorf("could not convert string to float: %s", pyRepr(t))
	case nil:
		return 0, fmt.Errorf("float() argument must be a string or a real number, not 'NoneType'")
	}
	f, err := nousCoerceFloat(v)
	if err != nil {
		return 0, err
	}
	if f == nil {
		return 0, fmt.Errorf("float() argument must be a string or a real number, not '%T'", v)
	}
	return *f, nil
}

// Refuses the redirects urllib refuses and keeps the bearer across hosts as urllib does
func nousBillingCheckRedirect(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1]
	code := 0
	if req.Response != nil {
		code = req.Response.StatusCode
	}
	method := prev.Method
	readable := method == http.MethodGet || method == http.MethodHead
	allowed := (readable && (code == 301 || code == 302 || code == 303 || code == 307 || code == 308)) ||
		(method == http.MethodPost && (code == 301 || code == 302 || code == 303))
	visited := map[string]int{}
	for _, r := range via[1:] {
		visited[r.URL.String()]++
	}
	if !allowed || visited[req.URL.String()] >= 4 || len(visited) >= 10 {
		return http.ErrUseLastResponse
	}
	req.Header.Del("Content-Type")
	req.Header.Del("Content-Length")
	if req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", via[0].Header.Get("Authorization"))
	}
	return nil
}

// Billing HTTP client from the env transport with the call's timeout and urllib redirect rules
func nousBillingClient(env PoolEnv, timeout time.Duration) *http.Client {
	client := &http.Client{}
	if env.HTTP != nil {
		copied := *env.HTTP
		client = &copied
	}
	client.Timeout = timeout
	client.CheckRedirect = nousBillingCheckRedirect
	return client
}

// Network failure as a typed billing error, every timeout reading as timed out
func nousBillingTransportError(err error) error {
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return &BillingError{Message: "Could not reach Nous Portal: timed out", ErrorCode: "network_error", Payload: map[string]any{}, Cause: err}
	}
	reason := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		reason = urlErr.Err.Error()
	}
	return &BillingError{Message: "Could not reach Nous Portal: " + reason, ErrorCode: "network_error", Payload: map[string]any{}, Cause: err}
}

// Authenticated billing call returning the decoded JSON, retrying a 401 once with a freshly resolved token
func nousBillingRequest(env PoolEnv, method, path string, body []nousBillingField, extra [][2]string, timeout time.Duration, retriedAuth bool) (any, error) {
	if timeout <= 0 {
		timeout = NousBillingDefaultTimeout
	}
	token, base, err := nousBillingResolveFn(env, !retriedAuth)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		data, err := nousBillingDumpsFields(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(data)
	}
	req, err := http.NewRequestWithContext(env.ctx(), method, base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range extra {
		req.Header.Set(h[0], h[1])
	}
	req.Header.Set("User-Agent", "icebath-agent")
	resp, err := nousBillingClient(env, timeout).Do(req)
	if err != nil {
		return nil, nousBillingTransportError(err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized && !retriedAuth {
			nousBillingInvalidateCachedToken()
			return nousBillingRequest(env, method, path, body, extra, timeout, true)
		}
		var payload any = map[string]any{}
		if readErr == nil && utf8.Valid(raw) && pyStripSpace(string(raw)) != "" {
			if decoded, err := decodeExactJSON(string(raw)); err == nil {
				payload = decoded
			}
		}
		return nil, nousBillingErrorFor(env, resp.StatusCode, payload, resp.Header)
	}
	if readErr != nil {
		var netErr net.Error
		if (errors.As(readErr, &netErr) && netErr.Timeout()) || errors.Is(readErr, context.DeadlineExceeded) {
			return nil, &BillingError{Message: "Could not reach Nous Portal: timed out", ErrorCode: "network_error", Payload: map[string]any{}, Cause: readErr}
		}
		return nil, readErr
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("'utf-8' codec can't decode the billing response")
	}
	if pyStripSpace(string(raw)) == "" {
		return map[string]any{}, nil
	}
	decoded, err := decodeExactJSON(string(raw))
	if err != nil {
		return nil, &BillingError{Message: "Billing endpoint returned a non-JSON response (it may not be available on this deployment).",
			Status: resp.StatusCode, ErrorCode: "endpoint_unavailable", Payload: map[string]any{}, Cause: err}
	}
	return decoded, nil
}

// Stripped non blank string or a typed billing error naming what is missing
func nousBillingRequireStr(value any, message, code string) (string, error) {
	s, ok := value.(string)
	if !ok || pyStripSpace(s) == "" {
		return "", &BillingError{Message: message, ErrorCode: code, Payload: map[string]any{}}
	}
	return pyStripSpace(s), nil
}

// POST carrying the mandatory Idempotency-Key header
func nousBillingPostIdempotent(env PoolEnv, path string, body []nousBillingField, idempotencyKey any, what string, timeout time.Duration) (any, error) {
	key, err := nousBillingRequireStr(idempotencyKey, "Idempotency-Key is required for "+what+".", "idempotency_key_required")
	if err != nil {
		return nil, err
	}
	return nousBillingRequest(env, http.MethodPost, path, body, [][2]string{{"Idempotency-Key", key}}, timeout, false)
}

// GetNousBillingState reads the role tiered billing overview
func GetNousBillingState(env PoolEnv, timeout time.Duration) (any, error) {
	return nousBillingRequest(env, http.MethodGet, "/api/billing/state", nil, nil, timeout, false)
}

// PatchNousAutoTopUp configures auto reload with the threshold and amount sent as JSON numbers
func PatchNousAutoTopUp(env PoolEnv, enabled bool, threshold, topUpAmount any, timeout time.Duration) (any, error) {
	thr, err := nousBillingFloat(threshold)
	if err != nil {
		return nil, err
	}
	amount, err := nousBillingFloat(topUpAmount)
	if err != nil {
		return nil, err
	}
	body := []nousBillingField{{"enabled", enabled}, {"threshold", thr}, {"topUpAmount", amount}}
	return nousBillingRequest(env, http.MethodPatch, "/api/billing/auto-top-up", body, nil, timeout, false)
}

// PatchNousKeepGoingOnCredits sets the window plan keep going on credits switch
func PatchNousKeepGoingOnCredits(env PoolEnv, enabled bool, timeout time.Duration) (any, error) {
	return nousBillingRequest(env, http.MethodPatch, "/api/billing/keep-going-on-credits", []nousBillingField{{"enabled", enabled}}, nil, timeout, false)
}

// PostNousBillingCharge buys credits, the 202 chargeId still needing a status poll before money is confirmed
func PostNousBillingCharge(env PoolEnv, amountUSD, idempotencyKey any, timeout time.Duration) (any, error) {
	amount, err := nousBillingFloat(amountUSD)
	if err != nil {
		return nil, err
	}
	return nousBillingPostIdempotent(env, "/api/billing/charge", []nousBillingField{{"amountUsd", amount}}, idempotencyKey, "a charge", timeout)
}

// GetNousChargeStatus polls a charge with the id escaped so a stray slash keeps the path shape
func GetNousChargeStatus(env PoolEnv, chargeID any, timeout time.Duration) (any, error) {
	id, err := nousBillingRequireStr(chargeID, "A charge id is required.", "invalid_charge_id")
	if err != nil {
		return nil, err
	}
	return nousBillingRequest(env, http.MethodGet, "/api/billing/charge/"+pyQuoteAll(id), nil, nil, timeout, false)
}

// GetNousSubscriptionState reads the current plan, tiers and usage
func GetNousSubscriptionState(env PoolEnv, timeout time.Duration) (any, error) {
	return nousBillingRequest(env, http.MethodGet, "/api/billing/subscription", nil, nil, timeout, false)
}

// PostNousSubscriptionPreview quotes a plan change without charging
func PostNousSubscriptionPreview(env PoolEnv, subscriptionTypeID any, timeout time.Duration) (any, error) {
	return nousBillingRequest(env, http.MethodPost, "/api/billing/subscription/preview",
		[]nousBillingField{{"subscriptionTypeId", subscriptionTypeID}}, nil, timeout, false)
}

// PutNousSubscriptionPendingChange schedules a cancellation or an end of period tier change
func PutNousSubscriptionPendingChange(env PoolEnv, subscriptionTypeID any, cancel bool, timeout time.Duration) (any, error) {
	body := []nousBillingField{{"type", "cancellation"}}
	if !cancel {
		tier, err := nousBillingRequireStr(subscriptionTypeID, "A subscription tier is required to schedule a plan change.", "invalid_subscription_type")
		if err != nil {
			return nil, err
		}
		body = []nousBillingField{{"type", "tier_change"}, {"subscriptionTypeId", tier}}
	}
	return nousBillingRequest(env, http.MethodPut, "/api/billing/subscription/pending-change", body, nil, timeout, false)
}

// DeleteNousSubscriptionPendingChange clears a scheduled downgrade or cancellation
func DeleteNousSubscriptionPendingChange(env PoolEnv, timeout time.Duration) (any, error) {
	return nousBillingRequest(env, http.MethodDelete, "/api/billing/subscription/pending-change", nil, nil, timeout, false)
}

// PostNousSubscriptionUpgrade prorates, charges and flips the plan in one Stripe operation
func PostNousSubscriptionUpgrade(env PoolEnv, subscriptionTypeID, idempotencyKey any, timeout time.Duration) (any, error) {
	return nousBillingPostIdempotent(env, "/api/billing/subscription/upgrade",
		[]nousBillingField{{"subscriptionTypeId", subscriptionTypeID}}, idempotencyKey, "an upgrade", timeout)
}

type pyURLParts struct {
	scheme, netloc, path, params, query, fragment string
}

var (
	pyUsesRelative = setOf("", "ftp", "http", "gopher", "nntp", "imap", "wais", "file", "https", "shttp", "mms",
		"prospero", "rtsp", "rtsps", "rtspu", "sftp", "svn", "svn+ssh", "ws", "wss")
	pyUsesNetloc = setOf("", "ftp", "http", "gopher", "nntp", "telnet", "imap", "wais", "file", "mms", "https", "shttp",
		"snews", "prospero", "rtsp", "rtsps", "rtspu", "rsync", "svn", "svn+ssh", "sftp", "nfs", "git", "git+ssh", "ws", "wss", "itms-services")
	pyUsesParams = setOf("", "ftp", "hdl", "prospero", "http", "imap", "https", "shttp", "rtsp", "rtsps", "rtspu", "sip",
		"sips", "mms", "sftp", "tel")
	pyIPvFutureRe = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)
)

const pySchemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

// Python urlparse into scheme, netloc, path, params, query and fragment with its ValueError checks
func pyURLParse(raw, defaultScheme string) (pyURLParts, error) {
	u := strings.TrimLeftFunc(raw, func(r rune) bool { return r <= 0x20 })
	scheme := strings.TrimFunc(defaultScheme, func(r rune) bool { return r <= 0x20 })
	for _, unsafe := range []string{"\t", "\r", "\n"} {
		u, scheme = strings.ReplaceAll(u, unsafe, ""), strings.ReplaceAll(scheme, unsafe, "")
	}
	var p pyURLParts
	if i := strings.IndexByte(u, ':'); i > 0 && u[0] < 0x80 && ((u[0] >= 'a' && u[0] <= 'z') || (u[0] >= 'A' && u[0] <= 'Z')) &&
		strings.Trim(u[:i], pySchemeChars) == "" {
		scheme, u = strings.ToLower(u[:i]), u[i+1:]
	}
	p.scheme = scheme
	if strings.HasPrefix(u, "//") {
		end := len(u)
		for _, c := range "/?#" {
			if w := strings.IndexRune(u[2:], c); w >= 0 && w+2 < end {
				end = w + 2
			}
		}
		p.netloc, u = u[2:end], u[end:]
		open, closed := strings.Contains(p.netloc, "["), strings.Contains(p.netloc, "]")
		if open != closed {
			return p, errors.New("Invalid IPv6 URL")
		}
		if open {
			if err := pyCheckBracketedNetloc(p.netloc); err != nil {
				return p, err
			}
		}
	}
	if before, after, found := strings.Cut(u, "#"); found {
		u, p.fragment = before, after
	}
	if before, after, found := strings.Cut(u, "?"); found {
		u, p.query = before, after
	}
	if err := pyCheckNetloc(p.netloc); err != nil {
		return p, err
	}
	p.path = u
	if pyUsesParams[p.scheme] && strings.Contains(u, ";") {
		start := strings.LastIndexByte(u, '/')
		if start < 0 {
			start = 0
		}
		if i := strings.IndexByte(u[start:], ';'); i >= 0 {
			p.path, p.params = u[:start+i], u[start+i+1:]
		}
	}
	return p, nil
}

// Bracketed host must be an IPv6 address or a well formed IPvFuture literal
func pyCheckBracketedNetloc(netloc string) error {
	hostPort := netloc[strings.LastIndexByte(netloc, '@')+1:]
	before, bracketed, open := strings.Cut(hostPort, "[")
	var host string
	if open {
		if before != "" {
			return errors.New("Invalid IPv6 URL")
		}
		var port string
		host, port, _ = strings.Cut(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return errors.New("Invalid IPv6 URL")
		}
	} else {
		host, _, _ = strings.Cut(hostPort, ":")
	}
	if strings.HasPrefix(host, "v") {
		if !pyIPvFutureRe.MatchString(host) {
			return errors.New("IPvFuture address is invalid")
		}
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%s does not appear to be an IPv4 or IPv6 address", pyRepr(host))
	}
	if addr.Is4() {
		return errors.New("An IPv4 address cannot be in brackets")
	}
	return nil
}

// Rejects a non ASCII netloc whose NFKC form smuggles in a URL delimiter
func pyCheckNetloc(netloc string) error {
	ascii := true
	for i := 0; i < len(netloc); i++ {
		if netloc[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return nil
	}
	n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
	normalized := norm.NFKC.String(n)
	if n == normalized {
		return nil
	}
	for _, c := range "/?#@:" {
		if strings.ContainsRune(normalized, c) {
			return fmt.Errorf("netloc '%s' contains invalid characters under NFKC normalization", netloc)
		}
	}
	return nil
}

// Python urlunparse of the six components
func pyURLUnparse(p pyURLParts) string {
	u := p.path
	if p.params != "" {
		u += ";" + p.params
	}
	switch {
	case p.netloc != "":
		if u != "" && !strings.HasPrefix(u, "/") {
			u = "/" + u
		}
		u = "//" + p.netloc + u
	case strings.HasPrefix(u, "//"):
		u = "//" + u
	case p.scheme != "" && pyUsesNetloc[p.scheme] && (u == "" || strings.HasPrefix(u, "/")):
		u = "//" + u
	}
	if p.scheme != "" {
		u = p.scheme + ":" + u
	}
	if p.query != "" {
		u += "?" + p.query
	}
	if p.fragment != "" {
		u += "#" + p.fragment
	}
	return u
}

// Python urljoin resolving ref against base with RFC 3986 dot segment removal
func pyURLJoin(base, ref string) (string, error) {
	if base == "" {
		return ref, nil
	}
	if ref == "" {
		return base, nil
	}
	b, err := pyURLParse(base, "")
	if err != nil {
		return "", err
	}
	r, err := pyURLParse(ref, b.scheme)
	if err != nil {
		return "", err
	}
	if r.scheme != b.scheme || !pyUsesRelative[r.scheme] {
		return ref, nil
	}
	if pyUsesNetloc[r.scheme] {
		if r.netloc != "" {
			return pyURLUnparse(r), nil
		}
		r.netloc = b.netloc
	}
	if r.path == "" && r.params == "" {
		r.path, r.params = b.path, b.params
		if r.query == "" {
			r.query = b.query
		}
		return pyURLUnparse(r), nil
	}
	baseParts := strings.Split(b.path, "/")
	if baseParts[len(baseParts)-1] != "" {
		baseParts = baseParts[:len(baseParts)-1]
	}
	var segments []string
	if strings.HasPrefix(r.path, "/") {
		segments = strings.Split(r.path, "/")
	} else {
		segments = append(append([]string{}, baseParts...), strings.Split(r.path, "/")...)
		if len(segments) > 2 {
			kept := []string{segments[0]}
			for _, s := range segments[1 : len(segments)-1] {
				if s != "" {
					kept = append(kept, s)
				}
			}
			segments = append(kept, segments[len(segments)-1])
		}
	}
	var resolved []string
	for _, seg := range segments {
		switch seg {
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
		case ".":
		default:
			resolved = append(resolved, seg)
		}
	}
	if last := segments[len(segments)-1]; last == "." || last == ".." {
		resolved = append(resolved, "")
	}
	r.path = strings.Join(resolved, "/")
	if r.path == "" {
		r.path = "/"
	}
	return pyURLUnparse(r), nil
}
