package dashboard

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	oauthSessionTTLSeconds = 15 * 60
	longPollCapSeconds     = 25.0
)

// HTTP error that keeps the failure class its worker recorded for the setup metric
type oauthSetupHTTPError struct {
	*HTTPException
	failureClass string
}

func (e *oauthSetupHTTPError) Unwrap() error { return e.HTTPException }

func (e *oauthSetupHTTPError) SetupFailureClass() string { return e.failureClass }

// Registers a new pending OAuth session and returns its id with the session
func newOAuthSession(providerID, flow, profile string) (string, map[string]any) {
	sid := tokenURLSafe(16)
	var profileName any
	if name := oauthProfileName(profile); name != "" {
		profileName = name
	}
	sess := map[string]any{"session_id": sid, "provider": providerID, "flow": flow, "profile": profileName,
		"created_at": float64(time.Now().UnixNano()) / 1e9, "status": "pending", "error_message": nil}
	oauthSessionsMu.Lock()
	oauthSessions[sid] = sess
	oauthSessionsMu.Unlock()
	return sid, sess
}

// Forgets a session that never got started
func dropOAuthSession(sid string) {
	oauthSessionsMu.Lock()
	delete(oauthSessions, sid)
	oauthSessionsMu.Unlock()
}

// Ends the setup flow of a session from its status, the flow taken out under the lock so it ends once
func settleOAuthSession(ctx context.Context, sess map[string]any) {
	if sess == nil {
		return
	}
	oauthSessionsMu.Lock()
	snapshot := maps.Clone(sess)
	delete(sess, observability.OAuthSetupFlowKey)
	oauthSessionsMu.Unlock()
	observability.SettleOAuthSetup(ctx, snapshot, false)
	if flow, pending := snapshot[observability.OAuthSetupFlowKey]; pending {
		oauthSessionsMu.Lock()
		if _, replaced := sess[observability.OAuthSetupFlowKey]; !replaced {
			sess[observability.OAuthSetupFlowKey] = flow
		}
		oauthSessionsMu.Unlock()
	}
}

// Runs a poller for a session in the background with the request context, settling the setup flow when it returns
func startPoller(ctx context.Context, target func(ctx context.Context, sessionID string), sid string) {
	oauthSessionsMu.Lock()
	sess := oauthSessions[sid]
	oauthSessionsMu.Unlock()
	background := context.WithoutCancel(ctx)
	go func() {
		defer settleOAuthSession(background, sess)
		target(background, sid)
	}()
}

// Hands the setup flow to its session and settles it when the poller already ended, abandoned for a vanished session
func trackOAuthSetup(ctx context.Context, flow *observability.SetupFlow, sessionID string) {
	oauthSessionsMu.Lock()
	sess := oauthSessions[sessionID]
	if sess != nil {
		observability.AttachOAuthSetup(sess, flow)
	}
	oauthSessionsMu.Unlock()
	if sess == nil {
		observability.FinishProviderSetup(ctx, flow, "abandoned", "none")
		return
	}
	settleOAuthSession(ctx, sess)
}

// Registers a device code session carrying fields, starts its poller and returns the start body
func deviceSessionStarted(ctx context.Context, providerID, profile string, poller func(ctx context.Context, sessionID string),
	fields map[string]any, userCode, verificationURL string, expiresIn, pollInterval int64) *pyjson.Object {
	sid, sess := newOAuthSession(providerID, "device_code", profile)
	oauthSessionsMu.Lock()
	maps.Copy(sess, fields)
	oauthSessionsMu.Unlock()
	startPoller(ctx, poller, sid)
	return pyjson.ObjectOf("session_id", sid, "flow", "device_code", "user_code", userCode,
		"verification_url", verificationURL, "expires_in", expiresIn, "poll_interval", pollInterval)
}

// Status card of a raw auth status with the last refresh added when the provider reports one
func statusCard(raw map[string]any, source, sourceLabel, tokenPreview, expiresAt any, hasRefreshToken bool, lastRefresh ...any) *pyjson.Object {
	card := pyjson.ObjectOf("logged_in", pyrepr.Truthy(raw["logged_in"]), "source", source, "source_label", sourceLabel,
		"token_preview", tokenPreview, "expires_at", expiresAt, "has_refresh_token", hasRefreshToken)
	if len(lastRefresh) > 0 {
		card.Set("last_refresh", lastRefresh[0])
	}
	return card
}

