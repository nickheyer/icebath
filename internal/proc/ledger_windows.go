package proc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const zombiesExist = false

const selfJobFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK

func reparentedOrphan(int, func() map[int]bool) bool { return false }

func stopHelper(ctx context.Context, root Identity) bool {
	var ids []Identity
	if table, err := ReadTable(); err == nil {
		ids = table.Descendants(root)
	}
	ids = append(ids, root)
	for _, id := range ids {
		_ = id.Signal(syscall.SIGTERM)
	}
	wait, cancel := context.WithTimeout(ctx, orphanGrace)
	late := Await(wait, ids)
	cancel()
	for _, id := range late {
		_ = id.Signal(syscall.SIGKILL)
	}
	return true
}

func AttachSelfToKillOnCloseJob() (bool, error) {
	self := os.Getpid()
	stamp, err := Fingerprint(self)
	if err != nil {
		return false, err
	}
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`Local\icebath-owner-job-%d-%d`, self, stamp))
	if err != nil {
		return false, err
	}
	job, err := windows.CreateJobObject(nil, name)
	if err != nil {
		return false, fmt.Errorf("create job object: %w", err)
	}
	var member int32
	if r, _, _ := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), uintptr(job), uintptr(unsafe.Pointer(&member))); r != 0 && member != 0 {
		windows.CloseHandle(job)
		return true, nil
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = selfJobFlags
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return false, fmt.Errorf("set job limits: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return false, fmt.Errorf("assign to job object: %w", err)
	}
	slog.Debug("attached to kill-on-close job object")
	return true, nil
}

func IsOwnerDeathSupervisor([]string) bool { return false }

func (g *Guard) StartOwned(_ context.Context, cmd *exec.Cmd, _ *Ledger, _ string) error {
	return g.Start(cmd)
}
