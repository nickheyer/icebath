package tui

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Most columns or rows the demo grid takes
	gridMaxSize = 12
	// Rows of a demo cell, flat and with a nested grid inside, and of a cell of a nested grid
	flatCellRows, nestedCellRows, miniCellRows = 5, 9, 3
	// Rows of the header of the streams mode and of each of its row tracks
	streamHeaderRows, streamTrackRows = 3, 6
	// Samples a stream keeps for its chart
	streamHistory = 240
	// Pace the demo is ticked at, the fastest of its streams
	gridTickEvery = 90 * time.Millisecond
)

// Size of a demo grid written as columns x rows
var gridSizeRe = regexp.MustCompile(`(?i)^(\d+)\s*x\s*(\d+)$`)

// Words the token stream of the streams mode writes out, over and over
var streamWords = strings.Fields("Icebath streams tokens into the promoted cell while the grid reshapes around it. " +
	"Cells are keyed by id, so promotion never resets a panel — history, cursors and tickers all survive the relayout. " +
	"Row and column tracks re-solve to integer terminal cells on every change, spans bridge the gaps they cross, and dense " +
	"auto-placement backfills the holes the promoted panel leaves behind.")

// Tool calls the tool feed of the streams mode scrolls through
var streamTools = []string{"┊ read_file pkg/tui/app.go", "┊ terminal go vet ./pkg/tui/", "┊ search_files \"layoutGridAreas\"", "┊ patch pkg/tui/widgetgrid.go",
	"┊ web_search terminal cell layout", "┊ delegate_task refactor sparkline", "┊ terminal go build ./cmd/icebath", "┊ vision_analyze screenshot.png"}

// Live panel of the streams mode, a source that ticks at its own pace with the history its chart draws
type gridStream struct {
	id     string
	title  string
	every  time.Duration
	last   time.Time
	ticks  int
	series []float64
	// Theme role of the chart of a sampled stream
	role   string
	sample func(s *gridStream) float64
	format func(v float64) string
	draw   func(s *gridStream, c *widgetCanvas)
	// Value a random walk stands at
	walk float64
}

// Demo grid, its size and cursor, its layout switches and the panels of its streams mode
type gridState struct {
	cols, rows int
	col, row   int
	areas      bool
	nested     bool
	streams    bool
	zoomed     bool
	// Gap between columns and side padding, gridAuto leaving each to the room
	gap, pad int
	// Panel the keys of the streams mode point at and panel that holds the large slot
	streamFocus, streamMain int
	feeds                   []*gridStream
	// Moment the streams mode was entered, what its session panel counts from
	since time.Time
}

// Demo grid of a size, in the streams mode or not
func newGridState(cols, rows int, streams bool) *gridState {
	g := &gridState{cols: cols, rows: rows, gap: gridAuto, pad: gridAuto}
	if streams {
		g.startStreams()
	}
	return g
}

// Size of a demo grid from nothing, 4x3 or 4 3, both numbers needed and each held between one and twelve
func parseGridSize(arg string) (cols, rows int, ok bool) {
	trimmed := strings.TrimSpace(arg)
	if trimmed == "" {
		return 4, 3, true
	}
	fields := strings.Fields(trimmed)
	if m := gridSizeRe.FindStringSubmatch(trimmed); m != nil {
		fields = m[1:]
	}
	if len(fields) != 2 {
		return 0, 0, false
	}
	size := [2]int{4, 3}
	for i, field := range fields {
		n, err := strconv.ParseFloat(field, 64)
		endless := strings.Trim(field, "0123456789") == "" || strings.TrimLeft(field, "+-") == "Infinity"
		switch {
		case endless && (err != nil || math.IsInf(n, 0)):
			// A number too large to hold keeps the size the demo opens with
		case err != nil || math.IsNaN(n) || math.IsInf(n, 0):
			return 0, 0, false
		default:
			size[i] = int(min(max(math.Round(n), 1), gridMaxSize))
		}
	}
	return size[0], size[1], true
}

// Next value of a setting that cycles from automatic through zero up to its most and back to automatic
func cycleAuto(value, most int) int {
	switch {
	case value < 0:
		return 0
	case value >= most:
		return gridAuto
	}
	return value + 1
}

