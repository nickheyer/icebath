package pyjson

import (
	"fmt"
	"strings"
)

// Plain value of a JSON prefix like jiter partial mode, open containers closed and incomplete strings and literals dropped
func LoadsPartial(text string) (any, error) {
	p := &partialParser{s: text}
	p.ws()
	v, present, err := p.value()
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, &DecodeError{Msg: "EOF while parsing a value", Pos: p.i, Lineno: 1, Colno: p.i + 1}
	}
	p.ws()
	if p.i < len(p.s) {
		return nil, &DecodeError{Msg: "trailing characters", Pos: p.i, Lineno: 1, Colno: p.i + 1}
	}
	return v, nil
}

type partialParser struct {
	s string
	i int
}

func (p *partialParser) ws() {
	for p.i < len(p.s) && strings.IndexByte(" \t\n\r", p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *partialParser) fail(msg string) error {
	return &DecodeError{Msg: msg, Pos: p.i, Lineno: 1, Colno: p.i + 1}
}

// Next value and whether it is present, an absent value being one the input ended inside
func (p *partialParser) value() (any, bool, error) {
	if p.i >= len(p.s) {
		return nil, false, nil
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	for _, lit := range []string{"true", "false", "null"} {
		rest := p.s[p.i:]
		if strings.HasPrefix(rest, lit) {
			p.i += len(lit)
			v, err := Loads(lit)
			return Plain(v), true, err
		}
		if strings.HasPrefix(lit, rest) {
			p.i = len(p.s)
			return nil, false, nil
		}
	}
	return nil, false, p.fail("expected value")
}

func (p *partialParser) str() (any, bool, error) {
	start := p.i
	for j := p.i + 1; j < len(p.s); j++ {
		switch p.s[j] {
		case '\\':
			j++
		case '"':
			p.i = j + 1
			v, err := Loads(p.s[start:p.i])
			return Plain(v), err == nil, err
		}
	}
	p.i = len(p.s)
	return nil, false, nil
}

func (p *partialParser) number() (any, bool, error) {
	start := p.i
	for p.i < len(p.s) && strings.IndexByte("+-0123456789.eE", p.s[p.i]) >= 0 {
		p.i++
	}
	v, err := Loads(p.s[start:p.i])
	if err != nil {
		if p.i == len(p.s) {
			return nil, false, nil
		}
		return nil, false, p.fail("invalid number")
	}
	return Plain(v), true, nil
}

func (p *partialParser) object() (any, bool, error) {
	p.i++
	out := map[string]any{}
	for {
		p.ws()
		if p.i >= len(p.s) {
			return out, true, nil
		}
		if p.s[p.i] == '}' {
			p.i++
			return out, true, nil
		}
		if p.s[p.i] != '"' {
			return nil, false, p.fail("key must be a string")
		}
		key, ok, err := p.str()
		if err != nil || !ok {
			return out, true, err
		}
		p.ws()
		if p.i >= len(p.s) {
			return out, true, nil
		}
		if p.s[p.i] != ':' {
			return nil, false, p.fail("expected ':'")
		}
		p.i++
		p.ws()
		v, present, err := p.value()
		if err != nil {
			return nil, false, err
		}
		if !present {
			return out, true, nil
		}
		out[fmt.Sprint(key)] = v
		p.ws()
		if p.i >= len(p.s) {
			return out, true, nil
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return out, true, nil
		default:
			return nil, false, p.fail("expected ',' or '}'")
		}
	}
}

func (p *partialParser) array() (any, bool, error) {
	p.i++
	out := []any{}
	for {
		p.ws()
		if p.i >= len(p.s) {
			return out, true, nil
		}
		if p.s[p.i] == ']' {
			p.i++
			return out, true, nil
		}
		v, present, err := p.value()
		if err != nil {
			return nil, false, err
		}
		if !present {
			return out, true, nil
		}
		out = append(out, v)
		p.ws()
		if p.i >= len(p.s) {
			return out, true, nil
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, true, nil
		default:
			return nil, false, p.fail("expected ',' or ']'")
		}
	}
}
