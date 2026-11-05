package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// NousLoginExitError carries the process exit code a failed or cancelled Nous login asks for
type NousLoginExitError struct {
	Code int
	Err  error
}

func (e *NousLoginExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("nous login exited with status %d", e.Code)
}

func (e *NousLoginExitError) Unwrap() error { return e.Err }

// NousDeviceLoginArgs tune one device code flow, empty strings taking the env and registry defaults
type NousDeviceLoginArgs struct {
	PortalBaseURL    string
	InferenceBaseURL string
	ClientID         string
	Scope            string
	OpenBrowser      bool
	Timeout          time.Duration
	Insecure         bool
	CABundle         string
	OnVerification   func(verificationURL, userCode string)
}

// Verification for the login flow, disabled when insecure else the custom bundle else the platform default
func nousLoginVerify(insecure bool, caBundle string) oauthVerify {
	if insecure {
		return oauthVerify{Insecure: true, TLS: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	}
	if caBundle != "" {
		return oauthVerify{CABundle: caBundle, TLS: sharedTLSConfig(caBundle, false)}
	}
	return defaultOAuthVerify()
}

// Device code flow the login entry points run, swappable so tests can stand in for the Portal
var nousDeviceCodeLoginFn = nousDeviceCodeLogin

// Catalog reads of the post login picker, swappable so tests can script the Portal
var (
	nousLoginCuratedIDsFn    = GetCuratedNousModelIDs
	nousLoginPricingFn       = func(ctx context.Context) PricingTable { return GetPricingForProvider(ctx, "nous", "", false, false) }
	nousLoginFreeTierFn      = func(ctx context.Context) bool { return CheckNousFreeTier(ctx, true, false) }
	nousLoginPolicyAllowedFn = func(ctx context.Context) (map[string]bool, bool) { return NousPolicyAllowedIDs(ctx, false) }
)

// Sibling slice steps the login runs, swappable so tests can script or record them
var (
	nousPromptModelSelectionFn = promptModelSelection
	nousSaveModelChoiceFn      = saveModelChoice
	nousGuestHandoffDrainFn    = guestHandoffDrain
	nousSettleAfterUpgradeFn   = settleAfterUpgrade
	nousBillingInvalidateFn    = nousBillingInvalidateCachedToken
)

// Runs the Nous device code flow and returns the full OAuth state without persisting it
func nousDeviceCodeLogin(env PoolEnv, out io.Writer, args NousDeviceLoginArgs) (map[string]any, error) {
	portalBaseURL := strings.TrimRight(firstNonEmpty(args.PortalBaseURL, oauthGetenv("ICEBATH_PORTAL_BASE_URL"), oauthGetenv("NOUS_PORTAL_BASE_URL"), DefaultNousPortalURL), "/")
	requestedInferenceURL := strings.TrimRight(firstNonEmpty(args.InferenceBaseURL, oauthGetenv("NOUS_INFERENCE_BASE_URL"), DefaultNousInferenceURL), "/")
	clientID := firstNonEmpty(args.ClientID, DefaultNousClientID)
	scope := firstNonEmpty(args.Scope, DefaultNousScope)
	timeout := args.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	verify := nousLoginVerify(args.Insecure, args.CABundle)
	openBrowser := args.OpenBrowser
	if isRemoteSession() {
		openBrowser = false
	}
	fmt.Fprintln(out, "Starting Icebath login via Nous Portal...")
	fmt.Fprintf(out, "Portal: %s\n", portalBaseURL)
	if args.Insecure {
		fmt.Fprintln(out, "TLS verification: disabled (--insecure)")
	} else if args.CABundle != "" {
		fmt.Fprintf(out, "TLS verification: custom CA bundle (%s)\n", args.CABundle)
	}
	ctx := env.ctx()
	client := nousHTTPClientFn(env, timeout, verify)
	deviceData, err := requestDeviceCode(ctx, client, portalBaseURL, clientID, scope)
	if err != nil {
		return nil, err
	}
	verificationURL := stringifyAny(deviceData["verification_uri_complete"])
	userCode := stringifyAny(deviceData["user_code"])
	expiresIn, _ := anyInt(deviceData["expires_in"])
	interval, _ := anyInt(deviceData["interval"])
	if err := printDeviceCodeInstructions(out, verificationURL, userCode, openBrowser, "—", true); err != nil {
		return nil, err
	}
	if args.OnVerification != nil {
		func() {
			defer func() { recover() }()
			args.OnVerification(verificationURL, userCode)
		}()
	}
	effectiveInterval := interval
	if cap := int(DeviceAuthPollIntervalCap / time.Second); effectiveInterval > cap {
		effectiveInterval = cap
	}
	if effectiveInterval < 1 {
		effectiveInterval = 1
	}
	fmt.Fprintf(out, "Waiting for approval (polling every %ds)...\n", effectiveInterval)
	tokenData, err := pollForToken(ctx, client, portalBaseURL, clientID, stringifyAny(deviceData["device_code"]), expiresIn, interval, func() bool { return ctx.Err() != nil })
	if err != nil {
		return nil, err
	}
	now := nowFunc()
	tokenExpiresIn := coerceTTLSeconds(tokenData["expires_in"])
	resolvedInferenceURL := optionalBaseURL(tokenData["inference_base_url"])
	if resolvedInferenceURL == "" {
		resolvedInferenceURL = requestedInferenceURL
	}
	if resolvedInferenceURL != requestedInferenceURL {
		fmt.Fprintf(out, "Using portal-provided inference URL: %s\n", resolvedInferenceURL)
	}
	authState := map[string]any{
		"portal_base_url": portalBaseURL, "inference_base_url": resolvedInferenceURL,
		"client_id": clientID, "scope": nousOr(tokenData["scope"], scope),
		"token_type":   nousOr(tokenData["token_type"], "Bearer"),
		"access_token": tokenData["access_token"], "refresh_token": tokenData["refresh_token"],
		"obtained_at": nousISO(now), "expires_at": nousISOAfter(now, tokenExpiresIn),
		"expires_in": tokenExpiresIn, "tls": tlsStateFromVerify(verify.Insecure, verify.CABundle),
	}
	for k, v := range nousEmptyAgentKeyFields {
		authState[k] = v
	}
	refreshed, err := refreshNousOAuthFromState(env, authState, timeout, false, nil)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) && authErr.Code == "subscription_required" {
			portalURL := strings.TrimRight(anyString(nousOr(authState["portal_base_url"], DefaultNousPortalURL)), "/")
			fmt.Fprintln(out)
			fmt.Fprintln(out, formatAuthError(authErr))
			fmt.Fprintf(out, "  Subscribe here: %s/billing\n", portalURL)
			fmt.Fprintln(out)
			fmt.Fprintln(out, "After subscribing, run `icebath model` again to finish setup.")
			return nil, &NousLoginExitError{Code: 1, Err: err}
		}
		return nil, err
	}
	return refreshed, nil
}

