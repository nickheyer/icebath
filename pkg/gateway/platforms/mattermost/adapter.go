package mattermost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyaiohttpws"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	// Practical cap for readable posts, well under the server default of 16383
	maxPostLength      = 4000
	reconnectBaseDelay = 2.0
	reconnectMaxDelay  = 60.0
	reconnectJitter    = 0.2
	postWithFileError  = "Failed to post with file"
)

var (
	// Channel type codes of the Mattermost API, private channels reading as groups
	channelTypeMap = map[string]string{"D": "dm", "G": "group", "P": "group", "O": "channel"}
	// Mime prefix and message type pairs, the first matching pair naming the event type
	mediaMsgTypes = []struct {
		prefix string
		kind   gateway.MessageType
	}{{"image/", gateway.MessageTypePhoto}, {"audio/", gateway.MessageTypeVoice}}
	// Default extension of cached inbound media by mime prefix, in lookup order
	inboundCacheExt = [][2]string{{"image/", ".png"}, {"audio/", ".ogg"}}

	imageMarkdownRE = pyre.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	httpSchemeRE    = pyre.MustCompile(`^http`)
)

// AttributeError an attribute read raises on a value of another type
func attributeError(v any, attr string) error {
	return &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)}
}

// dict get of a decoded JSON value, the AttributeError a non dict raises
func dictGet(v any, key string, def any) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, attributeError(v, "get")
	}
	if val, ok := m[key]; ok {
		return val, nil
	}
	return def, nil
}

// Post payload with the property that stops Mattermost from firing mentions
func withMentionsDisabled(payload *pyjson.Object) *pyjson.Object {
	props := pyjson.NewObject()
	if existing, _ := payload.Get("props"); existing != nil {
		switch p := existing.(type) {
		case *pyjson.Object:
			props = p.Copy()
		case map[string]any:
			for k, v := range p {
				props.Set(k, v)
			}
		}
	}
	props.Set("disable_mentions", true)
	payload.Set("props", props)
	return payload
}

// Stripped non blank channel ids of a list or a comma separated value
func channelIDSet(raw any) map[string]bool {
	out := map[string]bool{}
	items, ok := raw.([]any)
	if !ok {
		for _, part := range strings.Split(pyrepr.Str(raw), ",") {
			items = append(items, part)
		}
	}
	for _, c := range items {
		if s := pystr.Strip(pyrepr.Str(c)); s != "" {
			out[s] = true
		}
	}
	return out
}

// Send result of a created or patched post, the given error when the reply carries no id
func postResult(data any, failure string) gateway.SendResult {
	m, _ := data.(map[string]any)
	id, ok := m["id"]
	if !pyrepr.Truthy(data) || !ok {
		return gateway.SendResult{Success: false, Error: failure}
	}
	return gateway.SendResult{Success: true, MessageID: id}
}

// Last path segment of a URL without its query, the fallback when it is empty
func urlFilename(rawURL, fallback string) string {
	tail := rawURL
	if i := strings.LastIndex(rawURL, "/"); i >= 0 {
		tail = rawURL[i+1:]
	}
	name, _, _ := strings.Cut(tail, "?")
	if name == "" {
		return fallback
	}
	return name
}

// Server URL and token, the config first and the scoped env after
func urlAndToken(ctx context.Context, config *gateway.PlatformConfig) (any, any, error) {
	var serverURL, token any
	if config != nil {
		serverURL, token = config.Extra["url"], config.Token
	}
	if !pyrepr.Truthy(serverURL) {
		scoped, err := gateway.GetScopedSecret(ctx, "MATTERMOST_URL", "", false)
		if err != nil {
			return nil, nil, err
		}
		serverURL = scoped
	}
	if !pyrepr.Truthy(token) {
		scoped, err := gateway.GetScopedSecret(ctx, "MATTERMOST_TOKEN", "", false)
		if err != nil {
			return nil, nil, err
		}
		token = scoped
	}
	return serverURL, token, nil
}

// The HTTP and WebSocket clients are compiled in
func checkMattermostRequirements() bool { return true }

// Whether the config or the env carries both a token and a server URL
func validateMattermostConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	serverURL, token, err := urlAndToken(ctx, config)
	if err != nil {
		return false, err
	}
	tokenText, ok := token.(string)
	if !ok {
		return false, attributeError(token, "strip")
	}
	if pystr.Strip(tokenText) == "" {
		slog.Debug("Mattermost: MATTERMOST_TOKEN not set")
		return false, nil
	}
	urlText, ok := serverURL.(string)
	if !ok {
		return false, attributeError(serverURL, "strip")
	}
	if pystr.Strip(urlText) == "" {
		slog.Warn("Mattermost: MATTERMOST_URL not set")
		return false, nil
	}
	return true, nil
}

// Gateway adapter for a self hosted or cloud Mattermost server over REST v4 and its WebSocket
type MattermostAdapter struct {
	*gateway.BasePlatformAdapter

	baseURL   string
	token     any
	replyMode string
	dedup     *gateway.MessageDeduplicator

	mu             sync.Mutex
	botUserID      any
	botUsername    any
	session        *http.Client
	sessionClosed  bool
	ws             *pyaiohttpws.Conn
	wsTask         *gateway.AdapterTask
	reconnectTask  *gateway.AdapterTask
	closing        bool
	lastPostStatus *int
	lastPostError  string
}

