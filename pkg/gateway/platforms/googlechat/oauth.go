package googlechat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	userOAuthScope   = "https://www.googleapis.com/auth/chat.messages.create"
	oauthRedirectURI = "http://localhost:1"
	googleAuthURI    = "https://accounts.google.com/o/oauth2/auth"
	googleRevokeURL  = "https://oauth2.googleapis.com/revoke"
	legacyIdentity   = "__legacy__"
	userTokensDir    = "google_chat_user_tokens"
	legacyTokenFile  = "google_chat_user_token.json"
	clientSecretFile = "google_chat_user_client_secret.json"
	pendingDir       = "google_chat_user_oauth_pending"
	legacyPending    = "google_chat_user_oauth_pending.json"
	verifierAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	stateAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

var emailFSRe = regexp.MustCompile(`[^a-z0-9._@-]+`)

// Lowercase file safe token key of an email
func sanitizeEmail(email string) string {
	cleaned := emailFSRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(email)), "_")
	if cleaned == "" {
		return "_unknown_"
	}
	return cleaned
}

// Home relative token file, per user under the tokens directory and the legacy file without an email
func tokenRel(email string) string {
	if email == "" {
		return legacyTokenFile
	}
	return userTokensDir + "/" + sanitizeEmail(email) + ".json"
}

func tokenPath(ctx context.Context, email string) string {
	return filepath.Join(home.Dir(ctx), filepath.FromSlash(tokenRel(email)))
}

func clientSecretPath(ctx context.Context) string { return filepath.Join(home.Dir(ctx), clientSecretFile) }

// Pending PKCE state file, one per user so several users can be mid flow
func pendingPath(ctx context.Context, email string) string {
	if email == "" {
		return filepath.Join(home.Dir(ctx), legacyPending)
	}
	return filepath.Join(home.Dir(ctx), pendingDir, sanitizeEmail(email)+".json")
}

// Writes JSON atomically with 0600 permissions under a 0700 directory
func writePrivateJSON(ctx context.Context, path string, data any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		return err
	}
	return writeFileAtomic(ctx, path, bytes.TrimRight(buf.Bytes(), "\n"), 0o600, 0o700)
}

