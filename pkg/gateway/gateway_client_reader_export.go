package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Whether the reader finished, every pending answer already failed and no further frame arriving
func (c *GatewayClient) ReaderDone() bool {
	select {
	case <-c.readDone:
		return true
	default:
		return false
	}
}

// Callbacks of a reader that reads on past frames it cannot decode, the terminal UI link
type FrameHooks struct {
	Inbound   func(raw string, frame *pyjson.Object)
	Malformed func(raw string)
}

// Installs the frame callbacks, called before Start so the reader sees them from its first frame
func (c *GatewayClient) SetFrameHooks(h FrameHooks) {
	c.mu.Lock()
	c.hooks = h
	c.mu.Unlock()
}

// Error the socket read failed with once the reader finished, nil while it reads
func (c *GatewayClient) ReadError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}
