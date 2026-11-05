//go:build windows

package provider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func lr3EnvEchoCmd(name string) *exec.Cmd {
	return exec.Command(os.Getenv("ComSpec"), "/d", "/c", "echo %"+name+"%")
}

// Contains the router in a job, closes it twice on the stop file, and idles until the exit file
func lr3OwnerRole(root string) {
	router := lr3HelperCmd(root, "router")
	job, err := SpawnServer(router, false)
	if err != nil {
		os.Exit(3)
	}
	_ = os.WriteFile(filepath.Join(root, "ready"), []byte("ready"), 0o644)
	for !lr3Exists(filepath.Join(root, "stop"))() {
		time.Sleep(20 * time.Millisecond)
	}
	_ = job.Close()
	_ = job.Close()
	done := make(chan struct{})
	go func() { _ = router.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		os.Exit(4)
	}
	_ = os.WriteFile(filepath.Join(root, "closed"), []byte("closed"), 0o644)
	for !lr3Exists(filepath.Join(root, "exit"))() {
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOwnerExitKillsRouterTreeNotExternal(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, stopMode := range []string{"graceful", "abrupt"} {
			t.Run(fmt.Sprintf("%s-nested-%v", stopMode, nested), func(t *testing.T) {
				lr3OwnerExitCase(t, stopMode, nested)
			})
		}
	}
}

func lr3OwnerExitCase(t *testing.T, stopMode string, nested bool) {
	root := t.TempDir()
	log, err := os.Create(filepath.Join(root, "children.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var launchers []*exec.Cmd
	var outerJob *WindowsJob
	defer func() {
		_ = outerJob.Close()
		matches, _ := filepath.Glob(filepath.Join(root, "*.json"))
		for _, path := range matches {
			lr3Kill(lr3Read(t, path))
		}
		for _, l := range launchers {
			_ = l.Process.Kill()
			_ = l.Wait()
		}
	}()
	for _, role := range []string{"control", "owner"} {
		cmd := lr3HelperCmd(root, role)
		cmd.Stdout, cmd.Stderr = log, log
		if nested && role == "owner" {
			outerJob, err = SpawnServer(cmd, false)
		} else {
			err = cmd.Start()
		}
		if err != nil {
			t.Fatal(err)
		}
		launchers = append(launchers, cmd)
	}
	owner := lr3Read(t, filepath.Join(root, "owner.json"))
	router := lr3Read(t, filepath.Join(root, "router.json"))
	grandchild := lr3Read(t, filepath.Join(root, "grandchild.json"))
	control := lr3Read(t, filepath.Join(root, "control.json"))
	if !lr3Wait(lr3Exists(filepath.Join(root, "ready")), 10*time.Second) {
		t.Fatal("owner never became ready")
	}
	for _, id := range []lr3Identity{owner, router, grandchild, control} {
		if !lr3Alive(id) {
			t.Fatalf("disposable identity %v not alive", id)
		}
	}
	treeGone := func() bool { return !lr3Alive(router) && !lr3Alive(grandchild) }
	if stopMode == "graceful" {
		_ = os.WriteFile(filepath.Join(root, "stop"), []byte("stop"), 0o644)
		if !lr3Wait(lr3Exists(filepath.Join(root, "closed")), 10*time.Second) {
			t.Fatal("close killed the owner itself")
		}
		if !lr3Alive(owner) || !lr3Wait(treeGone, 10*time.Second) || !lr3Alive(control) {
			t.Fatalf("graceful close state owner %v router %v grandchild %v control %v",
				lr3Alive(owner), lr3Alive(router), lr3Alive(grandchild), lr3Alive(control))
		}
		_ = os.WriteFile(filepath.Join(root, "exit"), []byte("exit"), 0o644)
	} else {
		lr3Kill(owner)
	}
	if !lr3Wait(func() bool { return !lr3Alive(owner) }, 10*time.Second) {
		t.Fatal("owner survived")
	}
	if !lr3Wait(treeGone, 10*time.Second) {
		t.Fatalf("uncontained router/grandchild survived owner exit %v %v", router, grandchild)
	}
	if !lr3Alive(control) {
		t.Fatal("unrelated external control was terminated")
	}
}

var getHandleInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetHandleInformation")

func lr3HandleFlags(h windows.Handle) (uint32, bool) {
	var flags uint32
	ok, _, _ := getHandleInformation.Call(uintptr(h), uintptr(unsafe.Pointer(&flags)))
	return flags, ok != 0
}

func TestFailedSetupNeverRunsChildAndReleasesHandles(t *testing.T) {
	for _, failure := range []string{"assign", "resume", "popen", "configure"} {
		t.Run(failure, func(t *testing.T) { lr3FailedSetupCase(t, failure) })
	}
}

func lr3FailedSetupCase(t *testing.T, failure string) {
	root := t.TempDir()
	marker := filepath.Join(root, "child executed")
	var jobs []*WindowsJob
	var children []*exec.Cmd
	var handles []windows.Handle
	realAPI, realNew, realAssign, realResume := windowsJobAPI, newWindowsJob, assignWindowsJob, resumeProcessFn
	t.Cleanup(func() {
		windowsJobAPI, newWindowsJob, assignWindowsJob, resumeProcessFn = realAPI, realNew, realAssign, realResume
		for _, c := range children {
			_ = c.Process.Kill()
		}
		for _, j := range jobs {
			_ = j.Close()
		}
	})
	if failure == "configure" {
		windowsJobAPI.set = func(job windows.Handle, class uint32, info uintptr, size uint32) error {
			handles = append(handles, job)
			return errors.New("injected cancellation configuring job")
		}
		windowsJobAPI.close = func(h windows.Handle) error {
			jobs = append(jobs, &WindowsJob{handle: h})
			return realAPI.close(h)
		}
	}
	newWindowsJob = func() (*WindowsJob, error) {
		job, err := realNew()
		if job != nil {
			jobs = append(jobs, job)
			handles = append(handles, job.handle)
		}
		return job, err
	}
	assignWindowsJob = func(job *WindowsJob, cmd *exec.Cmd) error {
		children = append(children, cmd)
		if lr3Exists(marker)() {
			t.Error("child ran before assignment")
		}
		var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		if err := windows.QueryInformationJobObject(job.handle, jobExtendedLimitCls, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)), nil); err != nil {
			t.Errorf("query job: %v", err)
		}
		flags := limits.BasicLimitInformation.LimitFlags
		if flags&0x2000 == 0 || flags&(0x800|0x1000) != 0 {
			t.Errorf("job limit flags %#x", flags)
		}
		if inherit, ok := lr3HandleFlags(job.handle); !ok || inherit&1 != 0 {
			t.Errorf("job handle inheritable or invalid: %#x %v", inherit, ok)
		}
		if failure == "assign" {
			return windowsJobAPI.assign(job.handle, 0)
		}
		return realAssign(job, cmd)
	}
	resumeProcessFn = func(int) error { return errors.New("injected cancellation before resume") }
	argv := []string{os.Args[0], "-test.run=^TestLocalRuntimeMarkerHelper$"}
	if failure == "popen" {
		argv = []string{filepath.Join(root, "missing.exe")}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "ICEBATH_LRT_MARKER="+marker)
	stdin, _ := cmd.StdinPipe()
	if _, err := SpawnServer(cmd, false); err == nil {
		t.Fatal("SpawnServer succeeded despite injected failure")
	}
	if lr3Exists(marker)() {
		t.Fatal("child executed")
	}
	if len(jobs) == 0 || jobs[0].open {
		t.Fatal("job handle not released")
	}
	for _, c := range children {
		if c.ProcessState == nil {
			t.Fatal("failed setup left a live suspended child")
		}
		if _, err := stdin.Write([]byte("x")); err == nil {
			t.Fatal("failed setup leaked the child's stdin pipe")
		}
	}
	for _, h := range handles {
		if _, ok := lr3HandleFlags(h); ok {
			t.Fatalf("job handle %v still open", h)
		}
	}
}

// Writes the marker file named by the environment, proving the child ran
func TestLocalRuntimeMarkerHelper(t *testing.T) {
	if path := os.Getenv("ICEBATH_LRT_MARKER"); path != "" {
		_ = os.WriteFile(path, []byte("ran"), 0o644)
		os.Exit(0)
	}
}
