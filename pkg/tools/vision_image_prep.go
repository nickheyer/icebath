package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/heic"
	"github.com/google/uuid"
	"golang.org/x/image/bmp"
	"golang.org/x/image/webp"
)

// MIME type by file extension for local image paths
var visionExtensionMimeTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".bmp": "image/bmp", ".webp": "image/webp", ".svg": "image/svg+xml",
}

// Media types the major vision providers accept inline, SVG BMP and TIFF are rejected
var anthropicSupportedMediaTypes = setOf("image/jpeg", "image/png", "image/gif", "image/webp")

// Formats the managed llama server decodes with stb_image, WebP fails silently there
var managedRuntimeImageMimes = setOf("image/png", "image/jpeg")

// Provider ids that always mean the managed llama server
var llamacppAliases = setOf("llamacpp", "llama.cpp", "llama-cpp")

// Bounds animated validation work by frame count and total decoded area
const (
	visionMaxValidatedFrameCount      = 100
	visionMaxValidatedAggregatePixels = 100_000_000
)

// Pillow MAX_IMAGE_PIXELS, opening anything over twice this is refused as a decompression bomb
const pilMaxImagePixels = 1024 * 1024 * 1024 / 4 / 3

// UnsupportedInlineImageMediaType names the image subtype of a data URL the inline wire paths reject, empty when accepted
func UnsupportedInlineImageMediaType(u string) string {
	header := strings.ToLower(strings.SplitN(u, ",", 2)[0])
	if !strings.HasPrefix(header, "data:image/") {
		return ""
	}
	subtype := strings.TrimSpace(strings.SplitN(header[len("data:image/"):], ";", 2)[0])
	if subtype == "" {
		subtype = "unknown"
	}
	mediaType := "image/" + subtype
	if subtype == "jpg" {
		mediaType = "image/jpeg"
	}
	if anthropicSupportedMediaTypes[mediaType] {
		return ""
	}
	return mediaType
}

// Suffix of the final path component the way pathlib reports it
func pySuffix(p string) string {
	name := p
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		name = p[i+1:]
	}
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

// MIME type from the file extension, JPEG when unknown
func determineMimeType(path string) string {
	if m, ok := visionExtensionMimeTypes[strings.ToLower(pySuffix(path))]; ok {
		return m
	}
	return "image/jpeg"
}

var (
	pngSignature = []byte("\x89PNG\r\n\x1a\n")
	cidRe        = regexp.MustCompile(`^\w\w\w\w$`)
)

// DetectImageMimeTypeFromBytes sniffs an image MIME type from magic bytes, empty for unknown data and SVG
func DetectImageMimeTypeFromBytes(data []byte) string { return detectImageMimeTypeFromBytes(data) }

// Magic byte MIME sniff that rejects corrupt PNG bytes before they can be embedded
func detectImageMimeTypeFromBytes(data []byte) string {
	format := sniffRasterFormat(data)
	if format == "png" {
		info, err := pngOpen(data)
		if err != nil || pngVerify(data, info) != nil {
			return ""
		}
	}
	switch format {
	case "png", "jpeg", "gif", "bmp", "webp", "heic", "avif":
		return "image/" + format
	}
	return ""
}

// Raster container name from the header bytes, empty when unrecognised
func sniffRasterFormat(data []byte) string {
	header := data[:min(len(data), 64)]
	switch {
	case bytes.HasPrefix(header, pngSignature):
		return "png"
	case bytes.HasPrefix(header, []byte{0xff, 0xd8, 0xff}):
		return "jpeg"
	case bytes.HasPrefix(header, []byte("GIF87a")), bytes.HasPrefix(header, []byte("GIF89a")):
		return "gif"
	case bytes.HasPrefix(header, []byte("BM")):
		return "bmp"
	case len(header) >= 12 && bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")):
		return "webp"
	}
	if len(header) >= 12 && bytes.Equal(header[4:8], []byte("ftyp")) {
		boxSize := int(binary.BigEndian.Uint32(header[:4]))
		limit := 16
		if boxSize >= 16 {
			limit = min(boxSize, len(header))
		}
		brands := map[string]bool{string(header[8:12]): true}
		for i := 16; i < limit-3; i += 4 {
			brands[string(header[i:i+4])] = true
		}
		for _, b := range []string{"avif", "avis", "av01"} {
			if brands[b] {
				return "avif"
			}
		}
		for _, b := range []string{"heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1"} {
			if brands[b] {
				return "heic"
			}
		}
	}
	return ""
}

// Python bytes repr of a chunk id for broken PNG messages
func pyBytesRepr(b []byte) string {
	var sb strings.Builder
	sb.WriteString("b'")
	for _, c := range b {
		switch {
		case c == '\\' || c == '\'':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			sb.WriteByte(c)
		default:
			fmt.Fprintf(&sb, "\\x%02x", c)
		}
	}
	sb.WriteString("'")
	return sb.String()
}

// One PNG chunk located inside the file
type pngChunk struct {
	typ   []byte
	data  []byte
	start int
	end   int
}

// Reads the chunk header and body at an offset, verifying the CRC when asked
func readPNGChunk(data []byte, off int, checkCRC bool) (pngChunk, error) {
	if off+8 > len(data) {
		return pngChunk{}, errors.New("truncated PNG file")
	}
	length := int(binary.BigEndian.Uint32(data[off : off+4]))
	cid := data[off+4 : off+8]
	if !cidRe.Match(cid) {
		return pngChunk{}, fmt.Errorf("broken PNG file (chunk %s)", pyBytesRepr(cid))
	}
	bodyStart := off + 8
	if length < 0 || bodyStart+length > len(data) {
		return pngChunk{}, errors.New("Truncated File Read")
	}
	body := data[bodyStart : bodyStart+length]
	end := bodyStart + length
	if checkCRC {
		if end+4 > len(data) {
			return pngChunk{}, fmt.Errorf("broken PNG file (incomplete checksum in %s)", pyBytesRepr(cid))
		}
		crc := crc32.NewIEEE()
		crc.Write(cid)
		crc.Write(body)
		if crc.Sum32() != binary.BigEndian.Uint32(data[end:end+4]) {
			return pngChunk{}, fmt.Errorf("broken PNG file (bad header checksum in %s)", pyBytesRepr(cid))
		}
	}
	return pngChunk{typ: cid, data: body, start: off, end: end + 4}, nil
}

// PNG header facts gathered while opening up to the first image data chunk
type pngInfo struct {
	width, height uint32
	depth, color  byte
	interlace     byte
	mode          string
	firstIDAT     int
	plte, trns    []byte
	numFrames     uint32
	defaultImage  bool
	preFcTL       []byte
	seq           int64
}

// Pillow modes by bit depth and colour type
var pngModes = map[[2]byte]string{
	{1, 0}: "1", {2, 0}: "L", {4, 0}: "L", {8, 0}: "L", {16, 0}: "I;16",
	{8, 2}: "RGB", {16, 2}: "RGB",
	{1, 3}: "P", {2, 3}: "P", {4, 3}: "P", {8, 3}: "P",
	{8, 4}: "LA", {16, 4}: "LA",
	{8, 6}: "RGBA", {16, 6}: "RGBA",
}

// Refuses images whose pixel count marks a likely decompression bomb
func pilBombCheck(w, h int) error {
	pixels := w * h
	if pixels > 2*pilMaxImagePixels {
		return fmt.Errorf("Image size (%d pixels) exceeds limit of %d pixels, could be decompression bomb DOS attack.", pixels, 2*pilMaxImagePixels)
	}
	return nil
}

