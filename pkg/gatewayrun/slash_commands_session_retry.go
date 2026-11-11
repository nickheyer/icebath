package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Re-send event of a retry carrying the retry event's source, raw message, channel prompt and callback
func retryResendEvent(ctx context.Context, event *gateway.MessageEvent, text string) *gateway.MessageEvent {
	turn := gateway.NewMessageEvent(text)
	turn.Source = event.Source
	turn.RawMessage = event.RawMessage
	turn.ChannelPrompt = event.ChannelPrompt
	gateway.CarryCallback(ctx, event, turn)
	return turn
}

// Re-sends the last user message after rewinding the transcript to just before it
func (r *GatewayRunner) handleRetryCommand(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	source := event.Source
	store := r.slashStore()
	entry, err := store.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return nil, err
	}
	owned, err := r.retryOwnedStore(ctx, entry.SessionID)
	if err != nil {
		return nil, err
	}
	var pinned []int64
	raw, readErr := func() ([]any, error) {
		if owned != nil {
			ids, err := state.AsyncCall(ctx, state.NewBorrowedAsyncSessionDB(owned), func(d *state.SessionDB) ([]int64, error) {
				return d.GetActiveMessageIDs(ctx, entry.SessionID)
			})
			if err != nil {
				return nil, err
			}
			pinned = ids
		}
		return store.LoadTranscript(ctx, entry.SessionID)
	}()
	if readErr != nil {
		var transcriptErr *gateway.TranscriptReadError
		if !errors.As(readErr, &transcriptErr) {
			runLog().Debug(fmt.Sprintf("/retry could not read the active rows of %s", entry.SessionID), "exc_info", readErr)
		}
		return historyUnreadable(ctx), nil
	}
	history := transcriptMaps(raw)
	lastUserIdx := -1
	for i := len(history) - 1; i >= 0; i-- {
		if agent.UserOriginatedTurnView(history[i]) != nil {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return i18n.T(ctx, "gateway.retry.no_previous", "", nil), nil
	}
	unsafe := func(err error) string {
		return i18n.T(ctx, "gateway.retry.unsafe", "", map[string]any{"error": pyrepr.ExceptionMessage(err)})
	}
	truncated, liveView, err := agent.HistoryBeforeUserOriginatedTurn(history, lastUserIdx)
	var lastUserMsg string
	if err == nil {
		lastUserMsg, err = agent.RetryableUserText(liveView["content"])
	}
	if err != nil {
		if !isValueError(err) {
			return nil, err
		}
		return unsafe(err), nil
	}
	handoff, _ := state.SplitUserOriginatedTurn(history[lastUserIdx])
	if reply, refused, err := r.retryReadmissionReply(ctx, source, lastUserMsg); err != nil || refused {
		if err != nil {
			return nil, err
		}
		return reply, nil
	}
	if owned != nil {
		return r.retryInAdmission(ctx, event, entry, owned, pinned, truncated, lastUserMsg, handoff != nil)
	}
	if handoff != nil {
		rewindResult, err := store.RewindSession(ctx, entry.SessionID, 1, true)
		if err != nil {
			if !isValueError(err) {
				return nil, err
			}
			return unsafe(err), nil
		}
		if rewindResult == nil {
			return i18n.T(ctx, "gateway.retry.failed_unchanged", "", nil), nil
		}
		lastUserMsg, _ = rewindResult["target_text"].(string)
	} else {
		if !store.RewriteTranscript(ctx, entry.SessionID, truncated, true, true) {
			return i18n.T(ctx, "gateway.retry.failed_unchanged", "", nil), nil
		}
	}
	entry.LastPromptTokens = 0
	r.recordModelFriction(ctx, "retry", source, entry.SessionID, 1)
	return r.handleMessage(ctx, retryResendEvent(ctx, event, lastUserMsg))
}

// Binds the planned rewrite to the re-send so admission commits both in one write, a refusal leaving the transcript
func (r *GatewayRunner) retryInAdmission(ctx context.Context, event *gateway.MessageEvent, entry *gateway.SessionEntry, db *state.SessionDB,
	pinned []int64, truncated []map[string]any, resendText string, composite bool) (any, error) {
	sessionID := entry.SessionID
	failedUnchanged := func() string { return i18n.T(ctx, "gateway.retry.failed_unchanged", "", nil) }
	var write func(ctx context.Context, conn state.SQLiteConn) error
	if composite {
		plan, err := state.AsyncCall(ctx, state.NewBorrowedAsyncSessionDB(db), func(d *state.SessionDB) (*state.RewindPlan, error) {
			return d.PlanUserTurnRewind(ctx, sessionID, -1, state.RewindUserTurnOptions{RequireRetryable: true, RequireComposite: true})
		})
		if err != nil {
			var unavailable *state.RewindTargetUnavailableError
			switch {
			case errors.As(err, &unavailable):
				return failedUnchanged(), nil
			case isValueError(err):
				return i18n.T(ctx, "gateway.retry.unsafe", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
			case pyrepr.ExceptionName(err) == "RuntimeError":
				runLog().Debug(fmt.Sprintf("/retry carrier plan refused for %s", sessionID), "exc_info", err)
				return failedUnchanged(), nil
			}
			return nil, err
		}
		if !slices.Equal(plan.ExpectedActiveIDs, pinned) {
			return failedUnchanged(), nil
		}
		resendText = ""
		if plan.LiveText != nil {
			resendText = *plan.LiveText
		}
		write = func(ctx context.Context, conn state.SQLiteConn) error {
			_, _, _, _, err := db.RewindInTransaction(ctx, conn, sessionID, plan.TargetRowID, state.RewindOptions{PreserveCompactionHandoff: true,
				ExpectedActiveIDs: plan.ExpectedActiveIDs, ExpectedTargetContent: plan.ExpectedTargetContent})
			return err
		}
	} else {
		rows := make([]map[string]any, len(truncated))
		for i, m := range truncated {
			rows[i] = maps.Clone(m)
		}
		write = func(ctx context.Context, conn state.SQLiteConn) error {
			return db.ReplaceMessagesInTransaction(ctx, conn, sessionID, rows, state.ReplaceOptions{ActiveOnly: true, RejectActiveTurnLease: true}, pinned)
		}
	}
	committed := func() {
		r.sessionStore.ClearDirtyTranscript(sessionID)
		entry.LastPromptTokens = 0
		r.recordModelFriction(ctx, "retry", event.Source, sessionID, 1)
	}
	rewrite := &gateway.RetryRewrite{DB: db, SessionID: sessionID, Source: event.Source, Write: write, OnCommit: committed}
	turn := retryResendEvent(ctx, event, resendText)
	reply, err := r.handleMessage(gateway.BindRetryRewrite(ctx, rewrite), turn)
	if err == nil {
		return reply, nil
	}
	var refused *gateway.RetryRewriteRefused
	if errors.As(err, &refused) {
		runLog().Info(fmt.Sprintf("/retry refused with the transcript unchanged for %s: %s", sessionID, refused.Error()))
		return failedUnchanged(), nil
	}
	var storeErr *state.RuntimeStoreError
	if errors.As(err, &storeErr) && !rewrite.Applied && storeErr.Reason == "runtime_draining" {
		return i18n.T(ctx, "gateway.busy.drain_rejected", "", map[string]any{"action": r.statusActionGerund(ctx)}), nil
	}
	return nil, err
}
