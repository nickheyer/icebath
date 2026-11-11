package gateway

import (
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	msgraphDefaultPort            = 8646
	msgraphDefaultWebhookPath     = "/msgraph/webhook"
	msgraphDefaultMaxSeenReceipts = 5000
	msgraphDefaultMaxBodyBytes    = 1_048_576
)

var msgraphTemplateKeyRE = regexp.MustCompile(`\{([a-zA-Z0-9_.]+)\}`)

// Plugin owned handler of one Graph notification and its internal event, replacing gateway inference
type NotificationScheduler func(ctx context.Context, notification *pyjson.Object, event *MessageEvent) error

// Whether the webhook server dependencies are available
func CheckMSGraphWebhookRequirements() bool { return true }

// Stripped text of a value, nil for None or blank text
func msgraphStringOrNone(value any) *string {
	if value == nil {
		return nil
	}
	text := pystr.Strip(pyrepr.Str(value))
	if text == "" {
		return nil
	}
	return &text
}

// Route path with a leading slash, the root for a blank value
func msgraphNormalizePath(path any) string {
	raw := ""
	if pyrepr.Truthy(path) {
		raw = pystr.Strip(pyrepr.Str(path))
	}
	if raw == "" {
		raw = "/"
	}
	if strings.HasPrefix(raw, "/") {
		return raw
	}
	return "/" + raw
}

// Items a Python for loop yields from a config value, the TypeError of a value that is not iterable
func msgraphIterate(value any) ([]any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case []any:
		return v, nil
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out, nil
	case string:
		var out []any
		for _, r := range v {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		var out []any
		for k := range v {
			out = append(out, k)
		}
		return out, nil
	case *pyjson.Object:
		out := make([]any, 0, len(v.Keys))
		for _, k := range v.Keys {
			out = append(out, k)
		}
		return out, nil
	}
	if !pyrepr.Truthy(value) {
		return nil, nil
	}
	return nil, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(value))}
}

// Optional CIDR allowlist, empty allowing every source and an invalid entry skipped with a warning
func msgraphParseAllowedSourceCIDRs(raw any) []netip.Prefix {
	var candidates []string
	switch v := raw.(type) {
	case string:
		candidates = strings.Split(v, ",")
	case []any:
		for _, chunk := range v {
			candidates = append(candidates, pyrepr.Str(chunk))
		}
	case []string:
		candidates = v
	default:
		return nil
	}
	var networks []netip.Prefix
	for _, candidate := range candidates {
		chunk := pystr.Strip(candidate)
		if chunk == "" {
			continue
		}
		network, ok := pyipaddress.IPNetwork(chunk, false)
		if !ok {
			slog.Warn(fmt.Sprintf("[msgraph_webhook] Ignoring invalid allowed_source_cidrs entry: %s", pyrepr.Repr(chunk)))
			continue
		}
		networks = append(networks, network)
	}
	return networks
}

// Whether the resource is the prefix itself or lies under it
func msgraphPrefixMatch(resource, prefix string) bool {
	return resource == prefix || strings.HasPrefix(resource, prefix+"/")
}

// First n code points of text, the Python slice up to n
func pySliceRunes(text string, n int) string {
	count := 0
	for i := range text {
		if count == n {
			return text[:i]
		}
		count++
	}
	return text
}

// Substitutes dotted key fields from the payload, unknown keys staying literal
func msgraphRenderTemplate(template string, payload *pyjson.Object) (string, error) {
	var failure error
	out := msgraphTemplateKeyRE.ReplaceAllStringFunc(template, func(match string) string {
		key := match[1 : len(match)-1]
		literal := "{" + key + "}"
		var value any = payload
		for _, part := range strings.Split(key, ".") {
			obj, ok := value.(*pyjson.Object)
			if !ok {
				return literal
			}
			value = obj.GetOr(part, literal)
		}
		switch value.(type) {
		case *pyjson.Object, []any:
			text, err := pyjson.Dumps(value, pyjson.SortKeys(true))
			if err != nil {
				failure = err
				return match
			}
			return pySliceRunes(text, 2000)
		}
		return pyrepr.Str(value)
	})
	return out, failure
}

