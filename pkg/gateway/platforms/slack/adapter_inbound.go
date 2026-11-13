package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Installed by the process wiring over pkg/commands, the native slash names the Slack manifest declares
var NativeSlashNames func(ctx context.Context) []string

// Installed by the process wiring over pkg/commands, canonical name and busy policy of a typed command
var ResolveCommandBusy func(name string) (canonical, busyPolicy string, ok bool)

// Plugin Slack action handlers, a package var the way hermes tests patch the plugin manager
var slackActionHandlersFn = func(ctx context.Context) ([]*plugins.SlackActionHandler, error) {
	manager := plugins.GetPluginManager(ctx)
	if manager == nil {
		return nil, errors.New("plugin manager unavailable")
	}
	return manager.GetSlackActionHandlers(), nil
}

// Sleeps for d unless ctx ends first, the asyncio.sleep the hermes tests patch
var sleepCtx = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Gateway dispatch of a built inbound event, a package var the way hermes tests replace handle_message
var handleMessageFn = func(a *SlackAdapter, ctx context.Context, event *gateway.MessageEvent) error {
	return a.HandleMessage(ctx, event)
}

// Inbound message entry the reaction and file share paths call, a package var the way hermes tests patch it
var handleSlackMessageFn = (*SlackAdapter).handleSlackMessage

// Delay letting the message file_share event land before the file_shared fallback
const fileSharedSettle = 750 * time.Millisecond

// Matcher for an action id string, a pattern or a Bolt constraint mapping
func newActionMatcher(actionID any) (actionMatcher, error) {
	switch v := actionID.(type) {
	case string:
		return actionMatcher{id: v}, nil
	case *regexp.Regexp:
		return actionMatcher{pattern: v}, nil
	case map[string]any:
		constraints := map[string]actionMatcher{}
		for key, value := range v {
			switch key {
			case "action_id", "block_id", "type", "callback_id":
			default:
				return actionMatcher{}, fmt.Errorf("unexpected keys %s for action listener", pyrepr.Repr(key))
			}
			m, err := newActionMatcher(value)
			if err != nil {
				return actionMatcher{}, err
			}
			constraints[key] = m
		}
		return actionMatcher{constraints: constraints}, nil
	}
	return actionMatcher{}, fmt.Errorf("action ($constraints) must be any of str, Pattern, and dict but %s was given", pyrepr.TypeName(actionID))
}

// AsyncApp event registration for plugin factories, an event type string or a pattern
func (app *slackApp) Event(matcher any, listener func(ctx context.Context, event, body map[string]any)) error {
	switch v := matcher.(type) {
	case string:
		app.event(v, listener)
	case *regexp.Regexp:
		app.eventPattern(v, listener)
	default:
		return fmt.Errorf("event type must be str or Pattern but %s was given", pyrepr.TypeName(matcher))
	}
	return nil
}

// AsyncApp block action registration for plugin factories
func (app *slackApp) Action(matcher any, listener func(ctx context.Context, ack func(payload map[string]any) error, body, action map[string]any)) error {
	m, err := newActionMatcher(matcher)
	if err != nil {
		return err
	}
	app.action(m, func(ctx context.Context, ack ackFunc, body, action map[string]any) { listener(ctx, ack, body, action) })
	return nil
}

// AsyncApp slash command registration for plugin factories, a command name or a pattern
func (app *slackApp) Command(matcher any, listener func(ctx context.Context, ack func(payload map[string]any) error, command map[string]any)) error {
	var pattern *regexp.Regexp
	switch v := matcher.(type) {
	case string:
		pattern = regexp.MustCompile("^" + regexp.QuoteMeta(v) + "$")
	case *regexp.Regexp:
		pattern = v
	default:
		return fmt.Errorf("command must be str or Pattern but %s was given", pyrepr.TypeName(matcher))
	}
	app.command(pattern, func(ctx context.Context, ack ackFunc, command map[string]any) { listener(ctx, ack, command) })
	return nil
}

// Inbound event handler shape shared by every Bolt event listener
type inboundHandler = func(ctx context.Context, event, body map[string]any) error

// Wires every Bolt listener onto the app, which must run before Socket Mode starts
func (a *SlackAdapter) registerBoltHandlers(ctx context.Context) error {
	app := a.currentApp()
	if app == nil {
		return errors.New("Slack app is not initialized")
	}
	noop := func(context.Context, map[string]any, map[string]any) error { return nil }
	reaction := func(removed bool) inboundHandler {
		return func(ctx context.Context, event, _ map[string]any) error {
			return a.handleSlackReaction(ctx, event, removed)
		}
	}
	listenerFor := func(handler inboundHandler) eventListener {
		return func(ctx context.Context, event, body map[string]any) {
			if err := handler(ctx, event, body); err != nil {
				slog.Error(fmt.Sprintf("Failed to run listener function (error: %v)", err))
			}
		}
	}
	for _, route := range []struct {
		eventType string
		handler   inboundHandler
	}{
		{"message", a.handleSlackMessage}, {"app_mention", a.handleSlackMessage},
		{"app_home_opened", a.handleAppHomeOpened},
		{"app_context_changed", a.handleAppContextChanged},
		{"file_shared", a.handleSlackFileShared}, {"file_created", noop},
		{"file_change", noop}, {"reaction_added", reaction(false)},
		{"reaction_removed", reaction(true)},
		{"assistant_thread_started", a.handleAssistantThreadLifecycleEvent},
		{"assistant_thread_context_changed", a.handleAssistantThreadLifecycleEvent},
	} {
		app.event(route.eventType, listenerFor(route.handler))
	}
	app.eventPattern(regexp.MustCompile(`.*`), func(_ context.Context, event, body map[string]any) {
		eventType := getOr(orEmptyMap(event), "type", getOr(asMap(getOr(orEmptyMap(body), "event", map[string]any{})), "type", "unknown"))
		slog.Debug(fmt.Sprintf("[Slack] Ignoring unhandled event type=%s (no listener registered; subscribed "+
			"events not handled by Icebath can be removed from the Slack app manifest via "+
			"`icebath slack manifest`)", pyrepr.Str(eventType)))
	})
	var slashNames []string
	if NativeSlashNames != nil {
		slashNames = NativeSlashNames(ctx)
	}
	slashPattern := regexp.MustCompile(`^/icebath$`)
	if len(slashNames) > 0 {
		quoted := make([]string, len(slashNames))
		for i, n := range slashNames {
			quoted[i] = regexp.QuoteMeta(n)
		}
		slashPattern = regexp.MustCompile(`^/(?:` + strings.Join(quoted, "|") + `)$`)
	}
	app.command(slashPattern, a.handleIcebathCommand)
	for _, id := range approvalActionOrder {
		app.action(actionMatcher{id: id}, a.handleApprovalAction)
	}
	for _, id := range confirmActionOrder {
		app.action(actionMatcher{id: id}, a.handleSlashConfirmAction)
	}
	app.action(actionMatcher{id: "hermes_feedback"}, a.handleFeedbackAction)
	app.action(actionMatcher{pattern: regexp.MustCompile(`^hermes_clarify_choice_\d+$`)}, a.handleClarifyAction)
	app.action(actionMatcher{id: "hermes_clarify_other"}, a.handleClarifyAction)
	for _, id := range modelPickerActionIDs {
		app.action(actionMatcher{id: id}, a.handleModelPickerAction)
	}
	if err := a.registerPluginActionHandlers(ctx); err != nil {
		return err
	}
	a.WirePluginHandlers(ctx, app)
	return nil
}

