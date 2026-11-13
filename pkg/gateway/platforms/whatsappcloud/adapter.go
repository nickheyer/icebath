package whatsappcloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	defaultAPIVersion          = "v20.0"
	defaultWebhookPort         = 8090
	defaultWebhookPath         = "/whatsapp/webhook"
	graphAPIBase               = "https://graph.facebook.com"
	webhookMaxBodyBytes        = 3 * 1024 * 1024
	wamidDedupCacheSize        = 5000
	interactiveStateCacheSize  = 1000
	maxMessageLength           = 4096
	maxTextInjectBytes         = 100 * 1024
	requestTimeout             = 30 * time.Second
	missingSecret              = "\x00whatsapp-cloud-missing"
	whatsappCloudAllowAllUsers = "WHATSAPP_CLOUD_ALLOW_ALL_USERS"
)

// Meta per type caps of the media endpoint, refused locally before a round trip
var mediaSizeLimits = map[string]int64{
	"image": 5 * 1024 * 1024, "video": 16 * 1024 * 1024, "audio": 16 * 1024 * 1024,
	"document": 100 * 1024 * 1024, "sticker": 100 * 1024,
}

// Mime types used when the path extension guesses none
var defaultMime = map[string]string{
	"image": "image/jpeg", "video": "video/mp4", "audio": "audio/mpeg",
	"document": "application/octet-stream", "sticker": "image/webp",
}

// Extensions pinned for the mimes Meta sends where mimetypes picks uncommon ones
var mimeExtensionOverrides = map[string]string{
	"audio/ogg": ".ogg", "audio/x-opus+ogg": ".ogg", "audio/opus": ".ogg",
	"audio/mp4": ".m4a", "audio/x-m4a": ".m4a", "image/jpeg": ".jpg",
}

var inboundMediaKinds = map[string]bool{"image": true, "video": true, "audio": true, "voice": true, "document": true, "sticker": true}

// Envelopes on the messages field that carry no user utterance
var contentlessKinds = map[string]bool{"system": true, "reaction": true, "unsupported": true, "unknown": true}

var textInjectExts = map[string]bool{".txt": true, ".md": true, ".csv": true, ".json": true, ".xml": true, ".yaml": true,
	".yml": true, ".log": true, ".py": true, ".js": true, ".ts": true, ".html": true, ".css": true}

var messageTypeByKind = map[string]gateway.MessageType{
	"text": gateway.MessageTypeText, "image": gateway.MessageTypePhoto, "video": gateway.MessageTypeVideo,
	"audio": gateway.MessageTypeVoice, "voice": gateway.MessageTypeVoice, "document": gateway.MessageTypeDocument,
	"sticker": gateway.MessageTypePhoto, "button": gateway.MessageTypeText, "interactive": gateway.MessageTypeText,
	"location": gateway.MessageTypeText, "contacts": gateway.MessageTypeText,
}

var optinTruthy = map[string]bool{"true": true, "1": true, "yes": true}

var (
	nonDigitRE = pyre.MustCompile(`\D`)
	mediaIDRE  = mustFullmatch(`[A-Za-z0-9._-]+`)
)

// ffmpeg found on PATH at load, empty when voice notes fall back to MP3 attachments
var ffmpegPath = func() string {
	path, _ := pyshutil.Which("ffmpeg", nil)
	return path
}()

