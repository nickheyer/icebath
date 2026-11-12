package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyhtml"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Catalog text of a key with alternating keyword names and values
func tr(ctx context.Context, key string, kv ...any) string {
	var kwargs map[string]any
	if len(kv) > 0 {
		kwargs = map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			kwargs[kv[i].(string)] = kv[i+1]
		}
	}
	return i18n.T(ctx, key, "", kwargs)
}

// Inline keyboard button carrying callback data
func button(text, data string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: data}
}

// Two buttons per row so labels stay readable on mobile
func rowsOfTwo(buttons []models.InlineKeyboardButton) [][]models.InlineKeyboardButton {
	var rows [][]models.InlineKeyboardButton
	for i := 0; i < len(buttons); i += 2 {
		end := min(i+2, len(buttons))
		rows = append(rows, append([]models.InlineKeyboardButton(nil), buttons[i:end]...))
	}
	return rows
}

// Sends a control message, retrying once without message_thread_id when Telegram lost the thread
func (a *TelegramAdapter) sendMessageWithThreadFallback(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
	if a.currentBot() == nil {
		return nil, &runtimeError{msg: "Not connected"}
	}
	msg, err := a.boundedSendMessage(ctx, params)
	if err == nil {
		return msg, nil
	}
	if params.MessageThreadID != 0 && isBadRequestError(err) && isThreadNotFoundError(err) {
		slog.Warn(fmt.Sprintf("[%s] Thread %d not found for control message, retrying without message_thread_id", a.Name(), params.MessageThreadID))
		a.core.pruneStaleDMTopicBinding(ctx, params.ChatID, params.MessageThreadID, nil)
		retry := *params
		retry.MessageThreadID = 0
		return a.boundedSendMessage(ctx, &retry)
	}
	return nil, err
}

// Sends a prompt or picker message with topic routing and the thread fallback
func (a *TelegramAdapter) sendControlMessage(ctx context.Context, chatID, text string, parseMode models.ParseMode, threadID *string,
	metadata map[string]any, replyMarkup models.ReplyMarkup, replyToMode string) (*models.Message, error) {
	replyToID := replyToMessageIDForSend(nil, metadata, replyToMode)
	thread, err := threadKwargsForSend(threadID, metadata, replyToID, replyToMode)
	if err != nil {
		return nil, err
	}
	kw := textSend{chatID: NormalizeTelegramChatID(chatID), replyTo: replyToID, thread: thread, replyMarkup: replyMarkup}
	return a.sendMessageWithThreadFallback(ctx, a.sendMessageParams(kw, text, parseMode))
}

// Text, keyboard and sent hook of a control prompt, or a result to return as is
type builtPrompt struct {
	text     string
	keyboard models.ReplyMarkup
	onSent   func(msg *models.Message)
	result   *gateway.SendResult
}

// Shared control prompt shell, the not connected guard, routed send, state hook and redacted failure log
func (a *TelegramAdapter) sendPrompt(ctx context.Context, what, chatID string, metadata map[string]any, build func() (builtPrompt, error),
	parseMode models.ParseMode, threadID *string, replyToMode string) (gateway.SendResult, error) {
	if a.currentBot() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	built, err := build()
	if err == nil && built.result != nil {
		return *built.result, nil
	}
	var msg *models.Message
	if err == nil {
		if parseMode == "" {
			parseMode = models.ParseModeMarkdown
		}
		msg, err = a.sendControlMessage(ctx, chatID, built.text, parseMode, threadID, metadata, built.keyboard, replyToMode)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return gateway.SendResult{}, err
		}
		slog.Warn(fmt.Sprintf("[%s] %s failed: %s", a.Name(), what, redactTelegramErrorText(err)))
		return gateway.SendResult{Success: false, Error: redactTelegramErrorText(err)}, nil
	}
	if built.onSent != nil {
		built.onSent(msg)
	}
	return gateway.SendResult{Success: true, MessageID: strconv.Itoa(msg.ID)}, nil
}

// Inline keyboard Yes and No prompt of the gateway update watcher
func (a *TelegramAdapter) SendUpdatePrompt(ctx context.Context, chatID, prompt, def, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	build := func() (builtPrompt, error) {
		hint := ""
		if def != "" {
			hint = tr(ctx, "platform.telegram.prompt.default_hint", "default", def)
		}
		text := a.FormatMessage(fmt.Sprintf("☤ *%s*\n\n%s%s", tr(ctx, "platform.telegram.prompt.update_header"), prompt, hint))
		keyboard := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			button(tr(ctx, "platform.telegram.prompt.affirm"), "update_prompt:y"), button(tr(ctx, "platform.telegram.prompt.negate"), "update_prompt:n")}}}
		return builtPrompt{text: text, keyboard: keyboard}, nil
	}
	return a.sendPrompt(ctx, "send_update_prompt", chatID, metadata, build, "", metadataThreadID(metadata), a.replyToMode)
}

// Approval card header with the localized text escaped before the bold wrapper
func (a *TelegramAdapter) EAHeader(ctx context.Context) string {
	return "⚠️ <b>" + pyhtml.Escape(tr(ctx, "gateway.exec_approval.header"), true) + "</b>\n\n"
}

// Escaped reason label of the approval card
func (a *TelegramAdapter) EAReasonLabel(ctx context.Context) string {
	return pyhtml.Escape(tr(ctx, "gateway.exec_approval.reason_label"), true) + ": "
}

// Smart deny line of the approval card with its bold label
func (a *TelegramAdapter) EASmartDenyLine(ctx context.Context) string {
	return "\n\n" + boldLabelHTML(tr(ctx, "gateway.exec_approval.smart_deny_line"))
}

// HTML escape of approval card text
func (a *TelegramAdapter) EAEscape(text string) string { return pyhtml.Escape(text, true) }

// Command preview budget left by the card framing under the 4096 UTF-16 cap
func (a *TelegramAdapter) ExecApprovalCmdBudget(ctx context.Context, description string, smartDenied bool) int {
	framing := a.EAHeader(ctx) + a.Traits.EACodeOpen + a.Traits.EACodeClose + a.EAReasonLabel(ctx) + a.EAEscape(description) + "..." +
		a.EADeadlineLine(ctx)
	if smartDenied {
		framing += a.EASmartDenyLine(ctx)
	}
	return max(0, maxMessageLength-gateway.UTF16Len(framing))
}

// Short approval button labels that share a mobile row
func (a *TelegramAdapter) EAActionLabelsFor(ctx context.Context) map[string]string {
	out := map[string]string{}
	for _, choice := range []string{"once", "session", "always", "deny"} {
		out[choice] = tr(ctx, "platform.telegram.approval.action_"+choice)
	}
	return out
}

