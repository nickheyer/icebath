package discord

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Discord's cap on global application commands
const discordMaxAppCommands = 100

// One slash command parameter, its option type, default and whether Discord requires it
type slashParam struct {
	name     string
	isInt    bool
	required bool
	def      any
}

// Registered application command with its payload, callback and autocomplete sources
type appCommand struct {
	name         string
	payload      map[string]any
	params       []slashParam
	callback     func(ctx context.Context, i *discordgo.Interaction, opts map[string]any)
	autocomplete map[string]func(ctx context.Context, i *discordgo.Interaction, current string) []*discordgo.ApplicationCommandOptionChoice
}

// Skill autocomplete row of the /skill command
type skillEntry struct{ name, desc, key string }

// Refusal text every unauthorized action gets, resolved per click for the active language
func unauthorized(ctx context.Context) string {
	return gateway.UnauthorizedActionNoticeFor(ctx, gateway.PlatformDiscord)
}

// Option payload of one parameter as discord.py serialises it
func paramPayload(p slashParam, desc string, choices [][2]string) map[string]any {
	typ := int(discordgo.ApplicationCommandOptionString)
	if p.isInt {
		typ = int(discordgo.ApplicationCommandOptionInteger)
	}
	out := map[string]any{"type": typ, "name": p.name, "description": desc, "required": p.required}
	if len(choices) > 0 {
		list := make([]any, len(choices))
		for n, c := range choices {
			list[n] = map[string]any{"name": c[0], "value": c[1]}
		}
		out["choices"] = list
	}
	return out
}

// Top level command payload as discord.py serialises a chat input command
func commandPayload(name, description string, options []any) map[string]any {
	if options == nil {
		options = []any{}
	}
	return map[string]any{"name": name, "description": description, "type": 1, "options": options, "nsfw": false,
		"contexts": nil, "integration_types": nil, "default_member_permissions": nil}
}

// Adds a command to the tree unless the name is taken, false on conflict
func (a *DiscordAdapter) addCommand(cmd *appCommand) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if slices.ContainsFunc(a.tree, func(c *appCommand) bool { return c.name == cmd.name }) {
		return false
	}
	a.tree = append(a.tree, cmd)
	return true
}

// Names of every command on the tree
func (a *DiscordAdapter) treeNames() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]bool, len(a.tree))
	for _, c := range a.tree {
		out[c.name] = true
	}
	return out
}

// Callback rendering the template from the invocation options and running it as a simple slash
func (a *DiscordAdapter) slashProxy(template string, followup string, strip bool) func(ctx context.Context, i *discordgo.Interaction, opts map[string]any) {
	return func(ctx context.Context, i *discordgo.Interaction, opts map[string]any) {
		text := template
		for k, v := range opts {
			text = strings.ReplaceAll(text, "{"+k+"}", pyrepr.Str(v))
		}
		if strip {
			text = pystr.Strip(text)
		}
		a.runSimpleSlash(ctx, i, text, followup)
	}
}

// Defers, dispatches the command text, then replaces or deletes the thinking indicator
func (a *DiscordAdapter) runSimpleSlash(ctx context.Context, i *discordgo.Interaction, commandText, followup string) {
	userName, userID := "?", "?"
	if u := interactionUser(i); u != nil {
		userName, userID = u.Username, u.ID
	}
	guild := "None"
	if i.GuildID != "" {
		guild = i.GuildID
	}
	slog.Info(fmt.Sprintf("[Discord] slash '%s' invoked by user=%s id=%s channel=%s guild=%s", commandText, userName, userID, i.ChannelID, guild))
	if !a.checkSlashAuthorization(ctx, i, commandText) {
		return
	}
	deferred, err := a.deferUnlessExpired(ctx, i, "[Discord] slash %s: interaction expired before defer. Executing command anyway, skipping interaction followup.", commandText)
	if err != nil {
		slog.Error(fmt.Sprintf("[Discord] slash %s failed: %s", commandText, err))
		return
	}
	if err := a.HandleMessage(ctx, a.buildSlashEvent(ctx, i, commandText)); err != nil {
		slog.Warn(fmt.Sprintf("[%s] Slash dispatch failed: %s", a.Name(), err))
	}
	if !deferred {
		return
	}
	client := a.liveClient()
	if client == nil {
		return
	}
	if followup != "" {
		content := i18n.T(ctx, followup, "", nil)
		_, err = client.InteractionResponseEdit(i, &discordgo.WebhookEdit{Content: &content}, discordgo.WithContext(ctx))
	} else {
		err = client.InteractionResponseDelete(i, discordgo.WithContext(ctx))
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Discord interaction cleanup failed: %s", dpyError(err)))
	}
}

