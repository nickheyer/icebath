package credentials

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// xAI's OIDC issuer and the client of its Grok CLI sign-in
const (
	Issuer    = "https://auth.x.ai"
	discovery = Issuer + "/.well-known/openid-configuration"
	deviceURL = Issuer + "/oauth2/device/code"
	ClientID  = "b1a00492-073a-47ea-816f-4c329264a828"
	scope     = "openid profile email offline_access grok-cli:access api:access"
	grant     = "urn:ietf:params:oauth:grant-type:device_code"
	// Renewal ahead of expiry of a token that lives long, and of one that lives at most 45 minutes
	longSkew  = time.Hour
	shortSkew = 2 * time.Minute
	relogin   = "Sign in again with `icebath auth add xai-oauth`."
)

// Grok sign-ins kept in accounts
type oauth struct{}

func (oauth) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != xaiconfig.OAuthName || account.Name == "" {
		return credentials.Credential{}, false
	}
	token := strings.TrimSpace(account.Fields.GetString(xaiconfig.AccessToken))
	if token == "" {
		return credentials.Credential{}, false
	}
	return credentials.Credential{APIKey: token, Auth: provider.AuthOAuth, Origin: credentials.Saved, BaseURL: Inference(account.Provider.Spec.BaseURL, account.Fields),
		Expires: credentials.JWTExpiry(token), Identity: credentials.Identity(token)}, true
}

// Address of xAI's inference API for a Grok bearer: ICEBATH_XAI_BASE_URL, else the provider's base URL, kept only when it is HTTPS on
// x.ai so the bearer never leaves xAI
func Inference(base string, fields interface {
	LookupEnv(name string) (string, bool)
}) string {
	if override, _ := fields.LookupEnv(xaiconfig.IcebathBaseURLEnv); strings.TrimSpace(override) != "" {
		base = override
	}
	candidate := strings.TrimRight(strings.TrimSpace(base), "/")
	if candidate == "" {
		return xaiconfig.OAuthBaseURL
	}
	if problem := origin(candidate); problem != "" {
		slog.Warn("Refusing an xAI base URL that would send the Grok sign-in elsewhere; using xAI's own", "base_url", candidate, "problem", problem)
		return xaiconfig.OAuthBaseURL
	}
	return candidate
}

// What keeps an address from being HTTPS on x.ai, empty when nothing does
func origin(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "malformed"
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case parsed.Scheme != "https":
		return "not HTTPS"
	case host == "":
		return "no host"
	case host != "x.ai" && !strings.HasSuffix(host, ".x.ai"):
		return "not on x.ai"
	}
	return ""
}

// How long before its expiry a token is renewed: an hour, or two minutes for one that lives at most 45
func skew(token string) time.Duration {
	if expires := credentials.JWTExpiry(token); !expires.IsZero() {
		if left := time.Until(expires); left > 0 && left <= 45*time.Minute {
			return shortSkew
		}
	}
	return longSkew
}

func (oauth) Due(account credentials.Account) bool {
	if account.Spec() != xaiconfig.OAuthName || account.Name == "" || account.Fields.GetString(xaiconfig.RefreshToken) == "" {
		return false
	}
	token := account.Fields.GetString(xaiconfig.AccessToken)
	expires := credentials.JWTExpiry(token)
	return !expires.IsZero() && time.Now().Add(skew(token)).After(expires)
}

// Whether the account holds an xAI sign-in, whose refresh token renews it once, so a copy in another profile can't share it
func (oauth) Rotates(account credentials.Account) bool {
	return account.Spec() == xaiconfig.OAuthName && account.Name != "" && account.Fields.GetString(xaiconfig.RefreshToken) != ""
}

