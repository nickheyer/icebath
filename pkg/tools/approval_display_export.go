package tools

// Secret redaction plus display sanitizing of approval text for the gateway's own prompts
func RedactApprovalDisplay(env *Env, text string, force bool) string {
	return redactApprovalDisplay(env, text, force)
}

// Whether a pending clarify was answered, cancelled or timed out
func (e *ClarifyEntry) Resolved() bool {
	clarifyMu.Lock()
	defer clarifyMu.Unlock()
	return e.resolved
}
