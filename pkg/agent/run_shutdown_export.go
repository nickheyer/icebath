package agent

import "github.com/nickheyer/icebath/pkg/state"

// Strips empty response retry scaffolding from the tail of the live transcript in place
func (a *AIAgent) DropTrailingSessionScaffolding() {
	messages := a.liveSessionMessages()
	a.dropTrailingEmptyResponseScaffolding(&messages)
	a.setSessionMessages(messages)
}

// Flushes the live in memory transcript to the session store, stamping the durable markers on its rows
func (a *AIAgent) FlushSessionMessages() state.FlushOutcome {
	return a.flushLiveMessages(a.liveSessionMessages(), nil)
}
