package selfupdate

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// Kernel calls the job joining launcher makes, a recording fake in tests and kernel32 and ntdll on Windows
type joinAPI interface {
	Assign(job, process uintptr) bool
	CurrentProcess() uintptr
	LastError() int
	CreateJob() uintptr
	SetKillOnClose(job uintptr) bool
	InJob(process, job uintptr) bool
	CloseHandle(handle uintptr)
	Resume(process uintptr) int
	Terminate(job uintptr, code uint32)
	ActiveProcesses(job uintptr) (uint32, bool)
	StartSuspended(argv []string) (joinChild, error)
	Stderr() io.Writer
}

// Suspended command the launcher started
type joinChild interface {
	Handle() uintptr
	Wait() int
	Kill()
}

// Joins the update job named by an inherited handle, runs the command only once it is in that job and reaps its tree
func joinJobMain(args []string, api joinAPI) int {
	stderr := io.Writer(os.Stderr)
	if api != nil {
		stderr = api.Stderr()
	}
	refuse := func(why string) int {
		note := fmt.Sprintf("%s (%s); the command was not run", custodyUnavailable, why)
		fmt.Fprintln(stderr, note)
		if len(args) > 1 {
			_ = os.WriteFile(args[1], []byte(note), 0o644)
		}
		return custodyRefusedExit
	}
	if api == nil {
		return refuse("this platform has no update job to join")
	}
	if len(args) < 3 {
		return refuse("the launcher was given no command")
	}
	parsed, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return refuse("the update job handle is not a number: " + args[0])
	}
	h := uintptr(parsed)
	if !api.Assign(h, api.CurrentProcess()) {
		return refuse(fmt.Sprintf("could not join the update job: %d", api.LastError()))
	}
	tree := api.CreateJob()
	if tree == 0 || !api.SetKillOnClose(tree) {
		return refuse(fmt.Sprintf("could not create the job for the command tree: %d", api.LastError()))
	}
	p, err := api.StartSuspended(args[2:])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !api.InJob(p.Handle(), h) && !(api.Assign(h, p.Handle()) && api.InJob(p.Handle(), h)) {
		p.Kill()
		p.Wait()
		return refuse("the command would start outside the update job")
	}
	if !api.Assign(tree, p.Handle()) {
		code := api.LastError()
		p.Kill()
		p.Wait()
		return refuse(fmt.Sprintf("could not hold the command tree in a job: %d", code))
	}
	api.CloseHandle(h)
	if api.Resume(p.Handle()) != 0 {
		p.Kill()
	}
	code := p.Wait()
	api.Terminate(tree, 1)
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); {
		active, ok := api.ActiveProcesses(tree)
		if !ok || active == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return code
}
