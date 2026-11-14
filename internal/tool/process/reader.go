package process

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	envPollInterval     = 2 * time.Second
	envDeltaTimeout     = 10 * time.Second
	envCheckTimeout     = 5 * time.Second
	finishRequestedWait = time.Millisecond
)

type readDeadliner interface {
	SetReadDeadline(t time.Time) error
}

func noiseFilter(r *Registry, s *Session, unlessExited bool) func(string) {
	headNoise := true
	return func(chunk string) {
		if headNoise {
			chunk = cleanShellNoise(chunk)
			headNoise = blank(chunk)
		}
		r.ingest(s, chunk, unlessExited)
	}
}

func processWait(p tool.Process) func(ctx context.Context) (*int, error) {
	return func(ctx context.Context) (*int, error) {
		code, err := p.Wait(ctx)
		if err != nil {
			return nil, err
		}
		return &code, nil
	}
}

func processExit(p tool.Process) func() *int {
	return func() *int {
		if code, ok := p.Exited(); ok {
			return &code
		}
		return nil
	}
}

func (r *Registry) readPipe(ctx context.Context, s *Session) {
	dec := &utf8Decoder{}
	appendChunk := noiseFilter(r, s, false)
	p := s.process
	if out := p.Output(); out != nil {
		r.pumpPipe(s, p, out, dec, appendChunk)
	}
	r.finishReader(ctx, s, dec, appendChunk, "Process", processWait(p), processExit(p))
}

func (r *Registry) pumpPipe(s *Session, p tool.Process, out io.Reader, dec *utf8Decoder, appendChunk func(string)) {
	deadliner, ok := out.(readDeadliner)
	selectable := ok && runtime.GOOS != "windows" && deadliner.SetReadDeadline(time.Time{}) == nil
	s.selectable.Store(selectable)
	buf := make([]byte, pipeReadBytes)
	idleAfterExit := 0
	drained := 0
	for {
		if selectable {
			wait := pipeIdleWait
			if s.finishRequested() {
				wait = finishRequestedWait
			}
			if err := deadliner.SetReadDeadline(time.Now().Add(wait)); err != nil {
				slog.Debug("Process stdout reader ended", "err", err)
				return
			}
		}
		n, err := out.Read(buf)
		if n == 0 && selectable && errors.Is(err, os.ErrDeadlineExceeded) {
			if s.finishRequested() {
				return
			}
			if _, exited := p.Exited(); exited {
				idleAfterExit++
			}
			if idleAfterExit >= pipeIdleCycles {
				return
			}
			continue
		}
		chunk := ""
		if n > 0 {
			chunk = dec.decode(buf[:n], false)
			if chunk != "" {
				appendChunk(chunk)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Debug("Process stdout reader ended", "err", err)
			}
			return
		}
		if s.finishRequested() {
			drained += utf8.RuneCountInString(chunk)
			if drained >= finalDrainMaxChars {
				return
			}
		}
		idleAfterExit = 0
	}
}

func (r *Registry) finishReader(ctx context.Context, s *Session, dec *utf8Decoder, appendChunk func(string), label string,
	wait func(ctx context.Context) (*int, error), known func() *int) {
	if tail := dec.decode(nil, true); tail != "" {
		appendChunk(tail)
	}
	code, err := wait(ctx)
	if err != nil {
		code = known()
		if code == nil {
			slog.Warn(label+" wait failed; leaving process tracked", "process", s.id, "err", err)
			return
		}
		slog.Warn(label+" wait failed; recording known exit status", "process", s.id, "err", err)
	}
	r.finishExited(ctx, s, code, reasonExited, "", true)
}

func (r *Registry) readPTY(ctx context.Context, s *Session) {
	j := s.pty
	dec := &utf8Decoder{}
	var responder *queryResponder
	if j.answersQueries() {
		responder = newQueryResponder(ptyRows, ptyCols)
	}
	ingest := func(text string) { r.ingest(s, text, true) }
	buf := make([]byte, pipeReadBytes)
	for j.alive() {
		n, err := j.read(buf)
		if n > 0 {
			chunk := buf[:n]
			if responder != nil {
				var replies []byte
				chunk, replies = responder.process(chunk)
				if len(replies) > 0 {
					if _, werr := j.write(replies); werr != nil {
						slog.Debug("PTY query response write failed", "err", werr)
					}
				}
			}
			if text := dec.decode(chunk, false); text != "" {
				ingest(text)
			}
		}
		if err != nil {
			break
		}
	}
	if responder != nil {
		if tail := dec.decode(responder.flush(), false); tail != "" {
			ingest(tail)
		}
	}
	r.finishReader(ctx, s, dec, ingest, "PTY", j.wait, func() *int {
		code, _ := j.exitCode()
		return code
	})
}

func lastLine(text string) string {
	lines := outputLines(text)
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

func (r *Registry) pollEnv(ctx context.Context, s *Session, env tool.Environment, logPath, pidPath, exitPath string) {
	q := shell.Quote
	lost := func() {
		if ctx.Err() == nil {
			r.finishExited(ctx, s, intPtr(-1), reasonLost, sourceLost, true)
		}
	}
	var prev int64
	for !s.isExited() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(envPollInterval):
		}
		res, err := env.Execute(ctx, logDeltaCommand(q(logPath), prev),
			tool.ExecOptions{Timeout: envDeltaTimeout, RewriteCompoundBackground: true})
		if err != nil {
			lost()
			return
		}
		header, delta, _ := strings.Cut(res.Output, "\n")
		fields := strings.Fields(header)
		var size, used int64
		parsed := len(fields) == 2
		if parsed {
			var e1, e2 error
			size, e1 = strconv.ParseInt(fields[0], 10, 64)
			used, e2 = strconv.ParseInt(fields[1], 10, 64)
			parsed = e1 == nil && e2 == nil
		}
		if !parsed {
			delta = ""
		} else {
			if used < prev {
				s.mu.Lock()
				if s.exited {
					s.mu.Unlock()
					return
				}
				s.out.replace("")
				s.mu.Unlock()
			}
			prev = size
		}
		if delta != "" {
			if !s.appendOutputIfRunning(delta) {
				return
			}
			r.checkWatch(s, delta)
			r.emitOutput(s, delta)
		}
		check, err := env.Execute(ctx, `kill -0 "$(cat `+q(pidPath)+` 2>/dev/null)" 2>/dev/null; echo $?`,
			tool.ExecOptions{Timeout: envCheckTimeout, RewriteCompoundBackground: true})
		if err != nil {
			lost()
			return
		}
		if out := strings.TrimSpace(check.Output); out != "" && lastLine(out) != "0" {
			res, err := env.Execute(ctx, "cat "+q(exitPath)+" 2>/dev/null",
				tool.ExecOptions{Timeout: envCheckTimeout, RewriteCompoundBackground: true})
			if err != nil {
				lost()
				return
			}
			code := -1
			if v, err := strconv.Atoi(lastLine(strings.TrimSpace(res.Output))); err == nil {
				code = v
			}
			r.finishExited(ctx, s, &code, reasonExited, "", true)
			return
		}
	}
}
