package gatewayrun

import "fmt"

// Remembers a good model for the session and process wide, or recovers an empty one from the last known good
func recoverOrRememberModel(r *GatewayRunner, sessionKey, model string) string {
	if model == "" {
		recovered := ""
		if sessionKey != "" {
			if state := r.peekSessionState(sessionKey); state != nil {
				recovered = state.conversation.lastResolvedModel
			}
		}
		if recovered == "" {
			if star := r.peekSessionState("*"); star != nil {
				recovered = star.conversation.lastResolvedModel
			}
		}
		if recovered != "" {
			runLog().Warn(fmt.Sprintf("Empty model resolved for session=%s — recovering last-known-good model %s (config read likely returned empty; see #35314)",
				sessionKey, recovered))
			model = recovered
		}
		return model
	}
	if sessionKey != "" {
		r.sessionState(sessionKey).conversation.lastResolvedModel = model
	}
	r.sessionState("*").conversation.lastResolvedModel = model
	return model
}
