package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"runtime"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	// Runtime status message of a connected adapter whose delivery path is unproven
	DegradedStatusMessage = "connected but not yet confirmed active; recovering in background"

	textBatchDefaultDelayS      = 0.3
	textBatchMaxDelayS          = 2.0
	textBatchDefaultSplitDelayS = 1.0
	textBatchMaxSplitDelayS     = 4.0
	splitThreshold              = 4000
)

// Thunk result, or the default when it fails with a numeric coercion error
func orDefault[T any](thunk func() (T, error), def T) T {
	v, err := thunk()
	if err != nil {
		return def
	}
	return v
}

// Logs the failure of a fatal error handler that outlived its cancelled carrier
func consumeDetachedHandlerException(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	slog.Error(fmt.Sprintf("Detached fatal-error handler task failed: %v", err))
}

// Map behind the pointer, created on first use for adapters built without the constructor
func lazyAttr[K comparable, V any](m *map[K]V) map[K]V {
	if *m == nil {
		*m = map[K]V{}
	}
	return *m
}

// AdapterTraits are the class attributes a concrete adapter overrides by assignment in its constructor
type AdapterTraits struct {
	SupportsCodeBlocks           bool
	SupportsStatusText           bool
	SupportsAsyncDelivery        bool
	SplitsLongMessages           bool
	TypedCommandPrefix           string
	SupportsInchannelContinuable bool
	InteractiveResume            bool
	ServesProfilePrefix          bool
	RequiresEditFinalize         bool
	SupportsMessageEditing       bool
	SerialReceiveLoop            bool
	MaxMessageLength             int
	AckEmoji                     string
	OkEmoji                      string
	FailEmoji                    string
	RuntimeStatusPlatformKey     string
	EACodeOpen                   string
	EACodeClose                  string
	EADeadlinePrefix             string
	EACmdBudget                  int
	EAReasonBudget               int
	EAActionStyles               map[string]string
}

// Class attribute defaults of the hermes base adapter
func DefaultAdapterTraits() AdapterTraits {
	return AdapterTraits{SupportsAsyncDelivery: true, TypedCommandPrefix: "/", InteractiveResume: true,
		SupportsMessageEditing: true, EACodeOpen: "```\n", EACodeClose: "\n```\n", EADeadlinePrefix: "\n\n",
		EACmdBudget: 3000, EAActionStyles: map[string]string{"once": "primary", "deny": "danger"}}
}

// AuthorizationOpts carries the keyword only arguments forwarded to the runner's authorization check
type AuthorizationOpts struct {
	IsBot    bool
	ThreadID *string
}

// AuthorizationCheck answers whether a sender is allowed, any non boolean result reads as unknown
type AuthorizationCheck func(userID string, chatType, chatID *string, opts AuthorizationOpts) (any, error)

// FatalErrorHandler is the runner callback run once an adapter hits a fatal error
type FatalErrorHandler func(ctx context.Context, adapter PlatformAdapter) error

// PlatformEventHandler is the runner owned boundary for normalized platform events
type PlatformEventHandler func(ctx context.Context, event map[string]any, source *SessionSource) error

// ReactionHandler receives normalized emoji reaction events
type ReactionHandler func(ctx context.Context, event map[string]any) error

// BusySessionHandler takes a message arriving while its session is busy and reports whether it handled it
type BusySessionHandler func(ctx context.Context, event *MessageEvent, sessionKey string) (bool, error)

// TopicRecoveryFn returns a replacement thread id for a source, nil keeping the current one
type TopicRecoveryFn func(source *SessionSource) (*string, error)

// StreamSink is the stream consumer surface the adapter renders message events onto
type StreamSink interface {
	OnDelta(text string)
	OnSegmentBreak()
	OnCommentary(text string)
}

// TransportOwnerRunner is the runner seam that maps a source back to the adapter and profile that made it
type TransportOwnerRunner interface {
	TransportOwner(source *SessionSource) (adapter PlatformAdapter, profile string, ok bool)
}

// Session store seam resolving the profile of a source before the runner stamps it
type sessionProfileResolver interface {
	ResolveProfileForKey(source *SessionSource) (any, error)
}

