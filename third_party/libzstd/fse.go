package libzstd

import "math/bits"

const (
	fseMinTableLog         = 5
	fseTableLogAbsoluteMax = 15
	fseMaxSymbolValue      = 255
	fseMaxTableLog         = 12
	hufTableLogMax         = 12
	hufSymbolValueMax      = 255
)

// FSE_decode_t cell of a decoding table
type fseDecode struct {
	newState uint16
	symbol   byte
	nbBits   byte
}

// FSE_DTable with its header fields apart
type fseDTable struct {
	tableLog uint32
	fastMode bool
	table    []fseDecode
}

// FSE_DState_t
type fseDState struct {
	state uint64
	table []fseDecode
}

func fseDTableSizeU32(tableLog uint32) int { return 1 + (1 << tableLog) }

func fseBuildDTableWkspSize(tableLog, maxSymbolValue uint32) int {
	return 2*int(maxSymbolValue+1) + (1 << tableLog) + 8
}

func fseDecompressWkspSizeU32(tableLog, maxSymbolValue uint32) int {
	return fseDTableSizeU32(tableLog) + 1 + (fseBuildDTableWkspSize(tableLog, maxSymbolValue)+3)/4 + (fseMaxSymbolValue+1)/2 + 1
}

// HUF_READ_STATS_WORKSPACE_SIZE in bytes
var hufReadStatsWorkspaceSize = fseDecompressWkspSizeU32(6, hufTableLogMax-1) * 4

// FSE_readNCount, the normalized counts of a table description and the bytes it took
func fseReadNCount(normalizedCounter []int16, maxSVPtr, tableLogPtr *uint32, header []byte) (int, ErrorCode) {
	hbSize := len(header)
	if hbSize < 8 {
		var buffer [8]byte
		copy(buffer[:], header)
		countSize, err := fseReadNCount(normalizedCounter, maxSVPtr, tableLogPtr, buffer[:])
		if err != 0 {
			return 0, err
		}
		if countSize > hbSize {
			return 0, ErrCorruptionDetected
		}
		return countSize, 0
	}
	iend, ip := hbSize, 0
	maxSV1 := *maxSVPtr + 1
	clear(normalizedCounter[:maxSV1])
	bitStream := readLE32(header, ip)
	nbBits := int(bitStream&0xF) + fseMinTableLog
	if nbBits > fseTableLogAbsoluteMax {
		return 0, ErrTableLogTooLarge
	}
	bitStream >>= 4
	bitCount := 4
	*tableLogPtr = uint32(nbBits)
	remaining := (1 << nbBits) + 1
	threshold := 1 << nbBits
	nbBits++
	charnum := uint32(0)
	previous0 := false
	advance := func() {
		if ip <= iend-7 || ip+(bitCount>>3) <= iend-4 {
			ip += bitCount >> 3
			bitCount &= 7
		} else {
			bitCount -= 8 * (iend - 4 - ip)
			bitCount &= 31
			ip = iend - 4
		}
		bitStream = readLE32(header, ip) >> uint(bitCount)
	}
	for {
		if previous0 {
			repeats := bits.TrailingZeros32(^bitStream|0x80000000) >> 1
			for repeats >= 12 {
				charnum += 3 * 12
				if ip <= iend-7 {
					ip += 3
				} else {
					bitCount -= 8 * (iend - 7 - ip)
					bitCount &= 31
					ip = iend - 4
				}
				bitStream = readLE32(header, ip) >> uint(bitCount)
				repeats = bits.TrailingZeros32(^bitStream|0x80000000) >> 1
			}
			charnum += uint32(3 * repeats)
			bitStream >>= uint(2 * repeats)
			bitCount += 2 * repeats
			charnum += bitStream & 3
			bitCount += 2
			if charnum >= maxSV1 {
				break
			}
			advance()
		}
		max := (2*threshold - 1) - remaining
		var count int
		if bitStream&uint32(threshold-1) < uint32(max) {
			count = int(bitStream & uint32(threshold-1))
			bitCount += nbBits - 1
		} else {
			count = int(bitStream & uint32(2*threshold-1))
			if count >= threshold {
				count -= max
			}
			bitCount += nbBits
		}
		count--
		if count >= 0 {
			remaining -= count
		} else {
			remaining += count
		}
		normalizedCounter[charnum] = int16(count)
		charnum++
		previous0 = count == 0
		if remaining < threshold {
			if remaining <= 1 {
				break
			}
			nbBits = int(highbit32(uint32(remaining))) + 1
			threshold = 1 << (nbBits - 1)
		}
		if charnum >= maxSV1 {
			break
		}
		advance()
	}
	if remaining != 1 {
		return 0, ErrCorruptionDetected
	}
	if charnum > maxSV1 {
		return 0, ErrMaxSymbolValueTooSmall
	}
	if bitCount > 32 {
		return 0, ErrCorruptionDetected
	}
	*maxSVPtr = charnum - 1
	ip += (bitCount + 7) >> 3
	return ip, 0
}

