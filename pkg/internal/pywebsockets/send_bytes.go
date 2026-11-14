package pywebsockets

import "github.com/gorilla/websocket"

// Sends one binary frame, ConnectionClosed when the connection already ended or the write fails
func (c *Conn) SendBytes(message []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return c.closedError()
	default:
	}
	if err := c.ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
		c.markDone()
		_ = c.ws.Close()
		return c.closedError()
	}
	return nil
}
