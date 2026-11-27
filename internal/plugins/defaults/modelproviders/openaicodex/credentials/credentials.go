package credentials

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
	codexmodels "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/models"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// How long before its expiry a Codex access token is renewed
	skew = 2 * time.Minute
	// Fewest seconds between two looks at whether an exhausted account's quota came back early
	probeEvery = 5 * time.Minute
)

// Client of the Codex CLI's sign-in, and the endpoints it signs in at
const (
	ClientID  = "app_EMoamEEZ73f0CkXaXp7hrann"
	Issuer    = "https://auth.openai.com"
	TokenURL  = Issuer + "/oauth/token"
	Authorize = Issuer + "/oauth/authorize"
	// Code of a refusal OpenAI gave because of a rate limit, not a credential fault
	RateLimited = "codex_rate_limited"
)

// Codex sign-ins kept in accounts, renewed with their refresh tokens
type source struct {
	mu sync.Mutex
	// Last look at the quota of each token, by a hash of it, and whether it was back then
	probes map[string]probe
}

type probe struct {
	at       time.Time
	restored bool
}

func Register(registry *credentials.Registry) {
	registry.Register(&source{probes: map[string]probe{}})
}

// Access token of the account's ChatGPT sign-in, with the account headers the Codex backend wants
func (s *source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != openaicodexconfig.Name || account.Name == "" {
		return credentials.Credential{}, false
	}
	token := strings.TrimSpace(account.Fields.GetString(openaicodexconfig.AccessToken))
	if token == "" {
		return credentials.Credential{}, false
	}
	return credentials.Credential{APIKey: token, Auth: provider.AuthOAuth, Origin: credentials.Saved, Headers: codexmodels.AccountHeaders(token),
		Expires: credentials.JWTExpiry(token), Identity: Identity(token)}, true
}

// Who a Codex token signs in as: its email, else the ChatGPT account it names
func Identity(token string) string {
	if email := credentials.Identity(token); email != "" {
		return email
	}
	if id, _ := credentials.Claims(token)["https://api.openai.com/auth"].(map[string]any)["chatgpt_account_id"].(string); id != "" {
		return "account " + id
	}
	return ""
}

func (s *source) Due(account credentials.Account) bool {
	if account.Spec() != openaicodexconfig.Name || account.Name == "" || account.Fields.GetString(openaicodexconfig.RefreshToken) == "" {
		return false
	}
	expires := credentials.JWTExpiry(account.Fields.GetString(openaicodexconfig.AccessToken))
	return !expires.IsZero() && time.Now().Add(skew).After(expires)
}

// Whether the account holds a Codex sign-in, whose refresh token renews it once, so a copy in another profile can't share it
func (s *source) Rotates(account credentials.Account) bool {
	return account.Spec() == openaicodexconfig.Name && account.Name != "" && account.Fields.GetString(openaicodexconfig.RefreshToken) != ""
}

func (s *source) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	if account.Spec() != openaicodexconfig.Name || account.Name == "" {
		return false, nil
	}
	refresh := strings.TrimSpace(account.Fields.GetString(openaicodexconfig.RefreshToken))
	if refresh == "" {
		return false, nil
	}
	tokens, err := Refresh(ctx, refresh, timeout(account))
	if err != nil {
		return true, err
	}
	for field, value := range map[string]any{openaicodexconfig.AccessToken: tokens.Access, openaicodexconfig.RefreshToken: tokens.Refresh,
		openaicodexconfig.LastRefresh: tokens.Refreshed} {
		if err := account.Fields.Set(field, value); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Whether an account a quota stopped has its quota back before the time it was given, asked of the Codex usage endpoint at most once in
// five minutes for each token
func (s *source) Restored(ctx context.Context, account credentials.Account) bool {
	if account.Spec() != openaicodexconfig.Name || account.Name == "" {
		return false
	}
	status := credentials.StatusOf(account)
	text := strings.ToLower(status.Reason + " " + status.Message)
	if status.State != credentials.Exhausted || status.Code != 429 && !strings.Contains(text, "rate_limit") && !strings.Contains(text, "usage_limit") &&
		!strings.Contains(text, "quota") && !strings.Contains(text, "rate limit") && !strings.Contains(text, "usage limit") {
		return false
	}
	token := strings.TrimSpace(account.Fields.GetString(openaicodexconfig.AccessToken))
	if token == "" || len(credentials.Claims(token)) == 0 {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:8])
	s.mu.Lock()
	last, ok := s.probes[key]
	s.mu.Unlock()
	if ok && time.Since(last.at) < probeEvery {
		return last.restored
	}
	restored := quotaBack(ctx, token, account.Provider.Spec.BaseURL)
	s.mu.Lock()
	s.probes[key] = probe{at: time.Now(), restored: restored}
	s.mu.Unlock()
	return restored
}

