package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Shows the thread status line through the session status API, refreshes keeping the first start time
func (a *SlackAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	if a.suppressedIgnored(ctx, chatID, "typing/status in", slog.LevelDebug) {
		return nil
	}
	if a.currentApp() == nil {
		return nil
	}
	threadTS := ""
	if len(metadata) > 0 {
		threadTS = a.resolveThreadTS(strOrEmpty(metadata["message_id"]), metadata)
	}
	if threadTS == "" {
		return nil
	}
	teamID := metadataTeamID(metadata)
	a.mu.Lock()
	if teamID == "" {
		teamID, _ = a.channelTeam.Get(chatID)
	}
	var started float64
	hasStarted := false
	if key := workspaceThreadKey(teamID, chatID, threadTS); key != nil {
		if prev, ok := a.activeStatusThreads.Get(*key); ok && prev != nil {
			started, hasStarted = prev.started, true
		}
		if !hasStarted {
			started, hasStarted = monotonic(), true
		}
		a.activeStatusThreads.Set(*key, &statusEntry{threadTS: threadTS, teamID: teamID, started: started})
		if a.activeStatusThreads.Len() > activeStatusThreadsMax {
			discardOldestFromDict(a.activeStatusThreads, a.activeStatusThreads.Len()-activeStatusThreadsMax/2, func(k threadKey) any { return k[2] })
		}
	}
	a.mu.Unlock()
	status, _ := a.StatusText(chatID)
	if status == "" {
		status = strOrEmpty(a.Config.TypingStatusText)
	}
	if status == "" {
		var startedPtr *float64
		if hasStarted {
			startedPtr = &started
		}
		status = defaultStatusText(ctx, startedPtr)
	}
	a.setThreadStatus(ctx, chatID, teamID, threadTS, status, "failed")
	return nil
}

// Sets or clears the thread status line, failures debug logged
func (a *SlackAdapter) setThreadStatus(ctx context.Context, chatID, teamID, threadTS, status, failLabel string) {
	client := a.getClient(chatID, teamID)
	if client == nil {
		slog.Debug(fmt.Sprintf("[Slack] assistant.threads.setStatus %s: not connected", failLabel))
		return
	}
	if err := sessionStatusMethod(client)(ctx, chatID, threadTS, status); err != nil {
		slog.Debug(fmt.Sprintf("[Slack] assistant.threads.setStatus %s: %v", failLabel, err))
	}
}

// Whether status and title go through the Agent Sessions API, as the pinned slack-sdk 3.45 ships
var agentSessionsSupported = true

// Status setter, agents.sessions.setStatus when supported else assistant.threads.setStatus
func sessionStatusMethod(client *webClient) func(ctx context.Context, channelID, threadTS, status string) error {
	method := "assistant.threads.setStatus"
	if agentSessionsSupported {
		method = "agents.sessions.setStatus"
	}
	return func(ctx context.Context, channelID, threadTS, status string) error {
		_, err := client.call(ctx, method, map[string]any{"channel_id": channelID, "thread_ts": threadTS, "status": status})
		return err
	}
}

// Title setter, agents.sessions.rename when supported else assistant.threads.setTitle
func sessionTitleMethod(client *webClient) func(ctx context.Context, channelID, threadTS, title string) error {
	method := "assistant.threads.setTitle"
	if agentSessionsSupported {
		method = "agents.sessions.rename"
	}
	return func(ctx context.Context, channelID, threadTS, title string) error {
		_, err := client.call(ctx, method, map[string]any{"channel_id": channelID, "thread_ts": threadTS, "title": title})
		return err
	}
}

// Clears the assistant thread status, a team less clear only touching a unique tracked match
func (a *SlackAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	if a.suppressedIgnored(ctx, chatID, "status clear in", slog.LevelDebug) {
		return nil
	}
	if a.currentApp() == nil {
		return nil
	}
	requestedThreadTS := ""
	if len(metadata) > 0 {
		requestedThreadTS = strOrEmpty(firstTruthy(metadata, "thread_id", "thread_ts"))
	}
	requestedTeamID := metadataTeamID(metadata)
	var active *statusEntry
	ambiguousTracked := false
	a.mu.Lock()
	if requestedThreadTS != "" && requestedTeamID != "" {
		if key := workspaceThreadKey(requestedTeamID, chatID, requestedThreadTS); key != nil {
			active, _ = a.activeStatusThreads.Get(*key)
			a.activeStatusThreads.Delete(*key)
		}
	} else {
		var matching []threadKey
		for _, k := range a.activeStatusThreads.Keys() {
			if k[1] == chatID && (requestedThreadTS == "" || k[2] == requestedThreadTS) {
				matching = append(matching, k)
			}
		}
		if len(matching) == 1 {
			active, _ = a.activeStatusThreads.Get(matching[0])
			a.activeStatusThreads.Delete(matching[0])
		}
		ambiguousTracked = requestedThreadTS != "" && len(matching) > 1
	}
	a.mu.Unlock()
	if active == nil {
		active = &statusEntry{}
	}
	threadTS := active.threadTS
	teamID := requestedTeamID
	if teamID == "" {
		teamID = active.teamID
	}
	if threadTS == "" && requestedThreadTS != "" && !ambiguousTracked {
		threadTS = requestedThreadTS
	}
	if threadTS == "" {
		return nil
	}
	a.setThreadStatus(ctx, chatID, teamID, threadTS, "", "clear failed")
	return nil
}