// Registers the /thread command whose handler creates a thread and starts a session in it
func (a *DiscordAdapter) registerThreadSlash(ctx context.Context, name, description string) {
	text := func(key string) string { return tDiscord(ctx, key, discordAppCommandTextLimit, nil) }
	params := []slashParam{{name: "name", required: true}, {name: "message", def: ""}, {name: "auto_archive_duration", isInt: true, def: 1440}}
	options := []any{
		paramPayload(params[0], text("platform.discord.command.thread.arg_name"), nil),
		paramPayload(params[1], text("platform.discord.command.thread.arg_message"), nil),
		paramPayload(params[2], text("platform.discord.command.thread.arg_auto_archive"), nil),
	}
	a.addCommand(&appCommand{name: name, payload: commandPayload(name, description, options), params: params,
		callback: func(ctx context.Context, i *discordgo.Interaction, opts map[string]any) {
			archive, _ := opts["auto_archive_duration"].(int)
			a.handleThreadCreateSlash(ctx, i, pyrepr.Str(opts["name"]), pyrepr.Str(opts["message"]), archive)
		}})
}

// Builds the slash command tree, native commands first, then registry and plugin commands and /skill
func (a *DiscordAdapter) registerSlashCommands(ctx context.Context) {
	if a.liveClient() == nil {
		return
	}
	a.mu.Lock()
	a.tree = nil
	a.mu.Unlock()
	for _, spec := range nativeSlashCommands(ctx) {
		if spec.Template == nil {
			a.registerThreadSlash(ctx, spec.Name, spec.Desc)
			continue
		}
		var params []slashParam
		var options []any
		for _, arg := range spec.Args {
			p := slashParam{name: arg.Name, isInt: arg.IsInt, required: arg.Required, def: arg.Default}
			params = append(params, p)
			options = append(options, paramPayload(p, arg.Desc, arg.Choices))
		}
		a.addCommand(&appCommand{name: spec.Name, payload: commandPayload(spec.Name, spec.Desc, options), params: params,
			callback: a.slashProxy(*spec.Template, spec.FollowupKey, spec.Name != "insights")})
	}
	alreadyRegistered := a.treeNames()
	slotCap := discordMaxAppCommands - 1
	droppedOverCap := 0
	autoRegister := func(name, description, argsHint string) {
		discordName := strings.ToLower(name)
		if len(discordName) > 32 {
			discordName = discordName[:32]
		}
		if alreadyRegistered[discordName] {
			return
		}
		if len(alreadyRegistered) >= slotCap {
			droppedOverCap++
			return
		}
		var params []slashParam
		var options []any
		template := "/" + name
		if argsHint != "" {
			p := slashParam{name: "args", def: ""}
			params = []slashParam{p}
			options = []any{paramPayload(p, tDiscord(ctx, "platform.discord.command.auto_args_description", discordAppCommandTextLimit,
				map[string]any{"args_hint": argsHint}), nil)}
			template = "/" + name + " {args}"
		}
		if description == "" {
			description = i18n.T(ctx, "platform.discord.command.auto_description_fallback", "", map[string]any{"name": name})
		}
		description = truncateDiscordComponentText(description, discordAppCommandTextLimit)
		if a.addCommand(&appCommand{name: discordName, payload: commandPayload(discordName, description, options), params: params,
			callback: a.slashProxy(template, "", argsHint != "")}) {
			alreadyRegistered[discordName] = true
		}
	}
	if gateway.GatewayRegistryCommands == nil {
		slog.Warn("Discord auto-register from COMMAND_REGISTRY failed: command registry is not wired")
	} else if entries, err := gateway.GatewayRegistryCommands(ctx); err != nil {
		slog.Warn(fmt.Sprintf("Discord auto-register from COMMAND_REGISTRY failed: %s", err))
	} else {
		for _, e := range entries {
			autoRegister(e.Name, e.Description, e.ArgsHint)
		}
		slog.Debug(fmt.Sprintf("Discord auto-registered %d commands from COMMAND_REGISTRY", len(alreadyRegistered)))
	}
	if gateway.PluginSlashCommandEntries == nil {
		slog.Warn("Discord auto-register from plugin commands failed: plugin commands are not wired")
	} else if entries, err := gateway.PluginSlashCommandEntries(ctx); err != nil {
		slog.Warn(fmt.Sprintf("Discord auto-register from plugin commands failed: %s", err))
	} else {
		for _, e := range entries {
			autoRegister(e.Name, e.Description, e.ArgsHint)
		}
	}
	a.registerSkillGroup(ctx)
	if droppedOverCap > 0 {
		slog.Warn(fmt.Sprintf("[%s] Reached Discord's limit of %d slash commands; skipped %d lower-priority command(s) to keep the command sync working. "+
			"Disable slash commands you don't need or trim installed plugins to surface them all.", a.Name(), discordMaxAppCommands, droppedOverCap))
	}
	switch pystr.Lower(gateway.PlatformGateEnv(ctx, "DISCORD_HIDE_SLASH_COMMANDS", "false")) {
	case "true", "1", "yes", "on":
		a.applyOwnerOnlyVisibility()
	}
	a.syncDesiredCommands()
}

