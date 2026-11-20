package tools

// Whether a mapping key names a secret by a word bounded secret keyword
func KeyHasSecretKeyword(key string) bool { return keyHasSecretKeyword(key) }
