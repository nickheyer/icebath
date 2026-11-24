package consoleutil

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/colorprofile"
)

// Window of advances the speed of a task is measured over
const speedWindow = 30 * time.Second

// Most advances kept for the speed estimate
const maxSamples = 1000

var (
	barComplete  = Style{fg: rgbColor(249, 38, 114)}
	barFinished  = Style{fg: rgbColor(114, 156, 31)}
	barBack      = Style{fg: paletteColor(237)}
	percentStyle = Style{fg: basicColor(5)}
	elapsedStyle = Style{fg: basicColor(3)}
	leftStyle    = Style{fg: basicColor(6)}
)

// What a progress column knows about one task when it draws
type Task struct {
	ID          int
	Description string
	// Steps done so far
	Completed int
	// Steps in all, below zero while unknown
	Total int
	// Time since the task was added, frozen once it finished
	Elapsed time.Duration
	// Whether the steps done reached the total
	Finished bool
}

type advance struct {
	at    time.Time
	steps int
}

type task struct {
	id          int
	description string
	total       int
	completed   int
	start       time.Time
	finished    bool
	took        time.Duration
	samples     []advance
}

func (t *task) elapsed(now time.Time) time.Duration {
	if t.finished {
		return t.took
	}
	return now.Sub(t.start)
}

// Records the finish once the steps done reach the total
func (t *task) markFinished(now time.Time) {
	if t.total >= 0 && t.completed >= t.total && !t.finished {
		t.finished, t.took = true, now.Sub(t.start)
	}
}

// Steps a second over the sample window, false while it cannot be measured
func (t *task) speed() (float64, bool) {
	if len(t.samples) < 2 {
		return 0, false
	}
	span := t.samples[len(t.samples)-1].at.Sub(t.samples[0].at).Seconds()
	if span <= 0 {
		return 0, false
	}
	steps := 0
	for _, s := range t.samples[1:] {
		steps += s.steps
	}
	return float64(steps) / span, true
}

// Whole seconds left at the measured speed, zero once finished, false while unknown
func (t *task) remaining() (time.Duration, bool) {
	if t.finished {
		return 0, true
	}
	sp, ok := t.speed()
	if !ok || sp <= 0 || t.total < 0 {
		return 0, false
	}
	secs := math.Ceil(float64(t.total-t.completed) / sp)
	if secs >= float64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64), true
	}
	return time.Duration(secs) * time.Second, true
}

// Percentage done from 0 to 100, a task of no steps being done
func (t *task) percent() float64 {
	if t.total <= 0 {
		if t.total == 0 {
			return 100
		}
		return 0
	}
	return math.Min(100, math.Max(0, float64(t.completed)/float64(t.total)*100))
}

func (t *task) snapshot(now time.Time) Task {
	return Task{ID: t.id, Description: t.description, Completed: t.completed, Total: t.total, Elapsed: t.elapsed(now), Finished: t.finished}
}

// Moment and settings one progress frame draws with
type progressFrame struct {
	now, started time.Time
	// whether the console shows colors, the bar's back part and the pulse gradient drawn only then
	colors bool
	// cells a bar takes
	width int
}

// One cell of each task row of a progress display
type ProgressColumn interface {
	cell(f progressFrame, t *task) segments
}

// Dots spinner, a blank once the task finishes
type SpinnerColumn struct{}

func (SpinnerColumn) cell(f progressFrame, t *task) segments {
	if t.finished {
		return segments{{" ", Style{}}}
	}
	return segments{{spinnerAt(f.now.Sub(f.started)), spinnerStyle}}
}

// Fixed markup, the same on every row
type TextColumn string

func (c TextColumn) cell(progressFrame, *task) segments { return markupText(string(c)) }

// Markup description of the task
type DescriptionColumn struct{}

func (DescriptionColumn) cell(_ progressFrame, t *task) segments { return markupText(t.description) }

// Markup a function builds from each task
type TaskColumn func(t Task) string

func (c TaskColumn) cell(f progressFrame, t *task) segments { return markupText(c(t.snapshot(f.now))) }

// Bar of Width cells, forty when zero, filling as steps are done and pulsing while the total is unknown
type BarColumn struct{ Width int }

func (c BarColumn) natural() int {
	if c.Width <= 0 {
		return 40
	}
	return c.Width
}

