package botmode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Folder name of the per user DM payload directory under the temp root
const dmDirName = "icebath-dm"

// Hours an orphaned DM payload file is kept, the stale window of a DM delivery
const DMStaleHours = 24.0

// Per user DM payload directory under the temp root, created private and refused when another user holds it
func dmDir() (string, error) {
	root, err := pypath.Gettempdir()
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
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(path, 0o700); err != nil {
			return "", err
		}
	}
	return path, nil
}

// Deletes orphaned DM payload files and live delivery intents older than maxAgeHours, returning the count
func CleanupBotDMCache(maxAgeHours float64, now *float64) int {
	root, err := pypath.Gettempdir()
	if err != nil {
		return 0
	}
	locations := [][2]string{{root, "icebath-dm-*.txt"}, {root, "icebath-relay-dm-*.txt"}}
	if dir, err := dmDir(); err == nil {
		locations = append(locations, [2]string{dir, "*.txt"}, [2]string{dir, "*.live.json"})
	}
	total := 0
	for _, loc := range locations {
		total += pyfs.UnlinkFilesOlderThan(loc[0], loc[1], maxAgeHours*3600, now)
	}
	return total
}

// Hourly sweep of stale relay artifacts under the install root of the process home
func CleanupBotRelayArtifacts() int {
	base := RelayRoot(InstallRoot(defaultHome()))
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		return 0
	}
	return sweepStaleRelay(base)
}
