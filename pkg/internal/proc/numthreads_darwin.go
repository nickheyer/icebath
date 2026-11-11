package proc

import "encoding/binary"

const taskInfoThreadNumOffset = 84

// Thread count of pid from the task info proc_pidinfo reports like psutil num_threads
func NumThreads(pid int) (int, error) {
	buf := make([]byte, taskInfoSize)
	n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDTaskInfo, 0, buf)
	if n <= 0 {
		return 0, raiseForPID(pid, "proc_pidinfo(PROC_PIDTASKINFO)", errno)
	}
	return int(int32(binary.LittleEndian.Uint32(buf[taskInfoThreadNumOffset:]))), nil
}
