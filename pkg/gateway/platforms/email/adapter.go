package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/mail"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	mailutil "github.com/nickheyer/icebath/pkg/utils/mail"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

const (
	smtpConnectTimeout = 30 * time.Second
	seenUIDsMax        = 2000
)

var (
	// Seen UID snapshot per account surviving adapter recreation with the INBOX UIDVALIDITY it belongs to, and the accounts warned about a missing authserv pin
	accountState = struct {
		sync.Mutex
		seenUIDs         map[string]map[uint32]bool
		uidValidity      map[string]uint32
		missingPinWarned map[string]bool
	}{seenUIDs: map[string]map[uint32]bool{}, uidValidity: map[string]uint32{}, missingPinWarned: map[string]bool{}}
)

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
	if text := strings.TrimSpace(raw); text != "" {
		return gateway.CoercePort(text, def), nil
	}
	return def, nil
}

// How the sending connection is secured for the profile ctx serves: the setting, else STARTTLS and TLS on port 465
func smtpSecurityFor(ctx context.Context, port int) string {
	if mode := settings.EmailSmtpSecurity.Get(gateway.SettingsContext(ctx)); mode != "" {
		return mode
	}
	if port == 465 {
		return "tls"
	}
	return "starttls"
}

// TLS settings verifying the server unless verification was explicitly turned off
func tlsContext(verify bool, host string) *tls.Config {
	if verify {
		return &tls.Config{}
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		pkgLog().Warn(fmt.Sprintf("TLS verification disabled for non-loopback host %s", host))
	}
	return &tls.Config{InsecureSkipVerify: true}
}

// Logs out, the connection closed even when the logout fails on a broken connection
func closeIMAP(ctx context.Context, imap *mailutil.IMAP) {
	_ = imap.Logout(context.WithoutCancel(ctx))
}

// Opens one SMTP connection with TLS established per the security mode
func openSMTP(ctx context.Context, host string, port int, security string, tlsConfig *tls.Config, dial mailutil.DialFunc,
	timeout time.Duration) (*mailutil.SMTP, error) {
	if security == "tls" {
		return mailutil.DialSMTP(ctx, host, port, timeout, dial, tlsConfig)
	}
	smtp, err := mailutil.DialSMTP(ctx, host, port, timeout, dial, nil)
	if err != nil {
		return nil, err
	}
	switch security {
	case "starttls":
		if err := smtp.StartTLS(ctx, tlsConfig); err != nil {
			_ = smtp.Close()
			return nil, err
		}
	case "plain":
		smtp.AllowPlaintextLogin()
	}
	return smtp, nil
}

// Sends the RFC 2971 client ID when the server advertises the extension, a refusal logged at debug
func sendIMAPID(ctx context.Context, imap *mailutil.IMAP) {
	if !imap.HasCapability("ID") {
		pkgLog().Debug("[Email] Server does not advertise IMAP ID capability; skipping ID")
		return
	}
	version := "0"
	if info, err := updaterecord.GetVersionInfo(); err == nil {
		version = info.BaseVersion
	}
	if _, err := imap.ID(ctx, map[string]string{"name": "icebath-agent", "version": version, "vendor": "Icebath",
		"support-url": "https://github.com/nickheyer/icebath/issues"}); err != nil {
		pkgLog().Debug(fmt.Sprintf("[Email] IMAP ID command not accepted: %s", err.Error()))
	}
}

// To field text for a recipient: a mailbox written as it parses, else the bare addr-spec with its local part quoted where it needs quoting
func recipientField(addr string) string {
	if parsed, err := mailutil.ParseAddress(addr); err == nil {
		return parsed.String()
	}
	return (&mail.Address{Address: strings.TrimSpace(addr)}).String()
}

// Fresh Message-ID under the domain of the address, localhost when the address has none
func newMessageID(address string) string {
	domain := "localhost"
	if i := strings.LastIndex(address, "@"); i >= 0 && address[i+1:] != "" {
		domain = address[i+1:]
	}
	return fmt.Sprintf("<icebath-%s@%s>", strings.ReplaceAll(uuid.NewString(), "-", "")[:12], domain)
}

// Whether two UIDVALIDITY values name different UID numberings of the mailbox, a value the server did not give never counting as a change
func uidValidityChanged(previous, current uint32) bool {
	return previous != 0 && current != 0 && previous != current
}