// Wires plugin Slack action callbacks once per app, each wrapped so a failure still acks
func (a *SlackAdapter) registerPluginActionHandlers(ctx context.Context) error {
	handlers, err := slackActionHandlersFn(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("[Slack] Could not load plugin action handlers: %v", err))
		handlers = nil
	}
	a.mu.Lock()
	app := a.app
	if a.pluginActionsApp != app {
		a.pluginActionsApp, a.pluginActionsWired = app, map[[2]string]bool{}
	}
	var pending []*plugins.SlackActionHandler
	for _, h := range handlers {
		if !a.pluginActionsWired[[2]string{pyrepr.Repr(h.ActionID), h.Plugin}] {
			pending = append(pending, h)
		}
	}
	a.mu.Unlock()
	for _, h := range pending {
		matcher, err := newActionMatcher(h.ActionID)
		if err != nil {
			return err
		}
		app.action(matcher, wrapPluginAction(h.Callback, h.Plugin))
		a.mu.Lock()
		a.pluginActionsWired[[2]string{pyrepr.Repr(h.ActionID), h.Plugin}] = true
		a.mu.Unlock()
		slog.Debug(fmt.Sprintf("[Slack] Registered plugin action handler %s (from %s)", pyrepr.Str(h.ActionID), h.Plugin))
	}
	if len(pending) > 0 {
		slog.Info(fmt.Sprintf("[Slack] Wired %d plugin action handler(s)", len(pending)))
	}
	return nil
}

// Listener running a plugin callback, logging any failure and acking so Slack never retries
func wrapPluginAction(cb any, pluginName string) actionListener {
	return func(ctx context.Context, ack ackFunc, body, action map[string]any) {
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			plain := func(payload map[string]any) error { return ack(payload) }
			switch f := cb.(type) {
			case func(context.Context, func(map[string]any) error, map[string]any, map[string]any) error:
				return f(ctx, plain, body, action)
			case func(context.Context, func(map[string]any) error, map[string]any, map[string]any):
				f(ctx, plain, body, action)
				return nil
			}
			return fmt.Errorf("TypeError: '%s' object is not callable", pyrepr.TypeName(cb))
		}()
		if err != nil {
			slog.Error(fmt.Sprintf("[Slack] Plugin '%s' action handler raised: %v", pluginName, err))
			_ = ack(nil)
		}
	}
}

// Late plugin loads rewire action handlers on the live app and the base handler factories
func (a *SlackAdapter) RewirePluginHandlers(ctx context.Context) {
	a.mu.Lock()
	live := a.app != nil && a.pluginActionsApp == a.app
	a.mu.Unlock()
	if live {
		if err := a.registerPluginActionHandlers(ctx); err != nil {
			slog.Error(fmt.Sprintf("[Slack] Plugin action handler rewire failed: %v", err))
		}
	}
	a.BasePlatformAdapter.RewirePluginHandlers(ctx)
}

// Primary bot user id, empty before auth
func (a *SlackAdapter) primaryBotUID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.botUserID == nil {
		return ""
	}
	return *a.botUserID
}

// Bot user id of the team, the primary one when the team has no entry
func (a *SlackAdapter) teamBotUID(teamID string) string {
	a.mu.Lock()
	uid, ok := a.teamBotUserIDs[teamID]
	a.mu.Unlock()
	if ok {
		return uid
	}
	return a.primaryBotUID()
}

// Bot user id of the team when set, the primary one otherwise
func (a *SlackAdapter) teamBotUIDOrPrimary(teamID string) string {
	a.mu.Lock()
	uid := a.teamBotUserIDs[teamID]
	a.mu.Unlock()
	if uid != "" {
		return uid
	}
	return a.primaryBotUID()
}

// Suggested prompts from extra suggested_prompts, invalid rows skipped and capped at four
func (a *SlackAdapter) assistantSuggestedPrompts() (string, []map[string]any) {
	raw := a.extra()["suggested_prompts"]
	title := ""
	rows := raw
	if m, ok := raw.(map[string]any); ok {
		title = pystr.Strip(strOrEmpty(m["title"]))
		rows = m["prompts"]
	}
	list, ok := rows.([]any)
	if !ok {
		return title, nil
	}
	var prompts []map[string]any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		promptTitle := pystr.Strip(strOrEmpty(m["title"]))
		promptMessage := pystr.Strip(strOrEmpty(m["message"]))
		if promptTitle != "" && promptMessage != "" {
			prompts = append(prompts, map[string]any{"title": pystr.Slice(promptTitle, 0, 75), "message": promptMessage})
		}
		if len(prompts) >= 4 {
			break
		}
	}
	return title, prompts
}

// Sets the Slack AI suggested prompts of a channel or thread, failures logged at debug
func (a *SlackAdapter) setAssistantSuggestedPrompts(ctx context.Context, channelID, teamID, threadTS string) {
	if a.currentApp() == nil || channelID == "" {
		return
	}
	title, prompts := a.assistantSuggestedPrompts()
	if len(prompts) == 0 {
		return
	}
	rows := make([]any, len(prompts))
	for i, p := range prompts {
		rows[i] = p
	}
	kwargs := map[string]any{"channel_id": channelID, "prompts": rows}
	if title != "" {
		kwargs["title"] = title
	}
	if threadTS != "" {
		kwargs["thread_ts"] = threadTS
	}
	if _, err := a.getClient(channelID, teamID).call(ctx, "assistant.threads.setSuggestedPrompts", kwargs); err != nil {
		slog.Debug(fmt.Sprintf("[Slack] assistant.threads.setSuggestedPrompts failed: %v", err))
	}
}

// Whether visible Slack AI DM threads get a title, on unless set to a false word
func (a *SlackAdapter) assistantThreadTitleEnabled() bool {
	raw, ok := a.extra()["assistant_thread_titles"]
	if !ok {
		return true
	}
	if s, isStr := raw.(string); isStr {
		switch pystr.Lower(pystr.Strip(s)) {
		case "0", "false", "no", "off":
			return false
		}
		return true
	}
	return pyrepr.Truthy(raw)
}

var whitespaceRunRe = mustPyRe(`\s+`)

// Titles a visible Slack AI DM thread once from the first prompt, failures logged at debug
func (a *SlackAdapter) setAssistantThreadTitle(ctx context.Context, channelID, threadTS, titleSource, teamID string) {
	if a.currentApp() == nil || channelID == "" || threadTS == "" || titleSource == "" || !a.assistantThreadTitleEnabled() {
		return
	}
	key := workspaceThreadKey(teamID, channelID, threadTS)
	if key == nil {
		return
	}
	a.mu.Lock()
	titled := a.titledAssistantThreads[*key]
	a.mu.Unlock()
	if titled {
		return
	}
	title, _ := whitespaceRunRe.Replace(titleSource, " ", -1, -1)
	title = pystr.Strip(title)
	if title == "" || strings.HasPrefix(title, "/") {
		return
	}
	if pystr.Len(title) > 80 {
		title = pystr.Rstrip(pystr.Slice(title, 0, 77)) + "..."
	}
	if err := sessionTitleMethod(a.getClient(channelID, teamID))(ctx, channelID, threadTS, title); err != nil {
		slog.Debug(fmt.Sprintf("[Slack] session title set failed: %v", err))
		return
	}
	a.mu.Lock()
	a.titledAssistantThreads[*key] = true
	evictOldestByTS(a.titledAssistantThreads, titledAssistantThreadsMax, func(k threadKey) any { return k[2] })
	a.mu.Unlock()
}

// Drops oldest ts first down to half the cap once the set exceeds it
func evictOldestByTS[K comparable](entries map[K]bool, limit int, tsGetter func(K) any) {
	if len(entries) > limit {
		discardOldestFromSet(entries, len(entries)-limit/2, tsGetter)
	}
}

// Session store surface that primes a DM session without an agent turn
type sessionCreator interface {
	GetOrCreateSession(ctx context.Context, source *gateway.SessionSource, forceNew, touchActivity bool) (*gateway.SessionEntry, error)
}

// Primes the session store for a DM, thread scoped when a thread ts is given
func (a *SlackAdapter) seedDMSession(ctx context.Context, metadata map[string]string, threadTS *string, failLog string) {
	store, _ := a.SessionStore().(sessionCreator)
	channelID, userID := metadata["channel_id"], metadata["user_id"]
	if store == nil || channelID == "" || userID == "" {
		return
	}
	a.mu.Lock()
	chatName, ok := a.channelNameCache.Get(teamKey{metadata["team_id"], channelID})
	a.mu.Unlock()
	if !ok {
		chatName = channelID
	}
	fields := gateway.SourceFields{ChatName: &chatName, ChatType: "dm", UserID: &userID, ThreadID: threadTS}
	if topic := metadata["context_channel_id"]; topic != "" {
		fields.ChatTopic = &topic
	}
	if team := metadata["team_id"]; team != "" {
		fields.ScopeID = &team
	}
	source := a.BuildSource(ctx, channelID, fields)
	if _, err := store.GetOrCreateSession(ctx, source, false, true); err != nil {
		slog.Debug(failLog, "error", err)
	}
}

