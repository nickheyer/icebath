package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	needsReauthMsg = "MCP server '%[1]s' requires re-authentication.%[2]s Run `icebath mcp login %[1]s` (or delete the tokens file under " +
		"ICEBATH_HOME/mcp-tokens/ and restart). Do NOT retry this tool — ask the user to re-authenticate."
	stdioNoRespawnMsg = "MCP server '%s' stdio subprocess had exited (this is not a timeout — the call never reached the server). A " +
		"respawn was requested but no fresh session came back within %.0fs. Wait a few seconds before retrying; if it " +
		"keeps failing the server is not starting and needs the user."
	stdioDiedAgainMsg = "MCP server '%s' respawned its stdio subprocess and it exited again immediately. The server is not starting " +
		"cleanly — do NOT retry this tool; ask the user to check the server's command and its stderr log."
	stdioOutcomeUncertainMsg = "MCP server '%s' lost its stdio subprocess after the tool call began. The operation may have completed, so " +
		"icebath did not replay it. Do NOT retry automatically; inspect the external state first."
	sessionOutcomeUncertainMsg = "The MCP transport session to '%s' expired while this write-capable call was in flight, so the outcome is " +
		"UNKNOWN — the operation may or may not have taken effect server-side. It was NOT automatically retried to " +
		"avoid a duplicate side effect. The connection has %s. Verify whether the operation took effect (e.g. " +
		"with a read-only tool) before re-invoking it."
)

// Approval card argument budget that keeps the whole card inside the tightest button surface message cap
const (
	trustGateArgumentsMaxChars         = 1200
	trustGateArgumentsTruncationMarker = "\n... [arguments truncated]"
)

// Drops cached tool check verdicts whenever an application declaration changes
func init() { onAppDeclarationChange(tools.InvalidateCheckCache) }

// Respawn wait of a dead stdio child, swapped by tests
var stdioRespawnWait = 15 * time.Second

// Reconnect wait after OAuth recovery or session expiry
const recoveryReconnectWait = 15 * time.Second

// OAuth 401 recovery bound on the loop
const authRecoveryTimeout = 10 * time.Second

// OAuth manager 401 recovery the auth recoverer drives, swapped by tests
var oauthHandle401 = func(ctx context.Context, serverName string) (bool, error) {
	return getManager().handle401(ctx, serverName, "")
}

// Approval prompt the trust gate asks through, swapped by tests
var requestTrustConsent = tools.RequestElicitationConsent

// RuntimeError is a call aborted by a deliberate reconnect teardown, retryable or outcome uncertain by its text
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string { return e.Msg }

// Stdio subprocess gone when or while a call ran, in flight when the call had been dispatched
type stdioChildExited struct {
	msg      string
	inFlight bool
	cause    error
}

func (e *stdioChildExited) Error() string { return e.msg }

func (e *stdioChildExited) Unwrap() error { return e.cause }

// Error payload with the bounded message first
func toolError(message string) string { return toolErrorWith(message) }

// Error payload with the bounded message first and the extra key value pairs after it in order
func toolErrorWith(message string, extra ...any) string {
	p := &orderedPayload{}
	p.set("error", tools.BoundErrorText(message))
	for i := 0; i+1 < len(extra); i += 2 {
		p.set(extra[i].(string), extra[i+1])
	}
	return tools.JSON(p)
}

// Context carrying the env profile home so key resolution matches the calling profile
func envContext(env *tools.Env) context.Context {
	ctx := context.Background()
	if env != nil && env.Home != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	return ctx
}

// True only when discovery captured readOnlyHint true for the tool on the calling connection
func toolIsReadOnly(ctx context.Context, serverName, toolName string) bool {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	return toolReadOnlyHints[resolveServerKey(ctx, serverName, "", true)][toolName]
}

// Approval key a session or always answer to the trust gate is remembered under
func trustApprovalKey(serverName, toolName string) string {
	return fmt.Sprintf("mcp:%s/%s", serverName, toolName)
}

// Remembered session or always answer, or approval bypass for the calling session
func trustGranted(env *tools.Env, serverName, toolName string) bool {
	return tools.IsApprovalBypassActive(env) || tools.IsApproved(env, tools.CurrentSessionKey(env), trustApprovalKey(serverName, toolName))
}

var caseFolder = cases.Fold()

// Credential field name or API key header name
func isSecretArgumentName(key string) bool {
	if tools.IsSecretFieldName(key) {
		return true
	}
	folded := caseFolder.String(key)
	for _, name := range tools.SecretHeaderNameList {
		if folded == name {
			return true
		}
	}
	return false
}

// Masks every non empty value under a credential named field at any depth
func maskSecretFields(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if item != nil && item != "" && isSecretArgumentName(k) {
				out[k] = "***"
			} else {
				out[k] = maskSecretFields(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = maskSecretFields(item)
		}
		return out
	}
	return value
}

// Object key order of a JSON document as emitted, so displays follow the model's argument order
type keyOrder struct {
	keys     []string
	children map[string]*keyOrder
	items    []*keyOrder
}

// Key order tree of raw JSON text, nil when the text is empty or does not decode
func parseKeyOrder(raw string) *keyOrder {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	o, err := readKeyOrder(dec)
	if err != nil {
		return nil
	}
	return o
}

