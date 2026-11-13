package sms

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Twilio REST base the Messages endpoint hangs off
var twilioAPIBase = "https://api.twilio.com/2010-04-01/Accounts"

const (
	maxSMSLength              = 1600
	defaultWebhookPort        = 8080
	defaultWebhookHost        = "127.0.0.1"
	twilioWebhookMaxBodyBytes = 65_536
	emptyTwiML                = `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`
	webhookRoute              = "/webhooks/twilio"
)

// Empty TwiML reply, replies going out through the REST API and never inline
func twimlResponse(status int) *gateway.WebResponse {
	return gateway.WebTextResponse(status, emptyTwiML, "application/xml", nil)
}

// HTTP Basic auth header value for Twilio
func basicAuth(accountSID, authToken string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(accountSID+":"+authToken))
}

// Messages.json URL and auth headers of the account
func messagesEndpoint(accountSID, authToken string) (string, map[string]string) {
	return twilioAPIBase + "/" + accountSID + "/Messages.json", map[string]string{"Authorization": basicAuth(accountSID, authToken)}
}

// Messages.json form body with From, To and Body in that order
func twilioForm(fromNumber, toNumber, body string) string {
	return pyurl.URLEncode([][2]string{{"From", fromNumber}, {"To", toNumber}, {"Body", body}})
}

// HTTP client with the 30 second total timeout of the Twilio session
func newSession(transport http.RoundTripper) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}

// Transport honouring proxy env vars only when the gateway trusts the environment
func trustEnvTransport(ctx context.Context) http.RoundTripper {
	return gateway.TrustEnvHTTPClient(ctx, 0).Transport
}

// Decoded JSON body of a Twilio answer, the aiohttp ContentTypeError for a non JSON content type
func twilioJSON(resp *http.Response, rawURL string) (map[string]any, error) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "application/json" {
		if mt == "" {
			mt = "application/octet-stream"
		}
		return nil, fmt.Errorf("%d, message='Attempt to decode JSON with unexpected mimetype: %s', url='%s'", resp.StatusCode, mt, rawURL)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// Twilio error message of a body, its str form when it carries no message
func twilioErrorMessage(body map[string]any) string {
	if msg, ok := body["message"]; ok {
		return pyrepr.Str(msg)
	}
	return pyrepr.Str(body)
}

// Scoped read of the Twilio account SID and auth token both being set
func checkSMSRequirements(ctx context.Context) bool {
	sid, err := gateway.GetScopedSecret(ctx, "TWILIO_ACCOUNT_SID", "", false)
	if err != nil || sid == "" {
		return false
	}
	token, err := gateway.GetScopedSecret(ctx, "TWILIO_AUTH_TOKEN", "", false)
	return err == nil && pystr.Strip(token) != ""
}

// Twilio SMS adapter, one session per inbound number with replies sent from TWILIO_PHONE_NUMBER
type SmsAdapter struct {
	*gateway.BasePlatformAdapter

	accountSID  string
	authToken   string
	fromNumber  string
	webhookPort int
	webhookHost string
	webhookURL  string

	mu          sync.Mutex
	runner      *gateway.WebAppRunner
	httpSession *http.Client
}

// Class attributes the SMS adapter declares over the base defaults
func smsAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.ServesProfilePrefix = true
	traits.MaxMessageLength = maxSMSLength
	traits.SplitsLongMessages = true
	return traits
}

// Adapter reading the Twilio credentials, from number and webhook settings from the profile scoped env
func NewSmsAdapter(ctx context.Context, config *gateway.PlatformConfig) (*SmsAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &SmsAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformSMS, a)
	a.Traits = smsAdapterTraits()
	read := func(name, def string) (string, error) { return gateway.GetScopedSecret(ctx, name, def, false) }
	var err error
	if a.accountSID, err = read("TWILIO_ACCOUNT_SID", ""); err != nil {
		return nil, err
	}
	token, err := read("TWILIO_AUTH_TOKEN", "")
	if err != nil {
		return nil, err
	}
	a.authToken = pystr.Strip(token)
	if a.fromNumber, err = read("TWILIO_PHONE_NUMBER", ""); err != nil {
		return nil, err
	}
	port, err := read("SMS_WEBHOOK_PORT", fmt.Sprint(defaultWebhookPort))
	if err != nil {
		return nil, err
	}
	n, err := pyint.Int64(port)
	if err != nil {
		return nil, err
	}
	a.webhookPort = int(n)
	if a.webhookHost, err = read("SMS_WEBHOOK_HOST", defaultWebhookHost); err != nil {
		return nil, err
	}
	webhookURL, err := read("SMS_WEBHOOK_URL", "")
	if err != nil {
		return nil, err
	}
	a.webhookURL = pystr.Strip(webhookURL)
	return a, nil
}

