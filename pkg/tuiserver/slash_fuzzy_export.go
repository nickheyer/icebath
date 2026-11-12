package tuiserver

// Lowercased value followed by its alphanumeric words, the search tokens of a slash menu field
func TokenizeSearchText(value string) []string { return tokenizeSearchText(value) }

// Slash query trimmed, stripped of leading slashes and lowercased
func NormalizeSlashSearchQuery(query string) string { return normalizeSlashSearchQuery(query) }

// Best exact, prefix or substring tier of any field plus offset, infinity on a miss
func ScoreFields(fields []string, query string, offset float64) float64 {
	return scoreFields(fields, query, offset)
}
