package libzstd

import "github.com/cespare/xxhash/v2"

const (
	v07Magic             = 0xFD2FB527
	v07FrameHeaderMin    = 5
	v07FrameHeaderMax    = 18
	v07SkippableHeader   = 8
	v07MagicSkippable    = 0x184D2A50
	v07WindowLogMax      = 27
	v07WindowLogMin      = 10
	v07HufTableLog       = 12
	v07dsDecodeSkippable = v06dsDecompressBlock + 1
	v07dsSkipFrame       = v06dsDecompressBlock + 2
)

var v07FcsFieldSize = [4]int{0, 2, 4, 8}
var v07DidFieldSize = [4]int{0, 1, 2, 4}
var v07RepStartValue = [lRepNum]uint32{1, 4, 8}

var v07MLBase = [v06MaxML + 1]uint32{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18,
	19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34,
	35, 37, 39, 41, 43, 47, 51, 59, 67, 83, 99, 0x83, 0x103, 0x203, 0x403, 0x803,
	0x1003, 0x2003, 0x4003, 0x8003, 0x10003}

var v07OFBase = [v06MaxOff + 1]uint32{0, 1, 1, 5, 0xD, 0x1D, 0x3D, 0x7D,
	0xFD, 0x1FD, 0x3FD, 0x7FD, 0xFFD, 0x1FFD, 0x3FFD, 0x7FFD,
	0xFFFD, 0x1FFFD, 0x3FFFD, 0x7FFFD, 0xFFFFD, 0x1FFFFD, 0x3FFFFD, 0x7FFFFD,
	0xFFFFFD, 0x1FFFFFD, 0x3FFFFFD, 0x7FFFFFD, 0xFFFFFFD}

type v07FrameParams struct {
	frameContentSize uint64
	windowSize       uint32
	dictID           uint32
	checksumFlag     uint32
}

// ZSTDv07_DCtx with its pointers kept as addresses, an output index plus lOrigin and zero for NULL
type v07DCtx struct {
	llTable, offTable, mlTable           *lfseDTable
	hufTable                             *v07HufDTable
	dictID                               uint32
	dict                                 []byte
	previousDstEnd, base, vBase, dictEnd int
	expected                             int
	rep                                  [lRepNum]uint32
	fParams                              v07FrameParams
	bType                                int
	stage                                int
	litEntropy                           bool
	fseEntropy                           bool
	xxh                                  *xxhash.Digest
	headerSize                           int
	lLiterals
	headerBuffer [v07FrameHeaderMax]byte
}

func newV07DCtx() *v07DCtx {
	d := &v07DCtx{
		llTable:   newLfseDTable(v06LLLog),
		offTable:  newLfseDTable(v06OffLog),
		mlTable:   newLfseDTable(v06MLLog),
		hufTable:  newV07HufDTable(v07HufTableLog),
		xxh:       xxhash.New(),
		lLiterals: lLiterals{litBuffer: make([]byte, lBlockSize+lWildcopyOver)},
	}
	d.decompressBegin()
	return d
}

func (d *v07DCtx) decompressBegin() {
	d.expected = v07FrameHeaderMin
	d.stage = v06dsGetFrameHeaderSize
	d.previousDstEnd, d.base, d.vBase, d.dictEnd = 0, 0, 0, 0
	d.hufTable.maxTableLog, d.hufTable.tableType, d.hufTable.tableLog = v07HufTableLog, 0, 0
	d.litEntropy, d.fseEntropy = false, false
	d.rep = v07RepStartValue
	d.dictID, d.dict = 0, nil
}

func v07FrameHeaderSize(src []byte) (int, ErrorCode) {
	if len(src) < v07FrameHeaderMin {
		return 0, ErrSrcSizeWrong
	}
	fhd := src[4]
	directMode := int(fhd>>5) & 1
	fcs := v07FcsFieldSize[fhd>>6]
	size := v07FrameHeaderMin + 1 - directMode + v07DidFieldSize[fhd&3] + fcs
	if directMode != 0 && fcs == 0 {
		size++
	}
	return size, 0
}

