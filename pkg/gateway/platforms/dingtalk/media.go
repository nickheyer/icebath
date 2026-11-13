package dingtalk

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/media"
)

const (
	maxUploadBytes    = 20 << 20
	maxVoiceBytes     = 2 << 20
	maxVoiceMs        = 60000.0
	voiceSegmentMs    = 59000.0
	voiceSegmentBytes = maxVoiceBytes - 32<<10
	transcodeTimeout  = 10 * time.Minute
)

// File types the media upload API accepts as plain files
var uploadFileTypes = map[string]bool{"doc": true, "docx": true, "xls": true, "xlsx": true, "ppt": true, "pptx": true,
	"zip": true, "pdf": true, "rar": true}

// Scratch directory of one outbound media send
type scratch struct{ dir string }

// Fresh scratch directory under the system temp dir
func newScratch() (*scratch, error) {
	dir, err := os.MkdirTemp("", "icebath-dingtalk-*")
	if err != nil {
		return nil, err
	}
	return &scratch{dir: dir}, nil
}

func (s *scratch) path(name string) string { return filepath.Join(s.dir, name) }

func (s *scratch) cleanup() { _ = os.RemoveAll(s.dir) }

// First n bytes of a file
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:read], nil
}

// Upload ready image re-encoded when DingTalk cannot take its format or size, false when it does not decode
func prepareImage(sc *scratch, path string) (string, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false, err
	}
	head, err := readHead(path, 512)
	if err != nil {
		return "", false, err
	}
	kind := http.DetectContentType(head)
	native := kind == "image/jpeg" || kind == "image/png" || kind == "image/gif" || kind == "image/bmp"
	if native && info.Size() <= maxUploadBytes {
		return path, true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return "", false, nil
	}
	if !native {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return "", false, err
		}
		if buf.Len() <= maxUploadBytes {
			out := sc.path("image.png")
			return out, true, os.WriteFile(out, buf.Bytes(), 0o600)
		}
	}
	for scale := 1.0; scale >= 1.0/32; scale /= 2 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, scaleImage(img, scale), &jpeg.Options{Quality: 85}); err != nil {
			return "", false, err
		}
		if buf.Len() <= maxUploadBytes {
			out := sc.path("image.jpg")
			return out, true, os.WriteFile(out, buf.Bytes(), 0o600)
		}
	}
	return "", false, nil
}

// Image scaled by the factor with a smooth filter
func scaleImage(img image.Image, scale float64) image.Image {
	if scale >= 1 {
		return img
	}
	b := img.Bounds()
	w, h := max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

// One voice message clip with its length in milliseconds
type voiceClip struct {
	path       string
	durationMs int64
}

// Audio cut into independently playable units with what each segment file needs around them
type audioUnits struct {
	ext    string
	data   []byte
	prefix []byte
	wavFmt []byte
	ranges [][2]int
	unitMs []float64
}

// Total length of the audio in milliseconds
func (u *audioUnits) totalMs() float64 {
	total := 0.0
	for _, ms := range u.unitMs {
		total += ms
	}
	return total
}

// Bytes of one segment file holding the units from i to j
func (u *audioUnits) segment(i, j int) []byte {
	var body bytes.Buffer
	for _, r := range u.ranges[i:j] {
		body.Write(u.data[r[0]:r[1]])
	}
	if u.wavFmt != nil {
		return append(wavHeader(u.wavFmt, body.Len()), body.Bytes()...)
	}
	return append(append([]byte{}, u.prefix...), body.Bytes()...)
}

// Header size every segment carries
func (u *audioUnits) overhead() int {
	if u.wavFmt != nil {
		return 20 + len(u.wavFmt) + len(u.wavFmt)%2
	}
	return len(u.prefix)
}

// Segment boundaries keeping each segment within the voice length and size limits
func (u *audioUnits) split() [][2]int {
	var out [][2]int
	start, ms, size := 0, 0.0, u.overhead()
	for i, r := range u.ranges {
		n := r[1] - r[0]
		if i > start && (ms+u.unitMs[i] > voiceSegmentMs || size+n > voiceSegmentBytes) {
			out = append(out, [2]int{start, i})
			start, ms, size = i, 0, u.overhead()
		}
		ms += u.unitMs[i]
		size += n
	}
	if start < len(u.ranges) {
		out = append(out, [2]int{start, len(u.ranges)})
	}
	return out
}

// Codec of audio bytes DingTalk voice messages accept, empty for anything else
func voiceFormat(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("#!AMR")):
		return "amr"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "wav"
	case bytes.HasPrefix(data, []byte("ID3")), len(data) >= 2 && data[0] == 0xFF && data[1]&0xE6 == 0xE2:
		return "mp3"
	}
	return ""
}

