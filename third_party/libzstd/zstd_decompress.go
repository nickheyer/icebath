package libzstd

import (
	"encoding/binary"
	"math"

	"github.com/cespare/xxhash/v2"
)

const (
	magicNumber                  = 0xFD2FB528
	magicSkippableStart          = 0x184D2A50
	magicSkippableMask           = 0xFFFFFFF0
	skippableHeaderSize          = 8
	frameHeaderSizePrefix        = 5
	frameHeaderSizeMin           = 6
	frameHeaderSizeMax           = 18
	blockHeaderSize              = 3
	windowLogAbsoluteMin         = 10
	windowLogMax                 = 31
	maxWindowSizeDefault         = 1<<27 + 1
	contentSizeUnknown           = math.MaxUint64
	noForwardProgressMax         = 16
	workspaceTooLargeFactor      = 3
	workspaceTooLargeMaxDuration = 128
)

// ZSTD_dStage
const (
	dsGetFrameHeaderSize = iota
	dsDecodeBlockHeader
	dsDecompressBlock
	dsDecompressLastBlock
	dsCheckChecksum
	dsSkipFrame
)

// ZSTD_dStreamStage
const (
	zdssInit = iota
	zdssLoadHeader
	zdssRead
	zdssLoad
	zdssFlush
)

// blockType_e
const (
	btRaw = iota
	btRLE
	btCompressed
	btReserved
)

var repStartValue = [3]uint32{1, 4, 8}

// One C allocation the decoder addresses
type region struct{ b []byte }

// C pointer into a region, a nil region standing for NULL
type cptr struct {
	r *region
	i int
}

// ZSTD_FrameHeader fields the decoder reads
type frameHeader struct {
	frameContentSize uint64
	windowSize       uint64
	blockSizeMax     uint64
	skippable        bool
	headerSize       int
	dictID           uint32
	checksumFlag     bool
}

// ZSTD_entropyDTables_t
type entropyDTables struct {
	llTable, ofTable, mlTable seqTable
	hufTable                  hufDTable
	rep                       [3]uint32
}

// DCtx is a ZSTD_DCtx and ZSTD_DStream of libzstd 1.5.7 built for x86-64 with BMI2 and legacy support from v0.5
type DCtx struct {
	llTptr, mlTptr, ofTptr *seqTable
	hufPtr                 hufDTable
	entropy                entropyDTables

	prefixStart, previousDstEnd, dictEnd cptr
	extDictLen                           int

	expected             uint64
	fParams              frameHeader
	bType                int
	stage                int
	litEntropy           bool
	fseEntropy           bool
	xxh                  *xxhash.Digest
	dictID               uint32
	ddictIsCold          bool
	isFrameDecompression bool
	validateChecksum     bool
	rleSize              uint64
	decodedSize          uint64

	litExtra          []byte
	litMem, litBufMem []byte
	litPtr            int
	litBuffer         int
	litBufferEnd      int
	litSize           int
	litBufferLocation int

	streamStage           int
	headerBuffer          [frameHeaderSizeMax]byte
	lhSize                int
	inBuff                []byte
	inBuffSize, inPos     uint64
	outBuff               *region
	outBuffSize           uint64
	outStart, outEnd      uint64
	hostageByte           bool
	noForwardProgress     int
	oversizedDuration     int
	maxWindowSize         uint64
	legacyContext         *LegacyStream
	ddict                 *DDict
	legacyVersion         uint32
	previousLegacyVersion uint32
}

// NewDStream is ZSTD_createDStream, a decoder with default parameters waiting for a frame
func NewDStream() *DCtx {
	dc := &DCtx{streamStage: zdssInit, isFrameDecompression: true, maxWindowSize: maxWindowSizeDefault,
		litExtra: make([]byte, litBufferExtraSize+wildcopyOverlength), outBuff: &region{}}
	dc.entropy = newEntropyDTables()
	return dc
}

// ResetSession is ZSTD_DCtx_reset with ZSTD_reset_session_only
func (dc *DCtx) ResetSession() {
	dc.streamStage = zdssInit
	dc.noForwardProgress = 0
	dc.isFrameDecompression = true
}

// ZSTD_frameHeaderSize_internal from the frame header descriptor, src holding at least the prefix
func frameHeaderSizeInternal(src []byte) int {
	fhd := src[frameHeaderSizePrefix-1]
	single := int(fhd>>5) & 1
	size := frameHeaderSizePrefix + 1 - single + [4]int{0, 1, 2, 4}[fhd&3] + [4]int{0, 2, 4, 8}[fhd>>6]
	if single == 1 && fhd>>6 == 0 {
		size++
	}
	return size
}