// Inline keyboard approval prompt whose buttons resolve the gateway approval
func (a *TelegramAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	build := func() (builtPrompt, error) {
		a.mu.Lock()
		a.approvalCounter++
		approvalID := a.approvalCounter
		a.mu.Unlock()
		buttons := make([]models.InlineKeyboardButton, 0, len(prompt.Actions))
		for _, action := range prompt.Actions {
			buttons = append(buttons, button(action.Label, fmt.Sprintf("ea:%s:%d", action.Choice, approvalID)))
		}
		return builtPrompt{text: prompt.Text, keyboard: &models.InlineKeyboardMarkup{InlineKeyboard: rowsOfTwo(buttons)},
			onSent: func(*models.Message) {
				a.mu.Lock()
				a.approvalState[approvalID] = prompt.SessionKey
				a.mu.Unlock()
			}}, nil
	}
	return a.sendPrompt(ctx, "send_exec_approval", prompt.ChatID, prompt.Metadata, build, models.ParseModeHTML,
		metadataThreadID(prompt.Metadata), a.replyToMode)
}

// Three button slash command confirmation prompt
func (a *TelegramAdapter) SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (gateway.SendResult, error) {
	build := func() (builtPrompt, error) {
		keyboard := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
			{button(tr(ctx, "platform.telegram.slash_confirm.approve_once"), "sc:once:"+confirmID),
				button(tr(ctx, "platform.telegram.slash_confirm.always_approve"), "sc:always:"+confirmID)},
			{button(tr(ctx, "platform.telegram.slash_confirm.cancel"), "sc:cancel:"+confirmID)}}}
		budget := maxMessageLength - gateway.UTF16Len(a.FormatMessage("..."))
		preview := a.FormatMessage(a.EAFit(message, budget, "...", a.FormatMessage))
		return builtPrompt{text: preview, keyboard: keyboard, onSent: func(*models.Message) {
			a.mu.Lock()
			a.slashConfirmState[confirmID] = sessionKey
			a.mu.Unlock()
		}}, nil
	}
	return a.sendPrompt(ctx, "send_slash_confirm", chatID, metadata, build, "", metadataThreadID(metadata), a.replyToMode)
}

// Clarify prompt with numbered choice buttons and the other button, or the plain question without choices
func (a *TelegramAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string,
	metadata map[string]any) (gateway.SendResult, error) {
	build := func() (builtPrompt, error) {
		text := "❓ " + pyhtml.Escape(question, true)
		var keyboard models.ReplyMarkup
		if len(choices) > 0 {
			lines := make([]string, 0, len(choices))
			for i, c := range choices {
				lines = append(lines, fmt.Sprintf("%d. %s", i+1, pyhtml.Escape(c, true)))
			}
			text += "\n\n" + strings.Join(lines, "\n")
			rows := make([][]models.InlineKeyboardButton, 0, len(choices)+1)
			for idx := range choices {
				rows = append(rows, []models.InlineKeyboardButton{button(strconv.Itoa(idx+1), fmt.Sprintf("cl:%s:%d", clarifyID, idx))})
			}
			rows = append(rows, []models.InlineKeyboardButton{button(tr(ctx, "platform.telegram.prompt.other"), fmt.Sprintf("cl:%s:other", clarifyID))})
			keyboard = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
		}
		return builtPrompt{text: text, keyboard: keyboard, onSent: func(*models.Message) {
			a.mu.Lock()
			a.clarifyState[clarifyID] = sessionKey
			a.mu.Unlock()
		}}, nil
	}
	return a.sendPrompt(ctx, "send_clarify", chatID, metadata, build, models.ParseModeHTML, metadataThreadID(metadata), "first")
}

// Interactive model picker state of a chat
type modelPickerState struct {
	msgID                int
	providers            []map[string]any
	sessionKey           string
	onModelSelected      func(ctx context.Context, chatID, modelID, providerSlug string) (string, error)
	currentModel         string
	currentProvider      string
	providerPage         int
	selectedProvider     string
	selectedProviderName string
	modelList            []string
	modelPage            int
}

// Choice picker state of a chat
type choicePickerState struct {
	msgID            int
	choices          []map[string]any
	sessionKey       string
	onChoiceSelected func(ctx context.Context, chatID, value string) (string, error)
}

// Thread id metadata value of a picker send
func pickerThreadID(metadata map[string]any) *string {
	v, ok := metadata["thread_id"]
	if !ok || v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	return &s
}

// Provider then model drill down picker edited in place
func (a *TelegramAdapter) SendModelPicker(ctx context.Context, chatID string, providers []map[string]any, currentModel, currentProvider, sessionKey string,
	onModelSelected func(ctx context.Context, chatID, modelID, providerSlug string) (string, error), metadata map[string]any) (gateway.SendResult, error) {
	build := func() (builtPrompt, error) {
		keyboard, pageInfo := a.buildProviderKeyboard(ctx, providers, 0)
		text := a.FormatMessage(providerListText(ctx, currentModel, provider.Label(currentProvider), pageInfo))
		return builtPrompt{text: text, keyboard: keyboard, onSent: func(msg *models.Message) {
			a.mu.Lock()
			a.modelPickerState[chatID] = &modelPickerState{msgID: msg.ID, providers: providers, sessionKey: sessionKey, onModelSelected: onModelSelected,
				currentModel: currentModel, currentProvider: currentProvider}
			a.mu.Unlock()
		}}, nil
	}
	return a.sendPrompt(ctx, "send_model_picker", chatID, metadata, build, "", pickerThreadID(metadata), a.replyToMode)
}

// Flat one tap choice picker for reasoning, fast and the other value commands
func (a *TelegramAdapter) SendChoicePicker(ctx context.Context, chatID, title string, choices []map[string]any, sessionKey string,
	onChoiceSelected func(ctx context.Context, chatID, value string) (string, error), metadata map[string]any) (gateway.SendResult, error) {
	build := func() (builtPrompt, error) {
		var buttons []models.InlineKeyboardButton
		for i, choice := range choices {
			label := pyrepr.Str(pyrepr.Or(choice["label"], pyrepr.Or(choice["value"], "")))
			if pyTruthy(choice["is_current"]) {
				label = "✓ " + label
			}
			buttons = append(buttons, button(label, fmt.Sprintf("cp:%d", i)))
		}
		if len(buttons) == 0 {
			return builtPrompt{result: &gateway.SendResult{Success: false, Error: "No choices"}}, nil
		}
		return builtPrompt{text: a.FormatMessage(title), keyboard: &models.InlineKeyboardMarkup{InlineKeyboard: rowsOfTwo(buttons)},
			onSent: func(msg *models.Message) {
				a.mu.Lock()
				a.choicePickerState[chatID] = &choicePickerState{msgID: msg.ID, choices: choices, sessionKey: sessionKey, onChoiceSelected: onChoiceSelected}
				a.mu.Unlock()
			}}, nil
	}
	return a.sendPrompt(ctx, "send_choice_picker", chatID, metadata, build, "", pickerThreadID(metadata), a.replyToMode)
}

