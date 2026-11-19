package tools

// ResetCachedSudoPasswords clears every cached sudo password
func ResetCachedSudoPasswords() {
	sudoPasswordCache.Lock()
	defer sudoPasswordCache.Unlock()
	sudoPasswordCache.byScope = map[string]string{}
}
