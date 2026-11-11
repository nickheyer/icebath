package gatewayrun

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var errAudioProbe = errors.New("audio probe failed")

// Formats whole seconds as minutes and seconds, with hours when present, rounding half to even and clamping at zero
func formatDuration(seconds float64) (string, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return "", errAudioProbe
	}
	total := max(0, int64(math.RoundToEven(seconds)))
	hours, rem := total/3600, total%3600
	minutes, secs := rem/60, rem%60
	if hours != 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, secs), nil
	}
	return fmt.Sprintf("%d:%02d", minutes, secs), nil
}

// Formatted duration of an audio file from its WAV header, Opus granule or ffprobe, false on failure
func probeAudioDuration(ctx context.Context, path string) (string, bool) {
	_, ext := pypath.Splitext(path)
	ext = strings.ToLower(ext)
	if ext == ".wav" {
		if seconds, err := wavDuration(path); err == nil {
			if out, err := formatDuration(seconds); err == nil {
				return out, true
			}
		}
	}
	if ext == ".ogg" || ext == ".opus" || ext == ".oga" {
		if seconds, err := oggOpusDuration(path); err == nil {
			if out, err := formatDuration(seconds); err == nil {
				return out, true
			}
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", false
	}
	text, err := pystr.DecodeUTF8(stdout.Bytes())
	if err != nil {
		return "", false
	}
	seconds, err := pyint.ParseFloat(pystr.Strip(text))
	if err != nil {
		return "", false
	}
	out, err := formatDuration(seconds)
	return out, err == nil
}

// RIFF chunk reader of the chunk module, bounded by its size and word aligned
type riffChunk struct {
	parent    *riffChunk
	file      io.ReadSeeker
	name      []byte
	chunksize int64
	sizeRead  int64
	offset    int64
}

// Reads up to n bytes from the enclosing chunk, or from the file for the outermost chunk
func (c *riffChunk) readFrom(n int64) []byte {
	if c.parent != nil {
		return c.parent.read(n)
	}
	buf := make([]byte, n)
	got, _ := io.ReadFull(c.file, buf)
	return buf[:got]
}

// Opens a chunk header, an EOF error when the name or size is short
func openRIFFChunk(parent *riffChunk, file io.ReadSeeker) (*riffChunk, error) {
	c := &riffChunk{parent: parent, file: file}
	c.name = c.readFrom(4)
	if len(c.name) < 4 {
		return nil, io.EOF
	}
	size := c.readFrom(4)
	if len(size) < 4 {
		return nil, io.EOF
	}
	c.chunksize = int64(binary.LittleEndian.Uint32(size))
	if parent != nil {
		c.offset = parent.sizeRead
		return c, nil
	}
	pos, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	c.offset = pos
	return c, nil
}

// Reads at most size bytes of the chunk body, consuming the pad byte at an odd end
func (c *riffChunk) read(size int64) []byte {
	if c.sizeRead >= c.chunksize {
		return nil
	}
	if size < 0 || size > c.chunksize-c.sizeRead {
		size = c.chunksize - c.sizeRead
	}
	data := c.readFrom(size)
	c.sizeRead += int64(len(data))
	if c.sizeRead == c.chunksize && c.chunksize&1 == 1 {
		c.sizeRead += int64(len(c.readFrom(1)))
	}
	return data
}

// Seeks the outermost chunk to pos of its body, an error past its bounds like the chunk module
func (c *riffChunk) seek(pos int64) error {
	if pos < 0 || pos > c.chunksize {
		return errAudioProbe
	}
	if _, err := c.file.Seek(c.offset+pos, io.SeekStart); err != nil {
		return err
	}
	c.sizeRead = pos
	return nil
}

// Skips the rest of a chunk nested in the outermost one, with its pad byte
func (c *riffChunk) skip() error {
	n := c.chunksize - c.sizeRead
	if c.chunksize&1 == 1 {
		n++
	}
	if err := c.parent.seek(c.parent.sizeRead + n); err != nil {
		return err
	}
	c.sizeRead += n
	return nil
}

var ksdataformatSubtypePCM = []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}

