package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const (
	SpotifyProvider               = "spotify"
	SpotifyDefaultAccountsBaseURL = "https://accounts.spotify.com"
	SpotifyDefaultAPIBaseURL      = "https://api.spotify.com/v1"
	SpotifyDefaultRedirectURI     = "http://127.0.0.1:43827/spotify/callback"
	SpotifyHelpCommand            = "icebath spotify --help"
	SpotifyDashboardURL           = "https://developer.spotify.com/dashboard"
	// Seconds before expiry a runtime read refreshes the access token
	SpotifyAccessTokenRefreshSkewSeconds = 120
)

// SpotifyDefaultScope is every scope the seven Spotify tools need, space separated
var SpotifyDefaultScope = strings.Join([]string{
	"user-modify-playback-state", "user-read-playback-state", "user-read-currently-playing",
	"user-read-recently-played", "playlist-read-private", "playlist-read-collaborative",
	"playlist-modify-public", "playlist-modify-private", "user-library-read", "user-library-modify",
}, " ")

// SpotifyLoginOptions are the parsed flags of icebath spotify login
type SpotifyLoginOptions struct {
	ClientID    string
	RedirectURI string
	Scope       string
	NoBrowser   bool
	Timeout     float64
}

// Opens a URL in the user's browser for the Spotify flows, swapped by callers that must not launch one
var spotifyOpenBrowser = func(rawURL string) error { return openBrowserFn(rawURL) }

// Reads the pasted Client ID line of the first time setup
var spotifyLineInput = lineInput

func spotifyErr(message, code string, relogin bool) *AuthError {
	return &AuthError{Message: message, Provider: SpotifyProvider, Code: code, ReloginRequired: relogin}
}

// Python str of value or empty string, stripped
func spotifyClean(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return strings.TrimSpace(pyrepr.Str(v))
}

