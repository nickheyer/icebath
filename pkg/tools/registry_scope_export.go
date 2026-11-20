package tools

// Drops the durable scope attribution of a plugin module namespace, for a case whose module is the test package itself
func (r *Registry) ForgetPluginModuleScopeForTests(namespace, scope string) {
	e := r.ext()
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.moduleScopes[namespace], ScopeKey(scope))
	if len(e.moduleScopes[namespace]) == 0 {
		delete(e.moduleScopes, namespace)
	}
}
