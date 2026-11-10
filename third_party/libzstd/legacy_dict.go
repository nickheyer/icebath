package libzstd

const (
	v05DictMagic = 0xEC30A435
	v06DictMagic = 0xEC30A436
	v07DictMagic = 0xEC30A437
	lDictOrigin  = -(1 << 40)
)

// ZSTDv0X_refDictContent, the dictionary content given its own address range below every output address
func lRefDictContent(previousDstEnd, base, vBase, dictEnd *int, dictSize int) {
	*dictEnd = *previousDstEnd
	*vBase = lDictOrigin - (*previousDstEnd - *base)
	*base = lDictOrigin
	*previousDstEnd = lDictOrigin + dictSize
}

// FSE tables of a legacy dictionary in offset, match length, literal length order, the bytes read
func lLoadFSETables(dict []byte, tables [3]*lfseDTable, maxes, logs [3]uint32, headerFirst bool) (int, ErrorCode) {
	p := 0
	for k, dt := range tables {
		var norm [v05MaxML + 1]int16
		max, log := maxes[k], uint32(0)
		n, e := lfseReadNCount(norm[:], &max, &log, dict[p:])
		if e != 0 || log > logs[k] {
			return 0, ErrDictionaryCorrupted
		}
		if e := lfseBuildDTable(dt, norm[:], max, log, headerFirst); e != 0 {
			return 0, ErrDictionaryCorrupted
		}
		p += n
	}
	return p, 0
}

// ZSTDv05_loadEntropy, the Huffman and FSE tables a v0.5 dictionary carries
func (d *v05DCtx) loadEntropy(dict []byte) (int, ErrorCode) {
	hSize, e := lhufReadDTableX4(d.hufTableX4, dict, true)
	if e != 0 {
		return 0, ErrDictionaryCorrupted
	}
	n, e := lLoadFSETables(dict[hSize:], [3]*lfseDTable{d.offTable, d.mlTable, d.llTable}, [3]uint32{v05MaxOff, v05MaxML, v05MaxLL}, [3]uint32{v05OffLog, v05MLLog, v05LLLog}, false)
	if e != 0 {
		return 0, e
	}
	d.flagStaticTables = true
	return hSize + n, 0
}

// ZSTDv05_decompress_insertDictionary, raw content unless the v0.5 dictionary magic leads it
func (d *v05DCtx) insertDictionary(dict []byte) ErrorCode {
	if readLE32(dict, 0) != v05DictMagic {
		lRefDictContent(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, len(dict))
		d.dict = dict
		return 0
	}
	dict = dict[4:]
	eSize, e := d.loadEntropy(dict)
	if e != 0 {
		return ErrDictionaryCorrupted
	}
	lRefDictContent(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, len(dict)-eSize)
	d.dict = dict[eSize:]
	return 0
}

// ZSTDv05_decompressBegin_usingDict
func (d *v05DCtx) decompressBeginUsingDict(dict []byte) ErrorCode {
	d.decompressBegin()
	if len(dict) > 0 && d.insertDictionary(dict) != 0 {
		return ErrDictionaryCorrupted
	}
	return 0
}

// Pre-built Huffman literals of a v0.5 block, usable once a dictionary loaded its table
func (d *v05DCtx) literalsRepeat() func(dst []byte, dstSize int, src []byte) ErrorCode {
	if !d.flagStaticTables {
		return nil
	}
	return func(dst []byte, dstSize int, src []byte) ErrorCode {
		_, e := lhufDecompress1X4UsingDTable(dst, dstSize, src, d.hufTableX4.dt, d.hufTableX4.log)
		return e
	}
}