// Seconds of PCM audio in a WAV file, failing where the wave module refuses the header
func wavDuration(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	riff, err := openRIFFChunk(nil, f)
	if err != nil {
		return 0, err
	}
	if string(riff.name) != "RIFF" || string(riff.read(4)) != "WAVE" {
		return 0, errAudioProbe
	}
	var framerate, framesize, nframes int64
	fmtRead, dataFound := false, false
	for {
		chunk, err := openRIFFChunk(riff, nil)
		if err != nil {
			break
		}
		switch string(chunk.name) {
		case "fmt ":
			head := chunk.read(14)
			if len(head) < 14 {
				return 0, errAudioProbe
			}
			formatTag := binary.LittleEndian.Uint16(head[0:2])
			nchannels := int64(binary.LittleEndian.Uint16(head[2:4]))
			framerate = int64(binary.LittleEndian.Uint32(head[4:8]))
			if formatTag != 0x0001 && formatTag != 0xFFFE {
				return 0, errAudioProbe
			}
			width := chunk.read(2)
			if len(width) < 2 {
				return 0, errAudioProbe
			}
			sampwidth := int64(binary.LittleEndian.Uint16(width))
			if formatTag == 0xFFFE {
				if ext := chunk.read(8); len(ext) < 8 {
					return 0, errAudioProbe
				}
				if sub := chunk.read(16); len(sub) < 16 || !bytes.Equal(sub, ksdataformatSubtypePCM) {
					return 0, errAudioProbe
				}
			}
			sampwidth = (sampwidth + 7) / 8
			if sampwidth == 0 || nchannels == 0 {
				return 0, errAudioProbe
			}
			framesize = nchannels * sampwidth
			fmtRead = true
		case "data":
			if !fmtRead {
				return 0, errAudioProbe
			}
			nframes = chunk.chunksize / framesize
			dataFound = true
		}
		if dataFound {
			break
		}
		if err := chunk.skip(); err != nil {
			return 0, err
		}
	}
	if !fmtRead || !dataFound {
		return 0, errAudioProbe
	}
	if framerate == 0 {
		framerate = 1
	}
	return float64(nframes) / float64(framerate), nil
}

// One Ogg page as mutagen reads it
type oggPage struct {
	flags    byte
	position int64
	serial   uint32
	sequence int64
	packets  [][]byte
	complete bool
}

// Reads the next Ogg page, io.EOF at a clean or zero padded end and errAudioProbe when malformed
func readOggPage(r io.Reader) (*oggPage, error) {
	header := make([]byte, 27)
	n, _ := io.ReadFull(r, header)
	header = header[:n]
	if len(header) < 27 && bytes.Count(header, []byte{0}) == len(header) {
		return nil, io.EOF
	}
	if len(header) < 27 || string(header[0:4]) != "OggS" || header[4] != 0 {
		return nil, errAudioProbe
	}
	p := &oggPage{
		flags:    header[5],
		position: int64(binary.LittleEndian.Uint64(header[6:14])),
		serial:   binary.LittleEndian.Uint32(header[14:18]),
		sequence: int64(binary.LittleEndian.Uint32(header[18:22])),
		complete: true,
	}
	segments := int(header[26])
	lacingBytes := make([]byte, segments)
	if got, _ := io.ReadFull(r, lacingBytes); got != segments {
		return nil, errAudioProbe
	}
	var lacings []int
	total := 0
	for _, c := range lacingBytes {
		total += int(c)
		if c < 255 {
			lacings = append(lacings, total)
			total = 0
		}
	}
	if total != 0 {
		lacings = append(lacings, total)
		p.complete = false
	}
	for _, l := range lacings {
		buf := make([]byte, l)
		got, _ := io.ReadFull(r, buf)
		if got != l {
			return nil, errAudioProbe
		}
		p.packets = append(p.packets, buf)
	}
	return p, nil
}

