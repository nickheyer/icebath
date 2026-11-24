package dingtalk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	qrutil "github.com/nickheyer/icebath/pkg/utils/qr"
)

// Registration API base of the device flow from dingtalk.registration_base_url
func registrationBaseURL(ctx context.Context) string {
	return strings.TrimRight(strings.TrimSpace(settings.DingtalkRegistrationBaseURL.Get(gateway.SettingsContext(ctx))), "/")
}

// Source name of the device flow from dingtalk.registration_source
func registrationSource(ctx context.Context) string {
	return strings.TrimSpace(settings.DingtalkRegistrationSource.Get(gateway.SettingsContext(ctx)))
}

var pollStatuses = map[string]bool{"WAITING": true, "SUCCESS": true, "FAIL": true, "EXPIRED": true}

// Seconds of transient errors or non success statuses tolerated before giving up
const retryWindow = 120.0

// Failure of a DingTalk registration API call
type RegistrationError struct{ Msg string }

func (e *RegistrationError) Error() string { return e.Msg }

// HTTP, clock, sleep and QR importability seams of the device flow, the module names its tests patch
var (
	registrationPost = func(ctx context.Context, endpoint string, payload any) (*httputil.Response, error) {
		body, err := jsonutil.Append(nil, payload, jsonutil.ASCII())
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Transport: httputil.TrustEnvCA(nil), CheckRedirect: httputil.RedirectPolicy(true)}
		return httputil.Do(client, req, httputil.EachPhase(15*time.Second))
	}
	authSleep     = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	authClockBase = time.Now()
	authMonotonic = func() float64 { return time.Since(authClockBase).Seconds() }
	apiPostFn     = apiPost
	pollFn        = pollRegistration
)

// POSTs to the registration API, the RegistrationError of a network failure or a non zero errcode
func apiPost(ctx context.Context, path string, payload *jsonutil.Object) (*jsonutil.Object, error) {
	endpoint := registrationBaseURL(ctx) + path
	network := func(err error) error {
		return &RegistrationError{fmt.Sprintf("Network error calling %s: %s", endpoint, err.Error())}
	}
	resp, err := registrationPost(ctx, endpoint, payload)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, network(err)
		}
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, network(resp.CheckStatus())
	}
	decoded, err := jsonutil.Decode(resp.Content)
	if err != nil {
		return nil, network(err)
	}
	data, ok := decoded.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("the DingTalk reply is %s, expected a mapping", errchain.Kind(decoded))
	}
	errcode, present := data.Get("errcode")
	if !present {
		errcode = int64(-1)
	}
	if !numberEquals(errcode, 0) {
		msg, ok := data.Get("errmsg")
		if !ok {
			msg = "unknown error"
		}
		return nil, &RegistrationError{fmt.Sprintf("API error [%s]: %s (errcode=%s)", path, anyutil.String(msg), anyutil.String(errcode))}
	}
	return data, nil
}

// Whether a decoded JSON value equals the integer n, a boolean counting as 1 or 0
func numberEquals(v any, n int64) bool {
	switch x := v.(type) {
	case int64:
		return x == n
	case float64:
		return x == float64(n)
	case bool:
		return (x && n == 1) || (!x && n == 0)
	}
	return false
}

// Stripped str of an object field, the default when it is missing
func fieldText(o *jsonutil.Object, key string, def any) string {
	v, ok := o.Get(key)
	if !ok {
		v = def
	}
	return strings.TrimSpace(anyutil.String(v))
}

// Integer of an object field converted from a number, numeric text or a boolean, the default when missing
func fieldInt(o *jsonutil.Object, key string, def int64) (int64, error) {
	v, ok := o.Get(key)
	if !ok {
		return def, nil
	}
	return anyutil.ToInt(v)
}

// Device flow registration parameters, the nonce exchanged for the device code and verification URL
type registration struct {
	DeviceCode, VerificationURIComplete string
	ExpiresIn, Interval                 int64
}

// Starts a device flow registration, init for the nonce then begin for the device code and URL
func beginRegistration(ctx context.Context) (*registration, error) {
	initData, err := apiPostFn(ctx, "/app/registration/init", jsonutil.ObjectOf("source", registrationSource(ctx)))
	if err != nil {
		return nil, err
	}
	nonce := fieldText(initData, "nonce", "")
	if nonce == "" {
		return nil, &RegistrationError{"init response missing nonce"}
	}
	begin, err := apiPostFn(ctx, "/app/registration/begin", jsonutil.ObjectOf("nonce", nonce))
	if err != nil {
		return nil, err
	}
	reg := &registration{DeviceCode: fieldText(begin, "device_code", ""), VerificationURIComplete: fieldText(begin, "verification_uri_complete", "")}
	if reg.DeviceCode == "" {
		return nil, &RegistrationError{"begin response missing device_code"}
	}
	if reg.VerificationURIComplete == "" {
		return nil, &RegistrationError{"begin response missing verification_uri_complete"}
	}
	if reg.ExpiresIn, err = fieldInt(begin, "expires_in", 7200); err != nil {
		return nil, err
	}
	interval, err := fieldInt(begin, "interval", 3)
	if err != nil {
		return nil, err
	}
	reg.Interval = max(interval, 2)
	return reg, nil
}

