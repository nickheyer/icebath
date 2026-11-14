package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/model"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/tool"
)

type TurnInput struct {
	Message    wire.Message
	System     string
	History    []*Message
	TaskID     string
	Persist    *PersistOverride
	MoA        *MixtureTurn
	Author     *Author
	Relay      json.RawMessage
	Title      *string
	Prelude    Prelude
	Stream     func(text string)
	Voice      bool
	Diagnostic bool
	Muted      bool
}

type PersistOverride struct {
	Content         *wire.Message
	Timestamp       *float64
	DisplayKind     string
	DisplayMetadata json.RawMessage
	PlatformID      string
}

type Author struct {
	ID    string
	Name  string
	IsBot bool
}

const (
	EnvTurnAuthor = "ICEBATH_TURN_AUTHOR"

	authorFieldLimit = 200
)

func cleanAuthorValue(v wire.Value) (string, bool) {
	raw, ok := v.Str()
	if !ok {
		return "", false
	}
	return cleanAuthorText(raw)
}

func cleanAuthorText(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range raw {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cs, r) || unicode.Is(unicode.Co, r) || !assignedRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	text := trimText(b.String())
	if text == "" {
		return "", false
	}
	runes := []rune(text)
	if len(runes) > authorFieldLimit {
		text = string(runes[:authorFieldLimit])
	}
	return text, true
}

func assignedRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) || unicode.IsPunct(r) ||
		unicode.IsSymbol(r) || unicode.Is(unicode.Z, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) ||
		unicode.Is(unicode.Cs, r) || unicode.Is(unicode.Co, r)
}

func authorBotFlag(v wire.Value) bool {
	if s, ok := v.Str(); ok {
		switch strings.ToLower(trimText(s)) {
		case "true", "1", "yes":
			return true
		}
		return false
	}
	if b, ok := v.Bool(); ok {
		return b
	}
	if n, ok := v.Int(); ok {
		return n != 0
	}
	return false
}

func BotAuthorID(profile, origin string) string {
	origin = trimText(origin)
	if origin != "" {
		return "bot:" + origin + "/" + profile
	}
	return "bot:" + profile
}

func AuthorOrigin(hostname string) string {
	text, _ := cleanAuthorText(hostname)
	return strings.ReplaceAll(text, "/", "")
}

func ParseAuthor(raw json.RawMessage, origin string) (*Author, bool) {
	v, err := wire.ParseValue(raw)
	if err != nil {
		return nil, false
	}
	if text, ok := v.Str(); ok {
		if v, err = wire.ParseValue([]byte(text)); err != nil {
			return nil, false
		}
	}
	if !v.IsObject() {
		return nil, false
	}
	id, hasID := cleanAuthorValue(v.Get("id"))
	name, hasName := cleanAuthorValue(v.Get("name"))
	if !hasID && !hasName {
		return nil, false
	}
	a := &Author{ID: id, Name: name, IsBot: authorBotFlag(v.Get("is_bot"))}
	qualifier, hasOrigin := cleanAuthorValue(v.Get("origin"))
	if !hasOrigin {
		qualifier, hasOrigin = cleanAuthorText(origin)
	}
	if hasOrigin && strings.HasPrefix(a.ID, "bot:") && !strings.Contains(a.ID, "/") {
		a.ID, _ = cleanAuthorText(BotAuthorID(strings.TrimPrefix(a.ID, "bot:"), qualifier))
	}
	return a, true
}

func (a *Author) Env() (string, string) {
	d := &wire.Dict{}
	if a.ID != "" {
		d.SetString("id", a.ID)
	} else {
		d.Set("id", json.RawMessage("null"))
	}
	if a.Name != "" {
		d.SetString("name", a.Name)
	} else {
		d.Set("name", json.RawMessage("null"))
	}
	d.SetBool("is_bot", a.IsBot)
	text, _ := wire.FormatJSON(d.JSON(), wire.TextLayout{})
	return EnvTurnAuthor, text
}

func (a *Author) A2AKey() (string, bool) {
	if a == nil || !a.IsBot || a.ID == "" {
		return "", false
	}
	return "a2a:" + a.ID, true
}

