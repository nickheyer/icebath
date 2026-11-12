package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	maxMessageLength          = 39000
	userNameCacheMax          = 5000
	channelNameCacheMax       = 5000
	dmConversationCacheMax    = 5000
	processedMessageTSMax     = 5000
	botTSMax                  = 5000
	mentionedThreadsMax       = 5000
	assistantThreadsMax       = 5000
	agentViewContextsMax      = 5000
	threadRehydrationCheckMax = 5000
	reactingMessageIDsMax     = 5000
	titledAssistantThreadsMax = 5000
	channelTeamMax            = 10000
	approvalResolvedMax       = 1000
	clarifyResolvedMax        = 1000
	activeStatusThreadsMax    = 1000
	clarifyMessageMax         = 1000
	modelPickerStateMax       = 100
	statusMessageIDsMax       = 2000
	threadCacheMax            = 2500
	threadCacheTTL            = 60.0
	slashCtxTTL               = 120.0
	slashCtxMax               = 1000
	streamMaxAgeS             = 15 * 60
)

// Tracked assistant status of one thread
type statusEntry struct {
	threadTS string
	teamID   string
	started  float64
}

// Live native draft stream of one thread
type draftStream struct {
	ts      string
	draftID int
	sent    string
	started float64
	base    int
}

// Pending slash reply route captured when the command arrived
type slashContext struct {
	responseURL string
	userID      string
	ts          float64
}

// Slack adapter over Socket Mode with workspace scoped clients, caches and interactive state
type SlackAdapter struct {
	*gateway.BasePlatformAdapter
	mu sync.Mutex

	app            *slackApp
	handler        *socketModeHandler
	socketModeTask *socketTask
	botUserID      *string
	botDisplayName *string

	teamClients    *pydict.Dict[string, *webClient]
	teamBotUserIDs map[string]string
	teamBotNames   map[string]string

	userNameCache       *pydict.Dict[teamKey, string]
	channelNameCache    *pydict.Dict[teamKey, string]
	userIsBotCache      *pydict.Dict[teamKey, bool]
	channelTeam         *pydict.Dict[string, string]
	channelTeams        *pydict.Dict[string, map[string]bool]
	dmConversationCache *pydict.Dict[string, string]

	dedup               *gateway.MessageDeduplicator
	processedMessageTS  map[string]float64
	botMessageTS        map[any]bool
	mentionedThreads    map[any]bool
	mentionedThreadsCap int
	processedTSCap      int

	assistantThreads      *pydict.Dict[threadKey, map[string]string]
	agentViewContexts     *pydict.Dict[teamKey, map[string]string]
	statusMessageIDs      *pydict.Dict[threadKey, string]
	reactingMessageIDs    map[any]bool
	activeStatusThreads   *pydict.Dict[threadKey, *statusEntry]
	nativeTaskCardStreams map[threadKey]*nativeTaskCardStream
	slashCommandContexts  *pydict.Dict[[3]string, *slashContext]
	slashLanes            *SlashHandoffLanes
	activeStreams         *pydict.Dict[threadKey, *draftStream]

	nativeStreamUnsupported bool
	appToken                string
	proxyURL                string
	socketWatchdogTask      *socketTask
	socketReconnectMu       sync.Mutex
	socketHandlerStarted    float64
	socketWatchdogInterval  time.Duration
	socketPingStaleFactor   float64
	socketFirstPingGrace    float64
	groupDMScopeWarned      map[string]bool
	userTokenWarned         map[string]bool
	apiHumanUsersCache      map[string]bool
	pluginActionsApp        *slackApp
	pluginActionsWired      map[[2]string]bool

	approvalResolved         *pydict.Dict[any, bool]
	clarifyResolved          *pydict.Dict[any, bool]
	clarifyMessages          *pydict.Dict[string, clarifyTarget]
	modelPickerState         *pydict.Dict[any, *modelPickerState]
	threadContextCache       *pydict.Dict[string, *threadContextCache]
	threadRehydrationChecked map[string]bool
	titledAssistantThreads   map[threadKey]bool
	compiledMentionPatterns  []*regexp2.Regexp
	mentionPatternsLoaded    bool
}

// Wall clock seconds since the epoch, the time.time counterpart
func wallTime() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Fresh adapter for the platform config with empty caches and the dedup window from config
func NewSlackAdapter(ctx context.Context, cfg *gateway.PlatformConfig) *SlackAdapter {
	a := &SlackAdapter{
		teamClients:            pydict.New[string, *webClient](),
		teamBotUserIDs:         map[string]string{},
		teamBotNames:           map[string]string{},
		userNameCache:          pydict.New[teamKey, string](),
		channelNameCache:       pydict.New[teamKey, string](),
		userIsBotCache:         pydict.New[teamKey, bool](),
		channelTeam:            pydict.New[string, string](),
		channelTeams:           pydict.New[string, map[string]bool](),
		dmConversationCache:    pydict.New[string, string](),
		dedup:                  gateway.NewMessageDeduplicator(2000, slackDedupTTLSeconds(ctx)),
		processedMessageTS:     map[string]float64{},
		botMessageTS:           map[any]bool{},
		mentionedThreads:       map[any]bool{},
		mentionedThreadsCap:    mentionedThreadsMax,
		processedTSCap:         processedMessageTSMax,
		assistantThreads:       pydict.New[threadKey, map[string]string](),
		agentViewContexts:      pydict.New[teamKey, map[string]string](),
		statusMessageIDs:       pydict.New[threadKey, string](),
		reactingMessageIDs:     map[any]bool{},
		activeStatusThreads:    pydict.New[threadKey, *statusEntry](),
		nativeTaskCardStreams:  map[threadKey]*nativeTaskCardStream{},
		slashCommandContexts:   pydict.New[[3]string, *slashContext](),
		slashLanes:             NewSlashHandoffLanes(),
		activeStreams:          pydict.New[threadKey, *draftStream](),
		socketWatchdogInterval: 15 * time.Second,
		socketPingStaleFactor:  4,
		socketFirstPingGrace:   60,
		groupDMScopeWarned:     map[string]bool{},
		userTokenWarned:        map[string]bool{},

		approvalResolved:         pydict.New[any, bool](),
		clarifyResolved:          pydict.New[any, bool](),
		clarifyMessages:          pydict.New[string, clarifyTarget](),
		modelPickerState:         pydict.New[any, *modelPickerState](),
		threadContextCache:       pydict.New[string, *threadContextCache](),
		threadRehydrationChecked: map[string]bool{},
		titledAssistantThreads:   map[threadKey]bool{},
	}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformSlack, a)
	a.Traits.MaxMessageLength = maxMessageLength
	a.Traits.SupportsCodeBlocks = true
	a.Traits.SupportsStatusText = true
	a.Traits.SplitsLongMessages = true
	a.Traits.TypedCommandPrefix = "!"
	a.Traits.SupportsInchannelContinuable = true
	a.Traits.EACodeOpen = "```"
	a.Traits.EACodeClose = "```\n"
	a.Traits.EAReasonBudget = 500
	return a
}