// Class attributes the Mattermost adapter declares over the base defaults
func mattermostAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.SplitsLongMessages = true
	traits.SerialReceiveLoop = true
	return traits
}

// Adapter for a platform config reading the server URL, token and reply mode from config then env
func NewMattermostAdapter(ctx context.Context, config *gateway.PlatformConfig) (*MattermostAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &MattermostAdapter{botUserID: "", botUsername: "", dedup: gateway.NewMessageDeduplicator(gateway.DefaultDedupMaxSize,
		gateway.DefaultDedupTTLSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformMattermost, a)
	a.Traits = mattermostAdapterTraits()
	serverURL, token, err := urlAndToken(ctx, config)
	if err != nil {
		return nil, err
	}
	urlText, ok := serverURL.(string)
	if !ok {
		return nil, attributeError(serverURL, "rstrip")
	}
	a.baseURL, a.token = pystr.RstripChars(urlText, "/"), token
	if config.Extra == nil {
		return nil, attributeError(nil, "get")
	}
	replyMode := config.Extra["reply_mode"]
	if !pyrepr.Truthy(replyMode) {
		scoped, err := gateway.GetScopedSecret(ctx, "MATTERMOST_REPLY_MODE", "off", false)
		if err != nil {
			return nil, err
		}
		replyMode = scoped
	}
	modeText, ok := replyMode.(string)
	if !ok {
		return nil, attributeError(replyMode, "lower")
	}
	a.replyMode = pystr.Lower(modeText)
	return a, nil
}

// Bearer authorization header of the bot token
func (a *MattermostAdapter) authHeader() map[string]string {
	return map[string]string{"Authorization": "Bearer " + pyrepr.Str(a.token)}
}

// Authorization and JSON content type headers of an API call
func (a *MattermostAdapter) headers() map[string]string {
	h := a.authHeader()
	h["Content-Type"] = "application/json"
	return h
}

// One REST v4 call returning the decoded reply, an empty dict on a refused or failed call, POST recording its status and error
func (a *MattermostAdapter) api(ctx context.Context, method, path string, payload *pyjson.Object) (any, error) {
	if strings.Contains(path, "..") {
		slog.Error(fmt.Sprintf("MM API path traversal blocked: %s", path))
		return map[string]any{}, nil
	}
	target := a.baseURL + "/api/v4/" + pystr.LstripChars(path, "/")
	isPost := method == "POST"
	if isPost {
		a.mu.Lock()
		a.lastPostStatus, a.lastPostError = nil, ""
		a.mu.Unlock()
	}
	var body []byte
	if payload != nil {
		encoded, err := pyjson.Dumps(payload)
		if err != nil {
			return nil, err
		}
		body = []byte(encoded)
	}
	failed := func(err error) (any, error) {
		var ce *clientError
		if !errors.As(err, &ce) {
			return nil, err
		}
		if isPost {
			a.mu.Lock()
			a.lastPostError = ce.text
			a.mu.Unlock()
		}
		slog.Error(fmt.Sprintf("MM API %s %s network error: %s", method, path, ce.text))
		return map[string]any{}, nil
	}
	resp, err := a.request(ctx, method, target, a.headers(), "", body, 30*time.Second)
	if err != nil {
		return failed(err)
	}
	if isPost {
		status := resp.status
		a.mu.Lock()
		a.lastPostStatus = &status
		a.mu.Unlock()
	}
	if resp.status >= 400 {
		text := resp.text()
		if isPost {
			a.mu.Lock()
			a.lastPostError = text
			a.mu.Unlock()
		}
		slog.Error(fmt.Sprintf("MM API %s %s → %d: %s", method, path, resp.status, pystr.Slice(text, 0, 200)))
		return map[string]any{}, nil
	}
	data, err := resp.json()
	if err != nil {
		return failed(err)
	}
	return data, nil
}

func (a *MattermostAdapter) apiGet(ctx context.Context, path string) (any, error) {
	return a.api(ctx, "GET", path, nil)
}

func (a *MattermostAdapter) apiPost(ctx context.Context, path string, payload *pyjson.Object) (any, error) {
	return a.api(ctx, "POST", path, payload)
}

// Whether the last failed POST clearly names an invalid or missing thread root
func (a *MattermostAdapter) lastPostFailureIsBrokenThreadRoot() bool {
	a.mu.Lock()
	status, body := a.lastPostStatus, pystr.Lower(a.lastPostError)
	a.mu.Unlock()
	if status == nil || (*status != 400 && *status != 404) || body == "" {
		return false
	}
	names := func(markers ...string) bool {
		for _, m := range markers {
			if strings.Contains(body, m) {
				return true
			}
		}
		return false
	}
	return names("root_id", "rootid", "root id", "thread", "post") && names("invalid", "not found", "does not exist", "missing")
}

// Posts once, a notify worthy threaded post whose root is broken going out flat with a warning
func (a *MattermostAdapter) postPreservingThread(ctx context.Context, chatID string, payload *pyjson.Object, metadata map[string]any) (any, error) {
	data, err := a.apiPost(ctx, "posts", payload)
	if err != nil {
		return nil, err
	}
	_, hasRoot := payload.Get("root_id")
	if pyrepr.Truthy(data) || !hasRoot || !pyrepr.Truthy(metadata["notify"]) || !a.lastPostFailureIsBrokenThreadRoot() {
		return data, nil
	}
	flat := pyjson.NewObject()
	for _, k := range payload.Keys {
		if k != "root_id" {
			flat.Set(k, payload.Vals[k])
		}
	}
	body := ""
	if message, _ := flat.Get("message"); pyrepr.Truthy(message) {
		body = pyrepr.Str(message)
	}
	visible := pystr.Strip("⚠️ Mattermost thread delivery failed; posting final reply in channel.\n\n" + body)
	var message any
	if text := a.WarningText(ctx, visible, &body, "", "", nil); text != nil {
		message = *text
	}
	flat.Set("message", message)
	slog.Warn(fmt.Sprintf("Mattermost: falling back to flat channel delivery for notify-worthy post in %s", chatID))
	return a.apiPost(ctx, "posts", flat)
}

// Builds a mentions disabled post, threaded under the resolved root in thread mode, and posts it
func (a *MattermostAdapter) postMessage(ctx context.Context, chatID, message, replyTo string, metadata map[string]any, fileIDs []any) (any, error) {
	base := pyjson.ObjectOf("channel_id", chatID, "message", message)
	if fileIDs != nil {
		base.Set("file_ids", fileIDs)
	}
	payload := withMentionsDisabled(base)
	if a.replyMode == "thread" {
		var candidate any = replyTo
		if replyTo == "" {
			candidate = metadata["thread_id"]
			if !pyrepr.Truthy(candidate) {
				candidate = metadata["root_id"]
			}
		}
		if pyrepr.Truthy(candidate) {
			root, err := a.resolveRootID(ctx, pyrepr.Str(candidate))
			if err != nil {
				return nil, err
			}
			payload.Set("root_id", root)
		}
	}
	return a.postPreservingThread(ctx, chatID, payload, metadata)
}

// Posts the caption with one uploaded file attached
func (a *MattermostAdapter) postWithFile(ctx context.Context, chatID string, fileID any, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	data, err := a.postMessage(ctx, chatID, caption, replyTo, metadata, []any{fileID})
	if err != nil {
		return gateway.SendResult{}, err
	}
	return postResult(data, postWithFileError), nil
}

// Uploads file bytes to a channel and returns the file id, nil when the server refuses or names none
func (a *MattermostAdapter) uploadFile(ctx context.Context, channelID string, fileData []byte, filename, contentType string) (any, error) {
	form, formType, err := uploadForm(channelID, fileData, filename, contentType)
	if err != nil {
		return nil, err
	}
	resp, err := a.request(ctx, "POST", a.baseURL+"/api/v4/files", a.authHeader(), formType, form, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.status >= 400 {
		slog.Error(fmt.Sprintf("MM file upload → %d: %s", resp.status, pystr.Slice(resp.text(), 0, 200)))
		return nil, nil
	}
	data, err := resp.json()
	if err != nil {
		return nil, err
	}
	infos, err := dictGet(data, "file_infos", []any{})
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(infos) {
		return nil, nil
	}
	list, ok := infos.([]any)
	if !ok {
		return nil, &gateway.PyError{Kind: "KeyError", Msg: "0"}
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(list[0]))}
	}
	id, ok := first["id"]
	if !ok {
		return nil, &gateway.PyError{Kind: "KeyError", Msg: "'id'"}
	}
	return id, nil
}

