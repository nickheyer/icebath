package sprint

// Size of the floating floor the team works on
const (
	floorW = 44.0
	floorD = 28.0
)

// Where the stations stand on the floor, x east and z south
var (
	whiteboardAt = Vec3{1, 0, 9}
	directorSpot = Vec3{4, 0, 9}
	roundTable   = Vec3{7.5, 0, 9}
	loungeSpot   = Vec3{7.5, 0, 12}
	planTable    = Vec3{18, 0, 3}
	architectAt  = Vec3{18, 0, 5.6}
	columnZ      = 9.0
	columnsX     = []float64{17, 22, 27}
	reviewDesk   = Vec3{37.5, 0, 10}
	reviewerAt   = Vec3{37.5, 0, 12.6}
	lampAt       = Vec3{40.3, 0, 8.6}
	scrapAt      = Vec3{41.5, 0, 13}
	plazaAt      = Vec3{22, 0, 14}
	corridorZ    = 14.0
	deskRowZ     = []float64{18.5, 24}
	deskStartX   = 9.0
	deskGapX     = 6.5
	doneColumn   = 1
	failColumn   = 2
	todoColumn   = 0
)

// Colors every surface of the floor paints with
var (
	colFloorA     = hexRGB(0x303b4b)
	colFloorB     = hexRGB(0x2a3442)
	colLoungeA    = hexRGB(0x423b37)
	colLoungeB    = hexRGB(0x3b3431)
	colWorkshopA  = hexRGB(0x27374a)
	colWorkshopB  = hexRGB(0x223042)
	colReviewA    = hexRGB(0x322e4c)
	colReviewB    = hexRGB(0x2c2943)
	colRim        = hexRGB(0x161c25)
	colCliff      = hexRGB(0x111620)
	colCliffDark  = hexRGB(0x0c1017)
	colWood       = hexRGB(0x8a6a4a)
	colWoodTop    = hexRGB(0xa0805c)
	colWoodDark   = hexRGB(0x5f4733)
	colFrame      = hexRGB(0x1c222b)
	colScreenOff  = hexRGB(0x182230)
	colBoard      = hexRGB(0xe6ebef)
	colBoardFrame = hexRGB(0x3a4350)
	colPedestal   = hexRGB(0x3b4858)
	colPedestalHi = hexRGB(0x4a5a6c)
	colSign       = hexRGB(0xd5dbe2)
	colPot        = hexRGB(0x7a4a33)
	colLeaf       = hexRGB(0x3f9a5a)
	colLeafDark   = hexRGB(0x2f7d47)
	colRack       = hexRGB(0x1b1f27)
	colRackHi     = hexRGB(0x2a3140)
	colLamp       = hexRGB(0xf2d58a)
	colLampPost   = hexRGB(0x2f3742)
	colScrap      = hexRGB(0x3a2a2e)
	colCoffee     = hexRGB(0x4a4f5a)
	colCoffeeHi   = hexRGB(0xc9d3dd)
	colMarker     = hexRGB(0x2f6fd6)
	colMarkerRed  = hexRGB(0xd6453f)
	colCrateTodo  = hexRGB(0x8c9097)
	colCrateWork  = hexRGB(0xe0a33a)
	colCrateRev   = hexRGB(0x5fa8ff)
	colCrateDone  = hexRGB(0x5cc98a)
	colCrateFail  = hexRGB(0xd04a5a)
	colCrateBlock = hexRGB(0x6b4f7a)
	colSkin       = hexRGB(0xf1c9a5)
	colEye        = hexRGB(0x1b1b24)
	colPants      = hexRGB(0x2d3440)
	colShoes      = hexRGB(0x1d222b)
	colHair       = hexRGB(0x3a2a22)
	colHardhat    = hexRGB(0xf2c14e)
	colVisor      = hexRGB(0x1f2a44)
	colVisorGlass = hexRGB(0x5ab4f0)
	colPaper      = hexRGB(0xf4f6f8)
)

// Direction the sun shines from, unit length
var sunDir = Vec3{-0.45, 1, 0.6}.Norm()

