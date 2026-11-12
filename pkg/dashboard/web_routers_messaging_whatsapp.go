package dashboard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	whatsappOnboardingTTL     = 600
	whatsappSessionNotFound   = "WhatsApp setup session was not found. Start a new setup."
	whatsappSetupStillActive  = "Another WhatsApp QR setup is still active. Cancel it or wait for it to stop before re-pairing."
	whatsappNotConnectedYet   = "WhatsApp setup is not connected yet."
	whatsappExitedEarly       = "WhatsApp pairing process exited before pairing completed."
	whatsappPairingLockScope  = "whatsapp-session"
	whatsappTerminateWaitSecs = 3
)

var (
	whatsappOnboardingMu = sync.Mutex{}
	whatsappTerminal     = map[string]bool{"connected": true, "error": true, "expired": true, "cancelled": true}
)

// Pairing bridge of an onboarding session, the slice of subprocess.Popen the onboarding flow drives
type whatsappPairingProc interface {
	Stdout() io.Reader
	Poll() *int
	Wait(timeout time.Duration) (int, bool)
	Terminate()
}

// Pair only bridge running in this process with its JSON events on one pipe, its exit code recorded once it returns
type inProcessPairing struct {
	stop   context.CancelFunc
	stdout *os.File
	done   chan struct{}
	code   int
}

// Read end of the event pipe, at EOF once the bridge returned
func (p *inProcessPairing) Stdout() io.Reader { return p.stdout }

// Exit code once the bridge returned, negative when it was stopped, nil while it runs
func (p *inProcessPairing) Poll() *int {
	select {
	case <-p.done:
		code := p.code
		return &code
	default:
		return nil
	}
}

// Exit code after the bridge returns, false when a non negative timeout passes first
func (p *inProcessPairing) Wait(timeout time.Duration) (int, bool) {
	if timeout < 0 {
		<-p.done
		return p.code, true
	}
	select {
	case <-p.done:
		return p.code, true
	case <-time.After(timeout):
		return 0, false
	}
}

// Cancels the bridge context, which stops the bridge with the SIGTERM exit code
func (p *inProcessPairing) Terminate() { p.stop() }

// Lowercased pairing mode, 400 for anything but bot or self-chat
func normalizeWhatsappOnboardingMode(value any) (string, error) {
	if !pyrepr.Truthy(value) {
		value = "bot"
	}
	mode := pystr.Lower(pystr.Strip(pyrepr.Str(value)))
	if mode != "bot" && mode != "self-chat" {
		return "", NewHTTPException(400, "WhatsApp mode must be 'bot' or 'self-chat'.")
	}
	return mode, nil
}

// Comma joined allowlist without spaces or empty entries
func normalizeWhatsappAllowedUsers(value any) string {
	raw := pystr.Strip(truthyStr(value))
	if raw == "" {
		return ""
	}
	var parts []string
	for _, part := range strings.Split(raw, ",") {
		if pystr.Strip(part) != "" {
			parts = append(parts, strings.ReplaceAll(part, " ", ""))
		}
	}
	return strings.Join(parts, ",")
}