// Authenticates the bot against the REST API and starts the WebSocket listener
func (a *MattermostAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.baseURL == "" || !pyrepr.Truthy(a.token) {
		slog.Error("Mattermost: URL or token not configured")
		return false, nil
	}
	a.mu.Lock()
	a.session, a.sessionClosed = gateway.TrustEnvHTTPClient(ctx, 0), false
	a.closing = false
	a.mu.Unlock()
	me, err := a.apiGet(ctx, "users/me")
	if err != nil {
		return false, err
	}
	meMap, _ := me.(map[string]any)
	id, hasID := meMap["id"]
	if !pyrepr.Truthy(me) || !hasID {
		slog.Error("Mattermost: failed to authenticate — check MATTERMOST_TOKEN and MATTERMOST_URL")
		a.closeSession()
		return false, nil
	}
	var username any = ""
	if v, ok := meMap["username"]; ok {
		username = v
	}
	a.mu.Lock()
	a.botUserID, a.botUsername = id, username
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("Mattermost: authenticated as @%s (%s) on %s", pyrepr.Str(username), pyrepr.Str(id), a.baseURL))
	task := gateway.SpawnAdapterTask(ctx, a.wsLoop)
	a.mu.Lock()
	a.wsTask = task
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Marks the HTTP session closed and drops its idle connections
func (a *MattermostAdapter) closeSession() {
	a.mu.Lock()
	session := a.session
	a.sessionClosed = true
	a.mu.Unlock()
	if session != nil {
		session.CloseIdleConnections()
	}
}

