package pyrich

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var progressEpoch = time.Now()

// Seconds on the monotonic clock progress timings are measured against
func progressNow() float64 { return time.Since(progressEpoch).Seconds() }

const (
	progressSpeedPeriod = 30.0
	pulseSize           = 20
)

type progressSample struct{ timestamp, completed float64 }

// ProgressTask is one bar of a Progress display with its timing samples
type ProgressTask struct {
	ID          int
	Description string
	// Total steps, nil for an indeterminate pulsing task
	Total        *float64
	Completed    float64
	startTime    float64
	finishedTime *float64
	samples      []progressSample
}

func (t *ProgressTask) finished() bool { return t.finishedTime != nil }

func (t *ProgressTask) elapsed(now float64) float64 { return now - t.startTime }

// Percentage complete clamped to 0 through 100, zero without a total
func (t *ProgressTask) percentage() float64 {
	if t.Total == nil || *t.Total == 0 {
		return 0
	}
	return math.Min(100, math.Max(0, t.Completed / *t.Total * 100))
}

// Steps per second over the sample window, nil when it cannot be measured
func (t *ProgressTask) speed() *float64 {
	if len(t.samples) == 0 {
		return nil
	}
	total := t.samples[len(t.samples)-1].timestamp - t.samples[0].timestamp
	if total == 0 {
		return nil
	}
	sum := 0.0
	for _, s := range t.samples[1:] {
		sum += s.completed
	}
	v := sum / total
	return &v
}

// Whole seconds left at the measured speed, zero once finished and nil when unknown
func (t *ProgressTask) timeRemaining() *float64 {
	if t.finished() {
		v := 0.0
		return &v
	}
	sp := t.speed()
	if sp == nil || *sp == 0 || t.Total == nil {
		return nil
	}
	v := math.Ceil((*t.Total - t.Completed) / *sp)
	return &v
}

// ProgressColumn renders one cell of a task row as markup
type ProgressColumn interface {
	cell(p *Progress, t *ProgressTask, now float64) string
}

// SpinnerColumn is the dots spinner, a blank once the task finishes
type SpinnerColumn struct{ start *float64 }

func (s *SpinnerColumn) cell(_ *Progress, t *ProgressTask, now float64) string {
	if t.finished() {
		return " "
	}
	if s.start == nil {
		v := now
		s.start = &v
	}
	frame := int((now-*s.start)/0.08) % len(spinnerFrames)
	return "[progress.spinner]" + spinnerFrames[frame] + "[/]"
}

// TextColumn is a str.format template over task fields rendered as markup
type TextColumn struct{ Format string }

func (c TextColumn) cell(_ *Progress, t *ProgressTask, now float64) string {
	return formatTaskTemplate(c.Format, t, now)
}

// Python value of a task attribute a template names
func taskField(t *ProgressTask, name string, now float64) any {
	num := func(v float64) any {
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return int64(v)
		}
		return v
	}
	opt := func(v *float64) any {
		if v == nil {
			return nil
		}
		return num(*v)
	}
	switch name {
	case "description":
		return t.Description
	case "completed":
		return num(t.Completed)
	case "total":
		return opt(t.Total)
	case "percentage":
		return t.percentage()
	case "id":
		return int64(t.ID)
	case "elapsed":
		return t.elapsed(now)
	case "finished":
		return t.finished()
	case "speed":
		if sp := t.speed(); sp != nil {
			return *sp
		}
		return nil
	case "time_remaining":
		return opt(t.timeRemaining())
	case "remaining":
		if t.Total == nil {
			return nil
		}
		return num(*t.Total - t.Completed)
	}
	panic(fmt.Sprintf("AttributeError: 'Task' object has no attribute '%s'", name))
}

