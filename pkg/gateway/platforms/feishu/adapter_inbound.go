package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/google/uuid"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

var displayNameSanitizeRe = pyre.MustCompile(`[^\w.\- ]`)

// Resolves approval waits, the tools approval queue in production
var resolveGatewayApprovalFn = tools.ResolveGatewayApproval

// Reaction created or deleted event fields the router reads
type reactionEvent struct {
	MessageID    string
	OperatorType string
	EmojiType    string
	UserID       *larkim.UserId
	Raw          any
}

func reactionFromCreated(e *larkim.P2MessageReactionCreatedV1) reactionEvent {
	out := reactionEvent{Raw: e}
	if e != nil && e.Event != nil {
		out.MessageID, out.OperatorType, out.UserID = sdkStr(e.Event.MessageId), sdkStr(e.Event.OperatorType), e.Event.UserId
		if e.Event.ReactionType != nil {
			out.EmojiType = sdkStr(e.Event.ReactionType.EmojiType)
		}
	}
	return out
}

func reactionFromDeleted(e *larkim.P2MessageReactionDeletedV1) reactionEvent {
	out := reactionEvent{Raw: e}
	if e != nil && e.Event != nil {
		out.MessageID, out.OperatorType, out.UserID = sdkStr(e.Event.MessageId), sdkStr(e.Event.OperatorType), e.Event.UserId
		if e.Event.ReactionType != nil {
			out.EmojiType = sdkStr(e.Event.ReactionType.EmojiType)
		}
	}
	return out
}

func botAddedChatID(e *larkim.P2ChatMemberBotAddedV1) string {
	if e == nil || e.Event == nil {
		return ""
	}
	return sdkStr(e.Event.ChatId)
}

func botDeletedChatID(e *larkim.P2ChatMemberBotDeletedV1) string {
	if e == nil || e.Event == nil {
		return ""
	}
	return sdkStr(e.Event.ChatId)
}

// Sender id tiers of an SDK user id
func senderIDOf(u *larkim.UserId) SenderID {
	if u == nil {
		return SenderID{}
	}
	return SenderID{OpenID: nonEmpty(u.OpenId), UserID: nonEmpty(u.UserId), UnionID: nonEmpty(u.UnionId)}
}

func nonEmpty(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}

func loopAcceptsCallbacks(loop *adapterLoop) bool { return loop != nil && !loop.closed() }

// Schedules background work on the adapter loop, logging a failed schedule and a failed run
func (a *FeishuAdapter) submitOnLoop(loop *adapterLoop, fn func(ctx context.Context) error) bool {
	if !loopAcceptsCallbacks(loop) {
		slog.Warn("[Feishu] Failed to schedule background callback work")
		return false
	}
	task := gateway.SpawnAdapterTask(loop.ctx, fn)
	task.AddDoneCallback(func() { logBackgroundFailure(task) })
	return true
}

func logBackgroundFailure(task *gateway.AdapterTask) {
	if err := task.Wait(context.Background()); err != nil {
		slog.Error("[Feishu] Background inbound processing failed: " + err.Error())
	}
}

// Queues a message event for replay while the loop is not ready, handling it on the loop otherwise
func (a *FeishuAdapter) onMessageEvent(data *larkim.P2MessageReceiveV1) {
	loop := a.currentLoop()
	if !loopAcceptsCallbacks(loop) {
		if a.enqueuePendingInboundEvent(data) {
			go a.drainPendingInboundEvents()
		}
		return
	}
	a.submitOnLoop(loop, func(ctx context.Context) error { return a.handleMessageEventData(ctx, data) })
}

// Queues an event for replay, dropping the oldest at the cap, true when the caller starts the drainer
func (a *FeishuAdapter) enqueuePendingInboundEvent(data *larkim.P2MessageReceiveV1) bool {
	a.pendingInboundMu.Lock()
	if len(a.pendingInboundEvents) >= a.pendingInboundMaxDepth {
		dropped := a.pendingInboundEvents[0]
		a.pendingInboundEvents = a.pendingInboundEvents[1:]
		messageID := "unknown"
		if dropped != nil && dropped.Event != nil && dropped.Event.Message != nil && sdkStr(dropped.Event.Message.MessageId) != "" {
			messageID = *dropped.Event.Message.MessageId
		}
		slog.Error(fmt.Sprintf("[Feishu] Pending-inbound queue full (%d); dropped oldest event %s", a.pendingInboundMaxDepth, messageID))
	}
	a.pendingInboundEvents = append(a.pendingInboundEvents, data)
	depth := len(a.pendingInboundEvents)
	shouldStart := !a.pendingDrainScheduled
	a.pendingDrainScheduled = true
	a.pendingInboundMu.Unlock()
	slog.Warn(fmt.Sprintf("[Feishu] Queued inbound event for replay (loop not ready, queue depth=%d)", depth))
	return shouldStart
}

// Replays queued events once the loop is ready, dropping them on shutdown or after two minutes
func (a *FeishuAdapter) drainPendingInboundEvents() {
	defer func() {
		a.pendingInboundMu.Lock()
		a.pendingDrainScheduled = false
		a.pendingInboundMu.Unlock()
	}()
	takeAll := func() []*larkim.P2MessageReceiveV1 {
		a.pendingInboundMu.Lock()
		defer a.pendingInboundMu.Unlock()
		batch := a.pendingInboundEvents
		a.pendingInboundEvents = nil
		return batch
	}
	queueEmpty := func() bool {
		a.pendingInboundMu.Lock()
		defer a.pendingInboundMu.Unlock()
		return len(a.pendingInboundEvents) == 0
	}
	waited := time.Duration(0)
	for {
		if !a.IsConnected() {
			if dropped := len(takeAll()); dropped > 0 {
				slog.Warn(fmt.Sprintf("[Feishu] Dropped %d queued inbound event(s) during shutdown", dropped))
			}
			return
		}
		loop := a.currentLoop()
		if loopAcceptsCallbacks(loop) {
			batch := takeAll()
			if len(batch) == 0 {
				if queueEmpty() {
					return
				}
				continue
			}
			var requeue []*larkim.P2MessageReceiveV1
			for _, e := range batch {
				ev := e
				if !a.submitOnLoop(loop, func(ctx context.Context) error { return a.handleMessageEventData(ctx, ev) }) {
					requeue = append(requeue, ev)
				}
			}
			if len(requeue) > 0 {
				a.pendingInboundMu.Lock()
				a.pendingInboundEvents = append(requeue, a.pendingInboundEvents...)
				a.pendingInboundMu.Unlock()
			}
			if n := len(batch) - len(requeue); n > 0 {
				slog.Info(fmt.Sprintf("[Feishu] Replayed %d queued inbound event(s)", n))
			}
			if len(requeue) == 0 && queueEmpty() {
				return
			}
			continue
		}
		if waited >= a.pendingMaxWait {
			slog.Error(fmt.Sprintf("[Feishu] Adapter loop unavailable for %.0fs; dropped %d queued inbound event(s)", a.pendingMaxWait.Seconds(), len(takeAll())))
			return
		}
		time.Sleep(a.pendingPollInterval)
		waited += a.pendingPollInterval
	}
}

// Shared inbound handling of websocket and webhook message events through dedup and admission
func (a *FeishuAdapter) handleMessageEventData(ctx context.Context, data *larkim.P2MessageReceiveV1) error {
	if data == nil || data.Event == nil || data.Event.Message == nil || data.Event.Sender == nil || data.Event.Sender.SenderId == nil {
		slog.Debug("[Feishu] Dropping malformed inbound event: missing message/sender")
		return nil
	}
	message, sender := data.Event.Message, data.Event.Sender
	messageID := sdkStr(message.MessageId)
	if messageID == "" {
		slog.Debug("[Feishu] Dropping duplicate/missing message_id: " + pyrepr.Str(ptrAny(message.MessageId)))
		return nil
	}
	dup, err := a.IsDuplicate(ctx, messageID)
	if err != nil {
		return err
	}
	if dup {
		slog.Debug("[Feishu] Dropping duplicate/missing message_id: " + messageID)
		return nil
	}
	if reason := a.admit(sender, message); reason != "" {
		slog.Debug("[Feishu] dropping inbound event: " + reason)
		if reason == "group_policy_rejected" {
			a.warnOnceEmptyAllowlistDeny(sdkStr(message.ChatId))
		}
		return nil
	}
	return a.processInboundMessage(ctx, data, message, senderIDOf(sender.SenderId), sdkStr(message.ChatType), messageID, isBotSender(sender))
}

func ptrAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Ignores read receipts, whose event carries no message to name
func (a *FeishuAdapter) onMessageReadEvent() {
	slog.Debug("[Feishu] Ignoring message_read event: ")
}

// Logs a bot membership change and forgets the chat's cached info
func (a *FeishuAdapter) onBotMembershipChange(chatID, verb string) {
	slog.Info(fmt.Sprintf("[Feishu] Bot %s chat: %s", verb, chatID))
	a.mu.Lock()
	delete(a.chatInfoCache, chatID)
	a.mu.Unlock()
}

