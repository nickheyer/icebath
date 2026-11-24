package bluebubbles

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

const (
	webhookMaxBodyBytes = 1_048_576
	maxTextLength       = 4000
	guidCacheSize       = 500
	requestTimeout      = 30 * time.Second
)

// Icebath wake words for require_mention groups, iMessage having no stable bot mention identity
var defaultMentionPatterns = []string{`(?<![\w@])@?icebath\s+agent\b[,:\-]?`, `(?<![\w@])@?icebath\b[,:\-]?`}

// Closed historical image mime map, unlisted mimes falling back to .jpg
var imageExtOverrides = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"image/heic": ".jpg", "image/heif": ".jpg", "image/tiff": ".jpg",
}

// Closed historical audio mime map, unlisted mimes falling back to .mp3
var audioExtOverrides = map[string]string{
	"audio/mp3": ".mp3", "audio/mpeg": ".mp3", "audio/ogg": ".ogg", "audio/wav": ".wav",
	"audio/x-caf": ".mp3", "audio/mp4": ".m4a", "audio/aac": ".m4a",
}

// Webhook event types carrying user messages
var messageEvents = map[string]bool{"new-message": true, "message": true, "updated-message": true}

var localHosts = map[string]bool{"0.0.0.0": true, "127.0.0.1": true, "localhost": true, "::": true}

var (
	phoneRE            = regexutil.Lazy(regexp.MustCompile, `\+?\p{Nd}{7,15}`)
	emailRE            = regexutil.Lazy(regexp.MustCompile, `[\p{L}\p{M}\p{N}\p{Pc}\x{200C}\x{200D}.+-]+@[\p{L}\p{M}\p{N}\p{Pc}\x{200C}\x{200D}-]+\.[\p{L}\p{M}\p{N}\p{Pc}\x{200C}\x{200D}.]+`)
	paginationSuffixRE = regexutil.Lazy(regexp.MustCompile, `\s*\(\d+/\d+\)$`)
	addressRE          = regexutil.Lazy(regexp.MustCompile, `^\+\d+`)
	paragraphBreakRE   = regexutil.Lazy(regexp.MustCompile, `\n[\s\v\x{85}\p{Z}]*\n`)
	schemeRE           = regexutil.Lazy(regexp.MustCompile, `(?i)^https?://`)
)

// Media cache writers and URL fetcher the adapter stores inbound and outbound media through
var (
	cacheImageFromBytes    = gateway.CacheImageFromBytesAsync
	cacheAudioFromBytes    = gateway.CacheAudioFromBytesAsync
	cacheDocumentFromBytes = gateway.CacheDocumentFromBytesAsync
	cacheImageFromURL      = gateway.CacheImageFromURL
)

// Failure for a decoded or configured value of the wrong JSON kind
func kindError(want string, v any) error {
	return fmt.Errorf("expected %s, got %s", want, anyutil.Kind(v))
}

// Map view of a decoded JSON object, a kind error for any other value
func dictOf(v any) (map[string]any, error) {
	if m, ok := jsonutil.AsMap(v); ok {
		return m, nil
	}
	return nil, kindError("an object", v)
}

// Value under key or the default when the key is absent, dict get with a default
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Text of a decoded value, empty for falsy values
func textOf(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return anyutil.String(v)
}

// Text of a REST failure for a log line or a send result: the kind of error and the proxy's host when the proxy URL the client reads from the environment
// holds a secret the raw text could repeat, else the error text with the password every request URL carries masked, the error kind when that is empty
func safeError(err error, serverURL string) string {
	if shown := gateway.DescribeProxyError(err, gateway.EnvProxyForRedaction(serverURL)); shown != "" {
		return shown
	}
	if text := tools.RedactSensitiveTextWith(nil, err.Error(), tools.RedactOptions{Force: true, URLCredentials: true}); text != "" {
		return text
	}
	return errorutil.Kind(err)
}

// Optional string pointer, nil for the empty value
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Phone numbers and emails replaced by a redaction marker for log output
func redact(text string) string {
	return emailRE().ReplaceAllLiteralString(phoneRE().ReplaceAllLiteralString(text, "[REDACTED]"), "[REDACTED]")
}

