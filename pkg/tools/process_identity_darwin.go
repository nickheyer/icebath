//go:build darwin

package tools

import (
	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// macOS process facts read through sysctl by the shared process probes

func (osProcessProber) Birth(pid int) (float64, error) { return proc.Birth(pid) }

func (osProcessProber) WorkerBirth(pid int) (float64, error) { return proc.WorkerBirth(pid) }

func (osProcessProber) BootStart(pid int) (float64, bool) { return proc.BootStart(pid) }

func (osProcessProber) Zombie(pid int) bool { return proc.Zombie(pid) }

func (osProcessProber) PPID(pid int) (int, bool) { return proc.PPID(pid) }

func (osProcessProber) Environ(pid int) (map[string]string, error) { return proc.Environ(pid) }

func (osProcessProber) Cmdline(pid int) ([]string, error) { return proc.Cmdline(pid) }

func (osProcessProber) Children(pid int, recursive bool) []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	kids := map[int][]int{}
	for _, kp := range procs {
		if kp.Proc.P_stat == proc.DarwinZombieState {
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