// Config extra map, empty when unset
func (a *SlackAdapter) extra() map[string]any {
	if a.Config == nil || a.Config.Extra == nil {
		return map[string]any{}
	}
	return a.Config.Extra
}

// Whether connect has completed and disconnect has not run
func (a *SlackAdapter) running() bool { return a.IsConnected() }

// Primary app, nil when not connected
func (a *SlackAdapter) currentApp() *slackApp {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.app
}

// Releases idle connections of every distinct workspace client
func (a *SlackAdapter) closeWorkspaceClients() {
	a.mu.Lock()
	var clients []*webClient
	if a.app != nil && a.app.client != nil {
		clients = append(clients, a.app.client)
	}
	clients = append(clients, a.teamClients.Values()...)
	a.mu.Unlock()
	seen := map[*webClient]bool{}
	for _, c := range clients {
		if c == nil || seen[c] {
			continue
		}
		seen[c] = true
		c.http.CloseIdleConnections()
	}
}

// Bot message markers trimmed oldest first down to half the cap once over it
func (a *SlackAdapter) trimBotMessageTimestamps() {
	if len(a.botMessageTS) > botTSMax {
		discardOldestFromSet(a.botMessageTS, len(a.botMessageTS)-botTSMax/2, identityTS[any])
	}
}

// Mentioned thread keys trimmed oldest thread first once over the cap
func (a *SlackAdapter) trimMentionedThreads() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.mentionedThreads) > a.mentionedThreadsCap {
		discardOldestFromSet(a.mentionedThreads, a.mentionedThreadsCap/2, identityTS[any])
	}
}

// Records the workspace owning a channel, dropping the unqualified fallback once ambiguous
func (a *SlackAdapter) rememberChannelTeam(channelID, teamID string) {
	if channelID == "" || teamID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	teams, ok := a.channelTeams.Get(channelID)
	if !ok {
		teams = map[string]bool{}
		a.channelTeams.Set(channelID, teams)
	}
	teams[teamID] = true
	if len(teams) == 1 {
		a.channelTeam.Set(channelID, teamID)
	} else {
		a.channelTeam.Delete(channelID)
	}
	trimOldestDictEntries(a.channelTeam, channelTeamMax)
	trimOldestDictEntries(a.channelTeams, channelTeamMax)
}

type slashUserIDKey struct{}

// Context carrying the slash invoker so send picks that user's stashed response_url
func withSlashUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, slashUserIDKey{}, userID)
}

// Slash invoker carried by the context, empty outside a slash command
func slashUserID(ctx context.Context) string {
	uid, _ := ctx.Value(slashUserIDKey{}).(string)
	return uid
}

// Fresh slash context for the chat matched on team, channel and the invoking user
func (a *SlackAdapter) popSlashContext(ctx context.Context, chatID, teamID string) *slashContext {
	a.purgeStaleSlashContexts()
	uid := slashUserID(ctx)
	if uid == "" {
		return nil
	}
	key := [3]string{teamID, chatID, uid}
	a.mu.Lock()
	defer a.mu.Unlock()
	sc, ok := a.slashCommandContexts.Get(key)
	if !ok {
		return nil
	}
	a.slashCommandContexts.Delete(key)
	return sc
}

// Drops slash contexts older than the slash TTL
func (a *SlackAdapter) purgeStaleSlashContexts() {
	now := monotonic()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, k := range a.slashCommandContexts.Keys() {
		if v, _ := a.slashCommandContexts.Get(k); now-v.ts > slashCtxTTL {
			a.slashCommandContexts.Delete(k)
		}
	}
}

// Content in mrkdwn split to the message limit, never empty
func (a *SlackAdapter) formatChunks(content string) []string {
	formatted := a.FormatMessage(content)
	chunks := gateway.TruncateMessage(formatted, maxMessageLength, a.MessageLenFn())
	if len(chunks) == 0 {
		return []string{formatted}
	}
	return chunks
}

// Replaces the slash ack through response_url, at most five posts with a truncation notice
func (a *SlackAdapter) sendSlashEphemeral(ctx context.Context, sc *slashContext, content string) gateway.SendResult {
	chunks := a.formatChunks(content)
	if len(chunks) > 5 {
		dropped := len(chunks) - 5
		chunks = chunks[:5]
		chunks[4] = pystr.Rstrip(chunks[4]) + i18n.T(ctx, "platform.slack.slash.reply_truncated", "", map[string]any{"count": strconv.Itoa(dropped)})
	}
	client := trustEnvHTTPClient(ctx, 10*time.Second)
	for idx, chunk := range chunks {
		payload, _ := json.Marshal(map[string]any{"response_type": "ephemeral", "replace_original": idx == 0, "text": chunk})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, sc.responseURL, bytes.NewReader(payload))
		if err != nil {
			slog.Warn(fmt.Sprintf("[Slack] response_url POST failed: %v", err))
			return gateway.SendResult{Success: false, Error: err.Error()}
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn(fmt.Sprintf("[Slack] response_url POST failed: %v", err))
			return gateway.SendResult{Success: false, Error: err.Error()}
		}
		if resp.StatusCode != http.StatusOK {
			body := readErrorTextLimited(resp.Body, slackErrorBodyLimitBytes)
			resp.Body.Close()
			slog.Warn(fmt.Sprintf("[Slack] response_url POST returned %d: %s", resp.StatusCode, pystr.Slice(body, 0, 200)))
			return gateway.SendResult{Success: false, Error: fmt.Sprintf("response_url POST returned %d", resp.StatusCode)}
		}
		resp.Body.Close()
	}
	return gateway.SendResult{Success: true}
}