// Mirrors the tree payloads into the desired command set the sync engine fingerprints
func (a *DiscordAdapter) syncDesiredCommands() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.desiredCommands = a.desiredCommands[:0]
	for _, c := range a.tree {
		v, err := jsonValue(c.payload)
		if err != nil {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			a.desiredCommands = append(a.desiredCommands, m)
		}
	}
}

// Hides every registered command from non administrators with zero default member permissions
func (a *DiscordAdapter) applyOwnerOnlyVisibility() {
	a.mu.Lock()
	applied := 0
	for _, c := range a.tree {
		c.payload["default_member_permissions"] = "0"
		applied++
	}
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[Discord] Hid %d slash command(s) from non-admin guild members (opt-in defense in depth via DISCORD_HIDE_SLASH_COMMANDS).", applied))
}

// Registers one flat /skill command whose name option autocompletes from the skill catalog
func (a *DiscordAdapter) registerSkillGroup(ctx context.Context) {
	existing := a.treeNames()
	a.mu.Lock()
	a.skillEntries, a.skillLookup, a.skillGroupReservedNames = nil, map[string][2]string{}, existing
	a.mu.Unlock()
	if err := a.refreshSkillCatalogState(ctx); err != nil {
		slog.Warn(fmt.Sprintf("[%s] Failed to register /skill command: %s", a.Name(), err))
		return
	}
	a.mu.Lock()
	count, hidden := len(a.skillEntries), a.skillGroupHiddenCount
	a.mu.Unlock()
	if count == 0 {
		return
	}
	autocomplete := func(ctx context.Context, i *discordgo.Interaction, current string) []*discordgo.ApplicationCommandOptionChoice {
		if allowed, _ := a.evaluateSlashAuthorization(ctx, i); !allowed {
			return []*discordgo.ApplicationCommandOptionChoice{}
		}
		q := pystr.Lower(pystr.Strip(current))
		a.mu.Lock()
		entries := a.skillEntries
		a.mu.Unlock()
		choices := []*discordgo.ApplicationCommandOptionChoice{}
		for _, e := range entries {
			if q == "" || strings.Contains(pystr.Lower(e.name), q) || (e.desc != "" && strings.Contains(pystr.Lower(e.desc), q)) {
				label := e.name
				if e.desc != "" {
					label = e.name + " — " + e.desc
				}
				if r := []rune(label); len(r) > 100 {
					label = string(r[:97]) + "..."
				}
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: label, Value: e.name})
				if len(choices) >= 25 {
					break
				}
			}
		}
		return choices
	}
	text := func(key string) string { return tDiscord(ctx, key, discordAppCommandTextLimit, nil) }
	params := []slashParam{{name: "name", required: true}, {name: "args", def: ""}}
	nameOpt := paramPayload(params[0], text("platform.discord.command.skill.arg_name"), nil)
	nameOpt["autocomplete"] = true
	options := []any{nameOpt, paramPayload(params[1], text("platform.discord.command.skill.arg_args"), nil)}
	a.addCommand(&appCommand{name: "skill", payload: commandPayload("skill", text("platform.discord.command.skill.description"), options), params: params,
		autocomplete: map[string]func(context.Context, *discordgo.Interaction, string) []*discordgo.ApplicationCommandOptionChoice{"name": autocomplete},
		callback: func(ctx context.Context, i *discordgo.Interaction, opts map[string]any) {
			if !a.checkSlashAuthorization(ctx, i, "/skill") {
				return
			}
			name := pyrepr.Str(opts["name"])
			a.mu.Lock()
			entry, ok := a.skillLookup[name]
			a.mu.Unlock()
			if !ok {
				a.respondEphemeral(ctx, i, i18n.T(ctx, "platform.discord.command.skill.unknown", "", map[string]any{"name": name}))
				return
			}
			a.runSimpleSlash(ctx, i, pystr.Strip(entry[1]+" "+pyrepr.Str(opts["args"])), "")
		}})
	slog.Info(fmt.Sprintf("[%s] Registered /skill command with %d skill(s) via autocomplete", a.Name(), count))
	if hidden > 0 {
		slog.Info(fmt.Sprintf("[%s] %d skill(s) filtered out of /skill (name clamp / reserved)", a.Name(), hidden))
	}
}

