package pyzip

import (
	"errors"
	"io"
)

// Input of a raw deflate stream ran out before its final block, which zlib reports as no error
var errInflateNeedInput = errors.New("inflate needs more input")

var (
	lengthBase  = [29]int{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
	lengthExtra = [29]uint{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
	distBase    = [30]int{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
	distExtra   = [30]uint{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}
	codeOrder   = [19]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
)

// Canonical Huffman code as symbol counts per length and symbols in code order
type huffman struct {
	count  [16]int
	symbol []int
	empty  bool
}

// Builds a code from lengths, returning the unused code space (negative when over subscribed)
func (h *huffman) build(lengths []int) int {
	h.count = [16]int{}
	h.symbol = make([]int, len(lengths))
	for _, l := range lengths {
		h.count[l]++
	}
	h.empty = h.count[0] == len(lengths)
	left := 1
	for l := 1; l < 16; l++ {
		left <<= 1
		left -= h.count[l]
		if left < 0 {
			return left
		}
	}
	var offs [16]int
	for l := 1; l < 15; l++ {
		offs[l+1] = offs[l] + h.count[l]
	}
	for sym, l := range lengths {
		if l != 0 {
			h.symbol[offs[l]] = sym
			offs[l]++
		}
	}
	return left
}

// Longest code length in use
func (h *huffman) maxLen() int {
	for l := 15; l > 0; l-- {
		if h.count[l] != 0 {
			return l
		}
	}
	return 0
}

// Raw deflate decoder that stops with the zlib inflate error texts
type inflater struct {
	in     io.ByteReader
	out    io.Writer
	bitbuf uint32
	bitcnt uint
	window [32768]byte
	wpos   int
	total  int64
	buf    []byte
}

// Decodes a raw deflate stream into out, returning a zlib.error for invalid data
func inflateRaw(in io.ByteReader, out io.Writer) error {
	s := &inflater{in: in, out: out}
	err := s.run()
	if errors.Is(err, errInflateNeedInput) {
		err = nil
	}
	if ferr := s.flush(); err == nil {
		return ferr
	}
	return err
}

// zlib.error of a Z_DATA_ERROR with its message
func dataError(msg string) error {
	return &ZlibError{"Error -3 while decompressing data: " + msg}
}

func (s *inflater) bits(n uint) (int, error) {
	for s.bitcnt < n {
		b, err := s.in.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, errInflateNeedInput
			}
			return 0, err
		}
		s.bitbuf |= uint32(b) << s.bitcnt
		s.bitcnt += 8
	}
	v := int(s.bitbuf & (1<<n - 1))
	s.bitbuf >>= n
	s.bitcnt -= n
	return v, nil
}

func (s *inflater) emit(b byte) error {
	s.window[s.wpos] = b
	s.wpos = (s.wpos + 1) & (len(s.window) - 1)
	s.total++
	s.buf = append(s.buf, b)
	if len(s.buf) >= 1<<15 {
		return s.flush()
	}
	return nil
}

func (s *inflater) flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	_, err := s.out.Write(s.buf)
	s.buf = s.buf[:0]
	return err
}

// Decodes one symbol, -1 when the bits name no code of an incomplete set
func (s *inflater) decode(h *huffman) (int, error) {
	if h.empty {
		return -1, nil
	}
	code, first, index := 0, 0, 0
	for l := 1; l < 16; l++ {
		b, err := s.bits(1)
		if err != nil {
			return 0, err
		}
		code |= b
		count := h.count[l]
		if code-count < first {
			return h.symbol[index+code-first], nil
		}
		index += count
		first += count
		first <<= 1
		code <<= 1
	}
	return -1, nil
}

func (s *inflater) run() error {
	for {
		last, err := s.bits(1)
		if err != nil {
			return err
		}
		kind, err := s.bits(2)
		if err != nil {
			return err
		}
		switch kind {
		case 0:
			err = s.stored()
		case 1:
			err = s.fixed()
		case 2:
			err = s.dynamic()
		default:
			return dataError("invalid block type")
		}
		if err != nil {
			return err
		}
		if last == 1 {
			return nil
		}
	}
}

