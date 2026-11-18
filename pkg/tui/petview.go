package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Frame clock of the pet animation, the FRAME_MS of the reference terminal UI
	petFrameEvery = 160 * time.Millisecond
	// Probe that catches a pet adopted, switched or put away elsewhere, the POLL_MS of the reference
	petPollEvery = 2500 * time.Millisecond
	// Transcript columns a right gutter must leave, below it the pet takes bottom rows instead
	petMinGutterBody = 72
	// Transcript rows the bottom band layout must leave above the pet
	petMinBandBody = 4
	// Narrowest transcript the band layout leaves beside nothing, below it the corner stack hides
	petMinBandCols = 24
	// Alpha a half block pixel needs to be drawn, the threshold of the reference sprite painter
	petOpaque = 32
)

// Next frame of the pet animation
type petTickMsg struct{ gen int }

// Next probe of the active pet
type petPollMsg struct{ gen int }

// Half block rows of every frame of one pet state and the grid size
type petFrames struct {
	rows       [][]string
	cols, high int
}

// Where the sprite sits inside the session window, in content cells
type petSpot struct {
	x, y, cols, rows int
	visible          bool
}

// Corner stack of the session window and the room it takes from the transcript
type petPlacement struct {
	rows   []string
	gutter int
	band   int
}

// Fitted frame pixels the packing goroutines share, keyed by frame, box and background
type petPixels struct {
	mu sync.Mutex
	m  map[petPixelKey][]byte
}

type petPixelKey struct {
	frame *image.NRGBA
	w, h  int
	bg    color.RGBA
}

// Animated pet of the session window, fed by the pet verbs and the pet events of the gateway
type petView struct {
	a        *app
	enabled  bool
	slug     string
	name     string
	scale    float64
	revision string
	// Animation state wanted, the frame clock position and the frames painted now
	state      string
	frame      int
	shown      int
	paint      *petFrames
	paintState string
	cells      map[string]*petFrames
	syncing    bool
	pollGen    int
	tickGen    int
	ticking    bool
	spot       petSpot
	ox, oy     int
	// Spritesheet frames of the image path by state, the revision they came from
	images       map[string][]*image.NRGBA
	sheetRev     string
	sheetLoading bool
	pixels       *petPixels
	surface      *gfx.Surface
	placedAt     *[2]int
	hatch        *hatchJob
	hatchSeq     int
}

// Animated pet of an app with nothing selected yet
func newPetView(a *app) *petView {
	return &petView{a: a, state: string(pet.StateIdle), cells: map[string]*petFrames{}, pixels: &petPixels{m: map[petPixelKey][]byte{}}}
}

// First probe and the poll that keeps the pet in step with the config
func (p *petView) start() tea.Cmd { return tea.Batch(p.refresh(), p.schedulePoll()) }

func (p *petView) schedulePoll() tea.Cmd {
	p.pollGen++
	gen := p.pollGen
	return tea.Tick(petPollEvery, func(time.Time) tea.Msg { return petPollMsg{gen: gen} })
}

// Handles the pet clocks, false for any other message
func (p *petView) update(m tea.Msg) (bool, tea.Cmd) {
	switch m := m.(type) {
	case petPollMsg:
		if m.gen != p.pollGen {
			return true, nil
		}
		return true, tea.Batch(p.refresh(), p.schedulePoll())
	case petTickMsg:
		if m.gen != p.tickGen {
			return true, nil
		}
		return true, p.onTick()
	}
	return false, nil
}

// Takes the pet events, a changed selection and the progress of a hatch
func (p *petView) observe(ev gatewayEventMsg) tea.Cmd {
	switch ev.Type {
	case "pet.changed":
		if ev.Payload == nil {
			return nil
		}
		return p.applyMeta(ev.Payload)
	case "pet.generate.progress":
		return p.draftProgress(ev.Payload)
	case "pet.hatch.progress":
		return p.rowProgress(ev.Payload)
	}
	return nil
}

