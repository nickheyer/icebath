package proc

import "encoding/binary"

const (
	procPIDTaskInfo        = 4
	taskInfoSize           = 96
	taskInfoResidentOffset = 8
)

// Resident set size of pid in bytes from the task info proc_pidinfo reports
func RSS(pid int) (int64, error) {
	buf := make([]byte, taskInfoSize)
	n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDTaskInfo, 0, buf)
	if n <= 0 {
		return 0, raiseForPID(pid, "proc_pidinfo(PROC_PIDTASKINFO)", errno)
	}
	return int64(binary.LittleEndian.Uint64(buf[taskInfoResidentOffset:])), nil
}