// Mirrors to the shared store and reseeds the pool, swallowing every failure
func nousMirrorStateBestEffort(env PoolEnv, authState map[string]any) {
	func() {
		defer func() { recover() }()
		writeSharedNousState(env, authState, false)
	}()
	nousSyncPoolFromAuthStore(env)
}

// StepUpNousBillingScope reruns the device flow asking for billing manage, false when the Portal downscoped it
func StepUpNousBillingScope(env PoolEnv, out io.Writer, openBrowser bool, timeout time.Duration, onVerification func(string, string)) (bool, error) {
	prior, err := env.Store.GetProviderAuthState("nous")
	if err != nil {
		return false, err
	}
	if prior == nil {
		prior = map[string]any{}
	}
	var priorScope []string
	if raw, ok := prior["scope"].(string); ok {
		priorScope = strings.Fields(raw)
	}
	if len(priorScope) == 0 {
		priorScope = []string{NousInferenceInvokeScope}
	}
	seen := map[string]bool{}
	var requested []string
	for _, s := range append(priorScope, NousBillingManageScope) {
		if !seen[s] {
			seen[s] = true
			requested = append(requested, s)
		}
	}
	authState, err := nousDeviceCodeLoginFn(env, out, NousDeviceLoginArgs{
		PortalBaseURL: anyString(prior["portal_base_url"]), InferenceBaseURL: anyString(prior["inference_base_url"]),
		ClientID: firstNonEmpty(anyString(prior["client_id"]), DefaultNousClientID), Scope: strings.Join(requested, " "),
		OpenBrowser: openBrowser, Timeout: timeout, OnVerification: onVerification})
	if err != nil {
		return false, err
	}
	if _, err := env.Store.SaveActiveProviderState("nous", authState); err != nil {
		return false, err
	}
	nousMirrorStateBestEffort(env, authState)
	if _, err := nousGuestHandoffDrainFn(env); err != nil {
		return false, err
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				env.log().Debug(fmt.Sprintf("Billing token cache invalidation failed: %v", r))
			}
		}()
		nousBillingInvalidateFn()
	}()
	nousSettleAfterUpgradeFn(env, authState)
	granted, ok := authState["scope"].(string)
	return ok && containsString(strings.Fields(granted), NousBillingManageScope), nil
}

