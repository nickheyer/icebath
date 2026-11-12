package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Action ids of the model picker select menus and buttons
const (
	modelPickerProviderAction = "hermes_model_provider"
	modelPickerModelAction    = "hermes_model_model"
	modelPickerBackAction     = "hermes_model_back"
	modelPickerCancelAction   = "hermes_model_cancel"
)

var modelPickerActionIDs = []string{modelPickerProviderAction, modelPickerModelAction, modelPickerBackAction, modelPickerCancelAction}

// Approval button action ids mapped to their choices, in declaration order
var (
	approvalActionOrder = []string{"hermes_approve_once", "hermes_approve_session", "hermes_approve_always", "hermes_deny"}
	approvalChoices     = map[string]string{
		"hermes_approve_once": "once", "hermes_approve_session": "session",
		"hermes_approve_always": "always", "hermes_deny": "deny"}
	approvalDecisionKeys = map[string]string{
		"once": "platform.slack.approval.resolved_once", "session": "platform.slack.approval.resolved_session",
		"always": "platform.slack.approval.resolved_always", "deny": "platform.slack.approval.resolved_deny"}
	confirmActionOrder = []string{"hermes_confirm_once", "hermes_confirm_always", "hermes_confirm_cancel"}
	confirmChoices     = map[string]string{
		"hermes_confirm_once": "once", "hermes_confirm_always": "always", "hermes_confirm_cancel": "cancel"}
	confirmDecisionKeys = map[string]string{
		"once": "platform.slack.approval.resolved_once", "always": "platform.slack.confirm.resolved_always",
		"cancel": "platform.slack.confirm.resolved_cancel"}
	eaActionIDs = map[string]string{"once": "hermes_approve_once", "session": "hermes_approve_session",
		"always": "hermes_approve_always", "deny": "hermes_deny"}
)

// Section text cap past which Slack rejects the blocks and no buttons render
const eaSectionCap = 3000

// Live clarify card of a clarify id, channel then ts then rendered question
type clarifyTarget struct {
	channelID, msgTS, question string
}

// Model picker context of one posted picker message
type modelPickerState struct {
	providers            []map[string]any
	sessionKey           string
	chatID               string
	teamID               string
	currentModel         string
	currentProvider      string
	onModelSelected      func(ctx context.Context, chatID, modelID, providerSlug string) (string, error)
	stage                string
	selectedProviderSlug string
}

// Value under key popped from the dict, def when absent
func dictPop[K comparable, V any](d *pydict.Dict[K, V], key K, def V) V {
	v, ok := d.Get(key)
	if !ok {
		return def
	}
	d.Delete(key)
	return v
}

// Block Kit button element, style and emoji keys only present when set
func button(text, actionID, value, style string, emoji bool) map[string]any {
	textObj := map[string]any{"type": "plain_text", "text": text}
	if emoji {
		textObj["emoji"] = true
	}
	btn := map[string]any{"type": "button", "text": textObj}
	if style != "" {
		btn["style"] = style
	}
	btn["action_id"] = actionID
	btn["value"] = value
	return btn
}

// chat.postMessage of blocks threaded through the metadata, returning the raw response
func (a *SlackAdapter) postInteractiveBlocks(ctx context.Context, chatID, text string, blocks []any, metadata map[string]any, sanitize, teamScoped bool) (*apiResult, error) {
	if sanitize {
		blocks = SanitizeBlocks(blocks)
	}
	kwargs := map[string]any{"channel": chatID, "text": text, "blocks": blocks}
	if threadTS := a.resolveThreadTS("", metadata); threadTS != "" {
		kwargs["thread_ts"] = threadTS
	}
	teamID := ""
	if teamScoped {
		teamID = metadataTeamID(metadata)
	}
	return a.getClient(chatID, teamID).call(ctx, "chat.postMessage", kwargs)
}

// Shared Block Kit prompt sender marking the posted message unresolved for the double click guard
func (a *SlackAdapter) sendInteractivePrompt(ctx context.Context, chatID string, metadata map[string]any, build func() (string, []any), label string,
	resolved *pydict.Dict[any, bool], resolvedMax int, teamScopedKey, sanitize bool) gateway.SendResult {
	if a.currentApp() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}
	}
	chatID = a.dmTarget(ctx, chatID, metadata)
	text, blocks := build()
	result, err := a.postInteractiveBlocks(ctx, chatID, text, blocks, metadata, sanitize, teamScopedKey)
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] %s failed: %v", label, err))
		return gateway.SendResult{Success: false, Error: err.Error()}
	}
	msgTS := pyrepr.Str(getOr(result.Data, "ts", ""))
	if msgTS != "" && resolved != nil {
		var key any = msgTS
		if teamScopedKey {
			key = workspaceMessageMarker(metadataTeamID(metadata), msgTS)
		}
		a.mu.Lock()
		resolved.Set(key, false)
		trimOldestDictEntries(resolved, resolvedMax)
		a.mu.Unlock()
	}
	return gateway.SendResult{Success: true, MessageID: msgTS, RawResponse: result.Data}
}

