package sentencepiece

import (
	"container/heap"
	"math"
)

// A segmentation model over normalized text
type encoder interface {
	encode(normalized string) []encoded
}

const unkPenalty = float32(10.0)

type trieNode struct {
	next map[byte]*trieNode
	id   int
}

// Unigram language model segmenting by the best scoring path
type unigramModel struct {
	*pieceTable
	root     *trieNode
	minScore float32
}

// Builds the piece trie and the lowest normal piece score
func newUnigramModel(proto *modelProto) (*unigramModel, error) {
	table, err := newPieceTable(proto, true)
	if err != nil {
		return nil, err
	}
	m := &unigramModel{pieceTable: table, root: &trieNode{id: -1}, minScore: math.MaxFloat32}
	for _, sp := range proto.pieces {
		if math.IsNaN(float64(sp.score)) || math.IsInf(float64(sp.score), 0) {
			return nil, fail("score is NaN or Inf.")
		}
		if sp.kind == pieceNormal {
			m.minScore = min(m.minScore, sp.score)
		}
	}
	if len(table.pieces) == 0 {
		return nil, fail("no pieces are loaded.")
	}
	for piece, id := range table.pieces {
		node := m.root
		for i := 0; i < len(piece); i++ {
			child := node.next[piece[i]]
			if child == nil {
				child = &trieNode{id: -1}
				if node.next == nil {
					node.next = map[byte]*trieNode{}
				}
				node.next[piece[i]] = child
			}
			node = child
		}
		node.id = id
	}
	return m, nil
}

func userDefinedScore(length int) float32 { return float32(0.1 * float64(length-1)) }

// Best path segmentation with unknown characters scored below every normal piece
func (m *unigramModel) encode(normalized string) []encoded {
	if normalized == "" {
		return nil
	}
	type bestPathNode struct {
		id            int
		bestPathScore float32
		startsAt      int
	}
	size := len(normalized)
	unkScore := m.minScore - unkPenalty
	ends := make([]bestPathNode, size+1)
	for i := range ends {
		ends[i] = bestPathNode{id: -1, startsAt: -1}
	}
	const scoreResetThreshold = float32(100000.0)
	maxFrontier := 0
	for startsAt := 0; startsAt < size; {
		till := ends[startsAt].bestPathScore
		if till < -scoreResetThreshold || till > scoreResetThreshold {
			offset := till
			for i := startsAt; i <= maxFrontier; i++ {
				if i == startsAt || ends[i].startsAt != -1 {
					ends[i].bestPathScore -= offset
				}
			}
			till = 0
		}
		hasSingleNode := false
		mblen := min(oneCharLen(normalized[startsAt]), size-startsAt)
		node := m.root
		for keyPos := startsAt; keyPos < size; {
			node = node.next[normalized[keyPos]]
			keyPos++
			if node == nil {
				break
			}
			if node.id < 0 || m.kind(node.id) == pieceUnused {
				continue
			}
			maxFrontier = max(maxFrontier, keyPos)
			target := &ends[keyPos]
			length := keyPos - startsAt
			score := m.proto.pieces[node.id].score
			if m.kind(node.id) == pieceUserDefined {
				score = userDefinedScore(length)
			}
			candidate := score + till
			if target.startsAt == -1 || candidate > target.bestPathScore {
				target.bestPathScore, target.startsAt, target.id = candidate, startsAt, node.id
			}
			if !hasSingleNode && length == mblen {
				hasSingleNode = true
			}
		}
		if !hasSingleNode {
			maxFrontier = max(maxFrontier, startsAt+mblen)
			target := &ends[startsAt+mblen]
			candidate := unkScore + till
			if target.startsAt == -1 || candidate > target.bestPathScore {
				target.bestPathScore, target.startsAt, target.id = candidate, startsAt, m.unkID
			}
		}
		startsAt += mblen
	}
	var results []encoded
	for endsAt := size; endsAt > 0; {
		node := ends[endsAt]
		results = append(results, encoded{normalized[node.startsAt:endsAt], node.id})
		endsAt = node.startsAt
	}
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return results
}

// Byte pair encoding model merging the best scoring adjacent pair first
type bpeModel struct{ *pieceTable }

func newBPEModel(proto *modelProto) (*bpeModel, error) {
	table, err := newPieceTable(proto, false)
	if err != nil {
		return nil, err
	}
	return &bpeModel{table}, nil
}

type symbolPair struct {
	score       int32
	left, right int
	size        int
}

type agenda []symbolPair

func (a agenda) Len() int      { return len(a) }
func (a agenda) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a *agenda) Push(x any)   { *a = append(*a, x.(symbolPair)) }
func (a *agenda) Pop() any {
	old := *a
	top := old[len(old)-1]
	*a = old[:len(old)-1]
	return top
}

// Orders the higher score first and the leftmost pair first among equal scores
func (a agenda) Less(i, j int) bool { return pairLess(a[j], a[i]) }

