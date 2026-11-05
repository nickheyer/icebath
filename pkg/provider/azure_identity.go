package provider

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	entraAzureCLIClientID      = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"
	entraDefaultAuthorityHost  = "login.microsoftonline.com"
	entraDefaultRefreshOffset  = 300
	entraMinRefreshInterval    = 30
	entraClientAssertionType   = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	entraIMDSDefaultEndpoint   = "http://169.254.169.254/metadata/identity/oauth2/token"
	entraDACTroubleshoot       = "\nTo mitigate this issue, please refer to the troubleshooting guidelines here at https://aka.ms/azsdk/python/identity/defaultazurecredential/troubleshoot."
	entraEnvUnavailable        = "EnvironmentCredential authentication unavailable. Environment variables are not fully configured.\nVisit https://aka.ms/azsdk/python/identity/environmentcredential/troubleshoot to troubleshoot this issue."
	entraIMDSUnavailable       = "ManagedIdentityCredential authentication unavailable, no response from the IMDS endpoint."
	entraInvalidTenant         = "Invalid tenant ID provided. You can locate your tenant ID by following the instructions listed here: https://learn.microsoft.com/partner-center/find-ids-and-domain-names"
	entraAmbientChainRefusal   = "Entra ID auth is refused for this profile: it sets no AZURE_* credential of its own, and under multiplexed profiles the ambient DefaultAzureCredential chain (process env, az CLI caches, host managed identity) mints the LAUNCH profile's identity, which would be sent to this profile's base_url. Set AZURE_TENANT_ID + AZURE_CLIENT_ID + AZURE_CLIENT_SECRET or AZURE_FEDERATED_TOKEN_FILE in this profile's own config; AZURE_CLIENT_ID alone selects the host's user-assigned managed identity."
	entraProbeTimeoutHint      = "DefaultAzureCredential can be slow when the token service is unreachable or when az login state is stale. Try `az login` or set AZURE_CLIENT_ID / AZURE_TENANT_ID / AZURE_CLIENT_SECRET."
	entraBrokerPackageRequired = "InteractiveBrowserBrokerCredential unavailable. The 'azure-identity-broker' package is required to use brokered authentication."
)

var (
	entraNow        = time.Now
	entraHTTPClient = func() *http.Client { return &http.Client{Timeout: 30 * time.Second} }
	entraSleep      = gcpBackoffSleep
	entraTenantRe   = regexp.MustCompile(`^[0-9A-Za-z.-]+$`)
)

// Entra knobs icebath manages, everything else flows through the standard AZURE_ variables
type EntraIdentityConfig struct {
	Scope                     string
	ExcludeInteractiveBrowser bool
}

// NewEntraIdentityConfig normalizes a blank scope to the Foundry inference default
func NewEntraIdentityConfig(scope string, excludeInteractiveBrowser bool) EntraIdentityConfig {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = ScopeAIAzureDefault
	}
	return EntraIdentityConfig{Scope: scope, ExcludeInteractiveBrowser: excludeInteractiveBrowser}
}

// ToDict is the config as the model.entra block stores it
func (c EntraIdentityConfig) ToDict() map[string]any {
	return map[string]any{"scope": c.Scope, "exclude_interactive_browser": c.ExcludeInteractiveBrowser}
}

// EntraIdentityConfigFromDict reads a model.entra block, the scope falling back to defaultScope then the default
func EntraIdentityConfigFromDict(data map[string]any, defaultScope string) EntraIdentityConfig {
	scope := strings.TrimSpace(cfgString(data["scope"]))
	if scope == "" {
		scope = defaultScope
	}
	exclude := true
	if v, ok := data["exclude_interactive_browser"]; ok {
		exclude = pyTruthy(v)
	}
	return NewEntraIdentityConfig(scope, exclude)
}

// Bearer token with its epoch expiry and the epoch after which a refresh is due
type EntraAccessToken struct {
	Token     string
	ExpiresOn int64
	RefreshOn int64
}

// Credential that did not attempt authentication because it lacks required data or state
type entraUnavailableError struct{ Msg string }

func (e *entraUnavailableError) Error() string { return e.Msg }

// Credential that attempted authentication and failed
type entraAuthError struct{ Msg string }

func (e *entraAuthError) Error() string { return e.Msg }

// One azure-identity credential type
type entraCredential interface {
	GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error)
	Name() string
}

// In memory token cache with the msal silent window and the azure-identity refresh throttle
type entraTokenCache struct {
	mu          sync.Mutex
	tokens      map[string]EntraAccessToken
	lastRequest int64
}

func (c *entraTokenCache) get(ctx context.Context, scopes []string, request func(context.Context) (EntraAccessToken, error)) (EntraAccessToken, error) {
	key := strings.Join(scopes, " ")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]EntraAccessToken{}
	}
	now := entraNow().Unix()
	cached, ok := c.tokens[key]
	if !ok || cached.ExpiresOn-now <= entraDefaultRefreshOffset {
		c.lastRequest = now
		tok, err := request(ctx)
		if err != nil {
			return EntraAccessToken{}, err
		}
		c.tokens[key] = tok
		return tok, nil
	}
	if cached.RefreshOn != 0 && now >= cached.RefreshOn && now-c.lastRequest >= entraMinRefreshInterval {
		c.lastRequest = now
		if tok, err := request(ctx); err == nil {
			c.tokens[key] = tok
			return tok, nil
		}
	}
	return cached, nil
}

// Authority host from AZURE_AUTHORITY_HOST as an https origin
func entraAuthority() string {
	a := strings.TrimSpace(os.Getenv("AZURE_AUTHORITY_HOST"))
	if a == "" {
		a = entraDefaultAuthorityHost
	}
	if !strings.Contains(a, "://") {
		a = "https://" + a
	}
	return strings.TrimRight(a, "/")
}

func entraValidateTenant(tenant string) error {
	if !entraTenantRe.MatchString(tenant) {
		return errors.New(entraInvalidTenant)
	}
	return nil
}

// Token from an AAD v2 response, refresh_on from refresh_in or half the lifetime of long tokens
func entraTokenFromResponse(data map[string]any, requestTime int64) (EntraAccessToken, error) {
	token, _ := data["access_token"].(string)
	if token == "" {
		return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Authentication failed: %v", firstNonNil(data["error_description"], data["error"]))}
	}
	num := func(v any) (int64, bool) {
		switch x := v.(type) {
		case float64:
			return int64(x), true
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			return n, err == nil
		}
		return 0, false
	}
	out := EntraAccessToken{Token: token}
	if on, ok := num(data["expires_on"]); ok {
		out.ExpiresOn = on
	} else if in, ok := num(data["expires_in"]); ok {
		out.ExpiresOn = requestTime + in
	} else {
		return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Unexpected response '%v'", data)}
	}
	if in, ok := num(data["refresh_in"]); ok {
		out.RefreshOn = requestTime + in
	} else if out.ExpiresOn-requestTime >= 7200 {
		out.RefreshOn = requestTime + (out.ExpiresOn-requestTime)/2
	}
	return out, nil
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}

// Posts an AAD v2 token request for the tenant and converts the answer
func entraTokenRequest(ctx context.Context, tenant string, form url.Values) (EntraAccessToken, error) {
	endpoint := entraAuthority() + "/" + tenant + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return EntraAccessToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	requestTime := entraNow().Unix()
	resp, err := entraHTTPClient().Do(req)
	if err != nil {
		return EntraAccessToken{}, &entraAuthError{Msg: err.Error()}
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Authentication failed: %s", strings.TrimSpace(string(raw)))}
	}
	return entraTokenFromResponse(data, requestTime)
}

// ---- service principal credentials ----

// Client secret service principal through the client credentials grant
type entraClientSecretCredential struct {
	tenant, clientID, secret string
	cache                    entraTokenCache
}

func newEntraClientSecretCredential(tenant, clientID, secret string) (*entraClientSecretCredential, error) {
	if err := entraValidateTenant(tenant); err != nil {
		return nil, err
	}
	return &entraClientSecretCredential{tenant: tenant, clientID: clientID, secret: secret}, nil
}

func (c *entraClientSecretCredential) Name() string { return "ClientSecretCredential" }

func (c *entraClientSecretCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	return c.cache.get(ctx, scopes, func(ctx context.Context) (EntraAccessToken, error) {
		return entraTokenRequest(ctx, c.tenant, url.Values{"client_id": {c.clientID}, "client_secret": {c.secret},
			"grant_type": {"client_credentials"}, "scope": {strings.Join(scopes, " ")}})
	})
}

// Service principal authenticating with a signed client assertion from a callback
type entraClientAssertionCredential struct {
	name, tenant, clientID string
	assertion              func(ctx context.Context, tokenURL string) (string, error)
	cache                  entraTokenCache
}

func (c *entraClientAssertionCredential) Name() string { return c.name }

func (c *entraClientAssertionCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	return c.cache.get(ctx, scopes, func(ctx context.Context) (EntraAccessToken, error) {
		assertion, err := c.assertion(ctx, entraAuthority()+"/"+c.tenant+"/oauth2/v2.0/token")
		if err != nil {
			return EntraAccessToken{}, err
		}
		return entraTokenRequest(ctx, c.tenant, url.Values{"client_id": {c.clientID}, "client_assertion": {assertion},
			"client_assertion_type": {entraClientAssertionType}, "grant_type": {"client_credentials"}, "scope": {strings.Join(scopes, " ")}})
	})
}

// Certificate service principal signing an RS256 assertion with the x5t thumbprint and optional x5c chain
func newEntraCertificateCredential(tenant, clientID, certPath, password string, sendChain bool) (*entraClientAssertionCredential, error) {
	if err := entraValidateTenant(tenant); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	key, certs, err := entraLoadCertificate(raw, password)
	if err != nil {
		return nil, err
	}
	thumb := sha1.Sum(certs[0].Raw)
	var chain []string
	for _, c := range certs {
		chain = append(chain, base64.StdEncoding.EncodeToString(c.Raw))
	}
	return &entraClientAssertionCredential{name: "CertificateCredential", tenant: tenant, clientID: clientID,
		assertion: func(_ context.Context, tokenURL string) (string, error) {
			now := entraNow().Unix()
			header := map[string]any{"alg": "RS256", "typ": "JWT", "x5t": base64.RawURLEncoding.EncodeToString(thumb[:])}
			if sendChain {
				header["x5c"] = chain
			}
			return entraSignJWT(key, header, map[string]any{"aud": tokenURL, "iss": clientID, "sub": clientID,
				"jti": uuid.NewString(), "nbf": now, "iat": now, "exp": now + 600})
		}}, nil
}