// ZSTDv06_loadEntropy, the Huffman and FSE tables a v0.6 dictionary carries
func (d *v06DCtx) loadEntropy(dict []byte) (int, ErrorCode) {
	hSize, e := lhufReadDTableX4(d.hufTableX4, dict, false)
	if e != 0 {
		return 0, ErrDictionaryCorrupted
	}
	n, e := lLoadFSETables(dict[hSize:], [3]*lfseDTable{d.offTable, d.mlTable, d.llTable}, [3]uint32{v06MaxOff, v06MaxML, v06MaxLL}, [3]uint32{v06OffLog, v06MLLog, v06LLLog}, true)
	if e != 0 {
		return 0, e
	}
	d.flagRepeatTable = true
	return hSize + n, 0
}

// ZSTDv06_decompress_insertDictionary, raw content unless the v0.6 dictionary magic leads it
func (d *v06DCtx) insertDictionary(dict []byte) ErrorCode {
	if readLE32(dict, 0) != v06DictMagic {
		lRefDictContent(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, len(dict))
		d.dict = dict
		return 0
	}
	dict = dict[4:]
	eSize, e := d.loadEntropy(dict)
	if e != 0 {
		return ErrDictionaryCorrupted
	}
	lRefDictContent(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, len(dict)-eSize)
	d.dict = dict[eSize:]
	return 0
}

// ZSTDv06_decompressBegin_usingDict
func (d *v06DCtx) decompressBeginUsingDict(dict []byte) ErrorCode {
	d.decompressBegin()
	if len(dict) > 0 && d.insertDictionary(dict) != 0 {
		return ErrDictionaryCorrupted
	}
	return 0
}

// Repeated Huffman literals of a v0.6 block, usable until the first block after the dictionary decoded its sequences
func (d *v06DCtx) literalsRepeat() func(dst []byte, dstSize int, src []byte) ErrorCode {
	if !d.flagRepeatTable {
		return nil
	}
	return func(dst []byte, dstSize int, src []byte) ErrorCode {
		_, e := lhufDecompress1X4UsingDTable(dst, dstSize, src, d.hufTableX4.dt, d.hufTableX4.log)
		return e
	}
}

// ZSTDv07_loadEntropy, the Huffman and FSE tables and repeat offsets a v0.7 dictionary carries
func (d *v07DCtx) loadEntropy(dict []byte) (int, ErrorCode) {
	hSize, e := d.hufTable.readX4(dict)
	if e != 0 {
		return 0, ErrDictionaryCorrupted
	}
	n, e := lLoadFSETables(dict[hSize:], [3]*lfseDTable{d.offTable, d.mlTable, d.llTable}, [3]uint32{v06MaxOff, v06MaxML, v06MaxLL}, [3]uint32{v06OffLog, v06MLLog, v06LLLog}, true)
	if e != 0 {
		return 0, e
	}
	p := hSize + n
	if p+12 > len(dict) {
		return 0, ErrDictionaryCorrupted
	}
	for i := range d.rep {
		d.rep[i] = readLE32(dict, p+4*i)
		if d.rep[i] == 0 || int(d.rep[i]) >= len(dict) {
			return 0, ErrDictionaryCorrupted
		}
	}
	d.litEntropy, d.fseEntropy = true, true
	return p + 12, 0
}

// ZSTDv07_decompress_insertDictionary, raw content unless the v0.7 dictionary magic and id lead it
func (d *v07DCtx) insertDictionary(dict []byte) ErrorCode {
	if len(dict) < 8 || readLE32(dict, 0) != v07DictMagic {
		lRefDictContent(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, len(dict))
		d.dict = dict
		return 0
	}
	d.dictID = readLE32(dict, 4)
	dict = dict[8:]
	eSize, e := d.loadEntropy(dict)
	if e != 0 {
		return ErrDictionaryCorrupted
	}
	lRefDictContent(&d.previousDstEnd, &d.base, &d.vBase, &d.dictEnd, len(dict)-eSize)
	d.dict = dict[eSize:]
	return 0
}

// ZSTDv07_decompressBegin_usingDict
func (d *v07DCtx) decompressBeginUsingDict(dict []byte) ErrorCode {
	d.decompressBegin()
	if len(dict) > 0 && d.insertDictionary(dict) != 0 {
		return ErrDictionaryCorrupted
	}
	return 0
}
