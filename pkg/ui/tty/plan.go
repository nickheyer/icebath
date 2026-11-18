package tty

import "slices"

// What the terminal should show in one column and the way to write it
type want struct {
	Cell
	enc   enc
	blind bool
}

// Run of target columns with one style, link, encoding and dirtiness, the unit a row plan acts on
type chunk struct {
	a, b  int
	dirty bool
	blank bool
	style Style
	link  uint32
	enc   enc
	blind bool
}

// Ways a row plan covers a chunk
const (
	actSkip = iota
	actPrint
	actErase
	actClear
)

// One step of a row plan
type action struct {
	kind  int
	chunk int
	pen   Style
}

// Terminal state a row plan reaches after some chunks, with the step that led there
type rowState struct {
	cur    cursor
	pen    Style
	link   uint32
	penok  bool
	dec    bool
	decok  bool
	cost   int
	parent int
	act    action
	done   bool
}

// Row plan and the state it leaves the terminal in
type rowPlan struct {
	acts  []action
	cost  int
	entry cursor
	epen  Style
	elink uint32
	lead  int
	exit  rowState
}

// Reusable buffers of the frame planner
type planner struct {
	dirty  []bool
	chunks []chunk
	states [][]rowState
	rep    bool
}

// Marks the columns of row y that differ from the target, widened to whole clusters on both sides
func (pl *planner) markDirty(s *screen, y int, row []want) bool {
	w := s.w
	pl.dirty = slices.Grow(pl.dirty[:0], w)[:w]
	d := pl.dirty
	mrow, known := s.row(y), s.rowKnown(y)
	any := false
	for x := 0; x < w; x++ {
		d[x] = !known[x] || row[x].blind || !sameCell(&mrow[x], &row[x].Cell)
		any = any || d[x]
	}
	if !any {
		return false
	}
	for changed := true; changed; {
		changed = false
		for x := 0; x < w; x++ {
			if !d[x] {
				continue
			}
			for side := 0; side < 2; side++ {
				a, b := 0, 0
				if side == 0 {
					a, b = wantSpan(row, x)
				} else {
					a, b = cellSpan(mrow, x)
				}
				for i := a; i < b; i++ {
					if !d[i] {
						d[i], changed = true, true
					}
				}
			}
		}
	}
	return true
}

// Columns the target cluster covering column x occupies
func wantSpan(row []want, x int) (int, int) {
	a := x
	for a > 0 && row[a].Width == 0 {
		a--
	}
	return a, min(a+max(int(row[a].Width), 1), len(row))
}

// Columns the model cluster covering column x occupies
func cellSpan(row []Cell, x int) (int, int) {
	a := x
	for a > 0 && row[a].Width == 0 {
		a--
	}
	return a, min(a+max(int(row[a].Width), 1), len(row))
}

// Splits a target row into chunks at every change of dirtiness, look, link or encoding
func (pl *planner) chunk(row []want) {
	pl.chunks = pl.chunks[:0]
	w := len(row)
	for x := 0; x < w; {
		c := &row[x]
		n := max(int(c.Width), 1)
		blank := c.Text == " " && c.Style == Style{Bg: c.Style.Bg} && c.Rider == "" && c.After == ""
		k := chunk{a: x, b: min(x+n, w), dirty: pl.dirty[x], blank: blank, style: c.Style, link: c.Link, enc: c.enc, blind: c.blind}
		if last := len(pl.chunks) - 1; last >= 0 {
			p := &pl.chunks[last]
			if p.dirty == k.dirty && p.blank == k.blank && p.style == k.style && p.link == k.link && p.enc == k.enc && !p.blind && !k.blind {
				p.b = k.b
				x += n
				continue
			}
		}
		pl.chunks = append(pl.chunks, k)
		x += n
	}
}

// Repeat runs of a chunk, each the start column and length of a run of one repeatable cluster
func (pl *planner) runs(row []want, k chunk, f func(x, n int)) {
	for x := k.a; x < k.b; {
		c := &row[x]
		n := 1
		if pl.rep && k.enc == encText && repeatable(c) {
			for x+n < k.b && row[x+n].Text == c.Text && repeatable(&row[x+n]) {
				n++
			}
		}
		f(x, n)
		x += max(int(c.Width), 1) * n
	}
}

// Reports whether REP may repeat the cell, one rune one column and nothing riding on it
func repeatable(c *want) bool {
	return c.Width == 1 && c.Rider == "" && c.After == "" && !c.blind && len(c.Text) <= 4 && len([]rune(c.Text)) == 1
}

// Bytes printing one cluster costs in its encoding, a fragile one paying for the motion that ends its text run
func clusterCost(c *want) int {
	n := len(c.Rider) + len(c.After)
	if c.enc == encText && c.Width > 0 && fragile(c.Text) {
		n += 5
	}
	switch c.enc {
	case encDEC:
		return n + 1
	case encSized:
		return n + 9 + digits(int(c.Width)) + len(c.Text)
	}
	return n + len(c.Text)
}

// Bytes printing a whole chunk costs, repeat runs going through REP where that is shorter
func (pl *planner) printCost(row []want, k chunk) int {
	total := 0
	pl.runs(row, k, func(x, n int) {
		one := clusterCost(&row[x])
		if n == 1 {
			total += one
			return
		}
		total += min(one*n, one+len(csiN('b', n-1)))
	})
	return total
}