func fseTableStep(tableSize uint32) uint32 { return (tableSize >> 1) + (tableSize >> 3) + 3 }

// FSE_buildDTable_internal filling dt for the counts, wkspSize the bytes its C workspace holds
func fseBuildDTable(dt *fseDTable, normalizedCounter []int16, maxSymbolValue, tableLog uint32, wkspSize int) ErrorCode {
	maxSV1 := maxSymbolValue + 1
	tableSize := uint32(1) << tableLog
	highThreshold := tableSize - 1
	if fseBuildDTableWkspSize(tableLog, maxSymbolValue) > wkspSize {
		return ErrMaxSymbolValueTooLarge
	}
	if maxSymbolValue > fseMaxSymbolValue {
		return ErrMaxSymbolValueTooLarge
	}
	if tableLog > fseMaxTableLog {
		return ErrTableLogTooLarge
	}
	if cap(dt.table) < int(tableSize) {
		dt.table = make([]fseDecode, tableSize)
	}
	dt.table = dt.table[:tableSize]
	tableDecode := dt.table
	var symbolNext [fseMaxSymbolValue + 1]uint16
	dt.tableLog, dt.fastMode = tableLog, true
	largeLimit := int16(1 << (tableLog - 1))
	for s := uint32(0); s < maxSV1; s++ {
		if normalizedCounter[s] == -1 {
			tableDecode[highThreshold].symbol = byte(s)
			highThreshold--
			symbolNext[s] = 1
		} else {
			if normalizedCounter[s] >= largeLimit {
				dt.fastMode = false
			}
			symbolNext[s] = uint16(normalizedCounter[s])
		}
	}
	tableMask := tableSize - 1
	step := fseTableStep(tableSize)
	if highThreshold == tableSize-1 {
		position := uint32(0)
		for s := uint32(0); s < maxSV1; s++ {
			for i := 0; i < int(normalizedCounter[s]); i++ {
				tableDecode[position].symbol = byte(s)
				position = (position + step) & tableMask
			}
		}
	} else {
		position := uint32(0)
		for s := uint32(0); s < maxSV1; s++ {
			for i := 0; i < int(normalizedCounter[s]); i++ {
				tableDecode[position].symbol = byte(s)
				position = (position + step) & tableMask
				for position > highThreshold {
					position = (position + step) & tableMask
				}
			}
		}
		if position != 0 {
			return ErrGeneric
		}
	}
	for u := uint32(0); u < tableSize; u++ {
		symbol := tableDecode[u].symbol
		nextState := uint32(symbolNext[symbol])
		symbolNext[symbol]++
		nb := tableLog - highbit32(nextState)
		tableDecode[u].nbBits = byte(nb)
		tableDecode[u].newState = uint16((nextState << nb) - tableSize)
	}
	return 0
}

// FSE_initDState
func (st *fseDState) init(d *bitDStream, dt *fseDTable) {
	st.state = d.readBits(dt.tableLog)
	d.reload()
	st.table = dt.table
}

// FSE_decodeSymbol and FSE_decodeSymbolFast
func (st *fseDState) decodeSymbol(d *bitDStream, fast bool) byte {
	info := st.table[st.state]
	var lowBits uint64
	if fast {
		lowBits = d.readBitsFast(uint32(info.nbBits))
	} else {
		lowBits = d.readBits(uint32(info.nbBits))
	}
	st.state = uint64(info.newState) + lowBits
	return info.symbol
}

