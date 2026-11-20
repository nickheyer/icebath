package libzstd

import "math/bits"

const (
	hufDecoderFastTableLog = 11
	hufDTableCapacityLog   = 12
	hufDTableSize          = 1 + 1<<hufDTableCapacityLog
)

// HUF_DTable of ZSTD_HUFFDTABLE_CAPACITY_LOG, word 0 the DTableDesc bytes, then 2 byte X1 or 4 byte X2 cells
type hufDTable []uint32

// Table as ZSTD_decompressBegin resets it, maxTableLog and reserved set to the capacity log
func newHufDTable() hufDTable {
	dt := make(hufDTable, hufDTableSize)
	dt[0] = hufDTableCapacityLog * 0x1000001
	return dt
}

func (dt hufDTable) maxTableLog() uint32 { return dt[0] & 0xFF }
func (dt hufDTable) tableType() uint32   { return dt[0] >> 8 & 0xFF }
func (dt hufDTable) tableLog() uint32    { return dt[0] >> 16 & 0xFF }

// Stores tableType and tableLog of the descriptor, keeping maxTableLog and reserved
func (dt hufDTable) setTypeLog(tableType, tableLog uint32) {
	dt[0] = dt[0]&0xFF0000FF | (tableType&0xFF)<<8 | (tableLog&0xFF)<<16
}

// HUF_DEltX1 cell i as its little endian 16 bit word, nbBits low and symbol high
func (dt hufDTable) x1(i int) uint32 { return dt[1+i>>1] >> (uint(i&1) * 16) & 0xFFFF }

func (dt hufDTable) setX1(i int, cell uint32) {
	k, s := 1+i>>1, uint(i&1)*16
	dt[k] = dt[k]&^(0xFFFF<<s) | cell<<s
}

// HUF_DEltX2 cell i, sequence low 16 bits, nbBits next byte, length top byte
func (dt hufDTable) x2(i int) uint32 { return dt[1+i] }

// HUF_rescaleStats raising every nonzero weight so the table reaches targetTableLog
func hufRescaleStats(huffWeight []byte, rankVal *[hufTableLogMax + 1]uint32, nbSymbols, tableLog, targetTableLog uint32) uint32 {
	if tableLog > targetTableLog {
		return tableLog
	}
	if tableLog < targetTableLog {
		scale := targetTableLog - tableLog
		for s := uint32(0); s < nbSymbols; s++ {
			if huffWeight[s] != 0 {
				huffWeight[s] += byte(scale)
			}
		}
		for s := targetTableLog; s > scale; s-- {
			rankVal[s] = rankVal[s-scale]
		}
		for s := scale; s > 0; s-- {
			rankVal[s] = 0
		}
	}
	return targetTableLog
}

// HUF_readDTableX1_wksp, the size of the table description read from src
func hufReadDTableX1(dt hufDTable, src []byte) (int, ErrorCode) {
	var huffWeight [hufSymbolValueMax + 1]byte
	var rankVal, rankStart [hufTableLogMax + 1]uint32
	var symbols [hufSymbolValueMax + 1]byte
	var tableLog, nbSymbols uint32
	iSize, err := hufReadStats(huffWeight[:], &rankVal, &nbSymbols, &tableLog, src)
	if err != 0 {
		return 0, err
	}
	maxTableLog := dt.maxTableLog() + 1
	tableLog = hufRescaleStats(huffWeight[:], &rankVal, nbSymbols, tableLog, min(maxTableLog, hufDecoderFastTableLog))
	if tableLog > dt.maxTableLog()+1 {
		return 0, ErrTableLogTooLarge
	}
	dt.setTypeLog(0, tableLog)
	next := uint32(0)
	for n := uint32(0); n < tableLog+1; n++ {
		rankStart[n] = next
		next += rankVal[n]
	}
	for n := uint32(0); n < nbSymbols; n++ {
		w := huffWeight[n]
		symbols[rankStart[w]] = byte(n)
		rankStart[w]++
	}
	symbol := int(rankVal[0])
	start := 0
	for w := uint32(1); w < tableLog+1; w++ {
		symbolCount := int(rankVal[w])
		length := (1 << w) >> 1
		u := start
		nbBits := tableLog + 1 - w
		for s := 0; s < symbolCount; s++ {
			cell := nbBits&0xFF | uint32(symbols[symbol+s])<<8
			for k := 0; k < length; k++ {
				dt.setX1(u+k, cell)
			}
			u += length
		}
		symbol += symbolCount
		start += symbolCount * length
	}
	return iSize, 0
}

