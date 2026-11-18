// Package setupflow is the question surface a setup flow on the gateway asks the terminal UI through
package setupflow

import (
	"errors"
	"io"
	"strings"
	"sync"
)

// Option is one row of a single choice
type Option struct {
	Title  string
	Detail string
}

// Item is one row of a multi choice, the tools it brings feeding the context estimate
type Item struct {
	Title  string
	Detail string
	Tools  []string
}

// Asker is what a setup flow prints and asks through, every question blocking until the user answers
type Asker interface {
	Say(line string)
	Choose(question string, options []Option, def int) (int, error)
	Check(question string, items []Item, selected map[int]bool, tokens map[string]int) (map[int]bool, error)
	Text(question, def string, secret bool) (string, error)
}

// ErrCancelled ends a flow the user left
var ErrCancelled = errors.New("setup cancelled")

// Writer that says each finished line through an asker, holding back a partial one until it ends
type lineWriter struct {
	mu      sync.Mutex
	ask     Asker
	partial string
}

// Lines is a writer whose output the asker says line by line
func Lines(ask Asker) io.Writer { return &lineWriter{ask: ask} }

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial += string(p)
	for {
		line, rest, whole := strings.Cut(w.partial, "\n")
		if !whole {
			return len(p), nil
		}
		w.partial = rest
		w.ask.Say(line)
	}
}
