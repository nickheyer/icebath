package credentials

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/msalruntime"
)

const (
	// Client of the VS Code Azure Resources extension
	vscodeClient    = "aebc6443-996d-45c2-90f0-388ff96faa56"
	vscodeMaxRecord = 10 * 1024
	brokerRedirect  = "https://login.microsoftonline.com/common/oauth2/nativeclient"
	// Redirect of a broker sign-in on macOS
	brokerRedirectMac    = "msauth.com.msauth.unsignedapp://auth"
	brokerGrant          = "broker"
	ciamSuffix           = ".ciamlogin.com"
	brokerDefaultFailure = "Failed to authenticate user with default broker account."
	brokerFailure        = "Failed to authenticate user with broker account."
	noUsableAccount      = "Failed to authenticate user"
	brokerPlatformOnly   = "InteractiveBrowserBrokerCredential unavailable. Brokered authentication is only supported on Windows and WSL platforms."
	interactionRequired  = "Interactive authentication is required to get a token. Call 'authenticate' to begin."
)

// Where the VS Code Azure Resources extension keeps the account it signed in, below the home folder
var vscodeRecords = []string{
	filepath.Join(".azure", "ms-azuretools.vscode-azureresourcegroups", "authRecord.json"),
	filepath.Join(".Azure", "ms-azuretools.vscode-azureresourcegroups", "authRecord.json"),
}

// Folder the broker library is fetched to on first use
func libraryHome() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "icebath")
}

// Whether this Linux runs under WSL, by its kernel release naming Microsoft
func wsl() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(raw)), "microsoft")
}

// A value that fails validation, which a broker member reports as unavailable
type valueError struct{ message string }

func (e *valueError) Error() string { return e.message }

func str(v any) string {
	s, _ := v.(string)
	return s
}

func whole64(v any) (int64, bool) {
	n, ok := config.Whole(v)
	return int64(n), ok
}

// ---- the VS Code sign-in record ----

// Account a sign-in is of, as the VS Code Azure Resources extension and the broker keep it
type record struct {
	authority, client, homeAccount, tenant, username string
}

// Python's name of a JSON value's type, as the record's errors name it
func kind(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	}
	return "dict"
}

// JSON text of the record: UTF-8, else UTF-16 by its byte order mark
func recordText(raw []byte) (string, error) {
	if utf8.Valid(raw) {
		return strings.TrimPrefix(string(raw), "\uFEFF"), nil
	}
	var order binary.ByteOrder
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		order = binary.LittleEndian
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		order = binary.BigEndian
	default:
		return "", errors.New("'utf-8' codec can't decode the authentication record")
	}
	body := raw[2:]
	if len(body)%2 != 0 {
		return "", errors.New("'utf-16' codec can't decode the authentication record: truncated data")
	}
	units := make([]uint16, len(body)/2)
	for i := range units {
		units[i] = order.Uint16(body[2*i:])
	}
	return string(utf16.Decode(units)), nil
}

// Record of the first place that has one, nil when none does
func vscodeRecord() (*record, error) {
	home, _ := os.UserHomeDir()
	for _, rel := range vscodeRecords {
		path := filepath.Join(home, rel)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Size() > vscodeMaxRecord {
			return nil, &valueError{"VS Code auth record file is unexpectedly large. Please check the file for corruption or unexpected content."}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text, err := recordText(raw)
		if err != nil {
			return nil, &valueError{err.Error()}
		}
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			return nil, &valueError{err.Error()}
		}
		data, ok := parsed.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("a broker record value is %s, expected a mapping", kind(parsed))
		}
		if err := validRecord(data); err != nil {
			return nil, err
		}
		return &record{authority: str(data["authority"]), client: str(data["clientId"]), homeAccount: str(data["homeAccountId"]),
			tenant: str(data["tenantId"]), username: str(data["username"])}, nil
	}
	return nil, nil
}

// Checks of the record's fields, every failure joined into one
func validRecord(data map[string]any) error {
	var errs []string
	field := func(k string) (string, bool, bool) {
		v, present := data[k]
		s, isText := v.(string)
		return s, present, isText && s != ""
	}
	if tenant, present, ok := field("tenantId"); !present {
		errs = append(errs, "tenantId field is missing")
	} else if !ok {
		errs = append(errs, "tenantId must be a non-empty string")
	} else if err := validTenant(tenant); err != nil {
		errs = append(errs, "tenantId validation failed: "+err.Error())
	}
	if client, present, ok := field("clientId"); !present {
		errs = append(errs, "clientId field is missing")
	} else if !ok {
		errs = append(errs, "clientId must be a non-empty string")
	} else if client != vscodeClient {
		errs = append(errs, "clientId must match expected VS Code Azure Resources extension client ID: "+vscodeClient)
	}
	for _, k := range []string{"username", "homeAccountId", "authority"} {
		if _, present, ok := field(k); !present {
			errs = append(errs, k+" field is missing")
		} else if !ok {
			errs = append(errs, k+" must be a non-empty string")
		}
	}
	if len(errs) > 0 {
		return &valueError{"Authentication record validation failed: " + strings.Join(errs, "; ")}
	}
	return nil
}

// ---- VisualStudioCodeCredential ----

