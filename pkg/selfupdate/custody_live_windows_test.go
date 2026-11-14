package selfupdate

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, the job class x/sys/windows leaves out
type cwJobAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

const cwJobObjectBasicAccountingInformation = 1

// Runs the join launcher with handle and the built child, returning its exit code, stdout, stderr and report
func cwJoinLaunch(t *testing.T, handle string, attr *syscall.SysProcAttr, childCode string) (int, string, string, string) {
	t.Helper()
	report := filepath.Join(t.TempDir(), "report.txt")
	cmd := exec.Command(os.Args[0], append([]string{CustodyJoinEntry, handle, report}, cuChildArgv("built", childCode)...)...)
	cmd.SysProcAttr = attr
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	raw, _ := os.ReadFile(report)
	return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String(), strings.TrimPrefix(string(raw), "\ufeff")
}

// Runs the join launcher in a job that lets its children break away, the command landing outside the job
func cwEscapingJobLaunch(t *testing.T, processLimit uint32) (int, string, string, uint32) {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK
	if processLimit != 0 {
		limits.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		limits.BasicLimitInformation.ActiveProcessLimit = processLimit
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetHandleInformation(job, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		t.Fatal(err)
	}
	attr := &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(job)}}
	code, stdout, stderr, _ := cwJoinLaunch(t, fmt.Sprint(uintptr(job)), attr, "0")
	var usage cwJobAccounting
	if err := windows.QueryInformationJobObject(job, cwJobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&usage)), uint32(unsafe.Sizeof(usage)), nil); err != nil {
		t.Fatal(err)
	}
	return code, stdout, stderr, usage.TotalProcesses
}

func TestCustodyWindowsARefusedJobJoinNeverRunsTheCommand(t *testing.T) {
	code, stdout, stderr, report := cwJoinLaunch(t, "0", nil, "3")
	if code != custodyRefusedExit || strings.Contains(stdout, "built") {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, custodyUnavailable) || !strings.Contains(report, custodyUnavailable) {
		t.Fatalf("%q %q", stderr, report)
	}
}

func TestCustodyWindowsACommandThatStartsOutsideTheJobIsPutInItAndRuns(t *testing.T) {
	code, stdout, stderr, total := cwEscapingJobLaunch(t, 0)
	if code != 0 || !strings.Contains(stdout, "built") {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
	if total < 2 {
		t.Fatal("the command ran outside the update job")
	}
}

func TestCustodyWindowsACommandTheJobWillNotTakeNeverRuns(t *testing.T) {
	code, stdout, stderr, _ := cwEscapingJobLaunch(t, 1)
	if code != custodyRefusedExit || strings.Contains(stdout, "built") || !strings.Contains(stderr, custodyUnavailable) {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
}
