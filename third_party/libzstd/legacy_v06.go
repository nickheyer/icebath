package libzstd

const (
	v06Magic          = 0xFD2FB526
	v06FrameHeaderMin = 5
	v06FrameHeaderMax = 13
	v06WindowLogMin   = 12
	v06MaxML          = 52
	v06MaxLL          = 35
	v06MaxOff         = 28
	v06MLLog          = 9
	v06LLLog          = 9
	v06OffLog         = 8
	lLongNbSeq        = 0x7F00
	lRepNum           = 3
)

var v06FcsFieldSize = [4]int{0, 1, 2, 8}

var lLLBits = [v06MaxLL + 1]uint32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 1, 1, 2, 2, 3, 3, 4, 6, 7, 8, 9, 10, 11, 12,
	13, 14, 15, 16}

var lLLDefaultNorm = [v06MaxLL + 1]int16{4, 3, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1, 1, 1,
	2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 2, 1, 1, 1, 1, 1,
	-1, -1, -1, -1}

var lMLBits = [v06MaxML + 1]uint32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 1, 1, 2, 2, 3, 3, 4, 4, 5, 7, 8, 9, 10, 11,
	12, 13, 14, 15, 16}

var lMLDefaultNorm = [v06MaxML + 1]int16{1, 4, 3, 2, 2, 2, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, -1, -1,
	-1, -1, -1, -1, -1}

var lOFDefaultNorm = [v06MaxOff + 1]int16{1, 1, 1, 1, 1, 1, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1, -1, -1, -1, -1, -1}

const (
	lLLDefaultNormLog = 6
	lMLDefaultNormLog = 6
	lOFDefaultNormLog = 5
)

var lLLBase = [v06MaxLL + 1]uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	16, 18, 20, 22, 24, 28, 32, 40, 48, 64, 0x80, 0x100, 0x200, 0x400, 0x800, 0x1000,
	0x2000, 0x4000, 0x8000, 0x10000}

var lMLBase = [v06MaxML + 1]uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
	32, 34, 36, 38, 40, 44, 48, 56, 64, 80, 96, 0x80, 0x100, 0x200, 0x400, 0x800,
	0x1000, 0x2000, 0x4000, 0x8000, 0x10000}

var lOFBase = [v06MaxOff + 1]uint32{0, 1, 3, 7, 0xF, 0x1F, 0x3F, 0x7F,
	0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF, 0x3FFF, 0x7FFF,
	0xFFFF, 0x1FFFF, 0x3FFFF, 0x7FFFF, 0xFFFFF, 0x1FFFFF, 0x3FFFFF, 0x7FFFFF,
	0xFFFFFF, 0x1FFFFFF, 0x3FFFFFF, 1, 1}

const (
	v06dsGetFrameHeaderSize = iota
	v06dsDecodeFrameHeader
	v06dsDecodeBlockHeader
	v06dsDecompressBlock
)

type v06FrameParams struct {
	frameContentSize uint64
	windowLog        uint32
}

// ZSTDv06_DCtx with its pointers kept as addresses, an output index plus lOrigin and zero for NULL
type v06DCtx struct {
	llTable, offTable, mlTable           *lfseDTable
	hufTableX4                           *lhufDTableX4
	flagRepeatTable                      bool
	dict                                 []byte
	previousDstEnd, base, vBase, dictEnd int
	expected                             int
	headerSize                           int
	fParams                              v06FrameParams
	bType                                int
	stage                                int
	lLiterals
	headerBuffer [v06FrameHeaderMax]byte
}

func newV06DCtx() *v06DCtx {
	d := &v06DCtx{
		llTable:    newLfseDTable(v06LLLog),
		offTable:   newLfseDTable(v06OffLog),
		mlTable:    newLfseDTable(v06MLLog),
		hufTableX4: newLhufDTableX4(12),
		lLiterals:  lLiterals{litBuffer: make([]byte, lBlockSize+lWildcopyOver)},
	}
	d.decompressBegin()
	return d
}

func (d *v06DCtx) decompressBegin() {
	d.expected = v06FrameHeaderMin
	d.stage = v06dsGetFrameHeaderSize
	d.previousDstEnd, d.base, d.vBase, d.dictEnd = 0, 0, 0, 0
	d.flagRepeatTable, d.dict = false, nil
}

func v06FrameHeaderSize(src []byte) (int, ErrorCode) {
	if len(src) < v06FrameHeaderMin {
		return 0, ErrSrcSizeWrong
	}
	return v06FrameHeaderMin + v06FcsFieldSize[src[4]>>6], 0
}

