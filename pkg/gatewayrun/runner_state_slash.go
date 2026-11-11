package gatewayrun

import (
	"os"
	"sync"
)

// Lazily set attributes of the kanban watcher mixin
type runnerSlashState struct {
	kanbanDispatcherLockMu     sync.Mutex
	kanbanDispatcherLockHandle *os.File
	kanbanSubFailCounts        map[kanbanSubKey]int
	telegramCapabilityHintTS   map[string]float64
	loginLock                  sync.Mutex
	loginAttempts              map[string]*signInAttempt
	loginExec                  *threadPoolExecutor
}
