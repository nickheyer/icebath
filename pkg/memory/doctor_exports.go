package memory

// Memory and user profile store flags of a memory config section, each enabled unless set falsy
func BuiltinMemoryStoreFlags(section map[string]any) (bool, bool) {
	return builtinMemoryStoreFlags(section)
}