// ZSTD_getFrameHeader_advanced, zero once h is filled, otherwise the input size it still needs
func getFrameHeader(h *frameHeader, src []byte) (int, ErrorCode) {
	le := binary.LittleEndian
	if len(src) < frameHeaderSizePrefix {
		if len(src) > 0 {
			hbuf := le.AppendUint32(nil, magicNumber)
			copy(hbuf, src[:min(4, len(src))])
			if le.Uint32(hbuf) != magicNumber {
				le.PutUint32(hbuf, magicSkippableStart)
				copy(hbuf, src[:min(4, len(src))])
				if le.Uint32(hbuf)&magicSkippableMask != magicSkippableStart {
					return 0, ErrPrefixUnknown
				}
			}
		}
		return frameHeaderSizePrefix, 0
	}
	*h = frameHeader{}
	if magic := le.Uint32(src); magic != magicNumber {
		if magic&magicSkippableMask != magicSkippableStart {
			return 0, ErrPrefixUnknown
		}
		if len(src) < skippableHeaderSize {
			return skippableHeaderSize, 0
		}
		*h = frameHeader{skippable: true, dictID: magic - magicSkippableStart, headerSize: skippableHeaderSize,
			frameContentSize: uint64(le.Uint32(src[4:]))}
		return 0, 0
	}
	fhsize := frameHeaderSizeInternal(src)
	if len(src) < fhsize {
		return fhsize, 0
	}
	h.headerSize = fhsize
	fhd := src[frameHeaderSizePrefix-1]
	pos := frameHeaderSizePrefix
	single := fhd>>5&1 == 1
	if fhd&0x08 != 0 {
		return 0, ErrFrameParameterUnsupport
	}
	if !single {
		wl := src[pos]
		pos++
		windowLog := uint(wl>>3) + windowLogAbsoluteMin
		if windowLog > windowLogMax {
			return 0, ErrWindowTooLarge
		}
		h.windowSize = 1 << windowLog
		h.windowSize += (h.windowSize >> 3) * uint64(wl&7)
	}
	switch fhd & 3 {
	case 1:
		h.dictID = uint32(src[pos])
		pos++
	case 2:
		h.dictID = uint32(le.Uint16(src[pos:]))
		pos += 2
	case 3:
		h.dictID = le.Uint32(src[pos:])
		pos += 4
	}
	h.frameContentSize = contentSizeUnknown
	switch fhd >> 6 {
	case 0:
		if single {
			h.frameContentSize = uint64(src[pos])
		}
	case 1:
		h.frameContentSize = uint64(le.Uint16(src[pos:])) + 256
	case 2:
		h.frameContentSize = uint64(le.Uint32(src[pos:]))
	case 3:
		h.frameContentSize = le.Uint64(src[pos:])
	}
	if single {
		h.windowSize = h.frameContentSize
	}
	h.blockSizeMax = min(h.windowSize, blockSizeMaxAbs)
	h.checksumFlag = fhd>>2&1 == 1
	return 0, 0
}

// ZSTD_getcBlockSize, the last block flag, block type, size field and compressed size
func getcBlockSize(src []byte) (last bool, blockType int, origSize uint64, cSize int, err ErrorCode) {
	if len(src) < blockHeaderSize {
		return false, 0, 0, 0, ErrSrcSizeWrong
	}
	header := uint32(src[0]) | uint32(src[1])<<8 | uint32(src[2])<<16
	size := int(header >> 3)
	last, blockType = header&1 == 1, int(header>>1&3)
	if blockType == btRLE {
		return last, blockType, uint64(size), 1, 0
	}
	if blockType == btReserved {
		return last, blockType, uint64(size), 0, ErrCorruptionDetected
	}
	return last, blockType, uint64(size), size, 0
}

// readSkippableFrameSize, the whole size of the skippable frame starting src
func readSkippableFrameSize(src []byte) (int, ErrorCode) {
	if len(src) < skippableHeaderSize {
		return 0, ErrSrcSizeWrong
	}
	sizeU32 := binary.LittleEndian.Uint32(src[4:])
	if sizeU32+skippableHeaderSize < sizeU32 {
		return 0, ErrFrameParameterUnsupport
	}
	size := skippableHeaderSize + uint64(sizeU32)
	if size > uint64(len(src)) {
		return 0, ErrSrcSizeWrong
	}
	return int(size), 0
}

