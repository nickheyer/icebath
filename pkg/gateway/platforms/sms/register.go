package sms

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Standalone send markdown rules, looser than the shared stripper and kept in hermes order
var smsMarkdownSubs = []struct {
	re   *regexp2.Regexp
	repl string
}{
	{pyre.MustCompile(`(?s)\*\*(.+?)\*\*`), "$1"}, {pyre.MustCompile(`(?s)\*(.+?)\*`), "$1"},
	{pyre.MustCompile(`(?s)__(.+?)__`), "$1"}, {pyre.MustCompile(`(?s)_(.+?)_`), "$1"},
	{pyre.MustCompile("```[a-z]*\\n?"), ""}, {pyre.MustCompile("`(.+?)`"), "$1"},
	{pyre.MustCompile(`(?m)^#{1,6}\s+`), ""}, {pyre.MustCompile(`\[([^\]]+)\]\([^\)]+\)`), "$1"},
	{pyre.MustCompile(`\n{3,}`), "\n\n"},
}

// Markdown stripped for SMS and the result stripped of outer whitespace
func stripMarkdownForSMS(message string) string {
	for _, sub := range smsMarkdownSubs {
		out, err := sub.re.Replace(message, sub.repl, -1, -1)
		if err != nil {
			panic(err)
		}
		message = out
	}
	return pystr.Strip(message)
}

// Out of process SMS delivery through the Twilio REST API
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	var rawToken any
	if pconfig != nil {
		rawToken = pconfig.APIKey
	}
	if !pyrepr.Truthy(rawToken) {
		token, err := gateway.GetScopedSecret(ctx, "TWILIO_AUTH_TOKEN", "", false)
		if err != nil {
			return nil, err
		}
		rawToken = token
	}
	authToken := ""
	if pyrepr.Truthy(rawToken) {
		authToken = pystr.Strip(pyrepr.Str(rawToken))
	}
	accountSID, err := gateway.GetScopedSecret(ctx, "TWILIO_ACCOUNT_SID", "", false)
	if err != nil {
		return nil, err
	}
	fromNumber, err := gateway.GetScopedSecret(ctx, "TWILIO_PHONE_NUMBER", "", false)
	if err != nil {
		return nil, err
	}
	if accountSID == "" || authToken == "" || fromNumber == "" {
		return gateway.SendError("SMS not configured (TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_PHONE_NUMBER required)"), nil
	}
	message = stripMarkdownForSMS(message)
	result, err := standalonePost(ctx, accountSID, authToken, fromNumber, chatID, message)
	if err != nil {
		return gateway.SendError(fmt.Sprintf("SMS send failed: %v", err)), nil
	}
	return result, nil
}

// One Messages.json post over the resolved proxy, a send error for a Twilio error answer
func standalonePost(ctx context.Context, accountSID, authToken, fromNumber, chatID, message string) (map[string]any, error) {
	proxyURL, err := gateway.ResolveProxyURL(ctx, "", nil, "")
	if err != nil {
		return nil, err
	}
	sessionKw, requestKw, err := gateway.ProxyKwargsForAiohttp(proxyURL)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if sessionKw.Connector != nil {
		transport = sessionKw.Connector.Transport()
	} else if requestKw.Proxy != "" {
		proxy, err := url.Parse(requestKw.Proxy)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	session := newSession(transport)
	defer session.CloseIdleConnections()
	endpoint, headers := messagesEndpoint(accountSID, authToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(twilioForm(fromNumber, chatID, message)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := session.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := twilioJSON(resp, endpoint)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return gateway.SendError(fmt.Sprintf("Twilio API error (%d): %s", resp.StatusCode, twilioErrorMessage(body))), nil
	}
	sid, ok := body["sid"]
	if !ok {
		sid = ""
	}
	return map[string]any{"success": true, "platform": "sms", "chat_id": chatID, "message_id": sid}, nil
}

// Model facing description of SMS limits, kept byte stable for the cached prompt
const platformHint = "You are communicating via SMS. Keep responses concise and use plain text only — no markdown, no " +
	"formatting. SMS messages are limited to ~1600 characters, so be brief and direct."

// SMS platform plugin compiled into the binary under the bundled platforms/sms key
type smsPlatformPlugin struct{}

func (smsPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "sms-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/sms",
		Author: "NousResearch",
		Description: "SMS gateway adapter for Icebath Agent via Twilio. Sends and receives SMS through the Twilio REST API + " +
			"inbound webhook, relaying texts between phone numbers and the Icebath agent. Markdown is stripped to plain text.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "TWILIO_ACCOUNT_SID", Description: "Twilio Account SID", URL: "https://www.twilio.com/"},
			{Name: "TWILIO_AUTH_TOKEN", Description: "Twilio Auth Token", Secret: true},
			{Name: "TWILIO_PHONE_NUMBER", Description: "Twilio phone number (SMS-capable, E.164 format)"}}}
}

// Adapter class the gateway migration code reads the profile prefix trait from
var smsAdapterClass = &gateway.AdapterClass{Name: "SmsAdapter", Traits: smsAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewSmsAdapter(ctx, config)
	}}

// Registers the SMS platform entry with its factory, gates, env bridge, standalone sender and hint
func (smsPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"standalone_send":        &gateway.StandaloneSendPolicy{TextDirect: true},
		"dashboard_description":  "Send and receive text messages via Twilio.",
		"docs_url":               "https://www.twilio.com/console",
		"dashboard_env_vars":     []string{"TWILIO_ACCOUNT_SID", "TWILIO_AUTH_TOKEN"},
		"dashboard_required_env": []string{"TWILIO_ACCOUNT_SID", "TWILIO_AUTH_TOKEN"},
		"scoped_check_fn":        checkSMSRequirements,
		"is_connected":           gateway.EnvIsConnected("TWILIO_ACCOUNT_SID"),
		"cron_deliver_env_var":   "SMS_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     maxSMSLength,
		"pii_safe":               true,
		"emoji":                  "📱",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "minimal",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "sms", Label: "SMS (Twilio)",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewSmsAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkSMSRequirements(context.Background()) },
		RequiredEnv: []string{"TWILIO_ACCOUNT_SID", "TWILIO_AUTH_TOKEN", "TWILIO_PHONE_NUMBER"},
		Extra:       extra,
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(smsAdapterClass)
	plugins.RegisterBundled(smsPlatformPlugin{})
}