// First truthy value, the last one when none is
func oauthFirstTruthy(values ...any) any {
	for _, v := range values {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	return values[len(values)-1]
}

// Zoned ISO text of an epoch in milliseconds, nil for none
func epochMsToISO(value any) (any, error) {
	if !pyrepr.Truthy(value) {
		return nil, nil
	}
	ms, ok := statusNumber(value)
	if !ok {
		return nil, fmt.Errorf("TypeError: unsupported operand type(s) for /: '%s' and 'int'", pyrepr.TypeName(value))
	}
	at, err := pydatetime.FromTimestampIn(ms/1000, time.UTC, pydatetime.LocaltimeAccepts)
	if err != nil {
		return nil, err
	}
	return pydatetime.ISOFormat(at), nil
}

// Status object of whatever a status reader answered, a mapping kept and anything else an error
func statusObject(value any) (*pyjson.Object, error) {
	switch v := value.(type) {
	case *pyjson.Object:
		return v, nil
	case map[string]any:
		keys := slices.Sorted(maps.Keys(v))
		out := pyjson.NewObject()
		for _, k := range keys {
			out.Set(k, v[k])
		}
		return out, nil
	}
	return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(value))
}

// Status of one account card through its own reader, its hand written card or the slug driven dispatcher
func resolveProviderStatus(ctx context.Context, providerID string, statusFn func(ctx context.Context) (any, error)) (status *pyjson.Object) {
	failed := func(text string) *pyjson.Object { return pyjson.ObjectOf("logged_in", false, "error", text) }
	defer func() {
		if r := recover(); r != nil {
			if err, isErr := r.(error); isErr {
				status = failed(err.Error())
				return
			}
			status = failed(pyrepr.Str(r))
		}
	}()
	if statusFn != nil {
		answer, err := statusFn(ctx)
		if err != nil {
			return failed(err.Error())
		}
		card, err := statusObject(answer)
		if err != nil {
			return failed(err.Error())
		}
		return card
	}
	env := bootstrapPoolEnv(ctx)
	switch providerID {
	case "nous":
		r := provider.GetNousAuthStatusLocal(env)
		card := statusCard(r, "nous_portal", oauthFirstTruthy(r["portal_base_url"], "Nous Portal"), truncateToken(r["access_token"], 6),
			r["access_expires_at"], pyrepr.Truthy(r["has_refresh_token"]))
		card.Set("free_tier", pyrepr.Truthy(r["free_tier"]))
		card.Set("account_tier", r["account_tier"])
		return card
	case "openai-codex":
		r := provider.GetCodexAuthStatus(env)
		return statusCard(r, oauthFirstTruthy(r["source"], "openai_codex"), oauthFirstTruthy(r["auth_mode"], "OpenAI Codex"),
			truncateToken(r["api_key"], 6), nil, false, r["last_refresh"])
	case "qwen-oauth":
		r := oauthGetQwenAuthStatus(env)
		expires, err := epochMsToISO(r["expires_at_ms"])
		if err != nil {
			return failed(err.Error())
		}
		return statusCard(r, "qwen_cli", oauthFirstTruthy(r["auth_file"], "Qwen CLI"), truncateToken(r["api_key"], 6), expires,
			pyrepr.Truthy(r["has_refresh_token"]))
	case "minimax-oauth":
		r := provider.GetMinimaxOAuthAuthStatus(env)
		region, present := r["region"]
		if !present {
			region = "global"
		}
		return statusCard(r, "minimax_oauth", fmt.Sprintf("MiniMax (%s)", pyrepr.Str(region)), nil, r["expires_at"], true)
	case "xai-oauth":
		r := provider.GetXAIOAuthAuthStatus(env)
		return statusCard(r, oauthFirstTruthy(r["source"], "xai_oauth"), oauthFirstTruthy(r["auth_store"], r["source"], "xAI Grok OAuth"),
			truncateToken(r["api_key"], 6), nil, true, r["last_refresh"])
	}
	raw := oauthGetAuthStatus(env, providerID)
	if _, present := raw["logged_in"]; present {
		return statusCard(raw, oauthFirstTruthy(raw["source"], raw["provider"], providerID),
			oauthFirstTruthy(raw["source_label"], raw["auth_store"], raw["auth_store_path"], raw["base_url"], raw["name"], ""),
			truncateToken(oauthFirstTruthy(raw["access_token"], raw["api_key"]), 6),
			oauthFirstTruthy(raw["expires_at"], raw["access_expires_at"]), pyrepr.Truthy(raw["has_refresh_token"]))
	}
	return pyjson.ObjectOf("logged_in", false)
}

