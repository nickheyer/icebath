package tools

// LookupSchemaRewriter returns the dynamic schema rewriter installed for a tool name, nil when none is
func LookupSchemaRewriter(name string) SchemaRewriter {
	schemaRewritersMu.RLock()
	defer schemaRewritersMu.RUnlock()
	return schemaRewriters[name]
}
