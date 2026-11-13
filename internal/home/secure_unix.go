//go:build unix

package home

import "os"

// Largest id a POSIX chown accepts, one below the all-ones value that means leave unchanged
const maxOwnerID = 1<<32 - 2

// Id an ICEBATH_UID or ICEBATH_GID variable names, -1 when unset or not an id
func configuredID(name string) int {
	n, ok := parseInt(envValue(name), 10)
	if !ok || n < 0 || n > maxOwnerID {
		return -1
	}
	return int(n)
}

// Chowns path to ICEBATH_UID and ICEBATH_GID when either is set, a refused chown left for the entrypoint to fix
func chownToConfigured(path string) {
	uid, gid := configuredID("ICEBATH_UID"), configuredID("ICEBATH_GID")
	if uid < 0 && gid < 0 {
		return
	}
	_ = os.Chown(path, uid, gid)
}
