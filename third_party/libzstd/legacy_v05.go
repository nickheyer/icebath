package libzstd

const (
	v05Magic          = 0xFD2FB525
	lBlockSize        = 128 << 10
	lBlockHeaderSize  = 3
	v05FrameHeader    = 5
	v05WindowLogMin   = 11
	lWildcopyOver     = 8
	lContentSizeError = ^uint64(0) - 1
	lOrigin           = 1
)

const (
	lbtCompressed = iota
	lbtRaw
	lbtRLE
	lbtEnd
)

const (
	v05dsGetFrameHeaderSize = iota
	v05dsDecodeFrameHeader
	v05dsDecodeBlockHeader
	v05dsDecompressBlock
)

type lseq struct{ litLength, matchLength, offset uint64 }

// ZSTDv05_DCtx with its pointers kept as addresses, an output index plus lOrigin and zero for NULL
type v05DCtx struct {
	llTable, offTable, mlTable           *lfseDTable
	hufTableX4                           *lhufDTableX4
	flagStaticTables                     bool
	dict                                 []byte
	previousDstEnd, base, vBase, dictEnd int
	expected                             int
	headerSize                           int
	bType                                int
	stage                                int
	lLiterals
	headerBuffer [v05FrameHeader]byte
}

// Literals of the block being decoded, held in litBuffer or read in place from the block
type lLiterals struct {
	litBuf    []byte
	litPtr    int
	litSize   int
	litBuffer []byte
}

func newV05DCtx() *v05DCtx {
	d := &v05DCtx{
		llTable:    newLfseDTable(10),
		offTable:   newLfseDTable(9),
		mlTable:    newLfseDTable(10),
		hufTableX4: newLhufDTableX4(12),
		lLiterals:  lLiterals{litBuffer: make([]byte, lBlockSize+lWildcopyOver)},
	}
	d.decompressBegin()
	return d
}

func (d *v05DCtx) decompressBegin() {
	d.expected = v05FrameHeader
	d.stage = v05dsGetFrameHeaderSize
	d.previousDstEnd, d.base, d.vBase, d.dictEnd = 0, 0, 0, 0
	d.flagStaticTables, d.dict = false, nil
}

// ZSTDv05_getFrameParams, the bytes still needed or zero once the window log is read
func v05GetFrameParams(windowLog *uint32, src []byte) (int, ErrorCode) {
	if len(src) < v05FrameHeader {
		return v05FrameHeader, 0
	}
	if readLE32(src, 0) != v05Magic {
		return 0, ErrPrefixUnknown
	}
	*windowLog = uint32(src[4]&15) + v05WindowLogMin
	if src[4]>>4 != 0 {
		return 0, ErrFrameParameterUnsupport
	}
	return 0, 0
}

func (d *v05DCtx) decodeFrameHeaderPart1(src []byte) (int, ErrorCode) {
	if len(src) != v05FrameHeader {
		return 0, ErrSrcSizeWrong
	}
	if readLE32(src, 0) != v05Magic {
		return 0, ErrPrefixUnknown
	}
	d.headerSize = v05FrameHeader
	return d.headerSize, 0
}

func (d *v05DCtx) decodeFrameHeaderPart2(src []byte) (int, ErrorCode) {
	var windowLog uint32
	if len(src) != d.headerSize {
		return 0, ErrSrcSizeWrong
	}
	return v05GetFrameParams(&windowLog, src)
}

// ZSTD_getcBlockSize of the legacy decoders, the block type and the size its content takes in the frame
func lGetcBlockSize(src []byte, srcSize int) (cSize int, blockType int, e ErrorCode) {
	if srcSize < 3 {
		return 0, 0, ErrSrcSizeWrong
	}
	c := int(src[2]) + int(src[1])<<8 + int(src[0]&7)<<16
	blockType = int(src[0] >> 6)
	if blockType == lbtEnd {
		return 0, blockType, 0
	}
	if blockType == lbtRLE {
		return 1, blockType, 0
	}
	return c, blockType, 0
}

func lCopyRawBlock(out []byte, dst, maxDstSize int, src []byte) (int, ErrorCode) {
	if len(src) > maxDstSize {
		return 0, ErrDstSizeTooSmall
	}
	copy(out[dst:], src)
	return len(src), 0
}