// RS256 compact JWS with an explicit header
func entraSignJWT(key *rsa.PrivateKey, header, payload map[string]any) (string, error) {
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hb) + "." + enc.EncodeToString(pb)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// RSA key and certificate chain from PEM text or a PKCS12 archive, leaf first
func entraLoadCertificate(raw []byte, password string) (*rsa.PrivateKey, []*x509.Certificate, error) {
	var keyAny any
	var certs []*x509.Certificate
	if bytes.Contains(raw, []byte("-----BEGIN")) {
		rest := raw
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			switch {
			case block.Type == "CERTIFICATE":
				c, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil, nil, err
				}
				certs = append(certs, c)
			case strings.HasSuffix(block.Type, "PRIVATE KEY"):
				der := block.Bytes
				if block.Type == "ENCRYPTED PRIVATE KEY" {
					d, err := pkcs12DecryptShrouded(der, password)
					if err != nil {
						return nil, nil, err
					}
					der = d
				}
				k, err := x509.ParsePKCS8PrivateKey(der)
				if err != nil {
					k, err = x509.ParsePKCS1PrivateKey(der)
				}
				if err != nil {
					return nil, nil, err
				}
				keyAny = k
			}
		}
	} else {
		k, cs, err := pkcs12Decode(raw, password)
		if err != nil {
			return nil, nil, err
		}
		keyAny, certs = k, cs
	}
	key, ok := keyAny.(*rsa.PrivateKey)
	if !ok || len(certs) == 0 {
		return nil, nil, errors.New("The certificate must include its private key")
	}
	for i, c := range certs {
		if pub, ok := c.PublicKey.(*rsa.PublicKey); ok && pub.Equal(&key.PublicKey) {
			certs[0], certs[i] = certs[i], certs[0]
			break
		}
	}
	return key, certs, nil
}

// Federated token file assertion re read at most every ten minutes
func newEntraWorkloadIdentityCredential(tenant, clientID, tokenFile string) (*entraClientAssertionCredential, error) {
	switch {
	case tenant == "":
		return nil, errors.New("'tenant_id' is required. Please pass it in or set the AZURE_TENANT_ID environment variable.")
	case clientID == "":
		return nil, errors.New("'client_id' is required. Please pass it in or set the AZURE_CLIENT_ID environment variable.")
	case tokenFile == "":
		return nil, errors.New("'token_file_path' is required. Please pass it in or set the AZURE_FEDERATED_TOKEN_FILE environment variable.")
	}
	if err := entraValidateTenant(tenant); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var jwt string
	var lastRead int64
	return &entraClientAssertionCredential{name: "WorkloadIdentityCredential", tenant: tenant, clientID: clientID,
		assertion: func(context.Context, string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			now := entraNow().Unix()
			if now-lastRead > 600 {
				b, err := os.ReadFile(tokenFile)
				if err != nil {
					return "", err
				}
				jwt, lastRead = string(b), now
			}
			return jwt, nil
		}}, nil
}

// Resource owner password grant for a public client
type entraUsernamePasswordCredential struct {
	tenant, clientID, username, password string
	cache                                entraTokenCache
}

func (c *entraUsernamePasswordCredential) Name() string { return "UsernamePasswordCredential" }

func (c *entraUsernamePasswordCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	return c.cache.get(ctx, scopes, func(ctx context.Context) (EntraAccessToken, error) {
		return entraTokenRequest(ctx, c.tenant, url.Values{"client_id": {c.clientID}, "grant_type": {"password"},
			"username": {c.username}, "password": {c.password}, "scope": {strings.Join(append(append([]string{}, scopes...), "offline_access", "openid", "profile"), " ")}})
	})
}

// Service principal from AZURE_ variables, client secret then certificate then username and password
type entraEnvironmentCredential struct{ inner entraCredential }