// MSGraphWebhookAdapter receives Microsoft Graph change notifications and surfaces them internally
type MSGraphWebhookAdapter struct {
	*BasePlatformAdapter
	host                  *string
	port                  int
	webhookPath           string
	healthPath            string
	acceptedResources     []string
	clientState           *string
	maxSeenReceipts       int
	maxBodyBytes          int64
	allowedSourceNetworks []netip.Prefix
	runner                *WebAppRunner
	notificationScheduler NotificationScheduler
	seenReceipts          map[string]struct{}
	seenReceiptOrder      []string
	acceptedCount         int
	duplicateCount        int
	stateMu               sync.Mutex
}

// Class attributes of the Graph webhook adapter, the base defaults serving the profile prefix
func msgraphWebhookAdapterTraits() AdapterTraits {
	traits := DefaultAdapterTraits()
	traits.ServesProfilePrefix = true
	return traits
}

// Adapter for a Graph webhook platform config, the ValueError of a malformed port or limit
func NewMSGraphWebhookAdapter(config *PlatformConfig) (*MSGraphWebhookAdapter, error) {
	a := &MSGraphWebhookAdapter{seenReceipts: map[string]struct{}{}}
	a.BasePlatformAdapter = NewBasePlatformAdapter(config, PlatformMSGraphWebhook, a)
	a.Traits = msgraphWebhookAdapterTraits()
	extra := config.Extra
	if rawHost := extra["host"]; pyrepr.Truthy(rawHost) {
		host := pyrepr.Str(rawHost)
		a.host = &host
	}
	port, err := msgraphIntExtra(extra, "port", msgraphDefaultPort)
	if err != nil {
		return nil, err
	}
	a.port = int(port)
	webhookPath, ok := extra["webhook_path"]
	if !ok {
		webhookPath = msgraphDefaultWebhookPath
	}
	a.webhookPath = msgraphNormalizePath(webhookPath)
	healthPath, ok := extra["health_path"]
	if !ok {
		healthPath = "/health"
	}
	a.healthPath = msgraphNormalizePath(healthPath)
	resources, err := msgraphIterate(extra["accepted_resources"])
	if err != nil {
		return nil, err
	}
	for _, value := range resources {
		if text := pystr.Strip(pyrepr.Str(value)); text != "" {
			a.acceptedResources = append(a.acceptedResources, text)
		}
	}
	a.clientState = msgraphStringOrNone(extra["client_state"])
	seen, err := msgraphIntExtra(extra, "max_seen_receipts", msgraphDefaultMaxSeenReceipts)
	if err != nil {
		return nil, err
	}
	a.maxSeenReceipts = int(max(1, seen))
	bodyBytes, err := msgraphIntExtra(extra, "max_body_bytes", msgraphDefaultMaxBodyBytes)
	if err != nil {
		return nil, err
	}
	a.maxBodyBytes = max(1, bodyBytes)
	a.allowedSourceNetworks = msgraphParseAllowedSourceCIDRs(extra["allowed_source_cidrs"])
	return a, nil
}

// Python int of an extra value, the default when the key is absent
func msgraphIntExtra(extra map[string]any, key string, def int64) (int64, error) {
	value, ok := extra[key]
	if !ok {
		return def, nil
	}
	return pyint.Int64(value)
}

// Installs the plugin owned notification pipeline, nil returning ingress to gateway inference
func (a *MSGraphWebhookAdapter) SetNotificationScheduler(scheduler NotificationScheduler) {
	a.stateMu.Lock()
	a.notificationScheduler = scheduler
	a.stateMu.Unlock()
}

// Installed notification scheduler
func (a *MSGraphWebhookAdapter) scheduler() NotificationScheduler {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.notificationScheduler
}

// Python attribute of the adapter by name, the client state secret as its token
func (a *MSGraphWebhookAdapter) AdapterAttr(name string) any {
	if name != "token" || a.clientState == nil {
		return nil
	}
	return *a.clientState
}

// Bound host, nil for the dual stack default
func (a *MSGraphWebhookAdapter) ListenHost() *string { return a.host }

// Configured listen port
func (a *MSGraphWebhookAdapter) ListenPort() int { return a.port }

// Webhook callback route path
func (a *MSGraphWebhookAdapter) WebhookPath() string { return a.webhookPath }

