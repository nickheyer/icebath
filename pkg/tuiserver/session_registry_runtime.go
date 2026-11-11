package tuiserver

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Auxiliary client main runtime of a live agent so a one shot inherits its session route, else nil
func mainRuntimeFromAgent(a any) map[string]any {
	var fields [][2]string
	switch live := a.(type) {
	case *agent.AIAgent:
		if live == nil {
			return nil
		}
		fields = [][2]string{{"provider", live.Provider()}, {"model", live.Model()}, {"base_url", live.BaseURL()},
			{"api_key", live.APIKey()}, {"api_mode", live.APIMode()}, {"session_id", live.SessionID()}}
	case *SyntheticHeavyAgent:
		if live == nil {
			return nil
		}
		fields = [][2]string{{"provider", live.Provider}, {"model", live.Model}, {"base_url", live.BaseURL},
			{"api_key", live.APIKey}, {"api_mode", live.APIMode}, {"session_id", live.SessionID}}
	default:
		return nil
	}
	runtime := map[string]any{}
	for _, field := range fields {
		if value := strings.TrimSpace(field[1]); value != "" {
			runtime[field[0]] = value
		}
	}
	if len(runtime) == 0 {
		return nil
	}
	return runtime
}

// Main runtime of a live agent for the gateway one shot verbs
func MainRuntimeFromAgent(a any) map[string]any { return mainRuntimeFromAgent(a) }
