package tools

// Routing verdict for one drained event under the owner predicate, false meaning requeue
func OwnsEvent(evt map[string]any, sessionKey string, owns func(map[string]any) bool, isAsyncDelegation bool) bool {
	return ownsEvent(evt, sessionKey, owns, isAsyncDelegation)
}