// HUF_decodeSymbolX1
func hufDecodeSymbolX1(d *bitDStream, dt hufDTable, dtLog uint32) byte {
	cell := dt.x1(int(d.lookBitsFast(dtLog)))
	d.skipBits(cell & 0xFF)
	return byte(cell >> 8)
}

// HUF_decodeStreamX1 filling dst from p to pEnd, the count written
func hufDecodeStreamX1(dst []byte, p int, d *bitDStream, pEnd int, dt hufDTable, dtLog uint32) int {
	pStart := p
	if pEnd-p > 3 {
		for d.reload() == bitUnfinished && p < pEnd-3 {
			for k := 0; k < 4; k++ {
				dst[p] = hufDecodeSymbolX1(d, dt, dtLog)
				p++
			}
		}
	} else {
		d.reload()
	}
	for p < pEnd {
		dst[p] = hufDecodeSymbolX1(d, dt, dtLog)
		p++
	}
	return pEnd - pStart
}

// HUF_decompress1X1_usingDTable_internal_body
func hufDecompress1X1Body(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	var d bitDStream
	if _, err := d.init(cSrc, 0, len(cSrc)); err != 0 {
		return 0, err
	}
	hufDecodeStreamX1(dst, 0, &d, dstSize, dt, dt.tableLog())
	if !d.endOfDStream() {
		return 0, ErrCorruptionDetected
	}
	return dstSize, 0
}

// Jump table lengths of a 4 stream block, the fourth computed in size_t arithmetic
func hufJumpTable(cSrc []byte) (l1, l2, l3 int, l4 uint64) {
	l1, l2, l3 = int(readLE16(cSrc, 0)), int(readLE16(cSrc, 2)), int(readLE16(cSrc, 4))
	return l1, l2, l3, uint64(len(cSrc)) - uint64(l1+l2+l3+6)
}

// HUF_decompress4X1_usingDTable_internal_body, the generic four stream decoder
func hufDecompress4X1Body(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	if len(cSrc) < 10 || dstSize < 6 {
		return 0, ErrCorruptionDetected
	}
	oend := dstSize
	olimit := oend - 3
	l1, l2, l3, l4 := hufJumpTable(cSrc)
	is1 := 6
	is2, is3 := is1+l1, is1+l1+l2
	is4 := is3 + l3
	segmentSize := (dstSize + 3) / 4
	opStart2 := segmentSize
	opStart3, opStart4 := opStart2+segmentSize, opStart2+2*segmentSize
	op := [4]int{0, opStart2, opStart3, opStart4}
	dtLog := dt.tableLog()
	if l4 > uint64(len(cSrc)) || opStart4 > oend {
		return 0, ErrCorruptionDetected
	}
	var bd [4]bitDStream
	starts := [4]int{is1, is2, is3, is4}
	lens := [4]int{l1, l2, l3, int(l4)}
	for s := 0; s < 4; s++ {
		if _, err := bd[s].init(cSrc, starts[s], lens[s]); err != 0 {
			return 0, err
		}
	}
	if oend-op[3] >= 8 {
		for endSignal := true; endSignal && op[3] < olimit; {
			for k := 0; k < 4; k++ {
				for s := 0; s < 4; s++ {
					dst[op[s]] = hufDecodeSymbolX1(&bd[s], dt, dtLog)
					op[s]++
				}
			}
			for s := 0; s < 4; s++ {
				endSignal = bd[s].reloadFast() == bitUnfinished && endSignal
			}
		}
	}
	if op[0] > opStart2 || op[1] > opStart3 || op[2] > opStart4 {
		return 0, ErrCorruptionDetected
	}
	ends := [4]int{opStart2, opStart3, opStart4, oend}
	for s := 0; s < 4; s++ {
		hufDecodeStreamX1(dst, op[s], &bd[s], ends[s], dt, dtLog)
	}
	if !(bd[0].endOfDStream() && bd[1].endOfDStream() && bd[2].endOfDStream() && bd[3].endOfDStream()) {
		return 0, ErrCorruptionDetected
	}
	return dstSize, 0
}