// Whether each top level DM reply thread is its own session
func (a *SlackAdapter) dmTopLevelThreadsAsSessions() bool {
	return a.extraFlag("dm_top_level_threads_as_sessions", true)
}

// Continuable cron surface, in_channel when configured, thread otherwise
func (a *SlackAdapter) CronContinuableSurface() string {
	if pystr.Lower(pystr.Strip(pyrepr.Str(a.extra()["cron_continuable_surface"]))) == "in_channel" {
		return "in_channel"
	}
	return "thread"
}

// Warns when in_channel cron delivery is set without flat replies
func (a *SlackAdapter) warnIfInchannelWithoutFlatReply(teamName string) {
	if a.CronContinuableSurface() == "in_channel" && pyrepr.Truthy(getOr(a.extra(), "reply_in_thread", true)) {
		slog.Warn(fmt.Sprintf("[Slack] %s: cron_continuable_surface=in_channel is set WITHOUT "+
			"reply_in_thread=false. A continuable in-channel cron job will deliver flat, "+
			"but the bot will still reply to your continuation in a thread — so it falls "+
			"back to a threaded continuation (≈ default behaviour), not the flat "+
			"channel session you asked for. Set platforms.slack.extra.reply_in_thread: "+
			"false to pair them.", teamName))
	}
}

// Normalized bot message policy, none, mentions or all
func (a *SlackAdapter) slackAllowBots(ctx context.Context) string {
	raw, _ := gateway.ExtraOrSecret(ctx, a.extra(), "allow_bots", "SLACK_ALLOW_BOTS", "none", true)
	value := pystr.Strip(pystr.Lower(pyrepr.Str(raw)))
	if value != "none" && value != "mentions" && value != "all" {
		slog.Warn(fmt.Sprintf("[Slack] Unknown allow_bots=%s; treating as 'none'", pyrepr.Repr(raw)))
		return "none"
	}
	return value
}

// User ids whose Web API posts count as human, from config or SLACK_API_HUMAN_USERS
func (a *SlackAdapter) slackAPIHumanUsers(ctx context.Context) map[string]bool {
	a.mu.Lock()
	cached := a.apiHumanUsersCache
	a.mu.Unlock()
	if cached != nil {
		return cached
	}
	raw, present := a.extra()["api_human_users"]
	if !present || raw == nil {
		raw, _ = gateway.GetScopedSecret(ctx, "SLACK_API_HUMAN_USERS", "", false)
	}
	var parts []any
	if list, ok := raw.([]any); ok {
		parts = list
	} else {
		for _, p := range strings.Split(pyrepr.Str(raw), ",") {
			parts = append(parts, p)
		}
	}
	out := map[string]bool{}
	for _, p := range parts {
		if s := pystr.Strip(pyrepr.Str(p)); s != "" {
			out[s] = true
		}
	}
	a.mu.Lock()
	a.apiHumanUsersCache = out
	a.mu.Unlock()
	return out
}

// Whether the event itself identifies a bot or app sender
func (a *SlackAdapter) eventDeclaresBotSender(ctx context.Context, event map[string]any) bool {
	if pyrepr.Truthy(event["bot_id"]) || pyrepr.Truthy(event["bot_profile"]) || event["subtype"] == "bot_message" {
		return true
	}
	if profile, ok := event["user_profile"].(map[string]any); ok && pyrepr.Truthy(profile["is_bot"]) {
		return true
	}
	if pyrepr.Truthy(event["app_id"]) && !pyrepr.Truthy(event["client_msg_id"]) {
		user, _ := event["user"].(string)
		return !a.slackAPIHumanUsers(ctx)[user]
	}
	return false
}

