package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
)

const (
	webhookDefaultPort           = 8644
	webhookInsecureNoAuth        = "INSECURE_NO_AUTH"
	webhookDynamicRoutesFilename = "webhook_subscriptions.json"
	webhookRateWindowSeconds     = 60.0
	webhookV2ReplayWindowSeconds = 300
)

var (
	webhookLoopbackHosts = map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true, "ip6-localhost": true, "ip6-loopback": true}
	webhookTemplateKeyRE = regexp.MustCompile(`\{([a-zA-Z0-9_.]+)\}`)
	webhookGHTokenVars   = []string{"GH_TOKEN", "GITHUB_TOKEN"}
)

// Cron job runner a cron_job route fires, assigned at startup to the cron package's event entry point
var WebhookExecuteJobForEvent func(ctx context.Context, h, jobRef string, extraPrompt *string) (map[string]any, error)

// Durable producer admission, run finalizer and receipt authority lookup of the agent run path
var (
	webhookAdmitProducer         = AdmitProducer
	webhookFinalizeDelivery      = (*WebhookAdapter).finalizeDelivery
	webhookAuthorityForProfileID = authorityForHome
)

// Logger of the webhook adapter module
func webhookLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.platforms.webhook")
}

// Seconds since the epoch as a float, the value time.time returns
func webhookNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// First value of a header, empty when absent, the CIMultiDict get with a blank default
func webhookHeader(header http.Header, name string) string {
	values := header.Values(name)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// Provider delivery id from the GitHub, Svix, Standard Webhooks or generic header, else a fresh one
func webhookHeaderDeliveryID(header http.Header) string {
	for _, name := range []string{"X-GitHub-Delivery", "svix-id", "webhook-id", "X-Request-ID"} {
		if values := header.Values(name); len(values) > 0 {
			return values[0]
		}
	}
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// Route and profile qualified identity of one provider delivery
type webhookDeliveryIdentity struct {
	Profile    string
	Route      string
	DeliveryID string
}

// Identity with a missing profile read as the default profile
func webhookIdentityFromParts(profile *string, route, deliveryID string) webhookDeliveryIdentity {
	name := "default"
	if profile != nil && *profile != "" {
		name = *profile
	}
	return webhookDeliveryIdentity{Profile: name, Route: route, DeliveryID: deliveryID}
}

// Versioned chat id carrying the canonical JSON tuple of profile, route and delivery id
func (i webhookDeliveryIdentity) SessionChatID() string {
	payload, _ := pyjson.Dumps([]any{i.Profile, i.Route, i.DeliveryID}, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	token := strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(payload)), "=")
	return "webhook:v2:" + token
}

// Whether a host binds only to the local machine, a blank host reading as a public bind
func webhookIsLoopbackHost(host *string) bool {
	return host != nil && *host != "" && webhookLoopbackHosts[strings.ToLower(pystr.Strip(*host))]
}

// Timing safe equality of two strings compared as UTF-8 bytes
func webhookHMACStrEqual(provided, expected string) bool {
	return hmac.Equal([]byte(provided), []byte(expected))
}

// Hex HMAC SHA256 of data under the secret
func webhookHexHMAC(secret string, data []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// Whether a secret is a string with at least one non space character
func WebhookIsUsableSecret(secret any) bool {
	text, ok := secret.(string)
	return ok && pystr.Strip(text) != ""
}

// Whether an integer timestamp header lies within the replay window, a stale one logging the message
func webhookTimestampFresh(raw, staleMsg string) bool {
	stamp, err := pyint.Of(raw)
	if err != nil {
		return false
	}
	age := new(big.Int).Sub(big.NewInt(time.Now().Unix()), stamp)
	if age.Abs(age).Cmp(big.NewInt(webhookV2ReplayWindowSeconds)) > 0 {
		webhookLog().Warn(staleMsg)
		return false
	}
	return true
}

// Whether the platform webhook adapter dependencies are available
func CheckWebhookRequirements() bool { return true }

// JSON error body with the status
func webhookJSONError(message string, status int) (*WebResponse, error) {
	return WebJSONResponse(pyjson.ObjectOf("error", message), status, nil)
}

// Verifies a Svix style base64 HMAC of id, timestamp and body against any v1 entry of the header
func webhookValidateSvixSignature(body []byte, secret, msgID, timestamp, signatureHeader string) bool {
	if msgID == "" || timestamp == "" || signatureHeader == "" || secret == "" {
		return false
	}
	if !webhookTimestampFresh(timestamp, "[webhook] Svix signature timestamp outside replay window") {
		return false
	}
	var key []byte
	if strings.HasPrefix(secret, "whsec_") {
		decoded, err := pybase64.B64DecodeValidate(strings.TrimPrefix(secret, "whsec_"))
		if err != nil {
			webhookLog().Debug("[webhook] Invalid whsec_ Svix signing secret")
			return false
		}
		if strings.Trim(string(decoded), "\x00 \t\r\n\x0b\x0c") == "" {
			return false
		}
		key = decoded
	} else {
		webhookLog().Debug("[webhook] Validating Svix-style signature with raw secret")
		key = []byte(secret)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID + "." + timestamp + "."))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, part := range strings.Fields(signatureHeader) {
		version, signature, found := strings.Cut(part, ",")
		if found && version == "v1" && webhookHMACStrEqual(signature, expected) {
			return true
		}
	}
	return false
}

// Value with every plain map turned into an ordered object, the dict model the adapter reads routes in
func webhookNormalize(value any) any {
	switch v := value.(type) {
	case map[string]any:
		obj, _ := pyjson.AsObject(v)
		out := pyjson.NewObject()
		for _, key := range obj.Keys {
			item, _ := obj.Get(key)
			out.Set(key, webhookNormalize(item))
		}
		return out
	case *pyjson.Object:
		if v == nil {
			return v
		}
		out := pyjson.NewObject()
		for _, key := range v.Keys {
			item, _ := v.Get(key)
			out.Set(key, webhookNormalize(item))
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = webhookNormalize(item)
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out
	}
	return value
}

// Route config as a dict, the AttributeError a get call on any other value raises
func webhookRouteObject(route any) (*pyjson.Object, error) {
	if obj, ok := route.(*pyjson.Object); ok && obj != nil {
		return obj, nil
	}
	return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(route))}
}

// One delivery info entry and the time it was recorded
type webhookDeliveryStamp struct {
	At  float64
	Key string
}

// WebhookAdapter receives signed webhook POSTs, renders them into prompts and routes the responses back
type WebhookAdapter struct {
	*BasePlatformAdapter
	host                      *string
	port                      int
	globalSecret              any
	staticRoutes              *pydict.Dict[string, any]
	dynamicRoutes             *pydict.Dict[string, any]
	dynamicRoutesStat         []any
	routes                    *pydict.Dict[string, any]
	runner                    *WebAppRunner
	v1SignatureWarned         map[string]bool
	deliveryInfo              map[string]*pyjson.Object
	deliveryInfoCreated       map[string]float64
	deliveryInfoOrder         []webhookDeliveryStamp
	seenDeliveries            map[webhookDeliveryIdentity]float64
	idempotencyTTL            int
	seenDeliveriesNextPruneAt float64
	rateCounts                map[string][]float64
	rateLimit                 int64
	maxBodyBytes              int64
	scriptTimeoutSeconds      int
	routeProcessor            *WebhookRouteProcessor
	coalescer                 *WebhookCoalescer
	stateMu                   sync.Mutex
}

// Class attributes of the webhook adapter, no interactive resume and the profile prefix served
func webhookAdapterTraits() AdapterTraits {
	traits := DefaultAdapterTraits()
	traits.InteractiveResume = false
	traits.ServesProfilePrefix = true
	return traits
}