// Delivers a slash reply privately through chat.postEphemeral when response_url failed
func (a *SlackAdapter) postEphemeralFallback(ctx context.Context, chatID string, sc *slashContext, content string) gateway.SendResult {
	if sc.userID == "" {
		return gateway.SendResult{Success: false, Error: "no user_id in slash context for postEphemeral"}
	}
	client := a.getClient(chatID, "")
	for _, chunk := range a.formatChunks(content) {
		if _, err := client.call(ctx, "chat.postEphemeral", map[string]any{"channel": chatID, "user": sc.userID, "text": chunk}); err != nil {
			var apiErr *slackAPIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusOK {
				errText := "unexpected_response"
				if len(apiErr.Response) > 0 {
					errText = pyrepr.Str(getOr(apiErr.Response, "error", "unknown_error"))
				}
				return gateway.SendResult{Success: false, Error: "chat.postEphemeral failed: " + errText}
			}
			return gateway.SendResult{Success: false, Error: err.Error()}
		}
	}
	return gateway.SendResult{Success: true}
}

// Warns once per workspace when the im history scope is granted without the mpim history scope
func (a *SlackAdapter) warnIfMissingGroupDMScopes(headers http.Header, teamName string) {
	raw := headers.Get("X-OAuth-Scopes")
	if raw == "" {
		return
	}
	granted := map[string]bool{}
	for _, s := range strings.Split(raw, ",") {
		if s = pystr.Strip(s); s != "" {
			granted[s] = true
		}
	}
	a.mu.Lock()
	warned := a.groupDMScopeWarned[teamName]
	if !warned && granted["im:history"] && !granted["mpim:history"] {
		a.groupDMScopeWarned[teamName] = true
	}
	a.mu.Unlock()
	if !warned && granted["im:history"] && !granted["mpim:history"] {
		team := teamName
		if team == "" {
			team = "this workspace"
		}
		slog.Warn(fmt.Sprintf("[Slack] Group DMs (multi-person DMs) will not work in workspace %s: the app "+
			"is missing the 'mpim:history' scope and 'message.mpim' event. Add "+
			"'mpim:history' (and 'mpim:read') to bot scopes, add 'message.mpim' to event "+
			"subscriptions, then REINSTALL the app to the workspace. Regenerating the app "+
			"from `icebath slack` produces a manifest with these already included.", team))
	}
}

// Warns once per workspace when auth.test resolved a member without a bot_id
func (a *SlackAdapter) warnIfNotBotToken(auth map[string]any, teamName string) {
	a.mu.Lock()
	if a.userTokenWarned[teamName] {
		a.mu.Unlock()
		return
	}
	botID := strOrEmpty(auth["bot_id"])
	userID := strOrEmpty(auth["user_id"])
	if userID == "" || botID != "" {
		a.mu.Unlock()
		return
	}
	a.userTokenWarned[teamName] = true
	a.mu.Unlock()
	team := teamName
	if team == "" {
		team = "this workspace"
	}
	slog.Warn(fmt.Sprintf("[Slack] The configured Slack token for workspace %s authenticated as a USER "+
		"(member %s), not a bot — the auth.test response has no 'bot_id'. This is "+
		"almost certainly a user token (xoxp-...) instead of a Bot User OAuth Token "+
		"(xoxb-...). The bot's identity is now bound to that member's ID, so mentions "+
		"OF THAT PERSON will be misrouted as mentions of the bot (the bot replies to "+
		"messages merely addressed to them). Use the 'Bot User OAuth Token' "+
		"(xoxb-...) from your Slack app's 'OAuth & Permissions' page in "+
		"SLACK_BOT_TOKEN.", team, userID))
}

// Workspace owning the chat, empty for unknown or multiply claimed channels
func (a *SlackAdapter) ScopeIDForChat(chatID string) string {
	if chatID == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	teamID, _ := a.channelTeam.Get(chatID)
	if teamID == "" && !a.channelTeams.Has(chatID) && a.teamClients.Len() == 1 {
		teamID = a.teamClients.Keys()[0]
	}
	return teamID
}

// Workspace client for a channel, the explicit team first, then the learned owner, then primary
func (a *SlackAdapter) getClient(chatID, teamID string) *webClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	if teamID != "" {
		if c, ok := a.teamClients.Get(teamID); ok {
			return c
		}
	}
	if owner, ok := a.channelTeam.Get(chatID); ok && owner != "" {
		if c, ok := a.teamClients.Get(owner); ok {
			return c
		}
	}
	if a.app == nil {
		return nil
	}
	return a.app.client
}

// Client for the chat scoped by outbound metadata
func (a *SlackAdapter) clientFor(chatID string, metadata map[string]any) *webClient {
	return a.getClient(chatID, metadataTeamID(metadata))
}

// DM conversation of a user target scoped by outbound metadata
func (a *SlackAdapter) dmTarget(ctx context.Context, chatID string, metadata map[string]any) string {
	return a.ensureDMConversation(ctx, chatID, metadataTeamID(metadata))
}

// Bare user id resolved to a DM conversation id through conversations.open, cached per team and user
func (a *SlackAdapter) ensureDMConversation(ctx context.Context, chatID, teamID string) string {
	if chatID == "" || (chatID[0] != 'U' && chatID[0] != 'W') {
		return chatID
	}
	cacheKey := teamID + ":" + chatID
	a.mu.Lock()
	cached, _ := a.dmConversationCache.Get(cacheKey)
	a.mu.Unlock()
	if cached != "" {
		return cached
	}
	client := a.getClient(chatID, teamID)
	if client == nil {
		return chatID
	}
	result, err := client.call(ctx, "conversations.open", map[string]any{"users": chatID})
	if err != nil {
		slog.Warn(fmt.Sprintf("[Slack] conversations.open failed for user target %s: %v (check the bot's im:write scope)", chatID, err))
		return chatID
	}
	dmID := strOrEmpty(asMap(result.get("channel"))["id"])
	if dmID == "" {
		return chatID
	}
	a.mu.Lock()
	a.dmConversationCache.Set(cacheKey, dmID)
	trimOldestDictEntries(a.dmConversationCache, dmConversationCacheMax)
	a.mu.Unlock()
	if teamID != "" {
		a.rememberChannelTeam(dmID, teamID)
	}
	return dmID
}