// Dye of an outfit color name
func dye(name string) RGB {
	switch name {
	case "teal":
		return hexRGB(0x2ec4b6)
	case "amber":
		return hexRGB(0xf0b429)
	case "violet":
		return hexRGB(0x9b6bff)
	case "lime":
		return hexRGB(0xa3d955)
	case "coral":
		return hexRGB(0xff7f66)
	case "sky":
		return hexRGB(0x5ab4f0)
	case "rose":
		return hexRGB(0xf07aa6)
	case "mint":
		return hexRGB(0x7fe0b8)
	case "crimson":
		return hexRGB(0xd62839)
	case "gold":
		return hexRGB(0xf2c14e)
	case "indigo":
		return hexRGB(0x5868d6)
	}
	return hexRGB(0x7f8c9b)
}

// Color a crate takes from the state of its ticket
func crateColor(status TicketStatus) RGB {
	switch status {
	case TicketActive:
		return colCrateWork
	case TicketReview:
		return colCrateRev
	case TicketDone:
		return colCrateDone
	case TicketFailed:
		return colCrateFail
	case TicketBlocked:
		return colCrateBlock
	}
	return colCrateTodo
}

// One voxel of the static floor
type voxel struct {
	col RGB
}

// Static voxels of the floor by integer cell
type grid map[[3]int]voxel

// Sets a voxel at a cell
func (g grid) set(x, y, z int, col RGB) { g[[3]int{x, y, z}] = voxel{col} }

// Fills a box of voxels from a least corner and a size in cells
func (g grid) box(x, y, z, w, h, d int, col RGB) {
	for i := range w {
		for j := range h {
			for k := range d {
				g.set(x+i, y+j, z+k, col)
			}
		}
	}
}

// Reports whether a cell holds a voxel
func (g grid) solid(x, y, z int) bool {
	_, ok := g[[3]int{x, y, z}]
	return ok
}

// Faces of every voxel not covered by a neighbor, each corner shaded by the voxels around it
func (g grid) mesh() []Quad {
	lights := sideLights(sunDir)
	out := make([]Quad, 0, len(g)*2)
	for cell, v := range g {
		x, y, z := cell[0], cell[1], cell[2]
		for side := range 6 {
			n := sideNormals[side]
			if g.solid(x+n[0], y+n[1], z+n[2]) {
				continue
			}
			q := faceQuad(x, y, z, side, v.col, lights[side])
			for c := range 4 {
				q.Light[c] *= 1 - 0.18*float64(g.occlusion(x, y, z, side, c))
			}
			out = append(out, q)
		}
	}
	return out
}

// Outward normal of each box side, top, bottom, north, south, west, east
var sideNormals = [6][3]int{{0, 1, 0}, {0, -1, 0}, {0, 0, -1}, {0, 0, 1}, {-1, 0, 0}, {1, 0, 0}}

// Corner offsets of each side in the order boxQuads lays them, as the two in plane axes of the corner
var sideCorners = [6][4][3]int{
	{{0, 1, 0}, {0, 1, 1}, {1, 1, 1}, {1, 1, 0}},
	{{0, 0, 1}, {0, 0, 0}, {1, 0, 0}, {1, 0, 1}},
	{{0, 0, 0}, {0, 1, 0}, {1, 1, 0}, {1, 0, 0}},
	{{1, 0, 1}, {1, 1, 1}, {0, 1, 1}, {0, 0, 1}},
	{{0, 0, 1}, {0, 1, 1}, {0, 1, 0}, {0, 0, 0}},
	{{1, 0, 0}, {1, 1, 0}, {1, 1, 1}, {1, 0, 1}},
}

// One face of a unit voxel at a cell
func faceQuad(x, y, z, side int, col RGB, light float64) Quad {
	var q Quad
	for c := range 4 {
		o := sideCorners[side][c]
		q.P[c] = Vec3{float64(x + o[0]), float64(y + o[1]), float64(z + o[2])}
		q.Light[c] = light
	}
	q.Color, q.Alpha = col, 1
	return q
}