// Units of mp3, wav or amr audio bytes
func parseAudioUnits(format string, data []byte) (*audioUnits, error) {
	switch format {
	case "mp3":
		return mp3Units(data)
	case "wav":
		return wavUnits(data)
	case "amr":
		return amrUnits(data)
	}
	return nil, fmt.Errorf("unsupported voice format %q", format)
}

// Voice clips within the voice length and size limits, false when the audio cannot become a voice message
func prepareVoice(ctx context.Context, sc *scratch, path string) ([]voiceClip, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	ffmpeg := media.FindFFmpegBinary()
	var units *audioUnits
	if format := voiceFormat(data); format != "" {
		if units, err = parseAudioUnits(format, data); err != nil {
			units = nil
		}
	}
	switch {
	case units == nil:
		if ffmpeg == "" {
			return nil, false, nil
		}
		if units, err = transcodeVoice(ctx, ffmpeg, sc, path); err != nil {
			slog.Warn(fmt.Sprintf("[Dingtalk] Voice transcode of %s failed: %v", path, err))
			return nil, false, nil
		}
	case units.totalMs() <= maxVoiceMs && len(data) <= maxVoiceBytes:
		return []voiceClip{{path: path, durationMs: int64(math.Round(units.totalMs()))}}, true, nil
	case units.ext != ".mp3" && ffmpeg != "" && len(data) > maxVoiceBytes:
		if compact, err := transcodeVoice(ctx, ffmpeg, sc, path); err == nil {
			units = compact
		}
	}
	var clips []voiceClip
	for i, seg := range units.split() {
		out := sc.path(fmt.Sprintf("voice-%03d%s", i, units.ext))
		if err := os.WriteFile(out, units.segment(seg[0], seg[1]), 0o600); err != nil {
			return nil, false, err
		}
		ms := 0.0
		for _, v := range units.unitMs[seg[0]:seg[1]] {
			ms += v
		}
		clips = append(clips, voiceClip{path: out, durationMs: int64(math.Round(ms))})
	}
	return clips, len(clips) > 0, nil
}

// Speech tuned mono mp3 of any audio file through ffmpeg
func transcodeVoice(ctx context.Context, ffmpeg string, sc *scratch, path string) (*audioUnits, error) {
	out := sc.path("voice.mp3")
	if err := runFFmpeg(ctx, ffmpeg, "-i", path, "-vn", "-ac", "1", "-ar", "16000", "-b:a", "32k", "-f", "mp3", out); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	return mp3Units(data)
}