// Reads one JSON value and records the first position of every object key like a Python dict
func readKeyOrder(dec *json.Decoder) (*keyOrder, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil, nil
	}
	o := &keyOrder{}
	if d == '{' {
		o.children = map[string]*keyOrder{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			child, err := readKeyOrder(dec)
			if err != nil {
				return nil, err
			}
			if _, dup := o.children[k]; !dup {
				o.keys = append(o.keys, k)
			}
			o.children[k] = child
		}
	} else {
		for dec.More() {
			child, err := readKeyOrder(dec)
			if err != nil {
				return nil, err
			}
			o.items = append(o.items, child)
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

// Keys of m in emitted order, keys the raw text never named follow sorted
func (o *keyOrder) ordered(m map[string]any) []string {
	if o == nil {
		return sortedKeys(m)
	}
	out := make([]string, 0, len(m))
	seen := make(map[string]bool, len(o.keys))
	for _, k := range o.keys {
		if _, ok := m[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	for _, k := range sortedKeys(m) {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

// Order tree of one object member, nil when unknown
func (o *keyOrder) child(k string) *keyOrder {
	if o == nil {
		return nil
	}
	return o.children[k]
}

// Order tree of one array item, nil when unknown
func (o *keyOrder) item(i int) *keyOrder {
	if o == nil || i >= len(o.items) {
		return nil
	}
	return o.items[i]
}

// Writes a value as json.dumps with indent 2, raw non ASCII, str for unknown types and keys in emitted order
func pyIndentJSON(b *strings.Builder, value any, level int, order *keyOrder) {
	pad := func(n int) { b.WriteString("\n" + strings.Repeat("  ", n)) }
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(pyJSONString(v))
	case json.Number:
		b.WriteString(string(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case float64:
		b.WriteString(pyJSONFloat(v))
	case map[string]any:
		if len(v) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range order.ordered(v) {
			if i > 0 {
				b.WriteByte(',')
			}
			pad(level + 1)
			b.WriteString(pyJSONString(k) + ": ")
			pyIndentJSON(b, v[k], level+1, order.child(k))
		}
		pad(level)
		b.WriteByte('}')
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			pad(level + 1)
			pyIndentJSON(b, item, level+1, order.item(i))
		}
		pad(level)
		b.WriteByte(']')
	default:
		b.WriteString(pyJSONString(pyStr(v)))
	}
}

// Float the way json.dumps writes it, integral values as the int the decoder saw
func pyJSONFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == math.Trunc(f) && math.Abs(f) < 1e16:
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return pyFloatRepr(f)
}

// Sorted keys of a decoded object
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Python unicode_escape spelling of one control or format character
func unicodeEscape(r rune) string {
	switch {
	case r < 0x100:
		return fmt.Sprintf(`\x%02x`, r)
	case r < 0x10000:
		return fmt.Sprintf(`\u%04x`, r)
	}
	return fmt.Sprintf(`\U%08x`, r)
}

// Redacted, bounded JSON of the call arguments for the approval card with hidden characters escaped
func formatTrustGateArguments(env *tools.Env, args map[string]any, rawArgs string) string {
	var b strings.Builder
	pyIndentJSON(&b, maskSecretFields(args), 0, parseKeyOrder(rawArgs))
	runes := []rune(b.String())
	window := 4 * trustGateArgumentsMaxChars
	cut := len(runes) > window
	if cut {
		runes = runes[:window]
	}
	var esc strings.Builder
	for _, r := range runes {
		if r != '\n' && (unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)) {
			esc.WriteString(unicodeEscape(r))
		} else {
			esc.WriteRune(r)
		}
	}
	rendered := tools.RedactSensitiveTextWith(env, esc.String(), tools.RedactOptions{Force: true})
	if cut || utf8.RuneCountInString(rendered) > trustGateArgumentsMaxChars {
		keep := trustGateArgumentsMaxChars - utf8.RuneCountInString(trustGateArgumentsTruncationMarker)
		rendered = strings.TrimRightFunc(string([]rune(rendered)[:keep]), unicode.IsSpace) + trustGateArgumentsTruncationMarker
	}
	return rendered
}

// Approval gate for write capable tools on untrusted servers plus the declared spend limit, empty to proceed
func trustGateCheck(ctx context.Context, env *tools.Env, serverName, toolName string, booked *[]*Charge, args map[string]any, rawArgs string) string {
	key := serverKey(ctx, serverName, "", true)
	mcpMu.Lock()
	trust, ok := serverTrustLevels[key]
	_, costed := serverCostPolicies[key]
	mcpMu.Unlock()
	if !ok {
		trust = TrustFull
	}
	gated := trust == TrustUntrusted && !toolIsReadOnly(ctx, serverName, toolName) && !trustGranted(env, serverName, toolName)
	var charge *Charge
	if costed {
		charge = PendingCharge(ctx, key, serverName, toolName, tools.CurrentSessionKey(env))
	}
	if charge == nil {
		if gated {
			return askForCall(ctx, env, serverName, toolName, nil, booked, true, args, rawArgs)
		}
		return ""
	}
	if !gated {
		if reserved, _ := charge.ReserveWithinLimit(); reserved {
			*booked = append(*booked, charge)
			return ""
		}
	}
	release := charge.Asking()
	defer release()
	return askForCall(ctx, env, serverName, toolName, charge, booked, gated, args, rawArgs)
}

// Consent answer, or an error when the approval system panicked
func askConsent(ctx context.Context, env *tools.Env, message, description string, o tools.ElicitationOptions) (answer string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return requestTrustConsent(ctx, env, message, description, o), nil
}

// Asks the user about one gated or costed call and books the charge only on an explicit yes
func askForCall(ctx context.Context, env *tools.Env, serverName, toolName string, charge *Charge, booked *[]*Charge, gated bool, args map[string]any, rawArgs string) string {
	spent, over := 0.0, false
	if charge != nil {
		if gated {
			spent = charge.Spent()
			over = charge.OverLimit(spent)
		} else {
			var reserved bool
			reserved, spent = charge.ReserveWithinLimit()
			if reserved {
				*booked = append(*booked, charge)
				return ""
			}
			over = true
		}
	}
	var extra []string
	if charge != nil {
		extra = charge.Lines(spent)
	}
	var message, description, title, rememberKey string
	if gated {
		parts := []string{fmt.Sprintf("MCP tool '%s' on UNTRUSTED server '%s' wants to run.", toolName, serverName)}
		if over {
			parts = append(parts, "This call goes past the chat's spend limit.")
		}
		parts = append(parts, extra...)
		parts = append(parts, "This tool is write-capable (no readOnlyHint=true annotation) and may modify external state.")
		message = strings.Join(parts, " ")
		if len(args) > 0 {
			message += "\n\nArguments:\n" + formatTrustGateArguments(env, args, rawArgs)
		}
		scope := "once, for this session, or always"
		if over {
			scope = "once"
		} else {
			rememberKey = trustApprovalKey(serverName, toolName)
		}
		description = fmt.Sprintf("Server '%s' is configured 'trust: untrusted'. Approve to run '%s' %s, or deny to block it.", serverName, toolName, scope)
		title = fmt.Sprintf("MCP server '%s' is asking", serverName)
	} else {
		parts := append([]string{fmt.Sprintf("MCP tool '%s' on server '%s' would take this chat past its spend limit.", toolName, serverName)}, extra...)
		message = strings.Join(parts, " ")
		description = fmt.Sprintf("Server '%s' declares a spend limit per chat. Approve to run '%s' once (every further paid call in this chat asks again), or deny to block it.", serverName, toolName)
		title = fmt.Sprintf("MCP server '%s' spend limit", serverName)
	}
	answer, err := askConsent(ctx, env, message, description, tools.ElicitationOptions{Surface: "mcp-trust/" + serverName, Title: title, RememberKey: rememberKey})
	if err != nil {
		mcpLogger().Error(fmt.Sprintf("MCP trust gate: approval check failed for %s.%s: %s", serverName, toolName, err))
		if over && charge != nil {
			return toolError(charge.Refusal(spent))
		}
		return toolError(fmt.Sprintf("MCP tool '%s' on untrusted server '%s' was blocked: the approval system was unavailable (fail-closed).", toolName, serverName))
	}
	if answer == "accept" {
		if charge != nil {
			charge.Book()
			*booked = append(*booked, charge)
		}
		return ""
	}
	verb, kind := "denied", "spend-limited"
	if answer == "cancel" {
		verb = "cancelled"
	}
	if gated {
		kind = "untrusted"
	}
	mcpLogger().Info(fmt.Sprintf("MCP trust gate: user %s '%s' on %s server '%s'", verb, toolName, kind, serverName))
	if over && charge != nil {
		return toolError(charge.Refusal(spent))
	}
	return toolError(fmt.Sprintf("The user did not approve running write-capable MCP tool '%s' on untrusted server '%s'. The command was NOT run. Do not retry without explicit user direction.", toolName, serverName))
}

// Open breaker error, empty when calls may proceed, half open once the cooldown passed
func checkCircuitBreaker(ctx context.Context, serverName string) string {
	mcpMu.Lock()
	key := resolveServerKey(ctx, serverName, "", true)
	failures := serverErrorCounts[key]
	openedAt, opened := serverBreakerOpenedAt[key]
	allApplication := serverErrorsAllApplication[key]
	mcpMu.Unlock()
	if failures < circuitBreakerThreshold || !opened {
		return ""
	}
	age := managerNow().Sub(openedAt)
	if age >= circuitBreakerCooldown {
		return ""
	}
	retryIn := max(1, int((circuitBreakerCooldown - age).Seconds()))
	if allApplication {
		return toolError(fmt.Sprintf("MCP server '%s' rejected the last %d calls (it is reachable; see the error text those calls returned). "+
			"Paused for ~%ds. Do NOT repeat the same call — fix the arguments/URL/target or use a different approach.", serverName, failures, retryIn))
	}
	return toolError(fmt.Sprintf("MCP server '%s' is unreachable after %d consecutive failures. Auto-retry available in ~%ds. Do NOT retry "+
		"this tool yet — use alternative approaches or ask the user to check the MCP server.", serverName, failures, retryIn))
}

// Nil for an empty string so a Python None renders as null
func noneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Server a call may dispatch to, else the error, waiting briefly for a reconnect before a breaker strike
func acquireCallServer(ctx context.Context, env *tools.Env, serverName string, toolTimeout float64) (*serverTask, string) {
	notConnected := toolError(fmt.Sprintf("MCP server '%s' is not connected", serverName))
	if decl, current, sentence, ok := unavailableDetails(ctx, serverName); ok {
		app := &orderedPayload{}
		app.set("name", decl.Name)
		app.set("version", noneIfEmpty(current.Availability.Version))
		app.set("path", noneIfEmpty(current.Availability.Path))
		notConnected = toolErrorWith(sentence, "server", serverName, "state", current.State, "app", app,
			"user_action", noneIfEmpty(current.UserAction), "retry", noneIfEmpty(current.Retry))
	}
	srv := getConnectedServerForCall(ctx, env, serverName)
	wait := 5.0
	if toolTimeout != 0 {
		wait = math.Min(5.0, toolTimeout)
	}
	if srv != nil && (srv.currentSession() != nil || waitForServerSessionReady(srv, nil, secondsDuration(wait))) {
		return srv, ""
	}
	bumpServerError(ctx, serverName, false)
	if srv != nil && signalReconnect(srv) {
		return nil, toolError(fmt.Sprintf("MCP server '%s' transport is down; reconnect requested. Do NOT retry this "+
			"tool immediately — give it a few seconds to come back.", serverName))
	}
	return nil, notConnected
}

// True only for a JSON payload carrying an error key, the way the Python in operator reads the decoded value
func resultIsError(result string) bool {
	var decoded any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		return false
	}
	switch v := decoded.(type) {
	case map[string]any:
		_, ok := v["error"]
		return ok
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == "error" {
				return true
			}
		}
	case string:
		return strings.Contains(v, "error")
	}
	return false
}

// Breaker bookkeeping where a tool error payload still strikes, flagged as an application error
func recordCallOutcome(ctx context.Context, serverName, result string) string {
	if resultIsError(result) {
		bumpServerError(ctx, serverName, true)
	} else {
		resetServerError(ctx, serverName)
	}
	return result
}

// Breaker strike plus the error payload
func strike(ctx context.Context, serverName, message string, extra ...any) string {
	bumpServerError(ctx, serverName, false)
	return toolErrorWith(message, extra...)
}

// Registered server that can be signalled to reconnect, with requireLoop also needing a running loop
func lookupReconnectableServer(ctx context.Context, serverName string, requireLoop bool) *serverTask {
	mcpMu.Lock()
	srv := mcpServers[resolveServerKey(ctx, serverName, "", true)]
	mcpMu.Unlock()
	if srv == nil || (requireLoop && runningLoop() == nil) {
		return nil
	}
	return srv
}

// One retry after a recovery step, false when the retry itself failed
func retryOnce(ctx context.Context, serverName string, retry func() (string, error), op, what string) (string, bool) {
	result, err := retry()
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP %s/%s retry after %s failed: %s", serverName, op, what, excStr(err)))
		return "", false
	}
	return recordCallOutcome(ctx, serverName, result), true
}

// recoverer handles one failure kind, false when the error is not its kind
type recoverer func(ctx context.Context, serverName string, err error, retry func() (string, error), op string) (string, bool)

// OAuth recovery through the manager then one retry, the needs_reauth error when recovery or retry fails
func handleAuthErrorAndRetry(ctx context.Context, serverName string, err error, retry func() (string, error), op string) (string, bool) {
	if !isAuthError(err) {
		return "", false
	}
	recovered, recErr := runOnLoop(ctx, authRecoveryTimeout, func(c context.Context) (bool, error) {
		return oauthHandle401(c, serverName)
	})
	if recErr != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP OAuth '%s': recovery attempt failed: %s", serverName, excStr(recErr)))
		recovered = false
	}
	if recovered {
		if srv := lookupReconnectableServer(ctx, serverName, false); srv != nil &&
			signalReconnectAndWait(serverName, srv, op+" after OAuth recovery", recoveryReconnectWait) {
			resetServerError(ctx, serverName)
		}
		if result, ok := retryOnce(ctx, serverName, retry, op, "auth recovery"); ok {
			return result, true
		}
	}
	return strike(ctx, serverName, fmt.Sprintf(needsReauthMsg, serverName, authErrorDetail(err)), "needs_reauth", true, "server", serverName), true
}

