package libzstd

// Frame decoder a buffered v0.6 or v0.7 stream drives
type lzbDCtx interface {
	decompressBeginUsingDict(dict []byte) ErrorCode
	nextSrcSize() int
	isSkipFrame() bool
	decompressContinue(out []byte, dst, dstCapacity int, src []byte) (int, ErrorCode)
}

// ZBUFFv06_DCtx and ZBUFFv07_DCtx, params reading the frame header into the bytes needed and the window size
type lzbuff struct {
	zd             lzbDCtx
	params         func(header []byte) (int, int, ErrorCode)
	hintAddsHeader bool
	stage          int
	inBuff         []byte
	inPos          int
	outBuff        []byte
	outStart       int
	outEnd         int
	blockSize      int
	headerBuffer   [18]byte
	lhSize         int
}

func (z *lzbuff) decompressInit(dict []byte) {
	z.stage = lzbLoadHeader
	z.lhSize, z.inPos, z.outStart, z.outEnd = 0, 0, 0, 0
	z.zd.decompressBeginUsingDict(dict)
}

// ZBUFF_decompressContinue of v0.6 and v0.7, the bytes written to dst and read from src then the next input hint
func (z *lzbuff) decompressContinue(dst, src []byte) (written, read int, hint uint64, e ErrorCode) {
	ip, iend := 0, len(src)
	op, oend := 0, len(dst)
	written, read = len(dst), len(src)
	notDone := true
	for notDone {
		stage := z.stage
		if stage == lzbInit {
			return written, read, 0, ErrInitMissing
		}
		if stage == lzbLoadHeader {
			hSize, windowSize, e := z.params(z.headerBuffer[:z.lhSize])
			if e != 0 {
				return written, read, 0, e
			}
			if hSize != 0 {
				toLoad := hSize - z.lhSize
				if toLoad > iend-ip {
					copy(z.headerBuffer[z.lhSize:], src[ip:iend])
					z.lhSize += iend - ip
					return 0, read, uint64(hSize-z.lhSize) + lBlockHeaderSize, 0
				}
				copy(z.headerBuffer[z.lhSize:], src[ip:ip+toLoad])
				z.lhSize = hSize
				ip += toLoad
				continue
			}
			h1Size := z.zd.nextSrcSize()
			if _, e := z.zd.decompressContinue(nil, 0, 0, z.headerBuffer[:h1Size]); e != 0 {
				return written, read, 0, e
			}
			if h1Size < z.lhSize {
				h2Size := z.zd.nextSrcSize()
				if _, e := z.zd.decompressContinue(nil, 0, 0, z.headerBuffer[h1Size:h1Size+h2Size]); e != 0 {
					return written, read, 0, e
				}
			}
			blockSize := min(windowSize, lBlockSize)
			z.blockSize = blockSize
			if len(z.inBuff) < blockSize {
				z.inBuff = make([]byte, blockSize)
			}
			if neededOutSize := windowSize + blockSize + lWildcopyOver*2; len(z.outBuff) < neededOutSize {
				z.outBuff = make([]byte, neededOutSize)
			}
			z.stage = lzbRead
			stage = lzbRead
		}
		if stage == lzbRead {
			neededInSize := z.zd.nextSrcSize()
			if neededInSize == 0 {
				z.stage = lzbInit
				notDone = false
				continue
			}
			if iend-ip >= neededInSize {
				isSkipFrame := z.zd.isSkipFrame()
				capacity := len(z.outBuff) - z.outStart
				if isSkipFrame {
					capacity = 0
				}
				decodedSize, e := z.zd.decompressContinue(z.outBuff, z.outStart, capacity, src[ip:ip+neededInSize])
				if e != 0 {
					return written, read, 0, e
				}
				ip += neededInSize
				if decodedSize == 0 && !isSkipFrame {
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
			neededInSize := z.zd.nextSrcSize()
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
			isSkipFrame := z.zd.isSkipFrame()
			decodedSize, e := z.zd.decompressContinue(z.outBuff, z.outStart, len(z.outBuff)-z.outStart, z.inBuff[:neededInSize])
			if e != 0 {
				return written, read, 0, e
			}
			z.inPos = 0
			if decodedSize == 0 && !isSkipFrame {
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
				if z.outStart+z.blockSize > len(z.outBuff) {
					z.outStart, z.outEnd = 0, 0
				}
				continue
			}
			notDone = false
		}
	}
	nextSrcSizeHint := uint64(z.zd.nextSrcSize())
	if z.hintAddsHeader && nextSrcSizeHint > lBlockHeaderSize {
		nextSrcSizeHint += lBlockHeaderSize
	}
	nextSrcSizeHint -= uint64(z.inPos)
	return op, ip, nextSrcSizeHint, 0
}
