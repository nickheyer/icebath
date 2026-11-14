package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyemail"
	"github.com/nickheyer/icebath/pkg/internal/pyimaplib"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysmtplib"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	smtpConnectTimeout = 30 * time.Second
	seenUIDsMax        = 2000
)

var (
	securityAliases = map[string]string{"tls": "tls", "ssl": "tls", "implicit": "tls", "starttls": "starttls", "plain": "plain", "none": "plain"}
	truthyValues    = map[string]bool{"true": true, "1": true, "yes": true}
	truthyStrings   = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

	// Seen UID snapshot per account surviving adapter recreation, and the accounts warned about a missing authserv pin
	accountState = struct {
		sync.Mutex
		seenUIDs         map[string]map[string]bool
		missingPinWarned map[string]bool
	}{seenUIDs: map[string]map[string]bool{}, missingPinWarned: map[string]bool{}}
)

// Bool of a config or env value under the shared truthy strings, the default for an unset one
func isTruthyValue(value any, def bool) bool {
	if value == nil {
		return def
	}
	if s, ok := value.(string); ok {
		return truthyStrings[pystr.Lower(pystr.Strip(s))]
	}
	return pyrepr.Truthy(value)
}

// Scope aware secret read with an empty default
func secret(ctx context.Context, name string) (string, error) {
	return gateway.GetScopedSecret(ctx, name, "", false)
}

// Scope aware integer read, the default when unset or unparseable
func esecretInt(ctx context.Context, name string, def int) (int, error) {
	raw, err := secret(ctx, name)
	if err != nil {
		return 0, err
	}
	if text := pystr.Strip(raw); text != "" {
		return gateway.CoercePort(text, def), nil
	}
	return def, nil
}

// Scope aware boolean read, the default when unset
func esecretBool(ctx context.Context, name string, def bool) (bool, error) {
	raw, err := secret(ctx, name)
	if err != nil {
		return false, err
	}
	if text := pystr.Strip(raw); text != "" {
		return isTruthyValue(text, def), nil
	}
	return def, nil
}

// Security mode as tls, starttls or plain, an unknown value warned about and read as the default
func normalizeSecurity(value any, def string) string {
	raw := ""
	if pyrepr.Truthy(value) {
		raw = strings.ReplaceAll(strings.ReplaceAll(pystr.Lower(pystr.Strip(pyrepr.Str(value))), "-", ""), "_", "")
	}
	mode, known := securityAliases[raw]
	if raw != "" && !known {
		slog.Warn(fmt.Sprintf("Unknown email security mode %s; using %s", pyrepr.Repr(value), pyrepr.Repr(def)))
	}
	if !known {
		return def
	}
	return mode
}

// TLS settings verifying the server unless verification was explicitly turned off
func tlsContext(verify bool, host string) *tls.Config {
	if verify {
		return &tls.Config{}
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		slog.Warn(fmt.Sprintf("TLS verification disabled for non-loopback host %s", host))
	}
	return &tls.Config{InsecureSkipVerify: true}
}

// Logs out, closing the socket even when the logout itself fails on a broken connection
func closeIMAP(imap *pyimaplib.IMAP4) {
	if _, _, err := imap.Logout(); err != nil {
		imap.Shutdown()
	}
}

// Opens one SMTP connection with TLS established per the security mode
func openSMTP(ctx context.Context, host string, port int, security string, tlsConfig *tls.Config, dial pysmtplib.Dialer,
	timeout time.Duration) (*pysmtplib.SMTP, error) {
	if security == "tls" {
		return pysmtplib.New(ctx, host, port, timeout, dial, tlsConfig)
	}
	smtp, err := pysmtplib.New(ctx, host, port, timeout, dial, nil)
	if err != nil {
		return nil, err
	}
	if security == "starttls" {
		if err := smtp.Starttls(ctx, tlsConfig); err != nil {
			smtp.Close()
			return nil, err
		}
	}
	return smtp, nil
}

// Sends the RFC 2971 client ID when the server advertises the extension, a refusal logged at debug
func sendIMAPID(imap *pyimaplib.IMAP4) {
	advertised := false
	for _, capability := range imap.Capabilities {
		advertised = advertised || capability == "ID"
	}
	if !advertised {
		slog.Debug("[Email] Server does not advertise IMAP ID capability; skipping ID")
		return
	}
	version := "0"
	if info, err := updaterecord.GetVersionInfo(); err == nil {
		version = info.BaseVersion
	}
	if _, _, err := imap.Xatom("ID", fmt.Sprintf(`("name" "icebath-agent" "version" "%s" `+
		`"vendor" "NousResearch" "support-email" "noreply@nousresearch.com")`, version)); err != nil {
		slog.Debug(fmt.Sprintf("[Email] IMAP ID command not accepted: %s", err.Error()))
	}
}

// Literal bytes of a one message FETCH response, false for any other shape
func imapPayload(data []any) ([]byte, bool) {
	if len(data) == 0 {
		return nil, false
	}
	literal, ok := data[0].(pyimaplib.Literal)
	if !ok {
		return nil, false
	}
	return literal.Data, true
}