// Session expiry recoverer that reconnects the transport and replays only calls without side effects
func sessionExpiredRecoverer(callMayHaveSideEffects bool) recoverer {
	return func(ctx context.Context, serverName string, err error, retry func() (string, error), op string) (string, bool) {
		return handleSessionExpiredAndRetry(ctx, serverName, err, retry, op, callMayHaveSideEffects)
	}
}

// Transport reconnect plus one retry on session expiry, an outcome uncertain error for write capable calls
func handleSessionExpiredAndRetry(ctx context.Context, serverName string, err error, retry func() (string, error), op string, callMayHaveSideEffects bool) (string, bool) {
	if !isSessionExpiredError(err) {
		return "", false
	}
	srv := lookupReconnectableServer(ctx, serverName, true)
	if callMayHaveSideEffects {
		reconnected := srv != nil && signalReconnectAndWait(serverName, srv, op+" (write, no auto-retry)", recoveryReconnectWait)
		outcome, state := "failed", "not recovered yet"
		if reconnected {
			resetServerError(ctx, serverName)
			outcome, state = "succeeded", "been re-established"
		} else {
			bumpServerError(ctx, serverName, false)
		}
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': %s failed with a session-expired/transport error after the request may "+
			"have been dispatched; NOT auto-retrying a write-capable tool (reconnect %s).", serverName, op, outcome))
		return toolErrorWith(fmt.Sprintf(sessionOutcomeUncertainMsg, serverName, state), "outcome_uncertain", true, "server", serverName), true
	}
	if srv == nil {
		return "", false
	}
	mcpLogger().Info(fmt.Sprintf("MCP server '%s': %s failed with session-expired error (%s); signalling transport reconnect "+
		"and retrying once.", serverName, op, excStr(err)))
	if !signalReconnectAndWait(serverName, srv, op, recoveryReconnectWait) {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': reconnect did not ready within 15s after session-expired error; "+
			"falling through to error response.", serverName))
		return "", false
	}
	return retryOnce(ctx, serverName, retry, op, "session reconnect")
}

