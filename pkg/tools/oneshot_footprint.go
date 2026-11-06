package tools

import "sync"

// OneshotSkillsLoadGuidance replaces the skills coaching in a one shot session prompt
const OneshotSkillsLoadGuidance = "## Skills\n" +
	"Scan the skills below and load one with skill_view(name) only when it carries domain knowledge you lack " +
	"for THIS task (an API, a tool's commands, a project's conventions). Do not load general process skills " +
	"(testing, debugging, review methodology) for work you already know how to do.\n"

// ActivePolicySource is the gateway session policy source of the turn, set by the gateway, false without a policy
var ActivePolicySource func(env *Env) (source string, ok bool)

// Finite single query session from the frozen gateway policy, else the session env marker
func IsSingleQuerySession(env *Env) bool {
	if ActivePolicySource != nil {
		if source, ok := ActivePolicySource(env); ok {
			return source == "oneshot"
		}
	}
	return SessionEnvValue(env, "ICEBATH_SINGLE_QUERY_SESSION") == "1"
}

// OneshotSession is the one shot fact an agent freezes at its first tool build
type OneshotSession struct {
	mu     sync.Mutex
	pinned bool
	value  bool
}

// Frozen fact and whether it was pinned, the getattr of the cached flag
func (o *OneshotSession) Pinned() (value, pinned bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.value, o.pinned
}

// One shot fact frozen on the agent at first use so later tool and prompt builds answer the same
func AgentIsOneshot(o *OneshotSession, env *Env) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.pinned {
		o.pinned, o.value = true, IsSingleQuerySession(env)
	}
	return o.value
}
