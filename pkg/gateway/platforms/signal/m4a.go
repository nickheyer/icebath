package signal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// AAC sampling rates by ADTS sampling frequency index
var aacSampleRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// Samples one AAC raw data block decodes to
const aacFrameSamples = 1024

// Access units of an ADTS stream with the configuration every frame shares
type adtsStream struct {
	objectType int
	sfIndex    int
	chanCfg    int
	samples    [][]byte
	durations  []uint32
}

// MSB first bit reader over a byte slice
type bitReader struct {
	data []byte
	pos  int
}

func (r *bitReader) read(n int) (uint32, error) {
	if r.pos+n > len(r.data)*8 {
		return 0, errors.New("program config element is truncated")
	}
	var v uint32
	for range n {
		v = v<<1 | uint32(r.data[r.pos>>3]>>(7-r.pos&7)&1)
		r.pos++
	}
	return v, nil
}

func (r *bitReader) align() { r.pos = (r.pos + 7) &^ 7 }

// MSB first bit writer growing its buffer as needed
type bitWriter struct {
	data []byte
	pos  int
}

func (w *bitWriter) write(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.pos>>3 >= len(w.data) {
			w.data = append(w.data, 0)
		}
		if v>>i&1 == 1 {
			w.data[w.pos>>3] |= 1 << (7 - w.pos&7)
		}
		w.pos++
	}
}

func (w *bitWriter) align() {
	for w.pos&7 != 0 {
		w.write(0, 1)
	}
}

// Copies n bits from the reader to the writer and returns them
func copyBits(r *bitReader, w *bitWriter, n int) (uint32, error) {
	v, err := r.read(n)
	if err != nil {
		return 0, err
	}
	w.write(v, n)
	return v, nil
}

// Copies the program config element opening a raw data block into the writer, returning channels and bytes consumed
func copyPCE(block []byte, w *bitWriter) (channels, consumed int, err error) {
	r := &bitReader{data: block}
	id, err := r.read(3)
	if err != nil {
		return 0, 0, err
	}
	if id != 5 {
		return 0, 0, errors.New("channel configuration 0 without a leading program config element")
	}
	bits := func(n int) uint32 {
		if err != nil {
			return 0
		}
		var v uint32
		v, err = copyBits(r, w, n)
		return v
	}
	bits(10)
	front, side, back := int(bits(4)), int(bits(4)), int(bits(4))
	lfe, assoc, cc := int(bits(2)), int(bits(3)), int(bits(4))
	for _, width := range []int{4, 4, 3} {
		if bits(1) == 1 {
			bits(width)
		}
	}
	if err != nil {
		return 0, 0, err
	}
	for range front + side + back {
		if bits(1) == 1 {
			channels += 2
		} else {
			channels++
		}
		bits(4)
	}
	channels += lfe
	for range lfe + assoc {
		bits(4)
	}
	for range cc {
		bits(5)
	}
	r.align()
	w.align()
	comment := int(bits(8))
	for range comment {
		bits(8)
	}
	if err != nil {
		return 0, 0, err
	}
	return channels, r.pos / 8, nil
}