// Respawns a dead stdio child and retries once only when it died before dispatch
func handleStdioChildExitedAndRetry(ctx context.Context, serverName string, err error, retry func() (string, error), op string) (string, bool) {
	var exited *stdioChildExited
	if !errors.As(err, &exited) {
		return "", false
	}
	reconnected := false
	if srv := lookupReconnectableServer(ctx, serverName, false); srv != nil {
		action := "respawning and retrying once"
		if exited.inFlight {
			action = "reconnecting without replay"
		}
		mcpLogger().Info(fmt.Sprintf("MCP server '%s': %s found the stdio subprocess dead (%s); %s.", serverName, op, exited.msg, action))
		if runningLoop() != nil {
			reconnected = signalReconnectAndWait(serverName, srv, op, stdioRespawnWait)
		} else {
			signalReconnect(srv)
		}
	}
	if exited.inFlight {
		return strike(ctx, serverName, fmt.Sprintf(stdioOutcomeUncertainMsg, serverName), "outcome_uncertain", true), true
	}
	if !reconnected {
		return strike(ctx, serverName, fmt.Sprintf(stdioNoRespawnMsg, serverName, stdioRespawnWait.Seconds())), true
	}
	result, retryErr := retry()
	if retryErr == nil {
		return recordCallOutcome(ctx, serverName, result), true
	}
	var again *stdioChildExited
	if errors.As(retryErr, &again) {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s': %s stdio subprocess exited again right after respawn (%s); not retrying further.",
			serverName, op, again.msg))
		if again.inFlight {
			return strike(ctx, serverName, fmt.Sprintf(stdioOutcomeUncertainMsg, serverName), "outcome_uncertain", true), true
		}
		return strike(ctx, serverName, fmt.Sprintf(stdioDiedAgainMsg, serverName)), true
	}
	mcpLogger().Warn(fmt.Sprintf("MCP %s/%s retry after stdio respawn failed: %s", serverName, op, excStr(retryErr)))
	return strike(ctx, serverName, sanitizeError(fmt.Sprintf("MCP call failed after respawning the stdio subprocess for '%s': %s: %s",
		serverName, errTypeName(retryErr), excStr(retryErr)))), true
}

