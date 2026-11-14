// Microsoft Entra ID tokens from the default credential chain, with the Windows account broker
package azure

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/toolchain"
)

const (
	DefaultScope = "https://ai.azure.com/.default"

	refreshOffset      = 300
	refreshRetryDelay  = 30
	developerClientID  = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"
	defaultAuthority   = "login.microsoftonline.com"
	processTimeout     = 10 * time.Second
	requestTimeout     = 60 * time.Second
	replyLimit         = 1 << 20
	fileLimit          = 1 << 20
	probeTimeoutHint   = "DefaultAzureCredential can be slow when the token service is unreachable or when az login state is stale. Try `az login` or set AZURE_CLIENT_ID / AZURE_TENANT_ID / AZURE_CLIENT_SECRET."
	invalidTenantText  = "invalid tenant ID provided, you can locate your tenant ID by following the instructions here: https://learn.microsoft.com/partner-center/find-ids-and-domain-names"
	chainFailureText   = "DefaultAzureCredential failed to retrieve a token from the included credentials."
	chainTroubleshoot  = "\nTo mitigate this issue, please refer to the troubleshooting guidelines here at https://aka.ms/azsdk/python/identity/defaultazurecredential/troubleshoot."
	nameEnvironment    = "EnvironmentCredential"
	nameWorkload       = "WorkloadIdentityCredential"
	nameManaged        = "ManagedIdentityCredential"
	nameCLI            = "AzureCliCredential"
	namePowerShell     = "AzurePowerShellCredential"
	nameDeveloperCLI   = "AzureDeveloperCliCredential"
	nameBrowser        = "InteractiveBrowserCredential"
	nameBroker         = "BrokerCredential"
	nameClientSecret   = "ClientSecretCredential"
	nameCertificate    = "CertificateCredential"
	nameDefault        = "DefaultAzureCredential"
	selectionVariable  = "AZURE_TOKEN_CREDENTIALS"
	authorityVariable  = "AZURE_AUTHORITY_HOST"
	tenantVariable     = "AZURE_TENANT_ID"
	clientVariable     = "AZURE_CLIENT_ID"
	secretVariable     = "AZURE_CLIENT_SECRET"
	tokenFileVariable  = "AZURE_FEDERATED_TOKEN_FILE"
	certPathVariable   = "AZURE_CLIENT_CERTIFICATE_PATH"
	certPassVariable   = "AZURE_CLIENT_CERTIFICATE_PASSWORD"
	certChainVariable  = "AZURE_CLIENT_SEND_CERTIFICATE_CHAIN"
	usernameVariable   = "AZURE_USERNAME"
	passwordVariable   = "AZURE_PASSWORD"
	podAuthorityHost   = "AZURE_POD_IDENTITY_AUTHORITY_HOST"
	identityEndpoint   = "IDENTITY_ENDPOINT"
	identityHeader     = "IDENTITY_HEADER"
	identityThumbprint = "IDENTITY_SERVER_THUMBPRINT"
	imdsEndpoint       = "IMDS_ENDPOINT"
	msiEndpoint        = "MSI_ENDPOINT"
	msiSecret          = "MSI_SECRET"
)

var ErrAmbientRefused = errors.New("Entra ID auth is refused for this profile: it sets no AZURE_* credential of its own, and under multiplexed profiles the ambient DefaultAzureCredential chain (process env, az CLI caches, host managed identity) mints the LAUNCH profile's identity, which would be sent to this profile's base_url. Set AZURE_TENANT_ID + AZURE_CLIENT_ID + AZURE_CLIENT_SECRET or AZURE_FEDERATED_TOKEN_FILE in this profile's own config; AZURE_CLIENT_ID alone selects the host's user-assigned managed identity.")

type UnavailableError struct {
	Credential string
	Message    string
	Err        error
}

func (e *UnavailableError) Error() string { return e.Message }

func (e *UnavailableError) Unwrap() error { return e.Err }

type AuthError struct {
	Credential string
	Message    string
	Code       string
	Status     int
	Err        error
}

func (e *AuthError) Error() string { return e.Message }

func (e *AuthError) Unwrap() error { return e.Err }

type Attempt struct {
	Credential string
	Message    string
}

type ChainError struct {
	Attempts []Attempt
	Err      error
}

func (e *ChainError) Error() string {
	var b strings.Builder
	b.WriteString(chainFailureText)
	b.WriteString("\nAttempted credentials:\n\t")
	for i, a := range e.Attempts {
		if i > 0 {
			b.WriteString("\n\t")
		}
		b.WriteString(a.Credential)
		if a.Message != "" {
			b.WriteString(": " + a.Message)
		}
	}
	b.WriteString(chainTroubleshoot)
	return b.String()
}

func (e *ChainError) Unwrap() error { return e.Err }

type Env func(name string) string

func ProcessEnv(name string) string { return os.Getenv(name) }

