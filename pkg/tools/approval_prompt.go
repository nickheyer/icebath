package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Inputs of the interactive dangerous command prompt
type promptOptions struct {
	command, description string
	timeout              time.Duration
	allowPermanent       bool
	allowSession         bool
	smartDenied          bool
	title                string
	approvalCallback     Asker
}

// One answer of the prompt, cause explains a cancelled value that never reached a human
type promptChoice struct {
	value string
	cause string
}

// Cancelled answer carrying why nobody answered
func unanswered(cause string) promptChoice { return promptChoice{value: "cancelled", cause: cause} }

var cliChoiceAliases = map[string]string{"o": "once", "once": "once", "s": "session", "session": "session", "a": "always", "always": "always"}

// Prompts the user to approve a dangerous command and returns once, session, always, deny, timeout, or cancelled
func promptDangerousApproval(ctx context.Context, env *Env, o promptOptions) promptChoice {
	if o.timeout <= 0 {
		o.timeout = time.Duration(getApprovalTimeout(env)) * time.Second
	}
	human := humanInputRequest(ctx, env, "approval", o.command, "")
	release := humanWaitWindow(env, "")
	choice := askHuman(ctx, env, o)
	release()
	human.finish(ctx, env, choice.value)
	return choice
}

// Session asker as the codex approval prompt answering once, session, always or deny, nil without an asker
func CodexApprovalPrompt(env *Env) func(command, description string, allowPermanent bool) (string, error) {
	if env == nil || env.Ask == nil {
		return nil
	}
	return func(command, description string, allowPermanent bool) (string, error) {
		choice := promptDangerousApproval(context.Background(), env, promptOptions{command: command, description: description, allowPermanent: allowPermanent, allowSession: true})
		return choice.value, nil
	}
}

// Running full screen prompt applications, the prompt_toolkit get_app_or_none check of the input fallback
var promptAppsRunning atomic.Int32

// Marks a full screen prompt application as owning the terminal until the returned release runs
func EnterPromptApp() func() {
	promptAppsRunning.Add(1)
	var once sync.Once
	return func() { once.Do(func() { promptAppsRunning.Add(-1) }) }
}

// Prints the prompt and reads one stdin line a byte at a time, end of input or a read error giving what was read
var readTerminalChoice = func(prompt string) string {
	fmt.Print(prompt)
	flushStdout()
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return string(line)
			}
			line = append(line, buf[0])
		}
		if err != nil {
			return string(line)
		}
	}
}

// Confirmation line of each terminal menu decision
var cliChoiceI18n = map[string]string{"once": "approval.allowed_once", "session": "approval.allowed_session",
	"always": "approval.allowed_always", "deny": "approval.denied"}

// Terminal menu of the dangerous command prompt answered on stdin, the input fallback when no callback is registered
func askTerminal(ctx context.Context, env *Env, o promptOptions, displayCommand, displayDescription string, onceOnly bool) promptChoice {
	os.Setenv("ICEBATH_SPINNER_PAUSE", "1")
	defer os.Unsetenv("ICEBATH_SPINNER_PAUSE")
	tctx := WithEnvHome(ctx, env)
	t := func(key string, kwargs map[string]any) string { return i18n.T(tctx, key, "", kwargs) }
	shape := "short"
	if onceOnly {
		shape = "smart_deny"
	} else if o.allowPermanent {
		shape = "long"
	}
	header := o.title
	if header == "" {
		header = t("approval.dangerous_header", map[string]any{"description": displayDescription})
	}
	fmt.Printf("\n  %s\n      %s\n\n%s\n\n", header, displayCommand, t("approval.choose_"+shape, nil))
	flushStdout()
	prompt := t("approval.prompt_"+shape, nil)
	answered := make(chan string, 1)
	go func() { answered <- pystr.Lower(pystr.Strip(readTerminalChoice(prompt))) }()
	var choice string
	select {
	case choice = <-answered:
	case <-time.After(o.timeout):
		fmt.Println("\n" + t("approval.timeout", approvalTimeoutNoticeKwargs(env)))
		return promptChoice{value: "timeout"}
	case <-ctx.Done():
		fmt.Println("\n" + t("approval.cancelled", nil))
		return unanswered("the prompt was interrupted before an answer was given")
	}
	decision := "deny"
	if onceOnly {
		inputs := map[string]string{}
		for _, k := range strings.Split(t("approval.smart_deny_once_inputs", nil), ",") {
			inputs[k] = "once"
		}
		for _, k := range strings.Split(t("approval.smart_deny_deny_inputs", nil), ",") {
			inputs[k] = "deny"
		}
		if d, ok := inputs[choice]; ok {
			decision = d
		}
	} else if d, ok := cliChoiceAliases[choice]; ok {
		decision = d
		if decision == "always" && !o.allowPermanent {
			decision = "session"
		}
	}
	fmt.Println(t(cliChoiceI18n[decision], nil))
	return promptChoice{value: decision}
}

