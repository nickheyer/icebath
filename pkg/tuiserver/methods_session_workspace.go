package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Live sessions of this process in registry order
func snapshotSessions() []tuihost.LiveSession {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	return tuihost.LiveSessions()
}
