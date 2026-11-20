package state

// Whether an error says the core or FTS tables are not built yet
func SchemaNotBuilt(err error) bool { return schemaNotBuilt(err) }