type Config struct {
	Scope                     string
	ExcludeInteractiveBrowser bool
}

func NewConfig(scope string) Config {
	return Config{Scope: scope, ExcludeInteractiveBrowser: true}.normalized()
}

func (c Config) normalized() Config {
	if c.Scope = strings.TrimSpace(c.Scope); c.Scope == "" {
		c.Scope = DefaultScope
	}
	return c
}

func (c Config) Map() map[string]any {
	c = c.normalized()
	return map[string]any{"scope": c.Scope, "exclude_interactive_browser": c.ExcludeInteractiveBrowser}
}

func ConfigFromMap(data map[string]any, defaultScope string) Config {
	scope, _ := data["scope"].(string)
	if scope = strings.TrimSpace(scope); scope == "" {
		scope = defaultScope
	}
	cfg := NewConfig(scope)
	if value, ok := data["exclude_interactive_browser"]; ok {
		switch v := value.(type) {
		case nil:
			cfg.ExcludeInteractiveBrowser = false
		case bool:
			cfg.ExcludeInteractiveBrowser = v
		case string:
			cfg.ExcludeInteractiveBrowser = v != ""
		case int:
			cfg.ExcludeInteractiveBrowser = v != 0
		case int64:
			cfg.ExcludeInteractiveBrowser = v != 0
		case float64:
			cfg.ExcludeInteractiveBrowser = v != 0
		case []any:
			cfg.ExcludeInteractiveBrowser = len(v) != 0
		case map[string]any:
			cfg.ExcludeInteractiveBrowser = len(v) != 0
		}
	}
	return cfg
}

type accessToken struct {
	value     string
	expiresOn int64
	refreshOn int64
}

func (accessToken) String() string { return "[entra access token]" }

func (t accessToken) GoString() string { return t.String() }

func (t accessToken) LogValue() slog.Value { return slog.StringValue(t.String()) }

type source interface {
	name() string
	token(ctx context.Context, scope string, inChain bool) (accessToken, error)
}

type tokenCache struct {
	mu          sync.Mutex
	tokens      map[string]accessToken
	lastRequest int64
}

func (c *tokenCache) get(ctx context.Context, key string, request func(ctx context.Context) (accessToken, error)) (accessToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]accessToken{}
	}
	now := time.Now().Unix()
	cached, ok := c.tokens[key]
	if !ok || cached.expiresOn-now < refreshOffset {
		c.lastRequest = now
		fresh, err := request(ctx)
		if err != nil {
			return accessToken{}, err
		}
		c.tokens[key] = fresh
		return fresh, nil
	}
	if cached.refreshOn != 0 && now >= cached.refreshOn && now-c.lastRequest >= refreshRetryDelay {
		c.lastRequest = now
		if fresh, err := request(ctx); err == nil {
			c.tokens[key] = fresh
			return fresh, nil
		}
	}
	return cached, nil
}

func (c *tokenCache) String() string { return "[entra token cache]" }

func resourceOf(scope string) string { return strings.TrimSuffix(scope, "/.default") }

func validTenant(tenant string) bool {
	if tenant == "" {
		return false
	}
	for i := 0; i < len(tenant); i++ {
		ch := tenant[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '.') {
			return false
		}
	}
	return true
}

type Options struct {
	Secrets   Env
	Routed    bool
	Multiplex bool
	HTTP      httpclient.Options
	Host      *hostinfo.Host
	Toolchain *toolchain.Store
}

type Identity struct {
	secrets   Env
	routed    bool
	multiplex bool
	c         *client
	host      *hostinfo.Host
	toolchain *toolchain.Store

	mu      sync.Mutex
	sources map[Config]source
}

func New(o Options) (*Identity, error) {
	c, err := newClient(o.HTTP)
	if err != nil {
		return nil, err
	}
	secrets := o.Secrets
	if secrets == nil {
		secrets = ProcessEnv
	}
	host := o.Host
	if host == nil {
		host = hostinfo.New()
	}
	return &Identity{secrets: secrets, routed: o.Routed, multiplex: o.Multiplex, c: c, host: host, toolchain: o.Toolchain, sources: map[Config]source{}}, nil
}

func (i *Identity) Reset() {
	i.mu.Lock()
	i.sources = map[Config]source{}
	i.mu.Unlock()
}

func (i *Identity) credential(cfg Config) (source, error) {
	cfg = cfg.normalized()
	i.mu.Lock()
	defer i.mu.Unlock()
	if s, ok := i.sources[cfg]; ok {
		return s, nil
	}
	var s source
	var err error
	if i.routed {
		s, err = i.scopedSource(cfg)
	} else {
		s, err = i.defaultChain(cfg)
	}
	if err != nil {
		return nil, err
	}
	i.sources[cfg] = s
	return s, nil
}

