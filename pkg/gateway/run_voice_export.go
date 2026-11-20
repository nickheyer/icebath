package gateway

// Chat set of the adapter's auto TTS overrides, the enabled set or the disabled set, mutated in place
func (b *BasePlatformAdapter) AutoTTSChats(enabled bool) map[string]struct{} {
	if enabled {
		return b.autoTTSEnabledChats
	}
	return b.autoTTSDisabledChats
}

// Sets whether replies are spoken by default when a chat holds no override
func (b *BasePlatformAdapter) SetAutoTTSDefault(on bool) { b.autoTTSDefault = on }
