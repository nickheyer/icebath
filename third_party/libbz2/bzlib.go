package libbz2

import (
	"errors"
	"iter"
)

// libbzip2 return codes of BZ2_bzDecompress
const (
	OK             = 0
	StreamEnd      = 4
	SequenceError  = -1
	DataError      = -4
	DataErrorMagic = -5
)

// BZ_RUN_OK, the True a corrupt BZ_GET_FAST returns from BZ2_decompress
const runOK = 1

const (
	stateIdle = iota + 1
	stateOutput
	stateDecode
)

const (
	hdrB         = 0x42
	hdrZ         = 0x5a
	hdrH         = 0x68
	hdr0         = 0x30
	maxAlphaSize = 258
	maxCodeLen   = 23
	runA         = 0
	runB         = 1
	nGroupsMax   = 6
	gSize        = 50
	maxSelectors = 2 + 900000/gSize
	mtfaSize     = 4096
	mtflSize     = 16
)

var errStopped = errors.New("libbz2 decompressor ended")

// Decompression state of one bzip2 stream, the DState and bz_stream pair of libbzip2 1.0.8
type Decompressor struct {
	in     []byte
	out    []byte
	outPos int
	state  int

	stateOutCh      byte
	stateOutLen     int32
	blockRandomised bool
	rNToGo, rTPos   int32
	bsBuff          uint32
	bsLive          int32
	blockSize100k   int32
	origPtr         int32
	tPos            uint32
	k0              int32
	unzftab         [256]int32
	nblockUsed      int32
	saveNblock      int32
	cftab           [257]int32
	tt              []uint32

	storedBlockCRC, storedCombinedCRC         uint32
	calculatedBlockCRC, calculatedCombinedCRC uint32

	nInUse      int32
	inUse       [256]bool
	inUse16     [16]bool
	seqToUnseq  [256]byte
	mtfa        [mtfaSize]byte
	mtfbase     [256 / mtflSize]int32
	selector    [maxSelectors]byte
	selectorMtf [maxSelectors]byte
	len         [nGroupsMax][maxAlphaSize]byte
	limit       [nGroupsMax][maxAlphaSize]int32
	base        [nGroupsMax][maxAlphaSize]int32
	perm        [nGroupsMax][maxAlphaSize]int32
	minLens     [nGroupsMax]int32

	next  func() (int, bool)
	stop  func()
	yield func(int) bool
}

// Fresh decompressor as BZ2_bzDecompressInit with small and verbosity zero leaves it
func NewDecompressor() *Decompressor {
	return &Decompressor{state: stateDecode}
}

// Runs BZ2_bzDecompress over in and out, returning the bytes consumed, the bytes produced and the return code
func (s *Decompressor) Decompress(in, out []byte) (int, int, int) {
	s.in, s.out, s.outPos = in, out, 0
	ret := s.bzDecompress()
	nin, nout := len(in)-len(s.in), s.outPos
	s.in, s.out = nil, nil
	return nin, nout, ret
}

// Releases the suspended stream decoder as BZ2_bzDecompressEnd frees it, later calls get SequenceError
func (s *Decompressor) End() {
	if s.stop != nil {
		s.stop()
		s.stop, s.next = nil, nil
	}
	s.state = stateIdle
}

// Output and stream loop of BZ2_bzDecompress, a data error leaves the stream unusable
func (s *Decompressor) bzDecompress() int {
	for {
		if s.state == stateIdle {
			return SequenceError
		}
		if s.state == stateOutput {
			if s.unRLEFast() {
				s.state = stateIdle
				return DataError
			}
			if s.nblockUsed != s.saveNblock+1 || s.stateOutLen != 0 {
				return OK
			}
			s.calculatedBlockCRC = ^s.calculatedBlockCRC
			if s.calculatedBlockCRC != s.storedBlockCRC {
				s.state = stateIdle
				return DataError
			}
			s.calculatedCombinedCRC = s.calculatedCombinedCRC<<1 | s.calculatedCombinedCRC>>31
			s.calculatedCombinedCRC ^= s.calculatedBlockCRC
			s.state = stateDecode
		}
		if s.state == stateDecode {
			if s.next == nil {
				s.next, s.stop = iter.Pull(s.run)
			}
			r, _ := s.next()
			if r == StreamEnd {
				if s.calculatedCombinedCRC != s.storedCombinedCRC {
					return DataError
				}
				return r
			}
			if s.state != stateOutput {
				return r
			}
		}
	}
}