// Template text with brace escapes undone and each task field formatted by its spec
func formatTaskTemplate(format string, t *ProgressTask, now float64) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		ch := format[i]
		if ch == '}' && i+1 < len(format) && format[i+1] == '}' {
			b.WriteByte('}')
			i++
			continue
		}
		if ch != '{' {
			b.WriteByte(ch)
			continue
		}
		if i+1 < len(format) && format[i+1] == '{' {
			b.WriteByte('{')
			i++
			continue
		}
		end := strings.IndexByte(format[i:], '}')
		if end < 0 {
			panic("ValueError: Single '{' encountered in format string")
		}
		field := format[i+1 : i+end]
		spec := ""
		if k := strings.IndexByte(field, ':'); k >= 0 {
			field, spec = field[:k], field[k+1:]
		}
		name, ok := strings.CutPrefix(field, "task.")
		if !ok {
			panic(fmt.Sprintf("KeyError: '%s'", field))
		}
		out, err := pyrepr.Format(taskField(t, name, now), spec)
		if err != nil {
			panic(err)
		}
		b.WriteString(out)
		i += end
	}
	return b.String()
}

// BarColumn is a bar of Width cells, forty by default, pulsing while the total is unknown
type BarColumn struct{ Width int }

func (c BarColumn) naturalWidth() int {
	if c.Width <= 0 {
		return 40
	}
	return c.Width
}

func (c BarColumn) cell(p *Progress, t *ProgressTask, now float64) string {
	return c.cellAt(p, t, now, c.naturalWidth())
}

// Bar drawn at width cells, the column width the table fit leaves it
func (c BarColumn) cellAt(p *Progress, t *ProgressTask, now float64, width int) string {
	if t.Total == nil {
		return p.pulse(width, now)
	}
	completed := math.Min(*t.Total, math.Max(0, t.Completed))
	halves := width * 2
	if *t.Total != 0 {
		halves = int(float64(width) * 2 * completed / *t.Total)
	}
	bars, half := halves/2, halves%2
	style := "bar.complete"
	if t.Completed >= *t.Total {
		style = "bar.finished"
	}
	var b strings.Builder
	if bars > 0 {
		b.WriteString("[" + style + "]" + strings.Repeat("━", bars) + "[/]")
	}
	if half > 0 {
		b.WriteString("[" + style + "]╸[/]")
	}
	if !p.c.NoColor && p.c.Color {
		remaining := width - bars - half
		if remaining > 0 && half == 0 && bars > 0 {
			b.WriteString("[bar.back]╺[/]")
			remaining--
		}
		if remaining > 0 {
			b.WriteString("[bar.back]" + strings.Repeat("━", remaining) + "[/]")
		}
	}
	return b.String()
}

// Pulse segments sliding with time, a cosine blend of the pulse and back colors on a color console
func (p *Progress) pulse(width int, now float64) string {
	var segs []string
	if !p.c.Color || p.c.NoColor {
		back := "━"
		if p.c.NoColor {
			back = " "
		}
		for i := 0; i < pulseSize; i++ {
			if i < pulseSize/2 {
				segs = append(segs, "[bar.pulse]━[/]")
			} else {
				segs = append(segs, "[bar.back]"+back+"[/]")
			}
		}
	} else {
		fore, back := [3]float64{249, 38, 114}, [3]float64{58, 58, 58}
		for i := 0; i < pulseSize; i++ {
			fade := 0.5 + math.Cos(float64(i)/pulseSize*math.Pi*2)/2
			var rgb [3]int
			for k := range rgb {
				rgb[k] = int(fore[k] + (back[k]-fore[k])*fade)
			}
			segs = append(segs, fmt.Sprintf("[rgb(%d,%d,%d)]━[/]", rgb[0], rgb[1], rgb[2]))
		}
	}
	all := make([]string, 0, len(segs)*(width/len(segs)+2))
	for i := 0; i < width/len(segs)+2; i++ {
		all = append(all, segs...)
	}
	offset := int(-now*15) % len(segs)
	if offset < 0 {
		offset += len(segs)
	}
	return strings.Join(all[offset:offset+width], "")
}

// TaskProgressColumn is the right aligned percentage, empty for a task without a total
type TaskProgressColumn struct{}

func (TaskProgressColumn) cell(_ *Progress, t *ProgressTask, now float64) string {
	if t.Total == nil {
		return ""
	}
	return formatTaskTemplate("[progress.percentage]{task.percentage:>3.0f}%", t, now)
}