// Thread ts for an API call, metadata thread over reply_to and flat replies when threading is off
func (a *SlackAdapter) resolveThreadTS(replyTo string, metadata map[string]any) string {
	if !pyrepr.Truthy(getOr(a.extra(), "reply_in_thread", true)) {
		existing := strOrEmpty(firstTruthy(orEmptyMap(metadata), "thread_id", "thread_ts"))
		if existing != "" && replyTo != "" && existing == replyTo {
			existing = ""
		}
		return existing
	}
	if len(metadata) > 0 {
		if v := metadata["thread_id"]; pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
		if v := metadata["thread_ts"]; pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	return replyTo
}

// Uploads a local path or in memory content with retries on transient failures
func (a *SlackAdapter) uploadWithRetry(ctx context.Context, chatID, filePath, filename, caption, threadTS string, metadata map[string]any, label string, content []byte, attempts int) (gateway.SendResult, error) {
	upload := fileUpload{File: filePath, Filename: filename}
	if content != nil {
		upload = fileUpload{Content: content, Filename: filename}
	}
	for attempt := 0; ; attempt++ {
		result, err := a.clientFor(chatID, metadata).filesUploadV2(ctx, chatID, []fileUpload{upload}, caption, threadTS)
		if err == nil {
			a.recordUploadedFileThread(chatID, threadTS, metadata)
			return gateway.SendResult{Success: true, RawResponse: result}, nil
		}
		if !a.isRetryableUploadError(err) || attempt >= attempts-1 {
			return gateway.SendResult{}, err
		}
		slog.Debug(fmt.Sprintf("[Slack] %s retry %d/2 for %s: %v", label, attempt+1, filePath, err))
		select {
		case <-ctx.Done():
			return gateway.SendResult{}, ctx.Err()
		case <-time.After(time.Duration(1.5 * float64(attempt+1) * float64(time.Second))):
		}
	}
}

// Missing local file failure of an upload
type fileNotFoundError struct{ path string }

func (e *fileNotFoundError) Error() string { return "File not found: " + e.path }

// Uploads a local file, a missing file reported as fileNotFoundError
func (a *SlackAdapter) uploadFile(ctx context.Context, chatID, filePath, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if blocked := a.outboundBlocked(ctx, chatID, "file upload in"); blocked != nil {
		return *blocked, nil
	}
	if _, err := os.Stat(filePath); err != nil {
		return gateway.SendResult{}, &fileNotFoundError{path: filePath}
	}
	chatID = a.dmTarget(ctx, chatID, metadata)
	threadTS := a.resolveThreadTS(replyTo, metadata)
	return a.uploadWithRetry(ctx, chatID, filePath, filepath.Base(filePath), caption, threadTS, metadata, "Upload", nil, 3)
}

// Shared body of video and document sends, a failure notice posted in place of a failed upload
func (a *SlackAdapter) sendLocalFile(ctx context.Context, chatID, filePath, caption, replyTo string, metadata map[string]any, kind, filename, notFoundError, failureNotice string) (gateway.SendResult, error) {
	if a.currentApp() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if _, err := os.Stat(filePath); err != nil {
		return gateway.SendResult{Success: false, Error: notFoundError}, nil
	}
	chatID = a.dmTarget(ctx, chatID, metadata)
	threadTS := a.resolveThreadTS(replyTo, metadata)
	label := pystr.Capitalize(kind) + " upload"
	result, err := a.uploadWithRetry(ctx, chatID, filePath, filename, caption, threadTS, metadata, label, nil, 3)
	if err == nil {
		return result, nil
	}
	slog.Error(fmt.Sprintf("[%s] Failed to send %s %s: %v", a.Name(), kind, filePath, err))
	return a.sendFailureNotice(ctx, chatID, caption, failureNotice, replyTo, metadata)
}

var imageCTExts = [][2]string{{"jpeg", "jpg"}, {"jpg", "jpg"}, {"gif", "gif"}, {"webp", "webp"}}

// Sends images ten per files_upload_v2 call, falling back to the per image loop for a failed batch
func (a *SlackAdapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if a.suppressedIgnored(ctx, chatID, "multi-image upload in", slog.LevelWarn) {
		return gateway.SendResult{Success: false, Error: "ignored_channel"}, nil
	}
	if a.currentApp() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	chatID = a.dmTarget(ctx, chatID, metadata)
	threadTS := a.resolveThreadTS("", metadata)
	const chunkSize = 10
	var chunks [][]gateway.ImageRef
	for i := 0; i < len(images); i += chunkSize {
		chunks = append(chunks, images[i:min(i+chunkSize, len(images))])
	}
	delivered := false
	for idx, chunk := range chunks {
		if humanDelay > 0 && idx > 0 {
			time.Sleep(time.Duration(humanDelay * float64(time.Second)))
		}
		uploads, comments := collectImageUploads(ctx, chunk)
		if len(uploads) == 0 {
			continue
		}
		slog.Info(fmt.Sprintf("[Slack] Sending %d image(s) in single files_upload_v2 (chunk %d/%d)", len(uploads), idx+1, len(chunks)))
		if _, err := a.clientFor(chatID, metadata).filesUploadV2(ctx, chatID, uploads, strings.Join(comments, "\n"), threadTS); err != nil {
			slog.Warn(fmt.Sprintf("[Slack] Multi-image files_upload_v2 failed (chunk %d/%d), falling back to per-image: %v", idx+1, len(chunks), err))
			fallback, _ := a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, chunk, metadata, humanDelay)
			delivered = delivered || fallback.Success
			continue
		}
		a.recordUploadedFileThread(chatID, threadTS, metadata)
		delivered = true
	}
	if delivered {
		return gateway.SendResult{Success: true}, nil
	}
	return gateway.SendResult{Success: false, Error: "all images failed to send"}, nil
}