// Enters the streams mode with every panel starting anew
func (g *gridState) startStreams() {
	g.streams, g.since = true, time.Now()
	spark := func(s *gridStream, c *widgetCanvas) {
		current := 0.0
		if len(s.series) > 0 {
			current = s.series[len(s.series)-1]
		}
		c.text(0, 0, c.w, s.format(current), "muted", false)
		for i, line := range sparkRows(s.series, c.w, max(c.h-1, 1)) {
			c.text(0, 1+i, c.w, line, s.role, false)
		}
	}
	g.feeds = []*gridStream{
		{id: "tokens", title: "token stream", every: gridTickEvery, draw: func(s *gridStream, c *widgetCanvas) {
			if c.w <= 0 || c.h <= 0 {
				return
			}
			// Enough of the latest words to fill the cell, the newest at the bottom
			var words []string
			for i, used := s.ticks%(len(streamWords)*4), 0; i >= 0 && used < max(16, c.w*c.h); i-- {
				word := streamWords[i%len(streamWords)]
				words = append([]string{word}, words...)
				used += len([]rune(word)) + 1
			}
			var lines []string
			line := ""
			for _, word := range words {
				if line != "" && len([]rune(line))+1+len([]rune(word)) > c.w {
					lines, line = append(lines, line), ""
				}
				if line != "" {
					line += " "
				}
				line += word
			}
			lines = append(lines, line)
			lines = lines[max(len(lines)-c.h, 0):]
			for y, text := range lines {
				c.text(0, y, c.w, text, "text", false)
			}
			c.put(min(len([]rune(lines[len(lines)-1])), c.w-1), len(lines)-1, '▌', "primary", false)
		}},
		{id: "throughput", title: "throughput", every: 150 * time.Millisecond, role: "accent", draw: spark,
			sample: func(*gridStream) float64 {
				return (math.Sin(float64(time.Now().UnixMilli())/700)+1)/2 + rand.Float64()*0.15
			},
			format: func(v float64) string { return fmt.Sprintf("%.0f tok/s", 20+v*40) }},
		{id: "heap", title: "memory", every: 500 * time.Millisecond, role: "ok", draw: spark,
			sample: func(*gridStream) float64 { return float64(sampleMemory().heapUsed) },
			format: func(v float64) string { return fmt.Sprintf("heap %.1f MB", v/(1<<20)) }},
		{id: "latency", title: "latency", every: 250 * time.Millisecond, role: "warn", draw: spark, walk: 0.5,
			sample: func(s *gridStream) float64 {
				s.walk = min(1, max(0, s.walk+(rand.Float64()-0.5)*0.2))
				return s.walk
			},
			format: func(v float64) string { return fmt.Sprintf("%.0f ms", 20+v*180) }},
		{id: "tools", title: "tool feed", every: 600 * time.Millisecond, draw: func(s *gridStream, c *widgetCanvas) {
			shown := min(max(c.h, 1), s.ticks+1)
			for age := range shown {
				role := "muted"
				if age == 0 {
					role = "text"
				}
				c.text(0, shown-1-age, c.w, streamTools[(s.ticks-age)%len(streamTools)], role, false)
			}
		}},
		{id: "meta", title: "session", every: time.Second, draw: func(s *gridStream, c *widgetCanvas) {
			up := int(time.Since(g.since).Seconds())
			c.text(0, 0, c.w, time.Now().Format("15:04:05"), "text", false)
			c.text(0, 1, c.w, fmt.Sprintf("up %dm %ds", up/60, up%60), "muted", false)
			c.text(0, 2, c.w, fmt.Sprintf("%d ticks", s.ticks), "muted", false)
		}},
	}
}

// Advances every stream whose own interval passed
func (g *gridState) tick(now time.Time) {
	if !g.streams {
		return
	}
	for _, s := range g.feeds {
		if now.Sub(s.last) < s.every {
			continue
		}
		s.last = now
		s.ticks++
		if s.sample != nil {
			s.series = append(s.series, s.sample(s))
			s.series = s.series[max(len(s.series)-streamHistory, 0):]
		}
	}
}

