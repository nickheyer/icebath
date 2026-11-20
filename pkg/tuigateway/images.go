package tuigateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Image a client wants laid out for its transcript, the cell budget it keeps its aspect inside and never grows past
type ImageRenderParams struct {
	Path string `json:"path"`
	Cols int64  `json:"cols"`
	Rows int64  `json:"rows"`
	// Whether the client owns a real terminal that takes escapes
	Graphics bool `json:"graphics,omitzero"`
	// Image form the client's own terminal paints, kitty or unicode, since the gateway's environment names no client's terminal
	Terminal string `json:"terminal,omitzero"`
}

type ImageRenderMode string

func (ImageRenderMode) Values() []string { return []string{"kitty", "unicode", "off"} }

// Image on a grid of cells, kitty writing transmit once and painting placeholder in color, unicode painting cells as half blocks, off drawing nothing
type ImageRenderResult struct {
	Mode        ImageRenderMode `json:"mode"`
	Cols        int64           `json:"cols,omitzero"`
	Rows        int64           `json:"rows,omitzero"`
	Color       string          `json:"color,omitzero"`
	Transmit    string          `json:"transmit,omitzero"`
	Placeholder []string        `json:"placeholder,omitzero"`
	Cells       [][][8]int64    `json:"cells,omitzero"`
}

// Lays out a local image of the gateway host for an inline terminal preview
var ImageRender = NewMethod[ImageRenderParams, ImageRenderResult]("image.render")

// File suffixes of the images an inline preview decodes
var renderableImageSuffixes = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
	".tiff": true, ".tif": true, ".ico": true}

// Result fields naming an image a call produced, read from the result or from the meta of a multimodal envelope
var resultImageKeys = []string{"image_path", "screenshot_path"}

// Tools whose image is what the call showed or made, each as the source and the key holding it
var toolImageFields = map[string][][2]string{
	"vision_analyze": {{"args", "image_url"}},
	"image_generate": {{"result", "image"}},
}

// Candidate as the path of an existing local image file, empty for anything else, URLs and data URLs included
func LocalImagePath(candidate any) string {
	text, ok := candidate.(string)
	text = strings.TrimSpace(text)
	if !ok || text == "" || strings.Contains(text, "://") || strings.HasPrefix(text, "data:") {
		return ""
	}
	if text == "~" || strings.HasPrefix(text, "~/") || strings.HasPrefix(text, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			text = filepath.Join(home, text[1:])
		}
	}
	name := filepath.Base(text)
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || !renderableImageSuffixes[strings.ToLower(name[dot:])] {
		return ""
	}
	if info, err := os.Stat(text); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return text
}

// Local image a successful tool call produced or looked at, empty otherwise, named fields only so a result listing paths in free text shows nothing
func ToolImagePath(toolName string, args map[string]any, result any, isError bool) string {
	if isError {
		return ""
	}
	fields, _ := result.(map[string]any)
	if text, ok := result.(string); ok {
		if json.Unmarshal([]byte(text), &fields) != nil {
			fields = nil
		}
	}
	meta, _ := fields["meta"].(map[string]any)
	sources := map[string]map[string]any{"args": args, "result": fields}
	lookups := append([][2]string{}, toolImageFields[toolName]...)
	for _, key := range resultImageKeys {
		lookups = append(lookups, [2]string{"result", key})
	}
	for _, lookup := range lookups {
		source, key := lookup[0], lookup[1]
		if path := LocalImagePath(sources[source][key]); path != "" {
			return path
		}
		if source == "result" {
			if path := LocalImagePath(meta[key]); path != "" {
				return path
			}
		}
	}
	return ""
}