// Rescans the skill catalog into the autocomplete entries in alphabetical order, no Discord call
func (a *DiscordAdapter) refreshSkillCatalogState(ctx context.Context) error {
	if gateway.DiscordSkillCommandsByCategory == nil {
		return fmt.Errorf("skill catalog is not wired")
	}
	a.mu.Lock()
	reserved := make(map[string]bool, len(a.skillGroupReservedNames))
	for k := range a.skillGroupReservedNames {
		reserved[k] = true
	}
	a.mu.Unlock()
	categories, uncategorized, hidden, err := gateway.DiscordSkillCommandsByCategory(ctx, reserved)
	if err != nil {
		return err
	}
	var entries []skillEntry
	for _, e := range uncategorized {
		entries = append(entries, skillEntry{e.Name, e.Description, e.CmdKey})
	}
	for _, cat := range categories {
		for _, e := range cat {
			entries = append(entries, skillEntry{e.Name, e.Description, e.CmdKey})
		}
	}
	slices.SortStableFunc(entries, func(x, y skillEntry) int { return strings.Compare(x.name, y.name) })
	lookup := make(map[string][2]string, len(entries))
	for _, e := range entries {
		lookup[e.name] = [2]string{e.desc, e.key}
	}
	a.mu.Lock()
	a.skillEntries, a.skillLookup, a.skillGroupHiddenCount = entries, lookup, hidden
	a.mu.Unlock()
	return nil
}

// Rescans skills for live /skill autocomplete and returns the new and hidden counts
func (a *DiscordAdapter) RefreshSkillGroup(ctx context.Context) (int, int) {
	if err := a.refreshSkillCatalogState(ctx); err != nil {
		slog.Warn(fmt.Sprintf("[%s] Failed to refresh /skill autocomplete after reload: %s", a.Name(), err))
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.skillEntries), 0
	}
	a.mu.Lock()
	count, hidden := len(a.skillEntries), a.skillGroupHiddenCount
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Refreshed /skill autocomplete: %d skill(s) available (%d filtered)", a.Name(), count, hidden))
	return count, hidden
}

// Guild id of a slash interaction, the channel guild when the payload lacks one
func (a *DiscordAdapter) interactionGuildID(i *discordgo.Interaction) string {
	if i.GuildID != "" {
		return i.GuildID
	}
	if ch := a.interactionChannel(i); ch != nil {
		return ch.GuildID
	}
	return ""
}

// Display name of the acting user of an interaction
func interactionDisplayName(i *discordgo.Interaction) string {
	if i.Member != nil && i.Member.Nick != "" {
		return i.Member.Nick
	}
	if u := interactionUser(i); u != nil {
		return u.DisplayName()
	}
	return ""
}