type Prelude interface {
	Next(ctx context.Context, lastResult *string) (PreludeStep, bool)
	Close()
}

type PreludeStep struct {
	Content string
	Name    string
	Args    tool.Args
}

type SessionUsage struct {
	Input, Output, CacheRead, CacheWrite, Reasoning, Prompt, Completion, Total int64
}

type FreeTier struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type BillingBlock struct {
	Provider      string `json:"provider"`
	ProviderLabel string `json:"provider_label"`
	Model         string `json:"model"`
	BillingURL    string `json:"billing_url"`
	IsPortal      bool   `json:"is_portal"`
	Message       string `json:"message"`
	Unverified    bool   `json:"unverified,omitempty"`
}

type PersistedTurn struct {
	RowIDs              []int64 `json:"row_ids"`
	Complete            bool    `json:"complete"`
	UserRowID           int64   `json:"user_row_id,omitempty"`
	UserRowIDs          []int64 `json:"user_row_ids,omitempty"`
	FinalAssistantRowID int64   `json:"final_assistant_row_id,omitempty"`
}

type TurnResult struct {
	Final                string
	FinalSet             bool
	LastReasoning        string
	Messages             []*Message
	APICalls             int
	Completed            bool
	Exit                 ExitReason
	Failed               bool
	Partial              bool
	Interrupted          bool
	HardInterrupted      bool
	InterruptMessage     string
	UserIntervened       bool
	PendingSteer         string
	Transformed          bool
	PreTransform         *string
	Previewed            bool
	Reused               bool
	Model                string
	RequestedModel       string
	ServedModel          string
	Provider             string
	BaseURL              string
	Usage                SessionUsage
	LastPromptTokens     int
	EstimatedCostUSD     float64
	CostStatus           string
	CostSource           string
	ServiceTier          string
	SessionID            string
	TurnID               string
	UserIndex            int
	Guardrail            json.RawMessage
	Error                string
	FailureReason        string
	FailureRetryable     bool
	FailureResetsAt      *float64
	FreeTier             *FreeTier
	Billing              *BillingBlock
	BillingUnverified    bool
	UsageWindowMessage   string
	UsageLimit           json.RawMessage
	CompressionDeferred  bool
	CompressionExhausted bool
	CleanupErrors        []string
	Surface              *model.Surface
	Persisted            *PersistedTurn
}

type ExitReason string

const (
	ExitUnknown                  ExitReason = "unknown"
	ExitBudgetExhausted          ExitReason = "budget_exhausted"
	ExitInterruptedByUser        ExitReason = "interrupted_by_user"
	ExitInterruptedDuringAPICall ExitReason = "interrupted_during_api_call"
	ExitGuardrailHalt            ExitReason = "guardrail_halt"
	ExitBlockedByPlugin          ExitReason = "blocked_by_plugin_pre_llm_call"
	ExitPersistenceFailed        ExitReason = "session_persistence_failed"
	ExitPendingToolResult        ExitReason = "pending_tool_result"
	ExitPartialStreamRecovery    ExitReason = "partial_stream_recovery"
	ExitFallbackPriorTurn        ExitReason = "fallback_prior_turn_content"
	ExitEmptyResponseExhausted   ExitReason = "empty_response_exhausted"
	ExitAllRetriesExhausted      ExitReason = "all_retries_exhausted_no_response"
	ExitCompressionExhausted     ExitReason = "context_compression_exhausted"
	ExitCompressionTimeout       ExitReason = "context_compression_timeout"
	ExitHandoffNotActionable     ExitReason = "compaction_handoff_not_actionable"
	ExitReviewInputBudget        ExitReason = "review_input_budget_exhausted"
	ExitRedirectRestartLimit     ExitReason = "redirect_restart_limit_exceeded"
	ExitRebuiltRestartLimit      ExitReason = "rebuilt_restart_limit_exceeded"
	ExitOllamaContextTooSmall    ExitReason = "ollama_runtime_context_too_small"
	ExitShutdown                 ExitReason = "interpreter_shutdown"

	exitTextResponsePrefix   = "text_response("
	exitMaxIterationsPrefix  = "max_iterations_reached("
	exitRepeatedOuterErrors  = "repeated_outer_errors"
	exitLocalProcessingError = "local_processing_error"
	exitErrorNearMaxIters    = "error_near_max_iterations"
	exitErrorDetailLimit     = 80
)

