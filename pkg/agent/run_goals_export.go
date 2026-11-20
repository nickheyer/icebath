package agent

// Whether the loop is due against a clock reading taken before the scan
func (m *LoopManager) IsDueAt(now float64) bool { return m.isDueAt(now) }
