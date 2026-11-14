package yuanbao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Session store surface the inbound middlewares read and rewrite transcripts through
type sessionStore interface {
	GetOrCreateSession(ctx context.Context, source *gateway.SessionSource, forceNew, touchActivity bool) (*gateway.SessionEntry, error)
	LoadTranscript(ctx context.Context, sessionID string) ([]any, error)
	RewriteTranscript(ctx context.Context, sessionID string, messages []map[string]any, activeOnly, rejectActiveTurnLease bool) bool
	AppendToTranscript(ctx context.Context, sessionID string, message map[string]any, skipDB bool) error
}

// Session store of the adapter, nil before the runner installed one
func sessionStoreOf(adapter *YuanbaoAdapter) sessionStore {
	store, ok := adapter.SessionStore().(sessionStore)
	if !ok {
		return nil
	}
	return store
}

// Transcript rows as the dicts a rewrite takes, a row that is not a dict failing like a get on it
func transcriptRows(transcript []any) ([]map[string]any, error) {
	rows := make([]map[string]any, len(transcript))
	for i, row := range transcript {
		d, err := dictOf(row)
		if err != nil {
			return nil, err
		}
		rows[i] = d
	}
	return rows, nil
}

// RecallGuardMiddleware redacts a recalled message in the transcript, notes an unseen one and interrupts one in progress
type RecallGuardMiddleware struct{ middlewareBase }

func newRecallGuardMiddleware() InboundMiddleware {
	return &RecallGuardMiddleware{middlewareBase{"RecallGuardMiddleware", "recall_guard"}}
}

var recallCommands = map[string]bool{"Group.CallbackAfterRecallMsg": true, "C2C.CallbackAfterMsgWithDraw": true}

const recallRedacted = "[This message was recalled/withdrawn by the sender; original content removed]"

// Handles a recall callback as terminal, every other push continuing down the chain
func (m *RecallGuardMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	var cmd any = ""
	if ic.Push != nil && ic.Push.Len() > 0 {
		cmd = ic.Push.GetOr("callback_command", "")
	}
	if err := unhashable(cmd); err != nil {
		return err
	}
	if name, isStr := cmd.(string); isStr && recallCommands[name] {
		return m.handleRecall(ctx, ic, name)
	}
	return next()
}

// Source of the group or direct chat a recall names
func recallSource(ctx context.Context, adapter *YuanbaoAdapter, groupCode, fromAccount string) *gateway.SessionSource {
	fields := gateway.SourceFields{ChatType: "dm"}
	chatID := "direct:" + fromAccount
	if groupCode != "" {
		chatID, fields.ChatType, fields.ThreadID = "group:"+groupCode, "group", strPtr("main")
	}
	if fromAccount != "" {
		fields.UserID = &fromAccount
	}
	return adapter.BuildSource(ctx, chatID, fields)
}

// Session id of the chat a recall names
func resolveRecallSID(ctx context.Context, store sessionStore, adapter *YuanbaoAdapter, groupCode, fromAccount string) (string, error) {
	entry, err := store.GetOrCreateSession(ctx, recallSource(ctx, adapter, groupCode, fromAccount), false, true)
	if err != nil {
		return "", err
	}
	return entry.SessionID, nil
}

// Blanks the entry in place and persists the transcript, logging the outcome and never failing
func redactEntry(ctx context.Context, adapter *YuanbaoAdapter, store sessionStore, sid string, transcript []map[string]any,
	entry map[string]any, okMsg, failMsg string) {
	entry["content"] = recallRedacted
	if err := recovered(func() { store.RewriteTranscript(ctx, sid, transcript, true, false) }); err != nil {
		slog.Warn(fmt.Sprintf(failMsg, adapter.Name(), errText(err)))
		return
	}
	slog.Info(okMsg)
}

