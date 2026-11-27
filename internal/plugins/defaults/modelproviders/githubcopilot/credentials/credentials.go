package credentials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// GitHub App of VS Code's Copilot, whose ghu_ tokens the Copilot API exchanges
	ClientID = "Iv1.b507a08c87ecfe98"
	// Where a GitHub token is exchanged for a Copilot API token
	exchangeURL = "https://api.github.com/copilot_internal/v2/token"
	// What the exchange and the Copilot API are told the client is
	EditorVersion    = "vscode/1.104.1"
	exchangeAgent    = "GitHubCopilotChat/0.26.7"
	RequestUserAgent = "IcebathAgent/1.0"
	// An exchanged token is renewed this long before it expires
	margin = 2 * time.Minute
	// Tries of one exchange, the wait growing by this between them
	attempts = 3
	backoff  = 1500 * time.Millisecond
	// How long a failed exchange isn't tried again: one that may pass, and one GitHub refused
	transientMemo = time.Minute
	permanentMemo = 30 * time.Minute
)

// Prefixes of the GitHub tokens the Copilot API takes
var prefixes = []string{"gho_", "github_pat_", "ghu_"}

// Variables a GitHub token is read from, in order
var variables = []string{githubcopilotconfig.CopilotTokenEnv, githubcopilotconfig.GHTokenEnv, githubcopilotconfig.GitHubTokenEnv}

// Refusals of a model that mean the exchanged token went stale or degraded
var staleMarkers = []string{"model_not_available_for_integrator", "not available for integrator", "model_not_supported", "the requested model is not supported"}

// proxy-ep field of an exchanged token naming the account's API host
var proxyEndpoint = regexp.MustCompile(`(?:^|;)\s*proxy-ep=([^;\s]+)`)

// Copilot API token one GitHub token was exchanged for, its expiry and the account's API address
type exchanged struct {
	Token   string  `json:"api_token"`
	Expires float64 `json:"expires_at"`
	BaseURL string  `json:"base_url,omitempty"`
}

func (e exchanged) fresh() bool {
	return e.Token != "" && time.Now().Add(margin).Before(time.Unix(int64(e.Expires), 0))
}

// GitHub tokens of accounts and variables, each exchanged for a Copilot API token kept in memory and on disk until it nears expiry
type source struct {
	mu       sync.Mutex
	tokens   map[string]exchanged
	failures map[string]time.Time
	// One exchange at a time for each GitHub token
	locks map[string]*sync.Mutex
	// Tokens whose exchange already warned that it fell back to the raw token
	warned map[string]bool
}

func Register(registry *credentials.Registry) {
	registry.Register(&source{tokens: map[string]exchanged{}, failures: map[string]time.Time{}, locks: map[string]*sync.Mutex{}, warned: map[string]bool{}})
	registry.Register(acp{})
}

// Evidence that the Copilot CLI behind copilot-acp can sign in: a GitHub token it takes in its variables, its own files never read
type acp struct{}

func (acp) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != githubcopilotconfig.ACPName {
		return credentials.Credential{}, false
	}
	for _, variable := range variables {
		if found, ok := account.Lookup("", variable); ok {
			if supported, _ := Supported(found.APIKey); supported {
				found.Auth = provider.AuthOAuth
				return found, true
			}
		}
	}
	return credentials.Credential{}, false
}

// Whether the Copilot API takes the token, and why not when it doesn't
func Supported(token string) (bool, string) {
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return false, "The token is empty."
	case strings.HasPrefix(token, "ghp_"):
		return false, "Copilot does not take classic personal access tokens, which start with ghp_. Sign in with `icebath auth add copilot`, " +
			"or use a fine-grained token, starting with github_pat_, that has the Copilot Requests permission."
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(token, prefix) {
			return true, "OK"
		}
	}
	return false, "Copilot does not take this kind of GitHub token. Use one that starts with " + strings.Join(prefixes, ", ") + "."
}

// GitHub token of the account, else of the first variable holding one the Copilot API takes
func raw(account credentials.Account) (credentials.Credential, bool) {
	if found, ok := account.Lookup(providerconfig.APIKeyField); ok {
		if supported, _ := Supported(found.APIKey); supported {
			return found, true
		}
	}
	for _, variable := range variables {
		found, ok := account.Lookup("", variable)
		if !ok {
			continue
		}
		if supported, why := Supported(found.APIKey); supported {
			return found, true
		} else {
			slog.Warn("A GitHub token is not one the Copilot API takes", "variable", variable, "why", why)
		}
	}
	return credentials.Credential{}, false
}

// Whether the profile chose Copilot, so its GitHub token may be sent to GitHub's exchange; a kind looked for without a provider isn't
func chosen(account credentials.Account) bool {
	return account.Provider.Settings.Prefix() != ""
}

func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// Headers every request to the Copilot API carries
func Headers() map[string]string {
	return map[string]string{"Editor-Version": EditorVersion, "User-Agent": RequestUserAgent, "Copilot-Integration-Id": "vscode-chat",
		"Openai-Intent": "conversation-edits", "x-initiator": "agent"}
}

