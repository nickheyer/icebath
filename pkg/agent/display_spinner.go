package agent

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Frames of the status spinner, the icebath braille wheel
var spinnerFrames = strings.Split("⣾⣽⣻⢿⡿⣟⣯⣷", "")

var defaultThinkingVerbs = []string{
	"pondering", "contemplating", "musing", "cogitating", "ruminating", "deliberating", "mulling",
	"reflecting", "processing", "reasoning", "analyzing", "computing", "synthesizing", "formulating",
	"brainstorming",
}

// Marks a stdout writer that redraws a TUI around each flush so carriage return frames never land
type StdoutProxy interface{ StdoutProxy() }

// Active skin spinner list under key as random choice candidates, fallback when absent or falsy
func skinSpinnerList(ctx context.Context, key string, fallback []string) ([]string, error) {
	skin := config.GetActiveSkin(ctx)
	if skin == nil {
		return fallback, nil
	}
	value, ok := skin.Spinner[key]
	if !ok || !config.PyTruthy(value) {
		return fallback, nil
	}
	switch v := value.(type) {
	case []any:
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = config.PyStr(item)
		}
		return out, nil
	case []string:
		return v, nil
	case string:
		return strings.Split(v, ""), nil
	case map[string]any:
		return nil, fmt.Errorf("skin spinner %s is a mapping, expected a list", key)
	}
	return nil, fmt.Errorf("skin spinner %s is %s, expected a list", key, errchain.Kind(value))
}

// Faces a skin puts ahead of a waiting label, none unless the skin authors them
func waitingFaces(ctx context.Context) ([]string, error) {
	return skinSpinnerList(ctx, "waiting_faces", nil)
}

// Faces a skin puts ahead of a thinking verb, none unless the skin authors them
func thinkingFaces(ctx context.Context) ([]string, error) {
	return skinSpinnerList(ctx, "thinking_faces", nil)
}

// Text behind a random face of the list, the bare text when the list is empty
func faced(faces []string, text string) string {
	if len(faces) == 0 {
		return text
	}
	return faces[rand.IntN(len(faces))] + " " + text
}

// Comma separated catalog verbs for the active language, English defaults when the catalog lacks them
func catalogThinkingVerbs(ctx context.Context) []string {
	var verbs []string
	for _, v := range strings.Split(i18n.T(ctx, "display.thinking_verbs", "", nil), ",") {
		if s := pystr.Strip(v); s != "" {
			verbs = append(verbs, s)
		}
	}
	if len(verbs) == 0 || (len(verbs) == 1 && verbs[0] == "display.thinking_verbs") {
		return defaultThinkingVerbs
	}
	return verbs
}

// Skin thinking verbs, then the catalog verbs, then the English defaults
func thinkingVerbs(ctx context.Context) ([]string, error) {
	return skinSpinnerList(ctx, "thinking_verbs", catalogThinkingVerbs(ctx))
}

type statusSpinner struct {
	ctx         context.Context
	mu          sync.Mutex
	message     string
	frames      []string
	running     bool
	stopCh      chan struct{}
	done        chan struct{}
	startTime   time.Time
	frameIdx    int
	lastLineLen int
	printFn     func(string)
	out         io.Writer
}

// Unstarted spinner that writes to print fn or the stdout ctx sees, captured now
func newStatusSpinner(ctx context.Context, message string, printFn func(string)) *statusSpinner {
	return &statusSpinner{ctx: ctx, message: message, frames: spinnerFrames, printFn: printFn, out: scopedStatusStdout(ctx)}
}

// Started spinner for the call sites that show one while a request or tool runs
func startStatusSpinner(ctx context.Context, message string, printFn func(string)) tools.Spinner {
	s := newStatusSpinner(ctx, message, printFn)
	s.Start()
	return s
}

// Sends text through print fn swallowing its panics, else writes text and end to the captured stdout
func (s *statusSpinner) write(text, end string, flush bool) {
	if s.printFn != nil {
		func() {
			defer func() { _ = recover() }()
			s.printFn(text)
		}()
		return
	}
	if _, err := io.WriteString(s.out, text+end); err != nil {
		return
	}
	if flush {
		if f, ok := s.out.(interface{ Flush() error }); ok {
			_ = f.Flush()
		}
	}
}