// Adapter for a webhook platform config, the error of a malformed port, limit or route table
func NewWebhookAdapter(config *PlatformConfig) (*WebhookAdapter, error) {
	a := &WebhookAdapter{v1SignatureWarned: map[string]bool{}, deliveryInfo: map[string]*pyjson.Object{},
		deliveryInfoCreated: map[string]float64{}, seenDeliveries: map[webhookDeliveryIdentity]float64{},
		idempotencyTTL: 3600, rateCounts: map[string][]float64{}, dynamicRoutes: pydict.New[string, any]()}
	a.BasePlatformAdapter = NewBasePlatformAdapter(config, PlatformWebhook, a)
	a.Traits = webhookAdapterTraits()
	extra := config.Extra
	if rawHost := extra["host"]; pyrepr.Truthy(rawHost) {
		host := pyrepr.Str(rawHost)
		a.host = &host
	}
	port, err := msgraphIntExtra(extra, "port", webhookDefaultPort)
	if err != nil {
		return nil, err
	}
	a.port = int(port)
	secret, ok := extra["secret"]
	if !ok {
		secret = ""
	}
	a.globalSecret = webhookNormalize(secret)
	routes, ok := extra["routes"]
	if !ok {
		routes = map[string]any{}
	}
	if a.staticRoutes, err = webhookRouteTable(routes); err != nil {
		return nil, err
	}
	a.routes = pydict.Merge(a.staticRoutes)
	if a.rateLimit, err = msgraphIntExtra(extra, "rate_limit", 30); err != nil {
		return nil, err
	}
	if a.maxBodyBytes, err = msgraphIntExtra(extra, "max_body_bytes", 1_048_576); err != nil {
		return nil, err
	}
	timeout, err := msgraphIntExtra(extra, "script_timeout_seconds", WebhookDefaultScriptTimeoutSeconds)
	if err != nil {
		return nil, err
	}
	a.scriptTimeoutSeconds = int(timeout)
	a.routeProcessor = NewWebhookRouteProcessor(a.scriptTimeoutSeconds)
	a.coalescer = NewWebhookCoalescer(a.spawnAgentRun, func(template string, payload any, eventType any, routeName string) (string, error) {
		return a.RenderPrompt(template, payload, eventType, routeName)
	})
	return a, nil
}

// Ordered route table of a routes config value, the TypeError dict raises for a non mapping
func webhookRouteTable(routes any) (*pydict.Dict[string, any], error) {
	obj, ok := webhookNormalize(routes).(*pyjson.Object)
	if !ok || obj == nil {
		return nil, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(routes))}
	}
	table := pydict.New[string, any]()
	for _, name := range obj.Keys {
		route, _ := obj.Get(name)
		table.Set(name, route)
	}
	return table, nil
}

// Signing credentials of the static routes as canonical JSON, the native replay binding of the connector
func (a *WebhookAdapter) Token() (string, error) {
	perRoute := pyjson.NewObject()
	a.stateMu.Lock()
	for name, route := range a.staticRoutes.All() {
		obj, err := webhookRouteObject(route)
		if err != nil {
			a.stateMu.Unlock()
			return "", err
		}
		perRoute.Set(name, obj.GetOr("secret", a.globalSecret))
	}
	a.stateMu.Unlock()
	return pyjson.Dumps([]any{a.globalSecret, perRoute}, pyjson.SortKeys(true))
}

// Python attribute of the adapter by name, the signing credentials as its token
func (a *WebhookAdapter) AdapterAttr(name string) any {
	if name != "token" {
		return nil
	}
	token, err := a.Token()
	if err != nil {
		return nil
	}
	return token
}

// Bound host, nil for the dual stack default
func (a *WebhookAdapter) ListenHost() *string { return a.host }

// Configured listen port
func (a *WebhookAdapter) ListenPort() int { return a.port }

// Running listener, nil before connect
func (a *WebhookAdapter) Runner() *WebAppRunner { return a.runner }

// Body cap the listener and handlers enforce
func (a *WebhookAdapter) MaxBodyBytes() int64 { return a.maxBodyBytes }

// Route coalescer of the adapter
func (a *WebhookAdapter) Coalescer() *WebhookCoalescer { return a.coalescer }

// Startup check of one route, a missing secret or a public INSECURE_NO_AUTH bind failing hard
func (a *WebhookAdapter) validateRoute(name string, route any) error {
	obj, err := webhookRouteObject(route)
	if err != nil {
		return err
	}
	secret := obj.GetOr("secret", a.globalSecret)
	if !WebhookIsUsableSecret(secret) {
		return &PyError{Kind: "ValueError", Msg: fmt.Sprintf("[webhook] Route '%s' HMAC secret is missing, blank, or not a string. Set 'secret' "+
			"on the route or globally. For testing without auth, set secret to '%s'.", name, webhookInsecureNoAuth)}
	}
	if secret == webhookInsecureNoAuth && !webhookIsLoopbackHost(a.host) {
		return &PyError{Kind: "ValueError", Msg: fmt.Sprintf("[webhook] Route '%s' uses INSECURE_NO_AUTH secret but is bound to non-loopback "+
			"host '%s'. INSECURE_NO_AUTH is for local testing only. Refusing to start to prevent accidental exposure.", name, pyHostText(a.host))}
	}
	if pyrepr.Truthy(obj.GetOr("deliver_only", nil)) {
		deliver := obj.GetOr("deliver", "log")
		if !pyrepr.Truthy(deliver) || pyrepr.Equal(deliver, "log") {
			return &PyError{Kind: "ValueError", Msg: fmt.Sprintf("[webhook] Route '%s' has deliver_only=true but deliver is '%s'. Direct "+
				"delivery requires a real target (telegram, discord, slack, github_comment, etc.).", name, pyrepr.Str(deliver))}
		}
		if pyrepr.Truthy(obj.GetOr("cron_job", nil)) {
			return &PyError{Kind: "ValueError", Msg: fmt.Sprintf("[webhook] Route '%s' sets both deliver_only and cron_job. They are mutually "+
				"exclusive: deliver_only pushes the rendered template as a message, cron_job fires "+
				"an existing cron job (which handles its own delivery).", name)}
		}
	}
	return ValidateCoalesceConfig(name, obj)
}

// Starts the webhook listener after validating every route, false when the port cannot be bound
func (a *WebhookAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	a.ReloadDynamicRoutes(ctx)
	a.stateMu.Lock()
	routes := pydict.Merge(a.routes)
	a.stateMu.Unlock()
	for name, route := range routes.All() {
		if err := a.validateRoute(name, route); err != nil {
			return false, err
		}
	}
	app := NewWebApplication(a.maxBodyBytes)
	for _, route := range []struct {
		method, path string
		handler      WebHandler
	}{{"GET", "/health", a.handleHealth}, {"POST", "/webhooks/{route_name}", a.handleWebhook},
		{"POST", "/p/{profile}/webhooks/{route_name}", a.handleWebhook}, {"*", "/p/{profile}/{tail:.*}", a.handleProfileIngress}} {
		var err error
		if route.method == "GET" {
			err = app.AddGet(route.path, route.handler)
		} else {
			err = app.AddRoute(route.method, route.path, route.handler)
		}
		if err != nil {
			return false, err
		}
	}
	runner, err := StartTCPSite(ctx, app, a.host, a.port, "webhook", AccessLog{})
	if err != nil {
		hostText := "all IPv4+IPv6 interfaces"
		if a.host != nil {
			hostText = *a.host
		}
		webhookLog().Error(fmt.Sprintf("[webhook] Could not bind %s:%d: %s. Set a different host or port in config.yaml under "+
			"platforms.webhook.extra.", hostText, a.port, err.Error()))
		return false, nil
	}
	a.runner = runner
	a.MarkConnected(ctx, ListenerBaseURL(a.host, a.port))
	hostText := "* (all interfaces, IPv4+IPv6)"
	if a.host != nil {
		hostText = *a.host
	}
	names := strings.Join(routes.Keys(), ", ")
	if names == "" {
		names = "(none configured)"
	}
	webhookLog().Info(fmt.Sprintf("[webhook] Listening on %s:%d — routes: %s", hostText, a.port, names))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Flushes buffered coalesced events, stops the listener and marks the adapter disconnected
func (a *WebhookAdapter) Disconnect(ctx context.Context) error {
	a.coalescer.Flush()
	if a.runner != nil {
		if err := a.runner.Cleanup(ctx); err != nil {
			return err
		}
		a.runner = nil
	}
	a.MarkDisconnected(ctx)
	webhookLog().Info("[webhook] Disconnected")
	return nil
}

// Delivers the agent response to the destination retained for its opaque chat id
func (a *WebhookAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error) {
	if IsAutonomousSilenceResponse(content) {
		webhookLog().Info(fmt.Sprintf("[webhook] Response for %s is a silence marker — not delivering", chatID))
		return SendResult{Success: true}, nil
	}
	delivery, err := webhookRetainedDestination(ctx, a, chatID)
	if err != nil {
		webhookLog().Warn(fmt.Sprintf("[webhook] Destination unavailable for %s", chatID), "error", err)
		return SendResult{Success: false, Error: "Webhook destination unavailable or unauthorized"}, nil
	}
	return a.deliverTo(ctx, delivery, chatID, content)
}

// Sends content to a validated destination by its deliver type
func (a *WebhookAdapter) deliverTo(ctx context.Context, delivery *pyjson.Object, chatID, content string) (SendResult, error) {
	raw, found := delivery.Get("deliver")
	if !found {
		return SendResult{}, &pyKeyError{"deliver"}
	}
	deliverType := pyrepr.Str(raw)
	switch {
	case pyrepr.Equal(raw, "log"):
		webhookLog().Info(fmt.Sprintf("[webhook] Response for %s: %s", chatID, pySliceRunes(content, 200)))
		return SendResult{Success: true}, nil
	case pyrepr.Equal(raw, "github_comment"):
		return a.deliverGitHubComment(ctx, content, delivery)
	case a.GatewayRunner != nil && webhookKnownPlatform(ctx, deliverType):
		return a.deliverCrossPlatform(ctx, raw, content, delivery)
	}
	webhookLog().Warn("[webhook] Unknown deliver type: " + deliverType)
	return SendResult{Success: false, Error: "Unknown deliver type: " + deliverType}, nil
}

// Drops delivery info entries older than the idempotency window, the caller holding the state lock
func (a *WebhookAdapter) pruneDeliveryInfo(now float64) {
	if len(a.deliveryInfoOrder) < len(a.deliveryInfoCreated) {
		order := make([]webhookDeliveryStamp, 0, len(a.deliveryInfoCreated))
		for key, at := range a.deliveryInfoCreated {
			order = append(order, webhookDeliveryStamp{At: at, Key: key})
		}
		sort.SliceStable(order, func(i, j int) bool { return order[i].At < order[j].At })
		a.deliveryInfoOrder = order
	}
	cutoff := now - float64(a.idempotencyTTL)
	for len(a.deliveryInfoOrder) > 0 && a.deliveryInfoOrder[0].At < cutoff {
		head := a.deliveryInfoOrder[0]
		a.deliveryInfoOrder = a.deliveryInfoOrder[1:]
		if created, ok := a.deliveryInfoCreated[head.Key]; ok && created == head.At {
			delete(a.deliveryInfo, head.Key)
			delete(a.deliveryInfoCreated, head.Key)
		}
	}
}

// Prunes expired delivery ids at most once per interval, the caller holding the state lock
func (a *WebhookAdapter) pruneSeenDeliveries(now float64) {
	if now < a.seenDeliveriesNextPruneAt {
		return
	}
	cutoff := now - float64(a.idempotencyTTL)
	for identity, seenAt := range a.seenDeliveries {
		if seenAt < cutoff {
			delete(a.seenDeliveries, identity)
		}
	}
	a.seenDeliveriesNextPruneAt = now + min(60.0, max(1.0, float64(a.idempotencyTTL)/10))
}

// Records a hit and reports whether the route is still within its per minute limit
func (a *WebhookAdapter) recordRateLimitHit(routeName string, now float64) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	window := a.rateCounts[routeName]
	cutoff := now - webhookRateWindowSeconds
	for len(window) > 0 && window[0] < cutoff {
		window = window[1:]
	}
	if int64(len(window)) >= a.rateLimit {
		a.rateCounts[routeName] = window
		return false
	}
	a.rateCounts[routeName] = append(window, now)
	return true
}

