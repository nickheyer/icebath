package termutil

import (
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
)

// Writer to what Route last sent standard output to, else to whatever os.Stdout holds at the moment of each write
var Stdout io.Writer = stdout{}

// Writer to what Route last sent standard error to, else to whatever os.Stderr holds at the moment of each write
var Stderr io.Writer = stderr{}

// One destination Route put in place
type route struct{ w io.Writer }

// Destinations routed for one stream, the latest one still in place answering each write
type routes struct {
	mu      sync.Mutex
	stack   []*route
	current atomic.Pointer[route]
}

func (r *routes) push(w io.Writer) *route {
	e := &route{w}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stack = append(r.stack, e)
	r.current.Store(e)
	return e
}

// Takes one destination out wherever it stands, the latest one left answering writes
func (r *routes) remove(e *route) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := slices.Index(r.stack, e); i >= 0 {
		r.stack = slices.Delete(r.stack, i, i+1)
	}
	if n := len(r.stack); n > 0 {
		r.current.Store(r.stack[n-1])
	} else {
		r.current.Store(nil)
	}
}

func (r *routes) write(p []byte, fallback *os.File) (int, error) {
	if e := r.current.Load(); e != nil {
		return e.w.Write(p)
	}
	return fallback.Write(p)
}

var stdoutRoutes, stderrRoutes routes

// Sends what is written to Stdout and Stderr to the given writers until restore is called, a nil writer leaving that stream as it is; routes nest, the latest one still in place answers, and restoring them in any order leaves the others in place. Fd, IsTerminal and As on Stdout and Stderr keep answering for os.Stdout and os.Stderr, and a destination must not write to Stdout or Stderr itself
func Route(stdout, stderr io.Writer) (restore func()) {
	var out, errOut *route
	if stdout != nil {
		out = stdoutRoutes.push(stdout)
	}
	if stderr != nil {
		errOut = stderrRoutes.push(stderr)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if out != nil {
				stdoutRoutes.remove(out)
			}
			if errOut != nil {
				stderrRoutes.remove(errOut)
			}
		})
	}
}

type stdout struct{}

// Writes p to the routed destination or the current os.Stdout
func (stdout) Write(p []byte) (int, error) { return stdoutRoutes.write(p, os.Stdout) }

// Descriptor of the current os.Stdout, read without changing its blocking mode
func (stdout) Fd() uintptr { return fileFd(os.Stdout) }

// Current os.Stdout, where capability lookups continue
func (stdout) Unwrap() io.Writer { return os.Stdout }

type stderr struct{}

// Writes p to the routed destination or the current os.Stderr
func (stderr) Write(p []byte) (int, error) { return stderrRoutes.write(p, os.Stderr) }

// Descriptor of the current os.Stderr, read without changing its blocking mode
func (stderr) Fd() uintptr { return fileFd(os.Stderr) }

// Current os.Stderr, where capability lookups continue
func (stderr) Unwrap() io.Writer { return os.Stderr }