func ExitTextResponse(finish string) ExitReason {
	return ExitReason(exitTextResponsePrefix + "finish_reason=" + finish + ")")
}

func ExitMaxIterations(used, limit int) ExitReason {
	return ExitReason(exitMaxIterationsPrefix + strconv.Itoa(used) + "/" + strconv.Itoa(limit) + ")")
}

func ExitInterruptedBySystem(issuer string) ExitReason {
	return ExitReason("interrupted_by_system(" + issuer + ")")
}

func exitWithDetail(prefix, detail string) ExitReason {
	runes := []rune(detail)
	if len(runes) > exitErrorDetailLimit {
		detail = string(runes[:exitErrorDetailLimit])
	}
	return ExitReason(prefix + "(" + detail + ")")
}

func ExitRepeatedOuterErrors(detail string) ExitReason {
	return exitWithDetail(exitRepeatedOuterErrors, detail)
}

func ExitLocalProcessingError(detail string) ExitReason {
	return exitWithDetail(exitLocalProcessingError, detail)
}

func ExitErrorNearMaxIterations(detail string) ExitReason {
	return exitWithDetail(exitErrorNearMaxIters, detail)
}

func (r ExitReason) TextResponse() bool {
	return strings.HasPrefix(string(r), exitTextResponsePrefix)
}

func (r ExitReason) BudgetExit() bool {
	return strings.HasPrefix(string(r), string(ExitBudgetExhausted)) || strings.HasPrefix(string(r), exitMaxIterationsPrefix)
}

const (
	FailureContextOverflow    = "context_overflow"
	FailureTruncated          = "truncated"
	FailureInvalidResponse    = "invalid_response"
	FailureEmptyResponse      = "empty_response"
	FailureLoopError          = "loop_error"
	FailureShutdown           = "interpreter_shutdown"
	FailureSessionBusy        = "session_busy"
	FailureGatewayConnection  = "gateway_connection"
	FailureHostError          = "host_error"
	failurePersistencePrefix  = "session_persistence_failed:"
	failureUnknownPersistence = "unknown"
)

func SiteFailureCode(code string) bool {
	switch code {
	case FailureContextOverflow, FailureTruncated, FailureInvalidResponse, FailureEmptyResponse, FailureLoopError,
		FailureShutdown, FailureSessionBusy, FailureGatewayConnection, FailureHostError:
		return true
	}
	return false
}

func PersistenceFailureReason(cause string) string {
	if cause == "" {
		cause = failureUnknownPersistence
	}
	return failurePersistencePrefix + cause
}

type ExitFailure struct {
	Reason    string
	Retryable bool
	FailsTurn bool
}

var exitReasonFailures = []struct {
	prefix string
	ExitFailure
}{
	{string(ExitEmptyResponseExhausted), ExitFailure{FailureEmptyResponse, true, false}},
	{string(ExitAllRetriesExhausted), ExitFailure{model.ClassServerError.String(), true, true}},
	{string(ExitPendingToolResult), ExitFailure{FailureLoopError, true, true}},
	{string(ExitShutdown), ExitFailure{FailureShutdown, false, true}},
	{exitLocalProcessingError, ExitFailure{FailureLoopError, false, false}},
	{exitRepeatedOuterErrors, ExitFailure{FailureLoopError, true, true}},
	{exitErrorNearMaxIters, ExitFailure{FailureLoopError, true, true}},
	{string(ExitCompressionTimeout), ExitFailure{FailureContextOverflow, false, true}},
	{string(ExitCompressionExhausted), ExitFailure{FailureContextOverflow, false, true}},
	{string(ExitOllamaContextTooSmall), ExitFailure{FailureContextOverflow, false, true}},
	{string(ExitRedirectRestartLimit), ExitFailure{FailureLoopError, true, false}},
	{string(ExitRebuiltRestartLimit), ExitFailure{FailureLoopError, true, false}},
}