// Runs call on the loop and walks the recoverers on failure, breaker bookkeeping on the first attempt when asked
func dispatch(ctx context.Context, serverName string, srv *serverTask, call func(ctx context.Context) (string, error), toolTimeout float64,
	op string, recoverers []recoverer, onFinalFailure func(error), recordOutcome bool) string {
	if srv != nil {
		srv.markToolCall()
	}
	callOnce := func() (string, error) { return runOnLoop(ctx, secondsDuration(toolTimeout), call) }
	result, err := callOnce()
	if err == nil {
		if recordOutcome {
			return recordCallOutcome(ctx, serverName, result)
		}
		return result
	}
	if errors.Is(err, ErrInterrupted) {
		return toolError("MCP call interrupted: user sent a new message")
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return toolError(mcpCallFailedMessage(err))
	}
	for _, rec := range recoverers {
		if recovered, ok := rec(ctx, serverName, err, callOnce, op); ok {
			return recovered
		}
	}
	onFinalFailure(err)
	return toolError(mcpCallFailedMessage(err))
}

// Utility dispatch with the auth and session expiry recoverers, logging the final failure
func dispatchUtilityCall(ctx context.Context, serverName string, srv *serverTask, op, label string, toolTimeout float64,
	call func(ctx context.Context) (string, error)) string {
	return dispatch(ctx, serverName, srv, call, toolTimeout, op, []recoverer{handleAuthErrorAndRetry, sessionExpiredRecoverer(false)},
		func(err error) {
			mcpLogger().Error(fmt.Sprintf("MCP %s/%s failed: %s", serverName, label, excStr(err)))
		}, false)
}

// Error a session less server gives a call that reached it between reconnects
func noSessionError(serverName string) error {
	return fmt.Errorf("MCP server '%s' has no active session", serverName)
}

// Tool call failing fast when the stdio child is dead before dispatch or dies mid call
func callToolRacingStdioDeath(ctx context.Context, srv *serverTask, serverName, toolName string, args map[string]any, progress ProgressFunc) (*CallToolResult, error) {
	if srv.stdioChildrenDead() {
		return nil, &stdioChildExited{msg: fmt.Sprintf("MCP stdio subprocess for '%s' had already exited when the call was dispatched", serverName)}
	}
	sess := srv.currentSession()
	if sess == nil {
		return nil, noSessionError(serverName)
	}
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		result *CallToolResult
		err    error
	}
	rpcDone := make(chan outcome, 1)
	watchDone := make(chan bool, 1)
	go func() {
		r, err := sess.CallTool(raceCtx, toolName, args, progress)
		rpcDone <- outcome{r, err}
	}()
	go func() { watchDone <- srv.watchStdioChildren(raceCtx) }()
	defer func() {
		cancel()
		<-watchDone
	}()
	var out outcome
	select {
	case out = <-rpcDone:
	case dead := <-watchDone:
		watchDone <- dead
		if dead {
			select {
			case out = <-rpcDone:
			default:
				cancel()
				<-rpcDone
				return nil, &stdioChildExited{msg: fmt.Sprintf("MCP stdio subprocess for '%s' exited mid-call", serverName), inFlight: true}
			}
		} else {
			out = <-rpcDone
		}
	}
	if out.err != nil && !srv.isHTTP() && isSessionExpiredError(out.err) {
		return nil, &stdioChildExited{msg: fmt.Sprintf("MCP stdio subprocess for '%s' closed its transport mid-call", serverName), inFlight: true, cause: out.err}
	}
	return out.result, out.err
}

// Handler calling one MCP tool through the trust gate, breaker, cost charge and recovery ladder
func makeToolHandler(serverName, toolName string, toolTimeout float64, nativeImages bool) tools.Handler {
	op := "tools/call " + toolName
	return func(ctx context.Context, call *tools.Call) string {
		env := call.Env
		var booked []*Charge
		if errText := trustGateCheck(ctx, env, serverName, toolName, &booked, call.Args, call.RawArgs); errText != "" {
			return errText
		}
		var srv *serverTask
		errText := checkCircuitBreaker(ctx, serverName)
		if errText == "" {
			srv, errText = acquireCallServer(ctx, env, serverName, toolTimeout)
		}
		if srv == nil {
			for _, charge := range booked {
				charge.Refund()
			}
			return errText
		}
		readOnly := toolIsReadOnly(ctx, serverName, toolName)
		var imgMu sync.Mutex
		var imagePaths []string
		var report ProgressFunc
		if relay := tools.CurrentToolProgress(ctx); relay != nil {
			report = ProgressFunc(relay)
		}
		callFn := func(c context.Context) (string, error) {
			if err := srv.rpc.Lock(c); err != nil {
				return "", err
			}
			result, err := func() (*CallToolResult, error) {
				defer srv.rpc.Unlock()
				callCtx, cancel := context.WithCancel(c)
				defer cancel()
				untrack := srv.trackInflight(cancel)
				defer untrack()
				srv.mu.Lock()
				srv.pendingCallContext = withCallEnv(ctx, env)
				srv.mu.Unlock()
				defer func() {
					srv.mu.Lock()
					srv.pendingCallContext = nil
					srv.mu.Unlock()
				}()
				r, err := callToolRacingStdioDeath(callCtx, srv, serverName, toolName, call.Args, report)
				if err != nil && errors.Is(err, context.Canceled) && c.Err() == nil && srv.isReconnecting() {
					advice := "the request may already have been dispatched, so verify its effect before re-invoking"
					if readOnly {
						advice = "retry the request on the rebuilt session"
					}
					err = &RuntimeError{Msg: fmt.Sprintf("MCP %s on '%s' was aborted by a reconnect teardown; %s", op, serverName, advice)}
				}
				return r, err
			}()
			if err != nil {
				return "", err
			}
			srv.markSessionProven()
			paths := []string{}
			text := RenderCallToolResult(tools.WithEnvHome(c, env), env, result, serverName, &paths)
			imgMu.Lock()
			imagePaths = paths
			imgMu.Unlock()
			return text, nil
		}
		onFailure := func(err error) {
			bumpServerError(ctx, serverName, false)
			mcpLogger().Error(fmt.Sprintf("MCP tool %s/%s call failed: %s", serverName, toolName, excStr(err)))
		}
		text := dispatch(ctx, serverName, srv, callFn, toolTimeout, op,
			[]recoverer{handleStdioChildExitedAndRetry, handleAuthErrorAndRetry, sessionExpiredRecoverer(!readOnly)}, onFailure, true)
		if !nativeImages || resultIsError(text) {
			return text
		}
		imgMu.Lock()
		paths := imagePaths
		imgMu.Unlock()
		switch v := resultWithNativeImages(ctx, env, call.SessionID, text, paths).(type) {
		case string:
			return v
		default:
			return tools.JSON(v)
		}
	}
}