// ApprovalCallbackOptions are the keywords an approval callback receives beside the command and its description
type ApprovalCallbackOptions struct {
	AllowPermanent bool
	AllowSession   bool
	SmartDenied    bool
	Title          string
}

// ApprovalCallbackAsker is an asker that is an approval callback, called with the redacted command and description and answering verbatim
type ApprovalCallbackAsker interface {
	ApprovalCallback(ctx context.Context, command, description string, o ApprovalCallbackOptions) (string, error)
}

func askHuman(ctx context.Context, env *Env, o promptOptions) promptChoice {
	displayCommand := redactApprovalDisplay(env, o.command, false)
	displayDescription := redactApprovalDisplay(env, o.description, false)
	onceOnly := o.smartDenied || !o.allowSession
	asker := resolveCLIApprovalCallback(env, o.approvalCallback)
	if asker == nil {
		if promptAppsRunning.Load() == 0 {
			return askTerminal(ctx, env, o, displayCommand, displayDescription, onceOnly)
		}
		envLog(env).Warn(fmt.Sprintf("Dangerous-command approval requested on a thread with no approval callback while prompt_toolkit is active; failing closed to avoid stdin deadlock. command=%s description=%s", pyrepr.Repr(o.command), pyrepr.Repr(o.description)))
		return unanswered("no approval callback is registered on this thread while prompt_toolkit owns the terminal, so the prompt could not be shown")
	}
	if callback, ok := asker.(ApprovalCallbackAsker); ok {
		answer, err := callback.ApprovalCallback(ctx, displayCommand, displayDescription, ApprovalCallbackOptions{
			AllowPermanent: o.allowPermanent, AllowSession: o.allowSession, SmartDenied: o.smartDenied, Title: o.title})
		if err != nil {
			envLog(env).Error("Approval callback failed", "error", err)
			return unanswered("the approval callback failed: " + pyrepr.ExceptionName(err))
		}
		return promptChoice{value: answer}
	}
	choices := []string{"once"}
	if !onceOnly {
		choices = append(choices, "session")
		if o.allowPermanent {
			choices = append(choices, "always")
		}
	}
	choices = append(choices, "deny")
	header := o.title
	if header == "" {
		header = "⚠️  DANGEROUS COMMAND: " + displayDescription
	}
	askCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	result, err := asker.Ask(askCtx, []Question{{Header: header, Text: displayCommand, Choices: choices}})
	if err != nil {
		if errors.Is(askCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return promptChoice{value: "timeout"}
		}
		if ctx.Err() != nil {
			return unanswered("the prompt was interrupted before an answer was given")
		}
		envLog(env).Error("Approval callback failed", "error", err)
		return unanswered("the approval callback failed: " + pyrepr.ExceptionName(err))
	}
	if result.Outcome == "cancelled" || len(result.Answers) == 0 {
		return unanswered("the prompt was interrupted before an answer was given")
	}
	raw := ""
	if len(result.Answers[0].Selected) > 0 {
		raw = result.Answers[0].Selected[0]
	} else {
		raw = result.Answers[0].Text
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "timeout" {
		return promptChoice{value: "timeout"}
	}
	decision, ok := cliChoiceAliases[raw]
	if !ok {
		decision = "deny"
	}
	if decision == "always" && !o.allowPermanent {
		decision = "session"
	}
	return promptChoice{value: decision}
}

// ApprovalTransportDecision is a transport response bound to one exact host created request
type ApprovalTransportDecision struct {
	RequestID     string
	RequestDigest string
	Choice        string
}

// ApprovalRequest is the immutable display only request passed to a transport plugin
type ApprovalRequest struct {
	SchemaVersion  int
	RequestID      string
	Digest         string
	Command        string
	Description    string
	PatternKey     string
	PatternKeys    []string
	Surface        string
	TimeoutSeconds float64
	AllowedChoices []string
}

// Respond builds the correlated response a transport returns
func (r ApprovalRequest) Respond(choice string) ApprovalTransportDecision {
	return ApprovalTransportDecision{RequestID: r.RequestID, RequestDigest: r.Digest, Choice: choice}
}

// Builds a request with its allowed choices and a digest over every field plus the session key
func newApprovalRequest(command, description, patternKey string, patternKeys []string, sessionKey, surface string, allowSession, allowPermanent bool, timeout float64) (ApprovalRequest, error) {
	choices := []string{"once"}
	if allowSession {
		choices = append(choices, "session")
	}
	if allowPermanent {
		choices = append(choices, "always")
	}
	choices = append(choices, "deny")
	req := ApprovalRequest{SchemaVersion: 1, RequestID: newRequestID(), Command: command, Description: description,
		PatternKey: patternKey, PatternKeys: append([]string{}, patternKeys...), Surface: surface,
		TimeoutSeconds: timeout, AllowedChoices: choices}
	canonical, err := json.Marshal(map[string]any{
		"schema_version": req.SchemaVersion, "request_id": req.RequestID, "command": command, "description": description,
		"pattern_key": patternKey, "pattern_keys": req.PatternKeys, "surface": surface, "timeout_seconds": timeout,
		"allowed_choices": choices, "session_key": sessionKey,
	})
	if err != nil {
		return ApprovalRequest{}, err
	}
	sum := sha256.Sum256(canonical)
	req.Digest = hex.EncodeToString(sum[:])
	return req, nil
}

// ApprovalPresentFunc shows one request through a plugin transport and returns the human decision
type ApprovalPresentFunc func(ctx context.Context, req ApprovalRequest) (ApprovalTransportDecision, error)

// RegisteredApprovalTransport is a plugin owned transport registration of one profile
type RegisteredApprovalTransport struct {
	Name        string
	Present     ApprovalPresentFunc
	PluginID    string
	ProfileHome string
}

// Resolver installed by the plugin manager that finds a transport by name for a profile
var approvalTransportResolver struct {
	sync.RWMutex
	fn func(env *Env, name string) (*RegisteredApprovalTransport, error)
}

// RegisterApprovalTransportResolver installs the plugin manager lookup for named transports
func RegisterApprovalTransportResolver(fn func(env *Env, name string) (*RegisteredApprovalTransport, error)) {
	approvalTransportResolver.Lock()
	defer approvalTransportResolver.Unlock()
	approvalTransportResolver.fn = fn
}

// Looks up a registered transport through the installed resolver
func resolveApprovalTransport(env *Env, name string) (*RegisteredApprovalTransport, error) {
	approvalTransportResolver.RLock()
	fn := approvalTransportResolver.fn
	approvalTransportResolver.RUnlock()
	if fn == nil {
		return nil, nil
	}
	return fn(env, name)
}

// Normalized transport result, any failure is a denial
type approvalTransportResult struct {
	choice  string
	failure string
}

const maxActiveTransportWorkers = 8

var transportWorkerSlots = make(chan struct{}, maxActiveTransportWorkers)

// Wait tuning of the transport host loop, the hermes poll_interval, on_poll and is_interrupted arguments
type transportWait struct {
	pollInterval  time.Duration
	onPoll        func()
	isInterrupted func() bool
}

// Clock of the transport host and its workers, swapped by tests while earlier workers may still stamp results
var transportClock atomic.Pointer[func() time.Time]

// Current time of the transport clock
func transportNow() time.Time {
	if fn := transportClock.Load(); fn != nil {
		return (*fn)()
	}
	return time.Now()
}

// Replaces the transport clock and returns the previous one, nil standing for the real clock
func swapTransportClock(fn func() time.Time) func() time.Time {
	var prev *func() time.Time
	if fn == nil {
		prev = transportClock.Swap(nil)
	} else {
		prev = transportClock.Swap(&fn)
	}
	if prev == nil {
		return nil
	}
	return *prev
}

// Redactor of the request text shown through a transport, swapped by tests for a failing redactor
var approvalRequestRedactor = func(env *Env, text string) (string, error) {
	return redactApprovalDisplay(env, text, true), nil
}

// Deny result carrying a failure code
func transportDeny(failure string) approvalTransportResult {
	return approvalTransportResult{choice: "deny", failure: failure}
}

// Runs the poll callback, a panic inside it only logged at debug level
func pollTransport(onPoll func()) {
	if onPoll == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			envLog(nil).Debug("Approval transport poll callback failed", "error", fmt.Sprint(r))
		}
	}()
	onPoll()
}

