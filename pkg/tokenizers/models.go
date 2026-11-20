package tokenizers

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"unicode/utf8"
)

// model turns one pre tokenized word into token ids and maps tokens to ids both ways
type model interface {
	tokenize(word string) ([]int, error)
	id(token string) (int, bool)
	token(id int) (string, bool)
}

// vocabulary is the token table shared by the dictionary models
type vocabulary struct {
	ids    map[string]int
	tokens map[int]string
}

func newVocabulary(ids map[string]int) vocabulary {
	v := vocabulary{ids: ids, tokens: make(map[int]string, len(ids))}
	for token, id := range ids {
		v.tokens[id] = token
	}
	return v
}

func (v vocabulary) id(token string) (int, bool) {
	id, ok := v.ids[token]
	return id, ok
}

func (v vocabulary) token(id int) (string, bool) {
	token, ok := v.tokens[id]
	return token, ok
}

// wordCache remembers the ids of up to ten thousand short words
type wordCache struct {
	mu    sync.RWMutex
	words map[string][]int
}

func (c *wordCache) get(word string) ([]int, bool) {
	c.mu.RLock()
	ids, ok := c.words[word]
	c.mu.RUnlock()
	return ids, ok
}

func (c *wordCache) put(word string, ids []int) {
	if len(word) > 256 {
		return
	}
	c.mu.Lock()
	if c.words == nil {
		c.words = map[string][]int{}
	}
	if len(c.words) < 10000 {
		c.words[word] = ids
	}
	c.mu.Unlock()
}

// bpe is the byte pair encoding model
type bpe struct {
	vocabulary
	merges       map[[2]int][2]int
	unk          *string
	prefix       *string
	suffix       *string
	fuseUnk      bool
	byteFallback bool
	ignoreMerges bool
	dropout      float64
	cache        wordCache
}

// Ranks each merge pair by its position and resolves the token it forms
func (m *bpe) setMerges(pairs [][2]string) error {
	skip := 0
	if m.prefix != nil {
		skip = len(*m.prefix)
	}
	for rank, pair := range pairs {
		a, okA := m.ids[pair[0]]
		b, okB := m.ids[pair[1]]
		if !okA || !okB {
			return fmt.Errorf("tokenizers: merge %d joins %q and %q, which the vocabulary does not hold", rank, pair[0], pair[1])
		}
		if len(pair[1]) < skip {
			return fmt.Errorf("tokenizers: merge %d joins %q, shorter than the continuing prefix", rank, pair[1])
		}
		joined, ok := m.ids[pair[0]+pair[1][skip:]]
		if !ok {
			return fmt.Errorf("tokenizers: merge %d forms %q, which the vocabulary does not hold", rank, pair[0]+pair[1][skip:])
		}
		m.merges[[2]int{a, b}] = [2]int{rank, joined}
	}
	return nil
}

type symbol struct {
	id         int
	prev, next int
	gone       bool
}

type merge struct{ pos, rank, id int }

type mergeQueue []merge