func newEntraEnvironmentCredential() (*entraEnvironmentCredential, error) {
	set := func(names ...string) bool {
		for _, n := range names {
			if _, ok := os.LookupEnv(n); !ok {
				return false
			}
		}
		return true
	}
	env := os.Getenv
	var inner entraCredential
	var err error
	switch {
	case set("AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "AZURE_TENANT_ID"):
		inner, err = newEntraClientSecretCredential(env("AZURE_TENANT_ID"), env("AZURE_CLIENT_ID"), env("AZURE_CLIENT_SECRET"))
	case set("AZURE_CLIENT_ID", "AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_TENANT_ID"):
		inner, err = newEntraCertificateCredential(env("AZURE_TENANT_ID"), env("AZURE_CLIENT_ID"), env("AZURE_CLIENT_CERTIFICATE_PATH"),
			env("AZURE_CLIENT_CERTIFICATE_PASSWORD"), env("AZURE_CLIENT_SEND_CERTIFICATE_CHAIN") != "")
	case set("AZURE_CLIENT_ID", "AZURE_USERNAME", "AZURE_PASSWORD"):
		tenant := env("AZURE_TENANT_ID")
		if tenant == "" {
			tenant = "organizations"
		}
		inner = &entraUsernamePasswordCredential{tenant: tenant, clientID: env("AZURE_CLIENT_ID"), username: env("AZURE_USERNAME"), password: env("AZURE_PASSWORD")}
	}
	if err != nil {
		return nil, err
	}
	if inner != nil {
		slog.Default().Info("Environment is configured for " + inner.Name())
	}
	return &entraEnvironmentCredential{inner: inner}, nil
}

func (c *entraEnvironmentCredential) Name() string { return "EnvironmentCredential" }

func (c *entraEnvironmentCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	if c.inner == nil {
		return EntraAccessToken{}, &entraUnavailableError{Msg: entraEnvUnavailable}
	}
	return c.inner.GetToken(ctx, scopes...)
}

// ---- managed identity ----

// Single resource of a managed identity request, the scope without its .default suffix
func entraScopesToResource(scopes []string) (string, error) {
	if len(scopes) != 1 {
		return "", errors.New("This credential requires exactly one scope per token request.")
	}
	return strings.TrimSuffix(scopes[0], "/.default"), nil
}

// Managed identity on the hosting platform the environment variables identify, IMDS otherwise
type entraManagedIdentityCredential struct {
	clientID  string
	kind      string
	workload  entraCredential
	inChain   bool
	cache     entraTokenCache
	mu        sync.Mutex
	imdsAlive bool
}

func newEntraManagedIdentityCredential(clientID string, excludeWorkload, inChain bool) *entraManagedIdentityCredential {
	c := &entraManagedIdentityCredential{clientID: clientID, inChain: inChain}
	env := os.Getenv
	switch {
	case env("IDENTITY_ENDPOINT") != "" && env("IDENTITY_HEADER") != "" && env("IDENTITY_SERVER_THUMBPRINT") != "":
		c.kind = "service_fabric"
	case env("IDENTITY_ENDPOINT") != "" && env("IDENTITY_HEADER") != "":
		c.kind = "app_service"
	case env("IDENTITY_ENDPOINT") != "" && env("IMDS_ENDPOINT") != "":
		c.kind = "azure_arc"
	case env("IDENTITY_ENDPOINT") != "":
		c.kind = ""
	case env("MSI_ENDPOINT") != "" && env("MSI_SECRET") != "":
		c.kind = "azure_ml"
	case env("MSI_ENDPOINT") != "":
		c.kind = "cloud_shell"
	case env("AZURE_TENANT_ID") != "" && env("AZURE_FEDERATED_TOKEN_FILE") != "" && env("AZURE_AUTHORITY_HOST") != "" && !excludeWorkload:
		cid := clientID
		if cid == "" {
			cid = env("AZURE_CLIENT_ID")
		}
		if w, err := newEntraWorkloadIdentityCredential(env("AZURE_TENANT_ID"), cid, env("AZURE_FEDERATED_TOKEN_FILE")); err == nil {
			c.kind, c.workload = "workload_identity", w
		} else {
			c.kind = "imds"
		}
	default:
		c.kind = "imds"
	}
	if c.kind != "" {
		slog.Default().Info("ManagedIdentityCredential will use " + strings.ReplaceAll(c.kind, "_", " "))
	}
	return c
}

func (c *entraManagedIdentityCredential) Name() string { return "ManagedIdentityCredential" }

func (c *entraManagedIdentityCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	if c.kind == "" {
		return EntraAccessToken{}, &entraUnavailableError{Msg: "ManagedIdentityCredential authentication unavailable, no managed identity endpoint found."}
	}
	if c.workload != nil {
		return c.workload.GetToken(ctx, scopes...)
	}
	resource, err := entraScopesToResource(scopes)
	if err != nil {
		return EntraAccessToken{}, err
	}
	return c.cache.get(ctx, scopes, func(ctx context.Context) (EntraAccessToken, error) { return c.request(ctx, resource) })
}

// One managed identity token request in the shape of the detected hosting platform
func (c *entraManagedIdentityCredential) request(ctx context.Context, resource string) (EntraAccessToken, error) {
	env := os.Getenv
	q := url.Values{"resource": {resource}}
	headers := map[string]string{}
	method, endpoint := http.MethodGet, ""
	var body io.Reader
	client := entraHTTPClient()
	switch c.kind {
	case "app_service":
		endpoint, headers["X-IDENTITY-HEADER"] = env("IDENTITY_ENDPOINT"), env("IDENTITY_HEADER")
		q.Set("api-version", "2019-08-01")
		if c.clientID != "" {
			q.Set("client_id", c.clientID)
		}
	case "service_fabric":
		endpoint, headers["Secret"] = env("IDENTITY_ENDPOINT"), env("IDENTITY_HEADER")
		q.Set("api-version", "2019-07-01-preview")
		if c.clientID != "" {
			q.Set("client_id", c.clientID)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	case "azure_ml":
		endpoint, headers["secret"] = env("MSI_ENDPOINT"), env("MSI_SECRET")
		q.Set("api-version", "2017-09-01")
		cid := c.clientID
		if cid == "" {
			cid = env("DEFAULT_IDENTITY_CLIENT_ID")
		}
		q.Set("clientid", cid)
	case "cloud_shell":
		if c.clientID != "" {
			return EntraAccessToken{}, &entraAuthError{Msg: "Cloud Shell does not support user-assigned identities"}
		}
		method, endpoint, headers["Metadata"] = http.MethodPost, env("MSI_ENDPOINT"), "true"
		body, q = strings.NewReader(url.Values{"resource": {resource}}.Encode()), url.Values{}
		headers["Content-Type"] = "application/x-www-form-urlencoded"
	case "azure_arc":
		if c.clientID != "" {
			return EntraAccessToken{}, &entraAuthError{Msg: "Azure Arc doesn't support user-assigned managed identities"}
		}
		endpoint, headers["Metadata"] = env("IDENTITY_ENDPOINT"), "true"
		q.Set("api-version", "2020-06-01")
	default:
		endpoint = entraIMDSDefaultEndpoint
		if host := strings.TrimSpace(env("AZURE_POD_IDENTITY_AUTHORITY_HOST")); host != "" {
			endpoint = strings.TrimRight(host, "/") + "/metadata/identity/oauth2/token"
		}
		headers["Metadata"] = "true"
		q.Set("api-version", "2018-02-01")
		if c.clientID != "" {
			q.Set("client_id", c.clientID)
		}
		return c.imds(ctx, endpoint+"?"+q.Encode(), headers)
	}
	target := endpoint
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	status, data, err := entraJSONRequest(ctx, client, method, target, headers, body)
	if err != nil {
		return EntraAccessToken{}, &entraAuthError{Msg: err.Error()}
	}
	if c.kind == "azure_arc" && status == http.StatusUnauthorized {
		if data, err = c.arcChallenge(ctx, client, target); err != nil {
			return EntraAccessToken{}, err
		}
	} else if status != http.StatusOK {
		return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Unexpected response '%v'", data)}
	}
	return entraTokenFromResponse(data, entraNow().Unix())
}

// Answers the Azure Arc challenge with the local key file the agent named
func (c *entraManagedIdentityCredential) arcChallenge(ctx context.Context, client *http.Client, target string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata", "true")
	resp, err := client.Do(req)
	if err != nil {
		return nil, &entraAuthError{Msg: err.Error()}
	}
	resp.Body.Close()
	header := resp.Header.Get("WWW-Authenticate")
	_, keyPath, ok := strings.Cut(header, "=")
	if !ok {
		return nil, &entraAuthError{Msg: "Did not receive a value from WWW-Authenticate header: " + header}
	}
	keyPath = strings.TrimSpace(keyPath)
	expectedDir := "/var/opt/azcmagent/tokens"
	if runtime.GOOS == "windows" {
		expectedDir = filepath.Join(os.Getenv("ProgramData"), "AzureConnectedMachineAgent", "Tokens")
	}
	if filepath.Dir(keyPath) != expectedDir || filepath.Ext(keyPath) != ".key" {
		return nil, &entraAuthError{Msg: "Unexpected file path from HTTP response: " + keyPath}
	}
	if st, err := os.Stat(keyPath); err != nil || st.Size() > 4096 {
		return nil, &entraAuthError{Msg: "Invalid file: " + keyPath}
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, &entraAuthError{Msg: err.Error()}
	}
	status, data, err := entraJSONRequest(ctx, client, http.MethodGet, target, map[string]string{"Metadata": "true", "Authorization": "Basic " + string(key)}, nil)
	if err != nil {
		return nil, &entraAuthError{Msg: err.Error()}
	}
	if status != http.StatusOK {
		return nil, &entraAuthError{Msg: fmt.Sprintf("Unexpected response '%v'", data)}
	}
	return data, nil
}

// IMDS token with the one second in chain reachability probe and retries over transient statuses
func (c *entraManagedIdentityCredential) imds(ctx context.Context, target string, headers map[string]string) (EntraAccessToken, error) {
	c.mu.Lock()
	alive := c.imdsAlive
	c.mu.Unlock()
	if c.inChain && !alive {
		probe := entraHTTPClient()
		probe.Timeout = time.Second
		if _, _, err := entraJSONRequest(ctx, probe, http.MethodGet, target, headers, nil); err != nil {
			return EntraAccessToken{}, &entraUnavailableError{Msg: entraIMDSUnavailable}
		}
		c.mu.Lock()
		c.imdsAlive = true
		c.mu.Unlock()
	}
	wait := 800 * time.Millisecond
	var status int
	var data map[string]any
	var err error
	for attempt := 0; attempt <= 5; attempt++ {
		if attempt > 0 {
			if err := entraSleep(ctx, wait); err != nil {
				return EntraAccessToken{}, err
			}
			wait *= 2
		}
		status, data, err = entraJSONRequest(ctx, entraHTTPClient(), http.MethodGet, target, headers, nil)
		if err != nil {
			if c.inChain {
				return EntraAccessToken{}, &entraUnavailableError{Msg: entraIMDSUnavailable}
			}
			return EntraAccessToken{}, &entraAuthError{Msg: err.Error()}
		}
		if status != http.StatusNotFound && status != http.StatusGone && status != http.StatusTooManyRequests && status < 500 {
			break
		}
	}
	switch {
	case status == http.StatusOK:
		return entraTokenFromResponse(data, entraNow().Unix())
	case status == http.StatusBadRequest:
		msg := "ManagedIdentityCredential authentication unavailable. "
		if c.clientID != "" {
			msg += "The requested identity has not been assigned to this resource."
		} else {
			msg += "No identity has been assigned to this resource."
		}
		if d := firstNonNil(data["error_description"], data["error"]); d != nil {
			msg += fmt.Sprintf(" Error: %v", d)
		}
		return EntraAccessToken{}, &entraUnavailableError{Msg: msg}
	case status == http.StatusForbidden && strings.Contains(fmt.Sprint(data["error"]), "unreachable"):
		return EntraAccessToken{}, &entraUnavailableError{Msg: fmt.Sprintf("ManagedIdentityCredential authentication unavailable. The request failed due to a gateway error. %v", data)}
	}
	return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Unexpected response '%v'", data)}
}

// JSON request returning the status and decoded object body
func entraJSONRequest(ctx context.Context, client *http.Client, method, target string, headers map[string]string, body io.Reader) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		data = map[string]any{"error": strings.TrimSpace(string(raw))}
	}
	return resp.StatusCode, data, nil
}