// Runs a transport on a bounded worker and validates its decision against the request and deadline
func invokeApprovalTransport(ctx context.Context, present ApprovalPresentFunc, req ApprovalRequest, timeout time.Duration, wait transportWait) approvalTransportResult {
	select {
	case transportWorkerSlots <- struct{}{}:
	default:
		envLog(nil).Warn("Approval transport worker capacity exhausted")
		return transportDeny("busy")
	}
	type outcome struct {
		decision    ApprovalTransportDecision
		err         error
		completedAt time.Time
	}
	results := make(chan outcome, 1)
	if timeout < 0 {
		timeout = 0
	}
	deadline := transportNow().Add(timeout)
	workerCtx, cancelWorker := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWorker()
	go func() {
		defer func() { <-transportWorkerSlots }()
		defer func() {
			if r := recover(); r != nil {
				results <- outcome{err: fmt.Errorf("transport panic: %v", r), completedAt: transportNow()}
			}
		}()
		decision, err := present(workerCtx, req)
		results <- outcome{decision: decision, err: err, completedAt: transportNow()}
	}()
	poll := wait.pollInterval
	if poll <= 0 {
		poll = time.Second
	}
	poll = max(poll, time.Millisecond)
	var got outcome
	for {
		if ctx.Err() != nil || (wait.isInterrupted != nil && wait.isInterrupted()) {
			envLog(nil).Info("Approval transport wait interrupted", "request_id", req.RequestID)
			return transportDeny("interrupted")
		}
		remaining := deadline.Sub(transportNow())
		if remaining <= 0 {
			envLog(nil).Warn("Approval transport timed out", "request_id", req.RequestID)
			return transportDeny("timeout")
		}
		timer := time.NewTimer(min(poll, remaining))
		select {
		case got = <-results:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			continue
		case <-timer.C:
			pollTransport(wait.onPoll)
			continue
		}
		break
	}
	if failure := validateTransportDecision(got.decision, got.err, got.completedAt, deadline, req); failure != "" {
		return transportDeny(failure)
	}
	return approvalTransportResult{choice: got.decision.Choice}
}