// Parses PNG chunks up to the first image data the way Pillow opens a PNG
func pngOpen(data []byte) (*pngInfo, error) {
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, errors.New("not a PNG file")
	}
	info := &pngInfo{seq: -1}
	haveHeader := false
	off := 8
	for {
		if off+8 > len(data) {
			return nil, errors.New("truncated PNG file")
		}
		cid := data[off+4 : off+8]
		if !cidRe.Match(cid) {
			return nil, fmt.Errorf("broken PNG file (chunk %s)", pyBytesRepr(cid))
		}
		switch string(cid) {
		case "IDAT":
			if !haveHeader {
				return nil, errors.New("cannot identify image file")
			}
			info.firstIDAT = off
			if info.numFrames > 0 && info.preFcTL == nil {
				info.defaultImage = true
			}
			return info, pilBombCheck(int(info.width), int(info.height))
		case "IEND":
			return nil, errors.New("cannot identify image file")
		}
		ch, err := readPNGChunk(data, off, true)
		if err != nil {
			return nil, err
		}
		switch string(cid) {
		case "IHDR":
			if len(ch.data) < 13 {
				return nil, errors.New("Truncated IHDR chunk")
			}
			info.width = binary.BigEndian.Uint32(ch.data[0:4])
			info.height = binary.BigEndian.Uint32(ch.data[4:8])
			info.depth, info.color = ch.data[8], ch.data[9]
			mode, ok := pngModes[[2]byte{info.depth, info.color}]
			if !ok {
				return nil, fmt.Errorf("unknown PNG mode (%d, %d)", info.depth, info.color)
			}
			if ch.data[10] != 0 {
				return nil, errors.New("unknown compression method")
			}
			if ch.data[11] != 0 {
				return nil, errors.New("unknown filter category")
			}
			info.mode, info.interlace = mode, ch.data[12]
			haveHeader = true
		case "PLTE":
			info.plte = ch.data
		case "tRNS":
			info.trns = ch.data
		case "acTL":
			if len(ch.data) < 8 {
				return nil, errors.New("APNG contains truncated acTL chunk")
			}
			n := binary.BigEndian.Uint32(ch.data[0:4])
			switch {
			case info.numFrames > 0:
				info.numFrames = 0
			case n == 0 || n > 0x80000000:
			default:
				info.numFrames = n
			}
		case "fcTL":
			if err := info.acceptFcTL(ch.data); err != nil {
				return nil, err
			}
			info.preFcTL = ch.data
		}
		off = ch.end
	}
}

// Validates an APNG frame control chunk against the canvas and the sequence counter
func (info *pngInfo) acceptFcTL(s []byte) error {
	if len(s) < 26 {
		return errors.New("APNG contains truncated fcTL chunk")
	}
	seq := int64(binary.BigEndian.Uint32(s[0:4]))
	if (info.seq < 0 && seq != 0) || (info.seq >= 0 && info.seq != seq-1) {
		return errors.New("APNG contains frame sequence errors")
	}
	info.seq = seq
	w, h := uint64(binary.BigEndian.Uint32(s[4:8])), uint64(binary.BigEndian.Uint32(s[8:12]))
	px, py := uint64(binary.BigEndian.Uint32(s[12:16])), uint64(binary.BigEndian.Uint32(s[16:20]))
	if px+w > uint64(info.width) || py+h > uint64(info.height) {
		return errors.New("APNG contains invalid frames")
	}
	return nil
}

// Checks every chunk CRC from the first image data through IEND the way Pillow verify does
func pngVerify(data []byte, info *pngInfo) error {
	off := info.firstIDAT
	for {
		if off+8 > len(data) {
			return errors.New("truncated PNG file")
		}
		if string(data[off+4:off+8]) == "IEND" {
			if !cidRe.Match(data[off+4 : off+8]) {
				return fmt.Errorf("broken PNG file (chunk %s)", pyBytesRepr(data[off+4:off+8]))
			}
			return nil
		}
		ch, err := readPNGChunk(data, off, true)
		if err != nil {
			return err
		}
		off = ch.end
	}
}

// Decoded raster with the Pillow mode its source would open as
type raster struct {
	img  image.Image
	mode string
}

// Pillow mode for a decoded Go image of a format without an explicit header mode
func modeOfImage(img image.Image) string {
	switch m := img.(type) {
	case *image.Gray:
		return "L"
	case *image.Gray16:
		return "I;16"
	case *image.Paletted:
		return "P"
	case *image.CMYK:
		return "CMYK"
	case *image.YCbCr:
		return "RGB"
	case *image.NYCbCrA:
		return "RGBA"
	case interface{ Opaque() bool }:
		if m.Opaque() {
			return "RGB"
		}
	}
	return "RGBA"
}

// Canvas size and validity of the first frame, refusing decompression bombs
func rasterConfig(data []byte) (int, int, string, error) {
	format := sniffRasterFormat(data)
	var cfg image.Config
	var err error
	r := bytes.NewReader(data)
	switch format {
	case "png":
		info, perr := pngOpen(data)
		if perr != nil {
			return 0, 0, format, perr
		}
		return int(info.width), int(info.height), format, nil
	case "jpeg":
		cfg, err = jpeg.DecodeConfig(r)
	case "gif":
		cfg, err = gif.DecodeConfig(r)
	case "webp":
		cfg, err = webp.DecodeConfig(r)
	case "bmp":
		cfg, err = bmp.DecodeConfig(r)
	case "heic":
		cfg, err = heic.DecodeConfig(r)
	case "avif":
		cfg, err = avif.DecodeConfig(r)
	default:
		return 0, 0, "", errors.New("cannot identify image file")
	}
	if err != nil {
		return 0, 0, format, err
	}
	if err := pilBombCheck(cfg.Width, cfg.Height); err != nil {
		return 0, 0, format, err
	}
	return cfg.Width, cfg.Height, format, nil
}

// Opens the first frame of an image file the way Pillow Image.open presents it
func openRasterFile(path string) (*raster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return openRaster(data)
}

// Decodes the first frame of image bytes composited onto its canvas
func openRaster(data []byte) (*raster, error) {
	if _, _, _, err := rasterConfig(data); err != nil {
		return nil, err
	}
	frames, err := openFrames(data)
	if err != nil {
		return nil, err
	}
	if _, _, ok, err := frames.next(); err != nil {
		return nil, err
	} else if !ok {
		return nil, errors.New("image not found")
	}
	return frames.load()
}

// Frame sequence of an image, advanced and decoded one frame at a time
type frameSeq interface {
	// Seeks to the next frame, false at the end of the sequence
	next() (width, height int, ok bool, err error)
	// Fully decodes the current frame onto its canvas
	load() (*raster, error)
}

// Frame iterator for the container format of the bytes
func openFrames(data []byte) (frameSeq, error) {
	switch sniffRasterFormat(data) {
	case "png":
		info, err := pngOpen(data)
		if err != nil {
			return nil, err
		}
		return &apngFrames{data: data, info: info, index: -1}, nil
	case "gif":
		return newGIFFrames(data)
	case "webp":
		return newWebPFrames(data)
	case "jpeg":
		return newJPEGFrames(data)
	case "bmp", "heic", "avif":
		return &singleFrame{data: data}, nil
	}
	return nil, errors.New("cannot identify image file")
}

// One frame image formats decoded through their Go decoders
type singleFrame struct {
	data []byte
	done bool
}

