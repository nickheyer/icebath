package gatewayrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Adapter whose persistent clarify card is retired once no answer can arrive
type clarifyCardRetirer interface {
	RetireClarifyCard(ctx context.Context, clarifyID, notice string) error
}

// Field of a clarify question entry
func clarifyEntryField(entry any, key string) any {
	switch typed := entry.(type) {
	case map[string]any:
		return typed[key]
	case *pyjson.Object:
		return typed.GetOr(key, nil)
	}
	return nil
}

// Sends the clarify card and registers it with the session controls once it posted
func (t *TurnRunner) sendSharedClarify(ctx context.Context, entry *tools.ClarifyEntry, args clarifySendArgs) (any, error) {
	result, err := t.ctx.statusAdapter.SendClarify(ctx, args.chatID, args.question, args.choices, args.clarifyID, args.sessionKey, args.metadata)
	if err != nil {
		return result, err
	}
	if owner := t.approvalOwner; owner != nil && result.Success {
		if err := owner.authority.RegisterClarify(ctx, owner.sessionID, owner.generation, entry, promptTimeoutS(t, "clarify")); err != nil {
			return result, err
		}
	}
	return result, nil
}

// Answers the clarify tool's questions one card at a time, stopping at the first the user never answers
func (t *TurnRunner) clarifyCallbackSync(ctx context.Context, questions []any) (map[string]any, error) {
	if finiteViewerTurn(ctx) {
		reply, _ := pyjson.Plain(gateway.FiniteClarifyReply()).(map[string]any)
		return reply, nil
	}
	if clarifyBatchTurn(ctx, t) {
		return askClarifyBatch(ctx, t, questions), nil
	}
	answers := map[string]any{}
	reply := map[string]any{"answers": answers, "outcome": "submitted"}
	last := len(questions) - 1
	hint := ""
	if t.ctx.source == nil || t.ctx.source.Platform != gateway.PlatformLocal {
		hint = "\n" + i18n.T(ctx, "gateway.clarify.skip_hint", "", nil)
	}
	for index, entry := range questions {
		question := pyrepr.Str(clarifyEntryField(entry, "question")) + hint
		var choices []string
		if raw, ok := clarifyEntryField(entry, "choices").([]any); ok {
			for _, choice := range raw {
				choices = append(choices, pyrepr.Str(choice))
			}
		}
		raw, answered := t.askClarifyQuestion(ctx, question, choices, pyrepr.Truthy(clarifyEntryField(entry, "multi_select")), index == last)
		if raw == tools.ClarifyCancelled {
			reply["outcome"] = "cancelled"
			break
		}
		if !answered {
			outcome := "timed_out"
			if raw == clarifyUndelivered || raw == clarifyUndeliveredDeclined || raw == clarifyUndeliveredNoSurface {
				outcome = "undelivered"
			}
			reply["outcome"], reply["notice"] = outcome, raw
			break
		}
		qid := pyrepr.Str(clarifyEntryField(entry, "qid"))
		if raw == tools.ClarifySkipped {
			answers[qid] = nil
		} else {
			answers[qid] = raw
		}
	}
	return reply, nil
}

func newClarifyID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])[:10]
}

