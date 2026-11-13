package mattermost

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	maxPostLength = 4000
	maxPostFiles  = 5
)

// Channel type codes of the API, private channels treated as groups
var channelTypes = map[string]string{"D": "dm", "G": "group", "P": "group", "O": "channel"}

var markdownImageRE = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// Adapter relays Mattermost channels and direct messages over the v4 REST API and its websocket event stream
type Adapter struct {
	*gateway.BasePlatformAdapter

	baseURL   string
	token     string
	replyMode string
	dedup     *gateway.MessageDeduplicator

	mu          sync.Mutex
	client      *http.Client
	botUserID   string
	botUsername string
	stream      *wsStream
}

// Adapter traits of Mattermost, chunked posts and a serial websocket reader
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.SplitsLongMessages = true
	t.SerialReceiveLoop = true
	t.MaxMessageLength = maxPostLength
	return t
}

// Adapter reading the server URL, token and reply mode from the config and the scoped env
func NewAdapter(ctx context.Context, config *gateway.PlatformConfig) (*Adapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &Adapter{dedup: gateway.NewMessageDeduplicator(gateway.DefaultDedupMaxSize, gateway.DefaultDedupTTLSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformMattermost, a)
	a.Traits = adapterTraits()
	var err error
	if a.baseURL, a.token, err = urlAndToken(ctx, config); err != nil {
		return nil, err
	}
	mode := textOf(config.Extra["reply_mode"])
	if mode == "" {
		if mode, err = gateway.GetScopedSecret(ctx, "MATTERMOST_REPLY_MODE", "off", false); err != nil {
			return nil, err
		}
	}
	a.replyMode = strings.ToLower(strings.TrimSpace(mode))
	return a, nil
}

// REST client of the live connection, nil while disconnected
func (a *Adapter) httpClient() *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// Bot user id and username resolved at connect
func (a *Adapter) botIdentity() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botUserID, a.botUsername
}

// Decoded JSON object of a v4 call on the live client, failures logged with method and path
func (a *Adapter) call(ctx context.Context, method, path string, payload any) (map[string]any, error) {
	client := a.httpClient()
	if client == nil {
		return nil, &apiError{body: "not connected"}
	}
	data, err := callJSON(ctx, client, a.baseURL, a.token, method, path, payload)
	if err != nil {
		status, body := failureOf(err)
		if status == 0 {
			slog.Error(fmt.Sprintf("MM API %s %s network error: %s", method, path, body))
		} else {
			slog.Error(fmt.Sprintf("MM API %s %s → %d: %s", method, path, status, clip(body, 200)))
		}
	}
	return data, err
}