// Schedules work on the adapter loop, or logs and drops it while the loop is not ready
func (a *FeishuAdapter) submitIfReady(label string, fn func(ctx context.Context) error) {
	loop := a.currentLoop()
	if !loopAcceptsCallbacks(loop) {
		slog.Warn(fmt.Sprintf("[Feishu] Dropping %s before adapter loop is ready", label))
		return
	}
	a.submitOnLoop(loop, fn)
}

// Hands a drive comment event to the comment handler on the adapter loop
func (a *FeishuAdapter) onDriveCommentEvent(data string) {
	a.submitIfReady("drive comment event", func(ctx context.Context) error {
		return HandleDriveCommentEvent(ctx, a.currentClient(), data, a.botOpenIDValue(), a.OwnerProfile() != "")
	})
}

// Hands a meeting invite event to the meeting invite handler on the adapter loop
func (a *FeishuAdapter) onMeetingInvitedEvent(data string) {
	a.submitIfReady("meeting invite event", func(ctx context.Context) error { return HandleMeetingInvitedEvent(ctx, a, data) })
}

func (a *FeishuAdapter) botOpenIDValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botOpenID
}

// Routes a human reaction on a bot message as a synthetic text event, ignoring bot and app reactions
func (a *FeishuAdapter) onReactionEvent(eventType string, e reactionEvent) {
	action := "removed"
	if strings.Contains(eventType, "created") {
		action = "added"
	}
	slog.Debug(fmt.Sprintf("[Feishu] Reaction %s on message %s (operator_type=%s, emoji=%s)", action, e.MessageID, e.OperatorType, e.EmojiType))
	loop := a.currentLoop()
	if e.OperatorType == "bot" || e.OperatorType == "app" || e.MessageID == "" || !loopAcceptsCallbacks(loop) {
		return
	}
	a.submitOnLoop(loop, func(ctx context.Context) error { return a.handleReactionEvent(ctx, eventType, e) })
}

// Card action event fields read from the callback payload
type cardActionEvent struct {
	Raw        any
	Token      string
	OpenChatID string
	OpenID     string
	UserID     string
	UnionID    string
	Tag        string
	Value      any
}

// Card action fields of a decoded card.action.trigger payload
func parseCardAction(payload any) cardActionEvent {
	out := cardActionEvent{Raw: payload}
	root, _ := payload.(*pyjson.Object)
	event, _ := dget(root, "event").(*pyjson.Object)
	out.Token = strOr(dget(event, "token"))
	ctxObj, _ := dget(event, "context").(*pyjson.Object)
	out.OpenChatID = strOr(dget(ctxObj, "open_chat_id"))
	op, _ := dget(event, "operator").(*pyjson.Object)
	out.OpenID, out.UserID, out.UnionID = strOr(dget(op, "open_id")), strOr(dget(op, "user_id")), strOr(dget(op, "union_id"))
	act, _ := dget(event, "action").(*pyjson.Object)
	out.Tag = strOr(dget(act, "tag"))
	out.Value = dget(act, "value")
	return out
}

// SDK card callback entry decoding the raw request body for the synchronous handler
func (a *FeishuAdapter) onCardActionTriggerSDK(ctx context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	var body []byte
	if e != nil && e.EventReq != nil {
		body = e.EventReq.Body
	}
	payload, err := pyjson.Loads(string(body))
	if err != nil {
		payload = pyjson.NewObject()
	}
	return a.onCardActionTrigger(parseCardAction(payload)), nil
}

// Answers approval and update prompt clicks with the resolved card inline and routes other clicks as commands
func (a *FeishuAdapter) onCardActionTrigger(e cardActionEvent) *callback.CardActionTriggerResponse {
	loop := a.currentLoop()
	if !loopAcceptsCallbacks(loop) {
		slog.Warn("[Feishu] Dropping card action before adapter loop is ready")
		return cardResponse(nil)
	}
	if value, ok := e.Value.(*pyjson.Object); ok {
		if pyrepr.Truthy(dget(value, "hermes_action")) {
			return a.handleApprovalCardAction(e, value, loop)
		}
		if pyrepr.Truthy(dget(value, "hermes_update_prompt_action")) {
			return a.handleUpdatePromptCardAction(e, value, loop)
		}
	}
	a.submitOnLoop(loop, func(ctx context.Context) error { return a.handleCardActionEvent(ctx, e) })
	return cardResponse(nil)
}

// Whether a card operator may answer gated prompts, the gateway authorization deciding when no Feishu list is set
func (a *FeishuAdapter) isInteractiveOperatorAuthorized(openID, userID, unionID, chatID string) bool {
	normalized := pystr.Strip(openID)
	if normalized == "" {
		return false
	}
	allowed := map[string]bool{}
	for k := range a.admins {
		allowed[k] = true
	}
	for k := range a.allowedGroupUsers {
		allowed[k] = true
	}
	if len(allowed) > 0 {
		if allowed["*"] {
			return true
		}
		for _, id := range []string{normalized, pystr.Strip(userID), pystr.Strip(unionID)} {
			if id != "" && allowed[id] {
				return true
			}
		}
		return false
	}
	principal := pystr.Strip(userID)
	if principal == "" {
		principal = normalized
	}
	chatType := "group"
	var chatPtr *string
	if chatID != "" {
		chatPtr = &chatID
	}
	verdict := a.IsSenderAuthorized(principal, &chatType, chatPtr, gateway.AuthorizationOpts{})
	if verdict == nil {
		slog.Debug(fmt.Sprintf("[Feishu] No gateway authorization check for card operator %s; denying", normalized))
		return false
	}
	return *verdict
}

// Synchronous card callback response, a raw card replacing the clicked one when given
func cardResponse(card *pyjson.Object) *callback.CardActionTriggerResponse {
	resp := &callback.CardActionTriggerResponse{}
	if card != nil {
		resp.Card = &callback.Card{Type: "raw", Data: card}
	}
	return resp
}

// Operator and chat checks shared by approval and update prompt clicks, false when already logged
func (a *FeishuAdapter) validateCardAction(e cardActionEvent, state map[string]string, label string, ident int) (string, string, string, bool) {
	expected := state["chat_id"]
	target := expected
	if target == "" {
		target = e.OpenChatID
	}
	if !a.isInteractiveOperatorAuthorized(e.OpenID, e.UserID, e.UnionID, target) {
		who := e.OpenID
		if who == "" {
			who = "<unknown>"
		}
		slog.Warn(fmt.Sprintf("[Feishu] Unauthorized %s click by %s", label, who))
		return "", "", "", false
	}
	if e.OpenChatID != "" && expected != "" && e.OpenChatID != expected {
		slog.Warn(fmt.Sprintf("[Feishu] %s callback chat mismatch for %d (expected=%s, got=%s)", pystr.Capitalize(label), ident, expected, e.OpenChatID))
		return "", "", "", false
	}
	userName := a.getCachedSenderName(e.OpenID)
	name := e.OpenID
	if userName != nil && *userName != "" {
		name = *userName
	}
	return e.OpenID, e.OpenChatID, name, true
}

// State id of a callback value, matching the int keys the prompt state is stored under
func stateIDOf(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case float64:
		if n == float64(int64(n)) {
			return int(n), true
		}
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func (a *FeishuAdapter) promptState(states map[int]map[string]string, id int) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return states[id]
}

// Schedules approval resolution and answers with the resolved card
func (a *FeishuAdapter) handleApprovalCardAction(e cardActionEvent, value *pyjson.Object, loop *adapterLoop) *callback.CardActionTriggerResponse {
	raw := dget(value, "approval_id")
	if raw == nil {
		slog.Debug("[Feishu] Card action missing approval_id, ignoring")
		return cardResponse(nil)
	}
	approvalID, ok := stateIDOf(raw)
	var state map[string]string
	if ok {
		state = a.promptState(a.approvalState, approvalID)
	}
	if len(state) == 0 {
		slog.Debug(fmt.Sprintf("[Feishu] Approval %s already resolved or unknown", pyrepr.Str(raw)))
		return cardResponse(nil)
	}
	choice, ok := approvalChoiceMap[pyrepr.Str(dget(value, "hermes_action"))]
	if !ok {
		choice = "deny"
	}
	openID, chatID, userName, ok := a.validateCardAction(e, state, "approval", approvalID)
	if !ok {
		return cardResponse(nil)
	}
	if !a.submitOnLoop(loop, func(ctx context.Context) error {
		return a.resolveApproval(ctx, approvalID, choice, userName, openID, chatID, e.UserID, e.UnionID)
	}) {
		return cardResponse(nil)
	}
	var body *string
	if b, has := state["body"]; has {
		body = &b
	}
	return cardResponse(buildResolvedApprovalCard(loop.ctx, choice, userName, body))
}

