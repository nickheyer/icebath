package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/home"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/msalruntime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	entraVSCodeClientID       = "aebc6443-996d-45c2-90f0-388ff96faa56"
	entraVSCodeMaxAuthRecord  = 10 * 1024
	entraBrokerRedirectURI    = "https://login.microsoftonline.com/common/oauth2/nativeclient"
	entraBrokerRedirectURIMac = "msauth.com.msauth.unsignedapp://auth"
	entraMSALSKU              = "MSAL.Python"
	entraMSALVersion          = "1.39.0"
	entraBrokerGrantType      = "broker"
	entraWorldWide            = "login.microsoftonline.com"
	entraCIAMSuffix           = ".ciamlogin.com"
	entraBrokerDefaultFailure = "Failed to authenticate user with default broker account."
	entraBrokerFailure        = "Failed to authenticate user with broker account."
	entraNoUsableAccount      = "Failed to authenticate user"
)

var (
	entraWellKnownHosts = map[string]bool{entraWorldWide: true, "login.microsoft.com": true, "login.windows.net": true,
		"sts.windows.net": true, "login.chinacloudapi.cn": true, "login.partner.microsoftonline.cn": true,
		"login.microsoftonline.de": true, "login-us.microsoftonline.com": true, "login.microsoftonline.us": true,
		"login.usgovcloudapi.net": true, "login.sovcloud-identity.fr": true, "login.sovcloud-identity.de": true,
		"login.sovcloud-identity.sg": true}
	entraB2CHosts = []string{"b2clogin.com", "b2clogin.cn", "b2clogin.us", "b2clogin.de", "ciamlogin.com"}

	// Broker library under the icebath home, fetched as a pinned archive member on first use
	entraEnsureMSALRuntime = msalruntime.Ensure
	// Native broker runtime loader, the msalruntime binding started once per process
	entraLoadMSALRuntime = msalruntime.Load
)

// Unavailable text of the default broker member outside Windows and WSL
const entraBrokerPlatformOnly = "InteractiveBrowserBrokerCredential unavailable. Brokered authentication is only supported on Windows and WSL platforms."

// Unavailable text of the broker member when the broker library is absent, with the reason it is
func entraBrokerLibraryRequired(reason error) string {
	return "InteractiveBrowserBrokerCredential unavailable. The msalruntime library is required to use brokered " +
		"authentication (" + reason.Error() + ")."
}

// Broker library path under the icebath home, fetched on first use
func entraBrokerLibrary(ctx context.Context) (string, error) {
	return entraEnsureMSALRuntime(ctx, home.Dir(ctx))
}

// Linux kernel release naming Microsoft, the way msal and azure-identity detect WSL
func entraIsWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(raw)), "microsoft")
}

// ---- VS Code authentication record ----

// Account the VS Code Azure Resources extension signed in, as its authRecord.json stores it
type entraAuthRecord struct {
	authority, clientID, homeAccountID, tenantID, username string
}

// ValueError raised while reading or validating the record, the class the VS Code credential reports
type entraValueError struct{ msg string }

func (e *entraValueError) Error() string { return e.msg }

var entraVSCodeAuthRecordPaths = []string{
	filepath.Join(".azure", "ms-azuretools.vscode-azureresourcegroups", "authRecord.json"),
	filepath.Join(".Azure", "ms-azuretools.vscode-azureresourcegroups", "authRecord.json"),
}

// Record from the first location that exists, nil when none does
func loadVSCodeAuthRecord() (*entraAuthRecord, error) {
	h, _ := os.UserHomeDir()
	for _, rel := range entraVSCodeAuthRecordPaths {
		path := filepath.Join(h, rel)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		if st.Size() > entraVSCodeMaxAuthRecord {
			return nil, &entraValueError{msg: "VS Code auth record file is unexpectedly large. Please check the file for corruption or unexpected content."}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New(pypath.OSErrorText(err) + ": " + pyrepr.StrRepr(path))
		}
		text := string(raw)
		if !utf8.Valid(raw) {
			if _, derr := pyjson.DecodeBytes(raw); derr != nil {
				return nil, &entraValueError{msg: derr.Error()}
			}
		}
		parsed, err := pyjson.Loads(text)
		if err != nil {
			return nil, &entraValueError{msg: err.Error()}
		}
		obj, ok := parsed.(*pyjson.Object)
		if !ok {
			return nil, errors.New(entraSubscriptTypeError(parsed))
		}
		if err := validateVSCodeAuthRecord(obj); err != nil {
			return nil, err
		}
		field := func(k string) string { v, _ := obj.Get(k); s, _ := v.(string); return s }
		return &entraAuthRecord{authority: field("authority"), clientID: field("clientId"), homeAccountID: field("homeAccountId"),
			tenantID: field("tenantId"), username: field("username")}, nil
	}
	return nil, nil
}

// TypeError text Python raises for indexing a non dict JSON value with a string key
func entraSubscriptTypeError(v any) string {
	switch v.(type) {
	case []any:
		return "list indices must be integers or slices, not str"
	case string:
		return "string indices must be integers, not 'str'"
	case bool:
		return "'bool' object is not subscriptable"
	case float64:
		return "'float' object is not subscriptable"
	case nil:
		return "'NoneType' object is not subscriptable"
	}
	return "'int' object is not subscriptable"
}

// Schema and value checks of the record, every failure joined into one ValueError
func validateVSCodeAuthRecord(data *pyjson.Object) error {
	var errs []string
	str := func(k string) (string, bool, bool) {
		v, present := data.Get(k)
		s, isStr := v.(string)
		return s, present, isStr && s != ""
	}
	if tenant, present, ok := str("tenantId"); !present {
		errs = append(errs, "tenantId field is missing")
	} else if !ok {
		errs = append(errs, "tenantId must be a non-empty string")
	} else if err := entraValidateTenant(tenant); err != nil {
		errs = append(errs, "tenantId validation failed: "+err.Error())
	}
	if client, present, ok := str("clientId"); !present {
		errs = append(errs, "clientId field is missing")
	} else if !ok {
		errs = append(errs, "clientId must be a non-empty string")
	} else if client != entraVSCodeClientID {
		errs = append(errs, "clientId must match expected VS Code Azure Resources extension client ID: "+entraVSCodeClientID)
	}
	for _, k := range []string{"username", "homeAccountId", "authority"} {
		if _, present, ok := str(k); !present {
			errs = append(errs, k+" field is missing")
		} else if !ok {
			errs = append(errs, k+" must be a non-empty string")
		}
	}
	if len(errs) > 0 {
		return &entraValueError{msg: "Authentication record validation failed: " + strings.Join(errs, "; ")}
	}
	return nil
}

