package proc

import (
	"encoding/binary"
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

// Kernel p_stat value of a zombie process
const DarwinZombieState = 5

// Kernel record of pid, ErrNoSuchProcess when the pid is gone
func kinfo(pid int) (*unix.KinfoProc, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return nil, ErrNoSuchProcess
		}
		return nil, err
	}
	if kp.Proc.P_pid == 0 && pid != 0 {
		return nil, ErrNoSuchProcess
	}
	return kp, nil
}

// Creation epoch of pid like psutil create_time
func Birth(pid int) (float64, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return float64(kp.Proc.P_starttime.Sec) + float64(kp.Proc.P_starttime.Usec)/1e6, nil
}

// Start stamp worker records compare against, the creation epoch on macOS
func WorkerBirth(pid int) (float64, error) { return Birth(pid) }

// Boot relative start, which macOS does not expose
func BootStart(pid int) (float64, bool) { return 0, false }

// Status letter of pid mapped from the kinfo process state like psutil
func State(pid int) (byte, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	switch kp.Proc.P_stat {
	case 1:
		return 'I', nil
	case 2:
		return 'R', nil
	case 4:
		return 'T', nil
	case DarwinZombieState:
		return 'Z', nil
	}
	return 'S', nil
}

// Whether pid exited and waits to be reaped
func Zombie(pid int) bool {
	kp, err := kinfo(pid)
	return err == nil && kp.Proc.P_stat == DarwinZombieState
}

// Parent pid of pid, false when unreadable
func PPID(pid int) (int, bool) {
	kp, err := kinfo(pid)
	if err != nil {
		return 0, false
	}
	return int(kp.Eproc.Ppid), true
}

// Argv and environment block of pid from the kernel's argument area
func procArgs(pid int) ([]string, string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ESRCH) {
			return nil, "", ErrNoSuchProcess
		}
		return nil, "", err
	}
	if len(raw) < 4 {
		return nil, "", errors.New("short procargs2 record")
	}
	argc := int(binary.NativeEndian.Uint32(raw[:4]))
	rest := raw[4:]
	if i := strings.IndexByte(string(rest), 0); i >= 0 {
		rest = rest[i:]
	}
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	parts := strings.Split(string(rest), "\x00")
	argv := []string{}
	var env strings.Builder
	for i, part := range parts {
		if i < argc {
			argv = append(argv, part)
			continue
		}
		if part == "" {
			break
		}
		env.WriteString(part)
		env.WriteByte(0)
	}
	return argv, env.String(), nil
}

// Environment of pid parsed from its argument area
func Environ(pid int) (map[string]string, error) {
	_, block, err := procArgs(pid)
	if err != nil {
		return nil, err
	}
	return parseEnvironBlock(block), nil
}

// Argument vector of pid from its argument area
func Cmdline(pid int) ([]string, error) {
	argv, _, err := procArgs(pid)
	return argv, err
}

// Every pid in the kernel process table like psutil pids
func listPIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(procs))
	for _, kp := range procs {
		pids = append(pids, int(kp.Proc.P_pid))
	}
	return pids, nil
}
