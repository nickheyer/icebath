package gateway

// Session ticket the owner control socket mints for endpoint, the desktop native grant client
func MintSessionTicket(home string, endpoint *GatewayEndpoint, purpose, scope string) (string, error) {
	return sessionTicket(home, endpoint, purpose, scope, "", false)
}