// Whether every email setting is present and non blank in the scoped env
func checkEmailRequirements(ctx context.Context) bool {
	for _, name := range []string{"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_IMAP_HOST", "EMAIL_SMTP_HOST"} {
		value, err := secret(ctx, name)
		if err != nil || pystr.Strip(value) == "" {
			return false
		}
	}
	return true
}

// Subject and Message-ID of the last mail of a sender, kept for threading the reply
type threadContext struct {
	subject   string
	messageID string
}

// One fetched message reduced to what authorization and dispatch need
type fetchedMessage struct {
	uid                 string
	senderAddr          string
	senderName          string
	subject             string
	messageID           string
	inReplyTo           string
	date                string
	senderAuthenticated bool
	authReason          string
	body                string
	attachments         []attachment
}

// Email gateway adapter receiving through polled IMAP and sending through SMTP
type EmailAdapter struct {
	*gateway.BasePlatformAdapter

	address                    string
	password                   string
	imapHost                   string
	imapPort                   int
	imapSecurity               string
	imapTLSVerify              bool
	smtpHost                   string
	smtpPort                   int
	smtpSecurity               string
	smtpTLSVerify              bool
	pollInterval               int
	skipAttachments            bool
	requireAuthenticatedSender bool
	authservID                 string

	mu              sync.Mutex
	seenUIDs        map[string]bool
	pollTask        *gateway.AdapterTask
	lastFetchFailed bool
	lastFetchError  string
	threadContexts  map[string]threadContext
}

// Class attributes the email adapter declares over the base defaults
func emailAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.SerialReceiveLoop = true
	traits.SplitsLongMessages = true
	return traits
}

// Stripped text of a setting value, the AttributeError strip raises on a non string
func stripSetting(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))}
	}
	return pystr.Strip(s), nil
}