// Failure code for a worker result, empty when the decision is valid
func validateTransportDecision(decision ApprovalTransportDecision, err error, completedAt, deadline time.Time, req ApprovalRequest) string {
	log := envLog(nil)
	if completedAt.After(deadline) {
		log.Warn("Approval transport timed out", "request_id", req.RequestID)
		return "timeout"
	}
	if err != nil {
		log.Warn("Approval transport failed", "request_id", req.RequestID)
		return "error"
	}
	if decision.RequestID == "" && decision.RequestDigest == "" {
		log.Warn("Approval transport returned an invalid decision type")
		return "invalid"
	}
	if decision.RequestID != req.RequestID || decision.RequestDigest != req.Digest {
		log.Warn("Approval transport returned a stale or mismatched decision")
		return "stale"
	}
	for _, allowed := range req.AllowedChoices {
		if decision.Choice == allowed {
			return ""
		}
	}
	log.Warn("Approval transport returned a disallowed choice")
	return "invalid"
}

// Inputs of one transport presentation
type transportRequest struct {
	command, description, patternKey string
	patternKeys                      []string
	sessionKey, surface              string
	allowSession, allowPermanent     bool
}

// Result shape of a transport presentation once a transport is selected
type transportAttempt struct {
	selected bool
	choice   string
	failure  string
	fallback string
	name     string
}

