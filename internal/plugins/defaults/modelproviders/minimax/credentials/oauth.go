package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	minimaxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	grant = "urn:ietf:params:oauth:grant-type:user_code"
	// How long before its expiry a MiniMax token is renewed
	skew = time.Minute
	// Most of an error answer shown
	errorLimit = 16 * 1024
)

// MiniMax sign-ins kept in accounts
type oauth struct{}

func (oauth) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != minimaxconfig.OAuthName || account.Name == "" {
		return credentials.Credential{}, false
	}
	token := strings.TrimSpace(account.Fields.GetString(minimaxconfig.AccessToken))
	if token == "" {
		return credentials.Credential{}, false
	}
	credential := credentials.Credential{APIKey: token, Auth: provider.AuthOAuth, Origin: credentials.Saved,
		BaseURL: strings.TrimRight(account.Fields.GetString(minimaxconfig.Endpoint), "/")}
	if ms := int64(account.Fields.GetInt(minimaxconfig.Expiry)); ms > 0 {
		credential.Expires = time.UnixMilli(ms)
	}
	return credential, true
}

func (o oauth) Due(account credentials.Account) bool {
	credential, ok := o.Credential(account)
	return ok && account.Fields.GetString(minimaxconfig.RefreshToken) != "" && !credential.Expires.IsZero() && time.Now().Add(skew).After(credential.Expires)
}

// Renews the sign-in at the portal and with the client it was made with
func (oauth) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	if account.Spec() != minimaxconfig.OAuthName || account.Name == "" {
		return false, nil
	}
	refresh := account.Fields.GetString(minimaxconfig.RefreshToken)
	if refresh == "" {
		return true, failed("This MiniMax sign-in keeps no way to renew itself. Sign in again.", "no_refresh_token", true)
	}
	portal := strs.Filled(account.Fields.GetString(minimaxconfig.Portal), minimaxconfig.OAuthPortal)
	client := strs.Filled(account.Fields.GetString(minimaxconfig.Client), minimaxconfig.OAuthClientID)
	reply, err := credentials.PostForm(ctx, credentials.Client(20*time.Second), portal+"/oauth/token",
		url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}}, map[string]string{"Accept": "application/json"})
	if err != nil {
		return true, err
	}
	if reply.Status != 200 {
		body := strs.Filled(reply.Text(errorLimit), fmt.Sprintf("HTTP status %d", reply.Status))
		lower := strings.ToLower(body)
		relogin := strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "refresh_token_reused") || strings.Contains(lower, "invalid_refresh_token")
		return true, failed("MiniMax refused to renew the sign-in: "+strings.TrimSuffix(body, ".")+".", "refresh_failed", relogin)
	}
	payload, err := reply.JSON()
	if err != nil {
		return true, err
	}
	if payload["status"] != "success" {
		return true, failed("MiniMax did not renew the sign-in. Sign in again.", "refresh_failed", true)
	}
	expires, err := expiry(payload["expired_in"])
	if err != nil {
		return true, err
	}
	for field, value := range map[string]any{minimaxconfig.AccessToken: credentials.Text(payload["access_token"]),
		minimaxconfig.RefreshToken: strs.Filled(credentials.Text(payload["refresh_token"]), refresh), minimaxconfig.Expiry: expires.UnixMilli()} {
		if err := account.Fields.Set(field, value); err != nil {
			return true, err
		}
	}
	return true, nil
}

func failed(message, code string, relogin bool) *credentials.AuthError {
	return credentials.Failed(minimaxconfig.OAuthName, code, message, relogin, nil)
}

// When a token expires from expired_in, which MiniMax gives as Unix milliseconds or as a lifetime in seconds
func expiry(value any) (time.Time, error) {
	n, ok := config.Whole(value)
	if !ok {
		return time.Time{}, fmt.Errorf("MiniMax gave a sign-in that can't be used, because its lifetime %v is not a whole number.", value)
	}
	now := time.Now()
	if int64(n) > now.UnixMilli()/2 {
		return time.UnixMilli(int64(n)), nil
	}
	return now.Add(time.Duration(max(1, n)) * time.Second), nil
}

// Sign-ins at MiniMax's global and China portals, by a code typed on the portal's page
var (
	SignIn   = credentials.SignIn{Spec: minimaxconfig.OAuthName, Title: "MiniMax account", Type: credentials.OAuth, Run: signIn}
	SignInCN = credentials.SignIn{Spec: minimaxconfig.OAuthName, Title: "MiniMax account in China", Type: "oauth-cn",
		Run: func(p *credentials.Prompter) (credentials.Signed, error) {
			p.Options["region"] = "cn"
			return signIn(p)
		}}
)

