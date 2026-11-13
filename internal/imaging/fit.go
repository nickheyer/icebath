package imaging

import (
	"bytes"
	"context"
	"image"
)

// Data URL length a fit aims under when no budget is given
const DefaultFitBytes = 5 * 1024 * 1024

// Budgets of one fit
type FitOptions struct {
	// Longest the data URL may be, zero meaning DefaultFitBytes
	MaxDataURLBytes int
	// Longest side in pixels, zero for no cap
	MaxDimension int
	// Re-encodes PNG sources as JPEG as well
	ForceJPEG bool
}

// Outcome of one fit
type Fit struct {
	DataURL   string
	MediaType string
	// Source dimensions, zero when the source could not be measured
	OrigWidth, OrigHeight int
	// Dimensions of the returned image, equal to the source unless Resized
	Width, Height int
	Resized       bool
	// Whether the data URL meets both budgets
	Fits bool
	// Why the source would not open and so passed through unchanged, nil when it opened or never needed to
	OpenErr error
}

// Fits image bytes under a data URL and side budget by up to four Lanczos halvings, failing on undecodable pixels
func FitDataURL(ctx context.Context, data []byte, mediaType string, o FitOptions) (Fit, error) {
	budget := o.MaxDataURLBytes
	if budget <= 0 {
		budget = DefaultFitBytes
	}
	res := Fit{MediaType: mediaType}
	cfg, cfgErr := configBytes(ctx, data)
	if err := ctx.Err(); err != nil {
		return Fit{}, err
	}
	if cfgErr == nil {
		res.OrigWidth, res.OrigHeight = cfg.Width, cfg.Height
		res.Width, res.Height = cfg.Width, cfg.Height
	}
	overDims := o.MaxDimension > 0 && cfgErr == nil && max(cfg.Width, cfg.Height) > o.MaxDimension
	raw := func() Fit {
		r := res
		r.DataURL = DataURL(mediaType, data)
		r.Fits = len(r.DataURL) <= budget && !overDims
		return r
	}
	if (len(data)*4)/3+100 <= budget && !overDims && dataURLLen(mediaType, len(data)) <= budget {
		return raw(), nil
	}
	if cfgErr != nil {
		r := raw()
		r.OpenErr = cfgErr
		return r, nil
	}
	img, info, err := decodeBytes(ctx, data, DecodeOptions{})
	if err != nil {
		return Fit{}, err
	}
	png := mediaType == "image/png" && !o.ForceJPEG
	if !png {
		switch img.(type) {
		case *image.Gray, *image.YCbCr:
		case *image.RGBA:
			if hasAlpha(img) {
				img = ToRGB(img)
			}
		default:
			img = ToRGB(img)
		}
	}
	qualities := []int{85, 70, 50}
	outType := "image/jpeg"
	if png {
		qualities, outType = []int{0}, "image/png"
	}
	ob := img.Bounds()
	origW, origH := ob.Dx(), ob.Dy()
	res.OrigWidth, res.OrigHeight = origW, origH
	prevW, prevH := origW, origH
	var candidate []byte
	cw, ch := origW, origH
	for attempt := 0; attempt < 5; attempt++ {
		if err := ctx.Err(); err != nil {
			return Fit{}, err
		}
		if attempt > 0 {
			nw, nh := max(int(float64(cw)*0.5), 64), max(int(float64(ch)*0.5), 64)
			if nw == 64 && cw > 0 {
				nh = max(int(float64(ch)*(64/float64(cw))), 64)
			} else if nh == 64 && ch > 0 {
				nw = max(int(float64(cw)*(64/float64(ch))), 64)
			}
			if nw == prevW && nh == prevH {
				break
			}
			img = Resize(img, nw, nh, Lanczos)
			prevW, prevH, cw, ch = nw, nh, nw, nh
		}
		dimsOK := o.MaxDimension <= 0 || max(cw, ch) <= o.MaxDimension
		for _, q := range qualities {
			var buf bytes.Buffer
			var err error
			if png {
				err = EncodePNG(&buf, img, PNGOptions{ICC: info.ICC})
			} else {
				err = EncodeJPEG(&buf, img, JPEGOptions{Quality: q})
			}
			if err != nil {
				return Fit{}, err
			}
			candidate = buf.Bytes()
			if dataURLLen(outType, len(candidate)) <= budget && dimsOK {
				return Fit{DataURL: DataURL(outType, candidate), MediaType: outType, OrigWidth: origW, OrigHeight: origH,
					Width: cw, Height: ch, Resized: cw != origW || ch != origH, Fits: true}, nil
			}
		}
	}
	return Fit{DataURL: DataURL(outType, candidate), MediaType: outType, OrigWidth: origW, OrigHeight: origH,
		Width: cw, Height: ch, Resized: cw != origW || ch != origH}, nil
}

// Data URL re-encoded under a budget as quality 90 JPEG or alpha PNG, the input itself when undecodable
func ShrinkDataURL(ctx context.Context, u string, budget int) (string, error) {
	_, payload := splitDataURL(u)
	data, err := decodeBase64Lenient(payload)
	if err != nil {
		return u, nil
	}
	img, info, err := decodeBytes(ctx, data, DecodeOptions{ApplyOrientation: true})
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return u, nil
	}
	alpha := hasAlpha(img)
	outType := "image/jpeg"
	if alpha {
		outType = "image/png"
		img = ToNRGBA(img)
	} else {
		img = ToRGB(img)
	}
	b := img.Bounds()
	side := max(b.Dx(), b.Dy())
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		frame := Thumbnail(img, side, side, Bicubic)
		var buf bytes.Buffer
		if alpha {
			err = EncodePNG(&buf, frame, PNGOptions{ICC: info.ICC})
		} else {
			err = EncodeJPEG(&buf, frame, JPEGOptions{Quality: 90})
		}
		if err != nil {
			return "", err
		}
		if dataURLLen(outType, buf.Len()) <= budget || side <= 256 {
			return DataURL(outType, buf.Bytes()), nil
		}
		side = int(float64(side) * 0.75)
	}
}