// Caches assistant lifecycle identity, seeds the thread session and sets suggested prompts
func (a *SlackAdapter) handleAssistantThreadLifecycleEvent(ctx context.Context, event, body map[string]any) error {
	metadata := extractAssistantThreadMetadata(event, body)
	a.cacheAssistantThreadMetadata(metadata)
	if threadTS := metadata["thread_ts"]; threadTS != "" {
		a.seedDMSession(ctx, metadata, &threadTS,
			fmt.Sprintf("[Slack] Failed to seed assistant thread session for %s/%s", metadata["channel_id"], threadTS))
	}
	a.setAssistantSuggestedPrompts(ctx, metadata["channel_id"], metadata["team_id"], metadata["thread_ts"])
	return nil
}

// Caches the current Agent view context without entering the agent loop
func (a *SlackAdapter) handleAppContextChanged(_ context.Context, event, body map[string]any) error {
	a.cacheAgentViewContext(agentViewEventFields(event, body))
	return nil
}

// Context channel, user and team of an Agent view lifecycle event, empty when absent
func agentViewEventFields(event, body map[string]any) map[string]string {
	context := firstTruthyValue(event["context"], event["app_context"])
	if context == nil {
		context = map[string]any{}
	}
	userID := firstTruthyValue(event["user"], event["user_id"])
	return map[string]string{
		"context_channel_id": contextChannelID(context),
		"user_id":            strOrEmpty(userID), "team_id": strOrEmpty(eventTeamID(event, body))}
}

// Agent DM open lifecycle caching identity and seeding the DM session without a reply
func (a *SlackAdapter) handleAppHomeOpened(ctx context.Context, event, body map[string]any) error {
	if event["tab"] != "messages" {
		return nil
	}
	channelID := firstTruthyValue(event["channel"], event["channel_id"])
	fields := agentViewEventFields(event, body)
	if fields["team_id"] != "" && pyrepr.Truthy(channelID) {
		a.rememberChannelTeam(pyrepr.Str(channelID), fields["team_id"])
	}
	metadata := map[string]string{
		"channel_id": strOrEmpty(channelID), "user_id": fields["user_id"],
		"team_id": fields["team_id"], "context_channel_id": fields["context_channel_id"]}
	a.cacheAgentViewContext(metadata)
	a.seedDMSession(ctx, metadata, nil, fmt.Sprintf("[Slack] Failed to seed agent DM session for %s", metadata["channel_id"]))
	a.setAssistantSuggestedPrompts(ctx, metadata["channel_id"], metadata["team_id"], "")
	return nil
}

// Reaction names mapped to the unicode emoji a user would type
var reactionEmojiMap = map[string]string{
	"thumbsup": "👍", "+1": "👍", "thumbsdown": "👎", "-1": "👎", "white_check_mark": "✅",
	"heavy_check_mark": "✅", "x": "❌", "no_entry": "⛔", "warning": "⚠️", "rotating_light": "🚨",
	"eyes": "👀", "rocket": "🚀", "tada": "🎉", "fire": "🔥", "wave": "👋"}

// Forwards a reaction to hooks and, when opted in, as a synthetic message from the reactor
func (a *SlackAdapter) handleSlackReaction(ctx context.Context, event map[string]any, removed bool) error {
	item := asMap(event["item"])
	if item["type"] != "message" {
		return nil
	}
	channelID, msgTS := strOrEmpty(item["channel"]), strOrEmpty(item["ts"])
	reactionName, userID := strOrEmpty(event["reaction"]), strOrEmpty(event["user"])
	if channelID == "" || msgTS == "" || userID == "" || reactionName == "" {
		return nil
	}
	if bot := a.primaryBotUID(); bot != "" && userID == bot {
		return nil
	}
	a.mu.Lock()
	teamID, _ := a.channelTeam.Get(channelID)
	if teamID == "" && a.teamClients.Len() > 0 {
		teamID = a.teamClients.Keys()[0]
	}
	var client *webClient
	if teamID != "" {
		client, _ = a.teamClients.Get(teamID)
	}
	a.mu.Unlock()
	action := "added"
	if removed {
		action = "removed"
	}
	if handler := a.ReactionHandler(); handler != nil {
		err := handler(ctx, map[string]any{
			"platform": "slack", "event_name": "reaction:" + action,
			"reaction": reactionName, "user_id": userID,
			"item_user_id": event["item_user"], "item_type": item["type"],
			"channel_id": channelID, "message_ts": msgTS, "team_id": teamID,
			"event_ts": event["event_ts"], "raw_event": event})
		if err != nil {
			slog.Debug(fmt.Sprintf("[Slack] reaction hook forwarding failed: %v", err))
		}
	}
	triggers := a.slackReactionTriggers(ctx)
	if triggers == nil {
		return nil
	}
	explicitAllowlist := len(triggers) > 0
	if explicitAllowlist && !triggers[pystr.StripChars(reactionName, ":")] {
		return nil
	}
	threadTS := a.reactionThreadTS(ctx, client, channelID, msgTS, event, teamID, explicitAllowlist)
	if threadTS == nil {
		return nil
	}
	return handleSlackMessageFn(a, ctx, a.syntheticReactionEvent(ctx, event, action, *threadTS, teamID), nil)
}

// Message shaped event of a reaction, optionally handed off to the configured target channel
func (a *SlackAdapter) syntheticReactionEvent(ctx context.Context, event map[string]any, action, threadTS, teamID string) map[string]any {
	item := asMap(event["item"])
	channelID, msgTS := item["channel"], item["ts"]
	reactionName, userID := strOrEmpty(event["reaction"]), event["user"]
	emojiText, ok := reactionEmojiMap[reactionName]
	if !ok {
		emojiText = reactionName
	}
	ts := event["event_ts"]
	if !pyrepr.Truthy(ts) {
		ts = fmt.Sprintf("reaction-%s-%s-%s", pyrepr.Str(msgTS), reactionName, pyrepr.Str(userID))
	}
	synthetic := map[string]any{
		"type":                  "message",
		"user":                  userID,
		"text":                  "reaction:" + action + ":" + emojiText,
		"channel":               channelID,
		"ts":                    ts,
		"thread_ts":             threadTS,
		"_hermes_force_process": true,
		"_hermes_reaction": map[string]any{
			"name": reactionName, "action": action, "reacted_to_ts": msgTS,
			"event_ts": event["event_ts"]}}
	if teamID != "" {
		synthetic["team"] = teamID
	}
	targetChannel, targetThread := a.slackReactionTriggerTarget(ctx)
	if targetChannel != "" {
		synthetic["channel"] = targetChannel
		synthetic["channel_type"] = "channel"
		if strings.HasPrefix(targetChannel, "D") {
			synthetic["channel_type"] = "im"
		}
		synthetic["_hermes_reaction_source_channel"] = channelID
		if targetThread != "" {
			synthetic["thread_ts"] = targetThread
		} else {
			delete(synthetic, "thread_ts")
			synthetic["_hermes_no_thread_response"] = true
		}
	}
	return synthetic
}

// Thread a reaction routes into, nil to drop, only the bot's own messages without an allowlist
func (a *SlackAdapter) reactionThreadTS(ctx context.Context, client *webClient, channelID, msgTS string, event map[string]any, teamID string, explicitAllowlist bool) *string {
	threadTS := msgTS
	itemUser := strOrEmpty(event["item_user"])
	if client != nil {
		history, err := client.call(ctx, "conversations.replies", map[string]any{"channel": channelID, "ts": msgTS, "limit": 1, "inclusive": true})
		if err != nil {
			slog.Debug(fmt.Sprintf("[Slack] reaction thread_ts lookup failed for %s: %v", msgTS, err))
			return &threadTS
		}
		messages := asList(history.get("messages"))
		if len(messages) == 0 {
			return &threadTS
		}
		first := asMap(messages[0])
		if v := firstTruthyValue(first["thread_ts"], first["ts"]); v != nil {
			threadTS = pyrepr.Str(v)
		}
		if itemUser == "" {
			itemUser = strOrEmpty(first["user"])
		}
	}
	if !explicitAllowlist {
		botUID := a.teamBotUIDOrPrimary(teamID)
		if itemUser != "" && botUID != "" && itemUser != botUID {
			return nil
		}
	}
	return &threadTS
}