// Copilot API token of the account's GitHub token while an exchange of it is fresh, else the GitHub token itself until one is
func (s *source) Credential(account credentials.Account) (credentials.Credential, bool) {
	if account.Spec() != githubcopilotconfig.CopilotName {
		return credentials.Credential{}, false
	}
	found, ok := raw(account)
	if !ok {
		return credentials.Credential{}, false
	}
	found.Auth, found.Headers = provider.AuthAPIKey, Headers()
	if !chosen(account) {
		return found, true
	}
	if token, ok := s.cached(found.APIKey); ok {
		found.APIKey, found.BaseURL, found.Expires = token.Token, token.BaseURL, time.Unix(int64(token.Expires), 0)
	}
	return found, true
}

// Exchange of the GitHub token kept in memory, else on disk, while fresh
func (s *source) cached(githubToken string) (exchanged, bool) {
	key := fingerprint(githubToken)
	s.mu.Lock()
	token, ok := s.tokens[key]
	s.mu.Unlock()
	if ok && token.fresh() {
		return token, true
	}
	stored := load()
	if token, ok := stored[key]; ok && token.fresh() {
		s.mu.Lock()
		s.tokens[key] = token
		s.mu.Unlock()
		return token, true
	}
	return exchanged{}, false
}

// File the exchanged tokens are kept in across runs, by a hash of the GitHub token each was exchanged for
func store() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "icebath", "copilot_jwt.json")
}

func load() map[string]exchanged {
	stored := map[string]exchanged{}
	raw, err := os.ReadFile(store())
	if err != nil || len(raw) > 1<<20 {
		return stored
	}
	json.Unmarshal(raw, &stored)
	return stored
}

// Keeps the exchange on disk, dropping the expired ones, or forgets it for no token
func save(key string, token exchanged) {
	stored := load()
	for k, kept := range stored {
		if time.Now().After(time.Unix(int64(kept.Expires), 0)) {
			delete(stored, k)
		}
	}
	if token.Token == "" {
		delete(stored, key)
	} else {
		stored[key] = token
	}
	encoded, err := json.Marshal(stored)
	if err == nil {
		err = fsx.Write(store(), encoded, 0o600)
	}
	if err != nil {
		slog.Debug("The exchanged Copilot token could not be kept", "error", err)
	}
}

// Whether the account's GitHub token has no fresh exchange yet
func (s *source) Due(account credentials.Account) bool {
	if account.Spec() != githubcopilotconfig.CopilotName || !chosen(account) {
		return false
	}
	found, ok := raw(account)
	if !ok {
		return false
	}
	_, fresh := s.cached(found.APIKey)
	return !fresh
}

// Exchanges the account's GitHub token for a Copilot API token anew, the one kept forgotten first
func (s *source) Renew(ctx context.Context, account credentials.Account) (bool, error) {
	if account.Spec() != githubcopilotconfig.CopilotName || !chosen(account) {
		return false, nil
	}
	found, ok := raw(account)
	if !ok {
		return false, nil
	}
	key := fingerprint(found.APIKey)
	s.mu.Lock()
	delete(s.tokens, key)
	delete(s.failures, key)
	lock, ok := s.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[key] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	save(key, exchanged{})
	token, err := s.exchange(ctx, found.APIKey, key)
	if err != nil {
		s.mu.Lock()
		first := !s.warned[key]
		s.warned[key] = true
		s.mu.Unlock()
		if first {
			slog.Warn("Copilot token exchange degraded to RAW token (exchange unavailable); enterprise-only models may 400 with " +
				"model_not_available_for_integrator until exchange recovers.")
		}
		return true, err
	}
	s.mu.Lock()
	s.tokens[key] = token
	s.mu.Unlock()
	save(key, token)
	return true, nil
}

