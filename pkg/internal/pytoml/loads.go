package pytoml

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// DecodeError is the TOMLDecodeError tomllib raises with the line and column it stopped at
type DecodeError struct {
	Msg  string
	Line int
	Col  int
}

func (e *DecodeError) Error() string {
	if e.Line == 0 {
		return e.Msg
	}
	return fmt.Sprintf("%s (at line %d, column %d)", e.Msg, e.Line, e.Col)
}

// Parses a TOML document into nested maps, a document tomllib refuses returns a DecodeError
func Loads(text string) (map[string]any, error) {
	out := map[string]any{}
	if err := toml.Unmarshal([]byte(text), &out); err != nil {
		var de *toml.DecodeError
		if errors.As(err, &de) {
			line, col := de.Position()
			return nil, &DecodeError{Msg: strings.TrimPrefix(de.Error(), "toml: "), Line: line, Col: col}
		}
		return nil, &DecodeError{Msg: strings.TrimPrefix(err.Error(), "toml: ")}
	}
	return out, nil
}
