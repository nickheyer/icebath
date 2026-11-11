package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Status of a provider nobody is signed in to
func oauthLoggedOut() *pyjson.Object { return pyjson.ObjectOf("logged_in", false, "source", nil) }

// Last characters of a token for display, the signature tail of a JWT and never the value a bearer callable yields
func truncateToken(value any, visible int) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	if _, isText := value.(string); !isText && reflect.ValueOf(value).Kind() == reflect.Func {
		return "<entra-id-bearer>"
	}
	s := pyrepr.Str(value)
	if strings.Count(s, ".") >= 2 {
		s = s[strings.LastIndex(s, ".")+1:]
	}
	runes := []rune(s)
	if len(runes) <= visible {
		return s
	}
	return "…" + string(runes[len(runes)-visible:])
}

// Signed in status card of a credential file with its token reduced to a preview
func tokenStatus(source, sourceLabel string, creds map[string]any) *pyjson.Object {
	return pyjson.ObjectOf("logged_in", true, "source", source, "source_label", sourceLabel,
		"token_preview", truncateToken(creds["accessToken"], 6),
		"expires_at", creds["expiresAt"], "has_refresh_token", pyrepr.Truthy(creds["refreshToken"]))
}

// Status of the Anthropic account card, the icebath PKCE file first and then the registry ordered env vars
func anthropicOAuthStatus(ctx context.Context) (*pyjson.Object, error) {
	env := bootstrapPoolEnv(ctx)
	if creds := provider.ReadHermesOAuthCredentials(env); len(creds) > 0 && pyrepr.Truthy(creds["accessToken"]) {
		return tokenStatus("hermes_pkce", fmt.Sprintf("Icebath PKCE (%s)", provider.HermesOAuthFile(env)), creds), nil
	}
	envVarOrder := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
	if row := provider.AuthRegistryGet(ctx, "anthropic"); row != nil {
		envVarOrder = row.KeyEnv
	}
	h := home.Dir(ctx)
	for _, name := range envVarOrder {
		value, _, err := config.GetEnvValue(ctx, h, name)
		if err != nil {
			return nil, err
		}
		if value == "" {
			value = os.Getenv(name)
		}
		if value != "" {
			return pyjson.ObjectOf("logged_in", true, "source", "env_var", "source_label", name+provider.FormatSecretSourceSuffix(h, name),
				"token_preview", truncateToken(value, 6), "expires_at", nil, "has_refresh_token", false), nil
		}
	}
	return oauthLoggedOut(), nil
}

// Status of the Claude Code CLI credentials as their own card, signed in only while the stored token is valid
func claudeCodeOnlyStatus(ctx context.Context) (*pyjson.Object, error) {
	creds := provider.ReadClaudeCodeCredentials(bootstrapPoolEnv(ctx))
	if len(creds) > 0 && provider.IsClaudeCodeTokenValid(creds) {
		return tokenStatus("claude_code_cli", "~/.claude/.credentials.json", creds), nil
	}
	return oauthLoggedOut(), nil
}

// Sign in command of an external process provider with the CLI its profile configures, the default otherwise
func externalProcessCLICommand(ctx context.Context, providerID, fallback string) string {
	row := provider.AuthRegistryGet(ctx, providerID)
	if row == nil || provider.RegistryAuthType(*row) != provider.AuthTypeExternalProcess {
		return fallback
	}
	status := provider.GetExternalProcessProviderStatus(bootstrapPoolEnv(ctx), providerID)
	command := ""
	if v := status["command"]; pyrepr.Truthy(v) {
		command = pystr.Strip(pyrepr.Str(v))
	}
	if command == "" {
		return fallback
	}
	if _, tail, found := strings.Cut(fallback, " "); found {
		return command + " " + tail
	}
	return command
}

// One account card of the OAuth catalog with the status reader a hand tuned card carries
type oauthCatalogEntry struct {
	id, name, flow, cliCommand, docsURL string
	statusFn                            func(ctx context.Context) (*pyjson.Object, error)
}

