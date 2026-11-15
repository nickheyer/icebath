package cliui

import (
	"strings"
	"sync"
	"time"
)

// Spinner drawn beside a label during a silent wait, gone the moment other output or a prompt appears
type busySpinner struct {
	mu      sync.Mutex
	region  *liveRegion
	label   string
	frame   int
	stopped bool
	done    chan struct{}
	release func()
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var spinnerFramesASCII = []string{"|", "/", "-", "\\"}

// Draws the next spinner frame unless the spinner has stopped
func (b *busySpinner) tick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	c, t := b.region.c, activeTheme()
	frames := spinnerFramesASCII
	if c.unicode() {
		frames = spinnerFrames
	}
	mark := frames[b.frame%len(frames)]
	b.frame++
	b.region.paint([]string{strings.Repeat(" ", flowMargin) + c.paint(mark, textStyle{color: t.Accent}) + " " +
		c.paint(b.label, textStyle{color: t.Muted})})
}

// Erases the spinner for good, drops the keys typed while it spun and lets the terminal echo again
func (b *busySpinner) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.stopped = true
	close(b.done)
	b.region.close(nil)
	b.region.c.FlushStdin()
	b.release()
}

// Stops the spinner of a wait in progress so other output starts on a clean line
func (f *flowWriter) stopBusy() {
	f.mu.Lock()
	b := f.busy
	f.busy = nil
	f.mu.Unlock()
	if b != nil {
		b.stop()
	}
}

// Await spins beside the label on a setup terminal, deaf to typed keys, until output, a prompt or the stop
func (c *Console) Await(label string) func() {
	f := c.flow()
	if f == nil {
		return func() {}
	}
	b := &busySpinner{region: c.openRegion(), label: label, done: make(chan struct{}), release: c.holdQuiet()}
	b.region.lead = false
	f.mu.Lock()
	f.busy = b
	f.mu.Unlock()
	go func() {
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			b.tick()
			select {
			case <-ticker.C:
			case <-b.done:
				return
			}
		}
	}()
	return b.stop
}

// How long typed keys stay unechoed after a setup prompt or setup output
const echoGrace = 250 * time.Millisecond

// Silences terminal echo until setup output and prompts have paused, so keys typed ahead of a prompt stay unseen
func (f *flowWriter) grace() {
	f.graceMu.Lock()
	defer f.graceMu.Unlock()
	if f.graceTimer != nil && f.graceTimer.Stop() {
		f.graceTimer.Reset(echoGrace)
		return
	}
	f.graceTimer = time.AfterFunc(echoGrace, f.c.holdQuiet())
}

// Starts the echo silence after a prompt on a setup terminal
func (c *Console) graceQuiet() {
	if f := c.flow(); f != nil {
		f.grace()
	}
}
