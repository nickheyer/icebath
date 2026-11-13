package simplex

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/gen2brain/avif"
	_ "github.com/gen2brain/heic"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

const (
	thumbSide    = 128
	thumbQuality = 70
	thumbPrefix  = "data:image/jpg;base64,"
)

// Path of a PNG or JPEG copy SimpleX shows inline and the JPEG preview data URI of the image
func prepareImage(ctx context.Context, path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return "", "", err
	}
	sendPath := path
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg":
	default:
		if sendPath, err = writePNG(ctx, img); err != nil {
			return "", "", err
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumbnail(img), &jpeg.Options{Quality: thumbQuality}); err != nil {
		return "", "", err
	}
	return sendPath, thumbPrefix + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// Writes the image as PNG into the image cache and returns the new path
func writePNG(ctx context.Context, img image.Image) (string, error) {
	dir, err := mediacache.ImageCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		return "", err
	}
	out, err := os.CreateTemp(dir, "simplex_*.png")
	if err != nil {
		return "", err
	}
	if err := png.Encode(out, img); err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", fmt.Errorf("encode PNG: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// Image shrunk to fit the preview box with its aspect kept and transparency flattened onto white
func thumbnail(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > thumbSide || h > thumbSide {
		scale := math.Min(float64(thumbSide)/float64(w), float64(thumbSide)/float64(h))
		w = max(1, int(math.Round(float64(w)*scale)))
		h = max(1, int(math.Round(float64(h)*scale)))
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}