// Python str of a truthy value, empty for a falsy one
func truthyStr(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// Digits of the user part of a WhatsApp jid, nil when it holds none
func whatsappPhoneFromIdentifier(value any) any {
	raw := pystr.Strip(truthyStr(value))
	if raw == "" {
		return nil
	}
	user, _, _ := strings.Cut(raw, "@")
	user, _, _ = strings.Cut(user, ":")
	var digits strings.Builder
	for _, r := range user {
		if unicode.Is(unicode.Nd, r) {
			digits.WriteRune(r)
		}
	}
	if digits.Len() == 0 {
		return nil
	}
	return digits.String()
}

// First non empty stripped value of the keys in a dict candidate, nil otherwise
func firstStr(candidate any, keys ...string) any {
	obj, ok := candidate.(*pyjson.Object)
	if !ok {
		return nil
	}
	for _, k := range keys {
		if v := pystr.Strip(truthyStr(obj.GetOr(k, nil))); v != "" {
			return v
		}
	}
	return nil
}

// Account id, name and phone of the device linked in the session creds, all nil when unreadable
func whatsappLinkedAccountFromSession(sessionPath string) (accountID, accountName, accountPhone any, err error) {
	raw, readErr := os.ReadFile(filepath.Join(sessionPath, "creds.json"))
	if readErr != nil {
		return nil, nil, nil, nil
	}
	text, decodeErr := pystr.DecodeUTF8Sig(raw)
	if decodeErr != nil {
		return nil, nil, nil, nil
	}
	payload, loadErr := pyjson.Loads(text)
	if loadErr != nil {
		return nil, nil, nil, nil
	}
	obj, ok := payload.(*pyjson.Object)
	if !ok {
		return nil, nil, nil, attributeGetError(payload)
	}
	candidates := []any{obj.GetOr("me", nil), obj.GetOr("account", nil), obj}
	for _, c := range candidates {
		if accountID = firstStr(c, "id", "jid", "lid"); accountID != nil {
			break
		}
	}
	for _, c := range candidates {
		if accountName = firstStr(c, "name", "verifiedName", "notify", "pushName"); accountName != nil {
			break
		}
	}
	return accountID, accountName, whatsappPhoneFromIdentifier(accountID), nil
}

// Pairing transport of the dashboard flow, the in process QR pairing of the whatsapp package
var whatsappRunPairing = whatsapp.RunPairingEnv

// Pairing spawn, the module attribute hermes tests patch
var spawnWhatsappPairingProcess = spawnWhatsappPairingBridge

// Starts the pair only JSON bridge on the session dir in this process with its events on one pipe
func spawnWhatsappPairingBridge(ctx context.Context, sessionPath, mode string) (whatsappPairingProc, error) {
	if err := os.MkdirAll(sessionPath, 0o777); err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	runCtx, stop := context.WithCancel(ctx)
	p := &inProcessPairing{stop: stop, stdout: reader, done: make(chan struct{})}
	env := map[string]string{"WHATSAPP_MODE": mode, "WHATSAPP_DM_POLICY": "pairing"}
	go func() {
		defer close(p.done)
		defer stop()
		p.code = whatsappRunPairing(runCtx, sessionPath, mode, env, writer, true)
		writer.Close()
	}()
	return p, nil
}

// Stops a pairing bridge and waits out the grace for it to return
func terminateWhatsappPairing(proc whatsappPairingProc) {
	if proc == nil || proc.Poll() != nil {
		return
	}
	proc.Terminate()
	proc.Wait(whatsappTerminateWaitSecs * time.Second)
}

// Marks the pairing errored unless its status is in unless or another process superseded proc
func failWhatsappPairing(pairingID, message string, proc whatsappPairingProc, unless map[string]bool) {
	whatsappOnboardingMu.Lock()
	defer whatsappOnboardingMu.Unlock()
	record := whatsappOnboardingSessions.get(pairingID)
	if record != nil && (proc == nil || record.proc == proc) && !unless[record.status] {
		record.status = "error"
		record.err = message
	}
}

// Folds one bridge JSON event into the pairing, AttributeError for an event that is no dict
func applyPairingEvent(record *whatsappOnboardingSession, payload any) error {
	obj, ok := payload.(*pyjson.Object)
	if !ok {
		return attributeGetError(payload)
	}
	switch event := pystr.Strip(truthyStr(obj.GetOr("event", nil))); {
	case event == "qr":
		if qr := pystr.Strip(truthyStr(obj.GetOr("qr", nil))); qr != "" {
			record.qrPayload = qr
			record.status = "waiting"
			record.err = nil
		}
	case event == "connected":
		if user, isDict := obj.GetOr("user", nil).(*pyjson.Object); isDict {
			accountID := pystr.Strip(truthyStr(user.GetOr("id", nil)))
			record.accountID = noneIfEmpty(accountID)
			record.accountName = noneIfEmpty(pystr.Strip(truthyStr(user.GetOr("name", nil))))
			record.accountPhone = whatsappPhoneFromIdentifier(accountID)
		}
		record.status = "connected"
		record.err = nil
	case event == "error":
		message := obj.GetOr("error", nil)
		if !pyrepr.Truthy(message) {
			message = "WhatsApp pairing failed."
		}
		record.status = "error"
		record.err = pyrepr.Str(message)
	case event == "disconnected" && record.status == "starting":
		record.status = "waiting"
	}
	return nil
}

// First truthy profile selector, empty when none is set
func firstProfile(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// The string, nil when empty
func noneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Lines of a text stream split at LF, CR LF or CR, each decoded as UTF-8 with replacement
func universalLines(r io.Reader, yield func(string) bool) error {
	br := bufio.NewReader(r)
	var line []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			if len(line) > 0 {
				yield(pystr.DecodeUTF8Replace(line))
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch b {
		case '\n':
			if !yield(pystr.DecodeUTF8Replace(append(line, '\n'))) {
				return nil
			}
			line = line[:0]
		case '\r':
			if next, err := br.Peek(1); err == nil && next[0] == '\n' {
				br.ReadByte()
			}
			if !yield(pystr.DecodeUTF8Replace(append(line, '\n'))) {
				return nil
			}
			line = line[:0]
		default:
			line = append(line, b)
		}
	}
}

// Applies the bridge events of proc to its pairing, then records why the bridge exited before connecting
func watchWhatsappPairing(pairingID string, proc whatsappPairingProc) {
	superseded := false
	returncode, err := func() (int, error) {
		var eventErr error
		if out := proc.Stdout(); out != nil {
			if closer, ok := out.(io.Closer); ok {
				defer closer.Close()
			}
			readErr := universalLines(out, func(line string) bool {
				raw := pystr.Strip(line)
				if raw == "" {
					return true
				}
				payload, err := pyjson.Loads(raw)
				if err != nil {
					return true
				}
				whatsappOnboardingMu.Lock()
				defer whatsappOnboardingMu.Unlock()
				record := whatsappOnboardingSessions.get(pairingID)
				if record == nil || record.proc != proc {
					superseded = true
					return false
				}
				if record.status != "cancelled" && record.status != "expired" {
					if err := applyPairingEvent(record, payload); err != nil {
						eventErr = err
						return false
					}
				}
				return true
			})
			if superseded {
				return 0, nil
			}
			if eventErr != nil {
				return 0, eventErr
			}
			if readErr != nil {
				return 0, readErr
			}
		}
		code, _ := proc.Wait(-1)
		return code, nil
	}()
	if superseded {
		return
	}
	if err != nil {
		failWhatsappPairing(pairingID, pyErrText(err), proc, whatsappTerminal)
		return
	}
	message := fmt.Sprintf("WhatsApp pairing process exited with code %d.", returncode)
	if returncode == 0 {
		message = whatsappExitedEarly
	}
	failWhatsappPairing(pairingID, message, proc, map[string]bool{"connected": true, "cancelled": true, "expired": true})
}

// Worker of one pairing, spawning its bridge and watching it until exit, the reservation released after
func runWhatsappPairing(ctx context.Context, pairingID, sessionPath, mode string) {
	whatsappOnboardingMu.Lock()
	record := whatsappOnboardingSessions.get(pairingID)
	if record == nil || whatsappTerminal[record.status] {
		if record != nil {
			releaseWhatsappReservation(record)
		}
		whatsappOnboardingMu.Unlock()
		return
	}
	record.status = "installing"
	record.startingProcess = true
	whatsappOnboardingMu.Unlock()
	defer func() {
		whatsappOnboardingMu.Lock()
		defer whatsappOnboardingMu.Unlock()
		record.startingProcess = false
		releaseWhatsappReservation(record)
	}()
	proc, err := spawnWhatsappPairingProcess(ctx, sessionPath, mode)
	if err != nil {
		failWhatsappPairing(pairingID, pyErrText(err), nil, whatsappTerminal)
		return
	}
	whatsappOnboardingMu.Lock()
	record.startingProcess = false
	record.proc = proc
	if whatsappTerminal[record.status] {
		terminateWhatsappPairing(proc)
		whatsappOnboardingMu.Unlock()
		return
	}
	record.status = "starting"
	whatsappOnboardingMu.Unlock()
	watchWhatsappPairing(pairingID, proc)
}

// Releases the session lock once no writer may still touch the session, called under the onboarding lock
func releaseWhatsappReservation(record *whatsappOnboardingSession) {
	if !record.sessionReserved || record.startingProcess {
		return
	}
	if record.proc == nil && !whatsappTerminal[record.status] {
		return
	}
	if record.proc != nil && record.proc.Poll() == nil {
		return
	}
	gateway.ReleaseScopedLock(whatsappPairingLockScope, record.sessionPath)
	record.sessionReserved = false
	if record.expiryTimer != nil {
		record.expiryTimer.Stop()
	}
}

// Expires a pairing that is still live or reserved, stopping its bridge, called under the onboarding lock
func expireWhatsappPairingLocked(record *whatsappOnboardingSession) {
	if !whatsappTerminal[record.status] || record.sessionReserved {
		record.status = "expired"
		record.err = "WhatsApp QR setup expired. Start a new setup."
		terminateWhatsappPairing(record.proc)
	}
	releaseWhatsappReservation(record)
}

// Expiry timer callback taking the onboarding lock
func expireWhatsappPairing(record *whatsappOnboardingSession) {
	whatsappOnboardingMu.Lock()
	defer whatsappOnboardingMu.Unlock()
	expireWhatsappPairingLocked(record)
}

// Current time in epoch seconds
func epochNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Expires overdue pairings, flags dead bridges and drops long finished sessions, called under the onboarding lock
func pruneWhatsappOnboardingSessions() {
	now := epochNow()
	var remove []string
	for _, pairingID := range whatsappOnboardingSessions.order() {
		record := whatsappOnboardingSessions.get(pairingID)
		releaseWhatsappReservation(record)
		live := !whatsappTerminal[record.status]
		if live && record.proc != nil && record.proc.Poll() != nil {
			record.status = "error"
			record.err = whatsappExitedEarly
		}
		if record.expiresAtTS <= now {
			expireWhatsappPairingLocked(record)
		}
		if whatsappTerminal[record.status] && !record.sessionReserved && record.expiresAtTS+300 <= now {
			remove = append(remove, pairingID)
		}
	}
	for _, pairingID := range remove {
		whatsappOnboardingSessions.pop(pairingID)
	}
}

// Pairing id generator, the secrets attribute hermes tests patch
var whatsappTokenURLSafe = tokenURLSafe

// Stores the record under a fresh id, cancelling live sessions on the same dir, called under the onboarding lock
func registerWhatsappSession(sessionPath string, record *whatsappOnboardingSession) string {
	pairingID := whatsappTokenURLSafe(16)
	pruneWhatsappOnboardingSessions()
	for _, id := range whatsappOnboardingSessions.order() {
		existing := whatsappOnboardingSessions.get(id)
		if existing.sessionPath == sessionPath && !whatsappTerminal[existing.status] {
			existing.status = "cancelled"
			existing.err = "Superseded by a newer WhatsApp setup session."
			terminateWhatsappPairing(existing.proc)
		}
	}
	whatsappOnboardingSessions.set(pairingID, record)
	return pairingID
}

// Takes the session lock before keys move or a writer starts, 409 while another setup or owner holds it
func reserveWhatsappSession(ctx context.Context, record *whatsappOnboardingSession) error {
	busy := "WhatsApp is in use. Disable WhatsApp and restart the gateway, then try re-pairing again."
	for _, id := range whatsappOnboardingSessions.order() {
		existing := whatsappOnboardingSessions.get(id)
		if existing.sessionPath == record.sessionPath && (!whatsappTerminal[existing.status] ||
			existing.sessionReserved || existing.startingProcess || (existing.proc != nil && existing.proc.Poll() == nil)) {
			return NewHTTPException(409, whatsappSetupStillActive)
		}
	}
	acquired, previous, err := gateway.AcquireScopedLock(ctx, whatsappPairingLockScope, record.sessionPath,
		pyjson.ObjectOf("owner_kind", "process"))
	if err != nil {
		return err
	}
	if !acquired || (previous != nil && previous.Len() > 0 && pyrepr.Equal(previous.GetOr("pid", nil), os.Getpid())) {
		return NewHTTPException(409, busy)
	}
	record.sessionReserved = true
	return nil
}

// Expiry timer and pairing worker starters, the threading attributes hermes tests patch
var (
	whatsappExpiryTimer = func(d time.Duration, fn func()) whatsappTimer { return time.AfterFunc(d, fn) }
	whatsappStartWorker = func(ctx context.Context, pairingID, sessionPath, mode string) error {
		go runWhatsappPairing(ctx, pairingID, sessionPath, mode)
		return nil
	}
)

// Arms the expiry timer and starts the pairing worker, the reservation released when either fails to start
func launchWhatsappPairing(ctx context.Context, pairingID string, record *whatsappOnboardingSession, sessionPath string) error {
	record.expiryTimer = whatsappExpiryTimer(whatsappOnboardingTTL*time.Second, func() { expireWhatsappPairing(record) })
	if err := whatsappStartWorker(context.WithoutCancel(ctx), pairingID, sessionPath, record.mode); err != nil {
		record.status = "error"
		releaseWhatsappReservation(record)
		return err
	}
	return nil
}

// Starts a QR pairing for the scoped profile, or reports the device already linked in its session
func startWhatsappOnboarding(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	mode, err := normalizeWhatsappOnboardingMode(body.GetOr("mode", nil))
	if err != nil {
		return nil, err
	}
	allowedUsers := normalizeWhatsappAllowedUsers(body.GetOr("allowed_users", nil))
	targetProfile := firstProfile(body.GetOr("profile", nil), profile)
	sctx, _, err := configProfileScope(ctx, targetProfile)
	if err != nil {
		return nil, err
	}
	whatsappOnboardingMu.Lock()
	defer whatsappOnboardingMu.Unlock()
	pruneWhatsappOnboardingSessions()
	sessionPath := whatsappSessionPath(sctx)
	for _, id := range whatsappOnboardingSessions.order() {
		if r := whatsappOnboardingSessions.get(id); r.sessionPath == sessionPath && r.sessionReserved {
			return nil, NewHTTPException(409, whatsappSetupStillActive)
		}
	}
	expiresAtTS := epochNow() + whatsappOnboardingTTL
	expiry, err := pydatetime.FromTimestampIn(expiresAtTS, time.UTC, pydatetime.LocaltimeAccepts)
	if err != nil {
		return nil, err
	}
	record := &whatsappOnboardingSession{mode: mode, allowedUsers: allowedUsers, sessionPath: sessionPath,
		expiresAt: strings.Replace(pydatetime.ISOFormatUTC(expiry), "+00:00", "Z", 1), expiresAtTS: expiresAtTS,
		profile: targetProfile, status: "starting"}
	resetSession := body.GetOr("reset_session", false) == true
	_, credsErr := os.Stat(filepath.Join(sessionPath, "creds.json"))
	alreadyLinked := !resetSession && credsErr == nil
	if alreadyLinked {
		accountID, accountName, accountPhone, err := whatsappLinkedAccountFromSession(sessionPath)
		if err != nil {
			return nil, err
		}
		record.status, record.accountID, record.accountName, record.accountPhone = "connected", accountID, accountName, accountPhone
		return whatsappOnboardingPayload(registerWhatsappSession(sessionPath, record), record), nil
	}
	if err := reserveWhatsappSession(sctx, record); err != nil {
		return nil, err
	}
	pairingID, err := func() (string, error) {
		if resetSession {
			if err := backupWhatsappSession(sessionPath); err != nil {
				return "", err
			}
		}
		pairingID := registerWhatsappSession(sessionPath, record)
		return pairingID, launchWhatsappPairing(sctx, pairingID, record, sessionPath)
	}()
	if err != nil {
		record.status = "error"
		releaseWhatsappReservation(record)
		return nil, err
	}
	return whatsappOnboardingPayload(pairingID, record), nil
}

// Pairing of an id after pruning, 404 when unknown, called under the onboarding lock
func whatsappRecordOr404(pairingID string) (*whatsappOnboardingSession, error) {
	pruneWhatsappOnboardingSessions()
	record := whatsappOnboardingSessions.get(pairingID)
	if record == nil {
		return nil, NewHTTPException(404, whatsappSessionNotFound)
	}
	return record, nil
}

// Status payload of a pairing, 410 once it expired
func getWhatsappOnboardingStatus(pairingID string) (*pyjson.Object, error) {
	whatsappOnboardingMu.Lock()
	defer whatsappOnboardingMu.Unlock()
	record, err := whatsappRecordOr404(pairingID)
	if err != nil {
		return nil, err
	}
	if record.status == "expired" {
		detail := any("WhatsApp setup expired.")
		if pyrepr.Truthy(record.err) {
			detail = record.err
		}
		return nil, NewHTTPException(410, detail)
	}
	return whatsappOnboardingPayload(pairingID, record), nil
}

// Waits for a connected bridge to flush its credentials and release the session, 409 while it still writes
func waitWhatsappPairingFlush(pairingID string) error {
	whatsappOnboardingMu.Lock()
	record, err := whatsappRecordOr404(pairingID)
	if err == nil && record.status != "connected" {
		err = NewHTTPException(409, whatsappNotConnectedYet)
	}
	reserved, proc := false, whatsappPairingProc(nil)
	if err == nil {
		reserved, proc = record.sessionReserved, record.proc
	}
	whatsappOnboardingMu.Unlock()
	if err != nil {
		return err
	}
	if reserved && proc != nil {
		if _, exited := proc.Wait(whatsappTerminateWaitSecs * time.Second); !exited {
			return NewHTTPException(409, "WhatsApp is still saving its linked device. Wait a moment and try Save and restart again.")
		}
	}
	whatsappOnboardingMu.Lock()
	defer whatsappOnboardingMu.Unlock()
	releaseWhatsappReservation(record)
	if record.sessionReserved {
		return NewHTTPException(409, "WhatsApp is still finishing pairing. Wait a moment and try Save and restart again.")
	}
	return nil
}

// Saves a connected pairing into the profile env and config, then restarts the gateway
func applyWhatsappOnboarding(ctx context.Context, pairingID string, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	if err := waitWhatsappPairingFlush(pairingID); err != nil {
		return nil, err
	}
	whatsappOnboardingMu.Lock()
	record, mode, allowedUsers, err := func() (*whatsappOnboardingSession, string, string, error) {
		record, err := whatsappRecordOr404(pairingID)
		if err != nil {
			return nil, "", "", err
		}
		if record.status != "connected" {
			return nil, "", "", NewHTTPException(409, whatsappNotConnectedYet)
		}
		requestedMode := body.GetOr("mode", nil)
		if !pyrepr.Truthy(requestedMode) {
			requestedMode = record.mode
		}
		mode, err := normalizeWhatsappOnboardingMode(requestedMode)
		if err != nil {
			return nil, "", "", err
		}
		requestedUsers := body.GetOr("allowed_users", nil)
		if requestedUsers == nil {
			requestedUsers = record.allowedUsers
		}
		allowedUsers := normalizeWhatsappAllowedUsers(requestedUsers)
		if mode == "self-chat" && allowedUsers == "" {
			allowedUsers = truthyStr(record.accountPhone)
			if allowedUsers == "" {
				allowedUsers = truthyStr(record.accountID)
			}
		}
		return record, mode, allowedUsers, nil
	}()
	whatsappOnboardingMu.Unlock()
	if err != nil {
		return nil, err
	}
	effectiveProfile := firstProfile(body.GetOr("profile", nil), profile, record.profile)
	err = func() error {
		sctx, _, err := configProfileScope(ctx, effectiveProfile)
		if err != nil {
			return err
		}
		h := home.Dir(sctx)
		if err := config.SaveEnvValueContext(sctx, h, "WHATSAPP_MODE", mode); err != nil {
			return err
		}
		if err := config.SaveEnvValueContext(sctx, h, "WHATSAPP_DM_POLICY", "pairing"); err != nil {
			return err
		}
		if allowedUsers != "" {
			if err := config.SaveEnvValueContext(sctx, h, "WHATSAPP_ALLOWED_USERS", allowedUsers); err != nil {
				return err
			}
		}
		if err := config.SaveEnvValueContext(sctx, h, "WHATSAPP_ENABLED", "true"); err != nil {
			return err
		}
		return writePlatformEnabled(sctx, "whatsapp", true)
	}()
	if err := onboardingSaveErrors(err, "WhatsApp onboarding apply failed", "Failed to save WhatsApp setup."); err != nil {
		return nil, err
	}
	whatsappOnboardingMu.Lock()
	if record.proc == nil || record.proc.Poll() != nil {
		whatsappOnboardingSessions.pop(pairingID)
	}
	whatsappOnboardingMu.Unlock()
	restartResult, err := restartGatewayAfterWhatsappOnboarding(ctx, effectiveProfile)
	if err != nil {
		return nil, err
	}
	out := pyjson.ObjectOf("ok", true, "platform", "whatsapp", "needs_restart", !pyrepr.Truthy(restartResult.GetOr("restart_started", nil)))
	for _, k := range restartResult.Keys {
		out.Set(k, restartResult.Vals[k])
	}
	return out, nil
}

// Cancels a pairing and stops its bridge, keeping it listed while its worker is still starting
func cancelWhatsappOnboarding(pairingID string) *pyjson.Object {
	whatsappOnboardingMu.Lock()
	defer whatsappOnboardingMu.Unlock()
	if record := whatsappOnboardingSessions.get(pairingID); record != nil {
		record.status = "cancelled"
		terminateWhatsappPairing(record.proc)
		releaseWhatsappReservation(record)
		if !record.startingProcess && (record.proc == nil || record.proc.Poll() != nil) {
			whatsappOnboardingSessions.pop(pairingID)
		}
	}
	return pyjson.ObjectOf("ok", true)
}