// ZSTDv07_getFrameParams, the header bytes needed or zero once the parameters are read
func v07GetFrameParams(fp *v07FrameParams, src []byte) (int, ErrorCode) {
	if len(src) < v07FrameHeaderMin {
		return v07FrameHeaderMin, 0
	}
	*fp = v07FrameParams{}
	if magic := readLE32(src, 0); magic != v07Magic {
		if magic&0xFFFFFFF0 == v07MagicSkippable {
			if len(src) < v07SkippableHeader {
				return v07SkippableHeader, 0
			}
			fp.frameContentSize = uint64(readLE32(src, 4))
			return 0, 0
		}
		return 0, ErrPrefixUnknown
	}
	if fhsize, _ := v07FrameHeaderSize(src); len(src) < fhsize {
		return fhsize, 0
	}
	fhdByte := src[4]
	pos := 5
	directMode := fhdByte>>5&1 != 0
	var windowSize, dictID uint32
	var frameContentSize uint64
	if fhdByte&0x08 != 0 {
		return 0, ErrFrameParameterUnsupport
	}
	if !directMode {
		wlByte := src[pos]
		pos++
		windowLog := uint32(wlByte>>3) + v07WindowLogMin
		if windowLog > v07WindowLogMax {
			return 0, ErrFrameParameterUnsupport
		}
		windowSize = 1 << windowLog
		windowSize += (windowSize >> 3) * uint32(wlByte&7)
	}
	switch fhdByte & 3 {
	case 1:
		dictID = uint32(src[pos])
		pos++
	case 2:
		dictID = readLE16(src, pos)
		pos += 2
	case 3:
		dictID = readLE32(src, pos)
		pos += 4
	}
	switch fhdByte >> 6 {
	case 0:
		if directMode {
			frameContentSize = uint64(src[pos])
		}
	case 1:
		frameContentSize = uint64(readLE16(src, pos)) + 256
	case 2:
		frameContentSize = uint64(readLE32(src, pos))
	case 3:
		frameContentSize = readLE64(src, pos)
	}
	if windowSize == 0 {
		windowSize = uint32(frameContentSize)
	}
	if windowSize > 1<<v07WindowLogMax {
		return 0, ErrFrameParameterUnsupport
	}
	fp.frameContentSize = frameContentSize
	fp.windowSize = windowSize
	fp.dictID = dictID
	fp.checksumFlag = uint32(fhdByte>>2) & 1
	return 0, 0
}

// ZSTDv07_decodeFrameHeader with no dictionary loaded, the checksum state restarted when the frame carries one
func (d *v07DCtx) decodeFrameHeader(src []byte) (int, ErrorCode) {
	need, e := v07GetFrameParams(&d.fParams, src)
	if d.fParams.dictID != 0 && d.dictID != d.fParams.dictID {
		return 0, ErrDictionaryWrong
	}
	if d.fParams.checksumFlag != 0 {
		d.xxh.Reset()
	}
	return need, e
}

func (d *v07DCtx) literalsHuf(dst []byte, dstSize int, src []byte, single bool) ErrorCode {
	var e ErrorCode
	if single {
		_, e = d.hufTable.decompress1X2(dst, dstSize, src)
	} else {
		_, e = d.hufTable.decompress4XHufOnly(dst, dstSize, src)
	}
	if e == 0 {
		d.litEntropy = true
	}
	return e
}

func (d *v07DCtx) literalsRepeat(dst []byte, dstSize int, src []byte) ErrorCode {
	_, e := d.hufTable.decompress1X4UsingDTable(dst, dstSize, src)
	return e
}

type v07SeqState struct {
	bitD       lbitD
	stateLL    lfseDState
	stateOffb  lfseDState
	stateML    lfseDState
	prevOffset [lRepNum]uint64
}

func v07DecodeSequence(st *v07SeqState) lseq {
	var seq lseq
	llCode := uint32(st.stateLL.peekSymbol())
	mlCode := uint32(st.stateML.peekSymbol())
	ofCode := uint32(st.stateOffb.peekSymbol())
	llBits := lLLBits[llCode]
	mlBits := lMLBits[mlCode]
	ofBits := ofCode
	totalBits := llBits + mlBits + ofBits
	var offset uint64
	if ofCode != 0 {
		offset = uint64(v07OFBase[ofCode]) + st.bitD.readBits(ofBits)
	}
	if ofCode <= 1 {
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
		st.prevOffset[2] = st.prevOffset[1]
		st.prevOffset[1] = st.prevOffset[0]
		st.prevOffset[0] = offset
	}
	seq.offset = offset
	seq.matchLength = uint64(v07MLBase[mlCode])
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
	return seq
}

