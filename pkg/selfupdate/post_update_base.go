package selfupdate

import (
	"fmt"
	"os"
	"time"
)

// A free backup name beside the file, bak-<stamp> then bak-<stamp>.<n>
func backupPath(path, stamp string) (string, error) {
	base := path + ".bak-" + stamp
	if !pathPresent(base) {
		return base, nil
	}
	for index := 1; index < 1000; index++ {
		candidate := fmt.Sprintf("%s.bak-%s.%d", path, stamp, index)
		if !pathPresent(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not choose a backup path for %s", path)
}

func pathPresent(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Copies a file with its mode and modification time like shutil.copy2
func copyFile2(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// Original to backup pairs of the regular files among paths, copied with a shared timestamp
type backupPair struct{ original, backup string }

func backupExisting(paths []string) ([]backupPair, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	var backups []backupPair
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		dest, err := backupPath(path, stamp)
		if err != nil {
			return backups, err
		}
		if err := copyFile2(path, dest); err != nil {
			return backups, err
		}
		backups = append(backups, backupPair{path, dest})
	}
	return backups, nil
}

// Backup copies of the regular files among paths for callers outside the package, in path order
func BackupExisting(paths []string) ([]string, error) {
	pairs, err := backupExisting(paths)
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.backup)
	}
	return out, err
}
