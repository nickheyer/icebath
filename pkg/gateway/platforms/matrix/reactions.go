package matrix

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Resolves the waiting gateway approval of a session with the chosen scope
var resolveGatewayApprovalFn = tools.ResolveGatewayApproval

// Sends an emoji reaction and returns its event id, empty on failure
func (a *MatrixAdapter) sendReaction(ctx context.Context, roomID, eventID, emoji string) string {
	client := a.currentClient()
	if client == nil {
		return ""
	}
	content := map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": eventID, "key": emoji}}
	resp, err := client.SendMessageEvent(ctx, id.RoomID(roomID), event.EventReaction, content)
	if err != nil {
		slog.Debug(fmt.Sprintf("Matrix: reaction send error: %v", err))
		return ""
	}
	slog.Debug(fmt.Sprintf("Matrix: sent reaction %s to %s", emoji, eventID))
	return string(resp.EventID)
}

// Redacts a reaction event
func (a *MatrixAdapter) redactReaction(ctx context.Context, roomID, reactionEventID, reason string) bool {
	return a.RedactMessage(ctx, roomID, reactionEventID, reason)
}

// Redacts a reaction after a short delay so the final message lands first
func (a *MatrixAdapter) scheduleReactionRedaction(ctx context.Context, roomID, reactionEventID, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delay := a.reactionRedactionDelaySeconds
	var task *bgTask
	ready := make(chan struct{})
	task = startTask(context.WithoutCancel(ctx), func(tctx context.Context) {
		<-ready
		defer func() {
			a.mu.Lock()
			delete(a.reactionRedactionTasks, task)
			a.mu.Unlock()
		}()
		if delay != 0 {
			select {
			case <-tctx.Done():
				return
			case <-time.After(time.Duration(delay * float64(time.Second))):
			}
		}
		if !a.redactReaction(tctx, roomID, reactionEventID, reason) {
			slog.Debug(fmt.Sprintf("Matrix: failed to redact reaction %s", reactionEventID))
		}
	})
	a.reactionRedactionTasks[task] = struct{}{}
	close(ready)
}

// Eyes reaction on the message being processed when reactions are enabled
func (a *MatrixAdapter) OnProcessingStart(ctx context.Context, ev *gateway.MessageEvent) error {
	msgID, roomID := eventIDs(ev)
	if a.reactionsEnabled && msgID != "" && roomID != "" {
		if reactionEventID := a.sendReaction(ctx, roomID, msgID, "\U0001f440"); reactionEventID != "" {
			a.mu.Lock()
			a.pendingReactions[[2]string{roomID, msgID}] = reactionEventID
			a.mu.Unlock()
		}
	}
	return nil
}

// Swaps the eyes reaction for a check or cross once processing ends, skipping cancelled runs
func (a *MatrixAdapter) OnProcessingComplete(ctx context.Context, ev *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	msgID, roomID := eventIDs(ev)
	if !a.reactionsEnabled || msgID == "" || roomID == "" || outcome == gateway.ProcessingCancelled {
		return nil
	}
	a.mu.Lock()
	key := [2]string{roomID, msgID}
	eyesEventID := a.pendingReactions[key]
	delete(a.pendingReactions, key)
	a.mu.Unlock()
	if eyesEventID != "" {
		a.scheduleReactionRedaction(ctx, roomID, eyesEventID, "processing complete")
	}
	emoji := "❌"
	if outcome == gateway.ProcessingSuccess {
		emoji = "✅"
	}
	a.sendReaction(ctx, roomID, msgID, emoji)
	return nil
}

// Message id and chat id of an event, empty when absent
func eventIDs(ev *gateway.MessageEvent) (string, string) {
	msgID, roomID := "", ""
	if ev.MessageID != nil {
		msgID = *ev.MessageID
	}
	if ev.Source != nil {
		roomID = ev.Source.ChatID
	}
	return msgID, roomID
}

// Routes a reaction to the approval, model picker and choice picker handlers in turn
func (a *MatrixAdapter) onReaction(ctx context.Context, evt *event.Event) error {
	sender := string(evt.Sender)
	if a.isSelfSender(sender) {
		return nil
	}
	if a.isDuplicateEvent(string(evt.ID)) {
		return nil
	}
	roomID := string(evt.RoomID)
	content := evt.Content.Raw
	if len(content) == 0 {
		return nil
	}
	relatesTo := objectOf(content["m.relates_to"])
	reactsTo := strOf(relatesTo["event_id"])
	key := strOf(relatesTo["key"])
	slog.Info(fmt.Sprintf("Matrix: reaction %s from %s on %s in %s", key, sender, reactsTo, roomID))
	for _, handler := range []func(ctx context.Context, roomID, reactsTo, key, sender string) bool{
		a.handleApprovalReaction, a.handleModelPickerReaction, a.handleChoicePickerReaction} {
		if handler(ctx, roomID, reactsTo, key, sender) {
			return nil
		}
	}
	return nil
}