func (d *v07DCtx) decompressSequences(out []byte, dst, maxDstSize int, src []byte) (int, ErrorCode) {
	op, oend := dst, dst+maxDstSize
	litPtr := d.litPtr
	litEnd := litPtr + d.litSize
	base, vBase, dictEnd := d.base-lOrigin, d.vBase-lOrigin, d.dictEnd-lOrigin
	nbSeq, hSize, e := lDecodeSeqHeaders(d.llTable, d.mlTable, d.offTable, d.fseEntropy, src)
	if e != 0 {
		return 0, e
	}
	if nbSeq != 0 {
		var st v07SeqState
		d.fseEntropy = true
		for i := range st.prevOffset {
			st.prevOffset[i] = uint64(d.rep[i])
		}
		if _, e := st.bitD.init(src[hSize:]); e != 0 {
			return 0, ErrCorruptionDetected
		}
		st.stateLL.init(&st.bitD, d.llTable)
		st.stateOffb.init(&st.bitD, d.offTable)
		st.stateML.init(&st.bitD, d.mlTable)
		for st.bitD.reload() <= lbitCompleted && nbSeq != 0 {
			nbSeq--
			sequence := v07DecodeSequence(&st)
			oneSeqSize, e := lExecSequence(out, op, oend, sequence, d.litBuf, &litPtr, litEnd, base, vBase, dictEnd, d.dict, 3, true)
			if e != 0 {
				return 0, e
			}
			op += oneSeqSize
		}
		if nbSeq != 0 {
			return 0, ErrCorruptionDetected
		}
		for i := range st.prevOffset {
			d.rep[i] = uint32(st.prevOffset[i])
		}
	}
	lastLLSize := litEnd - litPtr
	if lastLLSize > oend-op {
		return 0, ErrDstSizeTooSmall
	}
	if lastLLSize > 0 {
		copy(out[op:op+lastLLSize], d.litBuf[litPtr:litPtr+lastLLSize])
		op += lastLLSize
	}
	return op - dst, 0
}

func (d *v07DCtx) decompressBlockInternal(out []byte, dst, dstCapacity int, src []byte) (int, ErrorCode) {
	if len(src) >= lBlockSize {
		return 0, ErrSrcSizeWrong
	}
	var repeat func(dst []byte, dstSize int, src []byte) ErrorCode
	if d.litEntropy {
		repeat = d.literalsRepeat
	}
	litCSize, e := d.decodeLiteralsBlock(src, d.literalsHuf, repeat)
	if e != 0 {
		return 0, e
	}
	return d.decompressSequences(out, dst, dstCapacity, src[litCSize:])
}

// Regenerated size an rle block header declares
func lBlockOrigSize(header []byte) int {
	return int(header[2]) + int(header[1])<<8 + int(header[0]&7)<<16
}

// ZSTDv07_decompress_usingDict without a dictionary, one whole frame decoded into out
func v07Decompress(out []byte, src []byte, dict []byte) (int, ErrorCode) {
	d := newV07DCtx()
	d.decompressBeginUsingDict(dict)
	lCheckContinuity(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, lOrigin)
	srcSize := len(src)
	ip, iend := 0, srcSize
	op, oend := 0, len(out)
	remainingSize := srcSize
	if srcSize < v07FrameHeaderMin+lBlockHeaderSize {
		return 0, ErrSrcSizeWrong
	}
	frameHeaderSize, e := v07FrameHeaderSize(src[:v07FrameHeaderMin])
	if e != 0 {
		return 0, e
	}
	if srcSize < frameHeaderSize+lBlockHeaderSize {
		return 0, ErrSrcSizeWrong
	}
	if need, e := d.decodeFrameHeader(src[:frameHeaderSize]); need != 0 || e != 0 {
		return 0, ErrCorruptionDetected
	}
	ip += frameHeaderSize
	remainingSize -= frameHeaderSize
	for {
		var decodedSize int
		var de ErrorCode
		cBlockSize, blockType, e := lGetcBlockSize(src[ip:], iend-ip)
		if e != 0 {
			return 0, e
		}
		header := src[ip : ip+lBlockHeaderSize]
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
			length := lBlockOrigSize(header)
			if length > oend-op {
				de = ErrDstSizeTooSmall
			} else {
				for i := 0; i < length; i++ {
					out[op+i] = src[ip]
				}
				decodedSize = length
			}
		case lbtEnd:
			if remainingSize != 0 {
				return 0, ErrSrcSizeWrong
			}
		}
		if blockType == lbtEnd {
			break
		}
		if de != 0 {
			return 0, de
		}
		if d.fParams.checksumFlag != 0 {
			d.xxh.Write(out[op : op+decodedSize])
		}
		op += decodedSize
		ip += cBlockSize
		remainingSize -= cBlockSize
	}
	return op, 0
}

