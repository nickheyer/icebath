package pet

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"log/slog"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pypil"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

var (
	renderLog   = slog.Default().With("logger", "agent.pet.render")
	RenderModes = []string{"auto", "kitty", "iterm", "sixel", "unicode", "off"}
)

const (
	blankAlpha       = 8
	cellW, cellH     = 8, 16
	kittyPlaceholder = "\U0010eeee"
)

var rowColDiacritics = []rune{
	0x0305, 0x030D, 0x030E, 0x0310, 0x0312, 0x033D, 0x033E, 0x033F, 0x0346, 0x034A, 0x034B, 0x034C, 0x0350, 0x0351, 0x0352, 0x0357, 0x035B, 0x0363, 0x0364, 0x0365,
	0x0366, 0x0367, 0x0368, 0x0369, 0x036A, 0x036B, 0x036C, 0x036D, 0x036E, 0x036F, 0x0483, 0x0484, 0x0485, 0x0486, 0x0487, 0x0592, 0x0593, 0x0594, 0x0595, 0x0597,
	0x0598, 0x0599, 0x059C, 0x059D, 0x059E, 0x059F, 0x05A0, 0x05A1, 0x05A8, 0x05A9, 0x05AB, 0x05AC, 0x05AF, 0x05C4, 0x0610, 0x0611, 0x0612, 0x0613, 0x0614, 0x0615,
	0x0616, 0x0617, 0x0657, 0x0658, 0x0659, 0x065A, 0x065B, 0x065D, 0x065E, 0x06D6, 0x06D7, 0x06D8, 0x06D9, 0x06DA, 0x06DB, 0x06DC, 0x06DF, 0x06E0, 0x06E1, 0x06E2,
	0x06E4, 0x06E7, 0x06E8, 0x06EB, 0x06EC, 0x0730, 0x0732, 0x0733, 0x0735, 0x0736, 0x073A, 0x073D, 0x073F, 0x0740, 0x0741, 0x0743, 0x0745, 0x0747, 0x0749, 0x074A,
	0x07EB, 0x07EC, 0x07ED, 0x07EE, 0x07EF, 0x07F0, 0x07F1, 0x07F3, 0x0816, 0x0817, 0x0818, 0x0819, 0x081B, 0x081C, 0x081D, 0x081E, 0x081F, 0x0820, 0x0821, 0x0822,
	0x0823, 0x0825, 0x0826, 0x0827, 0x0829, 0x082A, 0x082B, 0x082C, 0x082D, 0x0951, 0x0953, 0x0954, 0x0F82, 0x0F83, 0x0F86, 0x0F87, 0x135D, 0x135E, 0x135F, 0x17DD,
	0x193A, 0x1A17, 0x1A75, 0x1A76, 0x1A77, 0x1A78, 0x1A79, 0x1A7A, 0x1A7B, 0x1A7C, 0x1B6B, 0x1B6D, 0x1B6E, 0x1B6F, 0x1B70, 0x1B71, 0x1B72, 0x1B73, 0x1CD0, 0x1CD1,
	0x1CD2, 0x1CDA, 0x1CDB, 0x1CE0, 0x1DC0, 0x1DC1, 0x1DC3, 0x1DC4, 0x1DC5, 0x1DC6, 0x1DC7, 0x1DC8, 0x1DC9, 0x1DCB, 0x1DCC, 0x1DD1, 0x1DD2, 0x1DD3, 0x1DD4, 0x1DD5,
	0x1DD6, 0x1DD7, 0x1DD8, 0x1DD9, 0x1DDA, 0x1DDB, 0x1DDC, 0x1DDD, 0x1DDE, 0x1DDF, 0x1DE0, 0x1DE1, 0x1DE2, 0x1DE3, 0x1DE4, 0x1DE5, 0x1DE6, 0x1DFE, 0x20D0, 0x20D1,
	0x20D4, 0x20D5, 0x20D6, 0x20D7, 0x20DB, 0x20DC, 0x20E1, 0x20E7, 0x20E9, 0x20F0, 0x2CEF, 0x2CF0, 0x2CF1, 0x2DE0, 0x2DE1, 0x2DE2, 0x2DE3, 0x2DE4, 0x2DE5, 0x2DE6,
	0x2DE7, 0x2DE8, 0x2DE9, 0x2DEA, 0x2DEB, 0x2DEC, 0x2DED, 0x2DEE, 0x2DEF, 0x2DF0, 0x2DF1, 0x2DF2, 0x2DF3, 0x2DF4, 0x2DF5, 0x2DF6, 0x2DF7, 0x2DF8, 0x2DF9, 0x2DFA,
	0x2DFB, 0x2DFC, 0x2DFD, 0x2DFE, 0x2DFF, 0xA66F, 0xA67C, 0xA67D, 0xA6F0, 0xA6F1, 0xA8E0, 0xA8E1, 0xA8E2, 0xA8E3, 0xA8E4, 0xA8E5, 0xA8E6, 0xA8E7, 0xA8E8, 0xA8E9,
	0xA8EA, 0xA8EB, 0xA8EC, 0xA8ED, 0xA8EE, 0xA8EF, 0xA8F0, 0xA8F1, 0xAAB0, 0xAAB2, 0xAAB3, 0xAAB7, 0xAAB8, 0xAABE, 0xAABF, 0xAAC1, 0xFE20, 0xFE21, 0xFE22, 0xFE23,
	0xFE24, 0xFE25, 0xFE26, 0x10A0F, 0x10A38, 0x1D185, 0x1D186, 0x1D187, 0x1D188, 0x1D189, 0x1D1AA, 0x1D1AB, 0x1D1AC, 0x1D1AD, 0x1D242, 0x1D243, 0x1D244,
}

