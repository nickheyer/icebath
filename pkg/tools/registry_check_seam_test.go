package tools

// CachedCheckResult returns a probe's live cached verdict without ever running it
func CachedCheckResult(env *Env, fn func(*Env) bool) (bool, bool) {
	scope, bypass := CheckFnCacheScope(env)
	if bypass {
		return false, false
	}
	key := checkKey{fnPointer(fn), scope}
	now := checkNow()
	checkMu.Lock()
	defer checkMu.Unlock()
	v, ok := checkCache[key]
	if !ok || now.Sub(v.at) >= checkTTL {
		return false, false
	}
	return v.value, true
}
