package botmode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Folder name of the per user DM payload directory under the temp root
const dmDirName = "icebath-dm"

// Hours an orphaned DM payload file is kept, the stale window of a DM delivery
const DMStaleHours = 24.0

// Per user DM payload directory under the temp root, created private and refused when another user holds it
func dmDir() (string, error) {
	root, err := pathutil.TempDir()
	if err != nil {
		return "", err
	}
	uid, hasUID := currentUID()
	name := dmDirName
	if hasUID {
		name = fmt.Sprintf("%s-%d", dmDirName, uid)
	}
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", &fs.PathError{Op: "DM temp path is not a directory", Path: path, Err: fs.ErrPermission}
	}
	if owner, ok := ownerUID(info); hasUID && ok && owner != uid {
		return "", &fs.PathError{Op: "DM temp directory is owned by another user", Path: path, Err: fs.ErrPermission}
	}
	if info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return "", err
		}
	}
	return path, nil
}

// Per user DM payload directory a message file is written into
func DMDir() (string, error) { return dmDir() }

// Deletes orphaned DM payload files and live delivery intents older than maxAgeHours, returning the count
func CleanupBotDMCache(maxAgeHours float64, now *float64) int {
	root, err := pathutil.TempDir()
	if err != nil {
		return 0
	}
	locations := [][2]string{{root, "icebath-dm-*.txt"}, {root, "icebath-relay-dm-*.txt"}}
	if dir, err := dmDir(); err == nil {
		locations = append(locations, [2]string{dir, "*.txt"}, [2]string{dir, "*.live.json"})
	}
	cutoff := time.Now()
	if now != nil {
		cutoff = timeutil.FromEpoch(*now)
	}
	cutoff = cutoff.Add(-time.Duration(maxAgeHours * float64(time.Hour)))
	total := 0
	for _, loc := range locations {
		total += fsutil.RemoveOlderThan(loc[0], loc[1], cutoff)
	}
	return total
}