func (q mergeQueue) Len() int { return len(q) }
func (q mergeQueue) Less(i, j int) bool {
	if q[i].rank != q[j].rank {
		return q[i].rank < q[j].rank
	}
	return q[i].pos < q[j].pos
}
func (q mergeQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *mergeQueue) Push(x any)   { *q = append(*q, x.(merge)) }
func (q *mergeQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

// The symbols of a word after every merge the table allows, lowest rank first
func (m *bpe) mergeAll(symbols []symbol) []int {
	queue := make(mergeQueue, 0, len(symbols))
	for i := 0; i+1 < len(symbols); i++ {
		if hit, ok := m.merges[[2]int{symbols[i].id, symbols[i+1].id}]; ok {
			queue = append(queue, merge{i, hit[0], hit[1]})
		}
	}
	heap.Init(&queue)
	var skipped []merge
	for queue.Len() > 0 {
		top := heap.Pop(&queue).(merge)
		if m.dropout > 0 && rand.Float64() < m.dropout {
			skipped = append(skipped, top)
			continue
		}
		for _, s := range skipped {
			heap.Push(&queue, s)
		}
		skipped = skipped[:0]
		cur := &symbols[top.pos]
		if cur.gone || cur.next < 0 {
			continue
		}
		right := symbols[cur.next]
		if hit, ok := m.merges[[2]int{cur.id, right.id}]; !ok || hit[1] != top.id {
			continue
		}
		symbols[cur.next].gone = true
		cur.id, cur.next = top.id, right.next
		if cur.next >= 0 {
			symbols[cur.next].prev = top.pos
			if hit, ok := m.merges[[2]int{cur.id, symbols[cur.next].id}]; ok {
				heap.Push(&queue, merge{top.pos, hit[0], hit[1]})
			}
		}
		if cur.prev >= 0 {
			if hit, ok := m.merges[[2]int{symbols[cur.prev].id, cur.id}]; ok {
				heap.Push(&queue, merge{cur.prev, hit[0], hit[1]})
			}
		}
	}
	ids := make([]int, 0, len(symbols))
	for _, s := range symbols {
		if !s.gone {
			ids = append(ids, s.id)
		}
	}
	return ids
}

func (m *bpe) tokenize(word string) ([]int, error) {
	if word == "" {
		return nil, nil
	}
	if m.dropout == 0 {
		if m.ignoreMerges {
			if id, ok := m.ids[word]; ok {
				return []int{id}, nil
			}
		}
		if ids, ok := m.cache.get(word); ok {
			return ids, nil
		}
	}
	var symbols []symbol
	add := func(id int) {
		n := len(symbols)
		if n > 0 {
			symbols[n-1].next = n
		}
		symbols = append(symbols, symbol{id: id, prev: n - 1, next: -1})
	}
	unk, pending := -1, false
	for i := 0; i < len(word); {
		_, size := utf8.DecodeRuneInString(word[i:])
		s := word[i : i+size]
		if i > 0 && m.prefix != nil {
			s = *m.prefix + s
		}
		if i+size == len(word) && m.suffix != nil {
			s += *m.suffix
		}
		i += size
		if id, ok := m.ids[s]; ok {
			if pending {
				add(unk)
				pending = false
			}
			add(id)
			continue
		}
		if m.byteFallback {
			bytes := make([]int, 0, len(s))
			for j := 0; j < len(s); j++ {
				id, ok := m.ids[fmt.Sprintf("<0x%02X>", s[j])]
				if !ok {
					bytes = nil
					break
				}
				bytes = append(bytes, id)
			}
			if bytes != nil {
				for _, id := range bytes {
					add(id)
				}
				continue
			}
		}
		if m.unk == nil {
			continue
		}
		if pending && !m.fuseUnk {
			add(unk)
		}
		id, ok := m.ids[*m.unk]
		if !ok {
			return nil, fmt.Errorf("tokenizers: the unknown token %q is not in the vocabulary", *m.unk)
		}
		unk, pending = id, true
	}
	if pending {
		add(unk)
	}
	ids := m.mergeAll(symbols)
	if m.dropout == 0 {
		m.cache.put(word, ids)
	}
	return ids, nil
}

// wordPiece is the greedy longest match first model
type wordPiece struct {
	vocabulary
	unk      string
	prefix   string
	maxChars int
}

func (m *wordPiece) tokenize(word string) ([]int, error) {
	unk := func() ([]int, error) {
		id, ok := m.ids[m.unk]
		if !ok {
			return nil, fmt.Errorf("tokenizers: the unknown token %q is not in the vocabulary", m.unk)
		}
		return []int{id}, nil
	}
	if utf8.RuneCountInString(word) > m.maxChars {
		return unk()
	}
	var ids []int
	for start := 0; start < len(word); {
		end, found := len(word), -1
		for start < end {
			sub := word[start:end]
			if start > 0 {
				sub = m.prefix + sub
			}
			if id, ok := m.ids[sub]; ok {
				found = id
				break
			}
			_, size := utf8.DecodeLastRuneInString(word[:end])
			end -= size
		}
		if found < 0 {
			return unk()
		}
		ids = append(ids, found)
		start = end
	}
	return ids, nil
}

// wordLevel maps whole words to ids
type wordLevel struct {
	vocabulary
	unk string
}

func (m *wordLevel) tokenize(word string) ([]int, error) {
	if id, ok := m.ids[word]; ok {
		return []int{id}, nil
	}
	if id, ok := m.ids[m.unk]; ok {
		return []int{id}, nil
	}
	return nil, fmt.Errorf("tokenizers: %q is not in the vocabulary and neither is the unknown token %q", word, m.unk)
}

// unigram segments a word along the highest scoring path through its vocabulary
type unigram struct {
	tokens       []string
	scores       []float64
	ids          map[string]int
	edges        map[uint32]int32
	ends         []int32
	unk          int
	minScore     float64
	byteFallback bool
	cache        wordCache
}

func newUnigram(tokens []string, scores []float64, unk int, byteFallback bool) (*unigram, error) {
	if unk >= len(tokens) {
		return nil, fmt.Errorf("tokenizers: the unknown id %d is past the %d tokens of the vocabulary", unk, len(tokens))
	}
	m := &unigram{tokens: tokens, scores: scores, ids: make(map[string]int, len(tokens)), edges: make(map[uint32]int32, len(tokens)*3),
		ends: []int32{-1}, unk: unk, minScore: math.Inf(1), byteFallback: byteFallback}
	for id, token := range tokens {
		m.ids[token] = id
		m.minScore = math.Min(m.minScore, scores[id])
		node := int32(0)
		for i := 0; i < len(token); i++ {
			key := uint32(node)<<8 | uint32(token[i])
			next, ok := m.edges[key]
			if !ok {
				next = int32(len(m.ends))
				m.ends = append(m.ends, -1)
				m.edges[key] = next
			}
			node = next
		}
		if token != "" {
			m.ends[node] = int32(id)
		}
	}
	return m, nil
}

func (m *unigram) id(token string) (int, bool) {
	id, ok := m.ids[token]
	return id, ok
}

func (m *unigram) token(id int) (string, bool) {
	if id < 0 || id >= len(m.tokens) {
		return "", false
	}
	return m.tokens[id], true
}

type pathNode struct {
	id    int
	score float64
	start int
}

// The substrings of the best path, runs of unknown characters joined into one
func (m *unigram) segment(word string) ([]string, error) {
	best := make([]pathNode, len(word)+1)
	for i := range best {
		best[i].start = -1
	}
	unkScore := m.minScore - 10
	for at := 0; at < len(word); {
		here := best[at].score
		_, size := utf8.DecodeRuneInString(word[at:])
		single := false
		node := int32(0)
		for i := at; i < len(word); i++ {
			next, ok := m.edges[uint32(node)<<8|uint32(word[i])]
			if !ok {
				break
			}
			node = next
			id := m.ends[node]
			if id < 0 {
				continue
			}
			target := &best[i+1]
			if score := m.scores[id] + here; target.start < 0 || score > target.score {
				*target = pathNode{int(id), score, at}
			}
			if i+1-at == size {
				single = true
			}
		}
		if !single {
			target := &best[at+size]
			if score := unkScore + here; target.start < 0 || score > target.score {
				if m.unk < 0 {
					return nil, fmt.Errorf("tokenizers: %q needs the unknown token and the vocabulary names none", word)
				}
				*target = pathNode{m.unk, score, at}
			}
		}
		at += size
	}
	var out, fused []string
	flush := func() {
		if len(fused) > 0 {
			for i, j := 0, len(fused)-1; i < j; i, j = i+1, j-1 {
				fused[i], fused[j] = fused[j], fused[i]
			}
			out = append(out, strings.Join(fused, ""))
			fused = nil
		}
	}
	for end := len(word); end > 0; {
		node := best[end]
		if m.unk >= 0 && node.id == m.unk {
			fused = append(fused, word[node.start:end])
		} else {
			flush()
			out = append(out, word[node.start:end])
		}
		end = node.start
	}
	flush()
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (m *unigram) tokenize(word string) ([]int, error) {
	if word == "" {
		return nil, nil
	}
	if ids, ok := m.cache.get(word); ok {
		return ids, nil
	}
	parts, err := m.segment(word)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(parts))
next:
	for _, part := range parts {
		if id, ok := m.ids[part]; ok {
			ids = append(ids, id)
			continue
		}
		if m.byteFallback {
			bytes := make([]int, 0, len(part))
			for i := 0; i < len(part); i++ {
				id, ok := m.ids[fmt.Sprintf("<0x%02X>", part[i])]
				if !ok {
					bytes = nil
					break
				}
				bytes = append(bytes, id)
			}
			if bytes != nil {
				ids = append(ids, bytes...)
				continue next
			}
		}
		if m.unk < 0 {
			return nil, fmt.Errorf("tokenizers: %q is not in the vocabulary, which names no unknown token", part)
		}
		ids = append(ids, m.unk)
	}
	m.cache.put(word, ids)
	return ids, nil
}

// The merge pairs of a model, each written as one "left right" string or as a two element list
func parseMerges(raw json.RawMessage) ([][2]string, error) {
	if isNull(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("tokenizers: merges: %w", err)
	}
	out := make([][2]string, len(items))
	for i, item := range items {
		var line string
		if json.Unmarshal(item, &line) == nil {
			parts := strings.Split(line, " ")
			if len(parts) != 2 {
				return nil, fmt.Errorf("tokenizers: merge %d %q is not two tokens", i, line)
			}
			out[i] = [2]string{parts[0], parts[1]}
			continue
		}
		var pair []string
		if err := json.Unmarshal(item, &pair); err != nil || len(pair) != 2 {
			return nil, fmt.Errorf("tokenizers: merge %d %s is not two tokens", i, truncate(item))
		}
		out[i] = [2]string{pair[0], pair[1]}
	}
	return out, nil
}

// The model of a tokenizer.json, its type inferred from its fields when an old file names none
func parseModel(raw json.RawMessage) (model, error) {
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil || f == nil {
		return nil, fmt.Errorf("tokenizers: the model is not an object")
	}
	var kind string
	if err := field(f, "type", &kind); err != nil {
		return nil, err
	}
	if kind == "" {
		switch {
		case f["merges"] != nil:
			kind = "BPE"
		case f["max_input_chars_per_word"] != nil || f["continuing_subword_prefix"] != nil:
			kind = "WordPiece"
		case f["unk_id"] != nil || strings.HasPrefix(strings.TrimSpace(string(f["vocab"])), "["):
			kind = "Unigram"
		default:
			kind = "WordLevel"
		}
	}
	if kind == "Unigram" {
		var entries [][2]json.RawMessage
		unk, byteFallback := -1, false
		if err := firstErr(field(f, "vocab", &entries), field(f, "unk_id", &unk), field(f, "byte_fallback", &byteFallback)); err != nil {
			return nil, err
		}
		tokens, scores := make([]string, len(entries)), make([]float64, len(entries))
		for i, entry := range entries {
			if err := firstErr(json.Unmarshal(entry[0], &tokens[i]), json.Unmarshal(entry[1], &scores[i])); err != nil {
				return nil, fmt.Errorf("tokenizers: vocabulary entry %d: %w", i, err)
			}
		}
		return newUnigram(tokens, scores, unk, byteFallback)
	}
	ids := map[string]int{}
	if err := field(f, "vocab", &ids); err != nil {
		return nil, err
	}
	switch kind {
	case "BPE":
		m := &bpe{vocabulary: newVocabulary(ids), merges: map[[2]int][2]int{}}
		var dropout *float64
		if err := firstErr(field(f, "unk_token", &m.unk), field(f, "continuing_subword_prefix", &m.prefix), field(f, "end_of_word_suffix", &m.suffix),
			field(f, "fuse_unk", &m.fuseUnk), field(f, "byte_fallback", &m.byteFallback), field(f, "ignore_merges", &m.ignoreMerges), field(f, "dropout", &dropout)); err != nil {
			return nil, err
		}
		if dropout != nil {
			if *dropout < 0 || *dropout > 1 {
				return nil, fmt.Errorf("tokenizers: dropout %v is outside 0 to 1", *dropout)
			}
			m.dropout = *dropout
		}
		pairs, err := parseMerges(f["merges"])
		if err != nil {
			return nil, err
		}
		return m, m.setMerges(pairs)
	case "WordPiece":
		m := &wordPiece{vocabulary: newVocabulary(ids), unk: "[UNK]", prefix: "##", maxChars: 100}
		return m, firstErr(field(f, "unk_token", &m.unk), field(f, "continuing_subword_prefix", &m.prefix), field(f, "max_input_chars_per_word", &m.maxChars))
	case "WordLevel":
		m := &wordLevel{vocabulary: newVocabulary(ids), unk: "<unk>"}
		return m, field(f, "unk_token", &m.unk)
	}
	return nil, fmt.Errorf("tokenizers: model %q is not one the format defines", kind)
}