// Status clear for send paths that skip the regular clear, its failure only debug logged
func (a *SlackAdapter) clearThreadStatusQuietly(ctx context.Context, chatID string, metadata map[string]any) {
	if err := a.StopTyping(ctx, chatID, metadata); err != nil {
		slog.Debug(fmt.Sprintf("[Slack] status cleanup failed: %v", err))
	}
}

// Whether the generic gateway must stay silent in the channel, thread suffixes stripped
func (a *SlackAdapter) IsIgnoredChannel(ctx context.Context, channelID string) bool {
	if channelID == "" {
		return false
	}
	ignored := a.slackIgnoredChannels(ctx)
	return ignored["*"] || ignored[strings.SplitN(channelID, ":", 2)[0]]
}

// Whether Slack native tool progress cards are explicitly enabled
func (a *SlackAdapter) NativeTaskCardsEnabled() (bool, error) {
	extra := a.extra()
	var progress map[string]any
	if streaming, ok := extra["streaming"].(map[string]any); ok {
		progress, _ = streaming["progress"].(map[string]any)
	}
	if progress == nil {
		progress = map[string]any{}
	}
	for _, scope := range []map[string]any{extra, progress} {
		value, ok := scope["native_task_cards"]
		if !ok {
			value = scope["nativeTaskCards"]
		}
		if value != nil {
			return truthyConfig(value), nil
		}
	}
	return false, nil
}

// Thread key of a native task card stream, nil when no thread target resolves
func (a *SlackAdapter) nativeTaskCardKey(chatID, replyTo string, metadata map[string]any) *threadKey {
	threadTS := a.resolveThreadTS(replyTo, metadata)
	if threadTS == "" {
		return nil
	}
	return workspaceThreadKey(metadataTeamID(metadata), chatID, threadTS)
}

// Placement eligibility of a native task card independent of connection state
func (a *SlackAdapter) NativeTaskCardDestinationSupported(chatID string, replyTo *string, metadata map[string]any) *bool {
	ok := a.nativeTaskCardKey(chatID, derefString(replyTo), metadata) != nil
	return &ok
}

// Value of an optional string, empty for nil
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Starts or updates a Slack native plan progress stream, reopening a server sealed card once
func (a *SlackAdapter) SendNativeTaskCardProgress(ctx context.Context, chatID string, tasks []map[string]string, title string, replyTo *string, metadata map[string]any, fallbackText string) (gateway.SendResult, error) {
	if title == "" {
		title = i18n.T(ctx, "platform.slack.task_card.title", "", nil)
	}
	if a.currentApp() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if len(tasks) == 0 {
		return gateway.SendResult{Success: false, Error: "No tasks"}, nil
	}
	key := a.nativeTaskCardKey(chatID, derefString(replyTo), metadata)
	if key == nil {
		return gateway.SendResult{Success: false, Error: "No Slack thread target"}, nil
	}
	a.mu.Lock()
	stream := a.nativeTaskCardStreams[*key]
	if stream == nil || stream.isStopped() {
		stream = newNativeTaskCardStream(key[0], chatID, key[2])
		a.nativeTaskCardStreams[*key] = stream
	}
	a.mu.Unlock()
	stream.lock <- struct{}{}
	defer func() { <-stream.lock }()
	if stream.stopped {
		return gateway.SendResult{Success: false, Error: "Progress stream already stopped"}, nil
	}
	client := a.getClient(chatID, stream.teamID)
	result, err := func() (gateway.SendResult, error) {
		if stream.streamTS == "" {
			if err := startNativeTaskCardStream(ctx, client, stream, metadata); err != nil {
				return gateway.SendResult{}, err
			}
		}
		chunks := []any{map[string]any{"type": "plan_update", "title": pystr.Slice(title, 0, 256)}}
		for _, task := range tasks {
			chunks = append(chunks, taskUpdateChunk(task))
		}
		_, err := client.call(ctx, "chat.appendStream", map[string]any{"channel": chatID, "ts": stream.streamTS, "chunks": chunks})
		if err != nil {
			if !slackErrorIs(err, "message_not_in_streaming_state") {
				return gateway.SendResult{}, err
			}
			slog.Info(fmt.Sprintf("[Slack] Native task-card stream %s expired (message_not_in_streaming_state); reopening a fresh card in thread %s", stream.streamTS, stream.threadTS))
			stream.streamTS = ""
			if err := startNativeTaskCardStream(ctx, client, stream, metadata); err != nil {
				return gateway.SendResult{}, err
			}
			if _, err := client.call(ctx, "chat.appendStream", map[string]any{"channel": chatID, "ts": stream.streamTS, "chunks": chunks}); err != nil {
				return gateway.SendResult{}, err
			}
		}
		return gateway.SendResult{Success: true, MessageID: stream.streamTS}, nil
	}()
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] Native task-card progress error: %v", err))
		return gateway.SendResult{Success: false, Error: err.Error(), Retryable: true}, nil
	}
	return result, nil
}

// Serialized state of one workspace scoped Slack progress stream guarded by its one slot lock
type nativeTaskCardStream struct {
	teamID   string
	channel  string
	threadTS string
	streamTS string
	stopped  bool
	lock     chan struct{}
}

// Stream state with its lock ready
func newNativeTaskCardStream(teamID, channel, threadTS string) *nativeTaskCardStream {
	return &nativeTaskCardStream{teamID: teamID, channel: channel, threadTS: threadTS, lock: make(chan struct{}, 1)}
}