func (c BarColumn) cell(f progressFrame, t *task) segments {
	width := f.width
	if width <= 0 {
		width = c.natural()
	}
	if t.total < 0 {
		return pulse(f, width)
	}
	halves := width * 2
	if t.total > 0 {
		done := min(t.total, max(0, t.completed))
		halves = int(float64(width) * 2 * float64(done) / float64(t.total))
	}
	bars, half := halves/2, halves%2
	st := barComplete
	if t.completed >= t.total {
		st = barFinished
	}
	var out segments
	out = out.with(strings.Repeat("━", bars), st)
	if half > 0 {
		out = out.with("╸", st)
	}
	if f.colors {
		rest := width - bars - half
		if rest > 0 && half == 0 && bars > 0 {
			out = out.with("╺", barBack)
			rest--
		}
		out = out.with(strings.Repeat("━", max(0, rest)), barBack)
	}
	return out
}

// Pulse segments sliding with time, a cosine blend of the bar and back colors when colors show, half bar and half blank otherwise
func pulse(f progressFrame, width int) segments {
	const size = 20
	var cycle [size]segment
	for i := range size {
		switch {
		case f.colors:
			fade := 0.5 + math.Cos(float64(i)/size*math.Pi*2)/2
			blend := func(a, b float64) uint8 { return uint8(a + (b-a)*fade) }
			cycle[i] = segment{"━", Style{fg: rgbColor(blend(249, 58), blend(38, 58), blend(114, 58))}}
		case i < size/2:
			cycle[i] = segment{"━", barComplete}
		default:
			cycle[i] = segment{" ", Style{}}
		}
	}
	offset := int(-f.now.Sub(f.started).Seconds()*15) % size
	if offset < 0 {
		offset += size
	}
	out := make(segments, width)
	for k := range width {
		out[k] = cycle[(offset+k)%size]
	}
	return out
}

// Right aligned percentage done, empty while the total is unknown
type TaskProgressColumn struct{}

func (TaskProgressColumn) cell(_ progressFrame, t *task) segments {
	if t.total < 0 {
		return nil
	}
	return segments{{fmt.Sprintf("%3.0f%%", t.percent()), percentStyle}}
}

// Hours, minutes and seconds of a duration as H:MM:SS
func clock(d time.Duration) string {
	d = max(0, d)
	return fmt.Sprintf("%d:%02d:%02d", int(d/time.Hour), int(d/time.Minute)%60, int(d/time.Second)%60)
}

// Time since the task was added, frozen at its finish
type TimeElapsedColumn struct{}

func (TimeElapsedColumn) cell(f progressFrame, t *task) segments {
	return segments{{clock(t.elapsed(f.now)), elapsedStyle}}
}

// Estimated time left, dashes while the speed is unknown, empty while the total is
type TimeRemainingColumn struct{}

func (TimeRemainingColumn) cell(_ progressFrame, t *task) segments {
	if t.total < 0 {
		return nil
	}
	left, ok := t.remaining()
	if !ok {
		return segments{{"-:--:--", leftStyle}}
	}
	return segments{{clock(left), leftStyle}}
}

// Live display of task rows redrawn in place on a terminal, the final rows printed once on any other writer
type Progress struct {
	c       *Console
	columns []ProgressColumn
	refresh time.Duration

	mu      sync.Mutex
	tasks   []*task
	nextID  int
	started time.Time

	state   sync.Mutex
	running bool
	live    *liveRegion
}

// Progress display over the console redrawn every refresh, a tenth of a second when not positive
func (c *Console) NewProgress(refresh time.Duration, columns ...ProgressColumn) *Progress {
	if refresh <= 0 {
		refresh = 100 * time.Millisecond
	}
	return &Progress{c: c, columns: columns, refresh: refresh, started: time.Now()}
}

func (p *Progress) find(id int) *task {
	for _, t := range p.tasks {
		if t.id == id {
			return t
		}
	}
	return nil
}

// Adds a running task of total steps, below zero for an unknown total, and returns its id
func (p *Progress) AddTask(description string, total int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	t := &task{id: p.nextID, description: description, total: total, start: now}
	p.nextID++
	t.markFinished(now)
	p.tasks = append(p.tasks, t)
	return t.id
}