var oauthProviderCatalog = []oauthCatalogEntry{
	{id: "nous", name: "Nous Portal", flow: "device_code", cliCommand: "icebath auth add nous", docsURL: "https://portal.nousresearch.com"},
	{id: "openai-codex", name: "ChatGPT or Codex Subscription", flow: "device_code", cliCommand: "icebath auth add openai-codex",
		docsURL: "https://platform.openai.com/docs"},
	{id: "qwen-oauth", name: "Qwen (via Qwen CLI)", flow: "external", cliCommand: "icebath auth add qwen-oauth",
		docsURL: "https://github.com/QwenLM/qwen-code"},
	{id: "minimax-oauth", name: "MiniMax (OAuth)", flow: "device_code", cliCommand: "icebath auth add minimax-oauth",
		docsURL: "https://www.minimax.io"},
	{id: "xai-oauth", name: "xAI Grok OAuth (SuperGrok / Premium+)", flow: "device_code", cliCommand: "icebath auth add xai-oauth",
		docsURL: "https://hermes-agent.nousresearch.com/docs/guides/xai-grok-oauth"},
	{id: "anthropic", name: "Anthropic Account", flow: "external", cliCommand: "icebath auth add anthropic",
		docsURL: "https://docs.claude.com/en/api/getting-started", statusFn: anthropicOAuthStatus},
	{id: "claude-code", name: "Anthropic OAuth: Required Extra Usage Credits to Use Subscription", flow: "external",
		cliCommand: "claude setup-token", docsURL: "https://docs.claude.com/en/docs/claude-code", statusFn: claudeCodeOnlyStatus},
}

var (
	oauthSessionsMu sync.Mutex
	oauthSessions   = map[string]map[string]any{}
)

// Profile name an OAuth session is bound to, empty for none or the current profile
func oauthProfileName(profile string) string {
	requested := pystr.Strip(profile)
	if requested == "" || pystr.Lower(requested) == "current" {
		return ""
	}
	return requested
}

// Session profile as the selector the profile scopes take, empty when the session has none
func oauthSessionProfile(sess map[string]any) string {
	name, _ := sess["profile"].(string)
	return name
}

// Wraps a poller body so a vanished session is ignored, success approves and a failure is recorded on the session
func oauthPoller(label string, fn func(ctx context.Context, sessionID string, sess map[string]any) error) func(ctx context.Context, sessionID string) {
	return func(ctx context.Context, sessionID string) {
		oauthSessionsMu.Lock()
		sess := oauthSessions[sessionID]
		oauthSessionsMu.Unlock()
		if len(sess) == 0 {
			return
		}
		if err := fn(ctx, sessionID, sess); err != nil {
			routersLog().Warn(fmt.Sprintf("%s device-code poll failed (session=%s): %s", label, sessionID, err))
			oauthSessionsMu.Lock()
			observability.NoteOAuthFailure(sess, err)
			sess["status"] = "error"
			sess["error_message"] = err.Error()
			oauthSessionsMu.Unlock()
			return
		}
		oauthSessionsMu.Lock()
		status := sess["status"]
		settled := status != "pending"
		if !settled {
			sess["status"] = "approved"
		}
		oauthSessionsMu.Unlock()
		if settled {
			routersLog().Info(fmt.Sprintf("oauth/device: %s login ended %s (session=%s)", label, pyrepr.Str(status), sessionID))
		} else {
			routersLog().Info(fmt.Sprintf("oauth/device: %s login completed (session=%s)", label, sessionID))
		}
	}
}

