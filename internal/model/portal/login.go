package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const portalDisplayName = "Icebath Portal"

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.Err }

func say(c auth.Console, text string) {
	if c.Out != nil {
		fmt.Fprintln(c.Out, text)
	}
}

type DeviceLoginOptions struct {
	PortalURL      string
	InferenceURL   string
	ClientID       string
	Scope          string
	NoBrowser      bool
	Timeout        time.Duration
	Insecure       bool
	CABundle       string
	OnVerification func(verificationURL, userCode string)
}

func (a *Account) deviceCodeLogin(ctx context.Context, c auth.Console, o DeviceLoginOptions) (*object, error) {
	portalBaseURL := o.PortalURL
	if portalBaseURL == "" {
		portalBaseURL = a.scopedOverride(ctx, EnvPortalBaseURL, EnvPortalBaseURLAlias)
	}
	if portalBaseURL == "" {
		portalBaseURL = auth.DefaultPortalURL
	}
	portalBaseURL = strings.TrimRight(portalBaseURL, "/")
	requestedInference := o.InferenceURL
	if requestedInference == "" {
		requestedInference = a.scopedOverride(ctx, EnvInferenceBaseURL)
	}
	if requestedInference == "" {
		requestedInference = auth.DefaultPortalInferenceURL
	}
	requestedInference = strings.TrimRight(requestedInference, "/")
	clientID := o.ClientID
	if clientID == "" {
		clientID = auth.DefaultPortalClientID
	}
	scope := o.Scope
	if scope == "" {
		scope = auth.DefaultPortalScope
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	openBrowser := !o.NoBrowser && !a.store.IsRemoteSession(ctx)
	say(c, fmt.Sprintf("Starting icebath login via %s...", portalDisplayName))
	say(c, "Portal: "+portalBaseURL)
	if o.Insecure {
		say(c, "TLS verification: disabled (--insecure)")
	} else if o.CABundle != "" {
		say(c, fmt.Sprintf("TLS verification: custom CA bundle (%s)", o.CABundle))
	}
	insecure := o.Insecure
	client, _, err := a.store.VerifiedClient(ctx, &insecure, o.CABundle, auth.TLSState{})
	if err != nil {
		return nil, err
	}
	tls := auth.TLSState{Insecure: o.Insecure, CABundle: o.CABundle}
	deviceRaw, err := a.store.RequestPortalDeviceCode(ctx, client, portalBaseURL, clientID, scope, timeout)
	if err != nil {
		return nil, err
	}
	device := objectOrEmpty(deviceRaw)
	for _, key := range []string{"verification_uri_complete", "user_code"} {
		if !device.has(key) {
			return nil, fmt.Errorf("Device code response carried no %s", key)
		}
	}
	verificationURL, userCode := textual(device.get("verification_uri_complete")), textual(device.get("user_code"))
	expiresIn, ok := intOf(device.get("expires_in"))
	if !ok {
		return nil, errors.New("Device code response carried a non-numeric expires_in")
	}
	interval, ok := intOf(device.get("interval"))
	if !ok {
		return nil, errors.New("Device code response carried a non-numeric interval")
	}
	if err := a.store.PrintDeviceCodeInstructions(ctx, c, verificationURL, userCode, openBrowser, "—", false); err != nil {
		return nil, err
	}
	if o.OnVerification != nil {
		o.OnVerification(verificationURL, userCode)
	}
	effective := max(1, min(interval, int64(auth.DevicePollIntervalCap/time.Second)))
	say(c, fmt.Sprintf("Waiting for approval (polling every %ds)...", effective))
	if !device.has("device_code") {
		return nil, errors.New("Device code response carried no device_code")
	}
	tokenRaw, err := a.store.PollPortalToken(ctx, client, portalBaseURL, clientID, textual(device.get("device_code")),
		time.Duration(expiresIn)*time.Second, time.Duration(interval)*time.Second, timeout, func() bool { return false })
	if err != nil {
		return nil, err
	}
	tokenData := objectOrEmpty(tokenRaw)
	if !tokenData.has("access_token") {
		return nil, errors.New("Token response carried no access_token")
	}
	state := a.loginState(tokenData, portalBaseURL, requestedInference, clientID, scope, tls)
	if resolved := state.str("inference_base_url"); resolved != requestedInference {
		say(c, "Using portal-provided inference URL: "+resolved)
	}
	refreshed, err := a.refreshFromState(ctx, state, timeout, false, nil)
	if err != nil {
		if ae, ok := authErrorOf(err); ok && ae.Code == "subscription_required" {
			say(c, "")
			say(c, a.store.FormatError(ctx, err))
			say(c, fmt.Sprintf("  Subscribe here: %s/billing", strings.TrimRight(state.str("portal_base_url"), "/")))
			say(c, "")
			say(c, "After subscribing, run `icebath model` again to finish setup.")
			return nil, &ExitError{Code: 1, Err: err}
		}
		return nil, err
	}
	return refreshed, nil
}

func (a *Account) DeviceCodeLogin(ctx context.Context, c auth.Console, o DeviceLoginOptions) (json.RawMessage, error) {
	state, err := a.deviceCodeLogin(a.bind(ctx), c, o)
	if err != nil {
		return nil, err
	}
	return state.raw(), nil
}

func (a *Account) OfferSharedImport(ctx context.Context, c auth.Console, timeout time.Duration) (json.RawMessage, bool) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	state := a.offerSharedImport(a.bind(ctx), c, timeout)
	if state == nil {
		return nil, false
	}
	return state.raw(), true
}