// Takes one key of the demo and answers whether it stays open
func (g *gridState) input(ctx context.Context, in WidgetInput) bool {
	ch := in.Text
	if in.Ctrl || in.Alt {
		ch = ""
	}
	back := in.Key == "esc" || ch == "q"
	switch {
	case in.Key == "ctrl+c":
		return false
	case ch == "d":
		// The host swaps this window for the dialog, a widget opening another through the typed call and no command line
		OpenWidget(ctx, "dialog-test", sampleDialog{zone: "center", title: "Overlay primitive", hint: "Esc/q/Enter close",
			body: "Dialog opened from /grid-test.\n\nA widget may open another, the host puts it in this window."})
	case g.streams:
		count := len(g.feeds)
		switch {
		case back || ch == "s":
			g.streams = false
		case in.Key == "enter":
			g.streamMain = g.streamFocus
		case ch == "r":
			*g = *newGridState(4, 3, false)
		case in.Key == "left" || in.Key == "up" || ch == "h" || ch == "k":
			g.streamFocus = (g.streamFocus + count - 1) % count
		case in.Key == "right" || in.Key == "down" || ch == "l" || ch == "j":
			g.streamFocus = (g.streamFocus + 1) % count
		}
	case g.zoomed && back:
		g.zoomed = false
	case back:
		return false
	case in.Key == "enter":
		g.nested, g.zoomed = true, true
	case ch == "n":
		g.nested = !g.nested
	case ch == "a":
		g.areas = !g.areas
	case ch == "s":
		g.areas = false
		g.startStreams()
	case ch == "g":
		g.gap = cycleAuto(g.gap, 3)
	case ch == "p":
		g.pad = cycleAuto(g.pad, 2)
	case ch == "r":
		*g = *newGridState(4, 3, false)
	case ch == "+" || ch == "=":
		g.cols = min(g.cols+1, gridMaxSize)
	case ch == "-" || ch == "_":
		g.cols = max(g.cols-1, 1)
	case ch == "]":
		g.rows = min(g.rows+1, gridMaxSize)
	case ch == "[":
		g.rows = max(g.rows-1, 1)
	case in.Key == "left" || ch == "h":
		g.col--
	case in.Key == "right" || ch == "l":
		g.col++
	case in.Key == "up" || ch == "k":
		g.row--
	case in.Key == "down" || ch == "j":
		g.row++
	}
	g.col, g.row = min(max(g.col, 0), g.cols-1), min(max(g.row, 0), g.rows-1)
	return true
}

// Cells of a nested grid, each in its own tone, the last of three spanning the row
func miniCells(count, columns int) []flowWidget {
	roles := []string{"ok", "warn", "accent", "muted", "primary"}
	cells := make([]flowWidget, count)
	for i := range cells {
		role, label := roles[i%len(roles)], "c"+strconv.Itoa(i+1)
		cells[i] = flowWidget{span: 1, height: func(int) int { return miniCellRows }, draw: func(c *widgetCanvas) {
			c.box(0, 0, c.w, c.h, role, false)
			c.centred(1, 1, c.w-2, label, role, false)
		}}
		if count == 3 && i == count-1 {
			cells[i].span = columns
		}
	}
	return cells
}

// Grid of nested cells drawn tight, two columns wide inside a demo cell and wider in the zoomed one
func nestedFlow(columns int) flowGridOptions {
	return flowGridOptions{columns: columns, depth: 1, gap: 1, padX: 0, rowGap: 0, minColumnWidth: 1}
}

// Draws one cell of the flat grid, its label in the middle or over a nested grid
func (g *gridState) drawCell(c *widgetCanvas, label string, active, nested bool) {
	padX := 0
	if c.w >= 14 {
		padX = 1
	}
	inner := max(1, c.w-2-2*padX)
	border, tone := "border", "label"
	if active {
		border, tone = "primary", "primary"
	}
	c.box(0, 0, c.w, c.h, border, true)
	body := c.sub(1+padX, 1, inner, c.h-2)
	if !nested || c.w < 10 {
		body.centred(0, (body.h-1)/2, inner, label, tone, active)
		return
	}
	body.centred(0, 0, inner, label, tone, active)
	drawFlowGrid(body.sub(0, 1, inner, body.h-1), nestedFlow(2), miniCells(3, 2))
}

// Canvas of the flat grid, every cell a box with its label, every other one holding a nested grid when nesting is on
func (g *gridState) flatCanvas(cols int) *widgetCanvas {
	o := flowGridOptions{columns: g.cols, gap: g.gap, padX: g.pad, rowGap: 0, minColumnWidth: 1}
	if g.gap < 0 && g.nested {
		o.gap = 1
	}
	rows := flatCellRows
	if g.nested {
		rows = nestedCellRows
	}
	cells := make([]flowWidget, g.rows*g.cols)
	for i := range cells {
		row, col := i/g.cols, i%g.cols
		cells[i] = flowWidget{span: 1, height: func(int) int { return rows }, draw: func(c *widgetCanvas) {
			// Nesting shows in every other cell of every other row, so the switch reads apart from the cursor
			g.drawCell(c, "c"+strconv.Itoa(i+1), row == g.row && col == g.col, g.nested && row%2 == 0 && col%2 == 0)
		}}
	}
	_, height := placeFlowGrid(cols, o, cells)
	c := newWidgetCanvas(cols, height)
	drawFlowGrid(c, o, cells)
	return c
}

