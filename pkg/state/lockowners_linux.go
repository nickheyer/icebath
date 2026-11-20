package state

import "golang.org/x/sys/unix"

// Holder lines from /proc/locks for the database, its WAL and its shared memory file
func writeLockHolders(dbPath string) ([]string, error) {
	base, err := pyRealpath(dbPath)
	if err != nil {
		return nil, err
	}
	inodes := map[DevIno]string{}
	for _, sidecar := range []string{"", "-wal", "-shm"} {
		var st unix.Stat_t
		if unix.Stat(base+sidecar, &st) != nil {
			continue
		}
		inodes[DevIno{Dev: st.Dev, Ino: st.Ino}] = sidecar
	}
	held, ok, err := procLocksHeld(inodes)
	if err != nil || !ok {
		return nil, err
	}
	lines := make([]string, 0, len(held))
	for _, h := range held {
		lines = append(lines, describeLockOwner(h.PID)+" holds "+h.Kind+" lock on "+pathlibName(base+h.Sidecar))
	}
	return lines, nil
}
