package tui

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

// Row or column an area item is free to take, the value of an item that is not pinned
const gridAuto = -1

// Track of a grid axis, a fixed count of cells or a weighted share of what the fixed tracks leave
type gridTrack struct {
	cells int
	// Weight of a shared track, zero for a fixed one
	share float64
	// Fewest cells a shared track keeps, one when unset
	floor int
}

// Track that takes a weighted share of the free cells
func sharedTrack(share float64) gridTrack { return gridTrack{share: share} }

// Tracks that share an axis evenly
func evenTracks(count int) []gridTrack {
	tracks := make([]gridTrack, max(count, 1))
	for i := range tracks {
		tracks[i] = sharedTrack(1)
	}
	return tracks
}

// Sum of whole numbers
func sumInts(values []int) int {
	total := 0
	for _, v := range values {
		total += v
	}
	return total
}

// Cells of a budget split by weight, each share rounded down and the remainder handed out from the left
func distributeByWeight(budget int, weights []float64) []int {
	shares := make([]int, len(weights))
	total := 0.0
	for _, w := range weights {
		total += w
	}
	if total <= 0 || budget <= 0 {
		return shares
	}
	for i, w := range weights {
		shares[i] = int(math.Floor(float64(budget) * w / total))
	}
	for i, rest := 0, budget-sumInts(shares); rest > 0 && i < len(shares); i, rest = i+1, rest-1 {
		shares[i]++
	}
	return shares
}

// Sizes of the tracks of an axis of total cells with a gap between tracks: fixed tracks take their size, shared ones split what is left by weight,
// a share under its floor pinned there and the rest solved again, and what still does not fit shaved off the last tracks down to one cell each
func resolveGridTracks(total, gap int, tracks []gridTrack) []int {
	count := len(tracks)
	if count == 0 {
		return nil
	}
	usable := max(count, total-max(gap, 0)*(count-1))
	sizes := make([]int, count)
	remaining := usable
	var open []int
	for i, t := range tracks {
		if t.share == 0 {
			sizes[i] = max(t.cells, 1)
			remaining -= sizes[i]
		} else {
			open = append(open, i)
		}
	}
	for len(open) > 0 {
		weights := make([]float64, len(open))
		for i, idx := range open {
			weights[i] = max(tracks[idx].share, 0.0001)
		}
		shares := distributeByWeight(max(remaining, 0), weights)
		var short []int
		for i, idx := range open {
			if shares[i] < max(tracks[idx].floor, 1) {
				short = append(short, idx)
			}
		}
		if len(short) == 0 {
			for i, idx := range open {
				sizes[idx] = shares[i]
			}
			break
		}
		for _, idx := range short {
			sizes[idx] = max(tracks[idx].floor, 1)
			remaining -= sizes[idx]
		}
		open = slices.DeleteFunc(open, func(idx int) bool { return slices.Contains(short, idx) })
	}
	for i, overflow := count-1, sumInts(sizes)-usable; i >= 0 && overflow > 0; i-- {
		give := min(overflow, sizes[i]-1)
		sizes[i] -= give
		overflow -= give
	}
	return sizes
}

// Cells a run of tracks covers with the gaps between them
func spanSize(sizes []int, start, span, gap int) int {
	end := min(len(sizes), start+span)
	if end <= start {
		return 0
	}
	return sumInts(sizes[start:end]) + max(gap, 0)*(end-start-1)
}

// Item of a flowing grid, the columns it spans and the column it starts at when pinned
type gridItem struct {
	id     string
	span   int
	pinned bool
	start  int
}

// Cell of a flowing grid, the column it starts at, the columns it spans and its width with the gaps it bridges
type gridCell struct {
	id    string
	col   int
	span  int
	width int
}

// Solved flowing grid, the width of each column and the cells of each row from left to right
type gridFlow struct {
	columns []int
	rows    [][]gridCell
}

// What a flowing grid is solved from: explicit tracks, else a column count, else as many columns of the least width as fit up to the most allowed
type gridFlowOptions struct {
	tracks         []gridTrack
	columns        int
	gap            int
	items          []gridItem
	maxColumns     int
	minColumnWidth int
	width          int
}