// FindFrameCompressedSize is ZSTD_findFrameCompressedSize, the compressed size of the frame starting src
func FindFrameCompressedSize(src []byte) (int, ErrorCode) {
	if IsLegacy(src) != 0 {
		size, _, e := FindFrameSizeInfoLegacy(src)
		return int(size), e
	}
	if len(src) >= skippableHeaderSize && binary.LittleEndian.Uint32(src)&magicSkippableMask == magicSkippableStart {
		return readSkippableFrameSize(src)
	}
	var h frameHeader
	need, err := getFrameHeader(&h, src)
	if err != 0 {
		return 0, err
	}
	if need > 0 {
		return 0, ErrSrcSizeWrong
	}
	ip := h.headerSize
	remaining := len(src) - ip
	for {
		last, _, _, cSize, err := getcBlockSize(src[ip:])
		if err != 0 {
			return 0, err
		}
		if blockHeaderSize+cSize > remaining {
			return 0, ErrSrcSizeWrong
		}
		ip += blockHeaderSize + cSize
		remaining -= blockHeaderSize + cSize
		if last {
			break
		}
	}
	if h.checksumFlag {
		if remaining < 4 {
			return 0, ErrSrcSizeWrong
		}
		ip += 4
	}
	return ip, 0
}

// ZSTD_decompressBegin resetting the frame decoder, its window and entropy for a new frame
func (dc *DCtx) decompressBegin() {
	dc.expected = frameHeaderSizePrefix
	dc.stage = dsGetFrameHeaderSize
	dc.decodedSize = 0
	dc.previousDstEnd, dc.prefixStart, dc.dictEnd = cptr{}, cptr{}, cptr{}
	dc.extDictLen = 0
	dc.entropy.hufTable[0] = hufDTableCapacityLog * 0x1000001
	dc.litEntropy, dc.fseEntropy = false, false
	dc.dictID = 0
	dc.bType = btReserved
	dc.isFrameDecompression = true
	dc.entropy.rep = repStartValue
	dc.llTptr, dc.mlTptr, dc.ofTptr = &dc.entropy.llTable, &dc.entropy.mlTable, &dc.entropy.ofTable
	dc.hufPtr = dc.entropy.hufTable
}

// ZSTD_decodeFrameHeader, a frame dictionary id other than the loaded one refused
func (dc *DCtx) decodeFrameHeader(src []byte) ErrorCode {
	need, err := getFrameHeader(&dc.fParams, src)
	if err != 0 {
		return err
	}
	if need > 0 {
		return ErrSrcSizeWrong
	}
	if dc.fParams.dictID != 0 && dc.dictID != dc.fParams.dictID {
		return ErrDictionaryWrong
	}
	dc.validateChecksum = dc.fParams.checksumFlag
	if dc.validateChecksum {
		dc.xxh = xxhash.New()
	}
	return 0
}

// ZSTD_copyRawBlock
func copyRawBlock(dst []byte, op, dstCapacity int, src []byte) (int, ErrorCode) {
	if len(src) > dstCapacity {
		return 0, ErrDstSizeTooSmall
	}
	return copy(dst[op:op+len(src)], src), 0
}

// ZSTD_setRleBlock
func setRleBlock(dst []byte, op, dstCapacity int, b byte, regenSize uint64) (int, ErrorCode) {
	if regenSize > uint64(dstCapacity) {
		return 0, ErrDstSizeTooSmall
	}
	for k := op; k < op+int(regenSize); k++ {
		dst[k] = b
	}
	return int(regenSize), 0
}