func (oauth) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	if account.Spec() != xaiconfig.OAuthName || account.Name == "" {
		return false, nil
	}
	refresh := strings.TrimSpace(account.Fields.GetString(xaiconfig.RefreshToken))
	if refresh == "" {
		return false, nil
	}
	wait := max(time.Duration(account.Provider.Settings.GetInt(xaiconfig.RefreshTimeout))*time.Second, 5*time.Second)
	tokens, err := Refresh(ctx, refresh, account.Fields.GetString(xaiconfig.TokenEndpoint), wait)
	if err != nil {
		return true, err
	}
	for field, value := range tokens.fields() {
		if err := account.Fields.Set(field, value); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Tokens of a Grok sign-in and the endpoint they renew at
type Tokens struct {
	Access, Refresh, ID string
	Endpoint            string
	Refreshed           string
}

func (t Tokens) fields() map[string]any {
	return map[string]any{xaiconfig.AccessToken: t.Access, xaiconfig.RefreshToken: t.Refresh, xaiconfig.IDToken: t.ID,
		xaiconfig.TokenEndpoint: t.Endpoint, xaiconfig.LastRefresh: t.Refreshed}
}

func failed(message, code string, relogin bool) *credentials.AuthError {
	return credentials.Failed(xaiconfig.OAuthName, code, message, relogin, nil)
}

// Endpoints of xAI's OIDC discovery, each HTTPS on x.ai
func discover(ctx context.Context, wait time.Duration) (authorize, token string, err error) {
	reply, err := credentials.Get(ctx, credentials.Client(wait), discovery, nil)
	if err != nil {
		failure := failed(fmt.Sprintf("icebath could not reach xAI's sign-in service: %s.", err), "xai_discovery_failed", false)
		failure.Err = err
		return "", "", failure
	}
	if reply.Status != 200 {
		return "", "", failed(fmt.Sprintf("xAI's sign-in service answered with HTTP status %d. Try again later.", reply.Status), "xai_discovery_failed", false)
	}
	payload, err := reply.JSON()
	if err != nil {
		return "", "", failed(fmt.Sprintf("xAI's sign-in service gave an answer that can't be read: %s.", err), "xai_discovery_invalid_json", false)
	}
	authorize, token = credentials.Text(payload["authorization_endpoint"]), credentials.Text(payload["token_endpoint"])
	if authorize == "" || token == "" {
		return "", "", failed("xAI's sign-in service did not say where to sign in. Try again later.", "xai_discovery_incomplete", false)
	}
	for field, value := range map[string]string{"authorization_endpoint": authorize, "token_endpoint": token} {
		if err := endpoint(value, field); err != nil {
			return "", "", err
		}
	}
	return authorize, token, nil
}

// Refuses an endpoint discovery or a cached field gave that isn't HTTPS on x.ai
func endpoint(raw, field string) error {
	switch origin(raw) {
	case "":
		return nil
	case "not HTTPS":
		return failed(fmt.Sprintf("xAI gave a sign-in address that is not secure, so it was refused: %s.", strconv.Quote(raw)), "xai_discovery_invalid", false)
	case "no host", "malformed":
		return failed(fmt.Sprintf("xAI gave a sign-in address with no host, so it was refused: %s.", strconv.Quote(raw)), "xai_discovery_invalid", false)
	}
	parsed, _ := url.Parse(raw)
	return failed(fmt.Sprintf("The kept sign-in address %s is not on x.ai, so icebath refuses it in case someone changed it. Sign in again with "+
		"`icebath auth add xai-oauth`.", strconv.Quote(parsed.Hostname())), "xai_discovery_invalid", false)
}

// Renews a Grok sign-in at its token endpoint, discovering it when none is kept; the old refresh token stays when none rotates in
func Refresh(ctx context.Context, refresh, tokenEndpoint string, wait time.Duration) (Tokens, error) {
	if strings.TrimSpace(refresh) == "" {
		return Tokens{}, failed("This Grok sign-in keeps no way to renew itself. "+relogin, "xai_auth_missing_refresh_token", true)
	}
	if strings.TrimSpace(tokenEndpoint) == "" {
		var err error
		if _, tokenEndpoint, err = discover(ctx, wait); err != nil {
			return Tokens{}, err
		}
	}
	if err := endpoint(tokenEndpoint, "token_endpoint"); err != nil {
		return Tokens{}, err
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {ClientID}, "refresh_token": {refresh}}
	reply, err := credentials.PostForm(ctx, credentials.Client(wait), tokenEndpoint, form, nil)
	if err != nil {
		return Tokens{}, err
	}
	if reply.Status != 200 {
		suffix := ""
		if detail := reply.Text(500); detail != "" {
			suffix = " xAI said: " + strings.TrimSuffix(detail, ".") + "."
		}
		if reply.Status == 403 {
			return Tokens{}, failed("xAI refused to renew the sign-in, with HTTP status 403."+suffix+" Your plan may not include use outside the Grok "+
				"app, and signing in again won't change that. Use an xAI API key with the xai provider instead, or upgrade at https://x.ai/grok.",
				"xai_oauth_tier_denied", false)
		}
		return Tokens{}, failed("xAI refused to renew the sign-in."+suffix, "xai_refresh_failed", reply.Status == 400 || reply.Status == 401)
	}
	payload, err := reply.JSON()
	if err != nil {
		return Tokens{}, failed(fmt.Sprintf("xAI's answer to renewing the sign-in can't be read: %s.", err), "xai_refresh_invalid_json", false)
	}
	tokens := issued(payload, refresh, tokenEndpoint)
	if tokens.Access == "" {
		return Tokens{}, failed("xAI renewed the sign-in without sending a new token. "+relogin, "xai_refresh_missing_access_token", true)
	}
	return tokens, nil
}

// Tokens an answer of the token endpoint gave, the refresh token kept when none rotated in
func issued(payload map[string]any, refresh, tokenEndpoint string) Tokens {
	return Tokens{Access: credentials.Text(payload["access_token"]), Refresh: cmp.Or(credentials.Text(payload["refresh_token"]), strings.TrimSpace(refresh)),
		ID: credentials.Text(payload["id_token"]), Endpoint: tokenEndpoint, Refreshed: time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")}
}

// Sign-in to xAI Grok (SuperGrok, Premium+) by device code
var SignIn = credentials.SignIn{Spec: xaiconfig.OAuthName, Title: "Grok account with SuperGrok or Premium+", Type: credentials.OAuth, Run: signIn}

func signIn(p *credentials.Prompter) (credentials.Signed, error) {
	p.Say("")
	p.Say("Signing in to your Grok account with SuperGrok or X Premium+.")
	p.Say("This sign-in is icebath's own.")
	wait := p.Wait(20 * time.Second)
	_, tokenEndpoint, err := discover(p.Ctx, wait)
	if err != nil {
		return credentials.Signed{}, err
	}
	client := credentials.Client(max(wait, 20*time.Second))
	header := map[string]string{"Accept": "application/json"}
	reply, err := credentials.PostForm(p.Ctx, client, deviceURL, url.Values{"client_id": {ClientID}, "scope": {scope}}, header)
	if err != nil {
		return credentials.Signed{}, err
	}
	if reply.Status != 200 {
		detail := ""
		if text := reply.Text(500); text != "" {
			detail = " xAI said: " + strings.TrimSuffix(text, ".") + "."
		}
		return credentials.Signed{}, failed(fmt.Sprintf("xAI refused to hand out a sign-in code, with HTTP status %d.%s", reply.Status, detail),
			"device_code_request_failed", false)
	}
	device, err := reply.JSON()
	if err != nil {
		return credentials.Signed{}, err
	}
	var missing []string
	for _, key := range []string{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"} {
		if _, ok := device[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return credentials.Signed{}, failed("xAI's sign-in code came back without "+strings.Join(missing, ", ")+". Start the sign-in again.",
			"device_code_invalid", false)
	}
	interval, ok := config.Whole(device["interval"])
	if !ok {
		return credentials.Signed{}, fmt.Errorf("xAI gave a sign-in code that can't be used, because its wait %v is not a whole number.", device["interval"])
	}
	expires, ok := config.Whole(device["expires_in"])
	if !ok {
		return credentials.Signed{}, fmt.Errorf("xAI gave a sign-in code that can't be used, because its lifetime %v is not a whole number.", device["expires_in"])
	}
	p.Instructions(cmp.Or(credentials.Text(device["verification_uri_complete"]), credentials.Text(device["verification_uri"])), credentials.Text(device["user_code"]))
	p.Pending("Waiting for you to approve the sign-in.")
	code := credentials.Text(device["device_code"])
	payload, err := credentials.Poll{
		Provider: xaiconfig.OAuthName,
		Post: func() (*credentials.Reply, error) {
			return credentials.PostForm(p.Ctx, client, tokenEndpoint, url.Values{"grant_type": {grant}, "client_id": {ClientID}, "device_code": {code}}, header)
		},
		Expires:  time.Duration(expires) * time.Second,
		Interval: time.Duration(max(1, interval)) * time.Second,
		Granted: func(payload map[string]any) error {
			for _, field := range [][2]string{{"access_token", "an"}, {"refresh_token", "a"}} {
				if credentials.Text(payload[field[0]]) == "" {
					return failed(fmt.Sprintf("xAI approved the sign-in without sending %s %s. Start the sign-in again.", field[1], field[0]),
						"xai_device_token_invalid", false)
				}
			}
			return nil
		},
	}.Run(p.Ctx)
	if err != nil {
		return credentials.Signed{}, err
	}
	tokens := issued(payload, "", tokenEndpoint)
	return credentials.Signed{Fields: tokens.fields(), Identity: credentials.Identity(tokens.Access)}, nil
}