// ZSTDv07_findFrameSizeInfoLegacy, the compressed size of the frame and the bound of its content
func v07FindFrameSizeInfo(src []byte) (cSize int, dBound uint64, e ErrorCode) {
	ip := 0
	remainingSize := len(src)
	nbBlocks := uint64(0)
	if len(src) < v07FrameHeaderMin+lBlockHeaderSize {
		return 0, lContentSizeError, ErrSrcSizeWrong
	}
	frameHeaderSize, e := v07FrameHeaderSize(src)
	if e != 0 {
		return 0, lContentSizeError, e
	}
	if readLE32(src, 0) != v07Magic {
		return 0, lContentSizeError, ErrPrefixUnknown
	}
	if len(src) < frameHeaderSize+lBlockHeaderSize {
		return 0, lContentSizeError, ErrSrcSizeWrong
	}
	ip += frameHeaderSize
	remainingSize -= frameHeaderSize
	for {
		cBlockSize, blockType, e := lGetcBlockSize(src[ip:], remainingSize)
		if e != 0 {
			return 0, lContentSizeError, e
		}
		ip += lBlockHeaderSize
		remainingSize -= lBlockHeaderSize
		if blockType == lbtEnd {
			break
		}
		if cBlockSize > remainingSize {
			return 0, lContentSizeError, ErrSrcSizeWrong
		}
		ip += cBlockSize
		remainingSize -= cBlockSize
		nbBlocks++
	}
	return ip, nbBlocks * lBlockSize, 0
}

func (d *v07DCtx) nextSrcSize() int  { return d.expected }
func (d *v07DCtx) isSkipFrame() bool { return d.stage == v07dsSkipFrame }

func (d *v07DCtx) decompressContinue(out []byte, dst, dstCapacity int, src []byte) (int, ErrorCode) {
	srcSize := len(src)
	if srcSize != d.expected {
		return 0, ErrSrcSizeWrong
	}
	if dstCapacity != 0 {
		lCheckContinuity(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, dst+lOrigin)
	}
	switch d.stage {
	case v06dsGetFrameHeaderSize:
		if srcSize != v07FrameHeaderMin {
			return 0, ErrSrcSizeWrong
		}
		if readLE32(src, 0)&0xFFFFFFF0 == v07MagicSkippable {
			copy(d.headerBuffer[:], src[:v07FrameHeaderMin])
			d.expected = v07SkippableHeader - v07FrameHeaderMin
			d.stage = v07dsDecodeSkippable
			return 0, 0
		}
		d.headerSize, _ = v07FrameHeaderSize(src)
		copy(d.headerBuffer[:], src[:v07FrameHeaderMin])
		if d.headerSize > v07FrameHeaderMin {
			d.expected = d.headerSize - v07FrameHeaderMin
			d.stage = v06dsDecodeFrameHeader
			return 0, 0
		}
		d.expected = 0
		fallthrough
	case v06dsDecodeFrameHeader:
		copy(d.headerBuffer[v07FrameHeaderMin:], src[:d.expected])
		if _, e := d.decodeFrameHeader(d.headerBuffer[:d.headerSize]); e != 0 {
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
			if d.fParams.checksumFlag != 0 {
				h32 := uint32(d.xxh.Sum64()>>11) & (1<<22 - 1)
				check32 := uint32(src[2]) + uint32(src[1])<<8 + uint32(src[0]&0x3F)<<16
				if check32 != h32 {
					return 0, ErrChecksumWrong
				}
			}
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
		if d.fParams.checksumFlag != 0 {
			d.xxh.Write(out[dst : dst+rSize])
		}
		return rSize, 0
	case v07dsDecodeSkippable:
		copy(d.headerBuffer[v07FrameHeaderMin:], src[:d.expected])
		d.expected = int(readLE32(d.headerBuffer[:], 4))
		d.stage = v07dsSkipFrame
		return 0, 0
	case v07dsSkipFrame:
		d.expected = 0
		d.stage = v06dsGetFrameHeaderSize
		return 0, 0
	}
	return 0, ErrGeneric
}

func newV07ZBuff() *lzbuff {
	var fp v07FrameParams
	return &lzbuff{zd: newV07DCtx(), params: func(header []byte) (int, int, ErrorCode) {
		need, e := v07GetFrameParams(&fp, header)
		return need, int(max(fp.windowSize, 1<<v07WindowLogMin)), e
	}}
}