// Runs ffmpeg quietly with overwrite and a timeout, its stderr in the error
func runFFmpeg(ctx context.Context, ffmpeg string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, transcodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, ffmpeg, append([]string{"-v", "error", "-y"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %v %s", err, clip(strings.TrimSpace(stderr.String()), 300))
	}
	return nil
}

// Bitrates in kbps of MPEG audio by version group and layer
var mp3Bitrates = map[[2]int][]int{
	{1, 1}: {32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
	{1, 2}: {32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
	{1, 3}: {32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	{2, 1}: {32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
	{2, 2}: {8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	{2, 3}: {8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
}

// Length in bytes, samples and sample rate of the MPEG audio frame with this header
func mp3FrameInfo(h uint32) (length, samples, rate int, ok bool) {
	if h>>21 != 0x7FF {
		return 0, 0, 0, false
	}
	version, layerBits := (h>>19)&3, (h>>17)&3
	brIdx, srIdx, pad := int((h>>12)&0xF), int((h>>10)&3), int((h>>9)&1)
	if version == 1 || layerBits == 0 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
		return 0, 0, 0, false
	}
	layer := 4 - int(layerBits)
	rate = []int{44100, 48000, 32000}[srIdx]
	group := 1
	switch version {
	case 2:
		rate, group = rate/2, 2
	case 0:
		rate, group = rate/4, 2
	}
	kbps := mp3Bitrates[[2]int{group, layer}][brIdx-1]
	switch {
	case layer == 1:
		samples = 384
		length = (12*kbps*1000/rate + pad) * 4
	case layer == 2 || group == 1:
		samples = 1152
		length = 144*kbps*1000/rate + pad
	default:
		samples = 576
		length = 72*kbps*1000/rate + pad
	}
	return length, samples, rate, length > 4
}

// Frames of an mp3 stream, tags and a leading VBR info frame left out
func mp3Units(data []byte) (*audioUnits, error) {
	off, end := 0, len(data)
	if end >= 10 && string(data[:3]) == "ID3" {
		off = 10 + (int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F))
		if data[5]&0x10 != 0 {
			off += 10
		}
	}
	if end-off >= 128 && string(data[end-128:end-125]) == "TAG" {
		end -= 128
	}
	u := &audioUnits{ext: ".mp3", data: data}
	first := true
	for off+4 <= end {
		length, samples, rate, ok := mp3FrameInfo(binary.BigEndian.Uint32(data[off:]))
		if !ok || off+length > end {
			off++
			continue
		}
		frame := data[off : off+length]
		if first {
			first = false
			probe := frame[:min(len(frame), 64)]
			if bytes.Contains(probe, []byte("Xing")) || bytes.Contains(probe, []byte("Info")) || bytes.Contains(probe, []byte("VBRI")) {
				off += length
				continue
			}
		}
		u.ranges = append(u.ranges, [2]int{off, off + length})
		u.unitMs = append(u.unitMs, float64(samples)*1000/float64(rate))
		off += length
	}
	if len(u.ranges) == 0 {
		return nil, errors.New("no mp3 frames found")
	}
	return u, nil
}

// Hundred millisecond sample blocks of a wav file and its format chunk
func wavUnits(data []byte) (*audioUnits, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a wav file")
	}
	var fmtChunk []byte
	dataStart, dataEnd := -1, -1
	for off := 12; off+8 <= len(data); {
		id, size := string(data[off:off+4]), int(binary.LittleEndian.Uint32(data[off+4:off+8]))
		body := off + 8
		if size < 0 || body+size > len(data) {
			size = len(data) - body
		}
		switch id {
		case "fmt ":
			fmtChunk = data[body : body+size]
		case "data":
			dataStart, dataEnd = body, body+size
		}
		off = body + size + size%2
	}
	if len(fmtChunk) < 16 || dataStart < 0 {
		return nil, errors.New("wav file has no format or data chunk")
	}
	byteRate := int(binary.LittleEndian.Uint32(fmtChunk[8:12]))
	blockAlign := int(binary.LittleEndian.Uint16(fmtChunk[12:14]))
	if byteRate <= 0 || blockAlign <= 0 {
		return nil, errors.New("wav file has an invalid format chunk")
	}
	step := max(blockAlign, byteRate/10/blockAlign*blockAlign)
	u := &audioUnits{ext: ".wav", data: data, wavFmt: fmtChunk}
	for off := dataStart; off < dataEnd; off += step {
		stop := min(off+step, dataEnd)
		u.ranges = append(u.ranges, [2]int{off, stop})
		u.unitMs = append(u.unitMs, float64(stop-off)*1000/float64(byteRate))
	}
	if len(u.ranges) == 0 {
		return nil, errors.New("wav file has no samples")
	}
	return u, nil
}

// RIFF header with the format chunk and a data chunk header of the given length
func wavHeader(fmtChunk []byte, dataLen int) []byte {
	pad := len(fmtChunk) % 2
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(4+8+len(fmtChunk)+pad+8+dataLen))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(fmtChunk)))
	b.Write(fmtChunk)
	if pad == 1 {
		b.WriteByte(0)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	return b.Bytes()
}

// Frame sizes in bytes by frame type of narrowband and wideband AMR
var (
	amrNBSizes = [16]int{13, 14, 16, 18, 20, 21, 27, 32, 6, 1, 1, 1, 1, 1, 1, 1}
	amrWBSizes = [16]int{18, 24, 33, 37, 41, 47, 51, 59, 61, 6, 1, 1, 1, 1, 1, 1}
)

// Twenty millisecond frames of an AMR file and its magic header
func amrUnits(data []byte) (*audioUnits, error) {
	magic, sizes := []byte("#!AMR\n"), amrNBSizes
	if bytes.HasPrefix(data, []byte("#!AMR-WB\n")) {
		magic, sizes = []byte("#!AMR-WB\n"), amrWBSizes
	} else if !bytes.HasPrefix(data, magic) {
		return nil, errors.New("not an amr file")
	}
	u := &audioUnits{ext: ".amr", data: data, prefix: magic}
	for off := len(magic); off < len(data); {
		n := sizes[(data[off]>>3)&0x0F]
		if off+n > len(data) {
			break
		}
		u.ranges = append(u.ranges, [2]int{off, off + n})
		u.unitMs = append(u.unitMs, 20)
		off += n
	}
	if len(u.ranges) == 0 {
		return nil, errors.New("amr file has no frames")
	}
	return u, nil
}

// Video ready for a video message with its cover frame and whole second length
type videoFile struct {
	path    string
	cover   string
	seconds int64
}

// MP4 within the upload limit with a cover image, false when it cannot become a video message
func prepareVideo(ctx context.Context, sc *scratch, path string) (*videoFile, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	head, err := readHead(path, 16)
	if err != nil {
		return nil, false, err
	}
	isMP4 := len(head) >= 12 && string(head[4:8]) == "ftyp" && !strings.HasPrefix(strings.ToLower(string(head[8:12])), "m4a") &&
		string(head[8:12]) != "qt  "
	ffmpeg := media.FindFFmpegBinary()
	videoPath := path
	if !isMP4 || info.Size() > maxUploadBytes {
		if ffmpeg == "" {
			return nil, false, nil
		}
		seconds, _ := mp4DurationSeconds(path)
		videoPath = sc.path("video.mp4")
		args := []string{"-i", path, "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "64k",
			"-movflags", "+faststart"}
		if seconds > 0 {
			kbps := min(4000, max(100, int(float64(maxUploadBytes)*0.9*8/1000/seconds)-64))
			args = append(args, "-b:v", strconv.Itoa(kbps)+"k", "-maxrate", strconv.Itoa(kbps)+"k", "-bufsize", strconv.Itoa(2*kbps)+"k")
		} else {
			args = append(args, "-crf", "28")
		}
		if err := runFFmpeg(ctx, ffmpeg, append(args, videoPath)...); err != nil {
			slog.Warn(fmt.Sprintf("[Dingtalk] Video transcode of %s failed: %v", path, err))
			return nil, false, nil
		}
		if st, err := os.Stat(videoPath); err != nil || st.Size() > maxUploadBytes {
			return nil, false, nil
		}
	}
	seconds, err := mp4DurationSeconds(videoPath)
	if err != nil {
		return nil, false, nil
	}
	cover := sc.path("cover.jpg")
	if ffmpeg == "" || runFFmpeg(ctx, ffmpeg, "-ss", strconv.FormatFloat(min(1, seconds/4), 'f', 2, 64), "-i", videoPath,
		"-frames:v", "1", "-vf", "scale=480:-2", "-q:v", "5", cover) != nil {
		if err := writePlaceholderCover(cover); err != nil {
			return nil, false, err
		}
	}
	return &videoFile{path: videoPath, cover: cover, seconds: max(1, int64(math.Round(seconds)))}, true, nil
}

// Length in seconds of an ISO media file from its movie header
func mp4DurationSeconds(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	moov, err := findBox(f, 0, st.Size(), "moov")
	if err != nil {
		return 0, err
	}
	mvhd, err := findBox(f, moov[0], moov[1], "mvhd")
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, mvhd[0])
	buf = buf[:n]
	var timescale, duration uint64
	switch {
	case len(buf) >= 20 && buf[0] == 0:
		timescale, duration = uint64(binary.BigEndian.Uint32(buf[12:16])), uint64(binary.BigEndian.Uint32(buf[16:20]))
	case len(buf) >= 32 && buf[0] == 1:
		timescale, duration = uint64(binary.BigEndian.Uint32(buf[20:24])), binary.BigEndian.Uint64(buf[24:32])
	default:
		return 0, errors.New("unreadable movie header")
	}
	if timescale == 0 {
		return 0, errors.New("movie header has no timescale")
	}
	return float64(duration) / float64(timescale), nil
}

// Payload range of the first box of the type between start and end
func findBox(r io.ReaderAt, start, end int64, typ string) ([2]int64, error) {
	hdr := make([]byte, 16)
	for off := start; off+8 <= end; {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return [2]int64{}, err
		}
		size, headerLen := int64(binary.BigEndian.Uint32(hdr[:4])), int64(8)
		switch size {
		case 1:
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return [2]int64{}, err
			}
			size, headerLen = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		case 0:
			size = end - off
		}
		if size < headerLen {
			return [2]int64{}, errors.New("corrupt media box")
		}
		if string(hdr[4:8]) == typ {
			return [2]int64{off + headerLen, min(off+size, end)}, nil
		}
		off += size
	}
	return [2]int64{}, fmt.Errorf("media box %s not found", typ)
}

// Dark cover with a play mark for videos no frame could be grabbed from
func writePlaceholderCover(path string) error {
	img := image.NewRGBA(image.Rect(0, 0, 480, 270))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{0x22, 0x26, 0x2e, 0xff}}, image.Point{}, draw.Src)
	cx, cy, h := 240, 135, 80
	x0, reach := cx-h/3, float64(h)*0.87
	for dy := -h / 2; dy <= h/2; dy++ {
		width := int(reach * (1 - math.Abs(float64(dy))/float64(h/2)))
		for x := x0; x <= x0+width; x++ {
			img.Set(x, cy+dy, color.White)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// Deflate compressed zip holding the one file under its name
func zipWrap(sc *scratch, path, name string) (string, string, error) {
	zipName := strings.TrimSuffix(name, filepath.Ext(name)) + ".zip"
	out := sc.path(zipName)
	f, err := os.Create(out)
	if err != nil {
		return "", "", err
	}
	zw := zip.NewWriter(f)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
	if err == nil {
		var src *os.File
		if src, err = os.Open(path); err == nil {
			_, err = io.Copy(w, src)
			src.Close()
		}
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", err
	}
	return out, zipName, nil
}