// Authenticates the token through users me and starts the websocket listener
func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.baseURL == "" || a.token == "" {
		slog.Error("Mattermost: URL or token not configured")
		return false, nil
	}
	a.stopStream()
	client := gateway.TrustEnvHTTPClient(ctx, 0)
	a.mu.Lock()
	a.client = client
	a.mu.Unlock()
	me, err := a.call(ctx, http.MethodGet, "users/me", nil)
	if err != nil || textOf(me["id"]) == "" {
		slog.Error("Mattermost: failed to authenticate — check MATTERMOST_TOKEN and MATTERMOST_URL")
		a.closeClient()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if status, _ := failureOf(err); status == http.StatusUnauthorized || status == http.StatusForbidden {
			a.SetFatalError(ctx, "mattermost_auth_error", fmt.Sprintf("Mattermost rejected the bot token (HTTP %d). Check MATTERMOST_TOKEN "+
				"and the bot account in the System Console.", status), false)
			return false, nil
		}
		detail := "Mattermost users/me returned no user id"
		if err != nil {
			detail = "Mattermost users/me failed: " + err.Error()
		}
		a.SetFatalError(ctx, "mattermost_connect_failed", detail, true)
		return false, nil
	}
	a.mu.Lock()
	a.botUserID, a.botUsername = textOf(me["id"]), textOf(me["username"])
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("Mattermost: authenticated as @%s (%s) on %s", textOf(me["username"]), textOf(me["id"]), a.baseURL))
	a.startStream(ctx)
	a.MarkConnected(ctx, "")
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Drops the REST client and its pooled connections
func (a *Adapter) closeClient() {
	a.mu.Lock()
	client := a.client
	a.client = nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

// Stops the websocket listener, closes the REST client and marks the adapter disconnected
func (a *Adapter) Disconnect(ctx context.Context) error {
	a.stopStream()
	a.closeClient()
	a.MarkDisconnected(ctx)
	slog.Info("Mattermost: disconnected")
	return nil
}

// Thread root of a post, the post itself when it is a root or the lookup fails
func (a *Adapter) resolveRootID(ctx context.Context, postID string) string {
	if postID == "" {
		return postID
	}
	data, err := a.call(ctx, http.MethodGet, "posts/"+postID, nil)
	if err == nil {
		if root := textOf(data["root_id"]); root != "" {
			return root
		}
	}
	return postID
}

// Whether a failed post names an invalid or missing thread root
func brokenThreadRoot(err error) bool {
	status, body := failureOf(err)
	body = strings.ToLower(body)
	if (status != 400 && status != 404) || body == "" {
		return false
	}
	return containsAny(body, "root_id", "rootid", "root id", "thread", "post") &&
		containsAny(body, "invalid", "not found", "does not exist", "missing")
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// Posts once, reposting flat with a visible notice when a notify post hits a broken thread root
func (a *Adapter) postPreservingThread(ctx context.Context, chatID string, payload, metadata map[string]any) (map[string]any, error) {
	data, err := a.call(ctx, http.MethodPost, "posts", payload)
	if err == nil {
		return data, nil
	}
	if _, threaded := payload["root_id"]; !threaded || !truthy(metadata["notify"]) || !brokenThreadRoot(err) {
		return nil, err
	}
	flat := map[string]any{}
	for k, v := range payload {
		if k != "root_id" {
			flat[k] = v
		}
	}
	body := textOf(flat["message"])
	visible := strings.TrimSpace("⚠️ Mattermost thread delivery failed; posting final reply in channel.\n\n" + body)
	if text := a.WarningText(ctx, visible, &body, "", chatID, metadata); text != nil {
		flat["message"] = *text
	}
	slog.Warn("Mattermost: falling back to flat channel delivery for notify-worthy post in " + chatID)
	return a.call(ctx, http.MethodPost, "posts", flat)
}

// Thread root a reply nests under, the reply target or the metadata thread, empty outside thread mode
func (a *Adapter) replyRoot(ctx context.Context, replyTo string, metadata map[string]any) string {
	if a.replyMode != "thread" {
		return ""
	}
	candidate := replyTo
	if candidate == "" {
		candidate = textOf(metadata["thread_id"])
	}
	if candidate == "" {
		candidate = textOf(metadata["root_id"])
	}
	if candidate == "" {
		return ""
	}
	return a.resolveRootID(ctx, candidate)
}

// Mentions disabled post with optional files, nested under the thread root when one is given
func (a *Adapter) postMessage(ctx context.Context, chatID, message, root string, metadata map[string]any, fileIDs []string) (map[string]any, error) {
	payload := mentionsDisabled(map[string]any{"channel_id": chatID, "message": message})
	if fileIDs != nil {
		payload["file_ids"] = fileIDs
	}
	if root != "" {
		payload["root_id"] = root
	}
	return a.postPreservingThread(ctx, chatID, payload, metadata)
}

// Send result of a created post, the failure text carrying the server answer
func postResult(data map[string]any, err error, failure string) gateway.SendResult {
	if err == nil && textOf(data["id"]) != "" {
		return gateway.SendResult{Success: true, MessageID: textOf(data["id"]), RawResponse: data}
	}
	if err != nil {
		failure += ": " + err.Error()
	}
	return gateway.SendResult{Success: false, Error: failure, Retryable: gateway.IsRetryableErrorText(failure)}
}

// Posts the content in chunks of the post cap, replies nested under the thread root in thread mode
func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if content == "" {
		return gateway.SendResult{Success: true}, nil
	}
	if a.httpClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	root := a.replyRoot(ctx, replyTo, metadata)
	result, err := gateway.SendChunks(ctx, gateway.TruncateMessage(a.FormatMessage(content), maxPostLength, nil),
		func(ctx context.Context, chunk string) (*gateway.SendResult, error) {
			data, err := a.postMessage(ctx, chatID, chunk, root, metadata, nil)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			r := postResult(data, err, "Failed to create post")
			return &r, nil
		})
	if err != nil {
		return gateway.SendResult{}, err
	}
	return *result, nil
}

// Name and kind of a channel, the id and channel kind when the lookup fails
func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	data, err := a.call(ctx, http.MethodGet, "channels/"+chatID, nil)
	if err != nil {
		return map[string]any{"name": chatID, "type": "channel"}, nil
	}
	name := textOf(data["display_name"])
	if name == "" {
		name = textOf(data["name"])
	}
	if name == "" {
		name = chatID
	}
	kind := channelTypes[textOf(data["type"])]
	if kind == "" {
		kind = "channel"
	}
	return map[string]any{"name": name, "type": kind}, nil
}

// Typing indicator in the channel, inside the reply thread in thread mode
func (a *Adapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	botID, _ := a.botIdentity()
	if botID == "" {
		return nil
	}
	payload := map[string]any{"channel_id": chatID}
	if a.replyMode == "thread" {
		if parent := textOf(metadata["thread_id"]); parent != "" {
			payload["parent_id"] = parent
		}
	}
	_, _ = a.call(ctx, http.MethodPost, "users/"+botID+"/typing", payload)
	return nil
}