var reactionTriggerSplitRe = mustPyRe(`[,\s]+`)

// Reaction routing opt in, nil disabled, empty for every emoji, else the allowlisted names
func (a *SlackAdapter) slackReactionTriggers(ctx context.Context) map[string]bool {
	raw, ok := a.extra()["reaction_triggers"]
	if !ok || raw == nil {
		secret, _ := gateway.GetScopedSecret(ctx, "SLACK_REACTION_TRIGGERS", "", false)
		if secret == "" {
			return nil
		}
		raw = secret
	}
	clean := func(p any) string { return pystr.StripChars(pystr.Strip(pyrepr.Str(p)), ":") }
	switch v := raw.(type) {
	case bool:
		if v {
			return map[string]bool{}
		}
		return nil
	case []any:
		out := map[string]bool{}
		for _, p := range v {
			if c := clean(p); c != "" {
				out[c] = true
			}
		}
		return out
	case []string:
		out := map[string]bool{}
		for _, p := range v {
			if c := clean(p); c != "" {
				out[c] = true
			}
		}
		return out
	}
	text := pystr.Strip(strOrEmpty(raw))
	switch pystr.Lower(text) {
	case "", "false", "0", "no", "off":
		return nil
	case "true", "1", "yes", "on", "all", "*":
		return map[string]bool{}
	}
	out := map[string]bool{}
	parts, _ := reactionTriggerSplitRe.Replace(text, ",", -1, -1)
	for _, p := range strings.Split(parts, ",") {
		if c := clean(p); c != "" {
			out[c] = true
		}
	}
	return out
}

// Optional reaction handoff target channel and thread from C123 or C123 colon ts
func (a *SlackAdapter) slackReactionTriggerTarget(ctx context.Context) (string, string) {
	raw, ok := a.extra()["reaction_trigger_target"]
	if !ok || raw == nil {
		secret, _ := gateway.GetScopedSecret(ctx, "SLACK_REACTION_TRIGGER_TARGET", "", false)
		raw = secret
	}
	channel, thread, _ := strings.Cut(pystr.Strip(strOrEmpty(raw)), ":")
	return pystr.Strip(channel), pystr.Strip(thread)
}

// First share entry of the channel, else the first share anywhere, else empty
func firstFileShare(fileObj map[string]any, channelID string) map[string]any {
	var share any
	shares := asMap(fileObj["shares"])
	for _, bucketKey := range sortedKeys(shares) {
		bucket, ok := shares[bucketKey].(map[string]any)
		if !ok {
			continue
		}
		if channelShares := asList(bucket[channelID]); len(channelShares) > 0 {
			return orEmptyMap(asMapOrNil(channelShares[0]))
		}
		if share == nil {
			for _, key := range sortedKeys(bucket) {
				if list := asList(bucket[key]); len(list) > 0 {
					share = list[0]
					break
				}
			}
		}
	}
	return orEmptyMap(asMapOrNil(share))
}