// Running listener, nil before connect or in shared listener mode
func (a *MSGraphWebhookAdapter) Runner() *WebAppRunner { return a.runner }

// Replaces the source allowlist
func (a *MSGraphWebhookAdapter) SetAllowedSourceNetworks(networks []netip.Prefix) {
	a.stateMu.Lock()
	a.allowedSourceNetworks = networks
	a.stateMu.Unlock()
}

// Forgets every remembered notification receipt
func (a *MSGraphWebhookAdapter) ClearSeenReceipts() {
	a.stateMu.Lock()
	clear(a.seenReceipts)
	a.seenReceiptOrder = nil
	a.stateMu.Unlock()
}

// Whether a network accessible bind lacks the source allowlist it requires
func (a *MSGraphWebhookAdapter) sourceAllowlistRequiredButMissing(ctx context.Context) bool {
	hostIsPublic := a.host == nil || IsNetworkAccessible(ctx, *a.host)
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return hostIsPublic && len(a.allowedSourceNetworks) == 0
}

// Starts the webhook listener, false without a client state or with a public bind lacking an allowlist
func (a *MSGraphWebhookAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.clientState == nil {
		slog.Error("[msgraph_webhook] Refusing to start without extra.client_state configured")
		return false, nil
	}
	if a.sourceAllowlistRequiredButMissing(ctx) {
		slog.Error(fmt.Sprintf("[msgraph_webhook] Refusing to start: binding to %s requires extra.allowed_source_cidrs. "+
			"Configure the Microsoft Graph source CIDRs or bind to loopback (127.0.0.1/::1) behind a "+
			"tunnel or reverse proxy.", pyHostText(a.host)))
		return false, nil
	}
	app := NewWebApplication(a.maxBodyBytes)
	if err := app.AddGet(a.healthPath, a.handleHealth); err != nil {
		return false, err
	}
	if err := app.AddGet(a.webhookPath, a.handleValidation); err != nil {
		return false, err
	}
	if err := app.AddPost(a.webhookPath, a.handleNotification); err != nil {
		return false, err
	}
	a.WirePluginHandlers(ctx, app)
	runner, err := BindListener(ctx, a, app, a.host, a.port, a.webhookPath, ListenOptions{})
	if err != nil {
		return false, err
	}
	a.runner = runner
	a.MarkConnected(ctx, "")
	if a.runner != nil {
		slog.Info(fmt.Sprintf("[msgraph_webhook] Listening on %s:%d%s", pyHostText(a.host), a.port, a.webhookPath))
	}
	return true, nil
}

// Python str of an optional host
func pyHostText(host *string) string {
	if host == nil {
		return "None"
	}
	return *host
}

// Stops the listener and marks the adapter disconnected
func (a *MSGraphWebhookAdapter) Disconnect(ctx context.Context) error {
	if a.runner != nil {
		if err := a.runner.Cleanup(ctx); err != nil {
			return err
		}
		a.runner = nil
	}
	a.MarkDisconnected(ctx)
	return nil
}

// Logs the response the internal turn produced, Graph has no reply channel
func (a *MSGraphWebhookAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error) {
	slog.Info(fmt.Sprintf("[msgraph_webhook] Response for %s: %s", chatID, pySliceRunes(content, 200)))
	return SendResult{Success: true}, nil
}

// Chat info of a webhook subscription chat
func (a *MSGraphWebhookAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "webhook"}, nil
}

// Health report with the accepted and duplicate counters, 403 for a disallowed source
func (a *MSGraphWebhookAdapter) handleHealth(ctx context.Context, request *WebRequest) (*WebResponse, error) {
	if !a.sourceIPAllowed(ctx, request) {
		return NewWebResponse(http.StatusForbidden, nil), nil
	}
	a.stateMu.Lock()
	accepted, duplicates := a.acceptedCount, a.duplicateCount
	a.stateMu.Unlock()
	return WebJSONResponse(pyjson.ObjectOf("status", "ok", "platform", string(a.Platform), "webhook_path", a.webhookPath,
		"accepted", accepted, "duplicates", duplicates), http.StatusOK, nil)
}