// Server URL stripped, given an http scheme when it has none and without trailing slashes
func normalizeServerURL(raw any) (string, error) {
	if !anyutil.IsTrue(raw) {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", kindError("a string", raw)
	}
	value := strings.TrimSpace(s)
	if value != "" {
		if !schemeRE().MatchString(value) {
			value = "http://" + value
		}
	}
	return strings.TrimRight(value, "/"), nil
}

// Extension from a closed mime map, unlisted or blank mimes taking the fallback
func closedExt(mime string, overrides map[string]string, fallback string) string {
	if ext := gateway.ExtForMime(mime, overrides, false, false, fallback); ext != "" {
		return ext
	}
	return fallback
}

// Temporary message GUID from the current Unix time in seconds
func tempGUID() string {
	return "temp-" + strconv.FormatFloat(timeutil.Epoch(time.Now()), 'f', -1, 64)
}

// Plain ok acknowledgement for webhook events accepted but not processed
func okResponse() *gateway.WebResponse { return gateway.WebTextResponse(http.StatusOK, "ok", "", nil) }

// Class attributes of the BlueBubbles adapter
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.ServesProfilePrefix = true
	t.SupportsMessageEditing = false
	t.MaxMessageLength = maxTextLength
	t.SplitsLongMessages = true
	return t
}

// BlueBubblesAdapter bridges iMessage through a BlueBubbles server REST API and its webhook callbacks
type BlueBubblesAdapter struct {
	*gateway.BasePlatformAdapter

	serverURL        string
	password         string
	webhookHost      string
	webhookPort      int
	webhookPath      string
	sendReadReceipts bool
	requireMention   bool
	mentionPatterns  []*regexp2.Regexp

	mu                sync.Mutex
	client            *http.Client
	followClient      *http.Client
	runner            *gateway.HTTPAppRunner
	privateAPIEnabled bool
	helperConnected   bool
	guidOrder         *list.List
	guidCache         map[string]*list.Element

	apiPost            func(ctx context.Context, path string, payload any) (any, error)
	resolveChatGUID    func(ctx context.Context, target string) (string, error)
	downloadAttachment func(ctx context.Context, attGUID string, attMeta map[string]any) (string, error)
	handleMessage      func(ctx context.Context, event *gateway.MessageEvent) error
}

type guidEntry struct{ target, guid string }

// Adapter configured from the bluebubbles settings and the password credential of the connection block or the scoped env
func NewBlueBubblesAdapter(ctx context.Context, config *gateway.PlatformConfig) (*BlueBubblesAdapter, error) {
	a := &BlueBubblesAdapter{guidOrder: list.New(), guidCache: map[string]*list.Element{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformBlueBubbles, a)
	a.Traits = adapterTraits()
	a.apiPost = a.apiPostJSON
	a.resolveChatGUID = a.resolveChatGUIDFromServer
	a.downloadAttachment = a.downloadAttachmentToCache
	a.handleMessage = a.BasePlatformAdapter.HandleMessage
	extra := map[string]any{}
	if config != nil && config.Extra != nil {
		extra = config.Extra
	}
	sctx := gateway.SettingsContext(ctx)
	var err error
	if a.serverURL, err = normalizeServerURL(strings.TrimSpace(settings.BluebubblesServerURL.Get(sctx))); err != nil {
		return nil, err
	}
	if pw := extra["password"]; anyutil.IsTrue(pw) {
		a.password = textOf(pw)
	} else if a.password, err = gateway.GetScopedSecret(ctx, "BLUEBUBBLES_PASSWORD", "", false); err != nil {
		return nil, err
	}
	a.webhookHost = settings.BluebubblesWebhookHost.Get(sctx)
	a.webhookPort = settings.BluebubblesWebhookPort.Get(sctx)
	a.webhookPath = settings.BluebubblesWebhookPath.Get(sctx)
	if !strings.HasPrefix(a.webhookPath, "/") {
		a.webhookPath = "/" + a.webhookPath
	}
	a.sendReadReceipts = settings.BluebubblesSendReadReceipts.Get(sctx)
	a.requireMention = settings.BluebubblesRequireMention.Get(sctx)
	a.mentionPatterns = compileMentionPatterns(settings.BluebubblesMentionPatterns.Get(sctx))
	return a, nil
}

// Group mention wake words of the bluebubbles.mention_patterns items, the Icebath defaults while the setting is unset
func compileMentionPatterns(items []any) []*regexp2.Regexp {
	var raw any
	if items != nil {
		raw = items
	}
	return gateway.CompileMentionPatterns(raw, gateway.MentionPatternOptions{LogPrefix: "bluebubbles", Defaults: defaultMentionPatterns})
}

// Whether non empty text matches any group wake word
func (a *BlueBubblesAdapter) messageMatchesMentionPatterns(text string) bool {
	if text == "" {
		return false
	}
	for _, re := range a.mentionPatterns {
		if ok, _ := re.MatchString(text); ok {
			return true
		}
	}
	return false
}

// Text with only a leading wake word removed, the original text when nothing would remain
func (a *BlueBubblesAdapter) cleanMentionText(text string) string {
	stripped := strings.TrimLeftFunc(text, unicode.IsSpace)
	for _, re := range a.mentionPatterns {
		m, err := re.FindStringMatch(stripped)
		if err != nil || m == nil || m.RuneIndex != 0 {
			continue
		}
		rest := strings.TrimLeft(string([]rune(stripped)[m.RuneIndex+m.RuneLength:]), " ,:-")
		if rest == "" {
			return text
		}
		return rest
	}
	return text
}

// Server URL of an API path with the password query parameter appended
func (a *BlueBubblesAdapter) apiURL(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return a.serverURL + path + sep + "password=" + url.QueryEscape(a.password)
}

// REST client in use, nil while disconnected
func (a *BlueBubblesAdapter) httpClient() *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// One request with a timeout on each phase, the context error when the caller gave up
func doRequest(ctx context.Context, client *http.Client, method, rawURL string, body []byte, contentType string, timeout time.Duration) (*httputil.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := httputil.Do(client, req, httputil.EachPhase(timeout))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return resp, nil
}

// Status error of a response outside the 2xx range naming the URL as the caller shows it, nil for a success
func checkStatus(resp *httputil.Response, shownURL string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &httputil.StatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: shownURL, Location: resp.Header.Get("Location")}
}