// Schedules update prompt resolution and answers with the answered card
func (a *FeishuAdapter) handleUpdatePromptCardAction(e cardActionEvent, value *pyjson.Object, loop *adapterLoop) *callback.CardActionTriggerResponse {
	raw := dget(value, "update_prompt_id")
	if raw == nil {
		slog.Debug("[Feishu] Card action missing update_prompt_id, ignoring")
		return cardResponse(nil)
	}
	promptID, ok := stateIDOf(raw)
	var state map[string]string
	if ok {
		state = a.promptState(a.updatePromptState, promptID)
	}
	if len(state) == 0 {
		slog.Debug(fmt.Sprintf("[Feishu] Update prompt %s already resolved or unknown", pyrepr.Str(raw)))
		return cardResponse(nil)
	}
	answer := pystr.Lower(pystr.Strip(strOr(dgetDef(value, "hermes_update_prompt_action", ""))))
	if answer != "y" && answer != "n" {
		slog.Debug("[Feishu] Card action has invalid update prompt answer=" + pyrepr.Repr(answer))
		return cardResponse(nil)
	}
	openID, chatID, userName, ok := a.validateCardAction(e, state, "update prompt", promptID)
	if !ok {
		return cardResponse(nil)
	}
	if !a.submitOnLoop(loop, func(ctx context.Context) error {
		return a.resolveUpdatePrompt(ctx, promptID, answer, userName, openID, chatID, e.UserID, e.UnionID)
	}) {
		return cardResponse(nil)
	}
	return cardResponse(buildResolvedUpdatePromptCard(loop.ctx, answer, userName))
}

// Revalidates a prompt click on the loop and pops its state, nil when it no longer applies
func (a *FeishuAdapter) popValidatedPromptState(states map[int]map[string]string, ident int, label, openID, chatID, unauthorizedFmt,
	operatorRepr, userID, unionID string) map[string]string {
	state := a.promptState(states, ident)
	if len(state) == 0 {
		slog.Debug(fmt.Sprintf("[Feishu] %s %d already resolved or unknown", label, ident))
		return nil
	}
	target := state["chat_id"]
	if target == "" {
		target = chatID
	}
	if !a.isInteractiveOperatorAuthorized(openID, userID, unionID, target) {
		slog.Warn(fmt.Sprintf(unauthorizedFmt, operatorRepr, ident))
		return nil
	}
	if expected := state["chat_id"]; expected != "" && chatID != "" && expected != chatID {
		slog.Warn(fmt.Sprintf("[Feishu] %s %d chat mismatch (expected=%s, got=%s)", label, ident, expected, chatID))
		return nil
	}
	a.mu.Lock()
	popped := states[ident]
	delete(states, ident)
	a.mu.Unlock()
	if len(popped) == 0 {
		slog.Debug(fmt.Sprintf("[Feishu] %s %d already resolved while validating callback", label, ident))
		return nil
	}
	return popped
}

// Pops approval state and unblocks the waiting agent, correcting the card when nothing was waiting
func (a *FeishuAdapter) resolveApproval(ctx context.Context, approvalID int, choice, userName, openID, chatID, userID, unionID string) error {
	who := openID
	if who == "" {
		who = "<unknown>"
	}
	state := a.popValidatedPromptState(a.approvalState, approvalID, "Approval", openID, chatID,
		"[Feishu] Unauthorized approval click by %s for approval %d", who, userID, unionID)
	if state == nil {
		return nil
	}
	count := resolveGatewayApprovalFn(state["session_key"], choice, false, "", "")
	slog.Info(fmt.Sprintf("Feishu button resolved %d approval(s) for session %s (choice=%s, user=%s)", count, state["session_key"], choice, userName))
	if count == 0 && choice != "deny" {
		target := state["chat_id"]
		if target == "" {
			target = chatID
		}
		if target != "" {
			if _, err := a.Send(ctx, target, i18n.T(ctx, "platform.shared.approval_expired", "", nil), "", nil); err != nil {
				slog.Debug("[Feishu] expired-approval notice failed")
			}
		}
	}
	return nil
}

// Persists an update prompt answer for the detached update process
func (a *FeishuAdapter) resolveUpdatePrompt(ctx context.Context, promptID int, answer, userName, openID, chatID, userID, unionID string) error {
	state := a.popValidatedPromptState(a.updatePromptState, promptID, "Update prompt", openID, chatID,
		"[Feishu] Unauthorized update prompt click by %s for prompt %d", openID, userID, unionID)
	if state == nil {
		return nil
	}
	if err := writeUpdatePromptResponse(ctx, answer); err != nil {
		slog.Error("Failed to resolve Feishu update prompt: " + err.Error())
		return nil
	}
	slog.Info(fmt.Sprintf("Feishu update prompt resolved for session %s (answer=%s, user=%s)", state["session_key"], answer, userName))
	return nil
}

// Emits a reaction on one of this bot's own messages as a synthetic text event
func (a *FeishuAdapter) handleReactionEvent(ctx context.Context, eventType string, e reactionEvent) error {
	client := a.currentClient()
	if client == nil || e.MessageID == "" {
		return nil
	}
	req := larkim.NewGetMessageReqBuilder().MessageId(e.MessageID).Build()
	var resp *larkim.GetMessageResp
	if err := a.runBlocking(ctx, func(ctx context.Context) error {
		var err error
		resp, err = client.GetMessage(ctx, req)
		return err
	}); err != nil {
		slog.Debug("[Feishu] Failed to fetch message for reaction routing")
		return nil
	}
	if !responseSucceeded(wrapResp(resp)) || resp.Data == nil || len(resp.Data.Items) == 0 || resp.Data.Items[0] == nil {
		return nil
	}
	msg := resp.Data.Items[0]
	if msg.Sender == nil || sdkStr(msg.Sender.Id) != a.settings.AppID {
		return nil
	}
	chatID := sdkStr(msg.ChatId)
	chatTypeRaw := "p2p"
	if chatID == "" {
		return nil
	}
	emoji := e.EmojiType
	if emoji == "" {
		emoji = "UNKNOWN"
	}
	action := "removed"
	if strings.Contains(eventType, "created") {
		action = "added"
	}
	slog.Info(fmt.Sprintf("[Feishu] Routing reaction %s:%s on bot message %s as synthetic event", action, emoji, e.MessageID))
	return a.dispatchSyntheticEvent(ctx, "reaction:"+action+":"+emoji, gateway.MessageTypeText, chatID, senderIDOf(e.UserID), chatTypeRaw, e.Raw, e.MessageID)
}

// Reports a card action token already handled inside the dedup window, pruning expired tokens
func (a *FeishuAdapter) isCardActionDuplicate(token string) bool {
	now := nowFn()
	a.mu.Lock()
	defer a.mu.Unlock()
	for t, ts := range a.cardActionTokens {
		if now-ts > feishuCardActionDedupTTLSeconds {
			delete(a.cardActionTokens, t)
		}
	}
	if _, ok := a.cardActionTokens[token]; ok {
		return true
	}
	a.cardActionTokens[token] = now
	return false
}

// Routes a card button click as a synthetic card command
func (a *FeishuAdapter) handleCardActionEvent(ctx context.Context, e cardActionEvent) error {
	if e.Token != "" && a.isCardActionDuplicate(e.Token) {
		slog.Debug("[Feishu] Dropping duplicate card action token: " + e.Token)
		return nil
	}
	if e.OpenChatID == "" || e.OpenID == "" {
		slog.Debug("[Feishu] Card action missing chat_id or operator open_id, dropping")
		return nil
	}
	tag := e.Tag
	if tag == "" {
		tag = "button"
	}
	text := "/card " + tag
	if pyrepr.Truthy(e.Value) {
		if dumped, err := pyjson.Dumps(e.Value, pyjson.EnsureASCII(false)); err == nil {
			text += " " + dumped
		}
	}
	slog.Info(fmt.Sprintf("[Feishu] Routing card action %s from %s in %s as synthetic command", pyrepr.Repr(tag), e.OpenID, e.OpenChatID))
	openID := e.OpenID
	messageID := e.Token
	if messageID == "" {
		messageID = uuid.NewString()
	}
	return a.dispatchSyntheticEvent(ctx, text, gateway.MessageTypeCommand, e.OpenChatID, SenderID{OpenID: &openID}, "group", e.Raw, messageID)
}

// Wraps a reaction or card click as a message event and runs it through the guarded pipeline
func (a *FeishuAdapter) dispatchSyntheticEvent(ctx context.Context, text string, messageType gateway.MessageType, chatID string, sender SenderID,
	eventChatType string, raw any, messageID string) error {
	profile, err := a.ResolveSenderProfile(ctx, sender, false)
	if err != nil {
		return err
	}
	info, _ := a.GetChatInfo(ctx, chatID)
	chatName := firstNonEmpty(pyrepr.Str(orEmpty(info["name"])), chatID, "Feishu Chat")
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatName: &chatName, ChatType: resolveSourceChatType(info, eventChatType),
		UserID: profile.UserID, UserName: profile.UserName, UserIDAlt: profile.UserIDAlt})
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage, event.MessageID = messageType, source, raw, &messageID
	event.ChannelPrompt = a.resolveChannelPrompt(chatID, "")
	event.Timestamp = time.Now()
	return a.HandleMessageWithGuards(ctx, event)
}