// UTF-16LE base64 command text as PowerShell -EncodedCommand expects it
func entraPowerShellEncode(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(units)*2)
	for _, u := range units {
		b = append(b, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

var entraExecCommand = exec.CommandContext

func entraHomeKey(ctx context.Context) string { return home.Key(home.Dir(ctx)) }

// ---- developer tool credentials ----

// Working directory for credential subprocesses that never runs from the caller directory
func entraSafeWorkingDir() string {
	if runtime.GOOS == "windows" {
		if root := os.Getenv("SYSTEMROOT"); root != "" {
			return root
		}
		return `C:\`
	}
	return "/bin"
}

var entraAccessTokenField = regexp.MustCompile(`"accessToken": "(.*?)("|$)`)

func entraSanitize(out string) string { return entraAccessTokenField.ReplaceAllString(out, "****") }

// Runs a credential CLI under its timeout, returning stdout, stderr, the exit code and a start failure
func entraRunCLI(ctx context.Context, timeout time.Duration, name string, args ...string) (string, string, int, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", "", -1, err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := entraExecCommand(runCtx, path, args...)
	cmd.Dir = entraSafeWorkingDir()
	cmd.Env = append(os.Environ(), "AZURE_CORE_NO_COLOR=true")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if runCtx.Err() != nil {
		return stdout.String(), stderr.String(), code, runCtx.Err()
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return stdout.String(), stderr.String(), code, err
	}
	return stdout.String(), stderr.String(), code, nil
}

// Error of the chain or standalone shape, unavailable inside DefaultAzureCredential
func entraCLIFailure(inChain bool, msg string) error {
	if inChain {
		return &entraUnavailableError{Msg: msg}
	}
	return &entraAuthError{Msg: msg}
}

// Azure CLI token through az account get-access-token for the scope resource
type entraAzureCLICredential struct {
	tenant  string
	timeout time.Duration
	inChain bool
}

func (c *entraAzureCLICredential) Name() string { return "AzureCliCredential" }

func (c *entraAzureCLICredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	resource, err := entraScopesToResource(scopes)
	if err != nil {
		return EntraAccessToken{}, err
	}
	args := []string{"account", "get-access-token", "--output", "json", "--resource", resource}
	if c.tenant != "" {
		args = append(args, "--tenant", c.tenant)
	}
	out, stderr, code, err := entraRunCLI(ctx, c.timeout, "az", args...)
	switch {
	case code == -1 && err != nil:
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Azure CLI not found on path"}
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Failed to invoke the Azure CLI"}
	case err != nil:
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Failed to execute 'az'"}
	case code == 127 || strings.HasPrefix(stderr, "'az' is not recognized"):
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Azure CLI not found on path"}
	case code != 0 && (strings.Contains(stderr, "az login") || strings.Contains(stderr, "az account set")):
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Please run 'az login' to set up an account"}
	case code != 0:
		msg := "Failed to invoke Azure CLI"
		if stderr != "" {
			msg = entraSanitize(stderr)
		}
		return EntraAccessToken{}, entraCLIFailure(c.inChain, msg)
	}
	var data map[string]any
	if json.Unmarshal([]byte(out), &data) == nil {
		if token, ok := data["accessToken"].(string); ok {
			if on, ok := data["expires_on"]; ok {
				switch n := on.(type) {
				case float64:
					return EntraAccessToken{Token: token, ExpiresOn: int64(n)}, nil
				case string:
					if v, err := strconv.ParseInt(n, 10, 64); err == nil {
						return EntraAccessToken{Token: token, ExpiresOn: v}, nil
					}
				}
			} else if s, ok := data["expiresOn"].(string); ok {
				if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999", s, time.Local); err == nil {
					return EntraAccessToken{Token: token, ExpiresOn: t.Unix()}, nil
				}
			}
		}
	}
	return EntraAccessToken{}, entraCLIFailure(c.inChain, fmt.Sprintf("Unexpected output from Azure CLI: '%s'. \nTo mitigate this issue, please refer to the troubleshooting guidelines here at https://aka.ms/azsdk/python/identity/azclicredential/troubleshoot.", entraSanitize(out)))
}

const entraAzdNotFound = "Azure Developer CLI could not be found. Please visit https://aka.ms/azure-dev for installation instructions and then, once installed, authenticate to your Azure account using 'azd auth login'."

// Azure Developer CLI token through azd auth token
type entraAzdCredential struct {
	tenant  string
	timeout time.Duration
	inChain bool
}

func (c *entraAzdCredential) Name() string { return "AzureDeveloperCliCredential" }

func (c *entraAzdCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	if len(scopes) == 0 {
		return EntraAccessToken{}, errors.New("Missing scope in request. \nTo mitigate this issue, please refer to the troubleshooting guidelines here at https://aka.ms/azsdk/python/identity/azdevclicredential/troubleshoot.")
	}
	args := []string{"auth", "token", "--output", "json", "--no-prompt"}
	for _, s := range scopes {
		args = append(args, "--scope", s)
	}
	if c.tenant != "" {
		args = append(args, "--tenant-id", c.tenant)
	}
	out, stderr, code, err := entraRunCLI(ctx, c.timeout, "azd", args...)
	switch {
	case code == -1 && err != nil:
		return EntraAccessToken{}, &entraUnavailableError{Msg: entraAzdNotFound}
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Failed to invoke the Azure Developer CLI"}
	case err != nil:
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Failed to execute 'azd'"}
	case code == 127 || strings.HasPrefix(stderr, "'azd' is not recognized"):
		return EntraAccessToken{}, &entraUnavailableError{Msg: entraAzdNotFound}
	case code != 0 && (strings.Contains(stderr, "azd auth login") || strings.Contains(stderr, "not logged in")):
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Please run 'azd auth login' from a command prompt to authenticate before using this credential."}
	case code != 0:
		msg := "Failed to invoke Azure Developer CLI"
		if stderr != "" {
			msg = entraSanitize(stderr)
		}
		return EntraAccessToken{}, entraCLIFailure(c.inChain, msg)
	}
	var data map[string]any
	if json.Unmarshal([]byte(out), &data) == nil {
		token, _ := data["token"].(string)
		exp, _ := data["expiresOn"].(string)
		if len(exp) >= 19 && token != "" {
			if t, err := time.Parse("2006-01-02T15:04:05", exp[:19]); err == nil {
				return EntraAccessToken{Token: token, ExpiresOn: t.Unix()}, nil
			}
		}
	}
	return EntraAccessToken{}, entraCLIFailure(c.inChain, fmt.Sprintf("Unexpected output from Azure Developer CLI: '%s'. \nTo mitigate this issue, please refer to the troubleshooting guidelines here at https://aka.ms/azsdk/python/identity/azdevclicredential/troubleshoot.", entraSanitize(out)))
}

const entraPowerShellScript = `$ErrorActionPreference = 'Stop'
[version]$minimumVersion = '2.2.0'
$m = Import-Module Az.Accounts -MinimumVersion $minimumVersion -PassThru -ErrorAction SilentlyContinue
if (! $m) {
    Write-Output NO_AZ_ACCOUNT_MODULE
    exit
}
$params = @{ 'ResourceUrl' = '%s'; 'WarningAction' = 'Ignore' }
%s
$useSecureString = $m.Version -ge [version]'2.17.0' -and $m.Version -lt [version]'5.0.0'
if ($useSecureString) { $params['AsSecureString'] = $true }
$token = Get-AzAccessToken @params
$plain = $token.Token
if ($plain -is [System.Security.SecureString]) {
    $plain = [System.Net.NetworkCredential]::new('', $plain).Password
}
Write-Output "` + "`" + `nazsdk%%$($plain)%%$($token.ExpiresOn.ToUnixTimeSeconds())` + "`" + `n"
`

// Azure PowerShell token through Get-AzAccessToken in a non interactive pwsh
type entraPowerShellCredential struct {
	tenant  string
	timeout time.Duration
	inChain bool
}

func (c *entraPowerShellCredential) Name() string { return "AzurePowerShellCredential" }

func (c *entraPowerShellCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	resource, err := entraScopesToResource(scopes)
	if err != nil {
		return EntraAccessToken{}, err
	}
	tenantLine := ""
	if c.tenant != "" {
		tenantLine = "$params['TenantId'] = '" + c.tenant + "'"
	}
	encoded := entraPowerShellEncode(fmt.Sprintf(entraPowerShellScript, resource, tenantLine))
	shell := "pwsh"
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("pwsh"); err != nil {
			shell = "powershell"
		}
	}
	out, stderr, code, err := entraRunCLI(ctx, c.timeout, shell, "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded)
	failure := "Failed to invoke PowerShell.\nTo mitigate this issue, please refer to the troubleshooting guidelines here at https://aka.ms/azsdk/python/identity/powershellcredential/troubleshoot."
	switch {
	case code == -1 && err != nil, code == 127, strings.HasPrefix(stderr, "'pwsh' is not recognized"):
		return EntraAccessToken{}, &entraUnavailableError{Msg: "PowerShell is not installed"}
	case err != nil:
		return EntraAccessToken{}, entraCLIFailure(c.inChain, failure)
	case strings.Contains(out, "NO_AZ_ACCOUNT_MODULE"):
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Az.Account module >= 2.2.0 is not installed"}
	case code != 0 && strings.Contains(stderr, "Run Connect-AzAccount to login"):
		return EntraAccessToken{}, &entraUnavailableError{Msg: `Please run "Connect-AzAccount" to set up account`}
	case code != 0 && strings.Contains(stderr, "AuthorizationManager check failed"):
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Execution policy prevented invoking Azure PowerShell"}
	case code != 0:
		return EntraAccessToken{}, entraCLIFailure(c.inChain, failure)
	}
	for _, line := range strings.Split(out, "\n") {
		if parts := strings.Split(strings.TrimSpace(line), "%"); len(parts) == 3 && parts[0] == "azsdk" {
			if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				return EntraAccessToken{Token: parts[1], ExpiresOn: n}, nil
			}
		}
	}
	return EntraAccessToken{}, entraCLIFailure(c.inChain, fmt.Sprintf("Unexpected output from Get-AzAccessToken: '%s'", out))
}

// Developer sign in cached by Microsoft tools in the shared msal token cache file
type entraSharedTokenCacheCredential struct {
	username, tenant string
	cache            entraTokenCache
}

func (c *entraSharedTokenCacheCredential) Name() string { return "SharedTokenCacheCredential" }

func entraSharedCachePath() string {
	h, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), ".IdentityService", "msal.cache")
	}
	return filepath.Join(h, ".IdentityService", "msal.cache")
}

func (c *entraSharedTokenCacheCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	unavailable := func(msg string) (EntraAccessToken, error) {
		return EntraAccessToken{}, &entraUnavailableError{Msg: "SharedTokenCacheCredential authentication unavailable. " + msg}
	}
	raw, _ := os.ReadFile(entraSharedCachePath())
	var store map[string]map[string]map[string]any
	_ = json.Unmarshal(raw, &store)
	var accounts []map[string]any
	for _, a := range store["Account"] {
		if a["authority_type"] != "MSSTS" {
			continue
		}
		realm, _ := a["realm"].(string)
		homeID, _ := a["home_account_id"].(string)
		if c.username != "" && !strings.EqualFold(fmt.Sprint(a["username"]), c.username) {
			continue
		}
		if c.tenant != "" && realm != c.tenant && !strings.HasSuffix(homeID, "."+c.tenant) {
			continue
		}
		accounts = append(accounts, a)
	}
	switch {
	case len(accounts) == 0 && len(store["Account"]) == 0:
		return unavailable("No accounts were found in the cache.")
	case len(accounts) == 0:
		return unavailable("No account matching the specified username and/or tenant was found in the cache.")
	case len(accounts) > 1:
		return unavailable("Multiple accounts were found in the cache. Use username and tenant id to disambiguate.")
	}
	account := accounts[0]
	homeID, realm := fmt.Sprint(account["home_account_id"]), fmt.Sprint(account["realm"])
	return c.cache.get(ctx, scopes, func(ctx context.Context) (EntraAccessToken, error) {
		now := entraNow().Unix()
		for _, at := range store["AccessToken"] {
			target := " " + fmt.Sprint(at["target"]) + " "
			covered := at["home_account_id"] == homeID && at["realm"] == realm
			for _, s := range scopes {
				covered = covered && strings.Contains(strings.ToLower(target), " "+strings.ToLower(s)+" ")
			}
			exp, _ := strconv.ParseInt(fmt.Sprint(at["expires_on"]), 10, 64)
			if covered && exp-now > entraDefaultRefreshOffset {
				return EntraAccessToken{Token: fmt.Sprint(at["secret"]), ExpiresOn: exp}, nil
			}
		}
		for _, rt := range store["RefreshToken"] {
			if rt["home_account_id"] != homeID || (rt["client_id"] != entraAzureCLIClientID && rt["family_id"] != "1") {
				continue
			}
			tok, err := entraTokenRequest(ctx, realm, url.Values{"client_id": {entraAzureCLIClientID}, "grant_type": {"refresh_token"},
				"refresh_token": {fmt.Sprint(rt["secret"])}, "scope": {strings.Join(append(append([]string{}, scopes...), "offline_access", "openid", "profile"), " ")}})
			if err == nil {
				return tok, nil
			}
		}
		return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Token acquisition failed for user '%v'. To fix, re-authenticate through developer tooling supporting Azure single sign on", account["username"])}
	})
}

// VS Code Azure Resources sign in, served only through the azure-identity-broker package
type entraVSCodeCredential struct{}

func (entraVSCodeCredential) Name() string { return "VisualStudioCodeCredential" }

func (entraVSCodeCredential) GetToken(context.Context, ...string) (EntraAccessToken, error) {
	h, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(h, ".azure", "ms-azuretools.vscode-azureresourcegroups", "authRecord.json")); err != nil {
		return EntraAccessToken{}, &entraUnavailableError{Msg: "Visual Studio Code Authentication is not available. Ensure you have have Azure Resources Extension installed in VS Code and have signed into Azure via VS Code."}
	}
	return EntraAccessToken{}, &entraUnavailableError{Msg: "VisualStudioCodeCredential requires the 'azure-identity-broker' package to be installed. You must also ensure you have the Azure Resources extension installed and have signed in to Azure via Visual Studio Code."}
}

// Platform broker sign in, served only through the azure-identity-broker package
type entraBrokerCredential struct{}

func (entraBrokerCredential) Name() string { return "BrokerCredential" }

func (entraBrokerCredential) GetToken(context.Context, ...string) (EntraAccessToken, error) {
	return EntraAccessToken{}, &entraUnavailableError{Msg: entraBrokerPackageRequired}
}

// Browser sign in with the authorization code and PKCE flow on a loopback redirect
type entraInteractiveBrowserCredential struct {
	tenant string
	cache  entraTokenCache
}

func (c *entraInteractiveBrowserCredential) Name() string { return "InteractiveBrowserCredential" }

func (c *entraInteractiveBrowserCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	return c.cache.get(ctx, scopes, func(ctx context.Context) (EntraAccessToken, error) {
		errFn := func(message, code string) *AuthError { return newAuthErr("azure-foundry", message, code, false) }
		handler, result := newLoopbackCallbackHandler("/", "Microsoft Entra ID")
		server, err := bindLoopbackCallbackServer("localhost", 0, handler, errFn, "entra_bind_failed")
		if err != nil {
			return EntraAccessToken{}, &entraAuthError{Msg: err.Error()}
		}
		redirect := fmt.Sprintf("http://localhost:%d", server.Port())
		verifier, challenge := generatePKCE()
		state := uuid.NewString()
		scope := strings.Join(append(append([]string{}, scopes...), "offline_access", "openid", "profile"), " ")
		authURL := entraAuthority() + "/" + c.tenant + "/oauth2/v2.0/authorize?" + url.Values{"client_id": {entraAzureCLIClientID},
			"response_type": {"code"}, "redirect_uri": {redirect}, "scope": {scope}, "state": {state}, "prompt": {"select_account"},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}}.Encode()
		if err := openBrowserURL(authURL); err != nil {
			return EntraAccessToken{}, &entraAuthError{Msg: "Failed to open a browser"}
		}
		values, err := serveLoopbackCallback(server, result, 300*time.Second, errFn, "entra_timeout")
		if err != nil {
			return EntraAccessToken{}, &entraAuthError{Msg: "Timed out after waiting 300 seconds for the user to authenticate"}
		}
		if e := anyString(values["error"]); e != "" {
			return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Authentication failed: %s", firstNonNil(values["error_description"], e))}
		}
		if anyString(values["state"]) != state {
			return EntraAccessToken{}, &entraAuthError{Msg: "Authentication failed: state mismatch"}
		}
		return entraTokenRequest(ctx, c.tenant, url.Values{"client_id": {entraAzureCLIClientID}, "grant_type": {"authorization_code"},
			"code": {anyString(values["code"])}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "scope": {scope}})
	})
}

// ---- DefaultAzureCredential ----

// Chain of credentials in the azure-identity DefaultAzureCredential order, remembering the first that succeeds
type entraDefaultAzureCredential struct {
	creds      []entraCredential
	mu         sync.Mutex
	successful entraCredential
}

var entraTokenCredentialNames = []string{"environmentcredential", "workloadidentitycredential", "managedidentitycredential",
	"sharedtokencachecredential", "visualstudiocodecredential", "azureclicredential", "azurepowershellcredential",
	"azuredeveloperclicredential", "interactivebrowsercredential"}

func newEntraDefaultAzureCredential(excludeInteractiveBrowser bool) (*entraDefaultAzureCredential, error) {
	include := map[string]bool{}
	for _, n := range entraTokenCredentialNames {
		include[n] = true
	}
	include["interactivebrowsercredential"] = !excludeInteractiveBrowser
	include["brokercredential"] = true
	switch sel := strings.ToLower(strings.TrimSpace(os.Getenv("AZURE_TOKEN_CREDENTIALS"))); {
	case sel == "":
	case sel == "dev":
		include["environmentcredential"], include["workloadidentitycredential"], include["managedidentitycredential"] = false, false, false
	case sel == "prod":
		for _, n := range []string{"sharedtokencachecredential", "visualstudiocodecredential", "azureclicredential", "azurepowershellcredential",
			"azuredeveloperclicredential", "interactivebrowsercredential", "brokercredential"} {
			include[n] = false
		}
	case include[sel] || sel == "interactivebrowsercredential":
		for n := range include {
			include[n] = n == sel
		}
	default:
		return nil, fmt.Errorf("Invalid value for AZURE_TOKEN_CREDENTIALS: %s. Valid values are 'dev', 'prod', or one of %v.", sel, entraTokenCredentialNames)
	}
	env := os.Getenv
	var creds []entraCredential
	if include["environmentcredential"] {
		ec, err := newEntraEnvironmentCredential()
		if err != nil {
			return nil, err
		}
		creds = append(creds, ec)
	}
	if include["workloadidentitycredential"] && env("AZURE_AUTHORITY_HOST") != "" && env("AZURE_TENANT_ID") != "" && env("AZURE_FEDERATED_TOKEN_FILE") != "" {
		if w, err := newEntraWorkloadIdentityCredential(env("AZURE_TENANT_ID"), env("AZURE_CLIENT_ID"), env("AZURE_FEDERATED_TOKEN_FILE")); err == nil {
			creds = append(creds, w)
		} else {
			return nil, err
		}
	}
	if include["managedidentitycredential"] {
		creds = append(creds, newEntraManagedIdentityCredential(env("AZURE_CLIENT_ID"), !include["workloadidentitycredential"], true))
	}
	if include["sharedtokencachecredential"] {
		creds = append(creds, &entraSharedTokenCacheCredential{username: env("AZURE_USERNAME"), tenant: env("AZURE_TENANT_ID")})
	}
	if include["visualstudiocodecredential"] {
		creds = append(creds, entraVSCodeCredential{})
	}
	if include["azureclicredential"] {
		creds = append(creds, &entraAzureCLICredential{timeout: 10 * time.Second, inChain: true})
	}
	if include["azurepowershellcredential"] {
		creds = append(creds, &entraPowerShellCredential{timeout: 10 * time.Second, inChain: true})
	}
	if include["azuredeveloperclicredential"] {
		creds = append(creds, &entraAzdCredential{timeout: 10 * time.Second, inChain: true})
	}
	if include["interactivebrowsercredential"] {
		tenant := env("AZURE_TENANT_ID")
		if tenant == "" {
			tenant = "organizations"
		}
		creds = append(creds, &entraInteractiveBrowserCredential{tenant: tenant})
	}
	if include["brokercredential"] {
		creds = append(creds, entraBrokerCredential{})
	}
	return &entraDefaultAzureCredential{creds: creds}, nil
}

func (c *entraDefaultAzureCredential) Name() string { return "DefaultAzureCredential" }

// Token from the remembered credential, else the first credential that does not report itself unavailable
func (c *entraDefaultAzureCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	c.mu.Lock()
	winner := c.successful
	c.mu.Unlock()
	if winner != nil {
		return winner.GetToken(ctx, scopes...)
	}
	var attempts strings.Builder
	for _, cred := range c.creds {
		tok, err := cred.GetToken(ctx, scopes...)
		if err == nil {
			slog.Default().Info(fmt.Sprintf("DefaultAzureCredential acquired a token from %s", cred.Name()))
			c.mu.Lock()
			c.successful = cred
			c.mu.Unlock()
			return tok, nil
		}
		fmt.Fprintf(&attempts, "\n\t%s: %s", cred.Name(), err.Error())
		var unavailable *entraUnavailableError
		if !errors.As(err, &unavailable) {
			break
		}
	}
	msg := "DefaultAzureCredential failed to retrieve a token from the included credentials."
	if attempts.Len() > 0 {
		msg += "\nAttempted credentials:" + attempts.String()
	}
	return EntraAccessToken{}, &entraAuthError{Msg: msg + entraDACTroubleshoot}
}

// ---- credential selection and caches ----

var (
	entraCacheMu        sync.Mutex
	entraDefaultCfg     EntraIdentityConfig
	entraDefaultCred    entraCredential
	entraCredsByHome    = map[entraHomeCredKey]entraCredential{}
	entraBuildDefaultFn = func(cfg EntraIdentityConfig) (entraCredential, error) {
		return newEntraDefaultAzureCredential(cfg.ExcludeInteractiveBrowser)
	}
)

type entraHomeCredKey struct {
	home string
	cfg  EntraIdentityConfig
}

// ResetEntraCredentialCache clears the cached credentials for tests and profile switches
func ResetEntraCredentialCache() {
	entraCacheMu.Lock()
	entraDefaultCred, entraDefaultCfg = nil, EntraIdentityConfig{}
	entraCredsByHome = map[entraHomeCredKey]entraCredential{}
	entraCacheMu.Unlock()
}

// Value of one AZURE_ name from the profile secret mapping installed on the context
func entraScopeRead(ctx context.Context, name string) string {
	s := SecretScopeFrom(ctx)
	if s == nil || s.Mapping == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.Mapping[name])
}

// Routed profile credential built from its own secret scope, refusing the ambient chain under multiplexing
func entraScopedCredential(ctx context.Context, cfg EntraIdentityConfig) (entraCredential, error) {
	tenant, client := entraScopeRead(ctx, "AZURE_TENANT_ID"), entraScopeRead(ctx, "AZURE_CLIENT_ID")
	secret, tokenFile := entraScopeRead(ctx, "AZURE_CLIENT_SECRET"), entraScopeRead(ctx, "AZURE_FEDERATED_TOKEN_FILE")
	switch {
	case tenant != "" && client != "" && secret != "":
		return newEntraClientSecretCredential(tenant, client, secret)
	case tenant != "" && client != "" && tokenFile != "":
		return newEntraWorkloadIdentityCredential(tenant, client, tokenFile)
	case client != "" && tenant == "" && secret == "" && tokenFile == "":
		return newEntraManagedIdentityCredential(client, false, false), nil
	}
	if IsMultiplexActive(ctx) {
		return nil, errors.New(entraAmbientChainRefusal)
	}
	return entraBuildDefaultFn(cfg)
}

// Process default chain when no home override routes the context, else the routed profile credential cached per home and config
func buildEntraCredential(ctx context.Context, cfg EntraIdentityConfig) (entraCredential, error) {
	entraCacheMu.Lock()
	defer entraCacheMu.Unlock()
	if home.Override(ctx) == "" {
		if entraDefaultCred != nil && entraDefaultCfg == cfg {
			return entraDefaultCred, nil
		}
		cred, err := entraBuildDefaultFn(cfg)
		if err != nil {
			return nil, err
		}
		entraDefaultCred, entraDefaultCfg = cred, cfg
		return cred, nil
	}
	key := entraHomeCredKey{home: entraHomeKey(ctx), cfg: cfg}
	if cred, ok := entraCredsByHome[key]; ok {
		return cred, nil
	}
	cred, err := entraScopedCredential(ctx, cfg)
	if err != nil {
		return nil, err
	}
	entraCredsByHome[key] = cred
	return cred, nil
}

// Config object first, then the scope argument, then the default scope
func resolveEntraConfig(cfg *EntraIdentityConfig, scope string, excludeInteractiveBrowser bool) EntraIdentityConfig {
	if cfg != nil {
		return *cfg
	}
	return NewEntraIdentityConfig(scope, excludeInteractiveBrowser)
}

// BuildEntraTokenProvider mints a bearer per call, reusing each credential's token until its refresh point
func BuildEntraTokenProvider(cfg *EntraIdentityConfig, scope string, excludeInteractiveBrowser bool) func(context.Context) (string, error) {
	resolved := resolveEntraConfig(cfg, scope, excludeInteractiveBrowser)
	var mu sync.Mutex
	tokens := map[entraCredential]EntraAccessToken{}
	src := func(ctx context.Context) (string, error) {
		cred, err := buildEntraCredential(ctx, resolved)
		if err != nil {
			return "", err
		}
		mu.Lock()
		defer mu.Unlock()
		now := entraNow().Unix()
		tok, ok := tokens[cred]
		if !ok || (tok.RefreshOn != 0 && tok.RefreshOn <= now) || tok.ExpiresOn-now < 300 {
			if tok, err = cred.GetToken(ctx, resolved.Scope); err != nil {
				return "", err
			}
			tokens[cred] = tok
		}
		return tok.Token, nil
	}
	return TagCredentialSource(src, CredentialSourceEntraID).Token
}

func entraTokenProvider(scope string) (func(context.Context) (string, error), error) {
	return BuildEntraTokenProvider(nil, scope, true), nil
}

// HasAzureIdentityInstalled reports the Entra chain is available, it is compiled into the binary
func HasAzureIdentityInstalled() bool { return true }

// Token or error from one chain walk under a deadline, nil when the deadline passes first
func entraProbeToken(ctx context.Context, cfg EntraIdentityConfig, timeout time.Duration) map[string]any {
	done := make(chan map[string]any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- map[string]any{"error": fmt.Sprint(r)}
			}
		}()
		cred, err := buildEntraCredential(ctx, cfg)
		if err == nil {
			var tok EntraAccessToken
			if tok, err = cred.GetToken(ctx, cfg.Scope); err == nil {
				done <- map[string]any{"token": tok}
				return
			}
		}
		done <- map[string]any{"error": err.Error()}
	}()
	if timeout < 10*time.Millisecond {
		timeout = 10 * time.Millisecond
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-done:
		return r
	case <-t.C:
		return nil
	}
}

// HasAzureIdentityCredentials reports whether the chain mints a token before the deadline, never failing loudly
func HasAzureIdentityCredentials(ctx context.Context, cfg *EntraIdentityConfig, scope string, timeout time.Duration) bool {
	result := entraProbeToken(ctx, resolveEntraConfig(cfg, scope, true), timeout)
	if result == nil {
		slog.Default().Debug(fmt.Sprintf("Entra token service probe timed out after %vs", timeout.Seconds()))
		return false
	}
	if e, ok := result["error"]; ok {
		slog.Default().Debug(fmt.Sprintf("Entra credential probe failed: %v", e))
		return false
	}
	tok, _ := result["token"].(EntraAccessToken)
	return tok.Token != ""
}

// Env driven credential sources in chain order with their presence checks
var entraEnvSourceChecks = []struct {
	label   string
	present func(ctx context.Context) bool
}{
	{"WorkloadIdentityCredential (AZURE_FEDERATED_TOKEN_FILE)", func(ctx context.Context) bool { return entraScopedEnv(ctx, "AZURE_FEDERATED_TOKEN_FILE") != "" }},
	{"EnvironmentCredential (client secret)", func(ctx context.Context) bool {
		return entraScopedEnv(ctx, "AZURE_CLIENT_ID") != "" && entraScopedEnv(ctx, "AZURE_CLIENT_SECRET") != "" && entraScopedEnv(ctx, "AZURE_TENANT_ID") != ""
	}},
	{"ManagedIdentityCredential (IDENTITY_ENDPOINT)", func(context.Context) bool {
		return strings.TrimSpace(os.Getenv("IDENTITY_ENDPOINT")) != "" || strings.TrimSpace(os.Getenv("MSI_ENDPOINT")) != ""
	}},
}

func entraScopedEnv(ctx context.Context, name string) string {
	return strings.TrimSpace(SecretScopeFrom(ctx).GetSecretStr(name, ""))
}

// DescribeActiveEntraCredential reports scope, tenant, env sources and token expiry for doctor and preflight
func DescribeActiveEntraCredential(ctx context.Context, cfg *EntraIdentityConfig, scope string, timeout time.Duration) map[string]any {
	resolved := resolveEntraConfig(cfg, scope, true)
	info := map[string]any{"ok": false, "scope": resolved.Scope}
	if tenant := entraScopedEnv(ctx, "AZURE_TENANT_ID"); tenant != "" {
		info["tenant_id_env"] = tenant
	}
	sources := []string{}
	for _, check := range entraEnvSourceChecks {
		if check.present(ctx) {
			sources = append(sources, check.label)
		}
	}
	info["env_sources"] = sources
	result := entraProbeToken(ctx, resolved, timeout)
	if result == nil {
		info["error"] = fmt.Sprintf("Token probe timed out after %.0fs", timeout.Seconds())
		info["hint"] = entraProbeTimeoutHint
		return info
	}
	if e, ok := result["error"]; ok {
		info["error"] = e
		return info
	}
	tok, ok := result["token"].(EntraAccessToken)
	if !ok {
		info["error"] = "credential chain exhausted"
		return info
	}
	info["ok"] = true
	info["expires_on"] = tok.ExpiresOn
	return info
}

// ---- consumer helpers ----

// IsTokenProvider reports a callable token minter rather than a string key
func IsTokenProvider(value any) bool {
	switch v := value.(type) {
	case func(context.Context) (string, error):
		return v != nil
	case *TaggedTokenSource:
		return v != nil
	}
	return false
}

// MaterializeBearerForHTTP mints one bearer from a token provider or returns a non empty string key
func MaterializeBearerForHTTP(ctx context.Context, value any) (string, error) {
	if IsTokenProvider(value) {
		var token string
		var err error
		switch v := value.(type) {
		case func(context.Context) (string, error):
			token, err = v(ctx)
		case *TaggedTokenSource:
			token, err = v.Token(ctx)
		}
		if err != nil {
			return "", err
		}
		if token == "" {
			return "", errors.New("token provider returned empty value")
		}
		return token, nil
	}
	if s, ok := value.(string); ok && s != "" {
		return s, nil
	}
	return "", errors.New("no usable api_key / token provider")
}

// Round tripper that mints a fresh Entra bearer per request and replaces every preset auth header
type EntraBearerTransport struct {
	Base          http.RoundTripper
	TokenProvider func(context.Context) (string, error)
	SendsAsAPIKey func(token string) bool
}

// NewEntraBearerTransport wraps base, requiring a token provider
func NewEntraBearerTransport(provider func(context.Context) (string, error), sendsAsAPIKey func(string) bool, base http.RoundTripper) (*EntraBearerTransport, error) {
	if provider == nil {
		return nil, errors.New("build_bearer_http_client requires a zero-arg callable token provider")
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &EntraBearerTransport{Base: base, TokenProvider: provider, SendsAsAPIKey: sendsAsAPIKey}, nil
}

func (t *EntraBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	token, err := MaterializeBearerForHTTP(req.Context(), t.TokenProvider)
	for _, h := range []string{"Authorization", "Api-Key", "X-Api-Key"} {
		out.Header.Del(h)
	}
	switch {
	case err != nil:
		slog.Default().Warn(fmt.Sprintf("Bearer hook: Entra ID token provider returned empty (%v) — stripping Authorization headers. "+
			"Azure will respond 401. Run `icebath doctor` or `az login` to recover.", err))
	case t.SendsAsAPIKey != nil && t.SendsAsAPIKey(token):
		out.Header.Set("x-api-key", token)
	default:
		out.Header.Set("Authorization", "Bearer "+token)
	}
	return t.Base.RoundTrip(out)
}

// ---- PKCS12 ----

var (
	oidP12Data          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidP12EncryptedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 6}
	oidP12KeyBag        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 1}
	oidP12ShroudedBag   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 2}
	oidP12CertBag       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 3}
	oidP12X509Cert      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 22, 1}
	oidP12SHA3DES       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 3}
	oidP12SHA128RC2     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 5}
	oidP12SHA40RC2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 6}
	oidPBES2            = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidDESEDE3CBC       = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
	oidAESCBC           = map[string]int{"2.16.840.1.101.3.4.1.2": 16, "2.16.840.1.101.3.4.1.22": 24, "2.16.840.1.101.3.4.1.42": 32}
	p12HMACPRFs         = map[string]func() hash.Hash{"1.2.840.113549.2.7": sha1.New, "1.2.840.113549.2.8": sha256.New224,
		"1.2.840.113549.2.9": sha256.New, "1.2.840.113549.2.10": sha512.New384, "1.2.840.113549.2.11": sha512.New}
	p12Digests = map[string]func() hash.Hash{"1.3.14.3.2.26": sha1.New, "2.16.840.1.101.3.4.2.4": sha256.New224,
		"2.16.840.1.101.3.4.2.1": sha256.New, "2.16.840.1.101.3.4.2.2": sha512.New384, "2.16.840.1.101.3.4.2.3": sha512.New}
)

type p12PFX struct {
	Version  int
	AuthSafe p12ContentInfo
	MacData  p12MacData `asn1:"optional"`
}

type p12ContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"tag:0,explicit,optional"`
}

type p12MacData struct {
	Mac        p12DigestInfo
	MacSalt    []byte
	Iterations int `asn1:"optional,default:1"`
}

type p12DigestInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Digest    []byte
}

type p12EncryptedData struct {
	Version int
	Info    p12EncryptedContentInfo
}

type p12EncryptedContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Algorithm   pkix.AlgorithmIdentifier
	Content     []byte `asn1:"tag:0,optional"`
}

