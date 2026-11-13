package state

import (
	"context"
	"fmt"
)

// Windows never unlinks a WAL another handle holds, so a lost generation leaves no inode of this process to copy
func (e *Engine) captureRetiredGeneration(_ context.Context, path string, sidecars map[string]fileIdentity, _ string) (string, error) {
	walID, ok := sidecars[walSuffix]
	if !ok {
		return "", &CaptureError{msg: fmt.Sprintf("no recorded WAL generation identity for %s; refusing to guess it by pathname", path)}
	}
	return "", &CaptureError{msg: fmt.Sprintf("this process no longer holds the retired WAL inode %v of %s", identityList(walID), path)}
}
