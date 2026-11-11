package tools

// Strips tier one secrets always and tier two provider credentials unless inherited, in place, returning the env
func ScrubCredentials(child map[string]string, inheritCredentials bool) map[string]string {
	scrubCredentials(nil, child, inheritCredentials)
	return child
}

// Copy of the current process environment, the first entry of a repeated name winning
func EnvironMap() map[string]string { return environMap() }

// Delivery attempts after which a pending async delegation result is dropped
const AsyncMaxDeliveryAttempts = asyncMaxDeliveryAttempts