// Per chat lock for serial processing, LRU bounded and evicting a free lock first
func (a *FeishuAdapter) getChatLock(chatID string) *chatLock {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l, ok := a.chatLocks[chatID]; ok {
		a.chatLockOrder = moveToEnd(a.chatLockOrder, chatID)
		return l
	}
	if len(a.chatLocks) >= a.chatLockMaxSize && len(a.chatLockOrder) > 0 {
		victim := a.chatLockOrder[0]
		for _, k := range a.chatLockOrder {
			if !a.chatLocks[k].locked() {
				victim = k
				break
			}
		}
		delete(a.chatLocks, victim)
		a.chatLockOrder = removeKey(a.chatLockOrder, victim)
	}
	l := &chatLock{}
	a.chatLocks[chatID] = l
	a.chatLockOrder = append(a.chatLockOrder, chatID)
	return l
}

func moveToEnd(order []string, key string) []string { return append(removeKey(order, key), key) }

func removeKey(order []string, key string) []string {
	for i, k := range order {
		if k == key {
			return append(order[:i:i], order[i+1:]...)
		}
	}
	return order
}

// Runs one event through the agent pipeline under its chat's lock
func (a *FeishuAdapter) HandleMessageWithGuards(ctx context.Context, event *gateway.MessageEvent) error {
	chatID := ""
	if event.Source != nil {
		chatID = event.Source.ChatID
	}
	l := a.getChatLock(chatID)
	l.lock()
	defer l.unlock()
	return a.HandleMessage(ctx, event)
}

// Whether processing reactions are on for this profile
func (a *FeishuAdapter) reactionsEnabled(ctx context.Context) bool {
	v, _ := gateway.GetScopedSecret(ctx, "FEISHU_REACTIONS", "true", false)
	switch pystr.Lower(pystr.Strip(v)) {
	case "false", "0", "no":
		return false
	}
	return true
}

// Adds an emoji reaction and returns its reaction id, nil on failure
func (a *FeishuAdapter) addReaction(ctx context.Context, messageID, emojiType string) *string {
	client := a.currentClient()
	if client == nil || messageID == "" || emojiType == "" {
		return nil
	}
	emoji := emojiType
	req := larkim.NewCreateMessageReactionReqBuilder().MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().ReactionType(&larkim.Emoji{EmojiType: &emoji}).Build()).Build()
	var resp *larkim.CreateMessageReactionResp
	err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.CreateReaction(ctx, req)
		return e
	})
	if !a.reactionOutcome(err, wrapResp(resp), "Add", messageID, emojiType) {
		return nil
	}
	if resp.Data == nil {
		return nil
	}
	return resp.Data.ReactionId
}

// Removes a reaction by its id, reporting success
func (a *FeishuAdapter) removeReaction(ctx context.Context, messageID, reactionID string) bool {
	client := a.currentClient()
	if client == nil || messageID == "" || reactionID == "" {
		return false
	}
	req := larkim.NewDeleteMessageReactionReqBuilder().MessageId(messageID).ReactionId(reactionID).Build()
	var resp *larkim.DeleteMessageReactionResp
	err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.DeleteReaction(ctx, req)
		return e
	})
	return a.reactionOutcome(err, wrapResp(resp), "Remove", messageID, reactionID)
}

// Shared add and remove outcome, logging a rejection or a raised call
func (a *FeishuAdapter) reactionOutcome(err error, r *larkResp, verb, messageID, ident string) bool {
	if err != nil {
		slog.Warn(fmt.Sprintf("[Feishu] %s reaction %s on %s raised: %s", verb, ident, messageID, err))
		return false
	}
	if responseSucceeded(r) {
		return true
	}
	var code, msg any
	if r != nil {
		code, msg = r.code, r.msg
	}
	slog.Debug(fmt.Sprintf("[Feishu] %s reaction %s on %s rejected: code=%s msg=%s", verb, ident, messageID, pyrepr.Str(code), pyrepr.Str(msg)))
	return false
}

// Marks the start of processing with a Typing reaction whose id is kept for removal
func (a *FeishuAdapter) OnProcessingStart(ctx context.Context, event *gateway.MessageEvent) error {
	messageID := sdkStr(event.MessageID)
	a.mu.Lock()
	_, pending := a.processingReactions[messageID]
	a.mu.Unlock()
	if !a.reactionsEnabled(ctx) || messageID == "" || pending {
		return nil
	}
	reactionID := a.addReaction(ctx, messageID, feishuReactionInProgress)
	if reactionID == nil || *reactionID == "" {
		return nil
	}
	a.mu.Lock()
	a.processingReactions[messageID] = *reactionID
	a.processingReactionOrder = moveToEnd(a.processingReactionOrder, messageID)
	for len(a.processingReactionOrder) > feishuProcessingReactionCacheCap {
		delete(a.processingReactions, a.processingReactionOrder[0])
		a.processingReactionOrder = a.processingReactionOrder[1:]
	}
	a.mu.Unlock()
	return nil
}

// Removes the Typing reaction and marks a failure with a cross, keeping the handle when removal fails
func (a *FeishuAdapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	messageID := sdkStr(event.MessageID)
	if !a.reactionsEnabled(ctx) || messageID == "" {
		return nil
	}
	a.mu.Lock()
	start := a.processingReactions[messageID]
	a.mu.Unlock()
	if start != "" {
		if !a.removeReaction(ctx, messageID, start) {
			return nil
		}
		a.mu.Lock()
		delete(a.processingReactions, messageID)
		a.processingReactionOrder = removeKey(a.processingReactionOrder, messageID)
		a.mu.Unlock()
	}
	if outcome == gateway.ProcessingFailure {
		a.addReaction(ctx, messageID, feishuReactionFailure)
	}
	return nil
}

// Channel prompt configured for a chat or its parent under channel_prompts
func (a *FeishuAdapter) resolveChannelPrompt(chatID, parentID string) *string {
	var extra map[string]any
	if a.Config != nil {
		extra = a.Config.Extra
	}
	if p, ok := gateway.ResolveChannelPrompt(extra, chatID, parentID); ok {
		return &p
	}
	return nil
}

// Normalizes an admitted message into a gateway event with reply context, sender profile and chat info
func (a *FeishuAdapter) processInboundMessage(ctx context.Context, data *larkim.P2MessageReceiveV1, message *larkim.EventMessage, sender SenderID,
	chatType, messageID string, isBot bool) error {
	content := a.extractMessageContent(ctx, message)
	text, inboundType := content.text, content.inboundType
	if inboundType == gateway.MessageTypeText {
		text = stripEdgeSelfMentions(text, content.mentions)
		if strings.HasPrefix(text, "/") {
			inboundType = gateway.MessageTypeCommand
		}
	}
	if inboundType == gateway.MessageTypeText && text == "" && len(content.mediaURLs) == 0 {
		slog.Debug("[Feishu] Ignoring empty text message id=" + messageID)
		return nil
	}
	if inboundType != gateway.MessageTypeCommand {
		if hint := buildMentionHint(content.mentions); hint != "" {
			if text != "" {
				text = hint + "\n\n" + text
			} else {
				text = hint
			}
		}
	}
	threadID := nonEmpty(message.ThreadId)
	replyTo := nonEmpty(message.ParentId)
	if replyTo == nil {
		replyTo = nonEmpty(message.RootId)
	}
	var replyText *string
	if replyTo != nil {
		replyText = a.fetchMessageText(ctx, *replyTo)
	}
	primary := "<unknown>"
	for _, id := range []*string{sender.OpenID, sender.UserID, sender.UnionID} {
		if id != nil && *id != "" {
			primary = *id
			break
		}
	}
	chatID := sdkStr(message.ChatId)
	kind, who := "group", "user"
	if chatType == "p2p" {
		kind = "dm"
	}
	if isBot {
		who = "bot"
	}
	slog.Info(fmt.Sprintf("[Feishu] Inbound %s message received: id=%s type=%s chat_id=%s sender=%s:%s text=%s media=%d", kind, messageID,
		string(inboundType), chatID, who, primary, pyrepr.Repr(pystr.Slice(text, 0, 120)), len(content.mediaURLs)))
	info, _ := a.GetChatInfo(ctx, chatID)
	profile, err := a.ResolveSenderProfile(ctx, sender, isBot)
	if err != nil {
		return err
	}
	chatName := firstNonEmpty(pyrepr.Str(orEmpty(info["name"])), chatID, "Feishu Chat")
	msgID := messageID
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatName: &chatName, ChatType: resolveSourceChatType(info, chatType),
		UserID: profile.UserID, UserName: profile.UserName, ThreadID: threadID, UserIDAlt: profile.UserIDAlt, IsBot: isBot, MessageID: &msgID})
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage, event.MessageID = inboundType, source, data, &msgID
	event.MediaURLs, event.MediaTypes, event.MediaTextInlined = content.mediaURLs, content.mediaTypes, content.inlined
	event.ReplyToMessageID, event.ReplyToText = replyTo, replyText
	parent := ""
	if threadID != nil {
		parent = *threadID
	}
	event.ChannelPrompt = a.resolveChannelPrompt(chatID, parent)
	event.Timestamp = time.Now()
	return a.dispatchInboundEvent(ctx, event)
}

// Text bursts go through the debounce, media through the media batch, everything else straight to the guarded pipeline
func (a *FeishuAdapter) dispatchInboundEvent(ctx context.Context, event *gateway.MessageEvent) error {
	if event.MessageType == gateway.MessageTypeText && !event.IsCommand() {
		return a.EnqueueTextEvent(ctx, event)
	}
	if shouldBatchMediaEvent(event) {
		return a.enqueueMediaEvent(ctx, event)
	}
	return a.HandleMessageWithGuards(ctx, event)
}