// Canvas of the zoomed cell, a box around a header and a child grid
func (g *gridState) zoomedCanvas(cols int) *widgetCanvas {
	columns := 2
	if cols >= 72 {
		columns = 4
	}
	body := 2 + 2*miniCellRows
	c := newWidgetCanvas(cols, body+4)
	c.box(0, 0, c.w, c.h, "primary", true)
	inner := c.sub(2, 2, max(1, cols-4), body)
	halves := resolveGridTracks(inner.w, 1, evenTracks(2))
	inner.text(0, 0, halves[0], "parent c"+strconv.Itoa(g.row*g.cols+g.col+1), "primary", true)
	inner.flushRight(halves[0]+1, 0, halves[1], "nested child grid", "muted", false)
	drawFlowGrid(inner.sub(0, 2, inner.w, body-2), nestedFlow(columns), miniCells(columns*2, columns))
	return c
}

// Canvas of the areas mode: the first column twice as wide when there is room, the first cell spanning two rows and the second two columns,
// the rest placed where they fit, the cursor lighting the whole area it stands in
func (g *gridState) areasCanvas(cols int) *widgetCanvas {
	tracks := evenTracks(g.cols)
	if g.cols >= 3 {
		tracks[0] = sharedTrack(2)
	}
	tall, wide := min(g.rows, 2), min(g.cols, 2)
	items := make([]areaItem, max(1, g.rows*g.cols-(tall-1)-(wide-1)))
	for i := range items {
		items[i] = autoArea("c"+strconv.Itoa(i+1), 1, 1)
	}
	items[0].rowSpan = tall
	if len(items) > 1 {
		items[1].colSpan = wide
	}
	gap := g.gap
	if gap < 0 {
		gap = 1
	}
	c := newWidgetCanvas(cols, g.rows*flatCellRows)
	for _, cell := range layoutGridAreas(areaOptions{columns: tracks, rows: evenTracks(g.rows), gap: gap, width: cols, height: c.h, items: items}) {
		active := g.row >= cell.row && g.row < cell.row+cell.rowSpan && g.col >= cell.col && g.col < cell.col+cell.colSpan
		border, tone := "border", "label"
		if active {
			border, tone = "primary", "primary"
		}
		box := c.sub(cell.x, cell.y, cell.width, cell.height)
		box.box(0, 0, box.w, box.h, border, true)
		lines := []string{cell.id}
		if cell.height >= 5 {
			lines = append(lines, strconv.Itoa(cell.width)+"w")
			if cell.colSpan > 1 || cell.rowSpan > 1 {
				lines[1] = strconv.Itoa(cell.colSpan) + "x" + strconv.Itoa(cell.rowSpan)
			}
		}
		top := 1 + max(box.h-2-len(lines), 0)/2
		box.centred(1, top, box.w-2, lines[0], tone, active)
		if len(lines) > 1 {
			box.centred(1, top+1, box.w-2, lines[1], "muted", false)
		}
	}
	return c
}

// Canvas of the streams mode: a header over six live panels, the promoted one in a slot of two by two and the others placed around it,
// every panel keeping its history as the grid reshapes
func (g *gridState) streamsCanvas(cols int) *widgetCanvas {
	main, focused := g.feeds[g.streamMain%len(g.feeds)], g.feeds[g.streamFocus%len(g.feeds)]
	ordered := []*gridStream{main}
	for _, s := range g.feeds {
		if s != main {
			ordered = append(ordered, s)
		}
	}
	items := []areaItem{autoArea("header", 3, 1)}
	for i, s := range ordered {
		span := 1
		if i == 0 {
			span = 2
		}
		items = append(items, autoArea(s.id, span, span))
	}
	c := newWidgetCanvas(cols, streamHeaderRows+3*streamTrackRows)
	rows := append([]gridTrack{{cells: streamHeaderRows}}, evenTracks(3)...)
	for i, cell := range layoutGridAreas(areaOptions{columns: evenTracks(3), rows: rows, gap: 1, width: cols, height: c.h, items: items}) {
		box := c.sub(cell.x, cell.y, cell.width, cell.height)
		if i == 0 {
			box.box(0, 0, box.w, box.h, "border", true)
			box.text(2, box.h/2, box.w-4, "icebath mission control", "primary", true)
			box.flushRight(2, box.h/2, box.w-4, "main: "+main.title, "muted", false)
			continue
		}
		s := ordered[i-1]
		border, tone, title := "border", "label", s.title
		switch {
		case s == focused:
			border, tone, title = "primary", "primary", "▸ "+title
		case s == main:
			border = "accent"
		}
		if s == main {
			title += " ·"
		}
		box.box(0, 0, box.w, box.h, border, true)
		box.text(2, 1, box.w-4, title, tone, s == focused)
		s.draw(s, box.sub(2, 2, max(1, box.w-4), max(1, box.h-3)))
	}
	return c
}

