package agent

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var statusStdout io.Writer = os.Stdout

// Reports whether stdout is a terminal, swapped by tests
var statusStdoutIsTTY = func() bool { return isatty.IsTerminal(os.Stdout.Fd()) }

type retryStatusLine struct {
	kind    string
	message any
}

type ctxOverflowWarnKey struct {
	tag, kind     string
	contextLength int
}

// Startup notices, overflow dedup and the buffered retry chatter the status rail keeps
type statusOutputState struct {
	notificationPlatform  *string
	notificationConfig    map[string]any
	pendingStartupPrints  []string
	pendingStartupNotices []provider.AgentNotice
	lastCtxOverflowWarn   *ctxOverflowWarnKey
	retryStatusBuffer     []retryStatusLine
	pendingFallbackNotice []string
}

// Hands the turn the kept migration notices when someone sees them, unattended turns leave them in the home
func deliverKeptStartupNotices(ctx context.Context, a *AIAgent) {
	if a.suppressStatusOutput || isUnattendedTurn(a) {
		return
	}
	platform := a.platform
	if platform == "" {
		platform = "cli"
	}
	if platform != "cli" && a.noticeCallback == nil {
		return
	}
	for _, message := range provider.ClaimKeptNotices(ctx) {
		a.emitStartupWarning(message)
	}
}

// Pins the per turn presentation platform and user config the gateway resolves for warnings
func (a *AIAgent) SetNotificationPresentation(platform string, userConfig map[string]any) {
	a.notificationPlatform, a.notificationConfig = &platform, userConfig
}

// Queues the one shot fallback switch lines surfaced on recovery or folded into the flushed trace
func (a *AIAgent) SetPendingFallbackNotice(lines []string) { a.pendingFallbackNotice = lines }

func statusText(message any) string {
	if d, ok := message.(config.DiagnosticText); ok {
		return string(d)
	}
	s, _ := message.(string)
	return s
}

// Prints through the injected renderer or stdout, swallowing write failures and muted replies
func (a *AIAgent) safePrint(text string, diagnostic bool) {
	if a.muteNotificationReply {
		return
	}
	if diagnostic && !a.warningPresentationEnabled() {
		return
	}
	if a.printFn != nil {
		a.printFn(text)
		return
	}
	_, _ = fmt.Fprintln(statusStdout, text)
}

func (a *AIAgent) SafePrint(text string, diagnostic bool) { a.safePrint(text, diagnostic) }

// Verbose print muted while tokens stream or after the response unless forced, quiet machine mode wins
func (a *AIAgent) vprint(text string, force, diagnostic bool) {
	if a.suppressStatusOutput {
		return
	}
	if diagnostic && !a.warningPresentationEnabled() {
		return
	}
	if force || !(a.mutePostResponse || (a.hasStreamConsumers() && !a.executingTools)) {
		a.safePrint(text, false)
	}
}

// True when quiet spinner output has a safe sink, an injected printer or a real terminal
func (a *AIAgent) shouldStartQuietSpinner() bool {
	if a.printFn != nil {
		return true
	}
	return statusStdoutIsTTY()
}

// True when quiet tool summaries print directly on the CLI with no callback owning rendering
func (a *AIAgent) shouldEmitQuietToolMessages() bool {
	if a.suppressStatusOutput {
		return false
	}
	return a.quietMode && a.toolProgressCallback == nil && a.platform == "cli"
}

// Calls a driver callback with the arguments, logging and swallowing any failure it raises
func (a *AIAgent) callCallback(name string, cb any, origin string, args ...any) {
	if cb == nil {
		return
	}
	fn := reflect.ValueOf(cb)
	if fn.Kind() == reflect.Func && fn.IsNil() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug(fmt.Sprintf("%s error in %s", name, origin), "error", r)
		}
	}()
	callDynamic(fn, args)
}

// Calls fn converting each argument to its parameter type the way a Python call passes subclasses
func callDynamic(fn reflect.Value, args []any) {
	ft := fn.Type()
	if !ft.IsVariadic() && ft.NumIn() != len(args) {
		panic(fmt.Sprintf("TypeError: callback takes %d positional arguments but %d were given", ft.NumIn(), len(args)))
	}
	in := make([]reflect.Value, len(args))
	for i, arg := range args {
		var pt reflect.Type
		if ft.IsVariadic() && i >= ft.NumIn()-1 {
			pt = ft.In(ft.NumIn() - 1).Elem()
		} else {
			pt = ft.In(i)
		}
		if arg == nil {
			in[i] = reflect.Zero(pt)
			continue
		}
		v := reflect.ValueOf(arg)
		if !v.Type().AssignableTo(pt) && v.Type().ConvertibleTo(pt) {
			v = v.Convert(pt)
		}
		in[i] = v
	}
	fn.Call(in)
}

