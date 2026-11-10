package pyzip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"iter"
	"math"
	"math/bits"

	"github.com/cespare/xxhash/v2"
	"github.com/klauspost/compress/zstd"
)

const (
	zstdMagicNumber          = 0xFD2FB528
	zstdMagicSkippableStart  = 0x184D2A50
	zstdMagicSkippableMask   = 0xFFFFFFF0
	zstdLegacyV05            = 0xFD2FB525
	zstdLegacyV06            = 0xFD2FB526
	zstdLegacyV07            = 0xFD2FB527
	zstdSkippableHeaderSize  = 8
	zstdFrameHeaderPrefix    = 5
	zstdFrameHeaderMin       = 6
	zstdFrameHeaderMax       = 18
	zstdBlockHeaderSize      = 3
	zstdBlockSizeMax         = 1 << 17
	zstdWindowLogMin         = 10
	zstdWindowLogMax         = 31
	zstdMaxWindowSizeDefault = 1<<27 + 1
	zstdContentSizeUnknown   = math.MaxUint64
	zstdNoProgressMax        = 16
	zstdWildcopyOverlength   = 32
	zstdTooLargeFactor       = 3
	zstdTooLargeMaxDuration  = 128
)

// libzstd ZSTD_getErrorName texts
type zstdErr string

func (e zstdErr) Error() string { return string(e) }

const (
	zstdErrGeneric          zstdErr = "Error (generic)"
	zstdErrPrefixUnknown    zstdErr = "Unknown frame descriptor"
	zstdErrFrameUnsupported zstdErr = "Unsupported frame parameter"
	zstdErrWindowTooLarge   zstdErr = "Frame requires too much memory for decoding"
	zstdErrCorruption       zstdErr = "Data corruption detected"
	zstdErrChecksumWrong    zstdErr = "Restored data doesn't match checksum"
	zstdErrDictionaryWrong  zstdErr = "Dictionary mismatch"
	zstdErrSrcSizeWrong     zstdErr = "Src size is incorrect"
	zstdErrDstSizeTooSmall  zstdErr = "Destination buffer is too small"
	zstdErrNoProgressFull   zstdErr = "Operation made no progress over multiple calls, due to output buffer being full"
	zstdErrNoProgressEmpty  zstdErr = "Operation made no progress over multiple calls, due to input being empty"
)

// ZSTD_dStage values of the frame decoder
const (
	stageGetFrameHeaderSize = iota
	stageDecodeBlockHeader
	stageDecompressBlock
	stageDecompressLastBlock
	stageCheckChecksum
	stageSkipFrame
)

// ZSTD_dStreamStage values of the streaming decoder
const (
	zdssInit = iota
	zdssLoadHeader
	zdssRead
	zdssLoad
	zdssFlush
)

// blockType_e values
const (
	btRaw = iota
	btRLE
	btCompressed
	btReserved
)

var errCoroutineStopped = errors.New("pyzip decoder ended")

// libzstd ZSTD_getErrorName text of a klauspost block decoding failure
func zstdErrorName(err error) string {
	switch {
	case errors.Is(err, zstd.ErrMagicMismatch):
		return "Unknown frame descriptor"
	case errors.Is(err, zstd.ErrCRCMismatch):
		return "Restored data doesn't match checksum"
	case errors.Is(err, zstd.ErrWindowSizeExceeded), errors.Is(err, zstd.ErrDecoderSizeExceeded):
		return "Frame requires too much memory for decoding"
	case errors.Is(err, zstd.ErrUnknownDictionary):
		return "Dictionary mismatch"
	case errors.Is(err, zstd.ErrWindowSizeTooSmall):
		return "Unsupported frame parameter"
	}
	return "Data corruption detected"
}

// ZSTD_FrameHeader fields the decoder reads
type zstdFrameHeader struct {
	frameContentSize uint64
	windowSize       uint64
	blockSizeMax     uint64
	skippable        bool
	headerSize       int
	dictID           uint32
	checksumFlag     bool
}

// ZSTD_isLegacy of a library built with ZSTD_LEGACY_SUPPORT 5, the legacy version or zero
func zstdIsLegacy(src []byte) uint32 {
	if len(src) < 4 {
		return 0
	}
	switch binary.LittleEndian.Uint32(src) {
	case zstdLegacyV05:
		return 5
	case zstdLegacyV06:
		return 6
	case zstdLegacyV07:
		return 7
	}
	return 0
}