// Gateway request whose failure leaves the pet as it is, the pet being cosmetic
func (p *petView) quiet(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd {
	return gatewayCall(p.a.gw, method, params, func(v any, err error) tea.Cmd {
		if err != nil {
			return then(nil)
		}
		return then(asRPCResult(v))
	})
}

// Probes the active pet, one probe at a time, then pulls what the cache lacks
func (p *petView) refresh() tea.Cmd {
	if p.syncing {
		return nil
	}
	p.syncing = true
	return p.quiet("pet.info.meta", map[string]any{}, func(meta map[string]any) tea.Cmd {
		p.syncing = false
		if meta == nil {
			return nil
		}
		return p.applyMeta(meta)
	})
}

// Takes a pet.info.meta shaped answer, dropping cached frames once the selection moved
func (p *petView) applyMeta(meta map[string]any) tea.Cmd {
	if !truthyJS(meta["enabled"]) {
		return p.disable()
	}
	slug := stringOr(meta["slug"], "")
	scale, _ := meta["scale"].(float64)
	revision := stringOr(meta["spritesheetRevision"], "")
	var cmd tea.Cmd
	if slug != p.slug || scale != p.scale || revision != p.revision {
		cmd = p.forget()
		p.slug, p.scale, p.revision = slug, scale, revision
	}
	p.name = stringOr(meta["displayName"], slug)
	return tea.Batch(cmd, p.pull())
}

// Drops the cached frames and the image of the selection, the last frame staying painted until new ones land
func (p *petView) forget() tea.Cmd {
	cmd := p.dropImage()
	p.cells, p.images, p.sheetRev, p.frame = map[string]*petFrames{}, nil, "", 0
	p.pixels = &petPixels{m: map[petPixelKey][]byte{}}
	return cmd
}

// Hides the pet and stops its clock
func (p *petView) disable() tea.Cmd {
	cmd := p.forget()
	p.paint, p.paintState = nil, ""
	p.enabled, p.slug, p.name, p.scale, p.revision = false, "", "", 0, ""
	p.ticking = false
	p.tickGen++
	return cmd
}

// Pulls the cell frames of the wanted state the cache lacks, then the spritesheet the image path needs
func (p *petView) pull() tea.Cmd {
	state := p.state
	var cmds []tea.Cmd
	if _, cached := p.cells[p.slug+":"+state]; cached {
		p.enabled = true
		cmds = append(cmds, p.startTicking())
	} else if !p.syncing {
		p.syncing = true
		cmds = append(cmds, p.quiet("pet.cells", map[string]any{"state": state}, func(res map[string]any) tea.Cmd {
			p.syncing = false
			return p.applyCells(state, res)
		}))
	}
	return tea.Batch(append(cmds, p.probe(), p.pullSheet())...)
}

// Takes a pet.cells answer, the frame answer winning over the probe when the selection moved between them
func (p *petView) applyCells(state string, res map[string]any) tea.Cmd {
	if res == nil {
		return nil
	}
	if !truthyJS(res["enabled"]) {
		return p.disable()
	}
	slug := stringOr(res["slug"], p.slug)
	scale := p.scale
	if s, isNum := res["scale"].(float64); isNum {
		scale = s
	}
	var cmd tea.Cmd
	if slug != p.slug || scale != p.scale {
		cmd = p.forget()
		if slug != p.slug {
			p.revision = ""
		}
		p.slug, p.scale = slug, scale
	}
	if frames := petCellFrames(res["frames"]); len(frames.rows) > 0 {
		p.cells[slug+":"+state] = frames
	}
	p.enabled = true
	return tea.Batch(cmd, p.startTicking(), p.probe(), p.pullSheet())
}

// Starts the frame clock once the pet is out
func (p *petView) startTicking() tea.Cmd {
	if p.ticking || !p.enabled {
		return nil
	}
	p.ticking = true
	p.tickGen++
	return p.onTick()
}

// One frame, the state followed, the next frame of it painted and the image sent
func (p *petView) onTick() tea.Cmd {
	if !p.enabled {
		p.ticking = false
		return nil
	}
	gen := p.tickGen
	cmds := []tea.Cmd{tea.Tick(petFrameEvery, func(time.Time) tea.Msg { return petTickMsg{gen: gen} })}
	if want := p.wanted(); want != p.state {
		p.state, p.frame = want, 0
		if _, cached := p.cells[p.slug+":"+want]; !cached {
			cmds = append(cmds, p.pull())
		}
	}
	entry := p.cells[p.slug+":"+p.state]
	if entry == nil || len(entry.rows) == 0 {
		return tea.Batch(cmds...)
	}
	p.paint, p.paintState = entry, p.state
	p.shown = p.frame % len(entry.rows)
	p.frame = p.shown + 1
	return tea.Batch(append(cmds, p.drawImage())...)
}

// State the session asks for, a reaction beat over awaited input, a running tool, reasoning and a busy turn
func (p *petView) wanted() string {
	s := p.a.store
	flash := ""
	if f := s.petFlash; f != nil && s.now().Before(f.Until) {
		flash = f.State
	}
	o := s.overlay
	awaiting := o.Clarify != nil || o.Approval != nil || o.Confirm != nil || o.Sudo != nil || o.Secret != nil || o.VaultUnlock != nil ||
		o.VaultSaveLogin != nil || o.VaultCode != nil
	return string(pet.DeriveState(pet.Signals{Error: flash == string(pet.StateFailed), Celebrate: flash == string(pet.StateJump),
		JustCompleted: flash == string(pet.StateWave), AwaitingInput: awaiting, ToolRunning: len(s.turn.Tools) > 0,
		Reasoning: s.turn.ReasoningActive, Busy: s.ui.Busy}))
}

// Configured display.pet.render_mode, auto for a missing or unknown one
func (p *petView) renderMode() string {
	conf, _ := p.a.store.ui.Pet.(map[string]any)
	mode := strings.ToLower(trimJS(stringOr(conf["render_mode"], "")))
	if !slices.Contains(pet.RenderModes, mode) {
		return "auto"
	}
	return mode
}

// Image protocol the render mode and the terminal allow, none for half block cells
func (p *petView) protocol() gfx.Protocol {
	g := p.a.arena.graphics
	switch p.renderMode() {
	case "auto":
		return g.Protocol()
	case "kitty":
		for _, proto := range []gfx.Protocol{gfx.KittyCells, gfx.KittyPlaced} {
			if g.Draws(proto) {
				return proto
			}
		}
	case "sixel":
		if g.Draws(gfx.Sixel) {
			return gfx.Sixel
		}
	case "iterm":
		if g.Draws(gfx.ITerm) {
			return gfx.ITerm
		}
	}
	return gfx.None
}

// Whether the render mode asks for images at all
func (p *petView) wantsImages() bool {
	mode := p.renderMode()
	return mode != "unicode" && mode != "off"
}

// Asks the terminal what it draws the first time the pet wants images
func (p *petView) probe() tea.Cmd {
	if !p.enabled || !p.wantsImages() || p.a.arena.probed {
		return nil
	}
	p.a.arena.probed = true
	return p.a.arena.graphics.Probe()
}

// Fetches the spritesheet of the revision and crops its frames beside the UI once the image path wants them
func (p *petView) pullSheet() tea.Cmd {
	if !p.enabled || p.sheetLoading || p.protocol() == gfx.None || (p.images != nil && p.sheetRev == p.revision) {
		return nil
	}
	p.sheetLoading = true
	slug := p.slug
	params := map[string]any{}
	if p.images != nil && p.sheetRev != "" {
		params["knownRevision"] = p.sheetRev
	}
	return p.quiet("pet.info", params, func(info map[string]any) tea.Cmd {
		if info == nil || !truthyJS(info["enabled"]) || stringOr(info["slug"], "") != slug {
			p.sheetLoading = false
			return nil
		}
		revision := stringOr(info["spritesheetRevision"], "")
		if truthyJS(info["spritesheetUnchanged"]) {
			p.sheetLoading, p.sheetRev = false, revision
			return nil
		}
		return func() tea.Msg {
			images, err := petSheetImages(info)
			return asyncDoneMsg{run: func() tea.Cmd {
				p.sheetLoading = false
				if err != nil || slug != p.slug {
					return nil
				}
				p.images, p.sheetRev = images, revision
				p.pixels = &petPixels{m: map[petPixelKey][]byte{}}
				return nil
			}}
		}
	})
}

// Frames of every driven state cropped from the spritesheet of a pet.info answer
func petSheetImages(info map[string]any) (map[string][]*image.NRGBA, error) {
	raw, err := base64.StdEncoding.DecodeString(stringOr(info["spritesheetBase64"], ""))
	if err != nil {
		return nil, err
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	whole := func(key string, fallback int) int {
		if n, isNum := info[key].(float64); isNum && n > 0 {
			return int(n)
		}
		return fallback
	}
	sheet := pypil.ToNRGBA(decoded)
	frameW, frameH, perState := whole("frameW", pet.FrameW), whole("frameH", pet.FrameH), whole("framesPerState", pet.FramesPerState)
	out := map[string][]*image.NRGBA{}
	for _, state := range pet.States {
		out[string(state)] = pet.SheetFrames(sheet, string(state), frameW, frameH, perState)
	}
	return out, nil
}

// Surface color of the window the sprite is drawn over
func (p *petView) background() color.RGBA {
	r, g, b, _ := p.a.toolkit.Surface.RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0xff}
}

// Packs the frame painted now for the image surface beside the UI, the surface swapped when the protocol changed
func (p *petView) drawImage() tea.Cmd {
	proto := p.protocol()
	var cmds []tea.Cmd
	if p.surface != nil && (p.surface.Protocol() != proto || !p.spot.visible) {
		cmds = append(cmds, p.dropImage())
	}
	frames := p.images[p.paintState]
	if proto == gfx.None || !p.spot.visible || len(frames) == 0 {
		return tea.Batch(cmds...)
	}
	if p.surface == nil {
		p.surface = p.a.arena.graphics.NewSurface(proto)
		p.surface.SetRate(int((time.Second + petFrameEvery - 1) / petFrameEvery))
	}
	s, px, bg := p.surface, p.pixels, p.background()
	if !s.Ready(time.Now()) {
		return tea.Batch(cmds...)
	}
	cols, rows := p.spot.cols, p.spot.rows
	w, h := s.Budget(cols, rows)
	frame := frames[p.shown%len(frames)]
	return tea.Batch(append(cmds, func() tea.Msg {
		packet := s.Pack(px.fit(frame, w, h, bg), w, h, cols, rows)
		return asyncDoneMsg{run: func() tea.Cmd { return p.emit(s, packet, cols, rows) }}
	})...)
}

// Sends a packed frame where the sprite sits, a placed image taken down first once it moved or got covered
func (p *petView) emit(s *gfx.Surface, packet *gfx.Packet, cols, rows int) tea.Cmd {
	if packet == nil || s != p.surface || !p.spot.visible || p.spot.cols != cols || p.spot.rows != rows {
		return nil
	}
	x, y := p.ox+p.spot.x, p.oy+p.spot.y
	if s.Protocol() != gfx.KittyCells {
		if !p.clear() || (p.placedAt != nil && *p.placedAt != [2]int{x, y}) {
			return p.takeDown()
		}
	}
	text := s.Emit(packet, x, y, time.Now())
	if text == "" {
		return nil
	}
	p.placedAt = &[2]int{x, y}
	return tea.Raw(text)
}

// Removes a placed image from the screen and keeps the surface for the next frame
func (p *petView) takeDown() tea.Cmd {
	if p.placedAt == nil || p.surface == nil {
		return nil
	}
	p.placedAt = nil
	if text := p.surface.Release(); text != "" {
		return tea.Raw(text)
	}
	return nil
}

// Releases the image surface, telling the terminal to forget its images
func (p *petView) dropImage() tea.Cmd {
	s := p.surface
	p.surface, p.placedAt = nil, nil
	if s == nil {
		return nil
	}
	if text := s.Release(); text != "" {
		return tea.Raw(text)
	}
	return nil
}

// Whether nothing covers the sprite, so an image placed by screen position may be drawn there
func (p *petView) clear() bool {
	m := p.a.wm
	if m.Capturing() || len(m.Toasts()) > 0 {
		return false
	}
	ws := m.Active()
	if ws == nil || m.WorkspaceOf(p.a.card) != ws {
		return false
	}
	if z := ws.Zoomed(); z != nil && z != wm.Card(p.a.card) {
		return false
	}
	x, y := p.ox+p.spot.x, p.oy+p.spot.y
	for _, c := range ws.Cards() {
		if c == wm.Card(p.a.card) {
			continue
		}
		if f, ok := m.FrameOf(c); ok && f.X < x+p.spot.cols && x < f.X+f.W && f.Y < y+p.spot.rows && y < f.Y+f.H {
			return false
		}
	}
	return true
}

// Whether the terminal shows an image of the pet now, half blocks painted until it does
func (p *petView) imageShown() bool {
	s := p.surface
	if s == nil {
		return false
	}
	if s.Protocol() == gfx.KittyCells {
		st := s.Stats()
		return st.Acked > 0 || st.Timeouts > 0
	}
	return p.placedAt != nil
}

// Name of the way the pet is drawn now, the image protocol or half blocks
func (p *petView) drawing() string {
	if p.imageShown() {
		return p.surface.Protocol().String()
	}
	return tr("slashCmd.session.pet.drawingCells")
}

// Screen cell where the session window content starts, set before it paints
func (p *petView) origin(x, y int) { p.ox, p.oy = x, y }

// Box of a kitty image placed under the cells, which must keep no background
func (p *petView) holes() []wm.Box {
	if p.surface == nil || p.surface.Protocol() != gfx.KittyPlaced || p.placedAt == nil || !p.spot.visible {
		return nil
	}
	return []wm.Box{{X: p.spot.x, Y: p.spot.y, W: p.spot.cols, H: p.spot.rows}}
}

// Rows of the sprite as it shows now, the image cells once the terminal draws it, else the half blocks
func (p *petView) spriteRows() ([]string, int, int) {
	if !p.enabled || p.paint == nil || len(p.paint.rows) == 0 || p.renderMode() == "off" {
		return nil, 0, 0
	}
	cols, high := p.paint.cols, p.paint.high
	if !p.imageShown() {
		return p.paint.rows[p.shown%len(p.paint.rows)], cols, high
	}
	buf := uv.NewBuffer(cols, high)
	p.surface.Paint(buf, 0, 0, cols, high, p.a.toolkit.Surface)
	rows := make([]string, high)
	for y := range rows {
		rows[y] = buf.Line(y).Render()
	}
	return rows, cols, high
}

// Corner stack for a body of w columns and h rows under the top dock rows, recording the sprite spot
func (p *petView) place(w, h, top int) petPlacement {
	p.spot.visible = false
	t := p.a.store.ui.Theme
	hatch := p.hatchBox(t)
	sprite, cols, high := p.spriteRows()
	var petBox []string
	if sprite != nil {
		petBox = framedBox(t, " "+p.name+" ", sprite, cols+4)
	}
	for _, withPet := range []bool{true, false} {
		stack := hatch
		if withPet {
			if petBox == nil {
				continue
			}
			stack = append(append([]string{}, hatch...), petBox...)
		}
		if len(stack) == 0 {
			return petPlacement{}
		}
		width := 0
		for _, row := range stack {
			width = max(width, ansi.StringWidth(row))
		}
		var out petPlacement
		switch {
		case w-width-2 >= petMinGutterBody && h >= len(stack):
			out = petPlacement{rows: stack, gutter: width + 2}
		case w-width-2 >= petMinBandCols && h-len(stack) >= petMinBandBody:
			out = petPlacement{rows: stack, band: len(stack)}
		default:
			continue
		}
		if withPet {
			p.spot = petSpot{x: w - cols - 3, y: top + h - len(petBox) + 1, cols: cols, rows: high, visible: true}
		}
		return out
	}
	return petPlacement{}
}

// Transcript rows with the band rows below them and the corner stack laid over their bottom right
func (pl petPlacement) over(body []string, w int) []string {
	if len(pl.rows) == 0 {
		return body
	}
	out := append(append([]string{}, body...), make([]string, pl.band)...)
	first := max(len(out)-len(pl.rows), 0)
	for i, row := range pl.rows {
		if first+i >= len(out) {
			break
		}
		keep := max(w-1-ansi.StringWidth(row), 0)
		left := ansi.Truncate(out[first+i], keep, "")
		out[first+i] = left + strings.Repeat(" ", max(keep-ansi.StringWidth(left), 0)) + row + " "
	}
	return out
}

// Half block of two pixels, a lone opaque half drawn alone so the window shows through the other
func halfBlock(top, bottom [4]uint8) string {
	upper, lower := top[3] >= petOpaque, bottom[3] >= petOpaque
	switch {
	case upper && lower:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm▀\x1b[m", top[0], top[1], top[2], bottom[0], bottom[1], bottom[2])
	case upper:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm▀\x1b[m", top[0], top[1], top[2])
	case lower:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm▄\x1b[m", bottom[0], bottom[1], bottom[2])
	}
	return " "
}

// Half block rows of a cell grid of the pet renderer
func halfBlockRows(grid [][]pet.Cell) []string {
	rows := make([]string, len(grid))
	for y, row := range grid {
		var b strings.Builder
		for _, cell := range row {
			b.WriteString(halfBlock(cell[0], cell[1]))
		}
		rows[y] = b.String()
	}
	return rows
}

// Painted frames of a pet.cells answer, each cell its top and bottom pixel as eight channel values
func petCellFrames(raw any) *petFrames {
	list, _ := raw.([]any)
	out := &petFrames{}
	for _, f := range list {
		grid, _ := f.([]any)
		cells := make([][]pet.Cell, 0, len(grid))
		for _, r := range grid {
			row, _ := r.([]any)
			line := make([]pet.Cell, len(row))
			for x, c := range row {
				values, _ := c.([]any)
				for i := 0; i < 8 && i < len(values); i++ {
					n, _ := values[i].(float64)
					line[x][i/4][i%4] = uint8(min(max(int(n), 0), 255))
				}
			}
			out.cols = max(out.cols, len(line))
			cells = append(cells, line)
		}
		out.high = max(out.high, len(cells))
		out.rows = append(out.rows, halfBlockRows(cells))
	}
	return out
}

// Frame fit into w by h pixels over the background, three bytes a pixel, cached for its next loop
func (c *petPixels) fit(frame *image.NRGBA, w, h int, bg color.RGBA) []byte {
	key := petPixelKey{frame: frame, w: w, h: h, bg: bg}
	c.mu.Lock()
	cached, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return cached
	}
	fw, fh := max(frame.Rect.Dx(), 1), max(frame.Rect.Dy(), 1)
	ratio := min(float64(w)/float64(fw), float64(h)/float64(fh))
	sw, sh := max(int(float64(fw)*ratio), 1), max(int(float64(fh)*ratio), 1)
	small := pypil.ResizeLanczos(frame, sw, sh)
	ox, oy := (w-sw)/2, (h-sh)/2
	out := make([]byte, w*h*3)
	for i := 0; i < len(out); i += 3 {
		out[i], out[i+1], out[i+2] = bg.R, bg.G, bg.B
	}
	for y := 0; y < sh && oy+y < h; y++ {
		for x := 0; x < sw && ox+x < w; x++ {
			src := small.PixOffset(x, y)
			alpha := int(small.Pix[src+3])
			dst := ((oy+y)*w + ox + x) * 3
			out[dst] = uint8((int(small.Pix[src])*alpha + int(bg.R)*(255-alpha)) / 255)
			out[dst+1] = uint8((int(small.Pix[src+1])*alpha + int(bg.G)*(255-alpha)) / 255)
			out[dst+2] = uint8((int(small.Pix[src+2])*alpha + int(bg.B)*(255-alpha)) / 255)
		}
	}
	c.mu.Lock()
	c.m[key] = out
	c.mu.Unlock()
	return out
}
