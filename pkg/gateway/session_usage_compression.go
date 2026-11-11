package gateway

// Agent whose context engine counts its compactions
type compressionCountingAgent interface {
	ContextCompressionCount() (int, bool)
	SeedContextCompressionCount(count int)
}

// Compactions of the conversation, the larger of the live session's carried count and the agent's own
func carriedCompressionCount(live *LiveSession, agent any) int {
	own := 0
	if counting, ok := agent.(compressionCountingAgent); ok {
		own, _ = counting.ContextCompressionCount()
	}
	carried := 0
	if live != nil {
		carried = live.CompressionCount
	}
	return max(carried, own)
}

// Counts one more compaction than the evicted agent saw after a committed compress
func noteCompression(a *SessionAuthority, ref SessionRef, live *LiveSession) {
	agent, err := a.agent(ref)
	if err != nil {
		agent = nil
	}
	live.CompressionCount = carriedCompressionCount(live, agent) + 1
}

// Seeds a successor agent's engine with the conversation's carried compaction count
func seedCompressionCount(live *LiveSession, agent any) {
	counting, ok := agent.(compressionCountingAgent)
	if live == nil || !ok {
		return
	}
	if _, has := counting.ContextCompressionCount(); has {
		counting.SeedContextCompressionCount(carriedCompressionCount(live, agent))
	}
}
