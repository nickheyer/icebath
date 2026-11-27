package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/models"
	codexmodels "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/models"
	"github.com/nickheyer/icebath/internal/provider"
)

// Share of a window at which it is spent
const exhausted = 100.0

// Addresses of the account's usage, of its banked resets and of spending one, as the Codex CLI splits them
func backend(base string) (usage, resets, consume string) {
	normalized := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(base), "/"), "/codex")
	if normalized == "" {
		normalized = "https://chatgpt.com/backend-api"
	}
	prefix := normalized + "/api/codex"
	if strings.Contains(normalized, "/backend-api") {
		prefix = normalized + "/wham"
	}
	return prefix + "/usage", prefix + "/rate-limit-reset-credits", prefix + "/rate-limit-reset-credits/consume"
}

func headers(token string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json", "User-Agent": "codex-cli"}
	maps.Copy(h, codexmodels.AccountHeaders(token))
	return h
}

// Windows of the ChatGPT account's Codex limits, its banked resets and its credit balance
func accountUsage(ctx context.Context, route provider.Route) (*provider.AccountUsage, error) {
	token := strings.TrimSpace(route.APIKey)
	if token == "" {
		return nil, nil
	}
	route, payload, err := renewed(ctx, route, func(route provider.Route) (map[string]any, error) {
		address, _, _ := backend(route.BaseURL)
		return models.Request(ctx, "GET", address, headers(route.APIKey), nil, 15*time.Second)
	})
	if err != nil {
		return nil, err
	}
	limits, _ := payload["rate_limit"].(map[string]any)
	var windows []provider.UsageWindow
	for _, position := range [][2]string{{"primary_window", "Session"}, {"secondary_window", "Weekly"}} {
		window, _ := limits[position[0]].(map[string]any)
		used, ok := window["used_percent"].(float64)
		if !ok {
			continue
		}
		label := position[1]
		switch window["limit_window_seconds"] {
		case float64(18000):
			label = "Session"
		case float64(604800):
			label = "Weekly"
		}
		windows = append(windows, provider.AccountWindow(label, &used, resetAt(window["reset_at"]), ""))
	}
	var details []string
	if count := banked(payload); count > 0 {
		details = append(details, fmt.Sprintf("You have %d reset%s banked - use /usage reset to activate", count, plural(count)))
	}
	credits, _ := payload["credits"].(map[string]any)
	if has, _ := credits["has_credits"].(bool); has {
		if balance, ok := credits["balance"].(float64); ok {
			details = append(details, fmt.Sprintf("Credits balance: $%.2f", balance))
		} else if unlimited, _ := credits["unlimited"].(bool); unlimited {
			details = append(details, "Credits balance: unlimited")
		}
	}
	reading := provider.NewAccountUsage(route.Provider, "usage_api", windows, details)
	if plan, _ := payload["plan_type"].(string); strings.TrimSpace(plan) != "" {
		reading.Plan = titled(plan)
	}
	reading.Identity = codexmodels.Identity(token)
	return reading, nil
}

// Resets the account has banked
func banked(payload map[string]any) int {
	credits, _ := payload["rate_limit_reset_credits"].(map[string]any)
	count, _ := credits["available_count"].(float64)
	return int(count)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Words of a slug with their first letters raised
func titled(slug string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(strings.TrimSpace(slug)))
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + strings.ToLower(word[1:])
	}
	return strings.Join(words, " ")
}

// Time of a reset in seconds since the epoch or as ISO text, nil when not known
func resetAt(value any) *time.Time {
	switch v := value.(type) {
	case float64:
		at := time.Unix(0, int64(v*1e9)).UTC()
		return &at
	case string:
		if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v)); err == nil {
			return &at
		}
	}
	return nil
}

