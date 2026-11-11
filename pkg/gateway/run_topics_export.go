package gateway

// Longest prefix of s within limit UTF-16 code units, never splitting a surrogate pair
func PrefixWithinUTF16Limit(s string, limit int) string { return prefixWithinUTF16Limit(s, limit) }
