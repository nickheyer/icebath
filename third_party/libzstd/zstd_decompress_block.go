package libzstd

const (
	blockSizeMaxAbs        = 1 << 17
	wildcopyOverlength     = 32
	wildcopyVecLen         = 16
	litBufferExtraSize     = 1 << 16
	minCBlockSize          = 2
	minSequencesSize       = 1
	minLiteralsFor4Streams = 6
	longNbSeq              = 0x7F00
	maxLL                  = 35
	maxML                  = 52
	maxOff                 = 31
	maxSeq                 = 52
	llFSELog               = 9
	mlFSELog               = 9
	offFSELog              = 8
)

// SymbolEncodingType_e
const (
	setBasic = iota
	setRLE
	setCompressed
	setRepeat
)

// ZSTD_litLocation_e
const (
	litNotInDst = iota
	litInDst
	litSplit
)

// ZSTD_seqSymbol
type seqSymbol struct {
	nextState        uint16
	nbAdditionalBits uint8
	nbBits           uint8
	baseValue        uint32
}

// ZSTD_seqSymbol table, its header entry held as fields
type seqTable struct {
	fastMode uint32
	tableLog uint32
	cells    []seqSymbol
}

// seq_t
type seqT struct {
	litLength, matchLength int
	offset                 uint64
}

// Window of a block, prefixStart and the external dictionary end, virtualStart kept as its distance before prefixStart
type seqWindow struct {
	ob          []byte
	prefixStart int
	extDictLen  int
	dict        []byte
	dictEnd     int
}

// ZSTD_blockSizeMax
func (dc *DCtx) blockSizeMax() int {
	if dc.isFrameDecompression {
		return int(dc.fParams.blockSizeMax)
	}
	return blockSizeMaxAbs
}

// ZSTD_allocateLiteralsBuffer placing the literals after the block, in the extra buffer, or split between both
func (dc *DCtx) allocateLiteralsBuffer(dst cptr, dstCapacity, litSize int, streaming bool, expectedWriteSize int, splitImmediately bool) {
	blockSizeMax := dc.blockSizeMax()
	ob := dst.r.b
	switch {
	case !streaming && dstCapacity > blockSizeMax+wildcopyOverlength+litSize+wildcopyOverlength:
		dc.litBufMem, dc.litBuffer = ob, dst.i+blockSizeMax+wildcopyOverlength
		dc.litMem, dc.litBufferEnd = ob, dc.litBuffer+litSize
		dc.litBufferLocation = litInDst
	case litSize <= litBufferExtraSize:
		dc.litBufMem, dc.litBuffer = dc.litExtra, 0
		dc.litMem, dc.litBufferEnd = dc.litExtra, litSize
		dc.litBufferLocation = litNotInDst
	default:
		if splitImmediately {
			dc.litBuffer = dst.i + expectedWriteSize - litSize + litBufferExtraSize - wildcopyOverlength
			dc.litBufferEnd = dc.litBuffer + litSize - litBufferExtraSize
		} else {
			dc.litBuffer = dst.i + expectedWriteSize - litSize
			dc.litBufferEnd = dst.i + expectedWriteSize
		}
		dc.litBufMem, dc.litMem = ob, ob
		dc.litBufferLocation = litSplit
	}
}