// ---- VisualStudioCodeCredential ----

// VS Code Azure Resources sign in, served through the platform broker from the extension's record
type entraVSCodeCredential struct {
	broker      *entraBrokerBrowserCredential
	unavailable string
}

func (*entraVSCodeCredential) Name() string { return "VisualStudioCodeCredential" }

// Credential over the record in its own tenant, unavailable without a sign in, a readable record, or the broker library
func newEntraVSCodeCredential(ctx context.Context) (*entraVSCodeCredential, error) {
	c := &entraVSCodeCredential{}
	unreadable := func(err error) bool {
		var ve *entraValueError
		if !errors.As(err, &ve) {
			return false
		}
		c.unavailable = "Failed to load authentication record from Visual Studio Code: " + ve.msg +
			". Please ensure you have the Azure Resources extension installed and signed in."
		return true
	}
	record, err := loadVSCodeAuthRecord()
	switch {
	case unreadable(err):
		return c, nil
	case err != nil:
		return c, err
	case record == nil:
		c.unavailable = "VisualStudioCodeCredential requires the user to be signed in to Azure via Visual Studio Code. " +
			"Please ensure you have the Azure Resources extension installed and have signed in."
		return c, nil
	}
	library, err := entraBrokerLibrary(ctx)
	if err != nil {
		c.unavailable = "VisualStudioCodeCredential requires the msalruntime library (" + err.Error() + ")."
		return c, nil
	}
	c.broker, err = newEntraBrokerBrowserCredential(library, entraBrokerOptions{clientID: entraVSCodeClientID,
		record: record, useDefaultBrokerAccount: true, disableInteractiveFallback: true, timeout: 300 * time.Second})
	if unreadable(err) {
		return c, nil
	}
	return c, err
}

// Token through the broker, an authentication failure reported as unavailable inside the chain
func (c *entraVSCodeCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	if c.broker == nil {
		return EntraAccessToken{}, &entraUnavailableError{Msg: c.unavailable}
	}
	tok, err := c.broker.GetToken(ctx, scopes...)
	var ae *entraAuthError
	if err != nil && entraWithinChain(ctx) && errors.As(err, &ae) {
		return EntraAccessToken{}, &entraUnavailableError{Msg: ae.Msg}
	}
	return tok, err
}

// ---- BrokerCredential ----

// Default broker account sign in, available on Windows and WSL
type entraBrokerCredential struct {
	broker      *entraBrokerBrowserCredential
	unavailable string
}

func (*entraBrokerCredential) Name() string { return "BrokerCredential" }

// Chain member over the developer sign on client, the default account of the broker in the given tenant
func newEntraBrokerCredential(ctx context.Context, tenant string) *entraBrokerCredential {
	if runtime.GOOS != "windows" && !entraIsWSL() {
		return &entraBrokerCredential{unavailable: entraBrokerPlatformOnly}
	}
	library, err := entraBrokerLibrary(ctx)
	if err != nil {
		return &entraBrokerCredential{unavailable: entraBrokerLibraryRequired(err)}
	}
	b, err := newEntraBrokerBrowserCredential(library, entraBrokerOptions{tenant: tenant, clientID: entraAzureCLIClientID,
		useDefaultBrokerAccount: true, disableInteractiveFallback: true, timeout: 300 * time.Second})
	if err != nil {
		return &entraBrokerCredential{unavailable: "InteractiveBrowserBrokerCredential initialization failed: " + err.Error()}
	}
	return &entraBrokerCredential{broker: b}
}

func (c *entraBrokerCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	if c.broker == nil {
		return EntraAccessToken{}, &entraUnavailableError{Msg: c.unavailable}
	}
	return c.broker.GetToken(ctx, scopes...)
}

// ---- InteractiveBrowserBrokerCredential ----

// Keywords of an InteractiveBrowserBrokerCredential, the parent window defaulting to the console window handle
type entraBrokerOptions struct {
	tenant, clientID           string
	record                     *entraAuthRecord
	parentWindow               *msalruntime.WindowHandle
	enableMSAPassthrough       bool
	useDefaultBrokerAccount    bool
	disableInteractiveFallback bool
	loginHint                  string
	timeout                    time.Duration
}

// Broker sign in with the azure-identity silent then interactive order and an msal application per tenant
type entraBrokerBrowserCredential struct {
	opts      entraBrokerOptions
	library   string
	authority string
	tenant    string
	mu        sync.Mutex
	record    *entraAuthRecord
	app       *entraPublicApp
}

func newEntraBrokerBrowserCredential(library string, opts entraBrokerOptions) (*entraBrokerBrowserCredential, error) {
	host := os.Getenv("AZURE_AUTHORITY_HOST")
	if host == "" {
		host = entraDefaultAuthorityHost
	}
	tenant := opts.tenant
	if opts.record != nil {
		host = opts.record.authority
	}
	authority, err := entraNormalizeAuthority(host)
	if err != nil {
		return nil, err
	}
	if opts.record != nil {
		opts.clientID = opts.record.clientID
		if tenant == "" {
			tenant = opts.record.tenantID
		}
	}
	if tenant == "" {
		tenant = "organizations"
	}
	if err := entraValidateTenant(tenant); err != nil {
		return nil, &entraValueError{msg: err.Error()}
	}
	return &entraBrokerBrowserCredential{opts: opts, library: library, authority: authority, tenant: tenant, record: opts.record}, nil
}

// https origin of an authority, a ValueError for any other scheme
func entraNormalizeAuthority(authority string) (string, error) {
	u, err := url.Parse(authority)
	if err != nil || u.Scheme == "" {
		return "https://" + strings.TrimRight(authority, " /"), nil
	}
	if u.Scheme != "https" {
		return "", &entraValueError{msg: fmt.Sprintf("'%s' is an invalid authority. The value must be a TLS protected (https) URL.", authority)}
	}
	return strings.TrimRight(authority, " /"), nil
}

