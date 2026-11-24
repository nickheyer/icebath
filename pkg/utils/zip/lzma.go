package ziputil

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"

	"github.com/ulikunitz/xz/lzma"
)

// LZMA decoder of a member; the member's directory record, found through where its data starts and how long it is (archive/zip hands decoders an *io.SectionReader over exactly that), gives the size, whether an end marker closes the stream, and a bound for the dictionary; without it the stream must end with its marker
func (r *Reader) unlzma(src io.Reader) io.ReadCloser {
	var f *zip.File
	if sr, ok := src.(*io.SectionReader); ok {
		_, off, n := sr.Outer()
		f = r.placement().at[dataSpan{off, n}]
	}
	var prefix [9]byte
	if _, err := io.ReadFull(src, prefix[:]); err != nil {
		return failed(err)
	}
	if binary.LittleEndian.Uint16(prefix[2:]) != 5 {
		return failed(errLZMAHeader)
	}
	var header [lzma.HeaderLen]byte
	copy(header[:5], prefix[4:])
	size := int64(-1)
	if f != nil {
		dict := int64(binary.LittleEndian.Uint32(header[1:5]))
		binary.LittleEndian.PutUint32(header[1:5], uint32(min(dict, max(int64(f.UncompressedSize64), lzma.MinDictCap))))
		if f.Flags&flagEndMarker == 0 {
			size = int64(f.UncompressedSize64)
		}
	}
	binary.LittleEndian.PutUint64(header[5:], uint64(size))
	d, err := lzma.NewReader(io.MultiReader(bytes.NewReader(header[:]), src))
	if err != nil {
		return failed(err)
	}
	return &codecReader{r: d}
}