// ZSTD_decodeLiteralsBlock, the size of the literals section
func (dc *DCtx) decodeLiteralsBlock(src []byte, dst cptr, dstCapacity int, streaming bool) (int, ErrorCode) {
	srcSize := len(src)
	if srcSize < minCBlockSize {
		return 0, ErrCorruptionDetected
	}
	litEncType := src[0] & 3
	blockSizeMax := dc.blockSizeMax()
	expectedWriteSize := min(blockSizeMax, dstCapacity)
	lhlCode := src[0] >> 2 & 3
	switch litEncType {
	case setRepeat, setCompressed:
		if litEncType == setRepeat && !dc.litEntropy {
			return 0, ErrDictionaryCorrupted
		}
		if srcSize < 5 {
			return 0, ErrCorruptionDetected
		}
		lhc := readLE32(src, 0)
		var lhSize, litSize, litCSize int
		singleStream := false
		switch lhlCode {
		case 0, 1:
			singleStream = lhlCode == 0
			lhSize, litSize, litCSize = 3, int(lhc>>4&0x3FF), int(lhc>>14&0x3FF)
		case 2:
			lhSize, litSize, litCSize = 4, int(lhc>>4&0x3FFF), int(lhc>>18)
		default:
			lhSize, litSize, litCSize = 5, int(lhc>>4&0x3FFFF), int(lhc>>22)+int(src[4])<<10
		}
		if litSize > blockSizeMax {
			return 0, ErrCorruptionDetected
		}
		if !singleStream && litSize < minLiteralsFor4Streams {
			return 0, ErrLiteralsHeaderWrong
		}
		if litCSize+lhSize > srcSize {
			return 0, ErrCorruptionDetected
		}
		if expectedWriteSize < litSize {
			return 0, ErrDstSizeTooSmall
		}
		dc.allocateLiteralsBuffer(dst, dstCapacity, litSize, streaming, expectedWriteSize, false)
		lb := dc.litBufMem[dc.litBuffer:]
		cs := src[lhSize : lhSize+litCSize]
		var hufErr ErrorCode
		switch {
		case litEncType == setRepeat && singleStream:
			_, hufErr = hufDecompress1XUsingDTable(lb, litSize, cs, dc.hufPtr)
		case litEncType == setRepeat:
			_, hufErr = hufDecompress4XUsingDTable(lb, litSize, cs, dc.hufPtr)
		case singleStream:
			_, hufErr = hufDecompress1X1DCtx(dc.entropy.hufTable, lb, litSize, cs)
		default:
			_, hufErr = hufDecompress4XHufOnly(dc.entropy.hufTable, lb, litSize, cs)
		}
		if dc.litBufferLocation == litSplit {
			m := dc.litBufMem
			copy(dc.litExtra[:litBufferExtraSize], m[dc.litBufferEnd-litBufferExtraSize:dc.litBufferEnd])
			copy(m[dc.litBuffer+litBufferExtraSize-wildcopyOverlength:], m[dc.litBuffer:dc.litBuffer+litSize-litBufferExtraSize])
			dc.litBuffer += litBufferExtraSize - wildcopyOverlength
			dc.litBufferEnd -= wildcopyOverlength
		}
		if hufErr != 0 {
			return 0, ErrCorruptionDetected
		}
		dc.litMem, dc.litPtr = dc.litBufMem, dc.litBuffer
		dc.litSize = litSize
		dc.litEntropy = true
		if litEncType == setCompressed {
			dc.hufPtr = dc.entropy.hufTable
		}
		return litCSize + lhSize, 0
	case setBasic:
		var lhSize, litSize int
		switch lhlCode {
		case 0, 2:
			lhSize, litSize = 1, int(src[0]>>3)
		case 1:
			lhSize, litSize = 2, int(readLE16(src, 0)>>4)
		default:
			if srcSize < 3 {
				return 0, ErrCorruptionDetected
			}
			lhSize, litSize = 3, int(readLE24(src, 0)>>4)
		}
		if litSize > blockSizeMax {
			return 0, ErrCorruptionDetected
		}
		if expectedWriteSize < litSize {
			return 0, ErrDstSizeTooSmall
		}
		dc.allocateLiteralsBuffer(dst, dstCapacity, litSize, streaming, expectedWriteSize, true)
		if lhSize+litSize+wildcopyOverlength > srcSize {
			if litSize+lhSize > srcSize {
				return 0, ErrCorruptionDetected
			}
			if dc.litBufferLocation == litSplit {
				copy(dc.litBufMem[dc.litBuffer:], src[lhSize:lhSize+litSize-litBufferExtraSize])
				copy(dc.litExtra[:litBufferExtraSize], src[lhSize+litSize-litBufferExtraSize:lhSize+litSize])
			} else {
				copy(dc.litBufMem[dc.litBuffer:], src[lhSize:lhSize+litSize])
			}
			dc.litMem, dc.litPtr = dc.litBufMem, dc.litBuffer
			dc.litSize = litSize
			return lhSize + litSize, 0
		}
		dc.litMem, dc.litPtr = src, lhSize
		dc.litSize = litSize
		dc.litBufferEnd = lhSize + litSize
		dc.litBufferLocation = litNotInDst
		return lhSize + litSize, 0
	default:
		var lhSize, litSize int
		switch lhlCode {
		case 0, 2:
			lhSize, litSize = 1, int(src[0]>>3)
		case 1:
			if srcSize < 3 {
				return 0, ErrCorruptionDetected
			}
			lhSize, litSize = 2, int(readLE16(src, 0)>>4)
		default:
			if srcSize < 4 {
				return 0, ErrCorruptionDetected
			}
			lhSize, litSize = 3, int(readLE24(src, 0)>>4)
		}
		if litSize > blockSizeMax {
			return 0, ErrCorruptionDetected
		}
		if expectedWriteSize < litSize {
			return 0, ErrDstSizeTooSmall
		}
		dc.allocateLiteralsBuffer(dst, dstCapacity, litSize, streaming, expectedWriteSize, true)
		b := src[lhSize]
		n := litSize
		if dc.litBufferLocation == litSplit {
			n = litSize - litBufferExtraSize
			for k := range dc.litExtra[:litBufferExtraSize] {
				dc.litExtra[k] = b
			}
		}
		for k := dc.litBuffer; k < dc.litBuffer+n; k++ {
			dc.litBufMem[k] = b
		}
		dc.litMem, dc.litPtr = dc.litBufMem, dc.litBuffer
		dc.litSize = litSize
		return lhSize + 1, 0
	}
}

