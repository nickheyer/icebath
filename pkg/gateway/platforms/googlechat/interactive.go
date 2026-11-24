package googlechat

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	// Button parameter naming the prompt kind a click answers
	clickKindParam = "icebath"
	clickClarify   = "clarify"
	clickApproval  = "approval"
	clickConfirm   = "confirm"
	// Form input name of the multi select clarify checkboxes
	clarifyChoicesInput = "icebath_choices"
	clarifyOtherChoice  = "__other__"
	approvalCmdBudget   = 2000
)

// Locale keys of an approval decision and of a slash confirm decision
var (
	approvalDecisionKeys = map[string]string{"once": "approval.resolved_once", "session": "approval.resolved_session",
		"always": "approval.resolved_always", "deny": "approval.resolved_deny"}
	confirmDecisionKeys = map[string]string{"once": "confirm.decided_once", "always": "confirm.decided_always", "cancel": "confirm.decided_cancel"}
)

type runnerAuthorizer interface {
	IsUserAuthorized(ctx context.Context, source *gateway.SessionSource, allowAdapterDelegation bool) (bool, error)
}

// One button click on a card this adapter sent, in either event format
type cardClick struct {
	params     map[string]string
	formValues map[string][]string
	user       map[string]any
	message    map[string]any
	space      map[string]any
	addon      bool
}

// Function a card button invokes, the HTTP endpoint for callback apps and a plain name for Pub/Sub apps
func (a *GoogleChatAdapter) buttonFunction() string {
	if a.httpEventsURL != "" {
		return a.httpEventsURL
	}
	return "icebath_action"
}

// Card spec button whose click carries the given routing parameters
func (a *GoogleChatAdapter) cardButton(label string, params map[string]string) map[string]any {
	parameters := make(map[string]any, len(params))
	for k, v := range params {
		parameters[k] = v
	}
	return map[string]any{"text": label, "action": a.buttonFunction(), "parameters": parameters}
}

// Card spec widget showing the text as written
func plainParagraph(text string) map[string]any {
	return map[string]any{"type": "plain_text", "text": text}
}

// Card spec widget rendering markdown text
func markdownParagraph(text string) map[string]any {
	return map[string]any{"type": "markdown_text", "text": text}
}

// Card spec widget of a monospace block
func codeParagraph(text string) map[string]any {
	return map[string]any{"type": "code_block", "text": text}
}

// Card spec widget of a button row
func buttonRow(buttons []any) map[string]any {
	return map[string]any{"type": "buttons", "buttons": buttons}
}

// Cards v2 entry of a header title and widget sections, rendered by the card spec renderer
func promptCard(cardID, title string, sections ...[]any) (any, error) {
	rendered := make([]any, 0, len(sections))
	for _, widgets := range sections {
		if len(widgets) > 0 {
			rendered = append(rendered, map[string]any{"widgets": widgets})
		}
	}
	return CardSpecToCardsV2(map[string]any{"card_id": cardID, "header": map[string]any{"title": title}, "sections": rendered})
}

// Approval prompt as a card whose buttons resolve the pending approval of the session
func (a *GoogleChatAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	requestID := ""
	if prompt.RequestID != nil {
		requestID = *prompt.RequestID
	}
	promptID := strings.ReplaceAll(uuid.NewString(), "-", "")
	var buttons []any
	for _, action := range prompt.Actions {
		buttons = append(buttons, a.cardButton(action.Label, map[string]string{clickKindParam: clickApproval,
			"session_key": prompt.SessionKey, "choice": action.Choice, "request_id": requestID}))
	}
	details := []any{codeParagraph(gateway.TruncatePreview(prompt.Command, approvalCmdBudget, "..."))}
	if prompt.Description != "" {
		details = append(details, plainParagraph(i18n.T(ctx, "gateway.exec_approval.reason_label", "", nil)+": "+prompt.Description))
	}
	details = append(details, plainParagraph(gateway.FormatApprovalDeadlineLine(ctx, gateway.ApprovalTimeoutSeconds(ctx))))
	if prompt.SmartDenied {
		details = append(details, plainParagraph(strings.TrimSpace(a.EASmartDenyLine(ctx))))
	}
	card, err := promptCard("approval-"+promptID, "⚠️ "+i18n.T(ctx, "gateway.exec_approval.header", "", nil), details, []any{buttonRow(buttons)})
	if err != nil {
		return gateway.SendResult{}, err
	}
	return a.sendCard(ctx, prompt.ChatID, card, prompt.Metadata), nil
}

