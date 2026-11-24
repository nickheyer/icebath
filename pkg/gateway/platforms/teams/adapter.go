package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
)

const (
	maxBodyBytes = 1_048_576
	webhookPath  = "/api/messages"
	maxMessage   = 28000
	sentIDsKept  = 500
	eaCmdBudget  = 2000
	eaPreviewCap = 256

	defaultTeamsServiceURL = "https://smba.trafficmanager.net/teams/"
)

// Hosts that may receive a freshly minted bearer token, exact match only
var allowedTeamsServiceHosts = []string{"smba.infra.gov.teams.microsoft.us", "smba.trafficmanager.net"}

// Conversation id charset that cannot path traverse out of the activities endpoint
var teamsConvIDRe = func(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune(":@-_.", r)
		if !ok {
			return false
		}
	}
	return true
}

// Whether every character of a non empty string is a digit, the str.isdigit form
func strIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// Whether a URL is https on an allowlisted host, with the default port when asked
func isAllowedHTTPSHost(raw string, checkPort bool) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	if checkPort {
		if p := parsed.Port(); p != "" && p != "443" {
			return false
		}
	}
	host := strings.ToLower(parsed.Hostname())
	for _, h := range allowedTeamsServiceHosts {
		if host == h {
			return true
		}
	}
	return false
}

// Whether a URL is a Bot Framework connector attachment host that may carry the bot token
func isBotframeworkAttachmentURL(raw string) bool { return isAllowedHTTPSHost(raw, true) }

// Service URL with a trailing slash, empty when it is not on the allowlist
func validateTeamsServiceURL(raw string) string {
	if raw == "" || !isAllowedHTTPSHost(raw, false) {
		return ""
	}
	if strings.HasSuffix(raw, "/") {
		return raw
	}
	return raw + "/"
}

// Teams chat types to normalized chat types
var chatTypes = map[string]string{"personal": "dm", "groupChat": "group", "channel": "channel"}

// Native Teams app state, its app id and the conversation references learned from inbound activities
type teamsApp struct {
	id        string
	validator *inboundTokenValidator
}

// Microsoft Teams adapter over the Bot Framework REST API with an inbound webhook
type TeamsAdapter struct {
	*gateway.BasePlatformAdapter

	clientID       string
	clientSecret   string
	tenantID       string
	port           int
	host           *string
	requireMention bool

	mu               sync.Mutex
	tokenMu          sync.Mutex
	bfTokenCache     *bfToken
	app              *teamsApp
	runner           *gateway.HTTPAppRunner
	dedup            *gateway.MessageDeduplicator
	convRefs         map[string]conversationRef
	sentIDs          []string
	approvalPreviews map[string][2]string
	approvalOrder    []string
}

// Cached Bot Framework bearer token with its monotonic expiry
type bfToken struct {
	token   string
	expires time.Time
}

// Class attributes the Teams adapter declares over the base defaults
func teamsAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.ServesProfilePrefix = true
	traits.MaxMessageLength = maxMessage
	traits.SplitsLongMessages = true
	traits.EACmdBudget = eaCmdBudget
	return traits
}

// Client id, client secret and tenant id from extra first, then the profile scoped env
func credentials(ctx context.Context, config *gateway.PlatformConfig) (string, string, string, error) {
	var extra map[string]any
	if config != nil {
		extra = config.Extra
	}
	out := make([]string, 3)
	for i, pair := range [][2]string{{"client_id", "TEAMS_CLIENT_ID"}, {"client_secret", "TEAMS_CLIENT_SECRET"}, {"tenant_id", "TEAMS_TENANT_ID"}} {
		if v := extra[pair[0]]; anyutil.IsTrue(v) {
			out[i] = anyutil.String(v)
			continue
		}
		val, err := gateway.GetScopedSecret(ctx, pair[1], "", false)
		if err != nil {
			return "", "", "", err
		}
		out[i] = val
	}
	return out[0], out[1], out[2], nil
}

