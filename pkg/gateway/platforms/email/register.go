package email

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyemail"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysmtplib"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// One shot email delivery over SMTP without a live adapter
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	var extra map[string]any
	if pconfig != nil {
		extra = pconfig.Extra
	}
	orSecret := func(key, env string) (any, error) {
		if v := extra[key]; pyrepr.Truthy(v) {
			return v, nil
		}
		return secret(ctx, env)
	}
	address, err := orSecret("address", "EMAIL_ADDRESS")
	if err != nil {
		return nil, err
	}
	password, err := secret(ctx, "EMAIL_PASSWORD")
	if err != nil {
		return nil, err
	}
	smtpHost, err := orSecret("smtp_host", "EMAIL_SMTP_HOST")
	if err != nil {
		return nil, err
	}
	smtpPort, err := esecretInt(ctx, "EMAIL_SMTP_PORT", 587)
	if err != nil {
		return nil, err
	}
	securityEnv, err := secret(ctx, "EMAIL_SMTP_SECURITY")
	if err != nil {
		return nil, err
	}
	var securityValue any = securityEnv
	if securityEnv == "" {
		securityValue = extra["smtp_security"]
	}
	securityDefault := "starttls"
	if smtpPort == 465 {
		securityDefault = "tls"
	}
	smtpSecurity := normalizeSecurity(securityValue, securityDefault)
	smtpTLSVerify, err := esecretBool(ctx, "EMAIL_SMTP_TLS_VERIFY", isTruthyValue(extra["smtp_tls_verify"], true))
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(address) || password == "" || !pyrepr.Truthy(smtpHost) {
		return gateway.SendError("Email not configured (EMAIL_ADDRESS, EMAIL_PASSWORD, EMAIL_SMTP_HOST required)"), nil
	}
	sendErr := func() error {
		host, from := pyrepr.Str(smtpHost), pyrepr.Str(address)
		msg := pyemail.NewTextUTF8(message)
		msg.Set("From", from)
		msg.Set("To", chatID)
		msg.Set("Subject", i18n.T(ctx, "platform.email.standalone_subject", "", nil))
		msg.Set("Date", pyemail.FormatDateLocal())
		server, err := openSMTP(ctx, host, smtpPort, smtpSecurity, tlsContext(smtpTLSVerify, host), pysmtplib.DialDefault, time.Duration(0))
		if err != nil {
			return err
		}
		if err := server.Login(from, password); err != nil {
			return err
		}
		if err := server.SendMessage(msg); err != nil {
			return err
		}
		return server.Quit()
	}()
	if sendErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return gateway.SendError("Email send failed: " + pyrepr.ExceptionMessage(sendErr)), nil
	}
	return map[string]any{"success": true, "platform": "email", "chat_id": chatID}, nil
}

// Whether an address is configured in the config extra or through EMAIL_ADDRESS
func isConnected(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	if config != nil && pyrepr.Truthy(config.Extra["address"]) {
		return true, nil
	}
	return gateway.EnvIsConnected("EMAIL_ADDRESS")(ctx, config)
}

// Model facing description of email formatting and attachments, kept byte stable for the cached prompt
const platformHint = "You are communicating via email. Write clear, well-structured responses suitable for email. Use " +
	"plain text formatting (no markdown). Keep responses concise but complete. You can send file " +
	"attachments — include MEDIA:/absolute/path/to/file in your response. The subject line is preserved " +
	"for threading. Do not include greetings or sign-offs unless contextually appropriate."

// Shape of an email address accepted as an explicit send target
const sendTargetPattern = `^\s*[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\s*$`

// Email platform plugin compiled into the binary under the bundled platforms/email key
type emailPlatformPlugin struct{}

func (emailPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "email-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/email",
		Author: "NousResearch",
		Description: "Email gateway adapter for Icebath Agent. Polls an IMAP mailbox for inbound messages and replies over SMTP, " +
			"relaying email threads to and from the Icebath agent.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "EMAIL_ADDRESS", Description: "Email account address"},
			{Name: "EMAIL_PASSWORD", Description: "Email account password / app password", Secret: true},
			{Name: "EMAIL_SMTP_HOST", Description: "SMTP host (e.g. smtp.gmail.com)"}}}
}

// Adapter class the gateway reads the long message and serial receive traits from
var emailAdapterClass = &gateway.AdapterClass{Name: "EmailAdapter", Traits: emailAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewEmailAdapter(ctx, config)
	}}

// Registers the email platform entry with its factory, gates, env bridge, standalone sender and hint
func (emailPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   gateway.RegexStrippedParser(sendTargetPattern),
		"standalone_send":        &gateway.StandaloneSendPolicy{TextDirect: true},
		"scoped_check_fn":        checkEmailRequirements,
		"is_connected":           isConnected,
		"dashboard_description":  "Talk to Icebath through an IMAP/SMTP mailbox.",
		"docs_url":               "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/",
		"dashboard_env_vars":     []string{"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_IMAP_HOST", "EMAIL_SMTP_HOST"},
		"dashboard_required_env": []string{"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_IMAP_HOST", "EMAIL_SMTP_HOST"},
		"cron_deliver_env_var":   "EMAIL_HOME_ADDRESS",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     50_000,
		"pii_safe":               true,
		"emoji":                  "📧",
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
		Name: "email", Label: "Email",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewEmailAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkEmailRequirements(context.Background()) },
		RequiredEnv: []string{"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_SMTP_HOST"},
		InstallHint: "Email uses the Go standard library — no extra deps",
		Extra:       extra,
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(emailAdapterClass)
	plugins.RegisterBundled(emailPlatformPlugin{})
}
