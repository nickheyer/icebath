package pyzip

import (
	"fmt"
	"math"

	"github.com/nickheyer/icebath/third_party/libbz2"
)

// BUFFER_BLOCK_SIZE steps the CPython blocks output buffer grows by
var bufferBlockSize = [...]int{32 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20, 8 << 20, 16 << 20, 16 << 20,
	32 << 20, 32 << 20, 32 << 20, 32 << 20, 64 << 20, 64 << 20, 128 << 20, 128 << 20, 256 << 20}

// Output of one decompress call grown block by block up to max_length like _BlocksOutputBuffer
type outputBuffer struct {
	data      []byte
	filled    int
	blocks    int
	maxLength int
}

// First block of 32 KiB, or max_length when smaller and not negative
func newOutputBuffer(maxLength int) *outputBuffer {
	size := bufferBlockSize[0]
	if maxLength >= 0 && maxLength < size {
		size = maxLength
	}
	return &outputBuffer{data: make([]byte, size), blocks: 1, maxLength: maxLength}
}

// Free space left in the current block
func (b *outputBuffer) avail() []byte { return b.data[b.filled:] }

// Appends the next block, cut so the total never passes max_length
func (b *outputBuffer) grow() {
	size := bufferBlockSize[min(b.blocks, len(bufferBlockSize)-1)]
	if b.maxLength >= 0 {
		size = min(size, b.maxLength-len(b.data))
	}
	b.data = append(b.data, make([]byte, size)...)
	b.blocks++
}

// Decompressor a zipfile member reads through, eof and needs_input as CPython exposes them
type pushDecompressor interface {
	decompress(data []byte, maxLength int) ([]byte, error)
	isEOF() bool
	wantsInput() bool
	release()
}

// bz2.BZ2Decompressor over the libbzip2 translation
type bz2Decompressor struct {
	bzs        *libbz2.Decompressor
	input      []byte
	eof        bool
	needsInput bool
	bzerror    bool
}

func newBZ2Decompressor() *bz2Decompressor {
	return &bz2Decompressor{bzs: libbz2.NewDecompressor(), needsInput: true}
}

func (d *bz2Decompressor) isEOF() bool      { return d.eof }
func (d *bz2Decompressor) wantsInput() bool { return d.needsInput }
func (d *bz2Decompressor) release()         { d.bzs.End() }

// OSError, RuntimeError or the unrecognized error catch_bz2_error raises for a libbzip2 code
func catchBZ2Error(ret int) error {
	switch ret {
	case libbz2.OK, libbz2.StreamEnd:
		return nil
	case libbz2.DataError, libbz2.DataErrorMagic:
		return &OSError{"Invalid data stream"}
	case libbz2.SequenceError:
		return &RuntimeError{"Internal error - Invalid sequence of commands sent to libbzip2"}
	}
	return &OSError{fmt.Sprintf("Unrecognized error from libbzip2: %d", ret)}
}

// Decompresses data after any unconsumed input, at most max_length bytes when it is not negative
func (d *bz2Decompressor) decompress(data []byte, maxLength int) ([]byte, error) {
	if d.eof {
		return nil, &EOFError{"End of stream already reached"}
	}
	if d.bzerror {
		return nil, &ValueError{"Decompressor is unusable after a previous error"}
	}
	in := data
	if d.input != nil {
		in = append(d.input, data...)
	}
	result, rest, err := d.decompressBuf(in, maxLength)
	if err != nil {
		d.input = nil
		return nil, err
	}
	switch {
	case d.eof:
		d.needsInput = false
	case len(rest) == 0:
		d.input = nil
		d.needsInput = true
	default:
		d.needsInput = false
		d.input = append([]byte(nil), rest...)
	}
	return result, nil
}

// Runs BZ2_bzDecompress until stream end, consumed input or max_length output, returning output and unconsumed input
func (d *bz2Decompressor) decompressBuf(in []byte, maxLength int) ([]byte, []byte, error) {
	buf := newOutputBuffer(maxLength)
	for {
		nin, nout, ret := d.bzs.Decompress(in[:min(len(in), math.MaxUint32)], buf.avail())
		in = in[nin:]
		buf.filled += nout
		if err := catchBZ2Error(ret); err != nil {
			d.bzerror = true
			d.needsInput = false
			d.bzs.End()
			return nil, nil, err
		}
		if ret == libbz2.StreamEnd {
			d.eof = true
			d.bzs.End()
			break
		}
		if len(in) == 0 {
			break
		}
		if buf.filled == len(buf.data) {
			if buf.filled == maxLength {
				break
			}
			buf.grow()
		}
	}
	return buf.data[:buf.filled], in, nil
}