// Warning emoji and the bold localized approval header
func (a *SlackAdapter) EAHeader(ctx context.Context) string {
	return ":warning: *" + i18n.T(ctx, "gateway.exec_approval.header", "", nil) + "*\n"
}

// Smart deny sentence with its label in bold on a new line
func (a *SlackAdapter) EASmartDenyLine(ctx context.Context) string {
	line := i18n.T(ctx, "gateway.exec_approval.smart_deny_line", "", nil)
	label, rest, sep := strings.Cut(line, ":")
	if sep {
		return "\n*" + label + ":*" + rest
	}
	return "\n" + line
}

// Command preview budget left under the section cap once every fixed part is measured
func (a *SlackAdapter) ExecApprovalCmdBudget(ctx context.Context, description string, smartDenied bool) int {
	fixed := pystr.Len(a.EAHeader(ctx)) + pystr.Len(a.Traits.EACodeOpen) + pystr.Len(a.Traits.EACodeClose) +
		pystr.Len(a.EAReasonLabel(ctx)) + pystr.Len(description) + pystr.Len("...") + pystr.Len(a.EADeadlineLine(ctx))
	if smartDenied {
		fixed += pystr.Len(a.EASmartDenyLine(ctx))
	}
	return max(0, eaSectionCap-fixed)
}

// Block Kit approval prompt whose buttons resolve the waiting approval
func (a *SlackAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	build := func() (string, []any) {
		actions := make([]any, 0, len(prompt.Actions))
		for _, act := range prompt.Actions {
			actions = append(actions, button(act.Label, eaActionIDs[act.Choice], prompt.SessionKey, act.Style, false))
		}
		blocks := []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": prompt.Text}},
			map[string]any{"type": "actions", "elements": actions}}
		return i18n.T(ctx, "platform.slack.approval.fallback_text", "", map[string]any{"command": pystr.Slice(prompt.Command, 0, 100)}), blocks
	}
	return a.sendInteractivePrompt(ctx, prompt.ChatID, prompt.Metadata, build, "send_exec_approval", a.approvalResolved, approvalResolvedMax, true, true), nil
}

// Three option Block Kit slash command confirmation prompt
func (a *SlackAdapter) SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (gateway.SendResult, error) {
	build := func() (string, []any) {
		t := title
		if t == "" {
			t = i18n.T(ctx, "platform.slack.confirm.default_title", "", nil)
		}
		t = pystr.Slice(t, 0, 150)
		budget := 3000 - pystr.Len("*"+t+"*\n\n") - pystr.Len("...")
		body := message
		if pystr.Len(message) > budget {
			body = pystr.Slice(message, 0, budget) + "..."
		}
		value := sessionKey + "|" + confirmID
		label := func(key string) string { return pystr.Slice(i18n.T(ctx, key, "", nil), 0, 75) }
		blocks := []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*" + t + "*\n\n" + body}},
			map[string]any{"type": "actions", "elements": []any{
				button(label("platform.slack.confirm.approve_once"), "hermes_confirm_once", value, "primary", false),
				button(label("platform.slack.confirm.always_approve"), "hermes_confirm_always", value, "", false),
				button(label("platform.slack.confirm.cancel"), "hermes_confirm_cancel", value, "danger", false)}}}
		return t + ": " + pystr.Slice(body, 0, 100), blocks
	}
	return a.sendInteractivePrompt(ctx, chatID, metadata, build, "send_slash_confirm", nil, 0, true, true), nil
}

// Plain text option object of a select menu
func pickerOption(text string, idx int) map[string]any {
	return map[string]any{"text": map[string]any{"type": "plain_text", "text": text, "emoji": true}, "value": strconv.Itoa(idx)}
}

// Danger styled cancel button of the model picker
func pickerCancelButton(ctx context.Context) map[string]any {
	return map[string]any{"type": "button",
		"text":  map[string]any{"type": "plain_text", "text": pystr.Slice(i18n.T(ctx, "platform.slack.picker.cancel", "", nil), 0, 75), "emoji": true},
		"style": "danger", "action_id": modelPickerCancelAction, "value": "cancel"}
}

// Number of models a provider row advertises, total_models when present
func providerModelCount(p map[string]any) any {
	if v, ok := p["total_models"]; ok {
		return v
	}
	return int64(len(asList(p["models"])))
}

