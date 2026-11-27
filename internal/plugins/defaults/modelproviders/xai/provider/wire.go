package provider

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// What an xAI sign-in that has no API access is told
const entitlementHint = "xAI rejected this OAuth account. NOTE: X Premium+ does NOT include xAI API access — only standalone SuperGrok " +
	"subscribers can use this provider. Other possible causes: no Grok subscription, your tier doesn't include this model, or your quota is " +
	"exhausted. Check https://grok.com/?_s=usage to see which, or run `/model` to switch providers."

// Why a Grok sign-in was refused when xAI says it has no subscription that covers the request
func rejectionHint(status int, _, message string) []string {
	lower := strings.ToLower(message)
	if status != 0 && status != 401 && status != 403 || strings.Contains(lower, "x premium+ does not include") ||
		strings.Contains(lower, "[wke=unauthenticated:") || strings.Contains(lower, "oauth2 access token could not be validated") {
		return nil
	}
	if strings.Contains(lower, "do not have an active grok subscription") ||
		strings.Contains(lower, "out of available resources") && strings.Contains(lower, "grok") ||
		strings.Contains(lower, "does not have permission") && strings.Contains(lower, "grok") {
		return []string{entitlementHint}
	}
	return nil
}

// Grok models that take the effort dial
var effortModels = []string{"grok-3-mini", "grok-4.20-multi-agent", "grok-4.3", "grok-4.5", "grok-4.6"}

// Efforts of Grok 4.6 and of the Grok models before it
var (
	grok46Efforts = []string{"low", "medium", "high", "xhigh"}
	legacyEfforts = []string{"low", "medium", "high"}
)

// Efforts a Grok model takes, none for one that takes no effort dial: its reasoning then stays on as the model has it
func efforts(_ context.Context, model string) ([]string, bool) {
	name := strings.ReplaceAll(provider.BareModel(model), "_", "-")
	if !provider.HasPrefix(name, effortModels...) {
		return nil, true
	}
	if provider.Grok46(name) {
		return slices.Clone(grok46Efforts), true
	}
	return slices.Clone(legacyEfforts), true
}