// Stops the listener, closes the WebSocket and the HTTP session
func (a *MattermostAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	a.closing = true
	wsTask, reconnectTask := a.wsTask, a.reconnectTask
	a.mu.Unlock()
	gateway.CancelTask(ctx, wsTask)
	if reconnectTask != nil && !reconnectTask.Done() {
		reconnectTask.Cancel()
	}
	a.mu.Lock()
	ws := a.ws
	a.ws = nil
	session, closed := a.session, a.sessionClosed
	a.mu.Unlock()
	if ws != nil {
		ws.Close(ctx)
	}
	if session != nil && !closed {
		a.closeSession()
	}
	slog.Info("Mattermost: disconnected")
	return nil
}

// Thread root of a post, the post itself when it is a root or cannot be read
func (a *MattermostAdapter) resolveRootID(ctx context.Context, postID string) (any, error) {
	if postID == "" {
		return postID, nil
	}
	data, err := a.apiGet(ctx, "posts/"+postID)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(data) {
		return postID, nil
	}
	root, err := dictGet(data, "root_id", nil)
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(root) {
		return root, nil
	}
	return postID, nil
}

// Posts the formatted content chunk by chunk, stopping at the first failed chunk
func (a *MattermostAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if content == "" {
		return gateway.SendResult{Success: true}, nil
	}
	result := gateway.SendResult{Success: true}
	for _, chunk := range gateway.TruncateMessage(a.FormatMessage(content), maxPostLength, a.MessageLenFn()) {
		data, err := a.postMessage(ctx, chatID, chunk, replyTo, metadata, nil)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if result = postResult(data, "Failed to create post"); !result.Success {
			break
		}
	}
	return result, nil
}

// Display name and chat type of a channel, the id as a channel when it cannot be read
func (a *MattermostAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	data, err := a.apiGet(ctx, "channels/"+chatID)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(data) {
		return map[string]any{"name": chatID, "type": "channel"}, nil
	}
	name, err := dictGet(data, "display_name", nil)
	if err != nil {
		return nil, err
	}
	m := data.(map[string]any)
	if !pyrepr.Truthy(name) {
		name = m["name"]
	}
	if !pyrepr.Truthy(name) {
		name = chatID
	}
	var code any = "O"
	if v, ok := m["type"]; ok {
		code = v
	}
	chatType := "channel"
	if s, ok := code.(string); ok {
		if mapped, ok := channelTypeMap[s]; ok {
			chatType = mapped
		}
	}
	return map[string]any{"name": name, "type": chatType}, nil
}

// Posts the typing indicator of the bot user to the channel
func (a *MattermostAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.mu.Lock()
	botUserID := a.botUserID
	a.mu.Unlock()
	_, err := a.apiPost(ctx, "users/"+pyrepr.Str(botUserID)+"/typing", pyjson.ObjectOf("channel_id", chatID))
	return err
}

// Patches a post with the formatted content, mentions disabled
func (a *MattermostAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	payload := withMentionsDisabled(pyjson.ObjectOf("message", a.FormatMessage(content)))
	data, err := a.api(ctx, "PUT", "posts/"+messageID+"/patch", payload)
	if err != nil {
		return gateway.SendResult{}, err
	}
	return postResult(data, "Failed to edit post"), nil
}

func (a *MattermostAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sendURLAsFile(ctx, chatID, imageURL, caption, replyTo, "image", metadata)
}

func (a *MattermostAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, imagePath, caption, replyTo, "", metadata)
}

func (a *MattermostAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, filePath, caption, replyTo, fileName, metadata)
}

func (a *MattermostAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, audioPath, caption, replyTo, "", metadata)
}

func (a *MattermostAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, videoPath, caption, replyTo, "", metadata)
}

// Markdown kept as is, image links reduced to the bare URL Mattermost previews inline
func (a *MattermostAdapter) FormatMessage(content string) string {
	out, err := imageMarkdownRE.Replace(content, "$2", -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Downloads a URL and posts it as a file attachment, the caption and URL as text when anything fails
func (a *MattermostAdapter) sendURLAsFile(ctx context.Context, chatID, rawURL, caption, replyTo, kind string, metadata map[string]any) (gateway.SendResult, error) {
	fallback := func() (gateway.SendResult, error) {
		return a.Send(ctx, chatID, pystr.Strip(caption+"\n"+rawURL), replyTo, metadata)
	}
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), rawURL) {
		slog.Warn("Mattermost: blocked unsafe URL (SSRF protection)")
		return fallback()
	}
	var fileData []byte
	var contentType string
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := a.request(ctx, "GET", rawURL, nil, "", nil, 30*time.Second)
		var ce *clientError
		var te *timeoutError
		switch {
		case errors.As(err, &ce) || errors.As(err, &te):
			if attempt == 2 {
				slog.Warn(fmt.Sprintf("Mattermost: failed to download %s after %d attempts: %s", rawURL, attempt+1, err.Error()))
				return fallback()
			}
		case err != nil:
			return gateway.SendResult{}, err
		case (resp.status >= 500 || resp.status == 429) && attempt < 2:
			slog.Debug(fmt.Sprintf("Mattermost download retry %d/2 for %s (status %d)", attempt+1, pystr.Slice(rawURL, 0, 80), resp.status))
		case resp.status >= 400:
			return fallback()
		default:
			fileData, contentType = resp.body, resp.contentType()
		}
		if contentType != "" {
			break
		}
		timer := time.NewTimer(time.Duration(1.5 * float64(attempt+1) * float64(time.Second)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return gateway.SendResult{}, ctx.Err()
		}
	}
	fileID, err := a.uploadFile(ctx, chatID, fileData, urlFilename(rawURL, kind+".png"), contentType)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if pyrepr.Truthy(fileID) {
		return a.postWithFile(ctx, chatID, fileID, caption, replyTo, metadata)
	}
	return fallback()
}

