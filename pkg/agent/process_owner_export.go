package agent

// Raw spawning task ids whose background processes close kills, pre compression ids included
func (a *AIAgent) ProcessOwnerTaskIDs() []string {
	if a == nil {
		return nil
	}
	a.processOwnerMu.Lock()
	defer a.processOwnerMu.Unlock()
	out := make([]string, 0, len(a.processOwnerTaskIDs))
	for owner := range a.processOwnerTaskIDs {
		out = append(out, owner)
	}
	return out
}
