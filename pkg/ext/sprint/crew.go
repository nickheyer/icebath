package sprint

import (
	"math"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Rigs a member may wear on its head
var RigNames = []string{"visor", "halo", "antenna", "hood", "crest", "mask", "horns", "plugs", "none"}

// Rig a hat name of the first arena stood for, so saved outfits still dress the crew
var hatRigs = map[string]string{"cap": "crest", "beanie": "hood", "fedora": "hood", "hardhat": "mask", "headphones": "plugs", "bandana": "crest", "crown": "halo"}

// World units one voxel of a figure spans
const figureScale = 0.25

// Rows of the head art
const headRows = 5

// Figure under construction, parts placed in model space and hung on their parents
type figure struct {
	m      *vxl.Model
	origin map[string]vxl.Vec3
}

// Adds a part at a model space position, its origin taken relative to its parent
func (f *figure) part(name, parent string, at, pivot vxl.Vec3, layers []string, rows int) {
	rel := at
	if parent != "" {
		rel = at.Sub(f.origin[parent])
	}
	f.m.Add(name, rel, pivot, layers, rows)
	if parent != "" {
		f.m.Link(name, parent)
	}
	f.origin[name] = at
}

// Layers of a box of one key
func solidLayers(w, h, d int, key rune) []string {
	row := strings.Repeat(string(key), w)
	out := make([]string, 0, h*d)
	for range d {
		for range h {
			out = append(out, row)
		}
	}
	return out
}

// Layers of the head of a figure wearing a rig, hair at the back and the face in front
func headLayers(rig string) []string {
	back := []string{".kkk.", "kkkkk", "kkkkk", "kkkkk", ".SSS."}
	mid := []string{".kkk.", "ksssk", "sssss", "sssss", ".SSS."}
	front := []string{".kkk.", "ksssk", "seses", "sssss", ".SSS."}
	layers := [][]rune{}
	for _, src := range [][]string{back, back, mid, mid, front} {
		for _, row := range src {
			layers = append(layers, []rune(row))
		}
	}
	at := func(z, y, x int) *rune { return &layers[z*headRows+(headRows-1-y)][x] }
	switch rig {
	case "visor":
		for x := range 5 {
			*at(4, 2, x) = 'v'
		}
		for z := 2; z < 4; z++ {
			*at(z, 2, 0), *at(z, 2, 4) = 'v', 'v'
		}
	case "mask":
		for y := range 5 {
			for x := range 5 {
				if *at(4, y, x) != '.' {
					*at(4, y, x) = 'k'
				}
			}
		}
		*at(4, 2, 1), *at(4, 2, 3) = 'v', 'v'
		for z := 2; z < 4; z++ {
			for y := 1; y < 4; y++ {
				*at(z, y, 0), *at(z, y, 4) = 'k', 'k'
			}
		}
	case "hood":
		for z := range 4 {
			for y := range 5 {
				for x := range 5 {
					*at(z, y, x) = 'k'
				}
			}
		}
		for x := range 5 {
			*at(4, 4, x), *at(4, 0, x) = 'k', 'k'
		}
		for y := 1; y < 4; y++ {
			*at(4, y, 0), *at(4, y, 4) = 'k', 'k'
		}
		*at(4, 2, 1), *at(4, 2, 3) = 'e', 'e'
	case "plugs":
		for y := 1; y < 3; y++ {
			*at(2, y, 0), *at(2, y, 4) = 'v', 'v'
		}
	}
	out := make([]string, 0, len(layers))
	for _, row := range layers {
		out = append(out, string(row))
	}
	return out
}

// Layers of the torso, a trace line down the chest
func torsoLayers(role Role) []string {
	back := []string{"dccccd", "cccccc", "cccccc", "cccccc", "cccccc", "cccccc", "kkkkkk"}
	mid := []string{"dccccd", "cccccc", "cccccc", "cccccc", "cccccc", "cccccc", "kkkkkk"}
	front := []string{"dccccd", "ccttcc", "cctccc", "cctccc", "cctccc", "cctccc", "kkkkkk"}
	if role == RoleReviewer {
		front = []string{"dccccd", "cttttc", "cctccc", "cctccc", "cctccc", "cctccc", "kkkkkk"}
	}
	out := append(append(append([]string{}, back...), mid...), front...)
	return out
}

var (
	modelMu sync.Mutex
	models  = map[string]*vxl.Model{}
)

// Figure of a role wearing a rig, built once and shared
func modelFor(role Role, rig string) *vxl.Model {
	key := string(role) + "/" + rig
	modelMu.Lock()
	defer modelMu.Unlock()
	if m, ok := models[key]; ok {
		return m
	}
	f := &figure{m: &vxl.Model{Scale: figureScale}, origin: map[string]vxl.Vec3{}}
	f.part("hips", "", vxl.V(-3, 8, -1.5), vxl.V(3, 0, 1.5), []string{"kkkkkk", "pppppp", "kkkkkk", "pppppp", "kkkkkk", "pppppp"}, 2)
	f.part("torso", "hips", vxl.V(-3, 10, -1.5), vxl.V(3, 0, 1.5), torsoLayers(role), 7)
	f.part("head", "torso", vxl.V(-2.5, 17, -2.5), vxl.V(2.5, 0, 2.5), headLayers(rig), headRows)
	arm := []string{"dd", "cc", "cc", "cc", "cc", "kk", "ss", "dd", "cc", "cc", "cc", "cc", "kk", "ss"}
	f.part("armL", "torso", vxl.V(-5, 10, -1), vxl.V(1, 7, 1), arm, 7)
	f.part("armR", "torso", vxl.V(3, 10, -1), vxl.V(1, 7, 1), arm, 7)
	thigh := solidLayers(2, 4, 3, 'p')
	shin := []string{"pp", "pp", "kk", "kk", "pp", "pp", "kk", "kk", "pp", "pp", "kk", "kk"}
	f.part("thighL", "hips", vxl.V(-3, 4, -1.5), vxl.V(1, 4, 1.5), thigh, 4)
	f.part("shinL", "thighL", vxl.V(-3, 0, -1.5), vxl.V(1, 4, 1.5), shin, 4)
	f.part("thighR", "hips", vxl.V(1, 4, -1.5), vxl.V(1, 4, 1.5), thigh, 4)
	f.part("shinR", "thighR", vxl.V(1, 0, -1.5), vxl.V(1, 4, 1.5), shin, 4)
	switch role {
	case RoleDirector:
		f.part("coat", "hips", vxl.V(-3, 2, -2.5), vxl.V(3, 6, 0.5), []string{"kkkkkk", "kkkkkk", "kkkkkk", "kkkkkk", "kkkkkk", "dkkkkd"}, 6)
	case RoleArchitect:
		f.part("pack", "torso", vxl.V(-2, 11, -3.5), vxl.V(2, 0, 1), []string{"kkkk", "kkkk", "kggk", "kkkk", "kkkk", "kkkk", "kkkk", "kkkk", "kkkk", "kkkk"}, 5)
	case RoleReviewer:
		cloak := make([]string, 0, 15)
		for range 15 {
			cloak = append(cloak, "dkkkkkkd")
		}
		f.part("cloak", "torso", vxl.V(-4, 2, -2.5), vxl.V(4, 15, 0.5), cloak, 15)
	}
	switch rig {
	case "halo":
		f.part("halo", "head", vxl.V(-3.5, 23.5, -3.5), vxl.V(3.5, 0, 3.5), []string{".ggggg.", "g.....g", "g.....g", "g.....g", "g.....g", "g.....g", ".ggggg."}, 1)
	case "antenna":
		f.part("antenna", "head", vxl.V(-2, 22, -2), vxl.V(0.5, 0, 0.5), []string{"v", "k", "k", "k"}, 4)
	case "crest":
		f.part("crest", "head", vxl.V(-0.5, 22, -2.5), vxl.V(0.5, 0, 2.5), []string{".", ".", "c", ".", "c", "c", "c", "c", "c", ".", "c", "c", ".", ".", "c"}, 3)
	case "horns":
		f.part("hornL", "head", vxl.V(-2.5, 22, -0.5), vxl.V(0.5, 0, 0.5), []string{"k", "k", "k"}, 3)
		f.part("hornR", "head", vxl.V(1.5, 22, -0.5), vxl.V(0.5, 0, 0.5), []string{"k", "k", "k"}, 3)
	}
	models[key] = f.m
	return f.m
}

// What a member is doing, driving its pose and motion
type actorState int

const (
	stateIdle actorState = iota
	stateWalk
	stateJack
	stateThink
	stateWait
	stateFrozen
	stateCheer
	stateSlump
	stateSit
	stateStagger
	stateInspect
	stateTalk
	stateBrief
	stateChart
	stateReview
	stateLeap
	stateExit
)

// Speed of a walking actor in floor units per second
const walkSpeed = 6.5

// Member of the crew as a figure on the platform
type actor struct {
	name  string
	role  Role
	index int
	col   vxl.RGB
	rig   string
	model *vxl.Model
	pal   vxl.Palette
	poses map[string]vxl.Pose
	pos   vxl.Vec3
	yaw   float64
	// Heading the actor settles on once it stands still
	face  float64
	state actorState
	// State taken once the errands are done, from the duty
	rest  actorState
	duty  duty
	phase float64
	bob   float64
	// Height above the floor while dropping in
	drop    float64
	dropVel float64
	landed  bool
	// Share of a tint toward failure red
	gloom float64
	// Share of the token budget spent, lighting the trace lines
	heat float64
	// Share of the ice encasing the actor while an approval waits
	frozen float64
	// Share of the beacon rising from the actor while it asks the person
	beacon float64
	// Share of the way the actor has jacked out
	exit float64
	gone bool
	// Height gained by a leap, and the arc it follows
	lift     float64
	leapFrom vxl.Vec3
	leapTo   vxl.Vec3
	leapT    float64
	emote    string
	emoteT   float64
	nod      float64
	// Errand the actor runs and the ambient behavior it came from
	steps   []step
	ambient string
	cool    float64
	partner *actor
	// Seconds the actor has been working at its node
	jacked float64
	// Share of the time limit of the current exchange spent
	clock float64
	// Last tool class seen, for the drone and the sparks
	toolKind toolKind
	drone    float64
	seat     vxl.Vec3
	shake    float64
}

// Figure and palette for a member
func newActor(m Member, look Look) *actor {
	a := &actor{name: m.Name, role: m.Role, index: m.Index, poses: map[string]vxl.Pose{}, landed: false, drop: 14}
	a.dress(look)
	return a
}

// Dresses the actor in an outfit, rebuilding its figure
func (a *actor) dress(look Look) {
	a.col, a.rig = dye(look.Color), look.Hat
	a.model = modelFor(a.role, a.rig)
	body := a.col
	if a.role == RoleReviewer {
		body = a.col.Mix(colDark, 0.55)
	}
	a.pal = vxl.Palette{Colors: map[rune]vxl.RGB{
		'c': body, 'C': body.Mix(colPaper, 0.3), 'd': body.Scale(0.6), 'k': colDark, 'p': colPants, 's': colSkin, 'S': colSkinDark, 'e': colEye,
		'v': colIce, 'g': a.col, 't': a.col, 'w': colPaper,
	}, Glow: map[rune]bool{'v': true, 'g': true, 't': true}}
}

// Where the head of the actor is, for labels, beacons and emotes
func (a *actor) headPos() vxl.Vec3 {
	head := a.model.Part("head")
	top := head.Place(vxl.V(2.5, 5.3, 2.5), a.poses)
	return a.model.World(top, vxl.Stance{Pos: a.stand(), Yaw: a.yaw})
}

// Where the hands of the actor meet in front of it
func (a *actor) handsPos() vxl.Vec3 {
	return a.stand().Add(vxl.UnitYaw(a.yaw).Mul(1.1)).Add(vxl.V(0, 2.6, 0))
}

// Where the feet of the actor touch down, with every height it has gained
func (a *actor) stand() vxl.Vec3 { return a.pos.Add(vxl.V(0, a.bob+a.drop+a.lift, 0)) }

// Unit vector the actor faces along
func (a *actor) facing() vxl.Vec3 { return vxl.UnitYaw(a.yaw) }

// Points the actor at a spot on the floor
func (a *actor) lookAt(p vxl.Vec3) {
	d := p.Sub(a.pos)
	if d.X*d.X+d.Z*d.Z > 1e-6 {
		a.face = vxl.HeadingOf(d)
	}
}

// Reports whether the actor has an errand to run
func (a *actor) busy() bool { return len(a.steps) > 0 }

// Color of the trace lines, the crew color running white then red as the budget goes
func (a *actor) traceColor() vxl.RGB {
	c := a.col.Mix(colPaper, min(a.heat*1.2, 1))
	if a.heat > 0.75 {
		c = c.Mix(colRed, min((a.heat-0.75)*4, 1))
	}
	return c
}

// Turns the actor toward a heading at a steady rate
func (a *actor) turnToward(target, dt float64) { a.yaw = vxl.ApproachYaw(a.yaw, target, 9*dt) }

// Sets every part of the figure for the state and the phase of the actor
func (a *actor) pose(dt float64) {
	if a.state == stateFrozen {
		return
	}
	P := a.poses
	clear(P)
	ph := a.phase
	breathe := 0.02 * math.Sin(ph*0.8)
	switch a.state {
	case stateWalk:
		a.phase += dt * 11
		s := math.Sin(ph)
		a.bob = 0.07 * math.Abs(s)
		P["thighL"] = vxl.Pose{Pitch: -0.6 * s}
		P["thighR"] = vxl.Pose{Pitch: 0.6 * s}
		P["shinL"] = vxl.Pose{Pitch: 0.7 * max(s, 0)}
		P["shinR"] = vxl.Pose{Pitch: 0.7 * max(-s, 0)}
		P["armL"] = vxl.Pose{Pitch: 0.55 * s}
		P["armR"] = vxl.Pose{Pitch: -0.55 * s}
		P["torso"] = vxl.Pose{Pitch: 0.07, Yaw: 0.06 * s}
		P["coat"] = vxl.Pose{Pitch: -0.3 - 0.15*math.Abs(s)}
		P["cloak"] = vxl.Pose{Pitch: -0.25 - 0.12*math.Abs(s)}
	case stateJack:
		a.phase += dt * 6
		a.bob = 0
		tw := 0.05 * math.Sin(ph*3.1)
		P["hips"] = vxl.Pose{Shift: vxl.V(0, -4, 0)}
		P["thighL"] = vxl.Pose{Pitch: -math.Pi / 2}
		P["shinL"] = vxl.Pose{Pitch: math.Pi / 2}
		P["shinR"] = vxl.Pose{Pitch: math.Pi / 2}
		P["torso"] = vxl.Pose{Pitch: 0.3 + breathe}
		P["armL"] = vxl.Pose{Pitch: -1.15 + tw, Yaw: 0.25}
		P["armR"] = vxl.Pose{Pitch: -1.15 - tw, Yaw: -0.25}
		P["head"] = vxl.Pose{Pitch: 0.2 + 0.05*math.Sin(ph*0.7)}
		P["coat"] = vxl.Pose{Pitch: -0.9}
		P["cloak"] = vxl.Pose{Pitch: -0.5}
	case stateThink:
		a.phase += dt * 2
		a.bob = 0
		P["armR"] = vxl.Pose{Pitch: -1.5, Yaw: -0.7}
		P["armL"] = vxl.Pose{Pitch: 0.1}
		P["head"] = vxl.Pose{Pitch: 0.12, Yaw: 0.2 * math.Sin(ph)}
		P["torso"] = vxl.Pose{Pitch: 0.05 + breathe}
	case stateWait:
		a.phase += dt * 3
		a.bob = 0
		P["armL"] = vxl.Pose{Pitch: -1.3, Yaw: 0.9}
		P["armR"] = vxl.Pose{Pitch: -1.3, Yaw: -0.9}
		P["head"] = vxl.Pose{Pitch: -0.25, Yaw: 0.1 * math.Sin(ph*0.5)}
		P["thighR"] = vxl.Pose{Pitch: -0.12 * max(math.Sin(ph*3), 0)}
		P["torso"] = vxl.Pose{Pitch: breathe}
	case stateCheer:
		a.phase += dt * 7
		a.bob = 1.0 * max(0, math.Sin(ph))
		P["armL"] = vxl.Pose{Pitch: -2.9, Roll: 0.3}
		P["armR"] = vxl.Pose{Pitch: -2.9, Roll: -0.3}
		P["head"] = vxl.Pose{Pitch: -0.25}
		P["torso"] = vxl.Pose{Pitch: -0.05}
	case stateSlump:
		a.phase += dt
		a.bob = 0
		P["hips"] = vxl.Pose{Shift: vxl.V(0, -1, 0)}
		P["shinL"] = vxl.Pose{Pitch: 0.25}
		P["shinR"] = vxl.Pose{Pitch: 0.25}
		P["thighL"] = vxl.Pose{Pitch: -0.25}
		P["thighR"] = vxl.Pose{Pitch: -0.25}
		P["torso"] = vxl.Pose{Pitch: 0.55 + breathe}
		P["head"] = vxl.Pose{Pitch: 0.45}
		P["armL"] = vxl.Pose{Pitch: 0.15}
		P["armR"] = vxl.Pose{Pitch: 0.15}
	case stateSit:
		a.phase += dt
		a.bob = 0
		P["hips"] = vxl.Pose{Shift: vxl.V(0, -7, 0)}
		P["thighL"] = vxl.Pose{Pitch: -math.Pi / 2}
		P["thighR"] = vxl.Pose{Pitch: -math.Pi / 2}
		P["shinL"] = vxl.Pose{Pitch: math.Pi/2 + 0.15*math.Sin(ph*1.7)}
		P["shinR"] = vxl.Pose{Pitch: math.Pi/2 - 0.15*math.Sin(ph*1.3)}
		P["torso"] = vxl.Pose{Pitch: -0.1 + breathe}
		P["armL"] = vxl.Pose{Pitch: -0.5}
		P["armR"] = vxl.Pose{Pitch: -0.5}
		P["head"] = vxl.Pose{Pitch: -0.1, Yaw: 0.15 * math.Sin(ph*0.4)}
		P["coat"] = vxl.Pose{Pitch: -1.3}
		P["cloak"] = vxl.Pose{Pitch: -1.2}
	case stateStagger:
		a.phase += dt * 9
		a.bob = 0
		P["hips"] = vxl.Pose{Yaw: 0.1 * math.Sin(ph*2.3)}
		P["torso"] = vxl.Pose{Pitch: -0.3, Roll: 0.1 * math.Sin(ph)}
		P["head"] = vxl.Pose{Pitch: -0.2, Yaw: 0.2 * math.Sin(ph*1.7)}
		P["armL"] = vxl.Pose{Pitch: -0.6, Roll: 1.0}
		P["armR"] = vxl.Pose{Pitch: -0.6, Roll: -1.0}
		P["thighL"] = vxl.Pose{Pitch: 0.2}
		P["thighR"] = vxl.Pose{Pitch: -0.3}
	case stateInspect:
		a.phase += dt * 2
		a.bob = 0
		P["hips"] = vxl.Pose{Shift: vxl.V(0, -3.7, 0)}
		P["thighL"] = vxl.Pose{Pitch: -1}
		P["shinL"] = vxl.Pose{Pitch: 1}
		P["thighR"] = vxl.Pose{Pitch: -1}
		P["shinR"] = vxl.Pose{Pitch: 1}
		P["torso"] = vxl.Pose{Pitch: 0.35 + breathe}
		P["head"] = vxl.Pose{Pitch: 0.3, Yaw: 0.25 * math.Sin(ph)}
		P["armR"] = vxl.Pose{Pitch: -0.9}
		P["armL"] = vxl.Pose{Pitch: -0.3}
		P["coat"] = vxl.Pose{Pitch: -0.8}
		P["cloak"] = vxl.Pose{Pitch: -0.6}
	case stateTalk:
		a.phase += dt * 1.2
		a.bob = 0
		P["armL"] = vxl.Pose{Pitch: -0.3 * math.Abs(math.Sin(ph*1.3))}
		P["armR"] = vxl.Pose{Pitch: -0.25 * math.Abs(math.Cos(ph*0.9))}
		P["head"] = vxl.Pose{Pitch: 0.05, Yaw: 0.08 * math.Sin(ph*2)}
		P["torso"] = vxl.Pose{Pitch: 0.04 + breathe}
	case stateBrief:
		a.phase += dt * 1.1
		a.bob = 0
		lift := 0.5 + 0.5*math.Sin(ph*0.7)
		P["armR"] = vxl.Pose{Pitch: -0.4 - 0.5*lift, Yaw: -0.25}
		P["armL"] = vxl.Pose{Pitch: -0.15, Roll: 0.15}
		P["head"] = vxl.Pose{Pitch: -0.1, Yaw: 0.25 * math.Sin(ph*0.45)}
		P["torso"] = vxl.Pose{Pitch: -0.03 + breathe}
	case stateChart:
		a.phase += dt * 2
		a.bob = 0
		P["torso"] = vxl.Pose{Pitch: 0.35 + breathe}
		P["armL"] = vxl.Pose{Pitch: -1.25 + 0.1*math.Sin(ph), Yaw: 0.3}
		P["armR"] = vxl.Pose{Pitch: -1.2 + 0.1*math.Cos(ph*1.4), Yaw: -0.3}
		P["head"] = vxl.Pose{Pitch: 0.4, Yaw: 0.2 * math.Sin(ph*0.7)}
	case stateReview:
		a.phase += dt * 2
		a.bob = 0
		P["armR"] = vxl.Pose{Pitch: -1.35 + 0.05*math.Sin(ph*3), Yaw: -0.2}
		P["armL"] = vxl.Pose{Pitch: 0.3, Yaw: 0.4}
		P["head"] = vxl.Pose{Pitch: 0.35, Yaw: 0.1 * math.Sin(ph)}
		P["torso"] = vxl.Pose{Pitch: 0.12 + breathe}
		P["cloak"] = vxl.Pose{Pitch: -0.15}
	case stateLeap:
		a.phase += dt * 4
		a.bob = 0
		P["thighL"] = vxl.Pose{Pitch: -0.9}
		P["thighR"] = vxl.Pose{Pitch: -0.9}
		P["shinL"] = vxl.Pose{Pitch: 1.3}
		P["shinR"] = vxl.Pose{Pitch: 1.3}
		P["armL"] = vxl.Pose{Pitch: -0.7, Roll: 0.6}
		P["armR"] = vxl.Pose{Pitch: -0.7, Roll: -0.6}
		P["cloak"] = vxl.Pose{Pitch: -1.6}
		P["coat"] = vxl.Pose{Pitch: -1.6}
	case stateExit:
		a.phase += dt * 2
		a.bob = 0
		P["head"] = vxl.Pose{Pitch: -0.45}
		P["armL"] = vxl.Pose{Pitch: -0.5, Roll: 0.7}
		P["armR"] = vxl.Pose{Pitch: -0.5, Roll: -0.7}
		P["torso"] = vxl.Pose{Pitch: -0.1}
	default:
		a.phase += dt * 2
		a.bob = 0
		P["torso"] = vxl.Pose{Pitch: breathe}
		P["armL"] = vxl.Pose{Pitch: 0.03 * math.Sin(ph)}
		P["armR"] = vxl.Pose{Pitch: -0.03 * math.Sin(ph)}
		P["head"] = vxl.Pose{Yaw: 0.12 * math.Sin(ph*0.35)}
	}
	if a.nod > 0 {
		h := P["head"]
		h.Pitch += 0.18 * math.Abs(math.Sin(a.nod*14))
		P["head"] = h
	}
	if a.shake > 0 {
		h := P["head"]
		h.Yaw += 0.3 * math.Sin(a.shake*20)
		P["head"] = h
	}
	if a.rig == "halo" {
		P["halo"] = vxl.Pose{Yaw: ph * 0.6, Shift: vxl.V(0, 0.6*math.Sin(ph*1.3), 0)}
	}
	if a.rig == "horns" {
		P["hornL"] = vxl.Pose{Roll: 0.5, Pitch: -0.3}
		P["hornR"] = vxl.Pose{Roll: -0.5, Pitch: -0.3}
	}
}

// Pixels a figure stands at or over to draw in full, and under which it shrinks to a marker
const (
	fullFigurePixels   = 14
	markerFigurePixels = 6
)

// Queues the figure at the detail its screen size calls for, tinted by failure and ice, traces at their heat
func (a *actor) draw(r *vxl.Raster) {
	if a.gone {
		return
	}
	stand := a.stand()
	tint := func(c vxl.RGB) vxl.RGB { return c }
	if a.gloom > 0 || a.frozen > 0 {
		gloom, frozen := a.gloom, a.frozen
		tint = func(c vxl.RGB) vxl.RGB { return c.Mix(vxl.Hex(0x4a1f2a), gloom*0.55).Mix(colIce, frozen*0.3) }
	}
	height := a.model.Height()
	_, _, depth, ok := r.Project(stand.Add(vxl.V(0, height/2, 0)))
	tall := 0.0
	if ok {
		tall = height * r.ScaleY(depth)
	}
	alpha := 1 - a.exit
	switch {
	case tall < markerFigurePixels:
		r.EmissiveBox(stand.Add(vxl.V(0, height/2, 0)), vxl.V(1, height, 1), a.yaw, tint(a.col), alpha, 1.4)
	case tall < fullFigurePixels:
		a.drawBlock(r, stand, tint(a.pal.Colors['c']), tint(a.col.Mix(colPaper, 0.35)), alpha)
	default:
		a.pal.Colors['t'] = a.traceColor()
		st := vxl.Stance{Pos: stand, Yaw: a.yaw, Poses: a.poses, Fade: a.exit}
		if a.gloom > 0 || a.frozen > 0 {
			st.Tint = tint
		}
		a.model.Draw(r, a.pal, st)
	}
}

// Queues the figure as a torso box from floor to posed shoulders under a head box at its posed head
func (a *actor) drawBlock(r *vxl.Raster, stand vxl.Vec3, body, head vxl.RGB, alpha float64) {
	if alpha <= 0 {
		return
	}
	st := vxl.Stance{Pos: stand, Yaw: a.yaw}
	torso := a.model.World(a.model.Part("torso").Place(vxl.V(3, 7, 1.5), a.poses), st)
	top := a.model.World(a.model.Part("head").Place(vxl.V(2.5, 5, 2.5), a.poses), st)
	h := max(torso.Y-stand.Y, 0.5)
	r.OrientedBox(vxl.V(torso.X, stand.Y+h/2, torso.Z), vxl.V(1.5, h, 0.75), a.yaw, body, alpha)
	hs := max(top.Y-torso.Y, 0.5)
	r.OrientedBox(vxl.V(top.X, torso.Y+hs/2, top.Z), vxl.V(1.25, hs, 1.25), a.yaw, head, alpha)
}

// Queues the ice encasing the actor while an approval waits, drawn after everything solid
func (a *actor) drawIce(r *vxl.Raster, t float64) {
	if a.frozen <= 0 || a.gone {
		return
	}
	size := 2.6 * a.frozen
	center := a.stand().Add(vxl.V(0, 2.9, 0))
	r.OrientedBox(center, vxl.V(size, size*1.9, size), a.yaw+0.3, colIce.Mix(colPaper, 0.2), 0.42*a.frozen)
	r.Billboard(center, 3.5*a.frozen, colIce, 0.12+0.06*math.Sin(t*5), true)
}