func (s *singleFrame) next() (int, int, bool, error) {
	if s.done {
		return 0, 0, false, nil
	}
	s.done = true
	w, h, _, err := rasterConfig(s.data)
	if err != nil {
		return 0, 0, false, err
	}
	return w, h, true, nil
}

func (s *singleFrame) load() (*raster, error) {
	r := bytes.NewReader(s.data)
	var img image.Image
	var err error
	switch sniffRasterFormat(s.data) {
	case "bmp":
		img, err = bmp.Decode(r)
	case "heic":
		img, err = heic.Decode(r)
	case "avif":
		img, err = avif.Decode(r)
	case "jpeg":
		img, err = jpeg.Decode(r)
	default:
		return nil, errors.New("cannot identify image file")
	}
	if err != nil {
		return nil, err
	}
	return &raster{img: img, mode: modeOfImage(img)}, nil
}

// PNG and APNG frames, the default image first and then every fcTL fdAT group
type apngFrames struct {
	data    []byte
	info    *pngInfo
	index   int
	pos     int
	payload []byte
	frameW  uint32
	frameH  uint32
	total   int
	seq     int64
}

func (a *apngFrames) next() (int, int, bool, error) {
	a.index++
	if a.index == 0 {
		a.total = 1
		if a.info.numFrames > 0 {
			a.total = int(a.info.numFrames)
			if a.info.defaultImage {
				a.total++
			}
		}
		a.seq = a.info.seq
		a.frameW, a.frameH = a.info.width, a.info.height
		if a.info.preFcTL != nil {
			a.frameW = binary.BigEndian.Uint32(a.info.preFcTL[4:8])
			a.frameH = binary.BigEndian.Uint32(a.info.preFcTL[8:12])
		}
		var idat []byte
		off := a.info.firstIDAT
		for {
			ch, err := readPNGChunk(a.data, off, false)
			if err != nil || string(ch.typ) != "IDAT" {
				break
			}
			idat = append(idat, ch.data...)
			off = ch.end
		}
		a.payload, a.pos = idat, off
		return int(a.info.width), int(a.info.height), true, nil
	}
	if a.index >= a.total {
		return 0, 0, false, nil
	}
	frameStart := false
	var payload []byte
	off := a.pos
	for {
		ch, err := readPNGChunk(a.data, off, false)
		if err != nil {
			return 0, 0, false, nil
		}
		switch string(ch.typ) {
		case "IEND":
			return 0, 0, false, nil
		case "fcTL":
			if frameStart {
				return 0, 0, false, errors.New("APNG missing frame data")
			}
			if len(ch.data) < 26 {
				return 0, 0, false, errors.New("APNG contains truncated fcTL chunk")
			}
			seq := int64(binary.BigEndian.Uint32(ch.data[0:4]))
			if (a.seq < 0 && seq != 0) || (a.seq >= 0 && a.seq != seq-1) {
				return 0, 0, false, errors.New("APNG contains frame sequence errors")
			}
			a.seq = seq
			w, h := binary.BigEndian.Uint32(ch.data[4:8]), binary.BigEndian.Uint32(ch.data[8:12])
			px, py := binary.BigEndian.Uint32(ch.data[12:16]), binary.BigEndian.Uint32(ch.data[16:20])
			if uint64(px)+uint64(w) > uint64(a.info.width) || uint64(py)+uint64(h) > uint64(a.info.height) {
				return 0, 0, false, errors.New("APNG contains invalid frames")
			}
			a.frameW, a.frameH = w, h
			frameStart = true
		case "fdAT":
			if len(ch.data) < 4 {
				return 0, 0, false, errors.New("APNG contains truncated fDAT chunk")
			}
			seq := int64(binary.BigEndian.Uint32(ch.data[0:4]))
			if a.seq != seq-1 {
				return 0, 0, false, errors.New("APNG contains frame sequence errors")
			}
			a.seq = seq
			if frameStart {
				payload = append(payload, ch.data[4:]...)
				next := ch.end
				for {
					more, err := readPNGChunk(a.data, next, false)
					if err != nil || string(more.typ) != "fdAT" || len(more.data) < 4 {
						break
					}
					mseq := int64(binary.BigEndian.Uint32(more.data[0:4]))
					if a.seq != mseq-1 {
						return 0, 0, false, errors.New("APNG contains frame sequence errors")
					}
					a.seq = mseq
					payload = append(payload, more.data[4:]...)
					next = more.end
				}
				a.payload, a.pos = payload, next
				return int(a.info.width), int(a.info.height), true, nil
			}
		}
		off = ch.end
	}
}

func (a *apngFrames) load() (*raster, error) {
	if a.index == 0 && a.info.preFcTL == nil {
		img, err := png.Decode(bytes.NewReader(a.data))
		if err != nil {
			return nil, err
		}
		return &raster{img: img, mode: a.info.mode}, nil
	}
	img, err := png.Decode(bytes.NewReader(a.standalone()))
	if err != nil {
		return nil, err
	}
	return &raster{img: img, mode: a.info.mode}, nil
}

// Standalone PNG holding the current frame dimensions and image data
func (a *apngFrames) standalone() []byte {
	var buf bytes.Buffer
	buf.Write(pngSignature)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], a.frameW)
	binary.BigEndian.PutUint32(ihdr[4:8], a.frameH)
	ihdr[8], ihdr[9], ihdr[12] = a.info.depth, a.info.color, a.info.interlace
	writePNGChunk(&buf, "IHDR", ihdr)
	if a.info.plte != nil {
		writePNGChunk(&buf, "PLTE", a.info.plte)
	}
	if a.info.trns != nil {
		writePNGChunk(&buf, "tRNS", a.info.trns)
	}
	writePNGChunk(&buf, "IDAT", a.payload)
	writePNGChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

// Appends one PNG chunk with its CRC
func writePNGChunk(buf *bytes.Buffer, typ string, body []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(body)))
	copy(hdr[4:], typ)
	buf.Write(hdr[:])
	buf.Write(body)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(body)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc.Sum32())
	buf.Write(sum[:])
}

// GIF frames located by walking the block structure, each decoded as a one frame GIF
type gifFrames struct {
	data    []byte
	head    []byte
	canvasW int
	canvasH int
	pos     int
	current []byte
}

// Header, logical screen, and global colour table of a GIF
func newGIFFrames(data []byte) (*gifFrames, error) {
	if len(data) < 13 {
		return nil, errors.New("cannot identify image file")
	}
	pos := 13
	if data[10]&0x80 != 0 {
		pos += 3 << ((data[10] & 7) + 1)
	}
	if pos > len(data) {
		pos = len(data)
	}
	w := int(binary.LittleEndian.Uint16(data[6:8]))
	h := int(binary.LittleEndian.Uint16(data[8:10]))
	if err := pilBombCheck(w, h); err != nil {
		return nil, err
	}
	return &gifFrames{data: data, head: data[:pos], canvasW: w, canvasH: h, pos: pos}, nil
}

// Offset just past a run of GIF data sub blocks, clamped to the data length
func skipGIFSubBlocks(data []byte, pos int) int {
	for pos < len(data) {
		n := int(data[pos])
		pos++
		if n == 0 {
			return pos
		}
		pos += n
	}
	return len(data)
}

