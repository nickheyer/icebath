package state

// Session recency SQL expression over a sessions alias row, newest activity, message or start time
func SQLSessionLastActive(alias string) string { return sqlSessionLastActive(alias) }
