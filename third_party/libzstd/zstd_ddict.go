package libzstd

const magicDictionary = 0xEC30A437

// DictContentType is ZSTD_dictContentType_e
type DictContentType int

// ZSTD_dct_auto, ZSTD_dct_rawContent and ZSTD_dct_fullDict
const (
	DictContentAuto DictContentType = iota
	DictContentRaw
	DictContentFull
)

// DDict is a ZSTD_DDict, a dictionary with its entropy tables digested
type DDict struct {
	content        *region
	dictSize       int
	entropy        entropyDTables
	dictID         uint32
	entropyPresent bool
}

// Tables of ZSTD_entropyDTables_t as the decoder allocates them
func newEntropyDTables() entropyDTables {
	return entropyDTables{llTable: seqTable{cells: make([]seqSymbol, 1<<llFSELog)},
		ofTable: seqTable{cells: make([]seqSymbol, 1<<offFSELog)}, mlTable: seqTable{cells: make([]seqSymbol, 1<<mlFSELog)},
		hufTable: newHufDTable()}
}

// NewDDict is ZSTD_createDDict_advanced, referencing dict when byRef and copying it otherwise
func NewDDict(dict []byte, byRef bool, contentType DictContentType) (*DDict, ErrorCode) {
	d := &DDict{entropy: newEntropyDTables(), dictSize: len(dict)}
	if byRef || len(dict) == 0 {
		d.content = &region{dict}
	} else {
		d.content = &region{append([]byte(nil), dict...)}
	}
	if err := d.loadEntropy(contentType); err != 0 {
		return nil, err
	}
	return d, 0
}

// ZSTD_loadEntropy_intoDDict, entropy present only for a dictionary led by the dictionary magic
func (d *DDict) loadEntropy(contentType DictContentType) ErrorCode {
	d.dictID, d.entropyPresent = 0, false
	if contentType == DictContentRaw {
		return 0
	}
	b := d.content.b
	if len(b) < 8 || readLE32(b, 0) != magicDictionary {
		if contentType == DictContentFull {
			return ErrDictionaryCorrupted
		}
		return 0
	}
	d.dictID = readLE32(b, 4)
	if _, err := loadDEntropy(&d.entropy, b); err != 0 {
		return ErrDictionaryCorrupted
	}
	d.entropyPresent = true
	return 0
}

// DictID is ZSTD_getDictID_fromDDict
func (d *DDict) DictID() uint32 { return d.dictID }

// ZSTD_loadDEntropy, the size of the dictionary header and entropy tables
func loadDEntropy(entropy *entropyDTables, dict []byte) (int, ErrorCode) {
	if len(dict) <= 8 {
		return 0, ErrDictionaryCorrupted
	}
	p := 8
	hSize, err := hufReadDTableX2(entropy.hufTable, dict[p:])
	if err != 0 {
		return 0, ErrDictionaryCorrupted
	}
	p += hSize
	for _, t := range []struct {
		dt         *seqTable
		max, log   uint32
		base       []uint32
		bitsOfCode []uint8
	}{{&entropy.ofTable, maxOff, offFSELog, ofBase[:], ofBits[:]}, {&entropy.mlTable, maxML, mlFSELog, mlBase[:], mlBits[:]},
		{&entropy.llTable, maxLL, llFSELog, llBase[:], llBits[:]}} {
		var norm [maxSeq + 1]int16
		max, log := t.max, uint32(0)
		n, err := fseReadNCount(norm[:], &max, &log, dict[p:])
		if err != 0 || max > t.max || log > t.log {
			return 0, ErrDictionaryCorrupted
		}
		buildFSETable(t.dt, norm[:], max, t.base, t.bitsOfCode, log)
		p += n
	}
	if p+12 > len(dict) {
		return 0, ErrDictionaryCorrupted
	}
	contentSize := uint64(len(dict) - (p + 12))
	for i := range entropy.rep {
		rep := readLE32(dict, p)
		p += 4
		if rep == 0 || uint64(rep) > contentSize {
			return 0, ErrDictionaryCorrupted
		}
		entropy.rep[i] = rep
	}
	return p, 0
}

// ZSTD_copyDDictParameters, the dictionary as the prefix and its tables in use
func (dc *DCtx) copyDDictParameters(d *DDict) {
	dc.dictID = d.dictID
	dc.prefixStart, dc.extDictLen = cptr{d.content, 0}, 0
	dc.dictEnd = cptr{d.content, d.dictSize}
	dc.previousDstEnd = dc.dictEnd
	if d.entropyPresent {
		dc.litEntropy, dc.fseEntropy = true, true
		dc.llTptr, dc.mlTptr, dc.ofTptr = &d.entropy.llTable, &d.entropy.mlTable, &d.entropy.ofTable
		dc.hufPtr = d.entropy.hufTable
		dc.entropy.rep = d.entropy.rep
		return
	}
	dc.litEntropy, dc.fseEntropy = false, false
}

// ZSTD_refDictContent, raw dictionary content as the segment before the first output
func (dc *DCtx) refDictContent(dict cptr, dictSize int) {
	dc.dictEnd = dc.previousDstEnd
	dc.extDictLen = dc.previousDstEnd.i - dc.prefixStart.i
	dc.prefixStart = dict
	dc.previousDstEnd = cptr{dict.r, dict.i + dictSize}
}

// ZSTD_decompress_insertDictionary, raw content unless the dictionary magic leads it
func (dc *DCtx) insertDictionary(dict []byte) ErrorCode {
	r := &region{dict}
	if len(dict) < 8 || readLE32(dict, 0) != magicDictionary {
		dc.refDictContent(cptr{r, 0}, len(dict))
		return 0
	}
	dc.dictID = readLE32(dict, 4)
	eSize, err := loadDEntropy(&dc.entropy, dict)
	if err != 0 {
		return ErrDictionaryCorrupted
	}
	dc.litEntropy, dc.fseEntropy = true, true
	dc.refDictContent(cptr{r, eSize}, len(dict)-eSize)
	return 0
}

// ZSTD_decompressBegin_usingDict
func (dc *DCtx) decompressBeginUsingDict(dict []byte) ErrorCode {
	dc.decompressBegin()
	if len(dict) > 0 && dc.insertDictionary(dict) != 0 {
		return ErrDictionaryCorrupted
	}
	return 0
}

// ZSTD_decompressBegin_usingDDict, the dictionary cold when its end is not the window end left by the last frame
func (dc *DCtx) decompressBeginUsingDDict(d *DDict) {
	if d != nil {
		dc.ddictIsCold = dc.dictEnd != cptr{d.content, d.dictSize}
	}
	dc.decompressBegin()
	if d != nil {
		dc.copyDDictParameters(d)
	}
}

// RefDDict is ZSTD_DCtx_refDDict, using d for every following frame
func (dc *DCtx) RefDDict(d *DDict) ErrorCode {
	if dc.streamStage != zdssInit {
		return ErrStageWrong
	}
	dc.ddict = d
	return 0
}

// Dictionary content handed to the legacy decoders, nil without a referenced dictionary
func (dc *DCtx) legacyDict() []byte {
	if dc.ddict == nil {
		return nil
	}
	return dc.ddict.content.b
}
