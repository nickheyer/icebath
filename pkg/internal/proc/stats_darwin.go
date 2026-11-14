package proc

import "encoding/binary"

// Descriptor counts are read through proc_info
const NumFDsAvailable = true

// Virtual memory size of pid in bytes from the task info proc_pidinfo reports
func VMS(pid int) (int64, error) {
	buf := make([]byte, taskInfoSize)
	n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDTaskInfo, 0, buf)
	if n <= 0 {
		return 0, raiseForPID(pid, "proc_pidinfo(PROC_PIDTASKINFO)", errno)
	}
	return int64(binary.LittleEndian.Uint64(buf)), nil
}

// Count of descriptors pid holds open, the records of its descriptor table
func NumFDs(pid int) (int, error) {
	fds, err := listFDs(pid)
	if err != nil {
		return 0, err
	}
	return len(fds) / procFDInfoSize, nil
}

// psutil status name of pid from its kinfo state
func StatusName(pid int) (string, error) {
	letter, err := State(pid)
	if err != nil {
		return "", err
	}
	switch letter {
	case 'I':
		return "idle", nil
	case 'R':
		return "running", nil
	case 'T':
		return "stopped", nil
	case 'Z':
		return "zombie", nil
	}
	return "sleeping", nil
}