// Coroutine body of BZ2_decompress, suspended whenever it needs input or a decoded block awaits output
func (s *Decompressor) run(yield func(int) bool) {
	s.yield = yield
	defer func() {
		if r := recover(); r != nil && r != errStopped {
			panic(r)
		}
	}()
	r := s.decode()
	if r != StreamEnd {
		s.state = stateIdle
	}
	yield(r)
}

// Suspends with a return code, unwinding the coroutine when the decompressor ended
func (s *Decompressor) suspend(code int) {
	if !s.yield(code) {
		panic(errStopped)
	}
}

// GET_BITS reading n bits most significant first, suspending while no input is left
func (s *Decompressor) getBits(n int32) uint32 {
	for {
		if s.bsLive >= n {
			v := (s.bsBuff >> uint32(s.bsLive-n)) & (1<<uint32(n) - 1)
			s.bsLive -= n
			return v
		}
		if len(s.in) == 0 {
			s.suspend(OK)
			continue
		}
		s.bsBuff = s.bsBuff<<8 | uint32(s.in[0])
		s.bsLive += 8
		s.in = s.in[1:]
	}
}

// Symbol table of the bytes the block uses in byte order
func (s *Decompressor) makeMaps() {
	s.nInUse = 0
	for i := range 256 {
		if s.inUse[i] {
			s.seqToUnseq[s.nInUse] = byte(i)
			s.nInUse++
		}
	}
}

// BZ_GET_FAST step through the inverse BWT vector, false when the position runs past the block
func (s *Decompressor) getFast() (int32, bool) {
	if s.tPos >= uint32(100000)*uint32(s.blockSize100k) {
		return 0, false
	}
	s.tPos = s.tt[s.tPos]
	c := int32(s.tPos & 0xff)
	s.tPos >>= 8
	return c, true
}

// BZ_RAND_UPD_MASK advancing the randomisation run counter
func (s *Decompressor) randUpdMask() {
	if s.rNToGo == 0 {
		s.rNToGo = rNums[s.rTPos]
		s.rTPos++
		if s.rTPos == 512 {
			s.rTPos = 0
		}
	}
	s.rNToGo--
}

// BZ_RAND_MASK, one at the end of a randomisation run
func (s *Decompressor) randMask() int32 {
	if s.rNToGo == 1 {
		return 1
	}
	return 0
}

// Stream header, then each block decoded and handed to output, then the end of stream marker and CRC
func (s *Decompressor) decode() int {
	for _, want := range [...]uint32{hdrB, hdrZ, hdrH} {
		if s.getBits(8) != want {
			return DataErrorMagic
		}
	}
	s.blockSize100k = int32(s.getBits(8))
	if s.blockSize100k < hdr0+1 || s.blockSize100k > hdr0+9 {
		return DataErrorMagic
	}
	s.blockSize100k -= hdr0
	s.tt = make([]uint32, s.blockSize100k*100000)
	for {
		uc := s.getBits(8)
		if uc == 0x17 {
			break
		}
		if uc != 0x31 {
			return DataError
		}
		for _, want := range [...]uint32{0x41, 0x59, 0x26, 0x53, 0x59} {
			if s.getBits(8) != want {
				return DataError
			}
		}
		if code := s.decodeBlock(); code != OK {
			if code == runOK {
				s.suspend(runOK)
				continue
			}
			return code
		}
		s.suspend(OK)
	}
	for _, want := range [...]uint32{0x72, 0x45, 0x38, 0x50, 0x90} {
		if s.getBits(8) != want {
			return DataError
		}
	}
	s.storedCombinedCRC = 0
	for range 4 {
		s.storedCombinedCRC = s.storedCombinedCRC<<8 | s.getBits(8)
	}
	s.state = stateIdle
	return StreamEnd
}

