package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/state"
	"github.com/nickheyer/icebath/internal/tool"
)

type Sink interface {
	Emit(e Event)
}

type Event struct {
	Kind       EventKind
	SessionID  string
	TurnID     string
	At         time.Time
	Text       string
	Inline     bool
	Streamed   bool
	Status     string
	Diagnostic bool
	Forced     bool
	Name       string
	CallID     string
	Args       tool.Args
	Result     *tool.Result
	Duration   time.Duration
	Failed     bool
	Finished   bool
	Error      string
	Risk       *tool.Risk
	Step       int
	Tools      []StepTool
	Notice     *Notice
	Fields     json.RawMessage
	Subagent   *SubagentEvent
	MoA        *MixtureEvent
	Usage      *wire.Usage
	Progress   *wire.Progress
}

type EventKind uint8

const (
	EventStreamStart EventKind = iota + 1
	EventText
	EventTextBreak
	EventStreamEnd
	EventReasoning
	EventThinking
	EventToolGenerating
	EventToolStarted
	EventToolProgress
	EventToolCompleted
	EventToolRisk
	EventReasoningReady
	EventInterim
	EventStep
	EventStatus
	EventNotice
	EventNoticeClear
	EventLifecycle
	EventReaction
	EventSubagent
	EventMoA
	EventTitle
	EventProgress
	EventUsage
	EventPrint
)

const (
	StatusLifecycle = "lifecycle"
	StatusWarn      = "warn"

	ProgressToolStarted        = "tool.started"
	ProgressToolProgress       = "tool.progress"
	ProgressToolCompleted      = "tool.completed"
	ProgressToolOutputRisk     = "tool.output_risk"
	ProgressReasoningAvailable = "reasoning.available"
	ProgressThinkingName       = "_thinking"

	LifecycleSessionCompress = "session:compress"

	NoticeSticky = "sticky"
	NoticeTTL    = "ttl"
)

type StepTool struct {
	Name      string
	Arguments string
	Result    *string
}

type SubagentEvent struct {
	Phase        string
	ID           string
	ParentID     string
	DelegationID string
	TaskIndex    int
	TaskCount    int
	Depth        int
	Goal         string
	Model        string
	Tool         string
	Preview      string
	Status       string
	Summary      string
	Duration     time.Duration
	Tokens       SessionUsage
	CostUSD      float64
}

type Notice struct {
	Key   string
	ID    string
	Level string
	Kind  string
	TTL   time.Duration
	Text  string
	Data  json.RawMessage
}

func (n Notice) Diagnostic() bool {
	return n.Level == "warn" || n.Level == "error" || strings.HasPrefix(n.Key, "credits.")
}

func presentationKind(k EventKind) bool {
	switch k {
	case EventText, EventTextBreak, EventInterim, EventReasoning, EventToolStarted, EventToolProgress,
		EventToolCompleted, EventToolRisk, EventReasoningReady, EventToolGenerating, EventReaction,
		EventSubagent, EventMoA, EventPrint:
		return true
	}
	return false
}

type bufferedLine struct {
	kind       string
	text       string
	diagnostic bool
}

type statusState struct {
	mu             sync.Mutex
	retry          []bufferedLine
	fallbackNotice []string
	startupPrints  []string
	overflowWarn   string
}

func (a *Agent) emit(e Event) {
	if a.muted.Load() && presentationKind(e.Kind) {
		return
	}
	if e.SessionID == "" {
		e.SessionID = a.SessionID()
	}
	if e.TurnID == "" {
		e.TurnID = a.currentTurnID()
	}
	if e.At.IsZero() {
		e.At = a.now()
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("event sink failed", "kind", int(e.Kind), "error", fmt.Sprint(r))
		}
	}()
	a.events.Emit(e)
}

func (a *Agent) Emit(e Event) {
	a.emit(e)
}

func (a *Agent) emitStatusKind(kind, text string, diagnostic bool) {
	a.emit(Event{Kind: EventStatus, Status: kind, Text: text, Diagnostic: diagnostic})
}

func (a *Agent) emitStatus(text string) {
	a.emitStatusKind(StatusLifecycle, text, false)
}

func (a *Agent) emitDiagnosticStatus(text string) {
	a.emitStatusKind(StatusLifecycle, text, true)
}

func (a *Agent) emitWarning(text string) {
	a.emitStatusKind(StatusWarn, text, false)
}

func (a *Agent) emitNotice(n Notice) {
	a.emit(Event{Kind: EventNotice, Name: n.Key, Text: n.Text, Notice: &n})
}

func (a *Agent) emitNoticeClear(key string) {
	a.emit(Event{Kind: EventNoticeClear, Name: key})
}

func (a *Agent) emitWaitNotice(ctx context.Context, text string, diagnostic bool) {
	a.touch(ctx, text, state.ProvenanceUnknown, false)
	a.emit(Event{Kind: EventThinking, Text: text, Diagnostic: diagnostic})
}

func (a *Agent) vprint(text string, force, diagnostic bool) {
	if a.muted.Load() {
		return
	}
	after := a.mutePost.Load()
	if !force && after {
		return
	}
	a.emit(Event{Kind: EventPrint, Text: text, Diagnostic: diagnostic, Forced: force || a.control.executing()})
}

