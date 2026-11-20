//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Fresh update home and the receipt scope of one update command
func ucReceiptHome(t *testing.T) (context.Context, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return updaterecord.UpdateReceiptScope(context.Background()), h
}

func TestExitsBeforeTheReceiptOpensLeaveTheHoldersReceiptAlone(t *testing.T) {
	ctx, h := ucReceiptHome(t)
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	latest := filepath.Join(h, "logs", "update_receipts", "latest.json")
	holders, err := os.ReadFile(latest)
	if err != nil {
		t.Fatal(err)
	}
	updaterecord.ClearCurrentReceipt(ctx)
	savedRetarget, savedPreflight, savedHangup, savedFinalize := updateRetarget, updatePreflight, updateInstallHangup, updateFinalizeOutput
	savedLock, savedDescribe, savedOperation := updateAcquireLock, updateDescribeHolder, updateGitOperation
	t.Cleanup(func() {
		updateRetarget, updatePreflight, updateInstallHangup, updateFinalizeOutput = savedRetarget, savedPreflight, savedHangup, savedFinalize
		updateAcquireLock, updateDescribeHolder, updateGitOperation = savedLock, savedDescribe, savedOperation
	})
	updateRetarget = func(context.Context, string) error { return nil }
	updatePreflight = func(context.Context, io.Writer, map[string]any) (bool, error) { return false, nil }
	updateInstallHangup = func(io.Writer, io.Writer, bool) *gateway.UpdateOutputState { return nil }
	updateFinalizeOutput = func(*gateway.UpdateOutputState) {}
	updateAcquireLock = func(context.Context, string) (bool, *updaterecord.UpdateHolder, func(), error) {
		return false, nil, func() {}, nil
	}
	updateDescribeHolder = func(*updaterecord.UpdateHolder) string { return "another update is running" }
	var out bytes.Buffer
	refused := cmdUpdate(ctx, UpdateInvocation{Namespace: map[string]any{"gateway": false}, Stdout: &out, Stderr: &out})
	updateGitOperation = func(string) (string, error) { return "rebase", nil }
	inProgress := cmdUpdateImpl(ctx, &out, map[string]any{}, false)
	var refusedExit, inProgressExit *pm.SystemExit
	if !errors.As(refused, &refusedExit) || !errors.As(inProgress, &inProgressExit) {
		t.Fatalf("exits = %v, %v", refused, inProgress)
	}
	if refusedExit.Code != 2 || inProgressExit.Code != 1 {
		t.Fatalf("exit codes = %d, %d", refusedExit.Code, inProgressExit.Code)
	}
	if after, err := os.ReadFile(latest); err != nil || !bytes.Equal(after, holders) {
		t.Fatalf("the holder's latest.json changed: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("another update is running")) {
		t.Fatalf("output = %q", out.String())
	}
}