// Authenticated REST call raising on HTTP errors and returning the decoded JSON body
func (a *BlueBubblesAdapter) apiJSON(ctx context.Context, method, path string, payload any) (any, error) {
	client := a.httpClient()
	if client == nil {
		return nil, errors.New("bluebubbles: REST client not connected")
	}
	var body []byte
	contentType := ""
	if payload != nil {
		text, err := jsonutil.Encode(payload)
		if err != nil {
			return nil, err
		}
		body, contentType = []byte(text), "application/json"
	}
	rawURL := a.apiURL(path)
	resp, err := doRequest(ctx, client, method, rawURL, body, contentType, requestTimeout)
	if err != nil {
		return nil, err
	}
	if err := checkStatus(resp, rawURL); err != nil {
		return nil, err
	}
	return resp.JSON()
}

// GET of an API path decoded as JSON
func (a *BlueBubblesAdapter) apiGet(ctx context.Context, path string) (any, error) {
	return a.apiJSON(ctx, http.MethodGet, path, nil)
}

// POST of a JSON payload to an API path decoded as JSON
func (a *BlueBubblesAdapter) apiPostJSON(ctx context.Context, path string, payload any) (any, error) {
	return a.apiJSON(ctx, http.MethodPost, path, payload)
}

// Message payload posted and its outcome wrapped as a send result
func (a *BlueBubblesAdapter) postMessage(ctx context.Context, path string, payload any) (gateway.SendResult, error) {
	res, err := a.apiPost(ctx, path, payload)
	if err == nil {
		var m map[string]any
		if m, err = dictOf(res); err == nil {
			data := m["data"]
			if !anyutil.IsTrue(data) {
				data = map[string]any{}
			}
			var dm map[string]any
			if dm, err = dictOf(data); err == nil {
				msgID := anyutil.String(anyutil.Or(anyutil.Or(dm["guid"], dm["messageGuid"]), "ok"))
				return gateway.SendResult{Success: true, MessageID: msgID, RawResponse: res}, nil
			}
		}
	}
	if ctx.Err() != nil {
		return gateway.SendResult{}, ctx.Err()
	}
	return gateway.SendResult{Success: false, Error: safeError(err, a.serverURL)}, nil
}