// PlatformAdapter is the overridable method set hermes platform subclasses override
type PlatformAdapter interface {
	Base() *BasePlatformAdapter
	Connect(ctx context.Context, isReconnect bool) (bool, error)
	Disconnect(ctx context.Context) error
	Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error)
	GetChatInfo(ctx context.Context, chatID string) (map[string]any, error)

	Name() string
	IsConnected() bool
	MessageLenFn() func(string) int
	MaxMessageLengthForChat(chatID string) (int, error)
	MessageLenFnForChat(chatID string) (func(string) int, error)
	EnforcesOwnAccessPolicy() bool
	AuthorizationIsUpstream() bool
	SupportsDraftStreaming(chatType string, metadata map[string]any, chatID string) bool
	PrefersFreshFinalStreaming(content string, metadata map[string]any) bool
	StreamingOverflowLimit() (int, bool)
	SendDraft(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) (SendResult, error)
	RenderMessageEvent(event StreamEvent, sink StreamSink)
	FormatToolEvent(ctx context.Context, event StreamEvent, mode string, previewMaxLen int) (string, bool)
	FormatToolPreview(preview tools.ToolPreview) string
	SendPathDegraded() bool
	MarkConnected(ctx context.Context, listenerBase string)
	MarkDisconnected(ctx context.Context)
	SetFatalError(ctx context.Context, code, message string, retryable bool)
	WirePluginHandlers(ctx context.Context, native any)
	RewirePluginHandlers(ctx context.Context)
	TextBatchKey(ctx context.Context, event *MessageEvent) (string, error)
	EnqueueTextEvent(ctx context.Context, event *MessageEvent) error
	TextBatchDelayFor(pending *MessageEvent) float64
	PopTextBatch(key string) *MessageEvent
	DispatchTextBatch(ctx context.Context, event *MessageEvent) error
	FlushTextBatch(ctx context.Context, key string) error
	HandleMessage(ctx context.Context, event *MessageEvent) error
	CreateHandoffThread(ctx context.Context, parentChatID, name string) (string, bool, error)
	EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (SendResult, error)
	SendPrivateNotice(ctx context.Context, chatID string, userID *string, content, replyTo string, metadata map[string]any) (SendResult, error)
	SendTyping(ctx context.Context, chatID string, metadata map[string]any) error
	StopTyping(ctx context.Context, chatID string, metadata map[string]any) error
	SendMultipleImages(ctx context.Context, chatID string, images []ImageRef, metadata map[string]any, humanDelay float64) (SendResult, error)
	SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (SendResult, error)
	SendAnimation(ctx context.Context, chatID, animationURL, caption, replyTo string, metadata map[string]any) (SendResult, error)
	SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error)
	SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error)
	SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error)
	SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (SendResult, error)
	EAHeader(ctx context.Context) string
	EAReasonLabel(ctx context.Context) string
	EASmartDenyLine(ctx context.Context) string
	EAActionLabelsFor(ctx context.Context) map[string]string
	EAEscape(text string) string
	ExecApprovalCmdBudget(description string, smartDenied bool) int
	SendExecApproval(ctx context.Context, chatID, command, sessionKey string, description *string, metadata map[string]any, allowPermanent, allowSession, smartDenied bool, requestID *string) (SendResult, error)
	SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (SendResult, error)
	SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (SendResult, error)
	EmitWarning(ctx context.Context, chatID, content, replyTo string, metadata map[string]any, logicalPlatform Platform) (*SendResult, error)
	EmitMediaWarning(ctx context.Context, chatID, notice, caption, replyTo string, metadata, shownMetadata map[string]any) (SendResult, error)
	WarningText(ctx context.Context, visible string, hidden *string, logicalPlatform Platform, chatID string, metadata map[string]any) *string
	WarningNotificationsEnabled(ctx context.Context, logicalPlatform Platform, chatID string, metadata map[string]any) bool
	PrepareTTSText(ctx context.Context, text string) string
	PlayTTS(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error)
	SupportsStreamingTTS(chatID string, format AudioFormat) bool
	BeginStreamingTTS(ctx context.Context, chatID string, format AudioFormat, metadata map[string]any) (*StreamingTTSHandle, error)
	WriteStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, chunk []byte) error
	FinishStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, interrupted bool) error
	AbortStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, errText *string) error
	OnProcessingStart(ctx context.Context, event *MessageEvent) error
	OnProcessingComplete(ctx context.Context, event *MessageEvent, outcome ProcessingOutcome) error
	SendRetryIsFinal(result SendResult) bool
	ResumePartialSend(ctx context.Context, chatID string, result SendResult, replyTo string, metadata map[string]any) (*SendResult, error)
	SendPlainFallback(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error)
	ProcessMessageBackground(ctx context.Context, event *MessageEvent, sessionKey string) error
	KeepTyping(ctx context.Context, chatID string, interval float64, metadata map[string]any, stopEvent *InterruptEvent)
	CancelBackgroundTasks(ctx context.Context)
	ToolsetsForSource(source *SessionSource) ([]string, bool)
	FormatMessage(content string) string
}

// BasePlatformAdapter is the shared instance state and default behaviour every platform adapter embeds
type BasePlatformAdapter struct {
	Config        *PlatformConfig
	Platform      Platform
	GatewayRunner any
	Traits        AdapterTraits

	self PlatformAdapter
	mu   sync.Mutex

	messageHandler       MessageHandler
	reactionHandler      ReactionHandler
	platformEventHandler PlatformEventHandler
	topicRecoveryFn      TopicRecoveryFn
	running              bool
	fatalErrorRetryable  bool
	fatalErrorCode       *string
	fatalErrorMessage    *string
	fatalErrorHandler    FatalErrorHandler
	detachedFatalTasks   map[*detachedTask]struct{}

	platformLockTakeoverAllowed   bool
	platformLockTakeoverAttempted bool
	platformLockScope             string
	platformLockIdentity          string

	textBatchMu           sync.Mutex
	pendingTextBatches    map[string]*MessageEvent
	pendingTextBatchTasks map[string]*adapterTask

	busySessionHandler        BusySessionHandler
	ownerProfile              string
	sharedListenerProfile     string
	sharedIngress             *SharedIngress
	authorizationCheck        AuthorizationCheck
	autoTTSDefault            bool
	autoTTSEnabledChats       map[string]struct{}
	autoTTSDisabledChats      map[string]struct{}
	statusText                map[string]string
	statusWriteLogged         map[[2]string]struct{}
	sessionStore              any
	pluginHandlerNative       any
	pluginHandlersWired       map[[2]string]struct{}
	textBatchDelaySeconds     float64
	textBatchSplitDelaySecond float64

	streamingTTSCompletedTurns map[string]struct{}
	typingPaused               map[string]struct{}
	activeSessions             map[string]*InterruptEvent
	postDeliveryCallbacks      map[string]postDeliveryEntry

	messageHandlerOwner     any
	noMessageHandlerLogged  bool
	pendingMessages         map[string]*MessageEvent
	requeueCounts           map[string]int
	sessionTasks            map[string]*adapterTask
	busyTextMode            string
	busyTextDebounceSeconds float64
	busyTextHardCapSeconds  float64
	humanDelayRangeMs       *[2]int
	textDebounce            map[string]*TextDebounceState
	backgroundTasks         map[*adapterTask]struct{}
	expectedCancelledTasks  map[*adapterTask]struct{}
	parkedNoticeSentAt      map[parkedNoticeKey]float64
	busyIngress             busyIngressState
}