// Lays items out row by row: each takes the first free run of columns its span fits, a pinned one its own column, and a row that has no room starts the next
func layoutGridFlow(o gridFlowOptions) gridFlow {
	gap, width := max(o.gap, 0), max(o.width, 1)
	var columns []int
	switch {
	case len(o.tracks) > 0:
		columns = resolveGridTracks(width, gap, o.tracks)
	case o.columns <= 0:
		fit := (width + gap) / (max(o.minColumnWidth, 1) + gap)
		columns = resolveGridTracks(width, gap, evenTracks(min(max(fit, 1), max(o.maxColumns, 1))))
	default:
		// A gap needs a cell on each side, so no more columns can be drawn than cells and gaps alternate into
		drawable := width
		if gap > 0 {
			drawable = max(1, (width+gap)/(gap+1))
		}
		columns = resolveGridTracks(width, gap, evenTracks(min(max(o.columns, 1), drawable)))
	}
	count := len(columns)
	flow := gridFlow{columns: columns}
	var row []gridCell
	occupied := make([]bool, count)
	free := func(start, span int) bool {
		return !slices.Contains(occupied[start:start+span], true)
	}
	push := func() {
		slices.SortFunc(row, func(a, b gridCell) int { return a.col - b.col })
		flow.rows = append(flow.rows, row)
		row, occupied = nil, make([]bool, count)
	}
	for _, item := range o.items {
		span := min(max(item.span, 1), count)
		col := -1
		if item.pinned {
			col = min(max(item.start, 0), count-span)
		} else {
			for c := 0; c <= count-span; c++ {
				if free(c, span) {
					col = c
					break
				}
			}
		}
		if col < 0 || !free(col, span) {
			if len(row) > 0 {
				push()
			}
			col = max(col, 0)
		}
		row = append(row, gridCell{id: item.id, col: col, span: span, width: spanSize(columns, col, span, gap)})
		for c := col; c < col+span; c++ {
			occupied[c] = true
		}
	}
	if len(row) > 0 {
		push()
	}
	return flow
}

// Item of an area grid, its spans and the row and column it is pinned to, gridAuto leaving either free
type areaItem struct {
	id      string
	col     int
	row     int
	colSpan int
	rowSpan int
}

// Area item that the layout places where it first fits
func autoArea(id string, colSpan, rowSpan int) areaItem {
	return areaItem{id: id, col: gridAuto, row: gridAuto, colSpan: colSpan, rowSpan: rowSpan}
}

// Cell of an area grid solved to a rectangle of terminal cells
type areaCell struct {
	id      string
	col     int
	row     int
	colSpan int
	rowSpan int
	x       int
	y       int
	width   int
	height  int
}

// What an area grid is solved from, rows the tracks do not name sharing the height evenly
type areaOptions struct {
	columns []gridTrack
	rows    []gridTrack
	gap     int
	rowGap  int
	width   int
	height  int
	items   []areaItem
}

// Lays items out on both axes: pinned items take their place, the others the first rectangle that fits in row order, rows added as needed,
// and every cell is solved to a rectangle, which is what lets an item span rows
func layoutGridAreas(o areaOptions) []areaCell {
	gap, rowGap := max(o.gap, 0), max(o.rowGap, 0)
	columns := o.columns
	if len(columns) == 0 {
		columns = evenTracks(1)
	}
	count := len(columns)
	occupied := [][]bool{make([]bool, count)}
	grow := func(rows int) {
		for len(occupied) < rows {
			occupied = append(occupied, make([]bool, count))
		}
	}
	free := func(row, col, rowSpan, colSpan int) bool {
		grow(row + rowSpan)
		for r := row; r < row+rowSpan; r++ {
			if slices.Contains(occupied[r][col:col+colSpan], true) {
				return false
			}
		}
		return true
	}
	cells := make([]areaCell, len(o.items))
	rowCount := max(len(o.rows), 1)
	for i, item := range o.items {
		colSpan, rowSpan := min(max(item.colSpan, 1), count), max(item.rowSpan, 1)
		pinnedCol := gridAuto
		if item.col >= 0 {
			pinnedCol = min(item.col, count-colSpan)
		}
		row, col := max(item.row, 0), max(pinnedCol, 0)
		switch {
		case item.row >= 0 && pinnedCol >= 0:
		case item.row >= 0:
			// A pinned row takes its first free run of columns and overlaps at the first column when the row is full
			for c := 0; c <= count-colSpan; c++ {
				if free(row, c, rowSpan, colSpan) {
					col = c
					break
				}
			}
		default:
			// A new row is always empty, so the search ends
			for placed := false; !placed; row++ {
				first, last := 0, count-colSpan
				if pinnedCol >= 0 {
					first, last = pinnedCol, pinnedCol
				}
				for c := first; c <= last && !placed; c++ {
					if free(row, c, rowSpan, colSpan) {
						col, placed = c, true
					}
				}
				if placed {
					break
				}
			}
		}
		grow(row + rowSpan)
		for r := row; r < row+rowSpan; r++ {
			for c := col; c < col+colSpan; c++ {
				occupied[r][c] = true
			}
		}
		cells[i] = areaCell{id: item.id, col: col, row: row, colSpan: colSpan, rowSpan: rowSpan}
		rowCount = max(rowCount, row+rowSpan)
	}
	rows := make([]gridTrack, rowCount)
	for i := range rows {
		rows[i] = sharedTrack(1)
		if i < len(o.rows) {
			rows[i] = o.rows[i]
		}
	}
	columnSizes, rowSizes := resolveGridTracks(max(o.width, 1), gap, columns), resolveGridTracks(max(o.height, 1), rowGap, rows)
	offsets := func(sizes []int, gap int) []int {
		starts, at := make([]int, len(sizes)), 0
		for i, size := range sizes {
			starts[i] = at
			at += size + gap
		}
		return starts
	}
	columnStarts, rowStarts := offsets(columnSizes, gap), offsets(rowSizes, rowGap)
	for i := range cells {
		cell := &cells[i]
		cell.x, cell.y = columnStarts[cell.col], rowStarts[cell.row]
		cell.width, cell.height = spanSize(columnSizes, cell.col, cell.colSpan, gap), spanSize(rowSizes, cell.row, cell.rowSpan, rowGap)
	}
	return cells
}