func (i *Identity) scopedSource(cfg Config) (source, error) {
	read := func(name string) string { return strings.TrimSpace(i.secrets(name)) }
	tenant, clientID, secret, tokenFile := read(tenantVariable), read(clientVariable), read(secretVariable), read(tokenFileVariable)
	switch {
	case tenant != "" && clientID != "" && secret != "":
		return i.c.clientSecret(tenant, clientID, secret, authorityHost(os.Getenv))
	case tenant != "" && clientID != "" && tokenFile != "":
		return i.c.workloadIdentity(tenant, clientID, tokenFile, authorityHost(os.Getenv))
	case clientID != "" && tenant == "" && secret == "" && tokenFile == "":
		return i.c.managedIdentity(clientID, false)
	case i.multiplex:
		return nil, ErrAmbientRefused
	}
	return i.defaultChain(cfg)
}

type tokenSource struct {
	credential source
	scope      string

	mu      sync.Mutex
	current accessToken
}

func (s *tokenSource) Token(ctx context.Context) (auth.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	if s.current.value == "" || (s.current.refreshOn != 0 && s.current.refreshOn <= now) || s.current.expiresOn-now < refreshOffset {
		fresh, err := s.credential.token(ctx, s.scope, false)
		if err != nil {
			return auth.Token{}, err
		}
		s.current = fresh
	}
	return auth.Token{Value: auth.NewSecret(s.current.value), Expiry: time.Unix(s.current.expiresOn, 0)}, nil
}

func (s *tokenSource) Kind() auth.SourceKind { return auth.SourceEntraID }

func (s *tokenSource) String() string { return "[entra token source " + s.scope + "]" }

func (i *Identity) TokenSource(cfg Config) (auth.TokenSource, error) {
	cfg = cfg.normalized()
	credential, err := i.credential(cfg)
	if err != nil {
		return nil, err
	}
	return &tokenSource{credential: credential, scope: cfg.Scope}, nil
}

type probeResult struct {
	token accessToken
	err   error
}

func (i *Identity) probe(ctx context.Context, cfg Config, timeout time.Duration) (accessToken, bool, error) {
	if timeout < 10*time.Millisecond {
		timeout = 10 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan probeResult, 1)
	go func() {
		credential, err := i.credential(cfg)
		if err != nil {
			done <- probeResult{err: err}
			return
		}
		token, err := credential.token(ctx, cfg.Scope, false)
		done <- probeResult{token: token, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return accessToken{}, true, nil
		}
		return result.token, false, result.err
	case <-ctx.Done():
		return accessToken{}, errors.Is(ctx.Err(), context.DeadlineExceeded), ctx.Err()
	}
}

func (i *Identity) HasCredentials(ctx context.Context, cfg Config, timeout time.Duration) bool {
	cfg = cfg.normalized()
	token, timedOut, err := i.probe(ctx, cfg, timeout)
	if timedOut {
		slog.Debug("Entra token service probe timed out", "seconds", timeout.Seconds())
		return false
	}
	if err != nil {
		slog.Debug("Entra credential probe failed", "error", err)
		return false
	}
	return token.value != ""
}

type Description struct {
	OK         bool
	Scope      string
	TenantID   string
	EnvSources []string
	Error      string
	Hint       string
	ExpiresOn  time.Time
}

func (i *Identity) envSources() []string {
	scoped := func(name string) string { return strings.TrimSpace(i.secrets(name)) }
	ambient := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	var labels []string
	if scoped(tokenFileVariable) != "" {
		labels = append(labels, nameWorkload+" ("+tokenFileVariable+")")
	}
	if scoped(clientVariable) != "" && scoped(secretVariable) != "" && scoped(tenantVariable) != "" {
		labels = append(labels, nameEnvironment+" (client secret)")
	}
	if ambient(identityEndpoint) != "" || ambient(msiEndpoint) != "" {
		labels = append(labels, nameManaged+" ("+identityEndpoint+")")
	}
	return labels
}

func (i *Identity) Describe(ctx context.Context, cfg Config, timeout time.Duration) Description {
	cfg = cfg.normalized()
	info := Description{Scope: cfg.Scope, TenantID: strings.TrimSpace(i.secrets(tenantVariable)), EnvSources: i.envSources()}
	token, timedOut, err := i.probe(ctx, cfg, timeout)
	switch {
	case timedOut:
		info.Error = "Token probe timed out after " + strconv.FormatFloat(timeout.Seconds(), 'f', 0, 64) + "s"
		info.Hint = probeTimeoutHint
	case err != nil:
		info.Error = err.Error()
	case token.value == "":
		info.Error = "credential chain exhausted"
	default:
		info.OK = true
		info.ExpiresOn = time.Unix(token.expiresOn, 0)
	}
	return info
}

func unavailable(credential, message string) error {
	return &UnavailableError{Credential: credential, Message: message}
}

func failure(inChain bool, credential, message string) error {
	if inChain {
		return &UnavailableError{Credential: credential, Message: message}
	}
	return &AuthError{Credential: credential, Message: message}
}