// Slash command confirmation card with approve once, always and cancel buttons
func (a *GoogleChatAdapter) SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (gateway.SendResult, error) {
	if title == "" {
		title = tr(ctx, "confirm.default_title", nil)
	}
	button := func(key, choice string) any {
		return a.cardButton(tr(ctx, key, nil), map[string]string{clickKindParam: clickConfirm, "session_key": sessionKey,
			"confirm_id": confirmID, "choice": choice})
	}
	card, err := promptCard("confirm-"+confirmID, textutil.First(title, 200), []any{markdownParagraph(textutil.First(message, maxTextLength))},
		[]any{buttonRow([]any{button("confirm.approve_once", "once"), button("confirm.always_approve", "always"), button("confirm.cancel", "cancel")})})
	if err != nil {
		return gateway.SendResult{}, err
	}
	return a.sendCard(ctx, chatID, card, metadata), nil
}

// String parameters of a click from the add-on, classic common or classic action parameter shapes
func clickParams(sources ...any) map[string]string {
	out := map[string]string{}
	for _, src := range sources {
		switch v := src.(type) {
		case map[string]any:
			for k, val := range v {
				out[k] = anyutil.String(val)
			}
		case []any:
			for _, item := range v {
				if kv, ok := item.(map[string]any); ok {
					out[anyutil.String(kv["key"])] = anyutil.String(kv["value"])
				}
			}
		}
	}
	return out
}

// Selected values of each form input of a click
func clickFormValues(raw any) map[string][]string {
	out := map[string][]string{}
	inputs, _ := raw.(map[string]any)
	for name, input := range inputs {
		entry, _ := input.(map[string]any)
		stringInputs, _ := entry["stringInputs"].(map[string]any)
		values, _ := stringInputs["value"].([]any)
		for _, v := range values {
			out[name] = append(out[name], anyutil.String(v))
		}
	}
	return out
}

// Card click carried by an envelope, false for every other event
func parseCardClick(envelope map[string]any) (*cardClick, bool) {
	var click *cardClick
	if chat, ok := envelope["chat"].(map[string]any); ok {
		payload, ok := chat["buttonClickedPayload"].(map[string]any)
		if !ok {
			return nil, false
		}
		common, _ := envelope["commonEventObject"].(map[string]any)
		user, _ := chat["user"].(map[string]any)
		message, _ := payload["message"].(map[string]any)
		space, _ := payload["space"].(map[string]any)
		click = &cardClick{params: clickParams(common["parameters"]), formValues: clickFormValues(common["formInputs"]),
			user: user, message: message, space: space, addon: true}
	} else {
		if anyutil.String(envelope["type"]) != "CARD_CLICKED" {
			return nil, false
		}
		common, _ := envelope["common"].(map[string]any)
		action, _ := envelope["action"].(map[string]any)
		user, _ := envelope["user"].(map[string]any)
		message, _ := envelope["message"].(map[string]any)
		space, _ := envelope["space"].(map[string]any)
		click = &cardClick{params: clickParams(action["parameters"], common["parameters"]), formValues: clickFormValues(common["formInputs"]),
			user: user, message: message, space: space}
	}
	if len(click.space) == 0 {
		click.space, _ = click.message["space"].(map[string]any)
	}
	return click, click.params[clickKindParam] != ""
}

// Clicker's display name, email or users id
func (c *cardClick) userLabel() string {
	return anyutil.String(firstTruthy(c.user["displayName"], c.user["email"], c.user["name"]))
}

// Message only the clicker sees
func (c *cardClick) privateNotice(text string) map[string]any {
	notice := map[string]any{"text": text}
	if name := anyutil.String(c.user["name"]); name != "" {
		notice["privateMessageViewer"] = map[string]any{"name": name}
	}
	return notice
}

