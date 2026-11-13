package state

// Device and inode of a file, or volume serial and file index on Windows
type fileIdentity struct {
	dev uint64
	ino uint64
}

// Identity of path following symlinks, false when missing or when the filesystem reports a zero device or inode
func statIdentity(path string) (fileIdentity, bool) {
	id, err := identityOf(path)
	if err != nil || id.dev == 0 || id.ino == 0 {
		return fileIdentity{}, false
	}
	return id, true
}

// Identities of the WAL and SHM sidecars that exist beside path, keyed by suffix
func sidecarIdentities(path string) map[string]fileIdentity {
	out := map[string]fileIdentity{}
	for _, suffix := range []string{walSuffix, shmSuffix} {
		if id, ok := statIdentity(path + suffix); ok {
			out[suffix] = id
		}
	}
	return out
}