// iMessage tapbacks by emoji and by name
var tapbacks = map[string]string{"❤": "love", "👍": "like", "👎": "dislike", "😂": "laugh", "‼": "emphasize", "❗": "emphasize", "❓": "question",
	"love": "love", "like": "like", "dislike": "dislike", "laugh": "laugh", "emphasize": "emphasize", "question": "question"}

// Adds a tapback to a message through the Private API helper
func (a *BlueBubblesAdapter) AddReaction(ctx context.Context, chatID, messageID, emoji string) error {
	tapback, ok := tapbacks[strings.ToLower(strings.ReplaceAll(strings.TrimSpace(emoji), "\ufe0f", ""))]
	if !ok {
		return fmt.Errorf("iMessage tapbacks are ❤️ 👍 👎 😂 ‼️ ❓ (love, like, dislike, laugh, emphasize, question), %s is none of them", emoji)
	}
	a.mu.Lock()
	ready := a.privateAPIEnabled && a.helperConnected && a.client != nil
	a.mu.Unlock()
	if !ready {
		return errors.New("tapbacks go through the BlueBubbles Private API helper, turn on the Private API in the BlueBubbles server settings")
	}
	guid, err := a.resolveChatGUID(ctx, chatID)
	if err != nil {
		return err
	}
	if guid == "" {
		return fmt.Errorf("BlueBubbles knows no chat %s", chatID)
	}
	body, err := json.Marshal(map[string]any{"chatGuid": guid, "selectedMessageGuid": messageID, "reaction": tapback, "partIndex": 0})
	if err != nil {
		return err
	}
	resp, err := doRequest(ctx, a.httpClient(), http.MethodPost, a.apiURL("/api/v1/message/react"), body, "application/json", 10*time.Second)
	if err != nil {
		return err
	}
	return checkStatus(resp, "/api/v1/message/react")
}

// Private API chat action for typing or read, true only when the call was made
func (a *BlueBubblesAdapter) privateAPIChatCall(ctx context.Context, chatID, action, method string) bool {
	a.mu.Lock()
	ready := a.privateAPIEnabled && a.helperConnected && a.client != nil
	a.mu.Unlock()
	if !ready {
		return false
	}
	guid, err := a.resolveChatGUID(ctx, chatID)
	if err != nil || guid == "" {
		return false
	}
	client := a.httpClient()
	if client == nil {
		return false
	}
	_, err = doRequest(ctx, client, method, a.apiURL("/api/v1/chat/"+url.PathEscape(guid)+"/"+action), nil, "", 5*time.Second)
	return err == nil
}

// Pings the server and records whether the private API and its helper are available
func (a *BlueBubblesAdapter) probeServer(ctx context.Context) error {
	if _, err := a.apiGet(ctx, "/api/v1/ping"); err != nil {
		return err
	}
	info, err := a.apiGet(ctx, "/api/v1/server/info")
	if err != nil {
		return err
	}
	if !anyutil.IsTrue(info) {
		info = map[string]any{}
	}
	im, err := dictOf(info)
	if err != nil {
		return err
	}
	serverData, err := dictOf(getOr(im, "data", map[string]any{}))
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.privateAPIEnabled = anyutil.IsTrue(serverData["private_api"])
	a.helperConnected = anyutil.IsTrue(serverData["helper_connected"])
	private, helper := a.privateAPIEnabled, a.helperConnected
	a.mu.Unlock()
	pkgLog().Info(fmt.Sprintf("[bluebubbles] connected to %s (private_api=%t, helper=%t)", a.serverURL, private, helper))
	return nil
}

