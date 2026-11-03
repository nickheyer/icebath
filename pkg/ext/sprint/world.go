package sprint

import (
	"container/heap"
	"math"
	"slices"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Spacing of the nodes across a row and between rows, and the most nodes one row holds
const (
	nodePitchX = 7
	nodePitchZ = 8
	rowNodes   = 5
	// Rows of the landing south of the fort where the crew gathers
	landingDepth = 9
	// Height of the reviewer spire
	spireHeight = 5
	// Rows the blackwall stands north of the fort
	wallGap = 7
)

// Colors every static surface of the dive paints with
var (
	colCliff     = vxl.Hex(0x0f1420)
	colConduit   = vxl.Hex(0x1b3140)
	colPedestal  = vxl.Hex(0x1a2434)
	colPedestalH = vxl.Hex(0x2c3c52)
	colPad       = vxl.Hex(0x2d3f57)
	colPadHi     = vxl.Hex(0x4a6a8c)
	colPillar    = vxl.Hex(0x202a38)
	colTable     = vxl.Hex(0x1c2533)
	colTableTop  = vxl.Hex(0x2a3748)
	colTower     = vxl.Hex(0x171f2b)
	colTowerHi   = vxl.Hex(0x29364a)
	colCradle    = vxl.Hex(0x1f2b3a)
	colIce       = vxl.Hex(0x9cc7e8)
	colIceDeep   = vxl.Hex(0x4a86b8)
	colPaper     = vxl.Hex(0xf4f6f8)
	colSkin      = vxl.Hex(0xe8c3a0)
	colSkinDark  = vxl.Hex(0xb08a6a)
	colDark      = vxl.Hex(0x12161d)
	colPants     = vxl.Hex(0x242b36)
	colEye       = vxl.Hex(0x0b0d12)
	colRed       = vxl.Hex(0xe3122b)
	colAmber     = vxl.Hex(0xe0b35a)
	colGreen     = vxl.Hex(0x7fc7a5)
	colViolet    = vxl.Hex(0x7a5aa8)
	colNodeTodo  = vxl.Hex(0x5a7fa8)
	colNodeWork  = vxl.Hex(0xe0b35a)
	colNodeRev   = vxl.Hex(0xf0e6c8)
	colNodeDone  = vxl.Hex(0x7fe0b8)
	colNodeFail  = vxl.Hex(0xff4757)
	colNodeBlock = vxl.Hex(0x6b4f8a)
)

// Dye of an outfit color name
func dye(name string) vxl.RGB {
	switch name {
	case "teal":
		return vxl.Hex(0x2ec4b6)
	case "amber":
		return vxl.Hex(0xf0b429)
	case "violet":
		return vxl.Hex(0x9b6bff)
	case "lime":
		return vxl.Hex(0xa3d955)
	case "coral":
		return vxl.Hex(0xff7f66)
	case "sky":
		return vxl.Hex(0x5ab4f0)
	case "rose":
		return vxl.Hex(0xf07aa6)
	case "mint":
		return vxl.Hex(0x7fe0b8)
	case "crimson":
		return vxl.Hex(0xd62839)
	case "gold":
		return vxl.Hex(0xf2c14e)
	case "indigo":
		return vxl.Hex(0x5868d6)
	}
	return vxl.Hex(0x7f8c9b)
}

// Color a node takes from the state of its ticket
func nodeColor(status TicketStatus) vxl.RGB {
	switch status {
	case TicketActive:
		return colNodeWork
	case TicketReview:
		return colNodeRev
	case TicketDone:
		return colNodeDone
	case TicketFailed:
		return colNodeFail
	case TicketBlocked:
		return colNodeBlock
	}
	return colNodeTodo
}

// Platform of a dive, everything placed from the plan of a sprint
type dive struct {
	// Platform extents, x from 0 to w and z from 0 to d, the fort north of the landing
	w, d  int
	fortD int
	// Node pedestal centers by ticket id and the level of each
	nodes  map[string]vxl.Vec3
	levels map[string]int
	// Tickets by level, nearest the landing first, in plan order
	byLevel [][]string
	deps    map[string][]string
	uplink  vxl.Vec3
	chart   vxl.Vec3
	spire   vxl.Vec3
	cradles []vxl.Vec3
	perches []perch
	wallZ   int
	tiles   map[[2]int]bool
	blocked map[[2]int]bool
	// Floor cells conduits run through, by dependent then dependency
	conduits map[[2]string][][2]int
	// Straight stretches of each conduit, one face apiece
	runs map[[2]string][]conduitRun
}

// Straight conduit stretch between two floor cell centers and how many cells along the conduit each lies
type conduitRun struct {
	a, b   [2]int
	sa, sb float64
}

// Spot on the edge of the platform and the way out faces from it
type perch struct {
	at  vxl.Vec3
	yaw float64
}

// Ticket as the dive needs it
type planNode struct {
	id   string
	deps []string
}

// Layout for a plan and a count of builders, a small dormant fort when the plan is empty
func buildDive(plan []planNode, builders int) dive {
	lay := dive{nodes: map[string]vxl.Vec3{}, levels: map[string]int{}, deps: map[string][]string{}, tiles: map[[2]int]bool{}, blocked: map[[2]int]bool{}, conduits: map[[2]string][][2]int{}, runs: map[[2]string][]conduitRun{}}
	known := map[string]bool{}
	for _, n := range plan {
		known[n.id] = true
	}
	for _, n := range plan {
		for _, d := range n.deps {
			if known[d] && d != n.id {
				lay.deps[n.id] = append(lay.deps[n.id], d)
			}
		}
	}
	var level func(id string, depth int) int
	level = func(id string, depth int) int {
		if l, ok := lay.levels[id]; ok {
			return l
		}
		l := 0
		if depth < 64 {
			for _, d := range lay.deps[id] {
				l = max(l, level(d, depth+1)+1)
			}
		}
		lay.levels[id] = l
		return l
	}
	depth := 0
	for _, n := range plan {
		depth = max(depth, level(n.id, 0)+1)
	}
	depth = max(depth, 1)
	lay.byLevel = make([][]string, depth)
	for _, n := range plan {
		l := lay.levels[n.id]
		lay.byLevel[l] = append(lay.byLevel[l], n.id)
	}
	cols, rows := 3, 0
	for _, ids := range lay.byLevel {
		cols = max(cols, min(len(ids), rowNodes))
		rows += levelRows(len(ids))
	}
	lay.w = cols*nodePitchX + 6
	lay.fortD = 4 + rows*nodePitchZ
	lay.d = lay.fortD + landingDepth
	row := 0
	for _, ids := range lay.byLevel {
		n := levelRows(len(ids))
		next := 0
		for k := range n {
			count := len(ids) / n
			if k < len(ids)%n {
				count++
			}
			z := float64(lay.fortD-4-row*nodePitchZ) + 0.5
			for i, id := range ids[next : next+count] {
				x := float64(lay.w)/2 + (float64(i)-float64(count-1)/2)*nodePitchX
				lay.nodes[id] = vxl.V(math.Floor(x)+0.5, 0, z)
			}
			next += count
			row++
		}
	}
	lay.uplink = vxl.V(math.Floor(float64(lay.w)/2)+0.5, 0, float64(lay.d-4)+0.5)
	lay.chart = vxl.V(5.5, 0, float64(lay.d-5)+0.5)
	lay.spire = vxl.V(math.Floor(float64(lay.w)/2)+0.5, 0, 1.5)
	lay.wallZ = -wallGap
	for i := range max(builders, 1) {
		x := float64(lay.w-3) - float64(i)*3
		if x < 10 {
			x = 10 + float64(i%4)*2.5
		}
		lay.cradles = append(lay.cradles, vxl.V(math.Floor(x)+0.5, 0, float64(lay.d-2)+0.5))
	}
	for x := range lay.w {
		for z := range lay.d {
			lay.tiles[[2]int{x, z}] = true
		}
	}
	block := func(x, z, w, d int) {
		for i := range w {
			for j := range d {
				lay.blocked[[2]int{x + i, z + j}] = true
			}
		}
	}
	for _, p := range lay.nodes {
		block(int(p.X)-1, int(p.Z)-1, 3, 3)
	}
	block(int(lay.spire.X)-1, int(lay.spire.Z)-1, 3, 3)
	block(int(lay.chart.X)-2, int(lay.chart.Z)-1, 5, 3)
	block(int(lay.uplink.X)-1, int(lay.uplink.Z)-1, 3, 3)
	for z := 3; z < lay.d-3; z += 6 {
		lay.perches = append(lay.perches, perch{at: vxl.V(0.5, 0, float64(z)+0.5), yaw: -math.Pi / 2}, perch{at: vxl.V(float64(lay.w)-0.5, 0, float64(z)+0.5), yaw: math.Pi / 2})
	}
	for x := 3; x < lay.w-3; x += 7 {
		if math.Abs(float64(x)-lay.uplink.X) > 4 && math.Abs(float64(x)-lay.chart.X) > 4 {
			lay.perches = append(lay.perches, perch{at: vxl.V(float64(x)+0.5, 0, float64(lay.d)-0.5), yaw: 0})
		}
	}
	keep := map[[2]int]bool{}
	for _, p := range lay.perches {
		keep[[2]int{int(p.at.X), int(p.at.Z)}] = true
	}
	for _, c := range lay.cradles {
		keep[[2]int{int(c.X), int(c.Z)}] = true
		keep[[2]int{int(c.X) + 1, int(c.Z)}] = true
	}
	for x := range lay.w {
		for z := range lay.d {
			ring := min(x, z, lay.w-1-x, lay.d-1-z)
			cell := [2]int{x, z}
			if keep[cell] || lay.blocked[cell] {
				continue
			}
			if ring == 0 && vxl.Hash2(x, z) > 0.72 || ring == 1 && vxl.Hash2(z, x) > 0.93 {
				delete(lay.tiles, cell)
			}
		}
	}
	for id, deps := range lay.deps {
		to := lay.nodes[id]
		for _, d := range deps {
			key := [2]string{id, d}
			lay.conduits[key] = lay.route(lay.nodes[d], to)
			lay.runs[key] = straightRuns(lay.conduits[key])
		}
	}
	return lay
}

// Rows a level of a count of nodes wraps onto, one for an empty level
func levelRows(n int) int { return max((n+rowNodes-1)/rowNodes, 1) }

// Floor cells of a conduit from a dependency north to its dependent, its long leg down a pedestal free lane
func (lay *dive) route(from, to vxl.Vec3) [][2]int {
	fx, fz, tx, tz := int(from.X), int(from.Z), int(to.X), int(to.Z)
	south, north := fz-2, tz+3
	lane := lay.lane(fx, tx, north, south)
	cells := vxl.LineCells(fx, south, lane, south)
	cells = append(cells, vxl.LineCells(lane, south, lane, north)...)
	cells = append(cells, vxl.LineCells(lane, north, tx, north)...)
	cells = append(cells, vxl.LineCells(tx, north, tx, tz+2)...)
	return slices.Compact(cells)
}

// Column nearest a start, then nearest an end, whose cells between two rows hold no pedestal
func (lay *dive) lane(from, to, z0, z1 int) int {
	best, bestD := from, math.MaxInt
	for x := range lay.w {
		open := true
		for z := z0; z <= z1 && open; z++ {
			open = !lay.blocked[[2]int{x, z}]
		}
		if d := 2*lay.w*max(x-from, from-x) + max(x-to, to-x); open && d < bestD {
			best, bestD = x, d
		}
	}
	return best
}

// Stretches of cells running one way, each from the cell it turns at to the cell of its next turn
func straightRuns(cells [][2]int) []conduitRun {
	var out []conduitRun
	for i := 0; i+1 < len(cells); {
		step := [2]int{cells[i+1][0] - cells[i][0], cells[i+1][1] - cells[i][1]}
		j := i + 1
		for j+1 < len(cells) && [2]int{cells[j+1][0] - cells[j][0], cells[j+1][1] - cells[j][1]} == step {
			j++
		}
		out = append(out, conduitRun{a: cells[i], b: cells[j], sa: float64(i), sb: float64(j)})
		i = j
	}
	if len(cells) == 1 {
		out = append(out, conduitRun{a: cells[0], b: cells[0]})
	}
	return out
}

// Reports whether a tile exists and nothing stands on it
func (lay *dive) walkable(x, z int) bool {
	c := [2]int{x, z}
	return lay.tiles[c] && !lay.blocked[c]
}

// Reports whether a spot is on a tile of the platform
func (lay *dive) onPlatform(p vxl.Vec3) bool {
	return lay.tiles[[2]int{int(math.Floor(p.X)), int(math.Floor(p.Z))}]
}

// Center of the walkable tile nearest a spot, searching outward
func (lay *dive) nearest(p vxl.Vec3) vxl.Vec3 {
	x, z := int(math.Floor(p.X)), int(math.Floor(p.Z))
	if lay.walkable(x, z) {
		return p
	}
	for r := 1; r < 12; r++ {
		best, found, bestD := vxl.Vec3{}, false, math.Inf(1)
		for dx := -r; dx <= r; dx++ {
			for dz := -r; dz <= r; dz++ {
				if max(dx, -dx, dz, -dz) != r || !lay.walkable(x+dx, z+dz) {
					continue
				}
				c := vxl.V(float64(x+dx)+0.5, 0, float64(z+dz)+0.5)
				if d := c.Flat(p); d < bestD {
					best, found, bestD = c, true, d
				}
			}
		}
		if found {
			return best
		}
	}
	return p
}

// Spot a builder stands at to work at a node, west of it, and the yaw toward the node
func (lay *dive) jackSpot(id string) (vxl.Vec3, float64) {
	n := lay.nodes[id]
	return lay.nearest(vxl.V(n.X-2.5, 0, n.Z+0.5)), math.Pi / 2
}

// Spot beside a node to look at it from, picked by a share from 0 to 1
func (lay *dive) viewSpot(id string, share float64) (vxl.Vec3, float64) {
	n := lay.nodes[id]
	ang := share * 2 * math.Pi
	at := lay.nearest(n.Add(vxl.V(3.2*math.Sin(ang), 0, 3.2*math.Cos(ang))))
	return at, vxl.HeadingOf(n.Sub(at))
}

// Spot behind the uplink pad where the director addresses the landing, facing the pad and the crew beyond it
func (lay *dive) podium() (vxl.Vec3, float64) {
	return lay.nearest(lay.uplink.Add(vxl.V(0, 0, -2.5))), 0
}

// Spot on the landing in front of the pad where the crew steps up from to jack out
func (lay *dive) padFoot() vxl.Vec3 {
	return lay.nearest(lay.uplink.Add(vxl.V(0, 0, 2.5)))
}

// Top of the uplink pad where a member stands to jack out
func (lay *dive) padTop() vxl.Vec3 { return lay.uplink.Add(vxl.V(0, 1, 0)) }

// Nearest walkable spot on the floor under or beside a point, however high it is
func (lay *dive) ground(p vxl.Vec3) vxl.Vec3 {
	q := lay.nearest(vxl.V(p.X, 0, p.Z))
	q.Y = 0
	return q
}

// Spot in front of the spire where the reviewer lands when it leaves the tower
func (lay *dive) spireFoot() vxl.Vec3 {
	return lay.nearest(lay.spire.Add(vxl.V(0, 0, 3.5)))
}

// Where the reviewer stands on top of the spire
func (lay *dive) spireTop() vxl.Vec3 { return lay.spire.Add(vxl.V(0, spireHeight, 0)) }

// Cradle of a builder by index
func (lay *dive) cradle(index int) vxl.Vec3 {
	if len(lay.cradles) == 0 {
		return lay.uplink
	}
	return lay.cradles[index%len(lay.cradles)]
}

// Random walkable spot within a reach of a point
func (lay *dive) roam(r *vxl.RNG, from vxl.Vec3, reach float64) vxl.Vec3 {
	for range 12 {
		p := from.Add(vxl.V(r.Spread()*reach, 0, r.Spread()*reach))
		x, z := int(math.Floor(p.X)), int(math.Floor(p.Z))
		if lay.walkable(x, z) {
			return vxl.V(float64(x)+0.5, 0, float64(z)+0.5)
		}
	}
	return lay.nearest(from)
}

// Box around the platform and the wall for the camera to fit
func (lay *dive) bounds() (lo, hi vxl.Vec3) {
	return vxl.V(-1, -2, -1), vxl.V(float64(lay.w)+1, 6, float64(lay.d)+1)
}

// Center of the platform
func (lay *dive) center() vxl.Vec3 { return vxl.V(float64(lay.w)/2, 1.5, float64(lay.d)/2) }

// Voxels the stations of every world are built from
type materials struct {
	conduit, pedestal, pedestalHi, socket, pad, padHi, pillar, table, tableTop, tower, towerHi, cradle vxl.Voxel
}

// Conduits, pedestals, the uplink pad, the chart table, the spire and the cradles as voxels of the materials, over tiles already laid
func (lay *dive) stations(g vxl.Grid, m materials) {
	for _, cells := range lay.conduits {
		for _, c := range cells {
			if lay.tiles[c] {
				g.Put(c[0], -1, c[1], m.conduit)
			}
		}
	}
	for _, p := range lay.nodes {
		x, z := int(p.X), int(p.Z)
		g.Fill(x-1, 0, z-1, 3, 1, 3, m.pedestal)
		for _, c := range [][2]int{{x - 1, z - 1}, {x + 1, z - 1}, {x - 1, z + 1}, {x + 1, z + 1}} {
			g.Put(c[0], 0, c[1], m.pedestalHi)
		}
		g.Del(x, 0, z)
		g.Put(x, -1, z, m.socket)
	}
	ux, uz := int(lay.uplink.X), int(lay.uplink.Z)
	g.Fill(ux-1, 0, uz-1, 3, 1, 3, m.pad)
	g.Put(ux, 0, uz, m.padHi)
	g.Fill(ux, 1, uz+1, 1, 3, 1, m.pillar)
	g.Put(ux, 4, uz+1, m.padHi)
	cx, cz := int(lay.chart.X), int(lay.chart.Z)
	g.Fill(cx-2, 0, cz-1, 1, 1, 3, m.table)
	g.Fill(cx+2, 0, cz-1, 1, 1, 3, m.table)
	g.Fill(cx-2, 1, cz-1, 5, 1, 3, m.tableTop)
	sx, sz := int(lay.spire.X), int(lay.spire.Z)
	g.Fill(sx-1, 0, sz-1, 3, spireHeight, 3, m.tower)
	for y := range spireHeight {
		for _, c := range [][2]int{{sx - 1, sz - 1}, {sx + 1, sz - 1}, {sx - 1, sz + 1}, {sx + 1, sz + 1}} {
			if (y+c[0]+c[1])%2 == 0 {
				g.Put(c[0], y, c[1], m.towerHi)
			}
		}
	}
	g.Fill(sx-1, spireHeight, sz-1, 3, 1, 3, m.towerHi)
	g.Fill(sx, spireHeight+1, sz-1, 1, 2, 1, m.pillar)
	for _, c := range lay.cradles {
		x, z := int(c.X), int(c.Z)
		g.Fill(x, -1, z, 2, 1, 1, m.cradle)
	}
}

// Ring of a tile from the edge of the platform, zero on the edge
func (lay *dive) ring(x, z int) int { return min(x, z, lay.w-1-x, lay.d-1-z) }

// Reports whether a tile lies on the edge of the platform or beside a missing tile
func (lay *dive) edge(x, z int) bool {
	return lay.ring(x, z) == 0 || !lay.tiles[[2]int{x - 1, z}] || !lay.tiles[[2]int{x + 1, z}] || !lay.tiles[[2]int{x, z - 1}] || !lay.tiles[[2]int{x, z + 1}]
}

// Lays every tile of the platform from a function of its cell and whether it sits in the fort
func (lay *dive) floor(g vxl.Grid, tile func(x, z int, fort bool) vxl.Voxel) {
	for cell := range lay.tiles {
		x, z := cell[0], cell[1]
		g.Put(x, -1, z, tile(x, z, z < lay.fortD))
	}
}

// Rims every edge tile and hangs a broken cliff of voxels under the edges, deeper where the hash falls
func (lay *dive) cliff(g vxl.Grid, rim, cliff, cliffDark vxl.Voxel, depth int) {
	for cell := range lay.tiles {
		x, z := cell[0], cell[1]
		switch {
		case lay.edge(x, z):
			g.Put(x, -1, z, rim)
			g.Put(x, -2, z, cliff)
			for y := 3; y <= depth; y++ {
				if vxl.Hash2(z*y, x) > 0.35+0.1*float64(y) {
					g.Put(x, -y, z, cliffDark)
				}
			}
		case lay.ring(x, z) < 3 && vxl.Hash2(x, z*5) > 0.6:
			g.Put(x, -2, z, cliff)
		}
	}
}

// Static voxels of the platform in a world, its terrain first and the stations over it
func (lay *dive) build(w world) vxl.Grid {
	g := vxl.Grid{}
	w.terrain(lay, g)
	lay.stations(g, w.materials())
	return g
}

// Node of the search over tiles
type pathNode struct {
	cell  [2]int
	f     float64
	index int
}

type pathHeap []*pathNode

func (h pathHeap) Len() int           { return len(h) }
func (h pathHeap) Less(i, j int) bool { return h[i].f < h[j].f }
func (h pathHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index, h[j].index = i, j }
func (h *pathHeap) Push(x any)        { n := x.(*pathNode); n.index = len(*h); *h = append(*h, n) }
func (h *pathHeap) Pop() any          { old := *h; n := old[len(old)-1]; *h = old[:len(old)-1]; return n }
func cellOf(p vxl.Vec3) [2]int        { return [2]int{int(math.Floor(p.X)), int(math.Floor(p.Z))} }
func centerOf(c [2]int) vxl.Vec3      { return vxl.V(float64(c[0])+0.5, 0, float64(c[1])+0.5) }
func octile(a, b [2]int) float64 {
	dx, dz := math.Abs(float64(a[0]-b[0])), math.Abs(float64(a[1]-b[1]))
	return max(dx, dz) + 0.41*min(dx, dz)
}
func (lay *dive) free(c [2]int) bool { return lay.walkable(c[0], c[1]) }

// Waypoints across the platform from a spot to another, corners cut where the way is clear, none when unreachable
func (lay *dive) path(from, to vxl.Vec3) []vxl.Vec3 {
	start, goal := cellOf(from), cellOf(lay.nearest(to))
	if !lay.free(start) {
		start = cellOf(lay.nearest(from))
	}
	if start == goal {
		return []vxl.Vec3{to}
	}
	came := map[[2]int][2]int{}
	g := map[[2]int]float64{start: 0}
	open := &pathHeap{}
	heap.Push(open, &pathNode{cell: start, f: octile(start, goal)})
	closed := map[[2]int]bool{}
	found := false
	for open.Len() > 0 && len(closed) < 20000 {
		cur := heap.Pop(open).(*pathNode)
		if cur.cell == goal {
			found = true
			break
		}
		if closed[cur.cell] {
			continue
		}
		closed[cur.cell] = true
		for dx := -1; dx <= 1; dx++ {
			for dz := -1; dz <= 1; dz++ {
				if dx == 0 && dz == 0 {
					continue
				}
				n := [2]int{cur.cell[0] + dx, cur.cell[1] + dz}
				if !lay.free(n) || closed[n] {
					continue
				}
				if dx != 0 && dz != 0 && (!lay.free([2]int{cur.cell[0] + dx, cur.cell[1]}) || !lay.free([2]int{cur.cell[0], cur.cell[1] + dz})) {
					continue
				}
				step := 1.0
				if dx != 0 && dz != 0 {
					step = 1.41
				}
				ng := g[cur.cell] + step
				if have, ok := g[n]; ok && ng >= have {
					continue
				}
				g[n], came[n] = ng, cur.cell
				heap.Push(open, &pathNode{cell: n, f: ng + octile(n, goal)})
			}
		}
	}
	if !found {
		return nil
	}
	cells := [][2]int{goal}
	for c := goal; c != start; {
		c = came[c]
		cells = append(cells, c)
	}
	slices.Reverse(cells)
	out := []vxl.Vec3{}
	anchor := 0
	for i := 2; i < len(cells); i++ {
		if !lay.clear(cells[anchor], cells[i]) {
			out = append(out, centerOf(cells[i-1]))
			anchor = i - 1
		}
	}
	return append(out, to)
}

// Reports whether every tile the straight line between two cell centers touches is walkable
func (lay *dive) clear(a, b [2]int) bool {
	from, to := centerOf(a), centerOf(b)
	steps := max(int(from.Flat(to)*5), 1)
	for i := 0; i <= steps; i++ {
		p := from.Lerp(to, float64(i)/float64(steps))
		for _, off := range [4][2]float64{{-0.3, -0.3}, {0.3, -0.3}, {-0.3, 0.3}, {0.3, 0.3}} {
			if !lay.free(cellOf(p.Add(vxl.V(off[0], 0, off[1])))) {
				return false
			}
		}
	}
	return true
}