func (c *entraBrokerBrowserCredential) Name() string { return "InteractiveBrowserBrokerCredential" }

// msal application of the credential's tenant, created on first use with the broker enabled
func (c *entraBrokerBrowserCredential) getApp(ctx context.Context) (*entraPublicApp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.app != nil {
		return c.app, nil
	}
	app, err := newEntraPublicApp(ctx, c.authority+"/"+c.tenant, c.opts.clientID, c.library, true)
	if err != nil {
		return nil, err
	}
	c.app = app
	return app, nil
}

// Application rebuilt with the broker off, the non Windows fallback of a failed interactive broker sign in
func (c *entraBrokerBrowserCredential) disableBroker(ctx context.Context) (*entraPublicApp, error) {
	app, err := newEntraPublicApp(ctx, c.authority+"/"+c.tenant, c.opts.clientID, c.library, false)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.app = app
	c.mu.Unlock()
	return app, nil
}

// Silent token from the msal cache for the record's account, then the broker request when none is usable
func (c *entraBrokerBrowserCredential) GetToken(ctx context.Context, scopes ...string) (EntraAccessToken, error) {
	if len(scopes) == 0 {
		return EntraAccessToken{}, &entraValueError{msg: "'get_token' requires at least one scope"}
	}
	tok, err := c.acquireSilent(ctx, scopes)
	if err == nil {
		return tok, nil
	}
	var required *entraAuthRequiredError
	if !errors.As(err, &required) {
		slog.Default().Warn(fmt.Sprintf("%s.get_token failed: %s", c.Name(), err))
		return EntraAccessToken{}, err
	}
	now := entraNow().Unix()
	result, err := c.requestToken(ctx, scopes)
	if err == nil {
		if _, ok := result["access_token"]; !ok {
			msg := firstNonNil(result["error_description"], result["error"])
			if msg == nil {
				msg = "None"
			}
			err = &entraAuthError{Msg: fmt.Sprintf("Authentication failed: %v", msg)}
		} else {
			var record *entraAuthRecord
			if record, err = entraBuildAuthRecord(result); err == nil {
				c.mu.Lock()
				c.record = record
				c.mu.Unlock()
			}
		}
	}
	if err != nil {
		slog.Default().Warn(fmt.Sprintf("%s.get_token failed: %s", c.Name(), err))
		return EntraAccessToken{}, err
	}
	slog.Default().Info(fmt.Sprintf("%s.get_token succeeded", c.Name()))
	expiresIn, _ := entraInt(result["expires_in"])
	out := EntraAccessToken{Token: fmt.Sprint(result["access_token"]), ExpiresOn: now + expiresIn}
	if on, ok := entraInt(result["refresh_on"]); ok {
		out.RefreshOn = on
	}
	return out, nil
}

// Silent authentication found no token, the condition that lets the credential prompt
type entraAuthRequiredError struct{ msg string }

func (e *entraAuthRequiredError) Error() string { return e.msg }

// Cache or broker refresh for the record's account, AuthenticationRequired when no account or token is usable
func (c *entraBrokerBrowserCredential) acquireSilent(ctx context.Context, scopes []string) (EntraAccessToken, error) {
	c.mu.Lock()
	record := c.record
	c.mu.Unlock()
	var result map[string]any
	if record != nil {
		app, err := c.getApp(ctx)
		if err != nil {
			return EntraAccessToken{}, &entraAuthError{Msg: "Authentication failed: " + err.Error()}
		}
		for _, account := range app.getAccounts(record.username) {
			if account.homeAccountID != record.homeAccountID {
				continue
			}
			now := entraNow().Unix()
			if result, err = app.acquireTokenSilentWithError(ctx, scopes, account); err != nil {
				return EntraAccessToken{}, &entraAuthError{Msg: "Authentication failed: " + err.Error()}
			}
			_, hasToken := result["access_token"]
			expiresIn, hasExpiry := entraInt(result["expires_in"])
			if result != nil && hasToken && hasExpiry {
				out := EntraAccessToken{Token: fmt.Sprint(result["access_token"]), ExpiresOn: now + expiresIn}
				if on, ok := entraInt(result["refresh_on"]); ok {
					out.RefreshOn = on
				}
				slog.Default().Info(fmt.Sprintf("%s.get_token succeeded", c.Name()))
				return out, nil
			}
		}
	}
	msg := "Interactive authentication is required to get a token. Call 'authenticate' to begin."
	return EntraAccessToken{}, &entraAuthRequiredError{msg: msg}
}

// Broker request in the azure-identity order, default account first and the account picker unless disabled
func (c *entraBrokerBrowserCredential) requestToken(ctx context.Context, scopes []string) (map[string]any, error) {
	app, err := c.getApp(ctx)
	if err != nil {
		return nil, &entraAuthError{Msg: "Authentication failed: " + err.Error()}
	}
	result := map[string]any{}
	if c.opts.useDefaultBrokerAccount {
		r, err := c.attempt(ctx, app, scopes, "none")
		if err == nil {
			if _, ok := r["access_token"]; ok {
				return r, nil
			}
			result = r
		} else if c.opts.disableInteractiveFallback {
			return nil, &entraUnavailableError{Msg: entraBrokerDefaultFailure}
		}
		if c.opts.disableInteractiveFallback {
			return result, c.checkResult(ctx, result)
		}
	}
	r, err := c.attempt(ctx, app, scopes, "select_account")
	var bindErr *entraBindError
	switch {
	case errors.As(err, &bindErr):
		return nil, &entraUnavailableError{Msg: "Couldn't start an HTTP server."}
	case err != nil && runtime.GOOS != "windows" && !entraIsWSL():
		fallback, ferr := c.disableBroker(ctx)
		if ferr != nil {
			return nil, &entraAuthError{Msg: "Authentication failed: " + ferr.Error()}
		}
		if r, err = c.attempt(ctx, fallback, scopes, "select_account"); err != nil {
			return nil, &entraAuthError{Msg: "Authentication failed: " + err.Error()}
		}
	case err != nil:
		return nil, &entraUnavailableError{Msg: entraBrokerFailure}
	}
	return r, c.checkResult(ctx, r)
}