// Voxels crowding a face corner, two along its edges and one diagonal, three when both edges are solid
func (g grid) occlusion(x, y, z, side, corner int) int {
	n := sideNormals[side]
	o := sideCorners[side][corner]
	// Direction of the corner along each in plane axis, toward the neighbor on that side
	dir := func(axis int) int {
		if n[axis] != 0 {
			return 0
		}
		if o[axis] == 1 {
			return 1
		}
		return -1
	}
	dx, dy, dz := dir(0), dir(1), dir(2)
	bx, by, bz := x+n[0], y+n[1], z+n[2]
	axes := [][3]int{}
	for _, d := range [][3]int{{dx, 0, 0}, {0, dy, 0}, {0, 0, dz}} {
		if d != [3]int{} {
			axes = append(axes, d)
		}
	}
	if len(axes) != 2 {
		return 0
	}
	s1 := g.solid(bx+axes[0][0], by+axes[0][1], bz+axes[0][2])
	s2 := g.solid(bx+axes[1][0], by+axes[1][1], bz+axes[1][2])
	if s1 && s2 {
		return 3
	}
	c := g.solid(bx+axes[0][0]+axes[1][0], by+axes[0][1]+axes[1][1], bz+axes[0][2]+axes[1][2])
	n2 := 0
	for _, b := range []bool{s1, s2, c} {
		if b {
			n2++
		}
	}
	return n2
}

// Desk of the builder at an index, its center
func deskAt(index int) Vec3 {
	row := index / 4
	if row >= len(deskRowZ) {
		row = len(deskRowZ) - 1
	}
	return Vec3{deskStartX + deskGapX*float64(index%4), 0, deskRowZ[row]}
}

// Spot a builder stands at to work at its desk
func builderSpot(index int) Vec3 {
	d := deskAt(index)
	return Vec3{d.X, 0, d.Z + 1.9}
}

// Center of the top of a kanban column
func columnTop(i int) Vec3 { return Vec3{columnsX[i] + 0.5, 1, columnZ + 0.5} }

// Floor color of a tile by zone, a two tone check with a touch of noise
func floorColor(x, z int) RGB {
	a, b := colFloorA, colFloorB
	switch {
	case x < 11 && z >= 4 && z < 14:
		a, b = colLoungeA, colLoungeB
	case z >= 16 && x >= 6 && x < 32:
		a, b = colWorkshopA, colWorkshopB
	case x >= 33 && z >= 6 && z < 16:
		a, b = colReviewA, colReviewB
	}
	c := a
	if (x+z)%2 == 1 {
		c = b
	}
	n := hash2(x, z)
	return c.Scale(0.96 + 0.08*n)
}

// Pseudo random share from 0 to 1 for a tile
func hash2(x, z int) float64 {
	h := uint32(x)*0x9e3779b1 ^ uint32(z)*0x85ebca77
	h ^= h >> 15
	h *= 0x2c1b3c6d
	h ^= h >> 12
	return float64(h&0xffff) / 0xffff
}

