package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

// Synchronous sink writing formatted records to a stream on the logging goroutine
type console struct {
	w       io.Writer
	level   slog.Level
	layout  Layout
	verbose bool
	mu      sync.Mutex
}

// Writes rec redacted under the redaction policy of the caller context
func (c *console) write(s *System, ctx context.Context, rec *record) {
	line := s.redact.Redact(ctx, c.layout.line(rec))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.w, line+"\n"); err != nil {
		reportError(line, err)
	}
}

// Adds a stream sink taking records at or above level in layout, lowering the root level so they reach it
func (s *System) AddConsole(w io.Writer, level slog.Level, layout Layout) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addConsoleLocked(&console{w: w, level: level, layout: layout})
}

// Appends a console and lowers the root level to its level, caller holding mu
func (s *System) addConsoleLocked(c *console) {
	s.consoles = append(s.consoles, c)
	if c.level < s.root {
		s.root = c.level
	}
	s.publish()
}

// Adds the debug console on stderr once with third-party sources kept at warning
func (s *System) EnableVerbose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.consoles {
		if c.verbose {
			return
		}
	}
	s.quietNoisyLocked()
	s.addConsoleLocked(&console{w: SafeStderr(), level: slog.LevelDebug, layout: VerboseLayout, verbose: true})
}

// Third-party sources whose chatter stays out of the logs below warning, named as the bridges into slog name them
var noisySources = [...]string{"grpc", "modal"}

// Pins the noisy third-party sources at warning, caller holding mu
func (s *System) quietNoisyLocked() {
	for _, name := range noisySources {
		s.levels[name] = slog.LevelWarn
	}
}

// Writer onto the stderr of the moment that sends valid UTF-8, invalid bytes escaped or replaced as the stream allows
func SafeStderr() io.Writer { return safeStderr{} }

// Stderr writer the consoles and logging notices use
type safeStderr struct{}

// Writes p with invalid UTF-8 escaped on a UTF-8 stream and replaced by question marks on any other
func (safeStderr) Write(p []byte) (int, error) {
	out := os.Stderr
	if utf8.Valid(p) {
		if _, err := out.Write(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	escape := stderrIsUTF8(out)
	var b strings.Builder
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRune(p[i:])
		switch {
		case r == utf8.RuneError && size <= 1 && escape:
			fmt.Fprintf(&b, `\udc%02x`, p[i])
		case r == utf8.RuneError && size <= 1:
			b.WriteByte('?')
		default:
			b.Write(p[i : i+size])
		}
		i += max(size, 1)
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}