// Cell is one half block cell, its top and bottom pixel as red, green, blue and alpha
type Cell [2][4]uint8

func isWezterm() bool {
	return strings.ToLower(os.Getenv("TERM_PROGRAM")) == "wezterm" || os.Getenv("WEZTERM_PANE") != ""
}

// DetectTerminalGraphics names the richest protocol the environment variables promise, never querying the terminal
func DetectTerminalGraphics() string {
	term := strings.ToLower(os.Getenv("TERM"))
	termProgram := strings.ToLower(os.Getenv("TERM_PROGRAM"))
	if termProgram == "vscode" {
		return "unicode"
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" || strings.Contains(term, "kitty") || strings.Contains(term, "ghostty") || termProgram == "ghostty" || isWezterm() {
		return "kitty"
	}
	if termProgram == "iterm.app" || os.Getenv("ITERM_SESSION_ID") != "" {
		return "iterm"
	}
	if termProgram == "mintty" || strings.Contains(term, "foot") || strings.Contains(term, "mlterm") || strings.Contains(term, "sixel") {
		return "sixel"
	}
	return "unicode"
}

// Reports a terminal that paints the kitty virtual image cells, which WezTerm speaks but cannot draw
func SupportsKittyPlaceholders() bool { return DetectTerminalGraphics() == "kitty" && !isWezterm() }

// ResolveMode returns the effective render mode from the configured one and the environment, off when not a tty
func ResolveMode(configured string, isTTY bool) string {
	mode := strings.ToLower(strings.TrimSpace(configured))
	if mode == "" || !slices.Contains(RenderModes, mode) {
		mode = "auto"
	}
	if mode == "off" || !isTTY {
		return "off"
	}
	if mode == "auto" {
		return DetectTerminalGraphics()
	}
	return mode
}

func frameIsBlank(frame *image.NRGBA) bool {
	for i := 3; i < len(frame.Pix); i += 4 {
		if frame.Pix[i] > blankAlpha {
			return false
		}
	}
	return true
}

// Copy of the box of an image, the area outside the image left transparent
func crop(img *image.NRGBA, box image.Rectangle) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, box.Dx(), box.Dy()))
	src := box.Intersect(img.Rect)
	for y := src.Min.Y; y < src.Max.Y; y++ {
		from := img.PixOffset(src.Min.X, y)
		to := out.PixOffset(src.Min.X-box.Min.X, y-box.Min.Y)
		copy(out.Pix[to:to+4*src.Dx()], img.Pix[from:from+4*src.Dx()])
	}
	return out
}

type frameKey struct {
	sheet, state                           string
	frameW, frameH, frames, scaleW, scaleH int
}

// frameCache is a least recently used cache of decoded frame rows
type frameCache struct {
	mu      sync.Mutex
	size    int
	order   []frameKey
	entries map[frameKey][]*image.NRGBA
}