// HUF_DecompressFastArgs, positions into the compressed and decoded buffers
type hufFastArgs struct {
	ip, op, iend [4]int
	bits         [4]uint64
	ilowest      int
	oend         int
}

// HUF_initFastDStream, the container shifted past the bits of the last byte above its end mark
func hufInitFastDStream(src []byte, ip int) uint64 {
	lastByte := src[ip+7]
	bitsConsumed := uint32(0)
	if lastByte != 0 {
		bitsConsumed = 8 - highbit32(uint32(lastByte))
	}
	return (readLE64(src, ip) | 1) << bitsConsumed
}

// HUF_DecompressFastArgs_init, 1 when the fast loop applies, 0 to fall back, or the error
func hufFastArgsInit(a *hufFastArgs, dstSize int, src []byte, dt hufDTable) (int, ErrorCode) {
	srcSize := len(src)
	if dstSize == 0 {
		return 0, 0
	}
	if srcSize < 10 {
		return 0, ErrCorruptionDetected
	}
	if dt.tableLog() != hufDecoderFastTableLog {
		return 0, 0
	}
	l1, l2, l3, l4 := hufJumpTable(src)
	a.iend[0] = 6
	a.iend[1] = a.iend[0] + l1
	a.iend[2] = a.iend[1] + l2
	a.iend[3] = a.iend[2] + l3
	if l1 < 8 || l2 < 8 || l3 < 8 || l4 < 8 {
		return 0, 0
	}
	if l4 > uint64(srcSize) {
		return 0, ErrCorruptionDetected
	}
	a.ip = [4]int{a.iend[1] - 8, a.iend[2] - 8, a.iend[3] - 8, srcSize - 8}
	seg := (dstSize + 3) / 4
	a.op = [4]int{0, seg, 2 * seg, 3 * seg}
	if a.op[3] >= dstSize {
		return 0, 0
	}
	for s := 0; s < 4; s++ {
		a.bits[s] = hufInitFastDStream(src, a.ip[s])
	}
	a.ilowest, a.oend = 0, dstSize
	return 1, 0
}

// HUF_initRemainingDStream, a bit stream resuming where the fast loop left one stream, starting at ilowest
func hufInitRemainingDStream(d *bitDStream, a *hufFastArgs, src []byte, stream, segmentEnd int) ErrorCode {
	if a.op[stream] > segmentEnd || a.ip[stream] < a.iend[stream]-8 {
		return ErrCorruptionDetected
	}
	*d = bitDStream{buf: src, bitContainer: readLE64(src, a.ip[stream]), bitsConsumed: uint32(bits.TrailingZeros64(a.bits[stream])),
		start: a.ilowest, limitPtr: a.ilowest + 8, ptr: a.ip[stream]}
	return 0
}

// HUF_decompress4X1_usingDTable_internal_fast_c_loop, the same steps as the amd64 assembly loop
func hufFastLoopX1(a *hufFastArgs, src, dst []byte, dt hufDTable) {
	bs, ip, op := a.bits, a.ip, a.op
outer:
	for {
		iters := min((a.oend-op[3])/5, (ip[0]-a.ilowest)/7)
		olimit := op[3] + iters*5
		if op[3] == olimit {
			break
		}
		for s := 1; s < 4; s++ {
			if ip[s] < ip[s-1] {
				break outer
			}
		}
		for {
			for sym := 0; sym < 5; sym++ {
				for s := 0; s < 4; s++ {
					entry := dt.x1(int(bs[s] >> 53))
					bs[s] <<= entry & 0x3F
					dst[op[s]+sym] = byte(entry >> 8)
				}
			}
			for s := 0; s < 4; s++ {
				ctz := bits.TrailingZeros64(bs[s])
				op[s] += 5
				ip[s] -= ctz >> 3
				bs[s] = (readLE64(src, ip[s]) | 1) << uint(ctz&7)
			}
			if op[3] >= olimit {
				break
			}
		}
	}
	a.bits, a.ip, a.op = bs, ip, op
}