// One acquire_token_interactive call with the credential's login hint, timeout, parent window and MSA passthrough
func (c *entraBrokerBrowserCredential) attempt(ctx context.Context, app *entraPublicApp, scopes []string, prompt string) (map[string]any, error) {
	return app.acquireTokenInteractive(ctx, entraInteractiveRequest{scopes: scopes, prompt: prompt, loginHint: c.opts.loginHint,
		timeout: c.opts.timeout, parentWindow: c.opts.parentWindow, enableMSAPassthrough: c.opts.enableMSAPassthrough})
}

// Error description of a tokenless result, unavailable inside the chain and an authentication failure outside it
func (c *entraBrokerBrowserCredential) checkResult(ctx context.Context, result map[string]any) error {
	if _, ok := result["access_token"]; ok {
		return nil
	}
	msg := entraNoUsableAccount
	if d, ok := result["error_description"]; ok {
		msg = fmt.Sprint(d)
		if d == nil {
			msg = "None"
		}
	}
	if entraWithinChain(ctx) {
		return &entraUnavailableError{Msg: msg}
	}
	return &entraAuthError{Msg: msg}
}

// AuthenticationRecord of a token response, from its client_info or the id token subject
func entraBuildAuthRecord(result map[string]any) (*entraAuthRecord, error) {
	fail := &entraAuthError{Msg: "Failed to build AuthenticationRecord from unexpected identity token"}
	claims, ok := result["id_token_claims"].(map[string]any)
	if !ok {
		return nil, fail
	}
	var homeAccountID string
	if raw, ok := result["client_info"].(string); ok {
		info, err := entraDecodeClientInfo(raw)
		if err != nil || info["uid"] == nil || info["utid"] == nil {
			return nil, fail
		}
		homeAccountID = fmt.Sprintf("%v.%v", info["uid"], info["utid"])
	} else if sub, ok := claims["sub"]; ok {
		homeAccountID = fmt.Sprint(sub)
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
	tenant := anyString(claims["tid"])
	if tenant == "" {
		tenant = strings.Trim(issuer.Path, "/")
	}
	username := anyString(claims["preferred_username"])
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
	return &entraAuthRecord{authority: issuer.Host, clientID: fmt.Sprint(aud), homeAccountID: homeAccountID, tenantID: tenant, username: username}, nil
}

// client_info JSON from its unpadded urlsafe base64
func entraDecodeClientInfo(raw string) (map[string]any, error) {
	raw += strings.Repeat("=", (4-len(raw)%4)%4)
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

func entraInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// ---- msal PublicClientApplication ----

// msal public client with its validated authority, broker runtime when enabled, and in memory token cache
type entraPublicApp struct {
	clientID              string
	instance, tenant      string
	authorizationEndpoint string
	tokenEndpoint         string
	knownToDeveloper      bool
	broker                *msalruntime.Runtime
	mu                    sync.Mutex
	accounts              map[string]entraCachedAccount
	accessTokens          map[string]entraCachedToken
	refreshTokens         map[string]entraCachedRefresh
}

// Account entry of the msal token cache
type entraCachedAccount struct {
	homeAccountID, environment, realm, localAccountID, username, authorityType, accountSource string
}

// Access token entry of the msal token cache
type entraCachedToken struct {
	secret, homeAccountID, environment, clientID, target, realm, tokenType string
	expiresOn, refreshOn                                                   int64
}

// Refresh token entry of the msal token cache
type entraCachedRefresh struct {
	secret, homeAccountID, environment, clientID, familyID string
}

// Builds the application, validating the authority through discovery and deciding the broker as msal does
func newEntraPublicApp(ctx context.Context, authorityURL, clientID, library string, enableBroker bool) (*entraPublicApp, error) {
	app := &entraPublicApp{clientID: clientID, accounts: map[string]entraCachedAccount{}, accessTokens: map[string]entraCachedToken{},
		refreshTokens: map[string]entraCachedRefresh{}}
	if err := app.initAuthority(ctx, authorityURL); err != nil {
		return nil, err
	}
	isADFS := strings.EqualFold(app.tenant, "adfs") && !strings.HasSuffix(app.instance, entraCIAMSuffix)
	isB2C := app.isB2C(authorityURL)
	enableBroker = enableBroker && !isADFS && !isB2C
	if enableBroker && runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		enableBroker = false
		slog.Default().Warn("Broker on macOS is supported only on Apple Silicon (arm64). We will fallback to non-broker.")
	}
	if enableBroker {
		rt, err := entraLoadMSALRuntime(library)
		var startup *msalruntime.StartupError
		switch {
		case errors.As(err, &startup):
			slog.Default().Warn("Broker is unavailable on this platform. We will fallback to non-broker.")
		case err != nil:
			return nil, err
		default:
			app.broker = rt
		}
	}
	slog.Default().Debug(fmt.Sprintf("Broker enabled? %s", pyrepr.Repr(app.broker != nil)))
	return app, nil
}

// msal canonicalize of an authority or endpoint URL into host and first path segment
func entraCanonicalize(raw string) (*url.URL, string, string, error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "https" && u.Hostname() != "" {
		parts := strings.Split(u.Path, "/")
		host := strings.ToLower(u.Hostname())
		if strings.HasSuffix(host, entraCIAMSuffix) {
			if len(parts) >= 2 && parts[1] != "" {
				return u, host, parts[1], nil
			}
			return u, host, strings.TrimSuffix(host, entraCIAMSuffix) + ".onmicrosoft.com", nil
		}
		if len(parts) >= 2 && parts[1] != "" {
			return u, host, parts[1], nil
		}
	}
	return nil, "", "", &entraValueError{msg: fmt.Sprintf("Your given address (%s) should consist of an https url with hostname and a "+
		"minimum of one segment in a path: e.g. https://login.microsoftonline.com/{tenant} or https://{tenant_name}.ciamlogin.com/{tenant} "+
		"or https://{tenant_name}.b2clogin.com/{tenant_name}.onmicrosoft.com/policy", raw)}
}

func (app *entraPublicApp) isB2C(authorityURL string) bool {
	u, _, _, err := entraCanonicalize(authorityURL)
	if err != nil {
		return false
	}
	for _, d := range entraB2CHosts {
		if strings.HasSuffix(app.instance, "."+d) {
			return true
		}
	}
	parts := strings.Split(u.Path, "/")
	return len(parts) == 3 && strings.HasPrefix(strings.ToLower(parts[2]), "b2c_")
}

// Instance discovery for hosts Microsoft does not list, then the tenant's OpenID configuration
func (app *entraPublicApp) initAuthority(ctx context.Context, authorityURL string) error {
	u, instance, tenant, err := entraCanonicalize(authorityURL)
	if err != nil {
		return err
	}
	app.instance = instance
	isCIAM := strings.HasSuffix(instance, entraCIAMSuffix)
	isADFS := strings.EqualFold(tenant, "adfs") && !isCIAM
	app.knownToDeveloper = isADFS || app.isB2C(authorityURL)
	client := entraHTTPClient()
	var discovery string
	if !entraWellKnownHosts[instance] && !app.knownToDeveloper {
		q := url.Values{"authorization_endpoint": {"https://" + instance + u.Path + "/oauth2/v2.0/authorize"}, "api-version": {"1.0"}}
		_, payload, err := entraJSONRequest(ctx, client, http.MethodGet, "https://"+entraWorldWide+"/common/discovery/instance?"+q.Encode(), nil, nil)
		if err != nil {
			return err
		}
		if payload["error"] == "invalid_instance" {
			return &entraValueError{msg: fmt.Sprintf("invalid_instance: The authority you provided, %s, is not known. If it is a valid domain "+
				"name known to you, you can turn off this check by passing in instance_discovery=False", authorityURL)}
		}
		discovery = anyString(payload["tenant_discovery_endpoint"])
	} else {
		path := u.Path
		if isCIAM && len(u.Path) <= 1 {
			path = tenant
		}
		version := "/v2.0"
		if isADFS {
			version = ""
		}
		d := *u
		d.Path = path + version + "/.well-known/openid-configuration"
		discovery = d.String()
	}
	config, err := entraTenantDiscovery(ctx, client, discovery)
	if err != nil {
		var ve *entraValueError
		if errors.As(err, &ve) {
			return &entraValueError{msg: fmt.Sprintf("Unable to get authority configuration for %s. Authority would typically be in a format of "+
				"https://login.microsoftonline.com/your_tenant or https://tenant_name.ciamlogin.com or "+
				"https://tenant_name.b2clogin.com/tenant.onmicrosoft.com/policy.  Also please double check your tenant name or GUID is correct.", authorityURL)}
		}
		return err
	}
	app.authorizationEndpoint = anyString(config["authorization_endpoint"])
	app.tokenEndpoint = anyString(config["token_endpoint"])
	_, _, app.tenant, err = entraCanonicalize(app.tokenEndpoint)
	return err
}

// OpenID configuration of a tenant, a ValueError for a 4xx answer and an HTTP error for other failures
func entraTenantDiscovery(ctx context.Context, client *http.Client, endpoint string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, &entraValueError{msg: err.Error()}
		}
		return out, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nil, &entraValueError{msg: fmt.Sprintf("OIDC Discovery failed on %s. HTTP status: %d, Error: %s", endpoint, resp.StatusCode, raw)}
	}
	return nil, fmt.Errorf("%d Server Error: %s for url: %s", resp.StatusCode, http.StatusText(resp.StatusCode), endpoint)
}

