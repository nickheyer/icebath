package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/tools"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Consecutive lagging turns after which the transcript lag line escalates from warning to error
const transcriptLagEscalationTurns = 3

// Exact card destination refusals kept for older adapters without destination preflight
var cardDestinationRefusals = map[string]bool{
	"No Slack thread target":                   true,
	"slack task_card requires a thread anchor": true,
	"slack task_card requires a thread anchor (Slack streams are thread replies)": true,
}

// Result keys the gateway projection carries verbatim for a finite viewer's usage ledger
var ledgerPassthroughKeys = []string{"estimated_cost_usd", "cost_status", "cost_source", "cache_read_tokens",
	"cache_write_tokens", "reasoning_tokens", "total_tokens", "provider", "turn_exit_reason", "service_tier"}

// Failure descriptor keys the error card and window plan rewording read beside failure_reason
var failureDescriptorKeys = []string{"free_tier", "failure_retryable", "failure_resets_at", "usage_window_message"}

// Notice a native clarify card shows once its wait ended without a click
func clarifyExpiredNotice(ctx context.Context) string {
	return i18n.T(ctx, "gateway.clarify.expired", "", nil)
}

// Whether a finite viewer that prints only the terminal reply hosts this turn
func finiteViewerTurn(ctx context.Context) bool {
	return gateway.FiniteTurnRequired(ctx) == true
}

// Panic value a stream delta callback raises when no sink consumed the text so the agent counts it undelivered
type noStreamConsumer struct{}

func (noStreamConsumer) Error() string { return "" }

// Error raised when the connector refused the approval card destination so the waiting tool unblocks
type execApprovalDeclinedError struct{ msg string }

func (e *execApprovalDeclinedError) Error() string { return e.msg }

// Exception class the error reports
func (e *execApprovalDeclinedError) PyTypeName() string { return "_ExecApprovalDeclined" }

// Session authority, owning session id and generation of a turn that has an owning session
type turnApprovalOwner struct {
	authority  *gateway.SessionAuthority
	sessionID  string
	generation int64
}

// Per turn collaborator carrying the progress, status, approval and clarify callbacks of one agent run
type TurnRunner struct {
	runner *GatewayRunner
	ctx    *TurnContext
	// Context every callback of the turn runs under, the turn's contextvars of hermes
	turnCtx       context.Context
	approvalOwner *turnApprovalOwner
	segmentsMu    sync.Mutex
	segmentsGen   int64
	segments      *tuigateway.TurnSegments
	// Result fields a classic CLI turn adds to the receipt, popped once run_sync settles
	cliResultFields map[string]any
	// Start times and before state snapshots of local viewer tool calls by call id
	localToolMu        sync.Mutex
	localToolStarts    map[string]float64
	localEditSnapshots map[string]*tools.LocalEditSnapshot
	// Terminal scope of the session policy while the turn runs inside it
	policyScope atomic.Pointer[tools.TerminalScope]
}

// Turn runner bound to the turn context, owned by the session authority when one holds the session
func newTurnRunner(ctx context.Context, runner *GatewayRunner, tc *TurnContext) (*TurnRunner, error) {
	tr := &TurnRunner{runner: runner, ctx: tc, turnCtx: ctx}
	authority, err := gateway.ActiveAuthority(ctx, runner)
	if err != nil || authority == nil {
		return tr, err
	}
	ownerID := ""
	if tc.source != nil && tc.source.Platform == gateway.PlatformLocal {
		ownerID = tc.source.ChatID
	} else if ownerID, err = authority.LogicalOwner(ctx, tc.sessionID); err != nil {
		return nil, err
	}
	if _, held := authority.Live(ownerID); !held {
		return tr, nil
	}
	row, err := authority.DB.GetSession(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &pyTypeError{"'NoneType' object is not subscriptable"}
	}
	generation := gateway.RowInt(row, "runtime_generation")
	if generation == nil {
		return nil, &pyTypeError{"KeyError: 'runtime_generation'"}
	}
	tr.approvalOwner = &turnApprovalOwner{authority: authority, sessionID: ownerID, generation: *generation}
	return tr, nil
}