// ZSTD_decompressFrame of one frame into dst, the bytes decoded and the bytes of src consumed
func (dc *DCtx) decompressFrame(dst cptr, dstCapacity int, src []byte) (int, int, ErrorCode) {
	ob := dst.r.b
	ostart, op := dst.i, dst.i
	oend := ostart + dstCapacity
	remaining := len(src)
	if remaining < frameHeaderSizeMin+blockHeaderSize {
		return 0, 0, ErrSrcSizeWrong
	}
	fhs := frameHeaderSizeInternal(src)
	if remaining < fhs+blockHeaderSize {
		return 0, 0, ErrSrcSizeWrong
	}
	if err := dc.decodeFrameHeader(src[:fhs]); err != 0 {
		return 0, 0, err
	}
	ip := fhs
	remaining -= fhs
	for {
		last, blockType, origSize, cBlockSize, err := getcBlockSize(src[ip:])
		if err != 0 {
			return 0, 0, err
		}
		ip += blockHeaderSize
		remaining -= blockHeaderSize
		if cBlockSize > remaining {
			return 0, 0, ErrSrcSizeWrong
		}
		var decoded int
		block := src[ip : ip+cBlockSize]
		switch blockType {
		case btCompressed:
			decoded, err = dc.decompressBlockInternal(cptr{dst.r, op}, oend-op, block, false)
		case btRaw:
			decoded, err = copyRawBlock(ob, op, oend-op, block)
		default:
			decoded, err = setRleBlock(ob, op, oend-op, block[0], origSize)
		}
		if err != 0 {
			return 0, 0, err
		}
		if dc.validateChecksum {
			dc.xxh.Write(ob[op : op+decoded])
		}
		op += decoded
		ip += cBlockSize
		remaining -= cBlockSize
		if last {
			break
		}
	}
	if dc.fParams.frameContentSize != contentSizeUnknown && uint64(op-ostart) != dc.fParams.frameContentSize {
		return 0, 0, ErrCorruptionDetected
	}
	if dc.fParams.checksumFlag {
		if remaining < 4 {
			return 0, 0, ErrChecksumWrong
		}
		if binary.LittleEndian.Uint32(src[ip:]) != uint32(dc.xxh.Sum64()) {
			return 0, 0, ErrChecksumWrong
		}
		ip += 4
	}
	return op - ostart, ip, 0
}

// ZSTD_decompressMultiFrame, every frame of src into dst with dict or ddict, the bytes decoded
func (dc *DCtx) decompressMultiFrame(dst cptr, dstCapacity int, src, dict []byte, ddict *DDict) (int, ErrorCode) {
	if ddict != nil {
		dict = ddict.content.b
	}
	dststart := dst.i
	moreThan1Frame := false
	for len(src) >= frameHeaderSizePrefix {
		if IsLegacy(src) != 0 {
			frameSize, _, err := FindFrameSizeInfoLegacy(src)
			if err != 0 {
				return 0, err
			}
			decoded, err := DecompressLegacy(dst.r.b[dst.i:dst.i+dstCapacity], src[:frameSize], dict)
			if err != 0 {
				return 0, err
			}
			if expected := GetDecompressedSizeLegacy(src); expected != 0 && expected != uint64(decoded) {
				return 0, ErrCorruptionDetected
			}
			dst.i += decoded
			dstCapacity -= decoded
			src = src[frameSize:]
			continue
		}
		if len(src) >= 4 && binary.LittleEndian.Uint32(src)&magicSkippableMask == magicSkippableStart {
			size, err := readSkippableFrameSize(src)
			if err != 0 {
				return 0, err
			}
			src = src[size:]
			continue
		}
		if ddict != nil {
			dc.decompressBeginUsingDDict(ddict)
		} else if err := dc.decompressBeginUsingDict(dict); err != 0 {
			return 0, err
		}
		dc.checkContinuity(dst, dstCapacity)
		decoded, consumed, err := dc.decompressFrame(dst, dstCapacity, src)
		if err == ErrPrefixUnknown && moreThan1Frame {
			return 0, ErrSrcSizeWrong
		}
		if err != 0 {
			return 0, err
		}
		dst.i += decoded
		dstCapacity -= decoded
		src = src[consumed:]
		moreThan1Frame = true
	}
	if len(src) != 0 {
		return 0, ErrSrcSizeWrong
	}
	return dst.i - dststart, 0
}

// Decompress is ZSTD_decompressDCtx of src into dst with the referenced dictionary, the size decoded
func (dc *DCtx) Decompress(dst, src []byte) (int, ErrorCode) {
	return dc.decompressMultiFrame(cptr{&region{dst}, 0}, len(dst), src, nil, dc.ddict)
}

// DecompressUsingDict is ZSTD_decompress_usingDict of src into dst, the size decoded
func (dc *DCtx) DecompressUsingDict(dst, src, dict []byte) (int, ErrorCode) {
	return dc.decompressMultiFrame(cptr{&region{dst}, 0}, len(dst), src, dict, nil)
}

