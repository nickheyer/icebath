package tui

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-isatty"

	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
	// Nominal pixels of a terminal cell the reference sizes an iTerm2 image box by
	petCellPxW, petCellPxH = 8, 16
)

// Next frame of the pet animation
type petTickMsg struct{ gen int }

// Next probe of the active pet
type petPollMsg struct{ gen int }

// End of a reaction beat, the state derived again
type petFlashMsg struct{ gen int }

// Frames of one pet state as pet.cells answered, half block rows or a kitty placement
type petFrames struct {
	rows       [][]string
	cols, high int
	kitty      *petKitty
}

// Kitty placement of a state, the transmit escape of every frame and the placeholder grid in the image colour
type petKitty struct {
	escapes     []string
	placeholder []string
	color       string
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

// Sixel or iTerm2 frames of the reference renderer for one protocol, by state, with their pixel size
type petSheet struct {
	proto  gfx.Protocol
	frames map[string][]string
	size   map[string][2]int
}

// Animated pet of the session window, fed by pet.info.meta and pet.cells as the reference terminal UI is
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
	flashSeen  *petFlash
	flashGen   int
	kittyID    int
	graphics   bool
	spot       petSpot
	ox, oy     int
	// Spritesheet frames of the sixel and iTerm2 path, the revision they came from
	sheet        *petSheet
	sheetRev     string
	sheetLoading bool
	surface      *gfx.Surface
	placedAt     *[2]int
	hatch        *hatchJob
}

// Animated pet of an app with nothing selected yet, asking for graphics when stdout is a terminal
func newPetView(a *app) *petView {
	return &petView{a: a, state: string(pet.StateIdle), cells: map[string]*petFrames{}, graphics: isatty.IsTerminal(os.Stdout.Fd())}
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
	case petFlashMsg:
		if m.gen != p.flashGen {
			return true, nil
		}
		return true, p.follow()
	}
	return false, nil
}

// Takes the progress events of a hatch and probes the pet again the moment the gateway reports it changed
func (p *petView) observe(ev gatewayEventMsg) tea.Cmd {
	switch ev.Type {
	case "pet.changed":
		return p.refresh()
	case "pet.generate.progress":
		if progress, err := tuigateway.PetGenerateProgressEvent.Payload(ev.Payload); err == nil {
			return p.draftProgress(progress)
		}
	case "pet.hatch.progress":
		if progress, err := tuigateway.PetHatchProgressEvent.Payload(ev.Payload); err == nil {
			return p.rowProgress(progress)
		}
	}
	return nil
}

// Typed gateway request whose failure or misfit leaves the pet as it is, the pet being cosmetic
func quietPet[P, R any](p *petView, m tuigateway.Method[P, R], params P, then func(*R) tea.Cmd) tea.Cmd {
	return call(p.a.gw, m, params, func(r *R, err error) tea.Cmd {
		if err != nil {
			return then(nil)
		}
		return then(r)
	})
}

// Probes the active pet for the state wanted now, one probe at a time, then pulls the frames the cache lacks
func (p *petView) refresh() tea.Cmd {
	if p.syncing {
		return nil
	}
	p.syncing = true
	state := p.state
	return quietPet(p, tuigateway.PetInfoMeta, tuigateway.ProfileParams{}, func(meta *tuigateway.PetInfoMetaResult) tea.Cmd {
		if meta == nil {
			p.syncing = false
			return nil
		}
		if !meta.Enabled {
			p.syncing = false
			return p.disable()
		}
		slug, revision, scale := meta.Slug, meta.SpritesheetRevision, meta.Scale
		var cmd tea.Cmd
		if slug != p.slug || scale != p.scale || revision != p.revision {
			cmd = p.forget()
			p.slug, p.scale, p.revision = slug, scale, revision
		}
		p.name = cmp.Or(meta.DisplayName, slug)
		if _, cached := p.cells[slug+":"+state]; cached {
			p.syncing = false
			p.enabled = true
			return tea.Batch(cmd, p.startTicking(), p.probe(), p.pullSheet())
		}
		cells := tuigateway.PetCellsParams{Graphics: p.graphics, State: state}
		return tea.Batch(cmd, quietPet(p, tuigateway.PetCells, cells, func(res *tuigateway.PetCellsResult) tea.Cmd {
			p.syncing = false
			return p.applyCells(state, meta, res)
		}))
	})
}