// Writes one sign in state onto the dashboard session under the sessions lock
func recordSignInState(sess map[string]any, state provider.SignInState) {
	kind := state.Kind()
	if kind == "code" || kind == "waiting" {
		return
	}
	oauthSessionsMu.Lock()
	defer oauthSessionsMu.Unlock()
	switch s := state.(type) {
	case provider.SignInCompleted:
		sess["account_email"], sess["model"] = nil, nil
		if s.Email != "" {
			sess["account_email"] = s.Email
		}
		if s.ModelChanged {
			sess["model"] = s.Model
		}
		handoff := make([]any, 0, len(s.Handoff))
		for _, line := range s.Handoff {
			handoff = append(handoff, line)
		}
		sess["handoff"] = handoff
		sess["handoff_status"] = nil
		if s.HandoffStatus != nil {
			sess["handoff_status"] = s.HandoffStatus
		}
		return
	case provider.SignInCancelled:
		sess["status"] = "cancelled"
		return
	case provider.SignInTimedOut:
		sess["status"], sess["reason"] = "error", "timeout"
		sess["error_message"] = s.Copy()
		if s.Detail != "" {
			sess["error_message"] = s.Detail
		}
		return
	}
	sess["status"] = "error"
	reason := kind
	if failed, ok := state.(provider.SignInFailed); ok && failed.Reason != "" {
		reason = failed.Reason
	}
	if reason == "" {
		reason = "error"
	}
	sess["reason"] = reason
	sess["error_message"] = state.Copy()
	retryable, retryAfter := false, int64(0)
	if r, ok := state.(interface{ Retryable() bool }); ok {
		retryable = r.Retryable()
	}
	if failed, ok := state.(provider.SignInFailed); ok {
		retryAfter = int64(failed.RetryAfter)
	}
	sess["retryable"], sess["retry_after"] = retryable, retryAfter
}

// Sign in flow a session holds, pulled one state at a time and stopped when its poller leaves
type signInPull struct {
	next func() (provider.SignInState, bool)
	stop func()
}

// Drains the Nous sign in the start route began, each state rendered onto the session
func nousSignInPollerBody(_ context.Context, _ string, sess map[string]any) error {
	gen, _ := sess["_sign_in"].(*signInPull)
	if gen == nil {
		return nil
	}
	defer gen.stop()
	for {
		state, ok := gen.next()
		if !ok {
			return nil
		}
		recordSignInState(sess, state)
	}
}

