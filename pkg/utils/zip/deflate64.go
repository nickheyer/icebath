package ziputil

import (
	"bufio"
	"errors"
	"io"
	"math/bits"
	"sync"
)

const (
	// Size of the Deflate64 window, which is also the farthest a match reaches back
	window64 = 1 << 16

	// Longest Huffman code, and the code bits the first table level resolves; longer codes go through a second level
	maxCodeLen  = 15
	primaryBits = 9
	primarySize = 1 << primaryBits

	// Table entry: symbol or second-level offset from bit 8 up, code length (or second-level index bits) in the low 4 bits, bit 4 marking a link to the second level; zero is no code
	linkFlag = 1 << 4
)

var (
	errDeflate64Block  = errors.New("deflate64: invalid block type")
	errDeflate64Stored = errors.New("deflate64: stored block length does not match its complement")
	errDeflate64Header = errors.New("deflate64: invalid dynamic block header")
	errDeflate64Table  = errors.New("deflate64: over-subscribed or incomplete Huffman code")
	errDeflate64Code   = errors.New("deflate64: invalid code")
	errDeflate64Dist   = errors.New("deflate64: distance reaches before the start of the data")
)

var (
	// Length codes 257 to 285: code 285 takes 16 extra bits from base 3 where Deflate has a fixed 258
	lengthBase64  = [29]uint16{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 3}
	lengthExtra64 = [29]uint8{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 16}

	// Distance codes 0 to 31: codes 30 and 31, unused in Deflate, reach 32,769 to 65,536 with 14 extra bits
	distBase64 = [32]uint32{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073,
		4097, 6145, 8193, 12289, 16385, 24577, 32769, 49153}
	distExtra64 = [32]uint8{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13, 14, 14}

	// Order the code length code lengths of a dynamic block come in
	codeLengthOrder = [19]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

	// Literal/length and distance codes of fixed Huffman blocks, built on first use and only read after
	fixedCodes64 = sync.OnceValue(func() *[2]huffman {
		var t [2]huffman
		var lit [288]uint8
		for i := range lit {
			switch {
			case i < 144:
				lit[i] = 8
			case i < 256:
				lit[i] = 9
			case i < 280:
				lit[i] = 7
			default:
				lit[i] = 8
			}
		}
		var dist [32]uint8
		for i := range dist {
			dist[i] = 5
		}
		t[0].build(lit[:], true)
		t[1].build(dist[:], true)
		return &t
	})

	inflaters64 sync.Pool
)

// Canonical Huffman code as a two-level lookup table indexed by the next stream bits, low bit first
type huffman struct {
	primary        [primarySize]uint32
	sub            []uint32
	minLen, maxLen uint
}

// Builds the table of the code with these lengths per symbol, refusing an over-subscribed code and an incomplete one, except, when partial, an empty code or one of a single one-bit code
func (h *huffman) build(lengths []uint8, partial bool) error {
	var count [maxCodeLen + 1]int
	for _, l := range lengths {
		count[l]++
	}
	count[0] = 0
	h.minLen, h.maxLen = 0, 0
	for l := 1; l <= maxCodeLen; l++ {
		if count[l] != 0 {
			if h.minLen == 0 {
				h.minLen = uint(l)
			}
			h.maxLen = uint(l)
		}
	}
	left := 1
	for l := 1; l <= maxCodeLen; l++ {
		left = left<<1 - count[l]
		if left < 0 {
			return errDeflate64Table
		}
	}
	if left > 0 && !(partial && h.maxLen <= 1) {
		return errDeflate64Table
	}
	var next [maxCodeLen + 1]int
	for l, code := 1, 0; l <= maxCodeLen; l++ {
		code = (code + count[l-1]) << 1
		next[l] = code
	}
	var codes [288]uint16
	var subMax [primarySize]uint8
	for sym, l := range lengths {
		if l == 0 {
			continue
		}
		rev := bits.Reverse16(uint16(next[l])) >> (16 - l)
		next[l]++
		codes[sym] = rev
		if l > primaryBits {
			p := rev & (primarySize - 1)
			subMax[p] = max(subMax[p], l)
		}
	}
	clear(h.primary[:])
	var subOff [primarySize]uint32
	total := 0
	for p, m := range subMax {
		if m != 0 {
			subOff[p] = uint32(total)
			h.primary[p] = uint32(total)<<8 | linkFlag | uint32(m-primaryBits)
			total += 1 << (m - primaryBits)
		}
	}
	if cap(h.sub) < total {
		h.sub = make([]uint32, total)
	}
	h.sub = h.sub[:total]
	clear(h.sub)
	for sym, l := range lengths {
		if l == 0 {
			continue
		}
		rev, e := uint32(codes[sym]), uint32(sym)<<8|uint32(l)
		if l <= primaryBits {
			for i := rev; i < primarySize; i += 1 << l {
				h.primary[i] = e
			}
			continue
		}
		p := rev & (primarySize - 1)
		sub := h.sub[subOff[p] : subOff[p]+1<<(subMax[p]-primaryBits)]
		for i := rev >> primaryBits; i < uint32(len(sub)); i += 1 << (l - primaryBits) {
			sub[i] = e
		}
	}
	return nil
}

