package tokenizers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type postProcessor func(ids []int) []int

// A token and its id as BertProcessing and RobertaProcessing write them
func tokenPair(raw json.RawMessage) (int, error) {
	var pair []json.RawMessage
	var id int
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 || json.Unmarshal(pair[1], &id) != nil {
		return 0, fmt.Errorf("tokenizers: %s is not a token with its id", truncate(raw))
	}
	return id, nil
}

func parsePostProcessor(raw json.RawMessage) (postProcessor, error) {
	if isNull(raw) {
		return nil, nil
	}
	kind, f, err := component(raw)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "ByteLevel":
		return nil, nil
	case "BertProcessing", "RobertaProcessing":
		cls, err := tokenPair(f["cls"])
		if err != nil {
			return nil, err
		}
		sep, err := tokenPair(f["sep"])
		if err != nil {
			return nil, err
		}
		return frame([]int{cls}, []int{sep}), nil
	case "TemplateProcessing":
		var single []map[string]struct {
			ID string `json:"id"`
		}
		var special map[string]struct {
			IDs []int `json:"ids"`
		}
		if err := firstErr(field(f, "single", &single), field(f, "special_tokens", &special)); err != nil {
			return nil, err
		}
		var before, after []int
		seen := false
		for _, part := range single {
			if _, ok := part["Sequence"]; ok {
				seen = true
				continue
			}
			tok, ok := part["SpecialToken"]
			if !ok {
				return nil, fmt.Errorf("tokenizers: a template piece is neither Sequence nor SpecialToken")
			}
			entry, ok := special[tok.ID]
			if !ok {
				return nil, fmt.Errorf("tokenizers: the template names the special token %q without its ids", tok.ID)
			}
			if seen {
				after = append(after, entry.IDs...)
			} else {
				before = append(before, entry.IDs...)
			}
		}
		if !seen {
			return nil, fmt.Errorf("tokenizers: the single template holds no sequence")
		}
		return frame(before, after), nil
	case "Sequence":
		parts, err := sequenceOf(f["processors"], parsePostProcessor)
		if err != nil {
			return nil, err
		}
		return func(ids []int) []int {
			for _, part := range parts {
				if part != nil {
					ids = part(ids)
				}
			}
			return ids
		}, nil
	}
	return nil, fmt.Errorf("tokenizers: post processor %q is not one the format defines", kind)
}

// The ids of a sequence between the special ids that open and close it
func frame(before, after []int) postProcessor {
	return func(ids []int) []int {
		out := make([]int, 0, len(before)+len(ids)+len(after))
		return append(append(append(out, before...), ids...), after...)
	}
}

type decoder func(tokens []string) ([]string, error)

// Length of the longest run at the head of b that begins no valid encoding
func invalidLen(b []byte) int {
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch c := b[0]; {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c == 0xED:
		need, hi = 2, 0x9F
	case c >= 0xE1 && c <= 0xEF:
		need = 2
	case c == 0xF0:
		need, lo = 3, 0x90
	case c == 0xF4:
		need, hi = 3, 0x8F
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	}
	n := 1
	for n <= need && n < len(b) && b[n] >= lo && b[n] <= hi {
		n++
		lo, hi = 0x80, 0xBF
	}
	return n
}

// Bytes as text, each run that begins no valid encoding becoming one replacement character
func lossyUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError || size > 1 {
			out.Write(b[:size])
			b = b[size:]
			continue
		}
		out.WriteString("�")
		b = b[invalidLen(b):]
	}
	return out.String()
}

func byteLevelDecoder(tokens []string) ([]string, error) {
	var raw []byte
	for _, token := range tokens {
		start := len(raw)
		for _, r := range token {
			b, ok := runeBytes[r]
			if !ok {
				raw = append(raw[:start], token...)
				break
			}
			raw = append(raw, b)
		}
	}
	return []string{lossyUTF8(raw)}, nil
}