type p12SafeBag struct {
	ID         asn1.ObjectIdentifier
	Value      asn1.RawValue `asn1:"tag:0,explicit"`
	Attributes asn1.RawValue `asn1:"optional"`
}

type p12CertBag struct {
	ID   asn1.ObjectIdentifier
	Data []byte `asn1:"tag:0,explicit"`
}

type p12EncryptedKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Data      []byte
}

type p12PBEParams struct {
	Salt       []byte
	Iterations int
}

type p12PBES2Params struct {
	KDF    pkix.AlgorithmIdentifier
	Scheme pkix.AlgorithmIdentifier
}

type p12PBKDF2Params struct {
	Salt       []byte
	Iterations int
	KeyLength  int                      `asn1:"optional"`
	PRF        pkix.AlgorithmIdentifier `asn1:"optional"`
}

// Rewrites BER indefinite lengths and constructed octet strings as DER
func p12BERToDER(in []byte) ([]byte, error) {
	out, rest, err := p12BERElement(in)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("pkcs12: trailing data")
	}
	return out, nil
}

func p12BERElement(in []byte) ([]byte, []byte, error) {
	if len(in) < 2 {
		return nil, nil, errors.New("pkcs12: truncated element")
	}
	tagLen := 1
	if in[0]&0x1f == 0x1f {
		for tagLen < len(in) && in[tagLen]&0x80 != 0 {
			tagLen++
		}
		tagLen++
	}
	if tagLen >= len(in) {
		return nil, nil, errors.New("pkcs12: truncated tag")
	}
	tag := append([]byte{}, in[:tagLen]...)
	constructed := in[0]&0x20 != 0
	rest := in[tagLen:]
	lb := rest[0]
	rest = rest[1:]
	var children [][]byte
	switch {
	case lb == 0x80:
		if !constructed {
			return nil, nil, errors.New("pkcs12: indefinite primitive")
		}
		for {
			if len(rest) >= 2 && rest[0] == 0 && rest[1] == 0 {
				rest = rest[2:]
				break
			}
			child, r, err := p12BERElement(rest)
			if err != nil {
				return nil, nil, err
			}
			children, rest = append(children, child), r
		}
	default:
		length := int(lb)
		if lb&0x80 != 0 {
			n := int(lb & 0x7f)
			if n > 4 || n > len(rest) {
				return nil, nil, errors.New("pkcs12: bad length")
			}
			length = 0
			for _, b := range rest[:n] {
				length = length<<8 | int(b)
			}
			rest = rest[n:]
		}
		if length > len(rest) {
			return nil, nil, errors.New("pkcs12: truncated content")
		}
		content := rest[:length]
		rest = rest[length:]
		if !constructed {
			return p12DEREncode(tag, content), rest, nil
		}
		for len(content) > 0 {
			child, r, err := p12BERElement(content)
			if err != nil {
				return nil, nil, err
			}
			children, content = append(children, child), r
		}
	}
	if tag[0] == 0x24 {
		var octets []byte
		for _, c := range children {
			var raw asn1.RawValue
			if _, err := asn1.Unmarshal(c, &raw); err != nil {
				return nil, nil, err
			}
			octets = append(octets, raw.Bytes...)
		}
		return p12DEREncode([]byte{0x04}, octets), rest, nil
	}
	return p12DEREncode(tag, bytes.Join(children, nil)), rest, nil
}

