package tiktoken

import (
	"container/heap"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"
)

const maxRank = math.MaxUint32

// Encoding is a BPE vocabulary with its pre-tokenizer pattern and special tokens
type Encoding struct {
	Name          string
	ranks         map[string]int
	pat           *regexp2.Regexp
	specialTokens map[string]int
	specialRe     *regexp.Regexp
}

// New builds an encoding from a split pattern in regexp2 syntax, mergeable ranks and special tokens
func New(name, pattern string, ranks map[string]int, special map[string]int) (*Encoding, error) {
	pat, err := regexp2.Compile(pattern, regexp2.None)
	if err != nil {
		return nil, fmt.Errorf("tiktoken: split pattern of %s: %w", name, err)
	}
	names := make([]string, 0, len(special))
	for tok := range special {
		names = append(names, regexp.QuoteMeta(tok))
	}
	sort.Slice(names, func(i, j int) bool {
		return len(names[i]) > len(names[j]) || len(names[i]) == len(names[j]) && names[i] < names[j]
	})
	return &Encoding{Name: name, ranks: ranks, pat: pat, specialTokens: special, specialRe: regexp.MustCompile("(" + strings.Join(names, "|") + ")")}, nil
}

const (
	endOfText   = "<|endoftext|>"
	fimPrefix   = "<|fim_prefix|>"
	fimMiddle   = "<|fim_middle|>"
	fimSuffix   = "<|fim_suffix|>"
	endOfPrompt = "<|endofprompt|>"
)

// The cl100k_base split pattern with possessive quantifiers as atomic groups and Unicode case folding spelled out
const cl100kPattern = `'(?:[sSſdDmMtT]|[lL][lL]|[vV][eE]|[rR][eE])|(?>[^\r\n\p{L}\p{N}]?)(?>\p{L}+)|(?>\p{N}{1,3})| ?(?>[^\s\p{L}\p{N}]+)(?>[\r\n]*)|(?>\s+)\z|\s*[\r\n]|\s+(?!\S)|\s`

// CL100kBlob and CL100kHash locate and verify the cl100k_base rank file
var (
	CL100kBlob = "https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken"
	CL100kHash = "223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7"
)

var (
	encodingsMu sync.Mutex
	encodings   = map[string]*Encoding{}
)

// CL100kBase returns the cl100k_base encoding, loaded once per process
func CL100kBase() (*Encoding, error) {
	encodingsMu.Lock()
	defer encodingsMu.Unlock()
	if enc := encodings["cl100k_base"]; enc != nil {
		return enc, nil
	}
	ranks, err := LoadRanks(CL100kBlob, CL100kHash)
	if err != nil {
		return nil, err
	}
	enc, err := New("cl100k_base", cl100kPattern, ranks, map[string]int{
		endOfText: 100257, fimPrefix: 100258, fimMiddle: 100259, fimSuffix: 100260, endOfPrompt: 100276,
	})
	if err != nil {
		return nil, err
	}
	encodings["cl100k_base"] = enc
	return enc, nil
}

// Encode returns the tokens of text, each special token in it as its id between ordinary runs
func (e *Encoding) Encode(text string) []int {
	if len(e.specialTokens) == 0 {
		return e.EncodeOrdinary(text)
	}
	var out []int
	for text != "" {
		loc := e.specialRe.FindStringIndex(text)
		if loc == nil {
			return append(out, e.EncodeOrdinary(text)...)
		}
		out = append(out, e.EncodeOrdinary(text[:loc[0]])...)
		out = append(out, e.specialTokens[text[loc[0]:loc[1]]])
		text = text[loc[1]:]
	}
	return out
}

// EncodeOrdinary returns the tokens of text with special tokens read as plain text
func (e *Encoding) EncodeOrdinary(text string) []int {
	var out []int
	m, err := e.pat.FindStringMatch(text)
	for err == nil && m != nil {
		piece := m.String()
		if tok, ok := e.ranks[piece]; ok {
			out = append(out, tok)
		} else {
			out = append(out, bytePairEncode([]byte(piece), e.ranks)...)
		}
		m, err = e.pat.FindNextMatch(m)
	}
	if err != nil {
		panic(err)
	}
	return out
}

