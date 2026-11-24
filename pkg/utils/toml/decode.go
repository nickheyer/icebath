package tomlutil

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Error of text that is not a TOML document, with the 1-based line and column reading stopped at, both zero when unknown
type DecodeError struct {
	Line   int
	Column int
	Msg    string
	err    error
}

// The message after its position, as in toml: line 3, column 7: expected character =
func (e *DecodeError) Error() string {
	if e.Line == 0 {
		return "toml: " + e.Msg
	}
	return fmt.Sprintf("toml: line %d, column %d: %s", e.Line, e.Column, e.Msg)
}

// Error of the TOML library this one was made from, a *toml.DecodeError whose String shows the offending lines
func (e *DecodeError) Unwrap() error { return e.err }

// Tables of a TOML document as nested maps, a *DecodeError when the text is not TOML; integers come back as int64, floats as float64, offset date-times as time.Time, local dates and times as toml.LocalDate, toml.LocalTime and toml.LocalDateTime, arrays as []any
func Decode[T ~string | ~[]byte](data T) (map[string]any, error) {
	doc := map[string]any{}
	if err := toml.Unmarshal([]byte(data), &doc); err != nil {
		return nil, decodeError(err)
	}
	return doc, nil
}

// Document as Decode reads it with every table, nested ones and those inside arrays included, an ordered object keeping the key order of the text
func DecodeOrdered[T ~string | ~[]byte](data T) (*jsonutil.Object, error) {
	raw := []byte(data)
	doc := map[string]any{}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, decodeError(err)
	}
	return ordered(doc, "", keyOrder(raw)).(*jsonutil.Object), nil
}

func decodeError(err error) error {
	out := &DecodeError{Msg: strings.TrimPrefix(err.Error(), "toml: "), err: err}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		out.Line, out.Column = de.Position()
	}
	return out
}
