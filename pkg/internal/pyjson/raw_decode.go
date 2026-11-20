package pyjson

import "github.com/nickheyer/icebath/pkg/internal/pystr"

// json.JSONDecoder().raw_decode of text from its first character, the value and the rune offset after it
func RawDecode(text string) (any, int, error) {
	p := &parser{s: pystr.Runes(text)}
	v, end, err := p.value(0)
	if stop, ok := err.(*stopError); ok {
		return nil, 0, p.fail("Expecting value", stop.pos)
	}
	if err != nil {
		return nil, 0, err
	}
	return v, end, nil
}
