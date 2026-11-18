package provider

import (
	"strings"
)

// Emits each line of the message as a hint, false when there is nothing to print
func printGuidance(emit func(...string), message string) bool {
	if message == "" {
		return false
	}
	for _, line := range pySplitLines(message) {
		emit("   💡 " + line)
	}
	return true
}

// BillingOrEntitlementMessage is the next step copy for a billing classified failure on any provider
func BillingOrEntitlementMessage(capability, provider, baseURL, model string, unverified bool) string {
	providerLabel := strings.TrimSpace(provider)
	if providerLabel == "" {
		providerLabel = "the selected provider"
	}
	modelLabel := strings.TrimSpace(model)
	if modelLabel == "" {
		modelLabel = "the selected model"
	}
	if strings.ToLower(strings.TrimSpace(provider)) == "anthropic" {
		switchLine := "You can also switch to an Anthropic API key or another provider with /model <model> --provider <provider>."
		if unverified {
			return strings.Join([]string{
				providerLabel + " reported that your Claude subscription usage may be exhausted for " + modelLabel +
					" (included quota + extra-usage credits) — but this specific error is not proof of a billing problem.",
				"If https://claude.ai/settings/usage still shows quota remaining, this is probably NOT a billing problem: on a Claude subscription (OAuth) token Anthropic returns this same message when its content filter rejects part of the request — typically a phrase in the system prompt.",
				"If usage really is exhausted: wait for the billing cycle to reset, or add extra usage at https://claude.ai/settings/usage",
				switchLine,
				"Retry with a fresh credential state: `icebath auth reset anthropic`. Until that cooldown clears, this error can be replayed from cache without contacting the API.",
			}, "\n")
		}
		return strings.Join([]string{
			providerLabel + " reported that your Claude subscription usage is exhausted for " + modelLabel + " (included quota + extra-usage credits).",
			"Options: wait for the billing cycle to reset, or add extra usage at https://claude.ai/settings/usage",
			switchLine,
		}, "\n")
	}
	link := BuildBillingBlock(provider, baseURL, model, "")
	if link.ProviderLabel != "" {
		providerLabel = link.ProviderLabel
	}
	lines := []string{
		providerLabel + " reported that billing, credits, or account entitlement is exhausted for " + modelLabel + ".",
		"Add credits or update billing with that provider, then retry.",
	}
	if link.BillingURL != "" {
		lines = append(lines, providerLabel+" billing: "+link.BillingURL)
	}
	lines = append(lines, "You can switch providers temporarily with /model <model> --provider <provider>.")
	return strings.Join(lines, "\n")
}

// BillingBlockDict is the structured billing descriptor, flagged unverified when the cause may be a filter
func BillingBlockDict(provider, baseURL, model, message string, unverified bool) map[string]any {
	built, err := BuildBillingBlockErr(provider, baseURL, model, message)
	if err != nil {
		return nil
	}
	block := built.ToDict()
	if unverified {
		block["unverified"] = true
	}
	return block
}

// BillingTerminalLabel prefixes a billing failure summary without asserting an unverified exhaustion
func BillingTerminalLabel(summary string, unverified bool) string {
	if unverified {
		return "Provider reported usage/credit exhaustion (unverified — the same error can be a content-filter rejection, not billing): " + summary
	}
	return "Billing or credits exhausted: " + summary
}

// PrintBillingOrEntitlementGuidance emits the billing or entitlement next steps, false when there are none
func PrintBillingOrEntitlementGuidance(emit func(...string), capability, provider, baseURL, model string, unverified bool) bool {
	return printGuidance(emit, BillingOrEntitlementMessage(capability, provider, baseURL, model, unverified))
}