// Upload entries of one image batch, local files by path and remote ones through the SSRF safe client
func collectImageUploads(ctx context.Context, chunk []gateway.ImageRef) ([]fileUpload, []string) {
	var uploads []fileUpload
	var comments []string
	client := tools.NewSSRFSafeClient(gateway.URLSafetyEnv(ctx), 30*time.Second, true)
	for _, img := range chunk {
		if img.Alt != "" {
			comments = append(comments, img.Alt)
		}
		if strings.HasPrefix(img.URL, "file://") {
			localPath, err := url.PathUnescape(img.URL[7:])
			if err != nil {
				localPath = img.URL[7:]
			}
			if _, err := os.Stat(localPath); err != nil {
				slog.Warn(fmt.Sprintf("[Slack] Skipping missing image: %s", localPath))
				continue
			}
			uploads = append(uploads, fileUpload{File: localPath, Filename: filepath.Base(localPath)})
			continue
		}
		if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), img.URL) {
			slog.Warn("[Slack] Blocked unsafe image URL in batch")
			continue
		}
		data, contentType, err := fetchURL(ctx, client, img.URL)
		if err != nil {
			slog.Warn(fmt.Sprintf("[Slack] Download failed for %s: %v", gateway.SafeURLForLog(img.URL, 80), err))
			continue
		}
		ext := "png"
		for _, pair := range imageCTExts {
			if strings.Contains(contentType, pair[0]) {
				ext = pair[1]
				break
			}
		}
		uploads = append(uploads, fileUpload{Content: data, Filename: fmt.Sprintf("image_%d.%s", len(uploads), ext)})
	}
	return uploads, comments
}

// Body and content type of a GET, an HTTP error status reported as httpStatusError
func fetchURL(ctx context.Context, client *http.Client, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", &httpStatusError{StatusCode: resp.StatusCode, URL: rawURL}
	}
	data, err := io.ReadAll(resp.Body)
	return data, resp.Header.Get("Content-Type"), err
}

// Counts a successful upload as bot participation in its thread
func (a *SlackAdapter) recordUploadedFileThread(chatID, threadTS string, metadata map[string]any) {
	if threadTS == "" {
		return
	}
	teamID := metadataTeamID(metadata)
	a.mu.Lock()
	a.botMessageTS[workspaceMessageMarker(teamID, threadTS)] = true
	a.trimBotMessageTimestamps()
	a.mu.Unlock()
}

var transientUploadMarkers = []string{"rate_limited", "ratelimited", "429", "connection reset", "service unavailable", "temporarily unavailable"}

// Whether an upload or send failure looks transient
func (a *SlackAdapter) isRetryableUploadError(err error) bool {
	if status := errorStatusCode(err); status != 0 {
		return status == 429 || status >= 500
	}
	body := pystr.Lower(err.Error())
	for _, m := range transientUploadMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return gateway.IsRetryableErrorText(body)
}