// Callback query of a button tap with the SDK bot that answers and edits it
type callbackQuery struct {
	q *models.CallbackQuery
	b *bot.Bot
}

// Message the tapped button belongs to, nil when inaccessible or inline
func (q callbackQuery) message() *models.Message { return q.q.Message.Message }

// Chat id of the tapped message, ok false without one
func (q callbackQuery) chatID() (int64, bool) {
	if m := q.q.Message.Message; m != nil {
		return m.Chat.ID, true
	}
	if m := q.q.Message.InaccessibleMessage; m != nil {
		return m.Chat.ID, true
	}
	return 0, false
}

// Text of the tapped message, empty when absent
func (q callbackQuery) messageText() string {
	if m := q.message(); m != nil {
		return m.Text
	}
	return ""
}

// Answers the tap, a toast when text is non empty
func (q callbackQuery) answer(ctx context.Context, text string) error {
	_, err := q.b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: q.q.ID, Text: text})
	return ptbError(err)
}

// Edits the tapped message text, the keyboard kept only when markup is given
func (q callbackQuery) editMessageText(ctx context.Context, text string, parseMode models.ParseMode, markup models.ReplyMarkup) error {
	params := &bot.EditMessageTextParams{Text: text, ParseMode: parseMode, ReplyMarkup: markup}
	if q.q.InlineMessageID != "" {
		params.InlineMessageID = q.q.InlineMessageID
	} else {
		chatID, _ := q.chatID()
		params.ChatID = chatID
		if m := q.q.Message.Message; m != nil {
			params.MessageID = m.ID
		} else if m := q.q.Message.InaccessibleMessage; m != nil {
			params.MessageID = m.MessageID
		}
	}
	_, err := q.b.EditMessageText(ctx, params)
	return ptbError(err)
}

// First name of the tapping user, empty when absent
func (q callbackQuery) firstName() string { return q.q.From.FirstName }

// Replaces a picker message with the result text, MarkdownV2 then plain, the keyboard removed
func (a *TelegramAdapter) editResultText(ctx context.Context, q callbackQuery, resultText string) {
	if err := q.editMessageText(ctx, a.FormatMessage(resultText), models.ParseModeMarkdown, nil); err != nil {
		_ = q.editMessageText(ctx, resultText, "", nil)
	}
}

// Handles choice picker taps of the cp prefix
func (a *TelegramAdapter) handleChoicePickerCallback(ctx context.Context, q callbackQuery, data, chatID string) error {
	a.mu.Lock()
	state := a.choicePickerState[chatID]
	a.mu.Unlock()
	if state == nil {
		return q.answer(ctx, toast(ctx, "platform.telegram.picker.expired_rerun", nil))
	}
	idx, err := pyIntOf(data[3:])
	if err == nil && idx < 0 {
		idx += len(state.choices)
	}
	if err != nil || idx < 0 || idx >= len(state.choices) {
		return q.answer(ctx, toast(ctx, "platform.telegram.picker.invalid_selection", nil))
	}
	choice := state.choices[idx]
	if state.onChoiceSelected == nil {
		return q.answer(ctx, toast(ctx, "platform.telegram.picker.expired", nil))
	}
	resultText, cbErr := state.onChoiceSelected(ctx, chatID, pyrepr.Str(pyrepr.Or(choice["value"], "")))
	if cbErr != nil {
		slog.Error(fmt.Sprintf("Choice picker selection failed: %s", pyrepr.ExceptionMessage(cbErr)))
		resultText = tr(ctx, "platform.telegram.picker.apply_error", "error", pyrepr.ExceptionMessage(cbErr))
	}
	a.editResultText(ctx, q, resultText)
	if err := q.answer(ctx, ""); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.choicePickerState, chatID)
	a.mu.Unlock()
	return nil
}

// Model count of a provider row, total_models else the length of its models
func providerModelCount(p map[string]any) any {
	if v, ok := p["total_models"]; ok {
		return v
	}
	return len(stringList(p["models"]))
}

// Strings of a list value
func stringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, pyrepr.Str(item))
		}
		return out
	}
	return nil
}

// Provider button with its model count and the current marker
func providerButton(p map[string]any) models.InlineKeyboardButton {
	label := fmt.Sprintf("%s (%s)", pyrepr.Str(p["name"]), pyrepr.Str(providerModelCount(p)))
	if pyTruthy(p["is_current"]) {
		label = "✓ " + label
	}
	return button(label, "mp:"+pyrepr.Str(p["slug"]))
}

// Previous, position and next row of a paged picker
func pickerNavRow(ctx context.Context, page, totalPages int, prefix string) []models.InlineKeyboardButton {
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, button(tr(ctx, "platform.telegram.picker.prev"), fmt.Sprintf("%s:%d", prefix, page-1)))
	}
	nav = append(nav, button(fmt.Sprintf("%d/%d", page+1, totalPages), "mx:noop"))
	if page < totalPages-1 {
		nav = append(nav, button(tr(ctx, "platform.telegram.picker.next"), fmt.Sprintf("%s:%d", prefix, page+1)))
	}
	return nav
}

// Back and cancel row of the picker
func pickerBackCancelRow(ctx context.Context) []models.InlineKeyboardButton {
	return []models.InlineKeyboardButton{button(tr(ctx, "platform.telegram.picker.back"), "mb"), button(tr(ctx, "platform.telegram.picker.cancel"), "mx")}
}

