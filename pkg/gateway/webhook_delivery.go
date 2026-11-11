package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Built in platform names a webhook route may deliver to
var webhookBuiltinDeliverPlatforms = map[string]bool{
	"telegram": true, "discord": true, "slack": true, "signal": true, "sms": true, "whatsapp": true, "matrix": true,
	"mattermost": true, "email": true, "dingtalk": true, "feishu": true, "wecom": true, "wecom_callback": true,
	"weixin": true, "bluebubbles": true, "qqbot": true, "yuanbao": true}

// Owner slash repo spelling a github comment target must match whole
var webhookRepoRE = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// Webhook adapter surface a delivery destination is validated and snapshotted against
type webhookDeliveryAdapter interface {
	ReloadDynamicRoutes(ctx context.Context)
	WebhookRoute(name string) (any, bool)
	DeliveryInfo(chatID string) any
	WebhookProfileScope(ctx context.Context, profile *string) (context.Context, error)
	WebhookDeliveryConfig(ctx context.Context, profile *string) (*GatewayConfig, error)
}

// Webhook delivery surface of an adapter, the AttributeError a Python call on any other adapter raises
func webhookAdapterOf(adapter PlatformAdapter, attribute string) (webhookDeliveryAdapter, error) {
	if hook, ok := adapter.(webhookDeliveryAdapter); ok {
		return hook, nil
	}
	return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '%s'", adapter, attribute)}
}

// True for a built in delivery platform or one a plugin registered
func webhookKnownPlatform(ctx context.Context, name string) (known bool) {
	if webhookBuiltinDeliverPlatforms[name] {
		return true
	}
	defer func() {
		if recover() != nil {
			known = false
		}
	}()
	return Registry.IsRegistered(ctx, name)
}

// Route a webhook delivery chat id names, decoded from a v2 token or the legacy route spelling
func webhookRouteFromChatID(chatID string) (string, error) {
	const prefix = "webhook:v2:"
	if strings.HasPrefix(chatID, prefix) {
		token := chatID[len(prefix):]
		padding := (4 - len(token)%4) % 4
		raw, err := pybase64.URLSafeB64Decode(token + strings.Repeat("=", padding))
		if err != nil {
			return "", &PyError{Kind: "ValueError", Msg: err.Error()}
		}
		decoded, err := pyjson.LoadsBytes(raw)
		if err != nil {
			return "", &PyError{Kind: "ValueError", Msg: err.Error()}
		}
		parts, err := webhookUnpackThree(decoded)
		if err != nil {
			return "", err
		}
		return pyrepr.Str(parts[1]), nil
	}
	pieces := strings.SplitN(chatID, ":", 3)
	if len(pieces) < 2 {
		return "", &PyError{Kind: "IndexError", Msg: "list index out of range"}
	}
	return pieces[1], nil
}

// Three items Python unpacking yields from a decoded token, list items, mapping keys or string characters
func webhookUnpackThree(decoded any) ([]any, error) {
	var items []any
	switch v := decoded.(type) {
	case []any:
		items = v
	case *pyjson.Object:
		for _, key := range v.Keys {
			items = append(items, key)
		}
	case string:
		for _, r := range v {
			items = append(items, string(r))
		}
	default:
		return nil, &PyError{Kind: "TypeError", Msg: "cannot unpack non-iterable " + jsonValueTypeName(decoded) + " object"}
	}
	if len(items) < 3 {
		return nil, &PyError{Kind: "ValueError", Msg: fmt.Sprintf("not enough values to unpack (expected 3, got %d)", len(items))}
	}
	if len(items) > 3 {
		return nil, &PyError{Kind: "ValueError", Msg: "too many values to unpack (expected 3)"}
	}
	return items, nil
}

