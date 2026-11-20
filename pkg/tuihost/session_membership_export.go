package tuihost

// Removes a transport from a session's peers and viewers, true when another live client keeps the session from parking
func DetachSessionTransport(session *Session, transport Transport) bool {
	return detachSessionTransport(session, transport)
}