// Adapter for a platform config with its credentials and the port, host and mention gate of the profile served
func NewTeamsAdapter(ctx context.Context, config *gateway.PlatformConfig) (*TeamsAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &TeamsAdapter{dedup: gateway.NewMessageDeduplicator(1000, 300), convRefs: map[string]conversationRef{},
		approvalPreviews: map[string][2]string{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("teams"), a)
	a.Traits = teamsAdapterTraits()
	var err error
	if a.clientID, a.clientSecret, a.tenantID, err = credentials(ctx, config); err != nil {
		return nil, err
	}
	sctx := gateway.SettingsContext(ctx)
	a.port = settings.TeamsPort.Get(sctx)
	if host := settings.TeamsHost.Get(sctx); host != "" {
		a.host = &host
	}
	a.requireMention = settings.TeamsRequireMention.Get(sctx)
	return a, nil
}

// Validates credentials, wires the activity routes and binds or publishes the webhook listener
func (a *TeamsAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.clientID == "" || a.clientSecret == "" || a.tenantID == "" {
		a.SetFatalError(ctx, "MISSING_CREDENTIALS", "TEAMS_CLIENT_ID, TEAMS_CLIENT_SECRET, and TEAMS_TENANT_ID are all required", false)
		return false, nil
	}
	if err := a.connect(ctx); err != nil {
		a.SetFatalError(ctx, "CONNECT_FAILED", fmt.Sprintf("Teams connection failed: %v", err), true)
		pkgLog().Error(fmt.Sprintf("[teams] Failed to connect: %v", err))
		return false, nil
	}
	return true, nil
}

// Builds the web app with the health and activity routes, wires plugins and binds the listener
func (a *TeamsAdapter) connect(ctx context.Context) error {
	app := gateway.NewHTTPApp(maxBodyBytes)
	if err := app.AddGet("/health", func(context.Context, *gateway.WebRequest) (*gateway.WebResponse, error) {
		return gateway.WebTextResponse(200, "ok", "", nil), nil
	}); err != nil {
		return err
	}
	native := &teamsApp{id: a.clientID, validator: newInboundTokenValidator(a.clientID)}
	a.mu.Lock()
	a.app = native
	a.mu.Unlock()
	a.WirePluginHandlers(ctx, native)
	if err := app.AddPost(webhookPath, a.handleActivityRequest); err != nil {
		return err
	}
	runner, err := gateway.BindListener(ctx, a, app, a.host, a.port, webhookPath, gateway.ListenOptions{})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.runner = runner
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	if runner != nil {
		hostText := "* (all interfaces, IPv4+IPv6)"
		if a.host != nil {
			hostText = *a.host
		}
		pkgLog().Info(fmt.Sprintf("[teams] Webhook server listening on %s:%d%s", hostText, a.port, webhookPath))
	}
	return nil
}

// Stops the listener, drops the app and marks the adapter disconnected
func (a *TeamsAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	runner := a.runner
	a.runner, a.app = nil, nil
	a.mu.Unlock()
	if runner != nil {
		if err := runner.Cleanup(ctx); err != nil {
			return err
		}
	}
	a.MarkDisconnected(ctx)
	pkgLog().Info("[teams] Disconnected")
	return nil
}

// Bot Framework bearer token from the client credentials flow, cached until five minutes before expiry
func (a *TeamsAdapter) getBotFrameworkToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if cached := a.bfTokenCache; cached != nil && cached.expires.After(time.Now().Add(300*time.Second)) {
		return cached.token, nil
	}
	if a.clientID == "" || a.clientSecret == "" || a.tenantID == "" {
		return "", &gateway.PyError{Kind: "ValueError", Msg: "Missing TEAMS_CLIENT_ID/SECRET/TENANT_ID for attachment auth"}
	}
	tokenURL, form := bfTokenRequest(a.tenantID, a.clientID, a.clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload map[string]any
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New(statusErrorText(resp, tokenURL))
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	expiresIn := 3600.0
	if v, ok := payload["expires_in"]; ok && anyutil.IsTrue(v) {
		if f, err := anyutil.ToFloat(v); err == nil {
			expiresIn = f
		} else {
			return "", err
		}
	}
	token, ok := payload["access_token"].(string)
	if !ok {
		return "", &gateway.PyError{Kind: "KeyError", Msg: "'access_token'"}
	}
	a.bfTokenCache = &bfToken{token: token, expires: time.Now().Add(time.Duration(expiresIn * float64(time.Second)))}
	return token, nil
}

// Downloads attachment bytes behind the SSRF guard, connector URLs carrying the bot bearer token
func (a *TeamsAdapter) fetchAttachmentBytes(ctx context.Context, rawURL string, timeout time.Duration) ([]byte, error) {
	env := gateway.URLSafetyEnv(ctx)
	if !tools.IsSafeURL(env, rawURL) {
		return nil, &gateway.PyError{Kind: "ValueError", Msg: "Blocked unsafe attachment URL (SSRF protection)"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; IcebathAgent/1.0)")
	if isBotframeworkAttachmentURL(rawURL) {
		if token, err := a.getBotFrameworkToken(ctx); err != nil {
			pkgLog().Warn(fmt.Sprintf("[teams] Could not acquire Bot Framework token for attachment: %v", err))
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	follow := true
	client := gateway.PlatformHTTPClient(ctx, gateway.PlatformHTTPClientOptions{Timeout: &timeout, FollowRedirects: &follow, DefaultLimits: true})
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New(statusErrorText(resp, rawURL))
	}
	return gateway.ReadHTTPBodyWithLimit(ctx, resp, "attachment")
}

// httpx raise_for_status text of a response
func statusErrorText(resp *http.Response, rawURL string) string {
	return (&httputil.StatusError{StatusCode: resp.StatusCode, URL: rawURL, Location: resp.Header.Get("Location")}).Error()
}

// Chat info of a conversation id
func (a *TeamsAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "unknown", "chat_id": chatID}, nil
}