// Mime type guessed from a file name, the fallback when none is known
func guessType(name, fallback string) string {
	if typ, _, err := pymimetypes.GuessType(name, true); err == nil && typ != "" {
		return typ
	}
	return fallback
}

// Uploads a local file and attaches it to a post, a missing file skipped as a success without an id
func (a *MattermostAdapter) sendLocalFile(ctx context.Context, chatID, filePath, caption, replyTo, fileName string, metadata map[string]any) (gateway.SendResult, error) {
	if _, err := os.Stat(filePath); err != nil {
		slog.Warn(fmt.Sprintf("Mattermost: local file not found, skipping: %s", filePath))
		return gateway.SendResult{Success: true}, nil
	}
	fname := fileName
	if fname == "" {
		fname = pypath.PathName(filePath)
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return gateway.SendResult{}, err
	}
	fileID, err := a.uploadFile(ctx, chatID, data, fname, guessType(fname, "application/octet-stream"))
	if err != nil {
		return gateway.SendResult{}, err
	}
	if !pyrepr.Truthy(fileID) {
		return gateway.SendResult{Success: false, Error: "File upload failed"}, nil
	}
	return a.postWithFile(ctx, chatID, fileID, caption, replyTo, metadata)
}

// Bytes, file name and content type of one batch image
type batchImage struct {
	data        []byte
	filename    string
	contentType string
}

// Reads a file URL or a safe remote image for a batch post, nil when it is missing, unsafe or fails to download
func (a *MattermostAdapter) loadBatchImage(ctx context.Context, imageURL string, index int) (*batchImage, error) {
	if strings.HasPrefix(imageURL, "file://") {
		localPath := pyurl.Unquote(imageURL[7:])
		if _, err := os.Stat(localPath); err != nil {
			slog.Warn(fmt.Sprintf("Mattermost: skipping missing image %s", localPath))
			return nil, nil
		}
		data, err := os.ReadFile(localPath)
		if err != nil {
			return nil, err
		}
		name := pypath.PathName(localPath)
		return &batchImage{data: data, filename: name, contentType: guessType(name, "image/png")}, nil
	}
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), imageURL) {
		slog.Warn("Mattermost: blocked unsafe image URL in batch")
		return nil, nil
	}
	resp, err := a.request(ctx, "GET", imageURL, nil, "", nil, 30*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("Mattermost: download failed for %s: %s", pystr.Slice(imageURL, 0, 80), err.Error()))
		return nil, nil
	}
	if resp.status >= 400 {
		slog.Warn(fmt.Sprintf("Mattermost: failed to download image (HTTP %d): %s", resp.status, pystr.Slice(imageURL, 0, 80)))
		return nil, nil
	}
	contentType := "image/png"
	if resp.header.Get("Content-Type") != "" {
		contentType = resp.contentType()
	}
	return &batchImage{data: resp.body, filename: urlFilename(imageURL, fmt.Sprintf("image_%d.png", index)), contentType: contentType}, nil
}

// Sends images five to a post, a chunk whose post fails going through the per image loop of the base
func (a *MattermostAdapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	var chunks [][]gateway.ImageRef
	for i := 0; i < len(images); i += 5 {
		chunks = append(chunks, images[i:min(i+5, len(images))])
	}
	delivered := false
	for chunkIdx, chunk := range chunks {
		if humanDelay > 0 && chunkIdx > 0 {
			timer := time.NewTimer(time.Duration(humanDelay * float64(time.Second)))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return gateway.SendResult{}, ctx.Err()
			}
		}
		posted, empty, err := a.postImageChunk(ctx, chatID, chunk, metadata, chunkIdx, len(chunks))
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		if empty {
			continue
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Mattermost: multi-image send failed (chunk %d/%d), falling back: %s", chunkIdx+1, len(chunks),
				pyrepr.ExceptionMessage(err)), "error", err)
		} else if posted {
			delivered = true
			continue
		} else {
			slog.Warn("Mattermost: multi-image post failed, falling back")
		}
		fallback, err := a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, chunk, metadata, humanDelay)
		if err != nil {
			return gateway.SendResult{}, err
		}
		delivered = delivered || fallback.Success
	}
	if delivered {
		return gateway.SendResult{Success: true}, nil
	}
	return gateway.SendResult{Success: false, Error: "all images failed to send"}, nil
}

