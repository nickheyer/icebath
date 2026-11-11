package gateway

// Ticket of the given purpose the endpoint's owner mints for this home, the _session_ticket a worker dials with
func SessionTicket(home string, endpoint *GatewayEndpoint, purpose string) (string, error) {
	return sessionTicket(home, endpoint, purpose, "", "", false)
}
