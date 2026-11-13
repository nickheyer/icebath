package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var errListIndex = errors.New("list index out of range")

// Value of a key a Python row indexes directly, the KeyError text when it is missing
func requiredKey(row map[string]any, key string) (any, error) {
	v, ok := row[key]
	if !ok {
		return nil, errors.New(pyrepr.Repr(key))
	}
	return v, nil
}

// Value of a key, the default when it is missing
func rowGet(row map[string]any, key string, def any) any {
	if v, ok := row[key]; ok {
		return v
	}
	return def
}

// Model ids listed under a provider row
func rowModels(row map[string]any) []string {
	switch v := row["models"].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, m := range v {
			out = append(out, pyrepr.Str(m))
		}
		return out
	}
	return nil
}

// Integer value of a numeric row field, zero for any other kind
func rowInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// First selected value of a select click
func selectedValue(i *discordgo.Interaction) (string, error) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return "", errListIndex
	}
	return values[0], nil
}

// Display name of the clicker, the fallback when the click carries no user
func clickerName(i *discordgo.Interaction, fallback string) string {
	if interactionUser(i) == nil {
		return fallback
	}
	return interactionDisplayName(i)
}

// Approve once, always approve and cancel buttons of a slash command confirmation
type slashConfirmView struct {
	hermesView
	sessionKey, confirmID                 string
	approveOnce, approveAlways, cancelBtn *viewItem
}

// Slash confirmation view with the configured prompt timeout and localized buttons
func (a *DiscordAdapter) newSlashConfirmView(ctx context.Context, sessionKey, confirmID string, users, roles strSet) (*slashConfirmView, error) {
	timeout, err := ReadDiscordPromptTimeout(ctx)
	if err != nil {
		return nil, err
	}
	v := &slashConfirmView{sessionKey: sessionKey, confirmID: confirmID}
	v.initView(a, "SlashConfirmView", v, users, roles, float64(timeout))
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, b := range []struct {
		dst    **viewItem
		label  string
		style  discordgo.ButtonStyle
		choice string
		color  int
		key    string
	}{
		{&v.approveOnce, "Approve Once", discordgo.SuccessButton, "once", colorGreen, "platform.discord.slash_confirm.resolved_once"},
		{&v.approveAlways, "Always Approve", discordgo.PrimaryButton, "always", colorPurple, "platform.discord.slash_confirm.resolved_always"},
		{&v.cancelBtn, "Cancel", discordgo.DangerButton, "cancel", colorGreyple, "platform.discord.slash_confirm.resolved_cancel"},
	} {
		choice, color, key := b.choice, b.color, b.key
		item, err := v.addButtonLocked(b.label, b.style, "", "", func(ctx context.Context, i *discordgo.Interaction) error {
			return v.resolve(ctx, i, choice, color, key)
		})
		if err != nil {
			return nil, err
		}
		*b.dst = item
	}
	v.localizeButtonsLocked(ctx, map[*viewItem]string{v.approveOnce: "platform.discord.slash_confirm.approve_once",
		v.approveAlways: "platform.discord.slash_confirm.always_approve", v.cancelBtn: "platform.discord.slash_confirm.cancel"})
	return v, nil
}

// Stamps the card with the choice and runs the stored slash confirm handler, posting its text as a followup
func (v *slashConfirmView) resolve(ctx context.Context, i *discordgo.Interaction, choice string, color int, labelKey string) error {
	resolvedMsg := i18n.T(ctx, "platform.discord.slash_confirm.already_resolved", "", nil)
	if ok, err := v.gate(ctx, i, &resolvedMsg, unauthorized(ctx)); !ok {
		return err
	}
	user := interactionDisplayName(i)
	footer := i18n.T(ctx, "platform.discord.approval.by_user", "", map[string]any{"label": i18n.T(ctx, labelKey, "", nil), "user": user})
	if err := v.finalizeEmbed(ctx, i, color, footer); err != nil {
		return err
	}
	if err := v.runSlashConfirm(ctx, i, choice, user); err != nil {
		slog.Error(fmt.Sprintf("Discord slash-confirm resolve failed: %s", err))
	}
	return nil
}