// Text of timedelta(seconds=n) as str renders it
func timedeltaStr(n int) string {
	days, rem := n/86400, n%86400
	clock := fmt.Sprintf("%d:%02d:%02d", rem/3600, rem%3600/60, rem%60)
	if days == 0 {
		return clock
	}
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	return strconv.Itoa(days) + " " + unit + ", " + clock
}

// TimeElapsedColumn is the elapsed time, frozen at the finish time once done
type TimeElapsedColumn struct{}

func (TimeElapsedColumn) cell(_ *Progress, t *ProgressTask, now float64) string {
	elapsed := t.elapsed(now)
	if t.finished() {
		elapsed = *t.finishedTime
	}
	return "[progress.elapsed]" + timedeltaStr(int(math.Max(0, elapsed))) + "[/]"
}

// TimeRemainingColumn is the estimated time left, dashes while the speed is unknown
type TimeRemainingColumn struct{}

func (TimeRemainingColumn) cell(_ *Progress, t *ProgressTask, _ float64) string {
	if t.Total == nil {
		return ""
	}
	left := t.timeRemaining()
	if left == nil {
		return "[progress.remaining]-:--:--[/]"
	}
	secs := int(*left)
	return fmt.Sprintf("[progress.remaining]%d:%02d:%02d[/]", secs/3600, secs%3600/60, secs%60)
}

// Progress is a rich live progress display of task rows redrawn in place on a terminal
type Progress struct {
	c       *Console
	columns []ProgressColumn
	period  time.Duration
	mu      sync.Mutex
	tasks   []*ProgressTask
	nextID  int
	height  int
	live    bool
	stop    chan struct{}
	done    chan struct{}
	pipes   []*os.File
	drained sync.WaitGroup
	stdout  *os.File
	stderr  *os.File
}

// Progress display over the console refreshed refreshPerSecond times a second
func (c *Console) NewProgress(refreshPerSecond float64, columns ...ProgressColumn) *Progress {
	return &Progress{c: c, columns: columns, period: time.Duration(float64(time.Second) / refreshPerSecond)}
}

// Adds a started task and returns its id, total nil for an indeterminate one
func (p *Progress) AddTask(description string, total *float64) int {
	mustMarkupText(description)
	p.mu.Lock()
	defer p.mu.Unlock()
	t := &ProgressTask{ID: p.nextID, Description: description, Total: total, startTime: progressNow()}
	p.nextID++
	p.tasks = append(p.tasks, t)
	p.markFinished(t)
	return t.ID
}

func (p *Progress) task(id int) *ProgressTask {
	for _, t := range p.tasks {
		if t.ID == id {
			return t
		}
	}
	panic(fmt.Sprintf("KeyError: %d", id))
}

// Records the finish time once completed reaches the total
func (p *Progress) markFinished(t *ProgressTask) {
	if t.Total != nil && t.Completed >= *t.Total && t.finishedTime == nil {
		v := t.elapsed(progressNow())
		t.finishedTime = &v
	}
}

// Drops speed samples older than the estimate window
func trimSamples(t *ProgressTask, now float64) {
	for len(t.samples) > 0 && t.samples[0].timestamp < now-progressSpeedPeriod {
		t.samples = t.samples[1:]
	}
}

// Advances a task by n steps and records a speed sample
func (p *Progress) Advance(id int, n float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.task(id)
	t.Completed += n
	now := progressNow()
	trimSamples(t, now)
	for len(t.samples) > 1000 {
		t.samples = t.samples[1:]
	}
	t.samples = append(t.samples, progressSample{now, n})
	p.markFinished(t)
}

// Replaces the description of a task
func (p *Progress) Update(id int, description string) {
	mustMarkupText(description)
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.task(id)
	t.Description = description
	trimSamples(t, progressNow())
	p.markFinished(t)
}

// Removes a task row from the display
func (p *Progress) RemoveTask(id int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.task(id)
	for i, x := range p.tasks {
		if x == t {
			p.tasks = append(p.tasks[:i], p.tasks[i+1:]...)
			break
		}
	}
}

// A column whose renderable takes the width the table fit assigns, measured at its natural width
type fittedColumn interface {
	naturalWidth() int
	cellAt(p *Progress, t *ProgressTask, now float64, width int) string
}

