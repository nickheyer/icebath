package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	formType          = "application/x-www-form-urlencoded"
	jsonType          = "application/json"
	instanceDiscovery = "https://login.microsoftonline.com/common/discovery/instance"
	phaseTimeout      = 30 * time.Second
)

var (
	knownAuthorityHosts = []string{"login.microsoftonline.com", "login.chinacloudapi.cn", "login-us.microsoftonline.com", "login.microsoftonline.us"}
	b2cHostSuffixes     = []string{"b2clogin.com", "b2clogin.cn", "b2clogin.us", "b2clogin.de", "ciamlogin.com"}
)

type endpoints struct {
	token     string
	authorize string
}

type client struct {
	api   *http.Client
	imds  *http.Client
	probe *http.Client
	loose *http.Client

	mu        sync.Mutex
	issuer    map[string]endpoints
	realmless map[string]bool
}

func newClient(o httpclient.Options) (*client, error) {
	build := func(mutate func(*httpclient.Options)) (*http.Client, error) {
		opts := o
		opts.Timeouts = httpclient.EachPhase(phaseTimeout)
		mutate(&opts)
		transport, err := httpclient.New(opts)
		if err != nil {
			return nil, err
		}
		return transport.Client(), nil
	}
	c := &client{issuer: map[string]endpoints{}, realmless: map[string]bool{}}
	var err error
	if c.api, err = build(func(opts *httpclient.Options) { opts.Timeouts = httpclient.EachPhase(requestTimeout) }); err != nil {
		return nil, err
	}
	if c.imds, err = build(func(opts *httpclient.Options) { opts.Timeouts.Connect = 2 * time.Second }); err != nil {
		return nil, err
	}
	if c.probe, err = build(func(opts *httpclient.Options) { opts.Timeouts.Connect = time.Second }); err != nil {
		return nil, err
	}
	if c.loose, err = build(func(opts *httpclient.Options) { opts.TLS.Insecure = true }); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *client) send(ctx context.Context, hc *http.Client, timeout time.Duration, method, target string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, replyLimit+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(data) > replyLimit {
		return 0, nil, nil, errors.New("reply is larger than 1 MiB")
	}
	return resp.StatusCode, resp.Header, data, nil
}

const (
	retryTotal      = 10
	retryStatus     = 3
	retryTransport  = 3
	retryBackoff    = 800 * time.Millisecond
	retryBackoffMax = 120 * time.Second
)

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return status >= 506 && status < 999
}

func retryAfter(header http.Header) (time.Duration, bool) {
	for _, name := range []string{"Retry-After-Ms", "X-Ms-Retry-After-Ms"} {
		if raw := strings.TrimSpace(header.Get(name)); raw != "" {
			if ms, err := strconv.ParseFloat(raw, 64); err == nil && ms >= 0 {
				return time.Duration(ms * float64(time.Millisecond)), true
			}
		}
	}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	if at, err := http.ParseTime(raw); err == nil {
		wait := time.Until(at)
		if wait < 0 {
			wait = 0
		}
		return wait, true
	}
	return 0, false
}

func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *client) call(ctx context.Context, method, target string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	failures, statusLeft, transportLeft, totalLeft := 0, retryStatus, retryTransport, retryTotal
	for {
		status, replyHeader, data, err := c.send(ctx, c.api, requestTimeout, method, target, header, body)
		var wait time.Duration
		told := false
		switch {
		case err != nil:
			if ctx.Err() != nil || transportLeft == 0 || totalLeft == 0 {
				return 0, nil, nil, err
			}
			transportLeft--
		case status < 400:
			return status, replyHeader, data, nil
		default:
			after, hasAfter := retryAfter(replyHeader)
			if !(hasAfter || retryableStatus(status)) || statusLeft == 0 || totalLeft == 0 {
				return status, replyHeader, data, nil
			}
			statusLeft--
			wait, told = after, hasAfter
		}
		totalLeft--
		failures++
		if !told && failures > 1 {
			wait = retryBackoff * time.Duration(1<<(failures-1))
			if wait > retryBackoffMax {
				wait = retryBackoffMax
			}
		}
		if err := pause(ctx, wait); err != nil {
			return 0, nil, nil, err
		}
	}
}

type document map[string]json.RawMessage

func parseDocument(data []byte) (document, bool) {
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		return nil, false
	}
	return doc, true
}

