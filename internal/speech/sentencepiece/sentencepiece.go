// SentencePiece models read without the C++ library: the pieces of a text as the keyword spotter's BPE vocabulary spells them
package sentencepiece

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
)

func fail(msg string) error { return errors.New("sentencepiece: " + msg) }

// Model types of the trainer spec
const (
	modelUnigram = 1
	modelBPE     = 2
	modelWord    = 3
	modelChar    = 4
)

// Piece types of the model proto
const (
	pieceNormal      = 1
	pieceUnknown     = 2
	pieceControl     = 3
	pieceUserDefined = 4
	pieceUnused      = 5
	pieceByte        = 6
)

type sentencePiece struct {
	piece string
	score float32
	kind  int
}

// Fields of a serialized ModelProto the encoder reads
type modelProto struct {
	pieces                  []sentencePiece
	modelType               int
	treatWhitespaceAsSuffix bool
	byteFallback            bool
	precompiledCharsmap     []byte
	addDummyPrefix          bool
	removeExtraWhitespaces  bool
	escapeWhitespaces       bool
}

var errWire = errors.New("malformed protobuf")

// Visits every field of one protobuf message with its varint value or bytes
func wireFields(b []byte, visit func(num int, wire int, value uint64, data []byte) error) error {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return errWire
		}
		b = b[n:]
		num, wire := int(key>>3), int(key&7)
		var value uint64
		var data []byte
		switch wire {
		case 0:
			if value, n = binary.Uvarint(b); n <= 0 {
				return errWire
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return errWire
			}
			data, b = b[:8], b[8:]
		case 2:
			size, n := binary.Uvarint(b)
			if n <= 0 || size > uint64(len(b)-n) {
				return errWire
			}
			data, b = b[n:n+int(size)], b[n+int(size):]
		case 5:
			if len(b) < 4 {
				return errWire
			}
			data, b = b[:4], b[4:]
		default:
			return errWire
		}
		if num == 0 {
			return errWire
		}
		if err := visit(num, wire, value, data); err != nil {
			return err
		}
	}
	return nil
}

