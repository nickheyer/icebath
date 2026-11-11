package pysentencepiece

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// A loaded sentencepiece model with its normalizer
type Processor struct {
	proto      *modelProto
	model      encoder
	table      *pieceTable
	normalizer *normalizer
}

// Loads a model file, a RuntimeError naming the file when it cannot be read
func Load(modelFile string) (*Processor, error) {
	if modelFile == "" {
		return nil, &Error{Class: "ValueError", Msg: "Either model_file or model_proto must be specified."}
	}
	serialized, err := os.ReadFile(modelFile)
	if err != nil {
		errno := syscall.EIO
		errors.As(err, &errno)
		return nil, &Error{Class: "RuntimeError", Msg: fmt.Sprintf("NOT_FOUND: \"%s\": %s Error #%d", modelFile, pypath.Strerror(errno), int(errno))}
	}
	return LoadFromSerializedProto(serialized)
}

// Builds the model and the normalizer from a serialized ModelProto
func LoadFromSerializedProto(serialized []byte) (*Processor, error) {
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

// Pieces of one text, runs of unknown characters joined into one piece or spelled as bytes under byte fallback
func (p *Processor) EncodeAsPieces(input string) ([]string, error) {
	output := []string{}
	if input == "" {
		return output, nil
	}
	isPrevUnk := false
	for _, piece := range p.model.encode(p.normalizer.normalize(input)) {
		w := piece.piece
		if w == "" {
			return nil, internalError("src/sentencepiece_processor.cc(1605) [!piece.first.empty()] Empty piece is not allowed.")
		}
		if p.table.kind(piece.id) == pieceControl {
			output = append(output, w)
			isPrevUnk = false
			continue
		}
		isUnk := p.table.kind(piece.id) == pieceUnknown
		switch {
		case isUnk && p.proto.byteFallback:
			for i := 0; i < len(w); i++ {
				output = append(output, byteToPiece(w[i]))
			}
		case !isPrevUnk || !isUnk:
			output = append(output, w)
		default:
			output[len(output)-1] += w
		}
		isPrevUnk = isUnk
	}
	return output, nil
}

// Pieces of every text in order
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