// Refuses to start without a from number or signature URL, then binds or publishes the Twilio webhook
func (a *SmsAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	insecure, err := gateway.GetScopedSecret(ctx, "SMS_INSECURE_NO_SIGNATURE", "", false)
	if err != nil {
		return false, err
	}
	insecureNoSig := pystr.Lower(insecure) == "true"
	var code, msg string
	if a.fromNumber == "" {
		code, msg = "sms_missing_phone_number", "[sms] TWILIO_PHONE_NUMBER not set — cannot send replies"
	} else if a.webhookURL == "" && !insecureNoSig {
		code, msg = "sms_missing_webhook_url", "[sms] Refusing to start: SMS_WEBHOOK_URL is required for Twilio "+
			"signature validation. Set it to the public URL configured in your "+
			"Twilio console (e.g. https://example.com/webhooks/twilio). "+
			"For local development without validation, set "+
			"SMS_INSECURE_NO_SIGNATURE=true (NOT recommended for production)."
	}
	if code != "" {
		slog.Error(msg)
		a.SetFatalError(ctx, code, msg, false)
		return false, nil
	}
	if insecureNoSig && a.webhookURL == "" {
		slog.Warn(fmt.Sprintf("[sms] SMS_INSECURE_NO_SIGNATURE=true — Twilio signature validation "+
			"is DISABLED. Any client that can reach port %d can inject messages. "+
			"Do NOT use this in production.", a.webhookPort))
	}
	app := gateway.NewWebApplication(twilioWebhookMaxBodyBytes)
	if err := app.AddPost(webhookRoute, a.handleWebhook); err != nil {
		return false, err
	}
	if err := app.AddGet("/health", func(context.Context, *gateway.WebRequest) (*gateway.WebResponse, error) {
		return gateway.WebTextResponse(200, "ok", "", nil), nil
	}); err != nil {
		return false, err
	}
	host := a.webhookHost
	runner, err := gateway.BindListener(ctx, a, app, &host, a.webhookPort, webhookRoute, gateway.ListenOptions{})
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	a.runner = runner
	a.httpSession = newSession(trustEnvTransport(ctx))
	a.mu.Unlock()
	a.SetRunning(true)
	if runner != nil {
		slog.Info(fmt.Sprintf("[sms] Twilio webhook server listening on %s:%d, from: %s", a.webhookHost, a.webhookPort,
			gateway.RedactPhone(a.fromNumber)))
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Closes the HTTP session, stops the listener and clears the running flag
func (a *SmsAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	session, runner := a.httpSession, a.runner
	a.httpSession, a.runner = nil, nil
	a.mu.Unlock()
	if session != nil {
		session.CloseIdleConnections()
	}
	if runner != nil {
		if err := runner.Cleanup(ctx); err != nil {
			return err
		}
	}
	a.SetRunning(false)
	slog.Info("[sms] Disconnected")
	return nil
}

// Markdown stripped text split at the SMS cap and posted chunk by chunk to Messages.json
func (a *SmsAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	endpoint, headers := messagesEndpoint(a.accountSID, a.authToken)
	a.mu.Lock()
	session := a.httpSession
	a.mu.Unlock()
	if session == nil {
		session = newSession(trustEnvTransport(ctx))
		defer session.CloseIdleConnections()
	}
	sendOne := func(ctx context.Context, chunk string) (*gateway.SendResult, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(twilioForm(a.fromNumber, chatID, chunk)))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			var resp *http.Response
			if resp, err = session.Do(req); err == nil {
				defer resp.Body.Close()
				var body map[string]any
				if body, err = twilioJSON(resp, endpoint); err == nil {
					if resp.StatusCode >= 400 {
						errorMsg := twilioErrorMessage(body)
						slog.Error(fmt.Sprintf("[sms] send failed to %s: %d %s", gateway.RedactPhone(chatID), resp.StatusCode, errorMsg))
						return &gateway.SendResult{Success: false, Error: fmt.Sprintf("Twilio %d: %s", resp.StatusCode, errorMsg)}, nil
					}
					sid, ok := body["sid"]
					if !ok {
						sid = ""
					}
					return &gateway.SendResult{Success: true, MessageID: sid}, nil
				}
			}
		}
		slog.Error(fmt.Sprintf("[sms] send error to %s: %v", gateway.RedactPhone(chatID), err))
		return &gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	chunks := gateway.TruncateMessage(a.FormatMessage(content), maxSMSLength, a.MessageLenFn())
	result, err := gateway.SendChunks(ctx, chunks, sendOne)
	if err != nil || result == nil {
		return gateway.SendResult{}, err
	}
	return *result, nil
}

// Chat info of a phone number chat
func (a *SmsAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "dm"}, nil
}

// Markdown stripped since SMS renders it as literal characters
func (a *SmsAdapter) FormatMessage(content string) string {
	return gateway.StripMarkdown(content, false)
}

