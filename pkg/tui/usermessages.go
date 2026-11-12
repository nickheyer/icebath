package tui

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
)

const (
	rpcInvalidParams    = 4000
	rpcSessionNotFound  = 4001
	rpcNotDispatchable  = 4018
	rpcUnknownMethod    = -32601
	detailLimit         = 300
	detailEllipsisUnits = detailLimit - 1
)

var (
	stderrNoiseRe       = regexp.MustCompile(`^\[(?:lifecycle|startup|protocol|sidecar|spawn)\]`)
	stderrProblemRe     = regexp.MustCompile(`Traceback|\b[A-Z][A-Za-z]*(?:Error|Exception)\b:|CRITICAL|\[gateway-turn\]|\[gateway-exit\]|^gateway (?:absent|conflict|draining|inaccessible|incompatible|starting|unavailable): `)
	stderrExceptionRe   = regexp.MustCompile(`([A-Z][A-Za-z]*(?:Error|Exception)):`)
	versionSkewRe       = regexp.MustCompile(`Extra inputs are not permitted|^unknown method:`)
	unavailableMethodRe = regexp.MustCompile(`^unknown method: (\S+) \x{2014} not available on this Icebath backend$`)
	sessionNotFoundRe   = regexp.MustCompile(`(?i)session not found`)
	notConnectedRe      = regexp.MustCompile(`^gateway not (?:connected|running)\b`)
	timedOutRe          = regexp.MustCompile(`^request timed out after (\d+)s`)
	jsWhitespaceRunRe   = regexp.MustCompile(`[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)
	notMineRefusalRe    = regexp.MustCompile(`^skill command: use command\.dispatch for /|use command\.dispatch for /snapshot restore`)
	slashFallbackStopRe = regexp.MustCompile(`slash worker|timed out|not connected|not running`)
	slashCrashRe        = regexp.MustCompile(`slash worker (?:exited|closed pipe|start failed)`)
	slashCrashPrefixRe  = regexp.MustCompile(`^slash worker (?:exited|closed pipe:?|start failed:?)[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*`)
	missingKeyRe        = regexp.MustCompile(`^No API key configured for provider '([^']*)'`)
	bareErrorPrefixRe   = regexp.MustCompile(`^Error:[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*`)
)

// Code and message of a failed request, the code absent for errors that carry none
type rpcShape struct {
	code       int64
	hasCode    bool
	message    string
	hasMessage bool
	data       map[string]any
}

// Shape of a request failure, a gateway refusal with its code and data or any error with its message
func shapeOf(err error) rpcShape {
	var refusal *rpcError
	if errors.As(err, &refusal) {
		return rpcShape{code: refusal.Code, hasCode: refusal.HasCode, message: refusal.Message, hasMessage: true, data: refusal.Data}
	}
	if err != nil {
		return rpcShape{message: err.Error(), hasMessage: true}
	}
	return rpcShape{}
}