// One card registered, sent, waited on, then retired without an answer or re-armed after one
func (t *TurnRunner) askClarifyQuestion(ctx context.Context, question string, choices []string, multiSelect, rearm bool) (string, bool) {
	tc := t.ctx
	if tc.statusAdapter == nil {
		return clarifyUndeliveredNoSurface, false
	}
	sessionKey := tc.sessionKey
	clarifyID := newClarifyID()
	if len(choices) == 0 {
		choices = nil
	}
	args := clarifySendArgs{chatID: tc.statusChatID, question: question, choices: choices, clarifyID: clarifyID,
		sessionKey: sessionKey, metadata: tc.statusThreadMetadata}
	textFallback := func() *concurrentFuture {
		coro := textFallbackCoro(tc.statusAdapter, args)
		if coro == nil {
			return nil
		}
		return t.schedule(coro, "Clarify text fallback failed to schedule")
	}
	entry := tools.RegisterGatewayClarify(clarifyID, sessionKey, question, choices, multiSelect)
	t.closeNativeStreamBoundary("Clarify", i18n.T(ctx, "gateway.clarify.native_stream_placeholder", "", nil), true)
	if base := tc.statusAdapter.Base(); base != nil {
		base.PauseTypingForChat(tc.statusChatID)
	}
	if sc := tc.streamConsumer(); sc != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("Stream-consumer flush before clarify prompt failed", "error", rec)
				}
			}()
			sc.FlushPendingSync(3.0)
		}()
	}
	fut := t.schedule(func(ctx context.Context) (any, error) { return t.sendSharedClarify(ctx, entry, args) }, "Clarify send failed to schedule")
	response, answered := clarifySendThenWait(ctx, t.toolsEnv(), fut, clarifyID, sessionKey, textFallback)
	if owner := t.approvalOwner; owner != nil {
		if live, ok := owner.authority.Live(owner.sessionID); ok && live != nil {
			live.SnapshotControls(owner.sessionID, owner.generation)
		}
	}
	if !answered {
		if retirer, ok := tc.statusAdapter.(clarifyCardRetirer); ok && adapterDeclares(tc.statusAdapter, "RetireClarifyCard") {
			notice := clarifyExpiredNotice(ctx)
			t.schedule(func(ctx context.Context) (any, error) {
				return nil, retirer.RetireClarifyCard(ctx, clarifyID, notice)
			}, "Clarify card retire failed to schedule")
		}
	} else if rearm {
		if sc := tc.streamConsumer(); sc != nil {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						runLog().Debug("request_reopen_seed after clarify answer failed", "error", rec)
					}
				}()
				sc.RequestReopenSeed()
			}()
		}
		if base := tc.statusAdapter.Base(); base != nil {
			base.ResumeTypingForChat(tc.statusChatID)
		}
	}
	return response, answered
}

// Wire keys of one clarify question in the batch request
var clarifyBatchQuestionKeys = []string{"qid", "question", "choices", "multi_select"}

// The whole clarify batch as one request to a classic CLI viewer's batch panel
func askClarifyBatch(ctx context.Context, t *TurnRunner, questions []any) map[string]any {
	owner := t.approvalOwner
	live, ok := owner.authority.Live(owner.sessionID)
	turnAgent := t.ctx.currentAgent()
	interrupted := func() bool { return turnAgent != nil && turnAgent.InterruptRequested() }
	if !ok || live == nil {
		return map[string]any{"answers": map[string]any{}, "outcome": "cancelled"}
	}
	wire := make([]any, 0, len(questions))
	qids := make([]string, 0, len(questions))
	known := map[string]bool{}
	for _, entry := range questions {
		item := pyjson.NewObject()
		for _, key := range clarifyBatchQuestionKeys {
			if value := clarifyEntryField(entry, key); value != nil {
				item.Set(key, value)
			}
		}
		wire = append(wire, item)
		qid := pyrepr.Str(clarifyEntryField(entry, "qid"))
		qids = append(qids, qid)
		known[qid] = true
	}
	timeout := float64(tools.GetClarifyTimeout(t.toolsEnv()))
	var timeoutArg *float64
	if timeout > 0 {
		timeoutArg = &timeout
	}
	result := gateway.AskViewerResult(live.EventStream, owner.sessionID, "clarify", pyjson.ObjectOf("questions", wire), interrupted, timeoutArg, qids)
	if result == nil {
		outcome := "timed_out"
		if interrupted() {
			outcome = "cancelled"
		}
		return map[string]any{"answers": map[string]any{}, "outcome": outcome}
	}
	answers, ok := result.GetOr("answers", nil).(*pyjson.Object)
	if !ok {
		return map[string]any{"answers": map[string]any{}, "outcome": "cancelled"}
	}
	clean := map[string]any{}
	for _, qid := range answers.Keys {
		if !known[qid] {
			continue
		}
		if s, isStr := answers.Vals[qid].(string); isStr {
			clean[qid] = s
		} else {
			clean[qid] = nil
		}
	}
	outcome := "cancelled"
	switch got := result.GetOr("outcome", nil).(type) {
	case string:
		if got == "submitted" || got == "timed_out" || got == "cancelled" {
			outcome = got
		}
	}
	return map[string]any{"answers": clean, "outcome": outcome}
}