// Page meta integer field
func metaInt(meta map[string]any, key string) int {
	switch v := meta[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// Keyboard of a page of buttons with the navigation row when paged and the tail row
func pagedKeyboard(ctx context.Context, buttons []models.InlineKeyboardButton, meta map[string]any, navPrefix string,
	tail []models.InlineKeyboardButton) (*models.InlineKeyboardMarkup, string) {
	rows := rowsOfTwo(buttons)
	if metaInt(meta, "total_pages") > 1 {
		rows = append(rows, pickerNavRow(ctx, metaInt(meta, "page"), metaInt(meta, "total_pages"), navPrefix))
	}
	rows = append(rows, tail)
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}, pyrepr.Str(meta["page_info"])
}

// Paginated provider keyboard folding provider families into one group button
func (a *TelegramAdapter) buildProviderKeyboard(ctx context.Context, providers []map[string]any, page int) (*models.InlineKeyboardMarkup, string) {
	bySlug := map[string]map[string]any{}
	slugs := make([]string, 0, len(providers))
	for _, p := range providers {
		slug := pyrepr.Str(p["slug"])
		bySlug[slug] = p
		slugs = append(slugs, slug)
	}
	var buttons []models.InlineKeyboardButton
	for _, row := range provider.GroupProviders(slugs) {
		if row.Kind == "group" {
			count, current := 0, false
			for _, m := range row.Members {
				member, ok := bySlug[m]
				if !ok {
					continue
				}
				count += pyCount(providerModelCount(member))
				current = current || pyTruthy(member["is_current"])
			}
			label := fmt.Sprintf("%s ▸ (%d)", row.Label, count)
			if current {
				label = "✓ " + label
			}
			buttons = append(buttons, button(label, "mpg:"+row.GroupID))
		} else if p, ok := bySlug[row.Slug]; ok {
			buttons = append(buttons, providerButton(p))
		}
	}
	pageButtons, meta := gateway.FormatChoicePage(ctx, buttons, page, providerPageSize)
	return pagedKeyboard(ctx, pageButtons, meta, "mpv", []models.InlineKeyboardButton{button(tr(ctx, "platform.telegram.picker.cancel"), "mx")})
}

// Integer of a count value
func pyCount(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

// Paginated model buttons, each carrying the plan word of window plans
func (a *TelegramAdapter) buildModelKeyboard(ctx context.Context, modelIDs []string, page int, providerRow map[string]any) (*models.InlineKeyboardMarkup, string) {
	pageModels, meta := gateway.FormatChoicePage(ctx, modelIDs, page, modelPageSize)
	start := metaInt(meta, "start")
	var row any
	if providerRow != nil {
		row = providerRow
	}
	buttons := make([]models.InlineKeyboardButton, 0, len(pageModels))
	for i, modelID := range pageModels {
		short := modelID
		if idx := strings.LastIndex(modelID, "/"); idx >= 0 {
			short = modelID[idx+1:]
		}
		suffix := provider.PickerRowSuffix(row, modelID)
		room := 38
		if suffix != "" {
			room -= len([]rune(suffix)) + 3
		}
		if len([]rune(short)) > room {
			short = string([]rune(short)[:max(room-3, 8)]) + "..."
		}
		if suffix != "" {
			short = fmt.Sprintf("%s · %s", short, suffix)
		}
		buttons = append(buttons, button(short, fmt.Sprintf("mm:%d", start+i)))
	}
	return pagedKeyboard(ctx, buttons, meta, "mg", pickerBackCancelRow(ctx))
}

// Re-renders the picker message in place as MarkdownV2 and acks the tap
func (a *TelegramAdapter) pickerEdit(ctx context.Context, q callbackQuery, textMD string, keyboard models.ReplyMarkup) error {
	if err := q.editMessageText(ctx, a.FormatMessage(textMD), models.ParseModeMarkdown, keyboard); err != nil {
		return err
	}
	return q.answer(ctx, "")
}

// Provider row of a slug, nil when absent
func findProvider(providers []map[string]any, slug string) map[string]any {
	for _, p := range providers {
		if pyrepr.Str(p["slug"]) == slug {
			return p
		}
	}
	return nil
}

// Renders the model page of the selected provider
func (a *TelegramAdapter) pickerShowModels(ctx context.Context, q callbackQuery, state *modelPickerState, page int) error {
	modelIDs := state.modelList
	state.modelPage = page
	p := findProvider(state.providers, state.selectedProvider)
	keyboard, pageInfo := a.buildModelKeyboard(ctx, modelIDs, page, p)
	total := len(modelIDs)
	if p != nil {
		total = pyCount(providerModelCount(p))
	}
	extra := ""
	if total > len(modelIDs) {
		extra = "\n_" + tr(ctx, "platform.telegram.picker.more_available", "count", strconv.Itoa(total-len(modelIDs))) + "_"
	}
	planLine := ""
	if p != nil && pyTruthy(p["plan_line"]) {
		planLine = "\n⚠️ " + pyrepr.Str(p["plan_line"])
	}
	text := fmt.Sprintf("⚙ *%s*\n\n%s%s%s\n%s%s", tr(ctx, "platform.telegram.picker.title"),
		tr(ctx, "platform.telegram.picker.provider_label", "provider", "*"+state.selectedProviderName+"*"), pageInfo, planLine,
		tr(ctx, "platform.telegram.picker.select_model"), extra)
	return a.pickerEdit(ctx, q, text, keyboard)
}

// Provider list body of the picker
func providerListText(ctx context.Context, currentModel, providerLabel, pageInfo string) string {
	model := currentModel
	if model == "" {
		model = tr(ctx, "platform.telegram.picker.unknown_model")
	}
	return fmt.Sprintf("⚙ *%s*\n\n%s\n%s\n\n%s%s", tr(ctx, "platform.telegram.picker.title"),
		tr(ctx, "platform.telegram.picker.current_model", "model", "`"+model+"`"),
		tr(ctx, "platform.telegram.picker.provider_label", "provider", providerLabel), tr(ctx, "platform.telegram.picker.select_provider"), pageInfo)
}

// Renders the folded and paginated provider list
func (a *TelegramAdapter) pickerShowProviders(ctx context.Context, q callbackQuery, state *modelPickerState, page int) error {
	keyboard, pageInfo := a.buildProviderKeyboard(ctx, state.providers, page)
	return a.pickerEdit(ctx, q, providerListText(ctx, state.currentModel, provider.Label(state.currentProvider), pageInfo), keyboard)
}

// Resolves an mm or mc index into the model, provider and callback, answering and returning false on error
func (a *TelegramAdapter) pickerSelection(ctx context.Context, q callbackQuery, state *modelPickerState, rawIdx string) (int, string, string, bool, error) {
	idx, err := pyIntOf(rawIdx)
	if err != nil {
		return 0, "", "", false, q.answer(ctx, toast(ctx, "platform.telegram.picker.invalid_selection", nil))
	}
	if idx < 0 || idx >= len(state.modelList) {
		return 0, "", "", false, q.answer(ctx, toast(ctx, "platform.telegram.picker.invalid_model_index", nil))
	}
	if state.onModelSelected == nil {
		return 0, "", "", false, q.answer(ctx, toast(ctx, "platform.telegram.picker.expired", nil))
	}
	return idx, state.modelList[idx], state.selectedProvider, true, nil
}

// Performs the switch, renders the result and drops the picker state
func (a *TelegramAdapter) pickerSwitch(ctx context.Context, q callbackQuery, chatID, modelID, providerSlug string,
	callback func(ctx context.Context, chatID, modelID, providerSlug string) (string, error)) error {
	failed := false
	resultText, err := callback(ctx, chatID, modelID, providerSlug)
	if err != nil {
		slog.Error(fmt.Sprintf("Model picker switch failed: %s", pyrepr.ExceptionMessage(err)))
		resultText = tr(ctx, "platform.telegram.picker.switch_error", "error", pyrepr.ExceptionMessage(err))
		failed = true
	}
	a.editResultText(ctx, q, resultText)
	key := "platform.telegram.picker.switched"
	if failed {
		key = "platform.telegram.picker.switch_failed"
	}
	if err := q.answer(ctx, toast(ctx, key, nil)); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.modelPickerState, chatID)
	a.mu.Unlock()
	return nil
}

// Page number of a callback, answering the invalid page toast when it does not parse
func parsePage(ctx context.Context, q callbackQuery, raw string) (int, bool, error) {
	page, err := pyIntOf(raw)
	if err != nil {
		return 0, false, q.answer(ctx, toast(ctx, "platform.telegram.picker.invalid_page", nil))
	}
	return page, true, nil
}

// Handles the model picker callbacks of the mp, mpg, mpv, mm, mc, mb, mx and mg prefixes
func (a *TelegramAdapter) handleModelPickerCallback(ctx context.Context, q callbackQuery, data, chatID string) error {
	a.mu.Lock()
	state := a.modelPickerState[chatID]
	a.mu.Unlock()
	if state == nil {
		return q.answer(ctx, toast(ctx, "platform.telegram.picker.expired_model", nil))
	}
	switch {
	case strings.HasPrefix(data, "mp:"):
		slug := data[3:]
		p := findProvider(state.providers, slug)
		if p == nil {
			return q.answer(ctx, toast(ctx, "platform.telegram.picker.provider_not_found", nil))
		}
		state.selectedProvider = slug
		state.selectedProviderName = pyrepr.Str(pyrepr.Or(p["name"], slug))
		if _, has := p["name"]; has {
			state.selectedProviderName = pyrepr.Str(p["name"])
		}
		state.modelList = stringList(p["models"])
		return a.pickerShowModels(ctx, q, state, 0)
	case strings.HasPrefix(data, "mg:"):
		page, ok, err := parsePage(ctx, q, data[3:])
		if !ok {
			return err
		}
		return a.pickerShowModels(ctx, q, state, page)
	case strings.HasPrefix(data, "mpv:"):
		page, ok, err := parsePage(ctx, q, data[4:])
		if !ok {
			return err
		}
		state.providerPage = page
		return a.pickerShowProviders(ctx, q, state, page)
	case strings.HasPrefix(data, "mc:"):
		_, modelID, slug, ok, err := a.pickerSelection(ctx, q, state, data[3:])
		if !ok {
			return err
		}
		return a.pickerSwitch(ctx, q, chatID, modelID, slug, state.onModelSelected)
	case strings.HasPrefix(data, "mm:"):
		idx, modelID, slug, ok, err := a.pickerSelection(ctx, q, state, data[3:])
		if !ok {
			return err
		}
		warning := provider.CombinedSelectionWarning(ctx, provider.SelectionRequest{Model: modelID, Provider: slug})
		if warning != nil {
			keyboard := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
				{button(tr(ctx, "platform.telegram.picker.switch_anyway"), fmt.Sprintf("mc:%d", idx))}, pickerBackCancelRow(ctx)}}
			if err := q.editMessageText(ctx, a.FormatMessage(fmt.Sprintf("⚠ *%s*\n\n%s", warning.Title, warning.Message)), models.ParseModeMarkdown,
				keyboard); err != nil {
				return err
			}
			return q.answer(ctx, toast(ctx, "platform.telegram.picker.confirm_selection", nil))
		}
		return a.pickerSwitch(ctx, q, chatID, modelID, slug, state.onModelSelected)
	case strings.HasPrefix(data, "mpg:"):
		groupID := data[4:]
		label := ""
		var memberSlugs []string
		for _, g := range provider.ProviderGroups() {
			if g.ID == groupID {
				label, memberSlugs = g.Label, g.Members
				break
			}
		}
		var buttons []models.InlineKeyboardButton
		for _, slug := range memberSlugs {
			if p := findProvider(state.providers, slug); p != nil {
				buttons = append(buttons, providerButton(p))
			}
		}
		if len(buttons) == 0 {
			return q.answer(ctx, toast(ctx, "platform.telegram.picker.group_not_found", nil))
		}
		rows := append(rowsOfTwo(buttons), pickerBackCancelRow(ctx))
		family := label
		if family == "" {
			family = groupID
		}
		text := fmt.Sprintf("⚙ *%s*\n\n%s\n\n%s", tr(ctx, "platform.telegram.picker.title"),
			tr(ctx, "platform.telegram.picker.provider_family", "family", "*"+family+"*"), tr(ctx, "platform.telegram.picker.select_provider"))
		return a.pickerEdit(ctx, q, text, &models.InlineKeyboardMarkup{InlineKeyboard: rows})
	case data == "mb":
		return a.pickerShowProviders(ctx, q, state, state.providerPage)
	case data == "mx":
		a.mu.Lock()
		delete(a.modelPickerState, chatID)
		a.mu.Unlock()
		if err := q.editMessageText(ctx, tr(ctx, "platform.telegram.picker.cancelled"), "", nil); err != nil {
			return err
		}
		return q.answer(ctx, "")
	}
	return q.answer(ctx, "")
}

