package pm

import (
	"bytes"
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCLIPMRepairReportsUnwritableRuntimeWithoutTraceback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	ctx := context.Background()
	root := RepoRoot()
	target := filepath.Join(InstallStateDir(ctx, root), "pm-runtime", ".prepare.lock")
	var stderr bytes.Buffer
	code, err := Main(WithCLIStderr(ctx, &stderr), func(context.Context) (int, error) {
		return 0, &fs.PathError{Op: "open", Path: target, Err: syscall.EACCES}
	})
	if err != nil || code != 1 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.Contains(stderr.String(), "install state is not writable by this user") || !strings.Contains(stderr.String(), target) {
		t.Fatalf("stderr %q", stderr.String())
	}
	lease := filepath.Join(InstallStateDir(ctx, root), "environments", "generation", ".leases", "reader")
	if InstallStatePermissionMessage(ctx, root, &fs.PathError{Op: "open", Path: lease, Err: syscall.EACCES}) == "" {
		t.Fatal("lease inside the install state gave no message")
	}
	outside := filepath.Join(tmp, "other", ".prepare.lock")
	if message := InstallStatePermissionMessage(ctx, root, &fs.PathError{Op: "open", Path: outside, Err: syscall.EACCES}); message != "" {
		t.Fatalf("outside path gave %q", message)
	}
}

func TestCLIMainMapsSystemExitToItsStatus(t *testing.T) {
	var stderr bytes.Buffer
	code, err := Main(WithCLIStderr(context.Background(), &stderr), func(context.Context) (int, error) {
		return 0, &SystemExit{Code: 7}
	})
	if err != nil || code != 7 || stderr.Len() != 0 {
		t.Fatalf("code %d err %v stderr %q", code, err, stderr.String())
	}
}