func p12DEREncode(tag, content []byte) []byte {
	out := append([]byte{}, tag...)
	n := len(content)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	default:
		var lb []byte
		for v := n; v > 0; v >>= 8 {
			lb = append([]byte{byte(v)}, lb...)
		}
		out = append(append(out, 0x80|byte(len(lb))), lb...)
	}
	return append(out, content...)
}

// RFC 7292 appendix B key derivation over a BMP password
func p12KDF(newHash func() hash.Hash, salt, password []byte, iterations int, id byte, size int) []byte {
	h := newHash()
	u, v := h.Size(), h.BlockSize()
	fill := func(src []byte) []byte {
		if len(src) == 0 {
			return nil
		}
		out := make([]byte, v*((len(src)+v-1)/v))
		for i := range out {
			out[i] = src[i%len(src)]
		}
		return out
	}
	D := bytes.Repeat([]byte{id}, v)
	I := append(fill(salt), fill(password)...)
	var A []byte
	for len(A) < size {
		h.Reset()
		h.Write(D)
		h.Write(I)
		Ai := h.Sum(nil)
		for i := 1; i < iterations; i++ {
			h.Reset()
			h.Write(Ai)
			Ai = h.Sum(nil)
		}
		A = append(A, Ai...)
		if len(A) >= size {
			break
		}
		B := fill(Ai)
		for j := 0; j < len(I); j += v {
			carry := 1
			for k := v - 1; k >= 0; k-- {
				sum := int(I[j+k]) + int(B[k]) + carry
				I[j+k], carry = byte(sum), sum>>8
			}
		}
		_ = u
	}
	return A[:size]
}