// Advances a task by n steps, an unknown id doing nothing
func (p *Progress) Advance(id int, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.find(id)
	if t == nil {
		return
	}
	now := time.Now()
	t.completed += n
	cut := 0
	for cut < len(t.samples) && now.Sub(t.samples[cut].at) > speedWindow {
		cut++
	}
	cut = max(cut, len(t.samples)+1-maxSamples)
	t.samples = append(t.samples[cut:], advance{now, n})
	t.markFinished(now)
}

// Replaces the markup description of a task, an unknown id doing nothing
func (p *Progress) Update(id int, description string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.find(id); t != nil {
		t.description = description
	}
}

// Removes a task row, an unknown id doing nothing
func (p *Progress) RemoveTask(id int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, t := range p.tasks {
		if t.id == id {
			p.tasks = append(p.tasks[:i:i], p.tasks[i+1:]...)
			return
		}
	}
}

// Rows of every task at the moment, columns set one cell apart, bars and then the widest text narrowed to fit width cells
func (p *Progress) rows(width int) []segments {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tasks) == 0 || len(p.columns) == 0 {
		return nil
	}
	f := progressFrame{now: time.Now(), started: p.started, colors: p.c.profile() > colorprofile.ASCII}
	n := len(p.columns)
	cells := make([][]segments, len(p.tasks))
	widths := make([]int, n)
	bar := make([]bool, n)
	for i, col := range p.columns {
		if b, ok := col.(interface{ natural() int }); ok {
			bar[i], widths[i] = true, b.natural()
		}
	}
	for ti, t := range p.tasks {
		cells[ti] = make([]segments, n)
		for i, col := range p.columns {
			if bar[i] {
				continue
			}
			c := flatten(col.cell(f, t))
			cells[ti][i] = c
			widths[i] = max(widths[i], c.width())
		}
	}
	excess := sum(widths) + n - 1 - width
	for i := range n {
		if bar[i] && excess > 0 {
			cut := min(widths[i]-1, excess)
			widths[i] -= cut
			excess -= cut
		}
	}
	for excess > 0 {
		widest := -1
		for i := range n {
			if !bar[i] && widths[i] > 1 && (widest < 0 || widths[i] > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		widths[widest]--
		excess--
	}
	out := make([]segments, len(p.tasks))
	for ti, t := range p.tasks {
		var line segments
		for i, col := range p.columns {
			c := cells[ti][i]
			if bar[i] {
				bf := f
				bf.width = widths[i]
				c = col.cell(bf, t)
			}
			if i > 0 {
				line = line.with(" ", Style{})
			}
			line = line.join(c.truncate(widths[i], "ellipsis", true, Style{}))
		}
		out[ti] = line
	}
	return out
}

// Text on one line, newlines turned to spaces
func flatten(t segments) segments {
	for i, s := range t {
		if strings.IndexByte(s.text, '\n') >= 0 {
			out := make(segments, len(t))
			copy(out, t)
			for k := i; k < len(out); k++ {
				out[k].text = strings.ReplaceAll(out[k].text, "\n", " ")
			}
			return out
		}
	}
	return t
}

// Starts the display: redrawn in place on a terminal, every console print to that terminal and, on Unix, whatever reaches the standard output and error descriptors shown above it whole lines at a time; nothing drawn on any other writer until Stop
func (p *Progress) Start() {
	p.state.Lock()
	defer p.state.Unlock()
	if p.running || p.c.Quiet {
		return
	}
	p.running = true
	p.mu.Lock()
	p.started = time.Now()
	p.mu.Unlock()
	p.live = startLive(p.c, p.refresh, p.rows)
}

// Stops the display after a last frame left on screen, the final rows printed once when nothing was drawn live; when it returns the standard descriptors are back, what was written to them has been printed and its goroutines and descriptors are gone
func (p *Progress) Stop() {
	p.state.Lock()
	defer p.state.Unlock()
	if !p.running {
		return
	}
	p.running = false
	if p.live != nil {
		p.live.close(true)
		p.live = nil
		return
	}
	if p.c.Quiet {
		return
	}
	lines := p.rows(p.c.width())
	if p.c.Capture != nil {
		p.c.capture(plainLines(lines))
		return
	}
	if p.c.Out != nil && len(lines) > 0 {
		pr := p.c.profile()
		p.c.write(lines, pr, pr > colorprofile.NoTTY && !p.c.NoLinks)
	}
}