// Whether the clicker may answer prompts, the injected check, then the runner, then the platform allowlist
func (a *GoogleChatAdapter) clickAuthorized(ctx context.Context, c *cardClick) bool {
	email, name := anyutil.String(c.user["email"]), anyutil.String(c.user["name"])
	userID := email
	if userID == "" {
		userID = name
	}
	if userID == "" {
		return false
	}
	spaceName := anyutil.String(c.space["name"])
	chatType := "group"
	if t := strings.ToUpper(anyutil.String(firstTruthy(c.space["type"], c.space["spaceType"]))); t == "DIRECT_MESSAGE" || t == "DM" {
		chatType = "dm"
	}
	if a.HasAuthorizationCheck() {
		if injected := a.IsSenderAuthorized(userID, &chatType, &spaceName, gateway.AuthorizationOpts{}); injected != nil {
			return *injected
		}
	}
	if auth, ok := a.MessageHandlerOwner().(runnerAuthorizer); ok {
		userName := c.userLabel()
		fields := gateway.SourceFields{ChatType: chatType, UserID: &userID, UserName: &userName}
		if name != "" {
			fields.UserIDAlt = &name
		}
		if allowed, err := auth.IsUserAuthorized(ctx, a.BuildSource(ctx, spaceName, fields), true); err == nil {
			return allowed
		}
	}
	sctx := gateway.SettingsContext(ctx)
	if settings.GoogleChatAllowAllUsers.Get(sctx) {
		return true
	}
	for _, entries := range [][]string{settings.GoogleChatAllowedUsers.Strings(sctx), settings.GatewayAllowedUsers.Strings(sctx)} {
		for _, entry := range entries {
			if entry = strings.ToLower(entry); entry == "*" || entry == strings.ToLower(email) || entry == strings.ToLower(name) {
				return true
			}
		}
	}
	return false
}

// Outcome line of a click, false when the prompt stays open for another click
func (a *GoogleChatAdapter) answerClick(ctx context.Context, c *cardClick) (string, bool) {
	user := c.userLabel()
	switch c.params[clickKindParam] {
	case clickApproval:
		choice := c.params["choice"]
		key, ok := approvalDecisionKeys[choice]
		if !ok {
			return i18n.T(ctx, "platform.shared.approval_expired", "", nil), true
		}
		if tools.ResolveGatewayApproval(c.params["session_key"], choice, false, "", c.params["request_id"]) == 0 {
			return i18n.T(ctx, "platform.shared.approval_expired", "", nil), true
		}
		pkgLog().Info(fmt.Sprintf("[GoogleChat] card button resolved approval (choice=%s, user=%s)", choice, user))
		return tr(ctx, key, map[string]any{"user": user}), true
	case clickConfirm:
		choice := c.params["choice"]
		key, ok := confirmDecisionKeys[choice]
		if !ok {
			choice, key = "cancel", confirmDecisionKeys["cancel"]
		}
		decision := tr(ctx, key, map[string]any{"user": user})
		if gateway.SlashConfirmResolve == nil {
			pkgLog().Error("[GoogleChat] slash confirm click arrived before the slash confirm resolver was installed")
			return decision, true
		}
		if result := gateway.SlashConfirmResolve(ctx, c.params["session_key"], c.params["confirm_id"], choice); result != "" {
			return decision + "\n\n" + result, true
		}
		return decision, true
	}
	clarifyID := c.params["clarify_id"]
	entry := tools.GetPendingClarify(clarifyID)
	if entry == nil {
		return i18n.T(ctx, "gateway.clarify.expired", "", nil), true
	}
	choice := c.params["choice"]
	if choice == clarifyOtherChoice {
		if !tools.MarkClarifyAwaitingText(clarifyID) {
			return i18n.T(ctx, "gateway.clarify.expired", "", nil), true
		}
		return tr(ctx, "clarify.awaiting", map[string]any{"user": user}), true
	}
	var picked []string
	if choice == "" {
		for _, raw := range c.formValues[clarifyChoicesInput] {
			if idx, err := strconv.Atoi(raw); err == nil && idx >= 0 && idx < len(entry.Choices) {
				picked = append(picked, strings.TrimSpace(entry.Choices[idx]))
			}
		}
	} else if idx, err := strconv.Atoi(choice); err == nil && idx >= 0 && idx < len(entry.Choices) {
		picked = []string{strings.TrimSpace(entry.Choices[idx])}
	}
	if len(picked) == 0 {
		return tr(ctx, "clarify.pick_one", nil), false
	}
	response := picked[0]
	if entry.MultiSelect {
		response = tools.JSON(picked)
	}
	if !tools.ResolveGatewayClarify(clarifyID, response) {
		return i18n.T(ctx, "gateway.clarify.expired", "", nil), true
	}
	pkgLog().Info(fmt.Sprintf("[GoogleChat] card button resolved clarify (id=%s, user=%s)", clarifyID, user))
	return tr(ctx, "clarify.resolved", map[string]any{"user": user, "choice": strings.Join(picked, ", ")}), true
}