func (g *gifFrames) next() (int, int, bool, error) {
	var ext []byte
	for g.pos < len(g.data) {
		switch g.data[g.pos] {
		case ';':
			return 0, 0, false, nil
		case '!':
			start := g.pos
			end := skipGIFSubBlocks(g.data, min(g.pos+2, len(g.data)))
			ext = append(ext, g.data[start:end]...)
			g.pos = end
		case ',':
			start := g.pos
			p := g.pos + 10
			if p > len(g.data) {
				p = len(g.data)
			} else if g.data[g.pos+9]&0x80 != 0 {
				p += 3 << ((g.data[g.pos+9] & 7) + 1)
			}
			p = min(p+1, len(g.data))
			end := skipGIFSubBlocks(g.data, p)
			g.current = append(append([]byte{}, ext...), g.data[start:end]...)
			g.pos = end
			return g.canvasW, g.canvasH, true, nil
		default:
			g.pos++
		}
	}
	return 0, 0, false, nil
}

func (g *gifFrames) load() (*raster, error) {
	var buf bytes.Buffer
	buf.Write(g.head)
	buf.Write(g.current)
	buf.WriteByte(';')
	anim, err := gif.DecodeAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, err
	}
	if len(anim.Image) == 0 {
		return nil, errors.New("image file is truncated")
	}
	frame := anim.Image[0]
	canvas := image.NewPaletted(image.Rect(0, 0, g.canvasW, g.canvasH), frame.Palette)
	fill := uint8(0)
	if idx, ok := gifTransparentIndex(g.current); ok {
		fill = idx
	}
	for i := range canvas.Pix {
		canvas.Pix[i] = fill
	}
	draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Src)
	return &raster{img: canvas, mode: "P"}, nil
}

// Transparent colour index declared by the graphic control extension of a frame
func gifTransparentIndex(block []byte) (uint8, bool) {
	for i := 0; i+7 < len(block); i++ {
		if block[i] == '!' && block[i+1] == 0xF9 && block[i+2] == 4 {
			if block[i+3]&1 != 0 {
				return block[i+6], true
			}
			return 0, false
		}
		if block[i] == ',' {
			break
		}
	}
	return 0, false
}

// RIFF chunk inside a WebP container
type riffChunk struct {
	fourcc string
	data   []byte
}

// Splits a run of RIFF chunks, stopping at the first truncated one
func splitRIFFChunks(data []byte) []riffChunk {
	var out []riffChunk
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		start := pos + 8
		if size < 0 || start+size > len(data) {
			break
		}
		out = append(out, riffChunk{fourcc: string(data[pos : pos+4]), data: data[start : start+size]})
		pos = start + size + size&1
	}
	return out
}

// WebP frames, the still image or every ANMF frame of an animation
type webpFrames struct {
	data     []byte
	canvasW  int
	canvasH  int
	hasAlpha bool
	frames   []riffChunk
	index    int
	animated bool
}

// Parses the WebP container into still or animated frame data
func newWebPFrames(data []byte) (*webpFrames, error) {
	if len(data) < 12 {
		return nil, errors.New("cannot identify image file")
	}
	w := &webpFrames{data: data, index: -1}
	chunks := splitRIFFChunks(data[12:])
	if len(chunks) > 0 && chunks[0].fourcc == "VP8X" && len(chunks[0].data) >= 10 {
		flags := chunks[0].data[0]
		w.hasAlpha = flags&0x10 != 0
		w.canvasW = int(uint32(chunks[0].data[4])|uint32(chunks[0].data[5])<<8|uint32(chunks[0].data[6])<<16) + 1
		w.canvasH = int(uint32(chunks[0].data[7])|uint32(chunks[0].data[8])<<8|uint32(chunks[0].data[9])<<16) + 1
		if flags&0x02 != 0 {
			w.animated = true
			for _, c := range chunks {
				if c.fourcc == "ANMF" {
					w.frames = append(w.frames, c)
				}
			}
		}
	}
	if !w.animated {
		cfg, err := webp.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		w.canvasW, w.canvasH = cfg.Width, cfg.Height
	}
	if err := pilBombCheck(w.canvasW, w.canvasH); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *webpFrames) next() (int, int, bool, error) {
	w.index++
	if !w.animated {
		if w.index > 0 {
			return 0, 0, false, nil
		}
		return w.canvasW, w.canvasH, true, nil
	}
	if w.index >= len(w.frames) {
		return 0, 0, false, nil
	}
	return w.canvasW, w.canvasH, true, nil
}

func (w *webpFrames) load() (*raster, error) {
	if !w.animated {
		img, err := webp.Decode(bytes.NewReader(w.data))
		if err != nil {
			return nil, err
		}
		return &raster{img: img, mode: modeOfImage(img)}, nil
	}
	anmf := w.frames[w.index].data
	if len(anmf) < 16 {
		return nil, errors.New("failed to decode next frame in WebP file")
	}
	x := 2 * int(uint32(anmf[0])|uint32(anmf[1])<<8|uint32(anmf[2])<<16)
	y := 2 * int(uint32(anmf[3])|uint32(anmf[4])<<8|uint32(anmf[5])<<16)
	fw := int(uint32(anmf[6])|uint32(anmf[7])<<8|uint32(anmf[8])<<16) + 1
	fh := int(uint32(anmf[9])|uint32(anmf[10])<<8|uint32(anmf[11])<<16) + 1
	var alph, bitstream riffChunk
	for _, c := range splitRIFFChunks(anmf[16:]) {
		switch c.fourcc {
		case "ALPH":
			alph = c
		case "VP8 ", "VP8L":
			bitstream = c
		}
	}
	if bitstream.fourcc == "" {
		return nil, errors.New("failed to decode next frame in WebP file")
	}
	img, err := webp.Decode(bytes.NewReader(standaloneWebP(fw, fh, alph, bitstream)))
	if err != nil {
		return nil, err
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, w.canvasW, w.canvasH))
	dst := image.Rect(x, y, x+fw, y+fh)
	draw.Draw(canvas, dst, img, img.Bounds().Min, draw.Src)
	mode := "RGB"
	if w.hasAlpha {
		mode = "RGBA"
	}
	return &raster{img: canvas, mode: mode}, nil
}

// Builds a still WebP file around one animation frame bitstream
func standaloneWebP(fw, fh int, alph, bitstream riffChunk) []byte {
	var body bytes.Buffer
	body.WriteString("WEBP")
	writeRIFF := func(fourcc string, data []byte) {
		body.WriteString(fourcc)
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(data)))
		body.Write(n[:])
		body.Write(data)
		if len(data)%2 == 1 {
			body.WriteByte(0)
		}
	}
	if alph.fourcc != "" && bitstream.fourcc == "VP8 " {
		vp8x := make([]byte, 10)
		vp8x[0] = 0x10
		wm, hm := uint32(fw-1), uint32(fh-1)
		vp8x[4], vp8x[5], vp8x[6] = byte(wm), byte(wm>>8), byte(wm>>16)
		vp8x[7], vp8x[8], vp8x[9] = byte(hm), byte(hm>>8), byte(hm>>16)
		writeRIFF("VP8X", vp8x)
		writeRIFF("ALPH", alph.data)
	}
	writeRIFF(bitstream.fourcc, bitstream.data)
	var out bytes.Buffer
	out.WriteString("RIFF")
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(body.Len()))
	out.Write(n[:])
	out.Write(body.Bytes())
	return out.Bytes()
}

// JPEG frames, a multi picture MPO yielding every indexed picture
type jpegFrames struct {
	data    []byte
	offsets []int
	index   int
}

