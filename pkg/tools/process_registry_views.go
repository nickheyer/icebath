package tools

import "sort"

// One tracked process as an owner projection reads it, its output trimmed to a tail
type ProcessView struct {
	ID               string
	Command          string
	SessionKey       string
	OwnerTaskID      string
	TaskID           string
	Exited           bool
	ExitCode         *int
	StartedAt        float64
	ExitedAt         float64
	CompletionReason string
	OutputTail       string
	ScopeStopPending bool
}

// Running then finished processes in spawn order, read under the registry lock and each session lock
func (r *ProcessRegistry) ProcessViews(tailLength int) []ProcessView {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []ProcessView{}
	for _, store := range []map[string]*ProcessSession{r.running, r.finished} {
		group := make([]*ProcessSession, 0, len(store))
		for _, s := range store {
			group = append(group, s)
		}
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].StartedAt != group[j].StartedAt {
				return group[i].StartedAt < group[j].StartedAt
			}
			return group[i].ID < group[j].ID
		})
		for _, s := range group {
			s.mu.Lock()
			out = append(out, ProcessView{ID: s.ID, Command: s.Command, SessionKey: s.SessionKey, OwnerTaskID: s.OwnerTaskID,
				TaskID: s.TaskID, Exited: s.Exited, ExitCode: s.ExitCode, StartedAt: s.StartedAt, ExitedAt: s.ExitedAt,
				CompletionReason: s.CompletionReason, OutputTail: tailChars(s.OutputBuffer, tailLength), ScopeStopPending: s.scopeStopPending})
			s.mu.Unlock()
		}
	}
	return out
}
