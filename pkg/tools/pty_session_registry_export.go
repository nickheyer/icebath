package tools

import (
	"context"
	"time"
)

// Removes and returns the session under key, nil when none is registered
func (r *PtySessionRegistry) Pop(key string) *PtySession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.popLocked(key)
}

// Closes a removed session on a goroutine shutdown waits for, the channel closed once it is closed
func (r *PtySessionRegistry) CloseInBackground(s *PtySession) <-chan struct{} {
	return r.closeInBackground(s)
}

// Whether s ended, lost its process, or outlived the retention TTL without a viewer
func (r *PtySessionRegistry) Reapable(s *PtySession, now time.Time) bool { return r.reapable(s, now) }

// Retention TTL of a detached session
func (r *PtySessionRegistry) TTL() time.Duration { return r.o.TTL }

// Whether CloseAll ran and no Reopen followed
func (r *PtySessionRegistry) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Viewer the session currently streams to, nil while detached
func (s *PtySession) Viewer() PtyViewer { return s.currentViewer() }

// Sends data to a viewer bounded by the viewer send timeout, a panic or timeout returned as the error
func SendToPtyViewer(ctx context.Context, ws PtyViewer, data []byte) error {
	return viewerCall(ctx, ptyViewerSendTimeout, func(c context.Context) error { return ws.SendBytes(c, data) })
}

// Closes a viewer with code and reason within five seconds, every failure ignored
func ClosePtyViewer(ws PtyViewer, code int, reason string) { closePtyViewer(ws, code, reason) }