// Whether a route and profile qualified delivery is new within the idempotency window, recording it
func (a *WebhookAdapter) recordDeliveryID(identity webhookDeliveryIdentity, now float64) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if seenAt, seen := a.seenDeliveries[identity]; seen {
		if now-seenAt < float64(a.idempotencyTTL) {
			return false
		}
		delete(a.seenDeliveries, identity)
	}
	a.seenDeliveries[identity] = now
	if int64(len(a.seenDeliveries)) > max(a.rateLimit*2, 128) {
		a.pruneSeenDeliveries(now)
	}
	return true
}

// Chat info of a webhook delivery chat
func (a *WebhookAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "webhook"}, nil
}

// Per route toolsets override keyed on the authenticated webhook user id, false when the route sets none
func (a *WebhookAdapter) ToolsetsForSource(source *SessionSource) ([]string, bool) {
	userID := ""
	if source != nil && source.UserID != nil {
		userID = *source.UserID
	}
	if !strings.HasPrefix(userID, "webhook:") {
		return nil, false
	}
	a.stateMu.Lock()
	route, _ := a.routes.Get(userID[len("webhook:"):])
	a.stateMu.Unlock()
	obj, ok := route.(*pyjson.Object)
	if !ok || obj == nil {
		return nil, false
	}
	toolsets, ok := obj.GetOr("toolsets", nil).([]any)
	if !ok {
		return nil, false
	}
	var out []string
	for _, item := range toolsets {
		if text := pystr.Strip(pyrepr.Str(item)); text != "" {
			out = append(out, text)
		}
	}
	return out, len(out) > 0
}

// Health check of the listener
func (a *WebhookAdapter) handleHealth(ctx context.Context, request *WebRequest) (*WebResponse, error) {
	return WebJSONResponse(pyjson.ObjectOf("status", "ok", "platform", "webhook"), http.StatusOK, nil)
}

// Whether a dynamic route has a usable secret, loopback for INSECURE_NO_AUTH and a valid coalesce block
func (a *WebhookAdapter) dynamicRouteAllowed(name string, route *pyjson.Object) bool {
	secret := route.GetOr("secret", a.globalSecret)
	if !WebhookIsUsableSecret(secret) {
		webhookLog().Warn(fmt.Sprintf("[webhook] Dynamic route '%s' skipped: 'secret' is missing, blank, or not a string. "+
			"Set a valid HMAC secret, or use '%s' to explicitly disable auth (testing only).", name, webhookInsecureNoAuth))
		return false
	}
	if secret == webhookInsecureNoAuth && !webhookIsLoopbackHost(a.host) {
		webhookLog().Warn(fmt.Sprintf("[webhook] Dynamic route '%s' skipped: INSECURE_NO_AUTH is only allowed on loopback "+
			"hosts. Current host: '%s'.", name, pyHostText(a.host)))
		return false
	}
	if err := ValidateCoalesceConfig(name, route); err != nil {
		webhookLog().Warn(fmt.Sprintf("[webhook] Dynamic route '%s' skipped: %s", name, pyrepr.ExceptionMessage(err)))
		return false
	}
	return true
}

// Reloads agent created subscriptions when the stat identity of the subscriptions file changes
func (a *WebhookAdapter) ReloadDynamicRoutes(ctx context.Context) {
	log := webhookLog()
	subsPath := filepath.Join(home.Dir(ctx), webhookDynamicRoutesFilename)
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	statKey, err := config.FileCacheVersion(subsPath, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if a.dynamicRoutes.Len() > 0 {
				a.dynamicRoutes, a.routes = pydict.New[string, any](), pydict.Merge(a.staticRoutes)
				log.Debug("[webhook] Dynamic subscriptions file removed, cleared dynamic routes")
			}
			a.dynamicRoutesStat = nil
			return
		}
		log.Error("[webhook] Failed to read dynamic routes: " + pyrepr.ExceptionMessage(err))
		return
	}
	if a.dynamicRoutesStat != nil && pyrepr.Equal(statKey, a.dynamicRoutesStat) {
		return
	}
	raw, err := os.ReadFile(subsPath)
	if err != nil {
		log.Error("[webhook] Failed to read dynamic routes: " + pyrepr.ExceptionMessage(err))
		return
	}
	var data any
	if utf8.Valid(raw) {
		data, err = pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	} else {
		err = &PyError{Kind: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode bytes: invalid start byte"}
	}
	if err != nil {
		a.dynamicRoutesStat = statKey
		log.Error("[webhook] Failed to parse dynamic routes: " + pyrepr.ExceptionMessage(err))
		return
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		a.dynamicRoutesStat = statKey
		return
	}
	dynamic := pydict.New[string, any]()
	for _, name := range obj.Keys {
		value, _ := obj.Get(name)
		route, isDict := value.(*pyjson.Object)
		if !isDict || a.staticRoutes.Has(name) || !a.dynamicRouteAllowed(name, route) {
			continue
		}
		dynamic.Set(name, route)
	}
	a.dynamicRoutes = dynamic
	a.routes = pydict.Merge(dynamic, a.staticRoutes)
	a.dynamicRoutesStat = statKey
	names := strings.Join(dynamic.Keys(), ", ")
	if names == "" {
		names = "(none)"
	}
	log.Info(fmt.Sprintf("[webhook] Reloaded %d dynamic route(s): %s", dynamic.Len(), names))
}

// Route of a name in the live table
func (a *WebhookAdapter) WebhookRoute(name string) (any, bool) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.routes.Get(name)
}