// One block after its magic, from the stored CRC to the inverse BWT vector ready for output
func (s *Decompressor) decodeBlock() int {
	s.storedBlockCRC = 0
	for range 4 {
		s.storedBlockCRC = s.storedBlockCRC<<8 | s.getBits(8)
	}
	s.blockRandomised = s.getBits(1) == 1
	s.origPtr = 0
	for range 3 {
		s.origPtr = s.origPtr<<8 | int32(s.getBits(8))
	}
	if s.origPtr < 0 || s.origPtr > 10+100000*s.blockSize100k {
		return DataError
	}
	for i := range 16 {
		s.inUse16[i] = s.getBits(1) == 1
	}
	for i := range 256 {
		s.inUse[i] = false
	}
	for i := range 16 {
		if s.inUse16[i] {
			for j := range 16 {
				if s.getBits(1) == 1 {
					s.inUse[i*16+j] = true
				}
			}
		}
	}
	s.makeMaps()
	if s.nInUse == 0 {
		return DataError
	}
	alphaSize := s.nInUse + 2
	nGroups := int32(s.getBits(3))
	if nGroups < 2 || nGroups > nGroupsMax {
		return DataError
	}
	nSelectors := int32(s.getBits(15))
	if nSelectors < 1 {
		return DataError
	}
	for i := int32(0); i < nSelectors; i++ {
		j := int32(0)
		for s.getBits(1) != 0 {
			j++
			if j >= nGroups {
				return DataError
			}
		}
		if i < maxSelectors {
			s.selectorMtf[i] = byte(j)
		}
	}
	nSelectors = min(nSelectors, maxSelectors)
	var pos [nGroupsMax]byte
	for v := range nGroups {
		pos[v] = byte(v)
	}
	for i := range nSelectors {
		v := s.selectorMtf[i]
		tmp := pos[v]
		for ; v > 0; v-- {
			pos[v] = pos[v-1]
		}
		pos[0] = tmp
		s.selector[i] = tmp
	}
	for t := range nGroups {
		curr := int32(s.getBits(5))
		for i := range alphaSize {
			for {
				if curr < 1 || curr > 20 {
					return DataError
				}
				if s.getBits(1) == 0 {
					break
				}
				if s.getBits(1) == 0 {
					curr++
				} else {
					curr--
				}
			}
			s.len[t][i] = byte(curr)
		}
	}
	for t := range nGroups {
		minLen, maxLen := int32(32), int32(0)
		for i := range alphaSize {
			maxLen = max(maxLen, int32(s.len[t][i]))
			minLen = min(minLen, int32(s.len[t][i]))
		}
		hbCreateDecodeTables(&s.limit[t], &s.base[t], &s.perm[t], &s.len[t], minLen, maxLen, alphaSize)
		s.minLens[t] = minLen
	}
	return s.decodeMTF(alphaSize, nSelectors)
}

// Huffman and MTF decoding of the block symbols into tt, then the cftab checks and the inverse BWT
func (s *Decompressor) decodeMTF(alphaSize, nSelectors int32) int {
	eob := s.nInUse + 1
	nblockMAX := 100000 * s.blockSize100k
	groupNo, groupPos := int32(-1), int32(0)
	var gMinlen int32
	var gLimit, gBase, gPerm *[maxAlphaSize]int32
	for i := range 256 {
		s.unzftab[i] = 0
	}
	kk := int32(mtfaSize - 1)
	for ii := int32(256/mtflSize - 1); ii >= 0; ii-- {
		for jj := int32(mtflSize - 1); jj >= 0; jj-- {
			s.mtfa[kk] = byte(ii*mtflSize + jj)
			kk--
		}
		s.mtfbase[ii] = kk + 1
	}
	mtfVal := func() (int32, int) {
		if groupPos == 0 {
			groupNo++
			if groupNo >= nSelectors {
				return 0, DataError
			}
			groupPos = gSize
			gSel := s.selector[groupNo]
			gMinlen = s.minLens[gSel]
			gLimit, gPerm, gBase = &s.limit[gSel], &s.perm[gSel], &s.base[gSel]
		}
		groupPos--
		zn := gMinlen
		zvec := int32(s.getBits(zn))
		for {
			if zn > 20 {
				return 0, DataError
			}
			if zvec <= gLimit[zn] {
				break
			}
			zn++
			zvec = zvec<<1 | int32(s.getBits(1))
		}
		if zvec-gBase[zn] < 0 || zvec-gBase[zn] >= maxAlphaSize {
			return 0, DataError
		}
		return gPerm[zvec-gBase[zn]], OK
	}
	nblock := int32(0)
	nextSym, code := mtfVal()
	if code != OK {
		return code
	}
	for nextSym != eob {
		if nextSym == runA || nextSym == runB {
			es, n := int32(-1), int32(1)
			for {
				if n >= 2*1024*1024 {
					return DataError
				}
				if nextSym == runA {
					es += n
				} else {
					es += 2 * n
				}
				n *= 2
				if nextSym, code = mtfVal(); code != OK {
					return code
				}
				if nextSym != runA && nextSym != runB {
					break
				}
			}
			es++
			uc := s.seqToUnseq[s.mtfa[s.mtfbase[0]]]
			s.unzftab[uc] += es
			for ; es > 0; es-- {
				if nblock >= nblockMAX {
					return DataError
				}
				s.tt[nblock] = uint32(uc)
				nblock++
			}
			continue
		}
		if nblock >= nblockMAX {
			return DataError
		}
		uc := s.mtfSymbol(uint32(nextSym - 1))
		s.unzftab[s.seqToUnseq[uc]]++
		s.tt[nblock] = uint32(s.seqToUnseq[uc])
		nblock++
		if nextSym, code = mtfVal(); code != OK {
			return code
		}
	}
	if s.origPtr < 0 || s.origPtr >= nblock {
		return DataError
	}
	for i := range 256 {
		if s.unzftab[i] < 0 || s.unzftab[i] > nblock {
			return DataError
		}
	}
	s.cftab[0] = 0
	for i := 1; i <= 256; i++ {
		s.cftab[i] = s.unzftab[i-1]
	}
	for i := 1; i <= 256; i++ {
		s.cftab[i] += s.cftab[i-1]
	}
	for i := 0; i <= 256; i++ {
		if s.cftab[i] < 0 || s.cftab[i] > nblock {
			return DataError
		}
	}
	for i := 1; i <= 256; i++ {
		if s.cftab[i-1] > s.cftab[i] {
			return DataError
		}
	}
	s.stateOutLen, s.stateOutCh = 0, 0
	s.calculatedBlockCRC = 0xffffffff
	s.state = stateOutput
	for i := range nblock {
		uc := byte(s.tt[i] & 0xff)
		s.tt[s.cftab[uc]] |= uint32(i) << 8
		s.cftab[uc]++
	}
	s.tPos = s.tt[s.origPtr] >> 8
	s.nblockUsed = 0
	s.saveNblock = nblock
	if s.blockRandomised {
		s.rNToGo, s.rTPos = 0, 0
	}
	k0, ok := s.getFast()
	if !ok {
		return runOK
	}
	s.k0 = k0
	s.nblockUsed++
	if s.blockRandomised {
		s.randUpdMask()
		s.k0 ^= s.randMask()
	}
	return OK
}