// Int of a decoded number, an error naming the key when the payload lacks it or holds no number
func oauthInt(payload map[string]any, key string) (int64, error) {
	value, present := payload[key]
	if !present {
		return 0, fmt.Errorf("KeyError: %s", pyrepr.StrRepr(key))
	}
	n, ok := statusNumber(value)
	if !ok {
		text, isText := value.(string)
		parsed, err := pyjson.Loads(pystr.Strip(text))
		if !isText || err != nil {
			return 0, fmt.Errorf("TypeError: int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(value))
		}
		if n, ok = statusNumber(parsed); !ok {
			return 0, fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", pyrepr.StrRepr(text))
		}
	}
	return int64(n), nil
}

// Polls MiniMax until the user code is approved, then saves the auth state the CLI login would save
func minimaxPollerBody(ctx context.Context, _ string, sess map[string]any) error {
	portalBaseURL, _ := sess["portal_base_url"].(string)
	clientID, _ := sess["client_id"].(string)
	userCode, _ := sess["user_code"].(string)
	verifier, _ := sess["code_verifier"].(string)
	expiredIn, _ := sess["expired_in_raw"].(int64)
	var intervalMS *int
	if v, ok := sess["interval_ms"].(int64); ok {
		n := int(v)
		intervalMS = &n
	}
	tokenData, err := oauthMinimaxPollToken(ctx, provider.MinimaxHTTPClient(15*time.Second), portalBaseURL, clientID, userCode, verifier, expiredIn, intervalMS)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tokenExpiredIn, err := oauthInt(tokenData, "expired_in")
	if err != nil {
		return err
	}
	expiresAtTS := provider.MinimaxResolveTokenExpiryUnix(tokenExpiredIn, now)
	for _, key := range []string{"access_token", "refresh_token"} {
		if _, ok := tokenData[key]; !ok {
			return fmt.Errorf("KeyError: %s", pyrepr.StrRepr(key))
		}
	}
	tokenType, hasType := tokenData["token_type"]
	if !hasType {
		tokenType = "Bearer"
	}
	region, hasRegion := sess["region"]
	if !hasRegion {
		region = "global"
	}
	expiresAt, err := pydatetime.FromTimestampIn(expiresAtTS, time.UTC, pydatetime.LocaltimeAccepts)
	if err != nil {
		return err
	}
	authState := map[string]any{
		"provider": "minimax-oauth", "region": region, "portal_base_url": portalBaseURL,
		"inference_base_url": provider.MinimaxOAuthGlobalInference, "client_id": clientID, "scope": provider.MinimaxOAuthScope,
		"token_type": tokenType, "access_token": tokenData["access_token"], "refresh_token": tokenData["refresh_token"],
		"resource_url": tokenData["resource_url"], "obtained_at": pydatetime.ISOFormat(now),
		"expires_at": pydatetime.ISOFormat(expiresAt),
		"expires_in": max(int64(0), int64(expiresAtTS-pydatetime.Timestamp(now))),
	}
	sctx, _, release, err := profileScope(ctx, oauthSessionProfile(sess))
	if err != nil {
		return err
	}
	defer release()
	oauthSessionsMu.Lock()
	defer oauthSessionsMu.Unlock()
	if pyrepr.Truthy(sess["cancelled"]) {
		sess["status"] = "cancelled"
		return nil
	}
	return provider.MinimaxSaveAuthState(bootstrapPoolEnv(sctx), authState, true)
}

// Stripped text of a token field, empty for a missing or null one
func tokenText(payload map[string]any, key string) string {
	if v := payload[key]; pyrepr.Truthy(v) {
		return pystr.Strip(pyrepr.Str(v))
	}
	return ""
}

// Polls xAI until the device code is approved, then saves the tokens without taking over the active provider
func xaiDevicePollerBody(ctx context.Context, _ string, sess map[string]any) error {
	discovery, err := provider.XAIOAuthDiscovery(ctx, 20*time.Second)
	if err != nil {
		return err
	}
	deviceCode, _ := sess["device_code"].(string)
	expiresAt, _ := statusNumber(sess["expires_at"])
	interval, _ := statusNumber(sess["interval"])
	tokenData, err := provider.XAIOAuthPollDeviceToken(ctx, provider.XAIHTTPClient(20*time.Second), discovery["token_endpoint"], deviceCode,
		max(60, int(expiresAt-float64(time.Now().UnixNano())/1e9)), int(interval))
	if err != nil {
		return err
	}
	tokenType := tokenText(tokenData, "token_type")
	if tokenType == "" {
		tokenType = "Bearer"
	}
	tokens := map[string]any{"access_token": tokenText(tokenData, "access_token"), "refresh_token": tokenText(tokenData, "refresh_token"),
		"id_token": tokenText(tokenData, "id_token"), "expires_in": tokenData["expires_in"], "token_type": tokenType}
	discoveryDoc := make(map[string]any, len(discovery))
	for k, v := range discovery {
		discoveryDoc[k] = v
	}
	sctx, _, release, err := profileScope(ctx, oauthSessionProfile(sess))
	if err != nil {
		return err
	}
	defer release()
	oauthSessionsMu.Lock()
	defer oauthSessionsMu.Unlock()
	if pyrepr.Truthy(sess["cancelled"]) {
		sess["status"] = "cancelled"
		return nil
	}
	env := bootstrapPoolEnv(sctx)
	lastRefresh := strings.ReplaceAll(pydatetime.ISOFormat(time.Now().UTC()), "+00:00", "Z")
	if err := provider.SaveXAIOAuthDeviceTokens(env, tokens, discoveryDoc, lastRefresh); err != nil {
		return err
	}
	if err := env.Store.MarkProviderActiveIfUnset("xai-oauth"); err != nil {
		return err
	}
	_, err = env.Store.UnsuppressCredentialSource("xai-oauth", "device_code")
	return err
}

// Sleeps in short ticks, true as soon as the session is cancelled
func sleepUnlessCancelled(sess map[string]any, seconds float64) bool {
	deadline := time.Now().Add(time.Duration(seconds * float64(time.Second)))
	for {
		oauthSessionsMu.Lock()
		cancelled := pyrepr.Truthy(sess["cancelled"])
		oauthSessionsMu.Unlock()
		if cancelled {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		time.Sleep(min(250*time.Millisecond, left))
	}
}

var deviceLoginEndings = map[string]string{"denied": "Authorization was denied.", "expired": "Device code expired before approval"}

// Mapping a provider profile hook answered, empty for nothing and an error for any other shape
func deviceLoginDict(value any) (map[string]any, error) {
	switch v := value.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = item
		}
		return out, nil
	case *pyjson.Object:
		out := make(map[string]any, v.Len())
		for _, k := range v.Keys {
			out[k] = v.Vals[k]
		}
		return out, nil
	}
	if !pyrepr.Truthy(value) {
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("TypeError: cannot convert dictionary update sequence element #0 to a sequence")
}

// Drives the device code login a provider profile owns, polling per interval and saving in the session profile scope
func profileDevicePollerBody(ctx context.Context, _ string, sess map[string]any) error {
	profile, ok := sess["_provider_profile"].(provider.Profile)
	if !ok {
		return errors.New("KeyError: '_provider_profile'")
	}
	state := sess["_device_state"]
	intervalValue, _ := statusNumber(sess["interval"])
	interval := int64(intervalValue)
	expiresAt, _ := statusNumber(sess["expires_at"])
	result := map[string]any{}
	for float64(time.Now().UnixNano())/1e9 < expiresAt {
		if sleepUnlessCancelled(sess, float64(interval)) {
			break
		}
		sctx, _, err := configProfileScope(ctx, oauthSessionProfile(sess))
		if err != nil {
			return err
		}
		answer, err := profile.PollDeviceLogin(sctx, state)
		if err != nil {
			return err
		}
		if result, err = deviceLoginDict(answer); err != nil {
			return err
		}
		if next, present := result["state"]; present {
			state = next
		}
		if result["status"] != "pending" {
			break
		}
		if v := result["interval"]; pyrepr.Truthy(v) {
			n, err := oauthInt(result, "interval")
			if err != nil {
				return err
			}
			interval = n
		}
	}
	sctx, _, release, err := profileScope(ctx, oauthSessionProfile(sess))
	if err != nil {
		return err
	}
	defer release()
	oauthSessionsMu.Lock()
	defer oauthSessionsMu.Unlock()
	if pyrepr.Truthy(sess["cancelled"]) {
		sess["status"] = "cancelled"
		return nil
	}
	status := result["status"]
	if status == nil || status == "pending" {
		status = "expired"
	}
	if status != "approved" {
		text, _ := status.(string)
		ending, known := deviceLoginEndings[text]
		sess["status"] = "error"
		if known {
			sess["status"] = text
		}
		switch {
		case pyrepr.Truthy(result["message"]):
			sess["error_message"] = result["message"]
		case known:
			sess["error_message"] = ending
		default:
			sess["error_message"] = "Sign-in failed"
		}
		return nil
	}
	return profile.SaveDeviceLogin(sctx, result["credential"])
}

// Auth layer calls of the oauth routes, the hermes_cli.auth attributes hermes tests patch
var (
	oauthMinimaxPKCEPair        = provider.MinimaxPKCEPair
	oauthMinimaxRequestUserCode = provider.MinimaxRequestUserCode
	oauthMinimaxPollToken       = provider.MinimaxPollToken
	oauthGetAuthStatus          = provider.GetAuthStatus
	oauthGetQwenAuthStatus      = provider.GetQwenAuthStatus
	oauthClearProviderAuth      = func(env provider.PoolEnv, providerID string) (bool, error) {
		return env.Store.ClearProviderAuth(providerID)
	}
)

// Device code pollers of the start route, the module attributes hermes tests patch
var (
	nousSignInPoller    = oauthPoller("nous", nousSignInPollerBody)
	minimaxPoller       = oauthPoller("minimax", minimaxPollerBody)
	xaiDevicePoller     = oauthPoller("xai", xaiDevicePollerBody)
	profileDevicePoller = oauthPoller("provider-profile", profileDevicePollerBody)
)