// Clicked card without its buttons and inputs, ending with the outcome line
func settledCard(message map[string]any, outcome string) map[string]any {
	decision := map[string]any{"widgets": []any{map[string]any{"textParagraph": map[string]any{"text": outcome, "textSyntax": "MARKDOWN"}}}}
	cards, _ := message["cardsV2"].([]any)
	if len(cards) == 0 {
		return map[string]any{"cardId": "settled", "card": map[string]any{"sections": []any{decision}}}
	}
	entry, _ := cards[0].(map[string]any)
	card, _ := entry["card"].(map[string]any)
	sections, _ := card["sections"].([]any)
	kept := []any{}
	for _, raw := range sections {
		section, _ := raw.(map[string]any)
		widgets, _ := section["widgets"].([]any)
		var still []any
		for _, w := range widgets {
			widget, _ := w.(map[string]any)
			if _, buttons := widget["buttonList"]; buttons {
				continue
			}
			if _, input := widget["selectionInput"]; input {
				continue
			}
			still = append(still, w)
		}
		if len(still) > 0 {
			copied := copyMap(section)
			copied["widgets"] = still
			kept = append(kept, copied)
		}
	}
	settled := copyMap(card)
	settled["sections"] = append(kept, decision)
	return map[string]any{"cardId": anyutil.String(orDefault(entry["cardId"], "settled")), "card": settled}
}

// Answers a card click, the synchronous HTTP reply when sync, otherwise the card patched through the REST API
func (a *GoogleChatAdapter) handleCardClick(ctx context.Context, c *cardClick, sync bool) any {
	if !a.clickAuthorized(ctx, c) {
		pkgLog().Warn("[GoogleChat] Unauthorized card click by " + c.userLabel() + " - ignoring")
		return a.clickReply(ctx, c, sync, c.privateNotice(i18n.T(ctx, "platform.shared.not_authorized", "", nil)), false)
	}
	outcome, settled := a.answerClick(ctx, c)
	if !settled {
		return a.clickReply(ctx, c, sync, c.privateNotice(outcome), false)
	}
	return a.clickReply(ctx, c, sync, map[string]any{"cardsV2": []any{settledCard(c.message, outcome)}}, true)
}

// Delivers a click answer, an update of the clicked message or a new message, in the event's reply format
func (a *GoogleChatAdapter) clickReply(ctx context.Context, c *cardClick, sync bool, message map[string]any, update bool) any {
	if sync {
		if c.addon {
			action := "createMessageAction"
			if update {
				action = "updateMessageAction"
			}
			return map[string]any{"hostAppDataAction": map[string]any{"chatDataAction": map[string]any{action: map[string]any{"message": message}}}}
		}
		reply := copyMap(message)
		kind := "NEW_MESSAGE"
		if update {
			kind = "UPDATE_MESSAGE"
		}
		reply["actionResponse"] = map[string]any{"type": kind}
		return reply
	}
	name := anyutil.String(c.message["name"])
	if update && name != "" {
		if _, err := a.patchMessage(ctx, name, message); err != nil {
			pkgLog().Warn("[GoogleChat] Could not update the clicked card: " + excText(err))
		}
		return nil
	}
	spaceName := anyutil.String(c.space["name"])
	if spaceName == "" {
		return nil
	}
	body := copyMap(message)
	if thread, _ := c.message["thread"].(map[string]any); anyutil.IsTrue(thread["name"]) {
		body["thread"] = map[string]any{"name": thread["name"]}
	}
	if _, err := a.createMessage(ctx, spaceName, body); err != nil {
		pkgLog().Warn("[GoogleChat] Could not answer the card click: " + excText(err))
	}
	return nil
}
