package websocketutil

import (
	"strconv"

	"github.com/gorilla/websocket"
)

// Kind of a data message, numbered as its WebSocket opcode
type MessageType int

// Data message kinds
const (
	// UTF-8 text
	Text MessageType = websocket.TextMessage
	// Arbitrary bytes
	Binary MessageType = websocket.BinaryMessage
)

// "text" or "binary", "opcode N" for any other value
func (t MessageType) String() string {
	switch t {
	case Text:
		return "text"
	case Binary:
		return "binary"
	}
	return "opcode " + strconv.Itoa(int(t))
}

// One data message received from the peer
type Message struct {
	Type MessageType
	// Payload, valid UTF-8 for a Text message
	Data []byte
}