func shouldBatchMediaEvent(event *gateway.MessageEvent) bool {
	switch event.MessageType {
	case gateway.MessageTypePhoto, gateway.MessageTypeVideo, gateway.MessageTypeDocument, gateway.MessageTypeAudio:
		return len(event.MediaURLs) > 0
	}
	return false
}

func (a *FeishuAdapter) mediaBatchKey(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	key, err := a.TextBatchKey(ctx, event)
	if err != nil {
		return "", err
	}
	return key + ":media:" + string(event.MessageType), nil
}

func mediaBatchIsCompatible(existing, incoming *gateway.MessageEvent) bool {
	return existing.MessageType == incoming.MessageType && textBatchIsCompatible(existing, incoming)
}

// Merges a media event into its pending batch, flushing first when the reply or thread context differs
func (a *FeishuAdapter) enqueueMediaEvent(ctx context.Context, event *gateway.MessageEvent) error {
	key, err := a.mediaBatchKey(ctx, event)
	if err != nil {
		return err
	}
	a.mu.Lock()
	existing := a.mediaBatches[key]
	if existing == nil {
		a.mediaBatches[key] = event
		a.mu.Unlock()
		a.scheduleMediaBatchFlush(ctx, key)
		return nil
	}
	a.mu.Unlock()
	if !mediaBatchIsCompatible(existing, event) {
		if err := a.flushMediaBatchNow(ctx, key); err != nil {
			return err
		}
		a.mu.Lock()
		a.mediaBatches[key] = event
		a.mu.Unlock()
		a.scheduleMediaBatchFlush(ctx, key)
		return nil
	}
	a.mu.Lock()
	existing.MediaURLs = append(existing.MediaURLs, event.MediaURLs...)
	existing.MediaTypes = append(existing.MediaTypes, event.MediaTypes...)
	existing.MediaTextInlined = append(existing.MediaTextInlined, event.MediaTextInlined...)
	if event.Text != "" {
		existing.Text = gateway.MergeCaption(existing.Text, event.Text)
	}
	existing.Timestamp = event.Timestamp
	if sdkStr(event.MessageID) != "" {
		existing.MessageID = event.MessageID
		if existing.Source != nil {
			existing.Source.MessageID = event.MessageID
		}
	}
	a.mu.Unlock()
	a.scheduleMediaBatchFlush(ctx, key)
	return nil
}

// Restarts the media batch timer of key
func (a *FeishuAdapter) scheduleMediaBatchFlush(ctx context.Context, key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if prior := a.mediaBatchTasks[key]; prior != nil && !prior.Done() {
		prior.Cancel()
	}
	var task *gateway.AdapterTask
	task = gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		defer func() {
			a.mu.Lock()
			if a.mediaBatchTasks[key] == gateway.CurrentAdapterTask(tctx) {
				delete(a.mediaBatchTasks, key)
			}
			a.mu.Unlock()
		}()
		if !sleepCtx(tctx, time.Duration(a.settings.MediaBatchDelaySeconds*float64(time.Second))) {
			return nil
		}
		return a.flushMediaBatchNow(tctx, key)
	})
	a.mediaBatchTasks[key] = task
}

// Dispatches the pending media batch of key at once
func (a *FeishuAdapter) flushMediaBatchNow(ctx context.Context, key string) error {
	a.mu.Lock()
	event := a.mediaBatches[key]
	delete(a.mediaBatches, key)
	a.mu.Unlock()
	if event == nil {
		return nil
	}
	slog.Info(fmt.Sprintf("[Feishu] Flushing media batch %s with %d attachment(s)", key, len(event.MediaURLs)))
	return a.HandleMessageWithGuards(ctx, event)
}

// Text events merge only when their reply and thread context is identical
func textBatchIsCompatible(existing, incoming *gateway.MessageEvent) bool {
	var et, it *string
	if existing.Source != nil {
		et = existing.Source.ThreadID
	}
	if incoming.Source != nil {
		it = incoming.Source.ThreadID
	}
	return optEqual(existing.ReplyToMessageID, incoming.ReplyToMessageID) && optEqual(existing.ReplyToText, incoming.ReplyToText) && optEqual(et, it)
}

func optEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Debounces rapid text bursts into one event, flushing early on context changes or the size caps
func (a *FeishuAdapter) EnqueueTextEvent(ctx context.Context, event *gateway.MessageEvent) error {
	key, err := a.TextBatchKey(ctx, event)
	if err != nil {
		return err
	}
	chunkLen := utf8.RuneCountInString(event.Text)
	start := func() {
		a.SetPendingTextBatch(key, event)
		a.mu.Lock()
		a.textBatchCounts[key] = 1
		a.mu.Unlock()
		a.RescheduleTextBatchFlush(ctx, key)
	}
	existing := a.PendingTextBatch(key)
	if existing == nil {
		event.SetLastChunkLen(chunkLen)
		start()
		return nil
	}
	if !textBatchIsCompatible(existing, event) {
		if err := a.FlushTextBatchNow(ctx, key); err != nil {
			return err
		}
		start()
		return nil
	}
	a.mu.Lock()
	count, ok := a.textBatchCounts[key]
	if !ok {
		count = 1
	}
	a.mu.Unlock()
	next := count + 1
	nextText := existing.Text
	switch {
	case existing.Text != "" && event.Text != "":
		nextText = existing.Text + "\n" + event.Text
	case existing.Text == "":
		nextText = event.Text
	}
	if next > a.settings.TextBatchMaxMessages || utf8.RuneCountInString(nextText) > a.settings.TextBatchMaxChars {
		if err := a.FlushTextBatchNow(ctx, key); err != nil {
			return err
		}
		start()
		return nil
	}
	existing.Text = nextText
	existing.MediaURLs = append(existing.MediaURLs, event.MediaURLs...)
	existing.MediaTypes = append(existing.MediaTypes, event.MediaTypes...)
	existing.MediaTextInlined = append(existing.MediaTextInlined, event.MediaTextInlined...)
	existing.SetLastChunkLen(chunkLen)
	existing.Timestamp = event.Timestamp
	if sdkStr(event.MessageID) != "" {
		existing.MessageID = event.MessageID
		if existing.Source != nil {
			existing.Source.MessageID = event.MessageID
		}
	}
	a.mu.Lock()
	a.textBatchCounts[key] = next
	a.mu.Unlock()
	a.RescheduleTextBatchFlush(ctx, key)
	return nil
}

// Pops the pending text batch of key together with its message count
func (a *FeishuAdapter) PopTextBatch(key string) *gateway.MessageEvent {
	a.mu.Lock()
	delete(a.textBatchCounts, key)
	a.mu.Unlock()
	return a.BasePlatformAdapter.PopTextBatch(key)
}

// Hands a flushed text batch to the guarded pipeline
func (a *FeishuAdapter) DispatchTextBatch(ctx context.Context, event *gateway.MessageEvent) error {
	return a.HandleMessageWithGuards(ctx, event)
}

func (a *FeishuAdapter) normalize(messageType, rawContent string, mentions []rawMention) FeishuNormalizedMessage {
	return normalizeFeishuMessage(messageType, rawContent, mentions, a.botIdentity())
}

type extractedContent struct {
	text        string
	inboundType gateway.MessageType
	mediaURLs   []string
	mediaTypes  []string
	inlined     []*bool
	mentions    []FeishuMentionRef
}

// Normalized text, downloaded media and inlined text documents of an inbound message
func (a *FeishuAdapter) extractMessageContent(ctx context.Context, message *larkim.EventMessage) extractedContent {
	rawContent, rawType, messageID := sdkStr(message.Content), sdkStr(message.MessageType), sdkStr(message.MessageId)
	slog.Info(fmt.Sprintf("[Feishu] Received raw message type=%s message_id=%s", rawType, messageID))
	normalized := a.normalize(rawType, rawContent, mentionsFromEvent(message.Mentions))
	urls, types := a.downloadFeishuMessageResources(ctx, messageID, normalized)
	out := extractedContent{text: normalized.TextContent, inboundType: a.resolveNormalizedMessageType(normalized, types), mediaURLs: urls,
		mediaTypes: types, inlined: []*bool{}, mentions: normalized.Mentions}
	var parts []string
	for i := range urls {
		extracted := a.maybeExtractTextDocument(urls[i], types[i])
		flag := extracted != ""
		out.inlined = append(out.inlined, &flag)
		if extracted != "" {
			parts = append(parts, extracted)
		}
	}
	if len(parts) > 0 {
		joined := strings.Join(parts, "\n\n")
		if out.text != "" {
			out.text = out.text + "\n\n" + joined
		} else {
			out.text = joined
		}
	}
	return out
}

// Cached paths and media types of every image and media resource a message references
func (a *FeishuAdapter) downloadFeishuMessageResources(ctx context.Context, messageID string, n FeishuNormalizedMessage) ([]string, []string) {
	urls, types := []string{}, []string{}
	collect := func(path, mediaType string) {
		if path != "" {
			urls = append(urls, path)
			types = append(types, mediaType)
		}
	}
	for _, key := range n.ImageKeys {
		collect(a.downloadFeishuImage(ctx, messageID, key))
	}
	for _, ref := range n.MediaRefs {
		collect(a.downloadFeishuMessageResource(ctx, messageID, ref.FileKey, ref.ResourceType, ref.FileName))
	}
	return urls, types
}

