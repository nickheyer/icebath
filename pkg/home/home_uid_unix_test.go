//go:build unix

package home

import (
	"context"
	"io/fs"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResolveHermesUidGidReturnsParsedValuesWhenBothSet(t *testing.T) {
	t.Setenv("ICEBATH_UID", "1000")
	t.Setenv("ICEBATH_GID", "911")
	uid, gid := resolveIcebathUIDGID()
	if uid == nil || gid == nil || uid.Int64() != 1000 || gid.Int64() != 911 {
		t.Fatalf("uid %v gid %v", uid, gid)
	}
}

func TestChownToHermesUidEpermIsSilentlySwallowed(t *testing.T) {
	t.Setenv("ICEBATH_UID", "1000")
	t.Setenv("ICEBATH_GID", "911")
	d := mkdirs(t, filepath.Join(t.TempDir(), "subdir"), 0)
	calls := 0
	swap(t, &chownPath, func(p string, u, g int) error {
		calls++
		return &fs.PathError{Op: "chown", Path: p, Err: syscall.EPERM}
	})
	chownToIcebathUID(d)
	if calls != 1 {
		t.Fatalf("chown called %d times", calls)
	}
}

type chownCall struct {
	path     string
	uid, gid int
}

func TestSecureDirChown(t *testing.T) {
	record := func(t *testing.T) *[]chownCall {
		var calls []chownCall
		swap(t, &chownPath, func(p string, u, g int) error {
			calls = append(calls, chownCall{p, u, g})
			return nil
		})
		return &calls
	}
	t.Run("test_secure_dir_invokes_chown_when_env_set", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		t.Setenv("ICEBATH_UID", "1000")
		t.Setenv("ICEBATH_GID", "911")
		d := mkdirs(t, filepath.Join(tmp, "subdir"), 0)
		calls := record(t)
		ApplySecureDirPolicy(context.Background(), d, filepath.Join(tmp, "home"))
		if len(*calls) != 1 || (*calls)[0] != (chownCall{d, 1000, 911}) {
			t.Fatalf("chown calls %v", *calls)
		}
	})
	t.Run("test_secure_dir_no_chown_when_env_unset", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		d := mkdirs(t, filepath.Join(tmp, "subdir"), 0)
		calls := record(t)
		ApplySecureDirPolicy(context.Background(), d, filepath.Join(tmp, "home"))
		if len(*calls) != 0 {
			t.Fatalf("chown calls %v", *calls)
		}
	})
}