// One poll of the registration status, unknown statuses reported as UNKNOWN
type pollResult struct {
	Status                             string
	ClientID, ClientSecret, FailReason string
}

// Polls the registration status once
func pollRegistration(ctx context.Context, deviceCode string) (*pollResult, error) {
	data, err := apiPostFn(ctx, "/app/registration/poll", jsonutil.ObjectOf("device_code", deviceCode))
	if err != nil {
		return nil, err
	}
	status := strings.ToUpper(fieldText(data, "status", ""))
	if !pollStatuses[status] {
		status = "UNKNOWN"
	}
	return &pollResult{Status: status, ClientID: fieldText(data, "client_id", ""), ClientSecret: fieldText(data, "client_secret", ""),
		FailReason: fieldText(data, "fail_reason", "")}, nil
}

// Blocks until the registration succeeds or times out, retrying failures inside the retry window
func waitForRegistrationSuccess(ctx context.Context, deviceCode string, interval, expiresIn int64, onWaiting func()) (string, string, error) {
	deadline := authMonotonic() + float64(expiresIn)
	retryStart := 0.0
	withinRetryWindow := func() bool {
		if retryStart == 0 {
			retryStart = authMonotonic()
		}
		return authMonotonic()-retryStart < retryWindow
	}
	for authMonotonic() < deadline {
		authSleep(float64(interval))
		result, err := pollFn(ctx, deviceCode)
		if err != nil {
			if _, ok := err.(*RegistrationError); ok && withinRetryWindow() {
				continue
			}
			return "", "", err
		}
		switch result.Status {
		case "WAITING":
			retryStart = 0
			if onWaiting != nil {
				onWaiting()
			}
		case "SUCCESS":
			if result.ClientID == "" || result.ClientSecret == "" {
				return "", "", &RegistrationError{"authorization succeeded but credentials are missing"}
			}
			return result.ClientID, result.ClientSecret, nil
		default:
			if !withinRetryWindow() {
				reason := result.FailReason
				if reason == "" {
					reason = result.Status
				}
				return "", "", &RegistrationError{"authorization failed: " + reason}
			}
		}
	}
	return "", "", &RegistrationError{"authorization timed out, please retry"}
}

// Prints the URL as a compact half block QR code, two matrix rows per character
func renderQRToTerminal(url string) error {
	code, err := qrutil.Encode(url, qrutil.Low)
	if err != nil {
		return err
	}
	art := strings.TrimSuffix(code.HalfBlocks(1, false), "\n")
	gateway.WizardPrintln("    " + strings.ReplaceAll(art, "\n", "\n    "))
	return nil
}

// Runs the interactive QR device flow authorization, the client id and secret or false when it did not complete
func dingtalkQRAuth(ctx context.Context) (string, string, bool, error) {
	s := gateway.WizardSetup
	gateway.WizardPrintln("")
	s.PrintInfo("  Initializing DingTalk device authorization...")
	s.PrintInfo("  Note: the scan page is branded 'OpenClaw' — DingTalk's")
	s.PrintInfo("        ecosystem onboarding bridge. Safe to use.")
	reg, err := beginRegistration(ctx)
	if err != nil {
		if regErr, ok := err.(*RegistrationError); ok {
			s.PrintError("  Authorization init failed: " + regErr.Error())
			return "", "", false, nil
		}
		return "", "", false, err
	}
	url := reg.VerificationURIComplete
	gateway.WizardPrintln("")
	s.PrintInfo("  Please scan the QR code below with DingTalk to authorize:")
	gateway.WizardPrintln("")
	if err := renderQRToTerminal(url); err != nil {
		return "", "", false, err
	}
	gateway.WizardPrintln("")
	s.PrintInfo("  Or open this link manually: " + url)
	gateway.WizardPrintln("")
	s.PrintInfo("  Waiting for QR scan authorization... (timeout: 2 hours)")
	dotCount := 0
	onWaiting := func() {
		dotCount++
		if dotCount%10 == 0 {
			fmt.Fprint(provider.SetupOutput(), ".")
		}
	}
	clientID, clientSecret, err := waitForRegistrationSuccess(ctx, reg.DeviceCode, reg.Interval, reg.ExpiresIn, onWaiting)
	if err != nil {
		if regErr, ok := err.(*RegistrationError); ok {
			gateway.WizardPrintln("")
			s.PrintError("  Authorization failed: " + regErr.Error())
			return "", "", false, nil
		}
		return "", "", false, err
	}
	gateway.WizardPrintln("")
	s.PrintSuccess("  QR scan authorization successful!")
	s.PrintSuccess("  Client ID:     " + clientID)
	masked := string([]rune(clientSecret)[:min(8, len([]rune(clientSecret)))]) + strings.Repeat("*", max(0, len([]rune(clientSecret))-8))
	s.PrintSuccess("  Client Secret: " + masked)
	return clientID, clientSecret, true, nil
}