func (a *Account) mirrorState(ctx context.Context, state *object) {
	a.writeShared(ctx, state, false)
	a.syncPool(ctx)
}

type StepUpOptions struct {
	NoBrowser      bool
	Timeout        time.Duration
	OnVerification func(verificationURL, userCode string)
}

func (a *Account) StepUpBillingScope(ctx context.Context, c auth.Console, o StepUpOptions) (bool, error) {
	ctx = a.bind(ctx)
	prior, err := a.currentState(ctx)
	if err != nil {
		return false, err
	}
	priorScope := strings.Fields(prior.str("scope"))
	if len(priorScope) == 0 {
		priorScope = []string{auth.PortalInferenceInvokeScope}
	}
	requested := []string{}
	for _, part := range append(priorScope, auth.PortalBillingManageScope) {
		if !slices.Contains(requested, part) {
			requested = append(requested, part)
		}
	}
	state, err := a.deviceCodeLogin(ctx, c, DeviceLoginOptions{
		PortalURL: textualOrEmpty(prior.get("portal_base_url")), InferenceURL: textualOrEmpty(prior.get("inference_base_url")),
		ClientID: textualOrEmpty(prior.get("client_id")), Scope: strings.Join(requested, " "),
		NoBrowser: o.NoBrowser, Timeout: o.Timeout, OnVerification: o.OnVerification,
	})
	if err != nil {
		return false, err
	}
	if err := a.store.SaveActive(ctx, Provider, state.raw()); err != nil {
		return false, err
	}
	a.mirrorState(ctx, state)
	if _, err := a.Drain(ctx); err != nil {
		return false, err
	}
	a.InvalidateBillingToken()
	a.settleAfterUpgrade(ctx, state)
	return slices.Contains(strings.Fields(state.str("scope")), auth.PortalBillingManageScope), nil
}

type PickRequest struct {
	PortalURL        string
	InferenceBaseURL string
	RuntimeKey       auth.Secret
}

type ModelSetup interface {
	Pick(ctx context.Context, r PickRequest) (string, error)
	Save(ctx context.Context, model string) error
	LargeModelNotice(ctx context.Context, model string) string
}

type LoginOptions struct {
	Device DeviceLoginOptions
	Models ModelSetup
}

func (a *Account) offerSharedImport(ctx context.Context, c auth.Console, timeout time.Duration) *object {
	shared := a.readShared()
	if shared == nil || isGuestState(shared) {
		return nil
	}
	say(c, "")
	say(c, "Found existing Icebath Portal OAuth credentials at "+a.sharedStorePath())
	if !auth.PromptYesNo(c, "Import these credentials? [Y/n]: ", true) {
		return nil
	}
	say(c, "Rehydrating Icebath Portal session from shared credentials...")
	state := a.tryImportShared(ctx, timeout)
	if state == nil {
		say(c, "Could not refresh shared credentials — falling back to device-code login.")
	}
	return state
}

