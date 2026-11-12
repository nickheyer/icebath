package gatewayrun

import "fmt"

// Remembers a good model for the session and process wide, or recovers an empty one from the last known good
func recoverOrRememberModel(r *GatewayRunner, sessionKey, model string) string {
	if model == "" {
		recovered := ""
		if sessionKey != "" {
			recovered, _ = r.lastResolvedModel().Get(sessionKey)
		}
		if recovered == "" {
			recovered, _ = r.lastResolvedModel().Get("*")
		}
		if recovered != "" {
			runLog().Warn(fmt.Sprintf("Empty model resolved for session=%s — recovering last-known-good model %s (config read likely returned empty; see #35314)",
				sessionKey, recovered))
			model = recovered
		}
		return model
	}
	if sessionKey != "" {
		r.lastResolvedModel().Set(sessionKey, model)
	}
	r.lastResolvedModel().Set("*", model)
	return model
}
