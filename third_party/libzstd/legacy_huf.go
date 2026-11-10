package libzstd

const (
	lhufAbsoluteMaxTableLog = 16
	lhufMaxTableLog         = 12
	lhufMaxSymbolValue      = 255
)

type lhufDEltX2 struct{ b, nbBits byte }

type lhufDEltX4 struct {
	sequence uint16
	nbBits   byte
	length   byte
}

type lhufSortedSymbol struct{ symbol, weight byte }

// Single symbol table of v0.5 and v0.6, log holding the first slot of the C array
type lhufDTableX2 struct {
	log uint16
	dt  []lhufDEltX2
}

// Double symbol table of v0.5 and v0.6, log holding the first slot of the C array
type lhufDTableX4 struct {
	log uint32
	dt  []lhufDEltX4
}

func newLhufDTableX2(maxLog uint16) *lhufDTableX2 {
	return &lhufDTableX2{log: maxLog, dt: make([]lhufDEltX2, 1<<maxLog)}
}

func newLhufDTableX4(maxLog uint32) *lhufDTableX4 {
	return &lhufDTableX4{log: maxLog, dt: make([]lhufDEltX4, 1<<maxLog)}
}

var lhufRLESizes = [14]int{1, 2, 3, 4, 7, 8, 15, 16, 31, 32, 63, 64, 127, 128}

// HUF_readStats of v0.5 and v0.6, weights of a tree description with their rank counts
func lhufReadStats(huffWeight []byte, hwSize int, rankStats *[lhufAbsoluteMaxTableLog + 1]uint32, nbSymbolsPtr, tableLogPtr *uint32, src []byte, v05 bool) (int, ErrorCode) {
	srcSize := len(src)
	if srcSize == 0 {
		return 0, ErrSrcSizeWrong
	}
	iSize := int(src[0])
	var oSize int
	if iSize >= 128 {
		if iSize >= 242 {
			oSize = lhufRLESizes[iSize-242]
			for i := 0; i < hwSize; i++ {
				huffWeight[i] = 1
			}
			iSize = 0
		} else {
			oSize = iSize - 127
			iSize = (oSize + 1) / 2
			if iSize+1 > srcSize {
				return 0, ErrSrcSizeWrong
			}
			if oSize >= hwSize {
				return 0, ErrCorruptionDetected
			}
			for n := 0; n < oSize; n += 2 {
				huffWeight[n] = src[1+n/2] >> 4
				huffWeight[n+1] = src[1+n/2] & 15
			}
		}
	} else {
		if iSize+1 > srcSize {
			return 0, ErrSrcSizeWrong
		}
		n, e := lfseDecompress(huffWeight[:hwSize-1], src[1:1+iSize], v05)
		if e != 0 {
			return 0, e
		}
		oSize = n
	}
	*rankStats = [lhufAbsoluteMaxTableLog + 1]uint32{}
	weightTotal := uint32(0)
	for n := 0; n < oSize; n++ {
		if huffWeight[n] >= lhufAbsoluteMaxTableLog {
			return 0, ErrCorruptionDetected
		}
		rankStats[huffWeight[n]]++
		weightTotal += (uint32(1) << huffWeight[n]) >> 1
	}
	if weightTotal == 0 {
		return 0, ErrCorruptionDetected
	}
	tableLog := highbit32(weightTotal) + 1
	if tableLog > lhufAbsoluteMaxTableLog {
		return 0, ErrCorruptionDetected
	}
	total := uint32(1) << tableLog
	rest := total - weightTotal
	verif := uint32(1) << highbit32(rest)
	lastWeight := highbit32(rest) + 1
	if verif != rest {
		return 0, ErrCorruptionDetected
	}
	huffWeight[oSize] = byte(lastWeight)
	rankStats[lastWeight]++
	if rankStats[1] < 2 || rankStats[1]&1 != 0 {
		return 0, ErrCorruptionDetected
	}
	*nbSymbolsPtr = uint32(oSize + 1)
	*tableLogPtr = tableLog
	return iSize + 1, 0
}

