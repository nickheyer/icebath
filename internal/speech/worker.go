package speech

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/proc"
)

// Python of a local engine that moves eSpeak NG data under a short path, as its C library fails on data paths of 160 bytes or more
const ShortEspeak = `
def short_espeak_data(data):
    import shutil, tempfile
    import hashlib
    if not data or len(data) < 150:
        return data
    tag = hashlib.sha256(data.encode()).hexdigest()[:16]
    short = os.path.join(tempfile.gettempdir(), "icebath-espeak-ng-data-" + tag)
    if not os.path.isfile(os.path.join(short, "phontab")):
        staging = short + ".%d" % os.getpid()
        shutil.copytree(data, staging)
        try:
            os.replace(staging, short)
        except OSError:
            shutil.rmtree(staging, ignore_errors=True)
    return short
`

// Long lived Python process of a local engine holding one loaded model, asked one JSON line at a time
type Worker struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	closed bool
}

// Starts the script in the interpreter of a provisioned Python set, its error output logged, with the variables given over the profile's
func (j *Job) Worker(extra, script string, env map[string]string) (*Worker, error) {
	python, err := j.Python(extra)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(python, "-c", script)
	cmd.Env = j.Environ(env)
	proc.HideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		lines := bufio.NewScanner(stderr)
		for lines.Scan() {
			slog.Debug(fmt.Sprintf("[%s] %s", extra, lines.Text()))
		}
	}()
	return &Worker{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}, nil
}

// Sends one request line and decodes the reply line into the reply
func (w *Worker) Ask(request, reply any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("local model was released")
	}
	line, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err := w.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("local engine process exited: %w", err)
	}
	raw, err := w.out.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("local engine process exited: %w", err)
	}
	return json.Unmarshal(raw, reply)
}

// Ends the process after any request in flight, killing it when it does not exit within five seconds
func (w *Worker) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	w.stdin.Close()
	done := make(chan struct{})
	go func() {
		w.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		w.cmd.Process.Kill()
		<-done
	}
}

// Runs a helper script in the interpreter of a provisioned Python set to its end within the timeout, its output captured
func (j *Job) Helper(ctx context.Context, extra string, args []string, timeout time.Duration) (string, string, int, error) {
	python, err := j.Python(extra)
	if err != nil {
		return "", "", 0, err
	}
	captured, err := Run(ctx, append([]string{python}, args...), timeout, j.Environ(nil))
	if captured == nil {
		return "", "", 0, err
	}
	return captured.Stdout, captured.Stderr, captured.Code, nil
}