// Joins the packets of consecutive pages of one stream, an error on a serial or sequence gap
func oggToPackets(pages []*oggPage) ([][]byte, error) {
	serial, sequence := pages[0].serial, pages[0].sequence
	var packets [][]byte
	if pages[0].flags&1 == 1 {
		packets = append(packets, []byte{})
	}
	for _, page := range pages {
		if page.serial != serial || page.sequence != sequence {
			return nil, errAudioProbe
		}
		sequence++
		if len(page.packets) == 0 {
			continue
		}
		if page.flags&1 == 1 {
			packets[len(packets)-1] = append(packets[len(packets)-1], page.packets[0]...)
		} else {
			packets = append(packets, append([]byte{}, page.packets[0]...))
		}
		for _, p := range page.packets[1:] {
			packets = append(packets, append([]byte{}, p...))
		}
	}
	return packets, nil
}

// Last page of the stream with a finishing packet, the fast tail read then the full scan
func oggFindLastFinishing(f *os.File, serial uint32) (*oggPage, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(0, info.Size()-256*256)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	index := bytes.LastIndex(tail, []byte("OggS"))
	if index < 0 {
		return nil, errAudioProbe
	}
	var best *oggPage
	if page, err := readOggPage(bytes.NewReader(tail[index:])); err == nil && page.serial == serial && page.position != -1 {
		if page.flags&4 == 4 {
			return page, nil
		}
		best = page
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	for {
		page, err := readOggPage(f)
		if err != nil {
			return best, nil
		}
		if page.serial == serial {
			if page.position != -1 {
				best = page
			}
			if page.flags&4 == 4 {
				return best, nil
			}
		}
	}
}

// Seconds of an Ogg Opus stream from its last granule position less the pre skip, failing like mutagen
func oggOpusDuration(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	page, err := readOggPage(f)
	for err == nil && (len(page.packets) == 0 || !bytes.HasPrefix(page.packets[0], []byte("OpusHead"))) {
		page, err = readOggPage(f)
	}
	if err != nil {
		return 0, err
	}
	serial := page.serial
	if page.flags&2 == 0 || len(page.packets[0]) < 19 {
		return 0, errAudioProbe
	}
	head := page.packets[0][8:19]
	preSkip := int64(binary.LittleEndian.Uint16(head[2:4]))
	if head[0]>>4 != 0 {
		return 0, errAudioProbe
	}
	if err := skipOpusTags(f, serial); err != nil {
		return 0, err
	}
	last, err := oggFindLastFinishing(f, serial)
	if err != nil {
		return 0, err
	}
	if last == nil {
		return 0, errAudioProbe
	}
	return float64(last.position-preSkip) / 48000, nil
}

// Reads the OpusTags pages and checks the Vorbis comment framing lengths are present
func skipOpusTags(f *os.File, serial uint32) error {
	page, err := readOggPage(f)
	for err == nil && (page.serial != serial || len(page.packets) == 0 || !bytes.HasPrefix(page.packets[0], []byte("OpusTags"))) {
		page, err = readOggPage(f)
	}
	if err != nil {
		return err
	}
	pages := []*oggPage{page}
	for !(pages[len(pages)-1].complete || len(pages[len(pages)-1].packets) > 1) {
		next, err := readOggPage(f)
		if err != nil {
			return err
		}
		if next.serial == pages[0].serial {
			pages = append(pages, next)
		}
	}
	packets, err := oggToPackets(pages)
	if err != nil {
		return err
	}
	if len(packets) == 0 {
		return errAudioProbe
	}
	data := bytes.NewReader(packets[0][min(8, len(packets[0])):])
	readLen := func() (int64, error) {
		raw := make([]byte, 4)
		if got, _ := io.ReadFull(data, raw); got < 4 {
			return 0, errAudioProbe
		}
		return int64(binary.LittleEndian.Uint32(raw)), nil
	}
	vendor, err := readLen()
	if err != nil {
		return err
	}
	_, _ = data.Seek(min(vendor, int64(data.Len())), io.SeekCurrent)
	count, err := readLen()
	if err != nil {
		return err
	}
	for i := int64(0); i < count; i++ {
		length, err := readLen()
		if err != nil {
			return err
		}
		_, _ = data.Seek(min(length, int64(data.Len())), io.SeekCurrent)
	}
	return nil
}