// Shared prompt gate, handled false when not a pending prompt and ok false when consumed without action
func (a *MatrixAdapter) claimReactionPrompt(ctx context.Context, p *promptState, roomID, reactsTo, key, sender, label, invalidText string,
	onExpired func(), choice func(key string) bool) (handled, ok bool) {
	a.mu.Lock()
	resolved := p == nil || p.resolved
	a.mu.Unlock()
	if resolved {
		return false, false
	}
	if roomID != p.chatID {
		return true, false
	}
	if a.matrixPromptExpired(p) {
		onExpired()
		return true, false
	}
	if !a.validateMatrixPromptReactor(ctx, roomID, reactsTo, sender, p, label) {
		return true, false
	}
	if !choice(key) {
		a.sendInvalidReactionFeedback(ctx, roomID, reactsTo, invalidText)
		return true, false
	}
	return true, true
}

// Resolves a pending exec approval from a reaction, true when the reaction targeted one
func (a *MatrixAdapter) handleApprovalReaction(ctx context.Context, roomID, reactsTo, key, sender string) bool {
	a.mu.Lock()
	prompt := a.approvalPromptsByEvent[reactsTo]
	a.mu.Unlock()
	var state *promptState
	if prompt != nil {
		state = &prompt.promptState
	}
	choice, known := a.approvalReactionMap[key]
	handled, ok := a.claimReactionPrompt(ctx, state, roomID, reactsTo, key, sender, "approval",
		i18n.T(ctx, "platform.matrix.approval.invalid_reaction", "", nil),
		func() { a.expireMatrixApprovalPrompt(ctx, roomID, reactsTo, prompt) },
		func(string) bool { return known })
	if !ok {
		return handled
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error(fmt.Sprintf("Failed to resolve gateway approval from Matrix reaction: %v", r))
			}
		}()
		count := resolveGatewayApprovalFn(prompt.sessionKey, choice, false, "", "")
		if count == 0 {
			return
		}
		a.mu.Lock()
		prompt.resolved = true
		delete(a.approvalPromptsByEvent, reactsTo)
		delete(a.approvalPromptBySession, prompt.sessionKey)
		a.mu.Unlock()
		slog.Info(fmt.Sprintf("Matrix reaction resolved %d approval(s) for session %s (choice=%s, user=%s)", count, prompt.sessionKey, choice, sender))
		a.redactBotApprovalReactions(ctx, roomID, &prompt.promptState)
	}()
	return true
}

// Applies a model picker reaction, true when it targeted a pending picker
func (a *MatrixAdapter) handleModelPickerReaction(ctx context.Context, roomID, reactsTo, key, sender string) bool {
	return a.handlePickerReaction(ctx, a.modelPickerPromptsByEvent, roomID, reactsTo, key, sender, "model picker",
		i18n.T(ctx, "platform.matrix.picker.invalid_model_reaction", "", nil),
		func(p *matrixPickerPrompt) { a.expireMatrixModelPickerPrompt(ctx, roomID, reactsTo, p) },
		"switch model", "platform.matrix.picker.verb_switch_model", true)
}

// Applies a choice picker reaction, true when it targeted a pending picker
func (a *MatrixAdapter) handleChoicePickerReaction(ctx context.Context, roomID, reactsTo, key, sender string) bool {
	expire := func(*matrixPickerPrompt) {
		a.mu.Lock()
		delete(a.choicePickerPromptsByEvent, reactsTo)
		a.mu.Unlock()
	}
	return a.handlePickerReaction(ctx, a.choicePickerPromptsByEvent, roomID, reactsTo, key, sender, "choice picker",
		i18n.T(ctx, "platform.matrix.picker.invalid_choice_reaction", "", nil), expire,
		"apply choice", "platform.matrix.picker.verb_apply_selection", false)
}

// Claims the picker, runs its selection callback and posts the confirmation or the failure
func (a *MatrixAdapter) handlePickerReaction(ctx context.Context, registry map[string]*matrixPickerPrompt, roomID, reactsTo, key, sender, label,
	invalidText string, onExpired func(*matrixPickerPrompt), logVerb, verbKey string, redactBotReactions bool) bool {
	a.mu.Lock()
	prompt := registry[reactsTo]
	a.mu.Unlock()
	var state *promptState
	var selection []string
	if prompt != nil {
		state = &prompt.promptState
		selection = prompt.choices[key]
	}
	handled, ok := a.claimReactionPrompt(ctx, state, roomID, reactsTo, key, sender, label, invalidText,
		func() { onExpired(prompt) }, func(string) bool { return selection != nil })
	if !ok {
		return handled
	}
	a.mu.Lock()
	prompt.resolved = true
	delete(registry, reactsTo)
	a.mu.Unlock()
	confirmation, err := prompt.onSelected(ctx, roomID, selection...)
	if err == nil && redactBotReactions {
		a.redactBotModelPickerReactions(ctx, roomID, &prompt.promptState)
	}
	if err == nil && confirmation != "" {
		_, err = a.Send(ctx, roomID, confirmation, reactsTo, nil)
	}
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to %s from Matrix reaction: %v", logVerb, err))
		_, _ = a.Send(ctx, roomID, i18n.T(ctx, "platform.matrix.picker.failed", "",
			map[string]any{"action": i18n.T(ctx, verbKey, "", nil), "error": err.Error()}), reactsTo, nil)
	}
	return true
}

