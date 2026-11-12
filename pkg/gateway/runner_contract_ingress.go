package gateway

// Runner methods the session ingress, local restore, drain, identity, route mount and worker construction call
type RunnerIngressContract interface {
	adapterMapRunner
	provenanceRunner
	servedProfileAdaptersRunner
	nativeAuthorizationRunner
	turnHandlerRunner
	DurableActiveTurnClearer
	classicTurnRunner
	identityActiveRunner
	restoredSourceRunner
	fallbackRefreshRunner
	channelPromptRunner
}