// ZSTD_buildSeqTable_rle
func buildSeqTableRLE(dt *seqTable, baseValue uint32, nbAddBits uint8) {
	dt.tableLog, dt.fastMode = 0, 0
	dt.cells[0] = seqSymbol{nbAdditionalBits: nbAddBits, baseValue: baseValue}
}

// ZSTD_buildFSETable_body spreading the symbols and filling each cell's next state, bit counts and base value
func buildFSETable(dt *seqTable, norm []int16, maxSymbolValue uint32, baseValue []uint32, nbAdditionalBits []uint8, tableLog uint32) {
	td := dt.cells
	maxSV1 := maxSymbolValue + 1
	tableSize := uint32(1) << tableLog
	var symbolNext [maxSeq + 1]uint16
	highThreshold := tableSize - 1
	dt.tableLog, dt.fastMode = tableLog, 1
	largeLimit := int16(1 << (tableLog - 1))
	for s := uint32(0); s < maxSV1; s++ {
		if norm[s] == -1 {
			td[highThreshold].baseValue = s
			highThreshold--
			symbolNext[s] = 1
		} else {
			if norm[s] >= largeLimit {
				dt.fastMode = 0
			}
			symbolNext[s] = uint16(norm[s])
		}
	}
	tableMask := tableSize - 1
	step := fseTableStep(tableSize)
	if highThreshold == tableSize-1 {
		spread := make([]byte, 0, tableSize+8)
		for s := uint32(0); s < maxSV1; s++ {
			for i := int16(0); i < norm[s]; i++ {
				spread = append(spread, byte(s))
			}
		}
		position := uint32(0)
		for s := uint32(0); s < tableSize; s += 2 {
			for u := uint32(0); u < 2; u++ {
				td[(position+u*step)&tableMask].baseValue = uint32(spread[s+u])
			}
			position = (position + 2*step) & tableMask
		}
	} else {
		position := uint32(0)
		for s := uint32(0); s < maxSV1; s++ {
			for i := int16(0); i < norm[s]; i++ {
				td[position].baseValue = s
				position = (position + step) & tableMask
				for position > highThreshold {
					position = (position + step) & tableMask
				}
			}
		}
	}
	for u := uint32(0); u < tableSize; u++ {
		symbol := td[u].baseValue
		nextState := uint32(symbolNext[symbol])
		symbolNext[symbol]++
		td[u].nbBits = byte(tableLog - highbit32(nextState))
		td[u].nextState = uint16((nextState << td[u].nbBits) - tableSize)
		td[u].nbAdditionalBits = nbAdditionalBits[symbol]
		td[u].baseValue = baseValue[symbol]
	}
}

// ZSTD_buildSeqTable for one of the literal length, offset or match length codes, the header bytes it read
func buildSeqTable(space *seqTable, tptr **seqTable, typ byte, max, maxLog uint32, src []byte, baseValue []uint32, nbAdditionalBits []uint8, def *seqTable, flagRepeatTable bool) (int, ErrorCode) {
	switch typ {
	case setRLE:
		if len(src) == 0 {
			return 0, ErrSrcSizeWrong
		}
		if uint32(src[0]) > max {
			return 0, ErrCorruptionDetected
		}
		buildSeqTableRLE(space, baseValue[src[0]], nbAdditionalBits[src[0]])
		*tptr = space
		return 1, 0
	case setBasic:
		*tptr = def
		return 0, 0
	case setRepeat:
		if !flagRepeatTable {
			return 0, ErrCorruptionDetected
		}
		return 0, 0
	default:
		var norm [maxSeq + 1]int16
		var tableLog uint32
		headerSize, err := fseReadNCount(norm[:], &max, &tableLog, src)
		if err != 0 || tableLog > maxLog {
			return 0, ErrCorruptionDetected
		}
		buildFSETable(space, norm[:], max, baseValue, nbAdditionalBits, tableLog)
		*tptr = space
		return headerSize, 0
	}
}

