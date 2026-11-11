package tuigateway

// Login a session was created under, its stamped slot or else the login of its transport
func SessionAuthUserID(session *Session) *string { return sessionAuthUserID(session) }

// Owner subject of a signed in transport, nil for anonymous or stdio transports
func TransportAuthUserID(transport Transport) *string { return transportAuthUserID(transport) }
