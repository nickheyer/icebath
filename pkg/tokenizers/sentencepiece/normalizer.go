package sentencepiece

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf8"
)

const (
	maxTrieResultsSize = 32
	spaceSymbol        = "\xe2\x96\x81"
)

// Text normalizer over the precompiled character map of a model
type normalizer struct {
	proto      *modelProto
	trie       []uint32
	normalized []byte
	table      *pieceTable
}

// Splits the precompiled map into its double array trie and its block of normalized strings
func newNormalizer(proto *modelProto, table *pieceTable) (*normalizer, error) {
	n := &normalizer{proto: proto, table: table}
	blob := proto.precompiledCharsmap
	if len(blob) == 0 {
		return n, nil
	}
	if len(blob) <= 4 {
		return nil, fail("Blob for normalization rule is broken.")
	}
	trieBlobSize := int(binary.LittleEndian.Uint32(blob))
	if trieBlobSize >= len(blob)-4 {
		return nil, fail("Trie data size exceeds the input blob size.")
	}
	if trieBlobSize < 1024 || trieBlobSize&0x3FF != 0 {
		return nil, fail("Trie data size is not divisible by 1024.")
	}
	blob = blob[4:]
	n.trie = make([]uint32, trieBlobSize/4)
	for i := range n.trie {
		n.trie[i] = binary.LittleEndian.Uint32(blob[4*i:])
	}
	n.normalized = blob[trieBlobSize:]
	if len(n.normalized) == 0 || n.normalized[len(n.normalized)-1] != 0 {
		return nil, fail("normalized block must be null terminated.")
	}
	return n, nil
}

func unitHasLeaf(u uint32) bool  { return (u>>8)&1 == 1 }
func unitValue(u uint32) uint32  { return u & (1<<31 - 1) }
func unitLabel(u uint32) uint32  { return u & (1<<31 | 0xFF) }
func unitOffset(u uint32) uint32 { return (u >> 10) << ((u & (1 << 9)) >> 6) }

// Longest key of the trie that prefixes input among its first results, with the value stored for it
func (n *normalizer) longestPrefix(input string) (length int, value uint32) {
	if len(n.trie) == 0 {
		return 0, 0
	}
	pos := uint32(0)
	unit := n.trie[pos]
	pos ^= unitOffset(unit)
	results := 0
	for i := 0; i < len(input); i++ {
		pos ^= uint32(input[i])
		if int(pos) >= len(n.trie) {
			break
		}
		unit = n.trie[pos]
		if unitLabel(unit) != uint32(input[i]) {
			break
		}
		pos ^= unitOffset(unit)
		if unitHasLeaf(unit) {
			if int(pos) >= len(n.trie) {
				break
			}
			if results < maxTrieResultsSize && (length == 0 || i+1 > length) {
				length, value = i+1, unitValue(n.trie[pos])
			}
			results++
		}
	}
	return length, value
}

// Normalized form of the longest rule matching the start of input and the bytes of input it consumes
func (n *normalizer) normalizePrefix(input string) (string, int) {
	if len(input) == 0 {
		return "", 0
	}
	if mblen, found := n.table.prefixMatch(input); found {
		return input[:mblen], mblen
	}
	length, value := n.longestPrefix(input)
	if length == 0 || length > len(input) || int(value) >= len(n.normalized) {
		r, size := utf8.DecodeRuneInString(input)
		if r == utf8.RuneError && size != 3 {
			return "\xEF\xBF\xBD", 1
		}
		return input[:size], size
	}
	rest := n.normalized[value:]
	return string(rest[:bytes.IndexByte(rest, 0)]), length
}

// Applies the character map, collapses whitespace per the spec and marks word starts with the space symbol
func (n *normalizer) normalize(input string) string {
	if len(input) == 0 {
		return ""
	}
	spec := n.proto
	if spec.removeExtraWhitespaces {
		for len(input) > 0 {
			sp, consumed := n.normalizePrefix(input)
			if sp != " " {
				break
			}
			input = input[consumed:]
		}
	}
	if len(input) == 0 {
		return ""
	}
	ws := " "
	if spec.escapeWhitespaces {
		ws = spaceSymbol
	}
	var out strings.Builder
	if !spec.treatWhitespaceAsSuffix && spec.addDummyPrefix {
		out.WriteString(ws)
	}
	isPrevSpace := spec.removeExtraWhitespaces
	for len(input) > 0 {
		sp, consumed := n.normalizePrefix(input)
		for isPrevSpace && strings.HasPrefix(sp, " ") {
			sp = sp[1:]
		}
		if sp != "" {
			for i := 0; i < len(sp); i++ {
				if sp[i] == ' ' {
					out.WriteString(ws)
				} else {
					out.WriteByte(sp[i])
				}
			}
			isPrevSpace = strings.HasSuffix(sp, " ")
		}
		input = input[consumed:]
		if !spec.removeExtraWhitespaces {
			isPrevSpace = false
		}
	}
	normalized := out.String()
	if spec.removeExtraWhitespaces {
		for strings.HasSuffix(normalized, ws) {
			normalized = normalized[:len(normalized)-len(ws)]
		}
	}
	if spec.treatWhitespaceAsSuffix && spec.addDummyPrefix {
		normalized += ws
	}
	return normalized
}