// Publishes an execution event of the owning session stamped with the turn segment, false without an owner
func (t *TurnRunner) publishExecution(eventType string, payload map[string]any) bool {
	owner := t.approvalOwner
	if owner == nil {
		return false
	}
	published, err := owner.authority.PublishExecution(owner.sessionID, owner.generation, eventType, t.stampSegment(owner.generation, eventType, payload))
	if err != nil {
		runLog().Debug(fmt.Sprintf("execution publish %s failed: %s", eventType, err))
		return false
	}
	return published
}

// Reply frame stamped with the turn wire identity, one segment cursor per execution generation
func (t *TurnRunner) stampSegment(generation int64, eventType string, payload map[string]any) map[string]any {
	t.segmentsMu.Lock()
	defer t.segmentsMu.Unlock()
	if t.segments == nil || t.segmentsGen != generation {
		t.segmentsGen, t.segments = generation, tuigateway.NewTurnSegments()
	}
	return tuigateway.StampReplyFrame(t.segments, eventType, payload)
}

// Hands the real tool payload to this admission's API observers only
func (t *TurnRunner) publishAPITool(eventType string, callID any, toolName any, args, result any) {
	owner := t.approvalOwner
	if owner == nil {
		return
	}
	call, name := "", "tool"
	if pyrepr.Truthy(callID) {
		call = pyrepr.Str(callID)
	}
	if pyrepr.Truthy(toolName) {
		name = pyrepr.Str(toolName)
	}
	if err := gateway.PublishAPIToolEvent(t.turnCtx, owner.authority, owner.sessionID, owner.generation, eventType, call, name, args, result); err != nil {
		runLog().Debug(fmt.Sprintf("API tool event %s failed: %s", eventType, err))
	}
}

// Served runtime source of an API adapter for a finished turn under a confirmed model lock
type turnRuntimeMetadataSource interface {
	TurnRuntimeMetadata(ag *agent.AIAgent, settings any, confirmedRuntimeLock bool) (*pyjson.Object, error)
}

// Settles a confirmed API model lock on the runtime it named, failing the turn on a mismatch
func settleAPIRuntimeLock(ctx context.Context, a *agent.AIAgent, result map[string]any, captured map[string]any) error {
	api, err := gateway.APIRuntimeLocked(ctx)
	if err != nil || api == nil || result == nil {
		return err
	}
	settings, _ := api.Get("settings")
	adapterValue, _ := api.Get("adapter")
	source, ok := adapterValue.(turnRuntimeMetadataSource)
	if !ok {
		return &pyAttributeError{fmt.Sprintf("'%T' object has no attribute '_turn_runtime_metadata'", adapterValue)}
	}
	usage, isMap := captured["usage"].(map[string]any)
	if !isMap {
		return &pyTypeError{"KeyError: 'usage'"}
	}
	runtime, err := source.TurnRuntimeMetadata(a, settings, true)
	if err != nil {
		result["final_response"], result["failed"], result["completed"], result["error"] = "", true, false, err.Error()
		return nil
	}
	usage["runtime"] = runtime
	return nil
}

// Chat text, failure reason and retryable flag for a model or credential resolution failure before the turn
func resolutionFailureReply(ctx context.Context, err error, platform gateway.Platform) (string, string, bool) {
	authPermanent := string(provider.ReasonAuthPermanent)
	var local *provider.LocalRuntimeUnavailableError
	if errors.As(err, &local) && local == err {
		return "⚠️ " + local.Error(), local.ErrorCode(), local.Retryable
	}
	var store *state.RuntimeStoreError
	if errors.As(err, &store) && store == err {
		if store.Reason == "launch_credentials_unavailable" {
			return agent.LaunchCredentialsCopy(ctx, store), "launch_credentials_unavailable", false
		}
		return fmt.Sprintf("⚠️ This session's launch credentials are no longer available (%s), so this "+
			"message wasn't processed. Start a new session from the CLI to bind them again.", store.Reason), authPermanent, false
	}
	if cause := errors.Unwrap(err); cause != nil && provider.IsRateLimitedAuthError(cause) {
		return gatewayProviderErrorReply(ctx, err.Error()), authPermanent, false
	}
	if platform == gateway.PlatformLocal {
		return "⚠️ " + err.Error(), authPermanent, false
	}
	var authErr *provider.AuthError
	if pypath.IsOSError(err) && !errors.As(err, &authErr) {
		return i18n.T(ctx, "gateway.errors.generic_failed", "", nil), "host_error", false
	}
	return i18n.T(ctx, "gateway.errors.no_credentials", "", nil), authPermanent, false
}