// Compressed input read both in runs and byte by byte
type byteSource interface {
	io.Reader
	io.ByteReader
}

// Streaming Deflate64 decoder: output goes into a 64 KiB ring that is also the match history, and Read hands out what is decoded at most a window at a time
type inflater64 struct {
	r      byteSource
	buf    *bufio.Reader
	bits   uint64
	nbits  uint
	hist   [window64]byte
	rd, wr int
	total  int64
	final  bool
	stored int
	lit    *huffman
	dist   *huffman
	copyN  int
	copyD  int
	err    error
	dynLit huffman
	dynDst huffman
}

// Deflate64 decoder of a member, taken from a pool and given back on close
func inflate64(src io.Reader) io.ReadCloser {
	d, _ := inflaters64.Get().(*inflater64)
	if d == nil {
		d = new(inflater64)
	}
	d.reset(src)
	return &codecReader{r: d, done: func() error {
		d.reset(nil)
		inflaters64.Put(d)
		return nil
	}}
}

// Starts the decoder over on src; the old window is never read again because no match may reach before the new data
func (d *inflater64) reset(src io.Reader) {
	r, ok := src.(byteSource)
	if !ok {
		if d.buf == nil {
			d.buf = bufio.NewReaderSize(src, 32<<10)
		} else {
			d.buf.Reset(src)
		}
		r = d.buf
	}
	if src == nil {
		r = nil
	}
	d.r, d.bits, d.nbits, d.rd, d.wr, d.total = r, 0, 0, 0, 0, 0
	d.final, d.stored, d.lit, d.dist, d.copyN, d.copyD, d.err = false, 0, nil, nil, 0, 0, nil
}

// Decoded bytes, decoding at most one window ahead of the caller; a stream cut short ends with io.ErrUnexpectedEOF
func (d *inflater64) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if d.rd < d.wr {
			n := copy(p, d.hist[d.rd:d.wr])
			d.rd += n
			if d.rd == window64 {
				d.rd, d.wr = 0, 0
			}
			return n, nil
		}
		if d.err != nil {
			return 0, d.err
		}
		d.err = d.step()
	}
}

