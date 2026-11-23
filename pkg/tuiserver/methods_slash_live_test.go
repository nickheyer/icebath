package tuiserver

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Local session record with a short user and assistant history and the given agent
func liveSlashSession(a *agent.AIAgent) *tuihost.Session {
	fields := map[string]any{"session_key": "sk-1", "session_id": "sid-1", "running": false, "history_lock": &sync.Mutex{},
		"history": []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": "hi"}}}
	if a != nil {
		fields["agent"] = a
	}
	return tuihost.NewSession(fields)
}

// Live agent on a dead loopback endpoint for the context view
func liveSlashAgent(t *testing.T) *agent.AIAgent {
	return testAgent(t, agent.AIAgentOptions{Model: "test-model", Provider: "openrouter"})
}

// Swaps the breakdown and details engines for the duration of a case
func stubContextEngines(t *testing.T, payload agent.ContextBreakdown, details *agent.ContextDetails) (*int, *int) {
	breakdownCalls, detailsCalls := new(int), new(int)
	prevBreakdown, prevDetails := computeSessionContextBreakdown, computeContextDetails
	computeSessionContextBreakdown = func(context.Context, *agent.AIAgent, []map[string]any) (agent.ContextBreakdown, error) {
		*breakdownCalls++
		return payload, nil
	}
	computeContextDetails = func(context.Context, *agent.AIAgent) (agent.ContextDetails, error) {
		*detailsCalls++
		if details == nil {
			return agent.ContextDetails{}, nil
		}
		return *details, nil
	}
	t.Cleanup(func() { computeSessionContextBreakdown, computeContextDetails = prevBreakdown, prevDetails })
	return breakdownCalls, detailsCalls
}

// Agent double recording each explicit background review it is asked to spawn
type refineRecordingAgent struct{ spawned []map[string]any }

func (a *refineRecordingAgent) ValidToolNames() map[string]bool {
	return map[string]bool{"memory": true, "skill_manage": true}
}

func (a *refineRecordingAgent) SpawnExplicitBackgroundReview(ctx context.Context, snapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string) {
	var f any
	if focus != nil {
		f = *focus
	}
	a.spawned = append(a.spawned, map[string]any{"messages_snapshot": snapshot, "review_memory": reviewMemory, "review_skills": reviewSkills,
		"focus": f, "explicit": true})
}

// Session record of the refine cases holding the recording agent
func refineSession(a *refineRecordingAgent, key string, history []any) *tuihost.Session {
	return tuihost.NewSession(map[string]any{"agent": a, "session_key": key, "history": history, "history_lock": &sync.Mutex{},
		"running": false, "cwd": "", "source": "tui"})
}

// Spawned review of the recording agent with only role and content kept on each snapshot row
func refineSpawned(a *refineRecordingAgent) []map[string]any {
	out := make([]map[string]any, 0, len(a.spawned))
	for _, call := range a.spawned {
		rows := []map[string]any{}
		for _, row := range call["messages_snapshot"].([]map[string]any) {
			rows = append(rows, map[string]any{"role": row["role"], "content": row["content"]})
		}
		copied := map[string]any{}
		for k, v := range call {
			copied[k] = v
		}
		copied["messages_snapshot"] = rows
		out = append(out, copied)
	}
	return out
}