// Fullmatch Python pattern, panicking on a pattern that does not compile
func mustFullmatch(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileFullmatch(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Python exception carried as a Go error with its class name
type pyErr struct{ kind, msg string }

func (e *pyErr) Error() string      { return e.msg }
func (e *pyErr) PyTypeName() string { return e.kind }

// AttributeError of calling a dict method on a value that is not a dict
func noAttr(v any, attr string) error {
	return &pyErr{"AttributeError", fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)}
}

// Dict view of a value or an empty dict for a falsy one, the AttributeError get raises on anything else
func dictOr(v any) (map[string]any, error) {
	if !pyrepr.Truthy(v) {
		return map[string]any{}, nil
	}
	if m, ok := pyjson.Dict(v); ok {
		return m, nil
	}
	return nil, noAttr(v, "get")
}

// Value under key or the default when the key is absent
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// str of a value with falsy values reading as empty, stripped
func strField(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Items Python iteration of a decoded value yields, the TypeError iterating a scalar raises
func pyIter(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	if m, ok := pyjson.AsObject(v); ok {
		out := []any{}
		for _, k := range m.Keys {
			out = append(out, k)
		}
		return out, nil
	}
	if m, ok := v.(map[string]any); ok {
		out := []any{}
		for k := range m {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, &pyErr{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

// str of an exception, the class name when its message is empty
func errText(err error) string {
	if msg := pyrepr.ExceptionMessage(err); msg != "" {
		return msg
	}
	return pyrepr.ExceptionName(err)
}

// On disk extension of a mime from the WhatsApp overrides then mimetypes, empty when neither knows it
func extForMime(mime string) string {
	if mime == "" {
		return ""
	}
	return gateway.ExtForMime(mime, mimeExtensionOverrides, false, true, "")
}

// Whether the profile opted into WHATSAPP_CLOUD_ALLOW_ALL_USERS
func cloudAllowAllOptedIn(ctx context.Context) (bool, error) {
	value, err := gateway.GetScopedSecret(ctx, whatsappCloudAllowAllUsers, "", false)
	return optinTruthy[pystr.Lower(pystr.Strip(value))], err
}

// Message id to quote from send metadata, nil when none is set
func replyToFrom(metadata map[string]any) any {
	return metadata["reply_to_message_id"]
}

// Scoped secret read, empty and false when the profile has no such key
func wsecret(ctx context.Context, name string) (string, bool, error) {
	value, err := gateway.GetScopedSecret(ctx, name, missingSecret, false)
	if err != nil || value == missingSecret {
		return "", false, err
	}
	return value, true, nil
}

// Path stripped with a leading slash, the root for an empty value
func normalizePath(path any) string {
	raw := strField(path)
	if raw == "" {
		raw = "/"
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return raw
}

// Allowlist entries reduced to the bare wa_id digits, an entry without digits kept as is
func normalizeAllowIDs(ids map[string]bool) map[string]bool {
	out := map[string]bool{}
	for entry := range ids {
		out[bareWAID(entry)] = true
	}
	return out
}

// Digits of the part before any JID suffix, the value itself when it has none
func bareWAID(value string) string {
	head := strings.SplitN(value, "@", 2)[0]
	bare, err := nonDigitRE.Replace(head, "", -1, -1)
	if err != nil || bare == "" {
		return value
	}
	return bare
}

// Class attributes of the Cloud API adapter, the WhatsApp mixin ones included
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.ServesProfilePrefix = true
	t.SplitsLongMessages = true
	t.SupportsCodeBlocks = true
	t.MaxMessageLength = maxMessageLength
	t.EACodeClose = "\n```\n\n"
	t.EACmdBudget = 800
	return t
}

// WhatsAppCloudAdapter sends through the Graph API and receives the signed Meta webhook
type WhatsAppCloudAdapter struct {
	*gateway.BasePlatformAdapter
	*whatsapp.WhatsAppBehaviorMixin

	phoneNumberID string
	accessToken   string
	appID         string
	appSecret     string
	wabaID        string
	verifyToken   string
	webhookHost   *string
	webhookPort   int
	webhookPath   string
	healthPath    string
	apiVersion    string

	mu                     sync.Mutex
	httpClient             *http.Client
	runner                 *gateway.WebAppRunner
	seenWamids             *pydict.Dict[string, bool]
	duplicateCount         int
	acceptedCount          int
	rejectedSignatureCount int
	warnedNoFFmpeg         bool
	lastInboundWamidByChat *pydict.Dict[string, string]
	clarifyState           *pydict.Dict[string, string]
	execApprovalState      *pydict.Dict[string, string]
	slashConfirmState      *pydict.Dict[string, string]

	handleMessage func(ctx context.Context, event *gateway.MessageEvent) error
}

// Adapter configured from extra and the scoped WhatsApp env, the int error for a malformed port
func NewWhatsAppCloudAdapter(ctx context.Context, config *gateway.PlatformConfig) (*WhatsAppCloudAdapter, error) {
	a := &WhatsAppCloudAdapter{
		seenWamids: pydict.New[string, bool](), lastInboundWamidByChat: pydict.New[string, string](),
		clarifyState: pydict.New[string, string](), execApprovalState: pydict.New[string, string](),
		slashConfirmState: pydict.New[string, string](),
	}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformWhatsAppCloud, a)
	a.Traits = adapterTraits()
	a.WhatsAppBehaviorMixin = whatsapp.NewWhatsAppBehaviorMixin(a.BasePlatformAdapter.Name(), config)
	a.OwnAccessPolicyMixin.EntryMatches = a.entryMatches
	a.OwnAccessPolicyMixin.ExtraAllowAllEnvNames = []string{whatsappCloudAllowAllUsers}
	a.handleMessage = a.BasePlatformAdapter.HandleMessage
	extra := map[string]any{}
	if config != nil && config.Extra != nil {
		extra = config.Extra
	}
	for key, field := range map[string]*string{"phone_number_id": &a.phoneNumberID, "access_token": &a.accessToken,
		"app_id": &a.appID, "app_secret": &a.appSecret, "waba_id": &a.wabaID, "verify_token": &a.verifyToken} {
		*field = strField(extra[key])
	}
	if host := extra["webhook_host"]; pyrepr.Truthy(host) {
		h := pyrepr.Str(host)
		a.webhookHost = &h
	}
	port, err := pyint.Int64(getOr(extra, "webhook_port", int64(defaultWebhookPort)))
	if err != nil {
		return nil, err
	}
	a.webhookPort = int(port)
	a.webhookPath = normalizePath(getOr(extra, "webhook_path", defaultWebhookPath))
	a.healthPath = normalizePath(getOr(extra, "health_path", "/health"))
	a.apiVersion = pyrepr.Str(getOr(extra, "api_version", defaultAPIVersion))
	if rp, ok := extra["reply_prefix"]; ok && rp != nil {
		s := pyrepr.Str(rp)
		a.ReplyPrefix = &s
	}
	readEnv := func(name string) (string, error) {
		v, _, err := wsecret(ctx, name)
		return v, err
	}
	allowRaw, err := a.SelectDMAllowlist(extra, []string{"WHATSAPP_CLOUD_ALLOW_FROM", "WHATSAPP_CLOUD_ALLOWED_USERS"}, readEnv)
	if err != nil {
		return nil, err
	}
	a.AllowFrom = normalizeAllowIDs(whatsapp.CoerceAllowList(allowRaw))
	defaultDMPolicy := "open"
	if len(a.AllowFrom) > 0 {
		optedIn, err := cloudAllowAllOptedIn(ctx)
		if err != nil {
			return nil, err
		}
		if !optedIn {
			defaultDMPolicy = "allowlist"
		}
	}
	if a.DMPolicy, err = firstPolicy(ctx, extra["dm_policy"], []string{"WHATSAPP_CLOUD_DM_POLICY", "WHATSAPP_DM_POLICY"}, defaultDMPolicy); err != nil {
		return nil, err
	}
	if a.GroupPolicy, err = firstPolicy(ctx, extra["group_policy"], []string{"WHATSAPP_CLOUD_GROUP_POLICY", "WHATSAPP_GROUP_POLICY"}, "open"); err != nil {
		return nil, err
	}
	_, rawGroups, err := whatsapp.SelectAllowlist(extra, []string{"group_allow_from", "groupAllowFrom"}, []string{"WHATSAPP_CLOUD_GROUP_ALLOW_FROM"}, readEnv)
	if err != nil {
		return nil, err
	}
	a.GroupAllowFrom = normalizeAllowIDs(whatsapp.CoerceAllowList(rawGroups))
	if a.MentionPatterns, err = a.CompileMentionPatterns(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Policy from the config value, then each scoped env carrier in order, then the default, lowercased
func firstPolicy(ctx context.Context, configured any, envs []string, def string) (string, error) {
	if pyrepr.Truthy(configured) {
		return pystr.Lower(pystr.Strip(pyrepr.Str(configured))), nil
	}
	for _, env := range envs {
		v, _, err := wsecret(ctx, env)
		if err != nil {
			return "", err
		}
		if v != "" {
			return pystr.Lower(pystr.Strip(v)), nil
		}
	}
	return pystr.Lower(pystr.Strip(def)), nil
}

// Platform display name, the base name over the mixin field
func (a *WhatsAppCloudAdapter) Name() string { return a.BasePlatformAdapter.Name() }

// The adapter gates DMs and groups itself
func (a *WhatsAppCloudAdapter) EnforcesOwnAccessPolicy() bool {
	return a.WhatsAppBehaviorMixin.EnforcesOwnAccessPolicy()
}

// Markdown converted to WhatsApp syntax
func (a *WhatsAppCloudAdapter) FormatMessage(content string) string {
	return whatsapp.FormatMessage(content)
}

// Graph API URL under the adapter's phone number scope
func (a *WhatsAppCloudAdapter) graphURL(path string) string {
	return fmt.Sprintf("%s/%s/%s/%s", graphAPIBase, a.apiVersion, a.phoneNumberID, strings.TrimPrefix(path, "/"))
}

// Bearer authorization headers, with the JSON content type unless a multipart body follows
func (a *WhatsAppCloudAdapter) authHeaders(jsonBody bool) map[string]string {
	headers := map[string]string{"Authorization": "Bearer " + a.accessToken}
	if jsonBody {
		headers["Content-Type"] = "application/json"
	}
	return headers
}

// Reply prefix of the Cloud API, empty since it has no self chat mode
func (a *WhatsAppCloudAdapter) effectiveReplyPrefix() string {
	if a.ReplyPrefix == nil {
		return ""
	}
	return strings.ReplaceAll(*a.ReplyPrefix, `\n`, "\n")
}

// Chunk budget with room reserved for the reply prefix, floored at 1024
func (a *WhatsAppCloudAdapter) outgoingChunkLimit() int {
	return max(1024, maxMessageLength-pystr.Len(a.effectiveReplyPrefix()))
}

// Bare wa_id membership first, then the shared WhatsApp matcher for wildcard and alias entries
func (a *WhatsAppCloudAdapter) entryMatches(ctx context.Context, entries map[string]bool, target string) (bool, error) {
	if normalizeAllowIDs(entries)[bareWAID(target)] {
		return true, nil
	}
	return whatsapp.MatchesWhatsAppAllowlist(ctx, target, entries)
}

// Whether a signed callback names the configured phone number id and, when set, the WABA id
func (a *WhatsAppCloudAdapter) webhookIdentityMatches(entryID, metadata any) bool {
	m, ok := pyjson.Dict(metadata)
	if !ok {
		m = map[string]any{}
	}
	incomingPhone := strField(m["phone_number_id"])
	incomingWaba := strField(entryID)
	return incomingPhone == a.phoneNumberID && (a.wabaID == "" || incomingWaba == a.wabaID)
}

// Outbound client in use, nil while disconnected
func (a *WhatsAppCloudAdapter) client() *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.httpClient
}

// Starts the Graph client and the webhook listener, a fatal error when the credentials are missing
func (a *WhatsAppCloudAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.phoneNumberID == "" || a.accessToken == "" {
		a.SetFatalError(ctx, "whatsapp_cloud_unconfigured", "WHATSAPP_CLOUD_PHONE_NUMBER_ID and WHATSAPP_CLOUD_ACCESS_TOKEN are required.", false)
		return false, nil
	}
	a.mu.Lock()
	a.httpClient = gateway.LimitedHTTPClient(gateway.PlatformHTTPXLimits(), false)
	a.mu.Unlock()
	app := gateway.NewWebApplication(webhookMaxBodyBytes)
	if err := app.AddGet(a.healthPath, a.handleHealth); err != nil {
		return false, err
	}
	if err := app.AddGet(a.webhookPath, a.handleVerify); err != nil {
		return false, err
	}
	if err := app.AddPost(a.webhookPath, a.handleWebhook); err != nil {
		return false, err
	}
	runner, err := gateway.BindListener(ctx, a, app, a.webhookHost, a.webhookPort, a.webhookPath, gateway.ListenOptions{})
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	a.runner = runner
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	if runner != nil {
		host := "None"
		if a.webhookHost != nil {
			host = *a.webhookHost
		}
		slog.Info(fmt.Sprintf("[whatsapp_cloud] Listening on %s:%d%s (Graph %s, phone_id=%s)", host, a.webhookPort, a.webhookPath, a.apiVersion, a.phoneNumberID))
	}
	if a.verifyToken == "" {
		slog.Warn("[whatsapp_cloud] WHATSAPP_CLOUD_VERIFY_TOKEN is not set — the GET subscription handshake will fail until it is.")
	}
	if a.appSecret == "" {
		slog.Warn("[whatsapp_cloud] WHATSAPP_CLOUD_APP_SECRET is not set — incoming webhook POSTs will be refused " +
			"with 503. Set the app secret to enable inbound message delivery.")
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the webhook server and closes the client, logging either failure, then marks disconnected
func (a *WhatsAppCloudAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	runner, client := a.runner, a.httpClient
	a.mu.Unlock()
	if runner != nil {
		if err := runner.Cleanup(ctx); err != nil {
			slog.Error("[whatsapp_cloud] webhook server cleanup failed: " + err.Error())
		}
		a.mu.Lock()
		a.runner = nil
		a.mu.Unlock()
	}
	if client != nil {
		client.CloseIdleConnections()
		a.mu.Lock()
		a.httpClient = nil
		a.mu.Unlock()
	}
	a.MarkDisconnected(ctx)
	return nil
}

// One request with httpx phase timeouts, transport failures carrying the httpx error text
func doRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte) (*pyhttpx.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := pyhttpx.Do(client, req, requestTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var te *pyhttpx.TimeoutError
		if errors.As(err, &te) {
			return nil, &pyErr{"ReadTimeout", te.Error()}
		}
		return nil, &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
	}
	return resp, nil
}

// JSON POST to a Graph URL with the bearer headers
func (a *WhatsAppCloudAdapter) postJSON(ctx context.Context, client *http.Client, rawURL string, payload any) (*pyhttpx.Response, error) {
	text, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	return doRequest(ctx, client, http.MethodPost, rawURL, a.authHeaders(true), []byte(text))
}

// Actionable error text of a non 200 Graph response, graph error code first when present
func responseError(resp *pyhttpx.Response) (string, error) {
	body, err := resp.JSON()
	if err != nil {
		text := []rune(resp.Text())
		if len(text) > 500 {
			text = text[:500]
		}
		body = pyjson.ObjectOf("raw", string(text))
	}
	bm, err := dictOr(body)
	if err != nil {
		return "", err
	}
	errBlock, err := dictOr(bm["error"])
	if err != nil {
		return "", err
	}
	message := errBlock["message"]
	if !pyrepr.Truthy(message) {
		raw, err := dictOf(body)
		if err != nil {
			return "", err
		}
		message = pyrepr.Or(raw["raw"], "unknown error")
	}
	if code, ok := errBlock["code"]; ok && code != nil {
		return fmt.Sprintf("graph error %s (HTTP %d): %s", pyrepr.Str(code), resp.StatusCode, pyrepr.Str(message)), nil
	}
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, pyrepr.Str(message)), nil
}

// Dict view of a decoded value, the AttributeError get raises otherwise
func dictOf(v any) (map[string]any, error) {
	if m, ok := pyjson.Dict(v); ok {
		return m, nil
	}
	return nil, noAttr(v, "get")
}

// Log lines of one messages POST, the reject line taking the status, its extra args and the error
type postLogs struct {
	fail, reject string
	rejectArgs   []any
}

// One messages POST, its returned message list or the error text, a raised error for a malformed answer
func (a *WhatsAppCloudAdapter) postMessages(ctx context.Context, client *http.Client, payload any, logs postLogs) ([]any, *string, error) {
	resp, err := a.postJSON(ctx, client, a.graphURL("messages"), payload)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		slog.Error(logs.fail + ": " + err.Error())
		msg := errText(err)
		return nil, &msg, nil
	}
	if resp.StatusCode != http.StatusOK {
		errorMsg, err := responseError(resp)
		if err != nil {
			return nil, nil, err
		}
		args := append(append([]any{resp.StatusCode}, logs.rejectArgs...), errorMsg)
		slog.Warn(fmt.Sprintf(logs.reject, args...))
		return nil, &errorMsg, nil
	}
	decoded, err := resp.JSON()
	if err != nil {
		return []any{}, nil, nil
	}
	m, ok := pyjson.Dict(decoded)
	if !ok {
		return []any{}, nil, nil
	}
	if !pyrepr.Truthy(m["messages"]) {
		return []any{}, nil, nil
	}
	items, err := pyIter(m["messages"])
	return items, nil, err
}

// Id of the first returned message, nil when the list is empty
func firstMessageID(ids []any) (any, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	m, err := dictOf(ids[0])
	if err != nil {
		return nil, err
	}
	return m["id"], nil
}

// Messages POST wrapped as a send result with the first message id, refused while disconnected
func (a *WhatsAppCloudAdapter) postMessageResult(ctx context.Context, payload any, logs postLogs) (gateway.SendResult, error) {
	client := a.client()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	ids, errMsg, err := a.postMessages(ctx, client, payload, logs)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if errMsg != nil {
		return gateway.SendResult{Success: false, Error: *errMsg}, nil
	}
	id, err := firstMessageID(ids)
	if err != nil {
		return gateway.SendResult{}, err
	}
	return gateway.SendResult{Success: true, MessageID: id}, nil
}

// Common messages envelope quoting replyTo when one is given
func outboundPayload(chatID, kind string, block, replyTo any) *pyjson.Object {
	payload := pyjson.ObjectOf("messaging_product", "whatsapp", "recipient_type", "individual", "to", chatID, "type", kind, kind, block)
	if pyrepr.Truthy(replyTo) {
		payload.Set("context", pyjson.ObjectOf("message_id", replyTo))
	}
	return payload
}

// Sends formatted text in chunks quoting the user on the first, indexing the last id for reply lookups
func (a *WhatsAppCloudAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	client := a.client()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if content == "" || pystr.Strip(content) == "" {
		return gateway.SendResult{Success: true, MessageID: nil}, nil
	}
	formatted := a.FormatMessage(content)
	var lastMessageID any
	for idx, chunk := range gateway.TruncateMessage(formatted, a.outgoingChunkLimit(), nil) {
		var quote any
		if idx == 0 && replyTo != "" {
			quote = replyTo
		}
		payload := outboundPayload(chatID, "text", pyjson.ObjectOf("body", chunk, "preview_url", true), quote)
		ids, errMsg, err := a.postMessages(ctx, client, payload, postLogs{fail: "[whatsapp_cloud] send failed",
			reject: "[whatsapp_cloud] send rejected (status=%d): %s"})
		if err != nil {
			return gateway.SendResult{}, err
		}
		if errMsg != nil {
			return gateway.SendResult{Success: false, Error: *errMsg}, nil
		}
		if len(ids) > 0 {
			if lastMessageID, err = firstMessageID(ids); err != nil {
				return gateway.SendResult{}, err
			}
		}
	}
	if pyrepr.Truthy(lastMessageID) {
		gateway.RichSentRecord(ctx, chatID, lastMessageID, formatted)
	}
	return gateway.SendResult{Success: true, MessageID: lastMessageID}, nil
}

// Marks the latest inbound message read with a typing indicator, every failure swallowed
func (a *WhatsAppCloudAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.mu.Lock()
	wamid, _ := a.lastInboundWamidByChat.Get(chatID)
	client := a.httpClient
	a.mu.Unlock()
	if client == nil || wamid == "" {
		return nil
	}
	payload := pyjson.ObjectOf("messaging_product", "whatsapp", "status", "read", "message_id", wamid,
		"typing_indicator", pyjson.ObjectOf("type", "text"))
	resp, err := a.postJSON(ctx, client, a.graphURL("messages"), payload)
	if err != nil || resp.StatusCode == http.StatusOK {
		return nil
	}
	var code any
	if decoded, err := resp.JSON(); err == nil {
		if body, err := dictOr(decoded); err == nil {
			if block, err := dictOr(body["error"]); err == nil {
				code = block["code"]
			}
		}
	}
	if pyrepr.Equal(code, int64(131009)) {
		slog.Info(fmt.Sprintf("[whatsapp_cloud] typing/read indicator rejected: wamid %s likely older than 30 days", wamid))
	} else {
		slog.Debug(fmt.Sprintf("[whatsapp_cloud] typing/read indicator returned %d (%s)", resp.StatusCode, pyrepr.Str(code)))
	}
	return nil
}

// No chat info endpoint exists, the profile name arrives with webhook contacts
func (a *WhatsAppCloudAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "dm"}, nil
}
