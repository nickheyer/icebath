package libzstd

// FSE_readNCount of the legacy decoders with the 16 bit arithmetic of the C kept
func lfseReadNCount(normalizedCounter []int16, maxSVPtr, tableLogPtr *uint32, header []byte) (int, ErrorCode) {
	iend := len(header)
	ip := 0
	charnum := uint32(0)
	previous0 := false
	if len(header) < 4 {
		return 0, ErrSrcSizeWrong
	}
	bitStream := readLE32(header, ip)
	nbBits := int32(bitStream&0xF) + lfseMinTableLog
	if nbBits > lfseTableLogAbsoluteMax {
		return 0, ErrTableLogTooLarge
	}
	bitStream >>= 4
	bitCount := int32(4)
	*tableLogPtr = uint32(nbBits)
	remaining := int32(1)<<uint(nbBits) + 1
	threshold := int32(1) << uint(nbBits)
	nbBits++
	for remaining > 1 && charnum <= *maxSVPtr {
		if previous0 {
			n0 := charnum
			for bitStream&0xFFFF == 0xFFFF {
				n0 += 24
				if ip < iend-5 {
					ip += 2
					bitStream = readLE32(header, ip) >> (uint32(bitCount) & 31)
				} else {
					bitStream >>= 16
					bitCount += 16
				}
			}
			for bitStream&3 == 3 {
				n0 += 3
				bitStream >>= 2
				bitCount += 2
			}
			n0 += bitStream & 3
			bitCount += 2
			if n0 > *maxSVPtr {
				return 0, ErrMaxSymbolValueTooSmall
			}
			for charnum < n0 {
				normalizedCounter[charnum] = 0
				charnum++
			}
			if ip <= iend-7 || ip+int(bitCount>>3) <= iend-4 {
				ip += int(bitCount >> 3)
				bitCount &= 7
				bitStream = readLE32(header, ip) >> (uint32(bitCount) & 31)
			} else {
				bitStream >>= 2
			}
		}
		max := int16((2*threshold - 1) - remaining)
		var count int16
		if bitStream&uint32(threshold-1) < uint32(int32(max)) {
			count = int16(bitStream & uint32(threshold-1))
			bitCount += nbBits - 1
		} else {
			count = int16(bitStream & uint32(2*threshold-1))
			if int32(count) >= threshold {
				count -= max
			}
			bitCount += nbBits
		}
		count--
		if count < 0 {
			remaining -= int32(int16(-count))
		} else {
			remaining -= int32(count)
		}
		normalizedCounter[charnum] = count
		charnum++
		previous0 = count == 0
		for remaining < threshold {
			nbBits--
			threshold >>= 1
		}
		if ip <= iend-7 || ip+int(bitCount>>3) <= iend-4 {
			ip += int(bitCount >> 3)
			bitCount &= 7
		} else {
			bitCount -= int32(8 * (iend - 4 - ip))
			ip = iend - 4
		}
		bitStream = readLE32(header, ip) >> (uint32(bitCount) & 31)
	}
	if remaining != 1 {
		return 0, ErrGeneric
	}
	*maxSVPtr = charnum - 1
	ip += int((bitCount + 7) >> 3)
	if ip > len(header) {
		return 0, ErrSrcSizeWrong
	}
	return ip, 0
}