// Uploads one chunk of images and posts them together, empty when no image of the chunk uploaded
func (a *MattermostAdapter) postImageChunk(ctx context.Context, chatID string, chunk []gateway.ImageRef, metadata map[string]any,
	chunkIdx, total int) (posted, empty bool, err error) {
	fileIDs, captionParts := []any{}, []string{}
	for _, image := range chunk {
		if image.Alt != "" {
			captionParts = append(captionParts, image.Alt)
		}
		loaded, err := a.loadBatchImage(ctx, image.URL, len(fileIDs))
		if err != nil {
			return false, false, err
		}
		if loaded == nil {
			continue
		}
		fid, err := a.uploadFile(ctx, chatID, loaded.data, loaded.filename, loaded.contentType)
		if err != nil {
			return false, false, err
		}
		if pyrepr.Truthy(fid) {
			fileIDs = append(fileIDs, fid)
		}
	}
	if len(fileIDs) == 0 {
		return false, true, nil
	}
	slog.Info(fmt.Sprintf("Mattermost: sending %d image(s) as single post (chunk %d/%d)", len(fileIDs), chunkIdx+1, total))
	data, err := a.postMessage(ctx, chatID, strings.Join(captionParts, "\n"), "", metadata, fileIDs)
	if err != nil {
		return false, false, err
	}
	m, _ := data.(map[string]any)
	_, hasID := m["id"]
	return pyrepr.Truthy(data) && hasID, false, nil
}

// Whether the adapter is shutting down
func (a *MattermostAdapter) isClosing() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closing
}

