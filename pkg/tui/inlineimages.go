package tui

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-isatty"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	// Cell budget of a picture in the transcript
	inlineImageMaxCols, inlineImageMaxRows = 64, 16
	// Laid out pictures kept, a kitty one down to its placeholder rows once transmitted and a half block one whole
	inlineImageCacheMax = 48
	// Alpha a half block pixel needs to be drawn
	halfBlockOpaque = 32
)

// Picture of one revision of a file laid out for a cell budget
type imageKey struct {
	path, revision string
	cols, rows     int
}

// Layout of a picture as the gateway answered it, its rows painted once
type imageRender struct {
	pending bool
	// Whether the gateway refused the picture or answered that it cannot be drawn
	off   bool
	mode  tuigateway.ImageRenderMode
	color string
	size  string
	lines []string
}

// Pictures of the transcript, laid out by the gateway once per file revision and width and painted as ordinary cells
type inlineImages struct {
	a       *app
	renders map[imageKey]*imageRender
	// Keys from the least to the most recently used
	order []imageKey
	// Grid the terminal holds for each kitty image colour, one virtual placement living per image id
	placed map[string]string
	wanted []imageKey
	// Whether a flush of the wanted pictures is already on its way to the update loop
	scheduled bool
	// Count of layouts that landed, part of the identity of every block that draws pictures
	gen      int
	graphics bool
	// Pictures the terminal draws as pixels, sized from their files here, least recently used first
	pictures     map[imageKey]*placedPicture
	pictureOrder []imageKey
	// Protocol and cell size the pixel pictures are laid out for
	look pictureLook
	// Screen cell the session content starts at, the boxes the pictures in view should be drawn in and the pictures on screen
	ox, oy  int
	desired []placement
	shown   map[placement]*standing
	// Layout the drawing waits to hold still, and whether a sixel or iTerm2 picture went down so only a full repaint clears it
	layoutGen int
	repaint   bool
	// Whether the screen is being left, no picture drawn again
	closed bool
}

// Empty picture store of an app, asking for graphics when stdout is a terminal
func newInlineImages(a *app) *inlineImages {
	return &inlineImages{a: a, renders: map[imageKey]*imageRender{}, placed: map[string]string{}, graphics: isatty.IsTerminal(os.Stdout.Fd()),
		pictures: map[imageKey]*placedPicture{}, shown: map[placement]*standing{}}
}

// Image form this terminal paints, kitty placeholders or half blocks, read from the environment alone.
// It travels with image.render and pet.cells because one gateway serves many clients, so the gateway's environment names no client's terminal
func terminalGraphicsPick(env func(string) string) string {
	term, program := strings.ToLower(strings.TrimSpace(env("TERM"))), strings.ToLower(strings.TrimSpace(env("TERM_PROGRAM")))
	switch {
	case program == "vscode", env("TMUX") != "", env("STY") != "", program == "wezterm", env("WEZTERM_PANE") != "":
		return "unicode"
	case env("KITTY_WINDOW_ID") != "", env("GHOSTTY_RESOURCES_DIR") != "", strings.Contains(term, "kitty"), strings.Contains(term, "ghostty"),
		program == "ghostty":
		return "kitty"
	}
	return "unicode"
}