// Whether a 400 says the model left the exchanged token, which a new exchange fixes
func (s *source) Stale(account credentials.Account, failure *provider.APIError) bool {
	if account.Spec() != githubcopilotconfig.CopilotName {
		return false
	}
	text := strings.ToLower(failure.Message)
	if failure.StatusCode != 400 && !strings.Contains(text, "error code: 400") {
		return false
	}
	for _, marker := range staleMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// One exchange of the GitHub token, tried three times unless GitHub refuses it; a failure isn't tried again for a while
func (s *source) exchange(ctx context.Context, githubToken, key string) (exchanged, error) {
	s.mu.Lock()
	until := s.failures[key]
	s.mu.Unlock()
	if left := time.Until(until); left > 0 {
		return exchanged{}, fmt.Errorf("GitHub refused Copilot access a moment ago, so icebath waits %s before it asks again.",
			strs.Span(max(left.Round(time.Second), time.Second)))
	}
	var last error
	permanent := false
	for attempt := 1; attempt <= attempts; attempt++ {
		token, status, err := once(ctx, githubToken)
		if err == nil {
			return token, nil
		}
		last = err
		if permanent = status == 401 || status == 403 || status == 404; permanent {
			break
		}
		if attempt < attempts {
			if err := credentials.Sleep(ctx, time.Duration(attempt)*backoff); err != nil {
				return exchanged{}, err
			}
		}
	}
	memo := transientMemo
	if permanent {
		memo = permanentMemo
	}
	s.mu.Lock()
	s.failures[key] = time.Now().Add(memo)
	s.mu.Unlock()
	return exchanged{}, fmt.Errorf("GitHub did not grant Copilot access after %d tries: %w", attempts, last)
}

func once(ctx context.Context, githubToken string) (exchanged, int, error) {
	capped, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	reply, err := credentials.Get(capped, credentials.Client(15*time.Second), exchangeURL, map[string]string{"Authorization": "token " + githubToken,
		"User-Agent": exchangeAgent, "Accept": "application/json", "Editor-Version": EditorVersion})
	if err != nil {
		return exchanged{}, 0, err
	}
	if reply.Status < 200 || reply.Status >= 300 {
		return exchanged{}, reply.Status, fmt.Errorf("GitHub answered with HTTP status %d", reply.Status)
	}
	payload, err := reply.JSON()
	if err != nil {
		return exchanged{}, reply.Status, err
	}
	token := exchanged{Token: credentials.Text(payload["token"])}
	if token.Token == "" {
		return exchanged{}, reply.Status, errors.New("GitHub granted Copilot access without sending a token")
	}
	if expires, ok := payload["expires_at"].(float64); ok && expires > 0 {
		token.Expires = expires
	} else {
		token.Expires = float64(time.Now().Add(30 * time.Minute).Unix())
	}
	if endpoints, ok := payload["endpoints"].(map[string]any); ok {
		token.BaseURL = strings.TrimRight(credentials.Text(endpoints["api"]), "/")
	}
	if token.BaseURL == "" {
		token.BaseURL = account(token.Token)
	}
	return token, reply.Status, nil
}

// Address of the account's Copilot API from the proxy-ep field of the token, proxy made api; empty when it names none
func account(token string) string {
	match := proxyEndpoint.FindStringSubmatch(token)
	if match == nil {
		return ""
	}
	host := strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(match[1], "https://"), "http://"), "/")
	if rest, ok := strings.CutPrefix(host, "proxy."); ok {
		host = "api." + rest
	}
	return "https://" + host
}

// GitHub device code sign-in for Copilot, the approved token checked against what the Copilot API takes
var SignIn = credentials.SignIn{Spec: githubcopilotconfig.CopilotName, Title: "GitHub account with a device code", Type: credentials.OAuth, Run: signIn}

func signIn(p *credentials.Prompter) (credentials.Signed, error) {
	client := credentials.Client(15 * time.Second)
	header := map[string]string{"Accept": "application/json", "User-Agent": RequestUserAgent}
	reply, err := credentials.PostForm(p.Ctx, client, "https://github.com/login/device/code", url.Values{"client_id": {ClientID}, "scope": {"read:user"}}, header)
	if err != nil {
		return credentials.Signed{}, err
	}
	device, err := reply.JSON()
	if err != nil || reply.Status != 200 {
		return credentials.Signed{}, fmt.Errorf("GitHub refused to hand out a sign-in code, with HTTP status %d. Try again later.", reply.Status)
	}
	code, user := credentials.Text(device["device_code"]), credentials.Text(device["user_code"])
	if code == "" || user == "" {
		return credentials.Signed{}, errors.New("GitHub did not hand out a sign-in code. Try again later.")
	}
	interval, ok := config.Whole(device["interval"])
	if !ok || interval < 1 {
		interval = 5
	}
	expires, ok := config.Whole(device["expires_in"])
	if !ok || expires <= 0 {
		expires = 900
	}
	link := credentials.Text(device["verification_uri"])
	if link == "" {
		link = "https://github.com/login/device"
	}
	p.Instructions(link, user)
	p.Pending("Waiting for you to approve the sign-in.")
	payload, err := credentials.Poll{
		Provider: githubcopilotconfig.CopilotName,
		Post: func() (*credentials.Reply, error) {
			return credentials.PostForm(p.Ctx, credentials.Client(10*time.Second), "https://github.com/login/oauth/access_token",
				url.Values{"client_id": {ClientID}, "device_code": {code}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, header)
		},
		Expires: time.Duration(expires) * time.Second,
		// GitHub asks a little slower than the interval it names
		Interval: time.Duration(interval+3) * time.Second,
	}.Run(p.Ctx)
	if err != nil {
		return credentials.Signed{}, err
	}
	token := credentials.Text(payload["access_token"])
	if supported, why := Supported(token); !supported {
		return credentials.Signed{}, credentials.Failed(githubcopilotconfig.CopilotName, "copilot_token_unsupported", why, false, nil)
	}
	return credentials.Signed{Fields: map[string]any{providerconfig.APIKeyField: token}}, nil
}