// Requested scope, whitespace normalized and deduplicated in first seen order
func spotifyScopeString(raw string) string {
	if raw == "" {
		raw = SpotifyDefaultScope
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range pystr.SplitWhitespace(raw, -1) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

// First non empty of the explicit value, env vars with .env, the stored state key, then the default
func spotifySetting(ctx context.Context, h string, state map[string]any, key string, envVars []string, def, explicit string, stripSlash bool) (string, error) {
	candidates := []any{explicit}
	for _, name := range envVars {
		v, ok, err := config.GetEnvValue(ctx, h, name)
		if err != nil {
			return "", err
		}
		if ok {
			candidates = append(candidates, v)
		} else {
			candidates = append(candidates, nil)
		}
	}
	if state != nil {
		candidates = append(candidates, state[key])
	} else {
		candidates = append(candidates, nil)
	}
	candidates = append(candidates, def)
	for _, c := range candidates {
		cleaned := spotifyClean(c)
		if stripSlash {
			cleaned = strings.TrimRight(cleaned, "/")
		}
		if cleaned != "" {
			return cleaned, nil
		}
	}
	return def, nil
}

// Client id from the flag, env or stored state, the missing client id error otherwise
func spotifyClientID(ctx context.Context, h, explicit string, state map[string]any) (string, error) {
	id, err := spotifySetting(ctx, h, state, "client_id", []string{"ICEBATH_SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_ID"}, "", explicit, false)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return "", spotifyErr("Spotify client_id is required. Set ICEBATH_SPOTIFY_CLIENT_ID or pass --client-id.", "spotify_client_id_missing", false)
}

func spotifyRedirectURI(ctx context.Context, h, explicit string, state map[string]any) (string, error) {
	return spotifySetting(ctx, h, state, "redirect_uri", []string{"ICEBATH_SPOTIFY_REDIRECT_URI", "SPOTIFY_REDIRECT_URI"}, SpotifyDefaultRedirectURI, explicit, false)
}

func spotifyAPIBaseURL(ctx context.Context, h string, state map[string]any) (string, error) {
	return spotifySetting(ctx, h, state, "api_base_url", []string{"ICEBATH_SPOTIFY_API_BASE_URL"}, SpotifyDefaultAPIBaseURL, "", true)
}

func spotifyAccountsBaseURL(ctx context.Context, h string, state map[string]any) (string, error) {
	return spotifySetting(ctx, h, state, "accounts_base_url", []string{"ICEBATH_SPOTIFY_ACCOUNTS_BASE_URL"}, SpotifyDefaultAccountsBaseURL, "", true)
}

// Authorize URL of the PKCE flow with its query in the order Spotify documents
func spotifyBuildAuthorizeURL(clientID, redirectURI, scope, state, codeChallenge, accountsBaseURL string) string {
	query := pyurl.URLEncode([][2]string{
		{"client_id", clientID}, {"response_type", "code"}, {"redirect_uri", redirectURI},
		{"scope", scope}, {"state", state}, {"code_challenge_method", "S256"}, {"code_challenge", codeChallenge},
	})
	return accountsBaseURL + "/authorize?" + query
}

// Path of a urlparse result, the params after the last segment's semicolon split off
func spotifyURLPath(u pyurl.URL) string {
	path := u.Path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		if j := strings.Index(path[i:], ";"); j >= 0 {
			return path[:i+j]
		}
	} else if j := strings.Index(path, ";"); j >= 0 {
		return path[:j]
	}
	return path
}

// Host, port and path of an http or https loopback redirect URI with an explicit port
func spotifyValidateRedirectURI(redirectURI string) (string, int, string, error) {
	parsed, err := pyurl.Split(redirectURI)
	if err != nil {
		return "", 0, "", err
	}
	host := parsed.Hostname()
	problem := ""
	port := 0
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		problem = "must use http:// or https:// on 127.0.0.1/localhost."
	case host != "127.0.0.1" && host != "localhost":
		problem = "must point to localhost or 127.0.0.1."
	default:
		n, present, err := parsed.Port()
		if err != nil {
			return "", 0, "", err
		}
		if !present || n == 0 {
			problem = "must include an explicit localhost port."
		}
		port = n
	}
	if problem != "" {
		return "", 0, "", spotifyErr("Spotify PKCE redirect_uri "+problem, "spotify_redirect_invalid", false)
	}
	path := spotifyURLPath(parsed)
	if path == "" {
		path = "/"
	}
	return host, port, path, nil
}