func lhufReadDTableX2(t *lhufDTableX2, src []byte, v05 bool) (int, ErrorCode) {
	iSize, _, e := lhufReadX2Cells(t.dt, uint32(t.log), src, v05, func(tableLog uint32) { t.log = uint16(tableLog) })
	return iSize, e
}

// Single symbol cells of a tree description no deeper than limit, accept called before the cells are written
func lhufReadX2Cells(dt []lhufDEltX2, limit uint32, src []byte, v05 bool, accept func(tableLog uint32)) (int, uint32, ErrorCode) {
	var huffWeight [lhufMaxSymbolValue + 1]byte
	var rankVal [lhufAbsoluteMaxTableLog + 1]uint32
	var tableLog, nbSymbols uint32
	iSize, e := lhufReadStats(huffWeight[:], lhufMaxSymbolValue+1, &rankVal, &nbSymbols, &tableLog, src, v05)
	if e != 0 {
		return 0, 0, e
	}
	if tableLog > limit {
		return 0, 0, ErrTableLogTooLarge
	}
	accept(tableLog)
	nextRankStart := uint32(0)
	for n := uint32(1); n <= tableLog; n++ {
		current := nextRankStart
		nextRankStart += rankVal[n] << (n - 1)
		rankVal[n] = current
	}
	for n := uint32(0); n < nbSymbols; n++ {
		w := uint32(huffWeight[n])
		length := (uint32(1) << w) >> 1
		d := lhufDEltX2{b: byte(n), nbBits: byte(tableLog + 1 - w)}
		for i := rankVal[w]; i < rankVal[w]+length; i++ {
			dt[i] = d
		}
		rankVal[w] += length
	}
	return iSize, tableLog, 0
}

func lhufDecodeSymbolX2(b *lbitD, dt []lhufDEltX2, dtLog uint32) byte {
	val := b.lookBitsFast(dtLog)
	c := dt[val].b
	b.skipBits(uint32(dt[val].nbBits))
	return c
}

// HUF_decodeStreamX2 filling dst from p up to pEnd
func lhufDecodeStreamX2(dst []byte, p int, b *lbitD, pEnd int, dt []lhufDEltX2, dtLog uint32) {
	for b.reload() == lbitUnfinished && p <= pEnd-4 {
		dst[p] = lhufDecodeSymbolX2(b, dt, dtLog)
		dst[p+1] = lhufDecodeSymbolX2(b, dt, dtLog)
		dst[p+2] = lhufDecodeSymbolX2(b, dt, dtLog)
		dst[p+3] = lhufDecodeSymbolX2(b, dt, dtLog)
		p += 4
	}
	for b.reload() == lbitUnfinished && p < pEnd {
		dst[p] = lhufDecodeSymbolX2(b, dt, dtLog)
		p++
	}
	for p < pEnd {
		dst[p] = lhufDecodeSymbolX2(b, dt, dtLog)
		p++
	}
}

// HUF_decompress1X2_usingDTable, v0.5 alone refusing an output no larger than its input
func lhufDecompress1X2UsingDTable(dst []byte, dstSize int, src []byte, dt []lhufDEltX2, dtLog uint32, v05 bool) (int, ErrorCode) {
	var bitD lbitD
	if v05 && dstSize <= len(src) {
		return 0, ErrDstSizeTooSmall
	}
	if _, e := bitD.init(src); e != 0 {
		return 0, e
	}
	lhufDecodeStreamX2(dst, 0, &bitD, dstSize, dt, dtLog)
	if !bitD.endOfDStream() {
		return 0, ErrCorruptionDetected
	}
	return dstSize, 0
}

// Four interleaved streams of a Huffman block, their start offsets and the bit readers over them
type lhuf4 struct {
	bitD                         [4]lbitD
	op                           [4]int
	opStart2, opStart3, opStart4 int
}