// Prints the line on the CLI and forwards kind and message to the status callback, never raising
func (a *AIAgent) emitStatusKind(kind string, message any, origin string) {
	func() {
		defer func() { _ = recover() }()
		if !config.IsWarningStatus(kind, message) || a.warningPresentationEnabled() {
			a.vprint(a.logPrefix+statusText(message), true, false)
		}
	}()
	a.callCallback("status_callback", a.statusCallback, origin, kind, message)
}

// Warning presentation preference of the notification platform, true when the preference fails
func (a *AIAgent) warningPresentationEnabled() (enabled bool) {
	platform := a.platform
	if a.notificationPlatform != nil {
		platform = *a.notificationPlatform
	}
	defer func() {
		if recover() != nil {
			enabled = true
		}
	}()
	return warningNotificationsEnabled(a.homeContext(), platform, a.notificationConfig)
}

var warningNotificationsEnabled = config.WarningNotificationsEnabled

func (a *AIAgent) emitDiagnosticStatus(text string) {
	a.emitStatusKind("lifecycle", config.DiagnosticText(text), "_emit_status")
}

func (a *AIAgent) emitStatus(message string) { a.emitStatusKind("lifecycle", message, "_emit_status") }

func (a *AIAgent) emitWarning(message string) { a.emitStatusKind("warn", message, "_emit_warning") }

// Prints a startup warning on the CLI, holds it for a gateway hosted CLI, else raises a sticky notice
func (a *AIAgent) emitStartupWarning(message string) {
	message = strings.TrimSpace(message)
	platform := a.platform
	if platform == "" {
		platform = "cli"
	}
	if platform == "cli" {
		if a.gatewaySessionKey != "" {
			a.pendingStartupPrints = append(append([]string{}, a.pendingStartupPrints...), message)
			return
		}
		a.emitWarning(message)
		return
	}
	key := fmt.Sprintf("startup-warning.%08x", crc32.ChecksumIEEE([]byte(message)))
	level := "warn"
	if strings.HasPrefix(message, "✓") {
		level = "success"
	}
	notice := provider.AgentNotice{Text: message, Level: level, Kind: "sticky", Key: key, ID: key}
	if a.noticeCallback != nil {
		a.emitNotice(notice)
		return
	}
	a.pendingStartupNotices = append(append([]provider.AgentNotice{}, a.pendingStartupNotices...), notice)
}

func (a *AIAgent) hasPendingStartupWarnings() bool {
	return len(a.pendingStartupNotices) > 0 || len(a.pendingStartupPrints) > 0
}

// Replays the held startup prints as warnings and the held notices once a notice sink exists
func (a *AIAgent) replayStartupWarnings() {
	if prints := a.pendingStartupPrints; len(prints) > 0 {
		a.pendingStartupPrints = []string{}
		for _, message := range prints {
			a.emitWarning(message)
		}
	}
	if pending := a.pendingStartupNotices; len(pending) > 0 && a.noticeCallback != nil {
		a.pendingStartupNotices = []provider.AgentNotice{}
		for _, notice := range pending {
			a.emitNotice(notice)
		}
	}
}

// Warns once per block kind when context is over the threshold but compression is blocked
func (a *AIAgent) warnContextOverflowBlocked(reason string, preflightTokens, thresholdTokens int) {
	kind := reason
	if kind == "" {
		kind = "unknown"
	}
	kind, _, _ = strings.Cut(kind, ":")
	key := ctxOverflowWarnKey{tag: "ctx_overflow_blocked", kind: kind}
	if a.lastCtxOverflowWarn != nil && *a.lastCtxOverflowWarn == key {
		return
	}
	a.lastCtxOverflowWarn = &key
	if kind == "cooldown" || kind == "ineffective" {
		a.touchActivity(fmt.Sprintf("compression blocked (%s)", reason), string(state.ActivityProvenanceAgentCompressionCooldown), false)
	}
	a.emitWarning(ContextOverflowBlockedWarning(preflightTokens, thresholdTokens, reason))
}

