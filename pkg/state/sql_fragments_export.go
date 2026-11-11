package state

// Non throwing JSON marker lookup over a JSON text column, for SQL built outside this package
func SQLJSONExtract(expression, path string) string { return sqlJSONExtract(expression, path) }

// Predicate of a reset child row under the table alias, for SQL built outside this package
func ResetChildSQL(alias string) string { return resetChildSQL(alias) }