// Provider stage blocks, a header section and a provider select with a cancel button
func (a *SlackAdapter) buildModelPickerProviderBlocks(ctx context.Context, providers []map[string]any, currentModel, providerLabel string) []any {
	var options []any
	for idx, p := range providers {
		if idx >= 100 {
			break
		}
		options = append(options, pickerOption(pystr.Slice(fmt.Sprintf("%s (%s models)", pyrepr.Str(p["name"]), pyrepr.Str(providerModelCount(p))), 0, 75), idx))
	}
	extra := ""
	if len(providers) > 100 {
		extra = i18n.T(ctx, "platform.slack.picker.more_available", "", map[string]any{"count": strconv.Itoa(len(providers) - 100)})
	}
	model := currentModel
	if model == "" {
		model = i18n.T(ctx, "platform.shared.unknown", "", nil)
	}
	sectionText := i18n.T(ctx, "platform.slack.picker.provider_header", "", map[string]any{"model": model, "provider": providerLabel, "extra": extra})
	return []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": pystr.Slice(sectionText, 0, 3000)}},
		map[string]any{"type": "actions", "elements": []any{
			map[string]any{"type": "static_select",
				"placeholder": map[string]any{"type": "plain_text", "text": pystr.Slice(i18n.T(ctx, "platform.slack.picker.choose_provider", "", nil), 0, 150), "emoji": true},
				"action_id":   modelPickerProviderAction, "options": options},
			pickerCancelButton(ctx)}}}
}

// Provider row with the slug, nil when none matches
func pickerProvider(providers []map[string]any, slug string) map[string]any {
	for _, p := range providers {
		if pyrepr.Str(p["slug"]) == slug {
			return p
		}
	}
	return nil
}

// Model ids of a provider row as strings
func providerModels(p map[string]any) []string {
	var out []string
	for _, m := range asList(p["models"]) {
		out = append(out, pyrepr.Str(m))
	}
	return out
}

// Model stage blocks, a header section and a model select with back and cancel buttons
func (a *SlackAdapter) buildModelPickerModelBlocks(ctx context.Context, providers []map[string]any, providerSlug string) []any {
	provider := pickerProvider(providers, providerSlug)
	pname := providerSlug
	if provider != nil {
		pname = pyrepr.Str(getOr(provider, "name", providerSlug))
	}
	models := providerModels(provider)
	if len(models) > 100 {
		models = models[:100]
	}
	var options []any
	for idx, modelID := range models {
		short := modelID
		if strings.Contains(modelID, "/") {
			parts := strings.Split(modelID, "/")
			short = parts[len(parts)-1]
		}
		var row any
		if provider != nil {
			row = provider
		}
		suffix := providerPickerRowSuffix(row, modelID)
		text := pystr.Slice(short, 0, 75)
		if suffix != "" {
			text = pystr.Slice(short, 0, 75-pystr.Len(suffix)-3) + " · " + suffix
		}
		options = append(options, pickerOption(text, idx))
	}
	total := int64(len(models))
	if provider != nil {
		if v, ok := provider["total_models"]; ok {
			total = intValue(v)
		}
	}
	extra := ""
	if total > int64(len(models)) {
		extra = i18n.T(ctx, "platform.slack.picker.more_available", "", map[string]any{"count": strconv.FormatInt(total-int64(len(models)), 10)})
	}
	planLine := ""
	if provider != nil && pyrepr.Truthy(provider["plan_line"]) {
		planLine = "⚠️ " + pyrepr.Str(provider["plan_line"]) + "\n"
	}
	sectionText := planLine + i18n.T(ctx, "platform.slack.picker.model_header", "", map[string]any{"provider": pname, "extra": extra})
	elements := []any{map[string]any{"type": "static_select",
		"placeholder": map[string]any{"type": "plain_text", "text": pystr.Slice(i18n.T(ctx, "platform.slack.picker.choose_model", "", map[string]any{"provider": pname}), 0, 150), "emoji": true},
		"action_id":   modelPickerModelAction, "options": options}}
	if providerSlug != "" {
		elements = append(elements, map[string]any{"type": "button",
			"text":      map[string]any{"type": "plain_text", "text": pystr.Slice(i18n.T(ctx, "platform.slack.picker.back", "", nil), 0, 75), "emoji": true},
			"action_id": modelPickerBackAction, "value": providerSlug})
	}
	elements = append(elements, pickerCancelButton(ctx))
	return []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": pystr.Slice(sectionText, 0, 3000)}},
		map[string]any{"type": "actions", "elements": elements}}
}

// Approval resolution of a session, a package var the way hermes tests patch resolve_gateway_approval
var resolveGatewayApprovalFn = tools.ResolveGatewayApproval

// Window plan suffix of a picker row, a package var the way hermes tests patch picker_row_suffix
var providerPickerRowSuffix = provider.PickerRowSuffix

// Provider display label, a package var the way hermes tests patch get_label
var providerLabelFn = provider.GetProviderLabel

