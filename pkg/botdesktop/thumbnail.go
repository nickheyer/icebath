package botdesktop

import (
	"bytes"
	"image"
	"image/jpeg"
	"math"

	"golang.org/x/image/draw"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Largest width and height of a screen thumbnail, and its JPEG quality
const (
	ThumbMaxW    = 960
	ThumbMaxH    = 600
	ThumbQuality = 72
)

// JPEG of the running screen shrunk into the box, nil when no screen is up, read only so neither the lease nor a person in control is disturbed
func Thumbnail(env *tools.Env, maxW, maxH, quality int) ([]byte, error) {
	published, err := PublishedEnv(env)
	if err != nil {
		return nil, err
	}
	display := published["DISPLAY"]
	if display == "" {
		return nil, nil
	}
	sandboxed, err := SandboxScreenRunning(env)
	if err != nil {
		return nil, err
	}
	if sandboxed {
		sandbox, err := ownedSandboxEnv(env)
		if err != nil || sandbox == nil {
			return nil, err
		}
		return GrabJPEG(sandbox, published, maxW, maxH, quality)
	}
	if _, ok := launcherPid(env); !ok {
		return nil, nil
	}
	xauthFile := published["XAUTHORITY"]
	if xauthFile == "" {
		xauthFile = xauFileName()
	}
	frame, err := grabX11(display, xauthFile)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, shrunkToBox(frame, maxW, maxH), &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Frame scaled down to fit the box with its aspect kept, the frame itself when it already fits
func shrunkToBox(frame *image.NRGBA, maxW, maxH int) image.Image {
	w, h := frame.Rect.Dx(), frame.Rect.Dy()
	if w <= maxW && h <= maxH {
		return frame
	}
	f := min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	out := image.NewRGBA(image.Rect(0, 0, max(int(math.Round(float64(w)*f)), 1), max(int(math.Round(float64(h)*f)), 1)))
	draw.CatmullRom.Scale(out, out.Rect, frame, frame.Rect, draw.Src, nil)
	return out
}