// Tells the user a clarify tap arrived after its entry expired
func (a *TelegramAdapter) notifyClarifyExpired(ctx context.Context, q callbackQuery) {
	_ = q.answer(ctx, toast(ctx, "platform.telegram.prompt.expired_toast", nil))
	a.editHTMLQuiet(ctx, q, fmt.Sprintf("❓ %s\n\n<i>%s</i>", pyhtml.Escape(q.messageText(), true),
		pyhtml.Escape(tr(ctx, "platform.telegram.prompt.expired_body"), true)))
}

// HTML edit with the keyboard removed, failures ignored
func (a *TelegramAdapter) editHTMLQuiet(ctx context.Context, q callbackQuery, text string) {
	_ = q.editMessageText(ctx, text, models.ParseModeHTML, nil)
}

// MarkdownV2 edit with the keyboard removed, failures ignored
func (a *TelegramAdapter) editMDQuiet(ctx context.Context, q callbackQuery, textMD string) {
	_ = q.editMessageText(ctx, a.FormatMessage(textMD), models.ParseModeMarkdown, nil)
}

// Answers an inline query with the searchable command and skill picker, an empty personal list for unauthorized users
func (a *TelegramAdapter) handleInlineQuery(ctx context.Context, update *Update) error {
	iq := update.Raw.InlineQuery
	if iq == nil {
		return nil
	}
	b := a.currentBot()
	userID := ""
	if iq.From != nil && iq.From.ID != 0 {
		userID = pystr.Strip(strconv.FormatInt(iq.From.ID, 10))
	}
	var userName *string
	if iq.From != nil && iq.From.Username != "" {
		userName = &iq.From.Username
	}
	private := "private"
	authorized := userID != "" && a.core.isCallbackUserAuthorized(ctx, userID, &userID, &private, nil, userName)
	if !authorized {
		a.AcceptUpdate(ctx)
		if b == nil {
			a.failUpdatePreparation(ctx)
			return nil
		}
		if _, err := b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{InlineQueryID: iq.ID, Results: []models.InlineQueryResult{},
			CacheTime: CacheTimeSeconds, IsPersonal: true}); err != nil {
			a.failUpdatePreparation(ctx)
			slog.Debug(fmt.Sprintf("[%s] inline picker empty answer failed", a.Name()), "error", ptbError(err))
		}
		return nil
	}
	results, nextOffset := BuildInlineResults(ctx, iq.Query, iq.Offset, 50)
	articles := make([]models.InlineQueryResult, 0, len(results))
	for _, r := range results {
		articles = append(articles, &models.InlineQueryResultArticle{ID: r.ID, Title: r.Title, Description: r.Description,
			InputMessageContent: &models.InputTextMessageContent{MessageText: r.MessageText}})
	}
	a.AcceptUpdate(ctx)
	if b == nil {
		a.failUpdatePreparation(ctx)
		return nil
	}
	if _, err := b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{InlineQueryID: iq.ID, Results: articles, CacheTime: CacheTimeSeconds,
		IsPersonal: true, NextOffset: nextOffset}); err != nil {
		a.failUpdatePreparation(ctx)
		slog.Debug(fmt.Sprintf("[%s] inline picker answer failed", a.Name()), "error", ptbError(err))
	}
	return nil
}