// Text cut to a number of UTF-16 units, the way a JavaScript slice cuts it
func sliceUnits(s string, n int) string {
	units := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// Length of text in UTF-16 units
func unitLen(s string) int {
	n := 0
	for _, r := range s {
		n += max(1, utf16.RuneLen(r))
	}
	return n
}

// Details line of raw text collapsed to one line and capped, empty for blank text
func detailLine(raw string) string {
	text := strings.TrimFunc(jsWhitespaceRunRe.ReplaceAllString(raw, " "), isJSSpace)
	if text == "" {
		return ""
	}
	if unitLen(text) > detailLimit {
		text = sliceUnits(text, detailEllipsisUnits) + "\u2026"
	}
	return tr("userMessages.details", text)
}

// Lines joined after dropping empty ones
func joinPresent(lines ...string) string {
	var kept []string
	for _, l := range lines {
		if l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// Notice of a backend that gave up, its exit code, last stderr line and the reconnect and logs advice
func backendGaveUp(code *int, lastLine string) string {
	title := tr("userMessages.backend.gaveUpTitle")
	if code != nil {
		title = tr("userMessages.backend.gaveUpTitleWithCode", strconv.Itoa(*code))
	}
	return joinPresent(title, detailLine(lastLine), tr("userMessages.backend.gaveUpReconnect"), tr("userMessages.backend.gaveUpLogs"))
}

// Last stderr line that is not lifecycle noise, false when none is left
func lastStderrLine(tail string) (string, bool) {
	last, found := "", false
	for _, l := range strings.Split(tail, "\n") {
		l = strings.TrimFunc(l, isJSSpace)
		if l != "" && !stderrNoiseRe.MatchString(l) {
			last, found = l, true
		}
	}
	return last, found
}

// Reconnect notice with its delay in whole seconds and the attempt when one is known
func backendReconnecting(attempt int, delayMs *float64) string {
	ms := 1000.0
	if delayMs != nil {
		ms = *delayMs
	}
	secs := strconv.Itoa(max(1, int(jsRound(ms/1000))))
	if attempt > 0 {
		return tr("userMessages.backend.reconnectingAttempt", secs, strconv.Itoa(attempt))
	}
	return tr("userMessages.backend.reconnecting", secs)
}

// Rounding half up as JavaScript Math.round does
func jsRound(x float64) float64 {
	f := float64(int64(x))
	if x < 0 && x != f {
		f--
	}
	if x-f >= 0.5 {
		return f + 1
	}
	return f
}

// Stderr line that reads as a failure worth an activity row
func stderrLooksLikeProblem(line string) bool { return stderrProblemRe.MatchString(line) }

// Activity row of a failing stderr line, naming the exception when the line has one
func stderrProblemActivity(line string) string {
	if m := stderrExceptionRe.FindStringSubmatch(line); m != nil {
		return tr("userMessages.backend.stderrProblemNamed", m[1])
	}
	return tr("userMessages.backend.stderrProblem")
}

// Failure caused by a client and backend of different versions, not a method the backend says it lacks
func isVersionSkewError(err error) bool {
	s := shapeOf(err)
	if unavailableMethodRe.MatchString(s.message) {
		return false
	}
	return (s.hasCode && s.code == rpcUnknownMethod) ||
		(s.hasCode && s.code == rpcInvalidParams && versionSkewRe.MatchString(s.message)) ||
		(!s.hasCode && versionSkewRe.MatchString(s.message))
}

// One wire error pattern and the copy it renders, the match groups handed to render
type rpcErrorRow struct {
	match  func(s rpcShape, text string) []string
	render func(m []string) string
}

// Match of a predicate as a matched row without groups
func matched(ok bool) []string {
	if ok {
		return []string{}
	}
	return nil
}

var rpcErrorRows = []rpcErrorRow{
	{func(s rpcShape, text string) []string {
		return matched((!s.hasCode || s.code == rpcSessionNotFound) && sessionNotFoundRe.MatchString(text))
	}, func([]string) string { return tr("userMessages.rpc.sessionNotFound") }},
	{func(_ rpcShape, text string) []string { return matched(text == "session_read_only") },
		func([]string) string { return tr("userMessages.rpc.sessionReadOnly") }},
	{func(_ rpcShape, text string) []string { return matched(text == "session_has_children") },
		func([]string) string { return tr("userMessages.rpc.sessionHasChildren") }},
	{func(_ rpcShape, text string) []string { return matched(notConnectedRe.MatchString(text)) },
		func([]string) string { return tr("userMessages.rpc.notConnected") }},
	{func(_ rpcShape, text string) []string { return unavailableMethodRe.FindStringSubmatch(text) },
		func(m []string) string { return tr("userMessages.rpc.methodUnavailable", groupOr(m, 1)) }},
	{func(_ rpcShape, text string) []string {
		return matched(strings.TrimFunc(text, isJSSpace) == "launch_credentials_unavailable")
	}, func([]string) string { return tr("userMessages.rpc.launchCredentialsChanged") }},
	{func(_ rpcShape, text string) []string { return timedOutRe.FindStringSubmatch(text) },
		func(m []string) string { return tr("userMessages.rpc.timedOut", groupOr(m, 1)) }},
}

// Match group i, a question mark when the row matched without groups
func groupOr(m []string, i int) string {
	if i < len(m) {
		return m[i]
	}
	return "?"
}

var (
	rpcLogMu   sync.Mutex
	rpcLogSink func(string)
)

// Installs the sink that receives the raw wire text a friendly message replaced
func setRPCErrorLogSink(sink func(string)) {
	rpcLogMu.Lock()
	rpcLogSink = sink
	rpcLogMu.Unlock()
}

// Records the raw wire text a friendly message replaced
func logReplacedWireText(s rpcShape, text string) {
	rpcLogMu.Lock()
	sink := rpcLogSink
	rpcLogMu.Unlock()
	if sink == nil {
		return
	}
	code := ""
	if s.hasCode {
		code = "code=" + strconv.FormatInt(s.code, 10) + " "
	}
	sink("[rpc] " + code + text)
}

// Plain copy of a request failure, the wire text passed through when no known pattern matches
func describeRPCError(err error) string {
	s := shapeOf(err)
	text := s.message
	if !s.hasMessage {
		text = tr("rpc.requestFailed")
	}
	if isVersionSkewError(err) {
		logReplacedWireText(s, text)
		return tr("userMessages.rpc.versionSkew")
	}
	return describeWireText(s, text)
}

// Plain copy of a failure given as bare text, which carries no code and is never read as version skew
func describeRPCErrorText(text string) string {
	if strings.TrimFunc(text, isJSSpace) == "" {
		text = tr("rpc.requestFailed")
	}
	return describeWireText(rpcShape{}, text)
}

// Copy of the first known wire pattern the text matches, the text itself otherwise
func describeWireText(s rpcShape, text string) string {
	for _, row := range rpcErrorRows {
		if m := row.match(s, text); m != nil {
			logReplacedWireText(s, text)
			return row.render(m)
		}
	}
	return text
}

// Refusal of a command the gateway does not support on this surface
func isUnsupportedCommand(err error) bool {
	return shapeOf(err).data["reason"] == "unsupported_command"
}

// Copy of a shell command failure, naming the terminal backend that refused it
func describeShellExecError(err error) string {
	data := shapeOf(err).data
	if data["reason"] == "unsupported_terminal_backend" {
		backend := ""
		if s, isString := data["detail"].(string); isString {
			backend = strings.TrimFunc(s, isJSSpace)
		}
		if backend != "" {
			return tr("userMessages.rpc.shellBackendRefused", backend)
		}
		return tr("userMessages.rpc.shellBackendRefusedUnnamed")
	}
	return describeRPCError(err)
}

// Copy of a slash command failure, an unsupported refusal, a helper timeout or crash, or the general copy
func describeSlashExecError(command string, err error) string {
	s := shapeOf(err)
	if isUnsupportedCommand(err) {
		refusal := tr("canonical.controls.notAvailable", command)
		if detail, isString := s.data["detail"].(string); isString && strings.TrimFunc(detail, isJSSpace) != "" {
			return refusal + "\n" + strings.TrimFunc(detail, isJSSpace)
		}
		return refusal
	}
	if strings.Contains(s.message, "slash worker timed out") {
		return tr("userMessages.rpc.slashTimedOut", command)
	}
	if slashCrashRe.MatchString(s.message) {
		return joinPresent(tr("userMessages.rpc.slashCrashed", command), detailLine(slashCrashPrefixRe.ReplaceAllString(s.message, "")))
	}
	return describeRPCError(err)
}

// Whether a slash.exec failure should go to command.dispatch instead
func shouldFallbackToDispatch(err error) bool {
	s := shapeOf(err)
	if s.hasCode && s.code == rpcNotDispatchable {
		return notMineRefusalRe.MatchString(s.message)
	}
	if s.hasCode {
		return false
	}
	return !slashFallbackStopRe.MatchString(s.message)
}

// Catalog key of each turn failure code
var turnCodeKey = map[string]string{
	"auth": "auth", "auth_permanent": "authPermanent", "billing": "billing", "billing_unverified": "billingUnverified",
	"content_policy_blocked": "contentPolicyBlocked", "context_overflow": "contextOverflow", "format_error": "formatError",
	"local_runtime_unavailable": "localRuntimeUnavailable", "model_not_found": "modelNotFound", "overloaded": "overloaded",
	"payload_too_large": "payloadTooLarge", "provider_policy_blocked": "providerPolicyBlocked", "rate_limit": "rateLimit",
	"server_error": "serverError", "ssl_cert_verification": "sslCertVerification", "timeout": "timeout",
	"upstream_blocked": "upstreamBlocked", "upstream_rate_limit": "upstreamRateLimit",
}

// Title and hints of a turn failure by code, then by layer, then the fallback
func turnCopyFor(code, layer string) (title, hint, hintNoRetry string) {
	base := "userMessages.turn.fallback"
	if key, known := turnCodeKey[code]; known && hasKey("userMessages.turn.code."+key+".title") {
		base = "userMessages.turn.code." + key
	} else if layer != "" && hasKey("userMessages.turn.layer."+layer+".title") {
		base = "userMessages.turn.layer." + layer
	}
	hint = tr(base + ".hint")
	hintNoRetry = hint
	if hasKey(base + ".hintNoRetry") {
		hintNoRetry = tr(base + ".hintNoRetry")
	}
	return tr(base + ".title"), hint, hintNoRetry
}

// English catalog has the key
func hasKey(key string) bool {
	if _, ok := enCatalog.Strings[key]; ok {
		return true
	}
	_, ok := enCatalog.Formats[key]
	return ok
}

// Copy of a failed turn, its title with the provider, the error details and the next step
func describeTurnFailure(payload map[string]any) string {
	surface, _ := payload["error_surface"].(map[string]any)
	code, _ := surface["code"].(string)
	layer, _ := surface["layer"].(string)
	title, hint, hintNoRetry := turnCopyFor(code, layer)
	if provider, isString := surface["provider"].(string); isString && provider != "" {
		title = tr("userMessages.turn.withProvider", title, provider)
	}
	nextStep := hint
	if surface["retryable"] == false || payload["recoverable"] == false {
		nextStep = hintNoRetry
	}
	raw, _ := payload["error"].(string)
	return joinPresent(tr("userMessages.turn.notAnswered", title), detailLine(bareErrorPrefixRe.ReplaceAllString(raw, "")), nextStep)
}

// Message text that only repeats the error, so the turn failure copy replaces it
func isBareErrorText(text, errText string) bool {
	t := strings.TrimFunc(text, isJSSpace)
	return t == "" || t == strings.TrimFunc("Error: "+errText, isJSSpace) || t == strings.TrimFunc(errText, isJSSpace)
}

// Catalog key of each prompt kind with a timeout notice
var promptTimeoutKey = map[string]string{
	"secret": "secret", "sudo": "sudo", "vault.code": "vaultCode", "vault.save_login": "vaultSaveLogin",
	"vault.unlock_prompt": "vaultUnlockPrompt",
}

// Notice of a password or vault prompt that closed on its timeout, false for any other reason
func promptTimeoutNotice(method, reason string) (string, bool) {
	key, known := promptTimeoutKey[method]
	if reason != "timeout" || !known {
		return "", false
	}
	return tr("userMessages.promptTimeout." + key), true
}

// Missing API key warning with the model switch fix, other warnings unchanged
func describeCredentialWarning(warning string) string {
	m := missingKeyRe.FindStringSubmatch(warning)
	if m == nil {
		return warning
	}
	provider := m[1]
	if provider == "" {
		provider = tr("userMessages.credential.currentProvider")
	}
	return tr("userMessages.credential.missingKey", provider)
}
