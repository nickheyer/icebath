package sentencepiece

import (
	"fmt"
	"os"
)

// A loaded sentencepiece model with its normalizer
type Processor struct {
	proto      *modelProto
	model      encoder
	table      *pieceTable
	normalizer *normalizer
}

// Load reads a model file
func Load(modelFile string) (*Processor, error) {
	serialized, err := os.ReadFile(modelFile)
	if err != nil {
		return nil, fmt.Errorf("sentencepiece: %w", err)
	}
	return New(serialized)
}

// New builds the model and the normalizer of a serialized model
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

// Pieces and ids of one text, a run of unknown characters one piece or spelled as bytes under byte fallback
func (p *Processor) encode(input string) ([]string, []int, error) {
	pieces, ids := []string{}, []int{}
	if input == "" {
		return pieces, ids, nil
	}
	isPrevUnk := false
	for _, piece := range p.model.encode(p.normalizer.normalize(input)) {
		w := piece.piece
		if w == "" {
			return nil, nil, fail("the model produced an empty piece")
		}
		if p.table.kind(piece.id) == pieceControl {
			pieces, ids = append(pieces, w), append(ids, piece.id)
			isPrevUnk = false
			continue
		}
		isUnk := p.table.kind(piece.id) == pieceUnknown
		switch {
		case isUnk && p.proto.byteFallback:
			for i := 0; i < len(w); i++ {
				pieces, ids = append(pieces, byteToPiece(w[i])), append(ids, p.table.pieceToID(byteToPiece(w[i])))
			}
		case !isPrevUnk || !isUnk:
			pieces, ids = append(pieces, w), append(ids, piece.id)
		default:
			pieces[len(pieces)-1] += w
		}
		isPrevUnk = isUnk
	}
	return pieces, ids, nil
}

// EncodeAsPieces returns the pieces of one text
func (p *Processor) EncodeAsPieces(input string) ([]string, error) {
	pieces, _, err := p.encode(input)
	return pieces, err
}

// EncodeAsIDs returns the piece ids of one text
func (p *Processor) EncodeAsIDs(input string) ([]int, error) {
	_, ids, err := p.encode(input)
	return ids, err
}

// Size returns the number of pieces the model defines
func (p *Processor) Size() int { return len(p.proto.pieces) }

// PieceID returns the id of a piece the model defines
func (p *Processor) PieceID(piece string) (int, bool) {
	if id, ok := p.table.reserved[piece]; ok {
		return id, true
	}
	id, ok := p.table.pieces[piece]
	return id, ok
}

// EncodeBatchAsPieces returns the pieces of every text in order
func (p *Processor) EncodeBatchAsPieces(inputs []string) ([][]string, error) {
	out := make([][]string, len(inputs))
	for i, input := range inputs {
		pieces, err := p.EncodeAsPieces(input)
		if err != nil {
			return nil, err
		}
		out[i] = pieces
	}
	return out, nil
}
