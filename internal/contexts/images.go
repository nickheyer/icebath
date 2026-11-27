package contexts

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	// Size a picture is shrunk under for a provider that refused one as too large, and the longest side it keeps by default
	shrinkBytes     = 4 * 1024 * 1024
	shrinkDimension = 8000
	// Shortest side a shrink goes down to
	shrinkFloor = 64
)

var (
	patchLimit = regexp.MustCompile(`exceeding the limit of\s*([0-9]{2,7})`)
	sideLimit  = regexp.MustCompile(`max allowed size(?:\s+for [^:]+)?:\s*([0-9]{3,5})\s*pixels?`)
)

// Longest side in pixels a provider's refusal of a picture allows, the default when it says none
func ImageLimit(message string) int {
	text := strings.ToLower(message)
	within := func(n int) int {
		if n < 512 || n > 8000 {
			return shrinkDimension
		}
		return n
	}
	if strings.Contains(text, "patches after processing") {
		if m := patchLimit.FindStringSubmatch(text); m != nil {
			if limit, err := strconv.Atoi(m[1]); err == nil {
				return within(int(math.Sqrt(float64(limit))) * 32)
			}
		}
		return shrinkDimension
	}
	if strings.Contains(text, "image") && strings.Contains(text, "dimension") && strings.Contains(text, "max allowed size") {
		if m := sideLimit.FindStringSubmatch(text); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return within(n)
			}
		}
	}
	return shrinkDimension
}

// Messages with their pictures shrunk under the size and longest side a provider takes, and whether any changed; false as well when a
// picture over the limits could not be shrunk, so sending them again would be refused again
func ShrinkImages(messages []*sessionsv1.Message, longest int) ([]*sessionsv1.Message, bool) {
	out := make([]*sessionsv1.Message, len(messages))
	changed, stuck := 0, 0
	for i, m := range messages {
		out[i] = m
		if len(m.GetMedia()) == 0 {
			continue
		}
		var media []*sessionsv1.Media
		touched := false
		for _, item := range m.GetMedia() {
			shrunk, did, over := shrink(item, longest)
			media = append(media, shrunk)
			touched = touched || did
			if did {
				changed++
			}
			if over {
				stuck++
			}
		}
		if touched {
			copied := Copy(m)
			copied.Media = media
			out[i] = copied
		}
	}
	limit := float64(shrinkBytes) / (1024 * 1024)
	if changed > 0 {
		slog.Info("image-shrink recovery: re-encoded image part(s) to fit under the limit", "parts", changed, "megabytes", limit)
	}
	if stuck > 0 {
		slog.Warn("image-shrink recovery: oversized image part(s) could not be shrunk under the limit — not retrying (would re-send rejected payload)",
			"parts", stuck, "megabytes", limit)
		return messages, false
	}
	return out, changed > 0
}

// Picture shrunk under the limits, whether it was, and whether it is over them and could not be
func shrink(media *sessionsv1.Media, longest int) (*sessionsv1.Media, bool, bool) {
	if !strings.HasPrefix(media.GetMimeType(), "image/") {
		return media, false, false
	}
	data := media.GetData()
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	known := err == nil
	over := base64.StdEncoding.EncodedLen(len(data)) > shrinkBytes || known && max(config.Width, config.Height) > longest
	if !over {
		return media, false, false
	}
	img, err := decodeImage(data, media.GetMimeType())
	if err != nil {
		slog.Warn("image-shrink recovery: re-encode failed", "error", err)
		return media, false, true
	}
	keepPNG := media.GetMimeType() == "image/png"
	current := img.Bounds().Size()
	scale := 1.0
	if side := max(current.X, current.Y); side > longest {
		scale = float64(longest) / float64(side)
	}
	for range 6 {
		size := image.Pt(max(int(float64(current.X)*scale), shrinkFloor), max(int(float64(current.Y)*scale), shrinkFloor))
		resized := img
		if size != current {
			canvas := image.NewNRGBA(image.Rectangle{Max: size})
			draw.CatmullRom.Scale(canvas, canvas.Bounds(), img, img.Bounds(), draw.Src, nil)
			resized = canvas
		}
		var encoded bytes.Buffer
		mime := "image/png"
		if keepPNG {
			err = png.Encode(&encoded, resized)
		} else {
			mime = "image/jpeg"
			err = jpeg.Encode(&encoded, opaque(resized), &jpeg.Options{Quality: 80})
		}
		if err != nil {
			slog.Warn("image-shrink recovery: re-encode failed", "error", err)
			return media, false, true
		}
		if base64.StdEncoding.EncodedLen(encoded.Len()) <= shrinkBytes && max(size.X, size.Y) <= longest {
			return &sessionsv1.Media{MimeType: mime, Data: encoded.Bytes()}, true, false
		}
		if size.X == shrinkFloor || size.Y == shrinkFloor {
			break
		}
		scale *= 0.7
		keepPNG = false
	}
	return media, false, true
}

func decodeImage(data []byte, mime string) (image.Image, error) {
	reader := bytes.NewReader(data)
	switch mime {
	case "image/png":
		return png.Decode(reader)
	case "image/jpeg", "image/jpg":
		return jpeg.Decode(reader)
	case "image/gif":
		return gif.Decode(reader)
	case "image/webp":
		return webp.Decode(reader)
	}
	img, _, err := image.Decode(reader)
	return img, err
}

// Picture on white, as JPEG keeps no transparency
func opaque(img image.Image) image.Image {
	bounds := img.Bounds()
	out := image.NewRGBA(bounds)
	draw.Draw(out, bounds, &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(out, bounds, img, bounds.Min, draw.Over)
	return out
}