// ZSTD_decodeSeqHeaders, the sequence count and the size of the sequences section header
func (dc *DCtx) decodeSeqHeaders(src []byte) (int, int, ErrorCode) {
	iend := len(src)
	if iend < minSequencesSize {
		return 0, 0, ErrSrcSizeWrong
	}
	ip := 0
	nbSeq := int(src[ip])
	ip++
	if nbSeq > 0x7F {
		if nbSeq == 0xFF {
			if ip+2 > iend {
				return 0, 0, ErrSrcSizeWrong
			}
			nbSeq = int(readLE16(src, ip)) + longNbSeq
			ip += 2
		} else {
			if ip >= iend {
				return 0, 0, ErrSrcSizeWrong
			}
			nbSeq = (nbSeq-0x80)<<8 + int(src[ip])
			ip++
		}
	}
	if nbSeq == 0 {
		if ip != iend {
			return 0, 0, ErrCorruptionDetected
		}
		return 0, ip, 0
	}
	if ip+1 > iend {
		return 0, 0, ErrSrcSizeWrong
	}
	if src[ip]&3 != 0 {
		return 0, 0, ErrCorruptionDetected
	}
	llType, ofType, mlType := src[ip]>>6, src[ip]>>4&3, src[ip]>>2&3
	ip++
	n, err := buildSeqTable(&dc.entropy.llTable, &dc.llTptr, llType, maxLL, llFSELog, src[ip:], llBase[:], llBits[:], &llDefaultDTable, dc.fseEntropy)
	if err != 0 {
		return 0, 0, ErrCorruptionDetected
	}
	ip += n
	n, err = buildSeqTable(&dc.entropy.ofTable, &dc.ofTptr, ofType, maxOff, offFSELog, src[ip:], ofBase[:], ofBits[:], &ofDefaultDTable, dc.fseEntropy)
	if err != 0 {
		return 0, 0, ErrCorruptionDetected
	}
	ip += n
	n, err = buildSeqTable(&dc.entropy.mlTable, &dc.mlTptr, mlType, maxML, mlFSELog, src[ip:], mlBase[:], mlBits[:], &mlDefaultDTable, dc.fseEntropy)
	if err != 0 {
		return 0, 0, ErrCorruptionDetected
	}
	return nbSeq, ip + n, 0
}

// ZSTD_wildcopy in 16 byte chunks, or 8 byte chunks for a source closer than 16 bytes before the destination
func wildcopy(dst []byte, op int, src []byte, ip int, length int, overlap bool) {
	oend := op + length
	if overlap && op-ip < wildcopyVecLen {
		for {
			copy(dst[op:op+8], src[ip:ip+8])
			op, ip = op+8, ip+8
			if op >= oend {
				return
			}
		}
	}
	copy(dst[op:op+16], src[ip:ip+16])
	if 16 >= length {
		return
	}
	op, ip = op+16, ip+16
	for {
		copy(dst[op:op+16], src[ip:ip+16])
		copy(dst[op+16:op+32], src[ip+16:ip+32])
		op, ip = op+32, ip+32
		if op >= oend {
			return
		}
	}
}

var dec32table = [8]int{0, 1, 2, 1, 4, 4, 4, 4}
var dec64table = [8]int{8, 8, 8, 7, 8, 9, 10, 11}

// ZSTD_overlapCopy8 copying 8 bytes and spreading a short offset to at least 8
func overlapCopy8(b []byte, op, ip *int, offset uint64) {
	if offset < 8 {
		sub2 := dec64table[offset]
		o, i := *op, *ip
		b[o] = b[i]
		b[o+1] = b[i+1]
		b[o+2] = b[i+2]
		b[o+3] = b[i+3]
		i += dec32table[offset]
		copy(b[o+4:o+8], b[i:i+4])
		*ip = i - sub2
	} else {
		copy(b[*op:*op+8], b[*ip:*ip+8])
	}
	*ip += 8
	*op += 8
}

// ZSTD_safecopy never writing past oend_w with a wild copy and finishing byte by byte
func safecopy(dst []byte, op, oendW int, src []byte, ip, length int, overlap bool) {
	diff := op - ip
	oend := op + length
	if length < 8 {
		for ; op < oend; op, ip = op+1, ip+1 {
			dst[op] = src[ip]
		}
		return
	}
	if overlap {
		overlapCopy8(dst, &op, &ip, uint64(diff))
		length -= 8
	}
	if oend <= oendW {
		wildcopy(dst, op, src, ip, length, overlap)
		return
	}
	if op <= oendW {
		n := oendW - op
		wildcopy(dst, op, src, ip, n, overlap)
		ip, op = ip+n, op+n
	}
	for ; op < oend; op, ip = op+1, ip+1 {
		dst[op] = src[ip]
	}
}

// ZSTD_safecopyDstBeforeSrc for literals stored ahead of the output in the same buffer
func safecopyDstBeforeSrc(dst []byte, op int, src []byte, ip, length int) {
	diff := op - ip
	oend := op + length
	if length < 8 || diff > -8 {
		for ; op < oend; op, ip = op+1, ip+1 {
			dst[op] = src[ip]
		}
		return
	}
	if op <= oend-wildcopyOverlength && diff < -wildcopyVecLen {
		n := oend - wildcopyOverlength - op
		wildcopy(dst, op, src, ip, n, false)
		ip, op = ip+n, op+n
	}
	for ; op < oend; op, ip = op+1, ip+1 {
		dst[op] = src[ip]
	}
}

// Match copy shared by the exec functions, from the external dictionary first when the offset reaches past the prefix
func (w *seqWindow) copyMatch(oLitEnd int, seq seqT) (int, int, bool, ErrorCode) {
	toPrefix := uint64(oLitEnd - w.prefixStart)
	if seq.offset <= toPrefix {
		return oLitEnd, oLitEnd - int(seq.offset), false, 0
	}
	if seq.offset > toPrefix+uint64(w.extDictLen) {
		return 0, 0, true, ErrCorruptionDetected
	}
	m := w.dictEnd - int(seq.offset-toPrefix)
	if m+seq.matchLength <= w.dictEnd {
		copy(w.ob[oLitEnd:oLitEnd+seq.matchLength], w.dict[m:m+seq.matchLength])
		return 0, 0, true, 0
	}
	length1 := w.dictEnd - m
	copy(w.ob[oLitEnd:oLitEnd+length1], w.dict[m:w.dictEnd])
	return oLitEnd + length1, w.prefixStart, false, 0
}