// Move to front of the symbol at index nn, returning the symbol
func (s *Decompressor) mtfSymbol(nn uint32) byte {
	if nn < mtflSize {
		pp := s.mtfbase[0]
		uc := s.mtfa[pp+int32(nn)]
		for nn > 3 {
			z := pp + int32(nn)
			s.mtfa[z] = s.mtfa[z-1]
			s.mtfa[z-1] = s.mtfa[z-2]
			s.mtfa[z-2] = s.mtfa[z-3]
			s.mtfa[z-3] = s.mtfa[z-4]
			nn -= 4
		}
		for ; nn > 0; nn-- {
			s.mtfa[pp+int32(nn)] = s.mtfa[pp+int32(nn)-1]
		}
		s.mtfa[pp] = uc
		return uc
	}
	lno, off := int32(nn/mtflSize), int32(nn%mtflSize)
	pp := s.mtfbase[lno] + off
	uc := s.mtfa[pp]
	for ; pp > s.mtfbase[lno]; pp-- {
		s.mtfa[pp] = s.mtfa[pp-1]
	}
	s.mtfbase[lno]++
	for ; lno > 0; lno-- {
		s.mtfbase[lno]--
		s.mtfa[s.mtfbase[lno]] = s.mtfa[s.mtfbase[lno-1]+mtflSize-1]
	}
	s.mtfbase[0]--
	s.mtfa[s.mtfbase[0]] = uc
	if s.mtfbase[0] == 0 {
		kk := int32(mtfaSize - 1)
		for ii := int32(256/mtflSize - 1); ii >= 0; ii-- {
			for jj := int32(mtflSize - 1); jj >= 0; jj-- {
				s.mtfa[kk] = s.mtfa[s.mtfbase[ii]+jj]
				kk--
			}
			s.mtfbase[ii] = kk + 1
		}
	}
	return uc
}

// Canonical Huffman limit, base and permutation tables of one coding group
func hbCreateDecodeTables(limit, base, perm *[maxAlphaSize]int32, length *[maxAlphaSize]byte, minLen, maxLen, alphaSize int32) {
	pp := int32(0)
	for i := minLen; i <= maxLen; i++ {
		for j := range alphaSize {
			if int32(length[j]) == i {
				perm[pp] = j
				pp++
			}
		}
	}
	for i := range maxCodeLen {
		base[i] = 0
	}
	for i := range alphaSize {
		base[length[i]+1]++
	}
	for i := 1; i < maxCodeLen; i++ {
		base[i] += base[i-1]
	}
	for i := range maxCodeLen {
		limit[i] = 0
	}
	vec := int32(0)
	for i := minLen; i <= maxLen; i++ {
		vec += base[i+1] - base[i]
		limit[i] = vec - 1
		vec <<= 1
	}
	for i := minLen + 1; i <= maxLen; i++ {
		base[i] = (limit[i-1]+1)<<1 - base[i]
	}
}

