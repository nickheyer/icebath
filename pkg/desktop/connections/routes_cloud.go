package connections

import (
	"sync"
)

// ManagedPrimaryRestore is a managed SSH primary restore in flight for one registry connection
type ManagedPrimaryRestore struct {
	CorrelationID string
	Profile       string
	Source        RegistryConnection
}

// Liveness trackers and the managed primary restores the revalidation routes read
type cloudSlice struct {
	remoteLiveness         *RemoteLivenessTracker
	pooledRemoteLiveness   *RemoteLivenessTracker
	mu                     sync.Mutex
	managedPrimaryRestores map[string]ManagedPrimaryRestore
}

func mustLivenessTracker(failureWindowMs int64) *RemoteLivenessTracker {
	tracker, err := NewRemoteLivenessTracker(RemoteLivenessFailureLimit, failureWindowMs, nowMillis)
	if err != nil {
		panic(err)
	}
	return tracker
}

// Primary tracker on the sub minute retry window, pooled tracker on the wider reconnect cadence window
func newCloudSlice() *cloudSlice {
	return &cloudSlice{
		remoteLiveness:         mustLivenessTracker(RemoteLivenessFailureWindowMs),
		pooledRemoteLiveness:   mustLivenessTracker(RemotePooledLivenessFailureWindowMs),
		managedPrimaryRestores: map[string]ManagedPrimaryRestore{},
	}
}

func (s *Service) managedPrimaryRestoreActive() bool {
	s.cloud.mu.Lock()
	defer s.cloud.mu.Unlock()
	return len(s.cloud.managedPrimaryRestores) > 0
}

// PrimaryIsRemote reports a managed SSH primary restore in flight or a primary backend resolving to a remote host
func (s *Service) PrimaryIsRemote() bool {
	return s.managedPrimaryRestoreActive() || s.primaryBackendIsRemote()
}

// ClearRemoteLiveness forgets the failure streaks counted against the primary
func (s *Service) ClearRemoteLiveness() {
	s.cloud.remoteLiveness.Clear()
}