// Patches the post text with mentions disabled
func (a *Adapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	payload := mentionsDisabled(map[string]any{"message": a.FormatMessage(content)})
	data, err := a.call(ctx, http.MethodPut, "posts/"+messageID+"/patch", payload)
	return postResult(data, err, "Failed to edit post"), nil
}

// Markdown kept as is with image tags reduced to their URL so the server renders a preview
func (a *Adapter) FormatMessage(content string) string {
	return markdownImageRE.ReplaceAllString(content, "$2")
}

// Downloads an image URL and attaches it, the caption and URL as text when that fails
func (a *Adapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sendURLAsFile(ctx, chatID, imageURL, caption, replyTo, "image", metadata)
}

// Uploads a local image as a post attachment
func (a *Adapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, imagePath, caption, replyTo, "", metadata)
}

// Uploads a local document under its display name as a post attachment
func (a *Adapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, filePath, caption, replyTo, fileName, metadata)
}

// Uploads a local audio file as a post attachment
func (a *Adapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, audioPath, caption, replyTo, "", metadata)
}

// Uploads a local video as a post attachment
func (a *Adapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, videoPath, caption, replyTo, "", metadata)
}

// Last path segment of a URL without its query, the fallback when it is empty
func urlFilename(rawURL, fallback string) string {
	name := rawURL[strings.LastIndex(rawURL, "/")+1:]
	if i := strings.IndexByte(name, '?'); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return fallback
	}
	return name
}

// Waits the duration, false when the context ends first
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Body and media type of a safe URL fetched through the SSRF guarded client
func fetchURL(ctx context.Context, client *http.Client, rawURL string) (int, []byte, string, error) {
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil, "", nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, "", err
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return resp.StatusCode, data, mediaType, nil
}

// Downloads a URL with two retries on server errors and uploads it, posting the URL as text on failure
func (a *Adapter) sendURLAsFile(ctx context.Context, chatID, rawURL, caption, replyTo, kind string, metadata map[string]any) (gateway.SendResult, error) {
	fallback := func() (gateway.SendResult, error) {
		return a.Send(ctx, chatID, strings.TrimSpace(caption+"\n"+rawURL), replyTo, metadata)
	}
	env := gateway.URLSafetyEnv(ctx)
	if !tools.IsSafeURL(env, rawURL) {
		slog.Warn("Mattermost: blocked unsafe URL (SSRF protection)")
		return fallback()
	}
	client := tools.NewSSRFSafeClient(env, requestTimeout, true)
	var data []byte
	contentType := ""
	for attempt := 0; ; attempt++ {
		status, body, ct, err := fetchURL(ctx, client, rawURL)
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		switch {
		case err != nil && attempt == 2:
			slog.Warn(fmt.Sprintf("Mattermost: failed to download %s after %d attempts: %v", gateway.SafeURLForLog(rawURL, 80), attempt+1, err))
			return fallback()
		case err != nil:
		case (status >= 500 || status == 429) && attempt < 2:
			slog.Debug(fmt.Sprintf("Mattermost download retry %d/2 for %s (status %d)", attempt+1, gateway.SafeURLForLog(rawURL, 80), status))
		case status >= 400:
			return fallback()
		default:
			data, contentType = body, ct
		}
		if data != nil {
			break
		}
		if !sleepCtx(ctx, time.Duration(1500*(attempt+1))*time.Millisecond) {
			return gateway.SendResult{}, ctx.Err()
		}
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	rest := a.httpClient()
	if rest == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	ids, err := uploadFile(ctx, rest, a.baseURL, a.token, chatID, data, urlFilename(rawURL, kind+".png"), contentType)
	if err != nil || len(ids) == 0 {
		if err != nil {
			slog.Error("MM file upload → " + err.Error())
		}
		return fallback()
	}
	return a.postWithFile(ctx, chatID, ids[0], caption, replyTo, metadata), nil
}

// Post carrying one uploaded file with the caption as its text
func (a *Adapter) postWithFile(ctx context.Context, chatID, fileID, caption, replyTo string, metadata map[string]any) gateway.SendResult {
	data, err := a.postMessage(ctx, chatID, caption, a.replyRoot(ctx, replyTo, metadata), metadata, []string{fileID})
	return postResult(data, err, "Failed to post with file")
}

// Uploads a local file under its display name and attaches it to a post
func (a *Adapter) sendLocalFile(ctx context.Context, chatID, filePath, caption, replyTo, fileName string, metadata map[string]any) (gateway.SendResult, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		slog.Warn(fmt.Sprintf("Mattermost: local file unreadable: %s: %v", filePath, err))
		return gateway.SendResult{Success: false, Error: "File not found: " + filePath}, nil
	}
	client := a.httpClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	name := fileName
	if name == "" {
		name = filepath.Base(filePath)
	}
	ids, err := uploadFile(ctx, client, a.baseURL, a.token, chatID, data, name, guessMime(name, "application/octet-stream"))
	if err != nil || len(ids) == 0 {
		msg := "File upload failed"
		if err != nil {
			slog.Error("MM file upload → " + err.Error())
			msg += ": " + err.Error()
		}
		return gateway.SendResult{Success: false, Error: msg}, nil
	}
	return a.postWithFile(ctx, chatID, ids[0], caption, replyTo, metadata), nil
}