// Starts a Nous sign in, its first state published as the device code and the rest drained by the poller
func startNousDeviceCode(ctx context.Context, profile string) (*pyjson.Object, error) {
	pconfig := provider.AuthRegistryGet(ctx, "nous")
	if pconfig == nil {
		return nil, errors.New("KeyError: 'nous'")
	}
	portalBaseURL := os.Getenv("ICEBATH_PORTAL_BASE_URL")
	if portalBaseURL == "" {
		portalBaseURL = os.Getenv("NOUS_PORTAL_BASE_URL")
	}
	if portalBaseURL == "" {
		portalBaseURL = pconfig.PortalBaseURL
	}
	portalBaseURL = strings.TrimRight(portalBaseURL, "/")
	sid, sess := newOAuthSession("nous", "device_code", profile)
	sctx, _, err := configProfileScope(ctx, oauthProfileName(profile))
	if err != nil {
		dropOAuthSession(sid)
		return nil, err
	}
	seq := provider.RunSignIn(bootstrapPoolEnv(context.WithoutCancel(sctx)), provider.SignInOptions{
		Timeout: 15 * time.Second,
		Cancelled: func() bool {
			oauthSessionsMu.Lock()
			defer oauthSessionsMu.Unlock()
			return pyrepr.Truthy(sess["cancelled"])
		},
		ReplaceAccount: true,
		PersistGuard: func(body func(mayPersist bool) error) error {
			oauthSessionsMu.Lock()
			defer oauthSessionsMu.Unlock()
			if pyrepr.Truthy(sess["cancelled"]) {
				sess["status"] = "cancelled"
				return body(false)
			}
			return body(true)
		},
		Scope: func(body func() error) error { return body() },
	})
	next, stop := iter.Pull(seq)
	first, ok := next()
	if !ok {
		stop()
		dropOAuthSession(sid)
		return nil, errors.New("StopIteration")
	}
	code, isCode := first.(provider.SignInCode)
	if !isCode {
		stop()
		dropOAuthSession(sid)
		return nil, NewHTTPException(400, first.CopyTerminal())
	}
	oauthSessionsMu.Lock()
	sess["portal_base_url"], sess["client_id"], sess["scope"] = portalBaseURL, pconfig.ClientID, pconfig.Scope
	sess["device_code"], sess["interval"] = "", int64(code.Interval)
	sess["expires_at"] = float64(time.Now().UnixNano())/1e9 + float64(code.ExpiresIn)
	sess["_sign_in"] = &signInPull{next: next, stop: stop}
	oauthSessionsMu.Unlock()
	startPoller(ctx, nousSignInPoller, sid)
	return pyjson.ObjectOf("session_id", sid, "flow", "device_code", "user_code", code.Code, "verification_url", code.Link,
		"expires_in", int64(code.ExpiresIn), "poll_interval", int64(code.Interval)), nil
}

