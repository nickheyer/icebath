package imaging

import (
	"context"
	"errors"
	"image"
	"io"
)

// Settings of one decode
type DecodeOptions struct {
	// Rotates and flips the pixels by the orientation the Exif, XMP or container declares
	ApplyOrientation bool
}

// Facts a decode reads beside the pixels
type Info struct {
	Format Format
	// ICC profile the file embeds, nil when it carries none
	ICC []byte
}

// Container and canvas size an image opens at
type Config struct {
	Format        Format
	Width, Height int
}

// Decodes an image's first frame, palette, grey, CMYK and YCbCr layouts kept, other colour as RGBA or straight NRGBA
func Decode(ctx context.Context, r io.Reader, o DecodeOptions) (image.Image, Info, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, Info{}, err
	}
	return decodeBytes(ctx, data, o)
}

// Decode of bytes already in memory
func decodeBytes(ctx context.Context, data []byte, o DecodeOptions) (image.Image, Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, Info{}, err
	}
	f := sniffContainer(data)
	info := Info{Format: f}
	var img image.Image
	var err error
	orientation := 1
	switch f {
	case PNG:
		var m pngMeta
		img, m, err = decodePNG(data)
		info.ICC, orientation = m.icc, orientationOf(m.exif, m.xmp)
	case JPEG:
		var m jpegMeta
		img, m, err = decodeJPEG(data)
		info.ICC, orientation = m.icc, orientationOf(m.exif, m.xmp)
	case GIF:
		img, err = decodeGIF(data)
	case BMP:
		img, err = decodeBMP(data)
	case WebP:
		var wf *webpFile
		img, wf, err = decodeWebP(data)
		if wf != nil {
			info.ICC, orientation = wf.icc, orientationOf(wf.exif, wf.xmp)
		}
	case TIFF:
		img, orientation, info.ICC, err = decodeTIFF(data)
	case ICO:
		var m pngMeta
		img, m, err = decodeICO(data)
		info.ICC = m.icc
	case HEIC, AVIF:
		img, info.Format, err = decodeHEIF(data, f, o.ApplyOrientation)
		info.ICC = heifICC(data)
	default:
		return nil, info, ErrUnknownFormat
	}
	if err != nil {
		return nil, info, err
	}
	if err := ctx.Err(); err != nil {
		return nil, info, err
	}
	if o.ApplyOrientation {
		img = transpose(img, orientation)
	}
	return img, info, nil
}

// Decodes an ISO-BMFF image with the codec its brands suggest, the other codec tried when that one cannot read it
func decodeHEIF(data []byte, f Format, orient bool) (image.Image, Format, error) {
	first, second := f, AVIF
	if f == AVIF {
		second = HEIC
	}
	img, err := decodeHEIFAs(data, first, orient)
	if err == nil {
		return img, first, nil
	}
	var be *BombError
	if errors.As(err, &be) {
		return nil, first, err
	}
	if img2, err2 := decodeHEIFAs(data, second, orient); err2 == nil {
		return img2, second, nil
	}
	return nil, first, err
}

// Decodes with one ISO-BMFF codec
func decodeHEIFAs(data []byte, f Format, orient bool) (image.Image, error) {
	if f == HEIC {
		return decodeHEIC(data)
	}
	return decodeAVIF(data, orient)
}

// Container and canvas size an image opens at, icons and HEIC decoded whole to learn the size they load at
func DecodeConfig(ctx context.Context, r io.Reader) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Config{}, err
	}
	return configBytes(ctx, data)
}

// Configuration of bytes already in memory
func configBytes(ctx context.Context, data []byte) (Config, error) {
	f := sniffContainer(data)
	c := Config{Format: f}
	var err error
	switch f {
	case PNG:
		c.Width, c.Height, err = configPNG(data)
	case JPEG:
		var cfg image.Config
		cfg, err = jpegConfig(data)
		c.Width, c.Height = cfg.Width, cfg.Height
	case GIF:
		c.Width, c.Height, err = configGIF(data)
	case BMP:
		c.Width, c.Height, err = configBMP(data)
	case WebP:
		var wf *webpFile
		wf, err = parseWebP(data)
		if wf != nil {
			c.Width, c.Height = wf.canvasW, wf.canvasH
		}
	case TIFF:
		c.Width, c.Height, err = configTIFF(data)
	case ICO:
		c.Width, c.Height, err = configICO(data)
	case AVIF, HEIC:
		if f == AVIF {
			if cfg, cerr := heifConfig(data, AVIF); cerr == nil {
				c.Width, c.Height = cfg.Width, cfg.Height
				break
			}
		}
		var img image.Image
		img, c.Format, err = decodeHEIF(data, f, false)
		if img != nil {
			c.Width, c.Height = img.Bounds().Dx(), img.Bounds().Dy()
		}
	default:
		return c, ErrUnknownFormat
	}
	if err == nil {
		err = ctx.Err()
	}
	return c, err
}