// Interactive two step provider then model Block Kit picker
func (a *SlackAdapter) SendModelPicker(ctx context.Context, chatID string, providers []map[string]any, currentModel, currentProvider, sessionKey string,
	onModelSelected func(ctx context.Context, chatID, modelID, providerSlug string) (string, error), metadata map[string]any) (gateway.SendResult, error) {
	if a.currentApp() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	teamID := metadataTeamID(metadata)
	chatID = a.ensureDMConversation(ctx, chatID, teamID)
	threadTS := a.resolveThreadTS("", metadata)
	providerLabel := providerLabelFn(ctx, currentProvider)
	if len(providers) == 0 {
		return gateway.SendResult{Success: false, Error: "No providers available"}, nil
	}
	blocks := a.buildModelPickerProviderBlocks(ctx, providers, currentModel, providerLabel)
	kwargs := map[string]any{"channel": chatID, "text": i18n.T(ctx, "platform.slack.picker.fallback_provider", "", nil), "blocks": SanitizeBlocks(blocks)}
	if threadTS != "" {
		kwargs["thread_ts"] = threadTS
	}
	result, err := a.getClient(chatID, teamID).call(ctx, "chat.postMessage", kwargs)
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] send_model_picker failed: %v", err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	msgTS := pyrepr.Str(getOr(result.Data, "ts", ""))
	if msgTS == "" {
		return gateway.SendResult{Success: false, Error: "No message timestamp returned"}, nil
	}
	a.mu.Lock()
	a.modelPickerState.Set(workspaceMessageMarker(teamID, msgTS), &modelPickerState{providers: providers, sessionKey: sessionKey, chatID: chatID,
		teamID: teamID, currentModel: currentModel, currentProvider: currentProvider, onModelSelected: onModelSelected, stage: "provider"})
	trimOldestDictEntries(a.modelPickerState, modelPickerStateMax)
	a.mu.Unlock()
	return gateway.SendResult{Success: true, MessageID: msgTS, RawResponse: result.Data}, nil
}

// Replaces the picker message body with a plain section without controls
func (a *SlackAdapter) updatePickerMessage(ctx context.Context, channelID, teamID, msgTS, sectionText string) {
	text := pystr.Slice(sectionText, 0, 3000)
	_, err := a.getClient(channelID, teamID).call(ctx, "chat.update", map[string]any{"channel": channelID, "ts": msgTS, "text": text,
		"blocks": SanitizeBlocks([]any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}})})
	if err != nil {
		slog.Warn(fmt.Sprintf("[Slack] Failed to update model picker message: %v", err))
	}
}

