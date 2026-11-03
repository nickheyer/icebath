package gfx

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
)

// PNG writer for three byte pixels, rows filtered against the row above and packed at the fastest zlib level
type pngEncoder struct {
	zw   *zlib.Writer
	idat bytes.Buffer
	out  bytes.Buffer
	line []byte
}

// PNG of the w by h pixels, valid until the next encode
func (e *pngEncoder) encode(raw []byte, w, h int) []byte {
	e.idat.Reset()
	if e.zw == nil {
		e.zw, _ = zlib.NewWriterLevel(&e.idat, zlib.BestSpeed)
	} else {
		e.zw.Reset(&e.idat)
	}
	stride := w * 3
	if cap(e.line) < stride+1 {
		e.line = make([]byte, stride+1)
	}
	line := e.line[:stride+1]
	line[0] = 2
	for y := range h {
		cur := raw[y*stride : (y+1)*stride]
		if y == 0 {
			copy(line[1:], cur)
		} else {
			prev := raw[(y-1)*stride : y*stride]
			for i, v := range cur {
				line[1+i] = v - prev[i]
			}
		}
		e.zw.Write(line)
	}
	e.zw.Close()
	e.out.Reset()
	e.out.WriteString("\x89PNG\r\n\x1a\n")
	var hdr [13]byte
	binary.BigEndian.PutUint32(hdr[0:], uint32(w))
	binary.BigEndian.PutUint32(hdr[4:], uint32(h))
	hdr[8], hdr[9] = 8, 2
	e.chunk("IHDR", hdr[:])
	e.chunk("IDAT", e.idat.Bytes())
	e.chunk("IEND", nil)
	return e.out.Bytes()
}

// Appends a PNG chunk with its length and checksum
func (e *pngEncoder) chunk(kind string, data []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	e.out.Write(n[:])
	e.out.WriteString(kind)
	e.out.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(kind))
	crc.Write(data)
	binary.BigEndian.PutUint32(n[:], crc.Sum32())
	e.out.Write(n[:])
}