// Cached accounts of the authority instance, filtered by username without regard to case
func (app *entraPublicApp) getAccounts(username string) []entraCachedAccount {
	app.mu.Lock()
	defer app.mu.Unlock()
	grouped := map[string]entraCachedAccount{}
	var order []string
	for _, a := range app.accounts {
		if a.environment != app.instance || (a.authorityType != "MSSTS" && a.authorityType != "ADFS") {
			continue
		}
		if _, seen := grouped[a.homeAccountID]; !seen {
			order = append(order, a.homeAccountID)
		}
		grouped[a.homeAccountID] = a
	}
	sort.Strings(order)
	var out []entraCachedAccount
	for _, id := range order {
		a := grouped[id]
		if username == "" || strings.EqualFold(a.username, username) {
			out = append(out, a)
		}
	}
	return out
}

// Scopes with the reserved OpenID scopes msal adds to every user flow
func entraDecorateScopes(scopes []string) []string {
	return append(append([]string{}, scopes...), "offline_access", "openid", "profile")
}

// Unexpired cached token, then a broker refresh for broker accounts, then the account's refresh token
func (app *entraPublicApp) acquireTokenSilentWithError(ctx context.Context, scopes []string, account entraCachedAccount) (map[string]any, error) {
	now := entraNow().Unix()
	var fromCache map[string]any
	app.mu.Lock()
	for _, at := range app.accessTokens {
		if at.clientID != app.clientID || at.environment != app.instance || at.realm != app.tenant || at.homeAccountID != account.homeAccountID {
			continue
		}
		if !entraTargetCovers(at.target, scopes) || at.expiresOn-now < 5*60 {
			continue
		}
		fromCache = map[string]any{"access_token": at.secret, "token_type": at.tokenType, "expires_in": at.expiresOn - now}
		if at.refreshOn != 0 {
			fromCache["refresh_on"] = at.refreshOn
			if at.refreshOn < now {
				break
			}
		}
		app.mu.Unlock()
		return fromCache, nil
	}
	app.mu.Unlock()
	if app.broker != nil && (account.accountSource == entraBrokerGrantType || account.accountSource == "") {
		response, err := app.brokerAcquireTokenSilently(ctx, account.localAccountID, scopes, uuid.NewString())
		if err != nil {
			return nil, err
		}
		if response != nil {
			_, failed := response["error"]
			if account.accountSource == entraBrokerGrantType || !failed {
				return app.processBrokerResponse(response, scopes), nil
			}
		}
	}
	result, err := app.refreshWithCachedToken(ctx, scopes, account)
	if err != nil {
		if fromCache == nil {
			return nil, err
		}
		return fromCache, nil
	}
	if result != nil {
		if _, failed := result["error"]; !failed || fromCache == nil {
			return result, nil
		}
	}
	if fromCache != nil {
		return fromCache, nil
	}
	return result, nil
}

