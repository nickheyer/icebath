package gateway

// Managed worker the uncounted total reads, live until its closed event is set
type managedWorkerHandle interface{ Closed() bool }

// Side agent holder the uncounted total reads, abandoned once stopped with no fork running
type sideAgentHolder interface{ Abandoned() bool }

// Live sessions whose drain task is running an execution generation
func executingSessions(a *SessionAuthority) map[string]*LiveSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]*LiveSession{}
	for sid, live := range a.Sessions {
		if live == nil || live.EventStream == nil || live.drain == nil {
			continue
		}
		if generation, _ := live.EventStream.Execution.Get("execution_generation"); generation == nil {
			continue
		}
		select {
		case <-live.drain:
		default:
			out[sid] = live
		}
	}
	return out
}

// Detached side agents of the authority whose thread has not unwound and that are not abandoned
func sideAgents(a *SessionAuthority) []sideAgentHolder {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []sideAgentHolder
	for _, holder := range a.sideAgents {
		if holder == nil || !holder.Abandoned() {
			out = append(out, holder)
		}
	}
	return out
}

// Claims absent from the ordinary agent map across the served authorities, pre bootstrap managed turns included
func UncountedRuntimeWork(runner any) int {
	running, _ := runner.(runningAgentsRunner)
	inRunning := func(route string) bool {
		if running == nil {
			return false
		}
		agent, pending := running.RunningAgent(route)
		return agent != nil || pending
	}
	authorities := allAuthorities(runner)
	total := 0
	for _, a := range authorities {
		a.mu.Lock()
		for _, worker := range a.managedWorkers {
			if !worker.Closed() {
				total++
			}
		}
		a.mu.Unlock()
	}
	for _, a := range authorities {
		for sid, live := range executingSessions(a) {
			a.mu.Lock()
			_, managed := a.managedWorkers[sid]
			a.mu.Unlock()
			if !inRunning(live.Route) && !managed {
				total++
			}
		}
	}
	for _, a := range authorities {
		total += len(mutationTasks(a)) + len(sideAgents(a))
	}
	return total
}