// Graph subscription validation handshake echoing validationToken as text, a bare GET rejected
func (a *MSGraphWebhookAdapter) handleValidation(ctx context.Context, request *WebRequest) (*WebResponse, error) {
	if !a.sourceIPAllowed(ctx, request) {
		return NewWebResponse(http.StatusForbidden, nil), nil
	}
	token := request.QueryGet("validationToken")
	if token == "" {
		return NewWebResponse(http.StatusBadRequest, nil), nil
	}
	return WebTextResponse(http.StatusOK, token, "text/plain", nil), nil
}

// Classifies and schedules one notification as accepted, duplicate, auth or other
func (a *MSGraphWebhookAdapter) ingestNotification(ctx context.Context, raw any) (string, error) {
	original, ok := raw.(*pyjson.Object)
	if !ok {
		return "other", nil
	}
	notification := original.Copy()
	resource := ""
	if value, _ := notification.Get("resource"); pyrepr.Truthy(value) {
		resource = pyrepr.Str(value)
	}
	if !a.resourceAccepted(resource) {
		return "other", nil
	}
	verified, err := a.verifyClientState(notification)
	if err != nil {
		return "", err
	}
	if !verified {
		return "auth", nil
	}
	var receiptKey *string
	explicitID := ""
	if value, _ := notification.Get("id"); pyrepr.Truthy(value) {
		explicitID = pystr.Strip(pyrepr.Str(value))
	}
	if explicitID != "" {
		key := "id:" + explicitID
		receiptKey = &key
	}
	if receiptKey != nil && a.scheduler() != nil {
		a.stateMu.Lock()
		_, seen := a.seenReceipts[*receiptKey]
		a.stateMu.Unlock()
		if seen {
			return "duplicate", nil
		}
	}
	event, err := a.buildMessageEvent(ctx, notification, receiptKey)
	if err != nil {
		return "", err
	}
	if err := a.scheduleNotification(ctx, notification, event); err != nil {
		return "", err
	}
	a.stateMu.Lock()
	if receiptKey != nil {
		a.rememberReceipt(*receiptKey)
	}
	a.acceptedCount++
	a.stateMu.Unlock()
	return "accepted", nil
}

// Answers a notification batch, 202 when any item is ingested or deduped and 403 when every item failed auth
func (a *MSGraphWebhookAdapter) handleNotification(ctx context.Context, request *WebRequest) (*WebResponse, error) {
	if !a.sourceIPAllowed(ctx, request) {
		return NewWebResponse(http.StatusForbidden, nil), nil
	}
	if token := request.QueryGet("validationToken"); token != "" {
		return WebTextResponse(http.StatusOK, token, "text/plain", nil), nil
	}
	status, notifications, err := a.readNotifications(request)
	if err != nil {
		return nil, err
	}
	if status != 0 {
		return NewWebResponse(status, nil), nil
	}
	counts := map[string]int{"accepted": 0, "duplicate": 0, "auth": 0, "other": 0}
	for _, raw := range notifications {
		outcome, err := a.ingestNotification(ctx, raw)
		if err != nil {
			slog.Error("[msgraph_webhook] Notification admission failed", "error", err)
			return NewWebResponse(http.StatusServiceUnavailable, nil), nil
		}
		counts[outcome]++
	}
	a.stateMu.Lock()
	a.duplicateCount += counts["duplicate"]
	a.stateMu.Unlock()
	if counts["accepted"] > 0 || counts["duplicate"] > 0 {
		return NewWebResponse(http.StatusAccepted, nil), nil
	}
	if counts["auth"] > 0 && counts["other"] == 0 {
		return NewWebResponse(http.StatusForbidden, nil), nil
	}
	return NewWebResponse(http.StatusBadRequest, nil), nil
}