// Whether a cached target holds every requested scope
func entraTargetCovers(target string, scopes []string) bool {
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

// Refresh token grant with the account's family or own refresh tokens, nil when the cache holds none
func (app *entraPublicApp) refreshWithCachedToken(ctx context.Context, scopes []string, account entraCachedAccount) (map[string]any, error) {
	app.mu.Lock()
	var candidates []entraCachedRefresh
	for _, rt := range app.refreshTokens {
		if rt.homeAccountID == account.homeAccountID && rt.environment == app.instance && (rt.familyID == "1" || rt.clientID == app.clientID) {
			candidates = append(candidates, rt)
		}
	}
	app.mu.Unlock()
	var last map[string]any
	anchor := map[string]string{"X-AnchorMailbox": "Oid:" + strings.ReplaceAll(account.homeAccountID, ".", "@")}
	for _, rt := range candidates {
		form := url.Values{"client_id": {app.clientID}, "grant_type": {"refresh_token"}, "refresh_token": {rt.secret},
			"scope": {strings.Join(entraDecorateScopes(scopes), " ")}}
		response, err := app.postToken(ctx, form, anchor)
		if err != nil {
			return nil, err
		}
		if _, failed := response["error"]; !failed {
			if s, ok := response["id_token"].(string); ok {
				response["id_token_claims"] = entraDecodeIDTokenClaims(s)
			}
			app.addToCache(response, scopes, "refresh_token", "", true)
			return entraCleanUp(response), nil
		}
		last = response
	}
	return last, nil
}

// Token endpoint POST returning the decoded response
func (app *entraPublicApp) postToken(ctx context.Context, form url.Values, headers map[string]string) (map[string]any, error) {
	all := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range headers {
		all[k] = v
	}
	status, data, err := entraJSONRequest(ctx, entraHTTPClient(), http.MethodPost, app.tokenEndpoint, all, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("token endpoint answered %d without a JSON body", status)
	}
	return data, nil
}

// One interactive acquisition, its prompt, hint, timeout, parent window and MSA passthrough
type entraInteractiveRequest struct {
	scopes               []string
	prompt, loginHint    string
	timeout              time.Duration
	parentWindow         *msalruntime.WindowHandle
	enableMSAPassthrough bool
}

// acquire_token_interactive, through the broker when enabled and the loopback browser flow otherwise
func (app *entraPublicApp) acquireTokenInteractive(ctx context.Context, req entraInteractiveRequest) (map[string]any, error) {
	if app.broker != nil {
		response, err := app.interactiveViaBroker(ctx, req)
		if err != nil {
			return nil, err
		}
		return app.processBrokerResponse(response, req.scopes), nil
	}
	return app.interactiveViaBrowser(ctx, req)
}

// Broker calls in the order that mimics the OIDC prompt values
func (app *entraPublicApp) interactiveViaBroker(ctx context.Context, req entraInteractiveRequest) (map[string]any, error) {
	authority := "https://" + app.instance + "/" + app.tenant
	if req.loginHint != "" && req.prompt != "select_account" {
		if accounts := app.getAccounts(req.loginHint); len(accounts) == 1 {
			slog.Default().Debug("Calling broker._acquire_token_silently()")
			response, err := app.brokerAcquireTokenSilently(ctx, accounts[0].localAccountID, req.scopes, uuid.NewString())
			if err != nil {
				return nil, err
			}
			if _, failed := response["error"]; response != nil && !failed {
				return response, nil
			}
		}
	}
	if req.prompt == "none" || req.prompt == "" {
		slog.Default().Debug("Calling broker._signin_silently()")
		response, err := app.brokerSignInSilently(ctx, authority, req)
		if err != nil {
			return nil, err
		}
		claims, _ := response["id_token_claims"].(map[string]any)
		_, hasToken := response["access_token"]
		wrong := hasToken && req.loginHint != "" && req.loginHint != anyString(claims["preferred_username"])
		const wrongAccount = `prompt="none" will not work for login_hint="non-default-user"`
		if wrong {
			slog.Default().Debug(wrongAccount)
		}
		if req.prompt == "none" {
			if wrong {
				return map[string]any{"error": "broker_error", "error_description": wrongAccount}, nil
			}
			return response, nil
		}
		status, _ := response["_broker_status"].(msalruntime.Status)
		_, failed := response["error"]
		recoverable := failed && (status == msalruntime.StatusAccountUnusable || status == msalruntime.StatusInteractionRequired)
		if !wrong && !recoverable {
			return response, nil
		}
	}
	slog.Default().Debug("Falls back to broker._signin_interactively()")
	return app.brokerSignInInteractively(ctx, authority, req)
}

// Auth parameters every broker call carries, the client sku, version and redirect URI of msal
func (app *entraPublicApp) brokerParams(authority string, scopes []string) (*msalruntime.AuthParameters, error) {
	params, err := app.broker.NewAuthParameters(app.clientID, authority)
	if err != nil {
		return nil, err
	}
	redirect := entraBrokerRedirectURI
	if runtime.GOOS == "darwin" {
		redirect = entraBrokerRedirectURIMac
	}
	for _, step := range []func() error{
		func() error { return params.SetAdditionalParameter("msal_client_sku", entraMSALSKU) },
		func() error { return params.SetAdditionalParameter("msal_client_ver", entraMSALVersion) },
		func() error { return params.SetRedirectURI(redirect) },
		func() error { return params.SetRequestedScopes(scopes) },
	} {
		if err := step(); err != nil {
			params.Release()
			return nil, err
		}
	}
	return params, nil
}

// validateAuthority parameter msal sends when the authority skips validation
func (app *entraPublicApp) validateAuthority(params *msalruntime.AuthParameters) error {
	if app.knownToDeveloper {
		return params.SetAdditionalParameter("validateAuthority", "no")
	}
	return nil
}

func (app *entraPublicApp) brokerSignInSilently(ctx context.Context, authority string, req entraInteractiveRequest) (map[string]any, error) {
	params, err := app.brokerParams(authority, req.scopes)
	if err != nil {
		return nil, err
	}
	defer params.Release()
	if err := app.validateAuthority(params); err != nil {
		return nil, err
	}
	if req.enableMSAPassthrough {
		if err := params.SetAdditionalParameter("msal_request_type", "consumer_passthrough"); err != nil {
			return nil, err
		}
	}
	result, err := app.broker.SignInSilently(ctx, params, uuid.NewString())
	if err != nil {
		return nil, err
	}
	defer result.Release()
	return app.convertResult(result)
}

// Interactive broker sign in parented on the given window, the console window, or the desktop window
func (app *entraPublicApp) brokerSignInInteractively(ctx context.Context, authority string, req entraInteractiveRequest) (map[string]any, error) {
	params, err := app.brokerParams(authority, req.scopes)
	if err != nil {
		return nil, err
	}
	defer params.Release()
	hint := req.loginHint
	if req.prompt != "" && req.prompt == "select_account" && hint != "" {
		hint = ""
		slog.Default().Warn("Using both select_account and login_hint is ambiguous. Ignoring login_hint.")
	} else if req.prompt != "" && req.prompt != "select_account" {
		slog.Default().Warn(fmt.Sprintf("prompt=%s is not supported by this module", req.prompt))
	}
	var parent msalruntime.WindowHandle
	if req.parentWindow == nil {
		if err := params.SetAdditionalParameter("msal_gui_thread", "true"); err != nil {
			return nil, err
		}
	} else {
		parent = *req.parentWindow
	}
	if req.enableMSAPassthrough {
		if err := params.SetAdditionalParameter("msal_request_type", "consumer_passthrough"); err != nil {
			return nil, err
		}
	}
	if err := app.validateAuthority(params); err != nil {
		return nil, err
	}
	if parent == 0 {
		parent = msalruntime.ConsoleWindow()
	}
	if parent == 0 {
		parent = msalruntime.DesktopWindow()
	}
	result, err := app.broker.SignInInteractively(ctx, parent, params, uuid.NewString(), hint)
	if err != nil {
		return nil, err
	}
	defer result.Release()
	return app.convertResult(result)
}

// Silent token for a broker account read by its id, nil when the broker holds no such account
func (app *entraPublicApp) brokerAcquireTokenSilently(ctx context.Context, accountID string, scopes []string, correlationID string) (map[string]any, error) {
	read, err := app.broker.ReadAccountByID(ctx, accountID, correlationID)
	if err != nil {
		return nil, err
	}
	defer read.Release()
	failure, err := read.Failure()
	if err != nil {
		return nil, err
	}
	if failure != nil {
		if rerr := entraRedirectURIError(failure, "None"); rerr != nil {
			return nil, rerr
		}
		slog.Default().Debug(fmt.Sprintf("read_account_by_id() error: %s", pyrepr.Repr(entraConvertBrokerError(failure))))
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
	result, err := app.broker.AcquireTokenSilently(ctx, params, correlationID, account)
	if err != nil {
		return nil, err
	}
	defer result.Release()
	return app.convertResult(result)
}

// Broker error as the broker_error response msal builds, a RedirectUriError for an unregistered redirect URI
func entraConvertBrokerError(e *msalruntime.Error) map[string]any {
	return map[string]any{"error": "broker_error",
		"error_description": fmt.Sprintf("%s. Status: %s, Error code: %d, Tag: %d", e.Context, e.Status, e.Code, e.Tag),
		"_broker_status":    e.Status, "_broker_error_code": e.Code, "_broker_tag": e.Tag}
}

// RedirectUriError msal raises for a broker error naming an unregistered redirect URI, nil for any other error
func entraRedirectURIError(e *msalruntime.Error, clientID string) error {
	if strings.Contains(e.Context, "AADSTS50011") || strings.Contains(e.Context, "AADSTS7000218") {
		return &entraValueError{msg: fmt.Sprintf("MsalRuntime needs the current app to register these redirect_uri\n"+
			"(1) ms-appx-web://Microsoft.AAD.BrokerPlugin/%s\n(2) %s\n(3) %s", clientID, entraBrokerRedirectURIMac, entraBrokerRedirectURI)}
	}
	return nil
}

// Broker result as the on-the-wire response msal mimics
func (app *entraPublicApp) convertResult(result *msalruntime.AuthResult) (map[string]any, error) {
	if _, err := result.Telemetry(); err != nil {
		return nil, err
	}
	failure, err := result.Failure()
	if err != nil {
		return nil, err
	}
	if failure != nil {
		if rerr := entraRedirectURIError(failure, app.clientID); rerr != nil {
			return nil, rerr
		}
		return entraConvertBrokerError(failure), nil
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
	var token string
	if pop {
		header, err := result.AuthorizationHeader()
		if err != nil {
			return nil, err
		}
		if fields := strings.Fields(header); len(fields) > 1 {
			token = fields[1]
		}
	} else if token, err = result.AccessToken(); err != nil {
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
	clientInfo, err := account.ClientInfo()
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
	out := map[string]any{"access_token": token, "expires_in": expires - entraNow().Unix(), "id_token": rawID,
		"id_token_claims": claims, "client_info": clientInfo, "_account_id": accountID, "token_type": tokenType}
	for k, v := range out {
		if v == "" || v == int64(0) || (k == "id_token_claims" && len(claims) == 0) {
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

// Stores a successful broker response in the token cache as a broker established account
func (app *entraPublicApp) processBrokerResponse(response map[string]any, scopes []string) map[string]any {
	if _, failed := response["error"]; failed {
		return entraCleanUp(response)
	}
	if s, ok := response["scope"].(string); ok {
		scopes = strings.Fields(s)
	}
	app.addToCache(response, scopes, entraBrokerGrantType, anyString(response["_account_id"]), false)
	response["token_source"] = "broker"
	return entraCleanUp(response)
}

// Response without the private keys msal strips before returning it
func entraCleanUp(response map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range response {
		if k != "refresh_in" && !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out
}

// TokenCache.add of a token response, writing the access token, account unless skipped, and refresh token
func (app *entraPublicApp) addToCache(response map[string]any, scopes []string, grantType, accountID string, skipAccount bool) {
	claims, _ := response["id_token_claims"].(map[string]any)
	if claims == nil {
		claims = map[string]any{}
		if idt := anyString(response["id_token"]); idt != "" {
			claims = entraDecodeIDTokenClaims(idt)
		}
	}
	var homeAccountID string
	hasClientInfo := false
	if raw, ok := response["client_info"].(string); ok {
		if info, err := entraDecodeClientInfo(raw); err == nil && info["uid"] != nil && info["utid"] != nil {
			homeAccountID, hasClientInfo = fmt.Sprintf("%v.%v", info["uid"], info["utid"]), true
		}
	}
	if !hasClientInfo && len(claims) > 0 {
		homeAccountID, hasClientInfo = fmt.Sprint(claims["sub"]), true
	}
	sorted := append([]string{}, scopes...)
	sort.Strings(sorted)
	target := strings.Join(sorted, " ")
	now := entraNow().Unix()
	app.mu.Lock()
	defer app.mu.Unlock()
	if token := anyString(response["access_token"]); token != "" {
		expiresIn := int64(600)
		if on, ok := entraInt(response["expires_on"]); ok {
			expiresIn = on - now
		}
		if in, ok := entraInt(response["expires_in"]); ok {
			expiresIn = in
		}
		tokenType := anyString(response["token_type"])
		if tokenType == "" {
			tokenType = "Bearer"
		}
		at := entraCachedToken{secret: token, homeAccountID: homeAccountID, environment: app.instance, clientID: app.clientID,
			target: target, realm: app.tenant, tokenType: tokenType, expiresOn: now + expiresIn}
		if in, ok := entraInt(response["refresh_in"]); ok {
			at.refreshOn = now + in
		}
		app.accessTokens[strings.Join([]string{homeAccountID, app.instance, app.clientID, app.tenant, target}, "|")] = at
	}
	if hasClientInfo && !skipAccount {
		if accountID == "" {
			accountID = anyString(claims["oid"])
			if accountID == "" {
				accountID = anyString(claims["sub"])
			}
		}
		username := anyString(claims["preferred_username"])
		if username == "" {
			username = anyString(claims["upn"])
		}
		authorityType := "MSSTS"
		if app.tenant == "adfs" {
			authorityType = "ADFS"
		}
		account := entraCachedAccount{homeAccountID: homeAccountID, environment: app.instance, realm: app.tenant,
			localAccountID: accountID, username: username, authorityType: authorityType}
		if grantType == entraBrokerGrantType || grantType == "authorization_code" {
			account.accountSource = grantType
		}
		app.accounts[strings.Join([]string{homeAccountID, app.instance, app.tenant}, "|")] = account
	}
	if rt := anyString(response["refresh_token"]); rt != "" {
		entry := entraCachedRefresh{secret: rt, homeAccountID: homeAccountID, environment: app.instance, clientID: app.clientID,
			familyID: anyString(response["foci"])}
		app.refreshTokens[strings.Join([]string{homeAccountID, app.instance, app.clientID}, "|")] = entry
	}
}

// Claims of an unverified id token payload
func entraDecodeIDTokenClaims(idToken string) map[string]any {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	claims, err := entraDecodeClientInfo(parts[1])
	if err != nil {
		return map[string]any{}
	}
	return claims
}

// Loopback authorization code flow with PKCE against the discovered endpoints, the non broker interactive path
func (app *entraPublicApp) interactiveViaBrowser(ctx context.Context, req entraInteractiveRequest) (map[string]any, error) {
	errFn := func(message, code string) *AuthError { return newAuthErr("azure-foundry", message, code, false) }
	handler, result := newLoopbackCallbackHandler("/", "Microsoft Entra ID")
	server, err := bindLoopbackCallbackServer("localhost", 0, handler, errFn, "entra_bind_failed")
	if err != nil {
		return nil, &entraBindError{msg: err.Error()}
	}
	redirect := fmt.Sprintf("http://localhost:%d", server.Port())
	verifier, challenge := generatePKCE()
	state := uuid.NewString()
	scope := strings.Join(entraDecorateScopes(req.scopes), " ")
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(base64.RawURLEncoding.EncodeToString(nonceBytes)))
	nonce := hex.EncodeToString(sum[:])
	q := url.Values{"client_id": {app.clientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {scope},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "nonce": {nonce}}
	if req.prompt != "" {
		q.Set("prompt", req.prompt)
	}
	if req.loginHint != "" {
		q.Set("login_hint", req.loginHint)
	}
	authURI := app.authorizationEndpoint + "?" + q.Encode()
	slog.Default().Info("Open a browser on this device to visit: " + authURI)
	if err := openBrowserURL(authURI); err != nil {
		slog.Default().Warn(fmt.Sprintf("Found no browser in current environment. If this program is being run inside a container "+
			"which either (1) has access to host network (i.e. started by `docker run --net=host -it ...`), or (2) published port %d "+
			"to host network (i.e. started by `docker run -p 127.0.0.1:%d:%d -it ...`), you can use browser on host to visit the "+
			"following link. Otherwise, this auth attempt would either timeout (current timeout setting is %d) or be aborted by "+
			"CTRL+C. Auth URI: %s", server.Port(), server.Port(), server.Port(), int(req.timeout.Seconds()), authURI))
	}
	values, err := serveLoopbackCallback(server, result, req.timeout, errFn, "entra_timeout")
	if err != nil {
		return nil, errors.New("User did not complete the flow in time")
	}
	if e := anyString(values["error"]); e != "" {
		return map[string]any{"error": e, "error_description": firstNonNil(values["error_description"], e)}, nil
	}
	if anyString(values["state"]) != state {
		return map[string]any{"error": "state_mismatch", "error_description": "state mismatch"}, nil
	}
	response, err := app.postToken(ctx, url.Values{"client_id": {app.clientID}, "grant_type": {"authorization_code"},
		"code": {anyString(values["code"])}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "scope": {scope}}, nil)
	if err != nil {
		return nil, err
	}
	if _, failed := response["error"]; !failed {
		if s, ok := response["id_token"].(string); ok {
			claims := entraDecodeIDTokenClaims(s)
			response["id_token_claims"] = claims
			if got := claims["nonce"]; got != nonce {
				shown := "None"
				if got != nil {
					shown = fmt.Sprint(got)
				}
				return nil, fmt.Errorf("The nonce in id token (\"%s\") should match our nonce (\"%s\")", shown, nonce)
			}
		}
		app.addToCache(response, req.scopes, "authorization_code", "", false)
		response["token_source"] = "identity_provider"
	}
	return entraCleanUp(response), nil
}

// Loopback server could not bind, the socket error azure-identity reports as unavailable
type entraBindError struct{ msg string }

func (e *entraBindError) Error() string { return e.msg }

// Context key marking a token request made by the DefaultAzureCredential chain
type entraWithinChainKey struct{}

func entraWithinChain(ctx context.Context) bool {
	v, _ := ctx.Value(entraWithinChainKey{}).(bool)
	return v
}
