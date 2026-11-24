package googlechat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Least privilege scope covering media upload and the message create that references it
var userScopes = []string{"https://www.googleapis.com/auth/chat.messages.create"}

const (
	oauthRedirectURI = "http://localhost:1"
	googleRevokeURL  = "https://oauth2.googleapis.com/revoke"
	defaultTokenURI  = "https://oauth2.googleapis.com/token"
	pkceAlphabet     = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	stateAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// Characters a token file name never carries
var emailFSRE = regexp.MustCompile(`[^a-z0-9._@-]+`)

// Failure of an OAuth helper step, its lines printed by the CLI and mapped to a chat reply by /setup-files
type oauthFailure struct{ lines []string }

func (e *oauthFailure) Error() string { return strings.Join(e.lines, "\n") }

// Failure of an OAuth helper step with the given lines
func failOAuth(lines ...string) error { return &oauthFailure{lines} }

// Lowercased file system safe key of an email
func sanitizeEmail(email string) string {
	cleaned := emailFSRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(email)), "_")
	if cleaned == "" {
		return "_unknown_"
	}
	return cleaned
}

// Home relative token file, per user under the tokens dir or the shared single user path
func tokenRel(email string) string {
	if email != "" {
		return "google_chat_user_tokens/" + sanitizeEmail(email) + ".json"
	}
	return "google_chat_user_token.json"
}

// Per user token directory under the home
func userTokensDir(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "google_chat_user_tokens")
}

// Token file of an email, the shared single user file when empty
func tokenPath(ctx context.Context, email string) string {
	return filepath.Join(home.Dir(ctx), tokenRel(email))
}

// Stored OAuth client secret of the profile
func clientSecretPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "google_chat_user_client_secret.json")
}

// Pending PKCE state of an email, the shared one when empty
func pendingAuthPath(ctx context.Context, email string) string {
	if email != "" {
		return filepath.Join(home.Dir(ctx), "google_chat_user_oauth_pending", sanitizeEmail(email)+".json")
	}
	return filepath.Join(home.Dir(ctx), "google_chat_user_oauth_pending.json")
}

// Whether a path exists
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// User credential with the authorized user payload it persists back to its token file
type userCreds struct {
	creds   *provider.GoogleCredentials
	payload map[string]any
	path    string
}

// Expiry of a stored token in the google auth formats, zero when absent or unparseable
func parseStoredExpiry(v any) time.Time {
	s, _ := v.(string)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999Z", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Credential of a stored authorized user payload seeded with its stored token
func credsFromPayload(payload map[string]any, path string) (*userCreds, error) {
	creds, err := provider.GoogleAuthorizedUserCredentials(payload, nil)
	if err != nil {
		return nil, err
	}
	if token, _ := payload["token"].(string); token != "" {
		creds.Seed(token, parseStoredExpiry(payload["expiry"]))
	}
	return &userCreds{creds: creds, payload: payload, path: path}, nil
}

// Valid token, refreshed and persisted when the stored one expired, nil when unusable
func (u *userCreds) refreshAndPersist(ctx context.Context, failureMsg string) *userCreds {
	before, _ := u.creds.Current()
	token, err := u.creds.Token(ctx)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[google_chat_user_oauth] %s: %v", failureMsg, err))
		return nil
	}
	if token != before {
		_, expiry := u.creds.Current()
		u.payload["token"] = token
		if !expiry.IsZero() {
			u.payload["expiry"] = expiry.UTC().Format("2006-01-02T15:04:05.000000Z")
		}
		if err := writePrivateJSON(u.path, normalizeAuthorizedUserPayload(u.payload)); err != nil {
			pkgLog().Debug(fmt.Sprintf("[google_chat_user_oauth] failed to persist credentials at %s", u.path), "error", err)
		}
	}
	return u
}

// Persisted user credentials of an email, nil when absent, corrupt or no longer refreshable
func loadUserCredentials(ctx context.Context, email string) *userCreds {
	path := tokenPath(ctx, email)
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if st.Mode().Perm()&0o077 != 0 {
		pkgLog().Warn(fmt.Sprintf("[google_chat_user_oauth] credential file %s is readable by other users (mode %o); chmod 600 it", path, st.Mode().Perm()))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[google_chat_user_oauth] token at %s is corrupt: %v", path, err))
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		pkgLog().Warn(fmt.Sprintf("[google_chat_user_oauth] token at %s is corrupt: %v", path, err))
		return nil
	}
	creds, err := credsFromPayload(payload, path)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[google_chat_user_oauth] token at %s is corrupt: %v", path, err))
		return nil
	}
	return creds.refreshAndPersist(ctx, "token refresh failed (user should re-run /setup-files)")
}

// Credentials refreshed when expired, nil on failure
func refreshOrNone(ctx context.Context, creds *userCreds) *userCreds {
	if creds == nil {
		return nil
	}
	return creds.refreshAndPersist(ctx, "refresh failed")
}