// Splits an ADTS stream into raw data blocks, resyncing past garbage and dropping a truncated tail frame
func parseADTS(data []byte) (*adtsStream, error) {
	s := &adtsStream{}
	for pos := 0; pos+7 <= len(data); {
		h := data[pos:]
		if h[0] != 0xFF || h[1]&0xF6 != 0xF0 {
			pos++
			continue
		}
		protectionAbsent := h[1]&0x01 == 1
		objectType := int(h[2]>>6) + 1
		sfIndex := int(h[2]>>2) & 0x0F
		chanCfg := int(h[2]&0x01)<<2 | int(h[3]>>6)
		frameLen := int(h[3]&0x03)<<11 | int(h[4])<<3 | int(h[5]>>5)
		blocks := int(h[6]&0x03) + 1
		headerLen := 7
		if !protectionAbsent {
			headerLen += 2 * blocks
		}
		if sfIndex >= len(aacSampleRates) || frameLen <= headerLen {
			pos++
			continue
		}
		if pos+frameLen > len(data) {
			break
		}
		if len(s.samples) == 0 {
			s.objectType, s.sfIndex, s.chanCfg = objectType, sfIndex, chanCfg
		} else if objectType != s.objectType || sfIndex != s.sfIndex || chanCfg != s.chanCfg {
			return nil, errors.New("ADTS stream changes its audio configuration mid-stream")
		}
		frame := data[pos : pos+frameLen]
		switch {
		case blocks == 1:
			s.samples, s.durations = append(s.samples, frame[headerLen:]), append(s.durations, aacFrameSamples)
		case protectionAbsent:
			s.samples, s.durations = append(s.samples, frame[7:]), append(s.durations, uint32(aacFrameSamples*blocks))
		default:
			body := frame[headerLen:]
			starts := []int{0}
			for i := range blocks - 1 {
				starts = append(starts, int(binary.BigEndian.Uint16(frame[7+2*i:])))
			}
			starts = append(starts, len(body))
			for i := range blocks {
				start, end := starts[i], starts[i+1]-2
				if start < 0 || end <= start || starts[i+1] > len(body) {
					return nil, fmt.Errorf("ADTS frame at byte %d has invalid raw data block positions", pos)
				}
				s.samples, s.durations = append(s.samples, body[start:end]), append(s.durations, aacFrameSamples)
			}
		}
		pos += frameLen
	}
	if len(s.samples) == 0 {
		return nil, errors.New("no ADTS frames found")
	}
	return s, nil
}

// Audio specific config of the stream and its channel count, the PCE moved into the config for configuration 0
func (s *adtsStream) audioSpecificConfig() ([]byte, int, error) {
	w := &bitWriter{}
	w.write(uint32(s.objectType), 5)
	w.write(uint32(s.sfIndex), 4)
	w.write(uint32(s.chanCfg), 4)
	w.write(0, 3)
	switch {
	case s.chanCfg == 7:
		return w.data, 8, nil
	case s.chanCfg > 0:
		return w.data, s.chanCfg, nil
	}
	channels, consumed, err := copyPCE(s.samples[0], w)
	if err != nil {
		return nil, 0, err
	}
	if channels == 0 {
		return nil, 0, errors.New("program config element declares no channels")
	}
	w.align()
	s.samples[0] = s.samples[0][consumed:]
	return w.data, channels, nil
}

// Lossless remux of raw ADTS AAC, the form Android voice notes use, into a faststart M4A file
func remuxADTSToM4A(data []byte) ([]byte, error) {
	s, err := parseADTS(data)
	if err != nil {
		return nil, err
	}
	asc, channels, err := s.audioSpecificConfig()
	if err != nil {
		return nil, err
	}
	return s.mux(asc, channels), nil
}

