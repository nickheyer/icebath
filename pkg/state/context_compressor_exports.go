package state

// Synthetic compression scaffolding user row such as a continuation or todo injection
func IsSyntheticCompressionUserTurn(message map[string]any) bool {
	return isSyntheticCompressionUserTurn(message)
}