// ZSTDv06_getFrameParams, the header bytes needed or zero once the parameters are read
func v06GetFrameParams(fp *v06FrameParams, src []byte) (int, ErrorCode) {
	if len(src) < v06FrameHeaderMin {
		return v06FrameHeaderMin, 0
	}
	if readLE32(src, 0) != v06Magic {
		return 0, ErrPrefixUnknown
	}
	if fhsize, _ := v06FrameHeaderSize(src); len(src) < fhsize {
		return fhsize, 0
	}
	*fp = v06FrameParams{}
	frameDesc := src[4]
	fp.windowLog = uint32(frameDesc&0xF) + v06WindowLogMin
	if frameDesc&0x20 != 0 {
		return 0, ErrFrameParameterUnsupport
	}
	switch frameDesc >> 6 {
	case 1:
		fp.frameContentSize = uint64(src[5])
	case 2:
		fp.frameContentSize = uint64(readLE16(src, 5)) + 256
	case 3:
		fp.frameContentSize = readLE64(src, 5)
	}
	return 0, 0
}

// ZSTD_buildSeqTable of v0.6 and v0.7, the description bytes used, any failure read as corruption by callers
func lBuildSeqTable(dt *lfseDTable, kind, max, maxLog uint32, src []byte, defaultNorm []int16, defaultLog uint32, repeat bool) (int, ErrorCode) {
	switch kind {
	case 1:
		if len(src) == 0 {
			return 0, ErrSrcSizeWrong
		}
		if uint32(src[0]) > max {
			return 0, ErrCorruptionDetected
		}
		lfseBuildDTableRLE(dt, src[0])
		return 1, 0
	case 0:
		lfseBuildDTable(dt, defaultNorm, max, defaultLog, true)
		return 0, 0
	case 2:
		if !repeat {
			return 0, ErrCorruptionDetected
		}
		return 0, 0
	}
	var tableLog uint32
	var norm [v06MaxML + 1]int16
	headerSize, e := lfseReadNCount(norm[:], &max, &tableLog, src)
	if e != 0 {
		return 0, ErrCorruptionDetected
	}
	if tableLog > maxLog {
		return 0, ErrCorruptionDetected
	}
	lfseBuildDTable(dt, norm[:], max, tableLog, true)
	return headerSize, 0
}

// ZSTD_decodeSeqHeaders of v0.6 and v0.7, the sequence count and the header size
func lDecodeSeqHeaders(ll, ml, off *lfseDTable, repeat bool, src []byte) (nbSeq, size int, e ErrorCode) {
	iend := len(src)
	ip := 0
	if len(src) < 1 {
		return 0, 0, ErrSrcSizeWrong
	}
	nbSeq = int(src[ip])
	ip++
	if nbSeq == 0 {
		return 0, 1, 0
	}
	if nbSeq > 0x7F {
		if nbSeq == 0xFF {
			if ip+2 > iend {
				return 0, 0, ErrSrcSizeWrong
			}
			nbSeq = int(readLE16(src, ip)) + lLongNbSeq
			ip += 2
		} else {
			if ip >= iend {
				return 0, 0, ErrSrcSizeWrong
			}
			nbSeq = (nbSeq-0x80)<<8 + int(src[ip])
			ip++
		}
	}
	if ip+4 > iend {
		return nbSeq, 0, ErrSrcSizeWrong
	}
	llType := uint32(src[ip] >> 6)
	offType := uint32(src[ip]>>4) & 3
	mlType := uint32(src[ip]>>2) & 3
	ip++
	n, e := lBuildSeqTable(ll, llType, v06MaxLL, v06LLLog, src[ip:iend], lLLDefaultNorm[:], lLLDefaultNormLog, repeat)
	if e != 0 {
		return nbSeq, 0, ErrCorruptionDetected
	}
	ip += n
	n, e = lBuildSeqTable(off, offType, v06MaxOff, v06OffLog, src[ip:iend], lOFDefaultNorm[:], lOFDefaultNormLog, repeat)
	if e != 0 {
		return nbSeq, 0, ErrCorruptionDetected
	}
	ip += n
	n, e = lBuildSeqTable(ml, mlType, v06MaxML, v06MLLog, src[ip:iend], lMLDefaultNorm[:], lMLDefaultNormLog, repeat)
	if e != 0 {
		return nbSeq, 0, ErrCorruptionDetected
	}
	ip += n
	return nbSeq, ip, 0
}