func (d *lLiterals) useLitBuffer(litSize int) {
	d.litBuf, d.litPtr, d.litSize = d.litBuffer, 0, litSize
	clear(d.litBuffer[litSize : litSize+lWildcopyOver])
}

// ZSTD_decodeLiteralsBlock of the legacy decoders, huf decoding a fresh tree and repeat the kept one when one is usable
func (d *lLiterals) decodeLiteralsBlock(src []byte, huf func(dst []byte, dstSize int, src []byte, single bool) ErrorCode, repeat func(dst []byte, dstSize int, src []byte) ErrorCode) (int, ErrorCode) {
	srcSize := len(src)
	if srcSize < 3 {
		return 0, ErrCorruptionDetected
	}
	lhSize := int(src[0]>>4) & 3
	switch src[0] >> 6 {
	case 0:
		var litSize, litCSize int
		singleStream := false
		if srcSize < 5 {
			return 0, ErrCorruptionDetected
		}
		switch lhSize {
		default:
			lhSize = 3
			singleStream = src[0]&16 != 0
			litSize = int(src[0]&15)<<6 + int(src[1]>>2)
			litCSize = int(src[1]&3)<<8 + int(src[2])
		case 2:
			lhSize = 4
			litSize = int(src[0]&15)<<10 + int(src[1])<<2 + int(src[2]>>6)
			litCSize = int(src[2]&63)<<8 + int(src[3])
		case 3:
			lhSize = 5
			litSize = int(src[0]&15)<<14 + int(src[1])<<6 + int(src[2]>>2)
			litCSize = int(src[2]&3)<<16 + int(src[3])<<8 + int(src[4])
		}
		if litSize > lBlockSize {
			return 0, ErrCorruptionDetected
		}
		if litCSize+lhSize > srcSize {
			return 0, ErrCorruptionDetected
		}
		if e := huf(d.litBuffer, litSize, src[lhSize:lhSize+litCSize], singleStream); e != 0 {
			return 0, ErrCorruptionDetected
		}
		d.useLitBuffer(litSize)
		return litCSize + lhSize, 0
	case 1:
		if lhSize != 1 {
			return 0, ErrCorruptionDetected
		}
		if repeat == nil {
			return 0, ErrDictionaryCorrupted
		}
		litSize := int(src[0]&15)<<6 + int(src[1]>>2)
		litCSize := int(src[1]&3)<<8 + int(src[2])
		if litCSize+3 > srcSize {
			return 0, ErrCorruptionDetected
		}
		if e := repeat(d.litBuffer, litSize, src[3:3+litCSize]); e != 0 {
			return 0, ErrCorruptionDetected
		}
		d.useLitBuffer(litSize)
		return litCSize + 3, 0
	case 2:
		var litSize int
		switch lhSize {
		default:
			lhSize = 1
			litSize = int(src[0] & 31)
		case 2:
			litSize = int(src[0]&15)<<8 + int(src[1])
		case 3:
			litSize = int(src[0]&15)<<16 + int(src[1])<<8 + int(src[2])
		}
		if lhSize+litSize+lWildcopyOver > srcSize {
			if litSize+lhSize > srcSize {
				return 0, ErrCorruptionDetected
			}
			copy(d.litBuffer, src[lhSize:lhSize+litSize])
			d.useLitBuffer(litSize)
			return lhSize + litSize, 0
		}
		d.litBuf, d.litPtr, d.litSize = src, lhSize, litSize
		return lhSize + litSize, 0
	default:
		var litSize int
		switch lhSize {
		default:
			lhSize = 1
			litSize = int(src[0] & 31)
		case 2:
			litSize = int(src[0]&15)<<8 + int(src[1])
		case 3:
			litSize = int(src[0]&15)<<16 + int(src[1])<<8 + int(src[2])
			if srcSize < 4 {
				return 0, ErrCorruptionDetected
			}
		}
		if litSize > lBlockSize {
			return 0, ErrCorruptionDetected
		}
		for i := 0; i < litSize+lWildcopyOver; i++ {
			d.litBuffer[i] = src[lhSize]
		}
		d.litBuf, d.litPtr, d.litSize = d.litBuffer, 0, litSize
		return lhSize + 1, 0
	}
}