// Fetches the curated tier and policy filtered Nous list and runs the interactive picker, empty when skipped
func nousPickModelAfterLogin(env PoolEnv, out io.Writer, authState map[string]any, inferenceBaseURL string) (string, error) {
	runtimeKey := anyString(nousOr(authState["agent_key"], authState["access_token"]))
	if runtimeKey == "" {
		return "", nousErr("No runtime API key available to fetch models", "invalid_token", false)
	}
	ctx := env.ctx()
	modelIDs := nousLoginCuratedIDsFn(ctx)
	portal := anyString(authState["portal_base_url"])
	fmt.Fprintln(out)
	var unavailableModels []string
	unavailableMessage := ""
	policyNarrowed := false
	var pricing PricingTable
	if len(modelIDs) > 0 {
		pricing = nousLoginPricingFn(ctx)
		freeTier := nousLoginFreeTierFn(ctx)
		policyAllowed, _ := nousLoginPolicyAllowedFn(ctx)
		if freeTier {
			unavailableMessage = nousPortalEntitlementMessageFor(env, "paid Nous models")
		}
		if freeTier {
			modelIDs, pricing = UnionWithPortalFreeRecommendations(ctx, modelIDs, pricing, portal, false)
		} else {
			modelIDs, pricing = UnionWithPortalPaidRecommendations(ctx, modelIDs, pricing, portal, false)
			modelIDs = UnionWithNousOnSaleModels(modelIDs, pricing)
		}
		before := modelIDs
		modelIDs = RestrictToNousPolicy(modelIDs, policyAllowed, true)
		policyNarrowed = strings.Join(modelIDs, "\x00") != strings.Join(before, "\x00")
		if freeTier {
			modelIDs, unavailableModels = PartitionNousModelsByTier(modelIDs, pricing, true)
		}
	}
	if len(modelIDs) > 0 {
		if notice := nousPolicyNotice(env, policyNarrowed); notice != "" {
			fmt.Fprintln(out, notice)
		}
		usage, featured, hasUsage := nousPickerUsage(env, true)
		if hasUsage {
			modelIDs = featuredFirst(modelIDs, featured)
		}
		fmt.Fprintf(out, "Showing %d curated models — use \"Enter custom model name\" for others.\n", len(modelIDs))
		selected, _ := nousPromptModelSelectionFn(env, out, ModelSelectionPrompt{
			ModelIDs: modelIDs, Pricing: pricing, UnavailableModels: unavailableModels, PortalURL: portal,
			UnavailableMessage: unavailableMessage, ConfirmProvider: "nous", ConfirmBaseURL: inferenceBaseURL,
			ConfirmAPIKey: runtimeKey, Usage: usage})
		return selected, nil
	}
	if len(unavailableModels) > 0 {
		u := strings.TrimRight(firstNonEmpty(portal, DefaultNousPortalURL), "/")
		fmt.Fprintln(out, "No free models currently available.")
		fmt.Fprintln(out, firstNonEmpty(unavailableMessage, fmt.Sprintf("Upgrade at %s to access paid models.", u)))
	} else {
		fmt.Fprintln(out, "No curated models available for Nous Portal.")
	}
	return "", nil
}

