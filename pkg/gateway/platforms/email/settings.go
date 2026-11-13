package email

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Security mode spellings accepted for IMAP and SMTP
var securityAliases = map[string]string{"tls": "tls", "ssl": "tls", "implicit": "tls", "starttls": "starttls", "plain": "plain", "none": "plain"}

// One mail server connection target
type endpoint struct {
	host     string
	port     int
	security string
	verify   bool
}

// Mailbox account and behavior settings read from the profile env and the platform config
type settings struct {
	address         string
	password        string
	imap            endpoint
	smtp            endpoint
	pollInterval    time.Duration
	skipAttachments bool
	requireAuth     bool
	authservID      string
}

// Scoped env and config extra reader keeping the first scope failure
type settingsReader struct {
	ctx   context.Context
	extra map[string]any
	err   error
}

// Scoped secret value, empty when unset or unreadable
func (r *settingsReader) secret(name string) string {
	v, err := gateway.GetScopedSecret(r.ctx, name, "", false)
	if err != nil && r.err == nil {
		r.err = err
	}
	return v
}

// Config extra value as text, empty when absent
func (r *settingsReader) extraText(key string) string {
	v, ok := r.extra[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Env value when set, else the config extra value, trimmed
func (r *settingsReader) setting(env, key string) string {
	if v := strings.TrimSpace(r.secret(env)); v != "" {
		return v
	}
	return strings.TrimSpace(r.extraText(key))
}

// Boolean of a truthy or falsy word, false in the second result for anything else
func parseBool(raw string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// Integer setting within bounds, the default with a warning when it does not parse
func (r *settingsReader) integer(env, key string, def, lo, hi int) int {
	raw := r.setting(env, key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		slog.Warn(fmt.Sprintf("[Email] Ignoring invalid %s %q; using %d", env, raw, def))
		return def
	}
	return n
}

// Verification switch that only an explicit falsy word turns off
func (r *settingsReader) verify(env, key string) bool {
	raw := r.setting(env, key)
	if raw == "" {
		return true
	}
	v, ok := parseBool(raw)
	if !ok {
		slog.Warn(fmt.Sprintf("[Email] Unrecognized %s %q; keeping TLS verification on", env, raw))
		return true
	}
	return v
}

// Security mode of the value, the default with a warning for an unknown spelling so a typo never means plaintext
func normalizeSecurity(value, def string) string {
	raw := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	if mode, ok := securityAliases[raw]; ok {
		return mode
	}
	if raw != "" {
		slog.Warn(fmt.Sprintf("Unknown email security mode %q; using %q", value, def))
	}
	return def
}

// Settings of the profile in ctx with the platform config extra filling what the env leaves unset
func loadSettings(ctx context.Context, extra map[string]any) (settings, error) {
	r := &settingsReader{ctx: ctx, extra: extra}
	s := settings{address: r.setting("EMAIL_ADDRESS", "address"), password: r.secret("EMAIL_PASSWORD")}
	s.imap.host = r.setting("EMAIL_IMAP_HOST", "imap_host")
	s.imap.security = normalizeSecurity(r.setting("EMAIL_IMAP_SECURITY", "imap_security"), "tls")
	imapPort := 993
	if s.imap.security != "tls" {
		imapPort = 143
	}
	s.imap.port = r.integer("EMAIL_IMAP_PORT", "imap_port", imapPort, 1, 65535)
	s.imap.verify = r.verify("EMAIL_IMAP_TLS_VERIFY", "imap_tls_verify")
	s.smtp.host = r.setting("EMAIL_SMTP_HOST", "smtp_host")
	s.smtp.port = r.integer("EMAIL_SMTP_PORT", "smtp_port", 587, 1, 65535)
	smtpSecurity := "starttls"
	if s.smtp.port == 465 {
		smtpSecurity = "tls"
	}
	s.smtp.security = normalizeSecurity(r.setting("EMAIL_SMTP_SECURITY", "smtp_security"), smtpSecurity)
	s.smtp.verify = r.verify("EMAIL_SMTP_TLS_VERIFY", "smtp_tls_verify")
	s.pollInterval = time.Duration(r.integer("EMAIL_POLL_INTERVAL", "poll_interval", 15, 0, 86400)) * time.Second
	s.skipAttachments, _ = parseBool(r.extraText("skip_attachments"))
	if raw, ok := extra["require_authenticated_sender"]; ok {
		v, known := parseBool(fmt.Sprint(raw))
		s.requireAuth = v || !known
	} else {
		trust, _ := parseBool(r.secret("EMAIL_TRUST_FROM_HEADER"))
		s.requireAuth = !trust
	}
	authserv := strings.TrimSpace(r.extraText("authserv_id"))
	if authserv == "" {
		authserv = strings.TrimSpace(r.secret("EMAIL_AUTHSERV_ID"))
	}
	s.authservID = strings.ToLower(authserv)
	return s, r.err
}