// Stream split shared by every legacy four stream decoder, v0.5 and v0.6 semantics
func (h *lhuf4) init(dstSize int, src []byte) ErrorCode {
	cSrcSize := uint64(len(src))
	length1 := uint64(readLE16(src, 0))
	length2 := uint64(readLE16(src, 2))
	length3 := uint64(readLE16(src, 4))
	istart1 := uint64(6)
	istart2 := istart1 + length1
	istart3 := istart2 + length2
	istart4 := istart3 + length3
	segmentSize := (dstSize + 3) / 4
	h.opStart2 = segmentSize
	h.opStart3 = h.opStart2 + segmentSize
	h.opStart4 = h.opStart3 + segmentSize
	h.op = [4]int{0, h.opStart2, h.opStart3, h.opStart4}
	length4 := cSrcSize - (length1 + length2 + length3 + 6)
	if length4 > cSrcSize {
		return ErrCorruptionDetected
	}
	if _, e := h.bitD[0].init(src[istart1:istart2]); e != 0 {
		return e
	}
	if _, e := h.bitD[1].init(src[istart2:istart3]); e != 0 {
		return e
	}
	if _, e := h.bitD[2].init(src[istart3:istart4]); e != 0 {
		return e
	}
	if _, e := h.bitD[3].init(src[istart4 : istart4+length4]); e != 0 {
		return e
	}
	return 0
}

func (h *lhuf4) reloadAll() int {
	return h.bitD[0].reload() | h.bitD[1].reload() | h.bitD[2].reload() | h.bitD[3].reload()
}

func (h *lhuf4) allEnded() bool {
	e := true
	for i := range h.bitD {
		e = e && h.bitD[i].endOfDStream()
	}
	return e
}

func lhufDecompress4X2UsingDTable(dst []byte, dstSize int, src []byte, dt []lhufDEltX2, dtLog uint32) (int, ErrorCode) {
	if len(src) < 10 {
		return 0, ErrCorruptionDetected
	}
	var h lhuf4
	if e := h.init(dstSize, src); e != 0 {
		return 0, e
	}
	oend := dstSize
	endSignal := h.reloadAll()
	for endSignal == lbitUnfinished && h.op[3] < oend-7 {
		for k := 0; k < 4; k++ {
			for s := 0; s < 4; s++ {
				dst[h.op[s]] = lhufDecodeSymbolX2(&h.bitD[s], dt, dtLog)
				h.op[s]++
			}
		}
		endSignal = h.reloadAll()
	}
	if h.op[0] > h.opStart2 || h.op[1] > h.opStart3 || h.op[2] > h.opStart4 {
		return 0, ErrCorruptionDetected
	}
	lhufDecodeStreamX2(dst, h.op[0], &h.bitD[0], h.opStart2, dt, dtLog)
	lhufDecodeStreamX2(dst, h.op[1], &h.bitD[1], h.opStart3, dt, dtLog)
	lhufDecodeStreamX2(dst, h.op[2], &h.bitD[2], h.opStart4, dt, dtLog)
	lhufDecodeStreamX2(dst, h.op[3], &h.bitD[3], oend, dt, dtLog)
	if !h.allEnded() {
		return 0, ErrCorruptionDetected
	}
	return dstSize, 0
}

func lhufDecompress4X2(dst []byte, dstSize int, src []byte, v05 bool) (int, ErrorCode) {
	t := newLhufDTableX2(lhufMaxTableLog)
	hSize, e := lhufReadDTableX2(t, src, v05)
	if e != 0 {
		return 0, e
	}
	if hSize >= len(src) {
		return 0, ErrSrcSizeWrong
	}
	return lhufDecompress4X2UsingDTable(dst, dstSize, src[hSize:], t.dt, uint32(t.log))
}

type lhufRankVal [lhufAbsoluteMaxTableLog][lhufAbsoluteMaxTableLog + 1]uint32

// HUF_fillDTableX4Level2 of v0.5 and v0.6 over the sub table starting at base
func lhufFillDTableX4Level2(dt []lhufDEltX4, sizeLog, consumed uint32, rankValOrigin *[lhufAbsoluteMaxTableLog + 1]uint32, minWeight int32, sortedSymbols []lhufSortedSymbol, nbBitsBaseline uint32, baseSeq uint16) {
	rankVal := *rankValOrigin
	if minWeight > 1 {
		skipSize := rankVal[minWeight]
		d := lhufDEltX4{sequence: baseSeq, nbBits: byte(consumed), length: 1}
		for i := uint32(0); i < skipSize; i++ {
			dt[i] = d
		}
	}
	for s := range sortedSymbols {
		symbol := uint32(sortedSymbols[s].symbol)
		weight := uint32(sortedSymbols[s].weight)
		nbBits := nbBitsBaseline - weight
		length := uint32(1) << ((sizeLog - nbBits) & 31)
		start := rankVal[weight]
		i := start
		end := start + length
		d := lhufDEltX4{sequence: uint16(uint32(baseSeq) + symbol<<8), nbBits: byte(nbBits + consumed), length: 2}
		for {
			dt[i] = d
			i++
			if i >= end {
				break
			}
		}
		rankVal[weight] += length
	}
}