// utilityRPC runs one utility request on the session under the rpc lock
type utilityRPC func(ctx context.Context, sess *Session, args map[string]any, serverName string) (any, error)

// utilityRender turns a utility result into the tool JSON payload
type utilityRender func(ctx context.Context, env *tools.Env, result any, serverName string) (any, error)

// Factory of one utility handler that validates the required argument before any transport work
func makeUtilityHandler(op, label string, rpc utilityRPC, render utilityRender, required string) func(serverName string, toolTimeout float64) tools.Handler {
	return func(serverName string, toolTimeout float64) tools.Handler {
		return func(ctx context.Context, call *tools.Call) string {
			srv := getConnectedServerForCall(ctx, call.Env, serverName)
			if srv == nil || srv.currentSession() == nil {
				return toolError(fmt.Sprintf("MCP server '%s' is not connected", serverName))
			}
			args := call.Args
			if args == nil {
				args = map[string]any{}
			}
			if required != "" && !pyTruthy(args[required]) {
				return toolError(fmt.Sprintf("Missing required parameter '%s'", required))
			}
			return dispatchUtilityCall(ctx, serverName, srv, op, label, toolTimeout, func(c context.Context) (string, error) {
				if err := srv.rpc.Lock(c); err != nil {
					return "", err
				}
				result, err := func() (any, error) {
					defer srv.rpc.Unlock()
					sess := srv.currentSession()
					if sess == nil {
						return nil, noSessionError(serverName)
					}
					return rpc(c, sess, args, serverName)
				}()
				if err != nil {
					return "", err
				}
				payload, err := render(tools.WithEnvHome(c, call.Env), call.Env, result, serverName)
				if err != nil {
					return "", err
				}
				return tools.JSON(payload), nil
			})
		}
	}
}

// Resource list entries with uri, name, and description and mimeType when set
func renderResourceList(ctx context.Context, env *tools.Env, result any, serverName string) (any, error) {
	resources := []any{}
	for _, r := range result.([]*Resource) {
		entry := &orderedPayload{}
		entry.set("uri", r.URI)
		entry.set("name", r.Name)
		if r.Description != "" {
			entry.set("description", r.Description)
		}
		if r.MimeType != "" {
			entry.set("mimeType", r.MimeType)
		}
		resources = append(resources, entry)
	}
	p := &orderedPayload{}
	p.set("resources", resources)
	return p, nil
}

// Resource contents joined by newline, binary blobs cached as documents
func renderReadResource(ctx context.Context, env *tools.Env, result any, serverName string) (any, error) {
	var parts []string
	for _, block := range result.(*ReadResourceResult).Contents {
		switch {
		case block.Text != nil:
			parts = append(parts, tools.StripUnicodeTags(*block.Text))
		case block.Blob != nil:
			rendered := renderResourceBlock(tools.WithEnvHome(ctx, env), env, &ContentBlock{Type: "resource", Resource: block}, serverName)
			if rendered == "" {
				rendered = fmt.Sprintf("[binary data, %d bytes]", utf8.RuneCountInString(*block.Blob))
			}
			parts = append(parts, rendered)
		}
	}
	p := &orderedPayload{}
	p.set("result", strings.Join(parts, "\n"))
	return p, nil
}

// Prompt list entries with name, description when set and the declared arguments
func renderPromptList(ctx context.Context, env *tools.Env, result any, serverName string) (any, error) {
	prompts := []any{}
	for _, pr := range result.([]*Prompt) {
		entry := &orderedPayload{}
		entry.set("name", pr.Name)
		if pr.Description != "" {
			entry.set("description", pr.Description)
		}
		if len(pr.Arguments) > 0 {
			argsOut := make([]any, 0, len(pr.Arguments))
			for _, a := range pr.Arguments {
				ae := &orderedPayload{}
				ae.set("name", a.Name)
				if a.Description != "" {
					ae.set("description", a.Description)
				}
				var req any
				if a.Required != nil {
					req = *a.Required
				}
				ae.set("required", req)
				argsOut = append(argsOut, ae)
			}
			entry.set("arguments", argsOut)
		}
		prompts = append(prompts, entry)
	}
	p := &orderedPayload{}
	p.set("prompts", prompts)
	return p, nil
}

// Prompt messages with role and text content, plus the description when set
func renderGetPrompt(ctx context.Context, env *tools.Env, result any, serverName string) (any, error) {
	r := result.(*GetPromptResult)
	messages := []any{}
	for i, msg := range r.Messages {
		entry := &orderedPayload{}
		entry.set("role", msg.Role)
		text, err := promptContentStr(msg.Content, fmt.Sprintf("messages.%d.content", i))
		if err != nil {
			return nil, err
		}
		entry.set("content", tools.StripUnicodeTags(text))
		messages = append(messages, entry)
	}
	p := &orderedPayload{}
	p.set("messages", messages)
	if r.Description != "" {
		p.set("description", r.Description)
	}
	return p, nil
}