// One detached fatal error handler run kept reachable until it finishes
type detachedTask struct {
	done chan struct{}
	err  error
}

// Whether the task has finished
func (t *adapterTask) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// One adapter owned goroutine with cancellation and completion, the asyncio task of hermes
type adapterTask struct {
	cancel    context.CancelFunc
	done      chan struct{}
	err       error
	mu        sync.Mutex
	callbacks []func()
	settled   bool
}

// Runs fn when the task finishes and before any waiter resumes, at once for a finished task
func (t *adapterTask) addDoneCallback(fn func()) {
	t.mu.Lock()
	if t.settled {
		t.mu.Unlock()
		fn()
		return
	}
	t.callbacks = append(t.callbacks, fn)
	t.mu.Unlock()
}

// Marks the task finished, runs its done callbacks in order and wakes its waiters
func (t *adapterTask) settle() {
	t.mu.Lock()
	callbacks := t.callbacks
	t.callbacks, t.settled = nil, true
	t.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
	close(t.done)
}

// Adapter owned goroutine handle the platform adapters hold and cancel
type AdapterTask = adapterTask

// Whether the task has finished, the asyncio Task done of hermes
func (t *adapterTask) Done() bool { return t.finished() }

// Base adapter bound to the concrete adapter that embeds it, with the hermes constructor defaults
func NewBasePlatformAdapter(config *PlatformConfig, platform Platform, self PlatformAdapter) *BasePlatformAdapter {
	b := &BasePlatformAdapter{Config: config, Platform: platform, Traits: DefaultAdapterTraits(), self: self}
	b.fatalErrorRetryable = true
	b.detachedFatalTasks = map[*detachedTask]struct{}{}
	b.pendingTextBatches = map[string]*MessageEvent{}
	b.pendingTextBatchTasks = map[string]*adapterTask{}
	b.autoTTSEnabledChats = map[string]struct{}{}
	b.autoTTSDisabledChats = map[string]struct{}{}
	b.statusText = map[string]string{}
	b.busyTextMode = "interrupt"
	b.busyTextDebounceSeconds = DefaultBusyTextDebounceSeconds
	b.busyTextHardCapSeconds = DefaultBusyTextHardCapSeconds
	return b
}

// The base itself, the embedded value every adapter exposes
func (b *BasePlatformAdapter) Base() *BasePlatformAdapter { return b }

// The concrete adapter bound at construction, the base itself when none was bound
func (b *BasePlatformAdapter) Self() PlatformAdapter {
	if b.self != nil {
		return b.self
	}
	return baseOnly{b}
}

// Base adapter seen through the overridable surface when no concrete adapter was bound
type baseOnly struct{ *BasePlatformAdapter }

func (a baseOnly) Connect(context.Context, bool) (bool, error) {
	return false, fmt.Errorf("NotImplementedError: %s.connect is abstract", "BasePlatformAdapter")
}

func (a baseOnly) Disconnect(context.Context) error {
	return fmt.Errorf("NotImplementedError: %s.disconnect is abstract", "BasePlatformAdapter")
}

func (a baseOnly) Send(context.Context, string, string, string, map[string]any) (SendResult, error) {
	return SendResult{}, fmt.Errorf("NotImplementedError: %s.send is abstract", "BasePlatformAdapter")
}

func (a baseOnly) GetChatInfo(context.Context, string) (map[string]any, error) {
	return nil, fmt.Errorf("NotImplementedError: %s.get_chat_info is abstract", "BasePlatformAdapter")
}