// HUF_decompress4X1_usingDTable_internal_fast, zero when the generic decoder must run instead
func hufDecompress4X1Fast(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	var a hufFastArgs
	if ret, err := hufFastArgsInit(&a, dstSize, cSrc, dt); err != 0 || ret == 0 {
		return 0, err
	}
	hufFastLoopX1(&a, cSrc, dst, dt)
	segmentSize := (dstSize + 3) / 4
	segmentEnd := 0
	for i := 0; i < 4; i++ {
		if segmentSize <= dstSize-segmentEnd {
			segmentEnd += segmentSize
		} else {
			segmentEnd = dstSize
		}
		var d bitDStream
		if err := hufInitRemainingDStream(&d, &a, cSrc, i, segmentEnd); err != 0 {
			return 0, err
		}
		a.op[i] += hufDecodeStreamX1(dst, a.op[i], &d, segmentEnd, dt, hufDecoderFastTableLog)
		if a.op[i] != segmentEnd {
			return 0, ErrCorruptionDetected
		}
	}
	return dstSize, 0
}

// HUF_decompress4X1_usingDTable_internal of a BMI2 x86-64 build, the fast loop first
func hufDecompress4X1UsingDTable(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	if ret, err := hufDecompress4X1Fast(dst, dstSize, cSrc, dt); err != 0 || ret != 0 {
		return ret, err
	}
	return hufDecompress4X1Body(dst, dstSize, cSrc, dt)
}

// HUF_decompress4X1_DCtx_wksp, reading the table then decoding the four streams
func hufDecompress4X1DCtx(dt hufDTable, dst []byte, dstSize int, cSrc []byte) (int, ErrorCode) {
	hSize, err := hufReadDTableX1(dt, cSrc)
	if err != 0 {
		return 0, err
	}
	if hSize >= len(cSrc) {
		return 0, ErrSrcSizeWrong
	}
	return hufDecompress4X1UsingDTable(dst, dstSize, cSrc[hSize:], dt)
}

// HUF_decompress1X1_DCtx_wksp
func hufDecompress1X1DCtx(dt hufDTable, dst []byte, dstSize int, cSrc []byte) (int, ErrorCode) {
	hSize, err := hufReadDTableX1(dt, cSrc)
	if err != 0 {
		return 0, err
	}
	if hSize >= len(cSrc) {
		return 0, ErrSrcSizeWrong
	}
	return hufDecompress1X1Body(dst, dstSize, cSrc[hSize:], dt)
}

// HUF_buildDEltX2U32 of a little endian build
func hufBuildDEltX2(symbol, nbBits, baseSeq uint32, level uint32) uint32 {
	seq := baseSeq + symbol<<8
	if level == 1 {
		seq = symbol
	}
	return seq + nbBits<<16 + level<<24
}

// HUF_fillDTableX2ForWeight writing each symbol over 1 << (tableLog - nbBits) cells from rank
func hufFillDTableX2ForWeight(dt hufDTable, rank int, symbols []byte, nbBits, tableLog uint32, baseSeq uint32, level uint32) {
	length := 1 << ((tableLog - nbBits) & 0x1F)
	for _, sym := range symbols {
		cell := hufBuildDEltX2(uint32(sym), nbBits, baseSeq, level)
		for k := 0; k < length; k++ {
			dt[1+rank+k] = cell
		}
		rank += length
	}
}

