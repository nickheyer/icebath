package packages

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
)

// A child the package manager runs
type command struct {
	argv []string
	dir  string
	// Its whole environment, this process's when nil
	env     map[string]string
	timeout time.Duration
	// Lines of its output as they come, captured instead when nil
	stream func(text string)
}

type result struct {
	code   int
	stdout string
	stderr string
}

// A child stopped at its deadline
var errTimedOut = errors.New("timed out")

// Runs a child to its end with no input, killing it at its deadline or when the context ends
func run(ctx context.Context, c command) (result, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...)
	cmd.Dir = c.dir
	if c.env != nil {
		cmd.Env = envList(c.env)
	}
	cmd.WaitDelay = 5 * time.Second
	proc.HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	if c.stream != nil {
		w := &lineWriter{emit: c.stream}
		cmd.Stdout, cmd.Stderr = w, w
		defer w.flush()
	} else {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	}
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return result{}, errTimedOut
	}
	if ctx.Err() != nil {
		return result{}, ctx.Err()
	}
	out := result{stdout: strings.ToValidUTF8(stdout.String(), "�"), stderr: strings.ToValidUTF8(stderr.String(), "�")}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		out.code = exit.ExitCode()
		return out, nil
	}
	return out, err
}

// Writer handing whole decoded text on, a character split across writes held until it completes
type lineWriter struct {
	emit    func(string)
	pending []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	data := append(w.pending, p...)
	end := len(data)
	for end > 0 && end > len(data)-utf8.UTFMax && !utf8.Valid(data[:end]) {
		end--
	}
	if end == 0 && len(data) >= utf8.UTFMax {
		end = len(data)
	}
	w.pending = slices.Clone(data[end:])
	if end > 0 {
		w.emit(strings.ToValidUTF8(string(data[:end]), "�"))
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.pending) > 0 {
		w.emit(strings.ToValidUTF8(string(w.pending), "�"))
		w.pending = nil
	}
}

// Environment as sorted KEY=value entries
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// Executable a name resolves to on a search path, Windows trying its program extensions; empty when none
func Which(name, path string) string {
	if strings.ContainsAny(name, `/\`) {
		if fsx.Executable(name) {
			return name
		}
		return ""
	}
	names := []string{name}
	if runtime.GOOS == "windows" {
		names = nil
		exts := strings.Split(cmp.Or(os.Getenv("PATHEXT"), ".COM;.EXE;.BAT;.CMD"), ";")
		for _, ext := range exts {
			if ext != "" && strings.HasSuffix(strings.ToUpper(name), strings.ToUpper(ext)) {
				names = []string{name}
				break
			}
		}
		for _, ext := range exts {
			if ext != "" {
				names = append(names, name+ext)
			}
		}
	}
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		for _, candidate := range names {
			full := dir + string(os.PathSeparator) + candidate
			if fsx.Executable(full) {
				return full
			}
		}
	}
	return ""
}