// Type name of the bound adapter for not implemented messages
func (b *BasePlatformAdapter) typeName() string {
	t := reflect.TypeOf(b.Self())
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// Stores or clears the live working state phrase the next typing refresh renders
func (b *BasePlatformAdapter) SetStatusText(chatID string, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	store := lazyAttr(&b.statusText)
	if text != "" {
		store[chatID] = text
	} else {
		delete(store, chatID)
	}
}

// Live working state phrase of a chat
func (b *BasePlatformAdapter) StatusText(chatID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	text, ok := b.statusText[chatID]
	return text, ok
}

// Length measure of message size, code points unless the platform counts differently
func (b *BasePlatformAdapter) MessageLenFn() func(string) int {
	return func(s string) int { return len([]rune(s)) }
}

// Per chat maximum length, the adapter's declared cap or 4096
func (b *BasePlatformAdapter) MaxMessageLengthForChat(chatID string) (int, error) {
	return orDefault(func() (int, error) {
		if b.Traits.MaxMessageLength == 0 {
			return 4096, nil
		}
		return b.Traits.MaxMessageLength, nil
	}, 4096), nil
}

// Per chat length measure, the adapter wide one unless a relay overrides it
func (b *BasePlatformAdapter) MessageLenFnForChat(chatID string) (func(string) int, error) {
	return b.Self().MessageLenFn(), nil
}

// Whether the adapter enforces its own config driven access policy at intake
func (b *BasePlatformAdapter) EnforcesOwnAccessPolicy() bool { return false }

// Whether a trusted upstream already authorized inbound, relay adapters only
func (b *BasePlatformAdapter) AuthorizationIsUpstream() bool { return false }

// Whether native streaming draft updates work for this chat
func (b *BasePlatformAdapter) SupportsDraftStreaming(chatType string, metadata map[string]any, chatID string) bool {
	return false
}

// Whether the stream consumer finalizes with a fresh message instead of editing the preview
func (b *BasePlatformAdapter) PrefersFreshFinalStreaming(content string, metadata map[string]any) bool {
	return false
}

// Single message length the stream consumer may accumulate, false meaning the declared maximum
func (b *BasePlatformAdapter) StreamingOverflowLimit() (int, bool) { return 0, false }

// Streaming draft preview, an error for adapters that never override it
func (b *BasePlatformAdapter) SendDraft(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) (SendResult, error) {
	return SendResult{}, fmt.Errorf("NotImplementedError: %s does not implement send_draft", b.typeName())
}

// Renders a message chunk, intermediate stop or commentary onto the stream sink
func (b *BasePlatformAdapter) RenderMessageEvent(event StreamEvent, sink StreamSink) {
	switch ev := event.(type) {
	case MessageChunk:
		if ev.Text != "" {
			sink.OnDelta(ev.Text)
		}
	case MessageStop:
		if !ev.Final {
			sink.OnSegmentBreak()
		}
	case Commentary:
		if ev.Text != "" {
			sink.OnCommentary(ev.Text)
		}
	}
}

// Rendered chrome of a tool call chunk, false when the event is not one
func (b *BasePlatformAdapter) FormatToolEvent(ctx context.Context, event StreamEvent, mode string, previewMaxLen int) (string, bool) {
	ev, ok := event.(ToolCallChunk)
	if !ok {
		return "", false
	}
	env := mediaToolsEnv(ctx)
	emoji, tool := tools.GetToolEmoji(env, ev.ToolName, "⚙️"), ev.ToolName
	if mode == "verbose" && len(ev.Args) > 0 {
		keys := []any{}
		ordered := pyjson.NewObject()
		for _, k := range ev.OrderedArgKeys() {
			keys = append(keys, k)
			ordered.Set(k, ev.Args[k])
		}
		argsStr := tools.VerboseToolArgsText(ordered, previewMaxLen, b.Traits.SupportsCodeBlocks)
		return i18n.T(ctx, "gateway.progress.tool_verbose", "", map[string]any{"emoji": emoji, "tool": tool,
			"keys": keys, "args": argsStr}), true
	}
	if ev.Preview == nil || *ev.Preview == "" {
		return i18n.T(ctx, "gateway.progress.tool_pending", "", map[string]any{"emoji": emoji, "tool": tool}), true
	}
	if mode == "verbose" {
		return i18n.T(ctx, "gateway.progress.tool_preview", "", map[string]any{"emoji": emoji, "tool": tool,
			"preview": *ev.Preview}), true
	}
	limit := 40
	if previewMaxLen > 0 {
		limit = previewMaxLen
	}
	prepared := tools.PrepareToolPreview(env, ev.ToolName, ev.Args, *ev.Preview, limit)
	return i18n.T(ctx, "gateway.progress.tool_preview", "", map[string]any{"emoji": emoji, "tool": tool,
		"preview": b.Self().FormatToolPreview(prepared)}), true
}

// Platform native formatting of a compact tool preview, the plain text by default
func (b *BasePlatformAdapter) FormatToolPreview(preview tools.ToolPreview) string {
	return preview.Text
}

// Whether a fatal error is recorded
func (b *BasePlatformAdapter) HasFatalError() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fatalErrorMessage != nil
}

// Recorded fatal error message
func (b *BasePlatformAdapter) FatalErrorMessage() *string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fatalErrorMessage
}

// Recorded fatal error code
func (b *BasePlatformAdapter) FatalErrorCode() *string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fatalErrorCode
}

// Whether the recorded fatal error may be retried
func (b *BasePlatformAdapter) FatalErrorRetryable() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fatalErrorRetryable
}

// Explicit voice on wins, then explicit voice off, then the voice auto tts default
func (b *BasePlatformAdapter) shouldAutoTTSForChat(chatID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.autoTTSEnabledChats[chatID]; ok {
		return true
	}
	_, off := b.autoTTSDisabledChats[chatID]
	return !off && b.autoTTSDefault
}

// Installs the runner callback for fatal errors
func (b *BasePlatformAdapter) SetFatalErrorHandler(handler FatalErrorHandler) {
	b.mu.Lock()
	b.fatalErrorHandler = handler
	b.mu.Unlock()
}

// Whether connect succeeded but delivery is not yet confirmed
func (b *BasePlatformAdapter) SendPathDegraded() bool { return false }

// Marks the adapter running, clears the fatal error and publishes connected or degraded status
func (b *BasePlatformAdapter) MarkConnected(ctx context.Context, listenerBase string) {
	b.mu.Lock()
	b.running = true
	b.fatalErrorCode, b.fatalErrorMessage = nil, nil
	b.fatalErrorRetryable = true
	b.mu.Unlock()
	if b.Self().SendPathDegraded() {
		b.markDegraded(ctx)
		return
	}
	fields := map[string]any{"platform_state": "connected", "error_code": nil, "error_message": nil}
	if listenerBase != "" {
		fields["listener_base"] = listenerBase
	}
	b.writeRuntimeStatusSafe(ctx, "connected", fields)
}