// Sign-in of the VS Code Azure Resources extension, served by the platform's broker from the extension's record
type vscode struct {
	env    environment
	record *record
	// Why it can't sign in, empty while it may
	why    string
	mu     sync.Mutex
	broker *brokered
}

func (*vscode) name() string { return "VisualStudioCodeCredential" }

// The credential of the record, unavailable without a sign-in or a readable record
func vscodeCredential(env environment) (*vscode, error) {
	c := &vscode{env: env}
	found, err := vscodeRecord()
	if ve, ok := errors.AsType[*valueError](err); ok {
		c.why = "Failed to load authentication record from Visual Studio Code: " + ve.message +
			". Please ensure you have the Azure Resources extension installed and signed in."
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if found == nil {
		c.why = "VisualStudioCodeCredential requires the user to be signed in to Azure via Visual Studio Code. " +
			"Please ensure you have the Azure Resources extension installed and have signed in."
	}
	c.record = found
	return c, nil
}

// Token through the broker, whose library is fetched on first use; a failure lets the chain go on
func (c *vscode) token(ctx context.Context, scopes []string) (token, error) {
	if c.why != "" {
		return token{}, &unavailable{c.why}
	}
	c.mu.Lock()
	if c.broker == nil {
		library, err := msalruntime.Ensure(ctx, libraryHome())
		if err != nil {
			c.mu.Unlock()
			return token{}, &unavailable{"VisualStudioCodeCredential requires the msalruntime library (" + err.Error() + ")."}
		}
		b, err := newBrokered(c.env, library, brokerOptions{client: vscodeClient, record: c.record, defaultAccount: true, noInteraction: true,
			timeout: 300 * time.Second, inChain: true})
		if err != nil {
			c.mu.Unlock()
			if ve, ok := errors.AsType[*valueError](err); ok {
				return token{}, &unavailable{"Failed to load authentication record from Visual Studio Code: " + ve.message +
					". Please ensure you have the Azure Resources extension installed and signed in."}
			}
			return token{}, err
		}
		c.broker = b
	}
	b := c.broker
	c.mu.Unlock()
	t, err := b.token(ctx, scopes)
	if err != nil {
		if _, ok := errors.AsType[*unavailable](err); !ok {
			return token{}, &unavailable{err.Error()}
		}
	}
	return t, err
}

func (c *vscode) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broker != nil {
		c.broker.forget()
	}
}

// ---- BrokerCredential ----

// The broker's default account, on Windows and WSL
type defaultBroker struct {
	env    environment
	tenant string
	mu     sync.Mutex
	broker *brokered
}

func (*defaultBroker) name() string { return "BrokerCredential" }

func (c *defaultBroker) token(ctx context.Context, scopes []string) (token, error) {
	if runtime.GOOS != "windows" && !wsl() {
		return token{}, &unavailable{brokerPlatformOnly}
	}
	c.mu.Lock()
	if c.broker == nil {
		library, err := msalruntime.Ensure(ctx, libraryHome())
		if err != nil {
			c.mu.Unlock()
			return token{}, &unavailable{"InteractiveBrowserBrokerCredential unavailable. The msalruntime library is required to use brokered " +
				"authentication (" + err.Error() + ")."}
		}
		b, err := newBrokered(c.env, library, brokerOptions{tenant: c.tenant, client: cliClient, defaultAccount: true, noInteraction: true,
			timeout: 300 * time.Second, inChain: true})
		if err != nil {
			c.mu.Unlock()
			return token{}, &unavailable{"InteractiveBrowserBrokerCredential initialization failed: " + err.Error()}
		}
		c.broker = b
	}
	b := c.broker
	c.mu.Unlock()
	return b.token(ctx, scopes)
}

func (c *defaultBroker) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broker != nil {
		c.broker.forget()
	}
}

// ---- InteractiveBrowserBrokerCredential ----

// How a broker sign-in goes
type brokerOptions struct {
	tenant, client string
	record         *record
	// Tries the broker's default account first
	defaultAccount bool
	// Never shows the account picker
	noInteraction bool
	timeout       time.Duration
	// Run by the default chain, where a failure lets it go on
	inChain bool
}

// Broker sign-in: the token the application keeps for the record's account, else the broker's
type brokered struct {
	opts      brokerOptions
	env       environment
	library   string
	authority string
	tenant    string
	mu        sync.Mutex
	record    *record
	app       *public
}

func newBrokered(env environment, library string, opts brokerOptions) (*brokered, error) {
	host := cmp.Or(env.get("AZURE_AUTHORITY_HOST"), defaultAuthority)
	tenant := opts.tenant
	if opts.record != nil {
		host = opts.record.authority
	}
	authority, err := normalAuthority(host)
	if err != nil {
		return nil, err
	}
	if opts.record != nil {
		opts.client = opts.record.client
		tenant = cmp.Or(tenant, opts.record.tenant)
	}
	tenant = cmp.Or(tenant, "organizations")
	if err := validTenant(tenant); err != nil {
		return nil, &valueError{err.Error()}
	}
	return &brokered{opts: opts, env: env, library: library, authority: authority, tenant: tenant, record: opts.record}, nil
}

// An authority as an https origin, refusing any other scheme
func normalAuthority(authority string) (string, error) {
	u, err := url.Parse(authority)
	if err != nil || u.Scheme == "" {
		return "https://" + strings.TrimRight(authority, " /"), nil
	}
	if u.Scheme != "https" {
		return "", &valueError{fmt.Sprintf("'%s' is an invalid authority. The value must be a TLS protected (https) URL.", authority)}
	}
	return strings.TrimRight(authority, " /"), nil
}