// Whether the stream is stopped, read under its lock
func (s *nativeTaskCardStream) isStopped() bool {
	select {
	case s.lock <- struct{}{}:
		stopped := s.stopped
		<-s.lock
		return stopped
	default:
		return false
	}
}

// Opens a plan mode card in the stream's thread and records its ts
func startNativeTaskCardStream(ctx context.Context, client *webClient, stream *nativeTaskCardStream, metadata map[string]any) error {
	payload := map[string]any{"channel": stream.channel, "thread_ts": stream.threadTS, "task_display_mode": "plan"}
	md := metadata
	if md == nil {
		md = map[string]any{}
	}
	if v := firstTruthy(md, "recipient_team_id", "team_id", "slack_team_id"); v != nil {
		payload["recipient_team_id"] = v
	}
	if v := firstTruthy(md, "recipient_user_id", "user_id"); v != nil {
		payload["recipient_user_id"] = v
	}
	result, err := client.call(ctx, "chat.startStream", payload)
	if err != nil {
		return err
	}
	stream.streamTS = strOrEmpty(firstTruthy(result.Data, "ts", "message_ts"))
	if stream.streamTS == "" {
		return errors.New("Slack startStream returned no stream timestamp")
	}
	return nil
}

// Finalizes a progress stream exactly once and forgets it
func (a *SlackAdapter) stopNativeTaskCardStream(ctx context.Context, key threadKey, stream *nativeTaskCardStream) {
	stream.lock <- struct{}{}
	defer func() { <-stream.lock }()
	if stream.stopped {
		return
	}
	stream.stopped = true
	if a.currentApp() != nil && stream.streamTS != "" {
		if _, err := a.getClient(stream.channel, stream.teamID).call(ctx, "chat.stopStream", map[string]any{"channel": stream.channel, "ts": stream.streamTS}); err != nil {
			slog.Debug(fmt.Sprintf("[Slack] Native task-card stopStream failed: %v", err))
		}
	}
	a.mu.Lock()
	if a.nativeTaskCardStreams[key] == stream {
		delete(a.nativeTaskCardStreams, key)
	}
	a.mu.Unlock()
}

// Finalizes the active native progress stream of the thread exactly once
func (a *SlackAdapter) StopNativeTaskCardProgress(ctx context.Context, chatID string, replyTo *string, metadata map[string]any) (gateway.SendResult, error) {
	key := a.nativeTaskCardKey(chatID, derefString(replyTo), metadata)
	if key == nil {
		return gateway.SendResult{}, nil
	}
	a.mu.Lock()
	stream := a.nativeTaskCardStreams[*key]
	a.mu.Unlock()
	if stream != nil {
		a.stopNativeTaskCardStream(ctx, *key, stream)
	}
	return gateway.SendResult{}, nil
}

// Whether the chat is an ignored channel, logged at the given level when it is
func (a *SlackAdapter) suppressedIgnored(ctx context.Context, chatID, what string, level slog.Level) bool {
	if a.IsIgnoredChannel(ctx, chatID) {
		slog.Log(ctx, level, fmt.Sprintf("[Slack] Suppressed %s configured ignored channel %s", what, chatID))
		return true
	}
	return false
}

// Failed result when the chat is ignored or the app is not connected, nil otherwise
func (a *SlackAdapter) outboundBlocked(ctx context.Context, chatID, what string) *gateway.SendResult {
	if a.suppressedIgnored(ctx, chatID, what, slog.LevelWarn) {
		return &gateway.SendResult{Success: false, Error: "ignored_channel"}
	}
	if a.currentApp() == nil {
		return &gateway.SendResult{Success: false, Error: "Not connected"}
	}
	return nil
}

// Web API call retried once without blocks when Slack rejects the Block Kit payload
func (a *SlackAdapter) callWithBlockFallback(ctx context.Context, clientFn func() *webClient, method string, kwargs map[string]any, verb string) (*apiResult, error) {
	result, err := clientFn().call(ctx, method, kwargs)
	if err == nil {
		return result, nil
	}
	if !pyrepr.Truthy(kwargs["blocks"]) || !isBlockPayloadRejection(err) {
		return nil, err
	}
	retry := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		retry[k] = v
	}
	if verb == "edit" {
		retry["blocks"] = []any{}
	} else {
		delete(retry, "blocks")
	}
	slog.Info(fmt.Sprintf("[Slack] Block Kit payload rejected; retrying %s without blocks: %v", verb, err))
	return clientFn().call(ctx, method, retry)
}

// Sends a message to a channel or DM, routing slash replies and sealing a finished native stream
func (a *SlackAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if blocked := a.outboundBlocked(ctx, chatID, "outbound generic send to"); blocked != nil {
		return *blocked, nil
	}
	chatID = a.dmTarget(ctx, chatID, metadata)
	teamID := metadataTeamID(metadata)
	if sc := a.popSlashContext(ctx, chatID, teamID); sc != nil {
		return a.sendSlashReply(ctx, chatID, sc, content, metadata), nil
	}
	if streamResult := a.tryFinalizeStream(ctx, chatID, content, metadata); streamResult != nil {
		return *streamResult, nil
	}
	formatted := a.FormatMessage(content)
	if pystr.Strip(formatted) == "" {
		a.clearThreadStatusQuietly(ctx, chatID, metadata)
		return gateway.SendResult{Success: true}, nil
	}
	threadTS := a.resolveThreadTS(replyTo, metadata)
	lastResult, err := a.postChunks(ctx, chatID, teamID, content, formatted, threadTS)
	if err != nil {
		a.clearThreadStatusQuietly(ctx, chatID, metadata)
		slog.Error(fmt.Sprintf("[Slack] Send error: %v", err))
		retryable := a.isRetryableUploadError(err)
		res := gateway.SendResult{Success: false, Error: err.Error(), Retryable: retryable}
		if retryable {
			res.RetryAfter = retryAfterFromErr(err)
		}
		return res, nil
	}
	if threadTS != "" {
		_ = a.StopTyping(ctx, chatID, metadata)
	}
	sentTS := strOrEmpty(lastResult.get("ts"))
	if sentTS != "" {
		a.mu.Lock()
		a.botMessageTS[workspaceMessageMarker(teamID, sentTS)] = true
		if threadTS != "" {
			a.botMessageTS[workspaceMessageMarker(teamID, threadTS)] = true
		}
		a.trimBotMessageTimestamps()
		a.mu.Unlock()
		return gateway.SendResult{Success: true, MessageID: sentTS, RawResponse: lastResult.Data}, nil
	}
	var raw any
	if lastResult != nil {
		raw = lastResult.Data
	}
	return gateway.SendResult{Success: true, RawResponse: raw}, nil
}