// Connects to the server, binds or publishes the webhook listener and registers the webhook with the server
func (a *BlueBubblesAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.serverURL == "" || a.password == "" {
		pkgLog().Error("[bluebubbles] BLUEBUBBLES_SERVER_URL and BLUEBUBBLES_PASSWORD are required")
		return false, nil
	}
	client := gateway.LimitedHTTPClient(gateway.PlatformHTTPXLimits(ctx), false)
	a.mu.Lock()
	a.client = client
	a.followClient = &http.Client{Transport: client.Transport, CheckRedirect: httputil.RedirectPolicy(true)}
	a.mu.Unlock()
	if err := a.probeServer(ctx); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		pkgLog().Error(fmt.Sprintf("[bluebubbles] cannot reach server at %s: %s", a.serverURL, safeError(err, a.serverURL)))
		a.closeClient()
		return false, nil
	}
	app := gateway.NewHTTPApp(webhookMaxBodyBytes)
	if err := app.AddGet("/health", func(context.Context, *gateway.WebRequest) (*gateway.WebResponse, error) {
		return okResponse(), nil
	}); err != nil {
		return false, err
	}
	if err := app.AddPost(a.webhookPath, a.handleWebhook); err != nil {
		return false, err
	}
	host := a.webhookHost
	runner, err := gateway.BindListener(ctx, a, app, &host, a.webhookPort, a.webhookPath,
		gateway.ListenOptions{AccessLog: gateway.AccessLog{Disabled: true}})
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	a.runner = runner
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	if runner != nil {
		pkgLog().Info(fmt.Sprintf("[bluebubbles] webhook listening on http://%s:%d%s", a.webhookHost, a.webhookPort, a.webhookPath))
	}
	if _, err := a.registerWebhook(ctx); err != nil {
		return false, err
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Releases the REST client and its pooled connections
func (a *BlueBubblesAdapter) closeClient() {
	a.mu.Lock()
	client := a.client
	a.client, a.followClient = nil, nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

// Unregisters the webhook, closes the client and stops the listener
func (a *BlueBubblesAdapter) Disconnect(ctx context.Context) error {
	a.unregisterWebhook(ctx)
	a.closeClient()
	a.mu.Lock()
	runner := a.runner
	a.mu.Unlock()
	if runner != nil {
		if err := runner.Cleanup(ctx); err != nil {
			return err
		}
		a.mu.Lock()
		a.runner = nil
		a.mu.Unlock()
	}
	a.MarkDisconnected(ctx)
	return nil
}

// External webhook URL the server posts to, the shared listener URL when a secondary is served by prefix
func (a *BlueBubblesAdapter) webhookURL() string {
	if shared := a.SharedIngress(); shared != nil && shared.URL != "" {
		return shared.URL
	}
	host := a.webhookHost
	if localHosts[host] {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%d%s", host, a.webhookPort, a.webhookPath)
}

// Webhook URL with the password parameter in the given form, bare when no password is set
func (a *BlueBubblesAdapter) webhookRegisterURLWith(passwordParam string) string {
	if a.password != "" {
		return a.webhookURL() + "?password=" + passwordParam
	}
	return a.webhookURL()
}

// Registered webhook URL carrying the password, the only way the server can authenticate its callbacks
func (a *BlueBubblesAdapter) webhookRegisterURL() string {
	return a.webhookRegisterURLWith(url.QueryEscape(a.password))
}

// Registered webhook URL with the password masked for logs
func (a *BlueBubblesAdapter) webhookRegisterURLForLog() string {
	return a.webhookRegisterURLWith("***")
}

// Server webhook entries registered for target, empty when the listing fails
func (a *BlueBubblesAdapter) findRegisteredWebhooks(ctx context.Context, target string) []map[string]any {
	res, err := a.apiGet(ctx, "/api/v1/webhook")
	if err != nil {
		return nil
	}
	m, err := dictOf(res)
	if err != nil {
		return nil
	}
	data, ok := m["data"].([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, item := range data {
		wh, err := dictOf(item)
		if err != nil {
			return nil
		}
		if s, isStr := wh["url"].(string); isStr && s == target {
			out = append(out, wh)
		}
	}
	return out
}

// Registers the webhook URL, reusing a registration left by an unclean shutdown
func (a *BlueBubblesAdapter) registerWebhook(ctx context.Context) (bool, error) {
	if a.httpClient() == nil {
		return false, nil
	}
	webhookURL, logURL := a.webhookRegisterURL(), a.webhookRegisterURLForLog()
	if len(a.findRegisteredWebhooks(ctx, webhookURL)) > 0 {
		pkgLog().Info("[bluebubbles] webhook already registered: " + logURL)
		return true, nil
	}
	res, err := a.apiPost(ctx, "/api/v1/webhook", jsonutil.ObjectOf("url", webhookURL, "events", []any{"new-message", "updated-message"}))
	if err == nil {
		var m map[string]any
		if m, err = dictOf(res); err == nil {
			status := getOr(m, "status", int64(0))
			var ok bool
			if ok, err = statusIs2xx(status); err == nil {
				if ok {
					pkgLog().Info("[bluebubbles] webhook registered with server: " + logURL)
					return true, nil
				}
				pkgLog().Warn(fmt.Sprintf("[bluebubbles] webhook registration returned status %s: %s", anyutil.String(status), anyutil.String(m["message"])))
				return false, nil
			}
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	pkgLog().Warn("[bluebubbles] failed to register webhook with server: " + safeError(err, a.serverURL))
	return false, nil
}

// Whether a decoded status lies in 200 to 299, a kind error for a value that is not a number
func statusIs2xx(status any) (bool, error) {
	if f, ok := status.(float64); ok {
		return f >= 200 && f < 300, nil
	}
	n, ok := anyutil.Int(status)
	if !ok {
		return false, kindError("a number", status)
	}
	return n >= 200 && n < 300, nil
}

// Removes every registration matching the webhook URL, true when any was removed
func (a *BlueBubblesAdapter) unregisterWebhook(ctx context.Context) bool {
	client := a.httpClient()
	if client == nil {
		return false
	}
	removed := false
	err := func() error {
		for _, wh := range a.findRegisteredWebhooks(ctx, a.webhookRegisterURL()) {
			id := wh["id"]
			if !anyutil.IsTrue(id) {
				continue
			}
			rawURL := a.apiURL("/api/v1/webhook/" + anyutil.String(id))
			resp, err := doRequest(ctx, client, http.MethodDelete, rawURL, nil, "", requestTimeout)
			if err != nil {
				return err
			}
			if err := checkStatus(resp, rawURL); err != nil {
				return err
			}
			removed = true
		}
		if removed {
			pkgLog().Info("[bluebubbles] webhook unregistered: " + a.webhookRegisterURLForLog())
		}
		return nil
	}()
	if err != nil {
		pkgLog().Debug("[bluebubbles] failed to unregister webhook (non-critical): " + safeError(err, a.serverURL))
	}
	return removed
}

// Cached chat GUID of a target, refreshed as most recently used
func (a *BlueBubblesAdapter) cachedGUID(target string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	el, ok := a.guidCache[target]
	if !ok {
		return "", false
	}
	a.guidOrder.MoveToBack(el)
	return el.Value.(*guidEntry).guid, true
}

// Records a resolved chat GUID, evicting the least recently used past the cache cap
func (a *BlueBubblesAdapter) storeGUID(target, guid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.guidCache[target]; ok {
		el.Value.(*guidEntry).guid = guid
	} else {
		a.guidCache[target] = a.guidOrder.PushBack(&guidEntry{target: target, guid: guid})
	}
	for a.guidOrder.Len() > guidCacheSize {
		oldest := a.guidOrder.Front()
		a.guidOrder.Remove(oldest)
		delete(a.guidCache, oldest.Value.(*guidEntry).target)
	}
}

// Chat GUID of an email or phone matched strictly on the chat identifier, raw GUIDs passing through
func (a *BlueBubblesAdapter) resolveChatGUIDFromServer(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" || strings.Contains(target, ";") {
		return target, nil
	}
	if guid, ok := a.cachedGUID(target); ok {
		return guid, nil
	}
	payload, err := a.apiPost(ctx, "/api/v1/chat/query", jsonutil.ObjectOf("limit", int64(100), "offset", int64(0)))
	if err != nil {
		return "", ctx.Err()
	}
	m, err := dictOf(payload)
	if err != nil {
		return "", nil
	}
	chats, ok := getOr(m, "data", []any{}).([]any)
	if !ok {
		return "", nil
	}
	for _, item := range chats {
		chat, err := dictOf(item)
		if err != nil {
			return "", nil
		}
		if ident, isStr := anyutil.Or(chat["chatIdentifier"], chat["identifier"]).(string); !isStr || ident != target {
			continue
		}
		guid := textOf(anyutil.Or(chat["guid"], chat["chatGuid"]))
		if guid != "" {
			a.storeGUID(target, guid)
		}
		return guid, nil
	}
	return "", nil
}

// New chat created by sending the first message to an address
func (a *BlueBubblesAdapter) createChatForHandle(ctx context.Context, address, message string) (gateway.SendResult, error) {
	return a.postMessage(ctx, "/api/v1/chat/new", jsonutil.ObjectOf("addresses", []any{address}, "message", message, "tempGuid", tempGUID()))
}

// Base splitter without the pagination suffixes, iMessage bubbles flowing naturally
func (a *BlueBubblesAdapter) TruncateMessage(content string, maxLength int, lenFn func(string) int) []string {
	chunks := gateway.TruncateMessage(content, maxLength, lenFn)
	for i, c := range chunks {
		chunks[i] = paginationSuffixRE().ReplaceAllLiteralString(c, "")
	}
	return chunks
}

// Paragraphs of text split on blank lines, stripped and without empty ones
func paragraphsOf(text string) []string {
	var out []string
	for _, p := range paragraphBreakRE().Split(text, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Sends each paragraph as its own bubble, creating a chat for an unknown address under the private API
func (a *BlueBubblesAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	text := a.FormatMessage(content)
	if text == "" {
		return gateway.SendResult{Success: false, Error: "BlueBubbles send requires text"}, nil
	}
	paragraphs := paragraphsOf(text)
	if len(paragraphs) == 0 {
		paragraphs = []string{text}
	}
	limit := a.Traits.MaxMessageLength
	var chunks []string
	for _, para := range paragraphs {
		if utf8.RuneCountInString(para) <= limit {
			chunks = append(chunks, para)
		} else {
			chunks = append(chunks, a.TruncateMessage(para, limit, nil)...)
		}
	}
	last := gateway.SendResult{Success: true}
	for _, chunk := range chunks {
		guid, err := a.resolveChatGUID(ctx, chatID)
		if err != nil {
			return gateway.SendResult{}, err
		}
		a.mu.Lock()
		private, helper := a.privateAPIEnabled, a.helperConnected
		a.mu.Unlock()
		if guid == "" {
			if private && (strings.Contains(chatID, "@") || addressRE().MatchString(chatID)) {
				return a.createChatForHandle(ctx, chatID, chunk)
			}
			return gateway.SendResult{Success: false, Error: "BlueBubbles chat not found for target: " + chatID}, nil
		}
		payload := jsonutil.ObjectOf("chatGuid", guid, "tempGuid", tempGUID(), "message", chunk)
		if replyTo != "" && private && helper {
			payload.Set("method", "private-api")
			payload.Set("selectedMessageGuid", replyTo)
			payload.Set("partIndex", int64(0))
		}
		if last, err = a.postMessage(ctx, "/api/v1/message/text", payload); err != nil || !last.Success {
			return last, err
		}
	}
	return last, nil
}

// Markdown stripped with link targets kept since iMessage auto links bare URLs
func (a *BlueBubblesAdapter) FormatMessage(content string) string {
	return gateway.StripMarkdown(content, true)
}

// Chat name, dm or group type and participant addresses of a chat
func (a *BlueBubblesAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if strings.Contains(chatID, ";+;") {
		chatType = "group"
	}
	info := map[string]any{"name": chatID, "type": chatType}
	guid, err := a.resolveChatGUID(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if guid == "" {
		return info, nil
	}
	res, err := a.apiGet(ctx, "/api/v1/chat/"+url.PathEscape(guid)+"?with=participants")
	if err != nil {
		return info, ctx.Err()
	}
	if !anyutil.IsTrue(res) {
		res = map[string]any{}
	}
	rm, err := dictOf(res)
	if err != nil {
		return info, nil
	}
	data, err := dictOf(getOr(rm, "data", map[string]any{}))
	if err != nil {
		return info, nil
	}
	info["name"] = anyutil.Or(anyutil.Or(data["displayName"], data["chatIdentifier"]), chatID)
	rawParticipants := getOr(data, "participants", []any{})
	if !anyutil.IsTrue(rawParticipants) {
		rawParticipants = []any{}
	}
	items, ok := rawParticipants.([]any)
	if !ok {
		return info, nil
	}
	var participants []any
	for _, item := range items {
		p, err := dictOf(item)
		if err != nil {
			return info, nil
		}
		addr := anyutil.Or(p["address"], "")
		s, isStr := addr.(string)
		if !isStr {
			return info, nil
		}
		if s = strings.TrimSpace(s); s != "" {
			participants = append(participants, s)
		}
	}
	if len(participants) > 0 {
		info["participants"] = participants
	}
	return info, nil
}