// Whether the prompt deadline passed
func (a *MatrixAdapter) matrixPromptExpired(p *promptState) bool {
	return p.expiresAt != nil && monotonic() > *p.expiresAt
}

// Live gateway authorization, the startup allowlist snapshot when no check is wired
func (a *MatrixAdapter) isAuthorizedUser(userID, roomID string) bool {
	if a.HasAuthorizationCheck() {
		authorized := a.IsSenderAuthorized(userID, nil, &roomID, gateway.AuthorizationOpts{})
		return authorized != nil && *authorized
	}
	allowAll, _ := gateway.GetScopedSecret(a.ctx, "GATEWAY_ALLOW_ALL_USERS", "", false)
	switch pystr.Lower(pystr.Strip(allowAll)) {
	case "true", "1", "yes":
		return true
	}
	return len(a.allowedUserIDs) > 0 && a.allowedUserIDs[userID]
}

// Whether the reactor may answer the prompt, posting feedback when not
func (a *MatrixAdapter) validateMatrixPromptReactor(ctx context.Context, roomID, targetEventID, sender string, p *promptState, label string) bool {
	if !a.isAuthorizedUser(sender, roomID) {
		slog.Info(fmt.Sprintf("Matrix: ignoring %s reaction from unauthorized user %s on %s", label, sender, targetEventID))
		a.sendInvalidReactionFeedback(ctx, roomID, targetEventID, i18n.T(ctx, "platform.matrix.reaction.unauthorized", "", nil))
		return false
	}
	if requester := p.requesterUserID; a.approvalRequireSender && requester != nil && *requester != "" && sender != *requester {
		slog.Info(fmt.Sprintf("Matrix: ignoring %s reaction from %s; requester is %s", label, sender, *requester))
		a.sendInvalidReactionFeedback(ctx, roomID, targetEventID, i18n.T(ctx, "platform.matrix.reaction.not_requester", "", nil))
		return false
	}
	return true
}

// Replies to the prompt with feedback text, errors logged at debug
func (a *MatrixAdapter) sendInvalidReactionFeedback(ctx context.Context, roomID, targetEventID, text string) {
	if _, err := a.Send(ctx, roomID, text, targetEventID, nil); err != nil {
		slog.Debug(fmt.Sprintf("Matrix: failed to send invalid reaction feedback: %v", err))
	}
}

// Expires an approval prompt, redacting its controls and telling the reactor
func (a *MatrixAdapter) expireMatrixApprovalPrompt(ctx context.Context, roomID, targetEventID string, prompt *matrixApprovalPrompt) {
	a.mu.Lock()
	prompt.resolved = true
	delete(a.approvalPromptsByEvent, targetEventID)
	delete(a.approvalPromptBySession, prompt.sessionKey)
	a.mu.Unlock()
	a.redactBotApprovalReactions(ctx, roomID, &prompt.promptState)
	a.sendInvalidReactionFeedback(ctx, roomID, targetEventID, i18n.T(ctx, "platform.matrix.approval.expired", "", nil))
}

// Expires a model picker, redacting its controls and telling the reactor
func (a *MatrixAdapter) expireMatrixModelPickerPrompt(ctx context.Context, roomID, targetEventID string, prompt *matrixPickerPrompt) {
	a.mu.Lock()
	prompt.resolved = true
	delete(a.modelPickerPromptsByEvent, targetEventID)
	a.mu.Unlock()
	a.redactBotModelPickerReactions(ctx, roomID, &prompt.promptState)
	a.sendInvalidReactionFeedback(ctx, roomID, targetEventID, i18n.T(ctx, "platform.shared.model_picker_expired", "", nil))
}

// Schedules delayed redaction of the bot seeded approval reactions
func (a *MatrixAdapter) redactBotApprovalReactions(ctx context.Context, roomID string, p *promptState) {
	for _, emoji := range a.botReactionKeys(p) {
		evtID := a.botReactionEvent(p, emoji)
		a.scheduleReactionRedaction(ctx, roomID, evtID, "approval resolved")
		slog.Debug(fmt.Sprintf("Matrix: scheduled bot reaction redaction %s (%s)", emoji, evtID))
	}
}

// Redacts the bot seeded model picker reactions at once
func (a *MatrixAdapter) redactBotModelPickerReactions(ctx context.Context, roomID string, p *promptState) {
	for _, emoji := range a.botReactionKeys(p) {
		evtID := a.botReactionEvent(p, emoji)
		a.RedactMessage(ctx, roomID, evtID, "model picker resolved")
		slog.Debug(fmt.Sprintf("Matrix: redacted model picker reaction %s (%s)", emoji, evtID))
	}
}

// Seeded emoji of a prompt in insertion order
func (a *MatrixAdapter) botReactionKeys(p *promptState) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), p.botReactionEvents.keys...)
}

// Bot reaction event id seeded for an emoji
func (a *MatrixAdapter) botReactionEvent(p *promptState, emoji string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return p.botReactionEvents.vals[emoji]
}
