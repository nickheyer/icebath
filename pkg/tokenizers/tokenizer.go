package tokenizers

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// AddedToken is a token cut out of the text before the model runs
type AddedToken struct {
	ID         int    `json:"id"`
	Content    string `json:"content"`
	SingleWord bool   `json:"single_word"`
	LStrip     bool   `json:"lstrip"`
	RStrip     bool   `json:"rstrip"`
	Normalized bool   `json:"normalized"`
	Special    bool   `json:"special"`
}

// matcher finds the leftmost longest added token at each position of a text
type matcher struct {
	edges  map[uint32]int32
	ends   []int32
	tokens []AddedToken
	first  [256]bool
}

func newMatcher() *matcher { return &matcher{edges: map[uint32]int32{}, ends: []int32{-1}} }

func (m *matcher) add(content string, token AddedToken) {
	if content == "" {
		return
	}
	m.first[content[0]] = true
	node := int32(0)
	for i := 0; i < len(content); i++ {
		key := uint32(node)<<8 | uint32(content[i])
		next, ok := m.edges[key]
		if !ok {
			next = int32(len(m.ends))
			m.ends = append(m.ends, -1)
			m.edges[key] = next
		}
		node = next
	}
	if m.ends[node] < 0 {
		m.ends[node] = int32(len(m.tokens))
		m.tokens = append(m.tokens, token)
	}
}

// The longest token that starts at byte i and where it ends
func (m *matcher) at(s string, i int) (*AddedToken, int) {
	if !m.first[s[i]] {
		return nil, 0
	}
	var found *AddedToken
	end := 0
	node := int32(0)
	for j := i; j < len(s); j++ {
		next, ok := m.edges[uint32(node)<<8|uint32(s[j])]
		if !ok {
			break
		}
		node = next
		if idx := m.ends[node]; idx >= 0 {
			found, end = &m.tokens[idx], j+1
		}
	}
	return found, end
}

func lastIsWord(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return s != "" && isWord(r)
}

func firstIsWord(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return s != "" && isWord(r)
}

// The text as pieces, each added token fixed with its id and the stretches between them left open
func (m *matcher) cut(t text) []piece {
	s := t.s
	var out []piece
	last := 0
	for i := 0; i < len(s); {
		token, end := m.at(s, i)
		if token == nil {
			i++
			continue
		}
		start, stop := i, end
		i = end
		if token.SingleWord && (lastIsWord(s[:start]) || firstIsWord(s[stop:])) {
			continue
		}
		if token.LStrip {
			start = max(len(strings.TrimRightFunc(s[:start], isSpace)), last)
		}
		if token.RStrip {
			stop = len(s) - len(strings.TrimLeftFunc(s[stop:], isSpace))
		}
		if last < start {
			out = append(out, piece{text: t.slice(last, start)})
		}
		out = append(out, piece{text: t.slice(min(start, stop), stop), id: token.ID, fixed: true})
		last = stop
	}
	if last < len(s) {
		out = append(out, piece{text: t.slice(last, len(s))})
	}
	return out
}

// Tokenizer runs the pipeline one tokenizer.json describes
type Tokenizer struct {
	normalizer normalizer
	pre        preTokenizer
	model      model
	post       postProcessor
	decoder    decoder
	added      []AddedToken
	shown      []string
	byID       map[int]int
	byContent  map[string]int
	raw        *matcher
	cooked     *matcher
}

