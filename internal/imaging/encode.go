package imaging

import (
	"fmt"
	"image"
	"io"

	"github.com/gen2brain/avif"
)

// Encodes the image in a container at its default settings, JPEG and AVIF at quality 75 and WebP lossless
func Encode(w io.Writer, img image.Image, f Format) error {
	if err := requireArea(img); err != nil {
		return err
	}
	switch f {
	case PNG:
		return EncodePNG(w, img, PNGOptions{})
	case JPEG:
		return EncodeJPEG(w, img, JPEGOptions{Quality: 75})
	case GIF:
		return encodeGIF(w, img)
	case WebP:
		return EncodeWebP(w, img)
	case BMP:
		return encodeBMP(w, img)
	case TIFF:
		return encodeTIFF(w, img)
	case ICO:
		return encodeICO(w, img)
	case AVIF:
		src := image.Image(ToNRGBA(img))
		if !hasAlpha(img) {
			src = ToRGB(img)
		}
		return avif.Encode(w, src, avif.Options{Quality: 75, QualityAlpha: 75, Speed: 6, ChromaSubsampling: image.YCbCrSubsampleRatio420})
	}
	return fmt.Errorf("%w: cannot write %s images", ErrUnsupported, f)
}

// Refuses an image without pixels, which no container can hold
func requireArea(img image.Image) error {
	if img.Bounds().Empty() {
		return fmt.Errorf("%w: empty image", ErrUnsupported)
	}
	return nil
}

// Media type and bytes of a base64 data URL, characters outside the alphabet skipped
func DecodeDataURL(u string) (string, []byte, error) {
	mediaType, payload := splitDataURL(u)
	data, err := decodeBase64Lenient(payload)
	if err != nil {
		return mediaType, nil, err
	}
	return mediaType, data, nil
}
