package remote

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
)

var (
	backendMu sync.Mutex
	backend   *remoteBackend
)

// Process wide remote backend with one poller and one in memory layer per profile home
func GetRemoteBackend() config.ConfigBackend {
	backendMu.Lock()
	defer backendMu.Unlock()
	if backend == nil {
		backend = newRemoteBackend().(*remoteBackend)
	}
	return backend
}

// Stops the poller of the process wide backend and drops it so the next read boots again
func ResetForTests() {
	backendMu.Lock()
	defer backendMu.Unlock()
	if backend != nil {
		backend.stopOnce.Do(func() { close(backend.stop) })
	}
	backend = nil
}

func init() {
	config.RegisterConfigBackend("remote", GetRemoteBackend)
}