// DecompressUsingDDict is ZSTD_decompress_usingDDict of src into dst, the size decoded
func (dc *DCtx) DecompressUsingDDict(dst, src []byte, d *DDict) (int, ErrorCode) {
	return dc.decompressMultiFrame(cptr{&region{dst}, 0}, len(dst), src, nil, d)
}

// ZSTD_nextSrcSizeToDecompressWithInputSize, raw blocks streamed as far as the input reaches
func (dc *DCtx) nextSrcSize(inputSize uint64) uint64 {
	if (dc.stage != dsDecompressBlock && dc.stage != dsDecompressLastBlock) || dc.bType != btRaw {
		return dc.expected
	}
	return max(1, min(inputSize, dc.expected))
}

// ZSTD_decompressContinue for the stages the streaming decoder reaches, the size decoded into dst
func (dc *DCtx) decompressContinue(dst cptr, dstCapacity int, src []byte, srcSize uint64) (int, ErrorCode) {
	if srcSize != dc.nextSrcSize(srcSize) {
		return 0, ErrSrcSizeWrong
	}
	dc.checkContinuity(dst, dstCapacity)
	switch dc.stage {
	case dsDecodeBlockHeader:
		last, blockType, origSize, cSize, err := getcBlockSize(src)
		if err != 0 {
			return 0, err
		}
		if uint64(cSize) > dc.fParams.blockSizeMax {
			return 0, ErrCorruptionDetected
		}
		dc.expected, dc.bType, dc.rleSize = uint64(cSize), blockType, origSize
		if cSize > 0 {
			dc.stage = dsDecompressBlock
			if last {
				dc.stage = dsDecompressLastBlock
			}
			return 0, 0
		}
		dc.endBlock(last)
		return 0, 0
	case dsDecompressBlock, dsDecompressLastBlock:
		var rSize int
		var err ErrorCode
		switch dc.bType {
		case btCompressed:
			rSize, err = dc.decompressBlockInternal(dst, dstCapacity, src, true)
			dc.expected = 0
		case btRaw:
			rSize, err = copyRawBlock(dst.r.b, dst.i, dstCapacity, src)
			if err != 0 {
				return 0, err
			}
			dc.expected -= uint64(rSize)
		case btRLE:
			rSize, err = setRleBlock(dst.r.b, dst.i, dstCapacity, src[0], dc.rleSize)
			dc.expected = 0
		default:
			return 0, ErrCorruptionDetected
		}
		if err != 0 {
			return 0, err
		}
		if uint64(rSize) > dc.fParams.blockSizeMax {
			return 0, ErrCorruptionDetected
		}
		dc.decodedSize += uint64(rSize)
		if dc.validateChecksum {
			dc.xxh.Write(dst.r.b[dst.i : dst.i+rSize])
		}
		dc.previousDstEnd = cptr{dst.r, dst.i + rSize}
		if dc.expected > 0 {
			return rSize, 0
		}
		if dc.stage == dsDecompressLastBlock {
			if dc.fParams.frameContentSize != contentSizeUnknown && dc.decodedSize != dc.fParams.frameContentSize {
				return 0, ErrCorruptionDetected
			}
			dc.endBlock(true)
			return rSize, 0
		}
		dc.endBlock(false)
		return rSize, 0
	case dsCheckChecksum:
		if dc.validateChecksum && binary.LittleEndian.Uint32(src) != uint32(dc.xxh.Sum64()) {
			return 0, ErrChecksumWrong
		}
		dc.expected, dc.stage = 0, dsGetFrameHeaderSize
		return 0, 0
	case dsSkipFrame:
		dc.expected, dc.stage = 0, dsGetFrameHeaderSize
		return 0, 0
	}
	return 0, ErrGeneric
}

// Next unit after a block, the checksum or frame end after the last one and a block header otherwise
func (dc *DCtx) endBlock(last bool) {
	switch {
	case !last:
		dc.expected, dc.stage = blockHeaderSize, dsDecodeBlockHeader
	case dc.fParams.checksumFlag:
		dc.expected, dc.stage = 4, dsCheckChecksum
	default:
		dc.expected, dc.stage = 0, dsGetFrameHeaderSize
	}
}

