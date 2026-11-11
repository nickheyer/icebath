package dashboard

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

// Spawns a profile scoped CLI action, a spawn failure answered 500 with the prefix
func opsSpawnAction(ctx context.Context, argv []string, name, logMsg, prefix, profile string) (*pyjson.Object, error) {
	return spawnProfileAction(ctx, profile, argv, name, logMsg, prefix)
}

// Pairing store of a validated named profile, the store of the dashboard home when unnamed
func opsPairingStore(ctx context.Context, profile string) (*gateway.PairingStore, error) {
	requested := pystr.Strip(profile)
	if requested == "" || pystr.Lower(requested) == "current" {
		return gateway.NewPairingStore(ctx, "")
	}
	if _, err := resolveProfileDir(requested); err != nil {
		return nil, err
	}
	return gateway.NewPairingStore(ctx, requested)
}

// Pending requests and approved users of the pairing store of a profile
func listPairing(ctx context.Context, profile string) (*pyjson.Object, error) {
	store, err := opsPairingStore(ctx, profile)
	if err != nil {
		return nil, err
	}
	pending, err := store.ListPending(ctx, "")
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(pending))
	for _, p := range pending {
		rows = append(rows, pyjson.ObjectOf("platform", p.Platform, "request_id", p.RequestID, "user_id", p.UserID,
			"user_name", p.UserName, "age_minutes", int64(p.AgeMinutes)))
	}
	approved, err := store.ListApproved("")
	if err != nil {
		return nil, err
	}
	approvedRows := make([]any, 0, len(approved))
	for _, row := range approved {
		approvedRows = append(approvedRows, row)
	}
	return pyjson.ObjectOf("pending", rows, "approved", approvedRows), nil
}

// Approves a pending pairing by request id or relayed code, 429 when codes are locked out and 404 otherwise
func approvePairing(ctx context.Context, body *pyjson.Object) (*pyjson.Object, error) {
	store, err := opsPairingStore(ctx, bodyStr(body, "profile"))
	if err != nil {
		return nil, err
	}
	platform := pystr.Strip(pystr.Lower(bodyStr(body, "platform")))
	requestID := bodyStr(body, "request_id")
	target := requestID
	if target == "" {
		target = bodyStr(body, "code")
	}
	target = pystr.Strip(target)
	if platform == "" || target == "" {
		return nil, NewHTTPException(400, "platform and request_id or code are required")
	}
	byRequestID := requestID != "" || gateway.PairingLooksLikeRequestID(target)
	var result *gateway.PairingApproval
	if byRequestID {
		result, err = store.ApproveRequest(ctx, platform, target)
	} else {
		result, err = store.ApproveCode(ctx, platform, pystr.Upper(target))
	}
	if err != nil {
		return nil, err
	}
	if result != nil {
		return pyjson.ObjectOf("ok", true, "user", result.Row), nil
	}
	if !byRequestID {
		locked, err := store.IsLockedOut(platform)
		if err != nil {
			return nil, err
		}
		if locked {
			return nil, NewHTTPException(429, fmt.Sprintf("Platform '%s' is locked out after too many failed approvals.", platform))
		}
	}
	return nil, NewHTTPException(404, fmt.Sprintf("Pairing request or code not found or expired for platform '%s'.", platform))
}

// Removes an approved user of a platform, 404 when the approved list lacks the user
func revokePairing(ctx context.Context, body *pyjson.Object) (*pyjson.Object, error) {
	store, err := opsPairingStore(ctx, bodyStr(body, "profile"))
	if err != nil {
		return nil, err
	}
	platform := pystr.Strip(pystr.Lower(bodyStr(body, "platform")))
	userID := bodyStr(body, "user_id")
	if platform == "" || userID == "" {
		return nil, NewHTTPException(400, "platform and user_id are required")
	}
	revoked, err := store.Revoke(ctx, platform, userID)
	if err != nil {
		return nil, err
	}
	if revoked {
		return pyjson.ObjectOf("ok", true), nil
	}
	return nil, NewHTTPException(404, fmt.Sprintf("User %s not found in approved list for %s.", userID, platform))
}

// Clears every pending pairing request of the store of a profile and reports the count
func clearPendingPairing(ctx context.Context, profile string) (*pyjson.Object, error) {
	store, err := opsPairingStore(ctx, profile)
	if err != nil {
		return nil, err
	}
	cleared, err := store.ClearPending(ctx, "")
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "cleared", int64(cleared)), nil
}

// Python list of a stored route value, a TypeError for a value that cannot be iterated
func opsListOf(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return []any{}, nil
	}
	switch v.(type) {
	case []any, []string, string, map[string]any, *pyjson.Object:
		return pyListOf(v), nil
	}
	return nil, fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(v))
}