// HUF_fillDTableX4 of v0.5 and v0.6
func lhufFillDTableX4(dt []lhufDEltX4, targetLog uint32, sortedList []lhufSortedSymbol, rankStart []uint32, rankValOrigin *lhufRankVal, maxWeight, nbBitsBaseline uint32) {
	rankVal := rankValOrigin[0]
	scaleLog := int32(nbBitsBaseline - targetLog)
	minBits := nbBitsBaseline - maxWeight
	for s := range sortedList {
		symbol := uint16(sortedList[s].symbol)
		weight := uint32(sortedList[s].weight)
		nbBits := nbBitsBaseline - weight
		start := rankVal[weight]
		length := uint32(1) << ((targetLog - nbBits) & 31)
		if targetLog-nbBits >= minBits {
			minWeight := int32(nbBits) + scaleLog
			if minWeight < 1 {
				minWeight = 1
			}
			sortedRank := rankStart[minWeight]
			lhufFillDTableX4Level2(dt[start:], targetLog-nbBits, nbBits, &rankValOrigin[nbBits], minWeight, sortedList[sortedRank:], nbBitsBaseline, symbol)
		} else {
			end := start + length
			d := lhufDEltX4{sequence: symbol, nbBits: byte(nbBits), length: 1}
			for i := start; i < end; i++ {
				dt[i] = d
			}
		}
		rankVal[weight] += length
	}
}

func lhufReadDTableX4(t *lhufDTableX4, src []byte, v05 bool) (int, ErrorCode) {
	return lhufReadX4Cells(t.dt, t.log, src, v05)
}

// Double symbol cells of a tree description filling a table of memLog bits
func lhufReadX4Cells(dt []lhufDEltX4, memLog uint32, src []byte, v05 bool) (int, ErrorCode) {
	var weightList [lhufMaxSymbolValue + 1]byte
	var sortedSymbol [lhufMaxSymbolValue + 1]lhufSortedSymbol
	var rankStats [lhufAbsoluteMaxTableLog + 1]uint32
	var rankStart0 [lhufAbsoluteMaxTableLog + 2]uint32
	rankStart := rankStart0[1:]
	var rankVal lhufRankVal
	var tableLog, nbSymbols uint32
	if memLog > lhufAbsoluteMaxTableLog {
		return 0, ErrTableLogTooLarge
	}
	iSize, e := lhufReadStats(weightList[:], lhufMaxSymbolValue+1, &rankStats, &nbSymbols, &tableLog, src, v05)
	if e != 0 {
		return 0, e
	}
	if tableLog > memLog {
		return 0, ErrTableLogTooLarge
	}
	maxW := tableLog
	for rankStats[maxW] == 0 {
		maxW--
	}
	nextRankStart := uint32(0)
	for w := uint32(1); w <= maxW; w++ {
		current := nextRankStart
		nextRankStart += rankStats[w]
		rankStart[w] = current
	}
	rankStart[0] = nextRankStart
	sizeOfSort := nextRankStart
	for s := uint32(0); s < nbSymbols; s++ {
		w := weightList[s]
		r := rankStart[w]
		rankStart[w]++
		sortedSymbol[r] = lhufSortedSymbol{symbol: byte(s), weight: w}
	}
	rankStart[0] = 0
	minBits := tableLog + 1 - maxW
	nextRankVal := uint32(0)
	rescale := int32(memLog-tableLog) - 1
	rankVal0 := &rankVal[0]
	for w := uint32(1); w <= maxW; w++ {
		current := nextRankVal
		nextRankVal += rankStats[w] << (uint32(int32(w)+rescale) & 31)
		rankVal0[w] = current
	}
	for consumed := minBits; consumed <= memLog-minBits; consumed++ {
		for w := uint32(1); w <= maxW; w++ {
			rankVal[consumed][w] = rankVal0[w] >> consumed
		}
	}
	lhufFillDTableX4(dt, memLog, sortedSymbol[:sizeOfSort], rankStart0[:], &rankVal, maxW, tableLog+1)
	return iSize, 0
}

