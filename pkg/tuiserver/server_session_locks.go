package tuiserver

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Reentrant claim lock of the registry check then act sections, the server _sessions_lock
var sessionsLock pythread.RLock

// Serializes a session.resume reattach against disconnect and reap claims, always taken before sessionsLock
var sessionResumeLock sync.Mutex
