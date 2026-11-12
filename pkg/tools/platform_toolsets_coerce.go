package tools

// Saved platform toolsets value as the list it encodes, false for an unset or non list value
func CoercePlatformToolsetsValue(value any, platform string) ([]string, bool) {
	return coercePlatformToolsetsValue(value, platform)
}