// Reads the MP index of a JPEG so MPO pictures iterate like Pillow MpoImageFile
func newJPEGFrames(data []byte) (*jpegFrames, error) {
	j := &jpegFrames{data: data, offsets: []int{0}, index: -1}
	if offs, ok := mpoOffsets(data); ok && len(offs) > 1 {
		j.offsets = offs
	}
	return j, nil
}

// Offsets of every picture listed in the APP2 MPF index, false when the file is a plain JPEG
func mpoOffsets(data []byte) ([]int, bool) {
	pos := 2
	var mp []byte
	mpOffset := 0
	for pos+4 <= len(data) {
		if data[pos] != 0xFF {
			return nil, false
		}
		marker := data[pos+1]
		if marker == 0xDA || marker == 0xD9 {
			break
		}
		if marker == 0xFF || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			pos += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		segStart := pos + 4
		segEnd := pos + 2 + length
		if length < 2 || segEnd > len(data) {
			return nil, false
		}
		seg := data[segStart:segEnd]
		if marker == 0xE1 && bytes.Contains(seg, []byte(` hdrgm:Version="`)) {
			return nil, false
		}
		if marker == 0xE2 && mp == nil && bytes.HasPrefix(seg, []byte("MPF\x00")) {
			mp = seg[4:]
			mpOffset = segStart + 4
		}
		pos = segEnd
	}
	if len(mp) < 8 {
		return nil, false
	}
	var order binary.ByteOrder = binary.LittleEndian
	if bytes.HasPrefix(mp, []byte("MM\x00\x2a")) {
		order = binary.BigEndian
	}
	ifd := int(order.Uint32(mp[4:8]))
	if ifd+2 > len(mp) {
		return nil, false
	}
	count := int(order.Uint16(mp[ifd : ifd+2]))
	quant := -1
	var entries []byte
	for i := 0; i < count; i++ {
		e := ifd + 2 + i*12
		if e+12 > len(mp) {
			return nil, false
		}
		tag := order.Uint16(mp[e : e+2])
		n := int(order.Uint32(mp[e+4 : e+8]))
		switch tag {
		case 0xB001:
			quant = int(order.Uint32(mp[e+8 : e+12]))
		case 0xB002:
			if n <= 4 {
				entries = mp[e+8 : e+8+n]
			} else {
				at := int(order.Uint32(mp[e+8 : e+12]))
				if at < 0 || at+n > len(mp) {
					return nil, false
				}
				entries = mp[at : at+n]
			}
		}
	}
	if quant <= 1 || len(entries) < quant*16 {
		return nil, false
	}
	offs := make([]int, quant)
	for i := 0; i < quant; i++ {
		offs[i] = int(order.Uint32(entries[i*16+8:i*16+12])) + mpOffset
	}
	offs[0] = 0
	return offs, true
}

func (j *jpegFrames) next() (int, int, bool, error) {
	j.index++
	if j.index >= len(j.offsets) {
		return 0, 0, false, nil
	}
	off := j.offsets[j.index]
	if off+4 > len(j.data) {
		return 0, 0, false, errors.New("No data found for frame")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(j.data[off:]))
	if err != nil {
		return 0, 0, false, err
	}
	return cfg.Width, cfg.Height, true, nil
}

func (j *jpegFrames) load() (*raster, error) {
	img, err := jpeg.Decode(bytes.NewReader(j.data[j.offsets[j.index]:]))
	if err != nil {
		return nil, err
	}
	return &raster{img: img, mode: modeOfImage(img)}, nil
}

// Error unless every frame decodes fully, bounding animated work by frame count and total pixels
func validateRasterImageDecodable(path string, maxFrames, maxPixels int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "Image could not be fully decoded: " + err.Error()
	}
	return validateRasterBytes(data, path, maxFrames, maxPixels)
}

// ValidateRasterImageDecodable reports why an image file cannot be fully decoded, empty when every frame decodes
func ValidateRasterImageDecodable(path string) string {
	return validateRasterImageDecodable(path, visionMaxValidatedFrameCount, visionMaxValidatedAggregatePixels)
}

// Frame by frame decode validation of image bytes
func validateRasterBytes(data []byte, path string, maxFrames, maxPixels int) string {
	fail := func(err error) string { return "Image could not be fully decoded: " + err.Error() }
	if sniffRasterFormat(data) == "" {
		return fail(fmt.Errorf("cannot identify image file '%s'", path))
	}
	if sniffRasterFormat(data) == "png" {
		info, err := pngOpen(data)
		if err != nil {
			return fail(err)
		}
		if err := pngVerify(data, info); err != nil {
			return fail(err)
		}
	} else if _, _, _, err := rasterConfig(data); err != nil {
		return fail(err)
	}
	frames, err := openFrames(data)
	if err != nil {
		return fail(err)
	}
	validated := 0
	for frameNumber := 1; ; frameNumber++ {
		w, h, ok, err := frames.next()
		if err != nil {
			return fail(err)
		}
		if !ok {
			return ""
		}
		if frameNumber > maxFrames {
			return fmt.Sprintf("Image validation rejected animation: frame %d exceeds the maximum %d validated frames.", frameNumber, maxFrames)
		}
		nextValidated := validated + w*h
		if nextValidated > maxPixels {
			return fmt.Sprintf("Image validation rejected animation: aggregate decoded pixel count would reach %d at frame %d, exceeding the maximum %d.", nextValidated, frameNumber, maxPixels)
		}
		if _, err := frames.load(); err != nil {
			return fail(err)
		}
		validated = nextValidated
	}
}

// Reports whether the longest side exceeds the limit, false for unreadable files
func imageExceedsDimension(path string, maxDimension int) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	w, h, _, err := rasterConfig(data)
	if err != nil {
		return false
	}
	return max(w, h) > maxDimension
}

// Reports whether a file exists with content
func nonemptyFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// External SVG rasterizers tried in order, replaced in tests
var rasterizeSVGToPNG = func(svgPath, outPath string) bool {
	for _, argv := range [][]string{
		{"rsvg-convert", "-o", outPath, svgPath},
		{"inkscape", svgPath, "--export-type=png", "--export-filename=" + outPath},
	} {
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdin = nil
		_, err := cmd.CombinedOutput()
		cancel()
		if err == nil && nonemptyFile(outPath) {
			return true
		}
	}
	return false
}

// Python default base64 decode that discards characters outside the alphabet
func lenientB64Decode(s string) ([]byte, error) {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			sb.WriteRune(r)
		}
	}
	return base64.StdEncoding.DecodeString(sb.String())
}