// Reports h1 ranking below h2 by score bits compared as floats, then by the larger left index
func pairLess(h1, h2 symbolPair) bool {
	i1, i2 := h1.score, h2.score
	if i1&i2 < 0 {
		return i1 > i2 || (i1 == i2 && h1.left > h2.left)
	}
	scoreLess := i1 < i2
	if i1^i2 < 0 {
		scoreLess = i1 < 0
	}
	return scoreLess || (i1 == i2 && h1.left > h2.left)
}

const maxBPEResegmentDepth = 100

// Merges adjacent symbols in score order, then splits unused pieces back into the pairs that formed them
func (m *bpeModel) encode(normalized string) []encoded {
	if normalized == "" {
		return nil
	}
	type symbol struct {
		prev, next int
		freeze     bool
		start, end int
	}
	var symbols []symbol
	for pos := 0; pos < len(normalized); {
		mblen, freeze := m.prefixMatch(normalized[pos:])
		s := symbol{prev: len(symbols) - 1, next: len(symbols) + 1, freeze: freeze, start: pos, end: pos + mblen}
		pos += mblen
		if pos >= len(normalized) {
			s.next = -1
		}
		symbols = append(symbols, s)
	}
	text := func(s symbol) string { return normalized[s.start:s.end] }
	revMerge := map[string][2]string{}
	queue := &agenda{}
	add := func(left, right int) {
		if left == -1 || right == -1 {
			return
		}
		l, r := symbols[left], symbols[right]
		if l.freeze || r.freeze {
			return
		}
		piece := normalized[l.start:r.end]
		id := m.pieceToIDNoReserved(piece)
		if id == m.unkID || m.isReservedID(id) {
			return
		}
		*queue = append(*queue, symbolPair{score: int32(math.Float32bits(m.proto.pieces[id].score)), left: left, right: right, size: len(piece)})
		if m.kind(id) == pieceUnused {
			revMerge[piece] = [2]string{text(l), text(r)}
		}
	}
	for right := 1; right < len(symbols); right++ {
		add(right-1, right)
	}
	heap.Init(queue)
	push := func(left, right int) {
		before := queue.Len()
		add(left, right)
		if queue.Len() > before {
			heap.Fix(queue, queue.Len()-1)
		}
	}
	for queue.Len() > 0 {
		top := heap.Pop(queue).(symbolPair)
		l, r := &symbols[top.left], &symbols[top.right]
		if l.end == l.start || r.end == r.start || (l.end-l.start)+(r.end-r.start) != top.size {
			continue
		}
		l.end = r.end
		l.next = r.next
		if r.next >= 0 {
			symbols[r.next].prev = top.left
		}
		r.start, r.end = 0, 0
		push(l.prev, top.left)
		push(top.left, l.next)
	}
	var output []encoded
	var resegment func(w string, depth int)
	resegment = func(w string, depth int) {
		id := m.pieceToIDNoReserved(w)
		parts, merged := revMerge[w]
		if depth > maxBPEResegmentDepth || id == -1 || m.kind(id) != pieceUnused || !merged {
			output = append(output, encoded{w, id})
			return
		}
		resegment(parts[0], depth+1)
		resegment(parts[1], depth+1)
	}
	for index := 0; index != -1; index = symbols[index].next {
		resegment(text(symbols[index]), 0)
	}
	return output
}

// Word model, one piece per whitespace delimited word
type wordModel struct{ *pieceTable }

// Splits at every space symbol, each word keeping its leading symbol
func (m *wordModel) encode(normalized string) []encoded {
	var output []encoded
	begin := 0
	for pos := 0; pos < len(normalized); {
		mblen := min(oneCharLen(normalized[pos]), len(normalized)-pos)
		if pos != 0 && normalized[pos:pos+mblen] == spaceSymbol {
			output = append(output, encoded{normalized[begin:pos], m.pieceToID(normalized[begin:pos])})
			begin = pos
		}
		pos += mblen
	}
	if begin < len(normalized) {
		output = append(output, encoded{normalized[begin:], m.pieceToID(normalized[begin:])})
	}
	return output
}

// Character model, one piece per character or user defined symbol
type charModel struct{ *pieceTable }

func (m *charModel) encode(normalized string) []encoded {
	var output []encoded
	for len(normalized) > 0 {
		mblen, _ := m.prefixMatch(normalized)
		output = append(output, encoded{normalized[:mblen], m.pieceToID(normalized[:mblen])})
		normalized = normalized[mblen:]
	}
	return output
}

// Model of the type the trainer spec names with its piece table
func newModel(proto *modelProto) (encoder, *pieceTable, error) {
	switch proto.modelType {
	case modelBPE:
		m, err := newBPEModel(proto)
		if err != nil {
			return nil, nil, err
		}
		return m, m.pieceTable, nil
	case modelWord, modelChar:
		table, err := newPieceTable(proto, true)
		if err != nil {
			return nil, nil, err
		}
		if proto.modelType == modelWord {
			return &wordModel{table}, table, nil
		}
		return &charModel{table}, table, nil
	}
	m, err := newUnigramModel(proto)
	if err != nil {
		return nil, nil, err
	}
	return m, m.pieceTable, nil
}
