package dashboard

// Browser credential generation a live gateway socket was admitted under, false for any other connection
func SocketAdmission(conn any) (string, bool) {
	var ws *WebSocket
	switch socket := conn.(type) {
	case *servedSocket:
		if socket != nil {
			ws = socket.ws
		}
	case *WebSocket:
		ws = socket
	}
	if ws == nil {
		return "", false
	}
	return admissionOf(ws)
}