const (
	v05MLbits = 7
	v05LLbits = 6
	v05Offbit = 5
	v05MaxML  = 1<<v05MLbits - 1
	v05MaxLL  = 1<<v05LLbits - 1
	v05MaxOff = 1<<v05Offbit - 1
	v05MLLog  = 10
	v05LLLog  = 10
	v05OffLog = 9
)

// One of the three sequence tables of a v0.5 block header, ip advanced past its description
func v05BuildSeqTable(dt *lfseDTable, kind uint32, src []byte, ip *int, rawBits, maxSymbol, maxLog uint32, rleMask byte, boundRLE, flagStatic bool) ErrorCode {
	iend := len(src)
	switch kind {
	case 1:
		if boundRLE && *ip > iend-2 {
			return ErrSrcSizeWrong
		}
		lfseBuildDTableRLE(dt, src[*ip]&rleMask)
		*ip++
	case 0:
		lfseBuildDTableRaw(dt, rawBits)
	case 2:
		if !flagStatic {
			return ErrCorruptionDetected
		}
	default:
		var norm [v05MaxML + 1]int16
		var log uint32
		max := maxSymbol
		headerSize, e := lfseReadNCount(norm[:], &max, &log, src[*ip:iend])
		if e != 0 {
			return ErrGeneric
		}
		if log > maxLog {
			return ErrCorruptionDetected
		}
		*ip += headerSize
		lfseBuildDTable(dt, norm[:], max, log, false)
	}
	return 0
}

// ZSTDv05_decodeSeqHeaders, the sequence count, the dumps area and the bytes of the header
func (d *v05DCtx) decodeSeqHeaders(src []byte) (nbSeq, dumps, dumpsLength, size int, e ErrorCode) {
	iend := len(src)
	ip := 0
	if len(src) < 1 {
		return 0, 0, 0, 0, ErrSrcSizeWrong
	}
	nbSeq = int(src[ip])
	ip++
	if nbSeq == 0 {
		return 0, 0, 0, 1, 0
	}
	if nbSeq >= 128 {
		if ip >= iend {
			return nbSeq, 0, 0, 0, ErrSrcSizeWrong
		}
		nbSeq = (nbSeq-128)<<8 + int(src[ip])
		ip++
	}
	if ip >= iend {
		return nbSeq, 0, 0, 0, ErrSrcSizeWrong
	}
	llType := uint32(src[ip] >> 6)
	offType := uint32(src[ip]>>4) & 3
	mlType := uint32(src[ip]>>2) & 3
	if src[ip]&2 != 0 {
		if ip+3 > iend {
			return nbSeq, 0, 0, 0, ErrSrcSizeWrong
		}
		dumpsLength = int(src[ip+2]) + int(src[ip+1])<<8
		ip += 3
	} else {
		if ip+2 > iend {
			return nbSeq, 0, 0, 0, ErrSrcSizeWrong
		}
		dumpsLength = int(src[ip+1]) + int(src[ip]&1)<<8
		ip += 2
	}
	dumps = ip
	ip += dumpsLength
	if ip > iend-3 {
		return nbSeq, dumps, dumpsLength, 0, ErrSrcSizeWrong
	}
	if e := v05BuildSeqTable(d.llTable, llType, src, &ip, v05LLbits, v05MaxLL, v05LLLog, 0xFF, false, d.flagStaticTables); e != 0 {
		return nbSeq, dumps, dumpsLength, 0, e
	}
	if e := v05BuildSeqTable(d.offTable, offType, src, &ip, v05Offbit, v05MaxOff, v05OffLog, v05MaxOff, true, d.flagStaticTables); e != 0 {
		return nbSeq, dumps, dumpsLength, 0, e
	}
	if e := v05BuildSeqTable(d.mlTable, mlType, src, &ip, v05MLbits, v05MaxML, v05MLLog, 0xFF, true, d.flagStaticTables); e != 0 {
		return nbSeq, dumps, dumpsLength, 0, e
	}
	return nbSeq, dumps, dumpsLength, ip, 0
}