// Whether every email setting is present and non blank in the scoped env
func checkEmailRequirements(ctx context.Context) bool {
	for _, name := range []string{"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_IMAP_HOST", "EMAIL_SMTP_HOST"} {
		value, err := secret(ctx, name)
		if err != nil || strings.TrimSpace(value) == "" {
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
	uid                 uint32
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
	pollInterval               time.Duration
	skipAttachments            bool
	requireAuthenticatedSender bool
	authservID                 string

	mu              sync.Mutex
	seenUIDs        map[uint32]bool
	uidValidity     uint32 // INBOX UIDVALIDITY the seen UIDs belong to, zero while the server has not given one
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

// Text of a setting value with the space around it cut off, an error for a value that is not text
func stripSetting(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", &errorutil.Error{Kind: "AttributeError", Msg: fmt.Sprintf("email setting %s must be text, got %s", key, anyutil.Kind(v))}
	}
	return strings.TrimSpace(s), nil
}

// Address or host of the mailbox, from the scoped env first and the connection's config after, the space around it cut off
func mailboxValue(ctx context.Context, extra map[string]any, env, key string) (string, error) {
	value, err := secret(ctx, env)
	if err != nil {
		return "", err
	}
	if value != "" {
		return strings.TrimSpace(value), nil
	}
	if v, ok := extra[key]; ok {
		return stripSetting(key, v)
	}
	return "", nil
}

// Adapter for a platform config: the mailbox from the scoped env first and the connection's config after, the rest from the settings of the profile served
func NewEmailAdapter(ctx context.Context, config *gateway.PlatformConfig) (*EmailAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &EmailAdapter{seenUIDs: map[uint32]bool{}, threadContexts: map[string]threadContext{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformEmail, a)
	a.Traits = emailAdapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	sctx := gateway.SettingsContext(ctx)
	var err error
	if a.address, err = mailboxValue(ctx, extra, "EMAIL_ADDRESS", "address"); err != nil {
		return nil, err
	}
	if a.password, err = secret(ctx, "EMAIL_PASSWORD"); err != nil {
		return nil, err
	}
	if a.imapHost, err = mailboxValue(ctx, extra, "EMAIL_IMAP_HOST", "imap_host"); err != nil {
		return nil, err
	}
	if a.imapPort, err = esecretInt(ctx, "EMAIL_IMAP_PORT", 993); err != nil {
		return nil, err
	}
	a.imapSecurity = settings.EmailImapSecurity.Get(sctx)
	a.imapTLSVerify = settings.EmailImapTLSVerify.Get(sctx)
	if a.smtpHost, err = mailboxValue(ctx, extra, "EMAIL_SMTP_HOST", "smtp_host"); err != nil {
		return nil, err
	}
	if a.smtpPort, err = esecretInt(ctx, "EMAIL_SMTP_PORT", 587); err != nil {
		return nil, err
	}
	a.smtpSecurity = smtpSecurityFor(ctx, a.smtpPort)
	a.smtpTLSVerify = settings.EmailSmtpTLSVerify.Get(sctx)
	a.pollInterval = settings.EmailPollInterval.Get(sctx)
	a.skipAttachments = settings.EmailSkipAttachments.Get(sctx)
	a.requireAuthenticatedSender = settings.EmailRequireAuthenticatedSender.Get(sctx)
	a.authservID = strings.ToLower(strings.TrimSpace(settings.EmailAuthservID.Get(sctx)))
	pkgLog().Info(fmt.Sprintf("[Email] Adapter initialized for %s", a.address))
	return a, nil
}

// Keeps only the highest half of the seen UIDs once over the cap, the adapter lock held
func (a *EmailAdapter) trimSeenUIDs() {
	if len(a.seenUIDs) <= seenUIDsMax {
		return
	}
	uids := slices.Sorted(maps.Keys(a.seenUIDs))
	kept := make(map[uint32]bool, seenUIDsMax/2)
	for _, uid := range uids[len(uids)-seenUIDsMax/2:] {
		kept[uid] = true
	}
	a.seenUIDs = kept
	pkgLog().Debug(fmt.Sprintf("[Email] Trimmed seen UIDs to %d entries", len(a.seenUIDs)))
}

// IMAP connection over implicit TLS, STARTTLS or plain text, fetched messages bounded by the inbound file limit
func (a *EmailAdapter) connectIMAP(ctx context.Context) (*mailutil.IMAP, error) {
	var implicit *tls.Config
	if a.imapSecurity == "tls" {
		implicit = tlsContext(a.imapTLSVerify, a.imapHost)
	}
	imap, err := mailutil.DialIMAP(ctx, a.imapHost, a.imapPort, 30*time.Second, mailutil.Dial, implicit)
	if err != nil {
		return nil, err
	}
	imap.SetMaxLiteral(maxFetchedMessageBytes(ctx))
	switch a.imapSecurity {
	case "starttls":
		if err := imap.StartTLS(ctx, tlsContext(a.imapTLSVerify, a.imapHost)); err != nil {
			closeIMAP(ctx, imap)
			return nil, err
		}
	case "plain":
		imap.AllowPlaintextLogin()
	}
	return imap, nil
}

// Largest message fetched whole: one file at gateway.max_inbound_media_bytes in base64 with its line breaks, plus a MiB for the headers and text; no bound of its own when the operator turned the file limit off
func maxFetchedMessageBytes(ctx context.Context) int64 {
	limit := mediacache.InboundMediaMaxBytes(ctx, home.Dir(ctx))
	if limit <= 0 || limit > math.MaxInt64/2 {
		return math.MaxInt64
	}
	return limit/3*4 + limit/28 + 1<<20
}

// Runs fn on a logged in IMAP handle with INBOX selected, the handle always closed afterwards
func (a *EmailAdapter) inbox(ctx context.Context, fn func(imap *mailutil.IMAP, mailbox *mailutil.Mailbox) error) error {
	imap, err := a.connectIMAP(ctx)
	if err != nil {
		return err
	}
	defer closeIMAP(ctx, imap)
	if err := imap.Login(ctx, a.address, a.password); err != nil {
		return err
	}
	sendIMAPID(ctx, imap)
	mailbox, err := imap.Select(ctx, "INBOX")
	if err != nil {
		return err
	}
	return fn(imap, mailbox)
}

// SMTP connection with TLS established, a connection failure retried once through IPv4 only
func (a *EmailAdapter) connectSMTP(ctx context.Context) (*mailutil.SMTP, error) {
	config := tlsContext(a.smtpTLSVerify, a.smtpHost)
	smtp, err := openSMTP(ctx, a.smtpHost, a.smtpPort, a.smtpSecurity, config, mailutil.Dial, smtpConnectTimeout)
	if err == nil {
		return smtp, nil
	}
	var tlsErr *mailutil.TLSError
	if ctx.Err() != nil || errors.As(err, &tlsErr) {
		return nil, err
	}
	return openSMTP(ctx, a.smtpHost, a.smtpPort, a.smtpSecurity, config, mailutil.DialIPv4, smtpConnectTimeout)
}

// Logs the error text, records a fatal error for the reconnect machinery and reports the connect as failed
func (a *EmailAdapter) fail(ctx context.Context, logFmt, text, code, detail string, retryable bool) bool {
	pkgLog().Error(fmt.Sprintf(logFmt, text))
	a.SetFatalError(ctx, code, detail, retryable)
	return false
}

// Tests the IMAP connection and sets the seen UID baseline, a failure recorded as a retryable fatal error
func (a *EmailAdapter) probeIMAP(ctx context.Context, isReconnect bool) bool {
	err := a.inbox(ctx, func(imap *mailutil.IMAP, mailbox *mailutil.Mailbox) error {
		accountState.Lock()
		snapshot, hasSnapshot := accountState.seenUIDs[a.address]
		snapshotValidity := accountState.uidValidity[a.address]
		accountState.Unlock()
		a.mu.Lock()
		defer a.mu.Unlock()
		if uidValidityChanged(a.uidValidity, mailbox.UIDValidity) {
			a.seenUIDs = map[uint32]bool{}
		}
		passed := "[Email] IMAP connection test passed. %d existing messages skipped."
		restore := isReconnect && hasSnapshot && !uidValidityChanged(snapshotValidity, mailbox.UIDValidity)
		if restore {
			a.seenUIDs = make(map[uint32]bool, len(snapshot))
			for uid := range snapshot {
				a.seenUIDs[uid] = true
			}
			passed = "[Email] IMAP reconnect test passed. Restored %d seen UIDs; messages received during the outage will be processed."
		} else {
			if isReconnect && hasSnapshot {
				pkgLog().Warn(fmt.Sprintf("[Email] INBOX UIDVALIDITY changed from %d to %d during the outage: the server renumbered "+
					"the mailbox, so the seen UIDs no longer apply and the messages now in it are taken as seen", snapshotValidity,
					mailbox.UIDValidity))
			}
			a.mu.Unlock()
			uids, err := imap.UIDSearch(ctx, "ALL")
			a.mu.Lock()
			if err != nil && !errors.Is(err, mailutil.ErrRefused) {
				return err
			}
			for _, uid := range uids {
				a.seenUIDs[uid] = true
			}
		}
		if mailbox.UIDValidity != 0 {
			a.uidValidity = mailbox.UIDValidity
		}
		a.trimSeenUIDs()
		pkgLog().Info(fmt.Sprintf(passed, len(a.seenUIDs)))
		return nil
	})
	if err != nil {
		return a.fail(ctx, "[Email] IMAP connection failed: %s", err.Error(), "email_imap_connect_error",
			fmt.Sprintf("IMAP connection to %s:%d failed: %s", a.imapHost, a.imapPort, err.Error()), true)
	}
	a.snapshotSeenUIDs()
	return true
}

// Starts the seen UIDs afresh from the messages now in INBOX when the server renumbered the mailbox since the last session, true when it did
func (a *EmailAdapter) rebaselineSeenUIDs(ctx context.Context, imap *mailutil.IMAP, validity uint32) (bool, error) {
	a.mu.Lock()
	previous := a.uidValidity
	if !uidValidityChanged(previous, validity) {
		if validity != 0 {
			a.uidValidity = validity
		}
		a.mu.Unlock()
		return false, nil
	}
	a.mu.Unlock()
	uids, err := imap.UIDSearch(ctx, "ALL")
	if err != nil && !errors.Is(err, mailutil.ErrRefused) {
		return false, err
	}
	a.mu.Lock()
	a.seenUIDs = make(map[uint32]bool, len(uids))
	for _, uid := range uids {
		a.seenUIDs[uid] = true
	}
	a.uidValidity = validity
	a.trimSeenUIDs()
	a.mu.Unlock()
	pkgLog().Warn(fmt.Sprintf("[Email] INBOX UIDVALIDITY changed from %d to %d: the server renumbered the mailbox, so the seen UIDs "+
		"were reset and the %d messages now in it are taken as seen", previous, validity, len(uids)))
	return true, nil
}

// Stores a copy of the seen UIDs and the UIDVALIDITY they belong to as the reconnect baseline of the account
func (a *EmailAdapter) snapshotSeenUIDs() {
	a.mu.Lock()
	snapshot := maps.Clone(a.seenUIDs)
	validity := a.uidValidity
	a.mu.Unlock()
	accountState.Lock()
	accountState.seenUIDs[a.address] = snapshot
	accountState.uidValidity[a.address] = validity
	accountState.Unlock()
}

// Tests SMTP connect and login, an authentication failure recorded as a fatal error that is never retried
func (a *EmailAdapter) probeSMTP(ctx context.Context) bool {
	err := func() error {
		smtp, err := a.connectSMTP(ctx)
		if err != nil {
			return err
		}
		if err := smtp.Login(ctx, a.address, a.password); err != nil {
			_ = smtp.Quit(ctx)
			return err
		}
		return smtp.Quit(ctx)
	}()
	if err == nil {
		pkgLog().Info("[Email] SMTP connection test passed.")
		return true
	}
	if errors.Is(err, mailutil.ErrAuthFailed) {
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
			pkgLog().Warn(fmt.Sprintf("[Email] %s: %s.%s", a.address, missingAuthservReason, missingAuthservHint))
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
	pkgLog().Info("[Email] Disconnected.")
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
			pkgLog().Error(fmt.Sprintf("[Email] Poll error: %s", err.Error()))
		}
		timer := time.NewTimer(a.pollInterval)
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
	gate := func(candidate *fetchedMessage) bool {
		return a.senderAccepted(ctx, candidate.senderAddr, candidate)
	}
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
func (a *EmailAdapter) markUIDConsumed(ctx context.Context, imap *mailutil.IMAP, uid uint32) error {
	a.mu.Lock()
	a.seenUIDs[uid] = true
	a.trimSeenUIDs()
	a.mu.Unlock()
	err := imap.UIDStore(ctx, uid, "+FLAGS.SILENT", `\Seen`)
	if errors.Is(err, mailutil.ErrRefused) {
		pkgLog().Warn(fmt.Sprintf("[Email] Could not mark rejected UID %d seen", uid))
		return nil
	}
	return err
}

// Fetches unseen messages, the bounded headers of each passing the pre authorization before its body is requested
func (a *EmailAdapter) fetchNewMessages(ctx context.Context, preauthorize func(*fetchedMessage) bool) []*fetchedMessage {
	results := []*fetchedMessage{}
	err := a.inbox(ctx, func(imap *mailutil.IMAP, mailbox *mailutil.Mailbox) error {
		if rebaselined, err := a.rebaselineSeenUIDs(ctx, imap, mailbox.UIDValidity); err != nil || rebaselined {
			return err
		}
		uids, err := imap.UIDSearch(ctx, "UNSEEN")
		if err != nil && !errors.Is(err, mailutil.ErrRefused) {
			return err
		}
		for _, uid := range uids {
			a.mu.Lock()
			seen := a.seenUIDs[uid]
			a.mu.Unlock()
			if seen {
				continue
			}
			rawHeaders, err := imap.UIDFetch(ctx, uid, preauthFetch)
			if errors.Is(err, mailutil.ErrRefused) {
				continue
			}
			if err != nil && !errors.Is(err, mailutil.ErrNotFound) {
				return err
			}
			if err != nil || len(rawHeaders) > maxPreauthHeaderBytes {
				pkgLog().Warn(fmt.Sprintf("[Email] Unusable pre-authorization headers for UID %d, skipping", uid))
				if err := a.markUIDConsumed(ctx, imap, uid); err != nil {
					return err
				}
				continue
			}
			candidate := a.parseFetchedHeaders(uid, rawHeaders)
			if candidate == nil || !preauthorize(candidate) {
				if err := a.markUIDConsumed(ctx, imap, uid); err != nil {
					return err
				}
				continue
			}
			rawEmail, err := imap.UIDFetch(ctx, uid, "(BODY[])")
			if errors.Is(err, mailutil.ErrRefused) {
				continue
			}
			var tooLarge *mailutil.LiteralTooLargeError
			if errors.As(err, &tooLarge) {
				pkgLog().Warn(fmt.Sprintf("[Email] Skipping UID %d: the message is %d bytes, over the %d byte limit set by "+
					"gateway.max_inbound_media_bytes", uid, tooLarge.Size, tooLarge.Limit))
				if !tooLarge.Drained {
					// the client closed the connection; the next poll reconnects and goes on with the other messages
					a.mu.Lock()
					a.seenUIDs[uid] = true
					a.trimSeenUIDs()
					a.mu.Unlock()
					return nil
				}
				if err := a.markUIDConsumed(ctx, imap, uid); err != nil {
					return err
				}
				continue
			}
			if err != nil && !errors.Is(err, mailutil.ErrNotFound) {
				return err
			}
			a.mu.Lock()
			a.seenUIDs[uid] = true
			a.trimSeenUIDs()
			a.mu.Unlock()
			if err != nil {
				pkgLog().Warn(fmt.Sprintf("[Email] Unexpected IMAP response structure for UID %d, skipping", uid))
				continue
			}
			parsed, parseErr := a.parseFetchedMessage(ctx, uid, rawEmail)
			if parseErr != nil {
				pkgLog().Error(fmt.Sprintf("[Email] Failed to process message UID %d, skipping: %s", uid, parseErr.Error()))
				continue
			}
			if parsed != nil {
				results = append(results, parsed)
			}
		}
		return nil
	})
	if err != nil {
		pkgLog().Error(fmt.Sprintf("[Email] IMAP fetch error: %s", err.Error()))
		a.mu.Lock()
		a.lastFetchFailed, a.lastFetchError = true, err.Error()
		a.mu.Unlock()
	}
	a.snapshotSeenUIDs()
	return results
}

// Sender facing header fields and the authentication verdict of a message, nil for an unparseable, ambiguous or automated sender
func (a *EmailAdapter) messageMetadata(uid uint32, header mailutil.Header) *fetchedMessage {
	if froms := header.Values("From"); len(froms) > 1 {
		pkgLog().Debug(fmt.Sprintf("[Email] Dropping message UID %d with %d From fields", uid, len(froms)))
		return nil
	}
	from := header.Get("From")
	senderAddr := extractEmailAddress(from)
	if senderAddr == "" {
		pkgLog().Debug(fmt.Sprintf("[Email] Dropping message with no parseable From address: %s", strconv.Quote(from)))
		return nil
	}
	senderName := header.Text("From")
	if strings.Contains(senderName, "<") {
		head, _, _ := strings.Cut(senderName, "<")
		senderName = strings.Trim(strings.TrimSpace(head), `"`)
	}
	subject := "(no subject)"
	if _, ok := header.Lookup("Subject"); ok {
		subject = header.Text("Subject")
	}
	if isAutomatedSender(senderAddr, header) {
		pkgLog().Debug(fmt.Sprintf("[Email] Skipping automated sender: %s", senderAddr))
		return nil
	}
	authenticated, reason := verifySenderAuthentication(header, senderAddr, a.authservID)
	return &fetchedMessage{uid: uid, senderAddr: senderAddr, senderName: senderName, subject: subject,
		messageID: header.Get("Message-ID"), inReplyTo: header.Get("In-Reply-To"), date: header.Get("Date"),
		senderAuthenticated: authenticated, authReason: reason}
}

// Metadata of the bounded header preflight, no MIME tree built
func (a *EmailAdapter) parseFetchedHeaders(uid uint32, rawHeaders []byte) *fetchedMessage {
	return a.messageMetadata(uid, mailutil.ParseHeader(rawHeaders))
}

// An authorized RFC822 payload parsed into a dispatchable message with its body and attachments
func (a *EmailAdapter) parseFetchedMessage(ctx context.Context, uid uint32, rawEmail []byte) (*fetchedMessage, error) {
	msg := mailutil.Parse(rawEmail)
	metadata := a.messageMetadata(uid, msg.Header)
	if metadata == nil {
		return nil, nil
	}
	metadata.body = extractTextBody(msg)
	var err error
	if metadata.attachments, err = extractAttachments(ctx, msg, a.skipAttachments); err != nil {
		return nil, err
	}
	return metadata, nil
}

// Whether the operator opted into any sender, for every chat app or for email
func allowAllSenders(ctx context.Context) bool {
	sctx := gateway.SettingsContext(ctx)
	return settings.GatewayAllowAllUsers.Get(sctx) || settings.EmailAllowAllUsers.Get(sctx)
}

// Whether the config answers unknown senders with a pairing code or a decline
func (a *EmailAdapter) answersUnknownSenders() bool {
	behavior, ok := a.Config.Extra["unauthorized_dm_behavior"].(string)
	if !ok {
		return false
	}
	behavior = strings.ToLower(strings.TrimSpace(behavior))
	return behavior == "pair" || behavior == "decline"
}

// Pre dispatch sender gate, self mail, automated senders, authorization, then From authentication
func (a *EmailAdapter) senderAccepted(ctx context.Context, senderAddr string, msg *fetchedMessage) bool {
	if senderAddr == strings.ToLower(a.address) {
		return false
	}
	if isAutomatedSender(senderAddr, nil) {
		pkgLog().Debug(fmt.Sprintf("[Email] Dropping automated sender at dispatch: %s", senderAddr))
		return false
	}
	sctx := gateway.SettingsContext(ctx)
	allowed := settings.EmailAllowedUsers.Strings(sctx)
	listed := map[string]bool{}
	for _, entries := range [][]string{allowed, settings.GatewayAllowedUsers.Strings(sctx)} {
		for _, entry := range entries {
			listed[strings.ToLower(entry)] = true
		}
	}
	isListed := listed[strings.ToLower(senderAddr)]
	granted := false
	localPart, _, _ := strings.Cut(senderAddr, "@")
	switch {
	case isListed:
		granted = true
	case listed[strings.ToLower(localPart)]:
		pkgLog().Debug(fmt.Sprintf("[Email] Dropping sender whose local part alone matches an allowlist entry: %s", senderAddr))
		return false
	default:
		dm := "dm"
		if verdict := a.IsSenderAuthorized(senderAddr, &dm, &senderAddr, gateway.AuthorizationOpts{}); verdict != nil {
			granted = *verdict
		} else if len(allowed) == 0 {
			granted = allowAllSenders(ctx)
		}
	}
	if !granted && !a.answersUnknownSenders() {
		pkgLog().Debug(fmt.Sprintf("[Email] Dropping unauthorized sender at dispatch (unknown senders are ignored): %s", senderAddr))
		return false
	}
	if a.requireAuthenticatedSender && !msg.senderAuthenticated {
		authReason := msg.authReason
		if authReason == "" {
			authReason = "no verdict"
		}
		hint := dropHints[authReason]
		if isListed || (granted && hint != "") {
			pkgLog().Warn(fmt.Sprintf("[Email] Dropping sender with unauthenticated From: %s (%s).%s", senderAddr, authReason, hint))
		} else {
			kind := "unknown"
			if granted {
				kind = "authorized"
			}
			pkgLog().Debug(fmt.Sprintf("[Email] Dropping %s sender with unauthenticated From: %s (%s)", kind, senderAddr, authReason))
		}
		return false
	}
	return true
}

// Converts a fetched email into a message event and dispatches it
func (a *EmailAdapter) dispatchMessage(ctx context.Context, msg *fetchedMessage) error {
	senderAddr := msg.senderAddr
	if !a.senderAccepted(ctx, senderAddr, msg) {
		return nil
	}
	subject, body := msg.subject, strings.TrimSpace(msg.body)
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
	pkgLog().Info(fmt.Sprintf("[Email] New message from %s: %s", senderAddr, subject))
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
			text := res.err.Error()
			pkgLog().Error(fmt.Sprintf(logFmt, append(logArgs, text)...))
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

// Threaded reply skeleton with its Message-ID and subject, the body attached when present or asked for
func (a *EmailAdapter) newReply(toAddr, body, replyToMsgID string, attachEmptyBody bool) (*mailutil.Part, string, string) {
	msg := mailutil.NewMultipart()
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
	msgID := newMessageID(a.address)
	msg.Set("From", a.address)
	msg.Set("To", recipientField(toAddr))
	msg.Set("Subject", subject)
	if originalMsgID != "" {
		msg.Set("In-Reply-To", originalMsgID)
		msg.Set("References", originalMsgID)
	}
	msg.Set("Date", time.Now().Format(time.RFC1123Z))
	msg.Set("Message-ID", msgID)
	if body != "" || attachEmptyBody {
		msg.Attach(mailutil.NewText(body))
	}
	return msg, msgID, subject
}

// Logs in, sends and always releases the SMTP connection
func (a *EmailAdapter) smtpSend(ctx context.Context, msg *mailutil.Part) error {
	smtp, err := a.connectSMTP(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = smtp.Quit(ctx) }()
	if err := smtp.Login(ctx, a.address, a.password); err != nil {
		return err
	}
	return smtp.SendMessage(ctx, msg)
}

// Sends a text reply and returns its Message-ID
func (a *EmailAdapter) sendEmail(ctx context.Context, toAddr, body, replyToMsgID string) (string, error) {
	msg, msgID, subject := a.newReply(toAddr, body, replyToMsgID, true)
	if err := a.smtpSend(ctx, msg); err != nil {
		return "", err
	}
	pkgLog().Info(fmt.Sprintf("[Email] Sent reply to %s (subject: %s)", toAddr, subject))
	return msgID, nil
}

// One file to attach with the name it is sent under
type outgoingFile struct {
	path string
	name string
}

// Attaches a file as base64 application/octet-stream under the name
func attachFile(msg *mailutil.Part, path, filename string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	msg.Attach(mailutil.NewAttachment(filename, "application/octet-stream", data))
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
			pkgLog().Warn(fmt.Sprintf("[Email] Failed to attach %s: %s", file.path, err.Error()))
		}
	}
	if err := a.smtpSend(ctx, msg); err != nil {
		return "", err
	}
	return msgID, nil
}

// Sends an image URL as a line of the email body
func (a *EmailAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.Send(ctx, chatID, strings.TrimSpace(caption+"\n\nImage: "+imageURL), replyTo, nil)
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
		localPath, err := urlutil.FilePath(image.URL)
		if err == nil {
			_, err = os.Stat(localPath)
		}
		if err != nil {
			missing := localPath
			if missing == "" {
				missing = image.URL
			}
			pkgLog().Warn(fmt.Sprintf("[Email] Skipping missing image: %s", missing))
			continue
		}
		localPaths = append(localPaths, localPath)
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
			pkgLog().Error(fmt.Sprintf("[Email] Multi-image send failed, falling back: %s", res.err.Error()), "error", res.err)
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
		files[i] = outgoingFile{path: path, name: pathutil.Name(path)}
	}
	msgID, err := a.sendWithFiles(ctx, toAddr, body, files, true, "")
	if err != nil {
		return "", err
	}
	pkgLog().Info(fmt.Sprintf("[Email] Sent multi-attachment email to %s (%d files)", toAddr, len(filePaths)))
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
		name = pathutil.Name(filePath)
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