func be16(v int) []byte    { return binary.BigEndian.AppendUint16(nil, uint16(v)) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// ISO base media box of kind holding the parts
func mp4Box(kind string, parts ...[]byte) []byte {
	payload := bytes.Join(parts, nil)
	return append(append(be32(uint32(8+len(payload))), kind...), payload...)
}

// Full box with version and flags
func mp4FullBox(kind string, version byte, flags uint32, parts ...[]byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return mp4Box(kind, append([][]byte{head}, parts...)...)
}

// MPEG-4 descriptor with a four byte expandable length
func mp4Descriptor(tag byte, parts ...[]byte) []byte {
	payload := bytes.Join(parts, nil)
	n := len(payload)
	head := []byte{tag, 0x80 | byte(n>>21&0x7F), 0x80 | byte(n>>14&0x7F), 0x80 | byte(n>>7&0x7F), byte(n & 0x7F)}
	return append(head, payload...)
}

// Unity transformation matrix of movie and track headers
var unityMatrix = bytes.Join([][]byte{be32(0x00010000), be32(0), be32(0), be32(0), be32(0x00010000), be32(0), be32(0), be32(0),
	be32(0x40000000)}, nil)

// Writes ftyp, moov and mdat with every access unit in one chunk after the movie header
func (s *adtsStream) mux(asc []byte, channels int) []byte {
	rate := aacSampleRates[s.sfIndex]
	var totalSamples uint64
	var payloadBytes, largest int
	for i, sample := range s.samples {
		totalSamples += uint64(s.durations[i])
		payloadBytes += len(sample)
		largest = max(largest, len(sample))
	}
	avgBitrate := uint32(uint64(payloadBytes) * 8 * uint64(rate) / max(totalSamples, 1))
	maxBitrate := avgBitrate
	var windowBytes int
	var windowSamples uint64
	for i, sample := range s.samples {
		windowBytes += len(sample)
		if windowSamples += uint64(s.durations[i]); windowSamples >= uint64(rate) {
			maxBitrate = max(maxBitrate, uint32(uint64(windowBytes)*8*uint64(rate)/windowSamples))
			windowBytes, windowSamples = 0, 0
		}
	}
	movieDuration := uint32((totalSamples*1000 + uint64(rate)/2) / uint64(rate))
	var stts [][]byte
	runs := 0
	for i := 0; i < len(s.durations); {
		j := i
		for j < len(s.durations) && s.durations[j] == s.durations[i] {
			j++
		}
		stts = append(stts, be32(uint32(j-i)), be32(s.durations[i]))
		runs++
		i = j
	}
	sizes := make([][]byte, len(s.samples))
	for i, sample := range s.samples {
		sizes[i] = be32(uint32(len(sample)))
	}
	sampleRateField := uint32(0)
	if rate <= 0xFFFF {
		sampleRateField = uint32(rate) << 16
	}
	esds := mp4FullBox("esds", 0, 0, mp4Descriptor(0x03, be16(1), []byte{0},
		mp4Descriptor(0x04, []byte{0x40, 0x15}, be32(uint32(largest))[1:], be32(maxBitrate), be32(avgBitrate), mp4Descriptor(0x05, asc)),
		mp4Descriptor(0x06, []byte{0x02})))
	mp4a := mp4Box("mp4a", make([]byte, 6), be16(1), make([]byte, 8), be16(channels), be16(16), be16(0), be16(0),
		be32(sampleRateField), esds)
	moov := func(chunkOffset uint32) []byte {
		stbl := mp4Box("stbl", mp4FullBox("stsd", 0, 0, be32(1), mp4a),
			mp4FullBox("stts", 0, 0, append([][]byte{be32(uint32(runs))}, stts...)...),
			mp4FullBox("stsc", 0, 0, be32(1), be32(1), be32(uint32(len(s.samples))), be32(1)),
			mp4FullBox("stsz", 0, 0, append([][]byte{be32(0), be32(uint32(len(s.samples)))}, sizes...)...),
			mp4FullBox("stco", 0, 0, be32(1), be32(chunkOffset)))
		minf := mp4Box("minf", mp4FullBox("smhd", 0, 0, be16(0), be16(0)),
			mp4Box("dinf", mp4FullBox("dref", 0, 0, be32(1), mp4FullBox("url ", 0, 1))), stbl)
		mdia := mp4Box("mdia", mp4FullBox("mdhd", 0, 0, be32(0), be32(0), be32(uint32(rate)), be32(uint32(totalSamples)), be16(0x55C4), be16(0)),
			mp4FullBox("hdlr", 0, 0, be32(0), []byte("soun"), make([]byte, 12), []byte("SoundHandler\x00")), minf)
		tkhd := mp4FullBox("tkhd", 0, 3, be32(0), be32(0), be32(1), be32(0), be32(movieDuration), make([]byte, 8), be16(0), be16(1),
			be16(0x0100), be16(0), unityMatrix, be32(0), be32(0))
		mvhd := mp4FullBox("mvhd", 0, 0, be32(0), be32(0), be32(1000), be32(movieDuration), be32(0x00010000), be16(0x0100),
			make([]byte, 10), unityMatrix, make([]byte, 24), be32(2))
		return mp4Box("moov", mvhd, mp4Box("trak", tkhd, mdia))
	}
	ftyp := mp4Box("ftyp", []byte("M4A "), be32(0x200), []byte("M4A isomiso2"))
	offset := uint32(len(ftyp) + len(moov(0)) + 8)
	out := bytes.Join([][]byte{ftyp, moov(offset), be32(uint32(8 + payloadBytes)), []byte("mdat")}, nil)
	for _, sample := range s.samples {
		out = append(out, sample...)
	}
	return out
}