// Names of the live routes in table order
func (a *WebhookAdapter) RouteNames() []string {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	return a.routes.Keys()
}

// Delivery info recorded for a session chat id, nil when absent
func (a *WebhookAdapter) DeliveryInfo(chatID string) any {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if info, ok := a.deliveryInfo[chatID]; ok {
		return info
	}
	return nil
}

// Forwards a profile prefixed request to the served profile's shared ingress app
func (a *WebhookAdapter) handleProfileIngress(ctx context.Context, request *WebRequest) (*WebResponse, error) {
	profile, rejected := a.resolveRequestProfile(ctx, request)
	if rejected || profile == nil {
		return webhookJSONError("Unknown or unconfigured profile", http.StatusNotFound)
	}
	return DispatchProfileIngress(ctx, a.GatewayRunner, *profile, request.MatchInfo["tail"], request, false)
}

// Profile a request prefix names, nil without a prefix or for this gateway's own profile, rejected when not served
func (a *WebhookAdapter) resolveRequestProfile(ctx context.Context, request *WebRequest) (*string, bool) {
	profile := pystr.Strip(request.MatchInfo["profile"])
	if profile == "" {
		return nil, false
	}
	multiplex := false
	if holder, ok := a.GatewayRunner.(gatewayConfigRunner); ok && holder != nil {
		if cfg := holder.GatewayConfig(); cfg != nil && cfg.MultiplexProfiles != nil {
			multiplex = *cfg.MultiplexProfiles
		}
	}
	if !multiplex {
		if webhookProfileMatchesHome(ctx, profile) {
			return nil, false
		}
		return nil, true
	}
	served, err := profiles.ProfilesToServe(ctx, true, false, false)
	if err != nil {
		return nil, true
	}
	for _, entry := range served {
		if entry.Name == profile {
			return &profile, false
		}
	}
	return nil, true
}

// Whether a prefix names the profile served from the current home, any failure reading as no
func webhookProfileMatchesHome(ctx context.Context, profile string) (matches bool) {
	defer func() {
		if recover() != nil {
			matches = false
		}
	}()
	return profiles.ProfileMatchesHome(ctx, profile, "")
}

// Whether a route is bound to the request profile, an omitted binding meaning default
func webhookRouteAllowsProfile(routeConfig *pyjson.Object, requestProfile *string) bool {
	configured, found := routeConfig.Get("profile")
	if !found {
		configured = "default"
	}
	text, ok := configured.(string)
	if !ok || pystr.Strip(text) == "" {
		return false
	}
	want := "default"
	if requestProfile != nil && *requestProfile != "" {
		want = *requestProfile
	}
	return pystr.Strip(text) == want
}

// Runtime scope of a resolved profile prefix, the context unchanged for a bare route
func (a *WebhookAdapter) WebhookProfileScope(ctx context.Context, profile *string) (context.Context, error) {
	if profile == nil || *profile == "" {
		return ctx, nil
	}
	dir, err := profiles.GetProfileDir(*profile)
	if err != nil {
		return nil, err
	}
	return ProfileRuntimeScope(ctx, dir, true), nil
}

// Body of an authenticated request, or the size, read, secret or signature error response
func (a *WebhookAdapter) readAuthenticatedBody(request *WebRequest, routeName string, routeConfig *pyjson.Object) ([]byte, *WebResponse, error) {
	if request.ContentLength != nil && *request.ContentLength > a.maxBodyBytes {
		resp, err := webhookJSONError("Payload too large", http.StatusRequestEntityTooLarge)
		return nil, resp, err
	}
	rawBody, err := request.Read()
	if err != nil {
		var tooLarge *WebHTTPError
		if errors.As(err, &tooLarge) && tooLarge.Status == http.StatusRequestEntityTooLarge {
			resp, err := webhookJSONError("Payload too large", http.StatusRequestEntityTooLarge)
			return nil, resp, err
		}
		webhookLog().Error("[webhook] Failed to read body: " + pyrepr.ExceptionMessage(err))
		resp, err := webhookJSONError("Bad request", http.StatusBadRequest)
		return nil, resp, err
	}
	if int64(len(rawBody)) > a.maxBodyBytes {
		resp, err := webhookJSONError("Payload too large", http.StatusRequestEntityTooLarge)
		return nil, resp, err
	}
	secret := routeConfig.GetOr("secret", a.globalSecret)
	if !WebhookIsUsableSecret(secret) {
		webhookLog().Error(fmt.Sprintf("[webhook] Route %s HMAC secret is missing, blank, or not a string; refusing request", routeName))
		resp, err := webhookJSONError("Webhook route is missing an HMAC secret", http.StatusForbidden)
		return nil, resp, err
	}
	if secret != webhookInsecureNoAuth && !a.ValidateSignature(request, rawBody, secret.(string)) {
		webhookLog().Warn("[webhook] Invalid signature for route " + routeName)
		resp, err := webhookJSONError("Invalid signature", http.StatusUnauthorized)
		return nil, resp, err
	}
	return rawBody, nil, nil
}

// Body parsed as JSON, else form encoded, false when neither parses
func webhookParseBody(rawBody []byte) (any, bool, error) {
	decoded, err := pyjson.LoadsBytes(rawBody)
	if err == nil {
		return decoded, true, nil
	}
	var decodeErr *pyjson.DecodeError
	if !errors.As(err, &decodeErr) {
		return nil, false, err
	}
	if !utf8.Valid(rawBody) {
		return nil, false, nil
	}
	form := pyjson.NewObject()
	for _, pair := range pyurl.ParseQSL(string(rawBody), false) {
		form.Set(pair[0], pair[1])
	}
	return form, true, nil
}

// Pushes the rendered prompt straight to the route target, skipping the agent
func (a *WebhookAdapter) handleDeliverOnly(ctx context.Context, prompt string, payload any, routeConfig *pyjson.Object, routeName string,
	eventType any, deliveryID string, profile *string) (*WebResponse, error) {
	extra, err := a.renderDeliveryExtra(routeConfig.GetOr("deliver_extra", pyjson.NewObject()), payload)
	if err != nil {
		return nil, err
	}
	var profileValue any
	if profile != nil {
		profileValue = *profile
	}
	delivery := pyjson.ObjectOf("deliver", routeConfig.GetOr("deliver", "log"), "payload", payload, "profile", profileValue,
		"deliver_extra", extra, "route", routeName, "mirror", routeConfig.GetOr("mirror_to_session", nil) == true)
	target := delivery.GetOr("deliver", nil)
	webhookLog().Info(fmt.Sprintf("[webhook] direct-deliver event=%s route=%s target=%s msg_len=%d delivery=%s", pyrepr.Str(eventType),
		routeName, pyrepr.Str(target), runeLen(prompt), deliveryID))
	failed := pyjson.ObjectOf("status", "error", "error", "Delivery failed", "delivery_id", deliveryID)
	result, err := a.directDeliver(ctx, prompt, delivery)
	if err != nil {
		webhookLog().Error(fmt.Sprintf("[webhook] direct-deliver failed route=%s delivery=%s", routeName, deliveryID), "error", err)
		return WebJSONResponse(failed, http.StatusBadGateway, nil)
	}
	if result.Success {
		return WebJSONResponse(pyjson.ObjectOf("status", "delivered", "route", routeName, "target", target,
			"delivery_id", deliveryID), http.StatusOK, nil)
	}
	webhookLog().Warn(fmt.Sprintf("[webhook] direct-deliver target rejected route=%s target=%s error=%s", routeName,
		pyrepr.Str(target), pyrepr.Str(result.Error)))
	return WebJSONResponse(failed, http.StatusBadGateway, nil)
}

// Fires an existing cron job for the event with the rendered prompt as per run context, answering 202 at once
func (a *WebhookAdapter) handleCronTrigger(ctx context.Context, prompt string, routeConfig *pyjson.Object, routeName string, eventType any,
	deliveryID string, profile *string) (*WebResponse, error) {
	rawJob, found := routeConfig.Get("cron_job")
	if !found {
		return nil, &pyKeyError{"cron_job"}
	}
	jobRef := pyrepr.Str(rawJob)
	eventContext := fmt.Sprintf("This run was triggered by webhook event '%s' on route '%s' (not the schedule).\n\n%s",
		pyrepr.Str(eventType), routeName, prompt)
	webhookLog().Info(fmt.Sprintf("[webhook] cron-trigger event=%s route=%s job=%s delivery=%s", pyrepr.Str(eventType), routeName, jobRef, deliveryID))
	task := spawnAdapterTask(context.WithoutCancel(ctx), func(taskCtx context.Context) error {
		a.fireCronJob(taskCtx, jobRef, eventContext, routeName, profile)
		return nil
	})
	a.trackBackgroundTask(task)
	return WebJSONResponse(pyjson.ObjectOf("status", "accepted", "route", routeName, "cron_job", jobRef, "event", eventType,
		"delivery_id", deliveryID), http.StatusAccepted, nil)
}