// Offers to rehydrate a Nous credential from another profile's shared store before a fresh device flow
func nousOfferSharedImport(env PoolEnv, out io.Writer, timeout time.Duration) (map[string]any, bool) {
	shared, ok := readSharedNousState(env)
	if !ok || isGuestState(shared) {
		return nil, false
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Found existing Nous OAuth credentials at %s\n", nousSharedStorePath(env))
	if !promptYesNo("Import these credentials? [Y/n]: ", "y") {
		return nil, false
	}
	fmt.Fprintln(out, "Rehydrating Nous session from shared credentials...")
	authState, ok := tryImportSharedNousState(env, timeout)
	if !ok {
		fmt.Fprintln(out, "Could not refresh shared credentials — falling back to device-code login.")
		return nil, false
	}
	return authState, true
}

// Undoes the active_provider nous that saving the login state wrote
func nousRestoreActiveProvider(env PoolEnv, priorActiveProvider any) error {
	return env.Store.Update(func(data AuthData) (bool, error) {
		if truthyAny(priorActiveProvider) {
			data["active_provider"] = priorActiveProvider
		} else {
			delete(data, "active_provider")
		}
		return true, nil
	})
}

// NousLoginArgs are the command line knobs of the Nous Portal login
type NousLoginArgs struct {
	PortalURL    string
	InferenceURL string
	ClientID     string
	Scope        string
	NoBrowser    bool
	Timeout      time.Duration
	CABundle     string
	Insecure     bool
}

// LoginNous runs the Nous Portal device authorization flow then the model picker
func LoginNous(env PoolEnv, out io.Writer, args NousLoginArgs) error {
	err := loginNous(env, out, args)
	if err == nil {
		return nil
	}
	var exit *NousLoginExitError
	if errors.As(err, &exit) {
		return err
	}
	var cancelled *DeviceFlowCancelledError
	var signInCancelled *SignInCancelledError
	if errors.Is(err, context.Canceled) || errors.As(err, &cancelled) || errors.As(err, &signInCancelled) {
		fmt.Fprintln(out, "\nLogin cancelled.")
		return &NousLoginExitError{Code: 130, Err: err}
	}
	env.log().Debug(fmt.Sprintf("nous login failed: %v", err))
	fmt.Fprintln(out)
	for _, line := range SignInFailureLines(err, nousPortalHost(args.PortalURL), "") {
		fmt.Fprintln(out, line)
	}
	return &NousLoginExitError{Code: 1, Err: err}
}

func loginNous(env PoolEnv, out io.Writer, args NousLoginArgs) error {
	timeout := args.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	caBundle := firstNonEmpty(args.CABundle, oauthGetenv("ICEBATH_CA_BUNDLE"), oauthGetenv("SSL_CERT_FILE"))
	authState, imported := nousOfferSharedImport(env, out, timeout)
	if !imported {
		var err error
		authState, err = nousDeviceCodeLoginFn(env, out, NousDeviceLoginArgs{
			PortalBaseURL: args.PortalURL, InferenceBaseURL: args.InferenceURL,
			ClientID: firstNonEmpty(args.ClientID, DefaultNousClientID), Scope: args.Scope,
			OpenBrowser: !args.NoBrowser, Timeout: timeout, Insecure: args.Insecure, CABundle: caBundle})
		if err != nil {
			return err
		}
	}
	inferenceBaseURL := anyString(authState["inference_base_url"])
	var priorActiveProvider any
	err := func() error {
		unlock, err := env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		data, err := env.Store.Load()
		if err != nil {
			return err
		}
		priorActiveProvider = data["active_provider"]
		return nil
	}()
	if err != nil {
		return err
	}
	savedTo, err := env.Store.SaveActiveProviderState("nous", authState)
	if err != nil {
		return err
	}
	writeSharedNousState(env, authState, false)
	handoff, err := nousGuestHandoffDrainFn(env)
	if err != nil {
		return err
	}
	nousSyncPoolFromAuthStore(env)
	nousSettleAfterUpgradeFn(env, authState)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Login successful!")
	fmt.Fprintf(out, "  Auth state: %s\n", savedTo)
	for _, view := range handoff {
		for _, line := range guestHandoffResultLines(view) {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	selected, pickErr := nousPickModelAfterLogin(env, out, authState, inferenceBaseURL)
	if pickErr != nil {
		message := pickErr.Error()
		var authErr *AuthError
		if errors.As(pickErr, &authErr) {
			message = formatAuthError(authErr)
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Login succeeded, but could not fetch available models. Reason: %s\n", message)
	}
	if selected == "" {
		if err := nousRestoreActiveProvider(env, priorActiveProvider); err != nil {
			return err
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "No provider change. Nous credentials saved for future use.")
		fmt.Fprintln(out, "  Run `icebath model` again to switch to Nous Portal.")
		return nil
	}
	configPath, err := updateConfigForProvider(env, "nous", inferenceBaseURL, selected, false)
	if err != nil {
		return err
	}
	if err := nousSaveModelChoiceFn(env, selected); err != nil {
		return err
	}
	fmt.Fprintf(out, "Default model set to: %s\n", selected)
	if notice := largeModelNotice(env, selected, "nous"); notice != "" {
		fmt.Fprintf(out, "  ⚠ %s\n", notice)
	}
	fmt.Fprintf(out, "  Config updated: %s (model.provider=nous)\n", configPath)
	return nil
}