// Resolves the pending confirmation and posts a returned result text in the same channel
func (v *slashConfirmView) runSlashConfirm(ctx context.Context, i *discordgo.Interaction, choice, user string) error {
	if gateway.SlashConfirmResolve == nil {
		return errors.New("gateway.SlashConfirmResolve unassigned")
	}
	if resultText := gateway.SlashConfirmResolve(ctx, v.sessionKey, v.confirmID, choice); resultText != "" {
		if err := v.adapter.followupSend(ctx, i, resultText); err != nil {
			return err
		}
	}
	slog.Info(fmt.Sprintf("Discord button resolved slash-confirm for session %s (choice=%s, user=%s)", v.sessionKey, choice, user))
	return nil
}

// Yes and no buttons of an update prompt whose answer the detached update process reads from a file
type updatePromptView struct {
	hermesView
	sessionKey    string
	yesBtn, noBtn *viewItem
}

// Update prompt view with the configured prompt timeout and localized buttons
func (a *DiscordAdapter) newUpdatePromptView(ctx context.Context, sessionKey string, users, roles strSet) (*updatePromptView, error) {
	timeout, err := ReadDiscordPromptTimeout(ctx)
	if err != nil {
		return nil, err
	}
	v := &updatePromptView{sessionKey: sessionKey}
	v.initView(a, "UpdatePromptView", v, users, roles, float64(timeout))
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.yesBtn, err = v.addButtonLocked("Yes", discordgo.SuccessButton, "✓", "", func(ctx context.Context, i *discordgo.Interaction) error {
		return v.respond(ctx, i, "y", colorGreen, "platform.discord.prompt.affirm")
	}); err != nil {
		return nil, err
	}
	if v.noBtn, err = v.addButtonLocked("No", discordgo.DangerButton, "✗", "", func(ctx context.Context, i *discordgo.Interaction) error {
		return v.respond(ctx, i, "n", colorRed, "platform.discord.prompt.negate")
	}); err != nil {
		return nil, err
	}
	v.localizeButtonsLocked(ctx, map[*viewItem]string{v.yesBtn: "platform.discord.prompt.affirm", v.noBtn: "platform.discord.prompt.negate"})
	return v, nil
}

// Stamps the card with the answer and writes it atomically to the update response file
func (v *updatePromptView) respond(ctx context.Context, i *discordgo.Interaction, answer string, color int, labelKey string) error {
	resolvedMsg := i18n.T(ctx, "platform.discord.prompt.already_answered", "", nil)
	if ok, err := v.gate(ctx, i, &resolvedMsg, unauthorized(ctx)); !ok {
		return err
	}
	user := interactionDisplayName(i)
	footer := i18n.T(ctx, "platform.discord.approval.by_user", "", map[string]any{"label": i18n.T(ctx, labelKey, "", nil), "user": user})
	if err := v.finalizeEmbed(ctx, i, color, footer); err != nil {
		return err
	}
	responsePath := filepath.Join(home.Dir(ctx), ".update_response")
	tmp := responsePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(answer), 0o644); err != nil {
		slog.Error(fmt.Sprintf("Failed to write update response: %s", err))
		return nil
	}
	if err := os.Rename(tmp, responsePath); err != nil {
		slog.Error(fmt.Sprintf("Failed to write update response: %s", err))
		return nil
	}
	slog.Info(fmt.Sprintf("Discord update prompt answered '%s' by %s", answer, user))
	return nil
}

// Two step provider then model select picker editing its message in place, two minute timeout
type modelPickerView struct {
	hermesView
	providers        []map[string]any
	currentModel     string
	currentProvider  string
	sessionKey       string
	onModelSelected  func(ctx context.Context, chatID, modelID, providerSlug string) (string, error)
	selectedProvider string
	pendingExpensive string
}

