package pycodecs

import (
	"strings"
	"sync"
)

const (
	mbIllegal    = "illegal multibyte sequence"
	mbIncomplete = "incomplete multibyte sequence"
)

// One decision of a stateless multibyte decoder, a character, an error of esize bytes or a further byte to read
type mbNode struct {
	kind  int
	out   string
	esize int
	next  *mbLevel
}

// Decisions keyed by the next byte, absent bytes failing with the default esize
type mbLevel struct {
	def   int
	nodes map[byte]*mbNode
}

func (l *mbLevel) lookup(b byte) *mbNode {
	if n, ok := l.nodes[b]; ok {
		return n
	}
	return &mbNode{kind: 1, esize: l.def}
}

// Stateless CPython multibyte codec as its per position automaton and its single and pair encode maps
type statelessCodec struct {
	name   string
	leads  [256]mbNode
	single map[rune][]byte
	pairs  map[[2]rune][]byte
	trunc  bool
}

func readLevel(r *blobReader) *mbLevel {
	l := &mbLevel{def: r.u8(), nodes: map[byte]*mbNode{}}
	count := r.u16()
	for i := 0; i < count; i++ {
		k := byte(r.u8())
		n := &mbNode{kind: r.u8()}
		switch n.kind {
		case 0:
			n.out = r.str()
		case 1:
			n.esize = r.u8()
		default:
			n.next = readLevel(r)
		}
		l.nodes[k] = n
	}
	return l
}

var statelessCodecs sync.Map

// Codec of a stateless module parsed from its generated tables once
func loadStateless(mod string) *statelessCodec {
	if v, ok := statelessCodecs.Load(mod); ok {
		return v.(*statelessCodec)
	}
	c := &statelessCodec{name: mod, single: map[rune][]byte{}, pairs: map[[2]rune][]byte{}, trunc: pairTruncates[mod]}
	r := &blobReader{b: blobBytes(cjkBlobs, "cjk", mod+".dec")}
	for lead := range c.leads {
		n := &c.leads[lead]
		n.kind = r.u8()
		switch n.kind {
		case 0:
			n.out = r.str()
		case 1:
			n.esize = r.u8()
		default:
			n.next = readLevel(r)
		}
	}
	r = &blobReader{b: blobBytes(cjkBlobs, "cjk", mod+".enc")}
	for r.more() {
		cp := r.u24()
		c.single[cp] = r.raw()
	}
	r = &blobReader{b: blobBytes(cjkBlobs, "cjk", mod+".pairs")}
	for r.more() {
		a, b := r.u24(), r.u24()
		c.pairs[[2]rune{a, b}] = r.raw()
	}
	v, _ := statelessCodecs.LoadOrStore(mod, c)
	return v.(*statelessCodec)
}

// Decode by the automaton, an error raised strictly or written as U+FFFD skipping its bytes when replace is set
func (c *statelessCodec) decode(data []byte, replace bool) (string, error) {
	var b strings.Builder
	for p := 0; p < len(data); {
		node := &c.leads[data[p]]
		need := 1
		for node.kind == 2 {
			if p+need >= len(data) {
				if !replace {
					return "", decodeErr(c.name, data, p, len(data), mbIncomplete)
				}
				b.WriteRune(0xfffd)
				return b.String(), nil
			}
			node = node.next.lookup(data[p+need])
			need++
		}
		if node.kind == 1 {
			if !replace {
				return "", decodeErr(c.name, data, p, p+node.esize, mbIllegal)
			}
			b.WriteRune(0xfffd)
			p += node.esize
			continue
		}
		b.WriteString(node.out)
		p += need
	}
	return b.String(), nil
}

// Encode pairing a base with the following character first, the pair key read through the codec's ucs2_t truncation
func (c *statelessCodec) encode(text string) (string, error) {
	runes := textRunes(text)
	var out []byte
	for i := 0; i < len(runes); {
		ch := runes[i]
		if i+1 < len(runes) && len(c.pairs) > 0 {
			if n := runes[i+1]; c.trunc || n <= 0xffff {
				if code, ok := c.pairs[[2]rune{ch, n & 0xffff}]; ok {
					out = append(out, code...)
					i += 2
					continue
				}
			}
		}
		code, ok := c.single[ch]
		if !ok {
			return "", encodeErr(c.name, runes, i, i+1, mbIllegal)
		}
		out = append(out, code...)
		i++
	}
	return string(out), nil
}

// Modules served by the stateless automaton
var statelessModules = map[string]bool{"johab": true, "euc_jis_2004": true, "euc_jisx0213": true, "shift_jis_2004": true, "shift_jisx0213": true}