// RasterizeSVGDataURL converts an SVG data URL to a PNG data URL, false when it cannot be decoded or rasterized
func RasterizeSVGDataURL(env *Env, u string) (string, bool) {
	header, payload, _ := strings.Cut(u, ",")
	var raw []byte
	if strings.Contains(strings.ToLower(header), ";base64") {
		decoded, err := lenientB64Decode(payload)
		if err != nil {
			return "", false
		}
		raw = decoded
	} else {
		unquoted, err := url.PathUnescape(strings.ReplaceAll(payload, "+", "%2B"))
		if err != nil {
			return "", false
		}
		raw = []byte(unquoted)
	}
	outDir, err := secureCacheDir(env, "cache/vision", "temp_vision_images")
	if err != nil {
		return "", false
	}
	stem := filepath.Join(outDir, "inline_"+uuid.NewString())
	svgPath, pngPath := stem+".svg", stem+".png"
	defer os.Remove(svgPath)
	defer os.Remove(pngPath)
	if err := os.WriteFile(svgPath, raw, 0o644); err != nil {
		return "", false
	}
	if !rasterizeSVGToPNG(svgPath, pngPath) {
		return "", false
	}
	out, err := os.ReadFile(pngPath)
	if err != nil {
		return "", false
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(out), true
}

// Reports whether a provider and base URL point at the managed local server
func isManagedLocalProvider(provider, baseURL string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	if llamacppAliases[p] {
		return true
	}
	if p != "custom" || baseURL == "" {
		return false
	}
	return VisionCapabilities != nil && VisionCapabilities.IsManagedEndpoint(baseURL)
}

// Formats the active main model server decodes, narrowed for the managed llama server
func supportedMediaTypes(env *Env) map[string]bool {
	if isManagedLocalProvider(runtimeMainText(env, "provider"), runtimeMainText(env, "base_url")) {
		return managedRuntimeImageMimes
	}
	return anthropicSupportedMediaTypes
}

// NormalizeToSupportedImage re-encodes formats providers reject as PNG, returning the path, MIME, and an error message
func NormalizeToSupportedImage(env *Env, imagePath, detectedMime string) (string, string, string) {
	return normalizeToSupportedImage(env, imagePath, detectedMime)
}

// Supported input passes unchanged, SVG rasterizes, other rasters become a PNG temp file the caller removes
func normalizeToSupportedImage(env *Env, imagePath, detectedMime string) (string, string, string) {
	if supportedMediaTypes(env)[detectedMime] {
		return imagePath, detectedMime, ""
	}
	outDir, err := secureCacheDir(env, "cache/vision", "temp_vision_images")
	if err != nil {
		return "", "", err.Error()
	}
	outPath := filepath.Join(outDir, "converted_"+uuid.NewString()+".png")
	if detectedMime == "image/svg+xml" {
		if rasterizeSVGToPNG(imagePath, outPath) {
			return outPath, "image/png", ""
		}
		return "", "", "This is an SVG, which vision models cannot read directly, and no " +
			"SVG rasterizer is installed (tried rsvg-convert, inkscape). Convert the SVG to PNG first — " +
			"e.g. open it in a browser and screenshot it — then re-run vision_analyze on the PNG."
	}
	r, err := openRasterFile(imagePath)
	if err == nil {
		if r.mode != "RGB" && r.mode != "RGBA" && r.mode != "L" {
			r = convertRaster(r, "RGBA")
		}
		if err = writePNGFile(outPath, r.img); err == nil && nonemptyFile(outPath) {
			return outPath, "image/png", ""
		}
	}
	os.Remove(outPath)
	if err != nil {
		envLog(env).Warn("Failed to normalize image to PNG", "mime", detectedMime, "error", err)
		switch detectedMime {
		case "image/heic":
			return "", "", fmt.Sprintf("This is a HEIC/HEIF image (common for iPhone photos), which "+
				"vision models cannot read directly, and its HEVC payload could not be decoded (%v). "+
				"Convert the image to PNG/JPEG first and re-run vision_analyze.", err)
		case "image/avif":
			return "", "", fmt.Sprintf("This is an AVIF image, which vision models cannot read "+
				"directly, and its AV1 payload could not be decoded (%v). Convert the image to PNG/JPEG "+
				"first and re-run vision_analyze.", err)
		}
	}
	return "", "", fmt.Sprintf("Image format %s is not supported by the vision API and could not be "+
		"converted to PNG. Convert it to PNG or JPEG and try again.", pyReprStr(detectedMime))
}

// Encodes an image as PNG to a file
func writePNGFile(path string, img image.Image) error {
	b, err := encodePNG(img)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// PNG bytes at the default compression level
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// JPEG bytes at a quality level
func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Region numbers as four floats, false unless exactly four non boolean numbers
func regionNumbers(region any) ([4]float64, bool) {
	var out [4]float64
	var items []any
	switch v := region.(type) {
	case []any:
		items = v
	case []float64:
		for _, f := range v {
			items = append(items, f)
		}
	case []int:
		for _, n := range v {
			items = append(items, n)
		}
	default:
		return out, false
	}
	if len(items) != 4 {
		return out, false
	}
	for i, it := range items {
		switch n := it.(type) {
		case float64:
			out[i] = n
		case float32:
			out[i] = float64(n)
		case int:
			out[i] = float64(n)
		case int64:
			out[i] = float64(n)
		case interface{ Float64() (float64, error) }:
			f, err := n.Float64()
			if err != nil {
				return out, false
			}
			out[i] = f
		default:
			return out, false
		}
	}
	return out, true
}

// Clamps a coordinate into zero through the bound
func clampInt(v, bound int) int {
	return max(0, min(v, bound))
}

// Crops to the region in original pixels before any downscale, returning the PNG path, MIME, and an error message
func cropImageRegion(imagePath string, region any, offsetOut map[string]int) (string, string, string) {
	nums, ok := regionNumbers(region)
	if !ok {
		return "", "", "Invalid region: expected [x1, y1, x2, y2] as four numbers (pixel coordinates in the original image)."
	}
	r, err := openRasterFile(imagePath)
	if err != nil {
		return "", "", "Failed to crop region: " + err.Error()
	}
	b := r.img.Bounds()
	width, height := b.Dx(), b.Dy()
	x1, y1, x2, y2 := int(math.Trunc(nums[0])), int(math.Trunc(nums[1])), int(math.Trunc(nums[2])), int(math.Trunc(nums[3]))
	cx1, cy1, cx2, cy2 := clampInt(x1, width), clampInt(y1, height), clampInt(x2, width), clampInt(y2, height)
	if cx2 <= cx1 || cy2 <= cy1 {
		return "", "", fmt.Sprintf("Invalid region [%d, %d, %d, %d]: crops to zero area after clamping to the image bounds. "+
			"The image is %dx%d px — pick x1<x2 and y1<y2 inside [0, 0, %d, %d].", x1, y1, x2, y2, width, height, width, height)
	}
	cropped := cropRaster(r, image.Rect(cx1, cy1, cx2, cy2))
	if offsetOut != nil {
		offsetOut["x"], offsetOut["y"], offsetOut["width"], offsetOut["height"] = cx1, cy1, cx2-cx1, cy2-cy1
	}
	stem := strings.TrimSuffix(filepath.Base(imagePath), pySuffix(imagePath))
	outPath := filepath.Join(filepath.Dir(imagePath), stem+"_region_"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8]+".png")
	switch cropped.mode {
	case "RGB", "RGBA", "L", "LA", "P":
	default:
		cropped = convertRaster(cropped, "RGB")
	}
	if err := writePNGFile(outPath, cropped.img); err != nil {
		os.Remove(outPath)
		return "", "", "Failed to crop region: " + err.Error()
	}
	return outPath, "image/png", ""
}

// Copies a sub rectangle of a raster into a new zero origin image of the same kind
func cropRaster(r *raster, rect image.Rectangle) *raster {
	b := r.img.Bounds()
	src := rect.Add(b.Min)
	dstRect := image.Rect(0, 0, rect.Dx(), rect.Dy())
	switch m := r.img.(type) {
	case *image.Paletted:
		out := image.NewPaletted(dstRect, m.Palette)
		draw.Draw(out, dstRect, m, src.Min, draw.Src)
		return &raster{img: out, mode: r.mode}
	case *image.Gray:
		out := image.NewGray(dstRect)
		draw.Draw(out, dstRect, m, src.Min, draw.Src)
		return &raster{img: out, mode: r.mode}
	case *image.Gray16:
		out := image.NewGray16(dstRect)
		draw.Draw(out, dstRect, m, src.Min, draw.Src)
		return &raster{img: out, mode: r.mode}
	case *image.CMYK:
		out := image.NewCMYK(dstRect)
		for y := 0; y < rect.Dy(); y++ {
			copy(out.Pix[y*out.Stride:y*out.Stride+rect.Dx()*4], m.Pix[m.PixOffset(src.Min.X, src.Min.Y+y):])
		}
		return &raster{img: out, mode: r.mode}
	}
	if r.mode == "RGB" {
		out := image.NewRGBA(dstRect)
		draw.Draw(out, dstRect, toOpaqueRGB(r.img), src.Min.Sub(b.Min), draw.Src)
		return &raster{img: out, mode: r.mode}
	}
	full := toNRGBA(r.img)
	out := image.NewNRGBA(dstRect)
	draw.Draw(out, dstRect, full, src.Min.Sub(b.Min), draw.Src)
	return &raster{img: out, mode: r.mode}
}

// Converts a raster to RGB, RGBA, or L the way Pillow convert does
func convertRaster(r *raster, mode string) *raster {
	switch mode {
	case "RGB":
		return &raster{img: toOpaqueRGB(r.img), mode: "RGB"}
	case "L":
		g := image.NewGray(image.Rect(0, 0, r.img.Bounds().Dx(), r.img.Bounds().Dy()))
		draw.Draw(g, g.Bounds(), toOpaqueRGB(r.img), image.Point{}, draw.Src)
		return &raster{img: g, mode: "L"}
	}
	return &raster{img: toNRGBA(r.img), mode: "RGBA"}
}

// Straight alpha copy of any image with a zero origin
func toNRGBA(img image.Image) *image.NRGBA {
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	switch m := img.(type) {
	case *image.NRGBA:
		for y := 0; y < b.Dy(); y++ {
			copy(out.Pix[y*out.Stride:y*out.Stride+b.Dx()*4], m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):])
		}
		return out
	case *image.Paletted:
		lut := make([]color.NRGBA, len(m.Palette))
		for i, c := range m.Palette {
			lut[i] = color.NRGBAModel.Convert(c).(color.NRGBA)
		}
		for y := 0; y < b.Dy(); y++ {
			row := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := 0; x < b.Dx(); x++ {
				var c color.NRGBA
				if int(row[x]) < len(lut) {
					c = lut[row[x]]
				}
				i := y*out.Stride + x*4
				out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = c.R, c.G, c.B, c.A
			}
		}
		return out
	case *image.Gray16:
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				v := uint8(m.Gray16At(b.Min.X+x, b.Min.Y+y).Y >> 8)
				i := y*out.Stride + x*4
				out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = v, v, v, 255
			}
		}
		return out
	case *image.YCbCr, *image.Gray, *image.RGBA:
		rgba := image.NewRGBA(out.Rect)
		draw.Draw(rgba, rgba.Rect, img, b.Min, draw.Src)
		for i := 0; i < len(rgba.Pix); i += 4 {
			a := rgba.Pix[i+3]
			out.Pix[i+3] = a
			switch a {
			case 0:
			case 255:
				out.Pix[i], out.Pix[i+1], out.Pix[i+2] = rgba.Pix[i], rgba.Pix[i+1], rgba.Pix[i+2]
			default:
				for c := 0; c < 3; c++ {
					out.Pix[i+c] = uint8(min(255, int(rgba.Pix[i+c])*255/int(a)))
				}
			}
		}
		return out
	}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			i := y*out.Stride + x*4
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = c.R, c.G, c.B, c.A
		}
	}
	return out
}