// Pieces and the trainer and normalizer settings of a serialized model
func parseModelProto(serialized []byte) (*modelProto, error) {
	m := &modelProto{modelType: modelUnigram, addDummyPrefix: true, removeExtraWhitespaces: true, escapeWhitespaces: true}
	err := wireFields(serialized, func(num, wire int, value uint64, data []byte) error {
		if wire != 2 {
			return nil
		}
		switch num {
		case 1:
			sp := sentencePiece{kind: pieceNormal}
			if err := wireFields(data, func(num, wire int, value uint64, data []byte) error {
				switch {
				case num == 1 && wire == 2:
					sp.piece = string(data)
				case num == 2 && wire == 5:
					sp.score = math.Float32frombits(binary.LittleEndian.Uint32(data))
				case num == 3 && wire == 0:
					sp.kind = int(value)
				}
				return nil
			}); err != nil {
				return err
			}
			m.pieces = append(m.pieces, sp)
		case 2:
			return wireFields(data, func(num, wire int, value uint64, data []byte) error {
				if wire != 0 {
					return nil
				}
				switch num {
				case 3:
					m.modelType = int(value)
				case 24:
					m.treatWhitespaceAsSuffix = value != 0
				case 35:
					m.byteFallback = value != 0
				}
				return nil
			})
		case 3:
			return wireFields(data, func(num, wire int, value uint64, data []byte) error {
				switch {
				case num == 2 && wire == 2:
					m.precompiledCharsmap = append([]byte(nil), data...)
				case num == 3 && wire == 0:
					m.addDummyPrefix = value != 0
				case num == 4 && wire == 0:
					m.removeExtraWhitespaces = value != 0
				case num == 5 && wire == 0:
					m.escapeWhitespaces = value != 0
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return nil, fail("the bytes are not a serialized model")
	}
	return m, nil
}

// Byte length of the UTF-8 sequence a lead byte announces
func oneCharLen(lead byte) int {
	return int("\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x01\x02\x02\x03\x04"[lead>>4])
}

// Piece of a byte under byte fallback
func byteToPiece(b byte) string { return fmt.Sprintf("<0x%02X>", b) }

// Byte a fallback piece names, -1 for any other
func pieceToByte(piece string) int {
	for i := 0; i < 256; i++ {
		if byteToPiece(byte(i)) == piece {
			return i
		}
	}
	return -1
}

// Piece tables every model type shares
type pieceTable struct {
	proto       *modelProto
	pieces      map[string]int
	reserved    map[string]int
	unkID       int
	userDefined []string
}

// Lookup tables of the pieces, the reserved ones apart when asked
func newPieceTable(proto *modelProto, useReservedIDMap bool) (*pieceTable, error) {
	t := &pieceTable{proto: proto, pieces: map[string]int{}, reserved: map[string]int{}, unkID: -1}
	byteFound := make([]bool, 256)
	for _, sp := range proto.pieces {
		if len(sp.piece) >= 8000 {
			return nil, fail("piece is too long.")
		}
	}
	for i, sp := range proto.pieces {
		if sp.piece == "" {
			return nil, fail("piece must not be empty.")
		}
		if strings.IndexByte(sp.piece, 0) >= 0 {
			return nil, fail("piece must not include null character.")
		}
		target := t.pieces
		if useReservedIDMap && sp.kind != pieceNormal && sp.kind != pieceUserDefined && sp.kind != pieceUnused {
			target = t.reserved
		}
		if _, dup := target[sp.piece]; dup {
			return nil, fail(sp.piece + " is already defined.")
		}
		target[sp.piece] = i
		if sp.kind == pieceUserDefined {
			t.userDefined = append(t.userDefined, sp.piece)
		}
		if sp.kind == pieceUnknown {
			if t.unkID >= 0 {
				return nil, fail("unk is already defined.")
			}
			t.unkID = i
		}
		if sp.kind == pieceByte {
			if !proto.byteFallback {
				return nil, fail("byte piece " + sp.piece + " is found although `byte_fallback` is false.")
			}
			b := pieceToByte(sp.piece)
			if b < 0 {
				return nil, fail("byte piece " + sp.piece + " is invalid.")
			}
			byteFound[b] = true
		}
	}
	if t.unkID == -1 {
		return nil, fail("unk is not defined.")
	}
	if proto.byteFallback {
		for _, found := range byteFound {
			if !found {
				return nil, fail("there are not 256 byte pieces although `byte_fallback` is true.")
			}
		}
	}
	return t, nil
}

func (t *pieceTable) pieceToIDNoReserved(piece string) int {
	if id, ok := t.pieces[piece]; ok {
		return id
	}
	return t.unkID
}

func (t *pieceTable) pieceToID(piece string) int {
	if id, ok := t.reserved[piece]; ok {
		return id
	}
	return t.pieceToIDNoReserved(piece)
}

func (t *pieceTable) kind(id int) int { return t.proto.pieces[id].kind }

func (t *pieceTable) isReservedID(id int) bool {
	k := t.kind(id)
	return k != pieceNormal && k != pieceUserDefined && k != pieceUnused
}

// Length of the longest user defined symbol at the start of w, else of its first character
func (t *pieceTable) prefixMatch(w string) (int, bool) {
	mblen := 0
	for _, symbol := range t.userDefined {
		if len(symbol) > mblen && strings.HasPrefix(w, symbol) {
			mblen = len(symbol)
		}
	}
	if mblen > 0 {
		return mblen, true
	}
	return min(len(w), oneCharLen(w[0])), false
}

// One encoded piece with its vocabulary id
type encoded struct {
	piece string
	id    int
}

// A loaded model with its normalizer
type Processor struct {
	proto      *modelProto
	model      encoder
	table      *pieceTable
	normalizer *normalizer
}

// Reads a model file
func Load(modelFile string) (*Processor, error) {
	serialized, err := os.ReadFile(modelFile)
	if err != nil {
		return nil, fmt.Errorf("sentencepiece: %w", err)
	}
	return New(serialized)
}

// Model and normalizer of a serialized model
func New(serialized []byte) (*Processor, error) {
	proto, err := parseModelProto(serialized)
	if err != nil {
		return nil, err
	}
	p := &Processor{proto: proto}
	if p.model, p.table, err = newModel(proto); err != nil {
		return nil, err
	}
	if p.normalizer, err = newNormalizer(proto, p.table); err != nil {
		return nil, err
	}
	return p, nil
}

// Pieces of one text, a run of unknown characters one piece or spelled as bytes under byte fallback
func (p *Processor) Pieces(input string) ([]string, error) {
	pieces := []string{}
	if input == "" {
		return pieces, nil
	}
	isPrevUnk := false
	for _, piece := range p.model.encode(p.normalizer.normalize(input)) {
		w := piece.piece
		if w == "" {
			return nil, fail("the model produced an empty piece")
		}
		if p.table.kind(piece.id) == pieceControl {
			pieces = append(pieces, w)
			isPrevUnk = false
			continue
		}
		isUnk := p.table.kind(piece.id) == pieceUnknown
		switch {
		case isUnk && p.proto.byteFallback:
			for i := 0; i < len(w); i++ {
				pieces = append(pieces, byteToPiece(w[i]))
			}
		case !isPrevUnk || !isUnk:
			pieces = append(pieces, w)
		default:
			pieces[len(pieces)-1] += w
		}
		isPrevUnk = isUnk
	}
	return pieces, nil
}
