package tools

import "sync"

const finiteTurnKey = "session.finite"

// SetFiniteTurn records the admitted turn's finite flag on the session env the tools read
func SetFiniteTurn(env *Env, finite bool) {
	if env == nil {
		return
	}
	if env.Values == nil {
		env.Values = &sync.Map{}
	}
	env.Values.Store(finiteTurnKey, finite)
}

// FiniteTurnFlag reports the recorded finite flag and whether the env carries one
func FiniteTurnFlag(env *Env) (bool, bool) {
	if env == nil || env.Values == nil {
		return false, false
	}
	v, ok := env.Values.Load(finiteTurnKey)
	if !ok {
		return false, false
	}
	b, isBool := v.(bool)
	return b && isBool, isBool
}
