package logging

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Encoding the interpreter would give the process stderr stream
var stderrEncoding = platformStderrEncoding

// Writer onto the current os.Stderr in UTF-8, lone surrogates backslash escaped or replaced by question marks
type safeStderrWriter struct{ replace bool }

func (w safeStderrWriter) Write(p []byte) (int, error) {
	if os.Stderr == nil {
		return len(p), nil
	}
	var data []byte
	if w.replace {
		data = pystr.EncodeUTF8Replace(string(p))
	} else {
		data = []byte(pystr.EncodeUTF8BackslashReplace(string(p)))
	}
	if _, err := os.Stderr.Write(data); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Stderr as is on a UTF-8 stream, otherwise rewrapped in UTF-8 with un-encodable text replaced
func safeStderr() io.Writer {
	encoding := ""
	if os.Stderr != nil {
		encoding = stderrEncoding()
	}
	if encoding == "" {
		encoding = "utf-8"
	}
	switch strings.ReplaceAll(strings.ToLower(encoding), "-", "") {
	case "utf8", "utf8surrogateescape":
		return safeStderrWriter{}
	}
	return safeStderrWriter{replace: true}
}

// One standard stream routed through redaction, the pipe its writers see and the pump draining it
type redactedStdio struct {
	target   **os.File
	original *os.File
	writer   *os.File
	done     chan struct{}
}

var (
	stdioMu        sync.Mutex
	redactedStdios []*redactedStdio
)

// Routes stdout and stderr of a detached process through secret redaction, once per stream
func RedactDetachedStdio() error {
	if RedactSensitiveText == nil {
		return errRedactUnlinked
	}
	stdioMu.Lock()
	defer stdioMu.Unlock()
	for _, target := range []**os.File{&os.Stdout, &os.Stderr} {
		current := *target
		if current == nil || isRedactedStdio(current) {
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		s := &redactedStdio{target: target, original: current, writer: w, done: make(chan struct{})}
		go pumpRedacted(r, current, s.done)
		*target = w
		redactedStdios = append(redactedStdios, s)
	}
	return nil
}

// Reports whether f is the pipe of a redacted stream
func isRedactedStdio(f *os.File) bool {
	for _, s := range redactedStdios {
		if s.writer == f {
			return true
		}
	}
	return false
}

// Copies each write from the pipe to dst redacted, holding a partial line only when a write overran the buffer
func pumpRedacted(r *os.File, dst *os.File, done chan struct{}) {
	defer close(done)
	defer func() { _ = r.Close() }()
	buf := make([]byte, 64*1024)
	var pending []byte
	for {
		n, err := r.Read(buf)
		data := append(pending, buf[:n]...)
		pending = nil
		if n == len(buf) && err == nil {
			if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
				data, pending = data[:i+1], append([]byte(nil), data[i+1:]...)
			}
		}
		if len(data) > 0 {
			_, _ = dst.Write([]byte(RedactSensitiveText(string(data))))
		}
		if err != nil {
			return
		}
	}
}

// Restores the original streams and waits until every redacted byte reached them
func flushDetachedStdio() {
	stdioMu.Lock()
	streams := redactedStdios
	redactedStdios = nil
	for _, s := range streams {
		if *s.target == s.writer {
			*s.target = s.original
		}
	}
	stdioMu.Unlock()
	for _, s := range streams {
		_ = s.writer.Close()
		<-s.done
	}
}

// Text stream that reports a terminal and can switch itself to line buffering
type lineReconfigurer interface {
	IsTerminal() bool
	Reconfigure(lineBuffering bool) error
}

// Stream that reports whether it already flushes at every newline
type lineBufferedStream interface {
	LineBuffering() bool
}

var (
	currentStdout      = func() any { return os.Stdout }
	lineBufferedStdout any
)

// Switches a piped stdout to line buffering once per stream, leaving a terminal or a fixed stream alone
func lineBufferPipedStdout() {
	stdioMu.Lock()
	defer stdioMu.Unlock()
	stream := currentStdout()
	if stream == nil || reflect.ValueOf(stream).Kind() == reflect.Pointer && reflect.ValueOf(stream).IsNil() {
		return
	}
	if reflect.TypeOf(stream).Comparable() && reflect.TypeOf(lineBufferedStdout) == reflect.TypeOf(stream) && lineBufferedStdout == stream {
		return
	}
	if lb, ok := stream.(lineBufferedStream); ok && lb.LineBuffering() {
		lineBufferedStdout = stream
		return
	}
	r, ok := stream.(lineReconfigurer)
	if !ok || r.IsTerminal() {
		return
	}
	if r.Reconfigure(true) == nil {
		lineBufferedStdout = stream
	}
}
