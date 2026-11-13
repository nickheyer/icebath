//go:build unix

package home

import (
	"context"
	"fmt"
	"io/fs"
)

// POSIX hosts keep no per-process grant state, since no token is ever elevated across a UAC boundary
type aclState struct{}

// Grant state of a POSIX process
func newACLState() *aclState { return &aclState{} }

// Reports false, since only a Windows token carries a UAC elevation
func (r *Resolver) Elevated() bool { return false }

// Creates the directory as a plain mkdir, POSIX ownership already belonging to the creating user
func (r *Resolver) makeUserDir(_ context.Context, target string, perm fs.FileMode, parents bool) error {
	return makeDir(target, perm, parents)
}

// Returns at once, an elevated update leaving Administrators-only entries being a Windows outcome alone
func (r *Resolver) RepairHomeACL(context.Context) error { return nil }

// One line repair for a directory this process may not use, a recursive chown back to the user
func UnusableDirRemedy(path string) string {
	return fmt.Sprintf(`run: sudo chown -R "$(id -un)" "%s"`, path)
}