// Chat, thread and user context of a button tap for the callback auth gate
type callbackCtx struct {
	chatID   *string
	chatType *string
	threadID *string
	userName *string
}

// Callback auth context of a tap
func newCallbackCtx(q callbackQuery) callbackCtx {
	var cb callbackCtx
	if m := q.message(); m != nil {
		chatID := strconv.FormatInt(m.Chat.ID, 10)
		chatType := string(m.Chat.Type)
		cb.chatID, cb.chatType = &chatID, &chatType
		if m.MessageThreadID != 0 {
			threadID := strconv.Itoa(m.MessageThreadID)
			cb.threadID = &threadID
		}
	} else if id, ok := q.chatID(); ok {
		chatID := strconv.FormatInt(id, 10)
		chatType := string(q.q.Message.InaccessibleMessage.Chat.Type)
		cb.chatID, cb.chatType = &chatID, &chatType
	}
	if name := q.firstName(); name != "" {
		cb.userName = &name
	}
	return cb
}

// Gates a tap on the callback allowlist, answering the denial text when refused
func (a *TelegramAdapter) callbackAuthorized(ctx context.Context, q callbackQuery, cb callbackCtx, denial string) (bool, error) {
	if a.core.isCallbackUserAuthorized(ctx, strconv.FormatInt(q.q.From.ID, 10), cb.chatID, cb.chatType, cb.threadID, cb.userName) {
		return true, nil
	}
	return false, q.answer(ctx, denial)
}

// Dispatches an inline keyboard tap on its callback data prefix
func (a *TelegramAdapter) handleCallbackQuery(ctx context.Context, update *Update) error {
	cq := update.Raw.CallbackQuery
	if cq == nil || cq.Data == "" {
		return nil
	}
	b := a.currentBot()
	if b == nil {
		return &runtimeError{msg: "Not connected"}
	}
	a.AcceptUpdate(ctx)
	q := callbackQuery{q: cq, b: b}
	data := cq.Data
	cb := newCallbackCtx(q)
	for _, route := range []struct {
		prefixes []string
		handler  func(ctx context.Context, q callbackQuery, data, chatID string) error
	}{{[]string{"mp:", "mpg:", "mpv:", "mm:", "mc:", "mb", "mx", "mg:"}, a.handleModelPickerCallback}, {[]string{"cp:"}, a.handleChoicePickerCallback}} {
		for _, prefix := range route.prefixes {
			if !strings.HasPrefix(data, prefix) {
				continue
			}
			id, ok := q.chatID()
			if !ok || q.message() == nil {
				return nil
			}
			authorized, err := a.callbackAuthorized(ctx, q, cb, unauthorizedToast(ctx))
			if err != nil || !authorized {
				return err
			}
			return route.handler(ctx, q, data, strconv.FormatInt(id, 10))
		}
	}
	for _, route := range []struct {
		prefix  string
		handler func(ctx context.Context, q callbackQuery, data string, cb callbackCtx) error
	}{{"gt:", a.handleGmailTriageCallback}, {"ea:", a.handleExecApprovalCallback}, {"sc:", a.handleSlashConfirmCallback},
		{"cl:", a.handleClarifyCallback}, {"update_prompt:", a.handleUpdatePromptCallback}} {
		if strings.HasPrefix(data, route.prefix) {
			return route.handler(ctx, q, data, cb)
		}
	}
	return nil
}

// Auth gates a tap then claims its pending entry, empty after answering when refused or expired
func claimCallbackState[K comparable](ctx context.Context, a *TelegramAdapter, q callbackQuery, cb callbackCtx, state map[K]string, key K,
	denial, resolved string, pop bool) (string, error) {
	authorized, err := a.callbackAuthorized(ctx, q, cb, denial)
	if err != nil || !authorized {
		return "", err
	}
	a.mu.Lock()
	sessionKey := state[key]
	if pop {
		delete(state, key)
	}
	a.mu.Unlock()
	if sessionKey == "" {
		return "", q.answer(ctx, resolved)
	}
	return sessionKey, nil
}

// Display name of the tapping user, the catalog fallback when unnamed
func userDisplay(ctx context.Context, q callbackQuery) string {
	if name := q.firstName(); name != "" {
		return name
	}
	return tr(ctx, "platform.telegram.user_fallback")
}

// Resolves an ea choice and approval id tap against the pending exec approval
func (a *TelegramAdapter) handleExecApprovalCallback(ctx context.Context, q callbackQuery, data string, cb callbackCtx) error {
	parts := strings.SplitN(data, ":", 3)
	if len(parts) != 3 {
		return nil
	}
	choice := parts[1]
	approvalID, err := pyIntOf(parts[2])
	if err != nil {
		return q.answer(ctx, toast(ctx, "platform.telegram.approval.toast_invalid_data", nil))
	}
	sessionKey, err := claimCallbackState(ctx, a, q, cb, a.approvalState, approvalID, unauthorizedToast(ctx),
		toast(ctx, "platform.telegram.approval.toast_already_resolved", nil), true)
	if sessionKey == "" {
		return err
	}
	user := userDisplay(ctx, q)
	count := tools.ResolveGatewayApproval(sessionKey, choice, false, "", "")
	slog.Info(fmt.Sprintf("Telegram button resolved %d approval(s) for session %s (choice=%s, user=%s)", count, sessionKey, choice, user))
	var label, editText string
	if count > 0 {
		labelKey, ok := map[string]string{"once": "resolved_once", "session": "resolved_session", "always": "resolved_always", "deny": "resolved_deny"}[choice]
		if !ok {
			labelKey = "resolved_generic"
		}
		label = tr(ctx, "platform.telegram.approval."+labelKey)
		editText = tr(ctx, "platform.telegram.approval.resolved_by_user", "label", label, "user", user)
	} else {
		label = tr(ctx, "platform.telegram.approval.expired")
		editText = tr(ctx, "platform.telegram.approval.expired_detail", "label", label)
	}
	if err := q.answer(ctx, clipRunes(label, toastLimit)); err != nil {
		return err
	}
	if err := editKeepingPrompt(ctx, a, q, editText); err != nil {
		return err
	}
	if count > 0 && cb.chatID != nil {
		a.ResumeTypingForChat(*cb.chatID)
	}
	return nil
}