func (c *brokered) name() string { return "InteractiveBrowserBrokerCredential" }

// Application of the credential's tenant with the broker on, made on first use
func (c *brokered) application(ctx context.Context) (*public, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.app != nil {
		return c.app, nil
	}
	app, err := newPublic(ctx, c.authority+"/"+c.tenant, c.opts.client, c.library, true)
	if err != nil {
		return nil, err
	}
	c.app = app
	return app, nil
}

// Application made again with the broker off, where a broker sign-in that fails falls back to the browser outside Windows
func (c *brokered) withoutBroker(ctx context.Context) (*public, error) {
	app, err := newPublic(ctx, c.authority+"/"+c.tenant, c.opts.client, c.library, false)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.app = app
	c.mu.Unlock()
	return app, nil
}

// Drops the application and its tokens, the record's account kept so the broker signs it in again
func (c *brokered) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.app = nil
}

func (c *brokered) token(ctx context.Context, scopes []string) (token, error) {
	if len(scopes) == 0 {
		return token{}, &valueError{"'get_token' requires at least one scope"}
	}
	t, err := c.silent(ctx, scopes)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, errInteraction) {
		slog.Warn(c.name() + ".get_token failed: " + err.Error())
		return token{}, err
	}
	now := time.Now().Unix()
	result, err := c.request(ctx, scopes)
	if err == nil {
		if _, ok := result["access_token"]; !ok {
			err = fmt.Errorf("Authentication failed: %v", firstNonEmpty(result["error_description"], result["error"], "None"))
		} else {
			var built *record
			if built, err = recordOf(result); err == nil {
				c.mu.Lock()
				c.record = built
				c.mu.Unlock()
			}
		}
	}
	if err != nil {
		slog.Warn(c.name() + ".get_token failed: " + err.Error())
		return token{}, err
	}
	slog.Info(c.name() + ".get_token succeeded")
	expiresIn, _ := whole64(result["expires_in"])
	out := token{value: fmt.Sprint(result["access_token"]), expiresOn: now + expiresIn}
	if on, ok := whole64(result["refresh_on"]); ok {
		out.refreshOn = on
	}
	return out, nil
}

// Silent sign-in found no token, which lets the credential ask the broker
var errInteraction = errors.New(interactionRequired)

// Token the application keeps or the broker renews for the record's account
func (c *brokered) silent(ctx context.Context, scopes []string) (token, error) {
	c.mu.Lock()
	current := c.record
	c.mu.Unlock()
	if current == nil {
		return token{}, errInteraction
	}
	app, err := c.application(ctx)
	if err != nil {
		return token{}, errors.New("Authentication failed: " + err.Error())
	}
	for _, account := range app.accountsOf(current.username) {
		if account.homeAccount != current.homeAccount {
			continue
		}
		now := time.Now().Unix()
		result, err := app.silent(ctx, scopes, account)
		if err != nil {
			return token{}, errors.New("Authentication failed: " + err.Error())
		}
		_, hasToken := result["access_token"]
		expiresIn, hasExpiry := whole64(result["expires_in"])
		if result != nil && hasToken && hasExpiry {
			out := token{value: fmt.Sprint(result["access_token"]), expiresOn: now + expiresIn}
			if on, ok := whole64(result["refresh_on"]); ok {
				out.refreshOn = on
			}
			slog.Info(c.name() + ".get_token succeeded")
			return out, nil
		}
	}
	return token{}, errInteraction
}

// Broker request: the default account first when asked, then the account picker unless the credential may not show it
func (c *brokered) request(ctx context.Context, scopes []string) (map[string]any, error) {
	app, err := c.application(ctx)
	if err != nil {
		return nil, errors.New("Authentication failed: " + err.Error())
	}
	result := map[string]any{}
	if c.opts.defaultAccount {
		r, err := c.attempt(ctx, app, scopes, "none")
		if err == nil {
			if _, ok := r["access_token"]; ok {
				return r, nil
			}
			result = r
		} else if c.opts.noInteraction {
			return nil, &unavailable{brokerDefaultFailure}
		}
		if c.opts.noInteraction {
			return result, c.check(result)
		}
	}
	r, err := c.attempt(ctx, app, scopes, "select_account")
	_, unbound := errors.AsType[*bindError](err)
	switch {
	case unbound:
		return nil, &unavailable{"Couldn't start an HTTP server."}
	case err != nil && runtime.GOOS != "windows" && !wsl():
		fallback, ferr := c.withoutBroker(ctx)
		if ferr != nil {
			return nil, errors.New("Authentication failed: " + ferr.Error())
		}
		if r, err = c.attempt(ctx, fallback, scopes, "select_account"); err != nil {
			return nil, errors.New("Authentication failed: " + err.Error())
		}
	case err != nil:
		return nil, &unavailable{brokerFailure}
	}
	return r, c.check(r)
}

func (c *brokered) attempt(ctx context.Context, app *public, scopes []string, prompt string) (map[string]any, error) {
	return app.interactive(ctx, interactiveRequest{scopes: scopes, prompt: prompt, timeout: c.opts.timeout})
}