// Rendered lines of every task row as one grid, fitted columns redrawn at the width the fit gives them
func (p *Progress) renderLines() []string {
	if len(p.tasks) == 0 {
		return nil
	}
	now := progressNow()
	cols := make([]Column, len(p.columns))
	grid := NewGrid(1, cols...)
	for _, t := range p.tasks {
		cells := make([]string, len(p.columns))
		for i, col := range p.columns {
			if fc, ok := col.(fittedColumn); ok {
				cells[i] = strings.Repeat("━", fc.naturalWidth())
				continue
			}
			cells[i] = col.cell(p, t, now)
		}
		grid.AddRow(cells...)
	}
	widths := grid.columnWidths(p.c.Width - grid.edgeCells())
	for i, col := range p.columns {
		fc, ok := col.(fittedColumn)
		if !ok {
			continue
		}
		w := max(1, min(fc.naturalWidth(), widths[i]-grid.padLeft(i)-grid.padRight(i)))
		for r, t := range p.tasks {
			grid.Rows[r][i] = fc.cellAt(p, t, now, w)
		}
	}
	lines := grid.RenderLines(p.c.Color, p.c.system(), p.c.Width)
	for i, l := range lines {
		lines[i] = p.c.colorless(l)
	}
	return lines
}

// Cursor moves erasing the lines drawn last time, leaving the cursor at their start
func (p *Progress) eraseSeq() string {
	if p.height == 0 {
		return ""
	}
	return "\r\x1b[2K" + strings.Repeat("\x1b[1A\x1b[2K", p.height-1)
}

// Redraws the display over its previous lines, caller holds the console lock
func (p *Progress) drawLocked() {
	p.mu.Lock()
	lines := p.renderLines()
	p.mu.Unlock()
	io.WriteString(p.c.Out, p.eraseSeq()+strings.Join(lines, "\n"))
	p.height = len(lines)
}

// Starts the live display, redrawing on a terminal and routing stdout and stderr above it
func (p *Progress) Start() {
	if p.c.Quiet || p.live {
		return
	}
	p.live = true
	if !p.c.Terminal {
		return
	}
	p.c.mu.Lock()
	io.WriteString(p.c.Out, "\x1b[?25l")
	p.drawLocked()
	p.c.mu.Unlock()
	p.redirect()
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.done)
		tick := time.NewTicker(p.period)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
				p.c.mu.Lock()
				p.drawLocked()
				p.c.mu.Unlock()
			}
		}
	}()
}

// Swaps os.Stdout and os.Stderr for pipes whose lines print above the display
func (p *Progress) redirect() {
	p.stdout, p.stderr = os.Stdout, os.Stderr
	for _, target := range []**os.File{&os.Stdout, &os.Stderr} {
		r, w, err := os.Pipe()
		if err != nil {
			continue
		}
		real := *target
		*target = w
		p.pipes = append(p.pipes, w)
		p.drained.Add(1)
		go func() {
			defer p.drained.Done()
			br := bufio.NewReader(r)
			for {
				line, rerr := br.ReadString('\n')
				if line != "" {
					p.c.mu.Lock()
					io.WriteString(p.c.Out, p.eraseSeq())
					p.height = 0
					io.WriteString(real, line)
					if !strings.HasSuffix(line, "\n") {
						io.WriteString(real, "\n")
					}
					p.drawLocked()
					p.c.mu.Unlock()
				}
				if rerr != nil {
					r.Close()
					return
				}
			}
		}()
	}
}

// Stops the display after a last refresh, the final rows printed once on a non terminal
func (p *Progress) Stop() {
	if !p.live {
		return
	}
	p.live = false
	if !p.c.Terminal {
		p.mu.Lock()
		lines := p.renderLines()
		p.mu.Unlock()
		p.c.write(lines)
		return
	}
	close(p.stop)
	<-p.done
	os.Stdout, os.Stderr = p.stdout, p.stderr
	for _, w := range p.pipes {
		w.Close()
	}
	p.drained.Wait()
	p.pipes = nil
	p.c.mu.Lock()
	p.drawLocked()
	io.WriteString(p.c.Out, "\n\x1b[?25h")
	p.c.mu.Unlock()
}
