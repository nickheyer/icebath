package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	qwenoauthconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Client of the Qwen CLI's sign-in, and the endpoints it signs in at
const (
	ClientID  = "f0304373b74a44d2b584a3fb70ca9e56"
	TokenURL  = "https://chat.qwen.ai/api/v1/oauth2/token"
	DeviceURL = "https://chat.qwen.ai/api/v1/oauth2/device/code"
	scope     = "openid profile email model.completion"
	grant     = "urn:ietf:params:oauth:grant-type:device_code"
	// How long before its expiry a Qwen token is renewed
	skew  = 2 * time.Minute
	rerun = "Sign in again with `icebath auth add qwen-oauth`, or with `qwen auth qwen-oauth` for the Qwen CLI."
)

// Program a borrowed sign-in comes from, as its source reads
const qwenCLI = "Qwen CLI"

// Qwen sign-ins kept in accounts, else the Qwen CLI's own, or a key from QWEN_API_KEY
type source struct{}

func Register(registry *credentials.Registry) {
	registry.Register(source{})
}

// Path of the Qwen CLI's sign-in
func CLIPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".qwen", "oauth_creds.json")
}

// Tokens of a Qwen sign-in as the Qwen CLI keeps them
type Tokens struct {
	Access   string `json:"access_token"`
	Refresh  string `json:"refresh_token"`
	Type     string `json:"token_type,omitempty"`
	Resource string `json:"resource_url,omitempty"`
	// Unix milliseconds
	Expiry int64 `json:"expiry_date,omitempty"`
}

// The Qwen CLI's sign-in, an error saying why when there is none to read
func readCLI() (Tokens, error) {
	path := CLIPath()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Tokens{}, failed("No Qwen CLI sign-in was found on this computer. Run `qwen auth qwen-oauth` first.", "qwen_auth_missing", nil)
	}
	var tokens Tokens
	if err == nil {
		err = json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &tokens)
	}
	if err != nil {
		return Tokens{}, failed(fmt.Sprintf("The Qwen CLI sign-in in %s can't be read: %s.", path, err), "qwen_auth_read_failed", err)
	}
	return tokens, nil
}

// Writes the Qwen CLI's sign-in privately and at once
func writeCLI(tokens Tokens) (string, error) {
	path := CLIPath()
	encoded, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return path, err
	}
	return path, fsx.Write(path, encoded, 0o600)
}

func failed(message, code string, cause error) *credentials.AuthError {
	return credentials.Failed(qwenoauthconfig.Name, code, message, false, cause)
}

// Whether the provider may take the Qwen CLI's sign-in
func adopts(account credentials.Account) bool {
	return !account.Ignores(qwenCLI)
}

// An account's own sign-in, or the Qwen CLI's sign-in for an account that uses it; outside the accounts, a key from QWEN_API_KEY, else the
// Qwen CLI's sign-in. The Qwen CLI's only while the provider may take it
func (source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != qwenoauthconfig.Name {
		return credentials.Credential{}, false
	}
	if account.Name != "" {
		if token := strings.TrimSpace(account.Fields.GetString(qwenoauthconfig.AccessToken)); token != "" {
			return credentials.Credential{APIKey: token, Auth: provider.AuthOAuth, Origin: credentials.Saved,
				Expires: millis(int64(account.Fields.GetInt(qwenoauthconfig.Expiry)))}, true
		}
		if account.Program() != qwenCLI {
			return credentials.Credential{}, false
		}
	} else if found, ok := account.Lookup("", qwenoauthconfig.APIKeyEnv); ok {
		found.Auth = provider.AuthAPIKey
		return found, true
	}
	if !adopts(account) {
		return credentials.Credential{}, false
	}
	tokens, err := readCLI()
	if err != nil || strings.TrimSpace(tokens.Access) == "" {
		return credentials.Credential{}, false
	}
	return credentials.Credential{APIKey: strings.TrimSpace(tokens.Access), Auth: provider.AuthOAuth, Origin: credentials.External, Source: qwenCLI,
		Expires: millis(tokens.Expiry)}, true
}

func millis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// Whether the sign-in the account uses expires within two minutes, or says nothing of when
func (s source) Due(account credentials.Account) bool {
	credential, ok := s.Credential(account)
	if !ok || credential.Auth != provider.AuthOAuth {
		return false
	}
	return credential.Expires.IsZero() || time.Now().Add(skew).After(credential.Expires)
}

