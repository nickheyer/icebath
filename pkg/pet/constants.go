package pet

import (
	"math"
	"slices"
)

const (
	FrameW          = 192
	FrameH          = 208
	FramesPerState  = 6
	LoopMS          = 1100
	DefaultScale    = 0.33
	MinScale        = 0.1
	MaxScale        = 3.0
	BaseUnicodeCols = FrameW / 8
	UnicodeMinCols  = 16
)

// State is an animation state a pet can be shown in
type State string

const (
	StateIdle    State = "idle"
	StateWave    State = "wave"
	StateRun     State = "run"
	StateFailed  State = "failed"
	StateReview  State = "review"
	StateJump    State = "jump"
	StateWaiting State = "waiting"
)

var (
	States          = []State{StateIdle, StateWave, StateRun, StateFailed, StateReview, StateJump, StateWaiting}
	LegacyStateRows = []string{"idle", "wave", "run", "failed", "review", "jump", "extra1", "extra2"}
	CodexStateRows  = []string{"idle", "running-right", "running-left", "waving", "jumping", "failed", "waiting", "running", "review"}
	StateRows       = CodexStateRows
	codexNames      = map[string]string{"wave": "waving", "jump": "jumping", "run": "running"}
	StateAliases    = func() map[string][]string {
		out := map[string][]string{}
		for _, s := range []string{"idle", "wave", "jump", "run", "failed", "review", "waiting"} {
			out[s] = []string{s}
			if alias, ok := codexNames[s]; ok {
				out[s] = []string{s, alias}
			}
		}
		return out
	}()
)

// ClampScale keeps a scale inside the user settable bounds
func ClampScale(scale float64) float64 { return max(MinScale, min(MaxScale, scale)) }

// ColsForScale is the half block width a scale implies, never under the legibility floor
func ColsForScale(scale float64) int {
	if scale == 0 {
		scale = DefaultScale
	}
	return max(UnicodeMinCols, int(math.RoundToEven(BaseUnicodeCols*scale)))
}

// ResolveCols returns the explicit column override when positive, else the width the scale implies
func ResolveCols(scale float64, unicodeCols int) int {
	if unicodeCols > 0 {
		return unicodeCols
	}
	return ColsForScale(scale)
}

// StateAliasesFor returns the accepted row names of a state, the state itself when it has none
func StateAliasesFor(state string) []string {
	if aliases := StateAliases[state]; len(aliases) > 0 {
		return aliases
	}
	return []string{state}
}

// StateRowsForGrid returns the row taxonomy of a spritesheet with the given row count
func StateRowsForGrid(rowCount int) []string {
	if rowCount >= len(CodexStateRows) {
		return CodexStateRows
	}
	return LegacyStateRows
}

// StateRowIndex returns the spritesheet row of a state, the idle row when the sheet has no such row
func StateRowIndex(state string, rowCount int) int {
	rows := StateRowsForGrid(rowCount)
	for _, name := range StateAliasesFor(state) {
		if i := slices.Index(rows, name); i >= 0 {
			return i
		}
	}
	return 0
}