// Mapping value or nil for anything else, so a falsy share reads as empty
func asMapOrNil(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// File share fallback for video uploads Slack never delivered as message files
func (a *SlackAdapter) handleSlackFileShared(ctx context.Context, event, body map[string]any) error {
	channelID := strOrEmpty(firstTruthyValue(event["channel_id"], event["channel"]))
	if a.IsIgnoredChannel(ctx, channelID) {
		slog.Info(fmt.Sprintf("[Slack] Ignoring file_shared event in configured ignored channel %s", channelID))
		return nil
	}
	fileID := strOrEmpty(firstTruthyValue(event["file_id"], asMap(event["file"])["id"]))
	if channelID == "" || fileID == "" {
		return nil
	}
	teamID := eventTeamID(event, body)
	var client *webClient
	if teamID != "" {
		a.mu.Lock()
		client, _ = a.teamClients.Get(teamID)
		a.mu.Unlock()
	}
	if client == nil {
		client = a.getClient(channelID, "")
	}
	info, err := client.call(ctx, "files.info", map[string]any{"file": fileID})
	if err != nil {
		var apiErr *slackAPIError
		var response map[string]any
		if errors.As(err, &apiErr) {
			response = apiErr.Response
		}
		detail := describeSlackAPIError(response, map[string]any{"id": fileID})
		if detail == "" {
			detail = err.Error()
		}
		slog.Warn(fmt.Sprintf("[Slack] files.info error for file_shared %s: %s", fileID, detail))
		return nil
	}
	fileObj := asMap(info.get("file"))
	if !strings.HasPrefix(pyrepr.Str(getOr(fileObj, "mimetype", "")), "video/") {
		return nil
	}
	share := firstFileShare(fileObj, channelID)
	ts := strOrEmpty(firstTruthyValue(share["ts"], event["event_ts"]))
	threadTS := strOrEmpty(share["thread_ts"])
	if err := sleepCtx(ctx, fileSharedSettle); err != nil {
		return err
	}
	if ts != "" && a.dedup.IsDuplicate(workspaceEventID(teamID, ts)) {
		return nil
	}
	channelType := "channel"
	if strings.HasPrefix(channelID, "D") {
		channelType = "im"
	}
	user := event["user_id"]
	if !pyrepr.Truthy(user) {
		user = getOr(fileObj, "user", "")
	}
	fallback := map[string]any{
		"type": "message", "subtype": "file_share", "text": "", "user": user,
		"channel": channelID, "channel_type": channelType, "team": teamID, "ts": "",
		"files": []any{fileObj}}
	if threadTS != "" && threadTS != ts {
		fallback["thread_ts"] = threadTS
	}
	return handleSlackMessageFn(a, ctx, fallback, nil)
}

// Records a thread as bot mentioned so later replies wake the bot, workspace scoped when known
func (a *SlackAdapter) registerMentionedThread(threadTS, teamID string) {
	if threadTS == "" {
		return
	}
	a.mu.Lock()
	a.mentionedThreads[workspaceMessageMarker(teamID, threadTS)] = true
	a.mu.Unlock()
	a.trimMentionedThreads()
}

// Whether this bot authored the thread root, cache first then one cached fetch on a miss
func (a *SlackAdapter) botAuthoredThreadRoot(ctx context.Context, channelID, threadTS, teamID string) bool {
	if threadTS == "" {
		return false
	}
	botUID := a.teamBotUID(teamID)
	if botUID == "" {
		return false
	}
	prefix := channelID + ":" + threadTS + ":"
	for attempt := 0; attempt < 2; attempt++ {
		a.mu.Lock()
		var found *threadContextCache
		for _, k := range a.threadContextCache.Keys() {
			if strings.HasPrefix(k, prefix) {
				found, _ = a.threadContextCache.Get(k)
				break
			}
		}
		a.mu.Unlock()
		if found != nil {
			return found.parentUserID != "" && found.parentUserID == botUID
		}
		if attempt == 0 {
			a.fetchThreadContext(ctx, channelID, threadTS, "", teamID, 30, "", false)
		}
	}
	return false
}

// Whether an unmentioned message wakes the bot through a bot thread, a prior mention or a session
func (a *SlackAdapter) shouldWakeOnUnmentionedMessage(ctx context.Context, eventThreadTS, channelID, userID string, isThreadReply bool, teamID, chatType string) bool {
	if eventThreadTS == "" {
		return false
	}
	marker := workspaceMessageMarker(teamID, eventThreadTS)
	a.mu.Lock()
	botThread := a.botMessageTS[marker] || a.botMessageTS[eventThreadTS]
	mentioned := a.mentionedThreads[marker] || a.mentionedThreads[eventThreadTS]
	a.mu.Unlock()
	if isThreadReply && botThread {
		return true
	}
	if mentioned {
		return true
	}
	if isThreadReply && a.hasActiveSessionForThread(ctx, channelID, eventThreadTS, userID, teamID, chatType) {
		return true
	}
	if isThreadReply && a.botAuthoredThreadRoot(ctx, channelID, eventThreadTS, teamID) {
		return true
	}
	if isThreadReply {
		if botUID := a.teamBotUID(teamID); botUID != "" {
			parentText := a.fetchThreadParentText(ctx, channelID, eventThreadTS, teamID, false)
			if parentText != "" && strings.Contains(parentText, "<@"+botUID+">") {
				if !a.slackStrictMention(ctx) {
					a.registerMentionedThread(eventThreadTS, "")
				}
				return true
			}
		}
	}
	return false
}

// Text with unseen Block Kit rich text and the redacted block payload appended
func appendBlockText(text string, blocks []any, botUID string) string {
	blocksText := extractAdditionalTextFromSlackBlocks(blocks, text, botUID)
	stripped := pystr.Strip(blocksText)
	if stripped != "" {
		slog.Debug(fmt.Sprintf("Slack: extracted additional text from blocks (likely quoted/forwarded content; chars=%d)", pystr.Len(stripped)))
		text = pystr.Strip(pystr.Strip(text) + "\n" + stripped)
	}
	if payload := serializeSlackBlocksForAgent(blocks, slackUnfurlBlocksMaxChars); payload != "" {
		text = pystr.Strip(pystr.Strip(text) + "\n\n" + payload)
	}
	return text
}

// Runner authorization surface reached through the bound message handler owner
type runnerAuthorizer interface {
	IsUserAuthorized(ctx context.Context, source *gateway.SessionSource, allowAdapterDelegation bool) (bool, error)
}

// Runner authorization check of the bound handler owner, nil when the handler has none
func (a *SlackAdapter) runnerAuthFn() runnerAuthorizer {
	auth, _ := a.MessageHandlerOwner().(runnerAuthorizer)
	return auth
}

// Whether the sender is definitively unauthorized, injected check first then the runner check
func (a *SlackAdapter) earlyRejectUnauthorized(ctx context.Context, userID, channelID string, isDM bool, threadID *string) bool {
	chatType := "group"
	if isDM {
		chatType = "dm"
	}
	var decision *bool
	if userID != "" && a.HasAuthorizationCheck() {
		decision = a.IsSenderAuthorized(userID, &chatType, &channelID, gateway.AuthorizationOpts{ThreadID: threadID})
	}
	if auth := a.runnerAuthFn(); decision == nil && userID != "" && auth != nil {
		empty := ""
		source := a.BuildSource(ctx, channelID, gateway.SourceFields{ChatName: &empty, ChatType: chatType, UserID: &userID, UserName: &empty, ThreadID: threadID})
		ok, err := auth.IsUserAuthorized(ctx, source, true)
		if err != nil {
			slog.Debug(fmt.Sprintf("[Slack] runner authorization check failed: %v", err))
		}
		decision = &ok
	}
	if decision != nil && !*decision {
		slog.Warn(fmt.Sprintf("[Slack] Early reject of unauthorized user %s in channel %s", userID, channelID))
		return true
	}
	return false
}

// Channel and MPIM gate, free channels and mentions admit, wake checks decide the rest
func (a *SlackAdapter) channelGateAllows(ctx context.Context, channelID, routingText, botUID string, isMentioned, isThreadReply bool, eventThreadTS, userID, teamID string, isDM, forceProcess bool) bool {
	if allowed := a.slackAllowedChannels(ctx); len(allowed) > 0 && !allowed[channelID] {
		slog.Debug(fmt.Sprintf("[Slack] Ignoring message in non-allowed channel: %s", channelID))
		return false
	}
	selfUIDs := map[string]bool{}
	for _, u := range []string{botUID, a.primaryBotUID()} {
		if u != "" {
			selfUIDs[u] = true
		}
	}
	if a.slackIgnoreOtherUserMentions(ctx) && !isMentioned && !slackMessageMentionsSelf(routingText, selfUIDs) &&
		slackMessageAddressedToOtherUser(routingText, selfUIDs) {
		slog.Debug(fmt.Sprintf("[Slack] Ignoring message addressed to another user in channel %s", channelID))
		return false
	}
	threadGated := a.slackThreadRequireMention(ctx) && isThreadReply && !isMentioned
	if forceProcess {
		return true
	}
	freeChannel := a.slackIsFreeChannel(ctx, channelID)
	if !freeChannel && a.slackStrictMention(ctx) && !isMentioned {
		return false
	}
	if threadGated {
		slog.Debug(fmt.Sprintf("[Slack] Ignoring thread reply without mention (thread_require_mention=true): channel=%s thread_ts=%s", channelID, eventThreadTS))
		return false
	}
	if freeChannel {
		return true
	}
	if !isMentioned {
		chatType := "group"
		if isDM {
			chatType = "dm"
		}
		return a.shouldWakeOnUnmentionedMessage(ctx, eventThreadTS, channelID, userID, isThreadReply, teamID, chatType)
	}
	return true
}

// Plain message event of a message_changed envelope, nil when malformed or already routed
func (a *SlackAdapter) normalizeChangedMessage(event map[string]any) map[string]any {
	updated, ok := event["message"].(map[string]any)
	if !ok {
		return nil
	}
	originalTS := strOrEmpty(updated["ts"])
	if originalTS != "" {
		a.mu.Lock()
		routed := a.processedMessageTS.Has(originalTS)
		a.mu.Unlock()
		if routed {
			return nil
		}
	}
	editedTS := ""
	if edited, ok := updated["edited"].(map[string]any); ok {
		editedTS = strOrEmpty(edited["ts"])
	}
	outerTS := strOrEmpty(event["ts"])
	changedTS := strOrEmpty(event["event_ts"])
	if changedTS == "" {
		changedTS = editedTS
	}
	if changedTS == "" && outerTS != originalTS {
		changedTS = outerTS
	}
	if changedTS == "" && originalTS != "" {
		changedTS = originalTS + ":changed"
	}
	normalized := make(map[string]any, len(updated)+1)
	for k, v := range updated {
		normalized[k] = v
	}
	for _, key := range []string{"channel", "channel_type", "team", "team_id"} {
		if !pyrepr.Truthy(normalized[key]) && pyrepr.Truthy(event[key]) {
			normalized[key] = event[key]
		}
	}
	if changedTS != "" {
		normalized["_slack_changed_event_ts"] = changedTS
	}
	return normalized
}

// Thread ts of the session key, per thread in DMs and replies, the message ts for reply_in_thread
func (a *SlackAdapter) sessionThreadTS(event map[string]any, ts string, isDM bool, assistantMeta map[string]string) *string {
	if isDM {
		threadTS := strOrEmpty(event["thread_ts"])
		if threadTS == "" {
			threadTS = assistantMeta["thread_ts"]
		}
		if threadTS == "" && a.dmTopLevelThreadsAsSessions() {
			threadTS = ts
		}
		if threadTS == "" {
			return nil
		}
		return &threadTS
	}
	if pyrepr.Truthy(event["_hermes_no_thread_response"]) {
		if t := strOrEmpty(event["thread_ts"]); t != "" {
			return &t
		}
		return nil
	}
	if raw := strOrEmpty(event["thread_ts"]); raw != "" && raw != ts {
		return &raw
	}
	if pyrepr.Truthy(getOr(a.extra(), "reply_in_thread", true)) {
		return &ts
	}
	return nil
}

// Thread context, root images and their types for a thread reply, advancing the watermark
func (a *SlackAdapter) hydrateThreadContext(ctx context.Context, channelID, eventThreadTS, ts, userID, teamID string, isThreadReply, isMentioned, isDM bool) (*string, []string, []string) {
	var channelContext *string
	var rootURLs, rootTypes []string
	if !isThreadReply {
		return channelContext, rootURLs, rootTypes
	}
	chatType := "group"
	if isDM {
		chatType = "dm"
	}
	hasSession := a.hasActiveSessionForThread(ctx, channelID, eventThreadTS, userID, teamID, chatType)
	fetch := func(afterTS string, forceRefresh bool) {
		if threadContext := a.fetchThreadContext(ctx, channelID, eventThreadTS, ts, teamID, 30, afterTS, forceRefresh); threadContext != "" {
			channelContext = &threadContext
		}
	}
	switch {
	case !hasSession:
		fetch("", false)
		rootURLs, rootTypes = a.collectThreadRootImages(ctx, channelID, eventThreadTS, teamID)
	case isMentioned:
		fetch(a.getThreadWatermark(ctx, channelID, eventThreadTS, userID, teamID), true)
	default:
		key := a.threadRehydrationKey(channelID, eventThreadTS, userID, teamID)
		a.mu.Lock()
		checked := a.threadRehydrationChecked[key]
		a.mu.Unlock()
		if checked {
			a.setThreadWatermark(ctx, channelID, eventThreadTS, userID, ts, teamID)
			return channelContext, rootURLs, rootTypes
		}
		if watermark := a.getThreadWatermark(ctx, channelID, eventThreadTS, userID, teamID); watermark != "" {
			fetch(watermark, true)
		}
	}
	a.setThreadWatermark(ctx, channelID, eventThreadTS, userID, ts, teamID)
	a.markThreadRehydrationChecked(channelID, eventThreadTS, userID, teamID)
	return channelContext, rootURLs, rootTypes
}

// Photo, video, voice or document by the first matching media prefix, text when none
func mediaMessageType(mediaTypes []string) gateway.MessageType {
	if len(mediaTypes) == 0 {
		return gateway.MessageTypeText
	}
	for _, kind := range []struct {
		prefix string
		t      gateway.MessageType
	}{{"image/", gateway.MessageTypePhoto}, {"video/", gateway.MessageTypeVideo}, {"audio/", gateway.MessageTypeVoice}} {
		for _, m := range mediaTypes {
			if strings.HasPrefix(m, kind.prefix) {
				return kind.t
			}
		}
	}
	return gateway.MessageTypeDocument
}

// Channel prompt with the bot's Slack identity prepended, nil when neither exists
func (a *SlackAdapter) channelPromptWithIdentity(channelID, teamID string) *string {
	prompt, ok := gateway.ResolveChannelPrompt(a.extra(), channelID, "")
	identity := a.buildIdentityPrompt(teamID)
	if identity != "" {
		if ok {
			prompt = pystr.Strip(identity + "\n\n" + prompt)
		} else {
			prompt = identity
		}
		return &prompt
	}
	if ok {
		return &prompt
	}
	return nil
}

// Marks the message for the reaction lifecycle, evicting oldest ts first past the cap
func (a *SlackAdapter) trackReactingMessage(teamID, ts string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reactingMessageIDs[workspaceMessageMarker(teamID, ts)] = true
	evictOldestByTS(a.reactingMessageIDs, reactingMessageIDsMax, identityTS[any])
}

// Inbound message guard releasing a ts claim this call made when the handler fails
func (a *SlackAdapter) handleSlackMessage(ctx context.Context, event, payload map[string]any) (err error) {
	ts := strOrEmpty(orEmptyMap(event)["ts"])
	a.mu.Lock()
	wasClaimed := a.processedMessageTS.Has(ts)
	a.mu.Unlock()
	wasClaimed = ts != "" && wasClaimed
	release := func() {
		a.mu.Lock()
		claimed := a.processedMessageTS.Has(ts)
		if ts != "" && !wasClaimed && claimed {
			a.processedMessageTS.Delete(ts)
			a.mu.Unlock()
			slog.Warn(fmt.Sprintf("[%s] handler failed after claiming ts=%s; claim released so a retry or edit can re-drive the turn", a.Name(), ts))
			return
		}
		a.mu.Unlock()
	}
	defer func() {
		if r := recover(); r != nil {
			release()
			panic(r)
		}
	}()
	if err = a.handleSlackMessageImpl(ctx, event, payload); err != nil {
		release()
	}
	return err
}

// Whether a bot sender is dropped under allow_bots, own posts always dropping
func (a *SlackAdapter) dropBotSender(ctx context.Context, event map[string]any) bool {
	msgUser := strOrEmpty(event["user"])
	senderIsBot := a.eventDeclaresBotSender(ctx, event)
	if !senderIsBot && msgUser != "" && !pyrepr.Truthy(event["client_msg_id"]) {
		senderIsBot = a.resolveUserIsBot(ctx, msgUser, strOrEmpty(event["channel"]), strOrEmpty(firstTruthyValue(event["team"], event["team_id"])))
	}
	if !senderIsBot {
		return false
	}
	allowBots := a.slackAllowBots(ctx)
	if allowBots == "none" {
		return true
	}
	bot := a.primaryBotUID()
	if allowBots == "mentions" {
		textCheck := slackMentionDetectionText(event)
		if bot != "" && !strings.Contains(textCheck, "<@"+bot+">") {
			slog.Debug(fmt.Sprintf("[Slack] Dropping bot message under allow_bots=mentions: no <@%s> mention in flat text or blocks", bot))
			return true
		}
	}
	return msgUser != "" && bot != "" && msgUser == bot
}

// Message subtypes that are a person speaking, everything else housekeeping
var conversationalSubtypes = map[string]bool{"": true, "file_share": true, "thread_broadcast": true, "me_message": true,
	"bot_message": true, "document_mention": true}

// Normalized event, team and channel of a message worth considering, nil once dropped
func (a *SlackAdapter) prefilterInbound(ctx context.Context, event, payload map[string]any) (map[string]any, string, string, bool) {
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		botName := ""
		if profile, ok := event["bot_profile"].(map[string]any); ok {
			botName = strOrEmpty(profile["name"])
		}
		slog.Debug(fmt.Sprintf("[Slack] event received type=%s subtype=%s user=%s bot_id=%s bot_name=%s channel=%s ts=%s thread_ts=%s",
			pyrepr.Str(event["type"]), pyrepr.Str(event["subtype"]), strOrEmpty(event["user"]), strOrEmpty(event["bot_id"]), botName,
			pyrepr.Str(getOr(event, "channel", "")), pyrepr.Str(getOr(event, "ts", "")), pyrepr.Str(getOr(event, "thread_ts", ""))))
	}
	if event["subtype"] == "message_changed" {
		event = a.normalizeChangedMessage(event)
		if event == nil {
			return nil, "", "", false
		}
	}
	eventTS := strOrEmpty(firstTruthyValue(event["_slack_changed_event_ts"], event["ts"]))
	dedupTeamID := eventTeamID(event, payload)
	if eventTS != "" && a.dedup.IsDuplicate(workspaceEventID(dedupTeamID, eventTS)) {
		return nil, "", "", false
	}
	channelID := strOrEmpty(event["channel"])
	if a.IsIgnoredChannel(ctx, channelID) {
		slog.Info(fmt.Sprintf("[Slack] Ignoring message in configured ignored channel %s", channelID))
		return nil, "", "", false
	}
	if a.dropBotSender(ctx, event) {
		return nil, "", "", false
	}
	subtype, isStr := event["subtype"].(string)
	if (event["subtype"] != nil && !isStr) || !conversationalSubtypes[subtype] {
		slog.Debug(fmt.Sprintf("[Slack] Dropping non-conversational message subtype=%s in channel %s", pyrepr.Str(event["subtype"]), channelID))
		return nil, "", "", false
	}
	return event, dedupTeamID, channelID, true
}