// Resolves an sc choice and confirm id tap and sends the returned follow up in the prompt's topic
func (a *TelegramAdapter) handleSlashConfirmCallback(ctx context.Context, q callbackQuery, data string, cb callbackCtx) error {
	parts := strings.SplitN(data, ":", 3)
	if len(parts) != 3 {
		return nil
	}
	choice, confirmID := parts[1], parts[2]
	sessionKey, err := claimCallbackState(ctx, a, q, cb, a.slashConfirmState, confirmID, unauthorizedToast(ctx),
		toast(ctx, "platform.telegram.slash_confirm.already_resolved", nil), true)
	if sessionKey == "" {
		return err
	}
	labelKey, ok := map[string]string{"once": "slash_confirm.resolved_once", "always": "slash_confirm.resolved_always",
		"cancel": "slash_confirm.resolved_cancel"}[choice]
	if !ok {
		labelKey = "approval.resolved_generic"
	}
	user := userDisplay(ctx, q)
	label := tr(ctx, "platform.telegram."+labelKey)
	if err := q.answer(ctx, clipRunes(label, toastLimit)); err != nil {
		return err
	}
	if err := editKeepingPrompt(ctx, a, q, tr(ctx, "platform.telegram.approval.resolved_by_user", "label", label, "user", user)); err != nil {
		return err
	}
	if err := a.sendSlashConfirmResult(ctx, q, sessionKey, confirmID, choice); err != nil {
		slog.Error(fmt.Sprintf("[%s] slash-confirm callback failed: %s", a.Name(), pyrepr.ExceptionMessage(err)))
	}
	return nil
}

// Runs the stored slash confirm handler and sends its text into the prompt's topic
func (a *TelegramAdapter) sendSlashConfirmResult(ctx context.Context, q callbackQuery, sessionKey, confirmID, choice string) error {
	if gateway.SlashConfirmResolve == nil {
		return &runtimeError{msg: "gateway.SlashConfirmResolve unassigned"}
	}
	resultText := gateway.SlashConfirmResolve(ctx, sessionKey, confirmID, choice)
	m := q.message()
	if resultText == "" || m == nil {
		return nil
	}
	kw := textSend{chatID: m.Chat.ID}
	if m.MessageThreadID != 0 {
		threadID := strconv.Itoa(m.MessageThreadID)
		meta := map[string]any{"thread_id": threadID}
		if m.Chat.Type == models.ChatTypePrivate {
			id := m.ID
			kw.replyTo = &id
			meta["telegram_dm_topic_reply_fallback"] = true
		}
		thread, err := threadKwargsForSend(&threadID, meta, kw.replyTo, a.replyToMode)
		if err != nil {
			return err
		}
		kw.thread = thread
	}
	params := a.sendMessageParams(kw, a.FormatMessage(resultText), models.ParseModeMarkdown)
	params.DisableNotification = false
	_, err := a.sendMessageWithThreadFallback(ctx, params)
	return err
}

// Resolves a cl clarify id and index tap or flips the clarify to typed answer capture
func (a *TelegramAdapter) handleClarifyCallback(ctx context.Context, q callbackQuery, data string, cb callbackCtx) error {
	parts := strings.SplitN(data, ":", 3)
	if len(parts) != 3 {
		return nil
	}
	clarifyID, token := parts[1], parts[2]
	sessionKey, err := claimCallbackState(ctx, a, q, cb, a.clarifyState, clarifyID, unauthorizedToast(ctx),
		toast(ctx, "platform.telegram.slash_confirm.already_resolved", nil), false)
	if sessionKey == "" {
		return err
	}
	user := userDisplay(ctx, q)
	if token == "other" {
		if !tools.MarkClarifyAwaitingText(clarifyID) {
			a.mu.Lock()
			delete(a.clarifyState, clarifyID)
			a.mu.Unlock()
			a.notifyClarifyExpired(ctx, q)
			return nil
		}
		if err := q.answer(ctx, toast(ctx, "platform.telegram.prompt.type_answer", nil)); err != nil {
			return err
		}
		a.editHTMLQuiet(ctx, q, fmt.Sprintf("❓ %s\n\n<i>%s</i>", q.messageText(),
			pyhtml.Escape(tr(ctx, "platform.telegram.prompt.awaiting_typed", "user", user), true)))
		return nil
	}
	idx, convErr := pyIntOf(token)
	if convErr != nil {
		return q.answer(ctx, toast(ctx, "platform.telegram.prompt.invalid_choice", nil))
	}
	var resolvedText *string
	if entry := tools.GetPendingClarify(clarifyID); entry != nil && len(entry.Choices) > 0 && idx >= 0 && idx < len(entry.Choices) {
		choice := entry.Choices[idx]
		resolvedText = &choice
	}
	if resolvedText == nil {
		fallback := fmt.Sprintf("choice %d", idx+1)
		resolvedText = &fallback
	}
	a.mu.Lock()
	delete(a.clarifyState, clarifyID)
	a.mu.Unlock()
	if tools.ResolveGatewayClarify(clarifyID, *resolvedText) {
		if err := q.answer(ctx, "✓ "+clipRunes(*resolvedText, 60)); err != nil {
			return err
		}
		a.editHTMLQuiet(ctx, q, fmt.Sprintf("❓ %s\n\n<b>%s:</b> %s", pyhtml.Escape(q.messageText(), true), pyhtml.Escape(user, true),
			pyhtml.Escape(*resolvedText, true)))
		slog.Info(fmt.Sprintf("Telegram clarify button resolved (id=%s, choice=%s, user=%s)", clarifyID, pyrepr.Repr(*resolvedText), user))
		return nil
	}
	a.notifyClarifyExpired(ctx, q)
	slog.Warn(fmt.Sprintf("Telegram clarify button: resolve_gateway_clarify returned False (id=%s)", clarifyID))
	return nil
}