// Model picker interaction advancing, going back, cancelling or switching the model
func (a *SlackAdapter) handleModelPickerAction(ctx context.Context, ack ackFunc, body, action map[string]any) {
	_ = ack(nil)
	teamID := eventTeamID(map[string]any{}, body)
	actionID := pyrepr.Str(getOr(action, "action_id", ""))
	msgTS := pyrepr.Str(getOr(asMap(getOr(body, "message", map[string]any{})), "ts", ""))
	channelID := pyrepr.Str(getOr(asMap(getOr(body, "channel", map[string]any{})), "id", ""))
	user := asMap(getOr(body, "user", map[string]any{}))
	userName := pyrepr.Str(getOr(user, "name", "unknown"))
	userID := pyrepr.Str(getOr(user, "id", ""))
	if !a.isInteractiveUserAuthorized(ctx, userID, channelID, &userName, teamID) {
		slog.Warn(fmt.Sprintf("[Slack] Unauthorized model picker click by %s (%s) - ignoring", userName, userID))
		return
	}
	var marker any = workspaceMessageMarker(teamID, msgTS)
	a.mu.Lock()
	if a.modelPickerState.Has(msgTS) {
		marker = msgTS
	}
	state, _ := a.modelPickerState.Get(marker)
	a.mu.Unlock()
	if state == nil {
		slog.Debug(fmt.Sprintf("[Slack] Model picker state not found for marker=%s", pyrepr.Repr(marker)))
		a.updatePickerMessage(ctx, channelID, teamID, msgTS, modelPickerExpiredNotice(ctx))
		return
	}
	dropState := func() {
		a.mu.Lock()
		a.modelPickerState.Delete(marker)
		a.mu.Unlock()
	}
	providers := state.providers
	switch {
	case actionID == modelPickerCancelAction:
		dropState()
		a.updatePickerMessage(ctx, channelID, teamID, msgTS, i18n.T(ctx, "platform.slack.picker.cancelled", "", nil))
	case actionID == modelPickerProviderAction:
		idxToken := getOr(asMap(getOr(action, "selected_option", map[string]any{})), "value", "")
		var picked map[string]any
		if idx, ok := pyIndex(idxToken); ok && idx >= 0 && idx < len(providers) {
			picked = providers[idx]
		}
		if picked == nil {
			slog.Warn(fmt.Sprintf("[Slack] Invalid provider picker index token: %s", pyrepr.Repr(idxToken)))
			dropState()
			a.updatePickerMessage(ctx, channelID, teamID, msgTS, modelPickerExpiredNotice(ctx))
			return
		}
		providerSlug := pyrepr.Str(getOr(picked, "slug", ""))
		if !pyrepr.Truthy(picked["models"]) {
			a.updatePickerMessage(ctx, channelID, teamID, msgTS, i18n.T(ctx, "platform.slack.picker.no_models_for_provider", "", map[string]any{"provider": providerSlug}))
			dropState()
			return
		}
		a.mu.Lock()
		state.stage, state.selectedProviderSlug = "model", providerSlug
		a.mu.Unlock()
		blocks := a.buildModelPickerModelBlocks(ctx, providers, providerSlug)
		_, err := a.getClient(channelID, teamID).call(ctx, "chat.update", map[string]any{"channel": channelID, "ts": msgTS,
			"text":   i18n.T(ctx, "platform.slack.picker.fallback_model", "", map[string]any{"provider": pyrepr.Str(getOr(picked, "name", providerSlug))}),
			"blocks": SanitizeBlocks(blocks)})
		if err != nil {
			slog.Warn(fmt.Sprintf("[Slack] Failed to update model picker (provider→model): %v", err))
		}
	case actionID == modelPickerBackAction:
		a.mu.Lock()
		state.stage, state.selectedProviderSlug = "provider", ""
		a.mu.Unlock()
		providerLabel := providerLabelFn(ctx, state.currentProvider)
		blocks := a.buildModelPickerProviderBlocks(ctx, providers, state.currentModel, providerLabel)
		_, err := a.getClient(channelID, teamID).call(ctx, "chat.update", map[string]any{"channel": channelID, "ts": msgTS,
			"text": i18n.T(ctx, "platform.slack.picker.fallback_provider", "", nil), "blocks": SanitizeBlocks(blocks)})
		if err != nil {
			slog.Warn(fmt.Sprintf("[Slack] Failed to update model picker (back): %v", err))
		}
	case actionID == modelPickerModelAction && state.stage == "model":
		idxToken := getOr(asMap(getOr(action, "selected_option", map[string]any{})), "value", "")
		providerSlug := state.selectedProviderSlug
		models := providerModels(pickerProvider(providers, providerSlug))
		modelID := ""
		found := false
		if idx, ok := pyIndex(idxToken); ok && idx >= 0 && idx < len(models) {
			modelID, found = models[idx], true
		}
		if !found {
			slog.Warn(fmt.Sprintf("[Slack] Invalid model picker index token: %s", pyrepr.Repr(idxToken)))
			dropState()
			a.updatePickerMessage(ctx, channelID, teamID, msgTS, modelPickerExpiredNotice(ctx))
			return
		}
		if state.onModelSelected == nil {
			dropState()
			a.updatePickerMessage(ctx, channelID, teamID, msgTS, modelPickerExpiredNotice(ctx))
			return
		}
		dropState()
		a.updatePickerMessage(ctx, channelID, teamID, msgTS, i18n.T(ctx, "platform.slack.picker.switching", "", map[string]any{"model": modelID}))
		switchFailed := false
		confirmation, err := state.onModelSelected(ctx, state.chatID, modelID, providerSlug)
		if err != nil {
			slog.Error(fmt.Sprintf("[Slack] Model picker callback failed: %v", err))
			confirmation = i18n.T(ctx, "platform.slack.picker.switch_failed", "", map[string]any{"error": err.Error()})
			switchFailed = true
		} else if prefix := pystr.Strip(i18n.T(ctx, "gateway.model.error_prefix", "", map[string]any{"error": ""})); prefix != "" && strings.HasPrefix(confirmation, prefix) {
			switchFailed = true
		}
		headerKey := "platform.slack.picker.switched_header"
		if switchFailed {
			headerKey = "platform.slack.picker.switch_failed_header"
		}
		a.updatePickerMessage(ctx, channelID, teamID, msgTS, i18n.T(ctx, headerKey, "", nil)+"\n\n"+confirmation)
	}
}

// Python int() of a picker index token, false when it does not parse
func pyIndex(token any) (int, bool) {
	switch v := token.(type) {
	case int64:
		return int(v), true
	case string:
		i, err := strconv.Atoi(pystr.Strip(v))
		return i, err == nil
	}
	return 0, false
}