// Posts each message length chunk with Block Kit on a single chunk and broadcast on the first
func (a *SlackAdapter) postChunks(ctx context.Context, chatID, teamID, content, formatted, threadTS string) (*apiResult, error) {
	chunks := gateway.TruncateMessage(formatted, maxMessageLength, a.MessageLenFn())
	broadcast := pyrepr.Truthy(a.extra()["reply_broadcast"])
	var blocks []any
	if len(chunks) == 1 {
		blocks = a.maybeBlocks(content)
	}
	var last *apiResult
	for i, chunk := range chunks {
		kwargs := map[string]any{"channel": chatID, "text": chunk, "mrkdwn": true}
		for k, v := range slackUnfurlKwargs(a.extra()) {
			kwargs[k] = v
		}
		if len(blocks) > 0 && i == 0 {
			kwargs["blocks"] = blocks
		}
		if threadTS != "" {
			kwargs["thread_ts"] = threadTS
			if broadcast && i == 0 {
				kwargs["reply_broadcast"] = true
			}
		}
		result, err := a.callWithBlockFallback(ctx, func() *webClient { return a.getClient(chatID, teamID) }, "chat.postMessage", kwargs, "send")
		if err != nil {
			return nil, err
		}
		last = result
	}
	return last, nil
}

// Ephemeral slash reply through response_url then chat.postEphemeral, never a public post
func (a *SlackAdapter) sendSlashReply(ctx context.Context, chatID string, sc *slashContext, content string, metadata map[string]any) gateway.SendResult {
	ephemeral := a.sendSlashEphemeral(ctx, sc, content)
	if ephemeral.Success {
		a.clearThreadStatusQuietly(ctx, chatID, metadata)
		return ephemeral
	}
	slog.Warn(fmt.Sprintf("[Slack] response_url slash reply failed (%s); retrying via chat.postEphemeral", pyrepr.Str(ephemeral.Error)))
	fallback := a.postEphemeralFallback(ctx, chatID, sc, content)
	if fallback.Success {
		a.clearThreadStatusQuietly(ctx, chatID, metadata)
		return fallback
	}
	slog.Error(fmt.Sprintf("[Slack] Ephemeral slash reply failed on both response_url and chat.postEphemeral (%s); dropping rather than posting publicly", pyrepr.Str(fallback.Error)))
	return fallback
}

// Sends an ephemeral message visible only to one user
func (a *SlackAdapter) SendPrivateNotice(ctx context.Context, chatID string, userID *string, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if blocked := a.outboundBlocked(ctx, chatID, "outbound generic ephemeral notice to"); blocked != nil {
		return *blocked, nil
	}
	if chatID == "" || derefString(userID) == "" {
		return gateway.SendResult{Success: false, Error: "chat_id and user_id are required"}, nil
	}
	formatted := a.FormatMessage(content)
	threadTS := a.resolveThreadTS(replyTo, metadata)
	kwargs := map[string]any{"channel": chatID, "user": *userID, "text": formatted, "mrkdwn": true}
	if threadTS != "" {
		kwargs["thread_ts"] = threadTS
	}
	result, err := a.clientFor(chatID, metadata).call(ctx, "chat.postEphemeral", kwargs)
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] Ephemeral send error: %v", err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return gateway.SendResult{Success: true, MessageID: firstTruthy(result.Data, "message_ts", "ts"), RawResponse: result.Data}, nil
}

// Sends a status message or edits the earlier one with the same channel, thread and key
func (a *SlackAdapter) SendOrUpdateStatus(ctx context.Context, chatID, statusKey, content string, metadata map[string]any) (gateway.SendResult, error) {
	key := threadKey{chatID, a.resolveThreadTS("", metadata), statusKey}
	a.mu.Lock()
	cachedID, cached := a.statusMessageIDs.Get(key)
	a.mu.Unlock()
	if cached {
		result := a.editMessage(ctx, chatID, cachedID, content, false, metadata)
		if result.Success {
			a.mu.Lock()
			if current, _ := a.statusMessageIDs.Get(key); pyrepr.Truthy(result.MessageID) && current == cachedID {
				a.statusMessageIDs.Set(key, pyrepr.Str(result.MessageID))
			}
			a.mu.Unlock()
			return result, nil
		}
		a.mu.Lock()
		a.statusMessageIDs.Delete(key)
		a.mu.Unlock()
	}
	result, err := a.Send(ctx, chatID, content, "", metadata)
	if err != nil {
		return result, err
	}
	if result.Success && pyrepr.Truthy(result.MessageID) {
		a.mu.Lock()
		if a.statusMessageIDs.Len() >= statusMessageIDsMax {
			for _, stale := range a.statusMessageIDs.Keys()[:statusMessageIDsMax/2] {
				a.statusMessageIDs.Delete(stale)
			}
		}
		a.statusMessageIDs.Set(key, pyrepr.Str(result.MessageID))
		a.mu.Unlock()
	}
	return result, nil
}

// Edits a previously sent message without outbound metadata
func (a *SlackAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	return a.editMessage(ctx, chatID, messageID, content, finalize, nil), nil
}