func rankOf(ranks map[string]int, b []byte) int {
	if r, ok := ranks[string(b)]; ok {
		return r
	}
	return maxRank
}

// Token ranks of one piece, the parts scan below a hundred bytes and the heap merge above
func bytePairEncode(piece []byte, ranks map[string]int) []int {
	if len(piece) == 1 {
		return []int{ranks[string(piece)]}
	}
	if len(piece) < 100 {
		parts := bytePairMerge(ranks, piece)
		out := make([]int, 0, len(parts)-1)
		for i := 0; i+1 < len(parts); i++ {
			out = append(out, ranks[string(piece[parts[i].start:parts[i+1].start])])
		}
		return out
	}
	return bytePairMergeLarge(ranks, piece)
}

type part struct{ start, rank int }

// Repeatedly merges the lowest ranked adjacent pair, leftmost first
func bytePairMerge(ranks map[string]int, piece []byte) []part {
	parts := make([]part, 0, len(piece)+1)
	minRank, minIdx := maxRank, -1
	for i := 0; i < len(piece)-1; i++ {
		r := rankOf(ranks, piece[i:i+2])
		if r < minRank {
			minRank, minIdx = r, i
		}
		parts = append(parts, part{i, r})
	}
	parts = append(parts, part{len(piece) - 1, maxRank}, part{len(piece), maxRank})
	getRank := func(i int) int {
		if i+3 < len(parts) {
			return rankOf(ranks, piece[parts[i].start:parts[i+3].start])
		}
		return maxRank
	}
	for minRank != maxRank {
		i := minIdx
		if i > 0 {
			parts[i-1].rank = getRank(i - 1)
		}
		parts[i].rank = getRank(i)
		parts = append(parts[:i+1], parts[i+2:]...)
		minRank, minIdx = maxRank, -1
		for j := 0; j < len(parts)-1; j++ {
			if parts[j].rank < minRank {
				minRank, minIdx = parts[j].rank, j
			}
		}
	}
	return parts
}

type merge struct{ start, rank int }

type mergeHeap []merge

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if h[i].rank != h[j].rank {
		return h[i].rank < h[j].rank
	}
	return h[i].start < h[j].start
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(merge)) }
func (h *mergeHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type mergeState struct{ prev, end, nextEnd, nextRank, curRank int }

// Heap driven merge for long pieces, lowest rank then leftmost start first
func bytePairMergeLarge(ranks map[string]int, piece []byte) []int {
	state := make([]mergeState, 0, len(piece))
	state = append(state, mergeState{prev: -1, end: 1, nextEnd: 2, nextRank: maxRank, curRank: maxRank})
	h := &mergeHeap{}
	for i := 0; i < len(piece)-1; i++ {
		if r, ok := ranks[string(piece[i:i+2])]; ok {
			heap.Push(h, merge{i, r})
			state[i].nextRank = r
		}
		state = append(state, mergeState{prev: i, end: i + 2, nextEnd: i + 3, nextRank: maxRank, curRank: maxRank})
	}
	potential := func(start, nextEnd int) {
		state[start].nextEnd = nextEnd
		state[start].nextRank = maxRank
		if nextEnd <= len(piece) {
			if r, ok := ranks[string(piece[start:nextEnd])]; ok {
				heap.Push(h, merge{start, r})
				state[start].nextRank = r
			}
		}
	}
	for h.Len() > 0 {
		left := heap.Pop(h).(merge)
		if left.rank == maxRank {
			break
		}
		if left.rank != state[left.start].nextRank {
			continue
		}
		leftStart := left.start
		rightStart := state[leftStart].end
		rightEnd := state[leftStart].nextEnd
		rightNextEnd := state[rightStart].nextEnd
		state[leftStart].curRank = state[leftStart].nextRank
		state[leftStart].end = rightEnd
		potential(leftStart, rightNextEnd)
		if rightEnd < len(state) {
			state[rightEnd].prev = leftStart
		}
		if leftStart > 0 {
			potential(state[leftStart].prev, rightEnd)
		}
		state[rightStart].nextRank = maxRank
	}
	var out []int
	for i := 0; i < len(state); i = state[i].end {
		if state[i].curRank != maxRank {
			out = append(out, state[i].curRank)
		} else {
			out = append(out, ranks[string(piece[i:state[i].end])])
		}
	}
	return out
}
