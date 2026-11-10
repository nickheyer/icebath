package feishu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyqrcode"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Accounts and open platform hosts of the scan to create onboarding per domain
var (
	onboardAccountsURLs = map[string]string{"feishu": "https://accounts.feishu.cn", "lark": "https://accounts.larksuite.com"}
	onboardOpenURLs     = map[string]string{"feishu": "https://open.feishu.cn", "lark": "https://open.larksuite.com"}
)

const (
	registrationPath          = "/oauth/v1/app/registration"
	onboardRequestTimeoutS    = 10
	onboardRequestTimeout     = onboardRequestTimeoutS * time.Second
	defaultQRRegisterTimeoutS = 600
)

// Accounts host of a domain, feishu for an unknown one
func accountsBaseURL(domain string) string {
	if u, ok := onboardAccountsURLs[domain]; ok {
		return u
	}
	return onboardAccountsURLs["feishu"]
}

// Open platform host of a domain, feishu for an unknown one
func onboardOpenBaseURL(domain string) string {
	if u, ok := onboardOpenURLs[domain]; ok {
		return u
	}
	return onboardOpenURLs["feishu"]
}

// HTTPError of urlopen, the status and the body the server sent
type HTTPError struct {
	Code int
	Body []byte
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP Error %d: %s", e.Code, http.StatusText(e.Code))
}

// URLError of urlopen, a request that got no answer
type URLError struct{ Reason string }

func (e *URLError) Error() string { return "<urlopen error " + e.Reason + ">" }

// Opens a request like urlopen, the 2xx body, HTTPError for 400 and up and URLError otherwise
var urlopenFn = func(ctx context.Context, req *http.Request, timeout time.Duration) ([]byte, error) {
	resp, err := (&http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: pyurl.UrlopenProxy}}).Do(req.WithContext(ctx))
	if err != nil {
		reason := err.Error()
		if pypath.OSErrorClass(err) != "OSError" {
			reason = pypath.OSErrorText(err)
		}
		return nil, &URLError{reason}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &URLError{err.Error()}
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{Code: resp.StatusCode, Body: body}
	}
	return body, nil
}