// Opaque RGB copy that drops alpha keeping the straight colour values
func toOpaqueRGB(img image.Image) *image.RGBA {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	switch img.(type) {
	case *image.YCbCr, *image.Gray, *image.CMYK:
		draw.Draw(out, out.Rect, img, b.Min, draw.Src)
		return out
	}
	n := toNRGBA(img)
	copy(out.Pix, n.Pix)
	for i := 3; i < len(out.Pix); i += 4 {
		out.Pix[i] = 255
	}
	return out
}

// Fixed point precision of the 8 bit resampling coefficients
const resamplePrecisionBits = 32 - 8 - 2

// Windowed sinc with three lobes
func lanczosFilter(x float64) float64 {
	if -3.0 <= x && x < 3.0 {
		return sincFilter(x) * sincFilter(x/3)
	}
	return 0
}

// Normalised sinc
func sincFilter(x float64) float64 {
	if x == 0 {
		return 1
	}
	x *= math.Pi
	return math.Sin(x) / x
}

// Per output pixel source windows and normalised weights for one axis
type resampleCoeffs struct {
	ksize  int
	bounds []int
	kk     []float64
}

// Computes Lanczos windows with the support widened by the downscale factor so the result is antialiased
func precomputeCoeffs(inSize, outSize int) resampleCoeffs {
	scale := float64(inSize) / float64(outSize)
	filterscale := max(scale, 1.0)
	support := 3.0 * filterscale
	ksize := int(math.Ceil(support))*2 + 1
	c := resampleCoeffs{ksize: ksize, bounds: make([]int, outSize*2), kk: make([]float64, outSize*ksize)}
	for xx := 0; xx < outSize; xx++ {
		center := (float64(xx) + 0.5) * scale
		ss := 1.0 / filterscale
		xmin := max(int(center-support+0.5), 0)
		xmax := min(int(center+support+0.5), inSize) - xmin
		k := c.kk[xx*ksize : (xx+1)*ksize]
		ww := 0.0
		for x := 0; x < xmax; x++ {
			w := lanczosFilter((float64(x+xmin) - center + 0.5) * ss)
			k[x] = w
			ww += w
		}
		for x := 0; x < xmax; x++ {
			if ww != 0 {
				k[x] /= ww
			}
		}
		c.bounds[xx*2], c.bounds[xx*2+1] = xmin, xmax
	}
	return c
}

// Fixed point copy of the weights for 8 bit channels
func (c resampleCoeffs) fixed() []int32 {
	out := make([]int32, len(c.kk))
	for i, v := range c.kk {
		if v < 0 {
			out[i] = int32(-0.5 + v*(1<<resamplePrecisionBits))
		} else {
			out[i] = int32(0.5 + v*(1<<resamplePrecisionBits))
		}
	}
	return out
}

// Clamps a fixed point accumulator back to an 8 bit channel
func clip8(in int64) uint8 {
	if in >= 1<<resamplePrecisionBits<<8 {
		return 255
	}
	if in <= 0 {
		return 0
	}
	return uint8(in >> resamplePrecisionBits)
}

