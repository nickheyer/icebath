package teams

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Attachment kinds in message type precedence, documents winning over images, video and audio
var mediaKindPrecedence = []struct {
	kind string
	typ  gateway.MessageType
}{{"document", gateway.MessageTypeDocument}, {"image", gateway.MessageTypePhoto}, {"video", gateway.MessageTypeVideo},
	{"audio", gateway.MessageTypeAudio}}

// Card action ids to approval choices
var approvalChoices = map[string]string{"approve_once": "once", "approve_session": "session", "approve_always": "always", "deny": "deny"}

// Approval choice to the catalog key of the resolved card footer
var approvalLabelKeys = map[string]string{"once": "platform.teams.approval.resolved_once",
	"session": "platform.teams.approval.resolved_session", "always": "platform.teams.approval.resolved_always",
	"deny": "platform.teams.approval.resolved_deny"}

// Rendered at tags Teams prepends for mentions
var atTagRe = pyre.MustCompile(`<at>[^<]*</at>\s*`)

// Object field of a decoded activity, nil when absent or not an object
func obj(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

// String field of a decoded activity, empty when absent or not a string
func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// Pyjson object of a JSON answer body
func jsonBody(status int, body any) (*gateway.WebResponse, error) {
	text, err := pyjson.Dumps(body)
	if err != nil {
		return nil, err
	}
	return gateway.WebTextResponse(status, text, "application/json", nil), nil
}

// Authenticates an inbound activity and routes messages and card actions, answering with the invoke response
func (a *TeamsAdapter) handleActivityRequest(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	raw, err := request.Read()
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	status, body := a.handleActivity(context.WithoutCancel(ctx), pyjson.Plain(decoded), request.Header.Get("Authorization"))
	if body == nil {
		return gateway.NewWebResponse(status, nil), nil
	}
	return jsonBody(status, body)
}

// Token check, activity validation and dispatch of one activity body, a 500 for any failure past auth
func (a *TeamsAdapter) handleActivity(ctx context.Context, decoded any, authorization string) (status int, body any) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error(pyrepr.Str(rec))
			status, body = 500, pyjson.ObjectOf("error", "Internal server error")
		}
	}()
	activity, ok := decoded.(map[string]any)
	if !ok {
		slog.Error(fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded)))
		return 500, pyjson.ObjectOf("error", "Internal server error")
	}
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app == nil {
		return 401, pyjson.ObjectOf("error", "Authentication not configured")
	}
	if !strings.HasPrefix(authorization, "Bearer ") {
		slog.Warn(fmt.Sprintf("inbound activity rejected (type=%s, id=%s): missing or malformed Authorization header (responding 401)",
			pyrepr.Str(activity["type"]), pyrepr.Str(activity["id"])))
		return 401, pyjson.ObjectOf("error", "Unauthorized")
	}
	serviceURL, _ := activity["serviceUrl"].(string)
	tokenServiceURL, err := app.validator.validate(strings.TrimPrefix(authorization, "Bearer "), serviceURL)
	if err != nil {
		slog.Warn(fmt.Sprintf("JWT token validation failed: %v", err))
		return 401, pyjson.ObjectOf("error", "Unauthorized")
	}
	if obj(activity, "from") == nil || obj(activity, "conversation") == nil || obj(activity, "recipient") == nil {
		slog.Error("activity requires from, conversation and recipient accounts")
		return 500, pyjson.ObjectOf("error", "Internal server error")
	}
	if serviceURL == "" {
		activity["serviceUrl"] = tokenServiceURL
	}
	status, body, err = a.routeActivity(ctx, activity)
	if err != nil {
		slog.Error(err.Error())
		return 500, pyjson.ObjectOf("error", "Internal server error")
	}
	return status, body
}

// Message activities to the message handler and adaptive card invokes to the card action handler
func (a *TeamsAdapter) routeActivity(ctx context.Context, activity map[string]any) (int, any, error) {
	switch str(activity, "type") {
	case "message":
		return 200, nil, a.onMessage(ctx, activity)
	case "invoke":
		if str(activity, "name") == "adaptiveCard/action" {
			if obj(obj(activity, "value"), "action") == nil {
				return 0, nil, fmt.Errorf("adaptive card invoke activity requires value.action")
			}
			status, body := a.onCardAction(ctx, activity)
			return status, body, nil
		}
	}
	return 200, nil, nil
}