// JSON of a UTF-8 response body, the UnicodeDecodeError or JSONDecodeError of a bad one
func decodeJSONBody(body []byte) (any, error) {
	text, err := pystr.DecodeUTF8(body)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Whether an error is the URLError, OSError or JSONDecodeError class onboarding treats as an expected failure
func expectedOnboardError(err error) bool {
	var httpErr *HTTPError
	var urlErr *URLError
	var decode *pyjson.DecodeError
	return errors.As(err, &httpErr) || errors.As(err, &urlErr) || errors.As(err, &decode)
}

// POSTs form data to the registration endpoint, parsing JSON even from a 4xx answer since poll pending is a 400
func postRegistration(ctx context.Context, baseURL string, body [][2]string) (*pyjson.Object, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+registrationPath, strings.NewReader(pyurl.URLEncode(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	raw, err := urlopenFn(ctx, req, onboardRequestTimeout)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && len(httpErr.Body) > 0 {
		decoded, decodeErr := decodeJSONBody(httpErr.Body)
		if decodeErr != nil {
			return nil, err
		}
		return asObject(decoded)
	}
	if err != nil {
		return nil, err
	}
	decoded, err := decodeJSONBody(raw)
	if err != nil {
		return nil, err
	}
	return asObject(decoded)
}

// The dict of a decoded body, the AttributeError a get on anything else raises
func asObject(v any) (*pyjson.Object, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(v))
	}
	return o, nil
}

// RuntimeError of an onboarding step the registration environment refused
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string { return e.Msg }

// Checks the registration environment supports client_secret auth
func initRegistration(ctx context.Context, domain string) error {
	res, err := postRegistration(ctx, accountsBaseURL(domain), [][2]string{{"action", "init"}})
	if err != nil {
		return err
	}
	methods := res.Vals["supported_auth_methods"]
	if !pyrepr.Truthy(methods) {
		methods = []any{}
	}
	supported := false
	switch m := methods.(type) {
	case []any:
		for _, item := range m {
			supported = supported || item == "client_secret"
		}
	case string:
		supported = strings.Contains(m, "client_secret")
	case *pyjson.Object:
		_, supported = m.Vals["client_secret"]
	default:
		return fmt.Errorf("TypeError: argument of type '%s' is not iterable", pyrepr.TypeName(methods))
	}
	if !supported {
		return &RuntimeError{"Feishu / Lark registration environment does not support client_secret auth. " +
			"Supported: " + pyrepr.Str(methods)}
	}
	return nil
}

// Device code flow parameters the begin step returns
type beginResult struct {
	DeviceCode any
	QRURL      string
	UserCode   any
	Interval   any
	ExpireIn   any
}

// Value or the default when the value is falsy, like value or default
func orDefault(v, def any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return def
}

// Starts the device code flow, returning the device code, tagged QR URL, user code, interval and expiry
func beginRegistration(ctx context.Context, domain string) (*beginResult, error) {
	res, err := postRegistration(ctx, accountsBaseURL(domain), [][2]string{
		{"action", "begin"}, {"archetype", "PersonalAgent"}, {"auth_method", "client_secret"}, {"request_user_info", "open_id"}})
	if err != nil {
		return nil, err
	}
	deviceCode := res.Vals["device_code"]
	if !pyrepr.Truthy(deviceCode) {
		return nil, &RuntimeError{"Feishu / Lark registration did not return a device_code"}
	}
	rawURL, ok := res.Vals["verification_uri_complete"]
	if !ok {
		rawURL = ""
	}
	qrURL, isStr := rawURL.(string)
	if !isStr {
		return nil, fmt.Errorf("TypeError: argument of type '%s' is not iterable", pyrepr.TypeName(rawURL))
	}
	if strings.Contains(qrURL, "?") {
		qrURL += "&"
	} else {
		qrURL += "?"
	}
	qrURL += "from=hermes&tp=hermes"
	userCode, ok := res.Vals["user_code"]
	if !ok {
		userCode = ""
	}
	return &beginResult{DeviceCode: deviceCode, QRURL: qrURL, UserCode: userCode,
		Interval: orDefault(res.Vals["interval"], int64(5)), ExpireIn: orDefault(res.Vals["expire_in"], int64(600))}, nil
}

// Credentials a scan returned, the bot fields filled by the probe after registration
type Registration struct {
	AppID, AppSecret any
	Domain           string
	OpenID           any
	BotName          any
	BotOpenID        any
}

// Seconds of a JSON number, the TypeError of anything time.sleep or a deadline cannot take
func seconds(v any) (float64, error) {
	switch x := v.(type) {
	case int64:
		return float64(x), nil
	case float64:
		return x, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	}
	if n, ok := v.(*big.Int); ok {
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, nil
	}
	return 0, fmt.Errorf("TypeError: '%s' object cannot be interpreted as an integer", pyrepr.TypeName(v))
}

// Clock and sleep of the poll loop, seams tests replace like the patched time module
var (
	pollClockBase = time.Now()
	pollMonotonic = func() float64 { return time.Since(pollClockBase).Seconds() }
	pollSleep     = func(w *gateway.InterruptWatch, s float64) error { return w.Sleep(s) }
)

// Polls until the scan returns credentials, nil on denial or timeout, switching to lark when the tenant says so
func pollRegistration(ctx context.Context, w *gateway.InterruptWatch, deviceCode any, interval, expireIn float64, domain string) (*Registration, error) {
	out := provider.SetupOutput()
	deadline := pollMonotonic() + expireIn
	current, switched, polls := domain, false, 0
	for pollMonotonic() < deadline {
		res, err := postRegistration(ctx, accountsBaseURL(current), [][2]string{{"action", "poll"}, {"device_code", pyrepr.Str(deviceCode)}, {"tp", "ob_app"}})
		if err != nil {
			if !expectedOnboardError(err) {
				return nil, err
			}
			if err := pollSleep(w, interval); err != nil {
				return nil, err
			}
			continue
		}
		polls++
		if polls == 1 {
			fmt.Fprint(out, "  Fetching configuration results...")
		} else if polls%6 == 0 {
			fmt.Fprint(out, ".")
		}
		userInfo, err := asObject(orDefault(res.Vals["user_info"], pyjson.NewObject()))
		if err != nil {
			return nil, err
		}
		if userInfo.Vals["tenant_brand"] == "lark" && !switched {
			current, switched = "lark", true
		}
		if pyrepr.Truthy(res.Vals["client_id"]) && pyrepr.Truthy(res.Vals["client_secret"]) {
			fmt.Fprintln(out)
			return &Registration{AppID: res.Vals["client_id"], AppSecret: res.Vals["client_secret"], Domain: current,
				OpenID: userInfo.Vals["open_id"]}, nil
		}
		if e, _ := res.Vals["error"].(string); e == "access_denied" || e == "expired_token" {
			fmt.Fprintln(out)
			slog.Warn("[Feishu onboard] Registration " + e)
			return nil, nil
		}
		if err := pollSleep(w, interval); err != nil {
			return nil, err
		}
	}
	if polls > 0 {
		fmt.Fprintln(out)
	}
	slog.Warn(fmt.Sprintf("[Feishu onboard] Poll timed out after %ds", int(expireIn)))
	return nil, nil
}

// Prints the QR code of the URL inverted, false when it cannot render
func renderQR(url string) bool {
	code, err := pyqrcode.Make(url, pyqrcode.ErrorCorrectM, 4)
	if err != nil {
		return false
	}
	fmt.Fprint(provider.SetupOutput(), code.PrintASCII(true))
	return true
}

// Bot name and app scoped open id of /open-apis/bot/v3/info, nil when the answer carries an error code
func parseBotResponse(data *pyjson.Object) (map[string]any, error) {
	if !numberIs(data.Vals["code"], 0) {
		return nil, nil
	}
	bot := data.Vals["bot"]
	if !pyrepr.Truthy(bot) {
		nested, ok := data.Vals["data"]
		if !ok {
			nested = pyjson.NewObject()
		}
		nestedObj, err := asObject(nested)
		if err != nil {
			return nil, err
		}
		bot = nestedObj.Vals["bot"]
	}
	botObj, err := asObject(orDefault(bot, pyjson.NewObject()))
	if err != nil {
		return nil, err
	}
	return map[string]any{"bot_name": orDefault(botObj.Vals["app_name"], botObj.Vals["bot_name"]), "bot_open_id": botObj.Vals["open_id"]}, nil
}

// Whether a decoded JSON number equals n the way Python compares int and float
func numberIs(v any, n int64) bool {
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

// Probes the bot behind the credentials through the tenant access token and bot info APIs, nil on an expected failure
func probeBot(ctx context.Context, appID, appSecret any, domain string) (map[string]any, error) {
	base := onboardOpenBaseURL(domain)
	getJSON := func(path string, data []byte, extra map[string]string) (*pyjson.Object, error) {
		method := http.MethodGet
		var body io.Reader
		if data != nil {
			method, body = http.MethodPost, bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, base+path, body)
		if err != nil {
			return nil, err
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		raw, err := urlopenFn(ctx, req, onboardRequestTimeout)
		if err != nil {
			return nil, err
		}
		decoded, err := decodeJSONBody(raw)
		if err != nil {
			return nil, err
		}
		return asObject(decoded)
	}
	result, err := func() (map[string]any, error) {
		payload, err := pyjson.EncodeWTF8PythonASCII(pyjson.ObjectOf("app_id", appID, "app_secret", appSecret))
		if err != nil {
			return nil, err
		}
		tokenRes, err := getJSON("/open-apis/auth/v3/tenant_access_token/internal", payload, nil)
		if err != nil {
			return nil, err
		}
		token := tokenRes.Vals["tenant_access_token"]
		if !pyrepr.Truthy(token) {
			return nil, nil
		}
		botRes, err := getJSON("/open-apis/bot/v3/info", nil, map[string]string{"Authorization": "Bearer " + pyrepr.Str(token)})
		if err != nil {
			return nil, err
		}
		return parseBotResponse(botRes)
	}()
	if err != nil && expectedOnboardError(err) {
		slog.Debug("[Feishu onboard] HTTP probe failed: " + err.Error())
		return nil, nil
	}
	return result, err
}

// Step seams of the scan to create flow, the module functions its tests replace
var (
	initRegistrationFn  = initRegistration
	beginRegistrationFn = beginRegistration
	pollRegistrationFn  = pollRegistration
	renderQRFn          = renderQR
	probeBotFn          = probeBot
)

// Scan to create flow with the bot probed after, nil on an expected failure and errors otherwise
func QRRegister(ctx context.Context, initialDomain string, timeoutSeconds int) (*Registration, error) {
	result, err := qrRegisterInner(ctx, initialDomain, timeoutSeconds)
	var runtime *RuntimeError
	if err != nil && (errors.As(err, &runtime) || expectedOnboardError(err)) {
		slog.Warn("[Feishu onboard] Registration failed: " + err.Error())
		return nil, nil
	}
	return result, err
}

// Runs init, begin, poll and probe, raising on network and protocol errors
func qrRegisterInner(ctx context.Context, initialDomain string, timeoutSeconds int) (*Registration, error) {
	out := provider.SetupOutput()
	watch := gateway.WatchInterrupt()
	defer watch.Stop()
	fmt.Fprint(out, "  Connecting to Feishu / Lark...")
	if err := initRegistrationFn(ctx, initialDomain); err != nil {
		return nil, err
	}
	begin, err := beginRegistrationFn(ctx, initialDomain)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(out, " done.")
	fmt.Fprintln(out)
	if renderQRFn(begin.QRURL) {
		fmt.Fprintf(out, "\n  Scan the QR code above, or open this URL directly:\n  %s\n", begin.QRURL)
	} else {
		fmt.Fprintf(out, "  Open this URL in Feishu / Lark on your phone:\n\n  %s\n\n", begin.QRURL)
		fmt.Fprintf(out, "  Tip: from the Icebath environment, run: %s to display a scannable QR code here next time\n", pm.InstallHint("messaging"))
	}
	fmt.Fprintln(out)
	interval, err := seconds(begin.Interval)
	if err != nil {
		return nil, err
	}
	expire, err := seconds(begin.ExpireIn)
	if err != nil {
		return nil, err
	}
	result, err := pollRegistrationFn(ctx, watch, begin.DeviceCode, interval, math.Min(expire, float64(timeoutSeconds)), initialDomain)
	if err != nil || result == nil {
		return nil, err
	}
	info, err := probeBotFn(ctx, result.AppID, result.AppSecret, result.Domain)
	if err != nil {
		return nil, err
	}
	if info != nil {
		result.BotName, result.BotOpenID = info["bot_name"], info["bot_open_id"]
	}
	return result, nil
}

// Scan to create registration of the wizard, the qr_register name its tests replace
var qrRegisterFn = func(ctx context.Context) (*Registration, error) {
	return QRRegister(ctx, "feishu", defaultQRRegisterTimeoutS)
}

// Feishu or Lark wizard of icebath gateway setup, the scan to create flow or manual credentials, then the policies
func InteractiveSetup(ctx context.Context) error {
	cli, cfg, choice := gateway.WizardCLI, gateway.WizardConfig, gateway.WizardSetup.PromptChoice
	cli.PrintHeader("Feishu / Lark")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Feishu / Lark", "Reconfigure Feishu / Lark?", "FEISHU_APP_ID")
	if err != nil || declined {
		return err
	}
	method := choice("How would you like to set up Feishu / Lark?",
		[]string{"Scan QR code to create a new bot automatically (recommended)", "Enter existing App ID and App Secret manually"}, 0)
	var creds *Registration
	if method == 0 {
		creds, err = qrRegisterFn(ctx)
		switch {
		case errors.Is(err, gateway.ErrKeyboardInterrupt):
			cli.PrintWarning("Feishu / Lark setup cancelled.")
			return nil
		case err != nil:
			cli.PrintWarning("QR registration failed: " + err.Error())
			creds = nil
		}
		if creds == nil {
			cli.PrintInfo("QR setup did not complete. Continuing with manual input.")
		}
	}
	usedQR := creds != nil
	if creds == nil {
		cli.PrintInfo("Go to https://open.feishu.cn/ (or https://open.larksuite.com/ for Lark)")
		cli.PrintInfo("Create an app, enable the Bot capability, and copy the credentials.")
		appID := cli.Prompt("App ID", "", false)
		if appID == "" {
			cli.PrintWarning("Skipped — Feishu / Lark won't work without an App ID.")
			return nil
		}
		appSecret := cli.Prompt("App Secret", "", true)
		if appSecret == "" {
			cli.PrintWarning("Skipped — Feishu / Lark won't work without an App Secret.")
			return nil
		}
		domain := "feishu"
		if choice("Domain", []string{"feishu (China)", "lark (International)"}, 0) == 1 {
			domain = "lark"
		}
		var botName any
		info, err := probeBotFn(ctx, appID, appSecret, domain)
		switch {
		case err != nil:
			cli.PrintWarning("Credential verification skipped: " + err.Error())
		case info != nil:
			botName = info["bot_name"]
			cli.PrintSuccess("Credentials verified — bot: " + pyrepr.Str(orDefault(botName, "unnamed")))
		default:
			cli.PrintWarning("Could not verify bot connection. Credentials saved anyway.")
		}
		creds = &Registration{AppID: appID, AppSecret: appSecret, Domain: domain, BotName: botName}
	}
	domain := creds.Domain
	for _, kv := range [][2]string{{"FEISHU_APP_ID", pyrepr.Str(creds.AppID)}, {"FEISHU_APP_SECRET", pyrepr.Str(creds.AppSecret)}, {"FEISHU_DOMAIN", domain}} {
		if err := cfg.SaveEnvValue(ctx, kv[0], kv[1]); err != nil {
			return err
		}
	}
	mode := "websocket"
	if !usedQR && choice("Connection mode", []string{"WebSocket (recommended — no public URL needed)", "Webhook (requires a reachable HTTP endpoint)"}, 0) == 1 {
		mode = "webhook"
		cli.PrintInfo("Webhook defaults: 127.0.0.1:8765/feishu/webhook")
		cli.PrintInfo("Override with FEISHU_WEBHOOK_HOST / FEISHU_WEBHOOK_PORT / FEISHU_WEBHOOK_PATH")
		cli.PrintInfo("For signature verification, set FEISHU_ENCRYPT_KEY and FEISHU_VERIFICATION_TOKEN")
	}
	if err := cfg.SaveEnvValue(ctx, "FEISHU_CONNECTION_MODE", mode); err != nil {
		return err
	}
	if pyrepr.Truthy(creds.BotName) {
		cli.PrintSuccess("Bot created: " + pyrepr.Str(creds.BotName))
	}
	access := choice("How should direct messages be authorized?",
		[]string{"Use DM pairing approval (recommended)", "Allow all direct messages", "Only allow listed user IDs"}, 0)
	allowAll := "false"
	if access == 1 {
		allowAll = "true"
	}
	if err := cfg.SaveEnvValue(ctx, "FEISHU_ALLOW_ALL_USERS", allowAll); err != nil {
		return err
	}
	if access == 2 {
		def := ""
		if pyrepr.Truthy(creds.OpenID) {
			def = pyrepr.Str(creds.OpenID)
		}
		allowlist := strings.ReplaceAll(cli.Prompt("Allowed user IDs (comma-separated)", def, false), " ", "")
		if err := cfg.SaveEnvValue(ctx, "FEISHU_ALLOWED_USERS", allowlist); err != nil {
			return err
		}
		cli.PrintSuccess("Allowlist saved.")
	} else {
		if err := cfg.SaveEnvValue(ctx, "FEISHU_ALLOWED_USERS", ""); err != nil {
			return err
		}
		if access == 0 {
			cli.PrintSuccess("DM pairing enabled.")
			cli.PrintInfo("Unknown users can request access; approve with `icebath pairing approve`.")
		} else {
			cli.PrintWarning("Open DM access enabled for Feishu / Lark.")
		}
	}
	group := choice("How should group chats be handled?",
		[]string{"Respond only when @mentioned in groups (recommended)", "Disable group chats"}, 0)
	policy, note := "disabled", "Group chats disabled."
	if group == 0 {
		policy, note = "open", "Group chats enabled (bot must be @mentioned)."
	}
	if err := cfg.SaveEnvValue(ctx, "FEISHU_GROUP_POLICY", policy); err != nil {
		return err
	}
	cli.PrintInfo(note)
	cli.PrintInfo("Leave blank to clear a previously saved home channel (cron / notifications).")
	if home := pystr.Strip(cli.Prompt("Home chat ID (optional, for cron/notifications)", "", false)); home != "" {
		if err := cfg.SaveEnvValue(ctx, "FEISHU_HOME_CHANNEL", home); err != nil {
			return err
		}
		cli.PrintSuccess("Home channel set to " + home)
	} else if removed, err := cfg.RemoveEnvValue(ctx, "FEISHU_HOME_CHANNEL"); err != nil {
		return err
	} else if removed {
		cli.PrintInfo("Home channel cleared.")
	}
	cli.PrintSuccess("🪽 Feishu / Lark configured!")
	cli.PrintInfo("App ID: " + pyrepr.Str(creds.AppID))
	cli.PrintInfo("Domain: " + domain)
	if pyrepr.Truthy(creds.BotName) {
		cli.PrintInfo("Bot: " + pyrepr.Str(creds.BotName))
	}
	return nil
}