func (r ExitReason) Failure() (ExitFailure, bool) {
	for _, row := range exitReasonFailures {
		if strings.HasPrefix(string(r), row.prefix) {
			return row.ExitFailure, true
		}
	}
	return ExitFailure{}, false
}

func invalidResponseFailureReason(code string, present bool) string {
	if !present {
		return FailureInvalidResponse
	}
	n, err := strconv.Atoi(strings.TrimSpace(code))
	if err != nil {
		return FailureInvalidResponse
	}
	switch n {
	case 429:
		return model.ClassRateLimit.String()
	case 500, 502:
		return model.ClassServerError.String()
	case 503, 529:
		return model.ClassOverloaded.String()
	case 504, 524:
		return model.ClassTimeout.String()
	}
	return FailureInvalidResponse
}

func (r *TurnResult) stampFailure(reason string, retryable bool) *TurnResult {
	r.FailureReason = reason
	r.FailureRetryable = retryable
	return r
}

func (r *TurnResult) stampFreeTier(kind, message string) *TurnResult {
	if kind == "" {
		kind = "refused"
	}
	r.FreeTier = &FreeTier{Kind: kind, Message: message}
	return r
}

func (r *TurnResult) MaxIterationHandoff() bool {
	if r == nil || r.Failed || r.Interrupted || r.Completed {
		return false
	}
	if !strings.HasPrefix(string(r.Exit), exitMaxIterationsPrefix) {
		return false
	}
	return trimText(r.Final) != ""
}

const (
	compressionPausedFinal = "Context compression is temporarily paused after a recent failed attempt. Please retry " +
		"in a moment — compression will resume automatically (or run /compress to force a retry now)."
	compressionRunningFinal = "Context compression is already running for this session. Please retry in a moment — " +
		"your next message will be processed once the concurrent compression finishes."
	compressionTimeoutFinal = "Context compression timed out without reducing this conversation. No messages were " +
		"dropped. Start a fresh session with /new, or check auxiliary.compression before retrying /compress."
	interruptWaitingForModelPrefix = "Operation interrupted: waiting for model response ("
	persistenceFailedError         = "session storage could not be written — check the state database " +
		"health (`icebath %sdoctor`), then send your message again"
)

func baseResult(final string, msgs []*Message, apiCalls int) *TurnResult {
	return &TurnResult{Final: final, FinalSet: true, Messages: msgs, APICalls: apiCalls, UserIndex: -1}
}

func partialResult(final string, msgs []*Message, apiCalls int) *TurnResult {
	r := baseResult(final, msgs, apiCalls)
	r.Error, r.Partial = final, true
	return r
}

func failedResult(final string, msgs []*Message, apiCalls int, errText string) *TurnResult {
	r := baseResult(final, msgs, apiCalls)
	r.Failed, r.Error = true, errText
	return r
}

func contentPolicyBlockedResult(final string, msgs []*Message, apiCalls int, detail string) *TurnResult {
	word := model.ClassContentPolicyBlocked.String()
	return failedResult(final, msgs, apiCalls, word+": "+detail).stampFailure(word, false)
}

func truncatedPartialResult(final string, msgs []*Message, apiCalls int) *TurnResult {
	return partialResult(final, msgs, apiCalls).stampFailure(FailureTruncated, true)
}

func (a *Agent) compressionDeferredResult(msgs []*Message, apiCalls int, transient bool, guard string) *TurnResult {
	sessionID := a.SessionID()
	logged := sessionID
	if logged == "" {
		logged = "none"
	}
	final := compressionRunningFinal
	if transient {
		if guard == "" {
			guard = "unknown guard"
		}
		slog.Info(fmt.Sprintf("turn deferred: compression transiently blocked (%s) (session=%s) — not counting as "+
			"compression exhaustion", guard, logged))
		final = compressionPausedFinal
	} else {
		if guard == "" {
			guard = "unconfirmed"
		}
		slog.Info(fmt.Sprintf("turn deferred: compression lock held by another path (session=%s holder=%s) — not "+
			"counting as compression exhaustion", logged, guard))
	}
	a.flushStatusBuffer()
	r := partialResult(final, msgs, apiCalls)
	r.CompressionDeferred, r.SessionID = true, sessionID
	return r
}