// ZSTD_execSequenceEnd, the careful path near the end of the output or the literals
func execSequenceEnd(w *seqWindow, op, oend int, seq seqT, lit []byte, litPtr *int, litLimit int) (int, ErrorCode) {
	oLitEnd := op + seq.litLength
	sequenceLength := seq.litLength + seq.matchLength
	iLitEnd := *litPtr + seq.litLength
	oendW := oend - wildcopyOverlength
	if sequenceLength > oend-op {
		return 0, ErrDstSizeTooSmall
	}
	if seq.litLength > litLimit-*litPtr {
		return 0, ErrCorruptionDetected
	}
	safecopy(w.ob, op, oendW, lit, *litPtr, seq.litLength, false)
	*litPtr = iLitEnd
	op2, match, done, err := w.copyMatch(oLitEnd, seq)
	if err != 0 || done {
		return sequenceLength, err
	}
	seq.matchLength -= op2 - oLitEnd
	safecopy(w.ob, op2, oendW, w.ob, match, seq.matchLength, true)
	return sequenceLength, 0
}

// ZSTD_execSequenceEndSplitLitBuffer, the careful path while literals still sit in the output buffer
func execSequenceEndSplitLitBuffer(w *seqWindow, op, oend, oendW int, seq seqT, lit []byte, litPtr *int, litLimit int) (int, ErrorCode) {
	oLitEnd := op + seq.litLength
	sequenceLength := seq.litLength + seq.matchLength
	iLitEnd := *litPtr + seq.litLength
	if sequenceLength > oend-op {
		return 0, ErrDstSizeTooSmall
	}
	if seq.litLength > litLimit-*litPtr {
		return 0, ErrCorruptionDetected
	}
	if op > *litPtr && op < *litPtr+seq.litLength {
		return 0, ErrDstSizeTooSmall
	}
	safecopyDstBeforeSrc(w.ob, op, lit, *litPtr, seq.litLength)
	*litPtr = iLitEnd
	op2, match, done, err := w.copyMatch(oLitEnd, seq)
	if err != 0 || done {
		return sequenceLength, err
	}
	seq.matchLength -= op2 - oLitEnd
	safecopy(w.ob, op2, oendW, w.ob, match, seq.matchLength, true)
	return sequenceLength, 0
}

// Literal and match copy of ZSTD_execSequence once both fit with room for wild copies
func execSequenceFast(w *seqWindow, op int, seq seqT, lit []byte, litPtr *int) (int, ErrorCode) {
	ob := w.ob
	oLitEnd := op + seq.litLength
	sequenceLength := seq.litLength + seq.matchLength
	copy(ob[op:op+16], lit[*litPtr:*litPtr+16])
	if seq.litLength > 16 {
		wildcopy(ob, op+16, lit, *litPtr+16, seq.litLength-16, false)
	}
	*litPtr += seq.litLength
	op2, match, done, err := w.copyMatch(oLitEnd, seq)
	if err != 0 || done {
		return sequenceLength, err
	}
	seq.matchLength -= op2 - oLitEnd
	if seq.offset >= wildcopyVecLen {
		wildcopy(ob, op2, ob, match, seq.matchLength, false)
		return sequenceLength, 0
	}
	overlapCopy8(ob, &op2, &match, seq.offset)
	if seq.matchLength > 8 {
		wildcopy(ob, op2, ob, match, seq.matchLength-8, true)
	}
	return sequenceLength, 0
}

// ZSTD_execSequence, the bytes the sequence produced
func execSequence(w *seqWindow, op, oend int, seq seqT, lit []byte, litPtr *int, litLimit int) (int, ErrorCode) {
	if *litPtr+seq.litLength > litLimit || op+seq.litLength+seq.matchLength > oend-wildcopyOverlength {
		return execSequenceEnd(w, op, oend, seq, lit, litPtr, litLimit)
	}
	return execSequenceFast(w, op, seq, lit, litPtr)
}

// ZSTD_execSequenceSplitLitBuffer, oendW keeping the output clear of literals not yet consumed
func execSequenceSplitLitBuffer(w *seqWindow, op, oend, oendW int, seq seqT, lit []byte, litPtr *int, litLimit int) (int, ErrorCode) {
	if *litPtr+seq.litLength > litLimit || op+seq.litLength+seq.matchLength > oendW {
		return execSequenceEndSplitLitBuffer(w, op, oend, oendW, seq, lit, litPtr, litLimit)
	}
	return execSequenceFast(w, op, seq, lit, litPtr)
}