func lhufDecodeSymbolX4(dst []byte, op int, b *lbitD, dt []lhufDEltX4, dtLog uint32) int {
	val := b.lookBitsFast(dtLog)
	dst[op] = byte(dt[val].sequence)
	dst[op+1] = byte(dt[val].sequence >> 8)
	b.skipBits(uint32(dt[val].nbBits))
	return int(dt[val].length)
}

func lhufDecodeLastSymbolX4(dst []byte, op int, b *lbitD, dt []lhufDEltX4, dtLog uint32) int {
	val := b.lookBitsFast(dtLog)
	dst[op] = byte(dt[val].sequence)
	if dt[val].length == 1 {
		b.skipBits(uint32(dt[val].nbBits))
	} else if b.bitsConsumed < 64 {
		b.skipBits(uint32(dt[val].nbBits))
		if b.bitsConsumed > 64 {
			b.bitsConsumed = 64
		}
	}
	return 1
}

// HUF_decodeStreamX4 filling dst from p up to pEnd
func lhufDecodeStreamX4(dst []byte, p int, b *lbitD, pEnd int, dt []lhufDEltX4, dtLog uint32) {
	for b.reload() == lbitUnfinished && p < pEnd-7 {
		p += lhufDecodeSymbolX4(dst, p, b, dt, dtLog)
		p += lhufDecodeSymbolX4(dst, p, b, dt, dtLog)
		p += lhufDecodeSymbolX4(dst, p, b, dt, dtLog)
		p += lhufDecodeSymbolX4(dst, p, b, dt, dtLog)
	}
	for b.reload() == lbitUnfinished && p <= pEnd-2 {
		p += lhufDecodeSymbolX4(dst, p, b, dt, dtLog)
	}
	for p <= pEnd-2 {
		p += lhufDecodeSymbolX4(dst, p, b, dt, dtLog)
	}
	if p < pEnd {
		lhufDecodeLastSymbolX4(dst, p, b, dt, dtLog)
	}
}

func lhufDecompress1X4UsingDTable(dst []byte, dstSize int, src []byte, dt []lhufDEltX4, dtLog uint32) (int, ErrorCode) {
	var bitD lbitD
	if _, e := bitD.init(src); e != 0 {
		return 0, e
	}
	lhufDecodeStreamX4(dst, 0, &bitD, dstSize, dt, dtLog)
	if !bitD.endOfDStream() {
		return 0, ErrCorruptionDetected
	}
	return dstSize, 0
}

func lhufDecompress4X4UsingDTable(dst []byte, dstSize int, src []byte, dt []lhufDEltX4, dtLog uint32) (int, ErrorCode) {
	if len(src) < 10 {
		return 0, ErrCorruptionDetected
	}
	var h lhuf4
	if e := h.init(dstSize, src); e != 0 {
		return 0, e
	}
	oend := dstSize
	endSignal := h.reloadAll()
	for endSignal == lbitUnfinished && h.op[3] < oend-7 {
		for k := 0; k < 4; k++ {
			for s := 0; s < 4; s++ {
				h.op[s] += lhufDecodeSymbolX4(dst, h.op[s], &h.bitD[s], dt, dtLog)
			}
		}
		endSignal = h.reloadAll()
	}
	if h.op[0] > h.opStart2 || h.op[1] > h.opStart3 || h.op[2] > h.opStart4 {
		return 0, ErrCorruptionDetected
	}
	lhufDecodeStreamX4(dst, h.op[0], &h.bitD[0], h.opStart2, dt, dtLog)
	lhufDecodeStreamX4(dst, h.op[1], &h.bitD[1], h.opStart3, dt, dtLog)
	lhufDecodeStreamX4(dst, h.op[2], &h.bitD[2], h.opStart4, dt, dtLog)
	lhufDecodeStreamX4(dst, h.op[3], &h.bitD[3], oend, dt, dtLog)
	if !h.allEnded() {
		return 0, ErrCorruptionDetected
	}
	return dstSize, 0
}