// Whether every window of the account's Codex limits sits below one hundred percent
func quotaBack(ctx context.Context, token, base string) bool {
	normalized := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(base), "/"), "/codex")
	if normalized == "" {
		normalized = strings.TrimSuffix(openaicodexconfig.BaseURL, "/codex")
	}
	address := normalized + "/api/codex/usage"
	if strings.Contains(normalized, "/backend-api") {
		address = normalized + "/wham/usage"
	}
	header := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json", "User-Agent": "codex-cli"}
	for name, value := range codexmodels.AccountHeaders(token) {
		header[name] = value
	}
	payload, err := models.Request(ctx, "GET", address, header, nil, 10*time.Second)
	if err != nil {
		return false
	}
	windows := []any{payload["rate_limit"]}
	if extras, ok := payload["additional_rate_limits"].([]any); ok {
		for _, extra := range extras {
			if limits, ok := extra.(map[string]any); ok {
				windows = append(windows, limits["rate_limit"])
			}
		}
	}
	worst, known := 0.0, false
	for _, limits := range windows {
		limits, _ := limits.(map[string]any)
		for _, position := range []string{"primary_window", "secondary_window"} {
			window, _ := limits[position].(map[string]any)
			if used, ok := window["used_percent"].(float64); ok {
				worst, known = max(worst, used), true
			}
		}
	}
	return known && worst < 100
}

// Tokens of a Codex sign-in, as an exchange or a refresh gave them
type Tokens struct {
	Access    string
	Refresh   string
	Refreshed string
}

// Time a refresh may take, from the provider's setting, at least five seconds
func timeout(account credentials.Account) time.Duration {
	return max(time.Duration(account.Provider.Settings.GetInt(openaicodexconfig.RefreshTimeout))*time.Second, 5*time.Second)
}

// Renews Codex tokens with the refresh token; the old refresh token stays when the answer rotates none in
func Refresh(ctx context.Context, refresh string, wait time.Duration) (Tokens, error) {
	if strings.TrimSpace(refresh) == "" {
		return Tokens{}, failed(fmt.Sprintf("This ChatGPT sign-in keeps no way to renew itself. Run `%s` to sign in again.", credentials.Relogin(openaicodexconfig.Name)),
			"codex_auth_missing_refresh_token", true)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {ClientID}}
	reply, err := credentials.PostForm(ctx, credentials.Client(wait), TokenURL, form, map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")})
	if err != nil {
		return Tokens{}, err
	}
	switch {
	case reply.Status == 429:
		message := "Your ChatGPT plan's usage limit is reached, but the sign-in still works. Try again after the limit resets."
		failure := failed(message, RateLimited, false)
		if wait, ok := reply.RetryAfter(); ok {
			failure.Message = "Your ChatGPT plan's usage limit is reached, but the sign-in still works. Try again in " +
				strs.Span(max(wait.Round(time.Second), time.Second)) + "."
			failure.RetryAfter = wait
		}
		return Tokens{}, failure
	case reply.Status != 200:
		return Tokens{}, refusal(reply)
	}
	payload, err := reply.JSON()
	if err != nil {
		return Tokens{}, failed("OpenAI's answer to renewing the sign-in could not be read.", "codex_refresh_invalid_json", false)
	}
	access, _ := payload["access_token"].(string)
	if access = strings.TrimSpace(access); access == "" {
		return Tokens{}, failed("OpenAI renewed the sign-in without sending a new token. Sign in again.", "codex_refresh_missing_access_token", true)
	}
	tokens := Tokens{Access: access, Refresh: strings.TrimSpace(refresh), Refreshed: time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")}
	if next, _ := payload["refresh_token"].(string); strings.TrimSpace(next) != "" {
		tokens.Refresh = strings.TrimSpace(next)
	}
	return tokens, nil
}

// Failure of a refresh the token endpoint refused, telling a grant gone for good apart
func refusal(reply *credentials.Reply) *credentials.AuthError {
	code, message := "codex_refresh_failed", fmt.Sprintf("OpenAI refused to renew the sign-in, with HTTP status %d.", reply.Status)
	if payload, err := reply.JSON(); err == nil {
		switch detail := payload["error"].(type) {
		case map[string]any:
			code = cmp.Or(credentials.Text(detail["code"]), credentials.Text(detail["type"]), code)
			if text := credentials.Text(detail["message"]); text != "" {
				message = "OpenAI refused to renew the sign-in: " + strings.TrimSuffix(text, ".") + "."
			}
		case string:
			if strings.TrimSpace(detail) != "" {
				code = strings.TrimSpace(detail)
				if text := cmp.Or(credentials.Text(payload["error_description"]), credentials.Text(payload["message"])); text != "" {
					message = "OpenAI refused to renew the sign-in: " + strings.TrimSuffix(text, ".") + "."
				}
			}
		}
	}
	if code == "refresh_token_reused" {
		message = "Another program, such as Codex CLI or the VS Code extension, already used this sign-in's renewal. Run `codex` to sign in " +
			"there, then run `" + credentials.Relogin(openaicodexconfig.Name) + "` to sign in here again."
	}
	relogin := code == "invalid_grant" || code == "invalid_token" || code == "invalid_request" || code == "refresh_token_reused" ||
		reply.Status == 401 || reply.Status == 403
	return failed(message, code, relogin)
}

func failed(message, code string, relogin bool) *credentials.AuthError {
	return credentials.Failed(openaicodexconfig.Name, code, message, relogin, nil)
}