// HUF_fillDTableX2Level2, the second symbols following baseSeq after consumedBits
func hufFillDTableX2Level2(dt hufDTable, base int, targetLog, consumedBits uint32, rankVal *[hufTableLogMax + 1]uint32,
	minWeight, maxWeight1 int, sorted []byte, rankStart *[hufTableLogMax + 3]uint32, nbBitsBaseline uint32, baseSeq uint32) {
	if minWeight > 1 {
		length := 1 << ((targetLog - consumedBits) & 0x1F)
		cell := hufBuildDEltX2(baseSeq, consumedBits, 0, 1)
		skipSize := int(rankVal[minWeight])
		switch length {
		case 2:
			dt[1+base], dt[2+base] = cell, cell
		case 4:
			for k := 0; k < 4; k++ {
				dt[1+base+k] = cell
			}
		default:
			for i := 0; i < skipSize; i += 8 {
				for k := 0; k < 8; k++ {
					dt[1+base+i+k] = cell
				}
			}
		}
	}
	for w := minWeight; w < maxWeight1; w++ {
		begin, end := rankStart[w], rankStart[w+1]
		nbBits := nbBitsBaseline - uint32(w)
		hufFillDTableX2ForWeight(dt, base+int(rankVal[w]), sorted[begin:end], nbBits+consumedBits, targetLog, baseSeq, 2)
	}
}

// HUF_fillDTableX2 over every weight, double symbol cells wherever a second symbol fits
func hufFillDTableX2(dt hufDTable, targetLog uint32, sorted []byte, rankStart *[hufTableLogMax + 3]uint32,
	rankValOrigin *[hufTableLogMax][hufTableLogMax + 1]uint32, maxWeight, nbBitsBaseline uint32) {
	rankVal := &rankValOrigin[0]
	scaleLog := int(nbBitsBaseline) - int(targetLog)
	minBits := nbBitsBaseline - maxWeight
	wEnd := int(maxWeight) + 1
	for w := 1; w < wEnd; w++ {
		begin, end := int(rankStart[w]), int(rankStart[w+1])
		nbBits := nbBitsBaseline - uint32(w)
		if targetLog-nbBits >= minBits {
			start := int(rankVal[w])
			length := 1 << ((targetLog - nbBits) & 0x1F)
			minWeight := max(int(nbBits)+scaleLog, 1)
			for s := begin; s != end; s++ {
				hufFillDTableX2Level2(dt, start, targetLog, nbBits, &rankValOrigin[nbBits], minWeight, wEnd, sorted, rankStart, nbBitsBaseline, uint32(sorted[s]))
				start += length
			}
		} else {
			hufFillDTableX2ForWeight(dt, int(rankVal[w]), sorted[begin:end], nbBits, targetLog, 0, 1)
		}
	}
}

// HUF_readDTableX2_wksp, the size of the table description read from src
func hufReadDTableX2(dt hufDTable, src []byte) (int, ErrorCode) {
	var rankVal [hufTableLogMax][hufTableLogMax + 1]uint32
	var rankStats [hufTableLogMax + 1]uint32
	var rankStart0 [hufTableLogMax + 3]uint32
	var sorted [hufSymbolValueMax + 1]byte
	var weightList [hufSymbolValueMax + 1]byte
	var tableLog, nbSymbols uint32
	maxTableLog := dt.maxTableLog()
	if maxTableLog > hufTableLogMax {
		return 0, ErrTableLogTooLarge
	}
	iSize, err := hufReadStats(weightList[:], &rankStats, &nbSymbols, &tableLog, src)
	if err != 0 {
		return 0, err
	}
	if tableLog > maxTableLog {
		return 0, ErrTableLogTooLarge
	}
	if tableLog <= hufDecoderFastTableLog && maxTableLog > hufDecoderFastTableLog {
		maxTableLog = hufDecoderFastTableLog
	}
	maxW := tableLog
	for rankStats[maxW] == 0 {
		maxW--
	}
	rankStart := rankStart0[1:]
	next := uint32(0)
	for w := uint32(1); w < maxW+1; w++ {
		rankStart[w] = next
		next += rankStats[w]
	}
	rankStart[0] = next
	rankStart[maxW+1] = next
	for s := uint32(0); s < nbSymbols; s++ {
		w := weightList[s]
		sorted[rankStart[w]] = byte(s)
		rankStart[w]++
	}
	rankStart[0] = 0
	rescale := int(maxTableLog) - int(tableLog) - 1
	nextRankVal := uint32(0)
	for w := uint32(1); w < maxW+1; w++ {
		rankVal[0][w] = nextRankVal
		nextRankVal += rankStats[w] << uint(int(w)+rescale)
	}
	minBits := tableLog + 1 - maxW
	for consumed := minBits; consumed < maxTableLog-minBits+1; consumed++ {
		for w := uint32(1); w < maxW+1; w++ {
			rankVal[consumed][w] = rankVal[0][w] >> consumed
		}
	}
	hufFillDTableX2(dt, maxTableLog, sorted[:], &rankStart0, &rankVal, maxW, tableLog+1)
	dt.setTypeLog(1, maxTableLog)
	return iSize, 0
}

