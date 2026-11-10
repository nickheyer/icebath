package pet

// Signals are the coarse activity flags an animation state is derived from
type Signals struct {
	Busy, AwaitingInput, Error, Celebrate, JustCompleted, ToolRunning, Reasoning bool
}

// Reports a non empty task list whose every status is completed or cancelled
func TodosAllDone(statuses []string) bool {
	if len(statuses) == 0 {
		return false
	}
	for _, status := range statuses {
		if status != "completed" && status != "cancelled" {
			return false
		}
	}
	return true
}

// DeriveState resolves the animation state from the activity signals, the most salient one winning
func DeriveState(s Signals) State {
	ranked := []struct {
		flag  bool
		state State
	}{{s.Error, StateFailed}, {s.Celebrate, StateJump}, {s.JustCompleted, StateWave}, {s.AwaitingInput, StateWaiting},
		{s.ToolRunning, StateRun}, {s.Reasoning, StateReview}, {s.Busy, StateRun}}
	for _, r := range ranked {
		if r.flag {
			return r.state
		}
	}
	return StateIdle
}
