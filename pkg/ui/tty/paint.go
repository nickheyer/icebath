package tty

// Most rows a frame tour orders by exact search
const tourExact = 8

// Row a frame repaints, with the plan it was costed at
type job struct {
	y    int
	plan rowPlan
}

// Brings rows to the target, scrolling first then rows in the cheapest order, inside DECSC and DECRC when saving
func (pl *planner) paint(s *screen, tgt []want, end cursor, save bool) {
	mark := len(s.out)
	rest := stateOf(s)
	pl.emitScroll(s, pl.scrollPlan(s, tgt))
	start := stateOf(s)
	var jobs []job
	for y := 0; y < s.h; y++ {
		if plan, ok := pl.planRow(s, y, tgt[y*s.w:(y+1)*s.w], start); ok && len(plan.acts) > 0 {
			jobs = append(jobs, job{y, plan})
		}
	}
	if save && len(jobs) == 0 && len(s.out) == mark {
		return
	}
	if save {
		end = cursor{}
	}
	order := pl.tour(s, jobs, start, end)
	if save {
		pen := rest.pen
		if len(jobs) > 0 && !(rest.penok && usesPen(jobs, rest.pen)) {
			pen = jobs[order[0]].plan.epen
		}
		var prefix []byte
		if !rest.penok || rest.pen != pen {
			prefix = appendSGR(prefix, rest.pen, rest.penok, pen)
		}
		if !rest.penok || rest.link != 0 {
			prefix = s.links.appendOSC8(prefix, 0)
		}
		prefix = append(prefix, "\x1b7"...)
		s.out = append(s.out[:mark], append(prefix, s.out[mark:]...)...)
		s.pen, s.link, s.penok = pen, 0, true
		rest.pen, rest.link, rest.penok = pen, 0, true
	}
	for _, i := range order {
		y := jobs[i].y
		row := tgt[y*s.w : (y+1)*s.w]
		if plan, ok := pl.planRow(s, y, row, stateOf(s)); ok {
			pl.emitRow(s, y, row, plan)
		}
	}
	if save {
		if s.link != 0 {
			s.setPen(s.pen, 0)
		}
		s.raw("\x1b8")
		s.cx, s.cy, s.xok, s.yok = rest.cur.x, rest.cur.y, rest.cur.xok, rest.cur.yok
		s.pen, s.link, s.penok = rest.pen, 0, true
		s.dec, s.decok = rest.dec, rest.decok
	}
}

// Reports whether any row plan writes with the pen, the one worth keeping as the saved pen
func usesPen(jobs []job, pen Style) bool {
	for _, j := range jobs {
		for _, a := range j.plan.acts {
			if a.pen == pen {
				return true
			}
		}
	}
	return false
}

// Bytes going from the end of one row plan through the whole of another
func (pl *planner) hop(s *screen, from rowState, to rowPlan) int {
	return motionCost(from.cur, to.entry.x, to.entry.y, s.h) + penCostFrom(from, to.epen, to.elink, s.links) + to.cost - to.lead
}

// Order of rows, cheapest in motion and pen bytes by Held-Karp up to tourExact rows, top to bottom past that
func (pl *planner) tour(s *screen, jobs []job, start rowState, end cursor) []int {
	n := len(jobs)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if n <= 1 || n > tourExact {
		return order
	}
	full := 1<<n - 1
	inf := int(^uint(0) >> 2)
	dp := make([]int, (full+1)*n)
	parent := make([]int8, (full+1)*n)
	for i := range dp {
		dp[i] = inf
	}
	for i := range jobs {
		dp[(1<<i)*n+i] = pl.hop(s, start, jobs[i].plan)
		parent[(1<<i)*n+i] = -1
	}
	for mask := 1; mask <= full; mask++ {
		for last := 0; last < n; last++ {
			cur := dp[mask*n+last]
			if cur == inf || mask&(1<<last) == 0 {
				continue
			}
			for next := 0; next < n; next++ {
				if mask&(1<<next) != 0 {
					continue
				}
				m := mask | 1<<next
				c := cur + pl.hop(s, jobs[last].plan.exit, jobs[next].plan)
				if c < dp[m*n+next] {
					dp[m*n+next] = c
					parent[m*n+next] = int8(last)
				}
			}
		}
	}
	best, bestCost := 0, inf
	for last := 0; last < n; last++ {
		c := dp[full*n+last]
		if c == inf {
			continue
		}
		if end.xok {
			c += motionCost(jobs[last].plan.exit.cur, end.x, end.y, s.h)
		}
		if c < bestCost {
			best, bestCost = last, c
		}
	}
	mask := full
	for i := n - 1; i >= 0; i-- {
		order[i] = best
		p := int(parent[mask*n+best])
		mask &^= 1 << best
		best = p
	}
	return order
}