// Clarify prompt as one button per choice plus an Other button, the base text prompt without choices
func (a *SlackAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	if len(choices) == 0 {
		return a.BasePlatformAdapter.SendClarify(ctx, chatID, question, choices, clarifyID, sessionKey, metadata)
	}
	build := func() (string, []any) {
		q := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(question)
		body := "❓ " + q
		budget := 3000 - pystr.Len("...")
		if pystr.Len(body) > budget {
			body = pystr.Slice(body, 0, budget) + "..."
		}
		var elements []any
		for idx, choice := range choices {
			label := pystr.Strip(choice)
			if label == "" {
				label = i18n.T(ctx, "platform.slack.clarify.option_n", "", map[string]any{"n": strconv.Itoa(idx + 1)})
			}
			elements = append(elements, button(pystr.Slice(label, 0, 75), fmt.Sprintf("hermes_clarify_choice_%d", idx), fmt.Sprintf("%s|%d", clarifyID, idx), "", true))
		}
		elements = append(elements, button(pystr.Slice(i18n.T(ctx, "platform.slack.clarify.other", "", nil), 0, 75), "hermes_clarify_other", clarifyID+"|other", "", true))
		blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": body}}}
		for start := 0; start < len(elements); start += 5 {
			blocks = append(blocks, map[string]any{"type": "actions", "elements": elements[start:min(start+5, len(elements))]})
		}
		return body, blocks
	}
	result := a.sendInteractivePrompt(ctx, chatID, metadata, build, "send_clarify", a.clarifyResolved, clarifyResolvedMax, false, false)
	if msgID, _ := result.MessageID.(string); result.Success && msgID != "" {
		questionText, _ := build()
		responseChannel := strOrEmpty(asMap(result.RawResponse)["channel"])
		if responseChannel == "" {
			responseChannel = chatID
		}
		a.mu.Lock()
		a.clarifyMessages.Set(clarifyID, clarifyTarget{responseChannel, msgID, questionText})
		trimOldestDictEntries(a.clarifyMessages, clarifyMessageMax)
		a.mu.Unlock()
	}
	return result, nil
}