// TLS config of the https loopback listener with a self signed 127.0.0.1 certificate cached under the home
func spotifyLoopbackTLSConfig(h string) (*tls.Config, error) {
	cacheDir := filepath.Join(home.CacheDir(h), "spotify-loopback")
	certPath, keyPath := filepath.Join(cacheDir, "cert.pem"), filepath.Join(cacheDir, "key.pem")
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if certErr != nil || keyErr != nil {
		if err := os.MkdirAll(cacheDir, 0o777); err != nil {
			return nil, err
		}
		if err := spotifyWriteLoopbackCertificate(certPath, keyPath); err != nil {
			return nil, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// Writes an RSA 2048 key and a 397 day SHA256 certificate for IP 127.0.0.1 and DNS localhost
func spotifyWriteLoopbackCertificate(certPath, keyPath string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serialBytes := make([]byte, 20)
	fillRandom(serialBytes)
	serial := new(big.Int).Rsh(new(big.Int).SetBytes(serialBytes), 1)
	name := pkix.Name{CommonName: "127.0.0.1"}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: name, Issuer: name,
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(397 * 24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		SignatureAlgorithm: x509.SHA256WithRSA,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.Chmod(keyPath, 0o600)
}

// Serves the loopback redirect, over TLS for an https URI, until the code lands or the timeout passes
func spotifyWaitForCallback(h, redirectURI string, timeoutSeconds float64) (map[string]any, error) {
	host, port, path, err := spotifyValidateRedirectURI(redirectURI)
	if err != nil {
		return nil, err
	}
	errFn := func(message, code string) *AuthError { return spotifyErr(message, code, false) }
	handler, result := newLoopbackCallbackHandler(path, "Spotify")
	server, err := bindLoopbackCallbackServer(host, port, handler, errFn, "spotify_callback_bind_failed")
	if err != nil {
		return nil, err
	}
	if parsed, _ := pyurl.Split(redirectURI); parsed.Scheme == "https" {
		cfg, err := spotifyLoopbackTLSConfig(h)
		if err != nil {
			_ = server.listener.Close()
			return nil, err
		}
		server.listener = tls.NewListener(server.listener, cfg)
		server.server.ErrorLog = log.New(io.Discard, "", 0)
	}
	return serveLoopbackCallback(context.Background(), server, result, time.Duration(timeoutSeconds*float64(time.Second)), errFn, "spotify_callback_timeout")
}

// Whole non negative seconds of an expires_in value, zero when int() of it fails
func spotifyTTLSeconds(v any) int64 {
	n, err := pyint.Of(v)
	if err != nil || n.Sign() < 0 {
		return 0
	}
	if !n.IsInt64() {
		return 0
	}
	return n.Int64()
}

// Python datetime isoformat of a UTC time, the fraction left out at zero microseconds
func spotifyISOFormat(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05-07:00")
	}
	return t.Format("2006-01-02T15:04:05.000000-07:00")
}

// Provider state of a token response merged over the previous state
func spotifyTokenPayloadToState(payload *pyjson.Object, clientID, redirectURI, requestedScope, accountsBaseURL, apiBaseURL string, previous map[string]any) map[string]any {
	now := nowFunc().UTC().Truncate(time.Microsecond)
	expiresIn := spotifyTTLSeconds(payload.GetOr("expires_in", int64(0)))
	expiresAt := now.Add(time.Duration(expiresIn) * time.Second)
	state := map[string]any{}
	for k, v := range previous {
		state[k] = v
	}
	granted := payload.GetOr("scope", nil)
	grantedText := requestedScope
	if pyrepr.Truthy(granted) {
		grantedText = pyrepr.Str(granted)
	}
	tokenType := payload.GetOr("token_type", "Bearer")
	if !pyrepr.Truthy(tokenType) {
		tokenType = "Bearer"
	}
	cleanedType := spotifyClean(tokenType)
	if cleanedType == "" {
		cleanedType = "Bearer"
	}
	refresh := payload.GetOr("refresh_token", nil)
	if !pyrepr.Truthy(refresh) {
		refresh = state["refresh_token"]
	}
	state["client_id"] = clientID
	state["redirect_uri"] = redirectURI
	state["accounts_base_url"] = accountsBaseURL
	state["api_base_url"] = apiBaseURL
	state["scope"] = requestedScope
	state["granted_scope"] = strings.TrimSpace(grantedText)
	state["token_type"] = cleanedType
	state["access_token"] = spotifyClean(payload.GetOr("access_token", nil))
	state["refresh_token"] = spotifyClean(refresh)
	state["obtained_at"] = spotifyISOFormat(now)
	state["expires_at"] = spotifyISOFormat(expiresAt)
	state["expires_in"] = expiresIn
	state["auth_type"] = "oauth_pkce"
	return state
}

// Error of a Spotify request that failed in transit, its httpx class name and text
type spotifyTransportError struct {
	name string
	text string
}

func (e *spotifyTransportError) Error() string      { return e.text }
func (e *spotifyTransportError) PyTypeName() string { return e.name }

// httpx exception of a request that failed in transit, timeouts named by their phase
func spotifyHTTPXError(err error) error {
	var te *pyhttpx.TimeoutError
	if errors.As(err, &te) {
		name := "ReadTimeout"
		switch te.Phase {
		case "connect":
			name = "ConnectTimeout"
		case "write":
			name = "WriteTimeout"
		case "pool":
			name = "PoolTimeout"
		}
		return &spotifyTransportError{name: name, text: pyhttpx.ErrorText(err)}
	}
	return &spotifyTransportError{name: pkceTransportErrorName(err), text: pyhttpx.ErrorText(err)}
}

// SpotifyHTTPDo sends one Spotify request like a default httpx call, failures named as the httpx exception
func SpotifyHTTPDo(ctx context.Context, method, rawURL string, body io.Reader, headers map[string]string, timeoutSeconds float64) (*pyhttpx.Response, error) {
	if msg := pyhttpx.ProtocolError(rawURL); msg != "" {
		return nil, &spotifyTransportError{name: "UnsupportedProtocol", text: msg}
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, &spotifyTransportError{name: "InvalidURL", text: err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := pyhttpx.Do(pyhttpx.PlainClient(false), req, time.Duration(timeoutSeconds*float64(time.Second)))
	if err != nil {
		return nil, spotifyHTTPXError(err)
	}
	return resp, nil
}

type spotifyTokenPost struct {
	what, failedCode, invalidCode, invalidMessage, failedSuffix string
	reloginRequired                                             bool
}

// Posts Spotify's token endpoint and returns the JSON object carrying an access token
func spotifyPostToken(ctx context.Context, accountsBaseURL string, data [][2]string, timeoutSeconds float64, p spotifyTokenPost) (*pyjson.Object, error) {
	resp, err := SpotifyHTTPDo(ctx, http.MethodPost, accountsBaseURL+"/api/token", strings.NewReader(pyurl.URLEncode(data)),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, timeoutSeconds)
	if err != nil {
		return nil, spotifyErr(fmt.Sprintf("Spotify %s failed: %s", p.what, err.Error()), p.failedCode, false)
	}
	if resp.StatusCode >= 400 {
		detail := strings.TrimSpace(resp.Text())
		message := fmt.Sprintf("Spotify %s failed.%s", p.what, p.failedSuffix)
		if detail != "" {
			message += " Response: " + detail
		}
		return nil, spotifyErr(message, p.failedCode, p.reloginRequired)
	}
	payload, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	obj, ok := payload.(*pyjson.Object)
	if !ok || spotifyClean(obj.GetOr("access_token", nil)) == "" {
		return nil, spotifyErr(p.invalidMessage, p.invalidCode, p.reloginRequired)
	}
	return obj, nil
}

// Refreshes a stored Spotify login and returns the merged provider state
var spotifyRefreshOAuthState = func(ctx context.Context, h string, state map[string]any, timeoutSeconds float64) (map[string]any, error) {
	refreshToken := spotifyClean(state["refresh_token"])
	if refreshToken == "" {
		return nil, spotifyErr("Spotify refresh token missing. Run `icebath spotify login` again.", "spotify_refresh_token_missing", true)
	}
	clientID, err := spotifyClientID(ctx, h, "", state)
	if err != nil {
		return nil, err
	}
	accountsBaseURL, err := spotifyAccountsBaseURL(ctx, h, state)
	if err != nil {
		return nil, err
	}
	payload, err := spotifyPostToken(ctx, accountsBaseURL,
		[][2]string{{"grant_type", "refresh_token"}, {"refresh_token", refreshToken}, {"client_id", clientID}},
		timeoutSeconds, spotifyTokenPost{what: "token refresh", failedCode: "spotify_refresh_failed",
			invalidCode: "spotify_refresh_invalid", invalidMessage: "Spotify refresh response did not include an access_token.",
			failedSuffix: " Run `icebath spotify login` again.", reloginRequired: true})
	if err != nil {
		return nil, err
	}
	redirectURI, err := spotifyRedirectURI(ctx, h, "", state)
	if err != nil {
		return nil, err
	}
	apiBaseURL, err := spotifyAPIBaseURL(ctx, h, state)
	if err != nil {
		return nil, err
	}
	requested := SpotifyDefaultScope
	if pyrepr.Truthy(state["scope"]) {
		requested = pyrepr.Str(state["scope"])
	}
	return spotifyTokenPayloadToState(payload, clientID, redirectURI, requested, accountsBaseURL, apiBaseURL, state), nil
}

// SpotifyResolveRuntimeCredentials reads the stored login, refreshing it when forced or expiring, and returns the API credentials
func SpotifyResolveRuntimeCredentials(ctx context.Context, h string, forceRefresh, refreshIfExpiring bool) (map[string]any, error) {
	store := NewStore(h)
	unlock, err := store.Lock(AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	state, err := func() (map[string]any, error) {
		defer unlock()
		data, err := store.Load()
		if err != nil {
			return nil, err
		}
		state := store.ProviderState(data, SpotifyProvider)
		if len(state) == 0 {
			return nil, spotifyErr("Spotify is not authenticated. Run `icebath spotify login` first.", "spotify_auth_missing", true)
		}
		shouldRefresh := forceRefresh
		if !shouldRefresh && refreshIfExpiring {
			shouldRefresh = IsExpiring(state["expires_at"], SpotifyAccessTokenRefreshSkewSeconds*time.Second)
		}
		if !shouldRefresh {
			return state, nil
		}
		refreshed, err := spotifyRefreshOAuthState(ctx, h, state, 20.0)
		if err == nil {
			state = refreshed
			store.StoreProviderState(data, SpotifyProvider, state)
			if err = store.Save(data); err == nil {
				return state, nil
			}
		}
		var authErr *AuthError
		if errors.As(err, &authErr) && authErr.ReloginRequired && pyrepr.Truthy(state["refresh_token"]) {
			quarantineFlatOAuthState(state, SpotifyProvider, authErr)
			store.StoreProviderState(data, SpotifyProvider, state)
			if saveErr := store.Save(data); saveErr != nil {
				store.Log.Debug(fmt.Sprintf("Spotify OAuth: failed to persist quarantined state: %v", saveErr))
			}
		}
		return nil, err
	}()
	if err != nil {
		return nil, err
	}
	accessToken := spotifyClean(state["access_token"])
	if accessToken == "" {
		return nil, spotifyErr("Spotify access token missing. Run `icebath spotify login` again.", "spotify_access_token_missing", true)
	}
	baseURL, err := spotifyAPIBaseURL(ctx, h, state)
	if err != nil {
		return nil, err
	}
	clientID, err := spotifyClientID(ctx, h, "", state)
	if err != nil {
		return nil, err
	}
	redirectURI, err := spotifyRedirectURI(ctx, h, "", state)
	if err != nil {
		return nil, err
	}
	tokenType := "Bearer"
	if v, ok := state["token_type"]; ok && pyrepr.Truthy(v) {
		tokenType = pyrepr.Str(v)
	}
	scope := state["granted_scope"]
	if !pyrepr.Truthy(scope) {
		scope = state["scope"]
	}
	return map[string]any{
		"provider": SpotifyProvider, "access_token": accessToken, "api_key": accessToken, "token_type": tokenType,
		"base_url": baseURL, "scope": spotifyClean(scope), "client_id": clientID, "redirect_uri": redirectURI,
		"expires_at": state["expires_at"], "refresh_token": spotifyClean(state["refresh_token"]),
	}, nil
}

// SpotifyAuthStatus reports whether the home holds a usable Spotify login and its stored fields
func SpotifyAuthStatus(h string) (map[string]any, error) {
	state, err := NewStore(h).GetProviderAuthState(SpotifyProvider)
	if err != nil {
		return nil, err
	}
	if len(state) == 0 {
		return map[string]any{"logged_in": false}, nil
	}
	expiresAt := state["expires_at"]
	refreshToken := spotifyClean(state["refresh_token"])
	authType, ok := state["auth_type"]
	if !ok {
		authType = "oauth_pkce"
	}
	scope := state["granted_scope"]
	if !pyrepr.Truthy(scope) {
		scope = state["scope"]
	}
	return map[string]any{
		"logged_in": refreshToken != "" || !IsExpiring(expiresAt, 0), "auth_type": authType,
		"client_id": state["client_id"], "redirect_uri": state["redirect_uri"], "scope": scope,
		"expires_at": expiresAt, "api_base_url": state["api_base_url"], "has_refresh_token": refreshToken != "",
	}, nil
}

// SpotifyLogout drops the stored Spotify login and reports whether anything was removed
func SpotifyLogout(h string) (bool, error) { return NewStore(h).ClearProviderAuth(SpotifyProvider) }

// Walks the user through creating a Spotify app and saves the pasted client id to the profile .env
func spotifyInteractiveSetup(ctx context.Context, h string, out io.Writer, redirectURIHint string) (string, error) {
	rule := strings.Repeat("=", 70)
	fmt.Fprintf(out, "\n%s\nSpotify first-time setup\n%s\n\n"+
		"Spotify requires every user to register their own lightweight\n"+
		"developer app. This takes about two minutes and only has to be\n"+
		"done once per machine.\n\n"+
		"Login help: %s\n\n"+
		"Steps:\n"+
		"  1. Opening %s in your browser...\n"+
		"  2. Click 'Create app' and fill in:\n"+
		"       App name:     anything (e.g. icebath-agent)\n"+
		"       Description:  anything\n"+
		"       Redirect URI: %s\n"+
		"       API/SDK:      Web API\n"+
		"  3. Agree to the terms, click Save.\n"+
		"  4. Open the app's Settings page and copy the Client ID.\n"+
		"  5. Paste it below.\n\n", rule, rule, SpotifyHelpCommand, SpotifyDashboardURL, redirectURIHint)
	if !isRemoteSession() {
		_ = spotifyOpenBrowser(SpotifyDashboardURL)
	}
	line, err := spotifyLineInput("Spotify Client ID: ")
	if err != nil {
		if errors.Is(err, cliui.ErrEOF) || errors.Is(err, cliui.ErrInterrupt) {
			fmt.Fprintln(out)
			return "", &AuthExitError{Code: 1, Message: "Spotify setup cancelled."}
		}
		return "", err
	}
	raw := strings.TrimSpace(line)
	if raw == "" {
		fmt.Fprintf(out, "\nNo Client ID entered. See %s for the login options.\n", SpotifyHelpCommand)
		return "", &AuthExitError{Code: 1, Message: "Spotify setup cancelled: empty Client ID."}
	}
	if err := config.SaveEnvValueContext(ctx, h, "ICEBATH_SPOTIFY_CLIENT_ID", raw); err != nil {
		return "", err
	}
	if redirectURIHint != "" && redirectURIHint != SpotifyDefaultRedirectURI {
		if err := config.SaveEnvValueContext(ctx, h, "ICEBATH_SPOTIFY_REDIRECT_URI", redirectURIHint); err != nil {
			return "", err
		}
	}
	fmt.Fprint(out, "\nSaved ICEBATH_SPOTIFY_CLIENT_ID to the profile's .env\n\n")
	return raw, nil
}

// Hex of a random version 4 UUID, the uuid.uuid4().hex analogue
func spotifyUUID4Hex() string {
	b := make([]byte, 16)
	fillRandom(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

// SpotifyLogin runs the loopback PKCE login, first time setup included, and stores the tokens under providers.spotify
func SpotifyLogin(ctx context.Context, h string, opts SpotifyLoginOptions, out io.Writer) error {
	store := NewStore(h)
	existing, err := store.GetProviderAuthState(SpotifyProvider)
	if err != nil {
		return err
	}
	if existing == nil {
		existing = map[string]any{}
	}
	clientID, err := spotifyClientID(ctx, h, opts.ClientID, existing)
	if err != nil {
		var authErr *AuthError
		if !errors.As(err, &authErr) || authErr.Code != "spotify_client_id_missing" {
			return err
		}
		hint := opts.RedirectURI
		if hint == "" {
			hint = SpotifyDefaultRedirectURI
		}
		if clientID, err = spotifyInteractiveSetup(ctx, h, out, hint); err != nil {
			return err
		}
	}
	redirectURI, err := spotifyRedirectURI(ctx, h, opts.RedirectURI, existing)
	if err != nil {
		return err
	}
	rawScope := opts.Scope
	if rawScope == "" && pyrepr.Truthy(existing["scope"]) {
		rawScope = pyrepr.Str(existing["scope"])
	}
	scope := spotifyScopeString(rawScope)
	accountsBaseURL, err := spotifyAccountsBaseURL(ctx, h, existing)
	if err != nil {
		return err
	}
	apiBaseURL, err := spotifyAPIBaseURL(ctx, h, existing)
	if err != nil {
		return err
	}
	codeVerifier := pkceCodeVerifier(64)
	stateNonce := spotifyUUID4Hex()
	authorizeURL := spotifyBuildAuthorizeURL(clientID, redirectURI, scope, stateNonce, pkceCodeChallenge(codeVerifier), accountsBaseURL)
	fmt.Fprintf(out, "Starting Spotify PKCE login...\nClient ID: %s\nRedirect URI: %s\n"+
		"Make sure this redirect URI is allow-listed in your Spotify app settings.\n\n"+
		"Open this URL to authorize Icebath:\n%s\n\nLogin help: %s\n\n", clientID, redirectURI, authorizeURL, SpotifyHelpCommand)
	printLoopbackSSHHint(out, redirectURI, SpotifyHelpCommand)
	if !opts.NoBrowser && !isRemoteSession() && canOpenGraphicalBrowser() {
		if spotifyOpenBrowser(authorizeURL) == nil {
			fmt.Fprintln(out, "Browser opened for Spotify authorization.")
		} else {
			fmt.Fprintln(out, "Could not open the browser automatically; use the URL above.")
		}
	}
	callbackTimeout := opts.Timeout
	if callbackTimeout == 0 {
		callbackTimeout = 180.0
	}
	callback, err := spotifyWaitForCallback(h, redirectURI, callbackTimeout)
	if err != nil {
		return err
	}
	if pyrepr.Truthy(callback["error"]) {
		detail := callback["error_description"]
		if !pyrepr.Truthy(detail) {
			detail = callback["error"]
		}
		return &AuthExitError{Code: 1, Message: "Spotify authorization failed: " + pyrepr.Str(detail)}
	}
	if got, _ := callback["state"].(string); got != stateNonce {
		return &AuthExitError{Code: 1, Message: "Spotify authorization failed: state mismatch."}
	}
	tokenTimeout := opts.Timeout
	if tokenTimeout == 0 {
		tokenTimeout = 20.0
	}
	code := ""
	if pyrepr.Truthy(callback["code"]) {
		code = pyrepr.Str(callback["code"])
	}
	payload, err := spotifyPostToken(ctx, accountsBaseURL, [][2]string{
		{"client_id", clientID}, {"grant_type", "authorization_code"}, {"code", code},
		{"redirect_uri", redirectURI}, {"code_verifier", codeVerifier},
	}, tokenTimeout, spotifyTokenPost{what: "token exchange", failedCode: "spotify_token_exchange_failed",
		invalidCode: "spotify_token_exchange_invalid", invalidMessage: "Spotify token response did not include an access_token."})
	if err != nil {
		return err
	}
	spotifyState := spotifyTokenPayloadToState(payload, clientID, redirectURI, scope, accountsBaseURL, apiBaseURL, nil)
	unlock, err := store.Lock(AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := store.Load()
	if err != nil {
		return err
	}
	store.StoreProviderState(data, SpotifyProvider, spotifyState)
	if err := store.Save(data); err != nil {
		return err
	}
	fmt.Fprintf(out, "Spotify login successful!\n  Auth state: %s\n  Provider state saved under providers.spotify\n  Help: %s\n", store.Path(), SpotifyHelpCommand)
	return nil
}
