package state

import (
	"path/filepath"
)

// Every canonical spelling of the WAL and SHM sidecars mapped to a literal path that can be stat'd for identity
func watchedSidecars(path string) map[string]string {
	literal := absPath(path)
	watched := map[string]string{}
	for _, suffix := range []string{walSuffix, shmSuffix} {
		watched[canonicalSQLitePath(literal+suffix)] = literal + suffix
	}
	for _, base := range []string{filepath.Join(resolvePath(filepath.Dir(literal)), filepath.Base(literal)), resolvePath(literal)} {
		for _, suffix := range []string{walSuffix, shmSuffix} {
			if _, seen := watched[canonicalSQLitePath(base+suffix)]; !seen {
				watched[canonicalSQLitePath(base+suffix)] = base + suffix
			}
		}
	}
	return watched
}

// Reports id naming a generation the watched path no longer holds, an unreadable path counting as gone
func identityUnlinked(id fileIdentity, watchedPath string) bool {
	now, err := identityOf(watchedPath)
	return err != nil || now != id
}