// X-Twilio-Signature check against the URL and its default port toggled variant
func (a *SmsAdapter) validateTwilioSignature(rawURL string, postParams map[string]string, signature string) (bool, error) {
	if a.checkSignature(rawURL, postParams, signature) {
		return true, nil
	}
	variant, err := portVariantURL(rawURL)
	if err != nil {
		return false, err
	}
	return variant != "" && a.checkSignature(variant, postParams, signature), nil
}

// Base64 HMAC-SHA1 of the URL and sorted params under the auth token, failing closed without a token
func (a *SmsAdapter) checkSignature(rawURL string, postParams map[string]string, signature string) bool {
	if a.authToken == "" {
		return false
	}
	keys := make([]string, 0, len(postParams))
	for k := range postParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var data strings.Builder
	data.WriteString(rawURL)
	for _, k := range keys {
		data.WriteString(k + postParams[k])
	}
	mac := hmac.New(sha1.New, []byte(a.authToken))
	mac.Write([]byte(data.String()))
	computed := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(computed), []byte(signature))
}

// URL with the scheme default port added or stripped, empty for a non default port or unknown scheme
func portVariantURL(rawURL string) (string, error) {
	parsed, err := pyurl.Parse(rawURL)
	if err != nil {
		return "", err
	}
	defaultPort, ok := map[string]int{"https": 443, "http": 80}[parsed.Scheme]
	if !ok {
		return "", nil
	}
	port, hasPort, err := parsed.Port()
	if err != nil {
		return "", err
	}
	switch {
	case hasPort && port == defaultPort:
		parsed.Netloc = parsed.Hostname()
	case !hasPort:
		parsed.Netloc = fmt.Sprintf("%s:%d", parsed.Hostname(), defaultPort)
	default:
		return "", nil
	}
	return pyurl.Unparse(parsed), nil
}

// Validates and parses a Twilio form webhook and hands the text to the gateway without blocking the answer
func (a *SmsAdapter) handleWebhook(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	form, status := a.readForm(request)
	if status != 0 {
		return twimlResponse(status), nil
	}
	if a.webhookURL != "" {
		sig := request.Header.Get("X-Twilio-Signature")
		flat := map[string]string{}
		for k, v := range form {
			if len(v) > 0 {
				flat[k] = v[0]
			}
		}
		rejected := ""
		if sig == "" {
			rejected = "missing X-Twilio-Signature header"
		} else {
			valid, err := a.validateTwilioSignature(a.webhookURL, flat, sig)
			if err != nil {
				return nil, err
			}
			if !valid {
				rejected = "invalid Twilio signature"
			}
		}
		if rejected != "" {
			slog.Warn("[sms] Rejected: " + rejected)
			return twimlResponse(403), nil
		}
	}
	field := func(key string) string {
		if v := form[key]; len(v) > 0 {
			return pystr.Strip(v[0])
		}
		return ""
	}
	fromNumber, toNumber, text, messageSID := field("From"), field("To"), field("Body"), field("MessageSid")
	if fromNumber == "" || text == "" {
		return twimlResponse(200), nil
	}
	if fromNumber == a.fromNumber {
		slog.Debug("[sms] ignoring echo from own number " + gateway.RedactPhone(fromNumber))
		return twimlResponse(200), nil
	}
	slog.Info(fmt.Sprintf("[sms] inbound from %s -> %s: %s", gateway.RedactPhone(fromNumber), gateway.RedactPhone(toNumber), pystr.Slice(text, 0, 80)))
	source := a.BuildSource(ctx, fromNumber, gateway.SourceFields{ChatName: &fromNumber, ChatType: "dm", UserID: &fromNumber,
		UserName: &fromNumber, MessageID: &messageSID})
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source = gateway.MessageTypeText, source
	raw := map[string]any{}
	for k, v := range form {
		raw[k] = v
	}
	event.RawMessage = raw
	event.MessageID = &messageSID
	task := gateway.SpawnAdapterTask(context.WithoutCancel(ctx), func(tctx context.Context) error { return a.HandleMessage(tctx, event) })
	a.TrackBackgroundTask(task)
	return twimlResponse(200), nil
}

// Form of a capped body, a 413 for an oversized body and a 400 for one that does not read or decode
func (a *SmsAdapter) readForm(request *gateway.WebRequest) (map[string][]string, int) {
	if request.ContentLength != nil && *request.ContentLength > twilioWebhookMaxBodyBytes {
		return nil, 413
	}
	raw, err := request.Read()
	if err == nil && len(raw) > twilioWebhookMaxBodyBytes {
		return nil, 413
	}
	var text string
	if err == nil {
		text, err = pystr.DecodeUTF8(raw)
	}
	if err != nil {
		slog.Error(fmt.Sprintf("[sms] webhook parse error: %v", err))
		return nil, 400
	}
	return pyurl.ParseQS(text, true), 0
}