func (s *statusSpinner) isTTY() bool { return stdoutIsTTY(s.out) }

func (s *statusSpinner) isPatchStdoutProxy() bool {
	_, ok := s.out.(StdoutProxy)
	return ok
}

func (s *statusSpinner) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Sleeps for d or until stop closes
func (s *statusSpinner) pause(stop chan struct{}, d time.Duration) {
	select {
	case <-stop:
	case <-time.After(d):
	}
}

// Draws frames with elapsed time on a terminal, logs one line otherwise, until stopped
func (s *statusSpinner) animate(stop, done chan struct{}) {
	defer close(done)
	tty := s.isTTY()
	if !tty {
		s.mu.Lock()
		message := s.message
		s.mu.Unlock()
		s.write(i18n.T(s.ctx, "display.spinner.tool_line", "", map[string]any{"message": message}), "\n", true)
	}
	if !tty || s.isPatchStdoutProxy() {
		interval := 100 * time.Millisecond
		if !tty {
			interval = 500 * time.Millisecond
		}
		for s.isRunning() {
			s.pause(stop, interval)
		}
		return
	}
	var wings [][2]string
	if skin := config.GetActiveSkin(s.ctx); skin != nil {
		wings = skin.GetSpinnerWings()
	}
	for s.isRunning() {
		if os.Getenv("ICEBATH_SPINNER_PAUSE") != "" {
			s.pause(stop, 100*time.Millisecond)
			continue
		}
		s.mu.Lock()
		frame := s.frames[s.frameIdx%len(s.frames)]
		elapsed := fmt.Sprintf("(%.1fs)", time.Since(s.startTime).Seconds())
		line := "  " + frame + " " + s.message + " " + elapsed
		if len(wings) > 0 {
			w := wings[s.frameIdx%len(wings)]
			line = "  " + w[0] + " " + frame + " " + s.message + " " + w[1] + " " + elapsed
		}
		width := utf8.RuneCountInString(line)
		pad := strings.Repeat(" ", max(s.lastLineLen-width, 0))
		s.mu.Unlock()
		s.write("\r"+line+pad, "", true)
		s.mu.Lock()
		s.lastLineLen = width
		s.frameIdx++
		s.mu.Unlock()
		s.pause(stop, 120*time.Millisecond)
	}
}

// Starts the animation goroutine unless one already runs
func (s *statusSpinner) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running, s.startTime = true, time.Now()
	s.stopCh, s.done = make(chan struct{}), make(chan struct{})
	stop, done := s.stopCh, s.done
	s.mu.Unlock()
	go s.animate(stop, done)
}

// Replaces the message the next frame draws
func (s *statusSpinner) UpdateText(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message = message
}

// Spaces wide enough to blank the last frame, never the erase escape that garbles under patch stdout
func (s *statusSpinner) clearLineBlanks() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Repeat(" ", max(s.lastLineLen+5, 40))
}

// Prints a line above the spinner, clearing the current frame first while it runs
func (s *statusSpinner) PrintAbove(text string) {
	if s.isRunning() {
		s.write("\r"+s.clearLineBlanks()+"\r  "+text, "\n", true)
		return
	}
	s.write("  "+text, "\n", true)
}

// Stops the animation, clears its line on a terminal and prints the final message with elapsed time
func (s *statusSpinner) Stop(finalMessage string) {
	s.mu.Lock()
	s.running = false
	stop, done, started := s.stopCh, s.done, s.startTime
	s.stopCh = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}
	isTTY := s.isTTY()
	if isTTY {
		s.write("\r"+s.clearLineBlanks()+"\r", "", true)
	}
	if finalMessage == "" {
		return
	}
	if isTTY {
		s.write("  "+finalMessage, "\n", true)
		return
	}
	elapsed := ""
	if !started.IsZero() {
		elapsed = fmt.Sprintf(" (%.1fs)", time.Since(started).Seconds())
	}
	s.write(i18n.T(s.ctx, "display.spinner.done_line", "", map[string]any{"message": finalMessage, "elapsed": elapsed}), "\n", true)
}