// Field of an SDK content model in declaration order with its camel alias and value kind
type modelField struct {
	name, alias, kind string
	required          bool
}

var (
	annotationsFields = []modelField{{"audience", "audience", "any", false}, {"priority", "priority", "float", false}, {"last_modified", "lastModified", "str", false}}
	iconFields        = []modelField{{"src", "src", "str", true}, {"mime_type", "mimeType", "str", false}, {"sizes", "sizes", "any", false}, {"theme", "theme", "str", false}}
	mediaFields       = []modelField{{"type", "type", "str", false}, {"data", "data", "str", true}, {"mime_type", "mimeType", "str", true}, {"annotations", "annotations", "annotations", false}, {"meta", "_meta", "any", false}}
	embeddedFields    = []modelField{{"type", "type", "str", false}, {"resource", "resource", "resource", true}, {"annotations", "annotations", "annotations", false}, {"meta", "_meta", "any", false}}
	resourceLinkField = []modelField{{"name", "name", "str", true}, {"title", "title", "str", false}, {"uri", "uri", "str", true}, {"description", "description", "str", false},
		{"mime_type", "mimeType", "str", false}, {"size", "size", "int", false}, {"icons", "icons", "icons", false}, {"annotations", "annotations", "annotations", false},
		{"meta", "_meta", "any", false}, {"type", "type", "str", false}}
)

// Prompt content as the SDK str of its model, text content as its text
func promptContentStr(raw json.RawMessage, loc string) (string, error) {
	fields, err := orderedObject(raw)
	if err != nil {
		return "", promptValidationError(loc, "Input should be a valid dictionary or instance of a model", "model_type")
	}
	var kind string
	if t, ok := fields.get("type"); ok {
		_ = json.Unmarshal(t, &kind)
	}
	switch kind {
	case "text":
		t, ok := fields.get("text")
		var text string
		if !ok || json.Unmarshal(t, &text) != nil {
			return "", promptValidationError(loc+".text.text", "Field required", "missing")
		}
		return text, nil
	case "image", "audio":
		return modelStr(fields, mediaFields, loc+"."+kind, " ")
	case "resource":
		return modelStr(fields, embeddedFields, loc+".resource", " ")
	case "resource_link":
		return modelStr(fields, resourceLinkField, loc+".resource_link", " ")
	}
	return "", promptValidationError(loc, fmt.Sprintf("Input tag %s found using 'type' does not match any of the expected tags: "+
		"'text', 'image', 'audio', 'resource_link', 'resource'", pyReprStr(kind)), "union_tag_invalid")
}

// Validation failure of a prompts/get result
func promptValidationError(loc, msg, typ string) error {
	return &ValidationError{Model: "GetPromptResult", Errors: []FieldError{{Loc: loc, Msg: msg, Type: typ}}}
}

// JSON object members in document order
type orderedFields struct {
	keys []string
	vals map[string]json.RawMessage
}

// Member by alias
func (o orderedFields) get(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Decodes one JSON object keeping member order
func orderedObject(raw json.RawMessage) (orderedFields, error) {
	out := orderedFields{vals: map[string]json.RawMessage{}}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return out, errors.New("not an object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return out, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return out, err
		}
		k := kt.(string)
		if _, seen := out.vals[k]; !seen {
			out.keys = append(out.keys, k)
		}
		out.vals[k] = v
	}
	return out, nil
}

// Pydantic str or repr body of a model, its fields as name=repr joined by sep
func modelStr(fields orderedFields, spec []modelField, loc, sep string) (string, error) {
	parts := make([]string, 0, len(spec))
	for _, f := range spec {
		raw, ok := fields.get(f.alias)
		if !ok {
			raw, ok = fields.get(f.name)
		}
		if !ok || string(raw) == "null" {
			if f.required {
				return "", promptValidationError(loc+"."+f.alias, "Field required", "missing")
			}
			parts = append(parts, f.name+"=None")
			continue
		}
		v, err := fieldRepr(raw, f, loc+"."+f.alias)
		if err != nil {
			return "", err
		}
		parts = append(parts, f.name+"="+v)
	}
	return strings.Join(parts, sep), nil
}

// Repr of one model field value by its declared kind
func fieldRepr(raw json.RawMessage, f modelField, loc string) (string, error) {
	switch f.kind {
	case "str":
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", promptValidationError(loc, "Input should be a valid string", "string_type")
		}
		return pyReprStr(s), nil
	case "float":
		var x float64
		if json.Unmarshal(raw, &x) != nil {
			return "", promptValidationError(loc, "Input should be a valid number", "float_type")
		}
		return pyFloatRepr(x), nil
	case "int":
		var x float64
		if json.Unmarshal(raw, &x) != nil || x != math.Trunc(x) {
			return "", promptValidationError(loc, "Input should be a valid integer", "int_type")
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case "annotations":
		sub, err := orderedObject(raw)
		if err != nil {
			return "", promptValidationError(loc, "Input should be a valid dictionary or instance of Annotations", "model_type")
		}
		body, err := modelStr(sub, annotationsFields, loc, ", ")
		return "Annotations(" + body + ")", err
	case "resource":
		sub, err := orderedObject(raw)
		if err != nil {
			return "", promptValidationError(loc, "Input should be a valid dictionary", "model_type")
		}
		base := []modelField{{"uri", "uri", "str", true}, {"mime_type", "mimeType", "str", false}, {"meta", "_meta", "any", false}}
		if _, ok := sub.get("text"); ok {
			body, err := modelStr(sub, append(base, modelField{"text", "text", "str", true}), loc+".TextResourceContents", ", ")
			return "TextResourceContents(" + body + ")", err
		}
		body, err := modelStr(sub, append(base, modelField{"blob", "blob", "str", true}), loc+".BlobResourceContents", ", ")
		return "BlobResourceContents(" + body + ")", err
	case "icons":
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return "", promptValidationError(loc, "Input should be a valid list", "list_type")
		}
		reprs := make([]string, 0, len(items))
		for i, item := range items {
			sub, err := orderedObject(item)
			if err != nil {
				return "", promptValidationError(fmt.Sprintf("%s.%d", loc, i), "Input should be a valid dictionary or instance of Icon", "model_type")
			}
			body, err := modelStr(sub, iconFields, fmt.Sprintf("%s.%d", loc, i), ", ")
			if err != nil {
				return "", err
			}
			reprs = append(reprs, "Icon("+body+")")
		}
		return "[" + strings.Join(reprs, ", ") + "]", nil
	}
	return jsonPyRepr(raw)
}