// Boolean config extra flag with string forms accepted, def when unset
func (a *SlackAdapter) extraFlag(key string, def bool) bool {
	raw, ok := a.extra()[key]
	if !ok || raw == nil {
		return def
	}
	switch pystr.Lower(pystr.Strip(pyrepr.Str(raw))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Blocks with feedback controls appended when enabled and the block budget allows
func (a *SlackAdapter) appendFeedbackBlock(blocks []any) []any {
	if len(blocks) > 0 && a.extraFlag("feedback_buttons", false) && len(blocks) < 50 {
		return append(append([]any(nil), blocks...), feedbackBlock(context.Background()))
	}
	return blocks
}

// Block Kit for the content, native markdown blocks over the local renderer, nil when disabled
func (a *SlackAdapter) maybeBlocks(content string) []any {
	if a.extraFlag("markdown_blocks", false) {
		if md := markdownBlockPayload(content); md != nil {
			return SanitizeBlocks(a.appendFeedbackBlock(md))
		}
	}
	if !a.extraFlag("rich_blocks", false) {
		return nil
	}
	return SanitizeBlocks(a.appendFeedbackBlock(RenderBlocks(content, a.FormatMessage)))
}

// Compact tool arguments kept out of mrkdwn emphasis conversion
func (a *SlackAdapter) FormatToolPreview(preview tools.ToolPreview) string {
	return "`" + strings.ReplaceAll(preview.Text, "`", "ˋ") + "`"
}

// Standard markdown converted to Slack mrkdwn with tables fenced, code protected and broadcasts escaped
func (a *SlackAdapter) FormatMessage(content string) string {
	return formatSlackMrkdwn(content)
}

// Adds or removes a reaction, true on success and failures debug logged
func (a *SlackAdapter) react(ctx context.Context, channel, timestamp, emoji, teamID string, remove bool) bool {
	if a.currentApp() == nil {
		return false
	}
	method, verb := "reactions.add", "add"
	if remove {
		method, verb = "reactions.remove", "remove"
	}
	if _, err := a.getClient(channel, teamID).call(ctx, method, map[string]any{"channel": channel, "timestamp": timestamp, "name": emoji}); err != nil {
		slog.Debug(fmt.Sprintf("[Slack] reactions.%s failed (%s): %v", verb, emoji, err))
		return false
	}
	return true
}

// Adds a reaction to a message
func (a *SlackAdapter) AddReaction(ctx context.Context, chatID, messageID, emoji string) error {
	a.react(ctx, chatID, messageID, emoji, "", false)
	return nil
}

// Removes the in progress reaction from a message
func (a *SlackAdapter) RemoveReaction(ctx context.Context, chatID, messageID string) error {
	a.react(ctx, chatID, messageID, "eyes", "", true)
	return nil
}

// Whether message reactions are enabled, SLACK_REACTIONS then config, on by default
func (a *SlackAdapter) ReactionsEnabled() bool {
	configured, _ := gateway.ExtraOrSecret(context.Background(), a.extra(), "reactions", "SLACK_REACTIONS", "true", true)
	switch pystr.Lower(pyrepr.Str(configured)) {
	case "false", "0", "no":
		return false
	}
	return true
}

// Message ts, team and marker of an event under reaction tracking, ok false otherwise
func (a *SlackAdapter) reactingTarget(event *gateway.MessageEvent) (string, string, any, bool) {
	if !a.ReactionsEnabled() {
		return "", "", nil, false
	}
	ts := derefString(event.MessageID)
	teamID := ""
	if event.Source != nil {
		teamID = derefString(event.Source.ScopeID)
	}
	if ts == "" {
		return "", "", nil, false
	}
	marker := workspaceMessageMarker(teamID, ts)
	a.mu.Lock()
	tracked := a.reactingMessageIDs[marker]
	a.mu.Unlock()
	if !tracked {
		return "", "", nil, false
	}
	return ts, teamID, marker, true
}

// Adds the in progress reaction when processing of a tracked message begins
func (a *SlackAdapter) OnProcessingStart(ctx context.Context, event *gateway.MessageEvent) error {
	ts, teamID, _, ok := a.reactingTarget(event)
	if !ok {
		return nil
	}
	if event.Source != nil && event.Source.ChatID != "" {
		a.react(ctx, event.Source.ChatID, ts, "eyes", teamID, false)
	}
	return nil
}

// Swaps the in progress reaction for the success or failure reaction
func (a *SlackAdapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	ts, teamID, marker, ok := a.reactingTarget(event)
	if !ok {
		return nil
	}
	a.mu.Lock()
	delete(a.reactingMessageIDs, marker)
	a.mu.Unlock()
	if event.Source == nil || event.Source.ChatID == "" {
		return nil
	}
	channelID := event.Source.ChatID
	a.react(ctx, channelID, ts, "eyes", teamID, true)
	switch outcome {
	case gateway.ProcessingSuccess:
		a.react(ctx, channelID, ts, "white_check_mark", teamID, false)
	case gateway.ProcessingFailure:
		a.react(ctx, channelID, ts, "x", teamID, false)
	}
	return nil
}

// Display name lookup of the inbound pipeline, the name hermes tests replace as _resolve_user_name
var resolveUserNameFn = (*SlackAdapter).resolveUserName

// Display name of a workspace local user id, cached per team and falling back to the id
func (a *SlackAdapter) resolveUserName(ctx context.Context, userID, chatID, teamID string) string {
	if userID == "" {
		return ""
	}
	a.mu.Lock()
	if teamID == "" {
		teamID, _ = a.channelTeam.Get(chatID)
	}
	key := teamKey{teamID, userID}
	if cached, ok := a.userNameCache.Get(key); ok {
		a.mu.Unlock()
		return cached
	}
	a.mu.Unlock()
	if a.currentApp() == nil {
		return userID
	}
	name := userID
	payload, err := a.usersInfoPayload(ctx, userID, chatID, teamID)
	if err != nil {
		slog.Debug(fmt.Sprintf("[Slack] users.info failed for %s: %v", userID, err))
	} else if len(payload) == 0 {
		a.mu.Lock()
		a.userIsBotCache.Set(key, false)
		a.userNameCache.Set(key, userID)
		a.mu.Unlock()
		return userID
	} else {
		var isBot bool
		name, isBot = parseUsersInfo(payload, userID)
		a.mu.Lock()
		a.userIsBotCache.Set(key, isBot)
		a.mu.Unlock()
	}
	a.mu.Lock()
	a.userNameCache.Set(key, name)
	trimOldestDictEntries(a.userNameCache, userNameCacheMax)
	a.mu.Unlock()
	return name
}

// Channel name or DM peer display name of a channel id, cached and falling back to the id
func (a *SlackAdapter) resolveChannelName(ctx context.Context, channelID, teamID string) string {
	if channelID == "" {
		return channelID
	}
	a.mu.Lock()
	if teamID == "" {
		teamID, _ = a.channelTeam.Get(channelID)
	}
	key := teamKey{teamID, channelID}
	if cached, ok := a.channelNameCache.Get(key); ok {
		a.mu.Unlock()
		return cached
	}
	a.mu.Unlock()
	if a.currentApp() == nil {
		return channelID
	}
	name := channelID
	result, err := a.getClient(channelID, teamID).call(ctx, "conversations.info", map[string]any{"channel": channelID})
	if err != nil {
		slog.Debug(fmt.Sprintf("[Slack] conversations.info failed for %s: %v", channelID, err))
	} else {
		ch := asMap(result.get("channel"))
		switch {
		case pyrepr.Truthy(ch["is_im"]):
			if peer := strOrEmpty(ch["user"]); peer != "" {
				name = resolveUserNameFn(a, ctx, peer, channelID, teamID)
			}
		default:
			if v := firstTruthy(ch, "name", "name_normalized"); v != nil {
				name = pyrepr.Str(v)
			}
		}
	}
	a.mu.Lock()
	a.channelNameCache.Set(key, name)
	trimOldestDictEntries(a.channelNameCache, channelNameCacheMax)
	a.mu.Unlock()
	return name
}

var userMentionIDRe = mustPyRe(`<@([A-Z0-9]+)(?:\|[^>]*)?>`)

// User mentions rewritten to @DisplayName so the agent never mistakes a human mention for itself
func (a *SlackAdapter) humanizeUserMentions(ctx context.Context, text, chatID, teamID string) string {
	if text == "" || !strings.Contains(text, "<@") {
		return text
	}
	seen := map[string]bool{}
	for _, m := range findAll(userMentionIDRe, text) {
		uid := m.groups[1]
		if seen[uid] {
			continue
		}
		seen[uid] = true
		name := resolveUserNameFn(a, ctx, uid, chatID, teamID)
		if name == "" {
			name = uid
		}
		display := pystr.Strip(name)
		if display == "" {
			display = uid
		}
		re := mustPyRe("<@" + regexp.QuoteMeta(uid) + `(?:\|[^>]*)?>`)
		text = replaceAll(re, text, func(reMatch) string { return "@" + display })
	}
	return text
}

// Ephemeral system prompt line naming the bot's handle, empty when no name is known
func (a *SlackAdapter) buildIdentityPrompt(teamID string) string {
	a.mu.Lock()
	name := ""
	if teamID != "" {
		name = a.teamBotNames[teamID]
	}
	if name == "" && a.botDisplayName != nil {
		name = *a.botDisplayName
	}
	a.mu.Unlock()
	name = pystr.Strip(name)
	if name == "" {
		return ""
	}
	return "You are connected to this Slack workspace as the bot " +
		"\"@" + name + "\". The adapter already applied mention and channel " +
		"routing; treat every delivered turn as intentionally routed to " +
		"you. Your routing mention \"@" + name + "\" may have been stripped from " +
		"the visible text — do not reject or ignore a message solely " +
		"because \"@" + name + "\" is absent. In messages, each line is prefixed " +
		"with the sender's name, and visible mentions are shown as " +
		"@DisplayName; a mention of any other participant is not a " +
		"mention of you, even if their name is similar."
}

// Whether a workspace local user id is a bot account, cached per team
func (a *SlackAdapter) resolveUserIsBot(ctx context.Context, userID, chatID, teamID string) bool {
	if userID == "" {
		return false
	}
	a.mu.Lock()
	if teamID == "" {
		teamID, _ = a.channelTeam.Get(chatID)
	}
	key := teamKey{teamID, userID}
	if cached, ok := a.userIsBotCache.Get(key); ok {
		a.mu.Unlock()
		return cached
	}
	a.mu.Unlock()
	if a.currentApp() == nil {
		a.mu.Lock()
		a.userIsBotCache.Set(key, false)
		a.mu.Unlock()
		return false
	}
	payload, err := a.usersInfoPayload(ctx, userID, chatID, teamID)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		slog.Debug(fmt.Sprintf("[Slack] users.info bot check failed for %s: %v", userID, err))
		a.userIsBotCache.Set(key, false)
		return false
	}
	if len(payload) == 0 {
		a.userIsBotCache.Set(key, false)
		if !a.userNameCache.Has(key) {
			a.userNameCache.Set(key, userID)
		}
		return false
	}
	name, isBot := parseUsersInfo(payload, userID)
	a.userIsBotCache.Set(key, isBot)
	trimOldestDictEntries(a.userIsBotCache, userNameCacheMax)
	a.userNameCache.Set(key, name)
	return isBot
}

// users.info payload through the channel's client or the primary one
func (a *SlackAdapter) usersInfoPayload(ctx context.Context, userID, chatID, teamID string) (map[string]any, error) {
	var client *webClient
	if chatID != "" {
		client = a.getClient(chatID, teamID)
	} else if app := a.currentApp(); app != nil {
		client = app.client
	}
	if client == nil {
		return nil, errors.New("Not connected")
	}
	result, err := client.call(ctx, "users.info", map[string]any{"user": userID})
	if err != nil {
		var apiErr *slackAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusOK {
			return apiErr.Response, nil
		}
		return nil, err
	}
	return result.Data, nil
}

// Uploads a local image file, a failure notice posted in place of a failed upload
func (a *SlackAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	result, err := a.uploadFile(ctx, chatID, imagePath, caption, replyTo, metadata)
	if err == nil {
		return result, nil
	}
	var nf *fileNotFoundError
	if errors.As(err, &nf) {
		return gateway.SendResult{Success: false, Error: "Image file not found: " + imagePath}, nil
	}
	slog.Error(fmt.Sprintf("[%s] Failed to send local Slack image %s: %v", a.Name(), imagePath, err))
	return a.sendFailureNotice(ctx, chatID, caption, i18n.T(ctx, "platform.shared.media.image_failed", "", nil), replyTo, metadata)
}

// Posts the notice prefixed by the caption in place of a failed media delivery
func (a *SlackAdapter) sendFailureNotice(ctx context.Context, chatID, caption, notice, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.EmitMediaWarning(ctx, chatID, notice, caption, replyTo, metadata, metadata)
}

// Uploads an image fetched from a safe URL, falling back to posting the URL as text
func (a *SlackAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if a.currentApp() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), imageURL) {
		slog.Warn("[Slack] Blocked unsafe image URL (SSRF protection)")
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, metadata)
	}
	data, _, err := fetchURL(ctx, tools.NewSSRFSafeClient(gateway.URLSafetyEnv(ctx), 30*time.Second, true), imageURL)
	var result gateway.SendResult
	if err == nil {
		threadTS := a.resolveThreadTS(replyTo, metadata)
		target := a.dmTarget(ctx, chatID, metadata)
		result, err = a.uploadWithRetry(ctx, target, "", "image.png", caption, threadTS, metadata, "Upload", data, 1)
	}
	if err == nil {
		return result, nil
	}
	slog.Warn(fmt.Sprintf("[Slack] Failed to upload image from URL %s, falling back to text: %v", gateway.SafeURLForLog(imageURL, 80), err))
	text := imageURL
	if caption != "" {
		text = caption + "\n" + imageURL
	}
	return a.Send(ctx, chatID, text, replyTo, metadata)
}