// HUF_decodeSymbolX2 writing both bytes of the cell sequence at p, the count it decoded
func hufDecodeSymbolX2(dst []byte, p int, d *bitDStream, dt hufDTable, dtLog uint32) int {
	cell := dt.x2(int(d.lookBitsFast(dtLog)))
	dst[p], dst[p+1] = byte(cell), byte(cell>>8)
	d.skipBits(cell >> 16 & 0xFF)
	return int(cell >> 24)
}

// HUF_decodeLastSymbolX2 writing one byte and never consuming past the container
func hufDecodeLastSymbolX2(dst []byte, p int, d *bitDStream, dt hufDTable, dtLog uint32) int {
	cell := dt.x2(int(d.lookBitsFast(dtLog)))
	dst[p] = byte(cell)
	if cell>>24 == 1 {
		d.skipBits(cell >> 16 & 0xFF)
	} else if d.bitsConsumed < 64 {
		d.skipBits(cell >> 16 & 0xFF)
		if d.bitsConsumed > 64 {
			d.bitsConsumed = 64
		}
	}
	return 1
}

// HUF_decodeStreamX2 filling dst from p to pEnd, the count written
func hufDecodeStreamX2(dst []byte, p int, d *bitDStream, pEnd int, dt hufDTable, dtLog uint32) int {
	pStart := p
	if pEnd-p >= 8 {
		if dtLog <= 11 {
			for d.reload() == bitUnfinished && p < pEnd-9 {
				for k := 0; k < 5; k++ {
					p += hufDecodeSymbolX2(dst, p, d, dt, dtLog)
				}
			}
		} else {
			for d.reload() == bitUnfinished && p < pEnd-7 {
				for k := 0; k < 4; k++ {
					p += hufDecodeSymbolX2(dst, p, d, dt, dtLog)
				}
			}
		}
	} else {
		d.reload()
	}
	if pEnd-p >= 2 {
		for d.reload() == bitUnfinished && p <= pEnd-2 {
			p += hufDecodeSymbolX2(dst, p, d, dt, dtLog)
		}
		for p <= pEnd-2 {
			p += hufDecodeSymbolX2(dst, p, d, dt, dtLog)
		}
	}
	if p < pEnd {
		p += hufDecodeLastSymbolX2(dst, p, d, dt, dtLog)
	}
	return p - pStart
}

// HUF_decompress1X2_usingDTable_internal_body
func hufDecompress1X2Body(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	var d bitDStream
	if _, err := d.init(cSrc, 0, len(cSrc)); err != 0 {
		return 0, err
	}
	hufDecodeStreamX2(dst, 0, &d, dstSize, dt, dt.tableLog())
	if !d.endOfDStream() {
		return 0, ErrCorruptionDetected
	}
	return dstSize, 0
}