// Password as a null terminated big endian BMPString
func p12BMPPassword(password string) []byte {
	units := utf16.Encode([]rune(password))
	out := make([]byte, 0, len(units)*2+2)
	for _, u := range units {
		out = append(out, byte(u>>8), byte(u))
	}
	return append(out, 0, 0)
}

// Decrypts content under a PKCS12 PBE or PBES2 algorithm and strips the block padding
func p12Decrypt(alg pkix.AlgorithmIdentifier, data []byte, password string) ([]byte, error) {
	var block cipher.Block
	var iv []byte
	var err error
	switch {
	case alg.Algorithm.Equal(oidP12SHA3DES), alg.Algorithm.Equal(oidP12SHA40RC2), alg.Algorithm.Equal(oidP12SHA128RC2):
		var params p12PBEParams
		if _, err := asn1.Unmarshal(alg.Parameters.FullBytes, &params); err != nil {
			return nil, err
		}
		pw := p12BMPPassword(password)
		iv = p12KDF(sha1.New, params.Salt, pw, params.Iterations, 2, 8)
		switch {
		case alg.Algorithm.Equal(oidP12SHA3DES):
			block, err = des.NewTripleDESCipher(p12KDF(sha1.New, params.Salt, pw, params.Iterations, 1, 24))
		case alg.Algorithm.Equal(oidP12SHA40RC2):
			block = newRC2Cipher(p12KDF(sha1.New, params.Salt, pw, params.Iterations, 1, 5), 40)
		default:
			block = newRC2Cipher(p12KDF(sha1.New, params.Salt, pw, params.Iterations, 1, 16), 128)
		}
	case alg.Algorithm.Equal(oidPBES2):
		var params p12PBES2Params
		if _, err := asn1.Unmarshal(alg.Parameters.FullBytes, &params); err != nil {
			return nil, err
		}
		if !params.KDF.Algorithm.Equal(oidPBKDF2) {
			return nil, fmt.Errorf("pkcs12: unsupported key derivation %v", params.KDF.Algorithm)
		}
		var kdf p12PBKDF2Params
		if _, err := asn1.Unmarshal(params.KDF.Parameters.FullBytes, &kdf); err != nil {
			return nil, err
		}
		prf := sha1.New
		if len(kdf.PRF.Algorithm) > 0 {
			if prf = p12HMACPRFs[kdf.PRF.Algorithm.String()]; prf == nil {
				return nil, fmt.Errorf("pkcs12: unsupported PRF %v", kdf.PRF.Algorithm)
			}
		}
		keyLen, isAES := oidAESCBC[params.Scheme.Algorithm.String()]
		if !isAES && !params.Scheme.Algorithm.Equal(oidDESEDE3CBC) {
			return nil, fmt.Errorf("pkcs12: unsupported cipher %v", params.Scheme.Algorithm)
		}
		if !isAES {
			keyLen = 24
		}
		if _, err := asn1.Unmarshal(params.Scheme.Parameters.FullBytes, &iv); err != nil {
			return nil, err
		}
		key, err := pbkdf2.Key(prf, password, kdf.Salt, kdf.Iterations, keyLen)
		if err != nil {
			return nil, err
		}
		if isAES {
			block, err = aes.NewCipher(key)
		} else {
			block, err = des.NewTripleDESCipher(key)
		}
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("pkcs12: unsupported algorithm %v", alg.Algorithm)
	}
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if len(data) == 0 || len(data)%bs != 0 || len(iv) != bs {
		return nil, errors.New("pkcs12: invalid ciphertext length")
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > bs || pad > len(out) || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("Invalid password or PKCS12 data")
	}
	return out[:len(out)-pad], nil
}

// PKCS8 key DER from an EncryptedPrivateKeyInfo
func pkcs12DecryptShrouded(der []byte, password string) ([]byte, error) {
	var info p12EncryptedKeyInfo
	if _, err := asn1.Unmarshal(der, &info); err != nil {
		return nil, err
	}
	return p12Decrypt(info.Algorithm, info.Data, password)
}