// Writes a file through a temp file and rename so readers never see a partial write
func writeFileAtomic(ctx context.Context, path string, data []byte, mode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if _, err := home.MakeUserDir(ctx, dir, dirMode, true, true); err != nil {
		return err
	}
	if dirMode&0o077 == 0 {
		_ = os.Chmod(dir, dirMode)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	_ = os.Chmod(path, mode)
	return nil
}

// Authorized user token file in the layout the Google auth libraries write
type tokenFile struct {
	Token          string   `json:"token,omitempty"`
	RefreshToken   string   `json:"refresh_token,omitempty"`
	TokenURI       string   `json:"token_uri,omitempty"`
	ClientID       string   `json:"client_id,omitempty"`
	ClientSecret   string   `json:"client_secret,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	RaptToken      string   `json:"rapt_token,omitempty"`
	UniverseDomain string   `json:"universe_domain,omitempty"`
	Account        string   `json:"account"`
	Expiry         string   `json:"expiry,omitempty"`
	Type           string   `json:"type,omitempty"`
}

// Expiry of the stored access token, zero when the file records none
func (t *tokenFile) expiryTime() time.Time {
	raw := strings.SplitN(strings.TrimSuffix(t.Expiry, "Z"), ".", 2)[0]
	if raw == "" {
		return time.Time{}
	}
	ts, err := time.ParseInLocation("2006-01-02T15:04:05", raw, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// One user's OAuth grant, refreshed on demand and written back to its token file
type userCredentials struct {
	path string
	mu   sync.Mutex
	data tokenFile
}

// Whether the stored access token is present and outside the refresh threshold
func (u *userCredentials) valid() bool {
	if u.data.Token == "" {
		return false
	}
	exp := u.data.expiryTime()
	return exp.IsZero() || time.Now().UTC().Before(exp.Add(-refreshThreshold))
}

// Mints a fresh access token with the refresh token and persists the grant
func (u *userCredentials) refresh(ctx context.Context) error {
	if u.data.RefreshToken == "" {
		return errors.New("token has no refresh_token")
	}
	tokenURI := u.data.TokenURI
	if tokenURI == "" {
		tokenURI = oauthTokenEndpoint
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {u.data.ClientID}, "client_secret": {u.data.ClientSecret},
		"refresh_token": {u.data.RefreshToken}}
	if u.data.RaptToken != "" {
		form.Set("rapt", u.data.RaptToken)
	}
	data, err := postToken(ctx, tokenURI, form, nil, nil)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	token, exp, err := tokenFrom(data, now)
	if err != nil {
		return err
	}
	u.data.Token = token
	u.data.Expiry = ""
	if !exp.IsZero() {
		u.data.Expiry = exp.Format("2006-01-02T15:04:05.999999") + "Z"
	}
	if rt := str(data, "refresh_token"); rt != "" {
		u.data.RefreshToken = rt
	}
	if scope := str(data, "scope"); scope != "" {
		u.data.Scopes = strings.Fields(scope)
	}
	if u.data.Type == "" {
		u.data.Type = "authorized_user"
	}
	if err := writePrivateJSON(ctx, u.path, u.data); err != nil {
		slog.Debug(fmt.Sprintf("[google_chat_user_oauth] failed to persist credentials at %s: %v", u.path, err))
	}
	return nil
}

// Whether the grant yields a usable access token, refreshing it when needed
func (u *userCredentials) usable(ctx context.Context) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.valid() {
		return true
	}
	if err := u.refresh(ctx); err != nil {
		slog.Warn(fmt.Sprintf("[google_chat_user_oauth] token refresh failed (user should re-run /setup-files): %v", err))
		return false
	}
	return true
}

// Sets the user bearer token, refreshing first when it has expired
func (u *userCredentials) authorize(ctx context.Context, h http.Header) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.valid() {
		if err := u.refresh(ctx); err != nil {
			return err
		}
	}
	h.Set("Authorization", "Bearer "+u.data.Token)
	return nil
}

// Stored grant of the user, or the legacy grant without an email, nil when absent, corrupt or no longer refreshable
func loadUserCredentials(ctx context.Context, email string) *userCredentials {
	path := tokenPath(ctx, email)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	config.WarnIfCredentialFileBroadlyReadable(path, "[google_chat_user_oauth]", nil)
	var data tokenFile
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data); err != nil {
		slog.Warn(fmt.Sprintf("[google_chat_user_oauth] token at %s is corrupt: %v", path, err))
		return nil
	}
	if data.RefreshToken == "" || data.ClientID == "" || data.ClientSecret == "" {
		slog.Warn(fmt.Sprintf("[google_chat_user_oauth] token at %s is corrupt: missing refresh_token, client_id or client_secret", path))
		return nil
	}
	u := &userCredentials{path: path, data: data}
	if !u.usable(ctx) {
		return nil
	}
	return u
}

// Sanitized emails with a stored per user token, sorted
func listAuthorizedEmails(ctx context.Context) []string {
	entries, err := os.ReadDir(filepath.Join(home.Dir(ctx), userTokensDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}

// A step the user has to fix or redo, its lines printed by the CLI before exiting 1
type oauthFailure struct{ lines []string }

func (e *oauthFailure) Error() string { return strings.Join(e.lines, "\n") }

func fail(lines ...string) error { return &oauthFailure{lines: lines} }

// OAuth client of a stored client_secret.json, the web entry preferred over installed
type oauthClient struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthURI      string `json:"auth_uri"`
	TokenURI     string `json:"token_uri"`
}

// Client of the stored client secret
func loadOAuthClient(ctx context.Context) (*oauthClient, error) {
	raw, err := os.ReadFile(clientSecretPath(ctx))
	if err != nil {
		return nil, fail("ERROR: No client secret stored. Run --client-secret first.")
	}
	var data map[string]*oauthClient
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data); err != nil {
		return nil, fmt.Errorf("client secret is not valid JSON: %w", err)
	}
	client := data["web"]
	if client == nil {
		client = data["installed"]
	}
	if client == nil || client.ClientID == "" {
		return nil, errors.New("client secrets must be for a web or installed app")
	}
	if client.AuthURI == "" {
		client.AuthURI = googleAuthURI
	}
	if client.TokenURI == "" {
		client.TokenURI = oauthTokenEndpoint
	}
	return client, nil
}

// Validates an OAuth client_secret.json and stores a private copy in the home, answering the stored path
func storeClientSecret(ctx context.Context, path string) (string, error) {
	src, err := filepath.Abs(expandUser(path))
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", fail("ERROR: File not found: " + src)
	}
	var data map[string]any
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data) != nil {
		return "", fail("ERROR: File is not valid JSON.")
	}
	_, installed := data["installed"]
	_, web := data["web"]
	if !installed && !web {
		return "", fail("ERROR: Not a Google OAuth client secret file (missing 'installed' or 'web' key).",
			"Download from: https://console.cloud.google.com/apis/credentials")
	}
	target := clientSecretPath(ctx)
	if err := writePrivateJSON(ctx, target, data); err != nil {
		return "", err
	}
	return target, nil
}

// Path with a leading tilde expanded to the user home
func expandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, path[1:])
		}
	}
	return path
}

// Uniform random string over the alphabet from the system CSPRNG
func randomString(alphabet string, n int) (string, error) {
	out := make([]byte, n)
	limit := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out), nil
}

// Pending PKCE session of one user
type pendingAuth struct {
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
	Email        string `json:"email"`
}

// OAuth consent URL for the user, its PKCE state persisted under the user's key
func authURL(ctx context.Context, email string) (string, error) {
	client, err := loadOAuthClient(ctx)
	if err != nil {
		return "", err
	}
	verifier, err := randomString(verifierAlphabet, 128)
	if err != nil {
		return "", err
	}
	state, err := randomString(stateAlphabet, 30)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {oauthRedirectURI},
		"scope": {userOAuthScope}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"}, "access_type": {"offline"}, "prompt": {"consent"}}
	if err := writePrivateJSON(ctx, pendingPath(ctx, email), pendingAuth{State: state, CodeVerifier: verifier,
		RedirectURI: oauthRedirectURI, Email: email}); err != nil {
		return "", err
	}
	sep := "?"
	if strings.Contains(client.AuthURI, "?") {
		sep = "&"
	}
	return client.AuthURI + sep + q.Encode(), nil
}

// Pending PKCE session of the user
func loadPendingAuth(ctx context.Context, email string) (*pendingAuth, error) {
	raw, err := os.ReadFile(pendingPath(ctx, email))
	if os.IsNotExist(err) {
		return nil, fail("ERROR: No pending OAuth session found. Run --auth-url first.")
	}
	var p pendingAuth
	if err == nil {
		err = json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &p)
	}
	if err != nil {
		return nil, fail("ERROR: Could not read pending OAuth session: "+err.Error(), "Run --auth-url again to start a fresh session.")
	}
	if p.State == "" || p.CodeVerifier == "" {
		return nil, fail("ERROR: Pending OAuth session is missing PKCE data.", "Run --auth-url again.")
	}
	return &p, nil
}

// Exchanges an auth code or the pasted failed redirect URL for a stored refresh token, answering the token path
func exchangeAuthCode(ctx context.Context, codeOrURL, email string) (string, error) {
	client, err := loadOAuthClient(ctx)
	if err != nil {
		return "", err
	}
	pending, err := loadPendingAuth(ctx, email)
	if err != nil {
		return "", err
	}
	code, state, urlScope := codeOrURL, "", ""
	if strings.HasPrefix(codeOrURL, "http") {
		parsed, err := url.Parse(codeOrURL)
		if err != nil {
			return "", fail("ERROR: No 'code' parameter found in URL.")
		}
		q := parsed.Query()
		if !q.Has("code") {
			return "", fail("ERROR: No 'code' parameter found in URL.")
		}
		code, state, urlScope = q.Get("code"), q.Get("state"), strings.TrimSpace(q.Get("scope"))
	}
	if state != "" && state != pending.State {
		return "", fail("ERROR: OAuth state mismatch. Run --auth-url again to start a fresh session.")
	}
	redirect := pending.RedirectURI
	if redirect == "" {
		redirect = oauthRedirectURI
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {client.ClientID}, "client_secret": {client.ClientSecret}, "code_verifier": {pending.CodeVerifier}}
	now := time.Now().UTC()
	data, err := postToken(ctx, client.TokenURI, form, nil, nil)
	if err != nil {
		return "", fail("ERROR: Token exchange failed: "+err.Error(), "The code may have expired. Run --auth-url to get a fresh URL.")
	}
	token, exp, err := tokenFrom(data, now)
	if err != nil {
		return "", fail("ERROR: Token exchange failed: "+err.Error(), "The code may have expired. Run --auth-url to get a fresh URL.")
	}
	scopes := []string{userOAuthScope}
	switch {
	case str(data, "scope") != "":
		scopes = strings.Fields(str(data, "scope"))
	case urlScope != "":
		scopes = strings.Fields(urlScope)
	}
	payload := tokenFile{Token: token, RefreshToken: str(data, "refresh_token"), TokenURI: client.TokenURI, ClientID: client.ClientID,
		ClientSecret: client.ClientSecret, Scopes: scopes, UniverseDomain: defaultUniverse, Type: "authorized_user"}
	if !exp.IsZero() {
		payload.Expiry = exp.Format("2006-01-02T15:04:05.999999") + "Z"
	}
	path := tokenPath(ctx, email)
	if err := writePrivateJSON(ctx, path, payload); err != nil {
		return "", err
	}
	_ = os.Remove(pendingPath(ctx, email))
	return path, nil
}

// Revokes the stored grant with Google and deletes it locally, answering the lines describing each step
func revokeUserToken(ctx context.Context, email string) ([]string, error) {
	path := tokenPath(ctx, email)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []string{"No token to revoke."}, nil
	}
	if err != nil {
		return nil, err
	}
	var lines []string
	var data tokenFile
	revokeErr := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data)
	if revokeErr == nil {
		revokeErr = revokeGrant(ctx, data)
	}
	if revokeErr == nil {
		lines = append(lines, "Token revoked with Google.")
	} else {
		lines = append(lines, "Remote revocation failed (token may already be invalid): "+revokeErr.Error())
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	_ = os.Remove(pendingPath(ctx, email))
	return append(lines, "Deleted "+path), nil
}

// Posts the refresh token, or the access token when there is none, to the Google revoke endpoint
func revokeGrant(ctx context.Context, data tokenFile) error {
	token := data.RefreshToken
	if token == "" {
		token = data.Token
	}
	if token == "" {
		return errors.New("token file holds no token")
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, googleRevokeURL+"?"+url.Values{"token": {token}}.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := authHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP Error %d", resp.StatusCode)
	}
	return nil
}

// Status line of the user's stored grant and whether it is usable
func checkUserAuth(ctx context.Context, email string) (string, bool) {
	path := tokenPath(ctx, email)
	if _, err := os.Stat(path); err != nil {
		return "NOT_AUTHENTICATED: No token at " + path, false
	}
	if loadUserCredentials(ctx, email) == nil {
		return fmt.Sprintf("TOKEN_INVALID: Re-run /setup-files (path: %s)", path), false
	}
	return "AUTHENTICATED: Token valid at " + path, true
}