// Photo, audio or video type of a media type, def otherwise
func resolveMediaMessageType(mediaType string, def gateway.MessageType) gateway.MessageType {
	normalized := pystr.Lower(mediaType)
	switch {
	case strings.HasPrefix(normalized, "image/"):
		return gateway.MessageTypePhoto
	case strings.HasPrefix(normalized, "audio/"):
		return gateway.MessageTypeAudio
	case strings.HasPrefix(normalized, "video/"):
		return gateway.MessageTypeVideo
	}
	return def
}

// Voice for native audio recordings so they get transcribed, the media type deciding photos and documents
func (a *FeishuAdapter) resolveNormalizedMessageType(n FeishuNormalizedMessage, mediaTypes []string) gateway.MessageType {
	switch n.PreferredMessageType {
	case "audio":
		return gateway.MessageTypeVoice
	case "photo", "document":
		def := gateway.MessageTypeDocument
		if n.PreferredMessageType == "photo" {
			def = gateway.MessageTypePhoto
		}
		first := ""
		if len(mediaTypes) > 0 {
			first = mediaTypes[0]
		}
		return resolveMediaMessageType(first, def)
	}
	return gateway.MessageTypeText
}

// Contents of a small text or markdown attachment under a content heading, empty otherwise
func (a *FeishuAdapter) maybeExtractTextDocument(cachedPath, mediaType string) string {
	if cachedPath == "" || !strings.HasPrefix(mediaType, "text/") {
		return ""
	}
	fail := func() string {
		slog.Warn("[Feishu] Failed to inject text document content from " + cachedPath)
		return ""
	}
	st, err := os.Stat(cachedPath)
	if err != nil {
		return fail()
	}
	if st.Size() > maxTextInjectBytes {
		return ""
	}
	ext := strings.ToLower(pypath.Suffix(cachedPath))
	if ext != ".txt" && ext != ".md" && mediaType != "text/plain" && mediaType != "text/markdown" {
		return ""
	}
	raw, err := os.ReadFile(cachedPath)
	if err != nil {
		return fail()
	}
	content, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return fail()
	}
	return "[Content of " + displayNameFromCachedPath(cachedPath) + "]:\n" + content
}

// Fetches one message resource of the given type
func (a *FeishuAdapter) fetchMessageResource(ctx context.Context, messageID, fileKey, resourceType string) (*larkim.GetMessageResourceResp, error) {
	client := a.currentClient()
	req := larkim.NewGetMessageResourceReqBuilder().MessageId(messageID).FileKey(fileKey).Type(resourceType).Build()
	var resp *larkim.GetMessageResourceResp
	err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.GetMessageResource(ctx, req)
		return e
	})
	return resp, err
}

// Downloads and caches an inbound image, returning its path and media type
func (a *FeishuAdapter) downloadFeishuImage(ctx context.Context, messageID, imageKey string) (string, string) {
	if a.currentClient() == nil || messageID == "" {
		return "", ""
	}
	resp, err := a.fetchMessageResource(ctx, messageID, imageKey, "image")
	if err != nil {
		slog.Warn("[Feishu] Failed to cache image resource " + imageKey)
		return "", ""
	}
	r := wrapResp(resp)
	if !responseSucceeded(r) {
		slog.Warn(fmt.Sprintf("[Feishu] Failed to download image %s: %s %s", imageKey, pyrepr.Str(respCode(r)), respMsg(r, "request failed")))
		return "", ""
	}
	data := readBinaryResponse(resp.File)
	if len(data) == 0 {
		return "", ""
	}
	contentType := r.header("Content-Type")
	filename := resp.FileName
	if filename == "" {
		filename = imageKey + ".jpg"
	}
	ext := guessExtension(filename, contentType, ".jpg", imageExtensions)
	path, err := gateway.CacheImageFromBytesAsync(ctx, data, ext)
	if err != nil {
		slog.Warn("[Feishu] Failed to cache image resource " + imageKey)
		return "", ""
	}
	return path, normalizeMediaType(contentType, defaultImageMediaType(ext))
}

// Downloads and caches an inbound file, audio or video resource, retrying audio and media as file
func (a *FeishuAdapter) downloadFeishuMessageResource(ctx context.Context, messageID, fileKey, resourceType, fallbackFilename string) (string, string) {
	if a.currentClient() == nil || messageID == "" {
		return "", ""
	}
	requestTypes := []string{resourceType}
	if resourceType == "audio" || resourceType == "media" {
		requestTypes = append(requestTypes, "file")
	}
	for _, requestType := range requestTypes {
		path, mediaType, ok := a.downloadResourceAs(ctx, messageID, fileKey, requestType, fallbackFilename)
		if ok {
			return path, mediaType
		}
	}
	return "", ""
}

func (a *FeishuAdapter) downloadResourceAs(ctx context.Context, messageID, fileKey, requestType, fallbackFilename string) (string, string, bool) {
	failed := func() (string, string, bool) {
		slog.Warn(fmt.Sprintf("[Feishu] Failed to cache message resource %s/%s", messageID, fileKey))
		return "", "", false
	}
	resp, err := a.fetchMessageResource(ctx, messageID, fileKey, requestType)
	if err != nil {
		return failed()
	}
	r := wrapResp(resp)
	if !responseSucceeded(r) {
		slog.Debug(fmt.Sprintf("[Feishu] Resource download failed for %s/%s via type=%s: %s %s", messageID, fileKey, requestType,
			pyrepr.Str(respCode(r)), respMsg(r, "request failed")))
		return "", "", false
	}
	data := readBinaryResponse(resp.File)
	if len(data) == 0 {
		return "", "", false
	}
	contentType := r.header("Content-Type")
	filename := firstNonEmpty(resp.FileName, fallbackFilename, requestType+"_"+fileKey)
	mediaType := normalizeMediaType(contentType, guessMediaTypeFromFilename(filename))
	var kind, path string
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		ext := guessExtension(filename, contentType, ".jpg", imageExtensions)
		kind = "image"
		if path, err = gateway.CacheImageFromBytesAsync(ctx, data, ext); err != nil {
			return failed()
		}
		if mediaType == "" {
			mediaType = defaultImageMediaType(ext)
		}
	case requestType == "audio" || strings.HasPrefix(mediaType, "audio/"):
		ext := guessExtension(filename, contentType, ".ogg", audioExtensions)
		kind = "audio"
		if path, err = gateway.CacheAudioFromBytesAsync(ctx, data, ext); err != nil {
			return failed()
		}
		if mediaType == "" {
			mediaType = "audio/" + firstNonEmpty(strings.TrimLeft(ext, "."), "ogg")
		}
	case strings.HasPrefix(mediaType, "video/"):
		if pypath.Suffix(filename) == "" {
			filename += ".mp4"
		}
		kind = "video"
		if path, err = gateway.CacheDocumentFromBytesAsync(ctx, data, filename); err != nil {
			return failed()
		}
	default:
		if ext, ok := gateway.DocumentMimeToExt()[mediaType]; ok && pypath.Suffix(filename) == "" {
			filename += ext
		}
		kind = "document"
		if path, err = gateway.CacheDocumentFromBytesAsync(ctx, data, filename); err != nil {
			return failed()
		}
		if mediaType == "" {
			mediaType = guessDocumentMediaType(filename)
		}
	}
	slog.Info(fmt.Sprintf("[Feishu] Cached message %s resource at %s", kind, path))
	return path, mediaType, true
}

// Allowed extension of the file name, else of the content type, else the default
func guessExtension(filename, contentType, def string, allowed map[string]bool) string {
	ext := strings.ToLower(pypath.Suffix(filename))
	if allowed[ext] {
		return ext
	}
	guessed := pymimetypes.GuessExtension(normalizeMediaType(contentType, ""), true)
	if allowed[guessed] {
		return guessed
	}
	return def
}

// Content type cut at its first semicolon, stripped and lowercased, def when empty
func normalizeMediaType(contentType, def string) string {
	normalized := pystr.Lower(pystr.Strip(strings.SplitN(contentType, ";", 2)[0]))
	if normalized == "" {
		return def
	}
	return normalized
}

// Supported document type of a file name, its guessed type, or octet stream
func guessDocumentMediaType(filename string) string {
	ext := strings.ToLower(pypath.Suffix(filename))
	if t, ok := mediacache.SupportedDocumentTypes[ext]; ok {
		return t
	}
	if t, _, _ := pymimetypes.GuessType(filename, true); t != "" {
		return t
	}
	return "application/octet-stream"
}

// Original file name of a cached path with characters outside the safe set replaced
func displayNameFromCachedPath(path string) string {
	base := filepath.Base(path)
	parts := strings.SplitN(base, "_", 3)
	name := base
	if len(parts) >= 3 {
		name = parts[2]
	}
	return reSub(displayNameSanitizeRe, name, func(regexp2.Match) string { return "_" })
}