func contextExhaustedResult(final string, msgs []*Message, apiCalls int, exit ExitReason) *TurnResult {
	r := partialResult(final, msgs, apiCalls)
	r.Failed, r.CompressionExhausted, r.Exit = true, true, exit
	return r.stampFailure(FailureContextOverflow, false)
}

func interruptedResult(final string, msgs []*Message, apiCalls int, st interruptState) *TurnResult {
	r := baseResult(final, msgs, apiCalls)
	r.Interrupted, r.HardInterrupted, r.InterruptMessage = true, st.hard, st.message
	return r
}

func (r *TurnResult) stampPersistenceFailure(selector, cause string) *TurnResult {
	if r.Error = r.Final; r.Error == "" {
		r.Error = fmt.Sprintf(persistenceFailedError, selector)
	}
	r.FailureReason = PersistenceFailureReason(cause)
	return r
}

func (r *TurnResult) stampExit(exit ExitReason) *TurnResult {
	r.Exit = exit
	if exit == ExitPersistenceFailed {
		return r
	}
	f, ok := exit.Failure()
	if !ok {
		return r
	}
	if r.Failed {
		if r.Error = r.Final; r.Error == "" {
			r.Error = string(exit)
		}
	}
	return r.stampFailure(f.Reason, f.Retryable)
}

type BusyError struct {
	SessionID string
}

func (e *BusyError) Error() string {
	return "agent: a turn is already running on session " + e.SessionID
}

var ErrShutdown = errors.New("agent: the process is shutting down")

func persistedTurnReceipt(msgs []*Message, start int, history []*Message, raw, status string,
	compressionUnchanged bool, prefixRowMatches func(before, after *Message) bool) *PersistedTurn {
	if start < 0 || start >= len(msgs) || msgs[start] == nil || msgs[start].Role != wire.RoleUser {
		return nil
	}
	tail := msgs[start:]
	anchorID, anchored := tail[0].CommittedRowID()
	for _, before := range history {
		if before == tail[0] {
			return nil
		}
		if id, ok := before.CommittedRowID(); anchored && ok && id == anchorID {
			return nil
		}
	}
	var rowIDs []int64
	for _, m := range tail {
		if id, ok := m.CommittedRowID(); ok {
			rowIDs = append(rowIDs, id)
		}
	}
	if len(rowIDs) == 0 {
		return nil
	}
	receipt := &PersistedTurn{RowIDs: rowIDs}
	if anchored {
		receipt.UserRowID = anchorID
	}
	var userRowIDs []int64
	users, usersCommitted, userRows := 0, true, 0
	for _, m := range tail {
		if m == nil || m.Role != wire.RoleUser {
			continue
		}
		userRows++
		if m.Row.DisplayKind.Valid && m.Row.DisplayKind.V == hiddenDisplayKind {
			continue
		}
		users++
		id, ok := m.CommittedRowID()
		if !ok {
			usersCommitted = false
			continue
		}
		userRowIDs = append(userRowIDs, id)
	}
	if users > 0 && usersCommitted {
		receipt.UserRowIDs = userRowIDs
	}
	last := tail[len(tail)-1]
	if status == "complete" && last != nil && last.Role == wire.RoleAssistant && !last.hasToolCalls() &&
		last.Parts == nil && last.Text == raw {
		if id, ok := last.CommittedRowID(); ok {
			receipt.FinalAssistantRowID = id
		}
	}
	prefixUnchanged := start == len(history)
	if prefixUnchanged {
		for i, before := range history {
			if !prefixRowMatches(before, msgs[i]) {
				prefixUnchanged = false
				break
			}
		}
	}
	receipt.Complete = prefixUnchanged && compressionUnchanged && len(rowIDs) == len(tail) && userRows == 1 &&
		receipt.FinalAssistantRowID != 0
	return receipt
}
