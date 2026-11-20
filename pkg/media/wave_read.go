package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Channel count, sample width in bytes, frame rate and frame bytes of a WAV file
type WaveFile struct {
	Channels, SampWidth, FrameRate int
	Frames                         []byte
}

// Header fields and frames of a PCM WAV file read the way the wave module opens one
func ReadWave(path string) (*WaveFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, io.EOF
	}
	if string(data[:4]) != "RIFF" {
		return nil, errors.New("file does not start with RIFF id")
	}
	riffEnd := min(len(data), 8+int(binary.LittleEndian.Uint32(data[4:8])))
	if riffEnd < 12 || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a WAVE file")
	}
	pos, framesize, rate, channels, sampwidth, fmtRead := 12, 0, 0, 0, 0, false
	for pos+8 <= riffEnd {
		name, size := string(data[pos:pos+4]), int(binary.LittleEndian.Uint32(data[pos+4:pos+8]))
		body := data[pos+8 : min(riffEnd, pos+8+size)]
		switch name {
		case "fmt ":
			if len(body) < 16 {
				return nil, io.EOF
			}
			var tag uint16
			tag, channels = binary.LittleEndian.Uint16(body[0:2]), int(binary.LittleEndian.Uint16(body[2:4]))
			if tag != 1 && tag != 0xFFFE {
				return nil, fmt.Errorf("unknown format: %d", tag)
			}
			sampwidth = (int(binary.LittleEndian.Uint16(body[14:16])) + 7) / 8
			if tag == 0xFFFE {
				if len(body) < 40 {
					return nil, io.EOF
				}
				pcm := []byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
				if string(body[24:40]) != string(pcm) {
					return nil, errors.New("unknown extended format")
				}
			}
			if sampwidth == 0 {
				return nil, errors.New("bad sample width")
			}
			if channels == 0 {
				return nil, errors.New("bad # of channels")
			}
			rate, framesize, fmtRead = int(binary.LittleEndian.Uint32(body[4:8])), channels*sampwidth, true
		case "data":
			if !fmtRead {
				return nil, errors.New("data chunk before fmt chunk")
			}
			nframes := size / framesize
			return &WaveFile{Channels: channels, SampWidth: sampwidth, FrameRate: rate, Frames: body[:min(len(body), nframes*framesize)]}, nil
		}
		pos += 8 + size + size%2
	}
	return nil, errors.New("fmt chunk and/or data chunk missing")
}