type v05SeqState struct {
	bitD       lbitD
	stateLL    lfseDState
	stateOffb  lfseDState
	stateML    lfseDState
	prevOffset uint64
	src        []byte
	dumps      int
	dumpsEnd   int
}

var v05OffsetPrefix = [v05MaxOff + 1]uint32{
	1, 1, 2, 4, 8, 16, 32, 64, 128, 256,
	512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144,
	524288, 1048576, 2097152, 4194304, 8388608, 16777216, 33554432, 1, 1, 1, 1, 1}

func v05DecodeSequence(seq *lseq, st *v05SeqState) {
	dumps, de := st.dumps, st.dumpsEnd
	litLength := uint64(st.stateLL.peekSymbol())
	prevOffset := st.prevOffset
	if litLength != 0 {
		prevOffset = seq.offset
	}
	if litLength == v05MaxLL {
		add := uint32(st.src[dumps])
		dumps++
		if add < 255 {
			litLength += uint64(add)
		} else if dumps+2 <= de {
			litLength = uint64(readLE16(st.src, dumps))
			dumps += 2
			if litLength&1 != 0 && dumps < de {
				litLength += uint64(st.src[dumps]) << 16
				dumps++
			}
			litLength >>= 1
		}
		if dumps >= de {
			dumps = de - 1
		}
	}
	offsetCode := uint32(st.stateOffb.peekSymbol())
	nbBits := offsetCode - 1
	if offsetCode == 0 {
		nbBits = 0
	}
	offset := uint64(v05OffsetPrefix[offsetCode]) + st.bitD.readBits(nbBits)
	if offsetCode == 0 {
		offset = prevOffset
	}
	if offsetCode != 0 || litLength == 0 {
		st.prevOffset = seq.offset
	}
	st.stateOffb.decodeSymbol(&st.bitD)
	st.stateLL.decodeSymbol(&st.bitD)
	matchLength := uint64(st.stateML.decodeSymbol(&st.bitD))
	if matchLength == v05MaxML {
		add := uint32(0)
		if dumps < de {
			add = uint32(st.src[dumps])
			dumps++
		}
		if add < 255 {
			matchLength += uint64(add)
		} else if dumps+2 <= de {
			matchLength = uint64(readLE16(st.src, dumps))
			dumps += 2
			if matchLength&1 != 0 && dumps < de {
				matchLength += uint64(st.src[dumps]) << 16
				dumps++
			}
			matchLength >>= 1
		}
		if dumps >= de {
			dumps = de - 1
		}
	}
	matchLength += 4
	seq.litLength, seq.offset, seq.matchLength = litLength, offset, matchLength
	st.dumps = dumps
}

// Huffman literals of a v0.5 block, one stream or the four stream form chosen by size
func v05LiteralsHuf(dst []byte, dstSize int, src []byte, single bool) ErrorCode {
	var e ErrorCode
	if single {
		_, e = lhufDecompress1X2(dst, dstSize, src, true)
	} else {
		_, e = v05HufDecompress(dst, dstSize, src)
	}
	return e
}

// ZSTD_wildcopy of the legacy decoders, eight bytes at a time and at least once
func lWildcopy(dst []byte, op int, src []byte, ip int, length int) {
	oend := op + length
	for {
		copy(dst[op:op+8], src[ip:ip+8])
		op += 8
		ip += 8
		if op >= oend {
			break
		}
	}
}

var lDec32Table = [8]int{0, 1, 2, 1, 4, 4, 4, 4}
var lDec64Table = [8]int{8, 8, 8, 7, 8, 9, 10, 11}

