package proc

import (
	"bytes"
	"unsafe"
)

// Darwin reads every process cwd through proc_info
const CwdAvailable = true

const (
	procPIDVnodePathInfo = 9
	vnodePathInfoSize    = 2352
	cdirPathOffset       = 152
)

// Working directory of pid from the current directory vnode path proc_pidinfo reports
func Cwd(pid int) (string, error) {
	buf := make([]byte, vnodePathInfoSize)
	n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDVnodePathInfo, 0, buf)
	if n <= 0 {
		return "", raiseForPID(pid, "proc_pidinfo(PROC_PIDVNODEPATHINFO)", errno)
	}
	path, _, _ := bytes.Cut(buf[cdirPathOffset:], []byte{0})
	return string(path), nil
}

// Real uid of pid from its kinfo_proc credentials
func RealUID(pid int) (int, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return int(kp.Eproc.Pcred.P_ruid), nil
}

// Kernel name of pid from its kinfo_proc command field
func comm(pid int) (string, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return "", err
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&kp.Proc.P_comm[0])), len(kp.Proc.P_comm))
	name, _, _ := bytes.Cut(raw, []byte{0})
	return string(name), nil
}
