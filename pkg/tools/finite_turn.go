package tools

import "sync"

const (
	finiteTurnKey     = "session.finite"
	unattendedTurnKey = "session.unattended_turn"
)

// Stores an admitted turn's finite and unattended flags on the session env, the returned func restoring the prior values
func BindFiniteTurn(env *Env, finite any, unattended bool) (restore func()) {
	if env == nil {
		return func() {}
	}
	if env.Values == nil {
		env.Values = &sync.Map{}
	}
	values := env.Values
	prevFinite, hadFinite := values.Load(finiteTurnKey)
	prevUnattended, hadUnattended := values.Load(unattendedTurnKey)
	values.Store(finiteTurnKey, finite)
	values.Store(unattendedTurnKey, unattended)
	return func() {
		for _, kv := range []struct {
			key  string
			prev any
			had  bool
		}{{finiteTurnKey, prevFinite, hadFinite}, {unattendedTurnKey, prevUnattended, hadUnattended}} {
			if kv.had {
				values.Store(kv.key, kv.prev)
			} else {
				values.Delete(kv.key)
			}
		}
	}
}

// True while the bound turn is finite and unattended, so nobody can answer a prompt and approvals pass
func UnattendedTurnFlag(env *Env) bool {
	if env == nil || env.Values == nil {
		return false
	}
	v, _ := env.Values.Load(unattendedTurnKey)
	b, _ := v.(bool)
	return b
}

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
