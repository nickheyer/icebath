package credentials

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/credentials"
)

const (
	// Client of the Azure CLI, which the browser and the shared cache sign in as
	cliClient        = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"
	defaultAuthority = "login.microsoftonline.com"
	// A token this close to its expiry is minted again, and a renewal this soon after the last isn't tried
	refreshOffset   = 300
	minRefresh      = 30
	assertionType   = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	imdsEndpoint    = "http://169.254.169.254/metadata/identity/oauth2/token"
	troubleshoot    = "\nMicrosoft's guide to fixing this: https://aka.ms/azsdk/python/identity/defaultazurecredential/troubleshoot"
	envUnavailable  = "The AZURE_ variables are not all set. Microsoft's guide: https://aka.ms/azsdk/python/identity/environmentcredential/troubleshoot"
	imdsUnavailable = "No managed identity answered, because the instance metadata service did not respond."
	invalidTenant   = "That tenant ID can't be right. Find yours as https://learn.microsoft.com/partner-center/find-ids-and-domain-names shows."
	azdNotFound     = "The Azure Developer CLI is not installed. Install it from https://aka.ms/azure-dev, then sign in with `azd auth login`."
	// Default scope of Azure AI Foundry
	DefaultScope = "https://ai.azure.com/.default"
	probeHint    = "Azure's sign-in is slow when its token service can't be reached or your az login is old. Run `az login`, or set AZURE_CLIENT_ID, AZURE_TENANT_ID and AZURE_CLIENT_SECRET."
)

var tenantName = regexp.MustCompile(`^[0-9A-Za-z.-]+$`)

// Token Entra minted, when it expires and when it should be renewed, in Unix seconds
type token struct {
	value     string
	expiresOn int64
	refreshOn int64
}

// Source of Entra tokens of one kind
type entra interface {
	token(ctx context.Context, scopes []string) (token, error)
	name() string
}

// Source that keeps the tokens it got, which a forced renewal drops
type forgetter interface{ forget() }

// A member of the chain that doesn't apply here, which lets the chain go on to the next
type unavailable struct{ message string }

func (e *unavailable) Error() string { return e.message }

// Tokens kept for each set of scopes, renewed as MSAL does: once near expiry, and early after refresh_on at most every 30 seconds
type cache struct {
	mu     sync.Mutex
	tokens map[string]token
	last   int64
}

func (c *cache) get(ctx context.Context, scopes []string, request func(ctx context.Context) (token, error)) (token, error) {
	key := strings.Join(scopes, " ")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]token{}
	}
	now := time.Now().Unix()
	cached, ok := c.tokens[key]
	if !ok || cached.expiresOn-now <= refreshOffset {
		c.last = now
		fresh, err := request(ctx)
		if err != nil {
			return token{}, err
		}
		c.tokens[key] = fresh
		return fresh, nil
	}
	if cached.refreshOn != 0 && now >= cached.refreshOn && now-c.last >= minRefresh {
		c.last = now
		if fresh, err := request(ctx); err == nil {
			c.tokens[key] = fresh
			return fresh, nil
		}
	}
	return cached, nil
}

// Drops the tokens kept, so the next is minted anew
func (c *cache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = nil
}

// Variables the chain reads, the profile's environment
type environment func(name string) (string, bool)

func (e environment) get(name string) string {
	v, _ := e(name)
	return v
}

func (e environment) has(names ...string) bool {
	for _, name := range names {
		if _, ok := e(name); !ok {
			return false
		}
	}
	return true
}