// Spends one banked reset of the account's limits, refusing while they aren't spent unless forced, since a reset restores them in full
func resetUsage(ctx context.Context, route provider.Route, force bool) string {
	token := strings.TrimSpace(route.APIKey)
	if token == "" {
		return "No Codex credentials available. Sign in with your ChatGPT account in /providers."
	}
	route, payload, err := renewed(ctx, route, func(route provider.Route) (map[string]any, error) {
		address, _, _ := backend(route.BaseURL)
		return models.Request(ctx, "GET", address, headers(route.APIKey), nil, 15*time.Second)
	})
	if err != nil {
		return failure(err)
	}
	token = route.APIKey
	_, _, consume := backend(route.BaseURL)
	available := banked(payload)
	if available <= 0 {
		return "No banked reset credits on this account — nothing to redeem."
	}
	limits, _ := payload["rate_limit"].(map[string]any)
	busiest, known := 0.0, false
	for _, position := range []string{"primary_window", "secondary_window"} {
		window, _ := limits[position].(map[string]any)
		if used, ok := window["used_percent"].(float64); ok {
			busiest, known = max(busiest, used), true
		}
	}
	if !force && (!known || busiest < exhausted) {
		note := "your current usage could not be confirmed as exhausted"
		if known {
			note = fmt.Sprintf("your busiest window is only %.0f%% used", busiest)
		}
		return fmt.Sprintf("⚠️ Not redeeming: %s. A banked reset restores your FULL 5h + weekly limits, so spending it now would waste most of it. "+
			"You have %d reset%s banked. Use `/usage reset --force` to redeem anyway.", note, available, plural(available))
	}
	request, _ := json.Marshal(map[string]string{"redeem_request_id": uuid.NewString()})
	consumeHeaders := headers(token)
	consumeHeaders["Content-Type"] = "application/json"
	body, err := models.Request(ctx, "POST", consume, consumeHeaders, request, 15*time.Second)
	if err != nil {
		return failure(err)
	}
	remaining := max(0, available-1)
	switch code, _ := body["code"].(string); strings.ToLower(strings.TrimSpace(code)) {
	case "reset":
		// The account's limit lifted, so the rotation may take it again
		if route.Keeper != nil && route.Keeper.Lifted != nil {
			route.Keeper.Lifted()
		}
		return fmt.Sprintf("✅ Reset redeemed — your usage limits have been reset. %d banked reset%s remaining.", remaining, plural(remaining))
	case "nothing_to_reset":
		return "Backend reports nothing to reset — your limits aren't exhausted. The credit was NOT spent."
	case "no_credit":
		return "Backend reports no available reset credit on this account."
	case "already_redeemed":
		return "This redemption was already processed — no additional credit was spent."
	}
	raw, _ := json.Marshal(body)
	return "Unexpected response from the Codex backend: " + string(raw)
}

// What a failed request to the Codex backend means for the person
func failure(err error) string {
	switch status := models.Status(err); {
	case status == 401 || status == 403:
		return fmt.Sprintf("Codex backend rejected the request (HTTP %d). Reset credits require ChatGPT-account (OAuth) auth — sign in with your "+
			"ChatGPT account in /providers.", status)
	case status != 0:
		return fmt.Sprintf("Codex backend error (HTTP %d) — try again shortly.", status)
	}
	return "Could not reach the Codex backend: " + err.Error()
}

// Answer of a call to the Codex backend, made once more with the sign-in renewed when the backend refused it; the route it ended with
func renewed(ctx context.Context, route provider.Route, call func(route provider.Route) (map[string]any, error)) (provider.Route, map[string]any, error) {
	payload, err := call(route)
	if status := models.Status(err); status != 401 || route.Keeper == nil {
		return route, payload, err
	}
	next, ok := route.Keeper.Recover(ctx, &provider.APIError{Provider: route.Provider, Model: route.Model, StatusCode: 401, Kind: provider.ErrAuth, Err: err})
	if !ok {
		return route, payload, err
	}
	payload, err = call(next)
	return next, payload, err
}