// Publishes retrying for a running adapter whose delivery path is unproven
func (b *BasePlatformAdapter) markDegraded(ctx context.Context) {
	b.writeRuntimeStatusSafe(ctx, "connected_degraded", map[string]any{"platform_state": "retrying", "error_code": nil,
		"error_message": DegradedStatusMessage})
}

// Marks the adapter stopped and publishes disconnected unless a fatal error stands
func (b *BasePlatformAdapter) MarkDisconnected(ctx context.Context) {
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	if !b.HasFatalError() {
		b.writeRuntimeStatusSafe(ctx, "disconnected", map[string]any{"platform_state": "disconnected", "error_code": nil,
			"error_message": nil})
	}
}

// Stops the adapter, records the fatal error and publishes fatal status
func (b *BasePlatformAdapter) SetFatalError(ctx context.Context, code, message string, retryable bool) {
	b.mu.Lock()
	b.running = false
	b.fatalErrorCode, b.fatalErrorMessage = &code, &message
	b.fatalErrorRetryable = retryable
	b.mu.Unlock()
	b.writeRuntimeStatusSafe(ctx, "fatal", map[string]any{"platform_state": "fatal", "error_code": code, "error_message": message})
}

// Publishes runtime status, logging a failure once at warning then at debug per context
func (b *BasePlatformAdapter) writeRuntimeStatusSafe(ctx context.Context, statusContext string, fields map[string]any) {
	key := b.Traits.RuntimeStatusPlatformKey
	if key == "" {
		key = string(b.Platform)
	}
	payload := map[string]any{"platform": key}
	for k, v := range fields {
		payload[k] = v
	}
	_, err := publishRuntimeStatusFn(ctx, RuntimeStatusUpdate{Fields: payload})
	if err == nil {
		return
	}
	b.mu.Lock()
	logged := lazyAttr(&b.statusWriteLogged)
	mark := [2]string{string(b.Platform), statusContext}
	_, seen := logged[mark]
	logged[mark] = struct{}{}
	b.mu.Unlock()
	if !seen {
		slog.Warn(fmt.Sprintf("Failed to write runtime status (%s) for %s: %v (further failures at debug level)", statusContext,
			string(b.Platform), err))
		return
	}
	slog.Debug(fmt.Sprintf("Failed to write runtime status (%s) for %s: %v", statusContext, string(b.Platform), err))
}

// Runtime status publisher, a package var the way hermes tests patch the status module
var publishRuntimeStatusFn = PublishRuntimeStatus

// Runs the fatal error handler shielded from the caller's cancellation, a detached run logs its own failure
func (b *BasePlatformAdapter) NotifyFatalError(ctx context.Context) error {
	b.mu.Lock()
	handler := b.fatalErrorHandler
	b.mu.Unlock()
	if handler == nil {
		return nil
	}
	task := &detachedTask{done: make(chan struct{})}
	b.mu.Lock()
	lazyAttr(&b.detachedFatalTasks)[task] = struct{}{}
	b.mu.Unlock()
	self := b.Self()
	go func() {
		defer close(task.done)
		task.err = handler(context.WithoutCancel(ctx), self)
		b.mu.Lock()
		delete(b.detachedFatalTasks, task)
		b.mu.Unlock()
	}()
	select {
	case <-task.done:
		return task.err
	case <-ctx.Done():
		go func() {
			<-task.done
			consumeDetachedHandlerException(task.err)
		}()
		return ctx.Err()
	}
}

// Fatal error handler runs still in flight
func (b *BasePlatformAdapter) DetachedFatalTaskCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.detachedFatalTasks)
}

// Arms or disarms the one explicit replace takeover of the initial connect
func (b *BasePlatformAdapter) SetPlatformLockTakeoverAllowed(allowed bool) {
	b.mu.Lock()
	b.platformLockTakeoverAllowed = allowed
	b.mu.Unlock()
}

// Scoped lock acquire hook, a package var the way hermes tests patch the status module
var acquireScopedLockFn = AcquireScopedLock

// Scoped lock takeover hook, a package var the way hermes tests patch the status module
var takeOverScopedLockHolderFn = func(ctx context.Context, record *pyjson.Object) (int, bool) {
	return TakeOverScopedLockHolder(ctx, record, 20, 20)
}