// Starts the Codex login worker and waits briefly for its user code, 500 when it failed and 504 when it stayed silent
func startCodexDeviceCode(ctx context.Context, profile string) (*pyjson.Object, error) {
	sid, _ := newOAuthSession("openai-codex", "device_code", profile)
	startPoller(ctx, codexFullLoginWorker, sid)
	snapshot := func() map[string]any {
		oauthSessionsMu.Lock()
		defer oauthSessionsMu.Unlock()
		return maps.Clone(oauthSessions[sid])
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := snapshot(); len(s) > 0 && (pyrepr.Truthy(s["user_code"]) || s["status"] != "pending") {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	s := snapshot()
	if s["status"] == "error" {
		failure, _ := oauthFirstTruthy(s[observability.OAuthFailureKey], "other").(string)
		return nil, &oauthSetupHTTPError{HTTPException: NewHTTPException(500, oauthFirstTruthy(s["error_message"], "device-auth failed")), failureClass: failure}
	}
	if !pyrepr.Truthy(s["user_code"]) {
		return nil, NewHTTPException(504, "device-auth timed out before returning a user code")
	}
	expiresIn, _ := statusNumber(oauthFirstTruthy(s["expires_in"], int64(900)))
	interval, _ := statusNumber(oauthFirstTruthy(s["interval"], int64(5)))
	return pyjson.ObjectOf("session_id", sid, "flow", "device_code", "user_code", s["user_code"], "verification_url", s["verification_url"],
		"expires_in", int64(expiresIn), "poll_interval", int64(interval)), nil
}

// Starts the MiniMax login, its verifier and state kept on the session for the token poll
func startMinimaxDeviceCode(ctx context.Context, profile string) (*pyjson.Object, error) {
	verifier, challenge, state := oauthMinimaxPKCEPair()
	portalBaseURL := os.Getenv("MINIMAX_PORTAL_BASE_URL")
	if portalBaseURL == "" {
		portalBaseURL = provider.MinimaxOAuthGlobalBase
	}
	portalBaseURL = strings.TrimRight(portalBaseURL, "/")
	deviceData, err := oauthMinimaxRequestUserCode(ctx, provider.MinimaxHTTPClient(15*time.Second), portalBaseURL,
		provider.MinimaxOAuthClientID, challenge, state)
	if err != nil {
		return nil, err
	}
	var intervalMS any
	intervalValue := int64(0)
	if deviceData["interval"] != nil {
		if intervalValue, err = oauthInt(deviceData, "interval"); err != nil {
			return nil, err
		}
		intervalMS = intervalValue
	}
	expiredInRaw, err := oauthInt(deviceData, "expired_in")
	if err != nil {
		return nil, err
	}
	now := float64(time.Now().UnixNano()) / 1e9
	expiresAt, expiresIn := now+float64(expiredInRaw), expiredInRaw
	if expiredInRaw > 1_000_000_000_000 {
		expiresAt = float64(expiredInRaw) / 1000.0
		expiresIn = max(int64(0), int64(expiresAt-now))
	}
	for _, key := range []string{"user_code", "verification_uri"} {
		if _, present := deviceData[key]; !present {
			return nil, fmt.Errorf("KeyError: %s", pyrepr.StrRepr(key))
		}
	}
	userCode := pyrepr.Str(deviceData["user_code"])
	pollMS := intervalValue
	if pollMS == 0 {
		pollMS = 2000
	}
	return deviceSessionStarted(ctx, "minimax-oauth", profile, minimaxPoller, map[string]any{
		"interval_ms": intervalMS, "user_code": userCode, "code_verifier": verifier, "state": state,
		"portal_base_url": portalBaseURL, "client_id": provider.MinimaxOAuthClientID, "region": "global",
		"expired_in_raw": expiredInRaw, "expires_at": expiresAt,
	}, userCode, pyrepr.Str(deviceData["verification_uri"]), expiresIn, max(int64(2), pollMS/1000)), nil
}

// Starts the xAI device code login
func startXAIDeviceCode(ctx context.Context, profile string) (*pyjson.Object, error) {
	deviceData, err := provider.XAIOAuthRequestDeviceCode(ctx, provider.XAIHTTPClient(20*time.Second))
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"device_code", "user_code"} {
		if _, present := deviceData[key]; !present {
			return nil, fmt.Errorf("KeyError: %s", pyrepr.StrRepr(key))
		}
	}
	interval, err := oauthInt(deviceData, "interval")
	if err != nil {
		return nil, err
	}
	expiresIn, err := oauthInt(deviceData, "expires_in")
	if err != nil {
		return nil, err
	}
	verification := deviceData["verification_uri_complete"]
	if !pyrepr.Truthy(verification) {
		var present bool
		if verification, present = deviceData["verification_uri"]; !present {
			return nil, errors.New("KeyError: 'verification_uri'")
		}
	}
	return deviceSessionStarted(ctx, "xai-oauth", profile, xaiDevicePoller, map[string]any{
		"device_code": pyrepr.Str(deviceData["device_code"]), "interval": interval,
		"expires_at": float64(time.Now().UnixNano())/1e9 + float64(expiresIn),
	}, pyrepr.Str(deviceData["user_code"]), pyrepr.Str(verification), expiresIn, interval), nil
}

// Provider profile that owns an in app device code login, false when it lacks any of the three hooks
func deviceLoginProfile(ctx context.Context, providerID string) (provider.Profile, bool) {
	prof, ok := provider.GetProviderProfile(ctx, providerID)
	if !ok || prof.StartDeviceLogin == nil || prof.PollDeviceLogin == nil || prof.SaveDeviceLogin == nil {
		return provider.Profile{}, false
	}
	return prof, true
}

// Starts the device code login a provider profile owns inside the request profile scope
func startProfileDeviceLogin(ctx context.Context, providerID, profile string) (*pyjson.Object, error) {
	type begun struct {
		prof  provider.Profile
		login map[string]any
	}
	started, err := scopedToThread(ctx, profile, func(sctx context.Context) (*begun, error) {
		prof, ok := deviceLoginProfile(sctx, providerID)
		if !ok {
			return nil, nil
		}
		answer, err := prof.StartDeviceLogin(sctx)
		if err != nil {
			return nil, err
		}
		login, err := deviceLoginDict(answer)
		return &begun{prof, login}, err
	})
	if err != nil {
		return nil, err
	}
	if started == nil || len(started.login) == 0 {
		return nil, NewHTTPException(400, fmt.Sprintf("Provider %s does not support device-code flow", providerID))
	}
	login := started.login
	expiresIn, err := oauthInt(login, "expires_in")
	if err != nil {
		return nil, err
	}
	interval, err := oauthInt(login, "interval")
	if err != nil {
		return nil, err
	}
	interval = max(int64(1), interval)
	for _, key := range []string{"user_code", "verification_url"} {
		if _, present := login[key]; !present {
			return nil, fmt.Errorf("KeyError: %s", pyrepr.StrRepr(key))
		}
	}
	return deviceSessionStarted(ctx, providerID, profile, profileDevicePoller, map[string]any{
		"_provider_profile": started.prof, "_device_state": login["state"], "interval": interval,
		"expires_at": float64(time.Now().UnixNano())/1e9 + float64(expiresIn),
	}, pyrepr.Str(login["user_code"]), pyrepr.Str(login["verification_url"]), expiresIn, interval), nil
}

// Device code starters of the built in providers, the module attributes hermes tests patch
var deviceCodeStarters = map[string]func(ctx context.Context, profile string) (*pyjson.Object, error){
	"nous": startNousDeviceCode, "openai-codex": startCodexDeviceCode,
	"minimax-oauth": startMinimaxDeviceCode, "xai-oauth": startXAIDeviceCode,
}

// Asks the provider for a device code, starts its poller and returns the fields the UI shows
func startDeviceCodeFlow(ctx context.Context, providerID, profile string) (*pyjson.Object, error) {
	if starter, ok := deviceCodeStarters[providerID]; ok {
		return starter(ctx, profile)
	}
	return startProfileDeviceLogin(ctx, providerID, profile)
}

// Host native command that removes the credential file Claude Code left behind
func claudeCodeDisconnectCommand(platform string) string {
	if platform == "win32" {
		literal := `"$HOME/.claude/.credentials.json"`
		return "if (Test-Path -LiteralPath " + literal + ") { Remove-Item -LiteralPath " + literal + " -Force -ErrorAction Stop }"
	}
	rmFile := "rm -f ~/.claude/.credentials.json"
	if platform == "darwin" {
		return `security delete-generic-password -s "Claude Code-credentials" 2>/dev/null; ` + rmFile
	}
	return rmFile
}

// Python sys.platform name of this host
func sysPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	}
	return runtime.GOOS
}

