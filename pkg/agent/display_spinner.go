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

	"github.com/mattn/go-isatty"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var kawaiiSpinnerFrames = map[string][]string{
	"dots": strings.Split("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏", ""), "bounce": strings.Split("⠁⠂⠄⡀⢀⠠⠐⠈", ""),
	"grow": strings.Split("▁▂▃▄▅▆▇█▇▆▅▄▃▂", ""), "arrows": strings.Split("←↖↑↗→↘↓↙", ""),
	"star": strings.Split("✶✷✸✹✺✹✸✷", ""), "moon": strings.Split("🌑🌒🌓🌔🌕🌖🌗🌘", ""),
	"pulse": strings.Split("◜◠◝◞◡◟", ""), "brain": strings.Split("🧠💭💡✨💫🌟💡💭", ""),
	"sparkle": strings.Split("⁺˚*✧✦✧*˚", ""),
}

var kawaiiWaiting = []string{
	"(｡◕‿◕｡)", "(◕‿◕✿)", "٩(◕‿◕｡)۶", "(✿◠‿◠)", "( ˘▽˘)っ", "♪(´ε` )", "(◕ᴗ◕✿)", "ヾ(＾∇＾)", "(≧◡≦)", "(★ω★)",
}

var kawaiiThinking = []string{
	"(｡•́︿•̀｡)", "(◔_◔)", "(¬‿¬)", "( •_•)>⌐■-■", "(⌐■_■)", "(´･_･`)", "◉_◉", "(°ロ°)", "( ˘⌣˘)♡", "ヽ(>∀<☆)☆",
	"٩(๑❛ᴗ❛๑)۶", "(⊙_⊙)", "(¬_¬)", "( ͡° ͜ʖ ͡°)", "ಠ_ಠ",
}

var kawaiiThinkingVerbs = []string{
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
		return nil, fmt.Errorf("KeyError: %d", rand.IntN(len(v)))
	}
	return nil, fmt.Errorf("TypeError: object of type '%s' has no len()", config.PyTypeName(value))
}

// Skin waiting faces or the kawaii waiting defaults
func waitingFaces(ctx context.Context) ([]string, error) {
	return skinSpinnerList(ctx, "waiting_faces", kawaiiWaiting)
}

// Skin thinking faces or the kawaii thinking defaults
func thinkingFaces(ctx context.Context) ([]string, error) {
	return skinSpinnerList(ctx, "thinking_faces", kawaiiThinking)
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
		return kawaiiThinkingVerbs
	}
	return verbs
}

// Skin thinking verbs, then the catalog verbs, then the English defaults
func thinkingVerbs(ctx context.Context) ([]string, error) {
	return skinSpinnerList(ctx, "thinking_verbs", catalogThinkingVerbs(ctx))
}

type kawaiiSpinner struct {
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

// Unstarted spinner of the named frame set that writes to print fn or the stdout captured now
func newKawaiiSpinner(ctx context.Context, message, spinnerType string, printFn func(string)) *kawaiiSpinner {
	frames, ok := kawaiiSpinnerFrames[spinnerType]
	if !ok {
		frames = kawaiiSpinnerFrames["dots"]
	}
	return &kawaiiSpinner{ctx: ctx, message: message, frames: frames, printFn: printFn, out: statusStdout}
}

// Started spinner for the call sites that show one while a request or tool runs
func startKawaiiSpinner(ctx context.Context, message, spinnerType string, printFn func(string)) tools.Spinner {
	s := newKawaiiSpinner(ctx, message, spinnerType, printFn)
	s.Start()
	return s
}

// Sends text through print fn swallowing its panics, else writes text and end to the captured stdout
func (s *kawaiiSpinner) write(text, end string, flush bool) {
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

func (s *kawaiiSpinner) isTTY() bool {
	f, ok := s.out.(interface{ Fd() uintptr })
	return ok && isatty.IsTerminal(f.Fd())
}

func (s *kawaiiSpinner) isPatchStdoutProxy() bool {
	_, ok := s.out.(StdoutProxy)
	return ok
}

func (s *kawaiiSpinner) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Sleeps for d or until stop closes
func (s *kawaiiSpinner) pause(stop chan struct{}, d time.Duration) {
	select {
	case <-stop:
	case <-time.After(d):
	}
}

// Draws frames with elapsed time on a terminal, logs one line otherwise, until stopped
func (s *kawaiiSpinner) animate(stop, done chan struct{}) {
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
func (s *kawaiiSpinner) Start() {
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
func (s *kawaiiSpinner) UpdateText(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message = message
}

// Spaces wide enough to blank the last frame, never the erase escape that garbles under patch stdout
func (s *kawaiiSpinner) clearLineBlanks() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Repeat(" ", max(s.lastLineLen+5, 40))
}

// Prints a line above the spinner, clearing the current frame first while it runs
func (s *kawaiiSpinner) PrintAbove(text string) {
	if s.isRunning() {
		s.write("\r"+s.clearLineBlanks()+"\r  "+text, "\n", true)
		return
	}
	s.write("  "+text, "\n", true)
}

// Stops the animation, clears its line on a terminal and prints the final message with elapsed time
func (s *kawaiiSpinner) Stop(finalMessage string) {
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