// HUF_decompress4X2_usingDTable_internal_body in the symbol order of a gcc build
func hufDecompress4X2Body(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	if len(cSrc) < 10 || dstSize < 6 {
		return 0, ErrCorruptionDetected
	}
	oend := dstSize
	olimit := oend - 7
	l1, l2, l3, l4 := hufJumpTable(cSrc)
	is1 := 6
	is2, is3 := is1+l1, is1+l1+l2
	is4 := is3 + l3
	segmentSize := (dstSize + 3) / 4
	opStart2 := segmentSize
	opStart3, opStart4 := opStart2+segmentSize, opStart2+2*segmentSize
	op := [4]int{0, opStart2, opStart3, opStart4}
	dtLog := dt.tableLog()
	if l4 > uint64(len(cSrc)) || opStart4 > oend {
		return 0, ErrCorruptionDetected
	}
	var bd [4]bitDStream
	starts := [4]int{is1, is2, is3, is4}
	lens := [4]int{l1, l2, l3, int(l4)}
	for s := 0; s < 4; s++ {
		if _, err := bd[s].init(cSrc, starts[s], lens[s]); err != 0 {
			return 0, err
		}
	}
	if oend-op[3] >= 8 {
		for endSignal := true; endSignal && op[3] < olimit; {
			for k := 0; k < 4; k++ {
				for s := 0; s < 4; s++ {
					op[s] += hufDecodeSymbolX2(dst, op[s], &bd[s], dt, dtLog)
				}
			}
			r0 := bd[0].reloadFast() == bitUnfinished
			r1 := bd[1].reloadFast() == bitUnfinished
			r2 := bd[2].reloadFast() == bitUnfinished
			r3 := bd[3].reloadFast() == bitUnfinished
			endSignal = r0 && r1 && r2 && r3
		}
	}
	if op[0] > opStart2 || op[1] > opStart3 || op[2] > opStart4 {
		return 0, ErrCorruptionDetected
	}
	ends := [4]int{opStart2, opStart3, opStart4, oend}
	for s := 0; s < 4; s++ {
		hufDecodeStreamX2(dst, op[s], &bd[s], ends[s], dt, dtLog)
	}
	if !(bd[0].endOfDStream() && bd[1].endOfDStream() && bd[2].endOfDStream() && bd[3].endOfDStream()) {
		return 0, ErrCorruptionDetected
	}
	return dstSize, 0
}

// HUF_decompress4X2_usingDTable_internal_fast_c_loop, the same steps as the amd64 assembly loop
func hufFastLoopX2(a *hufFastArgs, src, dst []byte, dt hufDTable) {
	bs, ip, op := a.bits, a.ip, a.op
	oend := [4]int{op[1], op[2], op[3], a.oend}
	decode := func(s int) {
		cell := dt.x2(int(bs[s] >> 53))
		dst[op[s]], dst[op[s]+1] = byte(cell), byte(cell>>8)
		bs[s] <<= (cell >> 16 & 0xFF) & 0x3F
		op[s] += int(cell >> 24)
	}
outer:
	for {
		iters := (ip[0] - a.ilowest) / 7
		for s := 0; s < 4; s++ {
			iters = min(iters, (oend[s]-op[s])/10)
		}
		olimit := op[3] + iters*5
		if op[3] == olimit {
			break
		}
		for s := 1; s < 4; s++ {
			if ip[s] < ip[s-1] {
				break outer
			}
		}
		for {
			for k := 0; k < 5; k++ {
				for s := 0; s < 3; s++ {
					decode(s)
				}
			}
			decode(3)
			for s := 0; s < 4; s++ {
				decode(3)
				ctz := bits.TrailingZeros64(bs[s])
				ip[s] -= ctz >> 3
				bs[s] = (readLE64(src, ip[s]) | 1) << uint(ctz&7)
			}
			if op[3] >= olimit {
				break
			}
		}
	}
	a.bits, a.ip, a.op = bs, ip, op
}

// HUF_decompress4X2_usingDTable_internal_fast, zero when the generic decoder must run instead
func hufDecompress4X2Fast(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	var a hufFastArgs
	if ret, err := hufFastArgsInit(&a, dstSize, cSrc, dt); err != 0 || ret == 0 {
		return 0, err
	}
	hufFastLoopX2(&a, cSrc, dst, dt)
	segmentSize := (dstSize + 3) / 4
	segmentEnd := 0
	for i := 0; i < 4; i++ {
		if segmentSize <= dstSize-segmentEnd {
			segmentEnd += segmentSize
		} else {
			segmentEnd = dstSize
		}
		var d bitDStream
		if err := hufInitRemainingDStream(&d, &a, cSrc, i, segmentEnd); err != 0 {
			return 0, err
		}
		a.op[i] += hufDecodeStreamX2(dst, a.op[i], &d, segmentEnd, dt, hufDecoderFastTableLog)
		if a.op[i] != segmentEnd {
			return 0, ErrCorruptionDetected
		}
	}
	return dstSize, 0
}