// Why a result holds no token: unavailable in the chain, a failure outside it
func (c *brokered) check(result map[string]any) error {
	if _, ok := result["access_token"]; ok {
		return nil
	}
	message := noUsableAccount
	if d, ok := result["error_description"]; ok {
		message = fmt.Sprint(firstNonEmpty(d, "None"))
	}
	if c.opts.inChain {
		return &unavailable{message}
	}
	return errors.New(message)
}

// Account of a token answer, from its client_info or its id token's subject
func recordOf(result map[string]any) (*record, error) {
	fail := errors.New("Failed to build AuthenticationRecord from unexpected identity token")
	claims, ok := result["id_token_claims"].(map[string]any)
	if !ok {
		return nil, fail
	}
	var home string
	if raw, ok := result["client_info"].(string); ok {
		info, err := clientInfo(raw)
		if err != nil || info["uid"] == nil || info["utid"] == nil {
			return nil, fail
		}
		home = fmt.Sprintf("%v.%v", info["uid"], info["utid"])
	} else if sub, ok := claims["sub"]; ok {
		home = fmt.Sprint(sub)
	} else {
		return nil, fail
	}
	iss, ok := claims["iss"].(string)
	if !ok {
		return nil, fail
	}
	issuer, err := url.Parse(iss)
	if err != nil {
		return nil, fail
	}
	username := str(claims["preferred_username"])
	if username == "" {
		upn, ok := claims["upn"]
		if !ok {
			return nil, fail
		}
		username = fmt.Sprint(upn)
	}
	aud, ok := claims["aud"]
	if !ok {
		return nil, fail
	}
	return &record{authority: issuer.Host, client: fmt.Sprint(aud), homeAccount: home, tenant: cmp.Or(str(claims["tid"]), strings.Trim(issuer.Path, "/")),
		username: username}, nil
}

// JSON of an unpadded URL safe base64 client_info or JWT segment
func clientInfo(raw string) (map[string]any, error) {
	raw += strings.Repeat("=", (4-len(raw)%4)%4)
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

// ---- public client ----

// Public client of a user sign-in: its validated authority, the broker when on, and the tokens it got
type public struct {
	client                string
	instance, tenant      string
	authorizationEndpoint string
	tokenEndpoint         string
	vouched               bool
	broker                *msalruntime.Runtime
	mu                    sync.Mutex
	accounts              map[string]cachedAccount
	accessTokens          map[string]cachedToken
	refreshTokens         map[string]cachedRefresh
}

type cachedAccount struct {
	homeAccount, environment, realm, localAccount, username, authorityType, source string
}

type cachedToken struct {
	secret, homeAccount, environment, client, target, realm, tokenType string
	expiresOn, refreshOn                                               int64
}

type cachedRefresh struct {
	secret, homeAccount, environment, client, family string
}

// The application, its authority validated by discovery, the broker on when asked and the platform and authority allow it
func newPublic(ctx context.Context, authority, client, library string, broker bool) (*public, error) {
	app := &public{client: client, accounts: map[string]cachedAccount{}, accessTokens: map[string]cachedToken{}, refreshTokens: map[string]cachedRefresh{}}
	if err := app.discover(ctx, authority); err != nil {
		return nil, err
	}
	adfs := strings.EqualFold(app.tenant, "adfs") && !strings.HasSuffix(app.instance, ciamSuffix)
	broker = broker && !adfs && !app.b2c(authority)
	if broker && runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		broker = false
		slog.Warn("Broker on macOS is supported only on Apple Silicon (arm64). We will fallback to non-broker.")
	}
	if broker {
		rt, err := msalruntime.Load(library)
		_, startup := errors.AsType[*msalruntime.StartupError](err)
		switch {
		case startup:
			slog.Warn("Broker is unavailable on this platform. We will fallback to non-broker.")
		case err != nil:
			return nil, err
		default:
			app.broker = rt
		}
	}
	slog.Debug(fmt.Sprintf("Broker enabled? %t", app.broker != nil))
	return app, nil
}

// Host and first path segment of an authority or endpoint
func canonical(raw string) (*url.URL, string, string, error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "https" && u.Hostname() != "" {
		parts := strings.Split(u.Path, "/")
		host := strings.ToLower(u.Hostname())
		if len(parts) >= 2 && parts[1] != "" {
			return u, host, parts[1], nil
		}
		if strings.HasSuffix(host, ciamSuffix) {
			return u, host, strings.TrimSuffix(host, ciamSuffix) + ".onmicrosoft.com", nil
		}
	}
	return nil, "", "", &valueError{fmt.Sprintf("Your given address (%s) should consist of an https url with hostname and a "+
		"minimum of one segment in a path: e.g. https://login.microsoftonline.com/{tenant} or https://{tenant_name}.ciamlogin.com/{tenant} "+
		"or https://{tenant_name}.b2clogin.com/{tenant_name}.onmicrosoft.com/policy", raw)}
}

func (app *public) b2c(authority string) bool {
	u, _, _, err := canonical(authority)
	if err != nil {
		return false
	}
	for _, host := range b2cHosts {
		if strings.HasSuffix(app.instance, "."+host) {
			return true
		}
	}
	parts := strings.Split(u.Path, "/")
	return len(parts) == 3 && strings.HasPrefix(strings.ToLower(parts[2]), "b2c_")
}