// ZSTD_fseState
type fseState struct {
	state uint64
	table *seqTable
}

// seqState_t
type seqState struct {
	d          bitDStream
	ll, of, ml fseState
	prevOffset [3]uint64
}

// ZSTD_initFseState
func (st *fseState) init(d *bitDStream, t *seqTable) {
	st.state = d.readBits(t.tableLog)
	d.reload()
	st.table = t
}

// ZSTD_decodeSequence of a 64 bit build, updating the repeat offsets
func (s *seqState) decode(isLastSeq bool) seqT {
	ll := s.ll.table.cells[s.ll.state]
	ml := s.ml.table.cells[s.ml.state]
	of := s.of.table.cells[s.of.state]
	seq := seqT{matchLength: int(ml.baseValue), litLength: int(ll.baseValue)}
	totalBits := ll.nbAdditionalBits + ml.nbAdditionalBits + of.nbAdditionalBits
	var offset uint64
	switch ofBits := uint32(of.nbAdditionalBits); {
	case ofBits > 1:
		offset = uint64(of.baseValue) + s.d.readBitsFast(ofBits)
		s.prevOffset[2], s.prevOffset[1], s.prevOffset[0] = s.prevOffset[1], s.prevOffset[0], offset
	case ofBits == 0:
		ll0 := 0
		if ll.baseValue == 0 {
			ll0 = 1
		}
		offset = s.prevOffset[ll0]
		s.prevOffset[1] = s.prevOffset[1-ll0]
		s.prevOffset[0] = offset
	default:
		ll0 := uint64(0)
		if ll.baseValue == 0 {
			ll0 = 1
		}
		offset = uint64(of.baseValue) + ll0 + s.d.readBitsFast(1)
		var temp uint64
		if offset == 3 {
			temp = s.prevOffset[0] - 1
		} else {
			temp = s.prevOffset[offset]
		}
		if temp == 0 {
			temp--
		}
		if offset != 1 {
			s.prevOffset[2] = s.prevOffset[1]
		}
		s.prevOffset[1] = s.prevOffset[0]
		s.prevOffset[0] = temp
		offset = temp
	}
	seq.offset = offset
	if ml.nbAdditionalBits > 0 {
		seq.matchLength += int(s.d.readBitsFast(uint32(ml.nbAdditionalBits)))
	}
	if totalBits >= 57-(llFSELog+mlFSELog+offFSELog) {
		s.d.reload()
	}
	if ll.nbAdditionalBits > 0 {
		seq.litLength += int(s.d.readBitsFast(uint32(ll.nbAdditionalBits)))
	}
	if !isLastSeq {
		s.ll.state = uint64(ll.nextState) + s.d.readBits(uint32(ll.nbBits))
		s.ml.state = uint64(ml.nextState) + s.d.readBits(uint32(ml.nbBits))
		s.of.state = uint64(of.nextState) + s.d.readBits(uint32(of.nbBits))
		s.d.reload()
	}
	return seq
}

// Window seen by a block written at dst
func (dc *DCtx) window(dst cptr) seqWindow {
	w := seqWindow{ob: dst.r.b, prefixStart: dc.prefixStart.i, extDictLen: dc.extDictLen}
	if dc.dictEnd.r != nil {
		w.dict, w.dictEnd = dc.dictEnd.r.b, dc.dictEnd.i
	}
	return w
}

// Bit stream and FSE states of a sequences section, false when the stream is empty or unterminated
func (dc *DCtx) initSeqState(st *seqState, src []byte) bool {
	dc.fseEntropy = true
	for i := range st.prevOffset {
		st.prevOffset[i] = uint64(dc.entropy.rep[i])
	}
	if _, err := st.d.init(src, 0, len(src)); err != 0 {
		return false
	}
	st.ll.init(&st.d, dc.llTptr)
	st.of.init(&st.d, dc.ofTptr)
	st.ml.init(&st.d, dc.mlTptr)
	return true
}

func (dc *DCtx) saveReps(st *seqState) {
	for i := range st.prevOffset {
		dc.entropy.rep[i] = uint32(st.prevOffset[i])
	}
}

// ZSTD_decompressSequences_body for literals outside the output or wholly after the block
func (dc *DCtx) decompressSequences(dst cptr, maxDstSize int, seqSrc []byte, nbSeq int) (int, ErrorCode) {
	w := dc.window(dst)
	ostart := dst.i
	oend := ostart + maxDstSize
	if dc.litBufferLocation != litNotInDst {
		oend = dc.litBuffer
	}
	op := ostart
	lit, litPtr := dc.litMem, dc.litPtr
	litEnd := litPtr + dc.litSize
	if nbSeq != 0 {
		var st seqState
		if !dc.initSeqState(&st, seqSrc) {
			return 0, ErrCorruptionDetected
		}
		for ; nbSeq > 0; nbSeq-- {
			n, err := execSequence(&w, op, oend, st.decode(nbSeq == 1), lit, &litPtr, litEnd)
			if err != 0 {
				return 0, err
			}
			op += n
		}
		if !st.d.endOfDStream() {
			return 0, ErrCorruptionDetected
		}
		dc.saveReps(&st)
	}
	lastLLSize := litEnd - litPtr
	if lastLLSize > oend-op {
		return 0, ErrDstSizeTooSmall
	}
	copy(w.ob[op:op+lastLLSize], lit[litPtr:litEnd])
	return op + lastLLSize - ostart, 0
}