// Conversation reference an inbound activity answers through
func refOf(activity map[string]any) conversationRef {
	return conversationRef{serviceURL: str(activity, "serviceUrl"), bot: obj(activity, "recipient"), conversation: obj(activity, "conversation")}
}

// Drops self echoes, duplicates and gated group posts, caches attachments and hands the message to the gateway
func (a *TeamsAdapter) onMessage(ctx context.Context, activity map[string]any) error {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	botIDs := map[string]bool{}
	if app != nil && app.id != "" {
		botIDs[app.id] = true
	}
	if rid := str(obj(activity, "recipient"), "id"); rid != "" {
		botIDs[rid] = true
	}
	for id := range botIDs {
		if !strings.HasPrefix(id, "28:") {
			botIDs["28:"+id] = true
		}
	}
	from := obj(activity, "from")
	if botIDs[str(from, "id")] {
		return nil
	}
	msgID := str(activity, "id")
	if msgID != "" && a.dedup.IsDuplicate(msgID) {
		return nil
	}
	conv := obj(activity, "conversation")
	convID := str(conv, "id")
	if convID != "" {
		a.mu.Lock()
		a.convRefs[convID] = refOf(activity)
		a.mu.Unlock()
	}
	text := str(activity, "text")
	if a.requireMention && str(conv, "conversationType") != "personal" {
		a.mu.Lock()
		repliedToOurs := false
		replyTo := str(activity, "replyToId")
		for _, id := range a.sentIDs {
			if id == replyTo && replyTo != "" {
				repliedToOurs = true
				break
			}
		}
		a.mu.Unlock()
		if !activityMentionsBot(activity, botIDs, text) && !repliedToOurs {
			slog.Debug(fmt.Sprintf("[teams] Dropping non-personal message without a bot mention (chat=%s, msg=%s)", convID, msgID))
			return nil
		}
	}
	if strings.Contains(text, "<at>") {
		stripped, err := atTagRe.Replace(text, "", -1, -1)
		if err != nil {
			return err
		}
		text = pystr.Strip(stripped)
	}
	userID := str(from, "aadObjectId")
	if userID == "" {
		userID = str(from, "id")
	}
	chatName, userName := str(conv, "name"), str(from, "name")
	chatType, ok := chatTypes[str(conv, "conversationType")]
	if !ok {
		chatType = "dm"
	}
	guildID := str(conv, "tenantId")
	if guildID == "" {
		guildID = a.tenantID
	}
	source := a.BuildSource(ctx, convID, gateway.SourceFields{ChatName: &chatName, ChatType: chatType, UserID: &userID,
		UserName: &userName, GuildID: &guildID, MessageID: &msgID})
	var mediaURLs, mediaTypes []string
	kinds := map[string]bool{}
	attachments, _ := activity["attachments"].([]any)
	for _, raw := range attachments {
		att, _ := raw.(map[string]any)
		if att == nil {
			continue
		}
		if path, mediaType, kind, ok := a.cacheAttachment(ctx, att); ok {
			mediaURLs, mediaTypes = append(mediaURLs, path), append(mediaTypes, mediaType)
			kinds[kind] = true
		}
	}
	msgType := gateway.MessageTypeText
	for _, p := range mediaKindPrecedence {
		if kinds[p.kind] {
			msgType = p.typ
			break
		}
	}
	ev := gateway.NewMessageEvent(text)
	ev.Source, ev.MessageType = source, msgType
	if msgID != "" {
		ev.MessageID = &msgID
	}
	ev.MediaURLs, ev.MediaTypes = mediaURLs, mediaTypes
	return a.HandleMessage(ctx, ev)
}