// Sanitized emails with stored per user tokens, sorted
func listAuthorizedEmails(ctx context.Context) []string {
	entries, err := os.ReadDir(userTokensDir(ctx))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && filepath.Ext(e.Name()) == ".json" {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}

// Token payload with the type field google auth expects
func normalizeAuthorizedUserPayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		out[k] = v
	}
	if t, _ := out["type"].(string); t == "" {
		out["type"] = "authorized_user"
	}
	return out
}

// Atomic indented JSON write with owner only permissions on the file and its directory
func writePrivateJSON(path string, data any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)
	text, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := config.AtomicWrite(path, text, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// Status line of the stored token and whether it is usable
func checkAuth(ctx context.Context, email string) (string, bool) {
	path := tokenPath(ctx, email)
	if !pathExists(path) {
		return "NOT_AUTHENTICATED: No token at " + path, false
	}
	if loadUserCredentials(ctx, email) == nil {
		return fmt.Sprintf("TOKEN_INVALID: Re-run /setup-files (path: %s)", path), false
	}
	return "AUTHENTICATED: Token valid at " + path, true
}

// Validates a Google OAuth client secret file and stores it under the home
func storeClientSecret(ctx context.Context, path string) (string, error) {
	src, err := filepath.Abs(pypath.Expanduser(path))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", failOAuth("ERROR: File not found: " + src)
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	var data map[string]any
	if err != nil || json.Unmarshal([]byte(text), &data) != nil {
		return "", failOAuth("ERROR: File is not valid JSON.")
	}
	_, installed := data["installed"]
	_, web := data["web"]
	if !installed && !web {
		return "", failOAuth("ERROR: Not a Google OAuth client secret file (missing 'installed' or 'web' key).",
			"Download from: https://console.cloud.google.com/apis/credentials")
	}
	target := clientSecretPath(ctx)
	if err := writePrivateJSON(target, data); err != nil {
		return "", err
	}
	return "OK: Client secret saved to " + target, nil
}

// Installed or web client section of the stored client secret
func loadClientConfig(ctx context.Context) (map[string]any, error) {
	if !pathExists(clientSecretPath(ctx)) {
		return nil, failOAuth("ERROR: No client secret stored. Run --client-secret first.")
	}
	raw, err := os.ReadFile(clientSecretPath(ctx))
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return nil, err
	}
	for _, key := range []string{"web", "installed"} {
		if section, ok := data[key].(map[string]any); ok {
			return section, nil
		}
	}
	return nil, errors.New("Client secrets must be for a web or installed app.")
}

// Random text of the given length over the alphabet
func randomText(n int, alphabet string) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// OAuth consent URL with PKCE state persisted per email so several users can be mid flow at once
func getAuthURL(ctx context.Context, email string) (string, error) {
	client, err := loadClientConfig(ctx)
	if err != nil {
		return "", err
	}
	verifier, err := randomText(128, pkceAlphabet)
	if err != nil {
		return "", err
	}
	state, err := randomText(30, stateAlphabet)
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {fmt.Sprint(client["client_id"])}, "redirect_uri": {oauthRedirectURI},
		"scope": {strings.Join(userScopes, " ")}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
		"access_type": {"offline"}, "prompt": {"consent"}}
	authURI, _ := client["auth_uri"].(string)
	if authURI == "" {
		authURI = "https://accounts.google.com/o/oauth2/auth"
	}
	if err := writePrivateJSON(pendingAuthPath(ctx, email), map[string]any{"state": state, "code_verifier": verifier,
		"redirect_uri": oauthRedirectURI, "email": email}); err != nil {
		return "", err
	}
	return authURI + "?" + q.Encode(), nil
}

// Pending PKCE state of an email
func loadPendingAuth(ctx context.Context, email string) (map[string]any, error) {
	path := pendingAuthPath(ctx, email)
	if !pathExists(path) {
		return nil, failOAuth("ERROR: No pending OAuth session found. Run --auth-url first.")
	}
	raw, err := os.ReadFile(path)
	var data map[string]any
	if err == nil {
		var text string
		if text, err = pystr.DecodeUTF8Sig(raw); err == nil {
			err = json.Unmarshal([]byte(text), &data)
		}
	}
	if err != nil {
		return nil, failOAuth(fmt.Sprintf("ERROR: Could not read pending OAuth session: %v", err), "Run --auth-url again to start a fresh session.")
	}
	if s, _ := data["state"].(string); s == "" {
		return nil, failOAuth("ERROR: Pending OAuth session is missing PKCE data.", "Run --auth-url again.")
	}
	if v, _ := data["code_verifier"].(string); v == "" {
		return nil, failOAuth("ERROR: Pending OAuth session is missing PKCE data.", "Run --auth-url again.")
	}
	return data, nil
}