// Moves the literal cursor to the extra buffer after copying what is left of the in-output literals
func (dc *DCtx) switchToExtraLiterals(ob []byte, op *int, oend int, lit *[]byte, litPtr, litBufferEnd *int, litLength *int) ErrorCode {
	leftoverLit := dc.litBufferEnd - *litPtr
	if leftoverLit != 0 {
		if leftoverLit > oend-*op {
			return ErrDstSizeTooSmall
		}
		safecopyDstBeforeSrc(ob, *op, *lit, *litPtr, leftoverLit)
		*litLength -= leftoverLit
		*op += leftoverLit
	}
	*lit, *litPtr, *litBufferEnd = dc.litExtra, 0, litBufferExtraSize
	dc.litBufferLocation = litNotInDst
	return 0
}

// Copies the last literals, first those still in the output buffer and then the extra buffer
func (dc *DCtx) lastLiterals(ob []byte, op, oend int, lit []byte, litPtr, litBufferEnd int) (int, ErrorCode) {
	if dc.litBufferLocation == litSplit {
		lastLLSize := litBufferEnd - litPtr
		if lastLLSize > oend-op {
			return 0, ErrDstSizeTooSmall
		}
		copy(ob[op:op+lastLLSize], lit[litPtr:litBufferEnd])
		op += lastLLSize
		lit, litPtr, litBufferEnd = dc.litExtra, 0, litBufferExtraSize
		dc.litBufferLocation = litNotInDst
	}
	lastLLSize := litBufferEnd - litPtr
	if lastLLSize > oend-op {
		return 0, ErrDstSizeTooSmall
	}
	copy(ob[op:op+lastLLSize], lit[litPtr:litBufferEnd])
	return op + lastLLSize, 0
}

// ZSTD_decompressSequences_bodySplitLitBuffer for literals split between the output and the extra buffer
func (dc *DCtx) decompressSequencesSplitLitBuffer(dst cptr, maxDstSize int, seqSrc []byte, nbSeq int) (int, ErrorCode) {
	w := dc.window(dst)
	ostart := dst.i
	oend := ostart + maxDstSize
	op := ostart
	lit, litPtr, litBufferEnd := dc.litMem, dc.litPtr, dc.litBufferEnd
	if nbSeq != 0 {
		var st seqState
		if !dc.initSeqState(&st, seqSrc) {
			return 0, ErrCorruptionDetected
		}
		var seq seqT
		for ; nbSeq > 0; nbSeq-- {
			seq = st.decode(nbSeq == 1)
			if litPtr+seq.litLength > dc.litBufferEnd {
				break
			}
			n, err := execSequenceSplitLitBuffer(&w, op, oend, litPtr+seq.litLength-wildcopyOverlength, seq, lit, &litPtr, litBufferEnd)
			if err != 0 {
				return 0, err
			}
			op += n
		}
		if nbSeq > 0 {
			if err := dc.switchToExtraLiterals(w.ob, &op, oend, &lit, &litPtr, &litBufferEnd, &seq.litLength); err != 0 {
				return 0, err
			}
			n, err := execSequence(&w, op, oend, seq, lit, &litPtr, litBufferEnd)
			if err != 0 {
				return 0, err
			}
			op += n
			nbSeq--
		}
		for ; nbSeq > 0; nbSeq-- {
			n, err := execSequence(&w, op, oend, st.decode(nbSeq == 1), lit, &litPtr, litBufferEnd)
			if err != 0 {
				return 0, err
			}
			op += n
		}
		if !st.d.endOfDStream() {
			return 0, ErrCorruptionDetected
		}
		dc.saveReps(&st)
	}
	end, err := dc.lastLiterals(w.ob, op, oend, lit, litPtr, litBufferEnd)
	if err != 0 {
		return 0, err
	}
	return end - ostart, 0
}

// One delayed sequence of ZSTD_decompressSequencesLong_body
func (dc *DCtx) execLong(w *seqWindow, op *int, oend int, seq *seqT, lit *[]byte, litPtr, litBufferEnd *int) ErrorCode {
	if dc.litBufferLocation == litSplit && *litPtr+seq.litLength > dc.litBufferEnd {
		if err := dc.switchToExtraLiterals(w.ob, op, oend, lit, litPtr, litBufferEnd, &seq.litLength); err != 0 {
			return err
		}
		n, err := execSequence(w, *op, oend, *seq, *lit, litPtr, *litBufferEnd)
		if err != 0 {
			return err
		}
		*op += n
		return 0
	}
	var n int
	var err ErrorCode
	if dc.litBufferLocation == litSplit {
		n, err = execSequenceSplitLitBuffer(w, *op, oend, *litPtr+seq.litLength-wildcopyOverlength, *seq, *lit, litPtr, *litBufferEnd)
	} else {
		n, err = execSequence(w, *op, oend, *seq, *lit, litPtr, *litBufferEnd)
	}
	if err != 0 {
		return err
	}
	*op += n
	return 0
}