// Warns once per window when uncompressed context exceeds the model limit, pointing at compact
func (a *AIAgent) warnUncompressedContextOverflow(preflightTokens, contextLength int) {
	key := ctxOverflowWarnKey{tag: "uncompressed_ctx_overflow", contextLength: contextLength}
	if a.lastCtxOverflowWarn != nil && *a.lastCtxOverflowWarn == key {
		return
	}
	a.lastCtxOverflowWarn = &key
	a.emitWarning(i18n.T(a.homeContext(), "display.notice.uncompressed_context_overflow", "",
		map[string]any{"tokens": groupThousands(preflightTokens), "context_length": groupThousands(contextLength)}))
}

func (a *AIAgent) clearContextOverflowWarn() { a.lastCtxOverflowWarn = nil }

func (a *AIAgent) emitNotice(notice provider.AgentNotice) {
	a.callCallback("notice_callback", a.noticeCallback, "_emit_notice", notice)
}

func (a *AIAgent) emitNoticeClear(key string) {
	a.callCallback("notice_clear_callback", a.noticeClearCallback, "_emit_notice_clear", key)
}

// Rewrites the live status line so a long provider wait is not an anonymous spinner
func (a *AIAgent) emitWaitNotice(text string) { a.emitWaitNoticeValue(text) }

func (a *AIAgent) emitWaitNoticeValue(text any) {
	a.TouchActivity(statusText(text))
	a.callCallback("thinking_callback", a.thinkingCallback, "_emit_wait_notice", text)
}

// Carries the wait text as diagnostic text so the TUI tells a retry notice from a spinner verb
func (a *AIAgent) emitDiagnosticWait(text string) { a.emitWaitNoticeValue(config.DiagnosticText(text)) }

// Calls the thinking callback directly so a failure it raises propagates to the caller
func (a *AIAgent) callThinking(text any) {
	if a.thinkingCallback == nil {
		return
	}
	if fn := reflect.ValueOf(a.thinkingCallback); fn.Kind() != reflect.Func || !fn.IsNil() {
		callDynamic(fn, []any{text})
	}
}

func (a *AIAgent) EmitWaitNotice(text string) { a.emitWaitNotice(text) }

func (a *AIAgent) EmitDiagnosticWait(text string) { a.emitDiagnosticWait(text) }

func (a *AIAgent) bufferRetryMessage(kind string, message any) {
	a.retryStatusBuffer = append(a.retryStatusBuffer, retryStatusLine{kind: kind, message: message})
}

func (a *AIAgent) bufferStatus(message string) { a.bufferRetryMessage("status", message) }

func (a *AIAgent) BufferDiagnosticStatus(text string) {
	a.bufferRetryMessage("status", config.DiagnosticText(text))
}

func (a *AIAgent) bufferVprint(text string) { a.bufferRetryMessage("vprint", text) }

func (a *AIAgent) clearStatusBuffer() { a.retryStatusBuffer = a.retryStatusBuffer[:0] }

// Surfaces the one shot fallback switch lines on recovery, cleared before emitting
func (a *AIAgent) emitPendingFallbackNotice() {
	notice := a.pendingFallbackNotice
	if len(notice) == 0 {
		return
	}
	a.pendingFallbackNotice = nil
	for _, item := range notice {
		func() {
			defer func() { _ = recover() }()
			a.emitDiagnosticStatus(item)
		}()
	}
}

// Emits the buffered retry lines on terminal failure, drained first so a failing sink never repeats them
func (a *AIAgent) flushStatusBuffer() {
	a.pendingFallbackNotice = nil
	if len(a.retryStatusBuffer) == 0 {
		return
	}
	messages := append([]retryStatusLine{}, a.retryStatusBuffer...)
	a.retryStatusBuffer = a.retryStatusBuffer[:0]
	for _, line := range messages {
		func() {
			defer func() { _ = recover() }()
			switch line.kind {
			case "status":
				a.emitStatusKind("lifecycle", line.message, "_emit_status")
			case "warn":
				a.emitWarning(statusText(line.message))
			default:
				if a.warningPresentationEnabled() {
					a.vprint(a.logPrefix+statusText(line.message), true, false)
				}
			}
		}()
	}
}