// Runs the cron job under the routed profile scope, logging an unclean or failed run
func (a *WebhookAdapter) fireCronJob(ctx context.Context, jobRef, eventContext, routeName string, profile *string) {
	failed := func(err any) {
		webhookLog().Error(fmt.Sprintf("[webhook] cron-trigger failed job=%s route=%s", jobRef, routeName), "error", err)
	}
	defer func() {
		if rec := recover(); rec != nil {
			failed(rec)
		}
	}()
	scoped, err := a.WebhookProfileScope(ctx, profile)
	if err != nil {
		failed(err)
		return
	}
	if WebhookExecuteJobForEvent == nil {
		failed(&PyError{Kind: "ImportError", Msg: "cannot import name 'execute_job_for_event' from 'tools.cronjob_tools'"})
		return
	}
	result, err := WebhookExecuteJobForEvent(scoped, home.Dir(scoped), jobRef, &eventContext)
	if err != nil {
		failed(err)
		return
	}
	if !pyrepr.Truthy(result["success"]) {
		webhookLog().Warn(fmt.Sprintf("[webhook] cron-trigger job=%s route=%s did not complete cleanly: %s", jobRef, routeName,
			pyrepr.Str(result["error"])))
	}
}

// Route, profile and error response of a POST, the route nil whenever the response is set
func (a *WebhookAdapter) resolveRoute(ctx context.Context, request *WebRequest) (string, *pyjson.Object, *string, *WebResponse, error) {
	a.ReloadDynamicRoutes(ctx)
	routeName := request.MatchInfo["route_name"]
	a.stateMu.Lock()
	route, _ := a.routes.Get(routeName)
	a.stateMu.Unlock()
	profile, rejected := a.resolveRequestProfile(ctx, request)
	if rejected {
		resp, err := webhookJSONError("Unknown or unconfigured profile", http.StatusNotFound)
		return routeName, nil, nil, resp, err
	}
	if !pyrepr.Truthy(route) {
		resp, err := webhookJSONError("Unknown route: "+routeName, http.StatusNotFound)
		return routeName, nil, profile, resp, err
	}
	routeConfig, err := webhookRouteObject(route)
	if err != nil {
		return routeName, nil, profile, nil, err
	}
	if !webhookRouteAllowsProfile(routeConfig, profile) {
		label := "default"
		if profile != nil {
			label = *profile
		}
		webhookLog().Warn(fmt.Sprintf("[webhook] Route %s is not authorized for profile %s", routeName, pyrepr.Repr(label)))
		resp, err := webhookJSONError("Unknown route: "+routeName, http.StatusNotFound)
		return routeName, nil, profile, resp, err
	}
	if routeConfig.GetOr("enabled", true) == false {
		resp, err := webhookJSONError("Route disabled: "+routeName, http.StatusForbidden)
		return routeName, nil, profile, resp, err
	}
	return routeName, routeConfig, profile, nil, nil
}

// Prompt replaced by the first configured skill that exists, unchanged when none loads
func webhookApplySkills(ctx context.Context, prompt string, skillList any) string {
	log := webhookLog()
	names, err := msgraphIterate(skillList)
	if err != nil {
		log.Warn("[webhook] Skill loading failed: " + pyrepr.ExceptionMessage(err))
		return prompt
	}
	scope := skills.NewScope(home.Dir(ctx), nil)
	commands := scope.GetSkillCommands()
	for _, name := range names {
		cmdKey := "/" + pyrepr.Str(name)
		if _, ok := commands[cmdKey]; !ok {
			log.Warn(fmt.Sprintf("[webhook] Skill '%s' not found", pyrepr.Str(name)))
			continue
		}
		content, ok, err := scope.BuildSkillInvocationMessage(ctx, cmdKey, prompt, "", "")
		if err != nil {
			log.Warn("[webhook] Skill loading failed: " + pyrepr.ExceptionMessage(err))
			return prompt
		}
		if ok && content != "" {
			return content
		}
	}
	return prompt
}

// Event type from the GitHub or GitLab header, else the payload event_type or type, else unknown
func webhookEventType(header http.Header, payload any) (any, error) {
	if value := webhookHeader(header, "X-GitHub-Event"); value != "" {
		return value, nil
	}
	if value := webhookHeader(header, "X-GitLab-Event"); value != "" {
		return value, nil
	}
	for _, key := range []string{"event_type", "type"} {
		value, err := webhookPayloadGet(payload, key, "")
		if err != nil {
			return nil, err
		}
		if pyrepr.Truthy(value) {
			return value, nil
		}
	}
	return "unknown", nil
}

// Request headers as a dict of their last values in sorted name order
func webhookHeaderDict(header http.Header) *pyjson.Object {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	out := pyjson.NewObject()
	for _, name := range names {
		if values := header[name]; len(values) > 0 {
			out.Set(name, values[len(values)-1])
		}
	}
	return out
}

// Receives one webhook POST, authenticates, filters, renders and admits or delivers it
func (a *WebhookAdapter) handleWebhook(ctx context.Context, request *WebRequest) (*WebResponse, error) {
	routeName, routeConfig, profile, errResp, err := a.resolveRoute(ctx, request)
	if err != nil || errResp != nil {
		return errResp, err
	}
	rawBody, errResp, err := a.readAuthenticatedBody(request, routeName, routeConfig)
	if err != nil || errResp != nil {
		return errResp, err
	}
	header := request.Header
	deliveryID := webhookHeaderDeliveryID(header)
	profileName := ""
	if profile != nil {
		profileName = *profile
	}
	errResp, err = ParkedProfileRefusalResponse(ctx, a.GatewayRunner, profileName, routeConfig.Vals, func() (*SessionSource, error) {
		return a.admissionSource(ctx, routeName, profile, deliveryID), nil
	})
	if err != nil || errResp != nil {
		return errResp, err
	}
	if !a.recordRateLimitHit(routeName, webhookNow()) {
		return webhookJSONError("Rate limit exceeded", http.StatusTooManyRequests)
	}
	payload, parsed, err := webhookParseBody(rawBody)
	if err != nil {
		return nil, err
	}
	if !parsed {
		return webhookJSONError("Cannot parse body", http.StatusBadRequest)
	}
	if control, err := HandleMCPEventControl(ctx, a, routeName, routeConfig, payload, profile); err != nil || control != nil {
		return control, err
	}
	if eventID, ok := MCPEventDeliveryID(routeConfig, payload); ok {
		deliveryID = eventID
	}
	eventType, err := webhookEventType(header, payload)
	if err != nil {
		return nil, err
	}
	if allowed := routeConfig.GetOr("events", []any{}); pyrepr.Truthy(allowed) {
		member, err := webhookPyIn(eventType, allowed)
		if err != nil {
			return nil, err
		}
		if !member {
			webhookLog().Debug(fmt.Sprintf("[webhook] Ignoring event %s for route %s (allowed: %s)", pyrepr.Str(eventType), routeName, pyrepr.Str(allowed)))
			return WebJSONResponse(pyjson.ObjectOf("status", "ignored", "event", eventType), http.StatusOK, nil)
		}
	}
	matched, err := a.routeProcessor.RouteFiltersMatch(ctx, routeConfig, payload, eventType, webhookHeaderDict(header))
	if err != nil {
		return nil, err
	}
	if !matched {
		webhookLog().Info(fmt.Sprintf("[webhook] filtered event=%s route=%s", pyrepr.Str(eventType), routeName))
		return WebJSONResponse(pyjson.ObjectOf("status", "ignored", "reason", "filter", "route", routeName), http.StatusOK, nil)
	}
	scoped, err := a.WebhookProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	if script := routeConfig.GetOr("script", nil); pyrepr.Truthy(script) {
		keep, transformed, err := a.routeProcessor.RunRouteScript(scoped, script, payload)
		if err != nil {
			return nil, err
		}
		if !keep {
			webhookLog().Info(fmt.Sprintf("[webhook] script ignored event=%s route=%s", pyrepr.Str(eventType), routeName))
			return WebJSONResponse(pyjson.ObjectOf("status", "ignored", "reason", "script", "route", routeName), http.StatusOK, nil)
		}
		if transformed != nil && transformed.Len() > 0 {
			payload = transformed
		}
	}
	prompt, rendered, err := RenderMCPEventPrompt(a, routeConfig, payload, eventType, routeName)
	if err != nil {
		return nil, err
	}
	if !rendered || prompt == "" {
		if prompt, err = a.RenderPrompt(routeConfig.GetOr("prompt", ""), payload, eventType, routeName); err != nil {
			return nil, err
		}
	}
	if skillList := routeConfig.GetOr("skills", []any{}); pyrepr.Truthy(skillList) && !pyrepr.Truthy(routeConfig.GetOr("cron_job", nil)) {
		prompt = webhookApplySkills(scoped, prompt, skillList)
	}
	now := webhookNow()
	identity := webhookIdentityFromParts(profile, routeName, deliveryID)
	coalesce, coalesceIsDict := routeConfig.GetOr("coalesce", nil).(*pyjson.Object)
	nonDurable := pyrepr.Truthy(routeConfig.GetOr("deliver_only", nil)) || pyrepr.Truthy(routeConfig.GetOr("cron_job", nil)) || coalesceIsDict
	if nonDurable && !a.recordDeliveryID(identity, now) {
		webhookLog().Info(fmt.Sprintf("[webhook] Skipping duplicate delivery %s on route %s", deliveryID, routeName))
		return WebJSONResponse(pyjson.ObjectOf("status", "duplicate", "delivery_id", deliveryID), http.StatusOK, nil)
	}
	if pyrepr.Truthy(routeConfig.GetOr("cron_job", nil)) {
		return a.handleCronTrigger(ctx, prompt, routeConfig, routeName, eventType, deliveryID, profile)
	}
	if pyrepr.Truthy(routeConfig.GetOr("deliver_only", nil)) {
		return a.handleDeliverOnly(ctx, prompt, payload, routeConfig, routeName, eventType, deliveryID, profile)
	}
	if coalesceIsDict {
		buffered, err := a.coalescer.Enqueue(routeName, coalesce, payload, eventType, prompt, deliveryID, now,
			WebhookDispatchArgs{RouteConfig: routeConfig, Profile: profile})
		if err != nil {
			return nil, err
		}
		if buffered {
			return WebJSONResponse(pyjson.ObjectOf("status", "coalesced", "route", routeName, "event", eventType,
				"delivery_id", deliveryID), http.StatusAccepted, nil)
		}
	}
	return a.dispatchAgentRun(ctx, request, routeConfig, routeName, profile, payload, prompt, eventType, deliveryID, now)
}

