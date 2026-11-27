package provider

import (
	"cmp"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/credentials"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
	codexmodels "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/models"
	"github.com/nickheyer/icebath/internal/provider"
)

// Model id as the Codex backend names it: its own prefix or OpenAI's dropped
func native(spec provider.Spec, model string) string {
	if stripped := provider.Unprefixed(spec, model); stripped != model {
		return stripped
	}
	for _, prefix := range []string{"openai/", "openai:"} {
		if rest, ok := strings.CutPrefix(model, prefix); ok {
			return rest
		}
	}
	return model
}

// Identity the Codex backend takes a request under, with the account the token signs in
func identity(call *provider.Call) map[string]string {
	return codexmodels.Headers(call.Route.APIKey, call.Route.BaseURL)
}

// Cyber access program the research access setting asks for
const daybreakProgram = "daybreak_blue"

// The person's own surfaces, the only sessions research access is asked in
var ownSurfaces = []string{"cli", "tui", "gui", "desktop", "oneshot"}

// The request as the account's catalog takes it on OpenAI's own backend: a faster tier asked for clamped to one the catalog lists for the
// model, the default when it lists neither; and research access asked for in the person's own sessions when it is on and listed
func finish(call *provider.Call, body map[string]any) {
	token := strings.TrimSpace(call.Route.APIKey)
	if !provider.OfficialCodex(call.Route.BaseURL) || token == "" || len(credentials.Claims(token)) == 0 {
		return
	}
	model, _ := body["model"].(string)
	model = cmp.Or(model, call.Model)
	if requested, _ := body["service_tier"].(string); requested == "ultrafast" || requested == "priority" {
		if supported := codexmodels.Tiers(call.Context, token, call.Route.BaseURL, model); supported != nil {
			candidates := []string{"priority"}
			if requested == "ultrafast" {
				candidates = []string{"ultrafast", "priority"}
			}
			effective := "default"
			for _, tier := range candidates {
				if supported[tier] {
					effective = tier
					break
				}
			}
			if effective == "default" {
				delete(body, "service_tier")
			} else {
				body["service_tier"] = effective
			}
			if effective != requested {
				slog.Debug(fmt.Sprintf("Codex service tier %s -> %s: not advertised for selected credential", requested, effective))
			}
		}
	}
	if !call.Settings.GetBool(openaicodexconfig.Daybreak) || !slices.Contains(ownSurfaces, call.Source) {
		return
	}
	if !codexmodels.Programs(call.Context, token, call.Route.BaseURL, model)[daybreakProgram] {
		return
	}
	access, _ := body["access_programs"].(map[string]any)
	access = maps.Clone(access)
	if access == nil {
		access = map[string]any{}
	}
	access["cyber"] = daybreakProgram
	body["access_programs"] = access
}