// FSE_decompress_usingDTable_generic into dst, the count written
func fseDecompressUsingDTable(dst []byte, src []byte, dt *fseDTable, fast bool) (int, ErrorCode) {
	op, omax := 0, len(dst)
	olimit := omax - 3
	var bitD bitDStream
	var state1, state2 fseDState
	if _, err := bitD.init(src, 0, len(src)); err != 0 {
		return 0, err
	}
	state1.init(&bitD, dt)
	state2.init(&bitD, dt)
	if bitD.reload() == bitOverflow {
		return 0, ErrCorruptionDetected
	}
	for ; bitD.reload() == bitUnfinished && op < olimit; op += 4 {
		dst[op] = state1.decodeSymbol(&bitD, fast)
		dst[op+1] = state2.decodeSymbol(&bitD, fast)
		dst[op+2] = state1.decodeSymbol(&bitD, fast)
		dst[op+3] = state2.decodeSymbol(&bitD, fast)
	}
	for {
		if op > omax-2 {
			return 0, ErrDstSizeTooSmall
		}
		dst[op] = state1.decodeSymbol(&bitD, fast)
		op++
		if bitD.reload() == bitOverflow {
			dst[op] = state2.decodeSymbol(&bitD, fast)
			op++
			break
		}
		if op > omax-2 {
			return 0, ErrDstSizeTooSmall
		}
		dst[op] = state2.decodeSymbol(&bitD, fast)
		op++
		if bitD.reload() == bitOverflow {
			dst[op] = state1.decodeSymbol(&bitD, fast)
			op++
			break
		}
	}
	return op, 0
}

// FSE_decompress_wksp of a table description followed by its stream, wkspSize the bytes of the C workspace
func fseDecompressWksp(dst []byte, src []byte, maxLog uint32, wkspSize int) (int, ErrorCode) {
	var ncount [fseMaxSymbolValue + 1]int16
	var tableLog uint32
	maxSymbolValue := uint32(fseMaxSymbolValue)
	if wkspSize < 2*(fseMaxSymbolValue+1) {
		return 0, ErrGeneric
	}
	nCountLength, err := fseReadNCount(ncount[:], &maxSymbolValue, &tableLog, src)
	if err != 0 {
		return 0, err
	}
	if tableLog > maxLog {
		return 0, ErrTableLogTooLarge
	}
	src = src[nCountLength:]
	if fseDecompressWkspSizeU32(tableLog, maxSymbolValue)*4 > wkspSize {
		return 0, ErrTableLogTooLarge
	}
	wkspSize -= 2*(fseMaxSymbolValue+1) + fseDTableSizeU32(tableLog)*4
	var dt fseDTable
	if err := fseBuildDTable(&dt, ncount[:], maxSymbolValue, tableLog, wkspSize); err != 0 {
		return 0, err
	}
	return fseDecompressUsingDTable(dst, src, &dt, dt.fastMode)
}

// HUF_readStats, the weights of a Huffman tree description, its rank counts, symbol count, table log and size
func hufReadStats(huffWeight []byte, rankStats *[hufTableLogMax + 1]uint32, nbSymbolsPtr, tableLogPtr *uint32, src []byte) (int, ErrorCode) {
	hwSize := len(huffWeight)
	srcSize := len(src)
	if srcSize == 0 {
		return 0, ErrSrcSizeWrong
	}
	iSize := int(src[0])
	var oSize int
	if iSize >= 128 {
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
	} else {
		if iSize+1 > srcSize {
			return 0, ErrSrcSizeWrong
		}
		n, err := fseDecompressWksp(huffWeight[:hwSize-1], src[1:1+iSize], 6, hufReadStatsWorkspaceSize)
		if err != 0 {
			return 0, err
		}
		oSize = n
	}
	*rankStats = [hufTableLogMax + 1]uint32{}
	weightTotal := uint32(0)
	for n := 0; n < oSize; n++ {
		if huffWeight[n] > hufTableLogMax {
			return 0, ErrCorruptionDetected
		}
		rankStats[huffWeight[n]]++
		weightTotal += (1 << huffWeight[n]) >> 1
	}
	if weightTotal == 0 {
		return 0, ErrCorruptionDetected
	}
	tableLog := highbit32(weightTotal) + 1
	if tableLog > hufTableLogMax {
		return 0, ErrCorruptionDetected
	}
	*tableLogPtr = tableLog
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
	return iSize + 1, 0
}
