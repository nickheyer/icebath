package skills

// Installs fn as the core command lookup and returns the restore of the lookup installed before it
func SwapCoreCommandResolverForTests(fn func(name string) bool) (restore func()) {
	coreCommandMu.Lock()
	prev := coreCommandResolver
	coreCommandResolver = fn
	coreCommandMu.Unlock()
	return func() { RegisterCoreCommandResolver(prev) }
}