// Whether a peer agent post from a bot user must drop to avoid agent loops
func (a *SlackAdapter) peerBotDrop(ctx context.Context, event map[string]any, userID, botUID, channelID, teamID string, isMentioned bool) bool {
	if userID == "" || userID == botUID {
		return false
	}
	senderIsBotUser := a.eventDeclaresBotSender(ctx, event)
	if !senderIsBotUser {
		senderIsBotUser = a.resolveUserIsBot(ctx, userID, channelID, teamID)
	}
	if !senderIsBotUser {
		return false
	}
	allowBots := a.slackAllowBots(ctx)
	return allowBots == "none" || (allowBots == "mentions" && !isMentioned)
}

// Strips our mention, re-probes a command behind it and remembers the thread
func (a *SlackAdapter) applyBotMention(ctx context.Context, text, originalText, commandProbeText string, isCommandText bool, botUID string, threadTS *string, teamID string) (string, string, string, bool) {
	mention := "<@" + botUID + ">"
	text = pystr.Strip(strings.ReplaceAll(text, mention, ""))
	stripped := pystr.Strip(strings.ReplaceAll(originalText, mention, ""))
	commandText := stripped
	if !strings.HasPrefix(stripped, "/") {
		commandText = rewriteKnownBangCommand(ctx, stripped)
	}
	if strings.HasPrefix(commandText, "/") {
		originalText, text, commandProbeText = commandText, commandText, commandText
		isCommandText = true
	}
	if threadTS != nil && *threadTS != "" && !a.slackStrictMention(ctx) && !a.slackThreadRequireMention(ctx) {
		a.registerMentionedThread(*threadTS, teamID)
	}
	return text, originalText, commandProbeText, isCommandText
}