// Uploads an audio file
func (a *SlackAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	result, err := a.uploadFile(ctx, chatID, audioPath, caption, replyTo, metadata)
	if err == nil {
		return result, nil
	}
	var nf *fileNotFoundError
	if errors.As(err, &nf) {
		return gateway.SendResult{Success: false, Error: "Audio file not found: " + audioPath}, nil
	}
	slog.Error(fmt.Sprintf("[Slack] Failed to send audio file %s: %v", audioPath, err))
	return gateway.SendResult{Success: false, Error: err.Error()}, nil
}

// Uploads a video file with a failure notice on error
func (a *SlackAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, videoPath, caption, replyTo, metadata, "video", filepath.Base(videoPath),
		"Video file not found: "+videoPath, i18n.T(ctx, "platform.shared.media.video_failed", "", nil))
}

// Uploads a document under its display name, never echoing the host path
func (a *SlackAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	displayName := fileName
	if displayName == "" {
		displayName = filepath.Base(filePath)
	}
	return a.sendLocalFile(ctx, chatID, filePath, caption, replyTo, metadata, "document", displayName,
		"File not found: "+filePath, i18n.T(ctx, "platform.shared.media.file_failed", "", map[string]any{"name": displayName}))
}

// Name and kind of a channel, unknown when the lookup fails
func (a *SlackAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	if a.currentApp() == nil {
		return map[string]any{"name": chatID, "type": "unknown"}, nil
	}
	result, err := a.getClient(chatID, "").call(ctx, "conversations.info", map[string]any{"channel": chatID})
	if err != nil {
		slog.Error(fmt.Sprintf("[Slack] Failed to fetch chat info for %s: %v", chatID, err))
		return map[string]any{"name": chatID, "type": "unknown"}, nil
	}
	channel := asMap(getOr(result.Data, "channel", map[string]any{}))
	kind := "group"
	if pyrepr.Truthy(getOr(channel, "is_im", false)) {
		kind = "dm"
	}
	return map[string]any{"name": getOr(channel, "name", chatID), "type": kind}, nil
}