// Dashboard row of one webhook route with its delivery URL and the secret reduced to a flag
func webhookRouteSummary(name string, routeValue any, baseURL string) (*pyjson.Object, error) {
	route, ok := routeValue.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(routeValue))
	}
	events, err := opsListOf(route.GetOr("events", nil))
	if err != nil {
		return nil, err
	}
	skills, err := opsListOf(route.GetOr("skills", nil))
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf(
		"name", name,
		"description", route.GetOr("description", ""),
		"events", events,
		"deliver", route.GetOr("deliver", "log"),
		"deliver_only", pyrepr.Truthy(route.GetOr("deliver_only", nil)),
		"prompt", route.GetOr("prompt", ""),
		"script", route.GetOr("script", ""),
		"skills", skills,
		"created_at", route.GetOr("created_at", nil),
		"url", baseURL+"/webhooks/"+name,
		"secret_set", pyrepr.Truthy(route.GetOr("secret", nil)),
		"enabled", route.GetOr("enabled", true) != false,
	), nil
}

// Webhook platform state, base URL and every subscription of a profile with secrets redacted
func listWebhooks(ctx context.Context, profile string) (*pyjson.Object, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		h := home.Dir(sctx)
		baseURL := webhooksubs.BaseURL(sctx, h)
		enabled := webhooksubs.WebhookEnabled(sctx, h)
		subs := webhooksubs.Load(h)
		rows := make([]any, 0, subs.Len())
		for _, name := range subs.Keys {
			row, err := webhookRouteSummary(name, subs.Vals[name], baseURL)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		return pyjson.ObjectOf("enabled", enabled, "base_url", baseURL, "subscriptions", rows), nil
	})
}

// Enables the webhook platform in the profile config and restarts its gateway when it can
func enableWebhooks(ctx context.Context, profile string) (*pyjson.Object, error) {
	detail := "Failed to enable webhook platform."
	err := func() error {
		wctx, release, err := configWriteScope(ctx, profile)
		if err != nil {
			return err
		}
		defer release()
		return writePlatformEnabled(wctx, "webhook", true)
	}()
	if err := httpFailure(err, "Failed to enable webhook platform from dashboard", 500, "", &detail); err != nil {
		return nil, err
	}
	restart, err := restartGatewayAfter(ctx, profile, "enabling webhooks", "Webhook enable")
	if err != nil {
		return nil, err
	}
	out := pyjson.ObjectOf("ok", true, "platform", "webhook", "enabled", true,
		"needs_restart", !pyrepr.Truthy(restart.GetOr("restart_started", nil)))
	out.Update(restart)
	return out, nil
}

var webhookNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Stripped non empty items of a validated list of strings
func strippedItems(v any) []any {
	items, _ := v.([]any)
	out := make([]any, 0, len(items))
	for _, item := range items {
		if s := pystr.Strip(pyrepr.Str(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Creates a webhook subscription against the snapshot read first, its secret shown only in this answer
func createWebhook(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	enabled, err := configScopedToThread(ctx, profile, func(sctx context.Context) (bool, error) {
		return webhooksubs.WebhookEnabled(sctx, home.Dir(sctx)), nil
	})
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, NewHTTPException(400, "Webhook platform is not enabled. Enable it from the Webhooks page first.")
	}
	name := strings.ReplaceAll(pystr.Lower(pystr.Strip(bodyStr(body, "name"))), " ", "-")
	if !webhookNameRE.MatchString(name) {
		return nil, NewHTTPException(400, "Invalid name. Use lowercase alphanumeric with hyphens/underscores.")
	}
	deliverOnly := pyrepr.Truthy(body.GetOr("deliver_only", false))
	deliver := bodyStr(body, "deliver")
	if deliverOnly && deliver == "log" {
		return nil, NewHTTPException(400, "Direct delivery requires a real target (telegram, discord, …), not 'log'.")
	}
	expected, err := configScopedToThread(ctx, profile, func(sctx context.Context) (any, error) {
		current, present := webhooksubs.Load(home.Dir(sctx)).Get(name)
		if !present {
			return webhooksubs.Missing, nil
		}
		return current, nil
	})
	if err != nil {
		return nil, err
	}
	secret := bodyStr(body, "secret")
	if secret == "" {
		secret = tokenURLSafe(32)
	}
	description := bodyStr(body, "description")
	if description == "" {
		description = "Dashboard-created subscription: " + name
	}
	if deliver == "" {
		deliver = "log"
	}
	route := pyjson.ObjectOf(
		"description", description,
		"events", strippedItems(body.GetOr("events", nil)),
		"secret", secret,
		"prompt", bodyStr(body, "prompt"),
		"skills", strippedItems(body.GetOr("skills", nil)),
		"deliver", deliver,
		"created_at", time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	)
	if script := pystr.Strip(bodyStr(body, "script")); script != "" {
		route.Set("script", script)
	}
	if deliverOnly {
		route.Set("deliver_only", true)
	}
	if chatID := bodyStr(body, "deliver_chat_id"); chatID != "" {
		route.Set("deliver_extra", pyjson.ObjectOf("chat_id", chatID))
	}
	summary, err := configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		h := home.Dir(sctx)
		published, err := webhooksubs.Replace(h, name, route, expected)
		var conflict *webhooksubs.SubscriptionMutationConflict
		if errors.As(err, &conflict) {
			return nil, NewHTTPException(409, conflict.Error())
		}
		if err != nil {
			return nil, err
		}
		return webhookRouteSummary(name, published, webhooksubs.BaseURL(sctx, h))
	})
	if err != nil {
		return nil, err
	}
	summary.Set("secret", secret)
	return summary, nil
}

// Normalized key of an existing route in a lock held snapshot, 404 when the snapshot lacks it
func webhookKeyIn(subs *pyjson.Object, name string) (string, error) {
	key := pystr.Lower(pystr.Strip(name))
	if _, present := subs.Get(key); !present {
		return "", NewHTTPException(404, fmt.Sprintf("No subscription named '%s'", key))
	}
	return key, nil
}

// Removes a webhook subscription of the named profile, refused unnamed while several profiles are served
func deleteWebhook(ctx context.Context, name, profile string) (*pyjson.Object, error) {
	profile, err := destructiveProfile(ctx, profile, "DELETE /api/webhooks/{name}")
	if err != nil {
		return nil, err
	}
	_, err = configScopedToThread(ctx, profile, func(sctx context.Context) (bool, error) {
		return true, webhooksubs.Mutate(home.Dir(sctx), func(subs *pyjson.Object) error {
			key, err := webhookKeyIn(subs, name)
			if err != nil {
				return err
			}
			subs.Delete(key)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true), nil
}

// Turns one webhook route on or off in the store the gateway reloads on change
func setWebhookEnabled(ctx context.Context, name string, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	enabled := pyrepr.Truthy(body.GetOr("enabled", nil))
	key, err := configScopedToThread(ctx, profile, func(sctx context.Context) (string, error) {
		var key string
		err := webhooksubs.Mutate(home.Dir(sctx), func(subs *pyjson.Object) error {
			found, err := webhookKeyIn(subs, name)
			if err != nil {
				return err
			}
			route, ok := subs.Vals[found].(*pyjson.Object)
			if !ok {
				return fmt.Errorf("TypeError: '%s' object does not support item assignment", pyrepr.TypeName(subs.Vals[found]))
			}
			route.Set("enabled", enabled)
			key = found
			return nil
		})
		return key, err
	})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "name", key, "enabled", enabled), nil
}

// Starts the gateway of a profile in the background, 409 when the multiplexer already serves it
func startGateway(ctx context.Context, profile string) (*pyjson.Object, error) {
	refusal, err := multiplexedProfileRefusal(ctx, profile, "start")
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return nil, NewHTTPException(409, refusal)
	}
	proc, err := func() (actionProc, error) {
		argv, err := gatewaySubcommand(ctx, profile, "start")
		if err != nil {
			return nil, err
		}
		return spawnHermesAction(ctx, argv, "gateway-start", nil)
	}()
	if err := httpFailure(err, "Failed to spawn gateway start", 500, "Failed to start gateway", nil); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "pid", int64(proc.Pid()), "name", "gateway-start"), nil
}

// Stops the gateway of a profile in the background, 409 for a served profile or live sessions without interrupt
func stopGateway(ctx context.Context, profile string, interrupt bool) (*pyjson.Object, error) {
	refusal, err := multiplexedProfileRefusal(ctx, profile, "stop")
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return nil, NewHTTPException(409, refusal)
	}
	if !interrupt {
		refusal, err := liveSessionRefusal(ctx, profile, "stop")
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return nil, NewHTTPException(409, refusal)
		}
	}
	proc, err := func() (actionProc, error) {
		argv, err := gatewaySubcommand(ctx, profile, "stop")
		if err != nil {
			return nil, err
		}
		if interrupt {
			argv = append(argv, "--interrupt")
		}
		return spawnHermesAction(ctx, argv, "gateway-stop", nil)
	}()
	if err := httpFailure(err, "Failed to spawn gateway stop", 500, "Failed to stop gateway", nil); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "pid", int64(proc.Pid()), "name", "gateway-stop"), nil
}
