package consoleutil

import (
	"strings"
	"sync"
	"time"
)

// Frames of the dots spinner, one every spinnerFrame
var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerFrame = 80 * time.Millisecond

var spinnerStyle = Style{fg: basicColor(2)}

// Spinner frame for the time since start
func spinnerAt(since time.Duration) string {
	return spinnerFrames[int(since/spinnerFrame)%len(spinnerFrames)]
}

// Spinner beside a markup message on a terminal while work runs, erased when stopped, drawing nothing on other writers
type Status struct {
	mu    sync.Mutex
	text  segments
	start time.Time
	live  *liveRegion
}

// Starts a spinner beside the markup on a terminal, every console print to that terminal and, on Unix, whatever reaches the standard output and error descriptors shown above it meanwhile
func (c *Console) Status(markup string) *Status {
	s := &Status{start: time.Now()}
	s.Update(markup)
	l := startLive(c, spinnerFrame, s.frame)
	s.mu.Lock()
	s.live = l
	s.mu.Unlock()
	return s
}

// Replaces the markup beside the spinner
func (s *Status) Update(markup string) {
	t := markupText(strings.ReplaceAll(markup, "\n", " "))
	s.mu.Lock()
	s.text = t
	s.mu.Unlock()
}

func (s *Status) frame(int) []segments {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := segments{{spinnerAt(time.Since(s.start)), spinnerStyle}, {" ", Style{}}}
	return []segments{line.join(s.text)}
}

// Stops the spinner and erases its line, the standard descriptors given back and its goroutines gone when it returns, a second call doing nothing
func (s *Status) Stop() {
	s.mu.Lock()
	l := s.live
	s.live = nil
	s.mu.Unlock()
	if l != nil {
		l.close(false)
	}
}