// Media type of a file name from its guessed type or its known media extension
func guessMediaTypeFromFilename(filename string) string {
	if t, _, _ := pymimetypes.GuessType(filename, true); t != "" {
		return pystr.Lower(t)
	}
	ext := strings.ToLower(pypath.Suffix(filename))
	switch {
	case videoExtensions[ext]:
		return "video/" + strings.TrimLeft(ext, ".")
	case audioExtensions[ext]:
		return "audio/" + strings.TrimLeft(ext, ".")
	case imageExtensions[ext]:
		return defaultImageMediaType(ext)
	}
	return ""
}

// Gateway chat type of a Feishu chat type, forums for topic and thread chats
func mapChatType(raw string) string {
	normalized := pystr.Lower(pystr.Strip(raw))
	if normalized == "p2p" {
		return "dm"
	}
	for _, marker := range []string{"topic", "thread", "forum"} {
		if strings.Contains(normalized, marker) {
			return "forum"
		}
	}
	if normalized == "group" {
		return "group"
	}
	return "dm"
}

// Group or forum from the chat info, otherwise dm for p2p events and group for the rest
func resolveSourceChatType(info map[string]any, eventChatType string) string {
	resolved := pystr.Lower(pystr.Strip(strOr(info["type"])))
	if resolved == "group" || resolved == "forum" {
		return resolved
	}
	if eventChatType == "p2p" {
		return "dm"
	}
	return "group"
}

// Session source ids of a sender, the tenant user id first, union id as the alternate, and the API name
func (a *FeishuAdapter) ResolveSenderProfile(ctx context.Context, sender SenderID, isBot bool) (SenderProfile, error) {
	openID, userID, unionID := nonEmpty(sender.OpenID), nonEmpty(sender.UserID), nonEmpty(sender.UnionID)
	primary := userID
	if primary == nil {
		primary = openID
	}
	lookup := openID
	if !isBot {
		lookup = primary
		if lookup == nil {
			lookup = unionID
		}
	}
	name := a.resolveSenderNameFromAPI(ctx, sdkStr(lookup), isBot)
	return SenderProfile{UserID: primary, UserName: name, UserIDAlt: unionID}, nil
}

// Cached sender name while its TTL holds, nil when absent or expired
func (a *FeishuAdapter) getCachedSenderName(senderID string) *string {
	if senderID == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.senderNameCache[senderID]
	if !ok {
		return nil
	}
	if nowFn() < entry.expireAt {
		name := entry.name
		return &name
	}
	delete(a.senderNameCache, senderID)
	return nil
}

// Display name of a sender from the bot or contact API, cached, nil on any failure
func (a *FeishuAdapter) resolveSenderNameFromAPI(ctx context.Context, senderID string, isBot bool) *string {
	client := a.currentClient()
	trimmed := ""
	if senderID != "" && client != nil {
		trimmed = pystr.Strip(senderID)
	}
	if trimmed == "" {
		return nil
	}
	now := nowFn()
	if cached := a.getCachedSenderName(trimmed); cached != nil {
		if *cached == "" {
			return nil
		}
		return cached
	}
	if isBot {
		names := a.fetchBotNames(ctx, []string{trimmed})
		if names == nil {
			return nil
		}
		a.mu.Lock()
		for _, oid := range names.Keys() {
			n, _ := names.Get(oid)
			a.senderNameCache[oid] = nameEntry{name: n, expireAt: now + feishuSenderNameTTLSeconds}
		}
		hit, ok := a.senderNameCache[trimmed]
		a.mu.Unlock()
		if !ok || hit.name == "" {
			return nil
		}
		return &hit.name
	}
	idType := "user_id"
	if strings.HasPrefix(trimmed, "ou_") {
		idType = "open_id"
	} else if strings.HasPrefix(trimmed, "on_") {
		idType = "union_id"
	}
	req := larkcontact.NewGetUserReqBuilder().UserId(trimmed).UserIdType(idType).Build()
	var resp *larkcontact.GetUserResp
	if err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.GetUser(ctx, req)
		return e
	}); err != nil {
		slog.Debug("[Feishu] Failed to resolve sender name for " + senderID)
		return nil
	}
	if !responseSucceeded(wrapResp(resp)) || resp.Data == nil || resp.Data.User == nil {
		return nil
	}
	u := resp.Data.User
	name := firstNonEmpty(sdkStr(u.Name), sdkStr(u.Nickname), sdkStr(u.EnName))
	if pystr.Strip(name) == "" {
		return nil
	}
	name = pystr.Strip(name)
	a.mu.Lock()
	a.senderNameCache[trimmed] = nameEntry{name: name, expireAt: now + feishuSenderNameTTLSeconds}
	a.mu.Unlock()
	return &name
}

// Raw body of a tenant token GET, nil when the call fails
func (a *FeishuAdapter) tenantGetRaw(ctx context.Context, uri string, queries [][2]string) ([]byte, error) {
	client := a.currentClient()
	var body []byte
	err := a.runBlocking(ctx, func(ctx context.Context) error {
		resp, e := client.Do(ctx, tenantGetRequest(uri, queries))
		if resp != nil {
			body = resp.RawBody
		}
		return e
	})
	return body, err
}

// Names of bots by open id from the basic batch API, nil on failure
func (a *FeishuAdapter) fetchBotNames(ctx context.Context, botIDs []string) *pydict.Dict[string, string] {
	if a.currentClient() == nil || len(botIDs) == 0 {
		return nil
	}
	fail := func() *pydict.Dict[string, string] {
		slog.Debug(fmt.Sprintf("[Feishu] Failed to fetch bot names for %s", pyrepr.Repr(botIDs)))
		return nil
	}
	queries := make([][2]string, 0, len(botIDs))
	for _, oid := range botIDs {
		queries = append(queries, [2]string{"bot_ids", oid})
	}
	content, err := a.tenantGetRaw(ctx, "/open-apis/bot/v3/bots/basic_batch", queries)
	if err != nil {
		return fail()
	}
	if len(content) == 0 {
		return nil
	}
	payload, err := pyjson.Loads(string(content))
	if err != nil {
		return fail()
	}
	root, ok := payload.(*pyjson.Object)
	if !ok {
		return fail()
	}
	if !numberIs(dget(root, "code"), 0) {
		return nil
	}
	data := dget(root, "data")
	if !pyrepr.Truthy(data) {
		data = pyjson.NewObject()
	}
	dataObj, ok := data.(*pyjson.Object)
	if !ok {
		return fail()
	}
	bots := dget(dataObj, "bots")
	if !pyrepr.Truthy(bots) {
		bots = pyjson.NewObject()
	}
	botsObj, ok := bots.(*pyjson.Object)
	if !ok {
		return fail()
	}
	out := pydict.New[string, string]()
	for _, oid := range botsObj.Keys {
		info, _ := botsObj.Get(oid)
		infoObj, ok := info.(*pyjson.Object)
		if !ok {
			return fail()
		}
		if oid != "" {
			out.Set(oid, pystr.Strip(strOr(dget(infoObj, "name"))))
		}
	}
	return out
}

// Text of a replied to message for reply context, cached in a bounded LRU including misses
func (a *FeishuAdapter) fetchMessageText(ctx context.Context, messageID string) *string {
	client := a.currentClient()
	if client == nil || messageID == "" {
		return nil
	}
	a.mu.Lock()
	if cached, ok := a.messageTextCache[messageID]; ok {
		a.messageTextOrder = moveToEnd(a.messageTextOrder, messageID)
		a.mu.Unlock()
		return cached
	}
	a.mu.Unlock()
	req := larkim.NewGetMessageReqBuilder().MessageId(messageID).Build()
	var resp *larkim.GetMessageResp
	if err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.GetMessage(ctx, req)
		return e
	}); err != nil {
		slog.Warn("[Feishu] Failed to fetch parent message " + messageID)
		return nil
	}
	r := wrapResp(resp)
	if !responseSucceeded(r) {
		slog.Warn(fmt.Sprintf("[Feishu] Failed to fetch parent message %s: [%s] %s", messageID, pyrepr.Str(respCode(r)), respMsg(r, "message lookup failed")))
		return nil
	}
	var msgType, rawContent string
	var mentions []rawMention
	if resp.Data != nil && len(resp.Data.Items) > 0 && resp.Data.Items[0] != nil {
		parent := resp.Data.Items[0]
		msgType = sdkStr(parent.MsgType)
		if parent.Body != nil {
			rawContent = sdkStr(parent.Body.Content)
		}
		mentions = mentionsFromMessage(parent.Mentions)
	}
	text := a.extractTextFromRawContent(msgType, rawContent, mentions)
	a.mu.Lock()
	a.messageTextCache[messageID] = text
	a.messageTextOrder = moveToEnd(a.messageTextOrder, messageID)
	for len(a.messageTextOrder) > feishuMessageTextCacheSize {
		delete(a.messageTextCache, a.messageTextOrder[0])
		a.messageTextOrder = a.messageTextOrder[1:]
	}
	a.mu.Unlock()
	return text
}