// Half block of two pixels, a lone opaque half drawn alone so the window shows through the other
func halfBlock(top, bottom [4]uint8) string {
	upper, lower := top[3] >= halfBlockOpaque, bottom[3] >= halfBlockOpaque
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

// Rows of a laid out picture, kitty placeholders in the colour naming the image or half blocks
func imageRows(res *tuigateway.ImageRenderResult) []string {
	if res.Mode == "kitty" {
		var r, g, b int
		if _, err := fmt.Sscanf(res.Color, "#%02x%02x%02x", &r, &g, &b); err != nil || len(res.Placeholder) == 0 {
			return nil
		}
		rows := make([]string, len(res.Placeholder))
		for i, row := range res.Placeholder {
			rows[i] = fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[39m", r, g, b, row)
		}
		return rows
	}
	rows := make([]string, len(res.Cells))
	for y, row := range res.Cells {
		var sb strings.Builder
		for _, cell := range row {
			var px [2][4]uint8
			for i, n := range cell {
				px[i/4][i%4] = uint8(min(max(n, 0), 255))
			}
			sb.WriteString(halfBlock(px[0], px[1]))
		}
		rows[y] = sb.String()
	}
	return rows
}

// Marks a key as the most recently used and drops the oldest past the cache size
func (s *inlineImages) touch(key imageKey) {
	if i := slices.Index(s.order, key); i >= 0 {
		s.order = slices.Delete(s.order, i, i+1)
	}
	s.order = append(s.order, key)
	for len(s.order) > inlineImageCacheMax {
		delete(s.renders, s.order[0])
		s.order = s.order[1:]
	}
}

// Painted rows of a picture, blank rows the terminal draws its pixels over or rows of cells, none while its layout is on the way or when it
// cannot be drawn, asking for the layout the first time
func (s *inlineImages) rows(path, revision string, cols, rows int) ([]string, bool) {
	key := imageKey{path: path, revision: revision, cols: cols, rows: rows}
	if s.placing() {
		if lines := s.pictureRows(key); lines != nil {
			return lines, true
		}
	}
	return s.cellRows(key), false
}

// Rows of cells of a picture as the gateway lays it out, kitty placeholders or half blocks
func (s *inlineImages) cellRows(key imageKey) []string {
	if r := s.renders[key]; r != nil {
		switch {
		case r.pending, r.off:
			return nil
		case r.mode != "kitty" || s.placed[r.color] == r.size:
			return r.lines
		}
	}
	if !slices.Contains(s.wanted, key) {
		s.wanted = append(s.wanted, key)
	}
	if !s.scheduled && s.a.post != nil {
		s.scheduled = true
		s.a.post(asyncDoneMsg{run: s.flush})
	}
	return nil
}

// Asks the gateway for every wanted layout, one request per picture
func (s *inlineImages) flush() tea.Cmd {
	s.scheduled = false
	wanted := s.wanted
	s.wanted = nil
	var cmds []tea.Cmd
	for _, key := range wanted {
		if r := s.renders[key]; r != nil && (r.pending || r.off) {
			continue
		}
		s.renders[key] = &imageRender{pending: true}
		s.touch(key)
		params := tuigateway.ImageRenderParams{Path: key.path, Cols: int64(key.cols), Rows: int64(key.rows), Graphics: s.graphics, Terminal: terminalGraphicsPick(getenv)}
		cmds = append(cmds, call(s.a.gw, tuigateway.ImageRender, params, func(res *tuigateway.ImageRenderResult, err error) tea.Cmd {
			return s.landed(key, res, err)
		}))
	}
	return tea.Batch(cmds...)
}

// Takes a layout, writing a kitty image to the terminal once and keeping only its placeholder rows
func (s *inlineImages) landed(key imageKey, res *tuigateway.ImageRenderResult, err error) tea.Cmd {
	r := s.renders[key]
	if r == nil {
		return nil
	}
	s.gen++
	var lines []string
	if err == nil {
		lines = imageRows(res)
	}
	if len(lines) == 0 {
		*r = imageRender{off: true}
		return nil
	}
	*r = imageRender{mode: res.Mode, color: res.Color, size: strconv.FormatInt(res.Cols, 10) + "x" + strconv.FormatInt(res.Rows, 10), lines: lines}
	if res.Mode != "kitty" || res.Transmit == "" {
		return nil
	}
	s.placed[r.color] = r.size
	return tea.Raw(s.a.graphics.Wrap(res.Transmit))
}

// Forgets the pictures that could not be drawn so a setting that changed or a file that appeared since shows them
func (s *inlineImages) retry() {
	for key, r := range s.renders {
		if r.off {
			delete(s.renders, key)
			s.order = slices.DeleteFunc(s.order, func(k imageKey) bool { return k == key })
		}
	}
	s.gen++
}

// Rows of the pictures of a transcript row under the block indent and where the ones the terminal draws as pixels sit among them, none while
// inline images are off
func imageLines(m msg, c renderCtx) ([]string, []imageSpot) {
	if c.images == nil || !c.inlineImages || len(m.Images) == 0 {
		return nil, nil
	}
	maxRows := inlineImageMaxRows
	if m.ImageRows > 0 {
		maxRows = m.ImageRows
	}
	maxCols := max(1, min(inlineImageMaxCols*maxRows/inlineImageMaxRows, c.width-transcriptIndent))
	if c.images.placing() {
		if m.ImageRows <= 0 {
			maxRows = placedMaxRows
		}
		maxCols = max(1, min(placedMaxCols, c.width-transcriptIndent))
	}
	pad := strings.Repeat(" ", transcriptIndent)
	var lines []string
	var spots []imageSpot
	for _, path := range m.Images {
		rows, placed := c.images.rows(path, m.ImageRevision, maxCols, maxRows)
		if placed && len(rows) > 0 {
			spots = append(spots, imageSpot{key: imageKey{path: path, revision: m.ImageRevision, cols: maxCols, rows: maxRows}, row: len(lines),
				col: transcriptIndent, cols: ansi.StringWidth(rows[0]), rows: len(rows)})
		}
		for _, row := range rows {
			lines = append(lines, pad+row)
		}
	}
	return lines, spots
}
