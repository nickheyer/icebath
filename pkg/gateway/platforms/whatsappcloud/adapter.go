package whatsappcloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
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
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ""
	}
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

// Error carrying a kind label beside its message
type pyErr struct{ kind, msg string }

func (e *pyErr) Error() string     { return e.msg }
func (e *pyErr) ErrorKind() string { return e.kind }

// Error for a decoded value that is not the object the payload shape requires
func notObject(v any) error {
	return fmt.Errorf("expected an object, got %s", anyutil.Kind(v))
}

// Map view of a value or an empty map for an empty or missing one, an error for anything else
func dictOr(v any) (map[string]any, error) {
	if !anyutil.IsTrue(v) {
		return map[string]any{}, nil
	}
	if m, ok := jsonutil.AsMap(v); ok {
		return m, nil
	}
	return nil, notObject(v)
}

// Value under key or the default when the key is absent
func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Trimmed text of a value, empty for an empty or missing one
func strField(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return strings.TrimSpace(anyutil.String(v))
}

// Items of a decoded value: the elements of an array, the characters of a text, the keys of an object, nothing for an empty or missing value and an error for a scalar
func iterItems(v any) ([]any, error) {
	if !anyutil.IsTrue(v) {
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
	if m, ok := jsonutil.AsObject(v); ok {
		out := []any{}
		for _, k := range m.Keys() {
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
	return nil, fmt.Errorf("expected an array, got %s", anyutil.Kind(v))
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
	return optinTruthy[strings.ToLower(strings.TrimSpace(value))], err
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
	runner                 *gateway.HTTPAppRunner
	seenWamids             *orderedutil.Map[string, bool]
	duplicateCount         int
	acceptedCount          int
	rejectedSignatureCount int
	warnedNoFFmpeg         bool
	lastInboundWamidByChat *orderedutil.Map[string, string]
	clarifyState           *orderedutil.Map[string, string]
	execApprovalState      *orderedutil.Map[string, string]
	slashConfirmState      *orderedutil.Map[string, string]

	handleMessage func(ctx context.Context, event *gateway.MessageEvent) error
}

// Adapter configured from extra and the scoped WhatsApp env, the int error for a malformed port
func NewWhatsAppCloudAdapter(ctx context.Context, config *gateway.PlatformConfig) (*WhatsAppCloudAdapter, error) {
	a := &WhatsAppCloudAdapter{
		seenWamids: orderedutil.NewMap[string, bool](), lastInboundWamidByChat: orderedutil.NewMap[string, string](),
		clarifyState: orderedutil.NewMap[string, string](), execApprovalState: orderedutil.NewMap[string, string](),
		slashConfirmState: orderedutil.NewMap[string, string](),
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
	if host := extra["webhook_host"]; anyutil.IsTrue(host) {
		h := anyutil.String(host)
		a.webhookHost = &h
	}
	port, err := anyutil.ToInt(getOr(extra, "webhook_port", int64(defaultWebhookPort)))
	if err != nil {
		return nil, err
	}
	a.webhookPort = int(port)
	a.webhookPath = normalizePath(getOr(extra, "webhook_path", defaultWebhookPath))
	a.healthPath = normalizePath(getOr(extra, "health_path", "/health"))
	a.apiVersion = anyutil.String(getOr(extra, "api_version", defaultAPIVersion))
	if rp, ok := extra["reply_prefix"]; ok && rp != nil {
		s := anyutil.String(rp)
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
	_, rawGroups, err := whatsapp.SelectAllowlist(extra, []string{"group_allow_from"}, []string{"WHATSAPP_CLOUD_GROUP_ALLOW_FROM"}, readEnv)
	if err != nil {
		return nil, err
	}
	a.GroupAllowFrom = normalizeAllowIDs(whatsapp.CoerceAllowList(rawGroups))
	return a, nil
}

// Policy from the config value, then each scoped env carrier in order, then the default, lowercased
func firstPolicy(ctx context.Context, configured any, envs []string, def string) (string, error) {
	if anyutil.IsTrue(configured) {
		return strings.ToLower(strings.TrimSpace(anyutil.String(configured))), nil
	}
	for _, env := range envs {
		v, _, err := wsecret(ctx, env)
		if err != nil {
			return "", err
		}
		if v != "" {
			return strings.ToLower(strings.TrimSpace(v)), nil
		}
	}
	return strings.ToLower(strings.TrimSpace(def)), nil
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
	return max(1024, maxMessageLength-utf8.RuneCountInString(a.effectiveReplyPrefix()))
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
	m, ok := jsonutil.AsMap(metadata)
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
	a.httpClient = gateway.LimitedHTTPClient(gateway.PlatformHTTPXLimits(ctx), false)
	a.mu.Unlock()
	app := gateway.NewHTTPApp(webhookMaxBodyBytes)
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
		pkgLog().Info(fmt.Sprintf("[whatsapp_cloud] Listening on %s:%d%s (Graph %s, phone_id=%s)", host, a.webhookPort, a.webhookPath, a.apiVersion, a.phoneNumberID))
	}
	if a.verifyToken == "" {
		pkgLog().Warn("[whatsapp_cloud] WHATSAPP_CLOUD_VERIFY_TOKEN is not set — the GET subscription handshake will fail until it is.")
	}
	if a.appSecret == "" {
		pkgLog().Warn("[whatsapp_cloud] WHATSAPP_CLOUD_APP_SECRET is not set — incoming webhook POSTs will be refused " +
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
			pkgLog().Error("[whatsapp_cloud] webhook server cleanup failed: " + err.Error())
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

// One request with a timeout on each phase, the caller's cancellation reported before a transport failure
func doRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte) (*httputil.Response, error) {
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
	resp, err := httputil.Do(client, req, httputil.EachPhase(requestTimeout))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return resp, nil
}

// JSON POST to a Graph URL with the bearer headers
func (a *WhatsAppCloudAdapter) postJSON(ctx context.Context, client *http.Client, rawURL string, payload any) (*httputil.Response, error) {
	text, err := jsonutil.Encode(payload)
	if err != nil {
		return nil, err
	}
	return doRequest(ctx, client, http.MethodPost, rawURL, a.authHeaders(true), []byte(text))
}

// Actionable error text of a non 200 Graph response, graph error code first when present
func responseError(resp *httputil.Response) (string, error) {
	body, err := resp.JSON()
	if err != nil {
		text := []rune(resp.Text())
		if len(text) > 500 {
			text = text[:500]
		}
		body = jsonutil.ObjectOf("raw", string(text))
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
	if !anyutil.IsTrue(message) {
		raw, err := dictOf(body)
		if err != nil {
			return "", err
		}
		message = anyutil.Or(raw["raw"], "unknown error")
	}
	if code, ok := errBlock["code"]; ok && code != nil {
		return fmt.Sprintf("graph error %s (HTTP %d): %s", anyutil.String(code), resp.StatusCode, anyutil.String(message)), nil
	}
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, anyutil.String(message)), nil
}

// Map view of a decoded value, an error for anything that is not an object
func dictOf(v any) (map[string]any, error) {
	if m, ok := jsonutil.AsMap(v); ok {
		return m, nil
	}
	return nil, notObject(v)
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
		pkgLog().Error(logs.fail + ": " + err.Error())
		msg := err.Error()
		return nil, &msg, nil
	}
	if resp.StatusCode != http.StatusOK {
		errorMsg, err := responseError(resp)
		if err != nil {
			return nil, nil, err
		}
		args := append(append([]any{resp.StatusCode}, logs.rejectArgs...), errorMsg)
		pkgLog().Warn(fmt.Sprintf(logs.reject, args...))
		return nil, &errorMsg, nil
	}
	decoded, err := resp.JSON()
	if err != nil {
		return []any{}, nil, nil
	}
	m, ok := jsonutil.AsMap(decoded)
	if !ok {
		return []any{}, nil, nil
	}
	if !anyutil.IsTrue(m["messages"]) {
		return []any{}, nil, nil
	}
	items, err := iterItems(m["messages"])
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

// Adds an emoji reaction to a message as a Cloud API reaction message
func (a *WhatsAppCloudAdapter) AddReaction(ctx context.Context, chatID, messageID, emoji string) error {
	result, err := a.postMessageResult(ctx, outboundPayload(chatID, "reaction", jsonutil.ObjectOf("message_id", messageID, "emoji", emoji), nil),
		postLogs{fail: "[whatsapp_cloud] reaction send failed", reject: "[whatsapp_cloud] reaction rejected (status=%d): %s"})
	if err != nil {
		return err
	}
	if !result.Success {
		return errors.New(anyutil.String(result.Error))
	}
	return nil
}

// Common messages envelope quoting replyTo when one is given
func outboundPayload(chatID, kind string, block, replyTo any) *jsonutil.Object {
	payload := jsonutil.ObjectOf("messaging_product", "whatsapp", "recipient_type", "individual", "to", chatID, "type", kind, kind, block)
	if anyutil.IsTrue(replyTo) {
		payload.Set("context", jsonutil.ObjectOf("message_id", replyTo))
	}
	return payload
}

// Sends formatted text in chunks quoting the user on the first, indexing the last id for reply lookups
func (a *WhatsAppCloudAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	client := a.client()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if content == "" || strings.TrimSpace(content) == "" {
		return gateway.SendResult{Success: true, MessageID: nil}, nil
	}
	formatted := a.FormatMessage(content)
	var lastMessageID any
	for idx, chunk := range gateway.TruncateMessage(formatted, a.outgoingChunkLimit(), nil) {
		var quote any
		if idx == 0 && replyTo != "" {
			quote = replyTo
		}
		payload := outboundPayload(chatID, "text", jsonutil.ObjectOf("body", chunk, "preview_url", true), quote)
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
	if anyutil.IsTrue(lastMessageID) {
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
	payload := jsonutil.ObjectOf("messaging_product", "whatsapp", "status", "read", "message_id", wamid,
		"typing_indicator", jsonutil.ObjectOf("type", "text"))
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
	if anyutil.Equal(code, int64(131009)) {
		pkgLog().Info(fmt.Sprintf("[whatsapp_cloud] typing/read indicator rejected: wamid %s likely older than 30 days", wamid))
	} else {
		pkgLog().Debug(fmt.Sprintf("[whatsapp_cloud] typing/read indicator returned %d (%s)", resp.StatusCode, anyutil.String(code)))
	}
	return nil
}

// No chat info endpoint exists, the profile name arrives with webhook contacts
func (a *WhatsAppCloudAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "dm"}, nil
}