// Whether a Slack interactive caller may perform gated actions, injected check then runner then env
func (a *SlackAdapter) isInteractiveUserAuthorized(ctx context.Context, userID, channelID string, userName *string, teamID string) bool {
	normalized := pystr.Strip(userID)
	if normalized == "" {
		return false
	}
	chatType := "group"
	if strings.HasPrefix(channelID, "D") {
		chatType = "dm"
	}
	if a.HasAuthorizationCheck() {
		if injected := a.IsSenderAuthorized(normalized, &chatType, &channelID, gateway.AuthorizationOpts{}); injected != nil {
			return *injected
		}
	}
	if auth := a.runnerAuthFn(); auth != nil {
		chatID := channelID
		if chatID == "" {
			chatID = normalized
		}
		source := &gateway.SessionSource{Platform: gateway.PlatformSlack, ChatID: chatID, ChatType: chatType, UserID: &normalized}
		if userName != nil && *userName != "" {
			name := pystr.Strip(*userName)
			source.UserName = &name
		}
		if teamID != "" {
			source.ScopeID = &teamID
		}
		ok, err := auth.IsUserAuthorized(ctx, source, true)
		if err == nil {
			return ok
		}
		slog.Debug(fmt.Sprintf("[Slack] Falling back to env-only interactive auth for user %s: %v", normalized, err))
	}
	env := func(name string) string { return pystr.Lower(gateway.PlatformGateEnv(ctx, name, "")) }
	switch env("SLACK_ALLOW_ALL_USERS") {
	case "true", "1", "yes":
		return true
	}
	allowed := map[string]bool{}
	for _, name := range []string{"SLACK_ALLOWED_USERS", "GATEWAY_ALLOWED_USERS"} {
		for _, uid := range strings.Split(gateway.PlatformGateEnv(ctx, name, ""), ",") {
			if uid = pystr.Strip(uid); uid != "" {
				allowed[uid] = true
			}
		}
	}
	if len(allowed) > 0 {
		return allowed["*"] || allowed[normalized]
	}
	switch env("GATEWAY_ALLOW_ALL_USERS") {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Unpacked Block Kit interaction fields
type interaction struct {
	teamID, actionID, value string
	message                 map[string]any
	msgTS, channelID        string
	userName, userID        string
}

// Action id, value, message, ts, channel, user name and user id of an interaction payload
func interactionFields(body, action map[string]any) interaction {
	message := asMap(getOr(body, "message", map[string]any{}))
	user := asMap(getOr(body, "user", map[string]any{}))
	return interaction{
		actionID: pyrepr.Str(getOr(action, "action_id", "")), value: pyrepr.Str(getOr(action, "value", "")),
		message: message, msgTS: pyrepr.Str(getOr(message, "ts", "")),
		channelID: pyrepr.Str(getOr(asMap(getOr(body, "channel", map[string]any{})), "id", "")),
		userName:  pyrepr.Str(getOr(user, "name", "unknown")), userID: pyrepr.Str(getOr(user, "id", ""))}
}

// Acks a button click, unpacks it and authorizes the clicker, nil when unauthorized
func (a *SlackAdapter) beginInteraction(ctx context.Context, ack ackFunc, body, action map[string]any, kind string, teamScoped bool) *interaction {
	_ = ack(nil)
	in := interactionFields(body, action)
	in.teamID = eventTeamID(map[string]any{}, body)
	authTeam := ""
	if teamScoped {
		authTeam = in.teamID
	}
	if !a.isInteractiveUserAuthorized(ctx, in.userID, in.channelID, &in.userName, authTeam) {
		slog.Warn(fmt.Sprintf("[Slack] Unauthorized %s click by %s (%s) - ignoring", kind, in.userName, in.userID))
		return nil
	}
	return &in
}

// First section block text truncated to the limit, untruncated for a negative limit
func sectionText(message map[string]any, limit int) string {
	original := ""
	for _, raw := range asList(getOr(message, "blocks", []any{})) {
		block := asMap(raw)
		if block["type"] == "section" {
			original = pyrepr.Str(getOr(asMap(firstTruthyValue(block["text"], map[string]any{})), "text", ""))
			break
		}
	}
	if limit >= 0 {
		return pystr.Slice(original, 0, limit)
	}
	return original
}

// Rewrites a button prompt to show the outcome and drop the buttons
func (a *SlackAdapter) finalizeInteractiveMessage(ctx context.Context, channelID, msgTS, originalText, decisionText, fallbackText, label, teamID string, sanitize bool) {
	text := originalText
	if text == "" {
		text = fallbackText
	}
	blocks := []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
		map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": decisionText}}}}
	if sanitize {
		blocks = SanitizeBlocks(blocks)
	}
	if _, err := a.getClient(channelID, teamID).call(ctx, "chat.update", map[string]any{"channel": channelID, "ts": msgTS, "text": decisionText, "blocks": blocks}); err != nil {
		slog.Warn(fmt.Sprintf("[Slack] Failed to update %s message: %v", label, err))
	}
}

// Slash confirm button click resolving the pending confirmation and posting its result
func (a *SlackAdapter) handleSlashConfirmAction(ctx context.Context, ack ackFunc, body, action map[string]any) {
	in := a.beginInteraction(ctx, ack, body, action, "slash-confirm", true)
	if in == nil {
		return
	}
	sessionKey, confirmID, ok := strings.Cut(in.value, "|")
	if !ok {
		slog.Warn("[Slack] Malformed slash-confirm value: " + in.value)
		return
	}
	choice, known := confirmChoices[in.actionID]
	if !known {
		choice = "cancel"
	}
	decisionText := i18n.T(ctx, confirmDecisionKeys[choice], "", map[string]any{"user": in.userName})
	a.finalizeInteractiveMessage(ctx, in.channelID, in.msgTS, sectionText(in.message, 3000), decisionText, "Confirmation prompt", "slash-confirm", in.teamID, true)
	if gateway.SlashConfirmResolve == nil {
		slog.Error("Failed to resolve slash-confirm from Slack button: slash confirm resolver is not installed")
		return
	}
	if resultText := gateway.SlashConfirmResolve(ctx, sessionKey, confirmID, choice); resultText != "" {
		kwargs := map[string]any{"channel": in.channelID, "text": resultText}
		threadTS := strOrEmpty(in.message["thread_ts"])
		if threadTS == "" {
			threadTS = in.msgTS
		}
		if threadTS != "" {
			kwargs["thread_ts"] = threadTS
		}
		if _, err := a.getClient(in.channelID, in.teamID).call(ctx, "chat.postMessage", kwargs); err != nil {
			slog.Error(fmt.Sprintf("Failed to resolve slash-confirm from Slack button: %v", err))
			return
		}
	}
	slog.Info(fmt.Sprintf("Slack button resolved slash-confirm for session %s (choice=%s, user=%s)", sessionKey, choice, in.userName))
}

// Acks Slack AI feedback button clicks and logs the choice
func (a *SlackAdapter) handleFeedbackAction(_ context.Context, ack ackFunc, body, action map[string]any) {
	_ = ack(nil)
	value := strOrEmpty(action["value"])
	message := asMap(firstTruthyValue(body["message"], map[string]any{}))
	channelID := pyrepr.Str(getOr(asMap(firstTruthyValue(body["channel"], map[string]any{})), "id", ""))
	userID := pyrepr.Str(getOr(asMap(firstTruthyValue(body["user"], map[string]any{})), "id", ""))
	slog.Info(fmt.Sprintf("[Slack] Feedback button clicked: value=%s user=%s channel=%s ts=%s", value, userID, channelID, pyrepr.Str(getOr(message, "ts", ""))))
}

// Approval button click resolving the waiting approval once and rewriting the prompt
func (a *SlackAdapter) handleApprovalAction(ctx context.Context, ack ackFunc, body, action map[string]any) {
	in := a.beginInteraction(ctx, ack, body, action, "approval", true)
	if in == nil {
		return
	}
	sessionKey := in.value
	choice, known := approvalChoices[in.actionID]
	if !known {
		choice = "deny"
	}
	var approvalKey any = workspaceMessageMarker(in.teamID, in.msgTS)
	a.mu.Lock()
	if a.approvalResolved.Has(in.msgTS) {
		approvalKey = in.msgTS
	}
	alreadyResolved := dictPop(a.approvalResolved, approvalKey, true)
	a.mu.Unlock()
	if alreadyResolved {
		return
	}
	count := resolveGatewayApprovalFn(sessionKey, choice, false, "", "")
	slog.Info(fmt.Sprintf("Slack button resolved %d approval(s) for session %s (choice=%s, user=%s)", count, sessionKey, choice, in.userName))
	decisionText := i18n.T(ctx, approvalDecisionKeys[choice], "", map[string]any{"user": in.userName})
	if count == 0 {
		decisionText = i18n.T(ctx, "platform.shared.approval_expired", "", nil)
	}
	original := strings.ReplaceAll(sectionText(in.message, 3000), a.EADeadlineLine(ctx), "")
	a.finalizeInteractiveMessage(ctx, in.channelID, in.msgTS, original, decisionText, "Command approval request", "approval", in.teamID, true)
}

// Rewrites a clarify message to show the outcome and drop the buttons
func (a *SlackAdapter) updateClarifyMessage(ctx context.Context, channelID, msgTS, questionText, decisionText string) {
	a.finalizeInteractiveMessage(ctx, channelID, msgTS, questionText, decisionText, "Clarification", "clarify", "", false)
}

// Rewrites a still live clarify card into a terminal state once the gateway ends it without a click
func (a *SlackAdapter) RetireClarifyCard(ctx context.Context, clarifyID, notice string) error {
	a.mu.Lock()
	target, ok := a.clarifyMessages.Get(clarifyID)
	if ok {
		a.clarifyMessages.Delete(clarifyID)
		a.clarifyResolved.Set(target.msgTS, true)
	}
	a.mu.Unlock()
	if !ok {
		return nil
	}
	a.updateClarifyMessage(ctx, target.channelID, target.msgTS, target.question, notice)
	return nil
}

// Clarify choice or Other click resolving the clarify or switching it to text capture
func (a *SlackAdapter) handleClarifyAction(ctx context.Context, ack ackFunc, body, action map[string]any) {
	in := a.beginInteraction(ctx, ack, body, action, "clarify", false)
	if in == nil {
		return
	}
	clarifyID, token, ok := strings.Cut(in.value, "|")
	if !ok {
		slog.Warn("[Slack] Malformed clarify value: " + in.value)
		return
	}
	a.mu.Lock()
	alreadyResolved := dictPop(a.clarifyResolved, any(in.msgTS), true)
	a.mu.Unlock()
	if alreadyResolved {
		return
	}
	originalText := sectionText(in.message, -1)
	expiredText := i18n.T(ctx, "platform.slack.clarify.expired", "", map[string]any{"user": in.userName})
	dropMessage := func() {
		a.mu.Lock()
		a.clarifyMessages.Delete(clarifyID)
		a.mu.Unlock()
	}
	if in.actionID == "hermes_clarify_other" || token == "other" {
		if !tools.MarkClarifyAwaitingText(clarifyID) {
			dropMessage()
			a.updateClarifyMessage(ctx, in.channelID, in.msgTS, originalText, expiredText)
			return
		}
		a.updateClarifyMessage(ctx, in.channelID, in.msgTS, originalText, i18n.T(ctx, "platform.slack.clarify.awaiting", "", map[string]any{"user": in.userName}))
		return
	}
	idx, err := strconv.Atoi(pystr.Strip(token))
	if err != nil {
		slog.Warn("[Slack] Invalid clarify choice token: " + token)
		return
	}
	dropMessage()
	var resolvedText *string
	if entry := tools.GetPendingClarify(clarifyID); entry != nil && idx >= 0 && idx < len(entry.Choices) {
		choice := entry.Choices[idx]
		resolvedText = &choice
	}
	displayText := ""
	if resolvedText != nil {
		displayText = *resolvedText
	} else {
		fallback := fmt.Sprintf("choice %d", idx+1)
		resolvedText = &fallback
		displayText = i18n.T(ctx, "platform.slack.clarify.choice_n", "", map[string]any{"n": strconv.Itoa(idx + 1)})
	}
	if tools.ResolveGatewayClarify(clarifyID, *resolvedText) {
		a.updateClarifyMessage(ctx, in.channelID, in.msgTS, originalText,
			i18n.T(ctx, "platform.slack.clarify.resolved", "", map[string]any{"user": in.userName, "choice": displayText}))
		slog.Info(fmt.Sprintf("Slack button resolved clarify (id=%s, choice_index=%d, user=%s)", clarifyID, idx, in.userName))
		slog.Debug(fmt.Sprintf("Slack clarify choice text (id=%s): %s", clarifyID, pystr.Slice(pyrepr.Repr(*resolvedText), 0, 100)))
		return
	}
	a.updateClarifyMessage(ctx, in.channelID, in.msgTS, originalText, expiredText)
	slog.Warn(fmt.Sprintf("[Slack] clarify resolve returned False (id=%s) — expired/reset", clarifyID))
}
