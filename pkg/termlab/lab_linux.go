package termlab

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Directory where the X server of every display keeps its socket
const x11Dir = "/tmp/.X11-unix"

// Private X display served by Xvfb
type Lab struct {
	Display int
	W, H    int
	xvfb    *exec.Cmd
	done    chan struct{}
	log     bytes.Buffer
}

// Error naming every given tool absent from PATH, nil when all of them are there
func Missing(tools ...string) error {
	var gone []string
	for _, t := range tools {
		if _, err := exec.LookPath(t); err != nil {
			gone = append(gone, t)
		}
	}
	if len(gone) > 0 {
		return fmt.Errorf("missing %v on PATH", gone)
	}
	return nil
}

// Starts Xvfb with a w by h screen on the first display number holding no socket or lock file
func Start(w, h int) (*Lab, error) {
	if err := Missing("Xvfb", "import"); err != nil {
		return nil, err
	}
	var last error
	for n := 40; n < 400; n++ {
		if taken(n) {
			continue
		}
		l, err := startOn(n, w, h)
		if err == nil {
			return l, nil
		}
		last = err
	}
	return nil, fmt.Errorf("no free display: %w", last)
}

// Reports whether a display number has a socket or a lock file already
func taken(n int) bool {
	for _, p := range []string{filepath.Join(x11Dir, "X"+strconv.Itoa(n)), "/tmp/.X" + strconv.Itoa(n) + "-lock"} {
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}
	return false
}

// Starts Xvfb on one display and waits for its socket, failing when the server exits first
func startOn(n, w, h int) (*Lab, error) {
	l := &Lab{Display: n, W: w, H: h, done: make(chan struct{})}
	cmd := exec.Command("Xvfb", ":"+strconv.Itoa(n), "-screen", "0", fmt.Sprintf("%dx%dx24", w, h), "-nolisten", "tcp", "-noreset")
	cmd.Stdout, cmd.Stderr = &l.log, &l.log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	l.xvfb = cmd
	go func() {
		cmd.Wait()
		close(l.done)
	}()
	sock := filepath.Join(x11Dir, "X"+strconv.Itoa(n))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-l.done:
			return nil, fmt.Errorf("Xvfb :%d exited: %s", n, bytes.TrimSpace(l.log.Bytes()))
		default:
		}
		if _, err := os.Stat(sock); err == nil {
			if _, err := l.Shot(); err == nil {
				return l, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.Close()
	return nil, fmt.Errorf("Xvfb :%d gave no socket within 10s", n)
}

// Display name for the DISPLAY variable
func (l *Lab) Name() string { return ":" + strconv.Itoa(l.Display) }

// Captures the whole screen of the display
func (l *Lab) Shot() (*image.RGBA, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("import", "-display", l.Name(), "-window", "root", "-depth", "8", "ppm:-")
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("import: %w: %s", err, bytes.TrimSpace(errb.Bytes()))
	}
	return decodePPM(&out)
}

// Stops the X server, asking first so it removes its socket and lock, then killing it
func (l *Lab) Close() error {
	if l.xvfb == nil || l.xvfb.Process == nil {
		return nil
	}
	l.xvfb.Process.Signal(syscall.SIGTERM)
	select {
	case <-l.done:
		return nil
	case <-time.After(3 * time.Second):
	}
	proc.KillTree(l.xvfb.Process.Pid)
	select {
	case <-l.done:
	case <-time.After(5 * time.Second):
		return errors.New("Xvfb outlived a kill for 5s")
	}
	os.Remove(filepath.Join(x11Dir, "X"+strconv.Itoa(l.Display)))
	os.Remove("/tmp/.X" + strconv.Itoa(l.Display) + "-lock")
	return nil
}
