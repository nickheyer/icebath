package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/pkg/httpx"
)

// Authority hosts Microsoft lists, which need no instance discovery
var knownAuthorities = map[string]bool{"login.microsoftonline.com": true, "login.microsoft.com": true, "login.windows.net": true,
	"sts.windows.net": true, "login.chinacloudapi.cn": true, "login.partner.microsoftonline.cn": true, "login.microsoftonline.de": true,
	"login-us.microsoftonline.com": true, "login.microsoftonline.us": true, "login.usgovcloudapi.net": true, "login.sovcloud-identity.fr": true,
	"login.sovcloud-identity.de": true, "login.sovcloud-identity.sg": true}

// Hosts of B2C authorities, which the developer vouches for
var b2cHosts = []string{"b2clogin.com", "b2clogin.cn", "b2clogin.us", "b2clogin.de", "ciamlogin.com"}

// Authority AZURE_AUTHORITY_HOST names as an https origin, refusing any other scheme
func (e environment) strictAuthority() (string, error) {
	authority, ok := e("AZURE_AUTHORITY_HOST")
	if !ok {
		authority = defaultAuthority
	}
	parsed, err := url.Parse(strings.TrimSpace(authority))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" && !strings.Contains(authority, "://") {
		return "https://" + strings.TrimRight(strings.TrimSpace(authority), " /"), nil
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("The sign-in address %s can't be used, because it does not start with https.", authority)
	}
	return strings.TrimRight(strings.TrimSpace(authority), " /"), nil
}

// Statuses the token service may answer once more, every status up to 999 but those that settle the request
func retryable(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return status >= 506 && status < 999
}

// Sends one request to the token service, trying again over failed connections and passing statuses: at most ten tries, three for each
// kind of failure, waiting what Retry-After asks or 0.8 seconds doubled for each earlier failure up to two minutes
func send(ctx context.Context, method, target string, headers map[string]string, form url.Values, retryPOST bool) (int, []byte, error) {
	headers = mergeHeaders(headers, map[string]string{"x-ms-client-request-id": uuid.NewString()})
	body := form.Encode()
	total, connects, reads, statuses, history := 10, 3, 3, 3, 0
	client := &http.Client{Timeout: 300 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		var reader io.Reader
		if form != nil {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return 0, nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, err
			}
			if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "dial" {
				connects--
			} else if method == http.MethodPost && !retryPOST {
				return 0, nil, err
			} else {
				reads--
			}
			total, history = total-1, history+1
			if total < 0 || connects < 0 || reads < 0 {
				return 0, nil, err
			}
			if err := backoff(ctx, history, 0); err != nil {
				return 0, nil, err
			}
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		after, hasAfter := httpx.RetryAfter(resp.Header.Get("Retry-After"))
		retry := false
		if resp.StatusCode >= 400 {
			methodOK := method != http.MethodPost && method != http.MethodPatch || retryPOST || resp.StatusCode == 500 || resp.StatusCode == 503 || resp.StatusCode == 504
			retry = hasAfter || methodOK && total > 0 && retryable(resp.StatusCode)
		}
		if !retry {
			return resp.StatusCode, raw, nil
		}
		total, statuses, history = total-1, statuses-1, history+1
		if total < 0 || statuses < 0 {
			return resp.StatusCode, raw, nil
		}
		if err := backoff(ctx, history, after); err != nil {
			return 0, nil, err
		}
	}
}