// Remembers a user's current agent view context
func (a *SlackAdapter) cacheAgentViewContext(metadata map[string]string) {
	key, ok := agentViewContextKey(metadata["team_id"], metadata["user_id"])
	if !ok {
		return
	}
	kept := map[string]string{}
	for _, field := range []string{"channel_id", "context_channel_id", "team_id", "user_id"} {
		if v := metadata[field]; v != "" {
			kept[field] = v
		}
	}
	a.mu.Lock()
	a.agentViewContexts.Set(key, kept)
	trimOldestDictEntries(a.agentViewContexts, agentViewContextsMax)
	a.mu.Unlock()
}

// Inline agent context of the event, falling back to the cached lifecycle context
func (a *SlackAdapter) agentViewContextForEvent(event map[string]any, teamID, userID string) map[string]string {
	ctxChannel := contextChannelID(firstTruthy(event, "app_context", "context"))
	if ctxChannel == "" {
		if key, ok := agentViewContextKey(teamID, userID); ok {
			a.mu.Lock()
			cached, _ := a.agentViewContexts.Get(key)
			a.mu.Unlock()
			ctxChannel = cached["context_channel_id"]
		}
	}
	return map[string]string{"context_channel_id": ctxChannel, "team_id": teamID, "user_id": userID}
}