// ZSTD_execSequence of v0.5 and v0.6 over output indexes, base, vBase and dictEnd already less lOrigin
func lExecSequence(out []byte, op, oend int, seq lseq, litBuf []byte, litPtr *int, litLimit int, base, vBase, dictEnd int, dict []byte, minMatch int, v07 bool) (int, ErrorCode) {
	litLength, matchLength := int(seq.litLength), int(seq.matchLength)
	oLitEnd := op + litLength
	sequenceLength := litLength + matchLength
	oMatchEnd := op + sequenceLength
	oend8 := oend - 8
	litEnd := *litPtr + litLength
	match := oLitEnd - int(seq.offset)
	if v07 {
		if litLength+lWildcopyOver > oend-op {
			return 0, ErrDstSizeTooSmall
		}
		if sequenceLength > oend-op {
			return 0, ErrDstSizeTooSmall
		}
		if litLength > litLimit-*litPtr {
			return 0, ErrCorruptionDetected
		}
	} else {
		if sequenceLength > oend-op {
			return 0, ErrDstSizeTooSmall
		}
		if litLength > litLimit-*litPtr {
			return 0, ErrCorruptionDetected
		}
		if oLitEnd > oend8 {
			return 0, ErrDstSizeTooSmall
		}
		if oMatchEnd > oend {
			return 0, ErrDstSizeTooSmall
		}
		if litEnd > litLimit {
			return 0, ErrCorruptionDetected
		}
	}
	lWildcopy(out, op, litBuf, *litPtr, litLength)
	op = oLitEnd
	*litPtr = litEnd
	if seq.offset > uint64(oLitEnd-base) {
		if seq.offset > uint64(oLitEnd-vBase) {
			return 0, ErrCorruptionDetected
		}
		match = dictEnd - (base - match)
		src, off := out, 0
		if dictEnd < 0 {
			src, off = dict, lDictOrigin-lOrigin
		}
		if match+matchLength <= dictEnd {
			copy(out[oLitEnd:oLitEnd+matchLength], src[match-off:match-off+matchLength])
			return sequenceLength, 0
		}
		length1 := dictEnd - match
		copy(out[oLitEnd:oLitEnd+length1], src[match-off:match-off+length1])
		op = oLitEnd + length1
		matchLength -= length1
		match = base
		if op > oend8 || matchLength < minMatch {
			for op < oMatchEnd {
				out[op] = out[match]
				op++
				match++
			}
			return sequenceLength, 0
		}
	}
	if seq.offset < 8 {
		sub2 := lDec64Table[seq.offset]
		out[op] = out[match]
		out[op+1] = out[match+1]
		out[op+2] = out[match+2]
		out[op+3] = out[match+3]
		match += lDec32Table[seq.offset]
		copy(out[op+4:op+8], out[match:match+4])
		match -= sub2
	} else {
		copy(out[op:op+8], out[match:match+8])
	}
	op += 8
	match += 8
	if oMatchEnd > oend-(16-minMatch) {
		if op < oend8 {
			lWildcopy(out, op, out, match, oend8-op)
			match += oend8 - op
			op = oend8
		}
		for op < oMatchEnd {
			out[op] = out[match]
			op++
			match++
		}
	} else {
		lWildcopy(out, op, out, match, matchLength-8)
	}
	return sequenceLength, 0
}

func (d *v05DCtx) decompressSequences(out []byte, dst, maxDstSize int, src []byte) (int, ErrorCode) {
	op, oend := dst, dst+maxDstSize
	litPtr := d.litPtr
	litEnd := litPtr + d.litSize
	base, vBase, dictEnd := d.base-lOrigin, d.vBase-lOrigin, d.dictEnd-lOrigin
	nbSeq, dumps, dumpsLength, hSize, e := d.decodeSeqHeaders(src)
	if e != 0 {
		return 0, e
	}
	if nbSeq != 0 {
		var sequence lseq
		var st v05SeqState
		sequence.offset = 1
		st.src, st.dumps, st.dumpsEnd = src, dumps, dumps+dumpsLength
		st.prevOffset = 1
		if _, e := st.bitD.init(src[hSize:]); e != 0 {
			return 0, ErrCorruptionDetected
		}
		st.stateLL.init(&st.bitD, d.llTable)
		st.stateOffb.init(&st.bitD, d.offTable)
		st.stateML.init(&st.bitD, d.mlTable)
		for st.bitD.reload() <= lbitCompleted && nbSeq != 0 {
			nbSeq--
			v05DecodeSequence(&sequence, &st)
			oneSeqSize, e := lExecSequence(out, op, oend, sequence, d.litBuf, &litPtr, litEnd, base, vBase, dictEnd, d.dict, 4, false)
			if e != 0 {
				return 0, e
			}
			op += oneSeqSize
		}
		if nbSeq != 0 {
			return 0, ErrCorruptionDetected
		}
	}
	lastLLSize := litEnd - litPtr
	if litPtr > litEnd {
		return 0, ErrCorruptionDetected
	}
	if op+lastLLSize > oend {
		return 0, ErrDstSizeTooSmall
	}
	if lastLLSize > 0 {
		copy(out[op:op+lastLLSize], d.litBuf[litPtr:litPtr+lastLLSize])
		op += lastLLSize
	}
	return op - dst, 0
}