// Edits a sent message, Block Kit only on the final edit and transport failures kept retryable
func (a *SlackAdapter) editMessage(ctx context.Context, chatID, messageID, content string, finalize bool, metadata map[string]any) gateway.SendResult {
	if blocked := a.outboundBlocked(ctx, chatID, "message edit in"); blocked != nil {
		return *blocked
	}
	formatted := a.FormatMessage(content)
	if chunks := gateway.TruncateMessage(formatted, maxMessageLength, a.MessageLenFn()); len(chunks) > 0 {
		formatted = chunks[0]
	}
	kwargs := map[string]any{"channel": chatID, "ts": messageID, "text": formatted}
	if finalize {
		if blocks := a.maybeBlocks(content); len(blocks) > 0 {
			kwargs["blocks"] = blocks
		}
	}
	_, err := a.callWithBlockFallback(ctx, func() *webClient { return a.clientFor(chatID, metadata) }, "chat.update", kwargs, "edit")
	if finalize {
		a.clearThreadStatusQuietly(ctx, chatID, metadata)
	}
	if err == nil {
		return gateway.SendResult{Success: true, MessageID: messageID}
	}
	if isTransientTransportError(err) {
		slog.Error(fmt.Sprintf("[Slack] transient chat.update failure on message %s in channel %s: %v", messageID, chatID, err))
		kind := "transient"
		return gateway.SendResult{Success: false, Error: err.Error(), Retryable: true, ErrorKind: &kind}
	}
	apiError := slackErrorCode(err)
	if apiError == "" {
		apiError = "none"
	}
	slog.Error(fmt.Sprintf("[Slack] Failed to edit message %s in channel %s: api_error=%s: %v", messageID, chatID, apiError, err))
	return gateway.SendResult{Success: false, Error: err.Error()}
}

// Deletes a bot message, false on any failure
func (a *SlackAdapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	if a.currentApp() == nil {
		return false, nil
	}
	if _, err := a.getClient(chatID, "").call(ctx, "chat.delete", map[string]any{"channel": chatID, "ts": messageID}); err != nil {
		var apiErr *slackAPIError
		if errors.As(err, &apiErr) && apiErr.Response["ok"] == false && apiErr.StatusCode == http.StatusOK {
			slog.Debug(fmt.Sprintf("[Slack] chat.delete returned ok=false for message %s in channel %s: %s", messageID, chatID, pyrepr.Str(getOr(apiErr.Response, "error", "unknown"))))
			return false, nil
		}
		slog.Debug(fmt.Sprintf("[Slack] Failed to delete message %s in channel %s: %v", messageID, chatID, err))
		return false, nil
	}
	return true, nil
}

var nativeStreamUnsupportedMarkers = []string{
	"not_allowed", "missing_scope", "feature_not_enabled", "invalid_method", "unknown_method",
	"method_deprecated", "not_authed", "streaming_not_allowed",
}

// Whether the native stream can preserve configured behaviour, unfurl controls needing edits
func (a *SlackAdapter) SupportsDraftStreaming(chatType string, metadata map[string]any, chatID string) bool {
	a.mu.Lock()
	unsupported := a.nativeStreamUnsupported
	a.mu.Unlock()
	if unsupported || len(slackUnfurlKwargs(a.extra())) > 0 {
		return false
	}
	return a.currentApp() != nil
}

// Streams a frame through chat.startStream and chat.appendStream, reopening a sealed stream once
func (a *SlackAdapter) SendDraft(ctx context.Context, chatID string, draftID int, content string, metadata map[string]any) (gateway.SendResult, error) {
	if a.currentApp() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	a.mu.Lock()
	unsupported := a.nativeStreamUnsupported
	a.mu.Unlock()
	if unsupported {
		return gateway.SendResult{Success: false, Error: "native streaming unsupported"}, nil
	}
	text := stripStreamCursor(content)
	key := a.streamKey(chatID, metadata)
	if key == nil {
		return gateway.SendResult{Success: false, Error: "no thread_ts for native stream"}, nil
	}
	client := a.clientFor(chatID, metadata)
	a.mu.Lock()
	stream, _ := a.activeStreams.Get(*key)
	a.mu.Unlock()
	result, err := func() (gateway.SendResult, error) {
		if stream != nil && stream.draftID != draftID {
			a.sealStream(ctx, *key, stream, "")
			a.dropActiveStream(*key)
			stream = nil
		}
		if stream == nil {
			return a.startStream(ctx, client, *key, draftID, text, metadata, 0)
		}
		sent := stream.sent
		if text == sent {
			return gateway.SendResult{Success: true, MessageID: stream.ts}, nil
		}
		if !strings.HasPrefix(text, sent) {
			a.sealStream(ctx, *key, stream, "")
			a.dropActiveStream(*key)
			return gateway.SendResult{Success: false, Error: "stream prefix mismatch"}, nil
		}
		delta := text[len(sent):]
		if _, err := client.call(ctx, "chat.appendStream", map[string]any{"channel": chatID, "ts": stream.ts, "markdown_text": delta}); err != nil {
			if !slackErrorIs(err, "message_not_in_streaming_state") {
				return gateway.SendResult{}, err
			}
			slog.Info(fmt.Sprintf("[Slack] Native draft stream %s expired (message_not_in_streaming_state); reopening a fresh stream for chat %s", stream.ts, chatID))
			return a.startStream(ctx, client, *key, draftID, text, metadata, pystr.Len(sent))
		}
		stream.sent = text
		return gateway.SendResult{Success: true, MessageID: stream.ts}, nil
	}()
	if err == nil {
		return result, nil
	}
	a.dropActiveStream(*key)
	errText := err.Error()
	for _, marker := range nativeStreamUnsupportedMarkers {
		if strings.Contains(errText, marker) {
			a.mu.Lock()
			a.nativeStreamUnsupported = true
			a.mu.Unlock()
			slog.Warn(fmt.Sprintf("[Slack] Native streaming unavailable (%s). Falling back to edit-based "+
				"streaming. To enable native streaming, turn on the Agents & AI Apps feature "+
				"for this Slack app (and ensure the assistant:write scope).", errText))
			return gateway.SendResult{Success: false, Error: errText}, nil
		}
	}
	slog.Debug(fmt.Sprintf("[Slack] Native stream frame failed: %s", errText))
	return gateway.SendResult{Success: false, Error: errText}, nil
}

// Forgets the active stream of the thread key
func (a *SlackAdapter) dropActiveStream(key threadKey) {
	a.mu.Lock()
	a.activeStreams.Delete(key)
	a.mu.Unlock()
}