// Claims a message ts for the message_changed guard, keeping the newest entries under the cap
func (a *SlackAdapter) rememberProcessedMessageTS(ts string) {
	if ts == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.processedMessageTS[ts] = wallTime()
	if len(a.processedMessageTS) > a.processedTSCap {
		type entry struct {
			ts string
			at float64
		}
		entries := make([]entry, 0, len(a.processedMessageTS))
		for k, v := range a.processedMessageTS {
			entries = append(entries, entry{k, v})
		}
		sortEntries := func(i, j int) bool { return entries[i].at < entries[j].at }
		sort.Slice(entries, sortEntries)
		kept := map[string]float64{}
		for _, e := range entries[len(entries)-a.processedTSCap:] {
			kept[e.ts] = e.at
		}
		a.processedMessageTS = kept
	}
}

// Remembers workspace local assistant identity for later message events
func (a *SlackAdapter) cacheAssistantThreadMetadata(metadata map[string]string) {
	channelID, threadTS, teamID := metadata["channel_id"], metadata["thread_ts"], metadata["team_id"]
	key := workspaceThreadKey(teamID, channelID, threadTS)
	if key == nil {
		return
	}
	a.mu.Lock()
	existing, _ := a.assistantThreads.Get(*key)
	merged := map[string]string{}
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range metadata {
		if v != "" {
			merged[k] = v
		}
	}
	a.assistantThreads.Set(*key, merged)
	trimOldestDictEntries(a.assistantThreads, assistantThreadsMax)
	a.mu.Unlock()
	if teamID != "" && channelID != "" {
		a.rememberChannelTeam(channelID, teamID)
	}
}

// Workspace scoped assistant metadata of the current event merged over the cached entry
func (a *SlackAdapter) lookupAssistantThreadMetadata(event map[string]any, channelID, threadTS, teamID string, body map[string]any) map[string]string {
	metadata := extractAssistantThreadMetadata(event, body)
	if channelID != "" && metadata["channel_id"] == "" {
		metadata["channel_id"] = channelID
	}
	if threadTS != "" && metadata["thread_ts"] == "" {
		metadata["thread_ts"] = threadTS
	}
	if teamID != "" && metadata["team_id"] == "" {
		metadata["team_id"] = teamID
	}
	key := workspaceThreadKey(metadata["team_id"], metadata["channel_id"], metadata["thread_ts"])
	if key == nil {
		return metadata
	}
	a.mu.Lock()
	cached, _ := a.assistantThreads.Get(*key)
	a.mu.Unlock()
	if len(cached) == 0 {
		return metadata
	}
	merged := map[string]string{}
	for k, v := range cached {
		merged[k] = v
	}
	for k, v := range metadata {
		if v != "" {
			merged[k] = v
		}
	}
	return merged
}

// Whether channel messages need a mention, unrecognised values keeping the gate on
func (a *SlackAdapter) slackRequireMention(ctx context.Context) bool {
	configured, ok := a.extra()["require_mention"]
	if !ok || configured == nil {
		configured, _ = gateway.GetScopedSecret(ctx, "SLACK_REQUIRE_MENTION", "true", false)
	}
	if s, ok := configured.(string); ok {
		switch pystr.Lower(s) {
		case "false", "0", "no", "off":
			return false
		}
		return true
	}
	return pyrepr.Truthy(configured)
}

// Opt in boolean, config extra first then the environment variable, false by default
func (a *SlackAdapter) extraOrEnvFlag(ctx context.Context, key, envVar string, strip bool) bool {
	configured, _ := gateway.ExtraOrSecret(ctx, a.extra(), key, envVar, "false", false)
	if s, ok := configured.(string); ok {
		if strip {
			s = pystr.Strip(s)
		}
		switch pystr.Lower(s) {
		case "true", "1", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(configured)
}

// Channel id set from config extra as a list or CSV, else the environment variable CSV
func (a *SlackAdapter) extraOrEnvChannelSet(ctx context.Context, key, envVar string, coerceScalar bool) map[string]bool {
	raw, _ := gateway.ExtraOrSecret(ctx, a.extra(), key, envVar, "", false)
	out := map[string]bool{}
	if list, ok := raw.([]any); ok {
		for _, part := range list {
			if s := pystr.Strip(pyrepr.Str(part)); s != "" {
				out[s] = true
			}
		}
		return out
	}
	if coerceScalar {
		if raw == nil {
			raw = ""
		} else {
			raw = pystr.Strip(pyrepr.Str(raw))
		}
	}
	if s, ok := raw.(string); ok && pystr.Strip(s) != "" {
		for _, part := range strings.Split(s, ",") {
			if p := pystr.Strip(part); p != "" {
				out[p] = true
			}
		}
	}
	return out
}

// Channels where the generic gateway stays silent
func (a *SlackAdapter) slackIgnoredChannels(ctx context.Context) map[string]bool {
	return slackIgnoredChannelsGetter(a, ctx)
}