// Digest of the webhook route a delivery chat id names, not found for an unknown route
func webhookRouteDigest(ctx context.Context, adapter PlatformAdapter, chatID string) (string, error) {
	hook, err := webhookAdapterOf(adapter, "_reload_dynamic_routes")
	if err != nil {
		return "", err
	}
	hook.ReloadDynamicRoutes(ctx)
	name, err := webhookRouteFromChatID(chatID)
	if err != nil {
		return "", runtimeRefusal("not_found")
	}
	route, found := hook.WebhookRoute(name)
	if !found || route == nil {
		return "", runtimeRefusal("not_found")
	}
	encoded, err := pyjson.Dumps(route, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

// Mapping view of a delivery value, false for anything that is no dict
func webhookMapping(value any) (*pyjson.Object, bool) {
	switch v := value.(type) {
	case *pyjson.Object:
		return v, v != nil
	case map[string]any:
		obj := pyjson.NewObject()
		for key, item := range v {
			obj.Set(key, item)
		}
		return obj, true
	}
	return nil, false
}

// True when a github comment pull request number is all digits and positive
func webhookPositiveNumber(number any) (bool, error) {
	text := pyrepr.Str(number)
	if text == "" {
		return false, nil
	}
	for _, r := range text {
		if !pystr.IsDigit(r) {
			return false, nil
		}
	}
	parsed, err := pyint.Of(number)
	if err != nil {
		return false, err
	}
	return parsed.Sign() > 0, nil
}

// Delivery destination reduced to the fields its sender consumes, invalid params for any malformed one
func webhookValidateDestination(ctx context.Context, delivery any) (*pyjson.Object, error) {
	obj, ok := webhookMapping(delivery)
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	allowed := map[string]bool{"deliver": true, "deliver_extra": true, "profile": true, "mirror": true, "route": true}
	_, hasDeliver := obj.Get("deliver")
	_, hasExtra := obj.Get("deliver_extra")
	if !hasDeliver || !hasExtra {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, key := range obj.Keys {
		if !allowed[key] {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	profile, _ := obj.Get("profile")
	mirror, hasMirror := obj.Get("mirror")
	route, _ := obj.Get("route")
	_, profileText := profile.(string)
	_, mirrorFlag := mirror.(bool)
	_, routeText := route.(string)
	if (profile != nil && !profileText) || (hasMirror && !mirrorFlag) || (route != nil && !routeText) {
		return nil, runtimeRefusal("invalid_params")
	}
	rawTarget, _ := obj.Get("deliver")
	rawExtra, _ := obj.Get("deliver_extra")
	target, isText := rawTarget.(string)
	extra, isMap := webhookMapping(rawExtra)
	if !isText || !isMap {
		return nil, runtimeRefusal("invalid_params")
	}
	switch {
	case target == "github_comment":
		repo, _ := extra.Get("repo")
		number, _ := extra.Get("pr_number")
		repoText, isRepo := repo.(string)
		if !isRepo || !webhookRepoRE.MatchString(repoText) {
			return nil, runtimeRefusal("invalid_params")
		}
		positive, err := webhookPositiveNumber(number)
		if err != nil {
			return nil, err
		}
		if !positive {
			return nil, runtimeRefusal("invalid_params")
		}
	case target != "log" && !webhookKnownPlatform(ctx, target):
		return nil, runtimeRefusal("invalid_params")
	}
	fields := []string{"chat_id", "message_thread_id", "thread_id"}
	switch target {
	case "log":
		fields = nil
	case "github_comment":
		fields = []string{"repo", "pr_number"}
	}
	kept := pyjson.NewObject()
	for _, key := range fields {
		if value, found := extra.Get(key); found {
			kept.Set(key, pycopy.DeepCopy(value))
		}
	}
	result := pyjson.ObjectOf("deliver", target, "deliver_extra", kept)
	if pyTruthy(profile) {
		result.Set("profile", profile)
	}
	if mirror == true {
		result.Set("mirror", true)
		if pyTruthy(route) {
			result.Set("route", route)
		}
	}
	return result, nil
}

// Validated destination of a delivery chat id with the profile's home channel filled in where it names no chat
func webhookSnapshotDestination(ctx context.Context, adapter PlatformAdapter, chatID string) (*pyjson.Object, error) {
	hook, err := webhookAdapterOf(adapter, "_delivery_info")
	if err != nil {
		return nil, err
	}
	result, err := webhookValidateDestination(ctx, hook.DeliveryInfo(chatID))
	if err != nil {
		return nil, err
	}
	target := anyText(mustGet(result, "deliver"))
	extra := mustGet(result, "deliver_extra").(*pyjson.Object)
	chat, _ := extra.Get("chat_id")
	if target == "log" || target == "github_comment" || pyTruthy(chat) {
		return result, nil
	}
	var profile *string
	if raw, found := result.Get("profile"); found {
		text := raw.(string)
		profile = &text
	}
	scoped, err := hook.WebhookProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	cfg, err := hook.WebhookDeliveryConfig(scoped, profile)
	if err != nil {
		return nil, err
	}
	home := cfg.GetHomeChannel(Platform(target))
	if home == nil {
		return nil, runtimeRefusal("not_found")
	}
	extra.Set("chat_id", home.ChatID)
	return result, nil
}

// Value of a key the caller built, absent keys reading nil
func mustGet(obj *pyjson.Object, key string) any {
	value, _ := obj.Get(key)
	return value
}