// Walks the recalled ids of a callback, interrupting one in progress and patching the transcript of the others
func (m *RecallGuardMiddleware) handleRecall(ctx context.Context, ic *InboundContext, cmd string) error {
	adapter := ic.Adapter
	push := pyjson.NewObject()
	if ic.Push != nil {
		push = ic.Push
	}
	var seqList any
	if cmd == "Group.CallbackAfterRecallMsg" {
		seqList = pyrepr.Or(push.GetOr("recall_msg_seq_list", nil), []any{})
	} else {
		mid, seq := pyrepr.Or(push.GetOr("msg_id", nil), ""), push.GetOr("msg_seq", nil)
		seqList = []any{}
		if pyrepr.Truthy(mid) || pyrepr.Truthy(seq) {
			seqList = []any{pyjson.ObjectOf("msg_id", mid, "msg_seq", seq)}
		}
	}
	if !pyrepr.Truthy(seqList) {
		slog.Debug(fmt.Sprintf("[%s] Recall callback with empty seq_list, skipping", adapter.Name()))
		return nil
	}
	groupCode, err := strMethod(pyrepr.Or(push.GetOr("group_code", nil), ""), "strip")
	if err != nil {
		return err
	}
	fromAccount, err := strMethod(pyrepr.Or(push.GetOr("from_account", nil), ""), "strip")
	if err != nil {
		return err
	}
	groupCode, fromAccount = pystr.Strip(groupCode), pystr.Strip(fromAccount)
	entries, err := iterOf(seqList)
	if err != nil {
		return err
	}
	for _, seqEntry := range entries {
		d, err := dictOf(seqEntry)
		if err != nil {
			return err
		}
		recalled := d["msg_id"]
		if !pyrepr.Truthy(recalled) {
			recalled = strOrEmpty(d["msg_seq"])
		}
		if !pyrepr.Truthy(recalled) {
			continue
		}
		recalledID := pyrepr.Str(recalled)
		if matched, ok := findProcessingSession(adapter, recalledID); ok {
			interruptForRecall(ctx, adapter, matched, recalledID, groupCode, fromAccount)
			continue
		}
		if err := patchTranscript(ctx, adapter, recalledID, groupCode, fromAccount, adapter.cachedMsgContent(recalledID)); err != nil {
			return err
		}
	}
	return nil
}

// Session key whose message in progress is the recalled one and still holds an active guard
func findProcessingSession(adapter *YuanbaoAdapter, recalledID string) (string, bool) {
	for _, pair := range adapter.processingSnapshot() {
		if pair[1] == recalledID && adapter.HasActiveSession(pair[0]) {
			return pair[0], true
		}
	}
	return "", false
}

// Parks a recall notice as the pending message of the busy session, signals its interrupt and schedules the late redaction
func interruptForRecall(ctx context.Context, adapter *YuanbaoAdapter, sessionKey, recalledID, groupCode, fromAccount string) {
	where := "direct chat with " + fromAccount
	if groupCode != "" {
		where = "group " + groupCode
	}
	recallText := "[CRITICAL — MESSAGE RECALLED] The user message that triggered your current task " +
		"(message_id=\"" + recalledID + "\") in " + where + " has been recalled/withdrawn by the sender. " +
		"IGNORE any prior system note asking you to finish processing tool results — the original request is void. " +
		"Do NOT continue the task, do NOT call more tools, do NOT reference the recalled content. " +
		"Reply only with a brief acknowledgment such as \"The message has been recalled.\" in the " +
		"language the user was using."
	event := gateway.NewMessageEvent(recallText)
	event.MessageType, event.Source, event.Internal = gateway.MessageTypeText, recallSource(ctx, adapter, groupCode, fromAccount), true
	adapter.SetPendingMessage(sessionKey, event)
	if active := adapter.ActiveSessionEvent(sessionKey); active != nil {
		active.Set()
	}
	slog.Info(fmt.Sprintf("[%s] Recall interrupt: msg_id=%s session=%s", adapter.Name(), recalledID, pystr.Slice(sessionKey, 0, 30)))
	if recalledText := adapter.processingText(sessionKey); recalledText != "" {
		scheduleContentRedact(ctx, adapter, sessionKey, recalledText, groupCode, fromAccount)
	}
}