// Inbound Slack message gated, enriched with context and media, and dispatched to the gateway
func (a *SlackAdapter) handleSlackMessageImpl(ctx context.Context, event, payload map[string]any) error {
	event, dedupTeamID, channelID, ok := a.prefilterInbound(ctx, event, payload)
	if !ok {
		return nil
	}
	originalText := strOrEmpty(event["text"])
	commandProbeText := rewriteKnownBangCommand(ctx, pystr.Lstrip(originalText))
	if commandProbeText != pystr.Lstrip(originalText) {
		originalText = commandProbeText
	}
	isCommandText := strings.HasPrefix(commandProbeText, "/")
	text := originalText
	if blocks := asList(event["blocks"]); len(blocks) > 0 && !isCommandText {
		text = appendBlockText(text, blocks, a.teamBotUID(dedupTeamID))
	}
	text = appendLinkUnfurls(text, asList(event["attachments"]))
	ts := strOrEmpty(event["ts"])
	outerTeamID := dedupTeamID
	assistantMeta := a.lookupAssistantThreadMetadata(event, channelID, strOrEmpty(event["thread_ts"]), outerTeamID, payload)
	userID := strOrEmpty(event["user"])
	if userID == "" {
		userID = assistantMeta["user_id"]
	}
	if channelID == "" {
		channelID = assistantMeta["channel_id"]
	}
	teamID := outerTeamID
	if teamID == "" {
		teamID = assistantMeta["team_id"]
	}
	if teamID == "" {
		a.mu.Lock()
		teamID, _ = a.channelTeam.Get(channelID)
		a.mu.Unlock()
	}
	agentContext := a.agentViewContextForEvent(event, teamID, userID)
	if teamID != "" && channelID != "" {
		a.rememberChannelTeam(channelID, teamID)
	}
	channelType := strOrEmpty(event["channel_type"])
	if channelType == "" && strings.HasPrefix(channelID, "D") {
		channelType = "im"
	}
	isDM := channelType == "im" || channelType == "mpim"
	if isDM && a.slackDisableDMs(ctx) {
		slog.Info(fmt.Sprintf("[Slack] Ignoring DM because Slack DMs are disabled: channel=%s user=%s", channelID, userID))
		return nil
	}
	isOneToOneDM := channelType == "im"
	threadTS := a.sessionThreadTS(event, ts, isDM, assistantMeta)
	if a.earlyRejectUnauthorized(ctx, userID, channelID, isDM, threadTS) {
		return nil
	}
	botUID := a.teamBotUID(teamID)
	routingText := slackMentionDetectionText(event)
	if routingText == "" {
		routingText = originalText
	}
	isMentioned := (botUID != "" && strings.Contains(routingText, "<@"+botUID+">")) || a.slackMessageMatchesMentionPatterns(ctx, routingText)
	eventThreadTS := strOrEmpty(event["thread_ts"])
	isThreadReply := eventThreadTS != "" && eventThreadTS != ts
	forceProcess := pyrepr.Truthy(event["_hermes_force_process"])
	if a.peerBotDrop(ctx, event, userID, botUID, channelID, teamID, isMentioned) {
		return nil
	}
	if !isOneToOneDM && botUID != "" && !a.channelGateAllows(ctx, channelID, routingText, botUID, isMentioned, isThreadReply,
		eventThreadTS, userID, teamID, isDM, forceProcess) {
		return nil
	}
	if claim := strOrEmpty(event["ts"]); claim != "" {
		a.rememberProcessedMessageTS(claim)
	}
	if isMentioned {
		text, originalText, commandProbeText, isCommandText = a.applyBotMention(ctx, text, originalText, commandProbeText, isCommandText, botUID, threadTS, teamID)
	}
	channelContext, rootURLs, rootTypes := a.hydrateThreadContext(ctx, channelID, eventThreadTS, ts, userID, teamID, isThreadReply, isMentioned, isDM)
	mediaURLs, mediaTypes, inlined, text := a.collectInboundMedia(ctx, event, channelID, teamID, text, rootURLs, rootTypes)
	threadTSValue := ""
	if threadTS != nil {
		threadTSValue = *threadTS
	}
	replyExpected := a.slackReplyExpected(ctx, routingText, botUID, channelID,
		isOneToOneDM || isMentioned || isCommandText || forceProcess, threadTS != nil && threadTSValue == ts, isThreadReply && !isDM)
	msgEvent := a.buildMessageEvent(ctx, event, text, originalText, commandProbeText, isCommandText, channelID, teamID, ts, userID,
		threadTS, isDM, mediaURLs, mediaTypes, inlined, channelContext, replyExpected)
	if (isOneToOneDM || isMentioned) && a.ReactionsEnabled(ctx) {
		a.trackReactingMessage(teamID, ts)
	}
	if contextChannel := agentContext["context_channel_id"]; contextChannel != "" && contextChannel != channelID && !isCommandText {
		msgEvent.Text = fmt.Sprintf("[Slack app context: user is viewing channel %s]\n\n%s", contextChannel, msgEvent.Text)
	}
	if ts != "" {
		a.rememberProcessedMessageTS(ts)
	}
	return handleMessageFn(a, ctx, msgEvent)
}

// Resolves names, titles the DM thread and builds the gateway message event
func (a *SlackAdapter) buildMessageEvent(ctx context.Context, event map[string]any, text, originalText, commandProbeText string, isCommandText bool,
	channelID, teamID, ts, userID string, threadTS *string, isDM bool, mediaURLs, mediaTypes []string, mediaTextInlined []bool,
	channelContext *string, replyExpected *bool) *gateway.MessageEvent {
	if isCommandText {
		text = commandProbeText
	}
	msgType := mediaMessageType(mediaTypes)
	if isCommandText {
		msgType = gateway.MessageTypeCommand
	}
	userName := resolveUserNameFn(a, ctx, userID, channelID, teamID)
	channelName := a.resolveChannelName(ctx, channelID, teamID)
	if isDM && threadTS != nil && *threadTS != "" && msgType != gateway.MessageTypeCommand {
		source := originalText
		if source == "" {
			source = text
		}
		a.setAssistantThreadTitle(ctx, channelID, *threadTS, source, teamID)
	}
	chatType := "group"
	if isDM {
		chatType = "dm"
	}
	fields := gateway.SourceFields{ChatName: &channelName, ChatType: chatType, UserID: &userID, UserName: &userName,
		ThreadID: threadTS, MessageID: &ts, IsBot: a.eventDeclaresBotSender(ctx, event)}
	if teamID != "" {
		fields.ScopeID = &teamID
	}
	source := a.BuildSource(ctx, channelID, fields)
	text = a.humanizeUserMentions(ctx, text, channelID, teamID)
	finalText := text
	if isCommandText {
		finalText = commandProbeText
	}
	inlined := make([]*bool, len(mediaTextInlined))
	for i := range mediaTextInlined {
		v := mediaTextInlined[i]
		inlined[i] = &v
	}
	msg := gateway.NewMessageEvent(finalText)
	msg.MessageType, msg.Source, msg.RawMessage, msg.MessageID = msgType, source, event, &ts
	msg.MediaURLs, msg.MediaTypes, msg.MediaTextInlined = mediaURLs, mediaTypes, inlined
	msg.ChannelPrompt, msg.ChannelContext, msg.ReplyExpected = a.channelPromptWithIdentity(channelID, teamID), channelContext, replyExpected
	if threadTS != nil && *threadTS != ts {
		reply := *threadTS
		msg.ReplyToMessageID = &reply
	}
	if skills, ok := gateway.ResolveChannelSkills(a.extra(), channelID, ""); ok {
		msg.AutoSkill = skills
	}
	return msg
}

// Records a user facing attachment diagnostic, else logs the raw failure
func noteAttachmentFailure(notices *[]string, detail, fallbackMsg string) {
	if detail != "" {
		*notices = append(*notices, detail)
		slog.Warn("[Slack] " + detail)
		return
	}
	slog.Warn(fallbackMsg)
}