// Writes one output byte into the block CRC
func (s *Decompressor) emit(crc *uint32, ch byte) {
	s.out[s.outPos] = ch
	s.outPos++
	*crc = *crc<<8 ^ crc32Table[(*crc>>24)^uint32(ch)]
}

// unRLE_obuf_to_output_FAST, true when the block turns out corrupt
func (s *Decompressor) unRLEFast() bool {
	if s.blockRandomised {
		return s.unRLERandomised()
	}
	crc, ch, outLen := s.calculatedBlockCRC, s.stateOutCh, s.stateOutLen
	nblockUsed, k0, tPos := s.nblockUsed, s.k0, s.tPos
	limit := uint32(100000) * uint32(s.blockSize100k)
	saveNblockPP := s.saveNblock + 1
	get := func() (int32, bool) {
		if tPos >= limit {
			return 0, false
		}
		tPos = s.tt[tPos]
		c := int32(tPos & 0xff)
		tPos >>= 8
		return c, true
	}
	writeOne := false
loop:
	for {
		if !writeOne && outLen > 0 {
			for {
				if s.outPos == len(s.out) {
					break loop
				}
				if outLen == 1 {
					break
				}
				s.emit(&crc, ch)
				outLen--
			}
			writeOne = true
		}
		if writeOne {
			writeOne = false
			if s.outPos == len(s.out) {
				outLen = 1
				break loop
			}
			s.emit(&crc, ch)
		}
		if nblockUsed > saveNblockPP {
			return true
		}
		if nblockUsed == saveNblockPP {
			outLen = 0
			break loop
		}
		ch = byte(k0)
		k1, ok := get()
		if !ok {
			return true
		}
		nblockUsed++
		if k1 != k0 {
			k0 = k1
			writeOne = true
			continue
		}
		if nblockUsed == saveNblockPP {
			writeOne = true
			continue
		}
		outLen = 2
		if k1, ok = get(); !ok {
			return true
		}
		nblockUsed++
		if nblockUsed == saveNblockPP {
			continue
		}
		if k1 != k0 {
			k0 = k1
			continue
		}
		outLen = 3
		if k1, ok = get(); !ok {
			return true
		}
		nblockUsed++
		if nblockUsed == saveNblockPP {
			continue
		}
		if k1 != k0 {
			k0 = k1
			continue
		}
		if k1, ok = get(); !ok {
			return true
		}
		nblockUsed++
		outLen = k1 + 4
		if k0, ok = get(); !ok {
			return true
		}
		nblockUsed++
	}
	s.calculatedBlockCRC, s.stateOutCh, s.stateOutLen = crc, ch, outLen
	s.nblockUsed, s.k0, s.tPos = nblockUsed, k0, tPos
	return false
}

// Randomised block branch of unRLE_obuf_to_output_FAST, each byte XORed with the run mask
func (s *Decompressor) unRLERandomised() bool {
	next := func() (int32, bool) {
		k, ok := s.getFast()
		if !ok {
			return 0, false
		}
		s.randUpdMask()
		k ^= s.randMask()
		s.nblockUsed++
		return k, true
	}
	for {
		for {
			if s.outPos == len(s.out) {
				return false
			}
			if s.stateOutLen == 0 {
				break
			}
			s.emit(&s.calculatedBlockCRC, s.stateOutCh)
			s.stateOutLen--
		}
		if s.nblockUsed == s.saveNblock+1 {
			return false
		}
		if s.nblockUsed > s.saveNblock+1 {
			return true
		}
		s.stateOutLen = 1
		s.stateOutCh = byte(s.k0)
		k1, ok := next()
		if !ok {
			return true
		}
		if s.nblockUsed == s.saveNblock+1 {
			continue
		}
		if k1 != s.k0 {
			s.k0 = k1
			continue
		}
		s.stateOutLen = 2
		if k1, ok = next(); !ok {
			return true
		}
		if s.nblockUsed == s.saveNblock+1 {
			continue
		}
		if k1 != s.k0 {
			s.k0 = k1
			continue
		}
		s.stateOutLen = 3
		if k1, ok = next(); !ok {
			return true
		}
		if s.nblockUsed == s.saveNblock+1 {
			continue
		}
		if k1 != s.k0 {
			s.k0 = k1
			continue
		}
		if k1, ok = next(); !ok {
			return true
		}
		s.stateOutLen = k1 + 4
		if s.k0, ok = next(); !ok {
			return true
		}
	}
}