// Takes the adapter's scoped lock, an armed replace takes over a live holder once, a conflict is a retryable fatal error
func (b *BasePlatformAdapter) AcquirePlatformLock(ctx context.Context, scope, identity, resourceDesc string) (bool, error) {
	b.mu.Lock()
	b.platformLockScope, b.platformLockIdentity = scope, identity
	b.mu.Unlock()
	meta := pyjson.NewObject()
	meta.Set("platform", string(b.Platform))
	acquired, existing, err := acquireScopedLockFn(ctx, scope, identity, meta)
	var unusable *ScopedLockDirUnusable
	if errors.As(err, &unusable) {
		b.mu.Lock()
		b.platformLockIdentity = ""
		b.mu.Unlock()
		b.Self().SetFatalError(ctx, "lock_dir_unusable", fmt.Sprintf("%s lock cannot be taken: the gateway lock directory %s is not "+
			"usable by this user (likely created by an elevated Icebath). To repair, %s; the gateway retries on its own.",
			resourceDesc, unusable.Dir, home.UnusableDirRemedy(unusable.Dir)), true)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if acquired {
		return true, nil
	}
	name := b.Self().Name()
	b.mu.Lock()
	armed := b.platformLockTakeoverAllowed && !b.platformLockTakeoverAttempted && existing != nil
	if armed {
		b.platformLockTakeoverAllowed = false
		b.platformLockTakeoverAttempted = true
	}
	b.mu.Unlock()
	if armed {
		if ownerPID, ok := takeOverScopedLockHolderFn(ctx, existing); ok {
			slog.Warn(fmt.Sprintf("[%s] %s was held by gateway PID %d — explicit --replace handoff completed", name, resourceDesc, ownerPID))
			if acquired, existing, err = acquireScopedLockFn(ctx, scope, identity, meta); err != nil {
				return false, err
			}
			if acquired {
				slog.Info(fmt.Sprintf("[%s] Acquired %s after taking over PID %d", name, resourceDesc, ownerPID))
				return true, nil
			}
		}
	}
	var ownerPID any
	if existing != nil {
		ownerPID, _ = existing.Get("pid")
	}
	var ownerLabel string
	if existing != nil {
		ownerLabel = ScopedLockOwnerLabel(existing)
	} else {
		ownerLabel = ScopedLockOwnerLabel(nil)
	}
	pidPart := ""
	if pyrepr.Truthy(ownerPID) {
		pidPart = fmt.Sprintf(" (PID %s)", pyrepr.Str(ownerPID))
	}
	holder, remedy := pidPart, " Stop the other gateway first."
	if ownerLabel != "" {
		holder = fmt.Sprintf(" by the '%s' profile gateway%s", ownerLabel, pidPart)
		remedy = fmt.Sprintf(" Stop that gateway first (icebath --profile %s gateway stop).", ownerLabel)
	}
	message := fmt.Sprintf("%s already in use%s.%s", resourceDesc, holder, remedy)
	slog.Error(fmt.Sprintf("[%s] %s", name, message))
	b.Self().SetFatalError(ctx, scope+"_lock", message, true)
	return false, nil
}

// Releases the scoped lock the adapter acquired
func (b *BasePlatformAdapter) ReleasePlatformLock() {
	b.mu.Lock()
	scope, identity := b.platformLockScope, b.platformLockIdentity
	b.mu.Unlock()
	if identity == "" {
		return
	}
	ReleaseScopedLock(scope, identity)
	b.mu.Lock()
	b.platformLockIdentity = ""
	b.mu.Unlock()
}

// Identity of the held platform lock, empty when none is held
func (b *BasePlatformAdapter) PlatformLockIdentity() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.platformLockIdentity
}

// Plugin native handler factories of a platform, a package var the way hermes tests patch the plugin manager
var platformHandlerFactoriesFn = func(ctx context.Context, platform string) ([]*plugins.PlatformHandlerFactory, error) {
	manager := plugins.GetPluginManager(ctx)
	if manager == nil {
		return nil, errors.New("plugin manager unavailable")
	}
	return manager.GetPlatformHandlerFactories(platform), nil
}

// Qualified name of a factory, the Go function name standing in for the Python qualname
func factoryQualname(factory any) string {
	v := reflect.ValueOf(factory)
	if v.Kind() == reflect.Func {
		if fn := runtime.FuncForPC(v.Pointer()); fn != nil {
			return fn.Name()
		}
	}
	return pyrepr.Repr(factory)
}

// Calls a plugin handler factory with the native client and the adapter
func callHandlerFactory(factory any, native any, adapter PlatformAdapter) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	switch f := factory.(type) {
	case func(native, adapter any):
		f(native, adapter)
		return nil
	case func(native, adapter any) error:
		return f(native, adapter)
	case func(native any, adapter PlatformAdapter):
		f(native, adapter)
		return nil
	case func(native any, adapter PlatformAdapter) error:
		return f(native, adapter)
	}
	return fmt.Errorf("TypeError: '%s' object is not callable", pyrepr.TypeName(factory))
}

// Runs every plugin native handler factory not yet wired on this native client, each isolated
func (b *BasePlatformAdapter) WirePluginHandlers(ctx context.Context, native any) {
	name := b.Self().Name()
	factories, err := platformHandlerFactoriesFn(ctx, string(b.Platform))
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Could not load plugin handler factories: %v", name, err))
		return
	}
	b.mu.Lock()
	if b.pluginHandlersWired == nil || !sameNative(b.pluginHandlerNative, native) {
		b.pluginHandlerNative = native
		b.pluginHandlersWired = map[[2]string]struct{}{}
	}
	b.mu.Unlock()
	for _, entry := range factories {
		key := [2]string{entry.Plugin, factoryQualname(entry.Factory)}
		b.mu.Lock()
		_, wired := b.pluginHandlersWired[key]
		b.mu.Unlock()
		if wired {
			continue
		}
		if err := callHandlerFactory(entry.Factory, native, b.Self()); err != nil {
			slog.Error(fmt.Sprintf("[%s] Plugin '%s' handler factory raised: %v", name, entry.Plugin, err))
		} else {
			slog.Info(fmt.Sprintf("[%s] Wired native handlers from plugin '%s'", name, entry.Plugin))
		}
		b.mu.Lock()
		b.pluginHandlersWired[key] = struct{}{}
		b.mu.Unlock()
	}
}