// Shell command that clears the credentials of an external provider, nil when the provider has none
func oauthProviderDisconnectCommand(entry oauthCatalogRow, platform string) any {
	if entry.flow != "external" || entry.id != "claude-code" {
		return nil
	}
	if platform == "" {
		platform = sysPlatform()
	}
	return claudeCodeDisconnectCommand(platform)
}

// Manual disconnect path of a provider the API cannot clear, nil when the API can
func oauthProviderDisconnectHint(entry oauthCatalogRow, status *pyjson.Object) any {
	if entry.flow == "external" && entry.id != "anthropic" {
		if oauthProviderDisconnectCommand(entry, "") != nil {
			return "Managed outside Icebath — run the disconnect command to remove it."
		}
		return "Managed by that provider's CLI; remove it there."
	}
	if status != nil && status.GetOr("source", nil) == "env_var" {
		return "Remove the API key from Settings → Keys instead."
	}
	return nil
}

// One row of the accounts catalog a request sees
type oauthCatalogRow struct {
	id, name, flow, cliCommand, docsURL string
	statusFn                            func(ctx context.Context) (any, error)
}

// Accounts tab card of one catalog provider overlaid with what its profile declares, false for a keys tab provider with nothing declared
func profileAccountRow(ctx context.Context, d provider.ProviderDescriptor) (oauthCatalogRow, bool) {
	prof, _ := provider.GetProviderProfile(ctx, d.Slug)
	card := prof.AccountCard
	_, device := deviceLoginProfile(ctx, d.Slug)
	if d.Tab != "accounts" && len(card) == 0 && !device {
		return oauthCatalogRow{}, false
	}
	pick := func(key, fallback string) string {
		if v := card[key]; v != "" {
			return v
		}
		return fallback
	}
	flow := pick("flow", "external")
	if device {
		flow = "device_code"
	}
	return oauthCatalogRow{id: d.Slug, name: pick("name", d.Label), flow: flow, cliCommand: pick("cli_command", "icebath auth add "+d.Slug),
		docsURL: pick("docs_url", d.SignupURL), statusFn: prof.AccountStatus}, true
}

// Accounts tab provider list, the curated cards first and then every other accounts provider of the catalog
func buildOAuthCatalog(ctx context.Context) []oauthCatalogRow {
	rows := []oauthCatalogRow{}
	seen := map[string]bool{}
	for _, entry := range oauthProviderCatalog {
		if seen[entry.id] {
			continue
		}
		seen[entry.id] = true
		row := oauthCatalogRow{id: entry.id, name: entry.name, flow: entry.flow, cliCommand: entry.cliCommand, docsURL: entry.docsURL}
		if fn := entry.statusFn; fn != nil {
			row.statusFn = func(ctx context.Context) (any, error) { return fn(ctx) }
		}
		rows = append(rows, row)
	}
	for _, d := range provider.ProviderCatalog() {
		if seen[d.Slug] {
			continue
		}
		if row, ok := profileAccountRow(ctx, d); ok {
			seen[d.Slug] = true
			rows = append(rows, row)
		}
	}
	return rows
}

// Every OAuth capable provider of a profile with its current status and disconnect path
func listOAuthProviders(ctx context.Context, profile string) (*pyjson.Object, error) {
	return scopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		providers := []any{}
		for _, p := range buildOAuthCatalog(sctx) {
			status := resolveProviderStatus(sctx, p.id, p.statusFn)
			hint := oauthProviderDisconnectHint(p, status)
			providers = append(providers, pyjson.ObjectOf("id", p.id, "name", p.name, "flow", p.flow,
				"cli_command", externalProcessCLICommand(sctx, p.id, p.cliCommand), "docs_url", p.docsURL, "disconnect_hint", hint,
				"disconnect_command", oauthProviderDisconnectCommand(p, ""), "disconnectable", hint == nil, "status", status))
		}
		return pyjson.ObjectOf("providers", providers), nil
	})
}