func signIn(p *credentials.Prompter) (credentials.Signed, error) {
	region := strs.Filled(p.Option("region"), "global")
	portal, inference, where := minimaxconfig.OAuthPortal, minimaxconfig.OAuthBaseURL, "outside China"
	if region == "cn" {
		portal, inference, where = minimaxconfig.OAuthCNPortal, minimaxconfig.OAuthCNBaseURL, "in China"
	}
	verifier, challenge := credentials.PKCE(64)
	if len(verifier) > 96 {
		verifier = verifier[:96]
	}
	state := strs.Token(16)
	p.Say("Signing in to your MiniMax account %s.", where)
	p.Say("MiniMax sign-in page: %s", portal)
	client := credentials.Client(p.Wait(15 * time.Second))
	header := map[string]string{"x-request-id": uuid.NewString(), "Accept": "application/json"}
	reply, err := credentials.PostForm(p.Ctx, client, portal+"/oauth/code", url.Values{"response_type": {"code"}, "client_id": {minimaxconfig.OAuthClientID},
		"scope": {minimaxconfig.OAuthScope}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state}}, header)
	if err != nil {
		return credentials.Signed{}, err
	}
	if reply.Status != 200 {
		return credentials.Signed{}, failed("MiniMax refused to start the sign-in: "+strings.TrimSuffix(strs.Filled(reply.Text(errorLimit),
			fmt.Sprintf("HTTP status %d", reply.Status)), ".")+".", "authorization_failed", false)
	}
	var code map[string]any
	if err := json.Unmarshal(reply.Body, &code); err != nil {
		return credentials.Signed{}, err
	}
	for _, field := range []string{"user_code", "verification_uri", "expired_in"} {
		if _, ok := code[field]; !ok {
			return credentials.Signed{}, failed("MiniMax started the sign-in without sending "+field+". Start the sign-in again.", "authorization_incomplete", false)
		}
	}
	if got, _ := code["state"].(string); got != state {
		return credentials.Signed{}, failed("MiniMax's answer did not come from this sign-in, so it was refused. Start the sign-in again.", "state_mismatch", false)
	}
	p.Instructions(credentials.Text(code["verification_uri"]), credentials.Text(code["user_code"]))
	interval := 2 * time.Second
	if raw, ok := code["interval"]; ok && raw != nil {
		ms, ok := config.Whole(raw)
		if !ok {
			return credentials.Signed{}, fmt.Errorf("MiniMax gave a sign-in code that can't be used, because %v is not a whole number.", raw)
		}
		interval = max(2*time.Second, time.Duration(ms)*time.Millisecond)
	}
	deadline, err := expiry(code["expired_in"])
	if err != nil {
		return credentials.Signed{}, err
	}
	p.Pending("Waiting for you to approve the sign-in.")
	for time.Now().Before(deadline) {
		poll, err := credentials.PostForm(p.Ctx, client, portal+"/oauth/token", url.Values{"grant_type": {grant}, "client_id": {minimaxconfig.OAuthClientID},
			"user_code": {credentials.Text(code["user_code"])}, "code_verifier": {verifier}}, map[string]string{"Accept": "application/json"})
		if err != nil {
			if p.Ctx.Err() != nil {
				return credentials.Signed{}, errors.Join(credentials.ErrCancelled, p.Ctx.Err())
			}
			return credentials.Signed{}, err
		}
		if poll.Status != 200 {
			message := poll.Text(errorLimit)
			var refused struct {
				Base struct {
					Message string `json:"status_msg"`
				} `json:"base_resp"`
			}
			if json.Unmarshal(poll.Body, &refused) == nil && refused.Base.Message != "" {
				message = refused.Base.Message
			}
			return credentials.Signed{}, failed("MiniMax ended the sign-in. It said: "+strings.TrimSuffix(strs.Filled(message, "nothing more"), ".")+".",
				"token_exchange_failed", false)
		}
		payload, _ := poll.JSON()
		switch payload["status"] {
		case "error":
			return credentials.Signed{}, failed("MiniMax turned the sign-in down. Try again later.", "authorization_denied", false)
		case "success":
			for _, field := range []string{"access_token", "refresh_token", "expired_in"} {
				if payload[field] == nil || payload[field] == "" {
					return credentials.Signed{}, failed("MiniMax approved the sign-in without sending its tokens. Start the sign-in again.", "token_incomplete", false)
				}
			}
			expires, err := expiry(payload["expired_in"])
			if err != nil {
				return credentials.Signed{}, err
			}
			p.Say("✓ You are signed in to MiniMax.")
			if note := credentials.Text(payload["notification_message"]); note != "" {
				p.Say("MiniMax says: %s", note)
			}
			return credentials.Signed{Fields: map[string]any{minimaxconfig.AccessToken: credentials.Text(payload["access_token"]),
				minimaxconfig.RefreshToken: credentials.Text(payload["refresh_token"]), minimaxconfig.Expiry: expires.UnixMilli(),
				minimaxconfig.Portal: portal, minimaxconfig.Client: minimaxconfig.OAuthClientID, minimaxconfig.Region: region,
				minimaxconfig.Endpoint: inference}}, nil
		}
		if err := credentials.Sleep(p.Ctx, interval); err != nil {
			return credentials.Signed{}, err
		}
	}
	return credentials.Signed{}, failed("The sign-in was not approved in time.", credentials.Expired, false)
}