// Identity comparison of two native clients
func sameNative(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.Slice, reflect.UnsafePointer:
		return va.Pointer() == vb.Pointer()
	}
	if va.Comparable() {
		return a == b
	}
	return false
}

// Wires the handlers of plugins loaded after connect, nothing before connect wired once
func (b *BasePlatformAdapter) RewirePluginHandlers(ctx context.Context) {
	b.mu.Lock()
	wired, native := b.pluginHandlersWired, b.pluginHandlerNative
	b.mu.Unlock()
	if wired == nil {
		return
	}
	b.Self().WirePluginHandlers(ctx, native)
}

// Title cased platform value
func (b *BasePlatformAdapter) Name() string { return pystr.Title(string(b.Platform)) }

// Platform value the shared metrics label this adapter's deliveries with
func (b *BasePlatformAdapter) MetricsPlatform() any { return string(b.Platform) }

// Whether the adapter is connected
func (b *BasePlatformAdapter) IsConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// Installed incoming message handler
func (b *BasePlatformAdapter) MessageHandler() MessageHandler {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messageHandler
}

// Installs the incoming message handler
func (b *BasePlatformAdapter) SetMessageHandler(handler MessageHandler) {
	b.mu.Lock()
	b.messageHandler = handler
	b.messageHandlerOwner = nil
	b.mu.Unlock()
}

// Installs the gateway owned normalized platform event boundary
func (b *BasePlatformAdapter) SetPlatformEventHandler(handler PlatformEventHandler) {
	b.mu.Lock()
	b.platformEventHandler = handler
	b.mu.Unlock()
}

// Installed platform event boundary, nil failing closed
func (b *BasePlatformAdapter) PlatformEventHandler() PlatformEventHandler {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.platformEventHandler
}

// Installs or clears the thread id recovery hook run before session keying
func (b *BasePlatformAdapter) SetTopicRecoveryFn(fn TopicRecoveryFn) {
	b.mu.Lock()
	b.topicRecoveryFn = fn
	b.mu.Unlock()
}

// Rewrites the event's thread id in place when the recovery hook returns a different one
func (b *BasePlatformAdapter) applyTopicRecovery(event *MessageEvent) {
	b.mu.Lock()
	recoverFn := b.topicRecoveryFn
	b.mu.Unlock()
	source := event.Source
	if recoverFn == nil || source == nil {
		return
	}
	recovered, err := recoverFn(source)
	if err != nil {
		slog.Debug(fmt.Sprintf("topic recovery hook failed: %v", err))
		return
	}
	current := ""
	if source.ThreadID != nil {
		current = *source.ThreadID
	}
	if recovered == nil || *recovered == current {
		return
	}
	value := *recovered
	event.Source = ReplaceSource(source, func(s *SessionSource) { s.ThreadID = &value })
}

// Installed handler for messages arriving during active sessions
func (b *BasePlatformAdapter) BusySessionHandler() BusySessionHandler {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.busySessionHandler
}

// Installs the handler for messages arriving during active sessions
func (b *BasePlatformAdapter) SetBusySessionHandler(handler BusySessionHandler) {
	b.mu.Lock()
	b.busySessionHandler = handler
	b.mu.Unlock()
}

// Installs the handler for platform native emoji reaction events
func (b *BasePlatformAdapter) SetReactionHandler(handler ReactionHandler) {
	b.mu.Lock()
	b.reactionHandler = handler
	b.mu.Unlock()
}

// Installed reaction handler
func (b *BasePlatformAdapter) ReactionHandler() ReactionHandler {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reactionHandler
}

// Registers the runner's sender authorization check
func (b *BasePlatformAdapter) SetAuthorizationCheck(check AuthorizationCheck) {
	b.mu.Lock()
	b.authorizationCheck = check
	b.mu.Unlock()
}

// True or false from the registered check, nil when no check exists, it fails or answers a non boolean
func (b *BasePlatformAdapter) IsSenderAuthorized(userID string, chatType, chatID *string, opts AuthorizationOpts) *bool {
	b.mu.Lock()
	check := b.authorizationCheck
	b.mu.Unlock()
	if userID == "" || check == nil {
		return nil
	}
	name := b.Self().Name()
	result, err := check(userID, chatType, chatID, opts)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Authorization check raised for user %s; treating as unknown: %v", name, userID, err))
		return nil
	}
	if v, ok := result.(bool); ok {
		return &v
	}
	slog.Warn(fmt.Sprintf("[%s] Authorization check returned %s for user %s; treating as unknown", name, pyrepr.TypeName(result), userID))
	return nil
}

// Installs the session store adapters consult for active thread sessions
func (b *BasePlatformAdapter) SetSessionStore(store any) {
	b.mu.Lock()
	b.sessionStore = store
	b.mu.Unlock()
}

// Installed session store
func (b *BasePlatformAdapter) SessionStore() any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionStore
}

// Declares the owning multiplex profile, default and blank names reading as the primary
func (b *BasePlatformAdapter) SetOwnerProfile(profileName string) {
	name := pystr.Strip(profileName)
	if name == "default" {
		name = ""
	}
	b.mu.Lock()
	b.ownerProfile = name
	b.mu.Unlock()
}

// Owning multiplex profile, empty on the primary
func (b *BasePlatformAdapter) OwnerProfile() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ownerProfile
}

