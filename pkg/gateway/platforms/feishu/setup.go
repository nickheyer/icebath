package feishu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyqrcode"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	qrutil "github.com/nickheyer/icebath/pkg/utils/qr"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
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

// Text of a failed request for a log line or a send result: the kind of error and the proxy's host when the request went through a proxy whose URL
// holds a secret the raw text could repeat, the raw text otherwise
func proxySafeErrorText(err error, openURLs ...string) string {
	if shown := gateway.DescribeProxyError(err, gateway.EnvProxyForRedaction(openURLs...)); shown != "" {
		return shown
	}
	return err.Error()
}

// Text of a failed request to the open platform of the adapter's domain
func (a *FeishuAdapter) errorText(err error) string {
	return proxySafeErrorText(err, onboardOpenBaseURL(a.settings.DomainName))
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
	resp, err := (&http.Client{Timeout: timeout, Transport: &http.Transport{DialContext: netpref.DialContext((&net.Dialer{}).DialContext), Proxy: httputil.Proxy}}).Do(req.WithContext(ctx))
	if err != nil {
		return nil, &URLError{err.Error()}
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
	text, err := textutil.DecodeUTF8(body)
	if err != nil {
		return nil, err
	}
	return jsonutil.Decode(text)
}

// Whether an error is the URLError, OSError or JSONDecodeError class onboarding treats as an expected failure
func expectedOnboardError(err error) bool {
	var httpErr *HTTPError
	var urlErr *URLError
	var decode *jsonutil.SyntaxError
	return errors.As(err, &httpErr) || errors.As(err, &urlErr) || errors.As(err, &decode)
}

// POSTs form data to the registration endpoint, parsing JSON even from a 4xx answer since poll pending is a 400
func postRegistration(ctx context.Context, baseURL string, body [][2]string) (*jsonutil.Object, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+registrationPath, strings.NewReader(urlutil.EncodeQuery(body)))
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
func asObject(v any) (*jsonutil.Object, error) {
	o, ok := v.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("expected a mapping, got %s", errchain.Kind(v))
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
	methods := res.Value("supported_auth_methods")
	if !anyutil.IsTrue(methods) {
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
	case *jsonutil.Object:
		_, supported = m.Get("client_secret")
	default:
		return fmt.Errorf("the Feishu registration methods are %s, expected a list", errchain.Kind(methods))
	}
	if !supported {
		return &RuntimeError{"Feishu / Lark registration environment does not support client_secret auth. " +
			"Supported: " + anyutil.String(methods)}
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
	if anyutil.IsTrue(v) {
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
	deviceCode := res.Value("device_code")
	if !anyutil.IsTrue(deviceCode) {
		return nil, &RuntimeError{"Feishu / Lark registration did not return a device_code"}
	}
	rawURL, ok := res.Get("verification_uri_complete")
	if !ok {
		rawURL = ""
	}
	qrURL, isStr := rawURL.(string)
	if !isStr {
		return nil, fmt.Errorf("the Feishu registration URL is %s, expected text", errchain.Kind(rawURL))
	}
	if strings.Contains(qrURL, "?") {
		qrURL += "&"
	} else {
		qrURL += "?"
	}
	qrURL += "from=icebath&tp=icebath"
	userCode, ok := res.Get("user_code")
	if !ok {
		userCode = ""
	}
	return &beginResult{DeviceCode: deviceCode, QRURL: qrURL, UserCode: userCode,
		Interval: orDefault(res.Value("interval"), int64(5)), ExpireIn: orDefault(res.Value("expire_in"), int64(600))}, nil
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
	return 0, fmt.Errorf("expected a whole number of seconds, got %s", errchain.Kind(v))
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
		res, err := postRegistration(ctx, accountsBaseURL(current), [][2]string{{"action", "poll"}, {"device_code", anyutil.String(deviceCode)}, {"tp", "ob_app"}})
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
		userInfo, err := asObject(orDefault(res.Value("user_info"), jsonutil.NewObject()))
		if err != nil {
			return nil, err
		}
		if userInfo.Value("tenant_brand") == "lark" && !switched {
			current, switched = "lark", true
		}
		if anyutil.IsTrue(res.Value("client_id")) && anyutil.IsTrue(res.Value("client_secret")) {
			fmt.Fprintln(out)
			return &Registration{AppID: res.Value("client_id"), AppSecret: res.Value("client_secret"), Domain: current,
				OpenID: userInfo.Value("open_id")}, nil
		}
		if e, _ := res.Value("error").(string); e == "access_denied" || e == "expired_token" {
			fmt.Fprintln(out)
			pkgLog().Warn("[Feishu onboard] Registration " + e)
			return nil, nil
		}
		if err := pollSleep(w, interval); err != nil {
			return nil, err
		}
	}
	if polls > 0 {
		fmt.Fprintln(out)
	}
	pkgLog().Warn(fmt.Sprintf("[Feishu onboard] Poll timed out after %ds", int(expireIn)))
	return nil, nil
}

// Prints the QR code of the URL inverted, false when it cannot render
func renderQR(url string) bool {
	code, err := qrutil.Encode(url, qrutil.Medium)
	if err != nil {
		return false
	}
	fmt.Fprint(provider.SetupOutput(), code.HalfBlocks(4, true))
	return true
}

// Bot name and app scoped open id of /open-apis/bot/v3/info, nil when the answer carries an error code
func parseBotResponse(data *jsonutil.Object) (map[string]any, error) {
	if !numberIs(data.Value("code"), 0) {
		return nil, nil
	}
	bot := data.Value("bot")
	if !anyutil.IsTrue(bot) {
		nested, ok := data.Get("data")
		if !ok {
			nested = jsonutil.NewObject()
		}
		nestedObj, err := asObject(nested)
		if err != nil {
			return nil, err
		}
		bot = nestedObj.Value("bot")
	}
	botObj, err := asObject(orDefault(bot, jsonutil.NewObject()))
	if err != nil {
		return nil, err
	}
	return map[string]any{"bot_name": orDefault(botObj.Value("app_name"), botObj.Value("bot_name")), "bot_open_id": botObj.Value("open_id")}, nil
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

// Bot name and open id behind the credentials from the bot info API through the lark SDK, nil on failure
func probeBot(ctx context.Context, appID, appSecret any, domain string) (map[string]any, error) {
	return probeBotSDK(ctx, anyutil.String(appID), anyutil.String(appSecret), domain), nil
}

// Lark client for onboarding credentials and a feishu or lark domain name
func buildOnboardClient(appID, appSecret, domain string) larkAPI {
	return buildLarkClient(appID, appSecret, sdkDomain(domain))
}

// Bot info probe over the SDK tenant token request, nil when the request or its payload fails
func probeBotSDK(ctx context.Context, appID, appSecret, domain string) map[string]any {
	fail := func(err error) map[string]any {
		pkgLog().Debug("[Feishu onboard] SDK probe failed: " + err.Error())
		return nil
	}
	resp, err := buildOnboardClient(appID, appSecret, domain).Do(ctx, tenantGetRequest("/open-apis/bot/v3/info", nil))
	if err != nil {
		return fail(err)
	}
	if resp == nil || resp.RawBody == nil {
		return nil
	}
	decoded, err := jsonutil.Decode(string(resp.RawBody))
	if err != nil {
		return fail(err)
	}
	payload, err := asObject(decoded)
	if err != nil {
		return fail(err)
	}
	parsed, err := parseBotResponse(payload)
	if err != nil {
		return fail(err)
	}
	return parsed
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
		pkgLog().Warn("[Feishu onboard] Registration failed: " + err.Error())
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
			cli.PrintSuccess("Credentials verified — bot: " + anyutil.String(orDefault(botName, "unnamed")))
		default:
			cli.PrintWarning("Could not verify bot connection. Credentials saved anyway.")
		}
		creds = &Registration{AppID: appID, AppSecret: appSecret, Domain: domain, BotName: botName}
	}
	domain := creds.Domain
	for _, kv := range [][2]string{{"FEISHU_APP_ID", anyutil.String(creds.AppID)}, {"FEISHU_APP_SECRET", anyutil.String(creds.AppSecret)}} {
		if err := cfg.SaveEnvValue(ctx, kv[0], kv[1]); err != nil {
			return err
		}
	}
	if domain != "" {
		if _, err := settings.Set(gateway.SettingsContext(ctx), settings.FeishuDomain.Key, domain, settings.Global); err != nil {
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
	if _, err := settings.Set(gateway.SettingsContext(ctx), settings.FeishuConnectionMode.Key, mode, settings.Global); err != nil {
		return err
	}
	if anyutil.IsTrue(creds.BotName) {
		cli.PrintSuccess("Bot created: " + anyutil.String(creds.BotName))
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
		if anyutil.IsTrue(creds.OpenID) {
			def = anyutil.String(creds.OpenID)
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
	if home := strings.TrimSpace(cli.Prompt("Home chat ID (optional, for cron/notifications)", "", false)); home != "" {
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
	cli.PrintInfo("App ID: " + anyutil.String(creds.AppID))
	cli.PrintInfo("Domain: " + domain)
	if anyutil.IsTrue(creds.BotName) {
		cli.PrintInfo("Bot: " + anyutil.String(creds.BotName))
	}
	return nil
}