// ZSTD_checkContinuity of the legacy decoders, dst an address
func lCheckContinuity(previousDstEnd, base, vBase, dictEnd *int, dst int) {
	if dst != *previousDstEnd {
		*dictEnd = *previousDstEnd
		*vBase = dst - (*previousDstEnd - *base)
		*base = dst
		*previousDstEnd = dst
	}
}

func (d *v05DCtx) decompressBlockInternal(out []byte, dst, dstCapacity int, src []byte) (int, ErrorCode) {
	if len(src) >= lBlockSize {
		return 0, ErrSrcSizeWrong
	}
	litCSize, e := d.decodeLiteralsBlock(src, v05LiteralsHuf, d.literalsRepeat())
	if e != 0 {
		return 0, e
	}
	return d.decompressSequences(out, dst, dstCapacity, src[litCSize:])
}

// ZSTDv05_decompress_usingDict without a dictionary, one whole frame decoded into out
func v05Decompress(out []byte, src []byte, dict []byte) (int, ErrorCode) {
	d := newV05DCtx()
	d.decompressBeginUsingDict(dict)
	lCheckContinuity(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, lOrigin)
	srcSize := len(src)
	ip, iend := 0, srcSize
	op, oend := 0, len(out)
	remainingSize := srcSize
	if srcSize < v05FrameHeader+lBlockHeaderSize {
		return 0, ErrSrcSizeWrong
	}
	frameHeaderSize, e := d.decodeFrameHeaderPart1(src[:v05FrameHeader])
	if e != 0 {
		return 0, e
	}
	if srcSize < frameHeaderSize+lBlockHeaderSize {
		return 0, ErrSrcSizeWrong
	}
	ip += frameHeaderSize
	remainingSize -= frameHeaderSize
	if _, e := d.decodeFrameHeaderPart2(src[:frameHeaderSize]); e != 0 {
		return 0, e
	}
	for {
		decodedSize := 0
		var de ErrorCode
		cBlockSize, blockType, e := lGetcBlockSize(src[ip:], iend-ip)
		if e != 0 {
			return 0, e
		}
		ip += lBlockHeaderSize
		remainingSize -= lBlockHeaderSize
		if cBlockSize > remainingSize {
			return 0, ErrSrcSizeWrong
		}
		switch blockType {
		case lbtCompressed:
			decodedSize, de = d.decompressBlockInternal(out, op, oend-op, src[ip:ip+cBlockSize])
		case lbtRaw:
			decodedSize, de = lCopyRawBlock(out, op, oend-op, src[ip:ip+cBlockSize])
		case lbtRLE:
			return 0, ErrGeneric
		case lbtEnd:
			if remainingSize != 0 {
				return 0, ErrSrcSizeWrong
			}
		}
		if cBlockSize == 0 {
			break
		}
		if de != 0 {
			return 0, de
		}
		op += decodedSize
		ip += cBlockSize
		remainingSize -= cBlockSize
	}
	return op, 0
}

