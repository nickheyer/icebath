package pycodecs

import (
	"encoding/base64"
	"strings"
	"sync"
)

var blobCache sync.Map

// Bytes of one generated table, base64 decoded once per process
func blobBytes(set map[string]string, kind, name string) []byte {
	key := kind + ":" + name
	if v, ok := blobCache.Load(key); ok {
		return v.([]byte)
	}
	text, ok := set[name]
	if !ok {
		panic("pycodecs: no generated table " + key)
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		panic("pycodecs: generated table " + key + " is not base64")
	}
	blobCache.Store(key, raw)
	return raw
}

// Cursor over a generated table in the big endian layout the generator writes
type blobReader struct {
	b []byte
	p int
}

func (r *blobReader) more() bool { return r.p < len(r.b) }

func (r *blobReader) u8() int {
	v := int(r.b[r.p])
	r.p++
	return v
}

func (r *blobReader) u16() int {
	v := int(r.b[r.p])<<8 | int(r.b[r.p+1])
	r.p += 2
	return v
}

func (r *blobReader) u24() rune {
	v := rune(r.b[r.p])<<16 | rune(r.b[r.p+1])<<8 | rune(r.b[r.p+2])
	r.p += 3
	return v
}

// Text of a count prefixed run of code points
func (r *blobReader) str() string {
	n := r.u8()
	var b strings.Builder
	for i := 0; i < n; i++ {
		writeCodePoint(&b, r.u24())
	}
	return b.String()
}

// Count prefixed bytes
func (r *blobReader) raw() []byte {
	n := r.u8()
	v := append([]byte(nil), r.b[r.p:r.p+n]...)
	r.p += n
	return v
}

// Appends a code point, a lone surrogate written in its generalized UTF-8 form as CPython strings hold it
func writeCodePoint(b *strings.Builder, r rune) {
	if r >= 0xD800 && r <= 0xDFFF {
		b.WriteByte(byte(0xE0 | r>>12))
		b.WriteByte(byte(0x80 | (r>>6)&0x3F))
		b.WriteByte(byte(0x80 | r&0x3F))
		return
	}
	b.WriteRune(r)
}
