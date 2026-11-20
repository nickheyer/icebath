package tui

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/ui/tty"
)

// Frames the rate of the readout is averaged over
const frameMeterWindow = 30

// A frame reached the terminal while the readout is on, the model drawn again so the readout shows the rate the UI can hold
type frameShownMsg struct{}

// Listener of the renderer's frame clock behind the frame rate readout and the frame timing log, each on when its environment switch asks
type frameMeter struct {
	mu     sync.Mutex
	shown  bool
	stamps []time.Time
	total  uint64
	rate   float64
	last   time.Duration
	post   func(tea.Msg)
	// Frames faster than this stay out of the timing log
	floor   time.Duration
	logging bool
	logPath string
	log     *os.File
}

// Frame meter the environment asks for, nil when it asks for neither the readout (ICEBATH_TUI_FPS) nor the timing log (ICEBATH_DEV_PERF)
func newFrameMeter() *frameMeter {
	on := func(name string) bool { return trueRe.MatchString(strings.ToLower(strings.TrimSpace(os.Getenv(name)))) }
	m := &frameMeter{shown: on("ICEBATH_TUI_FPS"), logging: on("ICEBATH_DEV_PERF"), floor: 2 * time.Millisecond}
	if !m.shown && !m.logging {
		return nil
	}
	if raw, set := os.LookupEnv("ICEBATH_DEV_PERF_MS"); set {
		// Anything that is no number captures every frame, as a threshold of zero does
		ms, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || math.IsNaN(ms) || math.IsInf(ms, 0) {
			ms = 0
		}
		m.floor = time.Duration(ms * float64(time.Millisecond))
	}
	if m.logPath = strings.TrimSpace(os.Getenv("ICEBATH_DEV_PERF_LOG")); m.logPath == "" {
		m.logPath = filepath.Join(home.Process(), "perf.log")
	}
	return m
}

// Function the renderer tells every frame, nil without a meter so the renderer measures nothing
func (m *frameMeter) listener() func(tty.FrameStat) {
	if m == nil {
		return nil
	}
	return m.frame
}

// Takes the timing of one written frame into the readout and the log
func (m *frameMeter) frame(stat tty.FrameStat) {
	m.mu.Lock()
	m.stamps = append(m.stamps, stat.At)
	if len(m.stamps) > frameMeterWindow {
		m.stamps = m.stamps[len(m.stamps)-frameMeterWindow:]
	}
	m.total++
	if n := len(m.stamps); n >= 2 {
		if elapsed := m.stamps[n-1].Sub(m.stamps[0]).Seconds(); elapsed > 0 {
			m.rate, m.last = math.Round(float64(n-1)/elapsed*10)/10, stat.Total()
		}
	}
	if m.logging && stat.Total() >= m.floor {
		m.writeRow(stat)
	}
	shown, post := m.shown, m.post
	m.mu.Unlock()
	if shown && post != nil {
		post(frameShownMsg{})
	}
}

// Appends the timing of a frame to the log as one JSON line, a log that cannot be written never disturbing the UI
func (m *frameMeter) writeRow(stat tty.FrameStat) {
	if m.log == nil {
		if err := os.MkdirAll(filepath.Dir(m.logPath), 0o755); err != nil {
			m.logging = false
			return
		}
		file, err := os.OpenFile(m.logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			m.logging = false
			return
		}
		m.log = file
	}
	ms := func(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Millisecond)*100) / 100 }
	row, err := json.Marshal(map[string]any{
		"src": "frame", "ts": stat.At.UnixMilli(), "durationMs": ms(stat.Total()), "bytes": stat.Bytes,
		"phases": map[string]float64{"view": ms(stat.View), "compose": ms(stat.Compose), "write": ms(stat.Write)},
	})
	if err == nil {
		_, _ = m.log.Write(append(row, '\n'))
	}
}

// Closes the timing log
func (m *frameMeter) close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.log != nil {
		_ = m.log.Close()
		m.log, m.logging = nil, false
	}
}

// Model the program runs, the app itself or the app under the frame rate readout when the environment asks for it
func (m *frameMeter) wrap(a *app) tea.Model {
	if m == nil || !m.shown {
		return a
	}
	return &meteredModel{app: a, meter: m}
}

// App with the frame rate readout drawn over the bottom right corner of its view
type meteredModel struct {
	app   *app
	meter *frameMeter
	cols  int
}

func (m *meteredModel) Init() tea.Cmd { return m.app.Init() }

func (m *meteredModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch size := msg.(type) {
	case frameShownMsg:
		return m, nil
	case tea.WindowSizeMsg:
		m.cols = size.Width
	}
	_, cmd := m.app.Update(msg)
	return m, cmd
}

func (m *meteredModel) View() tea.View {
	v := m.app.View()
	m.meter.mu.Lock()
	rate, last, total := m.meter.rate, m.meter.last, m.meter.total
	m.meter.mu.Unlock()
	// Fixed widths keep the corner from jittering as the digits change
	text := fmt.Sprintf("%5.1ffps · %5.1fms · #%d", rate, float64(last)/float64(time.Millisecond), total)
	colors := m.app.store.ui.Theme.Color
	tone := colors.Error
	switch {
	case rate >= 50:
		tone = colors.StatusGood
	case rate >= 30:
		tone = colors.StatusWarn
	}
	lines := strings.Split(v.Content, "\n")
	row := len(lines) - 1
	if lines[row] == "" && row > 0 {
		row--
	}
	cols := m.cols
	if cols <= 0 {
		cols = ansi.StringWidth(lines[row])
	}
	// One blank column stays between the readout and the edge of the screen when the screen has room for it
	room, edge := max(cols-ansi.StringWidth(text)-1, 0), ""
	if cols > ansi.StringWidth(text) {
		edge = " "
	}
	left := ansi.Truncate(lines[row], room, "")
	lines[row] = left + "\x1b[m" + strings.Repeat(" ", room-ansi.StringWidth(left)) + toneStyle(tone).Render(ansi.Truncate(text, cols, "")) + edge
	v.Content = strings.Join(lines, "\n")
	return v
}