func (c *frameCache) get(key frameKey, build func() []*image.NRGBA) []*image.NRGBA {
	c.mu.Lock()
	if frames, ok := c.entries[key]; ok {
		c.order = append(slices.DeleteFunc(c.order, func(k frameKey) bool { return k == key }), key)
		c.mu.Unlock()
		return frames
	}
	c.mu.Unlock()
	frames := build()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		if len(c.order) >= c.size {
			delete(c.entries, c.order[0])
			c.order = c.order[1:]
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = frames
	return frames
}

var (
	rawFrameCache    = &frameCache{size: 16, entries: map[frameKey][]*image.NRGBA{}}
	scaledFrameCache = &frameCache{size: 8, entries: map[frameKey][]*image.NRGBA{}}
)

// Cropped frames of one state row up to the first blank column, none when the sheet does not decode
func rawFrames(sheetPath, state string, frameW, frameH, framesPerState int) []*image.NRGBA {
	return rawFrameCache.get(frameKey{sheet: sheetPath, state: state, frameW: frameW, frameH: frameH, frames: framesPerState}, func() (frames []*image.NRGBA) {
		defer func() {
			if r := recover(); r != nil {
				renderLog.Debug(fmt.Sprintf("pet frame decode failed (%s, %s): %v", sheetPath, state, r))
				frames = nil
			}
		}()
		f, err := os.Open(sheetPath)
		if err != nil {
			renderLog.Debug(fmt.Sprintf("pet frame decode failed (%s, %s): %s", sheetPath, state, err))
			return nil
		}
		defer f.Close()
		decoded, _, err := image.Decode(f)
		if err != nil {
			renderLog.Debug(fmt.Sprintf("pet frame decode failed (%s, %s): %s", sheetPath, state, err))
			return nil
		}
		sheet := pypil.ToNRGBA(decoded)
		width, height := sheet.Rect.Dx(), sheet.Rect.Dy()
		cols, rows := max(1, width/frameW), max(1, height/frameH)
		top := min(StateRowIndex(state, rows)*frameH, max(0, height-frameH))
		for i := 0; i < min(framesPerState, cols); i++ {
			frame := crop(sheet, image.Rect(i*frameW, top, (i+1)*frameW, top+frameH))
			if frameIsBlank(frame) {
				break
			}
			frames = append(frames, frame)
		}
		return frames
	})
}

// Raw frames scaled to the given pixel box, both steps cached
func framesFor(sheetPath, state string, frameW, frameH, framesPerState, scaleW, scaleH int) []*image.NRGBA {
	key := frameKey{sheet: sheetPath, state: state, frameW: frameW, frameH: frameH, frames: framesPerState, scaleW: scaleW, scaleH: scaleH}
	return scaledFrameCache.get(key, func() []*image.NRGBA {
		raw := rawFrames(sheetPath, state, frameW, frameH, framesPerState)
		if len(raw) == 0 || (scaleW == frameW && scaleH == frameH) {
			return append([]*image.NRGBA{}, raw...)
		}
		out := make([]*image.NRGBA, len(raw))
		for i, f := range raw {
			out[i] = pypil.ResizeLanczos(f, scaleW, scaleH)
		}
		return out
	})
}

// StateFrameCounts maps every driven state to its frame count with the blank padding trimmed
func StateFrameCounts(sheetPath string) map[string]int {
	out := map[string]int{}
	for _, s := range States {
		out[string(s)] = len(rawFrames(sheetPath, string(s), FrameW, FrameH, FramesPerState))
	}
	return out
}

func pngB64(frame *image.NRGBA) string {
	var buf bytes.Buffer
	if err := png.Encode(&buf, frame); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// Bounding box of the pixels with any alpha, false for a fully transparent frame
func alphaBBox(frame *image.NRGBA) (image.Rectangle, bool) {
	box, found := image.Rectangle{}, false
	for y := 0; y < frame.Rect.Dy(); y++ {
		for x := 0; x < frame.Rect.Dx(); x++ {
			if frame.Pix[frame.PixOffset(x, y)+3] == 0 {
				continue
			}
			cell := image.Rect(x, y, x+1, y+1)
			if !found {
				box, found = cell, true
			} else {
				box = box.Union(cell)
			}
		}
	}
	return box, found
}

// Crops the frames to their union opaque box and resizes them to whole terminal cells
func fitFramesToCellGrid(frames []*image.NRGBA) []*image.NRGBA {
	union, found := image.Rectangle{}, false
	for _, f := range frames {
		if box, ok := alphaBBox(f); ok {
			if !found {
				union, found = box, true
			} else {
				union = union.Union(box)
			}
		}
	}
	if found {
		cropped := make([]*image.NRGBA, len(frames))
		for i, f := range frames {
			cropped[i] = crop(f, union)
		}
		frames = cropped
	}
	w, h := frames[0].Rect.Dx(), frames[0].Rect.Dy()
	targetW := max(1, int(math.RoundToEven(float64(w)/cellW))) * cellW
	targetH := max(1, int(math.RoundToEven(float64(h)/cellH))) * cellH
	if w == targetW && h == targetH {
		return frames
	}
	out := make([]*image.NRGBA, len(frames))
	for i, f := range frames {
		out[i] = pypil.ResizeLanczos(f, targetW, targetH)
	}
	return out
}

// Kitty APC escape for the data, chunked into pieces of at most 4096 bytes
func kittyAPC(ctrl, data string) string {
	pieces := []string{}
	for i := 0; i < len(data); i += 4096 {
		pieces = append(pieces, data[i:min(i+4096, len(data))])
	}
	if len(pieces) == 0 {
		pieces = []string{""}
	}
	var b strings.Builder
	for i, piece := range pieces {
		b.WriteString("\x1b_G")
		if i == 0 {
			b.WriteString(ctrl + ",")
		}
		more := 0
		if i != len(pieces)-1 {
			more = 1
		}
		b.WriteString("m=" + strconv.Itoa(more) + ";" + piece + "\x1b\\")
	}
	return b.String()
}

// Terminal cell box of a scaled frame at eight by sixteen pixels per cell
func cellBox(frame *image.NRGBA) (int, int) {
	return max(1, frame.Rect.Dx()/cellW), max(1, frame.Rect.Dy()/cellH)
}

func encodeKitty(frame *image.NRGBA) string {
	cols, rows := cellBox(frame)
	return kittyAPC(fmt.Sprintf("f=100,a=T,q=2,c=%d,r=%d", cols, rows), pngB64(frame))
}

// Deterministic non zero image id of a slug, carried to the terminal in the cell foreground colour
func KittyImageID(slug string) int { return int(crc32.ChecksumIEEE([]byte(slug))%0x7FFE) + 1 }

// KittyColorHex is the foreground colour that encodes an image id for kitty
func KittyColorHex(imageID int) string { return fmt.Sprintf("#%06x", imageID&0xFFFFFF) }

// Builds the text grid kitty paints the image under, each row led by its row diacritic
func KittyPlaceholderRows(cols, rows int) []string {
	cols = max(1, cols)
	out := make([]string, max(1, rows))
	for r := range out {
		out[r] = kittyPlaceholder + string(rowColDiacritics[min(r, len(rowColDiacritics)-1)]) + strings.Repeat(kittyPlaceholder, cols-1)
	}
	return out
}

func encodeKittyVirtual(frame *image.NRGBA, imageID, cols, rows int) string {
	return kittyAPC(fmt.Sprintf("a=T,U=1,i=%d,c=%d,r=%d,f=100,q=2", imageID, cols, rows), pngB64(frame))
}

func encodeIterm(frame *image.NRGBA) string {
	payload := pngB64(frame)
	cols, rows := cellBox(frame)
	return fmt.Sprintf("\x1b]1337;File=inline=1;size=%d;preserveAspectRatio=1;width=%d;height=%d:%s\x07", len(payload), cols, rows, payload)
}

// Rows of half block cells of a frame downscaled to the target column count
func downscaleCells(frame *image.NRGBA, targetCols int) [][]Cell {
	targetCols = max(4, targetCols)
	aspect := float64(frame.Rect.Dy()) / float64(max(1, frame.Rect.Dx()))
	targetRows := max(2, int(math.RoundToEven(float64(targetCols)*aspect*0.5))*2)
	small := pypil.ResizeLanczos(frame, targetCols, targetRows)
	pixel := func(x, y int) [4]uint8 {
		i := small.PixOffset(x, y)
		return [4]uint8{small.Pix[i], small.Pix[i+1], small.Pix[i+2], small.Pix[i+3]}
	}
	var grid [][]Cell
	for y := 0; y < targetRows; y += 2 {
		row := make([]Cell, targetCols)
		for x := range row {
			row[x][0] = pixel(x, y)
			if y+1 < targetRows {
				row[x][1] = pixel(x, y+1)
			}
		}
		grid = append(grid, row)
	}
	return grid
}

func encodeUnicode(frame *image.NRGBA, targetCols int) string {
	var rows []string
	for _, row := range downscaleCells(frame, targetCols) {
		var b strings.Builder
		for _, cell := range row {
			top, bottom := cell[0], cell[1]
			if top[3] < 32 && bottom[3] < 32 {
				b.WriteString("\x1b[0m ")
				continue
			}
			fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀", top[0], top[1], top[2], bottom[0], bottom[1], bottom[2])
		}
		rows = append(rows, b.String()+"\x1b[0m")
	}
	return strings.Join(rows, "\n")
}

// Renderer holds a pet spritesheet and yields encoded frames per state and index
type Renderer struct {
	Spritesheet                    string
	Mode                           string
	Scale                          float64
	UnicodeCols                    int
	FrameW, FrameH, FramesPerState int
}

// KittyPayload carries one state as kitty virtual placements, the transmit escapes plus the static text grid
type KittyPayload struct {
	Cols, Rows  int
	Placeholder []string
	Frames      []string
}

// NewRenderer builds a renderer with the frame geometry defaults, an unknown mode reading as unicode
func NewRenderer(spritesheet, mode string, scale float64, unicodeCols int) *Renderer {
	if !slices.Contains(RenderModes, mode) {
		mode = "unicode"
	}
	return &Renderer{Spritesheet: spritesheet, Mode: mode, Scale: scale, UnicodeCols: unicodeCols, FrameW: FrameW, FrameH: FrameH, FramesPerState: FramesPerState}
}

// Available reports a renderer that is on and whose spritesheet file exists
func (r *Renderer) Available() bool {
	info, err := os.Stat(r.Spritesheet)
	return r.Mode != "off" && err == nil && info.Mode().IsRegular()
}

// FrameCount returns how many frames the state animates through
func (r *Renderer) FrameCount(state string) int { return len(r.frames(state)) }

func (r *Renderer) frames(state string) []*image.NRGBA {
	scaleW, scaleH := max(1, int(float64(r.FrameW)*r.Scale)), max(1, int(float64(r.FrameH)*r.Scale))
	return framesFor(r.Spritesheet, state, r.FrameW, r.FrameH, r.FramesPerState, scaleW, scaleH)
}

// Cells returns one frame as a half block grid at cols columns, the renderer width when cols is zero
func (r *Renderer) Cells(state string, index, cols int) [][]Cell {
	frames := r.frames(state)
	if len(frames) == 0 {
		return [][]Cell{}
	}
	if cols == 0 {
		cols = r.UnicodeCols
	}
	return downscaleCells(frames[pyMod(index, len(frames))], cols)
}

// Builds the virtual placement escapes and text grid of a state, nil when the state has no frames
func (r *Renderer) KittyPayload(state string, imageID int) *KittyPayload {
	frames := r.frames(state)
	if len(frames) == 0 {
		return nil
	}
	frames = fitFramesToCellGrid(frames)
	cols, rows := cellBox(frames[0])
	encoded := make([]string, len(frames))
	for i, f := range frames {
		encoded[i] = encodeKittyVirtual(f, imageID, cols, rows)
	}
	return &KittyPayload{Cols: cols, Rows: rows, Placeholder: KittyPlaceholderRows(cols, rows), Frames: encoded}
}

// Frame returns the encoded escape string of one frame, empty when the renderer is off or the state has none
func (r *Renderer) Frame(state string, index int) string {
	frames := r.frames(state)
	if r.Mode == "off" || len(frames) == 0 {
		return ""
	}
	frame := frames[pyMod(index, len(frames))]
	if r.Mode == "sixel" {
		return encodeSixel(frame)
	}
	return func() (out string) {
		defer func() {
			if rec := recover(); rec != nil {
				renderLog.Debug(fmt.Sprintf("pet frame encode failed (mode=%s): %v", r.Mode, rec))
				out = ""
			}
		}()
		switch r.Mode {
		case "kitty":
			return encodeKitty(frame)
		case "iterm":
			return encodeIterm(frame)
		}
		return encodeUnicode(frame, r.UnicodeCols)
	}()
}

// Python modulo of an index by a positive length
func pyMod(i, n int) int { return ((i % n) + n) % n }

// BuildRenderer resolves the mode from the configured one and the environment and constructs the renderer
func BuildRenderer(spritesheet, configuredMode string, scale float64, unicodeCols int, isTTY bool) *Renderer {
	return NewRenderer(spritesheet, ResolveMode(configuredMode, isTTY), scale, unicodeCols)
}