// Resolves a Slack Connect check_file_info file through files.info, nil when unresolvable
func (a *SlackAdapter) resolveCheckFileInfo(ctx context.Context, f map[string]any, channelID, teamID string, notices *[]string) map[string]any {
	fileID := strOrEmpty(f["id"])
	if fileID == "" {
		return nil
	}
	info, err := a.getClient(channelID, teamID).call(ctx, "files.info", map[string]any{"file": fileID})
	if err != nil {
		if notices != nil {
			var apiErr *slackAPIError
			var response map[string]any
			if errors.As(err, &apiErr) {
				response = apiErr.Response
			}
			noteAttachmentFailure(notices, describeSlackAPIError(response, f), fmt.Sprintf("[Slack] files.info error for %s: %v", fileID, err))
		}
		return nil
	}
	return asMap(info.get("file"))
}

// Image, audio, voice clip, video or document from the mimetype and voice clip heuristics
func slackFileKind(f map[string]any, mimetype string) string {
	for _, prefix := range []string{"image", "audio"} {
		if strings.HasPrefix(mimetype, prefix+"/") {
			return prefix
		}
	}
	if strings.HasPrefix(mimetype, "video/") {
		if isSlackVoiceClip(f) {
			return "voice clip"
		}
		return "video"
	}
	return "document"
}

// One cached inbound file as path, media type and text injection, nil when skipped
type cachedFile struct {
	path, mediaType, injection string
}

// Downloads and caches one inbound file by kind, documents through the document branch
func (a *SlackAdapter) cacheSlackFile(ctx context.Context, kind string, f map[string]any, url, mimetype, teamID string) (*cachedFile, error) {
	switch kind {
	case "image":
		parts := strings.Split(mimetype, "/")
		ext := "." + strings.Split(parts[len(parts)-1], ";")[0]
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		default:
			ext = ".jpg"
		}
		path, err := a.downloadSlackFile(ctx, url, ext, false, teamID)
		if err != nil {
			return nil, err
		}
		return &cachedFile{path, mimetype, ""}, nil
	case "audio", "voice clip":
		ext := resolveSlackAudioExt(f, mimetype)
		path, err := a.downloadSlackFile(ctx, url, ext, true, teamID)
		if err != nil {
			return nil, err
		}
		if kind == "audio" {
			return &cachedFile{path, mimetype, ""}, nil
		}
		slog.Debug(fmt.Sprintf("[Slack] Cached voice clip (mislabeled %s) as audio: %s", mimetype, path))
		mediaType, ok := slackExtToAudioMime[ext]
		if !ok {
			mediaType = "audio/mp4"
		}
		return &cachedFile{path, mediaType, ""}, nil
	case "video":
		ext := strings.ToLower(filepath.Ext(pyrepr.Str(getOr(f, "name", ""))))
		if _, ok := mediacache.SupportedVideoTypes[ext]; !ok {
			byMime := gateway.VideoMimeToExt()
			if e, ok := byMime[pystr.Lower(strings.SplitN(mimetype, ";", 2)[0])]; ok {
				ext = e
			} else {
				ext = ".mp4"
			}
		}
		raw, err := a.downloadSlackFileBytes(ctx, url, teamID, "file bytes")
		if err != nil {
			return nil, err
		}
		path, err := gateway.CacheVideoFromBytesAsync(ctx, raw, ext)
		if err != nil {
			return nil, err
		}
		slog.Debug("[Slack] Cached user video: " + path)
		mediaType, ok := mediacache.SupportedVideoTypes[ext]
		if !ok {
			mediaType = mimetype
			if mediaType == "" {
				mediaType = "video/mp4"
			}
		}
		return &cachedFile{path, mediaType, ""}, nil
	}
	return a.cacheSlackDocument(ctx, f, url, mimetype, teamID)
}

var displayNameUnsafeRe = mustPyRe(`[^\w.\- ]`)

// Document branch caching any extension under the 20 MB cap and inlining small text files
func (a *SlackAdapter) cacheSlackDocument(ctx context.Context, f map[string]any, url, mimetype, teamID string) (*cachedFile, error) {
	originalFilename := pyrepr.Str(getOr(f, "name", ""))
	ext := ""
	if originalFilename != "" {
		ext = strings.ToLower(filepath.Ext(originalFilename))
	}
	if ext == "" && mimetype != "" {
		ext = gateway.DocumentMimeToExt()[mimetype]
	}
	fileSize := intValue(getOr(f, "size", int64(0)))
	if fileSize == 0 || fileSize > 20*1024*1024 {
		slog.Warn(fmt.Sprintf("[Slack] Document too large or unknown size: %s", pyrepr.Str(getOr(f, "size", int64(0)))))
		return nil, nil
	}
	raw, err := a.downloadSlackFileBytes(ctx, url, teamID, "file bytes")
	if err != nil {
		return nil, err
	}
	name := originalFilename
	if name == "" {
		name = "document" + orDefault(ext, ".bin")
	}
	path, err := gateway.CacheDocumentFromBytesAsync(ctx, raw, name)
	if err != nil {
		return nil, err
	}
	docMime, ok := mediacache.SupportedDocumentTypes[ext]
	if !ok {
		docMime = orDefault(mimetype, "application/octet-stream")
	}
	slog.Debug(fmt.Sprintf("[Slack] Cached user document: %s (%s)", path, docMime))
	injection := ""
	isText := gateway.TextInjectExtensions[ext] || strings.HasPrefix(mimetype, "text/")
	if isText && len(raw) <= 100*1024 {
		if content, err := pystr.DecodeUTF8(raw); err == nil {
			display := originalFilename
			if display == "" {
				display = "document" + orDefault(ext, ".txt")
			}
			display, _ = displayNameUnsafeRe.Replace(display, "_", -1, -1)
			injection = fmt.Sprintf("[Content of %s]:\n%s", display, content)
		}
	}
	return &cachedFile{path, docMime, injection}, nil
}

// Value or the default when empty
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Integer of a decoded JSON number, zero for anything else
func intValue(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case bool:
		if n {
			return 1
		}
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i
		}
	}
	return 0
}

// Downloads and caches the event files after the thread root media, failures prepended as a notice
func (a *SlackAdapter) collectInboundMedia(ctx context.Context, event map[string]any, channelID, teamID, text string, rootURLs, rootTypes []string) ([]string, []string, []bool, string) {
	mediaURLs := append([]string{}, rootURLs...)
	mediaTypes := append([]string{}, rootTypes...)
	inlined := make([]bool, len(mediaURLs))
	var notices []string
	for _, raw := range asList(getOr(event, "files", []any{})) {
		f := asMap(raw)
		if f["file_access"] == "check_file_info" {
			f = a.resolveCheckFileInfo(ctx, f, channelID, teamID, &notices)
			if f == nil {
				continue
			}
		}
		mimetype := pyrepr.Str(getOr(f, "mimetype", "unknown"))
		url := strOrEmpty(f["url_private_download"])
		if url == "" {
			url = pyrepr.Str(getOr(f, "url_private", ""))
		}
		if url == "" {
			continue
		}
		kind := slackFileKind(f, mimetype)
		cached, err := a.cacheSlackFile(ctx, kind, f, url, mimetype, teamID)
		if err != nil {
			noteAttachmentFailure(&notices, describeSlackDownloadFailure(err, f), fmt.Sprintf("[Slack] Failed to cache %s from %s: %v", kind, url, err))
			continue
		}
		if cached == nil {
			continue
		}
		mediaURLs = append(mediaURLs, cached.path)
		mediaTypes = append(mediaTypes, cached.mediaType)
		inlined = append(inlined, cached.injection != "")
		if cached.injection != "" {
			if text != "" {
				text = cached.injection + "\n\n" + text
			} else {
				text = cached.injection
			}
		}
	}
	if len(notices) > 0 {
		lines := make([]string, len(notices))
		for i, n := range notices {
			lines[i] = "- " + n
		}
		block := "[Slack attachment notice]\n" + strings.Join(lines, "\n")
		if text != "" {
			text = block + "\n\n" + text
		} else {
			text = block
		}
	}
	return mediaURLs, mediaTypes, inlined, text
}
