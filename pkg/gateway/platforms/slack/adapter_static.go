package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const markdownBlockMax = 11500

type threadKey = [3]string

type teamKey = [2]string

func monotonic() float64 { return float64(time.Since(processStart)) / float64(time.Second) }

var processStart = time.Now()

func slackTimestampSortKey(ts any) (int64, int64, string) {
	if pair, ok := ts.([2]string); ok {
		ts = pair[1]
	}
	text := pyrepr.Str(ts)
	seconds, fraction, _ := strings.Cut(text, ".")
	frac := (fraction + "000000")[:6]
	return intOrZero(seconds), intOrZero(frac), text
}

func intOrZero(value string) int64 {
	n, err := strconv.ParseInt(pystr.Strip(strings.ReplaceAll(value, "_", "")), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func sortByTS[K comparable](keys []K, tsGetter func(K) any) []K {
	out := append([]K(nil), keys...)
	sort.SliceStable(out, func(i, j int) bool {
		as, af, at := slackTimestampSortKey(tsGetter(out[i]))
		bs, bf, bt := slackTimestampSortKey(tsGetter(out[j]))
		if as != bs {
			return as < bs
		}
		if af != bf {
			return af < bf
		}
		return at < bt
	})
	return out
}

func discardOldestFromSet[K comparable](entries map[K]bool, count int, tsGetter func(K) any) {
	if count <= 0 {
		return
	}
	keys := make([]K, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sorted := sortByTS(keys, tsGetter)
	for _, k := range sorted[:min(count, len(sorted))] {
		delete(entries, k)
	}
}

func discardOldestFromDict[K comparable, V any](entries *pydict.Dict[K, V], count int, tsGetter func(K) any) {
	if count <= 0 {
		return
	}
	sorted := sortByTS(entries.Keys(), tsGetter)
	for _, k := range sorted[:min(count, len(sorted))] {
		entries.Delete(k)
	}
}

func identityTS[K comparable](k K) any { return k }

func trimOldestDictEntries[K comparable, V any](mapping *pydict.Dict[K, V], maxSize int) {
	if mapping.Len() <= maxSize {
		return
	}
	excess := mapping.Len() - maxSize/2
	for _, k := range mapping.Keys()[:excess] {
		mapping.Delete(k)
	}
}

var slackPermissionErrors = map[string]bool{"access_denied": true, "file_access_denied": true, "no_permission": true, "not_allowed_token_type": true, "restricted_action": true}

var slackHTTPStatusTemplates = map[int]string{
	401: "Slack attachment access failed for %[1]s with HTTP 401. The bot token is not authorized for this file.",
	403: "Slack attachment access failed for %[1]s with HTTP 403. The bot likely lacks permission or scope to read this file.",
	404: "Slack attachment %[1]s returned HTTP 404 and is no longer reachable.",
}

var slackAPIErrorTemplates = []struct {
	codes    map[string]bool
	template string
}{
	{map[string]bool{"not_authed": true, "invalid_auth": true, "account_inactive": true, "token_revoked": true},
		"Slack attachment access failed for %[1]s because the bot token is not authorized (%[2]s). Refresh the token/reinstall the app."},
	{map[string]bool{"file_not_found": true, "file_deleted": true},
		"Slack attachment %[1]s is no longer available (%[2]s)."},
	{slackPermissionErrors,
		"Slack attachment access failed for %[1]s because the bot does not have permission (%[2]s). Check workspace permissions/scopes and reinstall if needed."},
}

func describeSlackAPIError(response map[string]any, fileObj map[string]any) string {
	if response == nil {
		return ""
	}
	errCode := pystr.Strip(strOrEmpty(response["error"]))
	if errCode == "" {
		return ""
	}
	fileLabel := attachmentLabel(fileObj)
	if errCode == "missing_scope" {
		needed := pystr.Strip(strOrEmpty(response["needed"]))
		provided := pystr.Strip(strOrEmpty(response["provided"]))
		neededHint := "Missing required Slack scope."
		if needed != "" {
			neededHint = "Missing scope: " + needed + "."
		}
		providedHint := ""
		if provided != "" {
			providedHint = " Current bot scopes: " + provided + "."
		}
		return "Slack attachment access failed for " + fileLabel + ". " + neededHint + providedHint +
			" Update the Slack app scopes/settings and reinstall the app to the workspace."
	}
	for _, row := range slackAPIErrorTemplates {
		if row.codes[errCode] {
			return fmt.Sprintf(row.template, fileLabel, errCode)
		}
	}
	return ""
}

// Non success HTTP status of a file download, the httpx HTTPStatusError counterpart
type httpStatusError struct {
	StatusCode int
	URL        string
	Location   string
}

func describeSlackDownloadFailure(err error, fileObj map[string]any) string {
	fileLabel := attachmentLabel(fileObj)
	var apiErr *slackAPIError
	if errors.As(err, &apiErr) {
		if detail := describeSlackAPIError(apiErr.Response, fileObj); detail != "" {
			return detail
		}
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		if template, ok := slackHTTPStatusTemplates[statusErr.StatusCode]; ok {
			return fmt.Sprintf(template, fileLabel)
		}
	}
	message := err.Error()
	if strings.Contains(message, "Slack returned HTML instead of media") || strings.Contains(message, "non-image data") {
		return "Slack attachment access failed for " + fileLabel + ": Slack returned an HTML/login or non-media response. " +
			"This usually means a scope, auth, or file-permission problem."
	}
	return ""
}

func metadataTeamID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if found := firstTruthy(metadata, "scope_id", "slack_team_id", "team_id", "team", "guild_id", "workspace_id"); found != nil {
		return pyrepr.Str(found)
	}
	switch source := metadata["source"].(type) {
	case map[string]any:
		if found := firstTruthy(source, "scope_id", "slack_team_id", "team_id", "guild_id"); found != nil {
			return pyrepr.Str(found)
		}
	case *gateway.SessionSource:
		if source != nil {
			if source.ScopeID != nil && *source.ScopeID != "" {
				return *source.ScopeID
			}
			if source.GuildID != nil && *source.GuildID != "" {
				return *source.GuildID
			}
		}
	}
	return ""
}

func workspaceEventID(teamID, eventID string) string {
	if teamID != "" {
		return teamID + ":" + eventID
	}
	return eventID
}

func workspaceMessageMarker(teamID, messageID string) any {
	if teamID != "" {
		return [2]string{teamID, messageID}
	}
	return messageID
}

func truthyConfig(value any) bool {
	if s, ok := value.(string); ok {
		switch pystr.Lower(pystr.Strip(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(value)
}

func taskUpdateChunk(task map[string]string) map[string]any {
	status := task["status"]
	if status == "" {
		status = "in_progress"
	}
	if status != "in_progress" && status != "complete" && status != "error" {
		status = "in_progress"
	}
	taskID := task["id"]
	if taskID == "" {
		taskID = task["task_id"]
	}
	if taskID == "" {
		taskID = "task"
	}
	title := task["title"]
	if title == "" {
		title = taskID
	}
	return map[string]any{"type": "task_update", "id": taskID, "title": pystr.Slice(title, 0, 256), "status": status}
}

func retryAfterFromErr(err error) *float64 {
	d, ok := provider.RetryAfterFromHeaders(errorHeaders(err))
	if !ok {
		return nil
	}
	secs := d.Seconds()
	return &secs
}

var streamCursorGlyphs = []string{"▉", "▍", "▌", "…"}

func stripStreamCursor(text string) string {
	stripped := pystr.Rstrip(text)
	for _, g := range streamCursorGlyphs {
		if strings.HasSuffix(stripped, g) {
			return pystr.Rstrip(strings.TrimSuffix(stripped, g))
		}
	}
	return text
}

var mrkdwnMarkerRe = mustPyRe(`[*_~]`)

func mrkdwnInsensitive(text string) string {
	return strings.Join(pystr.SplitWhitespace(replaceAll(mrkdwnMarkerRe, text, func(reMatch) string { return "" }), -1), " ")
}

func streamRelation(sent, text string) (string, string) {
	core := pystr.Strip(sent)
	if core == "" {
		return "unrelated", ""
	}
	if strings.HasPrefix(text, sent) {
		delta := text[len(sent):]
		if delta != "" {
			return "extends", delta
		}
		return "equal", ""
	}
	lead := len(text) - len(pystr.Lstrip(text))
	if strings.HasPrefix(text[lead:], core) {
		delta := text[lead+len(core):]
		if pystr.Strip(delta) != "" {
			return "extends", delta
		}
		return "equal", delta
	}
	return "unrelated", ""
}

func defaultStatusText(ctx context.Context, started *float64) string {
	elapsed := 0
	if started != nil {
		elapsed = int(monotonic() - *started)
	}
	if elapsed < 30 {
		return i18n.T(ctx, "platform.slack.status.thinking", "", nil)
	}
	mins, secs := elapsed/60, elapsed%60
	label := fmt.Sprintf("%ds", secs)
	if mins > 0 {
		label = fmt.Sprintf("%dm%02ds", mins, secs)
	}
	return i18n.T(ctx, "platform.slack.status.still_working", "", map[string]any{"elapsed": label})
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

var recoverableBlockCodes = []string{"invalid_blocks", "msg_too_long", "too_many_blocks"}

func isBlockPayloadRejection(err error) bool {
	code := slackErrorCode(err)
	text := err.Error()
	for _, c := range recoverableBlockCodes {
		if code == c || strings.Contains(text, c) {
			return true
		}
	}
	return false
}

func markdownBlockPayload(content string) []any {
	if pystr.Strip(content) == "" || pystr.Len(content) > markdownBlockMax {
		return nil
	}
	return []any{map[string]any{"type": "markdown", "text": content}}
}

func feedbackBlock(ctx context.Context) map[string]any {
	label := func(key string) string { return pystr.Slice(i18n.T(ctx, key, "", nil), 0, 75) }
	return map[string]any{
		"type": "context_actions",
		"elements": []any{map[string]any{
			"type":      "feedback_buttons",
			"action_id": "hermes_feedback",
			"positive_button": map[string]any{
				"text":                map[string]any{"type": "plain_text", "text": label("platform.slack.feedback.good")},
				"accessibility_label": label("platform.slack.feedback.good_a11y"),
				"value":               "positive",
			},
			"negative_button": map[string]any{
				"text":                map[string]any{"type": "plain_text", "text": label("platform.slack.feedback.bad")},
				"accessibility_label": label("platform.slack.feedback.bad_a11y"),
				"value":               "negative",
			},
		}},
	}
}

var (
	fenceLangRe       = mustPyRe("\\A```[^\\s`]+[ \\t]*(\\r?\\n)")
	headerBoldRe      = mustPyRe(`\*\*(.+?)\*\*`)
	formatFenceRe     = mustPyRe("(```(?:[^\\n]*\\n)?[\\s\\S]*?```)")
	formatCodeRe      = mustPyRe("(`[^`]+`)")
	formatLinkRe      = mustPyRe(`(?<!!)\[([^\]]+)\]\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)
	formatEntityRe    = mustPyRe(`(<(?:[@#!]|(?:https?|mailto|tel):)[^>\n]+>)`)
	formatQuoteRe     = mustPyRe(`(?m)^(>+\s)`)
	formatHeaderRe    = mustPyRe(`(?m)^#{1,6}\s+(.+)$`)
	formatBoldItalRe  = mustPyRe(`\*\*\*(.+?)\*\*\*`)
	formatBoldRe      = mustPyRe(`\*\*(.+?)\*\*`)
	formatItalicRe    = mustPyRe(`(?<!\*)\*(\S(?:[^*\n]*?\S)?)\*(?!\*)`)
	formatStrikeRe    = mustPyRe(`~~(.+?)~~`)
	specialMentionRe  = mustPyRe(`(?i)<!(?:everyone|channel|here)(?:\|[^>\n]*)?>`)
	formatEscapeAmpRe = regexp.MustCompile(`&(amp|lt|gt);`)
)

func replaceFirst(re *regexp2.Regexp, s string, fn func(reMatch) string) string {
	m, ok := search(re, s)
	if !ok {
		return s
	}
	r := []rune(s)
	return string(r[:m.start]) + fn(m) + string(r[m.end:])
}

func parseUsersInfo(payload map[string]any, userID string) (string, bool) {
	user := asMap(getOr(payload, "user", map[string]any{}))
	profile := asMap(getOr(user, "profile", map[string]any{}))
	isBot := pyrepr.Truthy(user["is_bot"]) || pyrepr.Truthy(user["is_workflow_bot"]) || pyrepr.Truthy(profile["bot_id"])
	name := userID
	if v := firstTruthy(profile, "display_name", "real_name"); v != nil {
		name = pyrepr.Str(v)
	} else if v := firstTruthy(user, "real_name", "name"); v != nil {
		name = pyrepr.Str(v)
	}
	return name, isBot
}

func workspaceThreadKey(teamID, channelID, threadTS string) *threadKey {
	if channelID == "" || threadTS == "" {
		return nil
	}
	return &threadKey{teamID, channelID, threadTS}
}

func agentViewContextKey(teamID, userID string) (teamKey, bool) {
	if teamID == "" || userID == "" {
		return teamKey{}, false
	}
	return teamKey{teamID, userID}, true
}

func eventTeamID(event, body map[string]any) string {
	for _, payload := range []map[string]any{event, body} {
		if payload == nil {
			continue
		}
		team := firstTruthy(payload, "team_id", "team")
		if s, ok := team.(string); ok && s != "" {
			return s
		}
		if m, ok := team.(map[string]any); ok && pyrepr.Truthy(m["id"]) {
			return pyrepr.Str(m["id"])
		}
	}
	for _, raw := range asList(orEmptyMap(body)["authorizations"]) {
		if auth, ok := raw.(map[string]any); ok && pyrepr.Truthy(auth["team_id"]) {
			return pyrepr.Str(auth["team_id"])
		}
	}
	return ""
}

func contextChannelID(context any) string {
	ctxMap, ok := context.(map[string]any)
	if !ok {
		return ""
	}
	if pyrepr.Truthy(ctxMap["channel_id"]) {
		return pyrepr.Str(ctxMap["channel_id"])
	}
	for _, raw := range asList(ctxMap["entities"]) {
		entity, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch value := entity["value"].(type) {
		case map[string]any:
			if pyrepr.Truthy(value["channel_id"]) {
				return pyrepr.Str(value["channel_id"])
			}
		case string:
			if strings.HasSuffix(strOrEmpty(entity["type"]), "channel_id") {
				return value
			}
		}
	}
	return ""
}

func extractAssistantThreadMetadata(event, body map[string]any) map[string]string {
	assistantThread := asMap(event["assistant_thread"])
	context := assistantThread["context"]
	if !pyrepr.Truthy(context) {
		context = firstTruthy(event, "app_context", "context")
	}
	ctxMap := asMap(context)
	channelID := firstTruthyValue(assistantThread["channel_id"], event["channel"], ctxMap["channel_id"])
	threadTS := firstTruthyValue(assistantThread["thread_ts"], firstTruthy(event, "thread_ts", "message_ts"))
	userID := firstTruthyValue(assistantThread["user_id"], event["user"], ctxMap["user_id"])
	teamID := eventTeamID(event, body)
	if teamID == "" {
		teamID = strOrEmpty(assistantThread["team_id"])
	}
	return map[string]string{
		"channel_id": strOrEmpty(channelID), "thread_ts": strOrEmpty(threadTS),
		"user_id": strOrEmpty(userID), "team_id": teamID,
		"context_channel_id": contextChannelID(context),
	}
}

func firstTruthyValue(values ...any) any {
	for _, v := range values {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	return nil
}

// Standard markdown converted to Slack mrkdwn with tables fenced, code protected and broadcasts escaped
func formatSlackMrkdwn(content string) string {
	if content == "" {
		return content
	}
	content = wrapMarkdownTables(content)
	var keys []string
	values := map[string]string{}
	ph := func(value string) string {
		key := fmt.Sprintf("\x00SL%d\x00", len(keys))
		keys = append(keys, key)
		values[key] = value
		return key
	}
	text := replaceAll(specialMentionRe, content, func(m reMatch) string { return strings.Replace(m.groups[0], "<", "&lt;", 1) })
	type pass struct {
		re *regexp2.Regexp
		fn func(m reMatch, runes []rune) string
	}
	passes := []*pass{
		{formatFenceRe, func(m reMatch, runes []rune) string {
			block := m.groups[0]
			if m.start == 0 || runes[m.start-1] == '\n' {
				block = replaceFirst(fenceLangRe, block, func(fm reMatch) string { return "```" + fm.groups[1] })
			}
			return ph(block)
		}},
		{formatCodeRe, func(m reMatch, _ []rune) string { return ph(m.groups[0]) }},
		{formatLinkRe, func(m reMatch, _ []rune) string {
			u := pystr.Strip(m.groups[2])
			if strings.HasPrefix(u, "<") && strings.HasSuffix(u, ">") {
				u = pystr.Strip(u[1 : len(u)-1])
			}
			return ph("<" + u + "|" + m.groups[1] + ">")
		}},
		{formatEntityRe, func(m reMatch, _ []rune) string { return ph(m.groups[1]) }},
		{formatQuoteRe, func(m reMatch, _ []rune) string { return ph(m.groups[0]) }},
		nil,
		{formatHeaderRe, func(m reMatch, _ []rune) string {
			inner := replaceAll(headerBoldRe, pystr.Strip(m.groups[1]), func(bm reMatch) string { return bm.groups[1] })
			return ph("*" + inner + "*")
		}},
		{formatBoldItalRe, func(m reMatch, _ []rune) string { return ph("*_" + m.groups[1] + "_*") }},
		{formatBoldRe, func(m reMatch, _ []rune) string {
			inner := m.groups[1]
			zw := ""
			if inner != "" {
				r := []rune(inner)
				last := r[len(r)-1]
				if !(pystr.IsAlnum(last) || last == '_') {
					zw = "​"
				}
			}
			return ph("*" + inner + zw + "*")
		}},
		{formatItalicRe, func(m reMatch, _ []rune) string { return ph("_" + m.groups[1] + "_") }},
		{formatStrikeRe, func(m reMatch, _ []rune) string { return ph("~" + m.groups[1] + "~") }},
	}
	for _, step := range passes {
		if step == nil {
			text = formatEscapeAmpRe.ReplaceAllStringFunc(text, func(s string) string { return slackHTMLEntities[s[1:len(s)-1]] })
			text = strings.ReplaceAll(text, "&", "&amp;")
			text = strings.ReplaceAll(text, "<", "&lt;")
			text = strings.ReplaceAll(text, ">", "&gt;")
			continue
		}
		runes := []rune(text)
		text = replaceAll(step.re, text, func(m reMatch) string { return step.fn(m, runes) })
	}
	for i := len(keys) - 1; i >= 0; i-- {
		text = strings.ReplaceAll(text, keys[i], values[keys[i]])
	}
	return text
}

func (e *httpStatusError) Error() string {
	kind := map[int]string{1: "Informational response", 3: "Redirect response", 4: "Client error", 5: "Server error"}[e.StatusCode/100]
	if kind == "" {
		kind = "Invalid status code"
	}
	msg := fmt.Sprintf("%s '%d %s' for url '%s'\n", kind, e.StatusCode, http.StatusText(e.StatusCode), e.URL)
	if e.StatusCode/100 == 3 && e.Location != "" {
		msg += fmt.Sprintf("Redirect location: '%s'\n", e.Location)
	}
	return msg + fmt.Sprintf("For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d", e.StatusCode)
}
