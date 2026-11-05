package tools

// Outcomes a batch clarify can end with
var clarifyOutcomes = map[string]bool{"submitted": true, "timed_out": true, "cancelled": true}

// SettleClarifyBatch computes answers and outcome for a batch from its locks and the closing frame or the way the wait ended
func SettleClarifyBatch(locked map[string]any, frameResult any, ended string) map[string]any {
	answers := map[string]any{}
	for k, v := range locked {
		answers[k] = v
	}
	if ended != "" {
		outcome := ended
		if !clarifyOutcomes[outcome] {
			outcome = "cancelled"
		}
		return map[string]any{"answers": answers, "outcome": outcome}
	}
	if frame, ok := frameResult.(map[string]any); ok {
		if frameAnswers, ok := frame["answers"].(map[string]any); ok {
			out := map[string]any{}
			for k, v := range frame {
				out[k] = v
			}
			for k, v := range frameAnswers {
				answers[k] = v
			}
			out["answers"] = answers
			outcome, _ := frame["outcome"].(string)
			if !clarifyOutcomes[outcome] {
				outcome = "submitted"
			}
			out["outcome"] = outcome
			return out
		}
	}
	return map[string]any{"answers": answers, "outcome": "cancelled"}
}