// 400 naming the manual path when a provider cannot be disconnected by the API
func rejectIfNotDisconnectable(entry oauthCatalogRow, status *pyjson.Object) error {
	if hint, ok := oauthProviderDisconnectHint(entry, status).(string); ok && hint != "" {
		return NewHTTPException(400, fmt.Sprintf("%s cannot be disconnected automatically. %s", entry.name, hint))
	}
	return nil
}

// Clears the icebath managed Anthropic PKCE file and auth store entry, never the Claude Code files
func clearAnthropicAuth(ctx context.Context) (bool, error) {
	env := bootstrapPoolEnv(ctx)
	cleared := false
	oauthFile := provider.HermesOAuthFile(env)
	if _, err := os.Stat(oauthFile); err == nil {
		if err := pypath.Unlink(oauthFile); err != nil {
			routersLog().Error("disconnect anthropic OAuth file failed", "exc_info", err)
			return false, err
		}
		cleared = true
	}
	storeCleared, err := oauthClearProviderAuth(env, "anthropic")
	if err != nil {
		routersLog().Error("disconnect anthropic auth store failed", "exc_info", err)
		return false, err
	}
	return storeCleared || cleared, nil
}

// Disconnect failure answer, 409 when nothing was stored and 500 when the removal failed
func disconnectHTTPError(status int, providerName string) *HTTPException {
	if status == 409 {
		return NewHTTPException(status, fmt.Sprintf("No stored credentials were removed for %s.", providerName))
	}
	return NewHTTPException(status, fmt.Sprintf("Failed to remove stored credentials for %s.", providerName))
}

// Removes the stored credentials of an OAuth provider of a profile
func disconnectOAuthProvider(ctx context.Context, req *Request, providerID, profile string) (*pyjson.Object, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	return scopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		catalog := buildOAuthCatalog(sctx)
		var entry *oauthCatalogRow
		ids := make([]string, 0, len(catalog))
		for i := range catalog {
			ids = append(ids, catalog[i].id)
			if catalog[i].id == providerID {
				entry = &catalog[i]
			}
		}
		if entry == nil {
			slices.Sort(ids)
			return nil, NewHTTPException(400, fmt.Sprintf("Unknown provider: %s. Available: %s", providerID, strings.Join(ids, ", ")))
		}
		if err := rejectIfNotDisconnectable(*entry, pyjson.NewObject()); err != nil {
			return nil, err
		}
		if err := rejectIfNotDisconnectable(*entry, resolveProviderStatus(sctx, providerID, entry.statusFn)); err != nil {
			return nil, err
		}
		var cleared bool
		var err error
		if providerID == "anthropic" {
			cleared, err = clearAnthropicAuth(sctx)
		} else {
			cleared, err = oauthClearProviderAuth(bootstrapPoolEnv(sctx), providerID)
			if err == nil && providerID == "nous" {
				provider.InvalidateNousAuthStatusCache()
			}
		}
		if err != nil {
			if h, isHTTP := asHTTPException(err); isHTTP {
				return nil, h
			}
			routersLog().Error(fmt.Sprintf("disconnect %s failed", providerID), "exc_info", err)
			return nil, disconnectHTTPError(500, entry.name)
		}
		if !cleared {
			return nil, disconnectHTTPError(409, entry.name)
		}
		if providerID == "anthropic" {
			routersLog().Info("oauth/disconnect: " + providerID)
		} else {
			routersLog().Info(fmt.Sprintf("oauth/disconnect: %s (cleared=True)", providerID))
		}
		return pyjson.ObjectOf("ok", true, "provider", providerID), nil
	})
}

// Drops every OAuth session older than the session lifetime
func gcOAuthSessions() {
	cutoff := float64(time.Now().UnixNano())/1e9 - oauthSessionTTLSeconds
	oauthSessionsMu.Lock()
	defer oauthSessionsMu.Unlock()
	for sid, sess := range oauthSessions {
		if created, _ := statusNumber(sess["created_at"]); created < cutoff {
			delete(oauthSessions, sid)
		}
	}
}

// Name of the requested profile after it proved to exist, empty for none
func validateOAuthProfile(profile string) (string, error) {
	name := oauthProfileName(profile)
	if name != "" {
		if _, err := resolveProfileDir(name); err != nil {
			return "", err
		}
	}
	return name, nil
}

// Starts the provider setup metric of a login, nil when the owning profile does not collect or the start fails
func beginOAuthSetupMetric(ctx context.Context, providerID, profile string) (flow *observability.SetupFlow) {
	defer func() {
		if recover() != nil {
			flow = nil
		}
	}()
	homeDir := ""
	if name := oauthProfileName(profile); name != "" {
		dir, err := resolveProfileDir(name)
		if err != nil {
			return nil
		}
		homeDir = dir
	}
	if !observability.CollectionEnabled(ctx, homeDir) {
		return nil
	}
	return observability.BeginOAuthSetup(ctx, providerID, homeDir)
}