// Presents through the operator selected plugin transport when one is configured
func presentWithSelectedTransport(ctx context.Context, env *Env, r transportRequest) transportAttempt {
	name, fallback := getApprovalTransportConfig(env)
	if name == "builtin" {
		return transportAttempt{}
	}
	log := envLog(env)
	registered, err := resolveApprovalTransport(env, name)
	if err != nil {
		log.Warn("Could not resolve selected approval transport", "transport", name)
		registered = nil
	}
	if registered == nil {
		log.Warn("Selected approval transport is unavailable", "transport", name)
		return transportAttempt{selected: true, choice: "deny", failure: "unavailable", fallback: fallback, name: name}
	}
	timeout := time.Duration(getApprovalTimeout(env)) * time.Second
	command, err := approvalRequestRedactor(env, r.command)
	var description string
	if err == nil {
		description, err = approvalRequestRedactor(env, r.description)
	}
	var req ApprovalRequest
	if err == nil {
		req, err = newApprovalRequest(command, description, r.patternKey, r.patternKeys, r.sessionKey, r.surface, r.allowSession, r.allowPermanent, timeout.Seconds())
	}
	if err != nil {
		log.Warn("Could not build redacted plugin approval request")
		return transportAttempt{selected: true, choice: "deny", failure: "error", name: name}
	}
	hookPayload := map[string]any{"command": req.Command, "description": req.Description, "pattern_key": r.patternKey,
		"pattern_keys": append([]string{}, r.patternKeys...), "session_key": r.sessionKey, "surface": "transport:" + name,
		"request_id": req.RequestID, "request_digest": req.Digest}
	fireApprovalHook(ctx, env, "pre_approval_request", hookPayload)
	human := humanInputRequest(ctx, env, "approval", req.Command, r.sessionKey)
	release := humanWaitWindow(env, r.sessionKey)
	result := invokeApprovalTransport(ctx, registered.Present, req, timeout, transportWait{
		onPoll: activityHeartbeat(env, "waiting for plugin approval transport"), isInterrupted: func() bool { return IsInterrupted(ctx) }})
	release()
	outcome := result.choice
	if result.failure != "" {
		outcome = "transport_" + result.failure
	}
	human.finish(ctx, env, outcome)
	post := copyMap(hookPayload)
	post["choice"] = outcome
	fireApprovalHook(ctx, env, "post_approval_response", post)
	return transportAttempt{selected: true, choice: result.choice, failure: result.failure, fallback: fallback, name: name}
}

