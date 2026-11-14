package pm

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestPMRepairReportsUnwritableRuntimeWithoutTraceback(t *testing.T) {
	tmp := isolateHome(t)
	t.Setenv(home.EnvHome, filepath.Join(tmp, "home"))
	ctx := context.Background()
	root := RepoRoot()
	lease := filepath.Join(InstallStateDir(ctx, root), "environments", "generation", ".leases", "reader")
	message := InstallStatePermissionMessage(ctx, root, &fs.PathError{Op: "open", Path: lease, Err: fs.ErrPermission})
	if !strings.Contains(message, "install state is not writable by this user") || !strings.Contains(message, resolvePath(lease)) {
		t.Fatalf("message %q", message)
	}
	outside := filepath.Join(tmp, "other", ".prepare.lock")
	if got := InstallStatePermissionMessage(ctx, root, &fs.PathError{Op: "open", Path: outside, Err: fs.ErrPermission}); got != "" {
		t.Fatalf("outside failure described as install state %q", got)
	}
}