// ZSTDv05_findFrameSizeInfoLegacy, the compressed size of the frame and the bound of its content
func v05FindFrameSizeInfo(src []byte) (cSize int, dBound uint64, e ErrorCode) {
	ip := 0
	remainingSize := len(src)
	nbBlocks := uint64(0)
	if len(src) < v05FrameHeader {
		return 0, lContentSizeError, ErrSrcSizeWrong
	}
	if readLE32(src, 0) != v05Magic {
		return 0, lContentSizeError, ErrPrefixUnknown
	}
	ip += v05FrameHeader
	remainingSize -= v05FrameHeader
	for {
		cBlockSize, _, e := lGetcBlockSize(src[ip:], remainingSize)
		if e != 0 {
			return 0, lContentSizeError, e
		}
		ip += lBlockHeaderSize
		remainingSize -= lBlockHeaderSize
		if cBlockSize > remainingSize {
			return 0, lContentSizeError, ErrSrcSizeWrong
		}
		if cBlockSize == 0 {
			break
		}
		ip += cBlockSize
		remainingSize -= cBlockSize
		nbBlocks++
	}
	return ip, nbBlocks * lBlockSize, 0
}

func (d *v05DCtx) decompressContinue(out []byte, dst, maxDstSize int, src []byte) (int, ErrorCode) {
	srcSize := len(src)
	if srcSize != d.expected {
		return 0, ErrSrcSizeWrong
	}
	lCheckContinuity(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, dst+lOrigin)
	switch d.stage {
	case v05dsGetFrameHeaderSize:
		if srcSize != v05FrameHeader {
			return 0, ErrSrcSizeWrong
		}
		if _, e := d.decodeFrameHeaderPart1(src); e != 0 {
			return 0, e
		}
		copy(d.headerBuffer[:], src[:v05FrameHeader])
		d.expected = 0
		fallthrough
	case v05dsDecodeFrameHeader:
		if _, e := d.decodeFrameHeaderPart2(d.headerBuffer[:d.headerSize]); e != 0 {
			return 0, e
		}
		d.expected = lBlockHeaderSize
		d.stage = v05dsDecodeBlockHeader
		return 0, 0
	case v05dsDecodeBlockHeader:
		blockSize, blockType, e := lGetcBlockSize(src, lBlockHeaderSize)
		if e != 0 {
			return 0, e
		}
		if blockType == lbtEnd {
			d.expected = 0
			d.stage = v05dsGetFrameHeaderSize
		} else {
			d.expected = blockSize
			d.bType = blockType
			d.stage = v05dsDecompressBlock
		}
		return 0, 0
	case v05dsDecompressBlock:
		var rSize int
		var e ErrorCode
		switch d.bType {
		case lbtCompressed:
			rSize, e = d.decompressBlockInternal(out, dst, maxDstSize, src)
		case lbtRaw:
			rSize, e = lCopyRawBlock(out, dst, maxDstSize, src)
		case lbtRLE:
			return 0, ErrGeneric
		case lbtEnd:
			rSize = 0
		default:
			return 0, ErrGeneric
		}
		d.stage = v05dsDecodeBlockHeader
		d.expected = lBlockHeaderSize
		if e != 0 {
			return 0, e
		}
		d.previousDstEnd = dst + rSize + lOrigin
		return rSize, 0
	}
	return 0, ErrGeneric
}

const (
	lzbInit = iota
	lzbReadHeader
	lzbLoadHeader
	lzbDecodeHeader
	lzbRead
	lzbLoad
	lzbFlush
)

// ZBUFFv05_DCtx, the buffered streaming decoder of v0.5 frames
type v05ZBuff struct {
	zc           *v05DCtx
	windowLog    uint32
	inBuff       []byte
	inPos        int
	outBuff      []byte
	outStart     int
	outEnd       int
	hPos         int
	stage        int
	headerBuffer [v05FrameHeader]byte
}

func newV05ZBuff() *v05ZBuff {
	return &v05ZBuff{zc: newV05DCtx(), stage: lzbInit}
}

func (z *v05ZBuff) decompressInit(dict []byte) {
	z.stage = lzbReadHeader
	z.hPos, z.inPos, z.outStart, z.outEnd = 0, 0, 0, 0
	z.zc.decompressBeginUsingDict(dict)
}

func lLimitCopy(dst, src []byte) int {
	return copy(dst, src)
}

