package pm

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// GCGraceSeconds is how long an untouched partial survives collection
const GCGraceSeconds = 6 * 60 * 60

// Takes the cross process ownership lock of one partial key, the release closing it
func PartialLock(root, key string, cancelled func() bool, wait bool) (release func(), acquired bool, err error) {
	locks := filepath.Join(root, ".locks")
	if err := os.MkdirAll(locks, 0o777); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(locks, key), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	release = func() { f.Close() }
	for {
		held, err := LockFD(f, false, 0)
		if err != nil {
			f.Close()
			return nil, false, err
		}
		if held {
			return release, true, nil
		}
		if !wait || (cancelled != nil && cancelled()) {
			return release, false, nil
		}
		time.Sleep(lockPollInterval)
	}
}

// Removes unowned partial pairs older than the grace period, skipping any a downloader owns
func CollectPartials(root string, graceSeconds float64) error {
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if ext := pypath.PathSuffix(name); ext == ".part" || ext == ".ranges" {
			keys[strings.TrimSuffix(name, ext)] = true
		}
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	now := time.Now()
	for _, key := range names {
		release, acquired, err := PartialLock(root, key, nil, false)
		if err != nil {
			return err
		}
		if acquired {
			collectPartialPair(root, key, now, graceSeconds)
		}
		release()
	}
	return nil
}

// Deletes one owned partial pair unless either file is younger than the grace period
func collectPartialPair(root, key string, now time.Time, graceSeconds float64) {
	pair := []string{filepath.Join(root, key+".part"), filepath.Join(root, key+".ranges")}
	for _, path := range pair {
		st, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return
		}
		if graceSeconds > 0 && now.Sub(st.ModTime()).Seconds() < graceSeconds {
			return
		}
	}
	for _, path := range pair {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return
		}
	}
}
