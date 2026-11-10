package libzstd

const (
	lbitUnfinished = iota
	lbitEndOfBuffer
	lbitCompleted
	lbitOverflow
)

// Backward bit reader shared by the v0.5, v0.6 and v0.7 decoders, positions counted from the stream start
type lbitD struct {
	bitContainer uint64
	bitsConsumed uint32
	src          []byte
	ptr          int
}

func (b *lbitD) init(src []byte) (int, ErrorCode) {
	srcSize := len(src)
	if srcSize < 1 {
		*b = lbitD{}
		return 0, ErrSrcSizeWrong
	}
	b.src = src
	lastByte := uint32(src[srcSize-1])
	if srcSize >= 8 {
		b.ptr = srcSize - 8
		b.bitContainer = readLE64(src, b.ptr)
		if lastByte == 0 {
			b.bitsConsumed = 0
			return 0, ErrGeneric
		}
		b.bitsConsumed = 8 - highbit32(lastByte)
		return srcSize, 0
	}
	b.ptr = 0
	b.bitContainer = uint64(src[0])
	switch srcSize {
	case 7:
		b.bitContainer += uint64(src[6]) << 48
		fallthrough
	case 6:
		b.bitContainer += uint64(src[5]) << 40
		fallthrough
	case 5:
		b.bitContainer += uint64(src[4]) << 32
		fallthrough
	case 4:
		b.bitContainer += uint64(src[3]) << 24
		fallthrough
	case 3:
		b.bitContainer += uint64(src[2]) << 16
		fallthrough
	case 2:
		b.bitContainer += uint64(src[1]) << 8
	}
	if lastByte == 0 {
		b.bitsConsumed = 0
		return 0, ErrGeneric
	}
	b.bitsConsumed = 8 - highbit32(lastByte)
	b.bitsConsumed += uint32(8-srcSize) * 8
	return srcSize, 0
}

func (b *lbitD) lookBits(nbBits uint32) uint64 {
	return ((b.bitContainer << (b.bitsConsumed & 63)) >> 1) >> ((63 - nbBits) & 63)
}

func (b *lbitD) lookBitsFast(nbBits uint32) uint64 {
	return (b.bitContainer << (b.bitsConsumed & 63)) >> ((64 - nbBits) & 63)
}

func (b *lbitD) skipBits(nbBits uint32) { b.bitsConsumed += nbBits }

func (b *lbitD) readBits(nbBits uint32) uint64 {
	v := b.lookBits(nbBits)
	b.bitsConsumed += nbBits
	return v
}

func (b *lbitD) readBitsFast(nbBits uint32) uint64 {
	v := b.lookBitsFast(nbBits)
	b.bitsConsumed += nbBits
	return v
}

func (b *lbitD) reload() int {
	if b.bitsConsumed > 64 {
		return lbitOverflow
	}
	if b.ptr >= 8 {
		b.ptr -= int(b.bitsConsumed >> 3)
		b.bitsConsumed &= 7
		b.bitContainer = readLE64(b.src, b.ptr)
		return lbitUnfinished
	}
	if b.ptr == 0 {
		if b.bitsConsumed < 64 {
			return lbitEndOfBuffer
		}
		return lbitCompleted
	}
	nbBytes := b.bitsConsumed >> 3
	result := lbitUnfinished
	if b.ptr-int(nbBytes) < 0 {
		nbBytes = uint32(b.ptr)
		result = lbitEndOfBuffer
	}
	b.ptr -= int(nbBytes)
	b.bitsConsumed -= nbBytes * 8
	b.bitContainer = readLE64(b.src, b.ptr)
	return result
}

func (b *lbitD) endOfDStream() bool { return b.ptr == 0 && b.bitsConsumed == 64 }

type lfseCell struct {
	newState uint16
	symbol   byte
	nbBits   byte
}

// Decoding table of the legacy FSE, cells sized for the largest table log its owner accepts
type lfseDTable struct {
	tableLog uint16
	fastMode uint16
	cells    []lfseCell
}

func newLfseDTable(maxTableLog uint) *lfseDTable {
	return &lfseDTable{cells: make([]lfseCell, 1<<maxTableLog)}
}

func (dt *lfseDTable) copyFrom(o *lfseDTable) {
	dt.tableLog, dt.fastMode = o.tableLog, o.fastMode
	copy(dt.cells, o.cells)
}

type lfseDState struct {
	state uint64
	table []lfseCell
}

func (s *lfseDState) init(b *lbitD, dt *lfseDTable) {
	s.state = b.readBits(uint32(dt.tableLog))
	b.reload()
	s.table = dt.cells
}

func (s *lfseDState) peekSymbol() byte { return s.table[s.state].symbol }

func (s *lfseDState) updateState(b *lbitD) {
	d := s.table[s.state]
	s.state = uint64(d.newState) + b.readBits(uint32(d.nbBits))
}

func (s *lfseDState) decodeSymbol(b *lbitD) byte {
	d := s.table[s.state]
	s.state = uint64(d.newState) + b.readBits(uint32(d.nbBits))
	return d.symbol
}

func (s *lfseDState) decodeSymbolFast(b *lbitD) byte {
	d := s.table[s.state]
	s.state = uint64(d.newState) + b.readBitsFast(uint32(d.nbBits))
	return d.symbol
}

func lfseBuildDTableRLE(dt *lfseDTable, symbolValue byte) {
	dt.tableLog, dt.fastMode = 0, 0
	dt.cells[0] = lfseCell{newState: 0, symbol: symbolValue, nbBits: 0}
}

const (
	lfseMaxSymbolValue      = 255
	lfseMaxTableLog         = 12
	lfseMinTableLog         = 5
	lfseTableLogAbsoluteMax = 15
)

func lfseTableStep(tableSize uint32) uint32 { return (tableSize >> 1) + (tableSize >> 3) + 3 }