// ZBUFFv05_decompressContinue, the bytes written to dst and read from src then the next input hint
func (z *v05ZBuff) decompressContinue(dst, src []byte) (written, read int, hint uint64, e ErrorCode) {
	ip, iend := 0, len(src)
	op, oend := 0, len(dst)
	written, read = len(dst), len(src)
	notDone := true
	for notDone {
		stage := z.stage
		if stage == lzbInit {
			return written, read, 0, ErrInitMissing
		}
		if stage == lzbReadHeader {
			headerSize, e := v05GetFrameParams(&z.windowLog, src)
			if e != 0 {
				return written, read, 0, e
			}
			if headerSize != 0 {
				copy(z.headerBuffer[z.hPos:], src)
				z.hPos += len(src)
				z.stage = lzbLoadHeader
				return 0, read, uint64(headerSize - z.hPos), 0
			}
			z.stage = lzbDecodeHeader
			continue
		}
		if stage == lzbLoadHeader {
			n := lLimitCopy(z.headerBuffer[z.hPos:], src)
			z.hPos += n
			ip += n
			headerSize, e := v05GetFrameParams(&z.windowLog, z.headerBuffer[:z.hPos])
			if e != 0 {
				return written, read, 0, e
			}
			if headerSize != 0 {
				return 0, read, uint64(headerSize - z.hPos), 0
			}
			stage = lzbDecodeHeader
		}
		if stage == lzbDecodeHeader {
			neededOutSize := 1 << z.windowLog
			if len(z.inBuff) < lBlockSize {
				z.inBuff = make([]byte, lBlockSize)
			}
			if len(z.outBuff) < neededOutSize {
				z.outBuff = make([]byte, neededOutSize)
			}
			if z.hPos != 0 {
				copy(z.inBuff, z.headerBuffer[:z.hPos])
				z.inPos = z.hPos
				z.hPos = 0
				z.stage = lzbLoad
				continue
			}
			z.stage = lzbRead
			stage = lzbRead
		}
		if stage == lzbRead {
			neededInSize := z.zc.expected
			if neededInSize == 0 {
				z.stage = lzbInit
				notDone = false
				continue
			}
			if iend-ip >= neededInSize {
				decodedSize, e := z.zc.decompressContinue(z.outBuff, z.outStart, len(z.outBuff)-z.outStart, src[ip:ip+neededInSize])
				if e != 0 {
					return written, read, 0, e
				}
				ip += neededInSize
				if decodedSize == 0 {
					continue
				}
				z.outEnd = z.outStart + decodedSize
				z.stage = lzbFlush
				continue
			}
			if ip == iend {
				notDone = false
				continue
			}
			z.stage = lzbLoad
			stage = lzbLoad
		}
		if stage == lzbLoad {
			neededInSize := z.zc.expected
			toLoad := neededInSize - z.inPos
			if uint64(toLoad) > uint64(len(z.inBuff)-z.inPos) {
				return written, read, 0, ErrCorruptionDetected
			}
			loadedSize := lLimitCopy(z.inBuff[z.inPos:z.inPos+toLoad], src[ip:iend])
			ip += loadedSize
			z.inPos += loadedSize
			if loadedSize < toLoad {
				notDone = false
				continue
			}
			decodedSize, e := z.zc.decompressContinue(z.outBuff, z.outStart, len(z.outBuff)-z.outStart, z.inBuff[:neededInSize])
			if e != 0 {
				return written, read, 0, e
			}
			z.inPos = 0
			if decodedSize == 0 {
				z.stage = lzbRead
				continue
			}
			z.outEnd = z.outStart + decodedSize
			z.stage = lzbFlush
			stage = lzbFlush
		}
		if stage == lzbFlush {
			toFlushSize := z.outEnd - z.outStart
			flushedSize := lLimitCopy(dst[op:oend], z.outBuff[z.outStart:z.outStart+toFlushSize])
			op += flushedSize
			z.outStart += flushedSize
			if flushedSize == toFlushSize {
				z.stage = lzbRead
				if z.outStart+lBlockSize > len(z.outBuff) {
					z.outStart, z.outEnd = 0, 0
				}
				continue
			}
			notDone = false
		}
	}
	nextSrcSizeHint := uint64(z.zc.expected)
	if nextSrcSizeHint > lBlockHeaderSize {
		nextSrcSizeHint += lBlockHeaderSize
	}
	nextSrcSizeHint -= uint64(z.inPos)
	return op, ip, nextSrcSizeHint, 0
}