// Instance discovery for a host Microsoft doesn't list, then the tenant's OpenID configuration
func (app *public) discover(ctx context.Context, authority string) error {
	u, instance, tenant, err := canonical(authority)
	if err != nil {
		return err
	}
	app.instance = instance
	ciam := strings.HasSuffix(instance, ciamSuffix)
	adfs := strings.EqualFold(tenant, "adfs") && !ciam
	app.vouched = adfs || app.b2c(authority)
	var discovery string
	if !knownAuthorities[instance] && !app.vouched {
		q := url.Values{"authorization_endpoint": {"https://" + instance + u.Path + "/oauth2/v2.0/authorize"}, "api-version": {"1.0"}}
		_, payload, err := call(ctx, httpClient, http.MethodGet, "https://"+defaultAuthority+"/common/discovery/instance?"+q.Encode(), nil, nil)
		if err != nil {
			return err
		}
		if payload["error"] == "invalid_instance" {
			return &valueError{fmt.Sprintf("invalid_instance: The authority you provided, %s, is not known. If it is a valid domain "+
				"name known to you, set AZURE_AUTHORITY_HOST to a host Microsoft lists", authority)}
		}
		discovery = str(payload["tenant_discovery_endpoint"])
	} else {
		path := u.Path
		if ciam && len(u.Path) <= 1 {
			path = tenant
		}
		version := "/v2.0"
		if adfs {
			version = ""
		}
		d := *u
		d.Path = path + version + "/.well-known/openid-configuration"
		discovery = d.String()
	}
	status, configuration, err := call(ctx, httpClient, http.MethodGet, discovery, nil, nil)
	switch {
	case err != nil:
		return err
	case status >= 400 && status < 500:
		return &valueError{fmt.Sprintf("Unable to get authority configuration for %s. Authority would typically be in a format of "+
			"https://login.microsoftonline.com/your_tenant or https://tenant_name.ciamlogin.com or "+
			"https://tenant_name.b2clogin.com/tenant.onmicrosoft.com/policy.  Also please double check your tenant name or GUID is correct.", authority)}
	case status != http.StatusOK:
		return fmt.Errorf("%d Server Error: %s for url: %s", status, http.StatusText(status), discovery)
	}
	app.authorizationEndpoint = str(configuration["authorization_endpoint"])
	app.tokenEndpoint = str(configuration["token_endpoint"])
	_, _, app.tenant, err = canonical(app.tokenEndpoint)
	return err
}

// Accounts the application keeps for its authority's host, of the user name when one is given
func (app *public) accountsOf(username string) []cachedAccount {
	app.mu.Lock()
	defer app.mu.Unlock()
	grouped := map[string]cachedAccount{}
	var order []string
	for _, a := range app.accounts {
		if a.environment != app.instance || a.authorityType != "MSSTS" && a.authorityType != "ADFS" {
			continue
		}
		if _, seen := grouped[a.homeAccount]; !seen {
			order = append(order, a.homeAccount)
		}
		grouped[a.homeAccount] = a
	}
	sort.Strings(order)
	var out []cachedAccount
	for _, id := range order {
		if a := grouped[id]; username == "" || strings.EqualFold(a.username, username) {
			out = append(out, a)
		}
	}
	return out
}

// Scopes with the OpenID scopes every user sign-in asks for
func decorated(scopes []string) []string {
	return append(append([]string{}, scopes...), "offline_access", "openid", "profile")
}

// A token kept for the account that is fresh, else the broker's renewal for a broker account, else the account's refresh token
func (app *public) silent(ctx context.Context, scopes []string, account cachedAccount) (map[string]any, error) {
	now := time.Now().Unix()
	var kept map[string]any
	app.mu.Lock()
	for _, at := range app.accessTokens {
		if at.client != app.client || at.environment != app.instance || at.realm != app.tenant || at.homeAccount != account.homeAccount {
			continue
		}
		if !covers(at.target, scopes) || at.expiresOn-now < refreshOffset {
			continue
		}
		kept = map[string]any{"access_token": at.secret, "token_type": at.tokenType, "expires_in": float64(at.expiresOn - now)}
		if at.refreshOn != 0 {
			kept["refresh_on"] = float64(at.refreshOn)
			if at.refreshOn < now {
				break
			}
		}
		app.mu.Unlock()
		return kept, nil
	}
	app.mu.Unlock()
	if app.broker != nil && (account.source == brokerGrant || account.source == "") {
		response, err := app.brokerSilent(ctx, account.localAccount, scopes, uuid.NewString())
		if err != nil {
			return nil, err
		}
		if response != nil {
			if _, failed := response["error"]; account.source == brokerGrant || !failed {
				return app.fromBroker(response, scopes), nil
			}
		}
	}
	result, err := app.refresh(ctx, scopes, account)
	if err != nil {
		if kept == nil {
			return nil, err
		}
		return kept, nil
	}
	if result != nil {
		if _, failed := result["error"]; !failed || kept == nil {
			return result, nil
		}
	}
	if kept != nil {
		return kept, nil
	}
	return result, nil
}

// Whether a kept token's scopes hold every one asked for
func covers(target string, scopes []string) bool {
	have := map[string]bool{}
	for _, s := range strings.Fields(target) {
		have[s] = true
	}
	for _, s := range scopes {
		if !have[s] {
			return false
		}
	}
	return true
}