// Admits the run and acknowledges only once the authority committed the delivery
func (a *WebhookAdapter) dispatchAgentRun(ctx context.Context, request *WebRequest, routeConfig *pyjson.Object, routeName string, profile *string,
	payload any, prompt string, eventType any, deliveryID string, now float64) (*WebResponse, error) {
	webhookLog().Info(fmt.Sprintf("[webhook] %s event=%s route=%s prompt_len=%d delivery=%s", request.Method, pyrepr.Str(eventType),
		routeName, runeLen(prompt), deliveryID))
	outcome, err := a.admitAgentRun(ctx, payload, prompt, deliveryID, now, WebhookDispatchArgs{RouteConfig: routeConfig,
		RouteName: routeName, Profile: profile, EventType: eventType})
	if err != nil {
		return nil, err
	}
	switch outcome {
	case "":
		return webhookJSONError("Admission unavailable; retry this delivery", http.StatusServiceUnavailable)
	case "duplicate":
		return WebJSONResponse(pyjson.ObjectOf("status", "duplicate", "delivery_id", deliveryID), http.StatusOK, nil)
	}
	return WebJSONResponse(pyjson.ObjectOf("status", "accepted", "route", routeName, "event", eventType,
		"delivery_id", deliveryID), http.StatusAccepted, nil)
}

// Admits a settled coalesced event as a task, releasing the batch delivery ids when admission fails
func (a *WebhookAdapter) spawnAgentRun(payload any, prompt, deliveryID string, now float64, args WebhookDispatchArgs, superseded []string) *adapterTask {
	ctx := context.Background()
	task := spawnAdapterTask(ctx, func(taskCtx context.Context) error {
		outcome, err := a.admitAgentRun(taskCtx, payload, prompt, deliveryID, now, args)
		if err != nil {
			return err
		}
		if outcome == "" {
			webhookLog().Error(fmt.Sprintf("[webhook] Coalesced delivery %s on route %s was not admitted", deliveryID, args.RouteName))
			a.stateMu.Lock()
			for _, released := range append(append([]string(nil), superseded...), deliveryID) {
				delete(a.seenDeliveries, webhookIdentityFromParts(args.Profile, args.RouteName, released))
			}
			a.stateMu.Unlock()
		}
		return nil
	})
	a.trackBackgroundTask(task)
	return task
}

// Session source admission builds for one delivery, a profile prefix overriding profile_routes
func (a *WebhookAdapter) admissionSource(ctx context.Context, routeName string, profile *string, deliveryID string) *SessionSource {
	identity := webhookIdentityFromParts(profile, routeName, deliveryID)
	chatName, userID, userName := "webhook/"+routeName, "webhook:"+routeName, routeName
	source := a.BuildSource(ctx, identity.SessionChatID(), SourceFields{ChatName: &chatName, ChatType: "webhook",
		UserID: &userID, UserName: &userName})
	if profile != nil && *profile != "" {
		name := *profile
		source.Profile = &name
		source.ProfileRouteRejected, source.rejectedRouteProfile = false, nil
	}
	return source
}

// Records delivery info and admits the run through the durable producer, accepted, duplicate or empty on failure
func (a *WebhookAdapter) admitAgentRun(ctx context.Context, payload any, prompt, deliveryID string, now float64, args WebhookDispatchArgs) (string, error) {
	identity := webhookIdentityFromParts(args.Profile, args.RouteName, deliveryID)
	sessionChatID := identity.SessionChatID()
	extra, err := a.renderDeliveryExtra(args.RouteConfig.GetOr("deliver_extra", pyjson.NewObject()), payload)
	if err != nil {
		return "", err
	}
	var profileValue any
	if args.Profile != nil {
		profileValue = *args.Profile
	}
	info := pyjson.ObjectOf("deliver", args.RouteConfig.GetOr("deliver", "log"), "profile", profileValue, "deliver_extra", extra,
		"route", args.RouteName, "mirror", args.RouteConfig.GetOr("mirror_to_session", nil) == true)
	a.stateMu.Lock()
	a.deliveryInfo[sessionChatID] = info
	a.deliveryInfoCreated[sessionChatID] = now
	a.deliveryInfoOrder = append(a.deliveryInfoOrder, webhookDeliveryStamp{At: now, Key: sessionChatID})
	a.pruneDeliveryInfo(now)
	a.stateMu.Unlock()
	source := a.admissionSource(ctx, args.RouteName, args.Profile, deliveryID)
	event := NewMessageEvent(prompt)
	event.MessageType = MessageTypeText
	event.Source = source
	event.RawMessage = payload
	event.MessageID = &deliveryID
	event.Timestamp = time.Unix(0, 0).UTC()
	receipt, err := webhookAdmitProducer(ctx, a, event)
	if err != nil {
		webhookLog().Error("[webhook] Durable admission failed for "+deliveryID, "error", err)
		return "", nil
	}
	if event.WebhookDuplicate() {
		return "duplicate", nil
	}
	if receipt == nil {
		if reply := event.ConsumerReply(); reply != nil && *reply != "" {
			destination, err := webhookSnapshotDestination(ctx, a, sessionChatID)
			if err != nil {
				return "", err
			}
			if _, err := a.deliverTo(ctx, destination, sessionChatID, *reply); err != nil {
				return "", err
			}
		}
		return "accepted", nil
	}
	runner := handlerOwner(a)
	authority, err := webhookAuthorityForProfileID(runner, receipt.Ref.ProfileID)
	if err != nil {
		return "", err
	}
	if authority == nil {
		authority, err = activeAuthority(ctx, runner)
		if err != nil {
			return "", err
		}
	}
	admitted := *receipt
	task := spawnAdapterTask(context.WithoutCancel(ctx), func(taskCtx context.Context) error {
		webhookFinalizeDelivery(a, taskCtx, event, authority, admitted)
		return nil
	})
	a.trackBackgroundTask(task)
	return "accepted", nil
}

