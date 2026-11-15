package config

// Drops the process and per home active skins so the next read loads the default skin again
func ResetActiveSkinsForTests() {
	skinStateMu.Lock()
	activeSkin, activeSkinName, activeSkinsByHome = nil, "default", map[string]skinEntry{}
	skinStateMu.Unlock()
}