// Normalized text of raw content, or its attachment label when the text is empty
func (a *FeishuAdapter) extractTextFromRawContent(msgType, rawContent string, mentions []rawMention) *string {
	n := a.normalize(msgType, rawContent, mentions)
	if n.TextContent != "" {
		t := n.TextContent
		return &t
	}
	label := pystr.Strip(pyrepr.Str(n.Metadata["placeholder_text"]))
	if label == "" {
		return nil
	}
	return &label
}

// Image media type of an extension, jpeg for jpg and empty extensions
func defaultImageMediaType(ext string) string {
	normalized := pystr.Lower(ext)
	if normalized == ".jpg" || normalized == ".jpeg" {
		return "image/jpeg"
	}
	return "image/" + firstNonEmpty(strings.TrimLeft(normalized, "."), "jpeg")
}

// Reject reason of an inbound message by self echo, bot policy, DM allowlist and group policy, empty when admitted
func (a *FeishuAdapter) admit(sender *larkim.EventSender, message *larkim.EventMessage) string {
	senderIDs := senderIdentity(sender)
	a.mu.Lock()
	selfIDs := map[string]bool{}
	for _, v := range []string{a.botOpenID, a.settings.BotUserID} {
		if v != "" {
			selfIDs[v] = true
		}
	}
	a.mu.Unlock()
	isBot := isBotSender(sender)
	isGroup := sdkStr(message.ChatType) != "p2p"
	chatID := sdkStr(message.ChatId)
	requireMention := isGroup && a.requireMentionFor(chatID)
	if len(selfIDs) > 0 && intersects(senderIDs, selfIDs) {
		return "self_echo"
	}
	if isBot {
		mode := a.settings.AllowBots
		if mode != "mentions" && mode != "all" {
			return "bots_disabled"
		}
		if len(selfIDs) == 0 || len(senderIDs) == 0 {
			return "self_ids_unknown"
		}
		if mode == "mentions" && !requireMention && !a.mentionsSelf(message) {
			return "bot_not_mentioned"
		}
	}
	if !isGroup {
		if a.settings.AllowAllDM || len(a.allowedGroupUsers) == 0 {
			return ""
		}
		if intersects(senderIDs, a.allowedGroupUsers) {
			return ""
		}
		return "dm_policy_rejected"
	}
	var sid *larkim.UserId
	if sender != nil {
		sid = sender.SenderId
	}
	if !a.allowGroupMessage(sid, chatID, isBot) {
		return "group_policy_rejected"
	}
	if requireMention && !a.mentionsSelf(message) {
		return "group_policy_rejected"
	}
	return ""
}

func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// One warning when group traffic dies on the untouched empty allowlist default
func (a *FeishuAdapter) warnOnceEmptyAllowlistDeny(chatID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.warnedEmptyAllowlistDeny {
		return
	}
	rules := make(map[string]any, len(a.settings.GroupRules))
	for k, v := range a.settings.GroupRules {
		rules[k] = v
	}
	users := make([]string, 0, len(a.allowedGroupUsers))
	for u := range a.allowedGroupUsers {
		users = append(users, u)
	}
	if text, ok := emptyAllowlistDropWarning(chatID, rules, a.settings.DefaultGroupPolicy, users); ok {
		a.warnedEmptyAllowlistDeny = true
		slog.Warn(text)
	}
}

// Group rule mention requirement when set, the global requirement otherwise
func (a *FeishuAdapter) requireMentionFor(chatID string) bool {
	if chatID != "" {
		if rule := a.settings.GroupRules[chatID]; rule != nil && rule.RequireMention != nil {
			return *rule.RequireMention
		}
	}
	return a.settings.RequireMention
}

// Per group policy gate, admins always passing and bots skipping the user lists
func (a *FeishuAdapter) allowGroupMessage(sender *larkim.UserId, chatID string, isBot bool) bool {
	ids := map[string]bool{}
	if sender != nil {
		for _, p := range []*string{sender.OpenId, sender.UserId} {
			if p != nil {
				ids[*p] = true
			}
		}
	}
	if len(ids) > 0 && len(a.admins) > 0 && intersects(ids, a.admins) {
		return true
	}
	var policy string
	var allowlist, blacklist map[string]bool
	if rule := a.settings.GroupRules[chatID]; chatID != "" && rule != nil {
		policy, allowlist, blacklist = rule.Policy, rule.Allowlist, rule.Blacklist
	} else {
		policy = a.settings.DefaultGroupPolicy
		if policy == "" {
			policy = a.settings.GroupPolicy
		}
		allowlist, blacklist = a.allowedGroupUsers, map[string]bool{}
	}
	switch {
	case policy == "disabled" || policy == "admin_only":
		return false
	case policy == "open" || isBot:
		return true
	case policy == "allowlist":
		return len(ids) > 0 && intersects(ids, allowlist)
	case policy == "blacklist":
		return len(ids) > 0 && !intersects(ids, blacklist)
	}
	return len(ids) > 0 && intersects(ids, a.allowedGroupUsers)
}

// Whether a message mentions the bot through at all, the mention list or the rendered post mentions
func (a *FeishuAdapter) mentionsSelf(message *larkim.EventMessage) bool {
	rawContent := sdkStr(message.Content)
	if strings.Contains(rawContent, "@_all") {
		return true
	}
	if len(message.Mentions) > 0 && a.messageMentionsBot(message.Mentions) {
		return true
	}
	n := a.normalize(sdkStr(message.MessageType), rawContent, mentionsFromEvent(message.Mentions))
	return postMentionsBot(n.Mentions)
}

// Whether any event mention matches the bot identity by open id, user id or exact name
func (a *FeishuAdapter) messageMentionsBot(mentions []*larkim.MentionEvent) bool {
	bot := a.botIdentity()
	for _, m := range mentions {
		if m == nil {
			continue
		}
		var openID, userID string
		if m.Id != nil {
			openID, userID = pystr.Strip(sdkStr(m.Id.OpenId)), pystr.Strip(sdkStr(m.Id.UserId))
		}
		if bot.matches(openID, userID, pystr.Strip(sdkStr(m.Name))) {
			return true
		}
	}
	return false
}

func postMentionsBot(mentions []FeishuMentionRef) bool {
	for _, m := range mentions {
		if m.IsSelf {
			return true
		}
	}
	return false
}

func (a *FeishuAdapter) botIdentity() feishuBotIdentity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return feishuBotIdentity{OpenID: a.botOpenID, UserID: a.settings.BotUserID, Name: a.botName}
}

// Bot open id and name from the bot info API, the application name filling a still missing name
func (a *FeishuAdapter) hydrateBotIdentity(ctx context.Context) {
	client := a.currentClient()
	if client == nil {
		return
	}
	if content, err := a.tenantGetRaw(ctx, "/open-apis/bot/v3/info", nil); err != nil {
		slog.Debug("[Feishu] /bot/v3/info probe failed during hydration")
	} else if len(content) > 0 {
		a.applyBotInfo(content)
	}
	a.mu.Lock()
	haveName := a.botName != ""
	a.mu.Unlock()
	if haveName {
		return
	}
	req := larkapplication.NewGetApplicationReqBuilder().AppId(a.settings.AppID).Lang("en_us").Build()
	var resp *larkapplication.GetApplicationResp
	if err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.GetApplication(ctx, req)
		return e
	}); err != nil {
		slog.Debug("[Feishu] Failed to hydrate bot name from application info")
		return
	}
	r := wrapResp(resp)
	if !responseSucceeded(r) {
		if r != nil && r.code == 99991672 {
			slog.Warn("[Feishu] Unable to hydrate bot name from application info. " +
				"Grant admin:app.info:readonly or application:application:self_manage " +
				"so group @mention gating can resolve the bot name precisely.")
		}
		return
	}
	if resp.Data == nil || resp.Data.App == nil {
		return
	}
	appName := pystr.Strip(sdkStr(resp.Data.App.AppName))
	a.mu.Lock()
	if appName != "" && a.botName == "" {
		a.botName = appName
	}
	a.mu.Unlock()
}

// Applies the open id and name of a bot info payload over stale env values
func (a *FeishuAdapter) applyBotInfo(content []byte) {
	payload, err := pyjson.Loads(string(content))
	if err != nil {
		slog.Debug("[Feishu] /bot/v3/info probe failed during hydration")
		return
	}
	root, ok := payload.(*pyjson.Object)
	if !ok {
		slog.Debug("[Feishu] /bot/v3/info probe failed during hydration")
		return
	}
	parsed, err := parseBotResponse(root)
	if err != nil {
		slog.Debug("[Feishu] /bot/v3/info probe failed during hydration")
		return
	}
	openID, name := pystr.Strip(strOr(parsed["bot_open_id"])), pystr.Strip(strOr(parsed["bot_name"]))
	a.mu.Lock()
	defer a.mu.Unlock()
	if openID != "" {
		if a.botOpenID != "" && a.botOpenID != openID {
			slog.Warn("[Feishu] FEISHU_BOT_OPEN_ID is stale; using /bot/v3/info open_id for group @mention gating.")
		}
		a.botOpenID = openID
	}
	if name != "" {
		if a.botName != "" && a.botName != name {
			slog.Info("[Feishu] FEISHU_BOT_NAME differs from /bot/v3/info; using hydrated bot name for group @mention gating.")
		}
		a.botName = name
	}
}