// Code, returned state and query of a raw code or the pasted failed redirect URL
func extractCodeAndState(codeOrURL string) (string, string, url.Values, error) {
	if !strings.HasPrefix(codeOrURL, "http") {
		return codeOrURL, "", nil, nil
	}
	parsed, err := url.Parse(codeOrURL)
	if err != nil {
		return "", "", nil, failOAuth("ERROR: No 'code' parameter found in URL.")
	}
	params := parsed.Query()
	if params.Get("code") == "" {
		return "", "", nil, failOAuth("ERROR: No 'code' parameter found in URL.")
	}
	return params.Get("code"), params.Get("state"), params, nil
}

// Exchanges an auth code or pasted redirect URL for a refresh token stored for the email
func exchangeAuthCode(ctx context.Context, codeOrURL, email string) (string, error) {
	client, err := loadClientConfig(ctx)
	if err != nil {
		return "", err
	}
	pending, err := loadPendingAuth(ctx, email)
	if err != nil {
		return "", err
	}
	code, returnedState, params, err := extractCodeAndState(codeOrURL)
	if err != nil {
		return "", err
	}
	if returnedState != "" && returnedState != pending["state"] {
		return "", failOAuth("ERROR: OAuth state mismatch. Run --auth-url again to start a fresh session.")
	}
	requested := append([]string(nil), userScopes...)
	if params != nil {
		if scope := strings.TrimSpace(params.Get("scope")); scope != "" {
			requested = strings.Fields(scope)
		}
	}
	redirect, _ := pending["redirect_uri"].(string)
	if redirect == "" {
		redirect = oauthRedirectURI
	}
	tokenURI, _ := client["token_uri"].(string)
	if tokenURI == "" {
		tokenURI = defaultTokenURI
	}
	clientID, clientSecret := fmt.Sprint(client["client_id"]), fmt.Sprint(client["client_secret"])
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "client_secret": {clientSecret}, "code_verifier": {fmt.Sprint(pending["code_verifier"])}}
	data, err := postForm(ctx, tokenURI, form)
	if err != nil {
		return "", failOAuth(fmt.Sprintf("ERROR: Token exchange failed: %v", err), "The code may have expired. Run --auth-url to get a fresh URL.")
	}
	access, _ := data["access_token"].(string)
	if access == "" {
		return "", failOAuth("ERROR: Token exchange failed: no access_token in response", "The code may have expired. Run --auth-url to get a fresh URL.")
	}
	payload := map[string]any{"token": access, "refresh_token": data["refresh_token"], "token_uri": tokenURI,
		"client_id": clientID, "client_secret": clientSecret, "scopes": requested, "universe_domain": "googleapis.com", "account": ""}
	if expiresIn, ok := data["expires_in"].(float64); ok {
		payload["expiry"] = time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).Format("2006-01-02T15:04:05.000000Z")
	}
	if granted, _ := data["scope"].(string); strings.TrimSpace(granted) != "" {
		payload["scopes"] = strings.Fields(granted)
	}
	path := tokenPath(ctx, email)
	if err := writePrivateJSON(path, normalizeAuthorizedUserPayload(payload)); err != nil {
		return "", err
	}
	_ = os.Remove(pendingAuthPath(ctx, email))
	return fmt.Sprintf("OK: Authenticated. Token saved to %s\nProfile path: %s/%s", path, home.Display(home.Dir(ctx)), tokenRel(email)), nil
}

// Form POST to a Google OAuth endpoint, the decoded JSON answer or the endpoint's error
func postForm(ctx context.Context, target string, form url.Values) (map[string]any, error) {
	reqCtx, cancel := context.WithTimeout(ctx, googleAuthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	_ = json.Unmarshal(body, &data)
	if resp.StatusCode >= 400 {
		if desc, _ := data["error_description"].(string); desc != "" {
			return nil, fmt.Errorf("(%v) %s", data["error"], desc)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return data, nil
}

// Revokes the stored token with Google and deletes it and any pending state locally
func revokeUserToken(ctx context.Context, email string) (string, error) {
	path := tokenPath(ctx, email)
	if !pathExists(path) {
		return "No token to revoke.", nil
	}
	var lines []string
	raw, err := os.ReadFile(path)
	var payload map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &payload)
	}
	var token string
	if err == nil {
		var creds *userCreds
		if creds, err = credsFromPayload(payload, path); err == nil {
			token, err = creds.creds.Token(ctx)
		}
	}
	if err == nil {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, googleRevokeURL+"?"+url.Values{"token": {token}}.Encode(), nil)
		if reqErr == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			var resp *http.Response
			if resp, reqErr = http.DefaultClient.Do(req); reqErr == nil {
				resp.Body.Close()
				if resp.StatusCode >= 400 {
					reqErr = fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
				}
			}
		}
		cancel()
		err = reqErr
	}
	if err != nil {
		lines = append(lines, fmt.Sprintf("Remote revocation failed (token may already be invalid): %v", err))
	} else {
		lines = append(lines, "Token revoked with Google.")
	}
	_ = os.Remove(path)
	_ = os.Remove(pendingAuthPath(ctx, email))
	lines = append(lines, "Deleted "+path)
	return strings.Join(lines, "\n"), nil
}
