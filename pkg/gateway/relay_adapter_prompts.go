package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs a session's pending slash confirm handler with the choice, empty when stale, superseded or resolved
var SlashConfirmResolve func(ctx context.Context, sessionKey, confirmID, choice string) string

var relayPromptUnavailable = SendResult{Success: false, Error: "relay prompt op unavailable"}

var relayAckEmojiDefault = [3]string{"\U0001f440", "✅", "❌"}

var relayAckEmojiByPlatform = map[string][3]string{"telegram": {"\U0001f440", "\U0001f44d", "\U0001f44e"}}

// Unix time in fractional seconds, the time.time of Python
func relayNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Registers a pending prompt under an owner nonce prefixed id and sweeps expired ones
func (a *RelayAdapter) mintPrompt(kind string, state map[string]any, timeoutS float64) string {
	suffix := make([]byte, 4)
	rand.Read(suffix)
	promptID := a.promptOwnerNonce + "." + hex.EncodeToString(suffix)
	entry := maps.Clone(state)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["kind"] = kind
	entry["expires_at"] = relayNow() + timeoutS
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pendingPrompts[promptID] = entry
	now := relayNow()
	for k, v := range a.pendingPrompts {
		if relayPromptExpiry(v) < now {
			delete(a.pendingPrompts, k)
		}
	}
	return promptID
}

// Expiry stamp of a pending prompt, zero when absent
func relayPromptExpiry(state map[string]any) float64 {
	if f, ok := state["expires_at"].(float64); ok {
		return f
	}
	return 0
}

// Whether this process minted the prompt, ids without an owner segment counting as ours
func (a *RelayAdapter) mintedHere(promptID string) bool {
	head, _, found := strings.Cut(promptID, ".")
	if !found {
		return true
	}
	return head == a.promptOwnerNonce
}

// Consumes a pending prompt, nil for an unknown or expired one
func (a *RelayAdapter) popPrompt(promptID string) map[string]any {
	a.mu.Lock()
	state, ok := a.pendingPrompts[promptID]
	delete(a.pendingPrompts, promptID)
	a.mu.Unlock()
	if !ok || len(state) == 0 || relayPromptExpiry(state) < relayNow() {
		return nil
	}
	return state
}

// Remembers an answered prompt in a bounded first in first out memory
func (a *RelayAdapter) notePromptResolved(promptID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resolvedPrompts.set(promptID, relayNow())
	a.resolvedPrompts.evict(relayResolvedPromptMemory)
}

// Egresses one prompt op, nil when the lane is unavailable and a failed result carrying an authorization decline
func (a *RelayAdapter) sendPrompt(ctx context.Context, chatID, promptKind, text, promptID string, options []any, replyTo *string, metadata map[string]any, timeoutS *int) *SendResult {
	action := map[string]any{"op": "prompt", "chat_id": chatID, "content": text, "prompt_kind": promptKind, "prompt_id": promptID,
		"options": options, "reply_to": relayWireReply(a.resolveReplyToForSend(chatID, replyTo, metadata)),
		"metadata": a.withScope(chatID, metadata)}
	if timeoutS != nil {
		action["timeout_s"] = *timeoutS
	}
	result := a.gatedOp(ctx, chatID, action, relayGateOptions{declineLevel: &relayLevelWarning, surfaceDeclines: true})
	if result == nil {
		return nil
	}
	if !pyrepr.Truthy(result["success"]) {
		return &SendResult{Success: false, Error: DeclineError(result), RawResponse: result}
	}
	return &SendResult{Success: true, MessageID: result["message_id"], RawResponse: result}
}

// Registers and egresses a prompt, unregistering it when the lane is unavailable or the connector refused it
func (a *RelayAdapter) mintAndSendPrompt(ctx context.Context, kind string, state map[string]any, chatID, promptKind, text string, options []any, metadata map[string]any) *SendResult {
	entry := maps.Clone(state)
	entry["chat_id"] = chatID
	promptID := a.mintPrompt(kind, entry, 3600.0)
	result := a.sendPrompt(ctx, chatID, promptKind, text, promptID, options, nil, metadata, nil)
	if result == nil || !result.Success {
		a.mu.Lock()
		delete(a.pendingPrompts, promptID)
		a.mu.Unlock()
	}
	return result
}