// Waits for the admitted run to settle and then closes its one shot webhook session
func (a *WebhookAdapter) finalizeDelivery(ctx context.Context, event *MessageEvent, authority *SessionAuthority, receipt AdmissionReceipt) {
	err := func() error {
		if authority == nil {
			return &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'db'"}
		}
		row, err := authority.DB.GetSessionAdmission(ctx, receipt.AdmissionID)
		if err != nil {
			return err
		}
		if row == nil {
			return &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
		}
		if status := row.GetOr("status", nil); status == "queued" || status == "started" {
			authority.mu.Lock()
			waiter := authority.waiters[receipt.AdmissionID]
			if waiter == nil {
				waiter = newAdmissionWaiter()
				authority.waiters[receipt.AdmissionID] = waiter
			}
			authority.mu.Unlock()
			if _, err := waiter.wait(ctx); err != nil {
				return err
			}
			if row, err = authority.DB.GetSessionAdmission(ctx, receipt.AdmissionID); err != nil {
				return err
			}
			if row == nil {
				return &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
			}
		}
		event.webhookReceipt = &webhookReceiptBinding{authority: authority, receipt: authority.receiptOf(row)}
		return a.endWebhookSession(ctx, event)
	}()
	if err != nil {
		webhookLog().Error("[webhook] Could not finalize admitted delivery "+receipt.AdmissionID, "error", err)
	}
}

// Authority and receipt of the admitted delivery whose one shot session an event closes
type webhookReceiptBinding struct {
	authority *SessionAuthority
	receipt   AdmissionReceipt
}

// Finalizes the admitted webhook delivery bound to the event, nothing for an event that carries none
func (a *WebhookAdapter) endWebhookSession(ctx context.Context, event *MessageEvent) error {
	binding := event.webhookReceipt
	if binding == nil {
		return nil
	}
	_, err := FinalizeWebhook(ctx, binding.authority, binding.receipt)
	return err
}

// Validates a GitHub, GitLab, Svix, Standard Webhooks, Linear or generic HMAC signature of the body
func (a *WebhookAdapter) ValidateSignature(request *WebRequest, body []byte, secret string) bool {
	header := request.Header
	names := []string{"svix-id", "svix-timestamp", "svix-signature"}
	svix := make([]string, 3)
	for i, name := range names {
		svix[i] = webhookHeader(header, name)
	}
	anySet := func() bool { return svix[0] != "" || svix[1] != "" || svix[2] != "" }
	if !anySet() && webhookHeader(header, "webhook-signature") != "" {
		for i, name := range []string{"webhook-id", "webhook-timestamp", "webhook-signature"} {
			svix[i] = webhookHeader(header, name)
		}
	}
	if anySet() {
		return webhookValidateSvixSignature(body, secret, svix[0], svix[1], svix[2])
	}
	for _, check := range []struct {
		provided string
		expected func() string
	}{
		{webhookHeader(header, "linear-signature"), func() string { return webhookHexHMAC(secret, body) }},
		{webhookHeader(header, "X-Hub-Signature-256"), func() string { return "sha256=" + webhookHexHMAC(secret, body) }},
		{webhookHeader(header, "X-Gitlab-Token"), func() string { return secret }},
	} {
		if check.provided != "" {
			return webhookHMACStrEqual(check.provided, check.expected())
		}
	}
	routeName := request.MatchInfo["route_name"]
	if v2Sig := webhookHeader(header, "X-Webhook-Signature-V2"); v2Sig != "" {
		v2Timestamp := webhookHeader(header, "X-Webhook-Timestamp")
		if v2Timestamp == "" {
			webhookLog().Warn(fmt.Sprintf("[webhook] Route '%s' sent X-Webhook-Signature-V2 with no X-Webhook-Timestamp — "+
				"rejecting rather than falling back to legacy V1", routeName))
			return false
		}
		if !webhookTimestampFresh(v2Timestamp, fmt.Sprintf("[webhook] Route '%s' generic HMAC V2 timestamp outside replay window", routeName)) {
			return false
		}
		return webhookHMACStrEqual(v2Sig, webhookHexHMAC(secret, append([]byte(v2Timestamp+"."), body...)))
	}
	if genericSig := webhookHeader(header, "X-Webhook-Signature"); genericSig != "" {
		a.stateMu.Lock()
		warned := a.v1SignatureWarned[routeName]
		a.v1SignatureWarned[routeName] = true
		a.stateMu.Unlock()
		if !warned {
			webhookLog().Warn(fmt.Sprintf("[webhook] Route '%s' uses legacy body-only HMAC (no timestamp), which is vulnerable "+
				"to replay attacks. Add an 'X-Webhook-Timestamp' header and switch to "+
				"'X-Webhook-Signature-V2' (HMAC-SHA256 of '<timestamp>.<body>').", routeName))
		}
		return webhookHMACStrEqual(genericSig, webhookHexHMAC(secret, body))
	}
	webhookLog().Debug("[webhook] Secret configured but no signature header found")
	return false
}

// Renders a prompt template with dotted payload fields, the raw payload JSON for an empty template
func (a *WebhookAdapter) RenderPrompt(template any, payload any, eventType any, routeName string) (string, error) {
	if !pyrepr.Truthy(template) {
		dumped, err := pyjson.Dumps(payload, pyjson.Indent(2))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Webhook event '%s' on route '%s':\n\n```json\n%s\n```", pyrepr.Str(eventType), routeName, pySliceRunes(dumped, 4000)), nil
	}
	text, ok := template.(string)
	if !ok {
		return "", &PyError{Kind: "TypeError", Msg: fmt.Sprintf("expected string or bytes-like object, got '%s'", pyrepr.TypeName(template))}
	}
	var failure error
	out := webhookTemplateKeyRE.ReplaceAllStringFunc(text, func(match string) string {
		if failure != nil {
			return match
		}
		key := match[1 : len(match)-1]
		literal := "{" + key + "}"
		switch key {
		case "__raw__":
			dumped, err := pyjson.Dumps(payload, pyjson.Indent(2))
			if err != nil {
				failure = err
				return match
			}
			return pySliceRunes(dumped, 4000)
		case "event_type":
			eventText, ok := eventType.(string)
			if !ok {
				failure = &PyError{Kind: "TypeError", Msg: fmt.Sprintf("expected str instance, %s found", pyrepr.TypeName(eventType))}
				return match
			}
			return eventText
		}
		value := payload
		for _, part := range strings.Split(key, ".") {
			obj, ok := value.(*pyjson.Object)
			if !ok {
				return literal
			}
			value = obj.GetOr(part, literal)
		}
		switch value.(type) {
		case *pyjson.Object, []any:
			dumped, err := pyjson.Dumps(value, pyjson.Indent(2))
			if err != nil {
				failure = err
				return match
			}
			return pySliceRunes(dumped, 2000)
		}
		return pyrepr.Str(value)
	})
	return out, failure
}

// Delivery extra with every string value rendered against the payload
func (a *WebhookAdapter) renderDeliveryExtra(extra any, payload any) (*pyjson.Object, error) {
	obj, ok := pyjson.AsObject(extra)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'items'", pyrepr.TypeName(extra))}
	}
	out := pyjson.NewObject()
	for _, key := range obj.Keys {
		value, _ := obj.Get(key)
		if text, isText := value.(string); isText {
			rendered, err := a.RenderPrompt(text, payload, "", "")
			if err != nil {
				return nil, err
			}
			out.Set(key, rendered)
			continue
		}
		out.Set(key, value)
	}
	return out, nil
}

// Sends deliver only content through the same helpers the agent reply leg uses
func (a *WebhookAdapter) directDeliver(ctx context.Context, content string, delivery *pyjson.Object) (SendResult, error) {
	deliverType := delivery.GetOr("deliver", "log")
	switch {
	case pyrepr.Equal(deliverType, "log"):
		webhookLog().Info("[webhook] direct-deliver log-only: " + pySliceRunes(content, 200))
		return SendResult{Success: true}, nil
	case pyrepr.Equal(deliverType, "github_comment"):
		return a.deliverGitHubComment(ctx, content, delivery)
	}
	return a.deliverCrossPlatform(ctx, deliverType, content, delivery)
}

// Delivery extra of a destination as a dict, the AttributeError of any other value
func webhookDeliveryExtra(delivery *pyjson.Object) (*pyjson.Object, error) {
	extra := delivery.GetOr("deliver_extra", pyjson.NewObject())
	obj, ok := pyjson.AsObject(extra)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(extra))}
	}
	return obj, nil
}