// Cell of a widget canvas, a rune in a theme role
type canvasCell struct {
	r    rune
	role string
	bold bool
}

// Rectangle of cells a widget draws boxes and text into before it becomes lines of spans, a part of it clipping what is drawn past its edges
type widgetCanvas struct {
	cells  []canvasCell
	stride int
	x0, y0 int
	w, h   int
}

// Blank canvas of a size
func newWidgetCanvas(w, h int) *widgetCanvas {
	w, h = max(w, 0), max(h, 0)
	return &widgetCanvas{cells: make([]canvasCell, w*h), stride: w, w: w, h: h}
}

// Part of the canvas that draws from its own corner and clips at its own edges
func (c *widgetCanvas) sub(x, y, w, h int) *widgetCanvas {
	left, top := min(max(x, 0), c.w), min(max(y, 0), c.h)
	right, bottom := min(max(x+w, left), c.w), min(max(y+h, top), c.h)
	return &widgetCanvas{cells: c.cells, stride: c.stride, x0: c.x0 + left, y0: c.y0 + top, w: right - left, h: bottom - top}
}

// Sets one cell, a cell outside the canvas left alone
func (c *widgetCanvas) put(x, y int, r rune, role string, bold bool) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	c.cells[(c.y0+y)*c.stride+c.x0+x] = canvasCell{r: r, role: role, bold: bold}
}

// Writes text rightwards from a cell, cut after its room
func (c *widgetCanvas) text(x, y, room int, s, role string, bold bool) {
	for i, r := range []rune(s) {
		if i >= room {
			return
		}
		c.put(x+i, y, r, role, bold)
	}
}

// Writes text in the middle of a run of cells
func (c *widgetCanvas) centred(x, y, room int, s, role string, bold bool) {
	c.text(x+max(room-len([]rune(s)), 0)/2, y, room, s, role, bold)
}

// Writes text against the right end of a run of cells
func (c *widgetCanvas) flushRight(x, y, room int, s, role string, bold bool) {
	c.text(x+max(room-len([]rune(s)), 0), y, room, s, role, bold)
}

// Draws the border of a box with round or square corners
func (c *widgetCanvas) box(x, y, w, h int, role string, round bool) {
	if w < 2 || h < 2 {
		return
	}
	corners := []rune("┌┐└┘")
	if round {
		corners = []rune("╭╮╰╯")
	}
	for i := 1; i < w-1; i++ {
		c.put(x+i, y, '─', role, false)
		c.put(x+i, y+h-1, '─', role, false)
	}
	for j := 1; j < h-1; j++ {
		c.put(x, y+j, '│', role, false)
		c.put(x+w-1, y+j, '│', role, false)
	}
	c.put(x, y, corners[0], role, false)
	c.put(x+w-1, y, corners[1], role, false)
	c.put(x, y+h-1, corners[2], role, false)
	c.put(x+w-1, y+h-1, corners[3], role, false)
}

// Lines of the canvas, neighbouring cells of one role and weight joined into one span
func (c *widgetCanvas) lines() []WidgetLine {
	out := make([]WidgetLine, c.h)
	for y := range c.h {
		var line WidgetLine
		var run strings.Builder
		var role string
		var bold bool
		flush := func() {
			if run.Len() > 0 {
				line.Spans = append(line.Spans, WidgetSpan{Text: run.String(), Role: role, Bold: bold})
				run.Reset()
			}
		}
		for x := range c.w {
			cell := c.cells[(c.y0+y)*c.stride+c.x0+x]
			if cell.r == 0 {
				cell = canvasCell{r: ' '}
			}
			if cell.role != role || cell.bold != bold {
				flush()
				role, bold = cell.role, cell.bold
			}
			run.WriteRune(cell.r)
		}
		flush()
		out[y] = line
	}
	return out
}