// Python repr of a decoded JSON value with object members in document order
func jsonPyRepr(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "null":
		return "None", nil
	case trimmed == "true":
		return "True", nil
	case trimmed == "false":
		return "False", nil
	case strings.HasPrefix(trimmed, `"`):
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return pyReprStr(s), nil
	case strings.HasPrefix(trimmed, "["):
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return "", err
		}
		reprs := make([]string, 0, len(items))
		for _, item := range items {
			r, err := jsonPyRepr(item)
			if err != nil {
				return "", err
			}
			reprs = append(reprs, r)
		}
		return "[" + strings.Join(reprs, ", ") + "]", nil
	case strings.HasPrefix(trimmed, "{"):
		obj, err := orderedObject(raw)
		if err != nil {
			return "", err
		}
		reprs := make([]string, 0, len(obj.keys))
		for _, k := range obj.keys {
			r, err := jsonPyRepr(obj.vals[k])
			if err != nil {
				return "", err
			}
			reprs = append(reprs, pyReprStr(k)+": "+r)
		}
		return "{" + strings.Join(reprs, ", ") + "}", nil
	}
	if !strings.ContainsAny(trimmed, ".eE") {
		return trimmed, nil
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return "", err
	}
	return pyFloatRepr(f), nil
}

// Prompt arguments as the string map prompts/get takes, a validation error for any other shape
func promptArguments(args map[string]any) (map[string]string, error) {
	raw, ok := args["arguments"]
	if !ok {
		return map[string]string{}, nil
	}
	m, isMap := raw.(map[string]any)
	if !isMap {
		return nil, &ValidationError{Model: "GetPromptRequestParams", Errors: []FieldError{{Loc: "arguments", Msg: "Input should be a valid dictionary", Type: "dict_type"}}}
	}
	out := make(map[string]string, len(m))
	var errs []FieldError
	for _, k := range sortedKeys(m) {
		s, isStr := m[k].(string)
		if !isStr {
			errs = append(errs, FieldError{Loc: "arguments." + k, Msg: "Input should be a valid string", Type: "string_type"})
			continue
		}
		out[k] = s
	}
	if len(errs) > 0 {
		return nil, &ValidationError{Model: "GetPromptRequestParams", Errors: errs}
	}
	return out, nil
}

// Required string argument of a utility request, a validation error for any other type
func stringArgument(args map[string]any, key, model string) (string, error) {
	s, ok := args[key].(string)
	if !ok {
		return "", &ValidationError{Model: model, Errors: []FieldError{{Loc: key, Msg: "Input should be a valid string", Type: "string_type"}}}
	}
	return s, nil
}

var (
	makeListResourcesHandler = makeUtilityHandler("resources/list", "list_resources",
		func(ctx context.Context, sess *Session, args map[string]any, serverName string) (any, error) {
			return sess.ListResources(ctx)
		}, renderResourceList, "")
	makeReadResourceHandler = makeUtilityHandler("resources/read", "read_resource",
		func(ctx context.Context, sess *Session, args map[string]any, serverName string) (any, error) {
			uri, err := stringArgument(args, "uri", "ReadResourceRequestParams")
			if err != nil {
				return nil, err
			}
			return sess.ReadResource(ctx, uri)
		}, renderReadResource, "uri")
	makeListPromptsHandler = makeUtilityHandler("prompts/list", "list_prompts",
		func(ctx context.Context, sess *Session, args map[string]any, serverName string) (any, error) {
			return sess.ListPrompts(ctx)
		}, renderPromptList, "")
	makeGetPromptHandler = makeUtilityHandler("prompts/get", "get_prompt",
		func(ctx context.Context, sess *Session, args map[string]any, serverName string) (any, error) {
			name, err := stringArgument(args, "name", "GetPromptRequestParams")
			if err != nil {
				return nil, err
			}
			promptArgs, err := promptArguments(args)
			if err != nil {
				return nil, err
			}
			return sess.GetPrompt(ctx, name, promptArgs)
		}, renderGetPrompt, "name")
)

// Connection alive check where lazy servers count, gated on the declared application being offerable
func makeCheckFn(serverName string) func(*tools.Env) bool {
	return func(env *tools.Env) bool {
		if !serverConnected(envContext(env), serverName) {
			return false
		}
		return declaredAppOfferable(serverName)
	}
}

// True when the calling profile's connection has a session or a recycled stdio slot, or is lazy registered
func serverConnected(ctx context.Context, serverName string) bool {
	mcpMu.Lock()
	key := resolveServerKey(ctx, serverName, "", true)
	srv := mcpServers[key]
	_, lazy := lazyServerConfigs[key]
	mcpMu.Unlock()
	return (srv != nil && (srv.currentSession() != nil || srv.isRecycledStdio())) || lazy
}

// True unless a registered application declaration is unavailable on this host
func declaredAppOfferable(serverName string) bool {
	apps := platformApps()
	decl := apps.Declaration(serverName)
	if decl == nil {
		return true
	}
	state := apps.Availability(decl).State
	return state == "available" || state == "no_requirements"
}