// Message event of a slash invocation with the source a message in the same channel would get
func (a *DiscordAdapter) buildSlashEvent(ctx context.Context, i *discordgo.Interaction, text string) *gateway.MessageEvent {
	ch := a.interactionChannel(i)
	isDM := isDMChannel(ch)
	isThread := ch != nil && ch.IsThread()
	chatType, threadID := "group", ""
	switch {
	case isDM:
		chatType = "dm"
	case isThread:
		chatType, threadID = "thread", i.ChannelID
	}
	chatName := ""
	switch {
	case isDM:
		if u := interactionUser(i); u != nil {
			chatName = u.Username
		}
	case isThread:
		chatName = a.formatThreadChatName(ch)
	case ch != nil && ch.Name != "":
		chatName = ch.Name
		if g := a.stateGuild(ch.GuildID); g != nil {
			chatName = g.Name + " / #" + chatName
		}
	}
	chatTopic := ""
	if ch != nil {
		chatTopic = a.getEffectiveTopic(ch, isThread)
	}
	parentID := ""
	if isThread {
		parentID = a.getParentChannelID(ch)
	}
	userID, userName := "", interactionDisplayName(i)
	if u := interactionUser(i); u != nil {
		userID = u.ID
	}
	fields := gateway.SourceFields{ChatName: &chatName, ChatType: chatType, UserID: &userID, UserName: &userName,
		RoleAuthorized: a.slashRoleGrant(ctx, i)}
	if threadID != "" {
		fields.ThreadID = &threadID
	}
	if chatTopic != "" {
		fields.ChatTopic = &chatTopic
	}
	if gid := a.interactionGuildID(i); gid != "" {
		fields.GuildID = &gid
	}
	if parentID != "" {
		fields.ParentChatID = &parentID
	}
	msgType := gateway.MessageTypeText
	if strings.HasPrefix(text, "/") {
		msgType = gateway.MessageTypeCommand
	}
	event := &gateway.MessageEvent{Text: text, MessageType: msgType, Source: a.BuildSource(ctx, i.ChannelID, fields), RawMessage: i}
	if prompt, ok := a.resolveChannelPrompt(i.ChannelID, parentID); ok {
		event.ChannelPrompt = &prompt
	}
	if skills := a.resolveChannelSkills(i.ChannelID, parentID); skills != nil {
		event.AutoSkill = skills
	}
	return event
}

// Ephemeral reply to an interaction
func (a *DiscordAdapter) respondEphemeral(ctx context.Context, i *discordgo.Interaction, content string) error {
	client := a.liveClient()
	if client == nil {
		return fmt.Errorf("Not connected")
	}
	err := client.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: a.allowedMentions}}, discordgo.WithContext(ctx))
	if err != nil {
		return dpyError(err)
	}
	return nil
}

// Ephemeral followup after a deferred interaction response
func (a *DiscordAdapter) followupEphemeral(ctx context.Context, i *discordgo.Interaction, content string) error {
	client := a.liveClient()
	if client == nil {
		return fmt.Errorf("Not connected")
	}
	_, err := client.FollowupMessageCreate(i, true, &discordgo.WebhookParams{Content: content, Flags: discordgo.MessageFlagsEphemeral,
		AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx))
	if err != nil {
		return dpyError(err)
	}
	return nil
}

// Creates a thread from /thread, links it back to the invoker and starts a session with the starter text
func (a *DiscordAdapter) handleThreadCreateSlash(ctx context.Context, i *discordgo.Interaction, name, message string, autoArchiveDuration int) {
	if !a.checkSlashAuthorization(ctx, i, "/thread") {
		return
	}
	deferred, err := a.deferUnlessExpired(ctx, i, "[Discord] /thread: interaction expired before defer. Creating the thread anyway, skipping interaction followups.")
	if err != nil {
		slog.Error(fmt.Sprintf("[Discord] /thread failed: %s", err))
		return
	}
	result := a.createThread(ctx, i, name, message, autoArchiveDuration)
	if ok, _ := result["success"].(bool); !ok {
		errText, _ := result["error"].(string)
		if errText == "" {
			errText = i18n.T(ctx, "platform.discord.command.thread.error_unknown", "", nil)
		}
		if deferred {
			if err := a.followupEphemeral(ctx, i, i18n.T(ctx, "platform.discord.command.thread.failed", "", map[string]any{"error": errText})); err != nil {
				slog.Error(fmt.Sprintf("[Discord] /thread followup failed: %s", err))
			}
		}
		return
	}
	threadID, _ := result["thread_id"].(string)
	threadName, _ := result["thread_name"].(string)
	if threadName == "" {
		threadName = name
	}
	link := "**" + threadName + "**"
	if threadID != "" {
		link = "<#" + threadID + ">"
	}
	if deferred {
		if err := a.followupEphemeral(ctx, i, i18n.T(ctx, "platform.discord.command.thread.created", "", map[string]any{"link": link})); err != nil {
			slog.Error(fmt.Sprintf("[Discord] /thread followup failed: %s", err))
			return
		}
	}
	if threadID != "" {
		if err := a.threads.MarkAsync(ctx, threadID); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Thread participation save failed: %s", a.Name(), err))
		}
	}
	if starter := pystr.Strip(message); starter != "" && threadID != "" {
		th, _ := result["thread"].(*discordgo.Channel)
		a.dispatchThreadSession(ctx, i, th, starter)
	}
}

