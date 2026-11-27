package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"go.yaml.in/yaml/v3"
	"golang.org/x/image/draw"

	"github.com/nickheyer/icebath/internal/generation"
	generatetools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/generate/tools"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

const (
	// Cells a picture takes in the transcript at most
	maxCols, maxRows = 64, 16
	// Pictures laid out kept, the least recently drawn going first
	kept = 48
	// Alpha a half of a cell needs to be drawn rather than showing the window behind it
	opaqueEnough = 32
)

// Rows of the pictures laid out so far, by preview, width and skin
type views struct {
	mu    sync.Mutex
	rows  map[string][]string
	order []string
}

// The picture image_generate made is drawn in the chat in half blocks, under a line naming what drew it; video_generate's result is the
// line and where the video is
func (v *views) Views() []toolsapp.View {
	return []toolsapp.View{
		{Tool: generatetools.Image, Result: v.picture, Settings: []string{toolsconfig.InlineImages}},
		{Tool: generatetools.Video, Result: film},
	}
}

// What a result says, as far as the chat shows it
type result struct {
	Success  bool    `json:"success"`
	Image    string  `json:"image"`
	Cached   string  `json:"cached_image"`
	Video    string  `json:"video"`
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	Modality string  `json:"modality"`
	Duration float64 `json:"duration"`
	Aspect   string  `json:"aspect_ratio"`
	Visible  string  `json:"agent_visible_image"`
	Revised  string  `json:"revised_prompt"`
	Notice   string  `json:"storage_notice"`
	Public   string  `json:"public_url"`
}

// Line naming what made the result
func head(kind string, r result, look theme.Theme) string {
	var about []string
	for _, part := range []string{r.Provider, r.Model} {
		if part != "" {
			about = append(about, part)
		}
	}
	if r.Modality == "image" {
		about = append(about, "from a picture")
	}
	if r.Modality == "reference" {
		about = append(about, "from references")
	}
	if r.Duration > 0 {
		about = append(about, fmt.Sprintf("%.0fs", r.Duration))
	}
	return render.Heading(kind, look) + render.Dim("  "+strings.Join(about, " · "), look)
}

// The picture drawn in half blocks with where it is kept under it; nil, so the text shows, when pictures are off, the skin has no colours or
// the call failed
func (v *views) picture(call toolsapp.Call, width int, look theme.Theme) *toolsapp.Shown {
	var r result
	if call.Failed || json.Unmarshal([]byte(call.Result), &r) != nil || !r.Success || look.Plain {
		return nil
	}
	var mode string
	if yaml.Unmarshal([]byte(call.Settings[toolsconfig.InlineImages]), &mode) != nil {
		mode = strings.Trim(strings.TrimSpace(call.Settings[toolsconfig.InlineImages]), `"'`)
	}
	if mode == toolsconfig.ImagesOff {
		return nil
	}
	shown := &toolsapp.Shown{Head: head("Image", r, look), Open: true}
	shown.Rows = v.blocks(call.Display, max(min(width-2, maxCols), 1), look)
	if data, _, err := generation.Inline(call.Display); err == nil && len(shown.Rows) > 0 {
		shown.Picture = &toolsapp.Picture{Data: data, Rows: len(shown.Rows), Cols: ansi.StringWidth(shown.Rows[0])}
		for _, path := range []string{r.Cached, r.Image} {
			if filepath.IsAbs(path) {
				shown.Picture.Path = path
				break
			}
		}
	}
	lines := []string{strings.TrimSpace(r.Image + " " + sandbox(r.Visible))}
	if r.Public != r.Image {
		lines = append(lines, r.Public)
	}
	for _, line := range append(lines, revised(r.Revised), r.Notice) {
		if line = strings.TrimSpace(line); line != "" {
			shown.Rows = append(shown.Rows, render.Truncate(render.Dim(line, look), max(width-2, 8), look))
		}
	}
	return shown
}

// Path the agent's commands see the picture at, said beside its own
func sandbox(path string) string {
	if path == "" {
		return ""
	}
	return "(" + path + " in the sandbox)"
}

func revised(prompt string) string {
	if prompt == "" {
		return ""
	}
	return "Revised prompt: " + prompt
}

// Line of the video and where it is
func film(call toolsapp.Call, width int, look theme.Theme) *toolsapp.Shown {
	var r result
	if call.Failed || json.Unmarshal([]byte(call.Result), &r) != nil || !r.Success {
		return nil
	}
	shown := &toolsapp.Shown{Head: head("Video", r, look), Open: true}
	lines := []string{r.Video}
	if r.Public != r.Video {
		lines = append(lines, r.Public)
	}
	for _, line := range append(lines, r.Notice) {
		if line = strings.TrimSpace(line); line != "" {
			shown.Rows = append(shown.Rows, render.Truncate(render.Dim(line, look), max(width-2, 8), look))
		}
	}
	return shown
}

// Rows of the preview laid out in half blocks at most cols wide, remembered for the next frame; none when there is no preview
func (v *views) blocks(preview string, cols int, look theme.Theme) []string {
	if !strings.HasPrefix(preview, "data:image/") {
		return nil
	}
	sum := fnv.New64a()
	sum.Write([]byte(preview))
	key := fmt.Sprintf("%x:%d:%s", sum.Sum64(), cols, look.Name)
	v.mu.Lock()
	rows, ok := v.rows[key]
	if ok {
		v.order = append(slices.DeleteFunc(v.order, func(k string) bool { return k == key }), key)
	}
	v.mu.Unlock()
	if ok {
		return rows
	}
	rows = lay(preview, cols, look)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rows[key] = rows
	v.order = append(v.order, key)
	for len(v.order) > kept {
		delete(v.rows, v.order[0])
		v.order = v.order[1:]
	}
	return rows
}

// The picture of a data URL fitted into cols cells across and maxRows down, each cell two pixels stacked: the upper one in the glyph's
// colour, the lower in its background; a half too clear to draw shows the window's background
func lay(preview string, cols int, look theme.Theme) []string {
	data, _, err := generation.Inline(preview)
	if err != nil {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	pw := min(cols, w)
	ph := max(pw*h/w, 1)
	if ph > 2*maxRows {
		ph = 2 * maxRows
		pw = max(ph*w/h, 1)
	}
	ph += ph % 2
	canvas := image.NewNRGBA(image.Rect(0, 0, pw, ph))
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), img, bounds, draw.Src, nil)
	br, bg, bb, _ := look.Bg.RGBA()
	behind := [3]uint8{uint8(br >> 8), uint8(bg >> 8), uint8(bb >> 8)}
	pixel := func(x, y int) [3]uint8 {
		c := canvas.NRGBAAt(x, y)
		if c.A < opaqueEnough {
			return behind
		}
		return over(c, behind)
	}
	rows := make([]string, 0, ph/2)
	for y := 0; y < ph; y += 2 {
		var line strings.Builder
		for x := range pw {
			top, bottom := pixel(x, y), pixel(x, y+1)
			fmt.Fprintf(&line, "\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm▀", top[0], top[1], top[2], bottom[0], bottom[1], bottom[2])
		}
		line.WriteString("\x1b[m")
		rows = append(rows, line.String())
	}
	return rows
}

// Colour of the pixel laid over the background by its alpha
func over(c color.NRGBA, behind [3]uint8) [3]uint8 {
	mix := func(front, back uint8) uint8 { return uint8((int(front)*int(c.A) + int(back)*(255-int(c.A))) / 255) }
	return [3]uint8{mix(c.R, behind[0]), mix(c.G, behind[1]), mix(c.B, behind[2])}
}