// Refresh token grant with the account's family or own refresh tokens, nil when none is kept
func (app *public) refresh(ctx context.Context, scopes []string, account cachedAccount) (map[string]any, error) {
	app.mu.Lock()
	var candidates []cachedRefresh
	for _, rt := range app.refreshTokens {
		if rt.homeAccount == account.homeAccount && rt.environment == app.instance && (rt.family == "1" || rt.client == app.client) {
			candidates = append(candidates, rt)
		}
	}
	app.mu.Unlock()
	var last map[string]any
	anchor := map[string]string{"X-AnchorMailbox": "Oid:" + strings.ReplaceAll(account.homeAccount, ".", "@")}
	for _, rt := range candidates {
		response, err := app.post(ctx, url.Values{"client_id": {app.client}, "grant_type": {"refresh_token"}, "refresh_token": {rt.secret},
			"scope": {strings.Join(decorated(scopes), " ")}}, anchor)
		if err != nil {
			return nil, err
		}
		if _, failed := response["error"]; !failed {
			if s, ok := response["id_token"].(string); ok {
				response["id_token_claims"] = idClaims(s)
			}
			app.keep(response, scopes, "refresh_token", "", true)
			return cleaned(response), nil
		}
		last = response
	}
	return last, nil
}

// Posts to the token endpoint, the answer decoded
func (app *public) post(ctx context.Context, form url.Values, headers map[string]string) (map[string]any, error) {
	_, data, err := call(ctx, httpClient, http.MethodPost, app.tokenEndpoint, mergeHeaders(map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		headers), strings.NewReader(form.Encode()))
	return data, err
}

// One interactive sign-in: its prompt and how long it may wait for the person
type interactiveRequest struct {
	scopes  []string
	prompt  string
	timeout time.Duration
}

// Interactive sign-in through the broker when it is on, else the browser on a loopback redirect
func (app *public) interactive(ctx context.Context, req interactiveRequest) (map[string]any, error) {
	if app.broker != nil {
		response, err := app.viaBroker(ctx, req)
		if err != nil {
			return nil, err
		}
		return app.fromBroker(response, req.scopes), nil
	}
	return app.viaBrowser(ctx, req)
}

// Broker sign-in in the order the OIDC prompt values ask: silently for none, else with the account picker
func (app *public) viaBroker(ctx context.Context, req interactiveRequest) (map[string]any, error) {
	authority := "https://" + app.instance + "/" + app.tenant
	if req.prompt == "none" || req.prompt == "" {
		response, err := app.brokerSignIn(ctx, authority, req, false)
		if err != nil {
			return nil, err
		}
		if req.prompt == "none" {
			return response, nil
		}
		status, _ := response["_broker_status"].(msalruntime.Status)
		_, failed := response["error"]
		if !failed || status != msalruntime.StatusAccountUnusable && status != msalruntime.StatusInteractionRequired {
			return response, nil
		}
	}
	return app.brokerSignIn(ctx, authority, req, true)
}

// Parameters every broker call carries: the redirect and the scopes
func (app *public) brokerParams(authority string, scopes []string) (*msalruntime.AuthParameters, error) {
	params, err := app.broker.NewAuthParameters(app.client, authority)
	if err != nil {
		return nil, err
	}
	redirect := brokerRedirect
	if runtime.GOOS == "darwin" {
		redirect = brokerRedirectMac
	}
	for _, step := range []func() error{
		func() error { return params.SetAdditionalParameter("msal_client_sku", "icebath") },
		func() error { return params.SetRedirectURI(redirect) },
		func() error { return params.SetRequestedScopes(scopes) },
	} {
		if err := step(); err != nil {
			params.Release()
			return nil, err
		}
	}
	if app.vouched {
		if err := params.SetAdditionalParameter("validateAuthority", "no"); err != nil {
			params.Release()
			return nil, err
		}
	}
	return params, nil
}

// Broker sign-in, silent or parented on the console window, else the desktop's
func (app *public) brokerSignIn(ctx context.Context, authority string, req interactiveRequest, interactively bool) (map[string]any, error) {
	params, err := app.brokerParams(authority, req.scopes)
	if err != nil {
		return nil, err
	}
	defer params.Release()
	var result *msalruntime.AuthResult
	if interactively {
		if req.prompt != "" && req.prompt != "select_account" {
			slog.Warn(fmt.Sprintf("prompt=%s is not supported by this module", req.prompt))
		}
		if err := params.SetAdditionalParameter("msal_gui_thread", "true"); err != nil {
			return nil, err
		}
		parent := msalruntime.ConsoleWindow()
		if parent == 0 {
			parent = msalruntime.DesktopWindow()
		}
		result, err = app.broker.SignInInteractively(ctx, parent, params, uuid.NewString(), "")
	} else {
		result, err = app.broker.SignInSilently(ctx, params, uuid.NewString())
	}
	if err != nil {
		return nil, err
	}
	defer result.Release()
	return app.convert(result)
}