// Opens a native stream for the first frame and registers it, text before base already visible
func (a *SlackAdapter) startStream(ctx context.Context, client *webClient, key threadKey, draftID int, text string, metadata map[string]any, base int) (gateway.SendResult, error) {
	teamID, chatID, threadTS := key[0], key[1], key[2]
	a.sealStaleStreams(ctx)
	kwargs := map[string]any{"channel": chatID, "thread_ts": threadTS}
	md := metadata
	if md == nil {
		md = map[string]any{}
	}
	if userID := firstTruthy(md, "user_id", "sender_id"); userID != nil {
		kwargs["recipient_user_id"] = pyrepr.Str(userID)
	}
	if teamID != "" {
		kwargs["recipient_team_id"] = teamID
	}
	tail := pystr.Slice(text, base, pystr.Len(text))
	if pystr.Strip(tail) != "" {
		kwargs["markdown_text"] = tail
	}
	response, err := client.call(ctx, "chat.startStream", kwargs)
	if err != nil {
		return gateway.SendResult{}, err
	}
	ts := strOrEmpty(response.get("ts"))
	if ts == "" {
		return gateway.SendResult{}, errors.New("chat.startStream returned no ts")
	}
	a.mu.Lock()
	a.activeStreams.Set(key, &draftStream{ts: ts, draftID: draftID, sent: text, started: wallTime(), base: base})
	a.botMessageTS[ts] = true
	a.mu.Unlock()
	return gateway.SendResult{Success: true, MessageID: ts}, nil
}

// Seals and drops streams older than the stream age limit
func (a *SlackAdapter) sealStaleStreams(ctx context.Context) {
	cutoff := wallTime() - streamMaxAgeS
	a.mu.Lock()
	var stale []threadKey
	var streams []*draftStream
	for _, k := range a.activeStreams.Keys() {
		s, _ := a.activeStreams.Get(k)
		if s.started < cutoff {
			stale = append(stale, k)
			streams = append(streams, s)
			a.activeStreams.Delete(k)
		}
	}
	a.mu.Unlock()
	for i, k := range stale {
		a.sealStream(ctx, k, streams[i], "")
	}
}

// Native stream identity of team, chat and thread ts, nil without a thread
func (a *SlackAdapter) streamKey(chatID string, metadata map[string]any) *threadKey {
	teamID := metadataTeamID(metadata)
	if teamID == "" {
		teamID = a.ScopeIDForChat(chatID)
	}
	return workspaceThreadKey(teamID, chatID, a.resolveThreadTS("", metadata))
}

// Stops an open stream appending the unsent tail, true on success
func (a *SlackAdapter) sealStream(ctx context.Context, key threadKey, stream *draftStream, delta string) bool {
	teamID, chatID := key[0], key[1]
	kwargs := map[string]any{"channel": chatID, "ts": stream.ts}
	if pystr.Strip(delta) != "" {
		kwargs["markdown_text"] = delta
	}
	client := a.getClient(chatID, teamID)
	if client == nil {
		slog.Debug(fmt.Sprintf("[Slack] chat.stopStream failed for %s/%s: not connected", chatID, stream.ts))
		return false
	}
	if _, err := client.call(ctx, "chat.stopStream", kwargs); err != nil {
		slog.Debug(fmt.Sprintf("[Slack] chat.stopStream failed for %s/%s: %v", chatID, stream.ts, err))
		return false
	}
	return true
}

// Seals the active stream when the content is its final, nil when unrelated so send posts fresh
func (a *SlackAdapter) tryFinalizeStream(ctx context.Context, chatID, content string, metadata map[string]any) *gateway.SendResult {
	md := metadata
	if md == nil {
		md = map[string]any{}
	}
	if pyrepr.Truthy(md["_interim_send"]) || pyrepr.Truthy(md["expect_edits"]) {
		return nil
	}
	key := a.streamKey(chatID, md)
	if key == nil {
		return nil
	}
	a.mu.Lock()
	stream, _ := a.activeStreams.Get(*key)
	a.mu.Unlock()
	if stream == nil {
		return nil
	}
	text := stripStreamCursor(content)
	sent := stream.sent
	kind, delta := streamRelation(sent, text)
	if kind == "unrelated" {
		if !(pyrepr.Truthy(md["notify"]) && pystr.Strip(text) != "" && pystr.Strip(sent) != "" && mrkdwnInsensitive(text) == mrkdwnInsensitive(sent)) {
			return nil
		}
		return a.commitStream(ctx, *key, stream, text, metadata, "", true)
	}
	if kind == "extends" && pystr.Len(delta) > maxMessageLength {
		a.dropActiveStream(*key)
		a.sealStream(ctx, *key, stream, "")
		return nil
	}
	return a.commitStream(ctx, *key, stream, text, metadata, delta, false)
}

// Closes the stream as the turn's final message, nil when send must post the final fresh
func (a *SlackAdapter) commitStream(ctx context.Context, key threadKey, stream *draftStream, text string, metadata map[string]any, delta string, replace bool) *gateway.SendResult {
	chatID, ts := key[1], stream.ts
	a.dropActiveStream(key)
	sealed := a.sealStream(ctx, key, stream, delta)
	if !sealed && pystr.Strip(delta) == "" {
		sealed = a.sealStream(ctx, key, stream, "")
	}
	var committed bool
	if sealed && !replace && len(a.maybeBlocks(text)) == 0 {
		committed = true
	} else {
		shown := text
		if !replace {
			shown = pystr.Slice(text, stream.base, pystr.Len(text))
		}
		edited := a.editMessage(ctx, chatID, ts, shown, true, metadata)
		committed = edited.Success || (sealed && !replace)
		if !sealed {
			outcome := "in-place update failed too, delivering the final as a fresh post"
			if committed {
				outcome = "final committed via chat.update instead (no duplicate post)"
			}
			slog.Warn(fmt.Sprintf("[Slack] chat.stopStream failed for %s/%s; %s", chatID, ts, outcome))
		}
	}
	if !committed {
		return nil
	}
	_ = a.StopTyping(ctx, chatID, metadata)
	return &gateway.SendResult{Success: true, MessageID: ts}
}