// Bytes, name and media type of a batch image from a file or safe remote URL, false to skip
func (a *Adapter) loadBatchImage(ctx context.Context, imageURL string, index int) ([]byte, string, string, bool) {
	if strings.HasPrefix(imageURL, "file://") {
		local, err := url.PathUnescape(imageURL[7:])
		if err != nil {
			local = imageURL[7:]
		}
		data, err := os.ReadFile(local)
		if err != nil {
			slog.Warn("Mattermost: skipping missing image " + local)
			return nil, "", "", false
		}
		name := filepath.Base(local)
		return data, name, guessMime(name, "image/png"), true
	}
	env := gateway.URLSafetyEnv(ctx)
	if !tools.IsSafeURL(env, imageURL) {
		slog.Warn("Mattermost: blocked unsafe image URL in batch")
		return nil, "", "", false
	}
	status, data, contentType, err := fetchURL(ctx, tools.NewSSRFSafeClient(env, requestTimeout, true), imageURL)
	if err != nil {
		slog.Warn(fmt.Sprintf("Mattermost: download failed for %s: %v", gateway.SafeURLForLog(imageURL, 80), err))
		return nil, "", "", false
	}
	if status >= 400 {
		slog.Warn(fmt.Sprintf("Mattermost: failed to download image (HTTP %d): %s", status, gateway.SafeURLForLog(imageURL, 80)))
		return nil, "", "", false
	}
	if contentType == "" {
		contentType = "image/png"
	}
	return data, urlFilename(imageURL, fmt.Sprintf("image_%d.png", index)), contentType, true
}

// Sends images as posts of up to five attachments, a failed post retried image by image
func (a *Adapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	var chunks [][]gateway.ImageRef
	for i := 0; i < len(images); i += maxPostFiles {
		chunks = append(chunks, images[i:min(i+maxPostFiles, len(images))])
	}
	delivered := false
	for idx, chunk := range chunks {
		if humanDelay > 0 && idx > 0 && !sleepCtx(ctx, time.Duration(humanDelay*float64(time.Second))) {
			return gateway.SendResult{}, ctx.Err()
		}
		ok, err := a.sendImageChunk(ctx, chatID, chunk, metadata, idx, len(chunks))
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		if ok {
			delivered = true
			continue
		}
		if err == nil {
			continue
		}
		slog.Warn(fmt.Sprintf("Mattermost: multi-image send failed (chunk %d/%d), falling back: %v", idx+1, len(chunks), err))
		fallback, ferr := a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, chunk, metadata, humanDelay)
		if ferr != nil {
			return gateway.SendResult{}, ferr
		}
		delivered = delivered || fallback.Success
	}
	if delivered {
		return gateway.SendResult{Success: true}, nil
	}
	return gateway.SendResult{Success: false, Error: "all images failed to send"}, nil
}

// Uploads one chunk of images into a single post, an error when the post or an upload fails
func (a *Adapter) sendImageChunk(ctx context.Context, chatID string, chunk []gateway.ImageRef, metadata map[string]any, idx, total int) (bool, error) {
	client := a.httpClient()
	if client == nil {
		return false, &apiError{body: "not connected"}
	}
	var fileIDs, captions []string
	for _, image := range chunk {
		if image.Alt != "" {
			captions = append(captions, image.Alt)
		}
		data, name, contentType, ok := a.loadBatchImage(ctx, image.URL, len(fileIDs))
		if !ok {
			continue
		}
		ids, err := uploadFile(ctx, client, a.baseURL, a.token, chatID, data, name, contentType)
		if err != nil {
			return false, err
		}
		if len(ids) > 0 {
			fileIDs = append(fileIDs, ids[0])
		}
	}
	if len(fileIDs) == 0 {
		return false, nil
	}
	slog.Info(fmt.Sprintf("Mattermost: sending %d image(s) as single post (chunk %d/%d)", len(fileIDs), idx+1, total))
	data, err := a.postMessage(ctx, chatID, strings.Join(captions, "\n"), a.replyRoot(ctx, "", metadata), metadata, fileIDs)
	if result := postResult(data, err, "multi-image post failed"); !result.Success {
		return false, fmt.Errorf("%v", result.Error)
	}
	return true, nil
}