// FSE_buildDTable, v0.5 storing the table header last and the later versions before the spread
func lfseBuildDTable(dt *lfseDTable, normalizedCounter []int16, maxSymbolValue, tableLog uint32, headerFirst bool) ErrorCode {
	var symbolNext [lfseMaxSymbolValue + 1]uint16
	if maxSymbolValue > lfseMaxSymbolValue {
		return ErrMaxSymbolValueTooLarge
	}
	if tableLog > lfseMaxTableLog {
		return ErrTableLogTooLarge
	}
	tableDecode := dt.cells
	tableSize := uint32(1) << tableLog
	tableMask := tableSize - 1
	step := lfseTableStep(tableSize)
	highThreshold := tableSize - 1
	largeLimit := int16(1 << (tableLog - 1))
	noLarge := uint16(1)
	if !headerFirst {
		for i := uint32(0); i < (maxSymbolValue+1+3)/4 && int(i) < len(tableDecode); i++ {
			clearCellBytes(&tableDecode[i], i, maxSymbolValue+1)
		}
	}
	for s := uint32(0); s <= maxSymbolValue; s++ {
		if normalizedCounter[s] == -1 {
			tableDecode[highThreshold].symbol = byte(s)
			highThreshold--
			symbolNext[s] = 1
		} else {
			if normalizedCounter[s] >= largeLimit {
				noLarge = 0
			}
			symbolNext[s] = uint16(normalizedCounter[s])
		}
	}
	if headerFirst {
		dt.tableLog, dt.fastMode = uint16(tableLog), noLarge
	}
	position := uint32(0)
	for s := uint32(0); s <= maxSymbolValue; s++ {
		for i := int32(0); i < int32(normalizedCounter[s]); i++ {
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
	for i := uint32(0); i < tableSize; i++ {
		symbol := tableDecode[i].symbol
		nextState := symbolNext[symbol]
		symbolNext[symbol]++
		nb := byte(tableLog - highbit32(uint32(nextState)))
		tableDecode[i].nbBits = nb
		tableDecode[i].newState = uint16(uint32(nextState)<<nb - tableSize)
	}
	if !headerFirst {
		dt.tableLog, dt.fastMode = uint16(tableLog), noLarge
	}
	return 0
}

// The v0.5 memset over the first bytes of the cell array, count bytes from its start
func clearCellBytes(c *lfseCell, index, count uint32) {
	n := count - index*4
	if n >= 1 {
		c.newState &= 0xFF00
	}
	if n >= 2 {
		c.newState = 0
	}
	if n >= 3 {
		c.symbol = 0
	}
	if n >= 4 {
		c.nbBits = 0
	}
}

func lfseBuildDTableRaw(dt *lfseDTable, nbBits uint32) ErrorCode {
	if nbBits < 1 {
		return ErrGeneric
	}
	dt.tableLog, dt.fastMode = uint16(nbBits), 1
	for s := uint32(0); s < 1<<nbBits; s++ {
		dt.cells[s] = lfseCell{newState: 0, symbol: byte(s), nbBits: byte(nbBits)}
	}
	return 0
}

// FSE_decompress_usingDTable, v0.5 ending on the stream end test and the later versions on overflow
func lfseDecompressUsingDTable(dst, src []byte, dt *lfseDTable, v05 bool) (int, ErrorCode) {
	fast := dt.fastMode != 0
	op, omax := 0, len(dst)
	olimit := omax - 3
	var bitD lbitD
	var state1, state2 lfseDState
	if _, e := bitD.init(src); e != 0 {
		return 0, e
	}
	state1.init(&bitD, dt)
	state2.init(&bitD, dt)
	get := func(s *lfseDState) byte {
		if fast {
			return s.decodeSymbolFast(&bitD)
		}
		return s.decodeSymbol(&bitD)
	}
	for ; bitD.reload() == lbitUnfinished && op < olimit; op += 4 {
		dst[op] = get(&state1)
		dst[op+1] = get(&state2)
		dst[op+2] = get(&state1)
		dst[op+3] = get(&state2)
	}
	if !v05 {
		for {
			if op > omax-2 {
				return 0, ErrDstSizeTooSmall
			}
			dst[op] = get(&state1)
			op++
			if bitD.reload() == lbitOverflow {
				dst[op] = get(&state2)
				op++
				break
			}
			if op > omax-2 {
				return 0, ErrDstSizeTooSmall
			}
			dst[op] = get(&state2)
			op++
			if bitD.reload() == lbitOverflow {
				dst[op] = get(&state1)
				op++
				break
			}
		}
		return op, 0
	}
	for {
		if bitD.reload() > lbitCompleted || op == omax || (bitD.endOfDStream() && (fast || state1.state == 0)) {
			break
		}
		dst[op] = get(&state1)
		op++
		if bitD.reload() > lbitCompleted || op == omax || (bitD.endOfDStream() && (fast || state2.state == 0)) {
			break
		}
		dst[op] = get(&state2)
		op++
	}
	if bitD.endOfDStream() && state1.state == 0 && state2.state == 0 {
		return op, 0
	}
	if op == omax {
		return 0, ErrDstSizeTooSmall
	}
	return 0, ErrCorruptionDetected
}

// FSE_decompress of a table description followed by its stream, as the Huffman weights are stored
func lfseDecompress(dst, src []byte, v05 bool) (int, ErrorCode) {
	var counting [lfseMaxSymbolValue + 1]int16
	var tableLog uint32
	maxSymbolValue := uint32(lfseMaxSymbolValue)
	if len(src) < 2 {
		return 0, ErrSrcSizeWrong
	}
	n, e := lfseReadNCount(counting[:], &maxSymbolValue, &tableLog, src)
	if e != 0 {
		return 0, e
	}
	if n >= len(src) {
		return 0, ErrSrcSizeWrong
	}
	dt := newLfseDTable(lfseMaxTableLog)
	if e := lfseBuildDTable(dt, counting[:], maxSymbolValue, tableLog, !v05); e != 0 {
		return 0, e
	}
	return lfseDecompressUsingDTable(dst, src[n:], dt, v05)
}
