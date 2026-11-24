package jsonschemautil

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"strconv"
	"sync"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	// Compiled schemas a cache generation holds; the cache keeps the current generation and the one before it
	cacheEntries = 64
	// Bytes of canonical document text past which a schema is compiled without being cached
	cacheDocumentLimit = 256 << 10
)

// Compiled schemas by a digest of their document and of the settings that shape them, so a caller compiling the same schema per validation compiles it once
type compileCache struct {
	mu       sync.Mutex
	current  map[[sha256.Size]byte]*Schema
	previous map[[sha256.Size]byte]*Schema
}

var schemaCache compileCache

func (c *compileCache) get(key [sha256.Size]byte) *Schema {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.current[key]; s != nil {
		return s
	}
	s := c.previous[key]
	if s != nil {
		c.add(key, s)
	}
	return s
}

func (c *compileCache) put(key [sha256.Size]byte, s *Schema) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.add(key, s)
}

// Stores an entry, the current generation becoming the previous one when it is full; the caller holds mu
func (c *compileCache) add(key [sha256.Size]byte, s *Schema) {
	if len(c.current) >= cacheEntries {
		c.previous, c.current = c.current, nil
	}
	if c.current == nil {
		c.current = make(map[[sha256.Size]byte]*Schema, cacheEntries)
	}
	c.current[key] = s
}

// Digest naming the compiled form of a normalized document under the settings, false when the settings hold what a digest cannot capture (loaders, given documents, format functions) or the document is too large to be worth keeping
func (cfg *config) cacheKey(doc any) ([sha256.Size]byte, bool) {
	if cfg.skipMeta || cfg.metaDepth > 0 || len(cfg.loaders) > 0 || len(cfg.resources) > 0 || len(cfg.formats) > 0 {
		return [sha256.Size]byte{}, false
	}
	b := make([]byte, 0, 512)
	b = strconv.AppendInt(b, int64(cfg.draft), 10)
	b = strconv.AppendBool(append(b, ';'), cfg.assertFormats)
	b = strconv.AppendBool(append(b, ';'), cfg.content)
	b = strconv.AppendInt(append(b, ';'), int64(cfg.timeout), 10)
	b = appendText(append(b, ';'), cfg.baseURI)
	b, ok := appendDocument(b, doc)
	if !ok {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(b), true
}

// Length-prefixed text, so that no two strings run into each other
func appendText(b []byte, s string) []byte {
	b = strconv.AppendInt(b, int64(len(s)), 10)
	b = append(b, ':')
	return append(b, s...)
}

// Exact text of a normalized document, key order and number types kept, false once it passes cacheDocumentLimit
func appendDocument(b []byte, v any) ([]byte, bool) {
	if len(b) > cacheDocumentLimit {
		return b, false
	}
	switch x := v.(type) {
	case nil:
		return append(b, 'n'), true
	case bool:
		if x {
			return append(b, 't'), true
		}
		return append(b, 'f'), true
	case string:
		return appendText(append(b, 's'), x), true
	case int64:
		return append(strconv.AppendInt(append(b, 'i'), x, 10), ';'), true
	case float64:
		return append(strconv.AppendUint(append(b, 'd'), math.Float64bits(x), 16), ';'), true
	case json.Number:
		return appendText(append(b, 'j'), string(x)), true
	case []any:
		b = append(b, '[')
		for _, item := range x {
			var ok bool
			if b, ok = appendDocument(b, item); !ok {
				return b, false
			}
		}
		return append(b, ']'), true
	case *jsonutil.Object:
		b = append(b, '{')
		for k, val := range x.All() {
			b = appendText(b, k)
			var ok bool
			if b, ok = appendDocument(b, val); !ok {
				return b, false
			}
		}
		return append(b, '}'), true
	}
	return b, false
}