// ZSTD_decompressContinueStream decoding into the buffered output and moving to flush
func (dc *DCtx) decompressContinueStream(src []byte, srcSize uint64) ErrorCode {
	isSkip := dc.stage == dsSkipFrame
	dstSize := 0
	if !isSkip {
		dstSize = int(dc.outBuffSize - dc.outStart)
	}
	decoded, err := dc.decompressContinue(cptr{dc.outBuff, int(dc.outStart)}, dstSize, src, srcSize)
	if err != 0 {
		return err
	}
	if decoded == 0 && !isSkip {
		dc.streamStage = zdssRead
		return 0
	}
	dc.outEnd = dc.outStart + uint64(decoded)
	dc.streamStage = zdssFlush
	return 0
}

// ZSTD_decodingBufferSize_internal, the ring buffer the frame needs
func decodingBufferSize(windowSize, frameContentSize, blockSizeMax uint64) uint64 {
	blockSize := min(windowSize, blockSizeMaxAbs, blockSizeMax)
	return min(frameContentSize, windowSize+blockSize*2+wildcopyOverlength*2)
}

// Buffer bookkeeping of zdss_loadHeader, kept across frames and shrunk after a long oversized run
func (dc *DCtx) sizeBuffers() {
	neededIn := max(dc.fParams.blockSizeMax, 4)
	neededOut := decodingBufferSize(dc.fParams.windowSize, dc.fParams.frameContentSize, dc.fParams.blockSizeMax)
	if dc.inBuffSize+dc.outBuffSize >= (neededIn+neededOut)*workspaceTooLargeFactor {
		dc.oversizedDuration++
	} else {
		dc.oversizedDuration = 0
	}
	if dc.inBuffSize < neededIn || dc.outBuffSize < neededOut || dc.oversizedDuration >= workspaceTooLargeMaxDuration {
		dc.inBuffSize, dc.outBuffSize = neededIn, neededOut
		dc.inBuff = make([]byte, neededIn)
		dc.outBuff = &region{b: make([]byte, neededOut)}
	}
}

// Runs the legacy stream decoder of the version in hand, returning to zdss_init when its frame ended
func (dc *DCtx) legacyStream(version uint32, out, in []byte, inPos int) (int, int, uint64, ErrorCode) {
	written, read, hint, err := dc.legacyContext.Decompress(version, out, in[inPos:])
	if err == 0 && hint == 0 {
		dc.streamStage = zdssInit
	}
	return written, inPos + read, hint, err
}