// Reads and validates the POST body, an error status or the notification list
func (a *MSGraphWebhookAdapter) readNotifications(request *WebRequest) (int, []any, error) {
	if request.ContentLength != nil && *request.ContentLength > a.maxBodyBytes {
		return http.StatusRequestEntityTooLarge, nil, nil
	}
	rawBody, err := request.Read()
	if err != nil {
		return http.StatusBadRequest, nil, nil
	}
	if int64(len(rawBody)) > a.maxBodyBytes {
		return http.StatusRequestEntityTooLarge, nil, nil
	}
	if !utf8.Valid(rawBody) {
		return http.StatusBadRequest, nil, nil
	}
	body, err := pyjson.Loads(string(rawBody))
	if err != nil {
		var decodeErr *pyjson.DecodeError
		if errors.As(err, &decodeErr) {
			return http.StatusBadRequest, nil, nil
		}
		return 0, nil, err
	}
	obj, ok := body.(*pyjson.Object)
	if !ok {
		return http.StatusBadRequest, nil, nil
	}
	value, _ := obj.Get("value")
	notifications, ok := value.([]any)
	if !ok {
		return http.StatusBadRequest, nil, nil
	}
	return 0, notifications, nil
}

// Whether the request peer may reach the endpoint, network accessible binds failing closed without an allowlist
func (a *MSGraphWebhookAdapter) sourceIPAllowed(ctx context.Context, request *WebRequest) bool {
	if a.sourceAllowlistRequiredButMissing(ctx) {
		return false
	}
	a.stateMu.Lock()
	networks := a.allowedSourceNetworks
	a.stateMu.Unlock()
	if len(networks) == 0 {
		return true
	}
	peer, ok := pyipaddress.IPAddress(request.Remote)
	if !ok {
		return false
	}
	for _, network := range networks {
		if pyipaddress.NetworkContains(network, peer) {
			return true
		}
	}
	return false
}

// Whether the resource lies under an accepted pattern, every resource accepted without patterns
func (a *MSGraphWebhookAdapter) resourceAccepted(resource string) bool {
	if len(a.acceptedResources) == 0 {
		return true
	}
	resource = strings.Trim(pystr.Strip(resource), "/")
	for _, raw := range a.acceptedResources {
		pattern := strings.Trim(pystr.Strip(raw), "/")
		if strings.HasSuffix(pattern, "*") {
			pattern = strings.TrimRight(pattern[:len(pattern)-1], "/")
		}
		if pattern != "" && msgraphPrefixMatch(resource, pattern) {
			return true
		}
	}
	return false
}

// Timing safe compare of the Graph supplied clientState against the configured shared secret
func (a *MSGraphWebhookAdapter) verifyClientState(notification *pyjson.Object) (bool, error) {
	expected := a.clientState
	value, _ := notification.Get("clientState")
	provided := msgraphStringOrNone(value)
	if expected == nil || provided == nil {
		return false, nil
	}
	for _, text := range []string{*provided, *expected} {
		if !utf8.ValidString(text) {
			return false, &PyError{Kind: "UnicodeEncodeError", Msg: "'utf-8' codec can't encode character: surrogates not allowed"}
		}
	}
	return subtle.ConstantTimeCompare([]byte(*provided), []byte(*expected)) == 1, nil
}

// Remembers a receipt and evicts the oldest past the bound, the caller holding the state lock
func (a *MSGraphWebhookAdapter) rememberReceipt(receiptKey string) {
	a.seenReceipts[receiptKey] = struct{}{}
	a.seenReceiptOrder = append(a.seenReceiptOrder, receiptKey)
	for len(a.seenReceiptOrder) > a.maxSeenReceipts {
		delete(a.seenReceipts, a.seenReceiptOrder[0])
		a.seenReceiptOrder = a.seenReceiptOrder[1:]
	}
}

// Internal webhook event of a notification keyed by its receipt or the digest of its content
func (a *MSGraphWebhookAdapter) buildMessageEvent(ctx context.Context, notification *pyjson.Object, receiptKey *string) (*MessageEvent, error) {
	messageID := ""
	if receiptKey != nil && *receiptKey != "" {
		messageID = *receiptKey
	} else {
		encoded, err := pyjson.Dumps(notification, pyjson.SortKeys(true))
		if err != nil {
			return nil, err
		}
		digest := sha1.Sum([]byte(encoded))
		messageID = "sha1:" + hex.EncodeToString(digest[:])
	}
	chatName, userID, userName := "msgraph/webhook", "msgraph", "Microsoft Graph"
	source := a.BuildSource(ctx, "msgraph:"+pyrepr.Str(notification.GetOr("subscriptionId", "unknown")), SourceFields{
		ChatName: &chatName, ChatType: "webhook", UserID: &userID, UserName: &userName})
	text, err := a.renderPrompt(notification)
	if err != nil {
		return nil, err
	}
	event := NewMessageEvent(text)
	event.MessageType = MessageTypeText
	event.Source = source
	event.RawMessage = notification
	event.MessageID = &messageID
	event.Internal = true
	return event, nil
}