func (s *inflater) stored() error {
	s.bitbuf, s.bitcnt = s.bitbuf>>(s.bitcnt&7), s.bitcnt&^7
	length, err := s.bits(16)
	if err != nil {
		return err
	}
	nlength, err := s.bits(16)
	if err != nil {
		return err
	}
	if length != ^nlength&0xffff {
		return dataError("invalid stored block lengths")
	}
	for range length {
		b, err := s.bits(8)
		if err != nil {
			return err
		}
		if err := s.emit(byte(b)); err != nil {
			return err
		}
	}
	return nil
}

func (s *inflater) fixed() error {
	lengths := make([]int, 288)
	for i := range lengths {
		switch {
		case i < 144:
			lengths[i] = 8
		case i < 256:
			lengths[i] = 9
		case i < 280:
			lengths[i] = 7
		default:
			lengths[i] = 8
		}
	}
	var lencode, distcode huffman
	lencode.build(lengths)
	dl := make([]int, 32)
	for i := range dl {
		dl[i] = 5
	}
	distcode.build(dl)
	return s.codes(&lencode, &distcode)
}

func (s *inflater) dynamic() error {
	nlen, err := s.bits(5)
	if err != nil {
		return err
	}
	ndist, err := s.bits(5)
	if err != nil {
		return err
	}
	ncode, err := s.bits(4)
	if err != nil {
		return err
	}
	nlen, ndist, ncode = nlen+257, ndist+1, ncode+4
	if nlen > 286 || ndist > 30 {
		return dataError("too many length or distance symbols")
	}
	lengths := make([]int, 19)
	for i := range ncode {
		if lengths[codeOrder[i]], err = s.bits(3); err != nil {
			return err
		}
	}
	var lencode huffman
	if left := lencode.build(lengths); !lencode.empty && left != 0 {
		return dataError("invalid code lengths set")
	}
	all := make([]int, nlen+ndist)
	for have := 0; have < nlen+ndist; {
		var sym int
		if lencode.empty {
			if _, err := s.bits(1); err != nil {
				return err
			}
		} else if sym, err = s.decode(&lencode); err != nil {
			return err
		}
		if sym < 16 {
			all[have] = sym
			have++
			continue
		}
		value, repeat := 0, 0
		switch sym {
		case 16:
			if have == 0 {
				return dataError("invalid bit length repeat")
			}
			value = all[have-1]
			r, err := s.bits(2)
			if err != nil {
				return err
			}
			repeat = 3 + r
		case 17:
			r, err := s.bits(3)
			if err != nil {
				return err
			}
			repeat = 3 + r
		default:
			r, err := s.bits(7)
			if err != nil {
				return err
			}
			repeat = 11 + r
		}
		if have+repeat > nlen+ndist {
			return dataError("invalid bit length repeat")
		}
		for range repeat {
			all[have] = value
			have++
		}
	}
	if all[256] == 0 {
		return dataError("invalid code -- missing end-of-block")
	}
	if left := lencode.build(all[:nlen]); left < 0 || (left > 0 && lencode.maxLen() != 1) {
		return dataError("invalid literal/lengths set")
	}
	var distcode huffman
	if left := distcode.build(all[nlen:]); left < 0 || (left > 0 && !distcode.empty && distcode.maxLen() != 1) {
		return dataError("invalid distances set")
	}
	return s.codes(&lencode, &distcode)
}

func (s *inflater) codes(lencode, distcode *huffman) error {
	for {
		sym, err := s.decode(lencode)
		if err != nil {
			return err
		}
		switch {
		case sym < 0 || sym > 285:
			return dataError("invalid literal/length code")
		case sym < 256:
			if err := s.emit(byte(sym)); err != nil {
				return err
			}
			continue
		case sym == 256:
			return nil
		}
		sym -= 257
		extra, err := s.bits(lengthExtra[sym])
		if err != nil {
			return err
		}
		length := lengthBase[sym] + extra
		dsym, err := s.decode(distcode)
		if err != nil {
			return err
		}
		if dsym < 0 || dsym > 29 {
			return dataError("invalid distance code")
		}
		dextra, err := s.bits(distExtra[dsym])
		if err != nil {
			return err
		}
		dist := distBase[dsym] + dextra
		if int64(dist) > s.total {
			return dataError("invalid distance too far back")
		}
		for range length {
			if err := s.emit(s.window[(s.wpos-dist)&(len(s.window)-1)]); err != nil {
				return err
			}
		}
	}
}
