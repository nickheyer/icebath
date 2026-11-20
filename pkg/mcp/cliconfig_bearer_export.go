package mcp

// Pasted token without a leading Bearer word and surrounding whitespace
func StripBearerPrefix(token string) string { return stripBearerPrefix(token) }

// Persisted Authorization header template naming the MCP_<NAME>_API_KEY env key
func BearerAuthHeaders(name string) map[string]any { return bearerAuthHeaders(name) }