// Interprets a transport attempt into the user's choice or a denied result
func transportChoice(env *Env, attempt transportAttempt, patternKey, description string) (string, GuardResult, bool) {
	if !attempt.selected {
		return "", GuardResult{}, false
	}
	if attempt.failure == "" {
		return attempt.choice, GuardResult{}, false
	}
	if attempt.fallback == "builtin" {
		envLog(env).Warn("Approval transport failed; using explicit builtin fallback", "transport", attempt.name, "failure", attempt.failure)
		return "", GuardResult{}, false
	}
	breaker := denialBreakerAddendum(env, getCurrentSessionKey(env, "default"))
	message := fmt.Sprintf("BLOCKED: Selected approval transport failed (%s); the user has NOT consented to this action. Do NOT retry this command or attempt the same outcome through another route.%s", attempt.failure, breaker)
	return "", deniedResult(env, message, patternKey, description, "transport_"+attempt.failure, "command"), true
}

// Maps an approval choice to an elicitation verdict, unresolved is the no answer outcome
func consent(choice, unresolved string) string {
	switch choice {
	case "once", "session", "always":
		return "accept"
	case "timeout", "cancelled":
		return unresolved
	}
	return "decline"
}

// Consent that records a session or always answer under the remember key
func rememberedConsent(env *Env, sessionKey, choice, rememberKey, unresolved string) string {
	if rememberKey != "" {
		persistChoice(env, sessionKey, choice, []string{rememberKey})
	}
	return consent(choice, unresolved)
}

// ElicitationOptions tunes one MCP elicitation consent request
type ElicitationOptions struct {
	Timeout     time.Duration
	Surface     string
	Title       string
	RememberKey string
}

// RequestElicitationConsent routes an MCP elicitation to the surface owning the session and returns accept, decline, or cancel
func RequestElicitationConsent(ctx context.Context, env *Env, message, description string, o ElicitationOptions) string {
	if o.Surface == "" {
		o.Surface = "mcp-elicitation"
	}
	if o.Title == "" {
		o.Title = "Confirm this action?"
	}
	log := envLog(env)
	sessionKey := getCurrentSessionKey(env, "default")
	if isSingleQueryApprovalContext(env) {
		return "decline"
	}
	scoped := o.RememberKey != ""
	apiRunContext := getSessionPlatform(env) == "api_server" && !isCronApprovalContext(env) && !isSingleQueryApprovalContext(env)
	if IsGatewayApprovalContext(env) || apiRunContext {
		notify := gatewayNotifyCb(sessionKey)
		if notify == nil {
			log.Warn("Elicitation requested in gateway session but no notify_cb is registered — failing closed", "session", sessionKey)
			return "decline"
		}
		decision := awaitGatewayDecision(ctx, env, sessionKey, notify, map[string]any{"command": message, "description": description,
			"pattern_key": "mcp_elicitation", "pattern_keys": []string{"mcp_elicitation"},
			"allow_session": scoped, "allow_permanent": scoped}, o.Surface)
		if decision.NotifyFailed {
			return "decline"
		}
		if !decision.Resolved || decision.Cancelled != "" {
			return "cancel"
		}
		return rememberedConsent(env, sessionKey, decision.Choice, o.RememberKey, "decline")
	}
	if NoUserCanAnswer(env) {
		log.Info("Elicitation consent declined: no user can answer in this session", "surface", o.Surface)
		return "decline"
	}
	hookPayload := map[string]any{"command": message, "description": description, "pattern_key": "mcp_elicitation",
		"pattern_keys": []string{"mcp_elicitation"}, "session_key": sessionKey, "surface": o.Surface}
	fireApprovalHook(ctx, env, "pre_approval_request", hookPayload)
	choice := promptDangerousApproval(ctx, env, promptOptions{command: message, description: description, timeout: o.Timeout,
		allowPermanent: scoped, allowSession: scoped, title: o.Title})
	post := copyMap(hookPayload)
	post["choice"] = choice.value
	fireApprovalHook(ctx, env, "post_approval_response", post)
	return rememberedConsent(env, sessionKey, choice.value, o.RememberKey, "cancel")
}