type v06SeqState struct {
	bitD       lbitD
	stateLL    lfseDState
	stateOffb  lfseDState
	stateML    lfseDState
	prevOffset [lRepNum]uint64
}

func v06DecodeSequence(seq *lseq, st *v06SeqState) {
	llCode := uint32(st.stateLL.peekSymbol())
	mlCode := uint32(st.stateML.peekSymbol())
	ofCode := uint32(st.stateOffb.peekSymbol())
	llBits := lLLBits[llCode]
	mlBits := lMLBits[mlCode]
	ofBits := ofCode
	totalBits := llBits + mlBits + ofBits
	var offset uint64
	if ofCode != 0 {
		offset = uint64(lOFBase[ofCode]) + st.bitD.readBits(ofBits)
	}
	if offset < lRepNum {
		if llCode == 0 && offset <= 1 {
			offset = 1 - offset
		}
		if offset != 0 {
			temp := st.prevOffset[offset]
			if offset != 1 {
				st.prevOffset[2] = st.prevOffset[1]
			}
			st.prevOffset[1] = st.prevOffset[0]
			offset = temp
			st.prevOffset[0] = offset
		} else {
			offset = st.prevOffset[0]
		}
	} else {
		offset -= lRepNum - 1
		st.prevOffset[2] = st.prevOffset[1]
		st.prevOffset[1] = st.prevOffset[0]
		st.prevOffset[0] = offset
	}
	seq.offset = offset
	seq.matchLength = uint64(lMLBase[mlCode]) + 3
	if mlCode > 31 {
		seq.matchLength += st.bitD.readBits(mlBits)
	}
	seq.litLength = uint64(lLLBase[llCode])
	if llCode > 15 {
		seq.litLength += st.bitD.readBits(llBits)
	}
	if totalBits > 64-7-(v06LLLog+v06MLLog+v06OffLog) {
		st.bitD.reload()
	}
	st.stateLL.updateState(&st.bitD)
	st.stateML.updateState(&st.bitD)
	st.stateOffb.updateState(&st.bitD)
}