// ZSTD_frameHeaderSize_internal from the frame header descriptor, src holding at least the prefix
func zstdFrameHeaderSize(src []byte) int {
	fhd := src[zstdFrameHeaderPrefix-1]
	single := int(fhd>>5) & 1
	size := zstdFrameHeaderPrefix + 1 - single + [4]int{0, 1, 2, 4}[fhd&3] + [4]int{0, 2, 4, 8}[fhd>>6]
	if single == 1 && fhd>>6 == 0 {
		size++
	}
	return size
}

// ZSTD_getFrameHeader_advanced, zero once h is filled, otherwise the input size it still needs
func zstdGetFrameHeader(h *zstdFrameHeader, src []byte) (int, error) {
	le := binary.LittleEndian
	if len(src) < zstdFrameHeaderPrefix {
		if len(src) > 0 {
			hbuf := le.AppendUint32(nil, zstdMagicNumber)
			copy(hbuf, src[:min(4, len(src))])
			if le.Uint32(hbuf) != zstdMagicNumber {
				le.PutUint32(hbuf, zstdMagicSkippableStart)
				copy(hbuf, src[:min(4, len(src))])
				if le.Uint32(hbuf)&zstdMagicSkippableMask != zstdMagicSkippableStart {
					return 0, zstdErrPrefixUnknown
				}
			}
		}
		return zstdFrameHeaderPrefix, nil
	}
	*h = zstdFrameHeader{}
	if magic := le.Uint32(src); magic != zstdMagicNumber {
		if magic&zstdMagicSkippableMask != zstdMagicSkippableStart {
			return 0, zstdErrPrefixUnknown
		}
		if len(src) < zstdSkippableHeaderSize {
			return zstdSkippableHeaderSize, nil
		}
		*h = zstdFrameHeader{skippable: true, dictID: magic - zstdMagicSkippableStart, headerSize: zstdSkippableHeaderSize,
			frameContentSize: uint64(le.Uint32(src[4:]))}
		return 0, nil
	}
	fhsize := zstdFrameHeaderSize(src)
	if len(src) < fhsize {
		return fhsize, nil
	}
	h.headerSize = fhsize
	fhd := src[zstdFrameHeaderPrefix-1]
	pos := zstdFrameHeaderPrefix
	single := fhd>>5&1 == 1
	if fhd&0x08 != 0 {
		return 0, zstdErrFrameUnsupported
	}
	if !single {
		wl := src[pos]
		pos++
		windowLog := uint(wl>>3) + zstdWindowLogMin
		if windowLog > zstdWindowLogMax {
			return 0, zstdErrWindowTooLarge
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
	h.frameContentSize = zstdContentSizeUnknown
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
	h.blockSizeMax = min(h.windowSize, zstdBlockSizeMax)
	h.checksumFlag = fhd>>2&1 == 1
	return 0, nil
}

// ZSTD_getcBlockSize, the last block flag, block type and size field with the compressed size
func zstdGetcBlockSize(src []byte) (last bool, blockType int, origSize uint64, cSize uint64, err error) {
	if len(src) < zstdBlockHeaderSize {
		return false, 0, 0, 0, zstdErrSrcSizeWrong
	}
	header := uint32(src[0]) | uint32(src[1])<<8 | uint32(src[2])<<16
	size := uint64(header >> 3)
	last, blockType = header&1 == 1, int(header>>1&3)
	if blockType == btRLE {
		return last, blockType, size, 1, nil
	}
	if blockType == btReserved {
		return last, blockType, size, 0, zstdErrCorruption
	}
	return last, blockType, size, size, nil
}

// readSkippableFrameSize, the whole size of the skippable frame starting src
func zstdReadSkippableFrameSize(src []byte) (uint64, error) {
	if len(src) < zstdSkippableHeaderSize {
		return 0, zstdErrSrcSizeWrong
	}
	sizeU32 := binary.LittleEndian.Uint32(src[4:])
	if sizeU32+zstdSkippableHeaderSize < sizeU32 {
		return 0, zstdErrFrameUnsupported
	}
	size := zstdSkippableHeaderSize + uint64(sizeU32)
	if size > uint64(len(src)) {
		return 0, zstdErrSrcSizeWrong
	}
	return size, nil
}

// ZSTD_findFrameCompressedSize_advanced, the compressed size of the frame starting src
func zstdFindFrameCompressedSize(src []byte) (uint64, error) {
	if zstdIsLegacy(src) != 0 {
		return zstdFindFrameCompressedSizeLegacy(src)
	}
	if len(src) >= zstdSkippableHeaderSize && binary.LittleEndian.Uint32(src)&zstdMagicSkippableMask == zstdMagicSkippableStart {
		return zstdReadSkippableFrameSize(src)
	}
	var h zstdFrameHeader
	need, err := zstdGetFrameHeader(&h, src)
	if err != nil {
		return 0, err
	}
	if need > 0 {
		return 0, zstdErrSrcSizeWrong
	}
	ip := uint64(h.headerSize)
	remaining := uint64(len(src)) - ip
	for {
		last, _, _, cSize, err := zstdGetcBlockSize(src[ip:])
		if err != nil {
			return 0, err
		}
		if zstdBlockHeaderSize+cSize > remaining {
			return 0, zstdErrSrcSizeWrong
		}
		ip += zstdBlockHeaderSize + cSize
		remaining -= zstdBlockHeaderSize + cSize
		if last {
			break
		}
	}
	if h.checksumFlag {
		if remaining < 4 {
			return 0, zstdErrSrcSizeWrong
		}
		ip += 4
	}
	return ip, nil
}

// Block payload event of the klauspost coroutine
type zstdEvent struct {
	out     []byte
	err     error
	starved bool
}

// klauspost decoder fed one frame block by block, suspended whenever it waits for the next block
type zstdBlocks struct {
	queue []byte
	skip  int
	next  func() (zstdEvent, bool)
	stop  func()
	yield func(zstdEvent) bool
}

// Session whose synthesized frame header carries only a window covering the frame
func newZstdBlocks(window uint64) *zstdBlocks {
	windowLog := max(zstdWindowLogMin, bits.Len64(window-1))
	header := binary.LittleEndian.AppendUint32(nil, zstdMagicNumber)
	return &zstdBlocks{queue: append(header, 0, byte(windowLog-zstdWindowLogMin)<<3)}
}

// Queues raw or RLE block bytes whose produced output was already delivered
func (b *zstdBlocks) add(data []byte, produced uint64) {
	b.queue = append(b.queue, data...)
	b.skip += int(produced)
}

// Hands klauspost the queued bytes, suspending while the queue is empty
func (b *zstdBlocks) Read(p []byte) (int, error) {
	for len(b.queue) == 0 {
		if !b.yield(zstdEvent{starved: true}) {
			panic(errCoroutineStopped)
		}
	}
	n := copy(p, b.queue)
	b.queue = b.queue[n:]
	return n, nil
}

// Coroutine body reading the synthesized frame through a synchronous klauspost decoder
func (b *zstdBlocks) run(yield func(zstdEvent) bool) {
	b.yield = yield
	defer func() {
		if r := recover(); r != nil && r != errCoroutineStopped {
			panic(r)
		}
	}()
	d, err := zstd.NewReader(b, zstd.WithDecoderConcurrency(1))
	if err != nil {
		yield(zstdEvent{err: err})
		return
	}
	defer d.Close()
	buf := make([]byte, zstdBlockSizeMax)
	for {
		n, err := d.Read(buf)
		if n > 0 && !yield(zstdEvent{out: buf[:n]}) {
			return
		}
		if err != nil {
			yield(zstdEvent{err: err})
			return
		}
	}
}

// Decodes the compressed block with its header already queued, returning only its own output
func (b *zstdBlocks) decompress(content []byte) ([]byte, error) {
	b.queue = append(b.queue, content...)
	if b.next == nil {
		b.next, b.stop = iter.Pull(b.run)
	}
	var out []byte
	for ev, ok := b.next(); ok && !ev.starved; ev, ok = b.next() {
		if ev.err != nil {
			b.end()
			return nil, zstdErr(zstdErrorName(ev.err))
		}
		o := ev.out
		if b.skip > 0 {
			k := min(b.skip, len(o))
			b.skip -= k
			o = o[k:]
		}
		out = append(out, o...)
	}
	return out, nil
}

// Releases the suspended klauspost decoder
func (b *zstdBlocks) end() {
	if b != nil && b.stop != nil {
		b.stop()
		b.stop, b.next = nil, nil
	}
}

// ZSTD_DCtx and ZSTD_DStream state of libzstd 1.5.7 decoding into its buffered output
type zstdDStream struct {
	stage            int
	expected         uint64
	bType            int
	rleSize          uint64
	decodedSize      uint64
	fParams          zstdFrameHeader
	validateChecksum bool
	xxh              *xxhash.Digest
	blocks           *zstdBlocks

	streamStage           int
	headerBuffer          [zstdFrameHeaderMax]byte
	lhSize                int
	inBuff                []byte
	inBuffSize            uint64
	inPos                 uint64
	outBuffSize           uint64
	outStart, outEnd      uint64
	outData               []byte
	hostageByte           bool
	noForwardProgress     int
	oversizedDuration     int
	maxWindowSize         uint64
	legacyVersion         uint32
	previousLegacyVersion uint32
}

func newZstdDStream() *zstdDStream {
	return &zstdDStream{streamStage: zdssInit, maxWindowSize: zstdMaxWindowSizeDefault}
}

// ZSTD_decompressBegin resetting the frame decoder for a new frame
func (z *zstdDStream) decompressBegin() {
	z.expected = zstdFrameHeaderPrefix
	z.stage = stageGetFrameHeaderSize
	z.decodedSize = 0
	z.bType = btReserved
	z.blocks.end()
	z.blocks = nil
}

// ZSTD_decodeFrameHeader with no dictionary loaded
func (z *zstdDStream) decodeFrameHeader(src []byte) error {
	need, err := zstdGetFrameHeader(&z.fParams, src)
	if err != nil {
		return err
	}
	if need > 0 {
		return zstdErrSrcSizeWrong
	}
	if z.fParams.dictID != 0 {
		return zstdErrDictionaryWrong
	}
	z.validateChecksum = z.fParams.checksumFlag
	if z.validateChecksum {
		z.xxh = xxhash.New()
	}
	return nil
}

// ZSTD_nextSrcSizeToDecompressWithInputSize, raw blocks streamed as far as the input reaches
func (z *zstdDStream) nextSrcSize(inputSize uint64) uint64 {
	if (z.stage != stageDecompressBlock && z.stage != stageDecompressLastBlock) || z.bType != btRaw {
		return z.expected
	}
	return max(1, min(inputSize, z.expected))
}

// ZSTD_decompressContinue for the stages the streaming decoder reaches, returning the bytes decoded
func (z *zstdDStream) decompressContinue(dstCapacity uint64, src []byte, srcSize uint64) ([]byte, error) {
	if srcSize != z.nextSrcSize(srcSize) {
		return nil, zstdErrSrcSizeWrong
	}
	switch z.stage {
	case stageDecodeBlockHeader:
		last, blockType, origSize, cSize, err := zstdGetcBlockSize(src)
		if err != nil {
			return nil, err
		}
		if cSize > z.fParams.blockSizeMax {
			return nil, zstdErrCorruption
		}
		z.expected, z.bType, z.rleSize = cSize, blockType, origSize
		if cSize > 0 {
			z.stage = stageDecompressBlock
			if last {
				z.stage = stageDecompressLastBlock
			}
			z.blocks.add(src[:zstdBlockHeaderSize], 0)
			return nil, nil
		}
		z.endBlock(last)
		return nil, nil
	case stageDecompressBlock, stageDecompressLastBlock:
		var r []byte
		switch z.bType {
		case btCompressed:
			out, err := z.blocks.decompress(src)
			if err != nil {
				return nil, err
			}
			if uint64(len(out)) > dstCapacity {
				return nil, zstdErrDstSizeTooSmall
			}
			r = out
			z.expected = 0
		case btRaw:
			if srcSize > dstCapacity {
				return nil, zstdErrDstSizeTooSmall
			}
			r = append([]byte(nil), src...)
			z.blocks.add(src, srcSize)
			z.expected -= srcSize
		case btRLE:
			if z.rleSize > dstCapacity {
				return nil, zstdErrDstSizeTooSmall
			}
			r = bytes.Repeat(src[:1], int(z.rleSize))
			z.blocks.add(src[:1], z.rleSize)
			z.expected = 0
		}
		if uint64(len(r)) > z.fParams.blockSizeMax {
			return nil, zstdErrCorruption
		}
		z.decodedSize += uint64(len(r))
		if z.validateChecksum {
			z.xxh.Write(r)
		}
		if z.expected > 0 {
			return r, nil
		}
		if z.stage == stageDecompressLastBlock {
			if z.fParams.frameContentSize != zstdContentSizeUnknown && z.decodedSize != z.fParams.frameContentSize {
				return nil, zstdErrCorruption
			}
			z.endBlock(true)
			return r, nil
		}
		z.endBlock(false)
		return r, nil
	case stageCheckChecksum:
		if z.validateChecksum && binary.LittleEndian.Uint32(src) != uint32(z.xxh.Sum64()) {
			return nil, zstdErrChecksumWrong
		}
		z.expected, z.stage = 0, stageGetFrameHeaderSize
		return nil, nil
	case stageSkipFrame:
		z.expected, z.stage = 0, stageGetFrameHeaderSize
		return nil, nil
	}
	return nil, zstdErrGeneric
}

// Next unit after a block, the checksum or frame end after the last one and a block header otherwise
func (z *zstdDStream) endBlock(last bool) {
	switch {
	case !last:
		z.expected, z.stage = zstdBlockHeaderSize, stageDecodeBlockHeader
	case z.fParams.checksumFlag:
		z.expected, z.stage = 4, stageCheckChecksum
	default:
		z.expected, z.stage = 0, stageGetFrameHeaderSize
	}
}

// ZSTD_decompressContinueStream decoding into the buffered output and moving to flush
func (z *zstdDStream) decompressContinueStream(src []byte, srcSize uint64) error {
	isSkip := z.stage == stageSkipFrame
	dstSize := uint64(0)
	if !isSkip {
		dstSize = z.outBuffSize - z.outStart
	}
	decoded, err := z.decompressContinue(dstSize, src, srcSize)
	if err != nil {
		return err
	}
	if len(decoded) == 0 && !isSkip {
		z.streamStage = zdssRead
		return nil
	}
	z.outEnd = z.outStart + uint64(len(decoded))
	z.outData = decoded
	z.streamStage = zdssFlush
	return nil
}

// ZSTD_decodingBufferSize_internal, the ring buffer the frame needs
func zstdDecodingBufferSize(windowSize, frameContentSize, blockSizeMax uint64) uint64 {
	blockSize := min(windowSize, zstdBlockSizeMax, blockSizeMax)
	return min(frameContentSize, windowSize+blockSize*2+zstdWildcopyOverlength*2)
}

// ZSTD_decompressFrame with compressed blocks decoded by klauspost, returning output and bytes consumed
func (z *zstdDStream) decompressFrame(dstCapacity uint64, src []byte) ([]byte, uint64, error) {
	if len(src) < zstdFrameHeaderMin+zstdBlockHeaderSize {
		return nil, 0, zstdErrSrcSizeWrong
	}
	fhs := zstdFrameHeaderSize(src)
	if len(src) < fhs+zstdBlockHeaderSize {
		return nil, 0, zstdErrSrcSizeWrong
	}
	if err := z.decodeFrameHeader(src[:fhs]); err != nil {
		return nil, 0, err
	}
	z.blocks = newZstdBlocks(max(1<<zstdWindowLogMin, min(z.fParams.windowSize, z.fParams.frameContentSize)))
	ip, remaining := uint64(fhs), uint64(len(src)-fhs)
	var dst []byte
	for {
		last, blockType, origSize, cSize, err := zstdGetcBlockSize(src[ip:])
		if err != nil {
			return nil, 0, err
		}
		ip += zstdBlockHeaderSize
		remaining -= zstdBlockHeaderSize
		if cSize > remaining {
			return nil, 0, zstdErrSrcSizeWrong
		}
		room := dstCapacity - uint64(len(dst))
		header, content := src[ip-zstdBlockHeaderSize:ip], src[ip:ip+cSize]
		var r []byte
		switch blockType {
		case btCompressed:
			if cSize > z.fParams.blockSizeMax {
				return nil, 0, zstdErrSrcSizeWrong
			}
			z.blocks.add(header, 0)
			if r, err = z.blocks.decompress(content); err != nil {
				return nil, 0, err
			}
			if uint64(len(r)) > room {
				return nil, 0, zstdErrDstSizeTooSmall
			}
		case btRaw:
			if cSize > room {
				return nil, 0, zstdErrDstSizeTooSmall
			}
			r = content
			z.blocks.add(header, 0)
			z.blocks.add(content, cSize)
		case btRLE:
			if origSize > room {
				return nil, 0, zstdErrDstSizeTooSmall
			}
			r = bytes.Repeat(content, int(origSize))
			z.blocks.add(header, 0)
			z.blocks.add(content, origSize)
		}
		if z.validateChecksum {
			z.xxh.Write(r)
		}
		dst = append(dst, r...)
		ip += cSize
		remaining -= cSize
		if last {
			break
		}
	}
	z.blocks.end()
	if z.fParams.frameContentSize != zstdContentSizeUnknown && uint64(len(dst)) != z.fParams.frameContentSize {
		return nil, 0, zstdErrCorruption
	}
	if z.fParams.checksumFlag {
		if remaining < 4 {
			return nil, 0, zstdErrChecksumWrong
		}
		if binary.LittleEndian.Uint32(src[ip:]) != uint32(z.xxh.Sum64()) {
			return nil, 0, zstdErrChecksumWrong
		}
		ip += 4
	}
	return dst, ip, nil
}

// ZSTD_decompressMultiFrame without a dictionary over src, the single-pass shortcut of the stream
func (z *zstdDStream) decompressMultiFrame(dstCapacity uint64, src []byte) ([]byte, error) {
	var dst []byte
	moreThan1Frame := false
	for len(src) >= zstdFrameHeaderPrefix {
		if zstdIsLegacy(src) != 0 {
			frameSize, err := zstdFindFrameCompressedSizeLegacy(src)
			if err != nil {
				return nil, err
			}
			decoded, err := zstdDecompressLegacy(dstCapacity, src[:frameSize])
			if err != nil {
				return nil, err
			}
			expectedSize, err := zstdGetDecompressedSizeLegacy(src)
			if err != nil {
				return nil, zstdErrCorruption
			}
			if expectedSize != 0 && expectedSize != uint64(len(decoded)) {
				return nil, zstdErrCorruption
			}
			dst = append(dst, decoded...)
			dstCapacity -= uint64(len(decoded))
			src = src[frameSize:]
			continue
		}
		if binary.LittleEndian.Uint32(src)&zstdMagicSkippableMask == zstdMagicSkippableStart {
			size, err := zstdReadSkippableFrameSize(src)
			if err != nil {
				return nil, err
			}
			src = src[size:]
			continue
		}
		z.decompressBegin()
		decoded, consumed, err := z.decompressFrame(dstCapacity, src)
		if errors.Is(err, zstdErrPrefixUnknown) && moreThan1Frame {
			return nil, zstdErrSrcSizeWrong
		}
		if err != nil {
			return nil, err
		}
		dst = append(dst, decoded...)
		dstCapacity -= uint64(len(decoded))
		src = src[consumed:]
		moreThan1Frame = true
	}
	if len(src) != 0 {
		return nil, zstdErrSrcSizeWrong
	}
	return dst, nil
}

// ZSTD_decompressStream, returning bytes written to out, the new input position and the size hint
func (z *zstdDStream) decompressStream(out, in []byte, inPos int) (int, int, uint64, error) {
	if inPos > len(in) {
		return 0, 0, 0, zstdErrSrcSizeWrong
	}
	istart, ip, iend := inPos, inPos, len(in)
	op, oend := 0, len(out)
	for someMoreWork := true; someMoreWork; {
		switch z.streamStage {
		case zdssInit:
			z.streamStage = zdssLoadHeader
			z.lhSize, z.inPos, z.outStart, z.outEnd, z.outData = 0, 0, 0, 0, nil
			z.legacyVersion = 0
			z.hostageByte = false
			fallthrough
		case zdssLoadHeader:
			if z.legacyVersion != 0 {
				n, pos, hint, err := zstdDecompressLegacyStream(z, z.legacyVersion, out, in, inPos)
				if err == nil && hint == 0 {
					z.streamStage = zdssInit
				}
				return n, pos, hint, err
			}
			hSize, err := zstdGetFrameHeader(&z.fParams, z.headerBuffer[:z.lhSize])
			if err != nil {
				version := zstdIsLegacy(in[istart:iend])
				if version == 0 {
					return 0, 0, 0, err
				}
				if err := zstdInitLegacyStream(z, z.previousLegacyVersion, version); err != nil {
					return 0, 0, 0, err
				}
				z.legacyVersion, z.previousLegacyVersion = version, version
				n, pos, hint, err := zstdDecompressLegacyStream(z, version, out, in, inPos)
				if err == nil && hint == 0 {
					z.streamStage = zdssInit
				}
				return n, pos, hint, err
			}
			if hSize != 0 {
				toLoad := hSize - z.lhSize
				if toLoad > iend-ip {
					z.lhSize += copy(z.headerBuffer[z.lhSize:], in[ip:iend])
					if _, err := zstdGetFrameHeader(&z.fParams, z.headerBuffer[:z.lhSize]); err != nil {
						return 0, 0, 0, err
					}
					return 0, iend, uint64(max(zstdFrameHeaderMin, hSize)-z.lhSize) + zstdBlockHeaderSize, nil
				}
				copy(z.headerBuffer[z.lhSize:], in[ip:ip+toLoad])
				z.lhSize = hSize
				ip += toLoad
				continue
			}
			if z.fParams.frameContentSize != zstdContentSizeUnknown && !z.fParams.skippable && uint64(oend-op) >= z.fParams.frameContentSize {
				cSize, err := zstdFindFrameCompressedSize(in[istart:iend])
				if err == nil && cSize <= uint64(iend-istart) {
					decoded, err := z.decompressMultiFrame(uint64(oend-op), in[istart:istart+int(cSize)])
					if err != nil {
						return 0, 0, 0, err
					}
					op += copy(out[op:], decoded)
					ip = istart + int(cSize)
					z.expected = 0
					z.streamStage = zdssInit
					someMoreWork = false
					continue
				}
			}
			z.decompressBegin()
			if binary.LittleEndian.Uint32(z.headerBuffer[:])&zstdMagicSkippableMask == zstdMagicSkippableStart {
				z.expected = uint64(binary.LittleEndian.Uint32(z.headerBuffer[4:]))
				z.stage = stageSkipFrame
			} else {
				if err := z.decodeFrameHeader(z.headerBuffer[:z.lhSize]); err != nil {
					return 0, 0, 0, err
				}
				z.expected = zstdBlockHeaderSize
				z.stage = stageDecodeBlockHeader
			}
			z.fParams.windowSize = max(z.fParams.windowSize, 1<<zstdWindowLogMin)
			if z.fParams.windowSize > z.maxWindowSize {
				return 0, 0, 0, zstdErrWindowTooLarge
			}
			z.sizeBuffers()
			if z.stage != stageSkipFrame {
				z.blocks = newZstdBlocks(z.fParams.windowSize)
			}
			z.streamStage = zdssRead
			fallthrough
		case zdssRead:
			neededInSize := z.nextSrcSize(uint64(iend - ip))
			if neededInSize == 0 {
				z.streamStage = zdssInit
				someMoreWork = false
				continue
			}
			if uint64(iend-ip) >= neededInSize {
				end := ip + int(neededInSize)
				if err := z.decompressContinueStream(in[ip:end], neededInSize); err != nil {
					return 0, 0, 0, err
				}
				ip = end
				continue
			}
			if ip == iend {
				someMoreWork = false
				continue
			}
			z.streamStage = zdssLoad
			fallthrough
		case zdssLoad:
			neededInSize := z.expected
			toLoad := neededInSize - z.inPos
			isSkip := z.stage == stageSkipFrame
			if !isSkip && toLoad > z.inBuffSize-z.inPos {
				return 0, 0, 0, zstdErrCorruption
			}
			loaded := min(toLoad, uint64(iend-ip))
			if !isSkip {
				z.inBuff = append(z.inBuff[:z.inPos], in[ip:ip+int(loaded)]...)
			}
			ip += int(loaded)
			z.inPos += loaded
			if loaded < toLoad {
				someMoreWork = false
				continue
			}
			z.inPos = 0
			var src []byte
			if !isSkip {
				src = z.inBuff[:neededInSize]
			}
			if err := z.decompressContinueStream(src, neededInSize); err != nil {
				return 0, 0, 0, err
			}
		case zdssFlush:
			toFlush := z.outEnd - z.outStart
			flushed := min(uint64(oend-op), toFlush)
			op += copy(out[op:], z.outData[:flushed])
			z.outData = z.outData[flushed:]
			z.outStart += flushed
			if flushed == toFlush {
				z.streamStage = zdssRead
				if z.outBuffSize < z.fParams.frameContentSize && z.outStart+z.fParams.blockSizeMax > z.outBuffSize {
					z.outStart, z.outEnd = 0, 0
				}
				continue
			}
			someMoreWork = false
		}
	}
	if ip == istart && op == 0 {
		z.noForwardProgress++
		if z.noForwardProgress >= zstdNoProgressMax {
			if op == oend {
				return 0, 0, 0, zstdErrNoProgressFull
			}
			if ip == iend {
				return 0, 0, 0, zstdErrNoProgressEmpty
			}
		}
	} else {
		z.noForwardProgress = 0
	}
	hint := z.expected
	if hint == 0 {
		if z.outEnd == z.outStart {
			if z.hostageByte {
				if ip >= iend {
					z.streamStage = zdssRead
					return op, ip, 1, nil
				}
				ip++
			}
			return op, ip, 0, nil
		}
		if !z.hostageByte {
			ip--
			z.hostageByte = true
		}
		return op, ip, 1, nil
	}
	if z.stage == stageDecompressBlock {
		hint += zstdBlockHeaderSize
	}
	return op, ip, hint - z.inPos, nil
}

// Buffer size bookkeeping of zdss_loadHeader, kept across frames and shrunk after a long oversized run
func (z *zstdDStream) sizeBuffers() {
	neededIn := max(z.fParams.blockSizeMax, 4)
	neededOut := zstdDecodingBufferSize(z.fParams.windowSize, z.fParams.frameContentSize, z.fParams.blockSizeMax)
	if z.inBuffSize+z.outBuffSize >= (neededIn+neededOut)*zstdTooLargeFactor {
		z.oversizedDuration++
	} else {
		z.oversizedDuration = 0
	}
	if z.inBuffSize < neededIn || z.outBuffSize < neededOut || z.oversizedDuration >= zstdTooLargeMaxDuration {
		z.inBuffSize, z.outBuffSize = neededIn, neededOut
		z.inBuff = nil
	}
}

// compression.zstd.ZstdDecompressor over the libzstd streaming translation
type zstdDecompressor struct {
	ds         *zstdDStream
	unconsumed []byte
	eof        bool
	needsInput bool
}

func newZstdDecompressor() *zstdDecompressor {
	return &zstdDecompressor{ds: newZstdDStream(), needsInput: true}
}

func (d *zstdDecompressor) isEOF() bool      { return d.eof }
func (d *zstdDecompressor) wantsInput() bool { return d.needsInput }
func (d *zstdDecompressor) release()         { d.ds.blocks.end() }

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
	if err != nil {
		d.unconsumed, d.needsInput, d.eof = nil, true, false
		d.ds.streamStage, d.ds.noForwardProgress = zdssInit, 0
		d.ds.blocks.end()
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
func (d *zstdDecompressor) decompressLockHeld(in []byte, maxLength int) ([]byte, int, error) {
	buf := newOutputBuffer(maxLength)
	pos := 0
	for {
		n, newPos, hint, err := d.ds.decompressStream(buf.avail(), in, pos)
		if err != nil {
			return nil, 0, err
		}
		buf.filled += n
		pos = newPos
		if hint == 0 {
			d.eof = true
			d.ds.blocks.end()
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
	return buf.data[:buf.filled], pos, nil
}
