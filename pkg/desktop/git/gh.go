package git

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	ghTimeout   = 30 * time.Second
	ghMaxBuffer = 8 * 1024 * 1024
)

// Process environment with PATH led by the gh folder and the package manager bins a GUI launch omits
func ghEnv(ghBin string) []string {
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"}
	if ghBin != "" {
		dirs = append([]string{hostPath.dirname(ghBin)}, dirs...)
	}
	var parts []string
	for _, d := range dirs {
		if d != "" && d != "." {
			parts = append(parts, d)
		}
	}
	if p := os.Getenv("PATH"); p != "" {
		parts = append(parts, p)
	}
	return append(os.Environ(), "PATH="+strings.Join(parts, string(os.PathListSeparator)))
}

type ghResult struct {
	ok     bool
	stdout string
	stderr string
}

// Buffer of one output stream that stops the process once it holds more than the cap
type cappedBuffer struct {
	buf      bytes.Buffer
	exceeded func()
	over     bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	room := ghMaxBuffer - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.over = true
		c.exceeded()
		return len(p), nil
	}
	return c.buf.Write(p)
}

// Runs gh in a repository, ok false on a missing binary, a failed exit, the thirty second deadline or eight MiB of output
func runGh(args []string, cwd, ghBin string) ghResult {
	bin := ghBin
	if bin == "" {
		bin = "gh"
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = ghEnv(ghBin)
	hideWindow(cmd)
	var once sync.Once
	stop := func() { once.Do(func() { terminate(cmd) }) }
	stdout := &cappedBuffer{exceeded: stop}
	stderr := &cappedBuffer{exceeded: stop}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return ghResult{}
	}
	timer := time.AfterFunc(ghTimeout, stop)
	err := cmd.Wait()
	timedOut := !timer.Stop()
	return ghResult{
		ok:     err == nil && !timedOut && !stdout.over && !stderr.over,
		stdout: stdout.buf.String(),
		stderr: stderr.buf.String(),
	}
}