// Whether a mention entity points at the bot, the rendered at tag deciding when no mention entity exists
func activityMentionsBot(activity map[string]any, botIDs map[string]bool, text string) bool {
	entities, _ := activity["entities"].([]any)
	var mentions []map[string]any
	for _, raw := range entities {
		if e, ok := raw.(map[string]any); ok && str(e, "type") == "mention" {
			mentions = append(mentions, e)
		}
	}
	if len(mentions) == 0 {
		return strings.Contains(text, "<at>")
	}
	for _, e := range mentions {
		id := ""
		if m := obj(e, "mentioned"); m != nil {
			id = pyrepr.Str(m["id"])
			if _, has := m["id"]; !has {
				id = ""
			}
		}
		if botIDs[id] {
			return true
		}
	}
	return false
}

// Downloads and caches one inbound attachment, false for body mirrors, cards and failures
func (a *TeamsAdapter) cacheAttachment(ctx context.Context, att map[string]any) (string, string, string, bool) {
	contentURL := str(att, "contentUrl")
	contentType := strings.ToLower(str(att, "contentType"))
	attName := str(att, "name")
	if ((contentType == "text/html" || contentType == "text/plain") && contentURL == "") || strings.HasPrefix(contentType, "application/vnd.microsoft.card") {
		return "", "", "", false
	}
	if contentType == "application/vnd.microsoft.teams.file.download.info" {
		content, _ := att["content"].(map[string]any)
		downloadURL := str(content, "downloadUrl")
		if downloadURL == "" {
			downloadURL = str(content, "download_url")
		}
		fileType := str(content, "fileType")
		if fileType == "" {
			fileType = str(content, "file_type")
		}
		fileType = strings.TrimLeft(fileType, ".")
		if downloadURL == "" {
			return "", "", "", false
		}
		filename := attName
		if filename == "" {
			filename = "document"
			if fileType != "" {
				filename = "document." + fileType
			}
		}
		data, err := a.fetchAttachmentBytes(ctx, downloadURL, 30*time.Second)
		if err == nil {
			var cached *gateway.CachedMedia
			if cached, err = gateway.CacheMediaBytesAsync(ctx, data, filename, "", ""); err == nil {
				if cached == nil {
					slog.Warn(fmt.Sprintf("[teams] Unsupported document type for attachment '%s', skipping", filename))
					return "", "", "", false
				}
				return cached.Path, cached.MediaType, cached.Kind, true
			}
		}
		slog.Warn(fmt.Sprintf("[teams] Failed to cache file attachment '%s': %v", filename, err))
		return "", "", "", false
	}
	if contentURL != "" && strings.HasPrefix(contentType, "image/") {
		path, mediaType, err := a.cacheImageAttachment(ctx, contentURL, contentType, attName)
		if err != nil {
			slog.Warn(fmt.Sprintf("[teams] Failed to cache image attachment: %v", err))
			return "", "", "", false
		}
		return path, mediaType, "image", path != ""
	}
	if contentURL != "" {
		data, err := a.fetchAttachmentBytes(ctx, contentURL, 30*time.Second)
		if err == nil {
			var cached *gateway.CachedMedia
			if cached, err = gateway.CacheMediaBytesAsync(ctx, data, attName, contentType, ""); err == nil {
				if cached == nil {
					return "", "", "", false
				}
				return cached.Path, cached.MediaType, cached.Kind, true
			}
		}
		label := attName
		if label == "" {
			label = contentURL
		}
		slog.Warn(fmt.Sprintf("[teams] Failed to cache attachment '%s' (%s): %v", label, contentType, err))
	}
	return "", "", "", false
}

// Image attachment cached through the connector token for Bot Framework hosts, the plain image cache otherwise
func (a *TeamsAdapter) cacheImageAttachment(ctx context.Context, contentURL, contentType, attName string) (string, string, error) {
	if isBotframeworkAttachmentURL(contentURL) {
		data, err := a.fetchAttachmentBytes(ctx, contentURL, 30*time.Second)
		if err != nil {
			return "", "", err
		}
		parts := strings.Split(contentType, "/")
		ext := strings.Split(parts[len(parts)-1], ";")[0]
		if ext == "" {
			ext = "png"
		}
		filename := attName
		if filename == "" {
			filename = "image." + ext
		}
		cached, err := gateway.CacheMediaBytesAsync(ctx, data, filename, contentType, "")
		if err != nil {
			return "", "", err
		}
		if cached == nil {
			label := attName
			if label == "" {
				label = contentURL
			}
			slog.Warn(fmt.Sprintf("[teams] Bot Framework attachment '%s' returned data that failed image validation, skipping", label))
			return "", "", nil
		}
		return cached.Path, cached.MediaType, nil
	}
	path, err := gateway.CacheImageFromURL(ctx, contentURL, ".jpg", 2)
	if err != nil {
		return "", "", err
	}
	return path, contentType, nil
}