// Ends the setup metric of a failed start with the class its error maps to
func endOAuthSetupMetric(ctx context.Context, flow *observability.SetupFlow, err error) {
	if flow == nil {
		return
	}
	defer func() { _ = recover() }()
	failure := ""
	if h, isHTTP := asHTTPException(err); isHTTP {
		failure = map[int]string{401: "auth", 403: "auth", 504: "network"}[h.StatusCode]
	}
	if failure == "" {
		failure = observability.SetupFailureClass(err)
	}
	observability.FinishProviderSetup(ctx, flow, "failed", failure)
}

// Starts the device code login of an accounts provider for a profile
func startOAuthLogin(ctx context.Context, req *Request, providerID, profile string) (*pyjson.Object, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	gcOAuthSessions()
	if _, err := validateOAuthProfile(profile); err != nil {
		return nil, err
	}
	catalog, err := scopedToThread(ctx, profile, func(sctx context.Context) ([]oauthCatalogRow, error) {
		return buildOAuthCatalog(sctx), nil
	})
	if err != nil {
		return nil, err
	}
	at := slices.IndexFunc(catalog, func(p oauthCatalogRow) bool { return p.id == providerID })
	if at < 0 {
		return nil, NewHTTPException(400, "Unknown provider "+providerID)
	}
	entry := catalog[at]
	if entry.flow == "external" {
		return nil, NewHTTPException(400, fmt.Sprintf("%s uses an external CLI; run `%s` manually", providerID, entry.cliCommand))
	}
	if entry.flow != "device_code" {
		return nil, NewHTTPException(400, "Unsupported flow")
	}
	flow := beginOAuthSetupMetric(ctx, providerID, profile)
	body, err := startDeviceCodeFlow(ctx, providerID, profile)
	if err != nil {
		endOAuthSetupMetric(ctx, flow, err)
		if _, isHTTP := asHTTPException(err); isHTTP {
			return nil, err
		}
		routersLog().Error(fmt.Sprintf("oauth/start %s failed", providerID), "exc_info", err)
		return nil, NewHTTPException(500, err.Error())
	}
	if flow != nil {
		sid, _ := body.GetOr("session_id", "").(string)
		trackOAuthSetup(ctx, flow, sid)
	}
	return body, nil
}

// Reads until changed holds or the capped wait runs out, the last read the answer and a wait of zero one plain read
func longPoll[T any](ctx context.Context, read func() (T, error), changed func(T) bool, wait, tick float64) (T, error) {
	deadline := time.Now().Add(time.Duration(min(max(wait, 0.0), longPollCapSeconds) * float64(time.Second)))
	value, err := read()
	for err == nil && !changed(value) {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return value, ctx.Err()
		case <-time.After(min(time.Duration(tick*float64(time.Second)), left)):
		}
		value, err = read()
	}
	return value, err
}

// Status of a device code session, answered once it leaves pending when the caller waits
func pollOAuthSession(ctx context.Context, providerID, sessionID, profile string, wait float64) (*pyjson.Object, error) {
	requested, err := validateOAuthProfile(profile)
	if err != nil {
		return nil, err
	}
	oauthSessionsMu.Lock()
	sess := oauthSessions[sessionID]
	var sessProvider, sessProfile any
	if len(sess) > 0 {
		sessProvider, sessProfile = sess["provider"], sess["profile"]
	}
	oauthSessionsMu.Unlock()
	if len(sess) == 0 {
		return nil, NewHTTPException(404, "Session not found or expired")
	}
	if sessProvider != providerID {
		return nil, NewHTTPException(400, "Provider mismatch for session")
	}
	if name, _ := sessProfile.(string); name != requested {
		return nil, NewHTTPException(400, "OAuth session profile mismatch")
	}
	if _, err := longPoll(ctx, func() (any, error) {
		oauthSessionsMu.Lock()
		defer oauthSessionsMu.Unlock()
		if pyrepr.Truthy(sess["cancelled"]) {
			return "cancelled", nil
		}
		return sess["status"], nil
	}, func(status any) bool { return status != "pending" }, wait, 0.25); err != nil {
		return nil, err
	}
	oauthSessionsMu.Lock()
	defer oauthSessionsMu.Unlock()
	return pyjson.ObjectOf("session_id", sessionID, "status", sess["status"],
		"error_message", sess["error_message"], "expires_at", sess["expires_at"],
		"reason", sess["reason"], "account_email", sess["account_email"], "model", sess["model"],
		"handoff", sess["handoff"], "handoff_status", sess["handoff_status"],
		"retryable", sess["retryable"], "retry_after", sess["retry_after"]), nil
}

// Latest free tier handoff of a profile, answered once it differs from the stamp the caller already shows
func nousHandoffStatus(ctx context.Context, req *Request, profile string, since *string, wait float64) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	name, err := validateOAuthProfile(profile)
	if err != nil {
		return nil, err
	}
	return longPoll(ctx, func() (map[string]any, error) {
		return configScopedToThread(ctx, name, func(sctx context.Context) (map[string]any, error) {
			status, err := provider.GuestHandoffStatus(bootstrapPoolEnv(sctx))
			if err != nil {
				return nil, err
			}
			return provider.GuestHandoffWireStatus(status), nil
		})
	}, func(status map[string]any) bool { return since == nil || status["updated_at"] != *since }, wait, 1.0)
}