// Releases the terminal side kitty image of the selection
func (p *petView) releaseKitty() tea.Cmd {
	if p.kittyID == 0 {
		return nil
	}
	id := p.kittyID
	p.kittyID = 0
	return tea.Raw(fmt.Sprintf("\x1b_Ga=d,d=i,i=%d,q=2\x1b\\", id))
}

// Drops the cached frames and images of the selection, the last frame staying painted until new ones land
func (p *petView) forget() tea.Cmd {
	cmds := []tea.Cmd{p.releaseKitty(), p.dropImage()}
	p.cells, p.sheet, p.sheetRev, p.frame = map[string]*petFrames{}, nil, "", 0
	return tea.Batch(cmds...)
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

// Takes a pet.cells answer, the frame answer winning over the probe when the selection moved between them
func (p *petView) applyCells(state string, meta *tuigateway.PetInfoMetaResult, res *tuigateway.PetCellsResult) tea.Cmd {
	if res == nil {
		p.enabled = true
		return tea.Batch(p.startTicking(), p.probe(), p.pullSheet())
	}
	if !res.Enabled {
		return p.disable()
	}
	slug, scale := cmp.Or(res.Slug, meta.Slug), cmp.Or(res.Scale, meta.Scale)
	var cmd tea.Cmd
	if slug != p.slug || scale != p.scale {
		cmd = p.forget()
		if slug != meta.Slug {
			p.revision = ""
		}
		p.slug, p.scale = slug, scale
	}
	var frames tuigateway.PetCellFrames
	if res.Frames != nil {
		frames = *res.Frames
	}
	placeholder := res.Placeholder
	if res.Graphics == "kitty" && len(frames.Kitty) > 0 && len(placeholder) > 0 {
		p.kittyID = int(res.ImageID)
		cols := 0
		for _, row := range placeholder {
			cols = max(cols, strings.Count(row, "\U0010eeee"))
		}
		p.cells[slug+":"+state] = &petFrames{cols: cols, high: len(placeholder),
			kitty: &petKitty{escapes: frames.Kitty, placeholder: placeholder, color: cmp.Or(res.Color, "#000001")}}
	} else if grid := petCellFrames(frames.Cells); len(grid.rows) > 0 {
		p.cells[slug+":"+state] = grid
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

// Takes a changed animation state at once, the frame clock and the poll restarted on it and its frames pulled
func (p *petView) follow() tea.Cmd {
	var cmds []tea.Cmd
	if f := p.a.store.petFlash; f != nil && f != p.flashSeen {
		p.flashSeen = f
		p.flashGen++
		gen := p.flashGen
		if wait := f.Until.Sub(p.a.store.now()); wait > 0 {
			cmds = append(cmds, tea.Tick(wait, func(time.Time) tea.Msg { return petFlashMsg{gen: gen} }))
		}
	}
	want := p.wanted()
	if want == p.state {
		return tea.Batch(cmds...)
	}
	p.state, p.frame = want, 0
	if _, cached := p.cells[p.slug+":"+want]; !cached {
		cmds = append(cmds, p.refresh())
	}
	cmds = append(cmds, p.schedulePoll())
	if p.enabled {
		p.tickGen++
		p.ticking = true
		cmds = append(cmds, p.onTick())
	}
	return tea.Batch(cmds...)
}

// One frame of the state, its kitty escape written or its image sent, the last frame kept while the state loads
func (p *petView) onTick() tea.Cmd {
	if !p.enabled {
		p.ticking = false
		return nil
	}
	gen := p.tickGen
	cmds := []tea.Cmd{tea.Tick(petFrameEvery, func(time.Time) tea.Msg { return petTickMsg{gen: gen} })}
	entry := p.cells[p.slug+":"+p.state]
	if entry == nil {
		return tea.Batch(cmds...)
	}
	count := len(entry.rows)
	if entry.kitty != nil {
		count = len(entry.kitty.escapes)
	}
	if count == 0 {
		return tea.Batch(cmds...)
	}
	p.paint, p.paintState = entry, p.state
	p.shown = p.frame % count
	p.frame = p.shown + 1
	if entry.kitty != nil {
		return tea.Batch(append(cmds, p.dropImage(), tea.Raw(entry.kitty.escapes[p.shown]))...)
	}
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

// Configured display.pet.render_mode, auto before the config arrived
func (p *petView) renderMode() string {
	if mode := p.a.store.ui.PetRenderMode; mode != "" {
		return string(mode)
	}
	return "auto"
}

// Sixel or iTerm2 protocol the render mode and the terminal allow, kitty coming from pet.cells alone
func (p *petView) protocol() gfx.Protocol {
	g := p.a.arena.graphics
	switch p.renderMode() {
	case "auto":
		if proto := g.Protocol(); proto == gfx.Sixel || proto == gfx.ITerm {
			return proto
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

// Asks the terminal what it draws the first time the pet may want sixel or iTerm2 images
func (p *petView) probe() tea.Cmd {
	mode := p.renderMode()
	if !p.enabled || mode == "unicode" || mode == "off" || mode == "kitty" || p.a.arena.probed {
		return nil
	}
	p.a.arena.probed = true
	return p.a.arena.graphics.Probe()
}

// Fetches the spritesheet of the revision and encodes its frames beside the UI once the sixel or iTerm2 path wants them
func (p *petView) pullSheet() tea.Cmd {
	proto := p.protocol()
	if !p.enabled || p.sheetLoading || proto == gfx.None || (p.sheet != nil && p.sheet.proto == proto && p.sheetRev == p.revision) {
		return nil
	}
	p.sheetLoading = true
	slug, scale := p.slug, p.scale
	params := tuigateway.PetInfoParams{}
	if p.sheet != nil && p.sheet.proto == proto && p.sheetRev != "" {
		params.KnownRevision = p.sheetRev
	}
	return quietPet(p, tuigateway.PetInfo, params, func(info *tuigateway.PetInfoResult) tea.Cmd {
		if info == nil || !info.Enabled || info.Slug != slug {
			p.sheetLoading = false
			return nil
		}
		revision := info.SpritesheetRevision
		if info.SpritesheetUnchanged {
			p.sheetLoading, p.sheetRev = false, revision
			return nil
		}
		return func() tea.Msg {
			sheet, err := petSheetEscapes(&info.PetSprite, proto, scale)
			return asyncDoneMsg{run: func() tea.Cmd {
				p.sheetLoading = false
				if err != nil || slug != p.slug {
					return nil
				}
				cmd := p.dropImage()
				p.sheet, p.sheetRev = sheet, revision
				return cmd
			}}
		}
	})
}

// Every driven state of a pet.info spritesheet scaled and encoded by the reference renderer for one protocol
func petSheetEscapes(info *tuigateway.PetSprite, proto gfx.Protocol, scale float64) (*petSheet, error) {
	raw, err := base64.StdEncoding.DecodeString(info.SpritesheetBase64)
	if err != nil {
		return nil, err
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	whole := func(n int64, fallback int) int {
		if n > 0 {
			return int(n)
		}
		return fallback
	}
	mode := "iterm"
	if proto == gfx.Sixel {
		mode = "sixel"
	}
	sheet := toNRGBA(decoded)
	out := &petSheet{proto: proto, frames: map[string][]string{}, size: map[string][2]int{}}
	for _, state := range pet.States {
		frames, w, h := pet.SheetFrameEscapes(sheet, string(state), mode, whole(info.FrameW, pet.FrameW), whole(info.FrameH, pet.FrameH),
			whole(info.FramesPerState, pet.FramesPerState), scale)
		out.frames[string(state)], out.size[string(state)] = frames, [2]int{w, h}
	}
	return out, nil
}

// Cell box the encoded image of the painted state fills, the reference box for iTerm2 and the measured pixels for sixel
func (p *petView) imageBox() (int, int, bool) {
	s := p.sheet
	proto := p.protocol()
	if s == nil || proto == gfx.None || s.proto != proto || p.paint == nil || p.paint.kitty != nil || len(s.frames[p.paintState]) == 0 {
		return 0, 0, false
	}
	size := s.size[p.paintState]
	if proto == gfx.ITerm {
		return max(1, size[0]/petCellPxW), max(1, size[1]/petCellPxH), true
	}
	cw, ch := p.a.arena.graphics.Cell()
	cw, ch = max(cw, 1), max(ch, 1)
	return (size[0] + cw - 1) / cw, (size[1] + ch - 1) / ch, true
}

// Sends the encoded frame painted now where the sprite sits, the surface swapped when the protocol changed
func (p *petView) drawImage() tea.Cmd {
	proto := p.protocol()
	var cmds []tea.Cmd
	if p.surface != nil && (p.surface.Protocol() != proto || !p.spot.visible) {
		cmds = append(cmds, p.dropImage())
	}
	cols, rows, ok := p.imageBox()
	if !ok || !p.spot.visible {
		return tea.Batch(cmds...)
	}
	if p.surface == nil {
		p.surface = p.a.arena.graphics.NewSurface(proto)
		p.surface.SetRate(int((time.Second + petFrameEvery - 1) / petFrameEvery))
	}
	s := p.surface
	if !s.Ready(time.Now()) {
		return tea.Batch(cmds...)
	}
	frames := p.sheet.frames[p.paintState]
	return tea.Batch(append(cmds, p.emit(s, s.Encoded([]byte(frames[p.shown%len(frames)]), cols, rows), cols, rows))...)
}

// Sends a packed frame where the sprite sits, a placed image taken down first once it moved or got covered
func (p *petView) emit(s *gfx.Surface, packet *gfx.Packet, cols, rows int) tea.Cmd {
	if packet == nil || s != p.surface || !p.spot.visible || p.spot.cols != cols || p.spot.rows != rows {
		return nil
	}
	x, y := p.ox+p.spot.x, p.oy+p.spot.y
	if !p.clear() || (p.placedAt != nil && *p.placedAt != [2]int{x, y}) {
		return p.takeDown()
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

// Name of the way the pet is drawn now, kitty from pet.cells, a sixel or iTerm2 image, or half blocks
func (p *petView) drawing() string {
	switch {
	case p.paint != nil && p.paint.kitty != nil:
		return gfx.KittyCells.String()
	case p.surface != nil && p.placedAt != nil:
		return p.surface.Protocol().String()
	}
	return tr("slashCmd.session.pet.drawingCells")
}

// Screen cell where the session window content starts, set before it paints
func (p *petView) origin(x, y int) { p.ox, p.oy = x, y }

// Cells the pet leaves without background for an image under them, none since its images are drawn over the cells
func (p *petView) holes() []wm.Box { return nil }

// Rows of the sprite as it shows now, the kitty placeholders, blank cells under a sixel or iTerm2 image, else half blocks
func (p *petView) spriteRows() ([]string, int, int) {
	if !p.enabled || p.paint == nil {
		return nil, 0, 0
	}
	if k := p.paint.kitty; k != nil {
		var r, g, b int
		_, err := fmt.Sscanf(k.color, "#%02x%02x%02x", &r, &g, &b)
		rows := make([]string, len(k.placeholder))
		for i, row := range k.placeholder {
			rows[i] = row
			if err == nil {
				rows[i] = fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[39m", r, g, b, row)
			}
		}
		return rows, p.paint.cols, p.paint.high
	}
	if cols, high, ok := p.imageBox(); ok {
		return slices.Repeat([]string{strings.Repeat(" ", cols)}, high), cols, high
	}
	if len(p.paint.rows) == 0 {
		return nil, 0, 0
	}
	return p.paint.rows[p.shown%len(p.paint.rows)], p.paint.cols, p.paint.high
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
func petCellFrames(list [][][][8]int64) *petFrames {
	out := &petFrames{}
	for _, grid := range list {
		cells := make([][]pet.Cell, 0, len(grid))
		for _, row := range grid {
			line := make([]pet.Cell, len(row))
			for x, values := range row {
				for i, n := range values {
					line[x][i/4][i%4] = uint8(min(max(n, 0), 255))
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

// Copy of an image as non premultiplied RGBA with its origin at zero
func toNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Rect, src, b.Min, draw.Src)
	return out
}
