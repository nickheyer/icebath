package tools

import (
	"bytes"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"os"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	// Longest side of the picture the chat shows of a result, twice the cells it draws it on at most so it stays sharp
	previewSide = 128
	// Largest file a preview is made from
	previewMax = 40 << 20
)

// Small copy of a picture a call saved, as a data URL the chat draws in place of the result's text; empty when it can't be read
func preview(file string) string {
	if !fsx.IsFile(file) {
		return ""
	}
	if info, err := os.Stat(file); err != nil || info.Size() > previewMax {
		return ""
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width*config.Height > 100_000_000 {
		return ""
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	w, h := config.Width, config.Height
	if w > previewSide || h > previewSide {
		if w >= h {
			w, h = previewSide, max(h*previewSide/w, 1)
		} else {
			w, h = max(w*previewSide/h, 1), previewSide
		}
	}
	small := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(small, small.Bounds(), img, img.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if small.Opaque() {
		if jpeg.Encode(&out, small, &jpeg.Options{Quality: 85}) != nil {
			return ""
		}
		return generation.DataURL("image/jpeg", out.Bytes())
	}
	if png.Encode(&out, small) != nil {
		return ""
	}
	return generation.DataURL("image/png", out.Bytes())
}