// Cancels a pending OAuth session so its worker stops polling, exchanging and saving
func cancelOAuthSession(ctx context.Context, req *Request, sessionID, profile string) (*pyjson.Object, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	requested, err := validateOAuthProfile(profile)
	if err != nil {
		return nil, err
	}
	oauthSessionsMu.Lock()
	sess := oauthSessions[sessionID]
	if sess != nil {
		if name, _ := sess["profile"].(string); name != requested {
			oauthSessionsMu.Unlock()
			return nil, NewHTTPException(400, "OAuth session profile mismatch")
		}
		sess["cancelled"] = true
		delete(oauthSessions, sessionID)
	}
	oauthSessionsMu.Unlock()
	if sess == nil {
		return pyjson.ObjectOf("ok", false, "message", "session not found"), nil
	}
	settleOAuthSession(ctx, sess)
	return pyjson.ObjectOf("ok", true, "session_id", sessionID), nil
}

// Router of the accounts list, disconnect and the device code login sessions
func oauthRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	providerPath := PathParam("provider_id", Str)
	r.Get("/api/providers/oauth", &Endpoint{
		Name: "list_oauth_providers",
		Doc: "Every OAuth-capable provider with current status (token_preview is the last\n" +
			"    N chars, never the full token; disconnect_command only for external providers).",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listOAuthProviders(ctx, profileQuery(c)) },
	})
	r.Delete("/api/providers/oauth/{provider_id}", &Endpoint{
		Name:   "disconnect_oauth_provider",
		Doc:    "Disconnect an OAuth provider. Token-protected (matches /env/reveal).",
		Params: []Param{providerPath, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return disconnectOAuthProvider(ctx, c.Request, c.Str("provider_id"), profileQuery(c))
		},
	})
	r.Post("/api/providers/oauth/{provider_id}/start", &Endpoint{
		Name:   "start_oauth_login",
		Doc:    "Initiate an OAuth login flow. Token-protected.",
		Params: []Param{providerPath, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return startOAuthLogin(ctx, c.Request, c.Str("provider_id"), profileQuery(c))
		},
	})
	r.Post("/api/providers/oauth/{provider_id}/submit", &Endpoint{
		Name:   "submit_oauth_code",
		Doc:    "Submit the auth code for PKCE flows. Token-protected.",
		Params: []Param{providerPath, optProfile},
		Body:   []BodyParam{Body("body", oauthSubmitBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if err := requireToken(c.Request); err != nil {
				return nil, err
			}
			return nil, NewHTTPException(400, "submit not supported for "+c.Str("provider_id"))
		},
	})
	r.Get("/api/providers/oauth/{provider_id}/poll/{session_id}", &Endpoint{
		Name: "poll_oauth_session",
		Doc: "Poll a session's status (no auth — read-only state). One endpoint serves\n" +
			"    every device-code flow: all report progress via the worker-updated ``status``.\n" +
			"    With *wait* (seconds) it is a long-poll: it answers once the session leaves ``pending``.",
		Params: []Param{providerPath, PathParam("session_id", Str), optProfile, QueryParam("wait", Float, 0.0)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return pollOAuthSession(ctx, c.Str("provider_id"), c.Str("session_id"), profileQuery(c), c.Float("wait"))
		},
	})
	r.Get("/api/providers/oauth/nous/handoff", &Endpoint{
		Name: "nous_handoff_status",
		Doc: "The profile's latest free-tier handoff (``guest_handoff.wire_status``). Token-protected. A\n" +
			"    long-poll: given *since* (the ``updated_at`` the caller already shows) and *wait*, it answers once\n" +
			"    the record changes. The keepalive retries a move in the background; this only reads.",
		Params: []Param{optProfile, QueryParam("since", Optional(Str), nil), QueryParam("wait", Float, 0.0)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return nousHandoffStatus(ctx, c.Request, profileQuery(c), optionalStr(c, "since"), c.Float("wait"))
		},
	})
	r.Delete("/api/providers/oauth/sessions/{session_id}", &Endpoint{
		Name: "cancel_oauth_session",
		Doc: "Cancel a pending OAuth session. Token-protected.\n\n" +
			"    Marks the session dict ``cancelled`` before popping it so a background\n" +
			"    worker still holding that dict (e.g. the Codex poller) stops\n" +
			"    polling/exchanging/saving instead of completing the login after the user\n" +
			"    believed it was aborted.",
		Params: []Param{PathParam("session_id", Str), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return cancelOAuthSession(ctx, c.Request, c.Str("session_id"), profileQuery(c))
		},
	})
	return r
}
