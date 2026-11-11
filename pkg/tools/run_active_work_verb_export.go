package tools

// Whether a delegation status is one of the live states a stop of the gateway interrupts
func AsyncDelegationLive(status any) bool {
	s, ok := status.(string)
	return ok && asyncLiveStates[s]
}