// Adaptive card body of an approval prompt, empty command or reason omitting their blocks unless always
func approvalBody(ctx context.Context, cmd, desc string, always bool) []any {
	body := []any{}
	if cmd != "" || always {
		body = append(body, textBlock("⚠️ "+i18n.T(ctx, "gateway.exec_approval.header", "", nil), "Bolder", false))
		body = append(body, textBlock("```\n"+cmd+"\n```", "", false))
	}
	if desc != "" || always {
		body = append(body, textBlock(i18n.T(ctx, "gateway.exec_approval.reason_label", "", nil)+": "+desc, "", true))
	}
	return body
}

// Resolves an approval button click for an authorized clicker and answers with the resolved card
func (a *TeamsAdapter) onCardAction(ctx context.Context, activity map[string]any) (int, any) {
	data := obj(obj(obj(activity, "value"), "action"), "data")
	if data == nil {
		data = map[string]any{}
	}
	hermesAction, sessionKey := str(data, "hermes_action"), str(data, "session_key")
	if hermesAction == "" || sessionKey == "" {
		return invokeMessage(i18n.T(ctx, "platform.teams.approval.unknown_action", "", nil))
	}
	if denied := cardActionDenied(ctx, obj(activity, "from")); denied != "" {
		return invokeMessage(denied)
	}
	choice, ok := approvalChoices[hermesAction]
	if !ok {
		return invokeMessage(i18n.T(ctx, "platform.teams.approval.unknown_action", "", nil))
	}
	a.mu.Lock()
	preview, found := a.approvalPreviews[str(data, "prompt_id")]
	a.mu.Unlock()
	if !found {
		preview = [2]string{str(data, "cmd"), str(data, "desc")}
	}
	if !tools.HasBlockingApproval(sessionKey) {
		body := approvalBody(ctx, preview[0], preview[1], false)
		return invokeCard(append(body, textBlock(i18n.T(ctx, "platform.shared.approval_expired", "", nil), "", false)))
	}
	tools.ResolveGatewayApproval(sessionKey, choice, false, "", "")
	body := approvalBody(ctx, preview[0], preview[1], false)
	body = append(body, textBlock(i18n.T(ctx, approvalLabelKeys[choice], "", nil), "Bolder", false))
	return invokeCard(body)
}

// Denial text for an approval click unless all users are allowed or the clicker is allowlisted
func cardActionDenied(ctx context.Context, from map[string]any) string {
	allowAll, _ := gateway.GetScopedSecret(ctx, "TEAMS_ALLOW_ALL_USERS", "", false)
	switch pystr.Lower(pystr.Strip(allowAll)) {
	case "1", "true", "yes":
		return ""
	}
	allowedCSV, _ := gateway.GetScopedSecret(ctx, "TEAMS_ALLOWED_USERS", "", false)
	allowedCSV = pystr.Strip(allowedCSV)
	if allowedCSV == "" {
		slog.Warn("[teams] card action rejected: TEAMS_ALLOWED_USERS not configured and TEAMS_ALLOW_ALL_USERS not set — default deny")
		return i18n.T(ctx, "platform.teams.approval.requires_allowlist", "", nil)
	}
	clicker := str(from, "aadObjectId")
	if clicker == "" {
		clicker = str(from, "id")
	}
	allowed := map[string]bool{}
	for _, uid := range strings.Split(allowedCSV, ",") {
		if s := pystr.Strip(uid); s != "" {
			allowed[s] = true
		}
	}
	if !allowed["*"] && !allowed[clicker] {
		slog.Warn(fmt.Sprintf("[teams] Unauthorized card action by %s — ignoring", clicker))
		return i18n.T(ctx, "platform.shared.not_authorized", "", nil)
	}
	return ""
}
