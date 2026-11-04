//go:build darwin

package tools

import (
	"encoding/binary"
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

// macOS process facts read through sysctl, the unadjusted kernel start every process sees

const darwinZombieState = 5

// Kernel record of pid, errNoSuchProcess when the pid is gone
func darwinKinfo(pid int) (*unix.KinfoProc, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return nil, errNoSuchProcess
		}
		return nil, err
	}
	if kp.Proc.P_pid == 0 && pid != 0 {
		return nil, errNoSuchProcess
	}
	return kp, nil
}

func (osProcessProber) Birth(pid int) (float64, error) {
	kp, err := darwinKinfo(pid)
	if err != nil {
		return 0, err
	}
	return float64(kp.Proc.P_starttime.Sec) + float64(kp.Proc.P_starttime.Usec)/1e6, nil
}

func (p osProcessProber) WorkerBirth(pid int) (float64, error) { return p.Birth(pid) }

func (osProcessProber) BootStart(pid int) (float64, bool) { return 0, false }

func (osProcessProber) Zombie(pid int) bool {
	kp, err := darwinKinfo(pid)
	return err == nil && kp.Proc.P_stat == darwinZombieState
}

func (osProcessProber) PPID(pid int) (int, bool) {
	kp, err := darwinKinfo(pid)
	if err != nil {
		return 0, false
	}
	return int(kp.Eproc.Ppid), true
}

// Argv and environment of pid from the kernel's argument area
func darwinProcArgs(pid int) ([]string, map[string]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ESRCH) {
			return nil, nil, errNoSuchProcess
		}
		return nil, nil, err
	}
	if len(raw) < 4 {
		return nil, nil, errors.New("short procargs2 record")
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
	env := map[string]string{}
	for i, part := range parts {
		if i < argc {
			argv = append(argv, part)
			continue
		}
		if part == "" {
			break
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			env[k] = v
		}
	}
	return argv, env, nil
}

func (osProcessProber) Environ(pid int) (map[string]string, error) {
	_, env, err := darwinProcArgs(pid)
	return env, err
}

func (osProcessProber) Cmdline(pid int) ([]string, error) {
	argv, _, err := darwinProcArgs(pid)
	return argv, err
}

func (osProcessProber) Children(pid int, recursive bool) []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	kids := map[int][]int{}
	for _, kp := range procs {
		if kp.Proc.P_stat == darwinZombieState {
			continue
		}
		kids[int(kp.Eproc.Ppid)] = append(kids[int(kp.Eproc.Ppid)], int(kp.Proc.P_pid))
	}
	if !recursive {
		return kids[pid]
	}
	seen := map[int]bool{pid: true}
	out := []int{}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, k := range kids[cur] {
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
			queue = append(queue, k)
		}
	}
	return out
}
