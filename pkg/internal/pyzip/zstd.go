package pyzip

import "github.com/nickheyer/icebath/third_party/libzstd"

// compression.zstd.ZstdDecompressor over the libzstd 1.5.7 streaming decoder
type zstdDecompressor struct {
	ds         *libzstd.DCtx
	unconsumed []byte
	eof        bool
	needsInput bool
}

func newZstdDecompressor() *zstdDecompressor {
	return &zstdDecompressor{ds: libzstd.NewDStream(), needsInput: true}
}

func (d *zstdDecompressor) isEOF() bool      { return d.eof }
func (d *zstdDecompressor) wantsInput() bool { return d.needsInput }
func (d *zstdDecompressor) release()         {}

// Decompresses data after any unconsumed input, resetting the session when decoding fails
func (d *zstdDecompressor) decompress(data []byte, maxLength int) ([]byte, error) {
	if d.eof {
		return nil, &EOFError{"Already at the end of a Zstandard frame."}
	}
	in := data
	if len(d.unconsumed) > 0 {
		in = append(d.unconsumed, data...)
	}
	ret, pos, err := d.decompressLockHeld(in, maxLength)
	if err != 0 {
		d.unconsumed, d.needsInput, d.eof = nil, true, false
		d.ds.ResetSession()
		return nil, &ZstdError{"Unable to decompress Zstandard data: " + err.Error()}
	}
	if pos == len(in) {
		d.needsInput = !(len(ret) == maxLength || d.eof)
		d.unconsumed = nil
	} else {
		d.needsInput = false
		d.unconsumed = append([]byte(nil), in[pos:]...)
	}
	return ret, nil
}

// Calls ZSTD_decompressStream until a frame ends, max_length is reached or the input is used up
func (d *zstdDecompressor) decompressLockHeld(in []byte, maxLength int) ([]byte, int, libzstd.ErrorCode) {
	buf := newOutputBuffer(maxLength)
	pos := 0
	for {
		n, newPos, hint, err := d.ds.DecompressStream(buf.avail(), in, pos)
		if err != 0 {
			return nil, 0, err
		}
		buf.filled += n
		pos = newPos
		if hint == 0 {
			d.eof = true
			break
		}
		if buf.filled == len(buf.data) {
			if len(buf.data) == maxLength {
				break
			}
			buf.grow()
		} else if pos == len(in) {
			break
		}
	}
	return buf.data[:buf.filled], pos, 0
}