// Routes a secondary's port binding adapter through the default profile's shared listener
func (b *BasePlatformAdapter) SetSharedListenerProfile(profile string) {
	b.mu.Lock()
	b.sharedListenerProfile = profile
	b.mu.Unlock()
}

// Profile served through the shared listener, empty when the adapter binds its own port
func (b *BasePlatformAdapter) SharedListenerProfile() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sharedListenerProfile
}

// Transport profile for identity resolution, empty deriving it from the registry
func (b *BasePlatformAdapter) ownerTransportProfile() string {
	owner := b.OwnerProfile()
	if pystr.Strip(owner) != "" {
		return owner
	}
	return ""
}

// Pins the source's routing identity before any key derivation, nil when unresolved or no runner seam exists
func (b *BasePlatformAdapter) canonicalize(ctx context.Context, source *SessionSource) *RoutingIdentity {
	if source == nil {
		return nil
	}
	if identity := IdentityOf(source); identity != nil {
		return identity
	}
	runner, ok := b.GatewayRunner.(TransportOwnerRunner)
	if !ok || runner == nil {
		return nil
	}
	identity, err := canonicalIdentity(ctx, source, runner, b.Self(), b.ownerTransportProfile())
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] identity resolution failed; using legacy key readers: %v", b.Self().Name(), err))
		return nil
	}
	return identity
}

// Whether the event must be dropped because its route targets a profile this gateway does not serve
func (b *BasePlatformAdapter) dropUnresolved(ctx context.Context, event *MessageEvent) bool {
	source := event.Source
	if b.canonicalize(ctx, source) != nil {
		return false
	}
	if source == nil || !source.ProfileRouteRejected {
		return false
	}
	dropRejectedRoute(ctx, b.Self(), source)
	return true
}

// Profile namespace of an adapter derived key, pinned identity then source profile then owner then store resolver
func (b *BasePlatformAdapter) sessionKeyProfile(source *SessionSource) string {
	if identity := IdentityOf(source); identity != nil {
		return identity.SessionKeyProfile()
	}
	if source != nil && source.Profile != nil && pystr.Strip(*source.Profile) != "" {
		return *source.Profile
	}
	if owner := b.OwnerProfile(); pystr.Strip(owner) != "" {
		return owner
	}
	resolver, ok := b.SessionStore().(sessionProfileResolver)
	if !ok || resolver == nil {
		return ""
	}
	resolved, err := resolver.ResolveProfileForKey(source)
	if err != nil {
		return ""
	}
	if s, ok := resolved.(string); ok && pystr.Strip(s) != "" {
		return s
	}
	return ""
}

// Float from config extra, a non finite, negative or unparseable value reading as the default, then clamped
func (b *BasePlatformAdapter) coerceFloatExtra(key string, def, minValue float64, maxValue *float64) float64 {
	var raw any
	if b.Config != nil && b.Config.Extra != nil {
		raw = b.Config.Extra[key]
	}
	parsed, err := pyint.Float(raw)
	if err != nil {
		parsed = def
	}
	if math.IsInf(parsed, 0) || math.IsNaN(parsed) || parsed < 0 {
		parsed = def
	}
	parsed = math.Max(parsed, minValue)
	if maxValue != nil && parsed > *maxValue {
		slog.Warn(fmt.Sprintf("%s=%s exceeds the %s ceiling; clamped", key, pyrepr.Float(parsed), pyrepr.Float(*maxValue)))
		parsed = *maxValue
	}
	return parsed
}

// Reads the text batch delay and split delay from config extra at the shared cadence
func (b *BasePlatformAdapter) ConfigureTextBatchDelays() {
	maxDelay, maxSplit := textBatchMaxDelayS, textBatchMaxSplitDelayS
	delay := b.coerceFloatExtra("text_batch_delay_seconds", textBatchDefaultDelayS, 0, &maxDelay)
	split := b.coerceFloatExtra("text_batch_split_delay_seconds", textBatchDefaultSplitDelayS, delay, &maxSplit)
	b.mu.Lock()
	b.textBatchDelaySeconds, b.textBatchSplitDelaySecond = delay, split
	b.mu.Unlock()
}

// Configured text batch delay and split delay in seconds
func (b *BasePlatformAdapter) TextBatchDelays() (float64, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.textBatchDelaySeconds, b.textBatchSplitDelaySecond
}

// Adapter level session key of an event, profile namespaced like the agent run
func (b *BasePlatformAdapter) EventSessionKey(ctx context.Context, event *MessageEvent) (string, error) {
	return b.SourceSessionKey(ctx, event.Source)
}

// Session key of a source after its identity is pinned
func (b *BasePlatformAdapter) SourceSessionKey(ctx context.Context, source *SessionSource) (string, error) {
	b.canonicalize(ctx, source)
	extra := map[string]any{}
	if b.Config != nil && b.Config.Extra != nil {
		extra = b.Config.Extra
	}
	groupPerUser, threadPerUser := true, false
	if v, ok := extra["group_sessions_per_user"]; ok {
		groupPerUser = pyrepr.Truthy(v)
	}
	if v, ok := extra["thread_sessions_per_user"]; ok {
		threadPerUser = pyrepr.Truthy(v)
	}
	return BuildSessionKey(ctx, source, groupPerUser, threadPerUser, b.sessionKeyProfile(source))
}

// Session scoped key for text batching
func (b *BasePlatformAdapter) TextBatchKey(ctx context.Context, event *MessageEvent) (string, error) {
	return b.EventSessionKey(ctx, event)
}
