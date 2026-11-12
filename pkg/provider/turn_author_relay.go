package provider

import (
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Bot author of a relayed DM's recipient turn from the reported sender fields, nil when no sender profile is named
func DeliveryTurnAuthor(fromProfile, fromHandle, fromConnection any) map[string]any {
	profile := pystr.Strip(relayText(fromProfile))
	if profile == "" {
		return nil
	}
	name := pystr.Strip(relayText(fromHandle))
	if name == "" {
		name = profile
	}
	return map[string]any{"id": BotAuthorID(profile, relayText(fromConnection)), "name": name, "is_bot": true}
}

// Bot author of a relayed DM whose sender fields came from a logged in client, built from its minted principal
func RelayingPrincipalAuthor(principal string) map[string]any {
	return map[string]any{"id": BotAuthorID("relay", pystr.Strip(principal)), "name": "relayed teammate", "is_bot": true}
}

// Python str of a value or empty string for a falsy one
func relayText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}