func mergeHeaders(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// Waits what the service asked, else 0.8 seconds doubled for each failure after the first, at most two minutes
func backoff(ctx context.Context, history int, asked time.Duration) error {
	wait := asked
	if wait <= 0 && history > 1 {
		wait = time.Duration(math.Min(120, 0.8*math.Pow(2, float64(history-1))) * float64(time.Second))
	}
	if wait <= 0 {
		return nil
	}
	return credentials.Sleep(ctx, wait)
}

// Confidential client of one service principal: the token endpoint its tenant's OpenID configuration names, and the tokens it got for
// each set of scopes
type confidential struct {
	mu     sync.Mutex
	client string
	// Address of the tenant's authority
	authority string
	// Fields that prove the client to the token endpoint: its secret, or an assertion signed for the endpoint
	proof    func(ctx context.Context, endpoint string) (url.Values, error)
	endpoint string
	tokens   map[string]token
}

func newConfidential(env environment, tenant, client string, proof func(ctx context.Context, endpoint string) (url.Values, error)) (*confidential, error) {
	authority, err := env.strictAuthority()
	if err != nil {
		return nil, err
	}
	return &confidential{client: client, authority: authority + "/" + tenant, proof: proof, tokens: map[string]token{}}, nil
}

// Token endpoint of the tenant from its OpenID configuration, an authority host Microsoft doesn't list checked by instance discovery first
func (a *confidential) discover(ctx context.Context) error {
	if a.endpoint != "" {
		return nil
	}
	u, err := url.Parse(a.authority)
	if err != nil {
		return err
	}
	instance := strings.ToLower(u.Host)
	parts := strings.Split(u.Path, "/")
	tenant := ""
	if len(parts) > 1 {
		tenant = parts[1]
	}
	adfs := strings.EqualFold(tenant, "adfs") && !strings.HasSuffix(instance, ".ciamlogin.com")
	vouched := adfs || len(parts) == 3 && strings.HasPrefix(strings.ToLower(parts[2]), "b2c_")
	for _, host := range b2cHosts {
		vouched = vouched || strings.HasSuffix(instance, "."+host)
	}
	version := "/v2.0"
	if adfs {
		version = ""
	}
	discovery := *u
	discovery.Path = u.Path + version + "/.well-known/openid-configuration"
	endpoint := discovery.String()
	if !knownAuthorities[instance] && !vouched {
		authorize := "https://" + instance + u.Path + "/oauth2/v2.0/authorize"
		status, raw, err := send(ctx, http.MethodGet, "https://login.microsoftonline.com/common/discovery/instance?"+
			url.Values{"authorization_endpoint": {authorize}, "api-version": {"1.0"}}.Encode(), nil, nil, false)
		if err != nil {
			return err
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		if payload["error"] == "invalid_instance" {
			return fmt.Errorf("Microsoft does not know the sign-in address %s. If you trust it, set AZURE_AUTHORITY_HOST to a host Microsoft "+
				"lists.", strings.TrimSuffix(a.authority, "/"+tenant))
		}
		next, ok := payload["tenant_discovery_endpoint"].(string)
		if !ok {
			return fmt.Errorf("Azure's answer about the tenant, with HTTP status %d, did not say where to sign in.", status)
		}
		endpoint = next
	}
	status, raw, err := send(ctx, http.MethodGet, endpoint, nil, nil, false)
	if err != nil {
		return err
	}
	if status >= 400 && status < 500 {
		return fmt.Errorf("Azure's sign-in service at %s answered with HTTP status %d: %s", endpoint, status, string(raw))
	}
	if status != http.StatusOK {
		return errors.New("Microsoft Entra ID gave an answer that can't be read.")
	}
	var configuration map[string]any
	if err := json.Unmarshal(raw, &configuration); err != nil {
		return err
	}
	tokenEndpoint, _ := configuration["token_endpoint"].(string)
	if tokenEndpoint == "" {
		return errors.New("Azure's sign-in service did not say where to get tokens.")
	}
	a.endpoint = tokenEndpoint
	return nil
}

// Token of the client credentials grant: the one kept while it is fresh, else a new one; a token past its refresh_on is kept when its
// renewal fails
func (a *confidential) token(ctx context.Context, scopes []string) (token, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.discover(ctx); err != nil {
		return token{}, err
	}
	key := strings.Join(scopes, " ")
	now := time.Now().Unix()
	cached, ok := a.tokens[key]
	aging := ok && cached.expiresOn-now >= refreshOffset && cached.refreshOn != 0 && cached.refreshOn < now
	if ok && cached.expiresOn-now >= refreshOffset && !aging {
		return cached, nil
	}
	fresh, err := a.request(ctx, scopes)
	if err != nil && aging {
		return cached, nil
	}
	return fresh, err
}

// Drops the tokens kept, so the next is minted anew
func (a *confidential) forget() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens = map[string]token{}
}

// One client credentials request, which the token service may ask to wait for and answer again
func (a *confidential) request(ctx context.Context, scopes []string) (token, error) {
	form, err := a.proof(ctx, a.endpoint)
	if err != nil {
		return token{}, err
	}
	form.Set("client_id", a.client)
	form.Set("grant_type", "client_credentials")
	form.Set("client_info", "1")
	if scope := strings.Join(scopes, " "); scope != "" {
		form.Set("scope", scope)
	}
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", "client-request-id": uuid.NewString(),
		"x-ms-lib-capability": "retry-after, h429"}
	requested := time.Now().Unix()
	status, raw, err := send(ctx, http.MethodPost, a.endpoint, headers, form, true)
	if err != nil {
		return token{}, err
	}
	if status >= 500 {
		return token{}, errors.New("Microsoft Entra ID gave an answer that can't be read.")
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return token{}, fmt.Errorf("Microsoft Entra ID refused the sign-in: %s", strings.TrimSpace(string(raw)))
	}
	value, ok := result["access_token"].(string)
	if !ok {
		return token{}, fmt.Errorf("Microsoft Entra ID refused the sign-in: %v", firstNonEmpty(result["error_description"], result["error"]))
	}
	expiresIn, ok := config.Whole(result["expires_in"])
	if !ok {
		expiresIn = 600
		if on, has := config.Whole(result["expires_on"]); has {
			expiresIn = on - int(requested)
		}
	}
	fresh := token{value: value, expiresOn: requested + int64(expiresIn)}
	if in, ok := config.Whole(result["refresh_in"]); ok {
		fresh.refreshOn = requested + int64(in)
	}
	a.tokens[strings.Join(scopes, " ")] = fresh
	return fresh, nil
}
