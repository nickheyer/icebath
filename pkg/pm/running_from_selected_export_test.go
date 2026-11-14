package pm

import (
	"context"
	"testing"
)

// The exported form answers exactly as the package function for the same root
func TestRunningFromSelectedEnvironmentExportedMatchesPackageFunction(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gotRunning, gotPrefix, gotErr := RunningFromSelectedEnvironment(ctx, root)
	wantRunning, wantPrefix, wantErr := runningFromSelectedEnvironment(ctx, root)
	if gotRunning != wantRunning || gotPrefix != wantPrefix || (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("exported (%v, %q, %v) differs from package (%v, %q, %v)", gotRunning, gotPrefix, gotErr, wantRunning, wantPrefix, wantErr)
	}
	if gotRunning {
		t.Fatalf("a fresh temp root has no selected environment, got running true with prefix %q", gotPrefix)
	}
}