// Polls the transcript until the interrupted turn wrote the recalled content, then redacts it
func scheduleContentRedact(ctx context.Context, adapter *YuanbaoAdapter, sessionKey, recalledText, groupCode, fromAccount string) {
	adapter.trackTask(gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		store := sessionStoreOf(adapter)
		if store == nil {
			return nil
		}
		sid, err := resolveRecallSID(tctx, store, adapter, groupCode, fromAccount)
		if err != nil {
			return tctx.Err()
		}
		for range 30 {
			if err := sleep(tctx, 0.5); err != nil {
				return err
			}
			transcript, err := store.LoadTranscript(tctx, sid)
			var unreadable *gateway.TranscriptReadError
			if errors.As(err, &unreadable) {
				slog.Warn(fmt.Sprintf("[%s] Recall redact: transcript unreadable for session %s: %s", adapter.Name(), sid, errText(err)))
				return nil
			}
			if err != nil {
				if tctx.Err() != nil {
					return tctx.Err()
				}
				continue
			}
			rows, err := transcriptRows(transcript)
			if err != nil {
				return err
			}
			for _, entry := range rows {
				if pyrepr.Equal(entry["role"], "user") && pyrepr.Equal(entry["content"], recalledText) {
					redactEntry(tctx, adapter, store, sid, rows, entry,
						fmt.Sprintf("[%s] Recall redact: session %s", adapter.Name(), pystr.Slice(sessionKey, 0, 30)),
						"[%s] Recall redact failed: %s")
					return nil
				}
			}
		}
		slog.Debug(fmt.Sprintf("[%s] Recall redact: content not found after polling, session %s", adapter.Name(), pystr.Slice(sessionKey, 0, 30)))
		return nil
	}))
}

// Redacts the recalled row by platform id or by content, else appends a system note naming the recalled id
func patchTranscript(ctx context.Context, adapter *YuanbaoAdapter, recalledID, groupCode, fromAccount, recalledContent string) error {
	store := sessionStoreOf(adapter)
	if store == nil {
		return nil
	}
	sid, err := resolveRecallSID(ctx, store, adapter, groupCode, fromAccount)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] Recall: failed to resolve session: %s", adapter.Name(), errText(err)))
		return nil
	}
	transcript, err := store.LoadTranscript(ctx, sid)
	var unreadable *gateway.TranscriptReadError
	switch {
	case errors.As(err, &unreadable):
		slog.Warn(fmt.Sprintf("[%s] Recall: transcript unreadable: %s", adapter.Name(), errText(err)))
		return nil
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		slog.Warn(fmt.Sprintf("[%s] Recall: failed to load transcript: %s", adapter.Name(), errText(err)))
		return nil
	}
	rows, err := transcriptRows(transcript)
	if err != nil {
		return err
	}
	var target map[string]any
	for _, entry := range rows {
		if pyrepr.Equal(entry["message_id"], recalledID) {
			target = entry
			break
		}
	}
	branchLabel := "branch A1: id match"
	if target == nil && recalledContent != "" {
		for _, entry := range rows {
			if pyrepr.Equal(entry["role"], "user") && pyrepr.Equal(entry["content"], recalledContent) {
				target = entry
				break
			}
		}
		branchLabel = "branch A2: content match"
	}
	if target != nil {
		redactEntry(ctx, adapter, store, sid, rows, target,
			fmt.Sprintf("[%s] Recall: redacted msg_id=%s (%s)", adapter.Name(), recalledID, branchLabel),
			"[%s] Recall: rewrite_transcript failed: %s")
		return nil
	}
	note := map[string]any{"role": "system", "timestamp": pydatetime.ISOFormatUTC(time.Now()),
		"content": "[recall] message_id=\"" + recalledID + "\" has been recalled; do not quote or reference it."}
	if err := store.AppendToTranscript(ctx, sid, note, false); err != nil {
		return err
	}
	slog.Info(fmt.Sprintf("[%s] Recall: system note for msg_id=%s (branch B)", adapter.Name(), recalledID))
	return nil
}