// Message event pointing at a fresh thread, sent through the gateway pipeline
func (a *DiscordAdapter) dispatchThreadSession(ctx context.Context, i *discordgo.Interaction, th *discordgo.Channel, text string) {
	threadID := th.ID
	parentID := a.getParentChannelID(th)
	chatName, topic := a.formatThreadChatName(th), a.getEffectiveTopic(th, true)
	userID, userName := "", interactionDisplayName(i)
	if u := interactionUser(i); u != nil {
		userID = u.ID
	}
	fields := gateway.SourceFields{ChatName: &chatName, ChatType: "thread", UserID: &userID, UserName: &userName, ThreadID: &threadID,
		RoleAuthorized: a.slashRoleGrant(ctx, i)}
	if topic != "" {
		fields.ChatTopic = &topic
	}
	if gid := a.interactionGuildID(i); gid != "" {
		fields.GuildID = &gid
	}
	if parentID != "" {
		fields.ParentChatID = &parentID
	}
	event := &gateway.MessageEvent{Text: text, MessageType: gateway.MessageTypeText, Source: a.BuildSource(ctx, threadID, fields), RawMessage: i}
	if skills := a.resolveChannelSkills(threadID, parentID); skills != nil {
		event.AutoSkill = skills
	}
	if prompt, ok := a.resolveChannelPrompt(threadID, parentID); ok {
		event.ChannelPrompt = &prompt
	}
	if err := a.HandleMessage(ctx, event); err != nil {
		slog.Warn(fmt.Sprintf("[%s] Thread session dispatch failed: %s", a.Name(), err))
	}
}

// Message gates applied to a slash invocation, false only after the ephemeral rejection went out
func (a *DiscordAdapter) checkSlashAuthorization(ctx context.Context, i *discordgo.Interaction, commandText string) bool {
	allowed, reason := a.evaluateSlashAuthorization(ctx, i)
	if allowed {
		return true
	}
	if reason == "" {
		reason = "unauthorized"
	}
	return a.rejectSlash(ctx, i, commandText, reason)
}

// Sends the ephemeral refusal, logs the attempt and schedules the operator alert
func (a *DiscordAdapter) rejectSlash(ctx context.Context, i *discordgo.Interaction, commandText, reason string) bool {
	userID, userName := "?", "?"
	if u := interactionUser(i); u != nil {
		userID, userName = u.ID, u.Username
	}
	chanID := i.ChannelID
	guildID := i.GuildID
	slog.Warn(fmt.Sprintf("[Discord] Unauthorized slash attempt: user=%s id=%s channel=%s guild=%s cmd=%s reason=%s",
		userName, userID, pyNone(chanID), pyNone(guildID), pyrepr.Repr(commandText), pyrepr.Repr(reason)))
	if err := a.respondEphemeral(ctx, i, unauthorized(ctx)); err != nil {
		slog.Debug(fmt.Sprintf("[Discord] Could not send unauthorized ephemeral: %s", err))
	}
	go a.notifyUnauthorizedSlash(context.WithoutCancel(ctx), userName, userID, chanID, guildID, commandText, reason)
	return false
}

