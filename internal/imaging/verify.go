package imaging

import (
	"context"
	"errors"
	"fmt"
)

// Error for a picture offset that points past the data
var errNoFrameData = errors.New("imaging: no data found for frame")

// Bounds on the decoding work a full validation may do
type Limits struct {
	// Frames decoded at most, zero for no bound
	MaxFrames int
	// Canvas pixels summed over the decoded frames at most, zero for no bound
	MaxPixels int
}

// Animation whose frame count passed the bound
type FrameLimitError struct {
	Frame int
	Max   int
}

// Frame number and the bound it crossed
func (e *FrameLimitError) Error() string {
	return fmt.Sprintf("imaging: frame %d exceeds the maximum %d validated frames", e.Frame, e.Max)
}

// Animation whose summed decoded pixel count would pass the bound
type PixelLimitError struct {
	Frame  int
	Pixels int
	Max    int
}

// Pixel count reached, the frame reaching it and the bound
func (e *PixelLimitError) Error() string {
	return fmt.Sprintf("imaging: aggregate decoded pixel count would reach %d at frame %d, exceeding the maximum %d", e.Pixels, e.Frame, e.Max)
}

// Frames of one image advanced and decoded one at a time
type frameSource interface {
	// Seeks to the next frame and reports the size it counts at, false past the last
	next() (w, h int, ok bool, err error)
	// Fully decodes the current frame
	decode() error
}

// Fully decodes every frame of the image, checking PNG checksums first and refusing work past the limits
func Verify(ctx context.Context, data []byte, lim Limits) error {
	f := sniffContainer(data)
	var src frameSource
	var err error
	switch f {
	case PNG:
		if err = verifyPNG(data); err == nil {
			src, err = newPNGFrames(data)
		}
	case JPEG:
		src, err = newJPEGFrames(data)
	case GIF:
		src, err = newGIFFrames(data)
	case WebP:
		src, err = newWebPFrames(data)
	case TIFF:
		src, err = newTIFFFrames(data)
	case BMP, ICO:
		src = &stillFrame{data: data}
	case HEIC, AVIF:
		src, err = newHEIFFrames(data, f, lim)
	default:
		return ErrUnknownFormat
	}
	if err != nil {
		return err
	}
	validated := 0
	for frame := 1; ; frame++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		w, h, ok, err := src.next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if lim.MaxFrames > 0 && frame > lim.MaxFrames {
			return &FrameLimitError{Frame: frame, Max: lim.MaxFrames}
		}
		reached := validated + w*h
		if lim.MaxPixels > 0 && reached > lim.MaxPixels {
			return &PixelLimitError{Frame: frame, Pixels: reached, Max: lim.MaxPixels}
		}
		if err := src.decode(); err != nil {
			return err
		}
		validated = reached
	}
}

// Single frame formats decoded whole
type stillFrame struct {
	data []byte
	done bool
}

// The one frame at its opened size
func (s *stillFrame) next() (int, int, bool, error) {
	if s.done {
		return 0, 0, false, nil
	}
	s.done = true
	c, err := configBytes(context.Background(), s.data)
	if err != nil {
		return 0, 0, false, err
	}
	return c.Width, c.Height, true, nil
}

// Decodes the frame
func (s *stillFrame) decode() error {
	_, _, err := decodeBytes(context.Background(), s.data, DecodeOptions{})
	return err
}