func (a *Agent) print(text string, diagnostic bool) {
	if a.muted.Load() {
		return
	}
	a.emit(Event{Kind: EventPrint, Text: text, Diagnostic: diagnostic, Forced: true})
}

func startupNoticeKey(text string) string {
	return fmt.Sprintf("startup-warning.%08x", crc32.ChecksumIEEE([]byte(text)))
}

func (a *Agent) emitStartupWarning(text string) {
	text = trimText(text)
	if a.o.Platform == "" || a.o.Platform == SourceCLI {
		if a.o.GatewaySessionKey != "" {
			a.status.mu.Lock()
			a.status.startupPrints = append(a.status.startupPrints, text)
			a.status.mu.Unlock()
			return
		}
		a.emitWarning(text)
		return
	}
	key := startupNoticeKey(text)
	level := "warn"
	if strings.HasPrefix(text, "✓") {
		level = "success"
	}
	a.emitNotice(Notice{Text: text, Level: level, Kind: NoticeSticky, Key: key, ID: key})
}

func (a *Agent) replayStartupWarnings() {
	a.status.mu.Lock()
	prints := a.status.startupPrints
	a.status.startupPrints = nil
	a.status.mu.Unlock()
	for _, text := range prints {
		a.emitWarning(text)
	}
}

func (a *Agent) bufferRetryMessage(kind, text string, diagnostic bool) {
	a.status.mu.Lock()
	a.status.retry = append(a.status.retry, bufferedLine{kind: kind, text: text, diagnostic: diagnostic})
	a.status.mu.Unlock()
}

func (a *Agent) bufferStatus(text string) {
	a.bufferRetryMessage("status", text, false)
}

func (a *Agent) bufferDiagnosticStatus(text string) {
	a.bufferRetryMessage("status", text, true)
}

func (a *Agent) bufferVprint(text string) {
	a.bufferRetryMessage("vprint", text, false)
}

func (a *Agent) bufferWarning(text string) {
	a.bufferRetryMessage("warn", text, false)
}

func (a *Agent) clearStatusBuffer() {
	a.status.mu.Lock()
	a.status.retry = nil
	a.status.mu.Unlock()
}

func (a *Agent) notePendingFallback(text string) {
	a.status.mu.Lock()
	a.status.fallbackNotice = append(a.status.fallbackNotice, text)
	a.status.mu.Unlock()
}

func (a *Agent) emitPendingFallbackNotice() {
	a.status.mu.Lock()
	notices := a.status.fallbackNotice
	a.status.fallbackNotice = nil
	a.status.mu.Unlock()
	for _, text := range notices {
		a.emitDiagnosticStatus(text)
	}
}

func (a *Agent) flushStatusBuffer() {
	a.status.mu.Lock()
	a.status.fallbackNotice = nil
	lines := a.status.retry
	a.status.retry = nil
	a.status.mu.Unlock()
	for _, line := range lines {
		switch line.kind {
		case "status":
			a.emitStatusKind(StatusLifecycle, line.text, line.diagnostic)
		case "warn":
			a.emitWarning(line.text)
		default:
			a.vprint(line.text, true, true)
		}
	}
}

const contextOverflowBlockedWarning = "⚠ Context is over the compression threshold (~%s tokens >= %s) " +
	"but compression is currently blocked (%s). The model may stop responding. Run /new to start a fresh " +
	"session or /compress to retry immediately."

func groupThousands(n int) string {
	digits := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return sign + b.String()
}

func (a *Agent) warnContextOverflowBlocked(ctx context.Context, reason string, preflightTokens, thresholdTokens int) {
	kind := reason
	if kind == "" {
		kind = "unknown"
	}
	kind, _, _ = strings.Cut(kind, ":")
	key := "ctx_overflow_blocked\x00" + kind
	a.status.mu.Lock()
	if a.status.overflowWarn == key {
		a.status.mu.Unlock()
		return
	}
	a.status.overflowWarn = key
	a.status.mu.Unlock()
	if kind == "cooldown" || kind == "ineffective" {
		a.touch(ctx, "compression blocked ("+reason+")", state.ProvenanceAgentCompressionCooldown, false)
	}
	a.emitWarning(fmt.Sprintf(contextOverflowBlockedWarning, groupThousands(preflightTokens), groupThousands(thresholdTokens), reason))
}

func (a *Agent) warnUncompressedContextOverflow(ctx context.Context, preflightTokens, contextLength int) error {
	if a.o.Text == nil {
		return &tool.UnfilledError{Name: "text catalog"}
	}
	key := "uncompressed_ctx_overflow\x00" + strconv.Itoa(contextLength)
	a.status.mu.Lock()
	if a.status.overflowWarn == key {
		a.status.mu.Unlock()
		return nil
	}
	a.status.overflowWarn = key
	a.status.mu.Unlock()
	a.emitWarning(a.o.Text.T(ctx, "display.notice.uncompressed_context_overflow", map[string]any{
		"tokens": groupThousands(preflightTokens), "context_length": groupThousands(contextLength),
	}))
	return nil
}

func (a *Agent) clearContextOverflowWarn() {
	a.status.mu.Lock()
	a.status.overflowWarn = ""
	a.status.mu.Unlock()
}
