package tuigateway

// Socket under the transport, what a viewer the peer opens records its admission against
func (t *WSTransport) Conn() WSConn { return t.ws }