// Native button exec approval, failing when the lane is unavailable so the text fallback runs
func (a *RelayAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *ExecApprovalPrompt) (SendResult, error) {
	options := make([]any, 0, len(prompt.Actions))
	for _, action := range prompt.Actions {
		option := map[string]any{"id": action.Choice, "label": action.Label}
		if action.Style != "" {
			option["style"] = action.Style
		}
		options = append(options, option)
	}
	result := a.mintAndSendPrompt(ctx, "exec_approval", map[string]any{"session_key": prompt.SessionKey}, prompt.ChatID,
		"approval", prompt.Text, options, prompt.Metadata)
	if result == nil {
		return relayPromptUnavailable, nil
	}
	return *result, nil
}

// Three button slash command confirmation, failing when the lane is unavailable so text intercept runs
func (a *RelayAdapter) SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (SendResult, error) {
	options := []any{
		map[string]any{"id": "once", "label": i18n.T(ctx, "platform.relay.confirm_approve_once", "", nil), "style": "primary"},
		map[string]any{"id": "always", "label": i18n.T(ctx, "platform.relay.confirm_always_approve", "", nil)},
		map[string]any{"id": "cancel", "label": i18n.T(ctx, "platform.relay.confirm_cancel", "", nil), "style": "danger"},
	}
	text := message
	if title != "" {
		text = "**" + title + "**\n\n" + message
	}
	result := a.mintAndSendPrompt(ctx, "slash_confirm", map[string]any{"session_key": sessionKey, "confirm_id": confirmID},
		chatID, "approval", text, options, metadata)
	if result == nil {
		return relayPromptUnavailable, nil
	}
	return *result, nil
}