var approvalNonASCIIRe = regexp.MustCompile("[^\x20-\x7e\t\n]+")

var approvalBlankGlyphs = func() map[rune]bool {
	set := map[rune]bool{}
	for _, r := range "͏ᅟᅠ឴឵᠋᠌᠍᠏⠀ㅤﾠ" {
		set[r] = true
	}
	for r := rune(0xFE00); r < 0xFE10; r++ {
		set[r] = true
	}
	for r := rune(0xE0100); r < 0xE01F0; r++ {
		set[r] = true
	}
	return set
}()

const approvalSpaceClass = `[ \t\x{00a0}\x{1680}\x{2000}-\x{200a}\x{202f}\x{205f}\x{3000}]`

var approvalPadRe = regexp2.MustCompile(`(?<=\S)` + approvalSpaceClass + `{21,}|(?m:^)` + approvalSpaceClass + `{65,}`)

var approvalBlankRe = regexp.MustCompile(`\n(?:` + approvalSpaceClass + `*\n){3,}`)

var proseBidiControls = map[rune]bool{0x202a: true, 0x202b: true, 0x202c: true, 0x202d: true, 0x202e: true, 0x2066: true, 0x2067: true, 0x2068: true, 0x2069: true}

// Reports whether a code point is hidden in the approval display sense
func hiddenApprovalRune(ch rune, prose bool) bool {
	control := unicode.Is(unicode.Cc, ch)
	separator := unicode.Is(unicode.Zl, ch) || unicode.Is(unicode.Zp, ch)
	if prose {
		return control || separator || proseBidiControls[ch]
	}
	assigned := unicode.In(ch, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.C)
	return approvalBlankGlyphs[ch] || unicode.Is(unicode.C, ch) || !assigned || separator
}

// Python unicode_escape spelling of one code point
func unicodeEscape(ch rune) string {
	switch {
	case ch < 0x100:
		return fmt.Sprintf("\\x%02x", ch)
	case ch < 0x10000:
		return fmt.Sprintf("\\u%04x", ch)
	}
	return fmt.Sprintf("\\U%08x", ch)
}

// Escapes the invisible code points of a non ASCII run, four or more repeats show once with a count
func escapeApprovalRun(run string, prose bool) string {
	var b strings.Builder
	runes := []rune(run)
	for i := 0; i < len(runes); {
		ch := runes[i]
		n := 1
		for i+n < len(runes) && runes[i+n] == ch {
			n++
		}
		if !hiddenApprovalRune(ch, prose) {
			b.WriteString(strings.Repeat(string(ch), n))
		} else if n >= 4 {
			fmt.Fprintf(&b, "⟨%s ×%d⟩", unicodeEscape(ch), n)
		} else {
			b.WriteString(strings.Repeat(unicodeEscape(ch), n))
		}
		i += n
	}
	return b.String()
}

// Makes hidden content in an approval prompt string visible, clean text comes back unchanged
func sanitizeCommandForDisplay(text string, prose bool) string {
	if text == "" {
		return ""
	}
	out := approvalNonASCIIRe.ReplaceAllStringFunc(text, func(run string) string { return escapeApprovalRun(run, prose) })
	out = approvalBlankRe.ReplaceAllStringFunc(out, func(m string) string {
		return fmt.Sprintf("\n⟨+%d blank lines⟩\n", strings.Count(m, "\n")-1)
	})
	padded, err := approvalPadRe.ReplaceFunc(out, func(m regexp2.Match) string {
		return fmt.Sprintf(" ⟨+%d whitespace chars⟩ ", len([]rune(m.String())))
	}, -1, -1)
	if err != nil {
		return out
	}
	return padded
}

// Secret redaction plus display sanitizing for every approval surface
func redactApprovalDisplay(env *Env, text string, force bool) string {
	return sanitizeCommandForDisplay(RedactSensitiveTextWith(env, text, RedactOptions{Force: force}), false)
}
