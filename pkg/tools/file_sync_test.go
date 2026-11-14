package tools

import (
	"testing"
)

func TestEnvironmentSyncManagerReadsEachRemoteBackend(t *testing.T) {
	if EnvironmentSyncManager(nil) != nil {
		t.Fatal("nil environment has a sync manager")
	}
	sm := &FileSyncManager{}
	if EnvironmentSyncManager(&sshEnv{syncManager: sm}) != sm || EnvironmentSyncManager(&modalEnv{syncManager: sm}) != sm ||
		EnvironmentSyncManager(&daytonaEnv{syncManager: sm}) != sm || EnvironmentSyncManager(&vercelEnv{syncManager: sm}) != sm {
		t.Fatal("remote backend sync manager not returned")
	}
}