// Python None text for an empty id
func pyNone(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// Adapter map and gateway config of the profile owning this adapter
func alertAdaptersAndConfig(ctx context.Context, runner any, profile string) (*gateway.AdapterMap, *gateway.GatewayConfig, error) {
	if gateway.ProfileAdaptersAndConfig == nil {
		return nil, nil, fmt.Errorf("profile adapter resolution is not wired")
	}
	return gateway.ProfileAdaptersAndConfig(ctx, runner, profile)
}

// Operator alert on Telegram then Slack inside this adapter's profile scope
func (a *DiscordAdapter) notifyUnauthorizedSlash(ctx context.Context, userName, userID, chanID, guildID, commandText, reason string) {
	runner := a.GatewayRunner
	if runner == nil {
		return
	}
	profile := a.OwnerProfile()
	if profile != "" {
		dir, err := profiles.GetProfileDir(profile)
		if err != nil {
			slog.Debug(fmt.Sprintf("[Discord] Admin notify: profile %s scope failed: %s", pyrepr.Repr(profile), err))
			return
		}
		ctx = gateway.ProfileRuntimeScope(ctx, dir, true)
	}
	a.deliverUnauthorizedSlashAlert(ctx, runner, profile, userName, userID, chanID, guildID, commandText, reason)
}

// Sends the alert to the first home channel that confirms delivery, stopping when policy vetoes it
func (a *DiscordAdapter) deliverUnauthorizedSlashAlert(ctx context.Context, runner any, profile, userName, userID, chanID, guildID, commandText, reason string) {
	adapters, cfg, err := alertAdaptersAndConfig(ctx, runner, profile)
	if err != nil {
		slog.Debug(fmt.Sprintf("[Discord] Admin notify: profile %s resolution failed: %s", pyrepr.Repr(profile), err))
		return
	}
	for _, target := range []gateway.Platform{gateway.PlatformTelegram, gateway.PlatformSlack} {
		adapter, ok := adapters.Get(target)
		if !ok || adapter == nil {
			continue
		}
		home := cfg.GetHomeChannel(target)
		if home == nil || home.ChatID == "" {
			continue
		}
		msg := i18n.T(ctx, "platform.discord.security.unauthorized_slash_alert", "", map[string]any{"user_name": userName, "user_id": userID,
			"chan_id": pyNone(chanID), "guild_id": pyNone(guildID), "command_text": commandText, "reason": reason})
		var result gateway.SendResult
		presented, err := config.PresentNotification(ctx, func(ctx context.Context) error {
			var sendErr error
			result, sendErr = adapter.Send(ctx, home.ChatID, msg, "", nil)
			return sendErr
		}, string(target), true, nil)
		if err != nil {
			slog.Debug(fmt.Sprintf("[Discord] Admin notify via %s failed: %s", target, err))
			continue
		}
		if !presented {
			return
		}
		if !result.Success {
			slog.Debug(fmt.Sprintf("[Discord] Admin notify via %s returned success=False (error=%s); falling through", target, pyrepr.Repr(result.Error)))
			continue
		}
		return
	}
}

// Routes an interaction to its slash command, autocomplete source or component view
func (a *DiscordAdapter) onInteraction(ctx context.Context, i *discordgo.Interaction) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand, discordgo.InteractionApplicationCommandAutocomplete:
		data := i.ApplicationCommandData()
		a.mu.Lock()
		var cmd *appCommand
		for _, c := range a.tree {
			if c.name == data.Name {
				cmd = c
			}
		}
		a.mu.Unlock()
		if cmd == nil {
			slog.Error(fmt.Sprintf("Ignoring exception in command tree: CommandNotFound: Application command %s not found", pyrepr.Repr(data.Name)))
			return
		}
		if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
			a.runAutocomplete(ctx, i, cmd, data)
			return
		}
		opts := map[string]any{}
		for _, p := range cmd.params {
			if !p.required {
				opts[p.name] = p.def
			}
		}
		for _, o := range data.Options {
			switch o.Type {
			case discordgo.ApplicationCommandOptionInteger:
				opts[o.Name] = int(o.IntValue())
			default:
				opts[o.Name] = o.StringValue()
			}
		}
		cmd.callback(ctx, i, opts)
	case discordgo.InteractionMessageComponent, discordgo.InteractionModalSubmit:
		a.dispatchComponent(ctx, i)
	}
}

// Answers an autocomplete request from the focused option's source
func (a *DiscordAdapter) runAutocomplete(ctx context.Context, i *discordgo.Interaction, cmd *appCommand, data discordgo.ApplicationCommandInteractionData) {
	for _, o := range data.Options {
		if !o.Focused {
			continue
		}
		source := cmd.autocomplete[o.Name]
		if source == nil {
			return
		}
		choices := source(ctx, i, o.StringValue())
		client := a.liveClient()
		if client == nil {
			return
		}
		if err := client.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: choices}}, discordgo.WithContext(ctx)); err != nil {
			slog.Error(fmt.Sprintf("Ignoring exception in autocomplete for %s: %s", pyrepr.Repr(cmd.name), dpyError(err)))
		}
		return
	}
}
