package dashboard

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"unicode/utf8"
)

// Raw header pair of latin-1 bytes, names lowercase the way ASGI carries them
type Header [2]string

// Host and port of a connection end, the ASGI client and server tuples
type Address struct {
	Host string
	Port int
}

// ASGI connection scope of one http, websocket or lifespan call
type Scope struct {
	Type              string
	HTTPVersion       string
	Method            string
	Scheme            string
	Path              string
	RawPath           string
	RootPath          string
	QueryString       string
	Headers           []Header
	Client            *Address
	Server            *Address
	Subprotocols      []string
	State             map[string]any
	App               *FastAPI
	Router            *Router
	Route             BaseRoute
	Endpoint          any
	PathParams        map[string]any
	Extensions        map[string]any
	Extra             map[string]any
	handlersInstalled bool
}

// Shallow copy of the scope, the dict(scope) of a middleware that rewrites one key
func (s *Scope) Clone() *Scope {
	c := *s
	c.Headers = append([]Header(nil), s.Headers...)
	c.PathParams = maps.Clone(s.PathParams)
	c.Extra = maps.Clone(s.Extra)
	return &c
}

// Extra scope key a middleware set, nil when absent
func (s *Scope) Get(key string) any { return s.Extra[key] }

// Stores an extra scope key
func (s *Scope) Set(key string, v any) {
	if s.Extra == nil {
		s.Extra = map[string]any{}
	}
	s.Extra[key] = v
}

// One ASGI event, the union of every http and websocket message field
type Message struct {
	Type        string
	Body        []byte
	MoreBody    bool
	Status      int
	Headers     []Header
	Trailers    bool
	Bytes       []byte
	Text        *string
	Code        int
	Reason      string
	Subprotocol *string
	Path        string
}

// Pulls the next event from the server
type Receive func(ctx context.Context) (Message, error)

// Pushes one event to the server
type Send func(ctx context.Context, m Message) error

// ASGI application callable
type ASGIApp func(ctx context.Context, scope *Scope, receive Receive, send Send) error

// Python str of latin-1 bytes, each byte one code point
func latin1Decode(b string) string {
	if isASCII(b) {
		return b
	}
	var sb strings.Builder
	for i := 0; i < len(b); i++ {
		sb.WriteRune(rune(b[i]))
	}
	return sb.String()
}

// Latin-1 bytes of a Python str, the UnicodeEncodeError for a code point past 255
func latin1Encode(s string) (string, error) {
	if isASCII(s) {
		return s, nil
	}
	out := make([]byte, 0, len(s))
	pos := 0
	for _, r := range s {
		if r > 0xff || r == utf8.RuneError {
			return "", &UnicodeEncodeError{Encoding: "latin-1", Char: r, Pos: pos}
		}
		out = append(out, byte(r))
		pos++
	}
	return string(out), nil
}

// Latin-1 bytes of a str known to fit, the header names and values Starlette builds itself
func mustLatin1(s string) string {
	b, err := latin1Encode(s)
	if err != nil {
		panic(err)
	}
	return b
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Python UnicodeEncodeError of a code point the codec cannot carry
type UnicodeEncodeError struct {
	Encoding string
	Char     rune
	Pos      int
}

func (e *UnicodeEncodeError) Error() string {
	return fmt.Sprintf("'%s' codec can't encode character '\\u%04x' in position %d: ordinal not in range(256)", e.Encoding, e.Char, e.Pos)
}

func (e *UnicodeEncodeError) PyTypeName() string { return "UnicodeEncodeError" }

// Python RuntimeError raised by the ASGI layer
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string      { return e.Msg }
func (e *RuntimeError) PyTypeName() string { return "RuntimeError" }