func (d document) text(key string) string {
	var s string
	if raw, ok := d[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func (d document) integer(key string) (int64, bool) {
	raw, ok := d[key]
	if !ok {
		return 0, false
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return int64(n), true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

type pair struct{ key, value string }

func encodeForm(pairs []pair) []byte {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p.key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p.value))
	}
	return []byte(b.String())
}

func withQuery(endpoint string, pairs []pair) string {
	if len(pairs) == 0 {
		return endpoint
	}
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	return endpoint + separator + string(encodeForm(pairs))
}

func authorityHost(env Env) string {
	authority := env(authorityVariable)
	if authority == "" {
		authority = defaultAuthority
	}
	if !strings.Contains(authority, "://") {
		authority = "https://" + authority
	}
	return strings.TrimRight(authority, " /")
}

func (c *client) discover(ctx context.Context, credential, authority, tenant string) (endpoints, error) {
	base := authority + "/" + tenant
	c.mu.Lock()
	known, ok := c.issuer[base]
	c.mu.Unlock()
	if ok {
		return known, nil
	}
	fail := func(message string, err error) (endpoints, error) {
		return endpoints{}, &AuthError{Credential: credential, Message: message, Err: err}
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fail("'"+authority+"' is an invalid authority. The value must be a TLS protected (https) endpoint.", nil)
	}
	host := strings.ToLower(u.Hostname())
	configURL := base + "/v2.0/.well-known/openid-configuration"
	wellKnown, b2c := false, false
	for _, name := range knownAuthorityHosts {
		wellKnown = wellKnown || host == name
	}
	for _, suffix := range b2cHostSuffixes {
		b2c = b2c || strings.HasSuffix(host, suffix)
	}
	switch {
	case tenant == "adfs":
		configURL = base + "/.well-known/openid-configuration"
	case !wellKnown && !b2c:
		target := withQuery(instanceDiscovery, []pair{{"authorization_endpoint", base + "/oauth2/v2.0/authorize"}, {"api-version", "1.0"}})
		_, _, data, err := c.call(ctx, http.MethodGet, target, http.Header{"Accept": {jsonType}}, nil)
		if err != nil {
			return fail("instance discovery could not be reached: "+httpclient.Message(err), err)
		}
		reply, _ := parseDocument(data)
		if reply.text("error") == "invalid_instance" {
			return fail("invalid_instance: the authority "+base+" is not a known Microsoft Entra authority. "+reply.text("error_description"), nil)
		}
		if endpoint := reply.text("tenant_discovery_endpoint"); endpoint != "" {
			configURL = endpoint
		}
	}
	status, _, data, err := c.call(ctx, http.MethodGet, configURL, http.Header{"Accept": {jsonType}}, nil)
	if err != nil {
		return fail("authority configuration for "+base+" could not be fetched: "+httpclient.Message(err), err)
	}
	reply, ok := parseDocument(data)
	found := endpoints{token: reply.text("token_endpoint"), authorize: reply.text("authorization_endpoint")}
	if status != http.StatusOK || !ok || found.token == "" {
		return fail("Unable to get authority configuration for "+base+". Authority would typically be in a format of https://login.microsoftonline.com/your_tenant. Also please double check your tenant name or GUID is correct. (HTTP "+strconv.Itoa(status)+")", nil)
	}
	c.mu.Lock()
	c.issuer[base] = found
	c.mu.Unlock()
	return found, nil
}

func tokenFromReply(credential string, status int, data []byte, requestTime int64) (accessToken, error) {
	reply, ok := parseDocument(data)
	if !ok {
		return accessToken{}, &AuthError{Credential: credential, Status: status, Message: "Authentication failed: token endpoint reply is not a JSON object"}
	}
	value := reply.text("access_token")
	if value == "" {
		detail := reply.text("error_description")
		if detail == "" {
			detail = reply.text("error")
		}
		if detail == "" {
			detail = "token endpoint reply carried no access token"
		}
		return accessToken{}, &AuthError{Credential: credential, Status: status, Code: reply.text("error"), Message: "Authentication failed: " + detail}
	}
	token := accessToken{value: value}
	if lifetime, ok := reply.integer("expires_in"); ok {
		token.expiresOn = requestTime + lifetime
	} else if on, ok := reply.integer("expires_on"); ok {
		token.expiresOn = on
	} else {
		return accessToken{}, &AuthError{Credential: credential, Status: status, Message: "Authentication failed: token endpoint reply carried no lifetime"}
	}
	if in, ok := reply.integer("refresh_in"); ok {
		token.refreshOn = requestTime + in
	}
	return token, nil
}

func (c *client) requestToken(ctx context.Context, credential, endpoint string, form []pair) (accessToken, document, error) {
	requestTime := time.Now().Unix()
	header := http.Header{"Content-Type": {formType}, "Accept": {jsonType}}
	status, _, data, err := c.call(ctx, http.MethodPost, endpoint, header, encodeForm(form))
	if err != nil {
		return accessToken{}, nil, &AuthError{Credential: credential, Message: "Authentication failed: " + httpclient.Message(err), Err: err}
	}
	token, err := tokenFromReply(credential, status, data, requestTime)
	if err != nil {
		return accessToken{}, nil, err
	}
	reply, _ := parseDocument(data)
	return token, reply, nil
}
