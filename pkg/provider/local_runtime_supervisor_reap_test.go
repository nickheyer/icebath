package provider

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// Windows reaps through retained job handles and never walks the process table by binary
func TestReapOrphansKillsOnlyOurParentlessBinaries(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "llama-server.exe")
	lr5Touch(t, exe, "")
	prevPids, prevExe, prevPPID, prevExists, prevKill := lrReapPids, lrReapExe, lrReapPPID, lrReapPidExists, lrReapKill
	t.Cleanup(func() {
		lrReapPids, lrReapExe, lrReapPPID, lrReapPidExists, lrReapKill = prevPids, prevExe, prevPPID, prevExists, prevKill
	})
	table := map[int][2]any{300: {exe, 9999}, 301: {exe, 1}, 302: {filepath.Join(dir, "other.exe"), 9999}}
	lrReapPids = func() []int { return []int{300, 301, 302} }
	lrReapExe = func(pid int) (string, error) { return table[pid][0].(string), nil }
	lrReapPPID = func(pid int) (int, error) { return table[pid][1].(int), nil }
	lrReapPidExists = func(pid int) bool { return pid == 1 }
	var reaped []int
	lrReapKill = func(pid int) error { reaped = append(reaped, pid); return nil }
	sup := &LlamaServerSupervisor{Binary: exe}
	if err := sup.reapOrphanedChildren(); err != nil {
		t.Fatal(err)
	}
	expected := []int{300}
	if runtime.GOOS == "windows" {
		expected = nil
	}
	if !slices.Equal(reaped, expected) {
		t.Fatal(reaped)
	}
}
