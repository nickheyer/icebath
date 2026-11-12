package state

// Whether an end reason is one a session can be resurrected from
func IsRecoverableEndReason(reason string) bool {
	for _, recoverable := range recoverableEndReasons {
		if recoverable == reason {
			return true
		}
	}
	return false
}