// Cheapest plan for row y from the terminal state in, by a DP over chunk boundaries
func (pl *planner) planRow(s *screen, y int, row []want, in rowState) (rowPlan, bool) {
	if !pl.markDirty(s, y, row) {
		return rowPlan{}, false
	}
	pl.chunk(row)
	n := len(pl.chunks)
	for len(pl.states) < n+1 {
		pl.states = append(pl.states, nil)
	}
	for i := 0; i <= n; i++ {
		pl.states[i] = pl.states[i][:0]
	}
	in.cost, in.parent, in.done = 0, -1, false
	pl.states[0] = append(pl.states[0], in)
	best := -1
	var final []rowState
	add := func(i int, st rowState) {
		if st.done {
			final = append(final, st)
			return
		}
		list := pl.states[i]
		for j := range list {
			o := &list[j]
			if o.cur == st.cur && o.pen == st.pen && o.link == st.link && o.penok == st.penok && o.dec == st.dec && o.decok == st.decok {
				if st.cost < o.cost {
					*o = st
				}
				return
			}
		}
		pl.states[i] = append(list, st)
	}
	for i := 0; i < n; i++ {
		k := pl.chunks[i]
		for si := range pl.states[i] {
			st := pl.states[i][si]
			step := func(kind int, pen Style, link uint32, extra int, cur cursor, done bool, dec bool) {
				nx := st
				nx.parent, nx.act, nx.done = si, action{kind: kind, chunk: i, pen: pen}, done
				nx.cost += extra
				if kind != actSkip {
					m := motionCost(st.cur, k.a, y, s.h)
					p := penCostFrom(st, pen, link, s.links)
					nx.cost += m + p
					nx.pen, nx.link, nx.penok = pen, link, true
				}
				if kind == actPrint {
					if !st.decok || st.dec != dec {
						nx.cost += 3
					}
					nx.dec, nx.decok = dec, true
				}
				nx.cur = cur
				add(i+1, nx)
			}
			if !k.dirty {
				step(actSkip, st.pen, st.link, 0, st.cur, false, st.dec)
			}
			if k.dirty || (!k.blind && !s.pixels) {
				end := cursor{x: k.b, y: y, xok: k.b < s.w && !k.blind, yok: true}
				step(actPrint, k.style, k.link, pl.printCost(row, k), end, false, k.enc == encDEC)
			}
			if k.dirty && k.blank && k.link == 0 {
				blank := Style{Bg: k.style.Bg}
				at := cursor{x: k.a, y: y, xok: true, yok: true}
				step(actErase, blank, 0, len(csiN('X', k.b-k.a)), at, false, st.dec)
				if pl.clearable(i) {
					step(actClear, blank, 0, 3, at, true, st.dec)
				}
			}
		}
	}
	final = append(final, pl.states[n]...)
	for i := range final {
		if best < 0 || final[i].cost < final[best].cost {
			best = i
		}
	}
	end := final[best]
	plan := rowPlan{cost: end.cost, exit: end}
	for st, i := end, n; st.parent >= 0; {
		if st.act.kind != actSkip {
			plan.acts = append(plan.acts, st.act)
		}
		i = st.act.chunk
		st = pl.states[i][st.parent]
	}
	slices.Reverse(plan.acts)
	if len(plan.acts) > 0 {
		first := pl.chunks[plan.acts[0].chunk]
		plan.entry = cursor{x: first.a, y: y, xok: true, yok: true}
		plan.epen, plan.elink = plan.acts[0].pen, first.link
		if plan.acts[0].kind != actPrint {
			plan.elink = 0
		}
		plan.lead = motionCost(in.cur, first.a, y, s.h) + penCostFrom(in, plan.epen, plan.elink, s.links)
	}
	return plan, true
}

// Reports whether EL from chunk i leaves the row as wanted, every later column dirty blank in one background
func (pl *planner) clearable(i int) bool {
	bg := pl.chunks[i].style.Bg
	for _, k := range pl.chunks[i:] {
		if !k.dirty || !k.blank || k.link != 0 || k.style.Bg != bg {
			return false
		}
	}
	return true
}

// Bytes taking a plan state's pen and link to the ones given
func penCostFrom(st rowState, pen Style, link uint32, links *linkTable) int {
	n := 0
	if !st.penok || st.pen != pen {
		n += sgrCost(st.pen, st.penok, pen)
	}
	if !st.penok || st.link != link {
		k := links.list[link]
		n += 6 + len(k.params) + len(k.url)
	}
	return n
}

// Terminal state of the model as a plan state
func stateOf(s *screen) rowState {
	return rowState{cur: s.cursor(), pen: s.pen, link: s.link, penok: s.penok, dec: s.dec, decok: s.decok}
}

// Writes a row plan through the model
func (pl *planner) emitRow(s *screen, y int, row []want, plan rowPlan) {
	for _, a := range plan.acts {
		k := pl.chunks[a.chunk]
		s.moveTo(k.a, y)
		switch a.kind {
		case actPrint:
			s.setPen(a.pen, k.link)
			pl.runs(row, k, func(x, n int) {
				c := row[x]
				s.moveTo(x, y)
				if c.blind {
					s.printBlind(c.Cell)
					return
				}
				one := clusterCost(&c)
				if n > 1 && one+len(csiN('b', n-1)) < one*n {
					s.print(c.Cell, c.enc)
					s.repeat(n - 1)
					return
				}
				for i := 0; i < n; i++ {
					at := x + i*max(int(c.Width), 1)
					s.moveTo(at, y)
					s.print(row[at].Cell, row[at].enc)
					if row[at].enc == encText && fragile(row[at].Text) {
						s.xok = false
					}
				}
			})
		case actErase:
			s.setPen(a.pen, 0)
			s.eraseChars(k.b - k.a)
		case actClear:
			s.setPen(a.pen, 0)
			s.eraseLine()
		}
	}
}