// Text with the spaces tokenization left before punctuation and English contractions removed
func cleanText(s string) string {
	for _, pair := range [][2]string{{" .", "."}, {" ?", "?"}, {" !", "!"}, {" ,", ","}, {" ' ", "'"}, {" n't", "n't"}, {" 'm", "'m"}, {" do not", " don't"}, {" 's", "'s"}, {" 've", "'ve"}, {" 're", "'re"}} {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	return s
}

func byteFallbackDecoder(tokens []string) ([]string, error) {
	var out []string
	var pending []byte
	flush := func() {
		if len(pending) == 0 {
			return
		}
		if utf8.Valid(pending) {
			out = append(out, string(pending))
		} else {
			for range pending {
				out = append(out, "�")
			}
		}
		pending = pending[:0]
	}
	for _, token := range tokens {
		if len(token) == 6 && strings.HasPrefix(token, "<0x") && token[5] == '>' {
			if b, err := strconv.ParseUint(token[3:5], 16, 8); err == nil {
				pending = append(pending, byte(b))
				continue
			}
		}
		flush()
		out = append(out, token)
	}
	flush()
	return out, nil
}

func parseDecoder(raw json.RawMessage) (decoder, error) {
	if isNull(raw) {
		return nil, nil
	}
	kind, f, err := component(raw)
	if err != nil {
		return nil, err
	}
	each := func(change func(i int, token string) string) decoder {
		return func(tokens []string) ([]string, error) {
			out := make([]string, len(tokens))
			for i, token := range tokens {
				out[i] = change(i, token)
			}
			return out, nil
		}
	}
	switch kind {
	case "BPEDecoder":
		suffix := "</w>"
		if err := field(f, "suffix", &suffix); err != nil {
			return nil, err
		}
		return func(tokens []string) ([]string, error) {
			out := make([]string, len(tokens))
			for i, token := range tokens {
				out[i] = strings.ReplaceAll(token, suffix, " ")
				if i == len(tokens)-1 {
					out[i] = strings.ReplaceAll(token, suffix, "")
				}
			}
			return out, nil
		}, nil
	case "ByteLevel":
		return byteLevelDecoder, nil
	case "ByteFallback":
		return byteFallbackDecoder, nil
	case "Fuse":
		return func(tokens []string) ([]string, error) { return []string{strings.Join(tokens, "")}, nil }, nil
	case "WordPiece":
		prefix, clean := "##", true
		if err := firstErr(field(f, "prefix", &prefix), field(f, "cleanup", &clean)); err != nil {
			return nil, err
		}
		return each(func(i int, token string) string {
			if i != 0 {
				if rest, ok := strings.CutPrefix(token, prefix); ok {
					token = rest
				} else {
					token = " " + token
				}
			}
			if clean {
				token = cleanText(token)
			}
			return token
		}), nil
	case "Metaspace":
		replacement, scheme, _, err := metaspaceFields(f)
		if err != nil {
			return nil, err
		}
		return each(func(i int, token string) string {
			return strings.Map(func(r rune) rune {
				switch {
				case r != replacement:
					return r
				case i == 0 && scheme != "never":
					return -1
				}
				return ' '
			}, token)
		}), nil
	case "CTC":
		pad, delimiter, clean := "<pad>", "|", true
		if err := firstErr(field(f, "pad_token", &pad), field(f, "word_delimiter_token", &delimiter), field(f, "cleanup", &clean)); err != nil {
			return nil, err
		}
		return func(tokens []string) ([]string, error) {
			var out []string
			for i, token := range tokens {
				if i > 0 && token == tokens[i-1] {
					continue
				}
				token = strings.ReplaceAll(token, pad, "")
				if clean {
					token = strings.ReplaceAll(cleanText(token), delimiter, " ")
				}
				if token != "" {
					out = append(out, token)
				}
			}
			return out, nil
		}, nil
	case "Replace":
		var content string
		if err := field(f, "content", &content); err != nil {
			return nil, err
		}
		p, err := patternField(f["pattern"])
		if err != nil {
			return nil, err
		}
		return func(tokens []string) ([]string, error) {
			out := make([]string, len(tokens))
			for i, token := range tokens {
				replaced, err := text{s: token}.replace(p, content)
				if err != nil {
					return nil, err
				}
				out[i] = replaced.s
			}
			return out, nil
		}, nil
	case "Strip":
		content, start, stop := " ", 0, 0
		if err := firstErr(field(f, "content", &content), field(f, "start", &start), field(f, "stop", &stop)); err != nil {
			return nil, err
		}
		cut, size := utf8.DecodeRuneInString(content)
		if size != len(content) || size == 0 {
			return nil, fmt.Errorf("tokenizers: Strip content %q is not one character", content)
		}
		return each(func(_ int, token string) string {
			runes := []rune(token)
			a, b := 0, len(runes)
			for a < start && a < len(runes) && runes[a] == cut {
				a++
			}
			for b > len(runes)-stop && b > a && runes[b-1] == cut {
				b--
			}
			return string(runes[a:b])
		}), nil
	case "Sequence":
		parts, err := sequenceOf(f["decoders"], parseDecoder)
		if err != nil {
			return nil, err
		}
		return func(tokens []string) ([]string, error) {
			var err error
			for _, part := range parts {
				if part == nil {
					continue
				}
				if tokens, err = part(tokens); err != nil {
					return nil, err
				}
			}
			return tokens, nil
		}, nil
	}
	return nil, fmt.Errorf("tokenizers: decoder %q is not one the format defines", kind)
}
