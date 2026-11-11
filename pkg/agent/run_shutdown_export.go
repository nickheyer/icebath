package agent

// Strips empty response retry scaffolding from the tail of the live transcript in place
func (a *AIAgent) DropTrailingSessionScaffolding() {
	a.dropTrailingEmptyResponseScaffolding(&a.sessionMessages)
}