// Lanczos resamples interleaved 8 bit pixels, horizontal pass first then vertical as Pillow does
func resample8(src []uint8, sw, sh, bpp, nch, dw, dh int) []uint8 {
	horiz := precomputeCoeffs(sw, dw)
	vert := precomputeCoeffs(sh, dh)
	cur, curW := src, sw
	if dw != sw {
		yFirst := vert.bounds[0]
		yLast := vert.bounds[dh*2-2] + vert.bounds[dh*2-1]
		for i := 0; i < dh; i++ {
			vert.bounds[i*2] -= yFirst
		}
		kk := horiz.fixed()
		rows := yLast - yFirst
		tmp := make([]uint8, dw*rows*bpp)
		for yy := 0; yy < rows; yy++ {
			srow := cur[(yy+yFirst)*curW*bpp:]
			drow := tmp[yy*dw*bpp:]
			for xx := 0; xx < dw; xx++ {
				xmin, xmax := horiz.bounds[xx*2], horiz.bounds[xx*2+1]
				k := kk[xx*horiz.ksize:]
				for ch := 0; ch < nch; ch++ {
					ss := int64(1 << (resamplePrecisionBits - 1))
					for x := 0; x < xmax; x++ {
						ss += int64(srow[(x+xmin)*bpp+ch]) * int64(k[x])
					}
					drow[xx*bpp+ch] = clip8(ss)
				}
			}
		}
		cur, curW = tmp, dw
	}
	if dh == sh {
		if dw == sw {
			return append([]uint8(nil), src...)
		}
		return cur
	}
	kk := vert.fixed()
	out := make([]uint8, curW*dh*bpp)
	for yy := 0; yy < dh; yy++ {
		ymin, ymax := vert.bounds[yy*2], vert.bounds[yy*2+1]
		k := kk[yy*vert.ksize:]
		drow := out[yy*curW*bpp:]
		for xx := 0; xx < curW; xx++ {
			for ch := 0; ch < nch; ch++ {
				ss := int64(1 << (resamplePrecisionBits - 1))
				for y := 0; y < ymax; y++ {
					ss += int64(cur[((y+ymin)*curW+xx)*bpp+ch]) * int64(k[y])
				}
				drow[xx*bpp+ch] = clip8(ss)
			}
		}
	}
	return out
}

// Lanczos resamples 16 bit grey samples with floating point weights
func resample16(src *image.Gray16, dw, dh int) *image.Gray16 {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	vals := make([]float64, sw*sh)
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			vals[y*sw+x] = float64(src.Gray16At(b.Min.X+x, b.Min.Y+y).Y)
		}
	}
	round := func(f float64) float64 {
		if f >= 0 {
			return math.Floor(f + 0.5)
		}
		return math.Ceil(f - 0.5)
	}
	clamp16 := func(f float64) float64 { return max(0, min(65535, round(f))) }
	if dw != sw {
		h := precomputeCoeffs(sw, dw)
		tmp := make([]float64, dw*sh)
		for y := 0; y < sh; y++ {
			for xx := 0; xx < dw; xx++ {
				xmin, xmax := h.bounds[xx*2], h.bounds[xx*2+1]
				ss := 0.0
				for x := 0; x < xmax; x++ {
					ss += vals[y*sw+x+xmin] * h.kk[xx*h.ksize+x]
				}
				tmp[y*dw+xx] = clamp16(ss)
			}
		}
		vals, sw = tmp, dw
	}
	if dh != sh {
		v := precomputeCoeffs(sh, dh)
		tmp := make([]float64, sw*dh)
		for yy := 0; yy < dh; yy++ {
			ymin, ymax := v.bounds[yy*2], v.bounds[yy*2+1]
			for x := 0; x < sw; x++ {
				ss := 0.0
				for y := 0; y < ymax; y++ {
					ss += vals[(y+ymin)*sw+x] * v.kk[yy*v.ksize+y]
				}
				tmp[yy*sw+x] = clamp16(ss)
			}
		}
		vals = tmp
	}
	out := image.NewGray16(image.Rect(0, 0, dw, dh))
	for i, f := range vals {
		out.SetGray16(i%dw, i/dw, color.Gray16{Y: uint16(f)})
	}
	return out
}

// Nearest neighbour source index for an output index along one axis
func nearestIndex(i, inSize, outSize int) int {
	return min(int(math.Floor((float64(i)+0.5)*float64(inSize)/float64(outSize))), inSize-1)
}

// Resizes a raster the way Pillow resize does, nearest for palette and bilevel modes and premultiplied Lanczos otherwise
func resizeRaster(r *raster, dw, dh int) *raster {
	b := r.img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	switch r.mode {
	case "P", "1":
		if p, ok := r.img.(*image.Paletted); ok {
			out := image.NewPaletted(image.Rect(0, 0, dw, dh), p.Palette)
			for y := 0; y < dh; y++ {
				sy := nearestIndex(y, sh, dh)
				for x := 0; x < dw; x++ {
					out.Pix[y*out.Stride+x] = p.Pix[p.PixOffset(b.Min.X+nearestIndex(x, sw, dw), b.Min.Y+sy)]
				}
			}
			return &raster{img: out, mode: r.mode}
		}
		src := image.NewGray(image.Rect(0, 0, sw, sh))
		draw.Draw(src, src.Rect, r.img, b.Min, draw.Src)
		out := image.NewGray(image.Rect(0, 0, dw, dh))
		for y := 0; y < dh; y++ {
			sy := nearestIndex(y, sh, dh)
			for x := 0; x < dw; x++ {
				out.Pix[y*out.Stride+x] = src.Pix[sy*src.Stride+nearestIndex(x, sw, dw)]
			}
		}
		return &raster{img: out, mode: r.mode}
	case "L":
		src := image.NewGray(image.Rect(0, 0, sw, sh))
		draw.Draw(src, src.Rect, r.img, b.Min, draw.Src)
		out := image.NewGray(image.Rect(0, 0, dw, dh))
		out.Pix = resample8(src.Pix, sw, sh, 1, 1, dw, dh)
		return &raster{img: out, mode: "L"}
	case "I;16":
		g, ok := r.img.(*image.Gray16)
		if !ok {
			g = image.NewGray16(image.Rect(0, 0, sw, sh))
			draw.Draw(g, g.Rect, r.img, b.Min, draw.Src)
		}
		return &raster{img: resample16(g, dw, dh), mode: "I;16"}
	case "CMYK":
		src := image.NewCMYK(image.Rect(0, 0, sw, sh))
		draw.Draw(src, src.Rect, r.img, b.Min, draw.Src)
		out := image.NewCMYK(image.Rect(0, 0, dw, dh))
		out.Pix = resample8(src.Pix, sw, sh, 4, 4, dw, dh)
		return &raster{img: out, mode: "CMYK"}
	case "RGB":
		src := toOpaqueRGB(r.img)
		out := image.NewRGBA(image.Rect(0, 0, dw, dh))
		out.Pix = resample8(src.Pix, sw, sh, 4, 3, dw, dh)
		for i := 3; i < len(out.Pix); i += 4 {
			out.Pix[i] = 255
		}
		return &raster{img: out, mode: "RGB"}
	}
	n := toNRGBA(r.img)
	pre := make([]uint8, len(n.Pix))
	for i := 0; i < len(n.Pix); i += 4 {
		a := uint32(n.Pix[i+3])
		for c := 0; c < 3; c++ {
			t := uint32(n.Pix[i+c])*a + 128
			pre[i+c] = uint8(((t >> 8) + t) >> 8)
		}
		pre[i+3] = n.Pix[i+3]
	}
	res := resample8(pre, sw, sh, 4, 4, dw, dh)
	out := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for i := 0; i < len(res); i += 4 {
		a := int(res[i+3])
		if a == 255 || a == 0 {
			out.Pix[i], out.Pix[i+1], out.Pix[i+2] = res[i], res[i+1], res[i+2]
		} else {
			for c := 0; c < 3; c++ {
				out.Pix[i+c] = uint8(max(0, min(255, 255*int(res[i+c])/a)))
			}
		}
		out.Pix[i+3] = res[i+3]
	}
	mode := r.mode
	if mode != "LA" {
		mode = "RGBA"
	}
	return &raster{img: out, mode: mode}
}
