//go:build windows

package tools

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Windows half of the process identity layer, the kill on close job object and the shared process probes

// Job handle kept alive for the life of the process so the kernel kills the job when it dies
var jobHandle windows.Handle

// Places this process in a job that dies whole when it dies, idempotent
func attachSelfToKillOnCloseJob() bool {
	if jobHandle != 0 {
		return true
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil || job == 0 {
		return false
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return false
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return false
	}
	jobHandle = job
	identityLogger().Debug("attached to kill-on-close job object")
	return true
}

// Windows has no process groups to probe, the pgid is read as a pid
func processGroupGone(pgid int) bool { return !(osProcessProber{}).Running(pgid) }

// Windows has no group signals, the pgid is terminated as a pid
func signalProcessGroup(pgid int, force bool) error { return (osProcessProber{}).Terminate(pgid) }

// The POSIX supervisor never runs on Windows, the job object owns the children
func startSupervisorProcess() (supervisorHandle, error) {
	return nil, errors.New("owner-death supervisor is POSIX only")
}

// Process facts read through the Win32 process API
type osProcessProber struct{}

func (osProcessProber) Running(pid int) bool {
	h, err := proc.OpenProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == proc.StillActive
}

func (osProcessProber) Birth(pid int) (float64, error) { return proc.Birth(pid) }

func (osProcessProber) WorkerBirth(pid int) (float64, error) { return proc.WorkerBirth(pid) }

func (osProcessProber) BootStart(pid int) (float64, bool) { return proc.BootStart(pid) }

func (osProcessProber) Zombie(pid int) bool { return proc.Zombie(pid) }

func (osProcessProber) PPID(pid int) (int, bool) { return proc.PPID(pid) }

func (osProcessProber) Children(pid int, recursive bool) []int {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	kids := map[int][]int{}
	for _, p := range table {
		kids[p.PPID] = append(kids[p.PPID], p.PID)
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

func (osProcessProber) Terminate(pid int) error {
	h, err := proc.OpenProcess(pid, windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 15)
}

func (p osProcessProber) Kill(pid int) error { return p.Terminate(pid) }

func (osProcessProber) Environ(pid int) (map[string]string, error) { return proc.Environ(pid) }

func (osProcessProber) Cmdline(pid int) ([]string, error) { return proc.Cmdline(pid) }
