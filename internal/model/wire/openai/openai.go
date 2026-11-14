// OpenAI Chat Completions and Responses wire protocols
package openai

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Prefix a provider-reserved client tool name takes on the wire
const reservedAliasPrefix = "icebath_"

// Longest prompt cache key and Responses item id the endpoints accept
const maxKeyLength = 64

var (
	_ wire.Protocol = ChatProtocol{}
	_ wire.Protocol = (*ResponsesProtocol)(nil)
)

// Returned when a reply holds no choice or output to read
var ErrInvalidResponse = errors.New("openai: reply holds no response")

// Returned with the reply when a router encoded an upstream connect timeout as a successful completion
var ErrRouterTimeoutShim = errors.New("openai: router answered with its connect timeout notice")

// Returned when a Responses stream ended without a terminal frame or any output
var ErrNoTerminal = errors.New("openai: Responses stream did not emit a terminal response")

// Request the protocol cannot put on the wire, with the reason
type RequestError struct {
	Reason string
}

func (e *RequestError) Error() string { return e.Reason }

// Session id of a scheduled job with its per-run timestamp
var cronSession = regexp.MustCompile(`^(cron_.+)_\d{8}_\d{6}$`)

// Trims whitespace of every script from both ends
func strip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// Reports whether a JSON value is a string with something other than whitespace in it
func nonblank(v wire.Value) bool {
	s, ok := v.Str()
	return ok && strip(s) != ""
}

// Logical cache scope of a request, a scheduled job's runs sharing one scope
func cacheScope(req *wire.Request) string {
	scope := req.CacheScope
	if scope == "" {
		scope = req.SessionID
	}
	if m := cronSession.FindStringSubmatch(scope); m != nil {
		return m[1]
	}
	return scope
}

// First hex digits of the SHA-256 of a text
func shaHex(text string, digits int) string {
	sum := sha256.Sum256([]byte(strings.ToValidUTF8(text, "?")))
	return hex.EncodeToString(sum[:])[:digits]
}

// Cache key within the 64 characters endpoints accept, empty for a blank key
func boundedCacheKey(key string) string {
	key = strip(key)
	if utf8.RuneCountInString(key) <= maxKeyLength {
		return key
	}
	return "pck_" + shaHex(key, 24)
}

// Bounds the prompt_cache_key member of a body in place, dropping an empty or non-string one
func boundCacheKeyField(d *wire.Dict) {
	v := d.Get("prompt_cache_key")
	if !v.Exists() {
		return
	}
	bounded := boundedCacheKey(v.Display())
	if v.Kind() == wire.KindNull || bounded == "" {
		d.Delete("prompt_cache_key")
		return
	}
	d.SetString("prompt_cache_key", bounded)
}

// Sorted compact UTF-8 form of JSON text, the form cache keys hash
func canonicalJSON(raw []byte) string {
	text, ok := wire.FormatJSON(raw, wire.TextLayout{Sorted: true, UTF8: true})
	if !ok {
		return string(raw)
	}
	return text
}

// Content-addressed cache key over a scope, the instructions and the tools sorted by name, empty when nothing is static
func contentCacheKey(instructions string, tools []json.RawMessage, scope string) string {
	if instructions == "" && len(tools) == 0 {
		return ""
	}
	toolsPart := ""
	if len(tools) > 0 {
		sorted := slices.Clone(tools)
		name := func(raw json.RawMessage) string {
			v, err := wire.ParseValue(raw)
			if err != nil {
				return ""
			}
			for _, key := range [...]string{"name", "type"} {
				if f := v.Get(key); f.Truthy() {
					return f.Display()
				}
			}
			return ""
		}
		slices.SortStableFunc(sorted, func(a, b json.RawMessage) int { return strings.Compare(name(a), name(b)) })
		toolsPart = canonicalJSON(wire.Array(sorted...))
	}
	return "pck_" + shaHex(scope+"\x00"+instructions+"\x00"+toolsPart, 24)
}

// Random version 4 identifier in the hyphenated form
func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Aliases the reserved names among tools, an alias a real tool holds taking a numbered suffix
func aliasReserved(names []string, reserved []string, aliases map[string]string) {
	taken := map[string]bool{}
	for _, name := range names {
		if name != "" {
			taken[name] = true
		}
	}
	for i, name := range names {
		if !slices.Contains(reserved, name) {
			continue
		}
		base := reservedAliasPrefix + name
		alias := base
		for suffix := 2; taken[alias]; suffix++ {
			alias = base + "_" + itoa(suffix)
		}
		taken[alias] = true
		aliases[alias] = name
		names[i] = alias
	}
}

// Decimal text of a small number
func itoa(n int) string {
	return string(wire.Int(int64(n)))
}

// Headers a call sends, the request's own over the protocol's
func callHeader(req *wire.Request, own map[string]string) http.Header {
	h := http.Header{"Content-Type": {"application/json"}}
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	}
	for k, v := range own {
		h.Set(k, v)
	}
	for k, v := range req.ExtraHeaders {
		h.Set(k, v)
	}
	return h
}

// Merges members over a body, a later value replacing an earlier one in place
func mergeInto(d *wire.Dict, extra map[string]json.RawMessage) {
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		d.Set(k, extra[k])
	}
}