// Silent token of a broker account read by its id, nil when the broker holds no such account
func (app *public) brokerSilent(ctx context.Context, accountID string, scopes []string, correlation string) (map[string]any, error) {
	read, err := app.broker.ReadAccountByID(ctx, accountID, correlation)
	if err != nil {
		return nil, err
	}
	defer read.Release()
	failure, err := read.Failure()
	if err != nil {
		return nil, err
	}
	if failure != nil {
		if rerr := redirectError(failure, "None"); rerr != nil {
			return nil, rerr
		}
		slog.Debug(fmt.Sprintf("read_account_by_id() error: %v", brokerError(failure)))
		return nil, nil
	}
	account, err := read.Account()
	if err != nil || account == nil {
		return nil, err
	}
	defer account.Release()
	params, err := app.brokerParams("https://"+app.instance+"/"+app.tenant, scopes)
	if err != nil {
		return nil, err
	}
	defer params.Release()
	result, err := app.broker.AcquireTokenSilently(ctx, params, correlation, account)
	if err != nil {
		return nil, err
	}
	defer result.Release()
	return app.convert(result)
}

// A broker failure as the answer of a token endpoint
func brokerError(e *msalruntime.Error) map[string]any {
	return map[string]any{"error": "broker_error",
		"error_description": fmt.Sprintf("%s. Status: %s, Error code: %d, Tag: %d", e.Context, e.Status, e.Code, e.Tag),
		"_broker_status":    e.Status}
}

// Failure of a broker that names a redirect the application doesn't register, nil for any other
func redirectError(e *msalruntime.Error, client string) error {
	if strings.Contains(e.Context, "AADSTS50011") || strings.Contains(e.Context, "AADSTS7000218") {
		return &valueError{fmt.Sprintf("MsalRuntime needs the current app to register these redirect_uri\n"+
			"(1) ms-appx-web://Microsoft.AAD.BrokerPlugin/%s\n(2) %s\n(3) %s", client, brokerRedirectMac, brokerRedirect)}
	}
	return nil
}

// A broker result as the answer of a token endpoint
func (app *public) convert(result *msalruntime.AuthResult) (map[string]any, error) {
	if _, err := result.Telemetry(); err != nil {
		return nil, err
	}
	failure, err := result.Failure()
	if err != nil {
		return nil, err
	}
	if failure != nil {
		if rerr := redirectError(failure, app.client); rerr != nil {
			return nil, rerr
		}
		return brokerError(failure), nil
	}
	idToken, err := result.IDToken()
	if err != nil {
		return nil, err
	}
	claims := map[string]any{}
	if idToken != "" {
		if err := json.Unmarshal([]byte(idToken), &claims); err != nil {
			return nil, err
		}
	}
	account, err := result.Account()
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errors.New("Account is expected to be always available")
	}
	defer account.Release()
	pop, err := result.IsPopAuthorization()
	if err != nil {
		return nil, err
	}
	var value string
	if pop {
		header, err := result.AuthorizationHeader()
		if err != nil {
			return nil, err
		}
		if fields := strings.Fields(header); len(fields) > 1 {
			value = fields[1]
		}
	} else if value, err = result.AccessToken(); err != nil {
		return nil, err
	}
	expires, err := result.ExpiresOn()
	if err != nil {
		return nil, err
	}
	rawID, err := result.RawIDToken()
	if err != nil {
		return nil, err
	}
	info, err := account.ClientInfo()
	if err != nil {
		return nil, err
	}
	accountID, err := account.AccountID()
	if err != nil {
		return nil, err
	}
	tokenType := "bearer"
	if pop {
		tokenType = "pop"
	}
	out := map[string]any{"access_token": value, "expires_in": float64(expires - time.Now().Unix()), "id_token": rawID, "id_token_claims": claims,
		"client_info": info, "_account_id": accountID, "token_type": tokenType}
	for k, v := range out {
		if v == "" || v == float64(0) || k == "id_token_claims" && len(claims) == 0 {
			delete(out, k)
		}
	}
	if _, ok := out["access_token"]; !ok {
		return nil, errors.New("'access_token'")
	}
	granted, err := result.GrantedScopes()
	if err != nil {
		return nil, err
	}
	if len(granted) > 0 {
		out["scope"] = strings.Join(granted, " ")
	}
	return out, nil
}

// Keeps a broker answer's token as a broker account's
func (app *public) fromBroker(response map[string]any, scopes []string) map[string]any {
	if _, failed := response["error"]; failed {
		return cleaned(response)
	}
	if s, ok := response["scope"].(string); ok {
		scopes = strings.Fields(s)
	}
	app.keep(response, scopes, brokerGrant, str(response["_account_id"]), false)
	response["token_source"] = "broker"
	return cleaned(response)
}