// Widget of a flowing grid, the columns it spans, how tall it is at a width and how it draws itself into its cell
type flowWidget struct {
	span   int
	height func(width int) int
	draw   func(c *widgetCanvas)
}

// How a flowing grid of widgets is drawn, a negative gap, padding or row gap chosen from the room and the nesting depth
type flowGridOptions struct {
	columns        int
	depth          int
	gap            int
	padX           int
	rowGap         int
	minColumnWidth int
	maxColumns     int
}

// Flowing grid whose gap, side padding and row gap are all left to the room
func autoFlowGrid(columns, depth int) flowGridOptions {
	return flowGridOptions{columns: columns, depth: depth, gap: gridAuto, padX: gridAuto, rowGap: gridAuto}
}

// Widget of a flowing grid placed at a rectangle of its canvas
type placedWidget struct {
	index      int
	x, y, w, h int
}

// Places the widgets of a flowing grid at a width and answers the rows the grid takes: nested and narrow grids draw tighter, a grid of many
// columns with no gap at all, and each row is as tall as its tallest widget
func placeFlowGrid(width int, o flowGridOptions, widgets []flowWidget) ([]placedWidget, int) {
	cols := max(width, 1)
	padX := o.padX
	if padX < 0 {
		switch {
		case o.depth <= 0 || cols < 24:
			padX = 0
		case cols >= 56:
			padX = 2
		default:
			padX = 1
		}
	}
	inner := max(1, cols-2*padX)
	gap := o.gap
	if gap < 0 {
		switch {
		case inner < 36 || o.columns >= 8:
			gap = 0
		case o.depth > 0 || inner < 72 || o.columns >= 4:
			gap = 1
		default:
			gap = 2
		}
	}
	rowGap := o.rowGap
	if rowGap < 0 {
		rowGap = 1
		if o.depth > 0 {
			rowGap = 0
		}
	}
	items := make([]gridItem, len(widgets))
	for i, w := range widgets {
		items[i] = gridItem{id: strconv.Itoa(i), span: w.span}
	}
	maxColumns, minColumnWidth := o.maxColumns, o.minColumnWidth
	if maxColumns <= 0 {
		maxColumns = 2
	}
	if minColumnWidth <= 0 {
		minColumnWidth = 46
	}
	flow := layoutGridFlow(gridFlowOptions{columns: o.columns, gap: gap, items: items, maxColumns: maxColumns, minColumnWidth: minColumnWidth, width: inner})
	var placed []placedWidget
	y := 0
	for r, row := range flow.rows {
		tallest := 0
		first := len(placed)
		for _, cell := range row {
			index, _ := strconv.Atoi(cell.id)
			height := widgets[index].height(cell.width)
			tallest = max(tallest, height)
			placed = append(placed, placedWidget{index: index, x: padX + sumInts(flow.columns[:cell.col]) + gap*cell.col, y: y, w: cell.width, h: height})
		}
		y += tallest
		if r < len(flow.rows)-1 && len(placed) > first {
			y += rowGap
		}
	}
	return placed, y
}

// Draws a flowing grid of widgets from the top of a canvas and answers the rows it took
func drawFlowGrid(c *widgetCanvas, o flowGridOptions, widgets []flowWidget) int {
	placed, height := placeFlowGrid(c.w, o, widgets)
	for _, p := range placed {
		widgets[p.index].draw(c.sub(p.x, p.y, p.w, p.h))
	}
	return height
}

// Column chart of the last values of a series over several rows of eighth blocks, top row first, a short series growing in from the right
func sparkRows(series []float64, width, rows int) []string {
	out := make([]string, rows)
	if len(series) == 0 {
		for i := range out {
			out[i] = strings.Repeat(" ", max(width, 0))
		}
		return out
	}
	view := series[max(0, len(series)-max(width, 1)):]
	lo, hi := slices.Min(view), slices.Max(view)
	spread := hi - lo
	if spread == 0 {
		spread = 1
	}
	for line := range rows {
		below := (rows - 1 - line) * len(sparkBlocks)
		var sb strings.Builder
		sb.WriteString(strings.Repeat(" ", max(width-len(view), 0)))
		for _, v := range view {
			level := max(1, int(math.Round((v-lo)/spread*float64(rows*len(sparkBlocks)))))
			if filled := min(len(sparkBlocks), level-below); filled > 0 {
				sb.WriteRune(sparkBlocks[filled-1])
			} else {
				sb.WriteByte(' ')
			}
		}
		out[line] = sb.String()
	}
	return out
}