// Private key and certificates of a PFX archive after its MAC check
func pkcs12Decode(raw []byte, password string) (any, []*x509.Certificate, error) {
	der, err := p12BERToDER(raw)
	if err != nil {
		return nil, nil, err
	}
	var pfx p12PFX
	if _, err := asn1.Unmarshal(der, &pfx); err != nil {
		return nil, nil, err
	}
	if !pfx.AuthSafe.ContentType.Equal(oidP12Data) {
		return nil, nil, errors.New("pkcs12: only password integrity mode is supported")
	}
	var authSafe []byte
	if _, err := asn1.Unmarshal(pfx.AuthSafe.Content.Bytes, &authSafe); err != nil {
		return nil, nil, err
	}
	if len(pfx.MacData.Mac.Algorithm.Algorithm) > 0 {
		newHash := p12Digests[pfx.MacData.Mac.Algorithm.Algorithm.String()]
		if newHash == nil {
			return nil, nil, fmt.Errorf("pkcs12: unsupported MAC digest %v", pfx.MacData.Mac.Algorithm.Algorithm)
		}
		ok := false
		for _, pw := range [][]byte{p12BMPPassword(password), nil} {
			key := p12KDF(newHash, pfx.MacData.MacSalt, pw, pfx.MacData.Iterations, 3, newHash().Size())
			mac := hmac.New(newHash, key)
			mac.Write(authSafe)
			if hmac.Equal(mac.Sum(nil), pfx.MacData.Mac.Digest) {
				ok = true
				break
			}
			if password != "" {
				break
			}
		}
		if !ok {
			return nil, nil, errors.New("Invalid password or PKCS12 data")
		}
	}
	var contents []p12ContentInfo
	if _, err := asn1.Unmarshal(authSafe, &contents); err != nil {
		return nil, nil, err
	}
	var key any
	var certs []*x509.Certificate
	for _, ci := range contents {
		var safe []byte
		switch {
		case ci.ContentType.Equal(oidP12Data):
			if _, err := asn1.Unmarshal(ci.Content.Bytes, &safe); err != nil {
				return nil, nil, err
			}
		case ci.ContentType.Equal(oidP12EncryptedData):
			var ed p12EncryptedData
			if _, err := asn1.Unmarshal(ci.Content.Bytes, &ed); err != nil {
				return nil, nil, err
			}
			if safe, err = p12Decrypt(ed.Info.Algorithm, ed.Info.Content, password); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, fmt.Errorf("pkcs12: unsupported content type %v", ci.ContentType)
		}
		var bags []p12SafeBag
		if _, err := asn1.Unmarshal(safe, &bags); err != nil {
			return nil, nil, err
		}
		for _, bag := range bags {
			switch {
			case bag.ID.Equal(oidP12CertBag):
				var cb p12CertBag
				if _, err := asn1.Unmarshal(bag.Value.Bytes, &cb); err != nil {
					return nil, nil, err
				}
				if !cb.ID.Equal(oidP12X509Cert) {
					continue
				}
				c, err := x509.ParseCertificate(cb.Data)
				if err != nil {
					return nil, nil, err
				}
				certs = append(certs, c)
			case bag.ID.Equal(oidP12KeyBag), bag.ID.Equal(oidP12ShroudedBag):
				pk := bag.Value.Bytes
				if bag.ID.Equal(oidP12ShroudedBag) {
					if pk, err = pkcs12DecryptShrouded(pk, password); err != nil {
						return nil, nil, err
					}
				}
				if key, err = x509.ParsePKCS8PrivateKey(pk); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return key, certs, nil
}

// RFC 2268 PITABLE
var rc2PiTable = [256]byte{
	0xd9, 0x78, 0xf9, 0xc4, 0x19, 0xdd, 0xb5, 0xed, 0x28, 0xe9, 0xfd, 0x79, 0x4a, 0xa0, 0xd8, 0x9d,
	0xc6, 0x7e, 0x37, 0x83, 0x2b, 0x76, 0x53, 0x8e, 0x62, 0x4c, 0x64, 0x88, 0x44, 0x8b, 0xfb, 0xa2,
	0x17, 0x9a, 0x59, 0xf5, 0x87, 0xb3, 0x4f, 0x13, 0x61, 0x45, 0x6d, 0x8d, 0x09, 0x81, 0x7d, 0x32,
	0xbd, 0x8f, 0x40, 0xeb, 0x86, 0xb7, 0x7b, 0x0b, 0xf0, 0x95, 0x21, 0x22, 0x5c, 0x6b, 0x4e, 0x82,
	0x54, 0xd6, 0x65, 0x93, 0xce, 0x60, 0xb2, 0x1c, 0x73, 0x56, 0xc0, 0x14, 0xa7, 0x8c, 0xf1, 0xdc,
	0x12, 0x75, 0xca, 0x1f, 0x3b, 0xbe, 0xe4, 0xd1, 0x42, 0x3d, 0xd4, 0x30, 0xa3, 0x3c, 0xb6, 0x26,
	0x6f, 0xbf, 0x0e, 0xda, 0x46, 0x69, 0x07, 0x57, 0x27, 0xf2, 0x1d, 0x9b, 0xbc, 0x94, 0x43, 0x03,
	0xf8, 0x11, 0xc7, 0xf6, 0x90, 0xef, 0x3e, 0xe7, 0x06, 0xc3, 0xd5, 0x2f, 0xc8, 0x66, 0x1e, 0xd7,
	0x08, 0xe8, 0xea, 0xde, 0x80, 0x52, 0xee, 0xf7, 0x84, 0xaa, 0x72, 0xac, 0x35, 0x4d, 0x6a, 0x2a,
	0x96, 0x1a, 0xd2, 0x71, 0x5a, 0x15, 0x49, 0x74, 0x4b, 0x9f, 0xd0, 0x5e, 0x04, 0x18, 0xa4, 0xec,
	0xc2, 0xe0, 0x41, 0x6e, 0x0f, 0x51, 0xcb, 0xcc, 0x24, 0x91, 0xaf, 0x50, 0xa1, 0xf4, 0x70, 0x39,
	0x99, 0x7c, 0x3a, 0x85, 0x23, 0xb8, 0xb4, 0x7a, 0xfc, 0x02, 0x36, 0x5b, 0x25, 0x55, 0x97, 0x31,
	0x2d, 0x5d, 0xfa, 0x98, 0xe3, 0x8a, 0x92, 0xae, 0x05, 0xdf, 0x29, 0x10, 0x67, 0x6c, 0xba, 0xc9,
	0xd3, 0x00, 0xe6, 0xcf, 0xe1, 0x9e, 0xa8, 0x2c, 0x63, 0x16, 0x01, 0x3f, 0x58, 0xe2, 0x89, 0xa9,
	0x0d, 0x38, 0x34, 0x1b, 0xab, 0x33, 0xff, 0xb0, 0xbb, 0x48, 0x0c, 0x5f, 0xb9, 0xb1, 0xcd, 0x2e,
	0xc5, 0xf3, 0xdb, 0x47, 0xe5, 0xa5, 0x9c, 0x77, 0x0a, 0xa6, 0x20, 0x68, 0xfe, 0x7f, 0xc1, 0xad,
}

// RC2 block cipher with an effective key length in bits, as legacy PKCS12 archives use
type rc2Cipher struct{ k [64]uint16 }

func newRC2Cipher(key []byte, effectiveBits int) *rc2Cipher {
	var L [128]byte
	copy(L[:], key)
	T := len(key)
	for i := T; i < 128; i++ {
		L[i] = rc2PiTable[L[i-1]+L[i-T]]
	}
	T8 := (effectiveBits + 7) / 8
	TM := byte(255 % (int(1) << uint(8+effectiveBits-8*T8)))
	L[128-T8] = rc2PiTable[L[128-T8]&TM]
	for i := 127 - T8; i >= 0; i-- {
		L[i] = rc2PiTable[L[i+1]^L[i+T8]]
	}
	c := &rc2Cipher{}
	for i := range c.k {
		c.k[i] = uint16(L[2*i]) | uint16(L[2*i+1])<<8
	}
	return c
}

func (c *rc2Cipher) BlockSize() int { return 8 }

func (c *rc2Cipher) Encrypt(dst, src []byte) {
	r := [4]uint16{uint16(src[0]) | uint16(src[1])<<8, uint16(src[2]) | uint16(src[3])<<8, uint16(src[4]) | uint16(src[5])<<8, uint16(src[6]) | uint16(src[7])<<8}
	shifts := [4]uint{1, 2, 3, 5}
	j := 0
	mix := func() {
		for i := 0; i < 4; i++ {
			r[i] += c.k[j] + (r[(i+3)%4] & r[(i+2)%4]) + (^r[(i+3)%4] & r[(i+1)%4])
			j++
			r[i] = r[i]<<shifts[i] | r[i]>>(16-shifts[i])
		}
	}
	mash := func() {
		for i := 0; i < 4; i++ {
			r[i] += c.k[r[(i+3)%4]&63]
		}
	}
	for _, rounds := range []int{5, 6, 5} {
		for n := 0; n < rounds; n++ {
			mix()
		}
		if j < 64 {
			mash()
		}
	}
	for i, w := range r {
		dst[2*i], dst[2*i+1] = byte(w), byte(w>>8)
	}
}

func (c *rc2Cipher) Decrypt(dst, src []byte) {
	r := [4]uint16{uint16(src[0]) | uint16(src[1])<<8, uint16(src[2]) | uint16(src[3])<<8, uint16(src[4]) | uint16(src[5])<<8, uint16(src[6]) | uint16(src[7])<<8}
	shifts := [4]uint{1, 2, 3, 5}
	j := 63
	unmix := func() {
		for i := 3; i >= 0; i-- {
			r[i] = r[i]>>shifts[i] | r[i]<<(16-shifts[i])
			r[i] -= c.k[j] + (r[(i+3)%4] & r[(i+2)%4]) + (^r[(i+3)%4] & r[(i+1)%4])
			j--
		}
	}
	unmash := func() {
		for i := 3; i >= 0; i-- {
			r[i] -= c.k[r[(i+3)%4]&63]
		}
	}
	for _, rounds := range []int{5, 6, 5} {
		for n := 0; n < rounds; n++ {
			unmix()
		}
		if j >= 0 {
			unmash()
		}
	}
	for i, w := range r {
		dst[2*i], dst[2*i+1] = byte(w), byte(w>>8)
	}
}