// Keeps one WebSocket session alive, reconnecting with jittered backoff and escalating a rejected login as fatal
func (a *MattermostAdapter) wsLoop(ctx context.Context) error {
	delay := reconnectBaseDelay
	for !a.isClosing() {
		err := a.wsConnectAndListen(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			delay = reconnectBaseDelay
		} else {
			if a.isClosing() {
				return nil
			}
			var handshake *pyaiohttpws.HandshakeError
			if errors.As(err, &handshake) && (handshake.Status == 401 || handshake.Status == 403) {
				slog.Error(fmt.Sprintf("Mattermost WS auth failed (HTTP %d) — stopping reconnect", handshake.Status))
				a.SetFatalError(ctx, "mattermost_auth_error", fmt.Sprintf("Mattermost WebSocket authentication rejected (HTTP %d). "+
					"The bot token is invalid, revoked, or lacks permission — check MATTERMOST_TOKEN and the bot account in "+
					"the System Console.", handshake.Status), false)
				return a.NotifyFatalError(ctx)
			}
			slog.Warn(fmt.Sprintf("Mattermost WS error: %s — reconnecting in %.0fs", pyrepr.ExceptionMessage(err), delay))
		}
		if a.isClosing() {
			return nil
		}
		timer := time.NewTimer(time.Duration((delay + delay*reconnectJitter*rand.Float64()) * float64(time.Second)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		delay = min(delay*2, reconnectMaxDelay)
	}
	return nil
}

// One WebSocket session, connecting, authenticating and handling events until it closes
func (a *MattermostAdapter) wsConnectAndListen(ctx context.Context) error {
	scheme, err := httpSchemeRE.Replace(a.baseURL, "ws", -1, -1)
	if err != nil {
		return err
	}
	wsURL := scheme + "/api/v4/websocket"
	slog.Info(fmt.Sprintf("Mattermost: connecting to %s", wsURL))
	a.mu.Lock()
	closed := a.session == nil || a.sessionClosed
	a.mu.Unlock()
	if closed {
		return &gateway.PyError{Kind: "RuntimeError", Msg: "Session is closed"}
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, err := pyaiohttpws.Connect(dctx, wsURL, pyaiohttpws.Options{Heartbeat: 30 * time.Second, TrustEnv: gateway.GatewayTrustEnv(ctx)})
	timedOut := dctx.Err() != nil && ctx.Err() == nil
	cancel()
	if err != nil {
		if timedOut {
			return &timeoutError{}
		}
		return err
	}
	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	challenge, err := pyjson.Dumps(pyjson.ObjectOf("seq", 1, "action", "authentication_challenge", "data",
		pyjson.ObjectOf("token", a.token)))
	if err != nil {
		return err
	}
	if err := ws.SendStr(challenge); err != nil {
		return err
	}
	slog.Info("Mattermost: WebSocket connected and authenticated")
	for {
		msg, err := ws.Receive(ctx)
		if err != nil {
			return err
		}
		switch msg.Type {
		case pyaiohttpws.Close, pyaiohttpws.Closing, pyaiohttpws.Closed:
			return nil
		}
		if a.isClosing() {
			return nil
		}
		switch msg.Type {
		case pyaiohttpws.Text, pyaiohttpws.Binary:
			var event any
			var perr error
			if text, ok := msg.Data.(string); ok {
				event, perr = pyjson.Loads(text)
			} else if raw, ok := msg.Data.([]byte); ok {
				event, perr = pyjson.LoadsBytes(raw)
			}
			if perr != nil {
				var decodeErr *pyjson.DecodeError
				if errors.As(perr, &decodeErr) {
					continue
				}
				return perr
			}
			if err := a.handleWSEvent(ctx, pyjson.Plain(event)); err != nil {
				return err
			}
		case pyaiohttpws.Error:
			slog.Info(fmt.Sprintf("Mattermost: WebSocket closed (%s)", msg.Type))
			return nil
		}
	}
}

// Mention gates a channel post, the cleaned text or false when the post is ignored
func (a *MattermostAdapter) applyChannelGating(ctx context.Context, channelID, messageText string) (string, bool, error) {
	extra := a.Config.Extra
	read := func(key, env string, def any) (any, error) { return gateway.ExtraOrSecret(ctx, extra, key, env, def, false) }
	allowedRaw, err := read("allowed_channels", "MATTERMOST_ALLOWED_CHANNELS", "")
	if err != nil {
		return "", false, err
	}
	if allowed := channelIDSet(allowedRaw); len(allowed) > 0 && !allowed[channelID] {
		slog.Debug(fmt.Sprintf("Mattermost: ignoring message in non-allowed channel: %s", channelID))
		return "", false, nil
	}
	requireRaw, err := read("require_mention", "MATTERMOST_REQUIRE_MENTION", "true")
	if err != nil {
		return "", false, err
	}
	requireMention := true
	switch pystr.Lower(pyrepr.Str(requireRaw)) {
	case "false", "0", "no":
		requireMention = false
	}
	freeRaw, err := read("free_response_channels", "MATTERMOST_FREE_RESPONSE_CHANNELS", "")
	if err != nil {
		return "", false, err
	}
	freeChannels := channelIDSet(freeRaw)
	a.mu.Lock()
	mentionPatterns := []string{"@" + pyrepr.Str(a.botUsername), "@" + pyrepr.Str(a.botUserID)}
	a.mu.Unlock()
	hasMention := false
	lowered := pystr.Lower(messageText)
	for _, pattern := range mentionPatterns {
		if strings.Contains(lowered, pystr.Lower(pattern)) {
			hasMention = true
			break
		}
	}
	if requireMention && !freeChannels[channelID] && !hasMention {
		slog.Debug(fmt.Sprintf("Mattermost: skipping non-DM message without @mention (channel=%s)", channelID))
		return "", false, nil
	}
	if hasMention {
		for _, pattern := range mentionPatterns {
			re, err := pyre.CompileIgnoreCase(regexp2.Escape(pattern))
			if err != nil {
				return "", false, err
			}
			stripped, err := re.Replace(messageText, "", -1, -1)
			if err != nil {
				return "", false, err
			}
			messageText = pystr.Strip(stripped)
		}
	}
	return messageText, true, nil
}

// Downloads every attachment into the media cache and returns the cached paths with their mime types
func (a *MattermostAdapter) downloadAttachments(ctx context.Context, fileIDs []any) ([]string, []string, error) {
	mediaURLs, mediaTypes := []string{}, []string{}
	for _, fid := range fileIDs {
		id := pyrepr.Str(fid)
		cached, mime, skip, err := a.downloadAttachment(ctx, id)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Mattermost: error downloading file %s: %s", id, pyrepr.ExceptionMessage(err)))
			continue
		}
		if skip {
			continue
		}
		mediaURLs, mediaTypes = append(mediaURLs, cached), append(mediaTypes, mime)
	}
	return mediaURLs, mediaTypes, nil
}

// Caches one attachment by its mime kind, skip set when the server refuses the download
func (a *MattermostAdapter) downloadAttachment(ctx context.Context, fid string) (cached, mime string, skip bool, err error) {
	fileInfo, err := a.apiGet(ctx, "files/"+fid+"/info")
	if err != nil {
		return "", "", false, err
	}
	nameValue, err := dictGet(fileInfo, "name", "file_"+fid)
	if err != nil {
		return "", "", false, err
	}
	mimeValue, _ := dictGet(fileInfo, "mime_type", "application/octet-stream")
	resp, err := a.request(ctx, "GET", a.baseURL+"/api/v4/files/"+fid, a.authHeader(), "", nil, 30*time.Second)
	if err != nil {
		return "", "", false, err
	}
	if resp.status >= 400 {
		slog.Warn(fmt.Sprintf("Mattermost: failed to download file %s: HTTP %d", fid, resp.status))
		return "", "", true, nil
	}
	mime, ok := mimeValue.(string)
	if !ok {
		return "", "", false, attributeError(mimeValue, "startswith")
	}
	fname, ok := nameValue.(string)
	if !ok {
		return "", "", false, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("expected str, bytes or os.PathLike object, not %s",
			pyrepr.TypeName(nameValue))}
	}
	for _, row := range inboundCacheExt {
		if !strings.HasPrefix(mime, row[0]) {
			continue
		}
		ext := pypath.PathSuffix(fname)
		if ext == "" {
			ext = row[1]
		}
		if row[0] == "image/" {
			cached, err = gateway.CacheImageFromBytesAsync(ctx, resp.body, ext)
		} else {
			cached, err = gateway.CacheAudioFromBytesAsync(ctx, resp.body, ext)
		}
		return cached, mime, false, err
	}
	cached, err = gateway.CacheDocumentFromBytesAsync(ctx, resp.body, fname)
	return cached, mime, false, err
}

// Turns a posted event into a message event, own posts, system posts and redeliveries dropped
func (a *MattermostAdapter) handleWSEvent(ctx context.Context, event any) error {
	kind, err := dictGet(event, "event", nil)
	if err != nil {
		return err
	}
	if !pyrepr.Equal(kind, "posted") {
		return nil
	}
	data := event.(map[string]any)["data"]
	if _, present := event.(map[string]any)["data"]; !present {
		data = map[string]any{}
	}
	rawPost, err := dictGet(data, "post", nil)
	if err != nil {
		return err
	}
	postText := ""
	if pyrepr.Truthy(rawPost) {
		text, ok := rawPost.(string)
		if !ok {
			return nil
		}
		postText = text
	}
	parsed, err := pyjson.Loads(postText)
	if err != nil {
		return nil
	}
	post := pyjson.Plain(parsed)
	senderValue, err := dictGet(post, "user_id", "")
	if err != nil {
		return err
	}
	postMap := post.(map[string]any)
	field := func(key string) any {
		if v, ok := postMap[key]; ok {
			return v
		}
		return ""
	}
	postIDValue := field("id")
	a.mu.Lock()
	botUserID := a.botUserID
	a.mu.Unlock()
	senderID, postID := pyrepr.Str(senderValue), pyrepr.Str(postIDValue)
	if pyrepr.Equal(senderValue, botUserID) || pyrepr.Truthy(postMap["type"]) || (pyrepr.Truthy(postIDValue) && a.dedup.IsDuplicate(postID)) {
		return nil
	}
	channelID := pyrepr.Str(field("channel_id"))
	dataMap := data.(map[string]any)
	var channelType any = "O"
	if v, ok := dataMap["channel_type"]; ok {
		channelType = v
	}
	isDM := pyrepr.Equal(channelType, "D")
	messageValue := field("message")
	messageText, ok := messageValue.(string)
	if !ok {
		return attributeError(messageValue, "lower")
	}
	if !isDM {
		gated, keep, err := a.applyChannelGating(ctx, channelID, messageText)
		if err != nil {
			return err
		}
		if !keep {
			return nil
		}
		messageText = gated
	}
	threadID := ""
	if root := postMap["root_id"]; pyrepr.Truthy(root) {
		threadID = pyrepr.Str(root)
	}
	if threadID == "" && a.replyMode == "thread" && !isDM && pyrepr.Truthy(postIDValue) {
		threadID = postID
	}
	if first := pystr.Slice(messageText, 0, 1); first != "" && pystr.Strip(first) == "" && strings.HasPrefix(pystr.Lstrip(messageText), "/") {
		messageText = pystr.Lstrip(messageText)
	}
	fileIDs, _ := postMap["file_ids"].([]any)
	if raw := postMap["file_ids"]; pyrepr.Truthy(raw) && fileIDs == nil {
		return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(raw))}
	}
	mediaURLs, mediaTypes, err := a.downloadAttachments(ctx, fileIDs)
	if err != nil {
		return err
	}
	msgType := gateway.MessageTypeText
	switch {
	case strings.HasPrefix(messageText, "/"):
		msgType = gateway.MessageTypeCommand
	case len(mediaTypes) > 0:
		msgType = gateway.MessageTypeDocument
	kinds:
		for _, row := range mediaMsgTypes {
			for _, m := range mediaTypes {
				if strings.HasPrefix(m, row.prefix) {
					msgType = row.kind
					break kinds
				}
			}
		}
	}
	chatType := "channel"
	if code, ok := channelType.(string); ok {
		if mapped, ok := channelTypeMap[code]; ok {
			chatType = mapped
		}
	}
	var senderNameValue any = ""
	if v, ok := dataMap["sender_name"]; ok {
		senderNameValue = v
	}
	senderName, ok := senderNameValue.(string)
	if !ok {
		return attributeError(senderNameValue, "lstrip")
	}
	userName := pystr.LstripChars(senderName, "@")
	if userName == "" {
		userName = senderID
	}
	source := a.BuildSource(ctx, channelID, gateway.SourceFields{ChatType: chatType, UserID: &senderID, UserName: &userName,
		ThreadID: &threadID, MessageID: &postID})
	msgEvent := gateway.NewMessageEvent(messageText)
	msgEvent.MessageType, msgEvent.Source, msgEvent.RawMessage = msgType, source, post
	msgEvent.MessageID = &postID
	msgEvent.MediaURLs, msgEvent.MediaTypes = mediaURLs, mediaTypes
	if prompt, found := gateway.ResolveChannelPrompt(a.Config.Extra, channelID, ""); found {
		msgEvent.ChannelPrompt = &prompt
	}
	return a.HandleMessage(ctx, msgEvent)
}
