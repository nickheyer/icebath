//go:build windows

package gateway

import "os"

// Lstat the control server cleanup compares file identities with
var controlLstat = os.Lstat

// Starts the platform listener, the named pipe on Windows
func (s *GatewayControlServer) startPlatform() error { return s.startWindows() }

// Starts the Windows named pipe listener with this server as its line handler
func (s *GatewayControlServer) startWindows() error {
	pipe := NewNativeControlServer(s.home, s.HandleRequestLine)
	s.mu.Lock()
	s.pipeServer = pipe
	s.mu.Unlock()
	return pipe.Start()
}
