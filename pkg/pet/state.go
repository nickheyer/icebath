package pet

// Signals are the coarse activity flags an animation state is derived from
type Signals struct {
	Busy, AwaitingInput, Error, Celebrate, JustCompleted, ToolRunning, Reasoning bool
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
