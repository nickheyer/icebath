package dingtalk

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyqrcode"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Registration API base and source of the device flow, env overrides first
func registrationBaseURL() string {
	if v, ok := os.LookupEnv("DINGTALK_REGISTRATION_BASE_URL"); ok {
		return strings.TrimRight(v, "/")
	}
	return "https://oapi.dingtalk.com"
}

func registrationSource() string {
	if v, ok := os.LookupEnv("DINGTALK_REGISTRATION_SOURCE"); ok {
		return v
	}
	return "openClaw"
}

var pollStatuses = map[string]bool{"WAITING": true, "SUCCESS": true, "FAIL": true, "EXPIRED": true}

// Seconds of transient errors or non success statuses tolerated before giving up
const retryWindow = 120.0

// Failure of a DingTalk registration API call
type RegistrationError struct{ Msg string }

func (e *RegistrationError) Error() string { return e.Msg }

// HTTP, clock, sleep and QR importability seams of the device flow, the module names its tests patch
var (
	registrationPost = func(ctx context.Context, url string, payload any) (*pyrequests.Response, error) {
		body, err := pyjson.EncodeWTF8PythonASCII(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return pyrequests.Send(ctx, http.DefaultTransport.(*http.Transport), pyrequests.Seconds(15), req)
	}
	authSleep     = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	authClockBase = time.Now()
	authMonotonic = func() float64 { return time.Since(authClockBase).Seconds() }
	apiPostFn     = apiPost
	pollFn        = pollRegistration
)

// POSTs to the registration API, the RegistrationError of a network failure or a non zero errcode
func apiPost(ctx context.Context, path string, payload *pyjson.Object) (*pyjson.Object, error) {
	url := registrationBaseURL() + path
	network := func(err error) error {
		return &RegistrationError{fmt.Sprintf("Network error calling %s: %s", url, err.Error())}
	}
	resp, err := registrationPost(ctx, url, payload)
	if err != nil {
		if _, ok := err.(*pyrequests.Error); ok {
			return nil, network(err)
		}
		return nil, err
	}
	if err := resp.RaiseForStatus(url); err != nil {
		return nil, network(err)
	}
	decoded, err := pyjson.LoadsBytes(resp.Body)
	if err != nil {
		return nil, network(err)
	}
	data, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	errcode, present := data.Vals["errcode"]
	if !present {
		errcode = int64(-1)
	}
	if !numberEquals(errcode, 0) {
		msg, ok := data.Vals["errmsg"]
		if !ok {
			msg = "unknown error"
		}
		return nil, &RegistrationError{fmt.Sprintf("API error [%s]: %s (errcode=%s)", path, pyrepr.Str(msg), pyrepr.Str(errcode))}
	}
	return data, nil
}

// Whether a decoded JSON value equals the integer n the way Python compares numbers
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
func fieldText(o *pyjson.Object, key string, def any) string {
	v, ok := o.Vals[key]
	if !ok {
		v = def
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Python int of an object field, the default when missing
func fieldInt(o *pyjson.Object, key string, def int64) (int64, error) {
	v, ok := o.Vals[key]
	if !ok {
		return def, nil
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Device flow registration parameters, the nonce exchanged for the device code and verification URL
type registration struct {
	DeviceCode, VerificationURIComplete string
	ExpiresIn, Interval                 int64
}

// Starts a device flow registration, init for the nonce then begin for the device code and URL
func beginRegistration(ctx context.Context) (*registration, error) {
	initData, err := apiPostFn(ctx, "/app/registration/init", pyjson.ObjectOf("source", registrationSource()))
	if err != nil {
		return nil, err
	}
	nonce := fieldText(initData, "nonce", "")
	if nonce == "" {
		return nil, &RegistrationError{"init response missing nonce"}
	}
	begin, err := apiPostFn(ctx, "/app/registration/begin", pyjson.ObjectOf("nonce", nonce))
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
	data, err := apiPostFn(ctx, "/app/registration/poll", pyjson.ObjectOf("device_code", deviceCode))
	if err != nil {
		return nil, err
	}
	status := pystr.Upper(fieldText(data, "status", ""))
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
	code, err := pyqrcode.Make(url, pyqrcode.ErrorCorrectL, 1)
	if err != nil {
		return err
	}
	matrix := code.Matrix()
	glyph := map[[2]bool]string{{true, true}: "█", {true, false}: "▀", {false, true}: "▄", {false, false}: " "}
	var lines []string
	for r := 0; r < len(matrix); r += 2 {
		bottom := make([]bool, len(matrix[r]))
		if r+1 < len(matrix) {
			bottom = matrix[r+1]
		}
		var b strings.Builder
		b.WriteString("    ")
		for i, top := range matrix[r] {
			b.WriteString(glyph[[2]bool{top, bottom[i]}])
		}
		lines = append(lines, b.String())
	}
	gateway.WizardPrintln(strings.Join(lines, "\n"))
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
