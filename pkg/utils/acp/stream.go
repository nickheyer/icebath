package acputil

import (
	"encoding/json"
	"fmt"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Direction of a frame a stream observer sees
type StreamDirection string

// Directions of the frames a connection reads and writes
const (
	// Frame read from the peer
	StreamIncoming StreamDirection = "incoming"
	// Frame written to the peer
	StreamOutgoing StreamDirection = "outgoing"
)

// One frame a connection read or wrote
type StreamEvent struct {
	Direction StreamDirection
	// Method of a request or notification, empty for a reply
	Method string
	// JSON text of the frame without its line end, shared by every observer and not to be modified
	Frame json.RawMessage
}

// Frame decoded with objects as *jsonutil.Object, nil when it does not parse
func (e StreamEvent) Message() any {
	v, err := jsonutil.Decode(e.Frame)
	if err != nil {
		return nil
	}
	return v
}

// Callback receiving every frame of a connection on the goroutine that read or wrote it, before an incoming frame is handled
type StreamObserver func(event StreamEvent)

// Registers a callback that receives every frame read or written from now on
func (c *Connection) AddObserver(observer StreamObserver) {
	c.obsMu.Lock()
	defer c.obsMu.Unlock()
	c.observers = append(c.observers, observer)
}

func (c *Connection) notify(direction StreamDirection, method string, frame []byte) {
	c.obsMu.Lock()
	observers := c.observers
	c.obsMu.Unlock()
	if len(observers) == 0 {
		return
	}
	event := StreamEvent{Direction: direction, Method: method, Frame: frame}
	for _, observer := range observers {
		c.observe(observer, event)
	}
}

// Calls one observer, a panic logged instead of ending the connection
func (c *Connection) observe(observer StreamObserver, event StreamEvent) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error(fmt.Sprintf("acp stream observer failed\n%v", r))
		}
	}()
	observer(event)
}