// Prompt of a notification from the configured template, else the notification as fenced JSON
func (a *MSGraphWebhookAdapter) renderPrompt(notification *pyjson.Object) (string, error) {
	template, ok := a.Config.Extra["prompt"]
	if !ok {
		template = ""
	}
	if pyrepr.Truthy(template) {
		text, isText := template.(string)
		if !isText {
			return "", &PyError{Kind: "TypeError", Msg: fmt.Sprintf("expected string or bytes-like object, got '%s'", pyrepr.TypeName(template))}
		}
		return msgraphRenderTemplate(text, pyjson.ObjectOf("notification", notification,
			"resource", notification.GetOr("resource", ""), "change_type", notification.GetOr("changeType", ""),
			"subscription_id", notification.GetOr("subscriptionId", "")))
	}
	rendered, err := pyjson.Dumps(notification, pyjson.Indent(2), pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	return "Microsoft Graph change notification:\n\n```json\n" + pySliceRunes(rendered, 4000) + "\n```", nil
}

// Hands the notification to the plugin scheduler when one owns the route, else admits it for inference
func (a *MSGraphWebhookAdapter) scheduleNotification(ctx context.Context, notification *pyjson.Object, event *MessageEvent) error {
	if scheduler := a.scheduler(); scheduler != nil {
		return scheduler(ctx, notification, event)
	}
	_, err := a.admitNotification(ctx, event)
	return err
}

// Commits the notification as an automation input of its route through the private envelope before the ACK
func (a *MSGraphWebhookAdapter) admitNotification(ctx context.Context, event *MessageEvent) (AdmissionReceipt, error) {
	scoped, authority, err := ProducerScope(ctx, a, event)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	if err := authority.requireAdmissionOpen(false); err != nil {
		return AdmissionReceipt{}, err
	}
	provenance, err := captureProvenance(scoped, authority.Runner, event)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	if provenance == nil {
		return AdmissionReceipt{}, runtimeRefusal("permission_denied")
	}
	sid, routeKey, err := authority.reserveRoute(scoped, event.Source)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	ref, err := authority.publishRoute(scoped, event.Source, sid, routeKey)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	authority.mu.Lock()
	live, ok := authority.Sessions[ref.SessionID]
	authority.mu.Unlock()
	if !ok {
		return AdmissionReceipt{}, &pyKeyError{ref.SessionID}
	}
	route := live.Route
	source := event.Source.ToDict()
	source.Set("is_bot", event.Source.IsBot)
	messageID := ""
	if event.MessageID != nil {
		messageID = *event.MessageID
	}
	envelope := pyjson.ObjectOf("source", source, "route", route, "provenance", provenance,
		"timestamp", "1970-01-01T00:00:00+00:00", "event", pyjson.ObjectOf("message_id", messageID),
		"automation", pyjson.ObjectOf("identity", messageID, "owner", ref.SessionID))
	payload := pyjson.ObjectOf("text", event.Text, "native_text_v1", envelope)
	var scheduleErr error
	result, err := trackedWrite(scoped, authority, func(taskCtx context.Context) (any, error) {
		return authority.DB.AdmitSessionInput(taskCtx, state.SessionInput{Epoch: authority.Epoch, PrincipalID: "msgraph:" + route,
			SessionID: ref.SessionID, RequestID: messageID, Payload: payload, AuthorizeWrite: authority.admissionGate(nil, false)})
	}, func(taskCtx context.Context, written any) (any, error) {
		return authority.admitted(taskCtx, ref, event, written.(*pyjson.Object))
	}, true, func(any) { scheduleErr = authority.scheduleAdmitted(scoped, ref) })
	if err == nil {
		err = scheduleErr
	}
	if err != nil {
		return AdmissionReceipt{}, err
	}
	return result.(AdmissionReceipt), nil
}
