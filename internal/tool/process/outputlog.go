package process

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/shell"
)

const (
	logTailInterval   = 200 * time.Millisecond
	logReadBytes      = 65536
	logFinalDrainMax  = 1 << 20
	resultRetention   = 7 * 24 * time.Hour
	maxRetainedResult = 64
)

func (r *Registry) outputDir(ctx context.Context) (string, error) {
	h, err := r.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return home.ProcessOutput.In(h), nil
}

func (r *Registry) workerOutputLog(ctx context.Context, id string) (string, error) {
	if !r.role.Worker() {
		return "", nil
	}
	dir, err := r.outputDir(ctx)
	if err != nil {
		return "", err
	}
	if err := r.homes.MakeUserDir(ctx, dir, 0o700, true); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	cutoff := time.Now().Add(-resultRetention)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "proc_") || !strings.HasSuffix(name, ".exit") {
			continue
		}
		exited := filepath.Join(dir, name)
		info, err := os.Stat(exited)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(strings.TrimSuffix(exited, ".exit") + ".log"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := os.Remove(exited); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return filepath.Join(dir, id+".log"), nil
}

func (r *Registry) adoptableLog(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	dir, err := r.outputDir(ctx)
	if err != nil {
		return ""
	}
	log := filepath.Clean(path)
	if filepath.Dir(log) != filepath.Clean(dir) || filepath.Ext(log) != ".log" {
		return ""
	}
	return log
}

func openOutputLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

func exitPath(log string) string {
	return strings.TrimSuffix(log, filepath.Ext(log)) + ".exit"
}

func recordExitCommand(log, command string) string {
	target := shell.Quote(filepath.ToSlash(exitPath(log)))
	return "__icebath_exit_file=" + target + `; trap 'printf "%s\n" "$?" > "$__icebath_exit_file"' EXIT` + "\n" + command
}

func recordedExitCode(log string) *int {
	text, err := fileutil.ReadText(exitPath(log))
	if err != nil {
		return nil
	}
	code, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return nil
	}
	return &code
}

func (r *Registry) readLog(ctx context.Context, s *Session) {
	dec := &utf8Decoder{}
	p := s.process
	s.selectable.Store(true)
	ingest := noiseFilter(r, s, false)
	s.mu.Lock()
	pid, expected, logPath := s.pid, s.hostStart, s.outputLog
	s.mu.Unlock()
	running := func() bool {
		if s.finishRequested() {
			return false
		}
		if p != nil {
			_, exited := p.Exited()
			return !exited
		}
		return detachedHostFate(pid, expected) == fateRunning
	}
	if f, err := os.Open(logPath); err != nil {
		slog.Warn("Process output log unreadable", "path", logPath, "err", err)
	} else {
		buf := make([]byte, logReadBytes)
		drained := 0
		for {
			alive := running()
			n, err := f.Read(buf)
			if n > 0 {
				if text := dec.decode(buf[:n], false); text != "" {
					ingest(text)
				}
				if !alive {
					drained += n
				}
				if drained < logFinalDrainMax {
					continue
				}
			}
			if err != nil && !errors.Is(err, io.EOF) {
				slog.Warn("Process output log unreadable", "path", logPath, "err", err)
				break
			}
			if !alive {
				break
			}
			select {
			case <-ctx.Done():
				_ = f.Close()
				return
			case <-time.After(logTailInterval):
			}
		}
		_ = f.Close()
	}
	if p != nil {
		r.finishReader(ctx, s, dec, ingest, "Log", processWait(p), processExit(p))
		return
	}
	if ctx.Err() != nil {
		return
	}
	if tail := dec.decode(nil, true); tail != "" {
		ingest(tail)
	}
	if code := recordedExitCode(logPath); code != nil {
		r.finishExited(ctx, s, code, reasonExited, "", true)
		return
	}
	r.finishExited(ctx, s, intPtr(-1), reasonLost, sourceLost, true)
}
