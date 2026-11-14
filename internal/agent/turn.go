package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/internal/model"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/tool"
)

type actionKind uint8

const (
	actionNext actionKind = iota
	actionAgain
	actionStop
	actionDone
	actionRetry
	actionLeave
)

type action struct {
	kind   actionKind
	result *TurnResult
}

func next() action {
	return action{kind: actionNext}
}

func again() action {
	return action{kind: actionAgain}
}

func stop() action {
	return action{kind: actionStop}
}

func done(r *TurnResult) action {
	return action{kind: actionDone, result: r}
}

func retry() action {
	return action{kind: actionRetry}
}

func leave() action {
	return action{kind: actionLeave}
}

type retryState struct {
	codexAuthRetryAttempted               bool
	anthropicAuthRetryAttempted           bool
	portalAuthRetryAttempted              bool
	portalPaidEntitlementRefreshAttempted bool
	welcomeModelSwitchAttempted           bool
	welcomeRouteHealAttempted             bool
	providerAuthRefreshAttempted          bool
	providerStaleRefreshAttempted         bool
	vertexAuthRetryAttempted              bool
	thinkingSigRetryAttempted             bool
	invalidEncryptedContentRetryAttempted bool
	nativeCompactionRejectRetryAttempted  bool
	imageShrinkRetryAttempted             bool
	multimodalToolContentRetryAttempted   bool
	reasoningMandatoryRetryAttempted      bool
	oauth1MBetaRetryAttempted             bool
	llamaCppGrammarRetryAttempted         bool
	primaryRecoveryAttempted              bool
	hasRetried429                         bool
	affordable402ClampAttempted           bool
	authFailoverAttempted                 bool
	autoRecoveryCyclesUsed                int
	restartWithCompressedMessages         bool
	restartWithLengthContinuation         bool
	restartWithRebuiltMessages            bool
	restartWithRedirectedMessages         bool
}

type truncatedPart struct {
	text string
	stub bool
}

type turn struct {
	a *Agent

	input         TurnInput
	userMessage   wire.Message
	systemMessage string
	moa           *MixtureTurn
	originalUser  wire.Message
	history       []*Message
	taskID        string
	turnID        string
	reviewMemory  bool
	pluginContext string
	prefetch      string

	messages               []*Message
	systemPrompt           string
	userIndex              int
	preflightBlocked       bool
	maxCompressionAttempts int

	apiCalls                     int
	final                        string
	finalSet                     bool
	interrupted                  bool
	failed                       bool
	codexAckContinuations        int
	lengthContinueRetries        int
	restartCount                 int
	outerErrors                  int
	truncatedToolCallRetries     int
	truncatedParts               []truncatedPart
	compressionAttempts          int
	lastPreflightPressure        *int
	overflowRecoveryPending      bool
	compressionTimeoutExhausted  bool
	exit                         ExitReason
	pendingVerification          *string
	pendingVerificationPreviewed bool
	pendingMoA                   *MixturePrepared

	apiMessages    []wire.Message
	tools          []wire.Tool
	moaPrepared    *MixturePrepared
	approxTokens   int
	pressureTokens int
	totalChars     int
	apiStart       time.Time
	retryCount     int
	maxRetries     int
	retry          retryState
	finishReason   wire.FinishReason
	response       *wire.Response
	prepared       *model.Prepared
	requestID      string
	trace          []model.TraceEntry
	apiDuration    time.Duration
	assistant      *Message

	interrupt *tool.Interrupt
}

func (a *Agent) newTurn(in TurnInput) *turn {
	return &turn{
		a:                      a,
		input:                  in,
		userMessage:            in.Message,
		systemMessage:          in.System,
		moa:                    in.MoA,
		originalUser:           in.Message,
		history:                in.History,
		taskID:                 in.TaskID,
		userIndex:              -1,
		maxCompressionAttempts: a.limits.maxCompressionAttempts,
		exit:                   ExitUnknown,
		finishReason:           wire.FinishStop,
	}
}

func requestID(turnID string, call int) string {
	return turnID + ":api:" + strconv.Itoa(call)
}

func (t *turn) spliceProviderProjection(resp *wire.Response) int {
	if resp == nil {
		return 0
	}
	now := t.a.now()
	for i := range resp.Projected {
		t.messages = Append(t.messages, &Message{Message: resp.Projected[i]}, now)
	}
	if rows := len(resp.Projected); rows > 0 {
		slog.Debug(fmt.Sprintf("spliced %d provider-projected transcript row(s) from %s", rows, t.a.o.Target.Provider))
	}
	if resp.ToolIterations > 0 {
		t.a.itersSinceSkill.Add(int64(resp.ToolIterations))
	}
	return len(resp.Projected)
}

func (t *turn) bindInterrupt(ctx context.Context) context.Context {
	tctx, in := tool.NewInterrupt(ctx)
	t.interrupt = in
	t.a.bindTurnInterrupt(in)
	return tctx
}

func (t *turn) beginIteration() {
	t.apiMessages, t.tools, t.moaPrepared = nil, nil, nil
	t.approxTokens, t.pressureTokens, t.totalChars = 0, 0, 0
	t.apiStart = time.Time{}
	t.retryCount = 0
	t.maxRetries = t.a.limits.apiMaxRetries
	t.retry = retryState{}
	t.finishReason = wire.FinishStop
	t.response, t.prepared, t.requestID, t.trace = nil, nil, "", nil
	t.apiDuration = 0
	t.assistant = nil
}