// An answer without the private keys
func cleaned(response map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range response {
		if k != "refresh_in" && !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out
}

// Keeps a token answer: its access token, its account unless skipped, and its refresh token
func (app *public) keep(response map[string]any, scopes []string, grant, accountID string, skipAccount bool) {
	claims, _ := response["id_token_claims"].(map[string]any)
	if claims == nil {
		claims = map[string]any{}
		if idt := str(response["id_token"]); idt != "" {
			claims = idClaims(idt)
		}
	}
	var home string
	known := false
	if raw, ok := response["client_info"].(string); ok {
		if info, err := clientInfo(raw); err == nil && info["uid"] != nil && info["utid"] != nil {
			home, known = fmt.Sprintf("%v.%v", info["uid"], info["utid"]), true
		}
	}
	if !known && len(claims) > 0 {
		home, known = fmt.Sprint(claims["sub"]), true
	}
	sorted := append([]string{}, scopes...)
	sort.Strings(sorted)
	target := strings.Join(sorted, " ")
	now := time.Now().Unix()
	app.mu.Lock()
	defer app.mu.Unlock()
	if value := str(response["access_token"]); value != "" {
		expiresIn := int64(600)
		if on, ok := whole64(response["expires_on"]); ok {
			expiresIn = on - now
		}
		if in, ok := whole64(response["expires_in"]); ok {
			expiresIn = in
		}
		at := cachedToken{secret: value, homeAccount: home, environment: app.instance, client: app.client, target: target, realm: app.tenant,
			tokenType: cmp.Or(str(response["token_type"]), "Bearer"), expiresOn: now + expiresIn}
		if in, ok := whole64(response["refresh_in"]); ok {
			at.refreshOn = now + in
		}
		app.accessTokens[strings.Join([]string{home, app.instance, app.client, app.tenant, target}, "|")] = at
	}
	if known && !skipAccount {
		accountID = cmp.Or(accountID, str(claims["oid"]), str(claims["sub"]))
		authorityType := "MSSTS"
		if app.tenant == "adfs" {
			authorityType = "ADFS"
		}
		account := cachedAccount{homeAccount: home, environment: app.instance, realm: app.tenant, localAccount: accountID,
			username: cmp.Or(str(claims["preferred_username"]), str(claims["upn"])), authorityType: authorityType}
		if grant == brokerGrant || grant == "authorization_code" {
			account.source = grant
		}
		app.accounts[strings.Join([]string{home, app.instance, app.tenant}, "|")] = account
	}
	if rt := str(response["refresh_token"]); rt != "" {
		app.refreshTokens[strings.Join([]string{home, app.instance, app.client}, "|")] = cachedRefresh{secret: rt, homeAccount: home,
			environment: app.instance, client: app.client, family: str(response["foci"])}
	}
}

// Unverified claims of an id token
func idClaims(idToken string) map[string]any {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	claims, err := clientInfo(parts[1])
	if err != nil {
		return map[string]any{}
	}
	return claims
}

// Authorization code sign-in with PKCE in the browser on a loopback redirect, the sign-in with the broker off
func (app *public) viaBrowser(ctx context.Context, req interactiveRequest) (map[string]any, error) {
	callback, err := credentials.Listen("localhost", 0, "/", "Microsoft Entra ID", "entra_bind_failed")
	if err != nil {
		return nil, &bindError{err.Error()}
	}
	redirect := strings.TrimSuffix(callback.Redirect, "/")
	verifier, challenge := credentials.PKCE(32)
	state := uuid.NewString()
	scope := strings.Join(decorated(req.scopes), " ")
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		callback.Close()
		return nil, err
	}
	sum := sha256.Sum256([]byte(base64.RawURLEncoding.EncodeToString(nonceBytes)))
	nonce := hex.EncodeToString(sum[:])
	q := url.Values{"client_id": {app.client}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {scope}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "nonce": {nonce}}
	if req.prompt != "" {
		q.Set("prompt", req.prompt)
	}
	link := app.authorizationEndpoint + "?" + q.Encode()
	slog.Info("Open a browser on this device to visit: " + link)
	if err := open(link); err != nil {
		port := strings.TrimPrefix(redirect, "http://localhost:")
		slog.Warn(fmt.Sprintf("Found no browser in current environment. If this program is being run inside a container which either (1) has "+
			"access to host network (i.e. started by `docker run --net=host -it ...`), or (2) published port %s to host network (i.e. started by "+
			"`docker run -p 127.0.0.1:%s:%s -it ...`), you can use browser on host to visit the following link. Otherwise, this auth attempt "+
			"would either timeout (current timeout setting is %d) or be aborted by CTRL+C. Auth URI: %s", port, port, port, int(req.timeout.Seconds()), link))
	}
	code, err := callback.Wait(ctx, req.timeout, state, "azure-foundry")
	if err != nil {
		failure, ok := errors.AsType[*credentials.AuthError](err)
		switch {
		case ok && failure.Code == "authorization_denied":
			return map[string]any{"error": failure.Code, "error_description": failure.Message}, nil
		case ok && failure.Code == "state_mismatch":
			return map[string]any{"error": "state_mismatch", "error_description": "state mismatch"}, nil
		}
		return nil, errors.New("User did not complete the flow in time")
	}
	response, err := app.post(ctx, url.Values{"client_id": {app.client}, "grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "scope": {scope}}, nil)
	if err != nil {
		return nil, err
	}
	if _, failed := response["error"]; !failed {
		if s, ok := response["id_token"].(string); ok {
			claims := idClaims(s)
			response["id_token_claims"] = claims
			if got := claims["nonce"]; got != nonce {
				return nil, fmt.Errorf("The nonce in id token (\"%v\") should match our nonce (\"%s\")", firstNonEmpty(got, "None"), nonce)
			}
		}
		app.keep(response, req.scopes, "authorization_code", "", false)
		response["token_source"] = "identity_provider"
	}
	return cleaned(response), nil
}

// The loopback server could not listen, which the chain takes as unavailable
type bindError struct{ message string }

func (e *bindError) Error() string { return e.message }