// Adapter for a platform config, each setting read from the scoped env first and the config extra after
func NewEmailAdapter(ctx context.Context, config *gateway.PlatformConfig) (*EmailAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &EmailAdapter{seenUIDs: map[string]bool{}, threadContexts: map[string]threadContext{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformEmail, a)
	a.Traits = emailAdapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	setting := func(env, key string) (any, error) {
		value, err := secret(ctx, env)
		if err != nil {
			return nil, err
		}
		if value != "" {
			return value, nil
		}
		if v, ok := extra[key]; ok {
			return v, nil
		}
		return "", nil
	}
	strippedSetting := func(env, key string) (string, error) {
		value, err := setting(env, key)
		if err != nil {
			return "", err
		}
		return stripSetting(value)
	}
	tlsVerify := func(env, key string) (bool, error) {
		return esecretBool(ctx, env, isTruthyValue(extra[key], true))
	}
	var err error
	if a.address, err = strippedSetting("EMAIL_ADDRESS", "address"); err != nil {
		return nil, err
	}
	if a.password, err = secret(ctx, "EMAIL_PASSWORD"); err != nil {
		return nil, err
	}
	if a.imapHost, err = strippedSetting("EMAIL_IMAP_HOST", "imap_host"); err != nil {
		return nil, err
	}
	if a.imapPort, err = esecretInt(ctx, "EMAIL_IMAP_PORT", 993); err != nil {
		return nil, err
	}
	imapSecurity, err := setting("EMAIL_IMAP_SECURITY", "imap_security")
	if err != nil {
		return nil, err
	}
	a.imapSecurity = normalizeSecurity(imapSecurity, "tls")
	if a.imapTLSVerify, err = tlsVerify("EMAIL_IMAP_TLS_VERIFY", "imap_tls_verify"); err != nil {
		return nil, err
	}
	if a.smtpHost, err = strippedSetting("EMAIL_SMTP_HOST", "smtp_host"); err != nil {
		return nil, err
	}
	if a.smtpPort, err = esecretInt(ctx, "EMAIL_SMTP_PORT", 587); err != nil {
		return nil, err
	}
	smtpSecurity, err := setting("EMAIL_SMTP_SECURITY", "smtp_security")
	if err != nil {
		return nil, err
	}
	smtpDefault := "starttls"
	if a.smtpPort == 465 {
		smtpDefault = "tls"
	}
	a.smtpSecurity = normalizeSecurity(smtpSecurity, smtpDefault)
	if a.smtpTLSVerify, err = tlsVerify("EMAIL_SMTP_TLS_VERIFY", "smtp_tls_verify"); err != nil {
		return nil, err
	}
	if a.pollInterval, err = esecretInt(ctx, "EMAIL_POLL_INTERVAL", 15); err != nil {
		return nil, err
	}
	a.skipAttachments = pyrepr.Truthy(extra["skip_attachments"])
	if required, ok := extra["require_authenticated_sender"]; ok {
		a.requireAuthenticatedSender = pyrepr.Truthy(required)
	} else {
		trust, err := esecretBool(ctx, "EMAIL_TRUST_FROM_HEADER", false)
		if err != nil {
			return nil, err
		}
		a.requireAuthenticatedSender = !trust
	}
	var authserv any = extra["authserv_id"]
	if !pyrepr.Truthy(authserv) {
		if authserv, err = secret(ctx, "EMAIL_AUTHSERV_ID"); err != nil {
			return nil, err
		}
	}
	authservText, err := stripSetting(authserv)
	if err != nil {
		return nil, err
	}
	a.authservID = pystr.Lower(authservText)
	slog.Info(fmt.Sprintf("[Email] Adapter initialized for %s", a.address))
	return a, nil
}

// Keeps only the highest half of the seen UIDs once over the cap, the adapter lock held
func (a *EmailAdapter) trimSeenUIDs() {
	if len(a.seenUIDs) <= seenUIDsMax {
		return
	}
	uids := make([]string, 0, len(a.seenUIDs))
	numeric := true
	values := map[string]int64{}
	for uid := range a.seenUIDs {
		uids = append(uids, uid)
		n, err := pyint.Int64(uid)
		if err != nil {
			numeric = false
		}
		values[uid] = n
	}
	if numeric {
		sort.Slice(uids, func(i, j int) bool { return values[uids[i]] < values[uids[j]] })
	}
	kept := map[string]bool{}
	for _, uid := range uids[len(uids)-seenUIDsMax/2:] {
		kept[uid] = true
	}
	a.seenUIDs = kept
	if numeric {
		slog.Debug(fmt.Sprintf("[Email] Trimmed seen UIDs to %d entries", len(a.seenUIDs)))
	}
}

// IMAP connection over implicit TLS, STARTTLS or plain text
func (a *EmailAdapter) connectIMAP(ctx context.Context) (*pyimaplib.IMAP4, error) {
	if a.imapSecurity == "tls" {
		return pyimaplib.Open(ctx, a.imapHost, a.imapPort, 30*time.Second, tlsContext(a.imapTLSVerify, a.imapHost))
	}
	imap, err := pyimaplib.Open(ctx, a.imapHost, a.imapPort, 30*time.Second, nil)
	if err != nil {
		return nil, err
	}
	if a.imapSecurity == "starttls" {
		if err := imap.Starttls(ctx, tlsContext(a.imapTLSVerify, a.imapHost)); err != nil {
			closeIMAP(imap)
			return nil, err
		}
	}
	return imap, nil
}

// Runs fn on a logged in IMAP handle with INBOX selected, the handle always closed afterwards
func (a *EmailAdapter) inbox(ctx context.Context, fn func(imap *pyimaplib.IMAP4) error) error {
	imap, err := a.connectIMAP(ctx)
	if err != nil {
		return err
	}
	defer closeIMAP(imap)
	if _, _, err := imap.Login(a.address, a.password); err != nil {
		return err
	}
	sendIMAPID(imap)
	if _, _, err := imap.Select("INBOX"); err != nil {
		return err
	}
	return fn(imap)
}

// SMTP connection with TLS established, a connection failure retried once through IPv4 only
func (a *EmailAdapter) connectSMTP(ctx context.Context) (*pysmtplib.SMTP, error) {
	config := tlsContext(a.smtpTLSVerify, a.smtpHost)
	smtp, err := openSMTP(ctx, a.smtpHost, a.smtpPort, a.smtpSecurity, config, pysmtplib.DialDefault, smtpConnectTimeout)
	if err == nil {
		return smtp, nil
	}
	var smtpErr *pysmtplib.Error
	if ctx.Err() != nil || !errors.As(err, &smtpErr) || smtpErr.IsSSL() || smtpErr.Class == "ValueError" {
		return nil, err
	}
	return openSMTP(ctx, a.smtpHost, a.smtpPort, a.smtpSecurity, config, pysmtplib.DialIPv4, smtpConnectTimeout)
}

// Logs the error, records a fatal error for the reconnect machinery and reports the connect as failed
func (a *EmailAdapter) fail(ctx context.Context, logFmt string, err any, code, detail string, retryable bool) bool {
	slog.Error(fmt.Sprintf(logFmt, pyrepr.Str(err)))
	a.SetFatalError(ctx, code, detail, retryable)
	return false
}

// Members of an IMAP search answer, none unless the server answered OK with data
func searchUIDs(status string, data []any) []string {
	if status != "OK" || len(data) == 0 {
		return nil
	}
	first, _ := data[0].([]byte)
	return strings.Fields(string(first))
}

// Tests the IMAP connection and sets the seen UID baseline, a failure recorded as a retryable fatal error
func (a *EmailAdapter) probeIMAP(ctx context.Context, isReconnect bool) bool {
	err := a.inbox(ctx, func(imap *pyimaplib.IMAP4) error {
		accountState.Lock()
		snapshot, hasSnapshot := accountState.seenUIDs[a.address]
		accountState.Unlock()
		a.mu.Lock()
		defer a.mu.Unlock()
		passed := "[Email] IMAP connection test passed. %d existing messages skipped."
		if isReconnect && hasSnapshot {
			a.seenUIDs = map[string]bool{}
			for uid := range snapshot {
				a.seenUIDs[uid] = true
			}
			passed = "[Email] IMAP reconnect test passed. Restored %d seen UIDs; messages received during the outage will be processed."
		} else {
			a.mu.Unlock()
			status, data, err := imap.UID("search", "ALL")
			a.mu.Lock()
			if err != nil {
				return err
			}
			for _, uid := range searchUIDs(status, data) {
				a.seenUIDs[uid] = true
			}
		}
		a.trimSeenUIDs()
		slog.Info(fmt.Sprintf(passed, len(a.seenUIDs)))
		return nil
	})
	if err != nil {
		return a.fail(ctx, "[Email] IMAP connection failed: %s", err.Error(), "email_imap_connect_error",
			fmt.Sprintf("IMAP connection to %s:%d failed: %s", a.imapHost, a.imapPort, err.Error()), true)
	}
	a.snapshotSeenUIDs()
	return true
}

// Stores a copy of the seen UIDs as the reconnect baseline of the account
func (a *EmailAdapter) snapshotSeenUIDs() {
	a.mu.Lock()
	snapshot := make(map[string]bool, len(a.seenUIDs))
	for uid := range a.seenUIDs {
		snapshot[uid] = true
	}
	a.mu.Unlock()
	accountState.Lock()
	accountState.seenUIDs[a.address] = snapshot
	accountState.Unlock()
}

// Tests SMTP connect and login, an authentication failure recorded as a fatal error that is never retried
func (a *EmailAdapter) probeSMTP(ctx context.Context) bool {
	err := func() error {
		smtp, err := a.connectSMTP(ctx)
		if err != nil {
			return err
		}
		loginErr := smtp.Login(a.address, a.password)
		if quitErr := smtp.Quit(); quitErr != nil {
			return quitErr
		}
		return loginErr
	}()
	if err == nil {
		slog.Info("[Email] SMTP connection test passed.")
		return true
	}
	var smtpErr *pysmtplib.Error
	if errors.As(err, &smtpErr) && smtpErr.Class == "SMTPAuthenticationError" {
		return a.fail(ctx, "[Email] SMTP authentication failed: %s", err.Error(), "email_auth_error",
			fmt.Sprintf("SMTP authentication failed for %s: %s. Check EMAIL_PASSWORD (for Gmail/Outlook "+
				"this must be an app password, not the account password).", a.address, err.Error()), false)
	}
	return a.fail(ctx, "[Email] SMTP connection failed: %s", err.Error(), "email_smtp_connect_error",
		fmt.Sprintf("SMTP connection to %s failed: %s", a.smtpHost, err.Error()), true)
}

// Validates the settings, probes IMAP and SMTP and starts polling for new messages
func (a *EmailAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	var missing []string
	for _, pair := range [][2]string{{"EMAIL_ADDRESS", a.address}, {"EMAIL_PASSWORD", a.password}, {"EMAIL_IMAP_HOST", a.imapHost},
		{"EMAIL_SMTP_HOST", a.smtpHost}} {
		if pair[1] == "" {
			missing = append(missing, pair[0])
		}
	}
	if len(missing) > 0 {
		message := fmt.Sprintf("Not configured — missing %s. Set it via `icebath gateway setup` (env) or platforms.email in config.yaml.",
			strings.Join(missing, ", "))
		return a.fail(ctx, "[Email] %s", message, "email_missing_configuration", message, false), nil
	}
	if !a.probeIMAP(ctx, isReconnect) || !a.probeSMTP(ctx) {
		return false, ctx.Err()
	}
	if a.requireAuthenticatedSender && a.authservID == "" {
		accountState.Lock()
		warned := accountState.missingPinWarned[a.address]
		accountState.missingPinWarned[a.address] = true
		accountState.Unlock()
		if !warned {
			slog.Warn(fmt.Sprintf("[Email] %s: %s.%s", a.address, missingAuthservReason, missingAuthservHint))
		}
	}
	a.SetRunning(true)
	task := gateway.SpawnAdapterTask(ctx, a.pollLoop)
	a.mu.Lock()
	a.pollTask = task
	a.mu.Unlock()
	fmt.Fprintf(os.Stdout, "[Email] Connected as %s\n", a.address)
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops polling
func (a *EmailAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.mu.Lock()
	task := a.pollTask
	a.pollTask = nil
	a.mu.Unlock()
	gateway.CancelTask(ctx, task)
	slog.Info("[Email] Disconnected.")
	return nil
}

// Checks the inbox at the poll interval until the adapter stops
func (a *EmailAdapter) pollLoop(ctx context.Context) error {
	for a.IsConnected() {
		err := a.checkInbox(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Error(fmt.Sprintf("[Email] Poll error: %s", pyrepr.ExceptionMessage(err)))
		}
		timer := time.NewTimer(time.Duration(a.pollInterval) * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return nil
}

// Fetches unseen messages off the caller, dispatches what arrived and escalates a failed fetch as a fatal error
func (a *EmailAdapter) checkInbox(ctx context.Context) error {
	gate := func(candidate *fetchedMessage) (bool, error) { return a.senderAccepted(ctx, candidate.senderAddr, candidate) }
	fetched := make(chan []*fetchedMessage, 1)
	go func() { fetched <- a.fetchNewMessages(context.WithoutCancel(ctx), gate) }()
	var messages []*fetchedMessage
	select {
	case messages = <-fetched:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, msg := range messages {
		if err := a.dispatchMessage(ctx, msg); err != nil {
			return err
		}
	}
	a.mu.Lock()
	failed, reason := a.lastFetchFailed, a.lastFetchError
	a.lastFetchFailed = false
	a.mu.Unlock()
	if failed {
		if reason == "" {
			reason = "IMAP fetch failed"
		}
		a.SetFatalError(ctx, "email_imap_fetch_failed", reason, true)
		return a.NotifyFatalError(ctx)
	}
	return nil
}

// Remembers a rejected UID and marks it seen on the server without fetching its body
func (a *EmailAdapter) markUIDConsumed(imap *pyimaplib.IMAP4, uid string) error {
	a.mu.Lock()
	a.seenUIDs[uid] = true
	a.trimSeenUIDs()
	a.mu.Unlock()
	status, _, err := imap.UID("store", uid, "+FLAGS.SILENT", `(\Seen)`)
	if err != nil {
		return err
	}
	if status != "OK" {
		slog.Warn(fmt.Sprintf("[Email] Could not mark rejected UID %s seen", uidRepr(uid)))
	}
	return nil
}

// Fetches unseen messages, the bounded headers of each passing the pre authorization before its body is requested
func (a *EmailAdapter) fetchNewMessages(ctx context.Context, preauthorize func(*fetchedMessage) (bool, error)) []*fetchedMessage {
	results := []*fetchedMessage{}
	err := a.inbox(ctx, func(imap *pyimaplib.IMAP4) error {
		status, data, err := imap.UID("search", "UNSEEN")
		if err != nil {
			return err
		}
		for _, uid := range searchUIDs(status, data) {
			a.mu.Lock()
			seen := a.seenUIDs[uid]
			a.mu.Unlock()
			if seen {
				continue
			}
			headerStatus, headerData, err := imap.UID("fetch", uid, preauthFetch)
			if err != nil {
				return err
			}
			if headerStatus != "OK" {
				continue
			}
			rawHeaders, ok := imapPayload(headerData)
			if !ok || len(rawHeaders) > maxPreauthHeaderBytes {
				slog.Warn(fmt.Sprintf("[Email] Unusable pre-authorization headers for UID %s, skipping", uidRepr(uid)))
				if err := a.markUIDConsumed(imap, uid); err != nil {
					return err
				}
				continue
			}
			accepted := false
			candidate, authErr := a.parseFetchedHeaders(uid, rawHeaders)
			if authErr == nil && candidate != nil {
				accepted, authErr = preauthorize(candidate)
			}
			if authErr != nil {
				slog.Error(fmt.Sprintf("[Email] Failed to authorize message UID %s, skipping: %s", uidRepr(uid), pyrepr.ExceptionMessage(authErr)))
				accepted = false
			}
			if !accepted {
				if err := a.markUIDConsumed(imap, uid); err != nil {
					return err
				}
				continue
			}
			status, msgData, err := imap.UID("fetch", uid, "(RFC822)")
			if err != nil {
				return err
			}
			if status != "OK" {
				continue
			}
			a.mu.Lock()
			a.seenUIDs[uid] = true
			a.trimSeenUIDs()
			a.mu.Unlock()
			rawEmail, ok := imapPayload(msgData)
			if !ok {
				slog.Warn(fmt.Sprintf("[Email] Unexpected IMAP response structure for UID %s, skipping", uidRepr(uid)))
				continue
			}
			parsed, parseErr := a.parseFetchedMessage(ctx, uid, rawEmail)
			if parseErr != nil {
				slog.Error(fmt.Sprintf("[Email] Failed to process message UID %s, skipping: %s", uidRepr(uid), pyrepr.ExceptionMessage(parseErr)))
				continue
			}
			if parsed != nil {
				results = append(results, parsed)
			}
		}
		return nil
	})
	if err != nil {
		slog.Error(fmt.Sprintf("[Email] IMAP fetch error: %s", err.Error()))
		a.mu.Lock()
		a.lastFetchFailed, a.lastFetchError = true, err.Error()
		a.mu.Unlock()
	}
	a.snapshotSeenUIDs()
	return results
}

// Sender facing headers and the authentication verdict of a message, nil for an unparseable or automated sender
func (a *EmailAdapter) messageMetadata(uid string, msg *pyemail.Message) (*fetchedMessage, error) {
	from, _ := msg.Get("From")
	senderAddr := extractEmailAddress(from.Str())
	if senderAddr == "" {
		slog.Debug(fmt.Sprintf("[Email] Dropping message with no parseable From address: %s", pyrepr.Repr(from.Str())))
		return nil, nil
	}
	senderName := decodeHeaderValue(from)
	if strings.Contains(senderName, "<") {
		head, _, _ := strings.Cut(senderName, "<")
		senderName = pystr.StripChars(pystr.Strip(head), `"`)
	}
	subjectValue, ok := msg.Get("Subject")
	if !ok {
		subjectValue = "(no subject)"
	}
	subject := decodeHeaderValue(subjectValue)
	headers := map[string]pyemail.HeaderValue{}
	for _, item := range msg.Items() {
		headers[item[0]] = pyemail.HeaderValue(item[1])
	}
	automated, err := isAutomatedSender(senderAddr, headers)
	if err != nil {
		return nil, err
	}
	if automated {
		slog.Debug(fmt.Sprintf("[Email] Skipping automated sender: %s", senderAddr))
		return nil, nil
	}
	authenticated, reason := verifySenderAuthentication(msg, senderAddr, a.authservID)
	text := func(name string) string {
		value, _ := msg.Get(name)
		return value.Str()
	}
	return &fetchedMessage{uid: uid, senderAddr: senderAddr, senderName: senderName, subject: subject, messageID: text("Message-ID"),
		inReplyTo: text("In-Reply-To"), date: text("Date"), senderAuthenticated: authenticated, authReason: reason}, nil
}

// Metadata of the bounded header preflight, no MIME tree built
func (a *EmailAdapter) parseFetchedHeaders(uid string, rawHeaders []byte) (*fetchedMessage, error) {
	return a.messageMetadata(uid, pyemail.ParseBytes(rawHeaders, true))
}

// An authorized RFC822 payload parsed into a dispatchable message with its body and attachments
func (a *EmailAdapter) parseFetchedMessage(ctx context.Context, uid string, rawEmail []byte) (*fetchedMessage, error) {
	msg := pyemail.ParseBytes(rawEmail, false)
	metadata, err := a.messageMetadata(uid, msg)
	if err != nil || metadata == nil {
		return nil, err
	}
	metadata.body = extractTextBody(msg)
	if metadata.attachments, err = extractAttachments(ctx, msg, a.skipAttachments); err != nil {
		return nil, err
	}
	return metadata, nil
}

// Whether the operator opted into any sender through the scoped allow all settings
func allowAllSenders(ctx context.Context) (bool, error) {
	for _, name := range []string{"EMAIL_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS"} {
		value, err := secret(ctx, name)
		if err != nil {
			return false, err
		}
		if truthyValues[pystr.Lower(pystr.Strip(value))] {
			return true, nil
		}
	}
	return false, nil
}

// Whether the config answers unknown senders with a pairing code or a decline
func (a *EmailAdapter) answersUnknownSenders() bool {
	behavior, ok := a.Config.Extra["unauthorized_dm_behavior"].(string)
	if !ok {
		return false
	}
	behavior = pystr.Lower(pystr.Strip(behavior))
	return behavior == "pair" || behavior == "decline"
}

// Pre dispatch sender gate, self mail, automated senders, authorization, then From authentication
func (a *EmailAdapter) senderAccepted(ctx context.Context, senderAddr string, msg *fetchedMessage) (bool, error) {
	if senderAddr == pystr.Lower(a.address) {
		return false, nil
	}
	if automated, _ := isAutomatedSender(senderAddr, nil); automated {
		slog.Debug(fmt.Sprintf("[Email] Dropping automated sender at dispatch: %s", senderAddr))
		return false, nil
	}
	allowedSecret, err := secret(ctx, "EMAIL_ALLOWED_USERS")
	if err != nil {
		return false, err
	}
	allowedRaw := pystr.Strip(allowedSecret)
	gatewayAllowed, err := secret(ctx, "GATEWAY_ALLOWED_USERS")
	if err != nil {
		return false, err
	}
	listed := map[string]bool{}
	for _, raw := range []string{allowedRaw, gatewayAllowed} {
		decoded := gateway.DecodeJSONListLiteral(raw)
		items, ok := decoded.([]any)
		if !ok {
			for _, part := range strings.Split(pyrepr.Str(decoded), ",") {
				items = append(items, part)
			}
		}
		for _, item := range items {
			if entry := pystr.Strip(pyrepr.Str(item)); entry != "" {
				listed[pystr.Lower(entry)] = true
			}
		}
	}
	isListed := listed[pystr.Lower(senderAddr)]
	granted := false
	localPart, _, _ := strings.Cut(senderAddr, "@")
	switch {
	case isListed:
		granted = true
	case listed[pystr.Lower(localPart)]:
		slog.Debug(fmt.Sprintf("[Email] Dropping sender whose local part alone matches an allowlist entry: %s", senderAddr))
		return false, nil
	default:
		dm := "dm"
		if verdict := a.IsSenderAuthorized(senderAddr, &dm, &senderAddr, gateway.AuthorizationOpts{}); verdict != nil {
			granted = *verdict
		} else if allowedRaw == "" {
			if granted, err = allowAllSenders(ctx); err != nil {
				return false, err
			}
		}
	}
	if !granted && !a.answersUnknownSenders() {
		slog.Debug(fmt.Sprintf("[Email] Dropping unauthorized sender at dispatch (unknown senders are ignored): %s", senderAddr))
		return false, nil
	}
	if a.requireAuthenticatedSender && !msg.senderAuthenticated {
		authReason := msg.authReason
		if authReason == "" {
			authReason = "no verdict"
		}
		hint := dropHints[authReason]
		if isListed || (granted && hint != "") {
			slog.Warn(fmt.Sprintf("[Email] Dropping sender with unauthenticated From: %s (%s).%s", senderAddr, authReason, hint))
		} else {
			kind := "unknown"
			if granted {
				kind = "authorized"
			}
			slog.Debug(fmt.Sprintf("[Email] Dropping %s sender with unauthenticated From: %s (%s)", kind, senderAddr, authReason))
		}
		return false, nil
	}
	return true, nil
}

// Converts a fetched email into a message event and dispatches it
func (a *EmailAdapter) dispatchMessage(ctx context.Context, msg *fetchedMessage) error {
	senderAddr := msg.senderAddr
	accepted, err := a.senderAccepted(ctx, senderAddr, msg)
	if err != nil || !accepted {
		return err
	}
	subject, body := msg.subject, pystr.Strip(msg.body)
	text := body
	if subject != "" && !strings.HasPrefix(subject, "Re:") {
		text = fmt.Sprintf("[Subject: %s]\n\n%s", subject, body)
	}
	kinds := map[string]bool{}
	mediaURLs, mediaTypes := []string{}, []string{}
	for _, att := range msg.attachments {
		kinds[att.kind] = true
		mediaURLs, mediaTypes = append(mediaURLs, att.path), append(mediaTypes, att.mediaType)
	}
	a.mu.Lock()
	a.threadContexts[senderAddr] = threadContext{subject: subject, messageID: msg.messageID}
	a.mu.Unlock()
	name := msg.senderName
	if name == "" {
		name = senderAddr
	}
	if text == "" {
		text = "(empty email)"
	}
	event := gateway.NewMessageEvent(text)
	messageID := msg.messageID
	event.MessageID = &messageID
	switch {
	case kinds["document"]:
		event.MessageType = gateway.MessageTypeDocument
	case kinds["image"]:
		event.MessageType = gateway.MessageTypePhoto
	default:
		event.MessageType = gateway.MessageTypeText
	}
	event.Source = a.BuildSource(ctx, senderAddr, gateway.SourceFields{ChatName: &name, ChatType: "dm", UserID: &senderAddr, UserName: &name,
		MessageID: &messageID})
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	if msg.inReplyTo != "" {
		inReplyTo := msg.inReplyTo
		event.ReplyToMessageID = &inReplyTo
	}
	slog.Info(fmt.Sprintf("[Email] New message from %s: %s", senderAddr, subject))
	return a.HandleMessage(ctx, event)
}

// Runs a blocking SMTP sender off the caller and wraps its Message-ID in a send result
func (a *EmailAdapter) runSend(ctx context.Context, fn func() (string, error), logFmt string, logArgs ...any) (gateway.SendResult, error) {
	type outcome struct {
		id  string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		id, err := fn()
		done <- outcome{id, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			text := pyrepr.ExceptionMessage(res.err)
			slog.Error(fmt.Sprintf(logFmt, append(logArgs, text)...))
			return gateway.SendResult{Success: false, Error: text}, nil
		}
		return gateway.SendResult{Success: true, MessageID: res.id}, nil
	case <-ctx.Done():
		return gateway.SendResult{}, ctx.Err()
	}
}

// Sends an email reply to the address
func (a *EmailAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	sctx := context.WithoutCancel(ctx)
	return a.runSend(ctx, func() (string, error) { return a.sendEmail(sctx, chatID, content, replyTo) },
		"[Email] Send failed to %s: %s", chatID)
}

// Domain for generated Message-IDs, localhost when the address has none
func (a *EmailAdapter) messageIDDomain() string {
	if i := strings.LastIndex(a.address, "@"); i >= 0 && a.address[i+1:] != "" {
		return a.address[i+1:]
	}
	return "localhost"
}

// Threaded reply skeleton with its Message-ID and subject, the body attached when present or asked for
func (a *EmailAdapter) newReply(toAddr, body, replyToMsgID string, attachEmptyBody bool) (*pyemail.Part, string, string) {
	msg := pyemail.NewMultipart()
	a.mu.Lock()
	ctx, known := a.threadContexts[toAddr]
	a.mu.Unlock()
	subject := "Icebath Agent"
	if known {
		subject = ctx.subject
	}
	if !strings.HasPrefix(subject, "Re:") {
		subject = "Re: " + subject
	}
	originalMsgID := replyToMsgID
	if originalMsgID == "" {
		originalMsgID = ctx.messageID
	}
	msgID := fmt.Sprintf("<icebath-%s@%s>", strings.ReplaceAll(uuid.NewString(), "-", "")[:12], a.messageIDDomain())
	msg.Set("From", a.address)
	msg.Set("To", toAddr)
	msg.Set("Subject", subject)
	if originalMsgID != "" {
		msg.Set("In-Reply-To", originalMsgID)
		msg.Set("References", originalMsgID)
	}
	msg.Set("Date", pyemail.FormatDateLocal())
	msg.Set("Message-ID", msgID)
	if body != "" || attachEmptyBody {
		msg.Attach(pyemail.NewTextUTF8(body))
	}
	return msg, msgID, subject
}

// Logs in, sends and always releases the SMTP connection
func (a *EmailAdapter) smtpSend(ctx context.Context, msg *pyemail.Part) error {
	smtp, err := a.connectSMTP(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if smtp.Quit() != nil {
			smtp.Close()
		}
	}()
	if err := smtp.Login(a.address, a.password); err != nil {
		return err
	}
	return smtp.SendMessage(msg)
}

// Sends a text reply and returns its Message-ID
func (a *EmailAdapter) sendEmail(ctx context.Context, toAddr, body, replyToMsgID string) (string, error) {
	msg, msgID, subject := a.newReply(toAddr, body, replyToMsgID, true)
	if err := a.smtpSend(ctx, msg); err != nil {
		return "", err
	}
	slog.Info(fmt.Sprintf("[Email] Sent reply to %s (subject: %s)", toAddr, subject))
	return msgID, nil
}

// One file to attach with the name it is sent under
type outgoingFile struct {
	path string
	name string
}

// Attaches a file as base64 application/octet-stream
func attachFile(msg *pyemail.Part, path, filename string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return &gateway.PyError{Kind: pypath.OSErrorClass(err), Msg: pypath.OSErrorText(err)}
	}
	part := pyemail.NewBase64("application", "octet-stream", data)
	part.Set("Content-Disposition", "attachment; filename="+filename)
	msg.Attach(part)
	return nil
}

// Sends a reply with attachments, a file that cannot be attached skipped with a warning when lenient
func (a *EmailAdapter) sendWithFiles(ctx context.Context, toAddr, body string, files []outgoingFile, lenient bool, replyToMsgID string) (string, error) {
	msg, msgID, _ := a.newReply(toAddr, body, replyToMsgID, false)
	for _, file := range files {
		if err := attachFile(msg, file.path, file.name); err != nil {
			if !lenient {
				return "", err
			}
			slog.Warn(fmt.Sprintf("[Email] Failed to attach %s: %s", file.path, pyrepr.ExceptionMessage(err)))
		}
	}
	if err := a.smtpSend(ctx, msg); err != nil {
		return "", err
	}
	return msgID, nil
}

// Sends an image URL as a line of the email body
func (a *EmailAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.Send(ctx, chatID, pystr.Strip(caption+"\n\nImage: "+imageURL), replyTo, nil)
}

// Sends one email per batch, local files attached and URL images linked in the body, the base loop on failure
func (a *EmailAdapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	var bodyParts, localPaths []string
	for _, image := range images {
		if image.Alt != "" {
			bodyParts = append(bodyParts, image.Alt)
		}
		if !strings.HasPrefix(image.URL, "file://") {
			bodyParts = append(bodyParts, i18n.T(ctx, "platform.email.image_line", "", map[string]any{"url": image.URL}))
			continue
		}
		localPath := pyurl.Unquote(image.URL[7:])
		if _, err := os.Stat(localPath); err == nil {
			localPaths = append(localPaths, localPath)
		} else {
			slog.Warn(fmt.Sprintf("[Email] Skipping missing image: %s", localPath))
		}
	}
	if len(localPaths) == 0 && len(bodyParts) == 0 {
		return gateway.SendResult{Success: false, Error: "no valid images in batch"}, nil
	}
	sctx := context.WithoutCancel(ctx)
	type outcome struct {
		id  string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		id, err := a.sendEmailWithAttachments(sctx, chatID, strings.Join(bodyParts, "\n\n"), localPaths)
		done <- outcome{id, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			slog.Error(fmt.Sprintf("[Email] Multi-image send failed, falling back: %s", pyrepr.ExceptionMessage(res.err)), "error", res.err)
			return a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, images, metadata, humanDelay)
		}
		return gateway.SendResult{Success: true, MessageID: res.id}, nil
	case <-ctx.Done():
		return gateway.SendResult{}, ctx.Err()
	}
}

// Sends an email with several attachments, files that cannot be attached skipped
func (a *EmailAdapter) sendEmailWithAttachments(ctx context.Context, toAddr, body string, filePaths []string) (string, error) {
	files := make([]outgoingFile, len(filePaths))
	for i, path := range filePaths {
		files[i] = outgoingFile{path: path, name: pypath.PathName(path)}
	}
	msgID, err := a.sendWithFiles(ctx, toAddr, body, files, true, "")
	if err != nil {
		return "", err
	}
	slog.Info(fmt.Sprintf("[Email] Sent multi-attachment email to %s (%d files)", toAddr, len(filePaths)))
	return msgID, nil
}

// Sends a file as an email attachment
func (a *EmailAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	sctx := context.WithoutCancel(ctx)
	return a.runSend(ctx, func() (string, error) {
		return a.sendEmailWithAttachment(sctx, chatID, caption, filePath, fileName, replyTo)
	}, "[Email] Send document failed: %s")
}

// Sends an email with one attachment, failing when the file cannot be attached
func (a *EmailAdapter) sendEmailWithAttachment(ctx context.Context, toAddr, body, filePath, fileName, replyToMsgID string) (string, error) {
	name := fileName
	if name == "" {
		name = pypath.PathName(filePath)
	}
	return a.sendWithFiles(ctx, toAddr, body, []outgoingFile{{path: filePath, name: name}}, false, replyToMsgID)
}

// Basic info of an email chat with the subject of its last mail
func (a *EmailAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	a.mu.Lock()
	subject := a.threadContexts[chatID].subject
	a.mu.Unlock()
	return map[string]any{"name": chatID, "type": "dm", "chat_id": chatID, "subject": subject}, nil
}