// FromJSON builds the tokenizer a tokenizer.json file describes
func FromJSON(data []byte) (*Tokenizer, error) {
	var doc struct {
		AddedTokens   []AddedToken    `json:"added_tokens"`
		Normalizer    json.RawMessage `json:"normalizer"`
		PreTokenizer  json.RawMessage `json:"pre_tokenizer"`
		PostProcessor json.RawMessage `json:"post_processor"`
		Decoder       json.RawMessage `json:"decoder"`
		Model         json.RawMessage `json:"model"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("tokenizers: tokenizer.json: %w", err)
	}
	t := &Tokenizer{byID: map[int]int{}, byContent: map[string]int{}}
	var err error
	if t.normalizer, err = parseNormalizer(doc.Normalizer); err != nil {
		return nil, err
	}
	if t.pre, err = parsePreTokenizer(doc.PreTokenizer); err != nil {
		return nil, err
	}
	if t.model, err = parseModel(doc.Model); err != nil {
		return nil, err
	}
	if t.post, err = parsePostProcessor(doc.PostProcessor); err != nil {
		return nil, err
	}
	if t.decoder, err = parseDecoder(doc.Decoder); err != nil {
		return nil, err
	}
	return t, t.addTokens(doc.AddedTokens)
}

// The id of a token, an added token before a model token
func (t *Tokenizer) tokenID(token string) (int, bool) {
	if i, ok := t.byContent[token]; ok {
		return t.added[i].ID, true
	}
	return t.model.id(token)
}

// Adds tokens under their ids, a token already added keeping its place with the new settings
func (t *Tokenizer) addTokens(tokens []AddedToken) error {
	for _, token := range tokens {
		if token.Content == "" {
			continue
		}
		if i, ok := t.byContent[token.Content]; ok {
			delete(t.byID, t.added[i].ID)
			t.added[i] = token
			t.byID[token.ID] = i
			continue
		}
		t.byContent[token.Content] = len(t.added)
		t.byID[token.ID] = len(t.added)
		t.added = append(t.added, token)
	}
	t.raw, t.cooked = newMatcher(), newMatcher()
	t.shown = make([]string, len(t.added))
	for pass := 0; pass < 2; pass++ {
		for i, token := range t.added {
			if token.Special != (pass == 0) {
				continue
			}
			t.shown[i] = token.Content
			if !token.Normalized {
				t.raw.add(token.Content, token)
				continue
			}
			content := newText(token.Content)
			if t.normalizer != nil {
				var err error
				if content, err = t.normalizer(content); err != nil {
					return err
				}
			}
			t.shown[i] = content.s
			t.cooked.add(content.s, token)
		}
	}
	return nil
}

// The id a new added token takes, its vocabulary id or the first id past every known one
func (t *Tokenizer) nextID(content string) int {
	if id, ok := t.tokenID(content); ok {
		return id
	}
	next := 0
	for id := range t.byID {
		next = max(next, id+1)
	}
	for {
		if _, ok := t.model.token(next); !ok {
			return next
		}
		next++
	}
}

// Encode returns the token ids of a text with the special tokens the post processor adds
func (t *Tokenizer) Encode(s string) ([]int, error) {
	var pieces []piece
	for _, p := range t.raw.cut(newText(strings.ToValidUTF8(s, "�"))) {
		if p.fixed {
			pieces = append(pieces, p)
			continue
		}
		if t.normalizer != nil {
			var err error
			if p.text, err = t.normalizer(p.text); err != nil {
				return nil, err
			}
		}
		pieces = append(pieces, t.cooked.cut(p.text)...)
	}
	if t.pre != nil {
		var err error
		if pieces, err = t.pre(pieces); err != nil {
			return nil, err
		}
	}
	ids := make([]int, 0, len(s)/3+2)
	for _, p := range pieces {
		if p.fixed {
			ids = append(ids, p.id)
			continue
		}
		got, err := t.model.tokenize(p.s)
		if err != nil {
			return nil, err
		}
		ids = append(ids, got...)
	}
	if t.post != nil {
		ids = t.post(ids)
	}
	return ids, nil
}

// Decode returns the text of token ids, leaving special tokens out when skipSpecial is set
func (t *Tokenizer) Decode(ids []int, skipSpecial bool) (string, error) {
	tokens := make([]string, 0, len(ids))
	for _, id := range ids {
		if i, ok := t.byID[id]; ok {
			if !skipSpecial || !t.added[i].Special {
				tokens = append(tokens, t.shown[i])
			}
			continue
		}
		if token, ok := t.model.token(id); ok {
			tokens = append(tokens, token)
		}
	}
	if t.decoder == nil {
		return strings.Join(tokens, " "), nil
	}
	out, err := t.decoder(tokens)
	if err != nil {
		return "", err
	}
	return strings.Join(out, ""), nil
}