func lhufDecompress4X4(dst []byte, dstSize int, src []byte, v05 bool) (int, ErrorCode) {
	t := newLhufDTableX4(lhufMaxTableLog)
	hSize, e := lhufReadDTableX4(t, src, v05)
	if e != 0 {
		return 0, e
	}
	if hSize >= len(src) {
		return 0, ErrSrcSizeWrong
	}
	return lhufDecompress4X4UsingDTable(dst, dstSize, src[hSize:], t.dt, t.log)
}

type lhufAlgoTime struct{ tableTime, decode256Time uint32 }

var lhufAlgoTimes = [16][3]lhufAlgoTime{
	{{0, 0}, {1, 1}, {2, 2}},
	{{0, 0}, {1, 1}, {2, 2}},
	{{38, 130}, {1313, 74}, {2151, 38}},
	{{448, 128}, {1353, 74}, {2238, 41}},
	{{556, 128}, {1353, 74}, {2238, 47}},
	{{714, 128}, {1418, 74}, {2436, 53}},
	{{883, 128}, {1437, 74}, {2464, 61}},
	{{897, 128}, {1515, 75}, {2622, 68}},
	{{926, 128}, {1613, 75}, {2730, 75}},
	{{947, 128}, {1729, 77}, {3359, 77}},
	{{1107, 128}, {2083, 81}, {4006, 84}},
	{{1177, 128}, {2379, 87}, {4785, 88}},
	{{1242, 128}, {2415, 93}, {5155, 84}},
	{{1349, 128}, {2644, 106}, {5260, 106}},
	{{1455, 128}, {2422, 124}, {4174, 124}},
	{{722, 128}, {1891, 145}, {1936, 146}},
}

// Decoder choice of HUF_decompress from the compression ratio, true for the double symbol one
func lhufSelectX4(dstSize, cSrcSize int) bool {
	q := uint32(uint64(cSrcSize) * 16 / uint64(dstSize))
	d256 := uint32(dstSize >> 8)
	var dtime [3]uint32
	for n := 0; n < 3; n++ {
		dtime[n] = lhufAlgoTimes[q][n].tableTime + lhufAlgoTimes[q][n].decode256Time*d256
	}
	dtime[1] += dtime[1] >> 4
	return dtime[1] < dtime[0]
}

func v05HufDecompress(dst []byte, dstSize int, src []byte) (int, ErrorCode) {
	if dstSize == 0 {
		return 0, ErrDstSizeTooSmall
	}
	if len(src) >= dstSize {
		return 0, ErrCorruptionDetected
	}
	if len(src) == 1 {
		for i := 0; i < dstSize; i++ {
			dst[i] = src[0]
		}
		return dstSize, 0
	}
	if lhufSelectX4(dstSize, len(src)) {
		return lhufDecompress4X4(dst, dstSize, src, true)
	}
	return lhufDecompress4X2(dst, dstSize, src, true)
}

func v06HufDecompress(dst []byte, dstSize int, src []byte) (int, ErrorCode) {
	if dstSize == 0 {
		return 0, ErrDstSizeTooSmall
	}
	if len(src) > dstSize {
		return 0, ErrCorruptionDetected
	}
	if len(src) == dstSize {
		copy(dst[:dstSize], src)
		return dstSize, 0
	}
	if len(src) == 1 {
		for i := 0; i < dstSize; i++ {
			dst[i] = src[0]
		}
		return dstSize, 0
	}
	if lhufSelectX4(dstSize, len(src)) {
		return lhufDecompress4X4(dst, dstSize, src, false)
	}
	return lhufDecompress4X2(dst, dstSize, src, false)
}