// ZSTD_decompressSequencesLong_body decoding eight sequences ahead of their execution
func (dc *DCtx) decompressSequencesLong(dst cptr, maxDstSize int, seqSrc []byte, nbSeq int) (int, ErrorCode) {
	w := dc.window(dst)
	ostart := dst.i
	oend := ostart + maxDstSize
	if dc.litBufferLocation == litInDst {
		oend = dc.litBuffer
	}
	op := ostart
	lit, litPtr, litBufferEnd := dc.litMem, dc.litPtr, dc.litBufferEnd
	if nbSeq != 0 {
		var seqs [8]seqT
		seqAdvance := min(nbSeq, 8)
		var st seqState
		if !dc.initSeqState(&st, seqSrc) {
			return 0, ErrCorruptionDetected
		}
		seqNb := 0
		for ; seqNb < seqAdvance; seqNb++ {
			seqs[seqNb] = st.decode(seqNb == nbSeq-1)
		}
		for ; seqNb < nbSeq; seqNb++ {
			seq := st.decode(seqNb == nbSeq-1)
			if err := dc.execLong(&w, &op, oend, &seqs[(seqNb-8)&7], &lit, &litPtr, &litBufferEnd); err != 0 {
				return 0, err
			}
			seqs[seqNb&7] = seq
		}
		if !st.d.endOfDStream() {
			return 0, ErrCorruptionDetected
		}
		for seqNb -= seqAdvance; seqNb < nbSeq; seqNb++ {
			if err := dc.execLong(&w, &op, oend, &seqs[seqNb&7], &lit, &litPtr, &litBufferEnd); err != 0 {
				return 0, err
			}
		}
		dc.saveReps(&st)
	}
	end, err := dc.lastLiterals(w.ob, op, oend, lit, litPtr, litBufferEnd)
	if err != 0 {
		return 0, err
	}
	return end - ostart, 0
}

// ZSTD_getOffsetInfo, the share of offset codes needing more than 22 extra bits scaled to OffFSELog
func getOffsetInfo(t *seqTable, nbSeq int) uint32 {
	share := uint32(0)
	if nbSeq != 0 {
		for u := 0; u < 1<<t.tableLog; u++ {
			if t.cells[u].nbAdditionalBits > 22 {
				share++
			}
		}
		share <<= offFSELog - t.tableLog
	}
	return share
}

// ZSTD_decompressBlock_internal of a compressed block written at dst, the size it decoded
func (dc *DCtx) decompressBlockInternal(dst cptr, dstCapacity int, src []byte, streaming bool) (int, ErrorCode) {
	if len(src) > dc.blockSizeMax() {
		return 0, ErrSrcSizeWrong
	}
	litCSize, err := dc.decodeLiteralsBlock(src, dst, dstCapacity, streaming)
	if err != 0 {
		return 0, err
	}
	src = src[litCSize:]
	blockSizeMax := min(dstCapacity, dc.blockSizeMax())
	usePrefetchDecoder := dc.ddictIsCold
	nbSeq, seqHSize, err := dc.decodeSeqHeaders(src)
	if err != 0 {
		return 0, err
	}
	src = src[seqHSize:]
	if dstCapacity == 0 && nbSeq > 0 {
		return 0, ErrDstSizeTooSmall
	}
	if !usePrefetchDecoder && nbSeq > 8 && dst.i-dc.prefixStart.i+blockSizeMax+dc.extDictLen > 1<<24 {
		usePrefetchDecoder = getOffsetInfo(dc.ofTptr, nbSeq) >= 7
	}
	dc.ddictIsCold = false
	switch {
	case usePrefetchDecoder:
		return dc.decompressSequencesLong(dst, dstCapacity, src, nbSeq)
	case dc.litBufferLocation == litSplit:
		return dc.decompressSequencesSplitLitBuffer(dst, dstCapacity, src, nbSeq)
	}
	return dc.decompressSequences(dst, dstCapacity, src, nbSeq)
}

// ZSTD_checkContinuity starting a new prefix when dst does not follow the previous output
func (dc *DCtx) checkContinuity(dst cptr, dstSize int) {
	if dst != dc.previousDstEnd && dstSize > 0 {
		dc.dictEnd = dc.previousDstEnd
		dc.extDictLen = dc.previousDstEnd.i - dc.prefixStart.i
		dc.prefixStart = dst
		dc.previousDstEnd = dst
	}
}
