package gateway

// SetLocalVoiceMode sets and persists a local session's voice mode on the running owner, nil when no owner here holds it
func SetLocalVoiceMode(sessionID any, mode string) (*bool, error) {
	runner := ActiveAPIRunner()
	id, ok := sessionID.(string)
	if runner == nil || !ok || id == "" {
		return nil, nil
	}
	for _, a := range AllAuthorities(runner) {
		live, held := a.live(id)
		if !held {
			continue
		}
		modes, isVoiceRunner := runner.(voiceModeRunner)
		if !isVoiceRunner {
			return nil, attributeError("_voice_mode")
		}
		modes.StoreVoiceMode(live.Source, mode)
		LocalVoiceTransition(live.Source, mode)
		held = true
		return &held, nil
	}
	return nil, nil
}