// Posts the response as a pull request comment through the gh CLI with the routed profile's credentials
func (a *WebhookAdapter) deliverGitHubComment(ctx context.Context, content string, delivery *pyjson.Object) (SendResult, error) {
	extra, err := webhookDeliveryExtra(delivery)
	if err != nil {
		return SendResult{}, err
	}
	repo, prNumber := extra.GetOr("repo", ""), extra.GetOr("pr_number", "")
	if !pyrepr.Truthy(repo) || !pyrepr.Truthy(prNumber) {
		webhookLog().Error("[webhook] github_comment delivery missing repo or pr_number")
		return SendResult{Success: false, Error: "Missing repo or pr_number"}, nil
	}
	prInt, err := pyint.Of(prNumber)
	if err == nil && prInt.Sign() <= 0 {
		err = &PyError{Kind: "ValueError", Msg: "non-positive"}
	}
	if err != nil {
		var pyErr *pyint.Error
		var gwErr *PyError
		if errors.As(err, &pyErr) && (pyErr.Kind == "ValueError" || pyErr.Kind == "TypeError") || errors.As(err, &gwErr) {
			webhookLog().Error("[webhook] invalid pr_number: " + pyrepr.Repr(prNumber))
			return SendResult{Success: false, Error: "Invalid pr_number"}, nil
		}
		return SendResult{}, err
	}
	repoText, ok := repo.(string)
	if !ok {
		return SendResult{}, &PyError{Kind: "TypeError", Msg: "expected string or bytes-like object, got '" + pyrepr.TypeName(repo) + "'"}
	}
	if !webhookRepoRE.MatchString(repoText) {
		webhookLog().Error("[webhook] invalid repo format: " + pyrepr.Repr(repo))
		return SendResult{Success: false, Error: "Invalid repo format"}, nil
	}
	env, err := a.githubEnv(ctx, delivery.GetOr("profile", nil))
	if err != nil {
		webhookLog().Error("[webhook] github_comment delivery error: " + pyrepr.ExceptionMessage(err))
		return SendResult{Success: false, Error: pyrepr.ExceptionMessage(err)}, nil
	}
	argv := []string{"gh", "pr", "comment", prInt.String(), "--repo", repoText, "--body", content}
	result, err := pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Env: env, Timeout: 30 * time.Second})
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			webhookLog().Error("[webhook] 'gh' CLI not found — install GitHub CLI for github_comment delivery")
			return SendResult{Success: false, Error: "gh CLI not installed"}, nil
		}
		text := webhookExecErrorText(err)
		var timeout *pysubprocess.TimeoutExpired
		if errors.As(err, &timeout) {
			text = timeout.Error()
		}
		webhookLog().Error("[webhook] github_comment delivery error: " + text)
		return SendResult{Success: false, Error: text}, nil
	}
	if result.Returncode == 0 {
		webhookLog().Info(fmt.Sprintf("[webhook] Posted comment on %s#%s", repoText, pyrepr.Str(prNumber)))
		return SendResult{Success: true}, nil
	}
	stderr := strings.ToValidUTF8(string(result.Stderr), "�")
	webhookLog().Error("[webhook] gh pr comment failed: " + stderr)
	return SendResult{Success: false, Error: stderr}, nil
}

// Environment of gh for a routed profile with only its own GitHub tokens, nil inheriting for bare routes
func (a *WebhookAdapter) githubEnv(ctx context.Context, profile any) ([]string, error) {
	name, ok := profile.(string)
	if !ok || name == "" || name == "default" {
		return nil, nil
	}
	env := map[string]string{}
	var order []string
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if slices.Contains(webhookGHTokenVars, key) {
			continue
		}
		if _, seen := env[key]; !seen {
			order = append(order, key)
		}
		env[key] = value
	}
	scoped, err := a.WebhookProfileScope(ctx, &name)
	if err != nil {
		return nil, err
	}
	for _, key := range webhookGHTokenVars {
		value, found, err := provider.GetSecret(scoped, key)
		if err != nil {
			return nil, err
		}
		if found && value != "" {
			if _, seen := env[key]; !seen {
				order = append(order, key)
			}
			env[key] = value
		}
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+env[key])
	}
	return out, nil
}

// Live adapter the routed profile delivers through, nil when that profile has none for the platform
func (a *WebhookAdapter) findAdapter(targetPlatform Platform, profile *string) PlatformAdapter {
	if targetPlatform == "" {
		return nil
	}
	holder, ok := a.GatewayRunner.(profileAdaptersRunner)
	if !ok {
		return nil
	}
	adapters := holder.AdaptersForProfile(profile)
	if adapters == nil {
		return nil
	}
	adapter, _ := adapters.Get(targetPlatform)
	return adapter
}

// Routes the response to another platform through the routed profile's own adapter and home channel
func (a *WebhookAdapter) deliverCrossPlatform(ctx context.Context, platformName any, content string, delivery *pyjson.Object) (SendResult, error) {
	if a.GatewayRunner == nil {
		return SendResult{Success: false, Error: "No gateway runner for cross-platform delivery"}, nil
	}
	nameText := pyrepr.Str(platformName)
	targetPlatform, err := ParsePlatform(ctx, platformName)
	if err != nil {
		return SendResult{Success: false, Error: "Unknown platform: " + nameText}, nil
	}
	var profile *string
	if raw := delivery.GetOr("profile", nil); raw != nil {
		text, isText := raw.(string)
		if !isText {
			return SendResult{}, &PyError{Kind: "TypeError", Msg: "profile must be a string"}
		}
		profile = &text
	}
	adapter := a.findAdapter(targetPlatform, profile)
	if adapter == nil {
		return SendResult{Success: false, Error: fmt.Sprintf("Platform %s not connected", nameText)}, nil
	}
	extra, err := webhookDeliveryExtra(delivery)
	if err != nil {
		return SendResult{}, err
	}
	chatID := extra.GetOr("chat_id", "")
	scoped, err := a.WebhookProfileScope(ctx, profile)
	if err != nil {
		return SendResult{}, err
	}
	if !pyrepr.Truthy(chatID) {
		cfg, err := a.WebhookDeliveryConfig(scoped, profile)
		if err != nil {
			return SendResult{}, err
		}
		homeChannel := cfg.GetHomeChannel(targetPlatform)
		if homeChannel == nil {
			return SendResult{Success: false, Error: "No chat_id or home channel for " + nameText}, nil
		}
		chatID = homeChannel.ChatID
	}
	threadID := extra.GetOr("message_thread_id", nil)
	if !pyrepr.Truthy(threadID) {
		threadID = extra.GetOr("thread_id", nil)
	}
	var metadata map[string]any
	if pyrepr.Truthy(threadID) {
		metadata = map[string]any{"thread_id": threadID}
	}
	chatText := pyrepr.Str(chatID)
	result, err := adapter.Send(scoped, chatText, content, "", metadata)
	if err != nil {
		return SendResult{}, err
	}
	if result.Success {
		a.mirrorDelivery(scoped, nameText, chatText, content, delivery, threadID)
	}
	return result, nil
}

// Gateway config of the profile a delivery is bound to, the runner's own for a bare route
func (a *WebhookAdapter) WebhookDeliveryConfig(ctx context.Context, profile *string) (*GatewayConfig, error) {
	if profile == nil || *profile == "" {
		holder, ok := a.GatewayRunner.(gatewayConfigRunner)
		if !ok || holder == nil {
			return nil, &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'config'"}
		}
		return holder.GatewayConfig(), nil
	}
	return LoadGatewayConfig(ctx)
}

// Mirrors a delivered response into the target chat transcript as a labelled user turn when the route opts in
func (a *WebhookAdapter) mirrorDelivery(ctx context.Context, platformName, chatID, content string, delivery *pyjson.Object, threadID any) {
	if delivery.GetOr("mirror", nil) != true {
		return
	}
	route := "webhook"
	if raw := delivery.GetOr("route", nil); pyrepr.Truthy(raw) {
		route = pyrepr.Str(raw)
	}
	defer func() {
		if rec := recover(); rec != nil {
			webhookLog().Debug(fmt.Sprintf("[webhook] Route '%s' delivery mirror into %s:%s failed: %v", route, platformName, chatID, rec))
		}
	}()
	var thread *string
	if threadID != nil {
		text := pyrepr.Str(threadID)
		thread = &text
	}
	ok := MirrorToSession(ctx, platformName, chatID, fmt.Sprintf("[Webhook delivery: %s]\n%s", route, content),
		MirrorOptions{SourceLabel: "webhook", ThreadID: thread, Role: "user"})
	level, verb := slog.LevelInfo, ""
	if !ok {
		level, verb = slog.LevelDebug, "not "
	}
	webhookLog().Log(ctx, level, fmt.Sprintf("[webhook] Route '%s' delivery %smirrored into %s:%s session", route, verb, platformName, chatID))
}