// Lines of the demo at a width: its title and size, the keys of its mode, the grid and the state of its switches
func (g *gridState) view(cols int) []WidgetLine {
	cols = max(cols, 12)
	title, size := "/grid-test", strconv.Itoa(g.cols)+"x"+strconv.Itoa(g.rows)+" grid"
	hint := "arrows/hjkl move · Enter zoom · d dialog · a areas · s streams · +/- cols · [] rows · g gap · p pad · n nest · q close"
	var canvas *widgetCanvas
	switch {
	case g.zoomed:
		title += " / r" + strconv.Itoa(g.row+1) + " c" + strconv.Itoa(g.col+1)
		hint = "arrows/hjkl switch cell · Esc/q back · Ctrl+C close"
		canvas = g.zoomedCanvas(cols)
	case g.streams:
		hint = "arrows/hjkl focus · Enter promote · d dialog · Esc/q back · Ctrl+C close"
		canvas = g.streamsCanvas(cols)
	case g.areas:
		canvas = g.areasCanvas(cols)
	default:
		canvas = g.flatCanvas(cols)
	}
	if g.streams {
		size = "streams"
	}
	head := newWidgetCanvas(cols, 1)
	head.flushRight(0, 0, cols, size, "muted", false)
	head.text(0, 0, cols, title, "primary", true)
	lines := append([]WidgetLine{{}}, head.lines()...)
	lines = append(lines, WidgetLine{}, widgetText("muted", hint), WidgetLine{})
	lines = append(lines, canvas.lines()...)
	if !g.zoomed && !g.streams {
		setting := func(value int) string {
			if value < 0 {
				return "auto"
			}
			return strconv.Itoa(value)
		}
		nested, areas := "off", "off"
		if g.nested {
			nested = "on"
		}
		if g.areas {
			areas = "on (2fr first col · c1 spans rows · c2 spans cols)"
		}
		lines = append(lines, WidgetLine{}, widgetText("muted", "gap "+setting(g.gap)+" · pad "+setting(g.pad)+" · nested "+nested+" · areas "+areas))
	}
	return append(lines, WidgetLine{})
}

// The grid demo: a grid of cells to move through, zoom into, nest, span and tile live panels in, every change laid out anew
func gridTestWidget() Widget {
	return Widget{ID: "grid-test", Help: "open an interactive widget grid demo", Mode: "modal", Width: 120, Every: gridTickEvery,
		Usage: "usage: /grid-test [cols]x[rows]  ·  /grid-test [cols] [rows]  ·  /grid-test streams",
		Rows:  func(state any, cols int) int { return len(state.(*gridState).view(cols)) },
		Open: func(_ context.Context, arg string) (any, bool, error) {
			if strings.EqualFold(strings.TrimSpace(arg), "streams") {
				return newGridState(4, 3, true), true, nil
			}
			cols, rows, ok := parseGridSize(arg)
			if !ok {
				return nil, false, nil
			}
			return newGridState(cols, rows, false), true, nil
		},
		Tick: func(_ context.Context, state any) (any, error) {
			state.(*gridState).tick(time.Now())
			return state, nil
		},
		Back: func(_ context.Context, state any) (any, bool, error) {
			g := state.(*gridState)
			stepped := g.streams || g.zoomed
			if g.streams {
				g.streams = false
			} else {
				g.zoomed = false
			}
			return g, stepped, nil
		},
		Input: func(ctx context.Context, state any, in WidgetInput) (any, bool, error) {
			return state, state.(*gridState).input(ctx, in), nil
		},
		Render: func(_ context.Context, frame WidgetFrame) (WidgetView, error) {
			return WidgetView{Lines: frame.State.(*gridState).view(frame.Cols)}, nil
		},
	}
}
