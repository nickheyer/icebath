package app

// Last lines of a log file that pass a level and component filter, the tail the dashboard logs route reads
func ReadLogTail(path string, numLines int, hasFilters bool, minLevel *string, componentPrefixes []string, hasComponent bool) ([]string, error) {
	return readLogTail(path, numLines, hasFilters, logFilters{minLevel: minLevel, componentPrefixes: componentPrefixes, hasComponent: hasComponent})
}