func (e environment) authority() string {
	host := strings.TrimSpace(e.get("AZURE_AUTHORITY_HOST"))
	if host == "" {
		host = defaultAuthority
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	return strings.TrimRight(host, "/")
}

func validTenant(tenant string) error {
	if !tenantName.MatchString(tenant) {
		return errors.New(invalidTenant)
	}
	return nil
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Token of an AAD v2 answer, refresh_on from refresh_in, else half the lifetime of a token that lives two hours or more
func parse(data map[string]any, requested int64) (token, error) {
	value, _ := data["access_token"].(string)
	if value == "" {
		return token{}, fmt.Errorf("Microsoft Entra ID refused the sign-in: %v", firstNonEmpty(data["error_description"], data["error"]))
	}
	number := func(v any) (int64, bool) {
		switch x := v.(type) {
		case float64:
			return int64(x), true
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			return n, err == nil
		}
		return 0, false
	}
	out := token{value: value}
	if on, ok := number(data["expires_on"]); ok {
		out.expiresOn = on
	} else if in, ok := number(data["expires_in"]); ok {
		out.expiresOn = requested + in
	} else {
		return token{}, fmt.Errorf("Microsoft Entra ID gave an answer without a token: %v", data)
	}
	if in, ok := number(data["refresh_in"]); ok {
		out.refreshOn = requested + in
	} else if out.expiresOn-requested >= 7200 {
		out.refreshOn = requested + (out.expiresOn-requested)/2
	}
	return out, nil
}

func firstNonEmpty(values ...any) any {
	for _, v := range values {
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}

// Posts an AAD v2 token request of the tenant
func request(ctx context.Context, env environment, tenant string, form url.Values) (token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.authority()+"/"+tenant+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	requested := time.Now().Unix()
	resp, err := httpClient.Do(req)
	if err != nil {
		return token{}, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return token{}, fmt.Errorf("Microsoft Entra ID refused the sign-in: %s", strings.TrimSpace(string(raw)))
	}
	return parse(data, requested)
}

// ---- service principals ----

// Service principal proving itself with a client secret or a signed assertion: through a confidential client that discovers the token
// endpoint, else straight to the tenant's token endpoint
type principal struct {
	label, tenant, client string
	env                   environment
	// Fields that prove the client, the assertion signed for the token endpoint
	proof func(ctx context.Context, endpoint string) (url.Values, error)
	app   *confidential
	cache cache
}

func (p *principal) name() string { return p.label }

func (p *principal) token(ctx context.Context, scopes []string) (token, error) {
	if p.app != nil {
		return p.app.token(ctx, scopes)
	}
	return p.cache.get(ctx, scopes, func(ctx context.Context) (token, error) {
		form, err := p.proof(ctx, p.env.authority()+"/"+p.tenant+"/oauth2/v2.0/token")
		if err != nil {
			return token{}, err
		}
		form.Set("client_id", p.client)
		form.Set("grant_type", "client_credentials")
		form.Set("scope", strings.Join(scopes, " "))
		return request(ctx, p.env, p.tenant, form)
	})
}

func (p *principal) forget() {
	if p.app != nil {
		p.app.forget()
	}
	p.cache.forget()
}

func secretPrincipal(env environment, tenant, client, secret string) (*principal, error) {
	if err := validTenant(tenant); err != nil {
		return nil, err
	}
	proof := func(context.Context, string) (url.Values, error) { return url.Values{"client_secret": {secret}}, nil }
	app, err := newConfidential(env, tenant, client, proof)
	if err != nil {
		return nil, err
	}
	return &principal{label: "ClientSecretCredential", tenant: tenant, client: client, env: env, proof: proof, app: app}, nil
}

// Service principal signing an RS256 assertion with its certificate's thumbprint, and its chain when asked
func certificatePrincipal(env environment, tenant, client, path, password string, sendChain bool) (*principal, error) {
	if err := validTenant(tenant); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("The certificate file %s can't be read: %w.", path, err)
	}
	key, certs, err := certificate(raw, password)
	if err != nil {
		if strings.HasPrefix(err.Error(), "The certificate") {
			return nil, err
		}
		return nil, fmt.Errorf("The certificate file %s can't be used, because its password is wrong or the file is damaged. The detail: %w.", path, err)
	}
	thumb := sha1.Sum(certs[0].Raw)
	var chain []string
	for _, c := range certs {
		chain = append(chain, base64.StdEncoding.EncodeToString(c.Raw))
	}
	proof := func(_ context.Context, endpoint string) (url.Values, error) {
		now := time.Now().Unix()
		header := map[string]any{"alg": "RS256", "typ": "JWT", "x5t": base64.RawURLEncoding.EncodeToString(thumb[:])}
		if sendChain {
			header["x5c"] = chain
		}
		signed, err := sign(key, header, map[string]any{"aud": endpoint, "iss": client, "sub": client, "jti": uuid.NewString(), "nbf": now, "iat": now, "exp": now + 600})
		if err != nil {
			return nil, err
		}
		return url.Values{"client_assertion": {signed}, "client_assertion_type": {assertionType}}, nil
	}
	app, err := newConfidential(env, tenant, client, proof)
	if err != nil {
		return nil, err
	}
	return &principal{label: "CertificateCredential", tenant: tenant, client: client, env: env, proof: proof, app: app}, nil
}

// RS256 compact JWS with the header given
func sign(key *rsa.PrivateKey, header, claims map[string]any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// RSA key and certificate chain of PEM text or a PFX archive, the key's certificate first
func certificate(raw []byte, password string) (*rsa.PrivateKey, []*x509.Certificate, error) {
	var key any
	var certs []*x509.Certificate
	if bytes.Contains(raw, []byte("-----BEGIN")) {
		for rest := raw; ; {
			var block *pem.Block
			if block, rest = pem.Decode(rest); block == nil {
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
					var err error
					if der, err = unshroud(der, password); err != nil {
						return nil, nil, err
					}
				}
				k, err := x509.ParsePKCS8PrivateKey(der)
				if err != nil {
					k, err = x509.ParsePKCS1PrivateKey(der)
				}
				if err != nil {
					return nil, nil, err
				}
				key = k
			}
		}
	} else {
		var err error
		if key, certs, err = pkcs12(raw, password); err != nil {
			return nil, nil, err
		}
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok || len(certs) == 0 {
		return nil, nil, errors.New("The certificate file must hold its private key too.")
	}
	for i, c := range certs {
		if public, ok := c.PublicKey.(*rsa.PublicKey); ok && public.Equal(&rsaKey.PublicKey) {
			certs[0], certs[i] = certs[i], certs[0]
			break
		}
	}
	return rsaKey, certs, nil
}

// Federated token file as the assertion, read again at most every ten minutes
func workloadIdentity(env environment, tenant, client, file string) (*principal, error) {
	switch {
	case tenant == "":
		return nil, errors.New("A tenant ID is needed. Give one, or set AZURE_TENANT_ID.")
	case client == "":
		return nil, errors.New("A client ID is needed. Give one, or set AZURE_CLIENT_ID.")
	case file == "":
		return nil, errors.New("A federated token file is needed. Set AZURE_FEDERATED_TOKEN_FILE.")
	}
	if err := validTenant(tenant); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var jwt string
	var read int64
	return &principal{label: "WorkloadIdentityCredential", tenant: tenant, client: client, env: env, proof: func(context.Context, string) (url.Values, error) {
		mu.Lock()
		defer mu.Unlock()
		if now := time.Now().Unix(); now-read > 600 {
			data, err := os.ReadFile(file)
			if err != nil {
				return nil, err
			}
			jwt, read = string(data), now
		}
		return url.Values{"client_assertion": {jwt}, "client_assertion_type": {assertionType}}, nil
	}}, nil
}

// Resource owner password grant of a public client
type password struct {
	tenant, client, username, secret string
	env                              environment
	cache                            cache
}

func (p *password) name() string { return "UsernamePasswordCredential" }

func (p *password) forget() { p.cache.forget() }

func (p *password) token(ctx context.Context, scopes []string) (token, error) {
	return p.cache.get(ctx, scopes, func(ctx context.Context) (token, error) {
		return request(ctx, p.env, p.tenant, url.Values{"client_id": {p.client}, "grant_type": {"password"}, "username": {p.username}, "password": {p.secret},
			"scope": {strings.Join(append(append([]string{}, scopes...), "offline_access", "openid", "profile"), " ")}})
	})
}

// Service principal of the AZURE_ variables: a client secret, else a certificate, else a user name and password
type fromEnvironment struct{ inner entra }

func environmentCredential(env environment) (*fromEnvironment, error) {
	var inner entra
	var err error
	switch {
	case env.has("AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "AZURE_TENANT_ID"):
		inner, err = secretPrincipal(env, env.get("AZURE_TENANT_ID"), env.get("AZURE_CLIENT_ID"), env.get("AZURE_CLIENT_SECRET"))
	case env.has("AZURE_CLIENT_ID", "AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_TENANT_ID"):
		inner, err = certificatePrincipal(env, env.get("AZURE_TENANT_ID"), env.get("AZURE_CLIENT_ID"), env.get("AZURE_CLIENT_CERTIFICATE_PATH"),
			env.get("AZURE_CLIENT_CERTIFICATE_PASSWORD"), env.get("AZURE_CLIENT_SEND_CERTIFICATE_CHAIN") != "")
	case env.has("AZURE_CLIENT_ID", "AZURE_USERNAME", "AZURE_PASSWORD"):
		inner = &password{tenant: cmp.Or(env.get("AZURE_TENANT_ID"), "organizations"), client: env.get("AZURE_CLIENT_ID"), username: env.get("AZURE_USERNAME"),
			secret: env.get("AZURE_PASSWORD"), env: env}
	}
	if err != nil {
		return nil, err
	}
	if inner != nil {
		slog.Info("Environment is configured for " + inner.name())
	}
	return &fromEnvironment{inner: inner}, nil
}

func (c *fromEnvironment) name() string { return "EnvironmentCredential" }

func (c *fromEnvironment) forget() {
	if f, ok := c.inner.(forgetter); ok {
		f.forget()
	}
}

func (c *fromEnvironment) token(ctx context.Context, scopes []string) (token, error) {
	if c.inner == nil {
		return token{}, &unavailable{envUnavailable}
	}
	return c.inner.token(ctx, scopes)
}

// ---- managed identity ----

// One resource of a managed identity request, the scope without .default
func resource(scopes []string) (string, error) {
	if len(scopes) != 1 {
		return "", errors.New("This Azure sign-in takes exactly one scope for each token.")
	}
	return strings.TrimSuffix(scopes[0], "/.default"), nil
}

// Managed identity of the platform the variables name, the instance metadata service otherwise
type managed struct {
	client, kind string
	env          environment
	workload     entra
	inChain      bool
	cache        cache
	mu           sync.Mutex
	alive        bool
}

func managedIdentity(env environment, client string, noWorkload, inChain bool) *managed {
	m := &managed{client: client, env: env, inChain: inChain}
	switch {
	case env.get("IDENTITY_ENDPOINT") != "" && env.get("IDENTITY_HEADER") != "" && env.get("IDENTITY_SERVER_THUMBPRINT") != "":
		m.kind = "service_fabric"
	case env.get("IDENTITY_ENDPOINT") != "" && env.get("IDENTITY_HEADER") != "":
		m.kind = "app_service"
	case env.get("IDENTITY_ENDPOINT") != "" && env.get("IMDS_ENDPOINT") != "":
		m.kind = "azure_arc"
	case env.get("IDENTITY_ENDPOINT") != "":
	case env.get("MSI_ENDPOINT") != "" && env.get("MSI_SECRET") != "":
		m.kind = "azure_ml"
	case env.get("MSI_ENDPOINT") != "":
		m.kind = "cloud_shell"
	case env.get("AZURE_TENANT_ID") != "" && env.get("AZURE_FEDERATED_TOKEN_FILE") != "" && env.get("AZURE_AUTHORITY_HOST") != "" && !noWorkload:
		if w, err := workloadIdentity(env, env.get("AZURE_TENANT_ID"), cmp.Or(client, env.get("AZURE_CLIENT_ID")), env.get("AZURE_FEDERATED_TOKEN_FILE")); err == nil {
			m.kind, m.workload = "workload_identity", w
		} else {
			m.kind = "imds"
		}
	default:
		m.kind = "imds"
	}
	if m.kind != "" {
		slog.Info("ManagedIdentityCredential will use " + strings.ReplaceAll(m.kind, "_", " "))
	}
	return m
}

func (m *managed) name() string { return "ManagedIdentityCredential" }

func (m *managed) forget() {
	if f, ok := m.workload.(forgetter); ok {
		f.forget()
	}
	m.cache.forget()
}

func (m *managed) token(ctx context.Context, scopes []string) (token, error) {
	if m.kind == "" {
		return token{}, &unavailable{"No managed identity is available here."}
	}
	if m.workload != nil {
		return m.workload.token(ctx, scopes)
	}
	res, err := resource(scopes)
	if err != nil {
		return token{}, err
	}
	return m.cache.get(ctx, scopes, func(ctx context.Context) (token, error) { return m.request(ctx, res) })
}

// One token request in the shape of the platform
func (m *managed) request(ctx context.Context, res string) (token, error) {
	q := url.Values{"resource": {res}}
	headers := map[string]string{}
	method, endpoint := http.MethodGet, ""
	var body io.Reader
	with := httpClient
	switch m.kind {
	case "app_service":
		endpoint, headers["X-IDENTITY-HEADER"] = m.env.get("IDENTITY_ENDPOINT"), m.env.get("IDENTITY_HEADER")
		q.Set("api-version", "2019-08-01")
		if m.client != "" {
			q.Set("client_id", m.client)
		}
	case "service_fabric":
		endpoint, headers["Secret"] = m.env.get("IDENTITY_ENDPOINT"), m.env.get("IDENTITY_HEADER")
		q.Set("api-version", "2019-07-01-preview")
		if m.client != "" {
			q.Set("client_id", m.client)
		}
		// Service Fabric presents a certificate of its own, which IDENTITY_SERVER_THUMBPRINT names rather than a CA
		with = &http.Client{Timeout: httpClient.Timeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}}
	case "azure_ml":
		endpoint, headers["secret"] = m.env.get("MSI_ENDPOINT"), m.env.get("MSI_SECRET")
		q.Set("api-version", "2017-09-01")
		q.Set("clientid", cmp.Or(m.client, m.env.get("DEFAULT_IDENTITY_CLIENT_ID")))
	case "cloud_shell":
		if m.client != "" {
			return token{}, errors.New("Cloud Shell does not take user-assigned managed identities.")
		}
		method, endpoint, headers["Metadata"] = http.MethodPost, m.env.get("MSI_ENDPOINT"), "true"
		body, q = strings.NewReader(url.Values{"resource": {res}}.Encode()), url.Values{}
		headers["Content-Type"] = "application/x-www-form-urlencoded"
	case "azure_arc":
		if m.client != "" {
			return token{}, errors.New("Azure Arc does not take user-assigned managed identities.")
		}
		endpoint, headers["Metadata"] = m.env.get("IDENTITY_ENDPOINT"), "true"
		q.Set("api-version", "2020-06-01")
	default:
		endpoint = imdsEndpoint
		if host := strings.TrimSpace(m.env.get("AZURE_POD_IDENTITY_AUTHORITY_HOST")); host != "" {
			endpoint = strings.TrimRight(host, "/") + "/metadata/identity/oauth2/token"
		}
		headers["Metadata"] = "true"
		q.Set("api-version", "2018-02-01")
		if m.client != "" {
			q.Set("client_id", m.client)
		}
		return m.imds(ctx, endpoint+"?"+q.Encode(), headers)
	}
	target := endpoint
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	status, data, err := call(ctx, with, method, target, headers, body)
	if err != nil {
		return token{}, err
	}
	if m.kind == "azure_arc" && status == http.StatusUnauthorized {
		if data, err = m.arc(ctx, with, target); err != nil {
			return token{}, err
		}
	} else if status != http.StatusOK {
		return token{}, fmt.Errorf("Microsoft Entra ID gave an answer without a token: %v", data)
	}
	return parse(data, time.Now().Unix())
}

// Answers the Azure Arc challenge with the key file its agent named, only from the agent's own folder
func (m *managed) arc(ctx context.Context, with *http.Client, target string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata", "true")
	resp, err := with.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	header := resp.Header.Get("WWW-Authenticate")
	_, keyPath, ok := strings.Cut(header, "=")
	if !ok {
		return nil, errors.New("Azure Arc gave no key file in its answer: " + header)
	}
	keyPath = strings.TrimSpace(keyPath)
	expected := "/var/opt/azcmagent/tokens"
	if runtime.GOOS == "windows" {
		expected = filepath.Join(os.Getenv("ProgramData"), "AzureConnectedMachineAgent", "Tokens")
	}
	if filepath.Dir(keyPath) != expected || filepath.Ext(keyPath) != ".key" {
		return nil, errors.New("Azure Arc named a key file outside its own folder, so it was refused: " + keyPath)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Size() > 4096 {
		return nil, errors.New("The Azure Arc key file can't be used: " + keyPath)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	status, data, err := call(ctx, with, http.MethodGet, target, map[string]string{"Metadata": "true", "Authorization": "Basic " + string(key)}, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Microsoft Entra ID gave an answer without a token: %v", data)
	}
	return data, nil
}

// Token of the instance metadata service: reached within a second first when in the chain, then tried over answers that may pass
func (m *managed) imds(ctx context.Context, target string, headers map[string]string) (token, error) {
	m.mu.Lock()
	alive := m.alive
	m.mu.Unlock()
	if m.inChain && !alive {
		if _, _, err := call(ctx, &http.Client{Timeout: time.Second}, http.MethodGet, target, headers, nil); err != nil {
			return token{}, &unavailable{imdsUnavailable}
		}
		m.mu.Lock()
		m.alive = true
		m.mu.Unlock()
	}
	wait := 800 * time.Millisecond
	var status int
	var data map[string]any
	var err error
	for attempt := 0; attempt <= 5; attempt++ {
		if attempt > 0 {
			if err := credentials.Sleep(ctx, wait); err != nil {
				return token{}, err
			}
			wait *= 2
		}
		if status, data, err = call(ctx, httpClient, http.MethodGet, target, headers, nil); err != nil {
			if m.inChain {
				return token{}, &unavailable{imdsUnavailable}
			}
			return token{}, err
		}
		if status != http.StatusNotFound && status != http.StatusGone && status != http.StatusTooManyRequests && status < 500 {
			break
		}
	}
	switch {
	case status == http.StatusOK:
		return parse(data, time.Now().Unix())
	case status == http.StatusBadRequest:
		message := "No managed identity is assigned to this computer."
		if m.client != "" {
			message = "The managed identity asked for is not assigned to this computer."
		}
		if d := firstNonEmpty(data["error_description"], data["error"]); d != nil {
			message += fmt.Sprintf(" Azure said: %v", d)
		}
		return token{}, &unavailable{message}
	case status == http.StatusForbidden && strings.Contains(fmt.Sprint(data["error"]), "unreachable"):
		return token{}, &unavailable{fmt.Sprintf("No managed identity answered, because a gateway in between failed: %v", data)}
	}
	return token{}, fmt.Errorf("Microsoft Entra ID gave an answer without a token: %v", data)
}

// JSON request with its status and object body
func call(ctx context.Context, with *http.Client, method, target string, headers map[string]string, body io.Reader) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := with.Do(req)
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

// ---- developer tools ----

// Folder a tool runs in, never the caller's
func safeDir() string {
	if runtime.GOOS == "windows" {
		return cmp.Or(os.Getenv("SYSTEMROOT"), `C:\`)
	}
	return "/bin"
}

var accessTokenField = regexp.MustCompile(`"accessToken": "(.*?)("|$)`)

func sanitize(out string) string { return accessTokenField.ReplaceAllString(out, "****") }

// Runs a tool within the timeout: its output, its error output, its exit code, -1 with an error when it can't start
func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, string, int, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", "", -1, err
	}
	capped, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(capped, path, args...)
	cmd.Dir = safeDir()
	cmd.Env = append(os.Environ(), "AZURE_CORE_NO_COLOR=true")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if capped.Err() != nil {
		return stdout.String(), stderr.String(), code, capped.Err()
	}
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
		return stdout.String(), stderr.String(), code, err
	}
	return stdout.String(), stderr.String(), code, nil
}

// Failure of a tool, which only lets the chain go on when it runs in one
func toolFailure(inChain bool, message string) error {
	if inChain {
		return &unavailable{message}
	}
	return errors.New(message)
}

// Token of the Azure CLI's sign-in, from az account get-access-token
type azureCLI struct {
	tenant  string
	inChain bool
}

func (c *azureCLI) name() string { return "AzureCliCredential" }

func (c *azureCLI) token(ctx context.Context, scopes []string) (token, error) {
	res, err := resource(scopes)
	if err != nil {
		return token{}, err
	}
	args := []string{"account", "get-access-token", "--output", "json", "--resource", res}
	if c.tenant != "" {
		args = append(args, "--tenant", c.tenant)
	}
	out, stderr, code, err := run(ctx, 10*time.Second, "az", args...)
	switch {
	case code == -1 && err != nil, code == 127, strings.HasPrefix(stderr, "'az' is not recognized"):
		return token{}, &unavailable{"The Azure CLI is not installed, or not on your PATH."}
	case errors.Is(err, context.DeadlineExceeded):
		return token{}, &unavailable{"The Azure CLI took too long to answer."}
	case err != nil:
		return token{}, &unavailable{"The Azure CLI could not be run."}
	case code != 0 && (strings.Contains(stderr, "az login") || strings.Contains(stderr, "az account set")):
		return token{}, &unavailable{"The Azure CLI is not signed in. Run `az login`."}
	case code != 0:
		return token{}, toolFailure(c.inChain, cmp.Or(sanitize(stderr), "The Azure CLI failed."))
	}
	var data map[string]any
	if json.Unmarshal([]byte(out), &data) == nil {
		if value, ok := data["accessToken"].(string); ok {
			switch on := data["expires_on"].(type) {
			case float64:
				return token{value: value, expiresOn: int64(on)}, nil
			case string:
				if n, err := strconv.ParseInt(on, 10, 64); err == nil {
					return token{value: value, expiresOn: n}, nil
				}
			}
			if s, ok := data["expiresOn"].(string); ok {
				if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999", s, time.Local); err == nil {
					return token{value: value, expiresOn: t.Unix()}, nil
				}
			}
		}
	}
	return token{}, toolFailure(c.inChain, fmt.Sprintf("The Azure CLI gave no token: %s\nMicrosoft's guide to fixing this: "+
		"https://aka.ms/azsdk/python/identity/azclicredential/troubleshoot", sanitize(out)))
}

// Token of the Azure Developer CLI's sign-in, from azd auth token
type azd struct {
	tenant  string
	inChain bool
}

func (c *azd) name() string { return "AzureDeveloperCliCredential" }

func (c *azd) token(ctx context.Context, scopes []string) (token, error) {
	if len(scopes) == 0 {
		return token{}, errors.New("The request for a token named no scope.\nMicrosoft's guide to fixing this: https://aka.ms/azsdk/python/identity/azdevclicredential/troubleshoot")
	}
	args := []string{"auth", "token", "--output", "json", "--no-prompt"}
	for _, s := range scopes {
		args = append(args, "--scope", s)
	}
	if c.tenant != "" {
		args = append(args, "--tenant-id", c.tenant)
	}
	out, stderr, code, err := run(ctx, 10*time.Second, "azd", args...)
	switch {
	case code == -1 && err != nil, code == 127, strings.HasPrefix(stderr, "'azd' is not recognized"):
		return token{}, &unavailable{azdNotFound}
	case errors.Is(err, context.DeadlineExceeded):
		return token{}, &unavailable{"The Azure Developer CLI took too long to answer."}
	case err != nil:
		return token{}, &unavailable{"The Azure Developer CLI could not be run."}
	case code != 0 && (strings.Contains(stderr, "azd auth login") || strings.Contains(stderr, "not logged in")):
		return token{}, &unavailable{"The Azure Developer CLI is not signed in. Run `azd auth login`."}
	case code != 0:
		return token{}, toolFailure(c.inChain, cmp.Or(sanitize(stderr), "The Azure Developer CLI failed."))
	}
	var data map[string]any
	if json.Unmarshal([]byte(out), &data) == nil {
		value, _ := data["token"].(string)
		if expires, _ := data["expiresOn"].(string); len(expires) >= 19 && value != "" {
			if t, err := time.Parse("2006-01-02T15:04:05", expires[:19]); err == nil {
				return token{value: value, expiresOn: t.Unix()}, nil
			}
		}
	}
	return token{}, toolFailure(c.inChain, fmt.Sprintf("The Azure Developer CLI gave no token: %s\nMicrosoft's guide to fixing this: "+
		"https://aka.ms/azsdk/python/identity/azdevclicredential/troubleshoot", sanitize(out)))
}

const powershellScript = `$ErrorActionPreference = 'Stop'
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

// Token of Azure PowerShell's sign-in, from Get-AzAccessToken in a pwsh that asks nothing
type powershell struct {
	tenant  string
	inChain bool
}

func (c *powershell) name() string { return "AzurePowerShellCredential" }

func (c *powershell) token(ctx context.Context, scopes []string) (token, error) {
	res, err := resource(scopes)
	if err != nil {
		return token{}, err
	}
	tenantLine := ""
	if c.tenant != "" {
		tenantLine = "$params['TenantId'] = '" + c.tenant + "'"
	}
	units := utf16.Encode([]rune(fmt.Sprintf(powershellScript, res, tenantLine)))
	encoded := make([]byte, 0, len(units)*2)
	for _, u := range units {
		encoded = append(encoded, byte(u), byte(u>>8))
	}
	shell := "pwsh"
	if _, err := exec.LookPath("pwsh"); err != nil && runtime.GOOS == "windows" {
		shell = "powershell"
	}
	out, stderr, code, err := run(ctx, 10*time.Second, shell, "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded))
	failure := "PowerShell could not be run.\nMicrosoft's guide to fixing this: https://aka.ms/azsdk/python/identity/powershellcredential/troubleshoot"
	switch {
	case code == -1 && err != nil, code == 127, strings.HasPrefix(stderr, "'pwsh' is not recognized"):
		return token{}, &unavailable{"PowerShell is not installed."}
	case err != nil:
		return token{}, toolFailure(c.inChain, failure)
	case strings.Contains(out, "NO_AZ_ACCOUNT_MODULE"):
		return token{}, &unavailable{"Azure PowerShell needs the Az.Account module, version 2.2.0 or later, which is not installed."}
	case code != 0 && strings.Contains(stderr, "Run Connect-AzAccount to login"):
		return token{}, &unavailable{"Azure PowerShell is not signed in. Run `Connect-AzAccount`."}
	case code != 0 && strings.Contains(stderr, "AuthorizationManager check failed"):
		return token{}, &unavailable{"PowerShell's execution policy stopped Azure PowerShell from running."}
	case code != 0:
		return token{}, toolFailure(c.inChain, failure)
	}
	for _, line := range strings.Split(out, "\n") {
		if parts := strings.Split(strings.TrimSpace(line), "%"); len(parts) == 3 && parts[0] == "azsdk" {
			if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				return token{value: parts[1], expiresOn: n}, nil
			}
		}
	}
	return token{}, toolFailure(c.inChain, fmt.Sprintf("Azure PowerShell gave no token: %s", out))
}

// Sign-in Microsoft's developer tools keep in the shared MSAL token cache
type shared struct {
	username, tenant string
	env              environment
	cache            cache
}

func (c *shared) name() string { return "SharedTokenCacheCredential" }

func (c *shared) forget() { c.cache.forget() }

func sharedCache() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), ".IdentityService", "msal.cache")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".IdentityService", "msal.cache")
}

func (c *shared) token(ctx context.Context, scopes []string) (token, error) {
	none := func(message string) (token, error) {
		return token{}, &unavailable{message}
	}
	raw, _ := os.ReadFile(sharedCache())
	var store map[string]map[string]map[string]any
	json.Unmarshal(raw, &store)
	var accounts []map[string]any
	for _, account := range store["Account"] {
		if account["authority_type"] != "MSSTS" {
			continue
		}
		realm, _ := account["realm"].(string)
		home, _ := account["home_account_id"].(string)
		if c.username != "" && !strings.EqualFold(fmt.Sprint(account["username"]), c.username) {
			continue
		}
		if c.tenant != "" && realm != c.tenant && !strings.HasSuffix(home, "."+c.tenant) {
			continue
		}
		accounts = append(accounts, account)
	}
	switch {
	case len(accounts) == 0 && len(store["Account"]) == 0:
		return none("The shared Azure sign-in cache holds no accounts.")
	case len(accounts) == 0:
		return none("The shared Azure sign-in cache holds no account with that user name and tenant.")
	case len(accounts) > 1:
		return none("The shared Azure sign-in cache holds several accounts. Set AZURE_USERNAME and AZURE_TENANT_ID to pick one.")
	}
	account := accounts[0]
	home, realm := fmt.Sprint(account["home_account_id"]), fmt.Sprint(account["realm"])
	return c.cache.get(ctx, scopes, func(ctx context.Context) (token, error) {
		now := time.Now().Unix()
		for _, at := range store["AccessToken"] {
			target := " " + strings.ToLower(fmt.Sprint(at["target"])) + " "
			covered := at["home_account_id"] == home && at["realm"] == realm
			for _, s := range scopes {
				covered = covered && strings.Contains(target, " "+strings.ToLower(s)+" ")
			}
			expires, _ := strconv.ParseInt(fmt.Sprint(at["expires_on"]), 10, 64)
			if covered && expires-now > refreshOffset {
				return token{value: fmt.Sprint(at["secret"]), expiresOn: expires}, nil
			}
		}
		for _, rt := range store["RefreshToken"] {
			if rt["home_account_id"] != home || rt["client_id"] != cliClient && rt["family_id"] != "1" {
				continue
			}
			if fresh, err := request(ctx, c.env, realm, url.Values{"client_id": {cliClient}, "grant_type": {"refresh_token"}, "refresh_token": {fmt.Sprint(rt["secret"])},
				"scope": {strings.Join(append(append([]string{}, scopes...), "offline_access", "openid", "profile"), " ")}}); err == nil {
				return fresh, nil
			}
		}
		return token{}, fmt.Errorf("No token could be had for %v. Sign in again with a tool that uses Azure single sign-on, such as `az login`.", account["username"])
	})
}

// Sign-in in the browser of this computer, by authorization code and PKCE on a loopback redirect
type browser struct {
	tenant string
	env    environment
	cache  cache
	open   func(link string) error
}

func (c *browser) name() string { return "InteractiveBrowserCredential" }

func (c *browser) forget() { c.cache.forget() }

func (c *browser) token(ctx context.Context, scopes []string) (token, error) {
	return c.cache.get(ctx, scopes, func(ctx context.Context) (token, error) {
		callback, err := credentials.Listen("localhost", 0, "/", "Microsoft Entra ID", "entra_bind_failed")
		if err != nil {
			return token{}, err
		}
		redirect := strings.TrimSuffix(callback.Redirect, "/")
		verifier, challenge := credentials.PKCE(32)
		state := uuid.NewString()
		scope := strings.Join(append(append([]string{}, scopes...), "offline_access", "openid", "profile"), " ")
		link := c.env.authority() + "/" + c.tenant + "/oauth2/v2.0/authorize?" + url.Values{"client_id": {cliClient}, "response_type": {"code"},
			"redirect_uri": {redirect}, "scope": {scope}, "state": {state}, "prompt": {"select_account"}, "code_challenge": {challenge},
			"code_challenge_method": {"S256"}}.Encode()
		if c.open == nil || c.open(link) != nil {
			callback.Close()
			return token{}, errors.New("A browser could not be opened to sign in to Azure.")
		}
		code, err := callback.Wait(ctx, 300*time.Second, state, "azure-foundry")
		if err != nil {
			if failure, ok := errors.AsType[*credentials.AuthError](err); ok && failure.Code == "callback_timeout" {
				return token{}, errors.New("The Azure sign-in in the browser did not finish within 5 minutes.")
			}
			return token{}, err
		}
		return request(ctx, c.env, c.tenant, url.Values{"client_id": {cliClient}, "grant_type": {"authorization_code"}, "code": {code},
			"redirect_uri": {redirect}, "code_verifier": {verifier}, "scope": {scope}})
	})
}

// ---- the default chain ----

// Members of the chain, with the AZURE_TOKEN_CREDENTIALS name that picks each alone
var members = []struct{ key, picks string }{
	{"environmentcredential", "environmentcredential"}, {"workloadidentitycredential", "workloadidentitycredential"},
	{"managedidentitycredential", "managedidentitycredential"}, {"sharedtokencachecredential", ""},
	{"visualstudiocodecredential", "visualstudiocodecredential"}, {"azureclicredential", "azureclicredential"},
	{"azuredeveloperclicredential", "azuredeveloperclicredential"}, {"azurepowershellcredential", "azurepowershellcredential"},
	{"interactivebrowsercredential", "interactivebrowsercredential"}, {"brokercredential", ""},
}

// Members AZURE_TOKEN_CREDENTIALS and the browser opt-in leave in
func included(env environment, browser bool) (map[string]bool, error) {
	include := map[string]bool{}
	for _, m := range members {
		include[m.key] = m.key != "interactivebrowsercredential"
	}
	dev := map[string]bool{"visualstudiocodecredential": true, "azureclicredential": true, "azuredeveloperclicredential": true,
		"azurepowershellcredential": true, "sharedtokencachecredential": true, "brokercredential": true}
	prod := map[string]bool{"environmentcredential": true, "workloadidentitycredential": true, "managedidentitycredential": true}
	switch picked := strings.ToLower(strings.TrimSpace(env.get("AZURE_TOKEN_CREDENTIALS"))); picked {
	case "":
	case "dev", "prod":
		keep := dev
		if picked == "prod" {
			keep = prod
		}
		for k := range include {
			include[k] = keep[k]
		}
	default:
		var valid []string
		chosen := ""
		for _, m := range members {
			if m.picks != "" {
				valid = append(valid, m.picks)
			}
			if m.picks == picked {
				chosen = m.key
			}
		}
		if chosen == "" {
			sort.Strings(valid)
			return nil, fmt.Errorf("AZURE_TOKEN_CREDENTIALS can't be %s. Set it to one of %s.", picked, strings.Join(append([]string{"dev", "prod"}, valid...), ", "))
		}
		for k := range include {
			include[k] = k == chosen
		}
	}
	if browser {
		include["interactivebrowsercredential"] = true
	}
	return include, nil
}

// DefaultAzureCredential's chain, keeping the first member that answers
type chain struct {
	members []entra
	mu      sync.Mutex
	winner  entra
}

// The chain AZURE_TOKEN_CREDENTIALS picks, without the members the provider ignores
func defaultChain(env environment, browserIncluded bool, open func(string) error, ignores func(name string) bool) (*chain, error) {
	include, err := included(env, browserIncluded)
	if err != nil {
		return nil, err
	}
	var all []entra
	if include["environmentcredential"] {
		e, err := environmentCredential(env)
		if err != nil {
			return nil, err
		}
		all = append(all, e)
	}
	if include["workloadidentitycredential"] && env.get("AZURE_AUTHORITY_HOST") != "" && env.get("AZURE_TENANT_ID") != "" && env.get("AZURE_FEDERATED_TOKEN_FILE") != "" {
		w, err := workloadIdentity(env, env.get("AZURE_TENANT_ID"), env.get("AZURE_CLIENT_ID"), env.get("AZURE_FEDERATED_TOKEN_FILE"))
		if err != nil {
			return nil, err
		}
		all = append(all, w)
	}
	if include["managedidentitycredential"] {
		all = append(all, managedIdentity(env, env.get("AZURE_CLIENT_ID"), !include["workloadidentitycredential"], true))
	}
	if include["sharedtokencachecredential"] {
		all = append(all, &shared{username: env.get("AZURE_USERNAME"), tenant: env.get("AZURE_TENANT_ID"), env: env})
	}
	if include["visualstudiocodecredential"] {
		v, err := vscodeCredential(env)
		if err != nil {
			return nil, err
		}
		all = append(all, v)
	}
	if include["azureclicredential"] {
		all = append(all, &azureCLI{inChain: true})
	}
	if include["azurepowershellcredential"] {
		all = append(all, &powershell{inChain: true})
	}
	if include["azuredeveloperclicredential"] {
		all = append(all, &azd{inChain: true})
	}
	if include["interactivebrowsercredential"] {
		all = append(all, &browser{tenant: cmp.Or(env.get("AZURE_TENANT_ID"), "organizations"), env: env, open: open})
	}
	if include["brokercredential"] {
		all = append(all, &defaultBroker{env: env, tenant: env.get("AZURE_TENANT_ID")})
	}
	var kept []entra
	for _, member := range all {
		if !ignores(member.name()) {
			kept = append(kept, member)
		}
	}
	return &chain{members: kept}, nil
}

func (c *chain) name() string { return "DefaultAzureCredential" }

func (c *chain) forget() {
	for _, member := range c.members {
		if f, ok := member.(forgetter); ok {
			f.forget()
		}
	}
}

func (c *chain) token(ctx context.Context, scopes []string) (token, error) {
	c.mu.Lock()
	winner := c.winner
	c.mu.Unlock()
	if winner != nil {
		return winner.token(ctx, scopes)
	}
	var attempts strings.Builder
	for _, member := range c.members {
		t, err := member.token(ctx, scopes)
		if err == nil {
			slog.Info("DefaultAzureCredential acquired a token from " + member.name())
			c.mu.Lock()
			c.winner = member
			c.mu.Unlock()
			return t, nil
		}
		fmt.Fprintf(&attempts, "\n  %s: %s", SignedInBy(member.name()), err.Error())
		if _, ok := errors.AsType[*unavailable](err); !ok {
			break
		}
	}
	message := "No Azure sign-in on this computer gave a token."
	if attempts.Len() > 0 {
		message += "\nWhat each one said:" + attempts.String()
	}
	return token{}, errors.New(message + troubleshoot)
}

// Whether the browser sign-in is among the members, which waits for the person
func (c *chain) interactive() bool {
	for _, member := range c.members {
		if _, ok := member.(*browser); ok {
			return true
		}
	}
	return false
}

// Member that answered, the chain until one has
func (c *chain) answered() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.winner != nil {
		return c.winner.name()
	}
	return c.name()
}

// Credential an account's own service principal names: its secret, else its certificate, else the federated token file of the variables,
// else the user-assigned managed identity of its client alone
func own(env environment) (entra, error) {
	tenant, client := env.get("AZURE_TENANT_ID"), env.get("AZURE_CLIENT_ID")
	secret, certificate, file := env.get("AZURE_CLIENT_SECRET"), env.get("AZURE_CLIENT_CERTIFICATE_PATH"), env.get("AZURE_FEDERATED_TOKEN_FILE")
	switch {
	case tenant != "" && client != "" && secret != "":
		return secretPrincipal(env, tenant, client, secret)
	case tenant != "" && client != "" && certificate != "":
		return certificatePrincipal(env, tenant, client, certificate, env.get("AZURE_CLIENT_CERTIFICATE_PASSWORD"), env.get("AZURE_CLIENT_SEND_CERTIFICATE_CHAIN") != "")
	case tenant != "" && client != "" && file != "":
		return workloadIdentity(env, tenant, client, file)
	case client != "" && tenant == "":
		return managedIdentity(env, client, false, false), nil
	}
	return nil, errors.New("The account's service principal needs a tenant ID, a client ID, and a client secret or a certificate. A client ID " +
		"alone stands for a user-assigned managed identity.")
}

// Variables of the profile that pick a credential of the chain, as labels
func sources(env environment) []string {
	var out []string
	if strings.TrimSpace(env.get("AZURE_FEDERATED_TOKEN_FILE")) != "" {
		out = append(out, "a workload identity from AZURE_FEDERATED_TOKEN_FILE")
	}
	if env.get("AZURE_CLIENT_ID") != "" && env.get("AZURE_CLIENT_SECRET") != "" && env.get("AZURE_TENANT_ID") != "" {
		out = append(out, "a client secret from AZURE_ variables")
	}
	if strings.TrimSpace(env.get("IDENTITY_ENDPOINT")) != "" || strings.TrimSpace(env.get("MSI_ENDPOINT")) != "" {
		out = append(out, "a managed identity")
	}
	return out
}

// Words for the Azure sign-in of the name, as a person reads it
func SignedInBy(name string) string {
	words := map[string]string{"VisualStudioCodeCredential": "the VS Code sign-in", "BrokerCredential": "the Windows sign-in broker",
		"InteractiveBrowserBrokerCredential": "the sign-in broker in a browser", "ClientSecretCredential": "a client secret",
		"CertificateCredential": "a certificate", "WorkloadIdentityCredential": "a workload identity", "UsernamePasswordCredential": "a user name and password",
		"EnvironmentCredential": "the AZURE_ variables", "ManagedIdentityCredential": "a managed identity", "AzureCliCredential": "az login",
		"AzureDeveloperCliCredential": "azd auth login", "AzurePowerShellCredential": "Azure PowerShell", "SharedTokenCacheCredential": "the shared sign-in cache",
		"InteractiveBrowserCredential": "a browser sign-in", "DefaultAzureCredential": "the usual Azure sign-ins"}
	return cmp.Or(words[name], name)
}