// Native button clarify with positional option ids, open ended or unavailable ones falling back to the base
func (a *RelayAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (SendResult, error) {
	if len(choices) > 0 && a.Descriptor().SupportsOp("prompt") {
		options := make([]any, 0, len(choices)+1)
		for i, choice := range choices {
			options = append(options, map[string]any{"id": "c" + strconv.Itoa(i), "label": relayRuneHead(choice, 75)})
		}
		options = append(options, map[string]any{"id": "other", "label": i18n.T(ctx, "platform.relay.prompt_other", "", nil)})
		choiceList := make([]any, len(choices))
		for i, c := range choices {
			choiceList[i] = c
		}
		result := a.mintAndSendPrompt(ctx, "clarify", map[string]any{"session_key": sessionKey, "clarify_id": clarifyID,
			"choices": choiceList}, chatID, "clarify", "❓ "+question, options, metadata)
		if result != nil {
			return *result, nil
		}
	}
	return a.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
}

// First n code points of the text
func relayRuneHead(text string, n int) string {
	runes := []rune(text)
	if len(runes) > n {
		return string(runes[:n])
	}
	return text
}

// Routes a prompt answer to its waiting primitive, true whenever the event was a prompt answer
func (a *RelayAdapter) consumePromptResponse(ctx context.Context, event *MessageEvent) (bool, error) {
	pr := event.PromptResponse
	if pr == nil {
		return false, nil
	}
	promptID := pyrepr.Str(pyrepr.Or(pr["prompt_id"], ""))
	optionID := pyrepr.Str(pyrepr.Or(pr["option_id"], ""))
	if promptID == "" || optionID == "" {
		return false, nil
	}
	if !a.mintedHere(promptID) {
		relayAdapterLog().Debug(fmt.Sprintf("relay prompt_response %s (option=%s) belongs to another gateway instance — ignoring", promptID, optionID))
		return true, nil
	}
	a.mu.Lock()
	repeat := a.resolvedPrompts.has(promptID)
	a.mu.Unlock()
	if repeat {
		relayAdapterLog().Debug(fmt.Sprintf("relay prompt_response %s (option=%s) already resolved — ignoring repeat", promptID, optionID))
		return true, nil
	}
	state := a.popPrompt(promptID)
	if state == nil {
		relayAdapterLog().Info(fmt.Sprintf("relay prompt_response for unknown/expired prompt %s (option=%s)", promptID, optionID))
		a.notifyPromptExpired(ctx, event)
		return true, nil
	}
	a.notePromptResolved(promptID)
	kind := state["kind"]
	chatSource := ""
	if event.Source != nil {
		chatSource = event.Source.ChatID
	}
	chatID := pyrepr.Str(pyrepr.Or(state["chat_id"], chatSource))
	ackMeta := a.promptReplyMetadata(event)
	var err error
	switch kind {
	case "exec_approval":
		err = a.resolveExecApproval(ctx, state, optionID, chatID, ackMeta)
	case "slash_confirm":
		err = a.resolveSlashConfirm(ctx, state, optionID, chatID, ackMeta)
	case "clarify":
		err = a.resolveClarify(ctx, state, optionID, chatID, ackMeta)
	default:
		relayAdapterLog().Warn(fmt.Sprintf("relay prompt_response with unknown kind %s", pyrepr.Repr(kind)))
	}
	if err != nil {
		if ctx.Err() != nil {
			return true, err
		}
		relayAdapterLog().Warn("relay prompt_response resolution failed", "exc_info", err)
	}
	return true, nil
}

// Resolves the session's waiting exec approval with the pressed choice and acknowledges it in channel
func (a *RelayAdapter) resolveExecApproval(ctx context.Context, state map[string]any, optionID, chatID string, ackMeta map[string]any) error {
	choice := optionID
	if _, known := relayExecApprovalLabels[choice]; !known {
		choice = "deny"
	}
	count := tools.ResolveGatewayApproval(pyrepr.Str(pyrepr.Or(state["session_key"], "")), choice, false, "", "")
	label := "⌛ Approval expired — no command was waiting."
	if count != 0 {
		label = relayExecApprovalLabels[choice]
	}
	a.sendLifecycleAck(ctx, chatID, label, ackMeta)
	if count != 0 {
		a.ResumeTypingForChat(chatID)
	}
	return nil
}

// Runs the session's pending slash confirm with the pressed choice and acknowledges the choice and its output
func (a *RelayAdapter) resolveSlashConfirm(ctx context.Context, state map[string]any, optionID, chatID string, ackMeta map[string]any) error {
	choice := optionID
	if _, known := relaySlashConfirmLabels[choice]; !known {
		choice = "cancel"
	}
	resultText := SlashConfirmResolve(ctx, pyrepr.Str(pyrepr.Or(state["session_key"], "")), pyrepr.Str(pyrepr.Or(state["confirm_id"], "")), choice)
	a.sendLifecycleAck(ctx, chatID, relaySlashConfirmLabels[choice], ackMeta)
	if resultText != "" {
		a.sendLifecycleAck(ctx, chatID, resultText, ackMeta)
	}
	return nil
}

// Resolves a clarify with the chosen text, flipping to text capture for other or an unmappable option
func (a *RelayAdapter) resolveClarify(ctx context.Context, state map[string]any, optionID, chatID string, ackMeta map[string]any) error {
	clarifyID := pyrepr.Str(pyrepr.Or(state["clarify_id"], ""))
	if optionID == "other" {
		tools.MarkClarifyAwaitingText(clarifyID)
		a.sendLifecycleAck(ctx, chatID, "✏️ Type your answer:", ackMeta)
		return nil
	}
	choices, _ := state["choices"].([]any)
	idx := -1
	if strings.HasPrefix(optionID, "c") {
		if n, ok := pystr.IntText(optionID[1:]); ok && n.IsInt64() {
			idx = int(n.Int64())
		}
	}
	if idx >= 0 && idx < len(choices) {
		tools.ResolveGatewayClarify(clarifyID, pyrepr.Str(choices[idx]))
		a.sendLifecycleAck(ctx, chatID, "✅ "+pyrepr.Str(choices[idx]), ackMeta)
		return nil
	}
	tools.MarkClarifyAwaitingText(clarifyID)
	a.sendLifecycleAck(ctx, chatID, "✏️ Type your answer:", ackMeta)
	return nil
}

// Sends a prompt lifecycle ack in the background so the read loop never waits on its own outbound result
func (a *RelayAdapter) sendLifecycleAck(ctx context.Context, chatID, text string, metadata map[string]any) {
	task := spawnAdapterTask(ctx, func(tctx context.Context) error {
		if _, err := a.Send(tctx, chatID, text, "", metadata); err != nil {
			relayAdapterLog().Debug("relay lifecycle ack failed", "exc_info", err)
		}
		return nil
	})
	a.mu.Lock()
	a.lifecycleAckTasks[task] = struct{}{}
	a.mu.Unlock()
	task.addDoneCallback(func() {
		a.mu.Lock()
		delete(a.lifecycleAckTasks, task)
		a.mu.Unlock()
	})
}

// Tells the presser their prompt is no longer waiting
func (a *RelayAdapter) notifyPromptExpired(ctx context.Context, event *MessageEvent) {
	if event.Source == nil || event.Source.ChatID == "" {
		return
	}
	a.sendLifecycleAck(ctx, event.Source.ChatID, i18n.T(ctx, "platform.relay.prompt_expired", "", nil), a.promptReplyMetadata(event))
}

// Interim marked metadata threading a prompt ack where its prompt lives
func (a *RelayAdapter) promptReplyMetadata(event *MessageEvent) map[string]any {
	meta := map[string]any{"_interim_send": true}
	if event.Source != nil && event.Source.ThreadID != nil && *event.Source.ThreadID != "" {
		meta["thread_id"] = *event.Source.ThreadID
	}
	return meta
}

// In progress, success and failure reactions of the event's lane, then the chat's, then the primary's
func (a *RelayAdapter) ackEmoji(event *MessageEvent, chatID string) [3]string {
	var candidates []string
	if event != nil && event.Source != nil {
		candidates = append(candidates, string(event.Source.Platform))
	}
	a.mu.Lock()
	candidates = append(candidates, a.platformByChat[chatID])
	a.mu.Unlock()
	candidates = append(candidates, a.Descriptor().Platform)
	for _, candidate := range candidates {
		name := pystr.Lower(candidate)
		if name != "" && name != string(PlatformRelay) {
			if set, ok := relayAckEmojiByPlatform[name]; ok {
				return set
			}
			return relayAckEmojiDefault
		}
	}
	return relayAckEmojiDefault
}

// Adds or removes one reaction through the op gated lane, false on any failure
func (a *RelayAdapter) react(ctx context.Context, chatID, messageID, emoji string, remove bool) bool {
	if chatID == "" || messageID == "" {
		return false
	}
	result := a.gatedOp(ctx, chatID, map[string]any{"op": "react", "chat_id": chatID, "message_id": messageID, "emoji": emoji,
		"remove": remove, "metadata": a.withScope(chatID, nil)}, relayGateOptions{})
	return result != nil
}

// Adds the in progress reaction
func (a *RelayAdapter) OnProcessingStart(ctx context.Context, event *MessageEvent) error {
	messageID, chatID := relayEventIDs(event)
	if messageID != nil && *messageID != "" && chatID != "" {
		a.react(ctx, chatID, *messageID, a.ackEmoji(event, chatID)[0], false)
	}
	return nil
}

// Swaps the in progress reaction for the outcome one
func (a *RelayAdapter) OnProcessingComplete(ctx context.Context, event *MessageEvent, outcome ProcessingOutcome) error {
	messageID, chatID := relayEventIDs(event)
	if messageID == nil || *messageID == "" || chatID == "" {
		return nil
	}
	set := a.ackEmoji(event, chatID)
	a.react(ctx, chatID, *messageID, set[0], true)
	switch outcome {
	case ProcessingSuccess:
		a.react(ctx, chatID, *messageID, set[1], false)
	case ProcessingFailure:
		a.react(ctx, chatID, *messageID, set[2], false)
	}
	return nil
}

// Creates a thread or topic under the parent through the connector, false on any failure
func (a *RelayAdapter) CreateHandoffThread(ctx context.Context, parentChatID, name string) (string, bool, error) {
	threadName := pystr.Strip(name)
	if threadName == "" {
		threadName = "handoff"
	}
	result := a.gatedOp(ctx, parentChatID, map[string]any{"op": "thread_create", "chat_id": parentChatID,
		"thread_name": relayRuneHead(threadName, 100), "metadata": a.withScope(parentChatID, nil)},
		relayGateOptions{declineLevel: &relayLevelInfo, subject: parentChatID})
	if result == nil {
		return "", false, nil
	}
	threadID := pyrepr.Or(result["thread_id"], result["message_id"])
	if !pyrepr.Truthy(threadID) {
		return "", false, nil
	}
	return pyrepr.Str(threadID), true, nil
}

// Renames a thread through the connector, its no clobber guard preferred over the legacy name guard
func (a *RelayAdapter) RenameThread(ctx context.Context, threadID, name string, opts ThreadRenameOptions) (bool, error) {
	cleaned := pystr.Strip(strings.Join(pystr.SplitWhitespace(name, -1), " "))
	if cleaned == "" || threadID == "" {
		return false, nil
	}
	chatID := threadID
	if opts.ParentChatID != nil && *opts.ParentChatID != "" {
		chatID = *opts.ParentChatID
	}
	action := map[string]any{"op": "thread_rename", "chat_id": chatID, "message_id": threadID,
		"thread_name": relayRuneHead(cleaned, 100), "metadata": a.withScope(chatID, nil)}
	if opts.PreferConnectorCreated {
		action["only_if_connector_created"] = true
	} else if opts.OnlyIfCurrentName != nil {
		action["only_if_current_name"] = *opts.OnlyIfCurrentName
	}
	a.mu.Lock()
	platform := a.platformByChat[chatID]
	if platform == "" {
		platform = a.platformByChat[threadID]
	}
	a.mu.Unlock()
	result := a.gatedOp(ctx, chatID, action, relayGateOptions{declineLevel: &relayLevelInfo, subject: threadID, platform: platform})
	return result != nil, nil
}