// DecompressStream is ZSTD_decompressStream over out and in from inPos, the bytes written, input position and hint
func (dc *DCtx) DecompressStream(out, in []byte, inPos int) (int, int, uint64, ErrorCode) {
	if inPos > len(in) {
		return 0, 0, 0, ErrSrcSizeWrong
	}
	istart, ip, iend := inPos, inPos, len(in)
	op, oend := 0, len(out)
	for someMoreWork := true; someMoreWork; {
		switch dc.streamStage {
		case zdssInit:
			dc.streamStage = zdssLoadHeader
			dc.lhSize, dc.inPos, dc.outStart, dc.outEnd = 0, 0, 0, 0
			dc.legacyVersion = 0
			dc.hostageByte = false
			fallthrough
		case zdssLoadHeader:
			if dc.legacyVersion != 0 {
				return dc.legacyStream(dc.legacyVersion, out, in, inPos)
			}
			hSize, err := getFrameHeader(&dc.fParams, dc.headerBuffer[:dc.lhSize])
			if err != 0 {
				version := IsLegacy(in[istart:iend])
				if version == 0 {
					return 0, 0, 0, err
				}
				dc.legacyContext = InitLegacyStream(dc.legacyContext, dc.previousLegacyVersion, version, dc.legacyDict())
				dc.legacyVersion, dc.previousLegacyVersion = version, version
				return dc.legacyStream(version, out, in, inPos)
			}
			if hSize != 0 {
				toLoad := hSize - dc.lhSize
				if toLoad > iend-ip {
					dc.lhSize += copy(dc.headerBuffer[dc.lhSize:], in[ip:iend])
					if _, err := getFrameHeader(&dc.fParams, dc.headerBuffer[:dc.lhSize]); err != 0 {
						return 0, 0, 0, err
					}
					return 0, iend, uint64(max(frameHeaderSizeMin, hSize)-dc.lhSize) + blockHeaderSize, 0
				}
				copy(dc.headerBuffer[dc.lhSize:], in[ip:ip+toLoad])
				dc.lhSize = hSize
				ip += toLoad
				continue
			}
			if dc.fParams.frameContentSize != contentSizeUnknown && !dc.fParams.skippable && uint64(oend-op) >= dc.fParams.frameContentSize {
				cSize, err := FindFrameCompressedSize(in[istart:iend])
				if err == 0 && cSize <= iend-istart {
					decoded, err := dc.decompressMultiFrame(cptr{&region{out[op:oend]}, 0}, oend-op, in[istart:istart+cSize], nil, dc.ddict)
					if err != 0 {
						return 0, 0, 0, err
					}
					op += decoded
					ip = istart + cSize
					dc.expected = 0
					dc.streamStage = zdssInit
					someMoreWork = false
					continue
				}
			}
			dc.decompressBeginUsingDDict(dc.ddict)
			if binary.LittleEndian.Uint32(dc.headerBuffer[:])&magicSkippableMask == magicSkippableStart {
				dc.expected = uint64(binary.LittleEndian.Uint32(dc.headerBuffer[4:]))
				dc.stage = dsSkipFrame
			} else {
				if err := dc.decodeFrameHeader(dc.headerBuffer[:dc.lhSize]); err != 0 {
					return 0, 0, 0, err
				}
				dc.expected = blockHeaderSize
				dc.stage = dsDecodeBlockHeader
			}
			dc.fParams.windowSize = max(dc.fParams.windowSize, 1<<windowLogAbsoluteMin)
			if dc.fParams.windowSize > dc.maxWindowSize {
				return 0, 0, 0, ErrWindowTooLarge
			}
			dc.sizeBuffers()
			dc.streamStage = zdssRead
			fallthrough
		case zdssRead:
			neededInSize := dc.nextSrcSize(uint64(iend - ip))
			if neededInSize == 0 {
				dc.streamStage = zdssInit
				someMoreWork = false
				continue
			}
			if uint64(iend-ip) >= neededInSize {
				end := ip + int(neededInSize)
				if err := dc.decompressContinueStream(in[ip:end], neededInSize); err != 0 {
					return 0, 0, 0, err
				}
				ip = end
				continue
			}
			if ip == iend {
				someMoreWork = false
				continue
			}
			dc.streamStage = zdssLoad
			fallthrough
		case zdssLoad:
			neededInSize := dc.expected
			toLoad := neededInSize - dc.inPos
			isSkip := dc.stage == dsSkipFrame
			if !isSkip && toLoad > dc.inBuffSize-dc.inPos {
				return 0, 0, 0, ErrCorruptionDetected
			}
			loaded := min(toLoad, uint64(iend-ip))
			if !isSkip {
				copy(dc.inBuff[dc.inPos:], in[ip:ip+int(loaded)])
			}
			ip += int(loaded)
			dc.inPos += loaded
			if loaded < toLoad {
				someMoreWork = false
				continue
			}
			dc.inPos = 0
			var src []byte
			if !isSkip {
				src = dc.inBuff[:neededInSize]
			}
			if err := dc.decompressContinueStream(src, neededInSize); err != 0 {
				return 0, 0, 0, err
			}
		case zdssFlush:
			toFlush := dc.outEnd - dc.outStart
			flushed := min(uint64(oend-op), toFlush)
			op += copy(out[op:], dc.outBuff.b[dc.outStart:dc.outStart+flushed])
			dc.outStart += flushed
			if flushed == toFlush {
				dc.streamStage = zdssRead
				if dc.outBuffSize < dc.fParams.frameContentSize && dc.outStart+dc.fParams.blockSizeMax > dc.outBuffSize {
					dc.outStart, dc.outEnd = 0, 0
				}
				continue
			}
			someMoreWork = false
		}
	}
	if ip == istart && op == 0 {
		dc.noForwardProgress++
		if dc.noForwardProgress >= noForwardProgressMax {
			if op == oend {
				return 0, 0, 0, ErrNoForwardProgressDestFull
			}
			if ip == iend {
				return 0, 0, 0, ErrNoForwardProgressInputEmpty
			}
		}
	} else {
		dc.noForwardProgress = 0
	}
	hint := dc.expected
	if hint == 0 {
		if dc.outEnd == dc.outStart {
			if dc.hostageByte {
				if ip >= iend {
					dc.streamStage = zdssRead
					return op, ip, 1, 0
				}
				ip++
			}
			return op, ip, 0, 0
		}
		if !dc.hostageByte {
			ip--
			dc.hostageByte = true
		}
		return op, ip, 1, 0
	}
	if dc.stage == dsDecompressBlock {
		hint += blockHeaderSize
	}
	return op, ip, hint - dc.inPos, 0
}
