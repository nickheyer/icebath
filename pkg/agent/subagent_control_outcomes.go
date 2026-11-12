package agent

// Success status, success note and failure template of a targeted control action, false for an unknown action
func SubagentControlOutcome(action string) (string, string, string, bool) {
	outcome, ok := controlOutcomes[action]
	return outcome[0], outcome[1], outcome[2], ok
}
