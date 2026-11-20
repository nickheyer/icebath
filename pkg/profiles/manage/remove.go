package manage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Wait a delete grants a multiplexer to release the gateway.lock of a profile it is tearing down
var OwnerReleaseWait = 60 * time.Second

// Poll interval of the owner wait, swapped by tests
var ownerPollInterval = 250 * time.Millisecond

// True once no gateway runtime holds the profile's gateway.lock, false when still held after timeout
func WaitProfileUnowned(profileDir string, timeout *time.Duration) (bool, error) {
	wait := OwnerReleaseWait
	if timeout != nil {
		wait = *timeout
	}
	deadline := time.Now().Add(wait)
	for {
		release, err := gatewayExclusiveMaintenance([]string{profileDir})
		if err == nil {
			release()
			return true, nil
		}
		var conflict *gatewayOwnershipConflict
		if !errors.As(err, &conflict) {
			unheld, lerr := lockUnheldReadOnly(filepath.Join(profileDir, "gateway.lock"))
			if lerr != nil || unheld {
				return unheld, lerr
			}
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(ownerPollInterval)
	}
}

// Error handler of an rmtree that adds owner write permission to the path and its parent and retries
func rmtreeMakeWritable(op pyfs.RmTreeOp, path string, err error) error {
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	for _, target := range []string{path, filepath.Dir(path)} {
		if target != "" {
			if st, serr := os.Stat(target); serr == nil {
				_ = os.Chmod(target, st.Mode().Perm()|0o200)
			}
		}
	}
	return op(path)
}

// Sleeps between rmtree attempts, swapped by tests
var rmtreeSleep = time.Sleep

// Tree removal each profile delete attempt runs, swapped by tests
var rmtreeProfile = pyfs.RmTree

// Removes a profile tree with three attempts, absorbing writes a just stopped process landed mid walk
func rmtreeWithRetry(profileDir string, onexc func(pyfs.RmTreeOp, string, error) error) error {
	const attempts = 3
	var last error
	for attempt := range attempts {
		err := rmtreeProfile(profileDir, onexc)
		if err == nil {
			return nil
		}
		last = err
		if !pathExists(profileDir) {
			return nil
		}
		if attempt < attempts-1 {
			rmtreeSleep(time.Duration(300*(attempt+1)) * time.Millisecond)
		}
	}
	return last
}
