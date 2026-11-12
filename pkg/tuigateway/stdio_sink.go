package tuigateway

import "sync"

// Guards the module stdio sink, which the stdio entry replaces with a tee when a sidecar mirrors its frames
var stdioSinkMu sync.RWMutex

// Module stdio sink a request falls back to when no transport is bound
func stdioTransport() Transport {
	stdioSinkMu.RLock()
	defer stdioSinkMu.RUnlock()
	return stdioSink
}

// Replaces the module stdio sink, the rebinding of server._stdio_transport
func SetModuleStdioTransport(t Transport) {
	stdioSinkMu.Lock()
	defer stdioSinkMu.Unlock()
	stdioSink = t
}
