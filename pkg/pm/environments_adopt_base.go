package pm

import (
	"sync"
)

var restartPending struct {
	sync.Mutex
	reason string
}

// Reason a restart is pending, empty when none was noted
func RestartPending() string {
	restartPending.Lock()
	defer restartPending.Unlock()
	return restartPending.reason
}