// Forwards an update_prompt y or n answer to the update process through the response file
func (a *TelegramAdapter) handleUpdatePromptCallback(ctx context.Context, q callbackQuery, data string, cb callbackCtx) error {
	answer := strings.SplitN(data, ":", 2)[1]
	authorized, err := a.callbackAuthorized(ctx, q, cb, unauthorizedToast(ctx))
	if err != nil || !authorized {
		return err
	}
	if err := q.answer(ctx, toast(ctx, "platform.telegram.prompt.update_sent", map[string]any{"answer": answer})); err != nil {
		return err
	}
	wordKey := "platform.telegram.prompt.negate_word"
	if answer == "y" {
		wordKey = "platform.telegram.prompt.affirm_word"
	}
	a.editMDQuiet(ctx, q, "☤ "+tr(ctx, "platform.telegram.prompt.update_answered", "answer", "*"+tr(ctx, wordKey)+"*"))
	responsePath := filepath.Join(home.Dir(ctx), ".update_response")
	tmp := strings.TrimSuffix(responsePath, filepath.Ext(responsePath)) + ".tmp"
	writeErr := os.WriteFile(tmp, []byte(answer), 0o644)
	if writeErr == nil {
		writeErr = os.Rename(tmp, responsePath)
	}
	if writeErr != nil {
		slog.Error(fmt.Sprintf("Failed to write update response from callback: %s", pyrepr.ExceptionMessage(writeErr)))
		return nil
	}
	slog.Info(fmt.Sprintf("Telegram update prompt answered '%s' by user %d", answer, q.q.From.ID))
	return nil
}

// Gmail triage verb dispatch, the script, its extra args, the success label key and whether the verb is sticky state
var gtVerbDispatch = map[string]struct {
	script     string
	extraArgs  []string
	successKey string
	isState    bool
}{
	"send": {"send-draft.sh", nil, "done_send", false}, "archive": {"archive.sh", nil, "done_archive", false},
	"draft": {"draft-blank.sh", nil, "done_draft", false}, "spam": {"spam.sh", nil, "done_spam", false},
	"mute": {"mute-add.sh", []string{"email"}, "done_mute", true}, "mute-domain": {"mute-add.sh", []string{"domain"}, "done_mute_domain", true},
	"trust":        {"trusted-ops-add.sh", []string{"email"}, "done_trust", true},
	"trust-domain": {"trusted-ops-add.sh", []string{"domain"}, "done_trust_domain", true},
	"vip":          {"vip-add.sh", []string{"email"}, "done_vip", true}, "vip-domain": {"vip-add.sh", []string{"domain"}, "done_vip_domain", true},
}

// Runs the gmail triage script of a gt verb and argument tap with a scrubbed environment
func (a *TelegramAdapter) handleGmailTriageCallback(ctx context.Context, q callbackQuery, data string, cb callbackCtx) error {
	parts := strings.SplitN(data, ":", 3)
	if len(parts) != 3 {
		return q.answer(ctx, toast(ctx, "platform.telegram.gmail_triage.invalid_data", nil))
	}
	verb, arg := parts[1], parts[2]
	authorized, err := a.callbackAuthorized(ctx, q, cb, unauthorizedToast(ctx))
	if err != nil || !authorized {
		return err
	}
	entry, ok := gtVerbDispatch[verb]
	if !ok {
		return q.answer(ctx, toast(ctx, "platform.telegram.gmail_triage.unknown_verb", map[string]any{"verb": verb}))
	}
	scriptPath := filepath.Join(home.Dir(ctx), "scripts", "gmail-triage", entry.script)
	if _, statErr := os.Stat(scriptPath); statErr != nil {
		if err := q.answer(ctx, toast(ctx, "platform.telegram.gmail_triage.script_missing", map[string]any{"script": entry.script})); err != nil {
			return err
		}
		slog.Error(fmt.Sprintf("[%s] gmail-triage script missing: %s", a.Name(), scriptPath))
		return nil
	}
	success := false
	label := ""
	env, envErr := tools.BuildSubprocessEnv(ctx, &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)},
		tools.SubprocessEnvOptions{StripLaunchProfile: true})
	if envErr != nil {
		label = tr(ctx, "platform.telegram.gmail_triage.error", "verb", verb, "error", pyrepr.ExceptionMessage(envErr))
		slog.Error(fmt.Sprintf("[%s] gmail-triage callback exception: verb=%s arg=%s err=%s", a.Name(), verb, arg, pyrepr.ExceptionMessage(envErr)))
	} else {
		runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		cmd := exec.CommandContext(runCtx, scriptPath, append([]string{arg}, entry.extraArgs...)...)
		cmd.Env = envList(env)
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &bytes.Buffer{}, &stderr
		runErr := cmd.Run()
		cancel()
		var exitErr *exec.ExitError
		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			label = tr(ctx, "platform.telegram.gmail_triage.timed_out", "verb", verb)
			slog.Error(fmt.Sprintf("[%s] gmail-triage callback timed out: verb=%s arg=%s", a.Name(), verb, arg))
		case runErr == nil:
			label = tr(ctx, "platform.telegram.gmail_triage."+entry.successKey)
			success = true
			slog.Info(fmt.Sprintf("[%s] gmail-triage callback ok: verb=%s arg=%s", a.Name(), verb, arg))
		case errors.As(runErr, &exitErr):
			stderrText := pystr.Strip(strings.ToValidUTF8(stderr.String(), "�"))
			lastLine := tr(ctx, "platform.telegram.gmail_triage.exit_code", "code", strconv.Itoa(exitErr.ExitCode()))
			if stderrText != "" {
				lines := pystr.Splitlines(stderrText, false)
				lastLine = lines[len(lines)-1]
			}
			label = tr(ctx, "platform.telegram.gmail_triage.failed", "verb", verb, "detail", clipRunes(lastLine, 80))
			slog.Error(fmt.Sprintf("[%s] gmail-triage callback failed: verb=%s arg=%s rc=%d stderr=%s", a.Name(), verb, arg, exitErr.ExitCode(), stderrText))
		default:
			label = tr(ctx, "platform.telegram.gmail_triage.error", "verb", verb, "error", pyrepr.ExceptionMessage(runErr))
			slog.Error(fmt.Sprintf("[%s] gmail-triage callback exception: verb=%s arg=%s err=%s", a.Name(), verb, arg, pyrepr.ExceptionMessage(runErr)))
		}
	}
	if err := q.answer(ctx, clipRunes(label, toastLimit)); err != nil {
		return err
	}
	if !success {
		return nil
	}
	original := q.messageText()
	appended := original + "\n" + tr(ctx, "platform.telegram.gmail_triage.appended_by", "label", label, "user", userDisplay(ctx, q))
	_ = q.editMessageText(ctx, appended, "", nil)
	return nil
}

// KEY=VALUE list of an environment map
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