// Static floor, walls, furniture and decor as voxels
func buildIsland(builders int) grid {
	g := grid{}
	w, d := int(floorW), int(floorD)
	for x := range w {
		for z := range d {
			g.set(x, -1, z, floorColor(x, z))
		}
	}
	for x := range w {
		for z := range d {
			edge := x == 0 || z == 0 || x == w-1 || z == d-1
			if edge {
				g.set(x, -1, z, colRim)
			}
			inset := func(k int) bool { return x >= k && z >= k && x < w-k && z < d-k }
			if inset(1) && !inset(2) || inset(3) && !inset(4) && hash2(x, z) > 0.4 {
				g.set(x, -2, z, colCliff)
			}
			if inset(5) && !inset(7) && hash2(z, x) > 0.55 {
				g.set(x, -3, z, colCliffDark)
			}
		}
	}
	// Whiteboard on its frame, with a few marker strokes
	g.box(int(whiteboardAt.X), 0, int(whiteboardAt.Z)-3, 1, 4, 7, colBoardFrame)
	for z := -2; z <= 2; z++ {
		for y := 1; y <= 3; y++ {
			g.set(int(whiteboardAt.X)+1, y, int(whiteboardAt.Z)+z, colBoard)
		}
	}
	g.set(int(whiteboardAt.X)+1, 3, int(whiteboardAt.Z)-1, colMarker)
	g.set(int(whiteboardAt.X)+1, 3, int(whiteboardAt.Z), colMarker)
	g.set(int(whiteboardAt.X)+1, 2, int(whiteboardAt.Z)+1, colMarkerRed)
	g.set(int(whiteboardAt.X)+1, 1, int(whiteboardAt.Z)-1, colMarker)
	// Round table in the lounge
	tx, tz := int(roundTable.X), int(roundTable.Z)
	g.box(tx-1, 0, tz-1, 2, 1, 2, colWoodDark)
	g.box(tx-2, 1, tz-2, 4, 1, 4, colWoodTop)
	g.set(tx-2, 1, tz-2, colWood)
	g.set(tx+1, 1, tz-2, colWood)
	g.set(tx-2, 1, tz+1, colWood)
	g.set(tx+1, 1, tz+1, colWood)
	// Planning table with a blueprint on it
	px, pz := int(planTable.X), int(planTable.Z)
	g.box(px-3, 0, pz-1, 1, 1, 2, colWoodDark)
	g.box(px+2, 0, pz-1, 1, 1, 2, colWoodDark)
	g.box(px-3, 1, pz-2, 6, 1, 4, colWoodTop)
	g.box(px-2, 2, pz-1, 4, 1, 2, hexRGB(0x3c6fd0))
	g.set(px-1, 2, pz-1, colPaper)
	g.set(px, 2, pz, colPaper)
	// Kanban columns, a pedestal under each
	for _, cx := range columnsX {
		x, z := int(cx), int(columnZ)
		g.box(x-1, 0, z-1, 3, 1, 3, colPedestal)
		g.set(x-1, 0, z-1, colPedestalHi)
		g.set(x+1, 0, z+1, colPedestalHi)
		g.box(x, 1, z-2, 1, 4, 1, colLampPost)
		g.box(x-1, 5, z-2, 3, 1, 1, colSign)
	}
	// Builder desks with a monitor frame each, the screen drawn live
	for i := range max(builders, 1) {
		dk := deskAt(i)
		x, z := int(dk.X), int(dk.Z)
		g.box(x-1, 0, z-1, 1, 1, 1, colWoodDark)
		g.box(x+1, 0, z-1, 1, 1, 1, colWoodDark)
		g.box(x-2, 1, z-1, 4, 1, 2, colWoodTop)
		g.box(x-1, 2, z-1, 2, 2, 1, colFrame)
	}
	// Review desk, lamp and scrap bin
	rx, rz := int(reviewDesk.X), int(reviewDesk.Z)
	g.box(rx-2, 0, rz-1, 1, 1, 2, colWoodDark)
	g.box(rx+1, 0, rz-1, 1, 1, 2, colWoodDark)
	g.box(rx-2, 1, rz-2, 5, 1, 3, colWoodTop)
	g.box(rx+2, 2, rz-2, 1, 1, 1, colPaper)
	g.box(int(lampAt.X), 0, int(lampAt.Z), 1, 5, 1, colLampPost)
	g.box(int(lampAt.X)-1, 5, int(lampAt.Z)-1, 3, 1, 3, colLampPost)
	g.box(int(scrapAt.X)-1, 0, int(scrapAt.Z)-1, 2, 2, 2, colScrap)
	// Plants in the corners and a coffee machine by the lounge
	for _, p := range [][2]int{{2, 2}, {w - 3, 2}, {2, d - 3}, {w - 3, d - 3}} {
		g.box(p[0], 0, p[1], 1, 1, 1, colPot)
		g.box(p[0]-1, 1, p[1]-1, 3, 1, 3, colLeafDark)
		g.box(p[0], 2, p[1], 1, 1, 1, colLeaf)
		g.set(p[0]-1, 2, p[1], colLeaf)
		g.set(p[0]+1, 2, p[1], colLeaf)
		g.set(p[0], 3, p[1], colLeaf)
	}
	g.box(2, 0, 20, 2, 3, 1, colCoffee)
	g.set(2, 3, 20, colCoffeeHi)
	// Server rack along the east edge, lights drawn live
	g.box(w-2, 0, 19, 1, 4, 5, colRack)
	g.box(w-2, 4, 19, 1, 1, 5, colRackHi)
	return g
}