func lhufDecompress1X2(dst []byte, dstSize int, src []byte, v05 bool) (int, ErrorCode) {
	t := newLhufDTableX2(lhufMaxTableLog)
	hSize, e := lhufReadDTableX2(t, src, v05)
	if e != 0 {
		return 0, e
	}
	if hSize >= len(src) {
		return 0, ErrSrcSizeWrong
	}
	return lhufDecompress1X2UsingDTable(dst, dstSize, src[hSize:], t.dt, uint32(t.log), v05)
}

// HUFv07_DTable, the descriptor bytes of its first slot and the cells of either table type
type v07HufDTable struct {
	maxTableLog, tableType, tableLog byte
	x2                               []lhufDEltX2
	x4                               []lhufDEltX4
}

func newV07HufDTable(maxTableLog byte) *v07HufDTable {
	return &v07HufDTable{maxTableLog: maxTableLog, x2: make([]lhufDEltX2, 2<<maxTableLog), x4: make([]lhufDEltX4, 1<<maxTableLog)}
}

func (t *v07HufDTable) readX2(src []byte) (int, ErrorCode) {
	iSize, _, e := lhufReadX2Cells(t.x2, uint32(t.maxTableLog)+1, src, false, func(tableLog uint32) {
		t.tableType, t.tableLog = 0, byte(tableLog)
	})
	return iSize, e
}

func (t *v07HufDTable) readX4(src []byte) (int, ErrorCode) {
	iSize, e := lhufReadX4Cells(t.x4, uint32(t.maxTableLog), src, false)
	if e != 0 {
		return 0, e
	}
	t.tableLog, t.tableType = t.maxTableLog, 1
	return iSize, 0
}

// HUFv07_selectDecoder, true for the double symbol decoder
func v07HufSelectX4(dstSize, cSrcSize int) bool {
	q := uint32(uint64(cSrcSize) * 16 / uint64(dstSize))
	d256 := uint32(dstSize >> 8)
	dTime0 := lhufAlgoTimes[q][0].tableTime + lhufAlgoTimes[q][0].decode256Time*d256
	dTime1 := lhufAlgoTimes[q][1].tableTime + lhufAlgoTimes[q][1].decode256Time*d256
	dTime1 += dTime1 >> 3
	return dTime1 < dTime0
}

// HUFv07_decompress1X2_DCtx
func (t *v07HufDTable) decompress1X2(dst []byte, dstSize int, src []byte) (int, ErrorCode) {
	hSize, e := t.readX2(src)
	if e != 0 {
		return 0, e
	}
	if hSize >= len(src) {
		return 0, ErrSrcSizeWrong
	}
	return lhufDecompress1X2UsingDTable(dst, dstSize, src[hSize:], t.x2, uint32(t.tableLog), false)
}

// HUFv07_decompress4X_hufOnly
func (t *v07HufDTable) decompress4XHufOnly(dst []byte, dstSize int, src []byte) (int, ErrorCode) {
	if dstSize == 0 {
		return 0, ErrDstSizeTooSmall
	}
	if len(src) >= dstSize || len(src) <= 1 {
		return 0, ErrCorruptionDetected
	}
	if v07HufSelectX4(dstSize, len(src)) {
		hSize, e := t.readX4(src)
		if e != 0 {
			return 0, e
		}
		if hSize >= len(src) {
			return 0, ErrSrcSizeWrong
		}
		return lhufDecompress4X4UsingDTable(dst, dstSize, src[hSize:], t.x4, uint32(t.tableLog))
	}
	hSize, e := t.readX2(src)
	if e != 0 {
		return 0, e
	}
	if hSize >= len(src) {
		return 0, ErrSrcSizeWrong
	}
	return lhufDecompress4X2UsingDTable(dst, dstSize, src[hSize:], t.x2, uint32(t.tableLog))
}

// HUFv07_decompress1X4_usingDTable, refused unless the table holds double symbol cells
func (t *v07HufDTable) decompress1X4UsingDTable(dst []byte, dstSize int, src []byte) (int, ErrorCode) {
	if t.tableType != 1 {
		return 0, ErrGeneric
	}
	return lhufDecompress1X4UsingDTable(dst, dstSize, src, t.x4, uint32(t.tableLog))
}