func (d *v06DCtx) decompressSequences(out []byte, dst, maxDstSize int, src []byte) (int, ErrorCode) {
	op, oend := dst, dst+maxDstSize
	litPtr := d.litPtr
	litEnd := litPtr + d.litSize
	base, vBase, dictEnd := d.base-lOrigin, d.vBase-lOrigin, d.dictEnd-lOrigin
	nbSeq, hSize, e := lDecodeSeqHeaders(d.llTable, d.mlTable, d.offTable, d.flagRepeatTable, src)
	if e != 0 {
		return 0, e
	}
	d.flagRepeatTable = false
	if nbSeq != 0 {
		var sequence lseq
		var st v06SeqState
		sequence.offset = 1
		st.prevOffset = [lRepNum]uint64{1, 1, 1}
		if _, e := st.bitD.init(src[hSize:]); e != 0 {
			return 0, ErrCorruptionDetected
		}
		st.stateLL.init(&st.bitD, d.llTable)
		st.stateOffb.init(&st.bitD, d.offTable)
		st.stateML.init(&st.bitD, d.mlTable)
		for st.bitD.reload() <= lbitCompleted && nbSeq != 0 {
			nbSeq--
			v06DecodeSequence(&sequence, &st)
			oneSeqSize, e := lExecSequence(out, op, oend, sequence, d.litBuf, &litPtr, litEnd, base, vBase, dictEnd, d.dict, 3, false)
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

func (d *v06DCtx) decompressBlockInternal(out []byte, dst, dstCapacity int, src []byte) (int, ErrorCode) {
	if len(src) >= lBlockSize {
		return 0, ErrSrcSizeWrong
	}
	litCSize, e := d.decodeLiteralsBlock(src, v06LiteralsHuf, d.literalsRepeat())
	if e != 0 {
		return 0, e
	}
	return d.decompressSequences(out, dst, dstCapacity, src[litCSize:])
}

// ZSTDv06_decompress_usingDict without a dictionary, one whole frame decoded into out
func v06Decompress(out []byte, src []byte, dict []byte) (int, ErrorCode) {
	d := newV06DCtx()
	d.decompressBeginUsingDict(dict)
	lCheckContinuity(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, lOrigin)
	srcSize := len(src)
	ip, iend := 0, srcSize
	op, oend := 0, len(out)
	remainingSize := srcSize
	if srcSize < v06FrameHeaderMin+lBlockHeaderSize {
		return 0, ErrSrcSizeWrong
	}
	frameHeaderSize, e := v06FrameHeaderSize(src[:v06FrameHeaderMin])
	if e != 0 {
		return 0, e
	}
	if srcSize < frameHeaderSize+lBlockHeaderSize {
		return 0, ErrSrcSizeWrong
	}
	if need, e := v06GetFrameParams(&d.fParams, src[:frameHeaderSize]); need != 0 || e != 0 {
		return 0, ErrCorruptionDetected
	}
	ip += frameHeaderSize
	remainingSize -= frameHeaderSize
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

// ZSTDv06_findFrameSizeInfoLegacy, the compressed size of the frame and the bound of its content
func v06FindFrameSizeInfo(src []byte) (cSize int, dBound uint64, e ErrorCode) {
	ip := 0
	remainingSize := len(src)
	nbBlocks := uint64(0)
	frameHeaderSize, e := v06FrameHeaderSize(src)
	if e != 0 {
		return 0, lContentSizeError, e
	}
	if readLE32(src, 0) != v06Magic {
		return 0, lContentSizeError, ErrPrefixUnknown
	}
	if len(src) < frameHeaderSize+lBlockHeaderSize {
		return 0, lContentSizeError, ErrSrcSizeWrong
	}
	ip += frameHeaderSize
	remainingSize -= frameHeaderSize
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

func (d *v06DCtx) nextSrcSize() int  { return d.expected }
func (d *v06DCtx) isSkipFrame() bool { return false }

func (d *v06DCtx) decompressContinue(out []byte, dst, dstCapacity int, src []byte) (int, ErrorCode) {
	srcSize := len(src)
	if srcSize != d.expected {
		return 0, ErrSrcSizeWrong
	}
	if dstCapacity != 0 {
		lCheckContinuity(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, dst+lOrigin)
	}
	switch d.stage {
	case v06dsGetFrameHeaderSize:
		if srcSize != v06FrameHeaderMin {
			return 0, ErrSrcSizeWrong
		}
		d.headerSize, _ = v06FrameHeaderSize(src)
		copy(d.headerBuffer[:], src[:v06FrameHeaderMin])
		if d.headerSize > v06FrameHeaderMin {
			d.expected = d.headerSize - v06FrameHeaderMin
			d.stage = v06dsDecodeFrameHeader
			return 0, 0
		}
		d.expected = 0
		fallthrough
	case v06dsDecodeFrameHeader:
		copy(d.headerBuffer[v06FrameHeaderMin:], src[:d.expected])
		if _, e := v06GetFrameParams(&d.fParams, d.headerBuffer[:d.headerSize]); e != 0 {
			return 0, e
		}
		d.expected = lBlockHeaderSize
		d.stage = v06dsDecodeBlockHeader
		return 0, 0
	case v06dsDecodeBlockHeader:
		cBlockSize, blockType, e := lGetcBlockSize(src, lBlockHeaderSize)
		if e != 0 {
			return 0, e
		}
		if blockType == lbtEnd {
			d.expected = 0
			d.stage = v06dsGetFrameHeaderSize
		} else {
			d.expected = cBlockSize
			d.bType = blockType
			d.stage = v06dsDecompressBlock
		}
		return 0, 0
	case v06dsDecompressBlock:
		var rSize int
		var e ErrorCode
		switch d.bType {
		case lbtCompressed:
			rSize, e = d.decompressBlockInternal(out, dst, dstCapacity, src)
		case lbtRaw:
			rSize, e = lCopyRawBlock(out, dst, dstCapacity, src)
		case lbtRLE:
			return 0, ErrGeneric
		case lbtEnd:
			rSize = 0
		default:
			return 0, ErrGeneric
		}
		d.stage = v06dsDecodeBlockHeader
		d.expected = lBlockHeaderSize
		if e != 0 {
			return 0, e
		}
		d.previousDstEnd = dst + rSize + lOrigin
		return rSize, 0
	}
	return 0, ErrGeneric
}

func newV06ZBuff() *lzbuff {
	var fp v06FrameParams
	return &lzbuff{zd: newV06DCtx(), hintAddsHeader: true, params: func(header []byte) (int, int, ErrorCode) {
		need, e := v06GetFrameParams(&fp, header)
		return need, 1 << fp.windowLog, e
	}}
}

// Huffman literals of a v0.6 block, one stream or the four stream form chosen by size
func v06LiteralsHuf(dst []byte, dstSize int, src []byte, single bool) ErrorCode {
	var e ErrorCode
	if single {
		_, e = lhufDecompress1X2(dst, dstSize, src, false)
	} else {
		_, e = v06HufDecompress(dst, dstSize, src)
	}
	return e
}