// Renews the sign-in the account uses: its own, the Qwen CLI's sign-in kept in step when it holds the same token, or the Qwen CLI's
func (s source) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	credential, ok := s.Credential(account)
	if account.Spec() != qwenoauthconfig.Name || !ok || credential.Auth != provider.AuthOAuth {
		return false, nil
	}
	unlock, err := fsx.Lock(ctx, strings.TrimSuffix(CLIPath(), ".json")+".lock", 25*time.Second)
	if err != nil {
		return true, err
	}
	defer unlock()
	cli, _ := readCLI()
	if credential.Origin == credentials.External {
		renewed, err := Refresh(ctx, cli)
		if err != nil {
			return true, err
		}
		_, err = writeCLI(renewed)
		return true, err
	}
	own := Tokens{Access: account.Fields.GetString(qwenoauthconfig.AccessToken), Refresh: account.Fields.GetString(qwenoauthconfig.RefreshToken),
		Resource: account.Fields.GetString(qwenoauthconfig.ResourceURL), Expiry: int64(account.Fields.GetInt(qwenoauthconfig.Expiry))}
	renewed, err := Refresh(ctx, own)
	if err != nil {
		return true, err
	}
	for field, value := range renewed.fields() {
		if err := account.Fields.Set(field, value); err != nil {
			return true, err
		}
	}
	// The Qwen CLI holding the same refresh token would replay the one just spent
	if cli.Refresh != "" && cli.Refresh == own.Refresh {
		if _, err := writeCLI(renewed); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (t Tokens) fields() map[string]any {
	return map[string]any{qwenoauthconfig.AccessToken: t.Access, qwenoauthconfig.RefreshToken: t.Refresh, qwenoauthconfig.ResourceURL: t.Resource,
		qwenoauthconfig.Expiry: t.Expiry}
}

// Renews Qwen tokens at Qwen's token endpoint
func Refresh(ctx context.Context, tokens Tokens) (Tokens, error) {
	refresh := strings.TrimSpace(tokens.Refresh)
	if refresh == "" {
		return Tokens{}, failed("This Qwen sign-in keeps no way to renew itself. "+rerun, "qwen_refresh_token_missing", nil)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {ClientID}}
	reply, err := credentials.PostForm(ctx, credentials.Client(20*time.Second), TokenURL, form, map[string]string{"Accept": "application/json"})
	if err != nil {
		return Tokens{}, failed(fmt.Sprintf("icebath could not reach Qwen to renew the sign-in: %s.", err), "qwen_refresh_failed", err)
	}
	if reply.Status >= 400 {
		suffix := ""
		if body := reply.Text(500); body != "" {
			suffix = " Qwen said: " + strings.TrimSuffix(body, ".") + "."
		}
		failure := failed("Qwen refused to renew the sign-in. "+rerun+suffix, "qwen_refresh_failed", nil)
		failure.Relogin = reply.Status == 400 || reply.Status == 401
		return Tokens{}, failure
	}
	payload, err := reply.JSON()
	if err != nil {
		return Tokens{}, failed(fmt.Sprintf("Qwen's answer to renewing the sign-in can't be read: %s.", err), "qwen_refresh_invalid_json", err)
	}
	return issued(payload, tokens)
}

// Tokens an answer of the token endpoint gave, what it left out kept from before
func issued(payload map[string]any, before Tokens) (Tokens, error) {
	access := credentials.Text(payload["access_token"])
	if access == "" {
		return Tokens{}, failed("Qwen renewed the sign-in without sending a new token. "+rerun, "qwen_refresh_invalid_response", nil)
	}
	seconds, ok := config.Whole(payload["expires_in"])
	if !ok {
		seconds = 6 * 60 * 60
	}
	return Tokens{Access: access, Refresh: strs.Filled(credentials.Text(payload["refresh_token"]), strings.TrimSpace(before.Refresh)),
		Type:     strs.Filled(credentials.Text(payload["token_type"]), before.Type, "Bearer"),
		Resource: strs.Filled(credentials.Text(payload["resource_url"]), before.Resource, "portal.qwen.ai"),
		Expiry:   time.Now().UnixMilli() + int64(max(1, seconds))*1000}, nil
}

// Sign-in to a Qwen account by device code, kept in the account and in the Qwen CLI's file as the Qwen CLI would
var SignIn = credentials.SignIn{Spec: qwenoauthconfig.Name, Title: "Qwen account", Type: credentials.OAuth, Run: signIn}

// The Qwen CLI's sign-in on this computer, used as it renews it
var QwenCLI = credentials.SignIn{Spec: qwenoauthconfig.Name, Title: "Qwen CLI sign-in on this computer", Type: "qwen-cli", Run: borrowed}

func signIn(p *credentials.Prompter) (credentials.Signed, error) {
	ctx := p.Ctx
	client := credentials.Client(20 * time.Second)
	verifier, challenge := credentials.PKCE(32)
	header := map[string]string{"x-request-id": uuid.NewString(), "Accept": "application/json"}
	reply, err := credentials.PostForm(ctx, client, DeviceURL, url.Values{"client_id": {ClientID}, "scope": {scope}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}}, header)
	if err != nil {
		return credentials.Signed{}, failed(fmt.Sprintf("icebath could not ask Qwen for a sign-in code: %s.", err), "qwen_device_code_failed", err)
	}
	device, err := reply.JSON()
	if reply.Status != 200 || err != nil || credentials.Text(device["device_code"]) == "" || credentials.Text(device["user_code"]) == "" {
		return credentials.Signed{}, failed(fmt.Sprintf("Qwen refused to hand out a sign-in code, with HTTP status %d. Try again later.", reply.Status), "qwen_device_code_failed", nil)
	}
	p.Instructions(strs.Filled(credentials.Text(device["verification_uri_complete"]), credentials.Text(device["verification_uri"])), credentials.Text(device["user_code"]))
	expires, ok := config.Whole(device["expires_in"])
	if !ok || expires <= 0 {
		expires = 600
	}
	p.Pending("Waiting for you to approve the sign-in.")
	deadline := time.Now().Add(time.Duration(expires) * time.Second)
	interval := 2 * time.Second
	for time.Now().Before(deadline) {
		if err := credentials.Sleep(ctx, interval); err != nil {
			return credentials.Signed{}, err
		}
		poll, err := credentials.PostForm(ctx, client, TokenURL, url.Values{"grant_type": {grant}, "client_id": {ClientID},
			"device_code": {credentials.Text(device["device_code"])}, "code_verifier": {verifier}}, map[string]string{"Accept": "application/json"})
		if err != nil {
			if ctx.Err() != nil {
				return credentials.Signed{}, errors.Join(credentials.ErrCancelled, ctx.Err())
			}
			return credentials.Signed{}, failed(fmt.Sprintf("The connection to Qwen failed while waiting for your approval: %s.", err), "qwen_device_token_failed", err)
		}
		body, _ := poll.JSON()
		switch code := credentials.Text(body["error"]); {
		case poll.Status == 400 && code == "authorization_pending":
			continue
		case poll.Status == 429 && code == "slow_down":
			interval = min(interval*3/2, 10*time.Second)
			continue
		case poll.Status != 200 || credentials.Text(body["access_token"]) == "":
			return credentials.Signed{}, failed("Qwen ended the sign-in. It said: "+strings.TrimSuffix(strs.Filled(credentials.Text(body["error_description"]), code), ".")+".",
				"qwen_device_token_failed", nil)
		}
		tokens := Tokens{Access: credentials.Text(body["access_token"]), Refresh: credentials.Text(body["refresh_token"]), Type: credentials.Text(body["token_type"]),
			Resource: credentials.Text(body["resource_url"])}
		if seconds, ok := config.Whole(body["expires_in"]); ok && seconds > 0 {
			tokens.Expiry = time.Now().UnixMilli() + int64(seconds)*1000
		}
		path, err := writeCLI(tokens)
		if err != nil {
			return credentials.Signed{}, err
		}
		p.Say("✓ You are signed in to Qwen. The Qwen CLI can use this sign-in too, kept in %s.", path)
		return credentials.Signed{Fields: tokens.fields(), Identity: credentials.Identity(tokens.Access)}, nil
	}
	return credentials.Signed{}, failed("The sign-in was not approved in time.", credentials.Expired, nil)
}

// Account that uses the Qwen CLI's sign-in, as the Qwen CLI renews it. The sign-in is read by the server, on its computer, so the server
// is the one that looks for it as it keeps the account, and refuses one it does not find
func borrowed(*credentials.Prompter) (credentials.Signed, error) {
	return credentials.Signed{Fields: map[string]any{providerconfig.Program: qwenCLI}}, nil
}

// Why an account that uses the Qwen CLI's sign-in has none: the setting that turns it off, the provider leaving it out, or no sign-in
// where the Qwen CLI keeps it
func (source) Why(account credentials.Account) (string, bool) {
	if account.Spec() != qwenoauthconfig.Name || account.Name == "" || account.Program() != qwenCLI {
		return "", false
	}
	label := account.Provider.Label()
	if account.Ignores(qwenCLI) {
		return "It uses the sign-in of the Qwen CLI, which " + label + " leaves out until you turn it on again.", true
	}
	if _, err := readCLI(); err != nil {
		return "It uses the sign-in of the Qwen CLI. " + err.Error(), true
	}
	return "The sign-in of the Qwen CLI in " + CLIPath() + " holds no token. Sign in again with `qwen auth qwen-oauth`.", true
}
