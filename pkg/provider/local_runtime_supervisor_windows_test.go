package provider

import (
	"path/filepath"
	"testing"
)

func lr6Job(t *testing.T) *WindowsJob {
	t.Helper()
	job, err := newWindowsJob()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = job.Close() })
	return job
}

func lr6JobOpen(job *WindowsJob) bool {
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.open
}

func TestLocalRuntimeSupervisorReapsOwnedJobEvenAfterRouterExit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	sup, err := NewLlamaServerSupervisor(filepath.Join(dir, "llama-server.exe"), dir, 4, 59998, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	first := lr6Job(t)
	sup.job = first
	if err := sup.reapOrphanedChildren(); err != nil {
		t.Fatal(err)
	}
	if lr6JobOpen(first) || sup.job != nil {
		t.Fatal("crashed router's model children escaped cleanup")
	}
	second := lr6Job(t)
	sup.job = second
	if err := sup.Stop(); err != nil {
		t.Fatal(err)
	}
	if lr6JobOpen(second) || sup.job != nil {
		t.Fatal("stop left the job open")
	}
}