func (a *Account) pickModelAfterLogin(ctx context.Context, models ModelSetup, state *object, inferenceBaseURL string) (string, error) {
	runtimeKey := state.str("agent_key")
	if runtimeKey == "" {
		runtimeKey = state.str("access_token")
	}
	if runtimeKey == "" {
		return "", portalErr("No runtime API key available to fetch models", "invalid_token", false)
	}
	return models.Pick(ctx, PickRequest{PortalURL: textualOrEmpty(state.get("portal_base_url")), InferenceBaseURL: inferenceBaseURL,
		RuntimeKey: auth.NewSecret(runtimeKey)})
}

func portalHost(portalURL string) string {
	if portalURL == "" {
		portalURL = auth.DefaultPortalURL
	}
	if host := hostOf(portalURL); host != "" {
		return host
	}
	return "portal.icebath.app"
}

func (a *Account) Login(ctx context.Context, c auth.Console, o LoginOptions) error {
	ctx = a.bind(ctx)
	if o.Models == nil {
		return errors.New("portal: login needs the model setup functions")
	}
	err := a.login(ctx, c, o)
	if err == nil {
		return nil
	}
	var exit *ExitError
	if errors.As(err, &exit) {
		return err
	}
	if auth.IsCancelled(err) {
		say(c, "\nLogin cancelled.")
		return &ExitError{Code: 130, Err: err}
	}
	slog.Debug("portal login failed", "error", fmt.Sprintf("%T", err))
	say(c, "")
	for _, line := range auth.SignInFailureLines(err, portalHost(o.Device.PortalURL), "") {
		say(c, line)
	}
	return &ExitError{Code: 1, Err: err}
}

func (a *Account) login(ctx context.Context, c auth.Console, o LoginOptions) error {
	device := o.Device
	if device.Timeout <= 0 {
		device.Timeout = defaultTimeout
	}
	device.CABundle = firstText(device.CABundle, os.Getenv("ICEBATH_CA_BUNDLE"), os.Getenv("SSL_CERT_FILE"))
	state := a.offerSharedImport(ctx, c, device.Timeout)
	if state == nil {
		fresh, err := a.deviceCodeLogin(ctx, c, device)
		if err != nil {
			return err
		}
		state = fresh
	}
	inferenceBaseURL := textual(state.get("inference_base_url"))
	priorActive, err := a.store.Active(ctx)
	if err != nil {
		return err
	}
	if err := a.store.SaveActive(ctx, Provider, state.raw()); err != nil {
		return err
	}
	a.writeShared(ctx, state, false)
	handoff, err := a.Drain(ctx)
	if err != nil {
		return err
	}
	a.syncPool(ctx)
	a.settleAfterUpgrade(ctx, state)
	say(c, "")
	say(c, "Login successful!")
	say(c, "  Auth state: "+a.store.Path())
	for _, view := range handoff {
		for _, line := range a.ResultLines(view) {
			say(c, "  "+line)
		}
	}
	selected, err := a.pickModelAfterLogin(ctx, o.Models, state, inferenceBaseURL)
	if err != nil {
		if auth.IsCancelled(err) {
			return err
		}
		message := err.Error()
		if _, ok := authErrorOf(err); ok {
			message = a.store.FormatError(ctx, err)
		}
		say(c, "")
		say(c, "Login succeeded, but could not fetch available models. Reason: "+message)
		selected = ""
	}
	if selected == "" {
		if err := a.restoreActive(ctx, priorActive); err != nil {
			return err
		}
		say(c, "")
		say(c, "No provider change. Icebath Portal credentials saved for future use.")
		say(c, "  Run `icebath model` again to switch to Icebath Portal.")
		return nil
	}
	configPath, err := a.update(ctx, ConfigUpdate{Provider: Provider, BaseURL: inferenceBaseURL, DefaultModel: selected})
	if err != nil {
		return err
	}
	if err := o.Models.Save(ctx, selected); err != nil {
		return err
	}
	say(c, "Default model set to: "+selected)
	if notice := o.Models.LargeModelNotice(ctx, selected); notice != "" {
		say(c, "  ⚠ "+notice)
	}
	say(c, fmt.Sprintf("  Config updated: %s (model.provider=%s)", configPath, Provider))
	return nil
}