// Model picker opened on the provider select
func (a *DiscordAdapter) newModelPickerView(ctx context.Context, providers []map[string]any, currentModel, currentProvider, sessionKey string,
	onModelSelected func(ctx context.Context, chatID, modelID, providerSlug string) (string, error), users, roles strSet) (*modelPickerView, error) {
	v := &modelPickerView{providers: providers, currentModel: currentModel, currentProvider: currentProvider, sessionKey: sessionKey, onModelSelected: onModelSelected}
	v.initView(a, "ModelPickerView", v, users, roles, 120)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.buildProviderSelectLocked(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// Provider row with the slug, nil when no row has it
func (v *modelPickerView) providerBySlug(slug string) map[string]any {
	for _, p := range v.providers {
		if pyrepr.Str(p["slug"]) == slug {
			return p
		}
	}
	return nil
}

// Provider dropdown with model counts and the current marker, plus cancel, nothing for no providers
func (v *modelPickerView) buildProviderSelectLocked(ctx context.Context) error {
	v.clearItemsLocked()
	var options []selectOption
	for _, p := range v.providers {
		name, err := requiredKey(p, "name")
		if err != nil {
			return err
		}
		slug, err := requiredKey(p, "slug")
		if err != nil {
			return err
		}
		count := strconv.Itoa(len(rowModels(p)))
		if total, ok := p["total_models"]; ok {
			count = pyrepr.Str(total)
		}
		opt := selectOption{value: pyrepr.Str(slug), label: tDiscord(ctx, "platform.discord.picker.provider_option", discordSelectFieldLimit,
			map[string]any{"provider": pyrepr.Str(name), "count": count})}
		if pyrepr.Truthy(p["is_current"]) {
			opt.description = tDiscord(ctx, "platform.discord.picker.current", discordSelectFieldLimit, nil)
		}
		options = append(options, opt)
	}
	if len(options) == 0 {
		return nil
	}
	if err := v.addSelectLocked(tDiscord(ctx, "platform.discord.picker.provider_placeholder", discordSelectHintLimit, nil),
		options[:min(len(options), discordSelectMaxOptions)], "model_provider_select", v.onProviderSelected); err != nil {
		return err
	}
	_, err := v.addButtonLocked(tDiscord(ctx, "platform.discord.picker.cancel", discordButtonLabelLimit, nil), discordgo.DangerButton, "", "model_cancel", v.onCancel)
	return err
}

// Model dropdowns of one provider partitioned across up to three selects, then back and cancel
func (v *modelPickerView) buildModelSelectLocked(ctx context.Context, slug string) error {
	v.clearItemsLocked()
	p := v.providerBySlug(slug)
	if p == nil {
		return nil
	}
	models := rowModels(p)
	if len(models) == 0 {
		return nil
	}
	var chunks [][]string
	for idx := 0; idx < len(models); idx += discordSelectMaxOptions {
		chunks = append(chunks, models[idx:min(idx+discordSelectMaxOptions, len(models))])
	}
	chunks = chunks[:min(len(chunks), discordSelectMaxRows-2)]
	hintBase := i18n.T(ctx, "platform.discord.picker.model_placeholder", "", map[string]any{"provider": pyrepr.Str(rowGet(p, "name", slug))})
	for idx, chunk := range chunks {
		options := make([]selectOption, 0, len(chunk))
		for _, modelID := range chunk {
			short := modelID[strings.LastIndex(modelID, "/")+1:]
			opt := selectOption{label: truncateDiscordComponentText(short, discordSelectFieldLimit), value: truncateDiscordComponentText(modelID, discordSelectFieldLimit)}
			if suffix := provider.PickerRowSuffix(p, modelID); suffix != "" {
				opt.description = truncateDiscordComponentText(suffix, discordSelectFieldLimit)
			}
			options = append(options, opt)
		}
		suffix := ""
		if len(chunks) > 1 {
			suffix = fmt.Sprintf(" (%d/%d)", idx+1, len(chunks))
		}
		if err := v.addSelectLocked(truncateDiscordComponentText(hintBase+suffix+"...", discordSelectHintLimit), options,
			fmt.Sprintf("model_model_select_%d", idx), v.onModelSelect); err != nil {
			return err
		}
	}
	if _, err := v.addButtonLocked(tDiscord(ctx, "platform.discord.picker.back", discordButtonLabelLimit, nil), discordgo.SecondaryButton, "", "model_back", v.onBack); err != nil {
		return err
	}
	_, err := v.addButtonLocked(tDiscord(ctx, "platform.discord.picker.cancel", discordButtonLabelLimit, nil), discordgo.DangerButton, "", "model_cancel2", v.onCancel)
	return err
}

// Switch anyway and cancel buttons confirming an unusually expensive model
func (v *modelPickerView) buildExpensiveConfirmLocked(ctx context.Context, modelID string) error {
	v.clearItemsLocked()
	v.pendingExpensive = modelID
	if _, err := v.addButtonLocked(tDiscord(ctx, "platform.discord.picker.switch_anyway", discordButtonLabelLimit, nil), discordgo.DangerButton, "",
		"model_expensive_confirm", v.onExpensiveConfirm); err != nil {
		return err
	}
	_, err := v.addButtonLocked(tDiscord(ctx, "platform.discord.picker.cancel", discordButtonLabelLimit, nil), discordgo.SecondaryButton, "", "model_expensive_cancel", v.onCancel)
	return err
}

// Picker embed with the default title and blue unless given
func (v *modelPickerView) configEmbed(ctx context.Context, description string, title *string, color *int) *discordgo.MessageEmbed {
	t := i18n.T(ctx, "platform.discord.picker.title", "", nil)
	if title != nil {
		t = *title
	}
	c := colorBlue
	if color != nil {
		c = *color
	}
	return newEmbed(truncateDiscordComponentText(t, discordEmbedTitleLimit), description, c)
}

// Edits the picker message in place with a config embed, keeping the view unless dropped
func (v *modelPickerView) edit(ctx context.Context, i *discordgo.Interaction, description string, keepView bool, title *string, color *int) error {
	embed := v.configEmbed(ctx, description, title, color)
	comps := []discordgo.MessageComponent{}
	var view *uiView
	if keepView {
		v.mu.Lock()
		comps, view = v.componentsLocked(), &v.uiView
		v.mu.Unlock()
	}
	return v.adapter.respondEditMessage(ctx, i, embed, comps, view)
}

// Shows the model selects of the chosen provider with the plan line and the overflow hint
func (v *modelPickerView) onProviderSelected(ctx context.Context, i *discordgo.Interaction) error {
	if ok, err := v.gate(ctx, i, nil, unauthorized(ctx)); !ok {
		return err
	}
	slug, err := selectedValue(i)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.selectedProvider = slug
	p := v.providerBySlug(slug)
	pname := slug
	if p != nil {
		pname = pyrepr.Str(rowGet(p, "name", slug))
	}
	err = v.buildModelSelectLocked(ctx, slug)
	v.mu.Unlock()
	if err != nil {
		return err
	}
	total, shown := 0, 0
	planLine := ""
	if p != nil {
		total = rowInt(rowGet(p, "total_models", 0))
		shown = min(len(rowModels(p)), discordModelSelectCapacity)
		if pyrepr.Truthy(p["plan_line"]) {
			planLine = "⚠️ " + pyrepr.Str(p["plan_line"]) + "\n"
		}
	}
	extra := ""
	if total > shown {
		extra = "\n*" + i18n.T(ctx, "platform.discord.picker.more_available", "", map[string]any{"count": strconv.Itoa(total - shown)}) + "*"
	}
	return v.edit(ctx, i, planLine+i18n.T(ctx, "platform.discord.picker.select_model", "", map[string]any{"provider": pname, "extra": extra}), true, nil, nil)
}

// Answers the picker, shows the switching card, runs the switch and replaces the card with its result
func (v *modelPickerView) switchSelectedModel(ctx context.Context, i *discordgo.Interaction, modelID string) error {
	resolvedMsg := i18n.T(ctx, "platform.discord.picker.already_resolved", "", nil)
	if ok, err := v.gate(ctx, i, &resolvedMsg, unauthorized(ctx)); !ok {
		return err
	}
	v.mu.Lock()
	v.resolved = true
	v.mu.Unlock()
	v.stop()
	v.mu.Lock()
	v.clearItemsLocked()
	selected := v.selectedProvider
	v.mu.Unlock()
	switchingTitle := i18n.T(ctx, "platform.discord.picker.switching_title", "", nil)
	if err := v.edit(ctx, i, i18n.T(ctx, "platform.discord.picker.switching", "", map[string]any{"model": modelID}), false, &switchingTitle, nil); err != nil {
		return err
	}
	resultText, err := v.onModelSelected(ctx, i.ChannelID, modelID, selected)
	if err != nil {
		resultText = i18n.T(ctx, "platform.discord.picker.switch_error", "", map[string]any{"error": err.Error()})
	}
	switchedTitle, green := i18n.T(ctx, "platform.discord.picker.switched_title", "", nil), colorGreen
	return v.adapter.editOriginalResponse(ctx, i, v.configEmbed(ctx, resultText, &switchedTitle, &green))
}

// Switches to the picked model, asking for confirmation first when it is unusually expensive
func (v *modelPickerView) onModelSelect(ctx context.Context, i *discordgo.Interaction) error {
	resolvedMsg := i18n.T(ctx, "platform.discord.picker.already_resolved", "", nil)
	if ok, err := v.gate(ctx, i, &resolvedMsg, unauthorized(ctx)); !ok {
		return err
	}
	modelID, err := selectedValue(i)
	if err != nil {
		return err
	}
	v.mu.Lock()
	selected := v.selectedProvider
	v.mu.Unlock()
	if warning := provider.CombinedSelectionWarning(ctx, provider.SelectionRequest{Model: modelID, Provider: selected}); warning != nil {
		v.mu.Lock()
		err := v.buildExpensiveConfirmLocked(ctx, modelID)
		v.mu.Unlock()
		if err != nil {
			return err
		}
		title, red := "⚠ "+warning.Title, colorRed
		return v.edit(ctx, i, warning.Message, true, &title, &red)
	}
	return v.switchSelectedModel(ctx, i, modelID)
}

// Switches to the expensive model the user confirmed, or says the selection expired
func (v *modelPickerView) onExpensiveConfirm(ctx context.Context, i *discordgo.Interaction) error {
	if ok, err := v.gate(ctx, i, nil, unauthorized(ctx)); !ok {
		return err
	}
	v.mu.Lock()
	pending := v.pendingExpensive
	v.mu.Unlock()
	if pending == "" {
		return v.adapter.respondEphemeral(ctx, i, i18n.T(ctx, "platform.discord.picker.expired_toast", "", nil))
	}
	return v.switchSelectedModel(ctx, i, pending)
}

// Returns to the provider select with the current model summary
func (v *modelPickerView) onBack(ctx context.Context, i *discordgo.Interaction) error {
	if ok, err := v.gate(ctx, i, nil, unauthorized(ctx)); !ok {
		return err
	}
	v.mu.Lock()
	err := v.buildProviderSelectLocked(ctx)
	v.mu.Unlock()
	if err != nil {
		return err
	}
	return v.edit(ctx, i, providerSelectText(ctx, v.currentModel, v.currentProvider), true, nil, nil)
}

// Closes the picker with the cancelled card
func (v *modelPickerView) onCancel(ctx context.Context, i *discordgo.Interaction) error {
	if ok, err := v.gate(ctx, i, nil, unauthorized(ctx)); !ok {
		return err
	}
	v.mu.Lock()
	v.resolved = true
	v.mu.Unlock()
	v.stop()
	v.mu.Lock()
	v.clearItemsLocked()
	v.mu.Unlock()
	greyple := colorGreyple
	return v.edit(ctx, i, i18n.T(ctx, "platform.discord.picker.cancelled", "", nil), true, nil, &greyple)
}

// Replaces an unanswered picker with the expired card, edit failures ignored
func (v *modelPickerView) onTimeout(ctx context.Context) {
	v.mu.Lock()
	if v.resolved {
		v.mu.Unlock()
		return
	}
	v.resolved = true
	v.clearItemsLocked()
	msg, comps := v.message, v.componentsLocked()
	v.mu.Unlock()
	if msg == nil {
		return
	}
	greyple := colorGreyple
	embed := v.configEmbed(ctx, i18n.T(ctx, "platform.discord.picker.expired", "", nil), nil, &greyple)
	_ = v.adapter.editViewMessage(ctx, msg, embed, comps, &v.uiView)
}

// Provider select summary naming the current model and the provider label
func providerSelectText(ctx context.Context, currentModel, currentProvider string) string {
	model := currentModel
	if model == "" {
		model = i18n.T(ctx, "platform.discord.picker.unknown_model", "", nil)
	}
	return i18n.T(ctx, "platform.discord.picker.select_provider", "", map[string]any{"model": model, "provider": provider.GetProviderLabel(ctx, currentProvider)})
}

// Flat single select picker for finite choice commands, two minute timeout
type choicePickerView struct {
	hermesView
	choices          []map[string]any
	onChoiceSelected func(ctx context.Context, chatID, value string) (string, error)
}

// Choice picker of up to 25 options with the current one marked
func (a *DiscordAdapter) newChoicePickerView(ctx context.Context, choices []map[string]any, onChoiceSelected func(ctx context.Context, chatID, value string) (string, error),
	users, roles strSet) (*choicePickerView, error) {
	v := &choicePickerView{choices: append([]map[string]any(nil), choices[:min(len(choices), discordSelectMaxOptions)]...), onChoiceSelected: onChoiceSelected}
	v.initView(a, "ChoicePickerView", v, users, roles, 120)
	options := make([]selectOption, 0, len(v.choices))
	for _, choice := range v.choices {
		label := pyrepr.Str(pyrepr.Or(choice["label"], pyrepr.Or(choice["value"], "")))
		opt := selectOption{label: truncateDiscordComponentText(label, discordSelectFieldLimit), value: pyrepr.Str(pyrepr.Or(choice["value"], ""))}
		if pyrepr.Truthy(choice["is_current"]) {
			opt.description = tDiscord(ctx, "platform.discord.picker.current", discordSelectFieldLimit, nil)
		}
		options = append(options, opt)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.addSelectLocked(tDiscord(ctx, "platform.discord.picker.choice_placeholder", discordSelectHintLimit, nil), options, "", v.onSelect); err != nil {
		return nil, err
	}
	return v, nil
}

// Applies the picked value once and replaces the picker with its result
func (v *choicePickerView) onSelect(ctx context.Context, i *discordgo.Interaction) error {
	if !v.hooks.checkAuth(ctx, i) {
		return v.adapter.respondEphemeral(ctx, i, unauthorized(ctx))
	}
	v.mu.Lock()
	if v.resolved {
		v.mu.Unlock()
		return v.adapter.respondDeferUpdate(ctx, i)
	}
	v.resolved = true
	v.mu.Unlock()
	value := ""
	if values := i.MessageComponentData().Values; values != nil {
		if len(values) == 0 {
			return errListIndex
		}
		value = values[0]
	}
	resultText, err := v.onChoiceSelected(ctx, i.ChannelID, value)
	if err != nil {
		slog.Error(fmt.Sprintf("Choice picker selection failed: %s", err))
		resultText = i18n.T(ctx, "platform.discord.picker.choice_error", "", map[string]any{"error": err.Error()})
	}
	embed := newEmbed("", resultText, colorGreen)
	v.mu.Lock()
	v.clearItemsLocked()
	comps := v.componentsLocked()
	v.mu.Unlock()
	v.stop()
	return v.adapter.respondEditMessage(ctx, i, embed, comps, &v.uiView)
}

// Replaces an unanswered picker with the expired card, edit failures ignored
func (v *choicePickerView) onTimeout(ctx context.Context) {
	v.mu.Lock()
	if v.resolved || v.message == nil {
		v.mu.Unlock()
		return
	}
	msg := v.message
	embed := newEmbed("", i18n.T(ctx, "platform.discord.picker.choice_expired", "", nil), colorGreyple)
	v.clearItemsLocked()
	comps := v.componentsLocked()
	v.mu.Unlock()
	_ = v.adapter.editViewMessage(ctx, msg, embed, comps, &v.uiView)
}

// One button per clarify choice plus Other, single use, the first valid click disabling all
type clarifyChoiceView struct {
	hermesView
	choices   []string
	clarifyID string
}

// Clarify view of up to 24 numbered choice buttons and the Other button
func (a *DiscordAdapter) newClarifyChoiceView(ctx context.Context, choices []string, clarifyID string, users, roles strSet) (*clarifyChoiceView, error) {
	timeout, err := ReadDiscordPromptTimeout(ctx)
	if err != nil {
		return nil, err
	}
	v := &clarifyChoiceView{choices: append([]string(nil), choices[:min(len(choices), 24)]...), clarifyID: clarifyID}
	v.initView(a, "ClarifyChoiceView", v, users, roles, float64(timeout))
	v.mu.Lock()
	defer v.mu.Unlock()
	for index, choice := range v.choices {
		index, choice := index, choice
		if _, err := v.addButtonLocked(clarifyButtonLabel(index, choice), discordgo.PrimaryButton, "", fmt.Sprintf("clarify:%s:%d", clarifyID, index),
			func(ctx context.Context, i *discordgo.Interaction) error {
				return v.resolveChoice(ctx, i, index, choice)
			}); err != nil {
			return nil, err
		}
	}
	if _, err := v.addButtonLocked(tDiscord(ctx, "platform.discord.prompt.other", discordButtonLabelLimit, nil), discordgo.SecondaryButton, "",
		"clarify:"+clarifyID+":other", v.onOther); err != nil {
		return nil, err
	}
	return v, nil
}

// Last index of a rune in a rune slice, minus one when absent
func lastRuneIndex(rs []rune, r rune) int {
	for idx := len(rs) - 1; idx >= 0; idx-- {
		if rs[idx] == r {
			return idx
		}
	}
	return -1
}

// Numbered label within the 80 unit cap, long choices cut at a word then a soft boundary in the trailing half
func clarifyButtonLabel(index int, choice string) string {
	prefix := strconv.Itoa(index+1) + ". "
	budget := discordButtonLabelLimit - gateway.UTF16Len(prefix)
	if gateway.UTF16Len(choice) <= budget {
		return prefix + choice
	}
	truncated := []rune(pystr.Rstrip(gateway.PrefixWithinUTF16Limit(choice, max(0, budget-gateway.UTF16Len(discordEllipsis)))))
	cutAt := -1
	if space := lastRuneIndex(truncated, ' '); space >= len(truncated)/2 {
		cutAt = space
	}
	if cutAt < 0 {
		latestSoft := -1
		for _, s := range []rune{'-', ',', '.', ')'} {
			latestSoft = max(latestSoft, lastRuneIndex(truncated, s))
		}
		if latestSoft >= len(truncated)/2 {
			cutAt = latestSoft + 1
		}
	}
	if cutAt > 0 {
		truncated = truncated[:cutAt]
	}
	return prefix + pystr.Rstrip(string(truncated)) + discordEllipsis
}

// Disables the buttons and stamps the card, a bare defer when the edit fails, stopping the view either way
func (v *clarifyChoiceView) finish(ctx context.Context, i *discordgo.Interaction, color int, footer string, logEditFailure bool) {
	v.mu.Lock()
	v.resolved = true
	v.disableAllLocked()
	embed := stampedEmbed(firstEmbed(i.Message), color, footer)
	comps := v.componentsLocked()
	v.mu.Unlock()
	defer v.stop()
	if err := v.adapter.respondEditMessage(ctx, i, embed, comps, &v.uiView); err != nil {
		if logEditFailure {
			slog.Debug(fmt.Sprintf("Discord clarify edit_message failed for %s", v.clarifyID), "error", err)
		}
		if derr := v.adapter.respondDeferUpdate(ctx, i); derr != nil {
			slog.Debug(fmt.Sprintf("Discord clarify defer failed for %s", v.clarifyID), "error", derr)
		}
	}
}

// Resolves the clarify with the canonical text of the chosen option
func (v *clarifyChoiceView) resolveChoice(ctx context.Context, i *discordgo.Interaction, index int, choice string) error {
	resolvedMsg := i18n.T(ctx, "platform.discord.prompt.clarify_already_answered", "", nil)
	if ok, err := v.gate(ctx, i, &resolvedMsg, unauthorized(ctx)); !ok {
		return err
	}
	v.finish(ctx, i, colorGreen, i18n.T(ctx, "platform.discord.prompt.answered_by", "", map[string]any{"user": clickerName(i, "user"), "choice": choice}), true)
	resolvedText := choice
	if entry := tools.GetPendingClarify(v.clarifyID); entry != nil && len(entry.Choices) > 0 && index >= 0 && index < len(entry.Choices) {
		resolvedText = entry.Choices[index]
	}
	resolved := tools.ResolveGatewayClarify(v.clarifyID, resolvedText)
	slog.Info(fmt.Sprintf("Discord clarify button resolved (id=%s, choice=%s, user=%s, ok=%s)", v.clarifyID, pyrepr.Repr(resolvedText),
		clickerName(i, "?"), pyrepr.Repr(resolved)))
	return nil
}

// Flips the clarify into text capture so the next message answers it
func (v *clarifyChoiceView) onOther(ctx context.Context, i *discordgo.Interaction) error {
	resolvedMsg := i18n.T(ctx, "platform.discord.prompt.clarify_already_answered", "", nil)
	if ok, err := v.gate(ctx, i, &resolvedMsg, unauthorized(ctx)); !ok {
		return err
	}
	tools.MarkClarifyAwaitingText(v.clarifyID)
	v.finish(ctx, i, colorBlue, i18n.T(ctx, "platform.discord.prompt.awaiting_typed", "", map[string]any{"user": clickerName(i, "user")}), false)
	return nil
}