// Decodes until the ring is full, the final block ends (io.EOF) or the stream fails
func (d *inflater64) step() error {
	for d.wr < window64 {
		var err error
		switch {
		case d.copyN > 0:
			d.copyN -= d.copyMatch(d.copyD, d.copyN)
		case d.stored > 0:
			err = d.storedData()
		case d.lit != nil:
			err = d.codes()
		case d.final:
			return io.EOF
		default:
			err = d.blockHeader()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// One more input byte in the bit buffer
func (d *inflater64) fill() error {
	b, err := d.r.ReadByte()
	if err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	d.bits |= uint64(b) << d.nbits
	d.nbits += 8
	return nil
}

// Next n bits of the stream, low bit first, n at most 16
func (d *inflater64) take(n uint) (int, error) {
	for d.nbits < n {
		if err := d.fill(); err != nil {
			return 0, err
		}
	}
	v := int(d.bits & (1<<n - 1))
	d.bits >>= n
	d.nbits -= n
	return v, nil
}

// Next symbol of code h, reading only the bits that decide it
func (d *inflater64) sym(h *huffman) (int, error) {
	n := h.minLen
	for {
		for d.nbits < n {
			if err := d.fill(); err != nil {
				return 0, err
			}
		}
		e := h.primary[d.bits&(primarySize-1)]
		if e&linkFlag != 0 {
			mask := uint32(1)<<(e&15) - 1
			e = h.sub[e>>8+uint32(d.bits>>primaryBits)&mask]
		}
		if l := uint(e & 15); l != 0 {
			if l <= d.nbits {
				d.bits >>= l
				d.nbits -= l
				return int(e >> 8), nil
			}
			n = l
			continue
		}
		if d.nbits >= h.maxLen {
			return 0, errDeflate64Code
		}
		n = h.maxLen
	}
}

// Reads a block header and sets up the block
func (d *inflater64) blockHeader() error {
	v, err := d.take(3)
	if err != nil {
		return err
	}
	d.final = v&1 == 1
	switch v >> 1 {
	case 0:
		d.bits >>= d.nbits & 7
		d.nbits &^= 7
		n, err := d.take(16)
		if err != nil {
			return err
		}
		complement, err := d.take(16)
		if err != nil {
			return err
		}
		if n != ^complement&0xffff {
			return errDeflate64Stored
		}
		d.stored = n
		return nil
	case 1:
		fixed := fixedCodes64()
		d.lit, d.dist = &fixed[0], &fixed[1]
		return nil
	case 2:
		return d.dynamicHeader()
	}
	return errDeflate64Block
}

// Reads the code lengths of a dynamic block and builds its codes
func (d *inflater64) dynamicHeader() error {
	v, err := d.take(14)
	if err != nil {
		return err
	}
	nlit, ndist, nclen := v&31+257, v>>5&31+1, v>>10&15+4
	if nlit > 286 {
		return errDeflate64Header
	}
	var lengths [286 + 32]uint8
	for i := range nclen {
		l, err := d.take(3)
		if err != nil {
			return err
		}
		lengths[codeLengthOrder[i]] = uint8(l)
	}
	if err := d.dynLit.build(lengths[:19], false); err != nil {
		return err
	}
	clear(lengths[:19])
	for i := 0; i < nlit+ndist; {
		sym, err := d.sym(&d.dynLit)
		if err != nil {
			return err
		}
		if sym < 16 {
			lengths[i] = uint8(sym)
			i++
			continue
		}
		var value uint8
		var repeat int
		switch sym {
		case 16:
			if i == 0 {
				return errDeflate64Header
			}
			value = lengths[i-1]
			repeat, err = d.take(2)
			repeat += 3
		case 17:
			repeat, err = d.take(3)
			repeat += 3
		default:
			repeat, err = d.take(7)
			repeat += 11
		}
		if err != nil {
			return err
		}
		if i+repeat > nlit+ndist {
			return errDeflate64Header
		}
		for range repeat {
			lengths[i] = value
			i++
		}
	}
	if lengths[256] == 0 {
		return errDeflate64Header
	}
	if err := d.dynLit.build(lengths[:nlit], true); err != nil {
		return err
	}
	if err := d.dynDst.build(lengths[nlit:nlit+ndist], true); err != nil {
		return err
	}
	d.lit, d.dist = &d.dynLit, &d.dynDst
	return nil
}

// Copies stored block bytes into the ring, those left in the bit buffer first
func (d *inflater64) storedData() error {
	for d.stored > 0 && d.wr < window64 {
		if d.nbits >= 8 {
			d.hist[d.wr] = byte(d.bits)
			d.bits >>= 8
			d.nbits -= 8
			d.wr++
			d.total++
			d.stored--
			continue
		}
		n := min(d.stored, window64-d.wr)
		got, err := io.ReadFull(d.r, d.hist[d.wr:d.wr+n])
		d.wr += got
		d.total += int64(got)
		d.stored -= got
		if err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

// Decodes literals and matches of a Huffman block until the ring is full or the block ends
func (d *inflater64) codes() error {
	for d.wr < window64 {
		sym, err := d.sym(d.lit)
		if err != nil {
			return err
		}
		if sym < 256 {
			d.hist[d.wr] = byte(sym)
			d.wr++
			d.total++
			continue
		}
		if sym == 256 {
			d.lit, d.dist = nil, nil
			return nil
		}
		sym -= 257
		if sym >= len(lengthBase64) {
			return errDeflate64Code
		}
		extra, err := d.take(uint(lengthExtra64[sym]))
		if err != nil {
			return err
		}
		length := int(lengthBase64[sym]) + extra
		dsym, err := d.sym(d.dist)
		if err != nil {
			return err
		}
		if extra, err = d.take(uint(distExtra64[dsym])); err != nil {
			return err
		}
		dist := int(distBase64[dsym]) + extra
		if int64(dist) > d.total {
			return errDeflate64Dist
		}
		if n := d.copyMatch(dist, length); n < length {
			d.copyN, d.copyD = length-n, dist
			return nil
		}
	}
	return nil
}

// Copies length bytes from dist back to the write position, as many as fit before the end of the ring, returning how many; a source behind the start of the ring is read from its end, where the previous lap still holds it
func (d *inflater64) copyMatch(dist, length int) int {
	dst := d.wr
	end := min(dst+length, window64)
	src := dst - dist
	if src < 0 {
		src += window64
		dst += copy(d.hist[dst:end], d.hist[src:])
		src = 0
	}
	for dst < end {
		dst += copy(d.hist[dst:end], d.hist[src:dst])
	}
	n := dst - d.wr
	d.wr = dst
	d.total += int64(n)
	return n
}