// HUF_decompress4X2_usingDTable_internal of a BMI2 x86-64 build, the fast loop first
func hufDecompress4X2UsingDTable(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	if ret, err := hufDecompress4X2Fast(dst, dstSize, cSrc, dt); err != 0 || ret != 0 {
		return ret, err
	}
	return hufDecompress4X2Body(dst, dstSize, cSrc, dt)
}

// HUF_decompress4X2_DCtx_wksp, reading the table then decoding the four streams
func hufDecompress4X2DCtx(dt hufDTable, dst []byte, dstSize int, cSrc []byte) (int, ErrorCode) {
	hSize, err := hufReadDTableX2(dt, cSrc)
	if err != 0 {
		return 0, err
	}
	if hSize >= len(cSrc) {
		return 0, ErrSrcSizeWrong
	}
	return hufDecompress4X2UsingDTable(dst, dstSize, cSrc[hSize:], dt)
}

// algoTime of huf_decompress.c, table build and per 256 byte decode costs of the single and double symbol decoders
var hufAlgoTime = [16][2][2]uint32{
	{{0, 0}, {1, 1}}, {{0, 0}, {1, 1}}, {{150, 216}, {381, 119}}, {{170, 205}, {514, 112}},
	{{177, 199}, {539, 110}}, {{197, 194}, {644, 107}}, {{221, 192}, {735, 107}}, {{256, 189}, {881, 106}},
	{{359, 188}, {1167, 109}}, {{582, 187}, {1570, 114}}, {{688, 187}, {1712, 122}}, {{825, 186}, {1965, 136}},
	{{976, 185}, {2131, 150}}, {{1180, 186}, {2070, 175}}, {{1377, 185}, {1731, 202}}, {{1412, 185}, {1695, 202}},
}

// HUF_selectDecoder, true for the double symbol decoder
func hufSelectDecoder(dstSize, cSrcSize int) bool {
	q := 15
	if cSrcSize < dstSize {
		q = cSrcSize * 16 / dstSize
	}
	d256 := uint32(dstSize >> 8)
	t0 := hufAlgoTime[q][0][0] + hufAlgoTime[q][0][1]*d256
	t1 := hufAlgoTime[q][1][0] + hufAlgoTime[q][1][1]*d256
	t1 += t1 >> 5
	return t1 < t0
}

// HUF_decompress1X_usingDTable dispatching on the table type
func hufDecompress1XUsingDTable(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	if dt.tableType() != 0 {
		return hufDecompress1X2Body(dst, dstSize, cSrc, dt)
	}
	return hufDecompress1X1Body(dst, dstSize, cSrc, dt)
}

// HUF_decompress4X_usingDTable dispatching on the table type
func hufDecompress4XUsingDTable(dst []byte, dstSize int, cSrc []byte, dt hufDTable) (int, ErrorCode) {
	if dt.tableType() != 0 {
		return hufDecompress4X2UsingDTable(dst, dstSize, cSrc, dt)
	}
	return hufDecompress4X1UsingDTable(dst, dstSize, cSrc, dt)
}

// HUF_decompress4X_hufOnly_wksp choosing the decoder by the compression ratio
func hufDecompress4XHufOnly(dt hufDTable, dst []byte, dstSize int, cSrc []byte) (int, ErrorCode) {
	if dstSize == 0 {
		return 0, ErrDstSizeTooSmall
	}
	if len(cSrc) == 0 {
		return 0, ErrCorruptionDetected
	}
	if hufSelectDecoder(dstSize, len(cSrc)) {
		return hufDecompress4X2DCtx(dt, dst, dstSize, cSrc)
	}
	return hufDecompress4X1DCtx(dt, dst, dstSize, cSrc)
}
