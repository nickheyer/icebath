//go:build windows

package executil

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Kills a started command, and every process below it when its attributes gave it a process group of its own with CREATE_NEW_PROCESS_GROUP
func kill(cmd *exec.Cmd) error {
	var tree []windows.Handle
	if a := cmd.SysProcAttr; a != nil && a.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP != 0 {
		tree = descendants(uint32(cmd.Process.Pid))
	}
	err := cmd.Process.Kill()
	for _, h := range tree {
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
	return err
}

// Handles open for termination of the processes root started and those they started in turn, as one snapshot of the process table links them; a process created before its listed parent holds a reused process ID and is left alone
func descendants(root uint32) []windows.Handle {
	rootStart, ok := startedAt(root)
	if !ok {
		return nil
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	children := make(map[uint32][]uint32)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if entry.ProcessID != entry.ParentProcessID {
			children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
		}
	}
	windows.CloseHandle(snap)
	type node struct {
		pid   uint32
		start int64
	}
	var handles []windows.Handle
	seen := map[uint32]bool{root: true}
	queue := []node{{root, rootStart}}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, pid := range children[parent.pid] {
			if seen[pid] {
				continue
			}
			seen[pid] = true
			h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
			if err != nil {
				continue
			}
			start, ok := creationTime(h)
			if !ok || start < parent.start {
				windows.CloseHandle(h)
				continue
			}
			handles = append(handles, h)
			queue = append(queue, node{pid, start})
		}
	}
	return handles
}

// Creation time of a running process in nanoseconds, false when it cannot be opened
func startedAt(pid uint32) (int64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	return creationTime(h)
}

func creationTime(h windows.Handle) (int64, bool) {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0, false
	}
	return created.Nanoseconds(), true
}
