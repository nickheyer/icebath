package sherpaonnx

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/tokenizers/sentencepiece"
)

// How phrases become model tokens, with the BPE model or phone lexicon the type needs
type Text2TokenOptions struct {
	TokensType string
	BPEModel   string
	Lexicon    string
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Lines of a UTF-8 text file with any line ending
func readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, nil
}

func isCJK(r rune) bool { return r >= 0x4e00 && r <= 0x9fff }

func allCJK(s string) bool {
	for _, r := range s {
		if !isCJK(r) {
			return false
		}
	}
	return s != ""
}

// Token table of a tokens.txt file, one symbol and id per line
func readTokenTable(path string) (map[string]bool, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	table := map[string]bool{}
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s line %d is not a token and its id", path, i+1)
		}
		if table[fields[0]] {
			return nil, fmt.Errorf("%s lists the token %s twice", path, fields[0])
		}
		table[fields[0]] = true
	}
	return table, nil
}

// Encodes each phrase into model tokens, nil for a phrase holding a word or token the model lacks
func Text2Token(ctx context.Context, texts []string, tokens string, o Text2TokenOptions) ([][]string, error) {
	tokensType := o.TokensType
	if tokensType == "" {
		tokensType = "cjkchar"
	}
	var py *pinyinDict
	if strings.Contains(tokensType, "pinyin") {
		var err error
		if py, err = loadPinyin(ctx); err != nil {
			return nil, err
		}
	}
	table, err := readTokenTable(tokens)
	if err != nil {
		return nil, err
	}
	var sp *sentencepiece.Processor
	if strings.Contains(tokensType, "bpe") {
		if !isFile(o.BPEModel) {
			return nil, fmt.Errorf("BPE model %s is missing", o.BPEModel)
		}
		if sp, err = sentencepiece.Load(o.BPEModel); err != nil {
			return nil, err
		}
	}
	phones := map[string][]string{}
	if tokensType == "phone+ppinyin" {
		if !isFile(o.Lexicon) {
			return nil, fmt.Errorf("phone lexicon %s is missing", o.Lexicon)
		}
		lines, err := readLines(o.Lexicon)
		if err != nil {
			return nil, err
		}
		for i, line := range lines {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return nil, fmt.Errorf("%s line %d has a word without phones", o.Lexicon, i+1)
			}
			phones[fields[0]] = fields[1:]
		}
	}
	var encoded [][]string
	switch tokensType {
	case "cjkchar":
		for _, text := range texts {
			var chars []string
			for _, r := range strings.Join(strings.Fields(text), "") {
				chars = append(chars, string(r))
			}
			encoded = append(encoded, chars)
		}
	case "bpe":
		if encoded, err = sp.EncodeBatchAsPieces(texts); err != nil {
			return nil, err
		}
	case "ppinyin", "fpinyin":
		for _, text := range texts {
			encoded = append(encoded, py.syllables(text, tokensType == "ppinyin"))
		}
	case "phone+ppinyin":
		for _, text := range texts {
			var list []string
			skip := false
			for _, w := range strings.Fields(text) {
				if p, ok := phones[w]; ok {
					list = append(list, p...)
					continue
				}
				if !allCJK(w) {
					slog.Warn("wake phrase word is neither in the phone lexicon nor Chinese, the phrase is skipped", "word", w, "phrase", text)
					skip = true
					break
				}
				list = append(list, py.syllables(w, true)...)
			}
			if skip {
				list = nil
			}
			encoded = append(encoded, list)
		}
	case "cjkchar+bpe":
		for _, text := range texts {
			var list []string
			flush := func(segment string) error {
				if strings.TrimSpace(segment) == "" {
					return nil
				}
				pieces, err := sp.EncodeAsPieces(segment)
				list = append(list, pieces...)
				return err
			}
			start := 0
			for i, r := range text {
				if !isCJK(r) {
					continue
				}
				if err := flush(text[start:i]); err != nil {
					return nil, err
				}
				list = append(list, string(r))
				start = i + len(string(r))
			}
			if err := flush(text[start:]); err != nil {
				return nil, err
			}
			encoded = append(encoded, list)
		}
	default:
		return nil, fmt.Errorf("token type %s is not one of cjkchar, bpe, cjkchar+bpe, ppinyin, fpinyin, phone+ppinyin", tokensType)
	}
	for i, list := range encoded {
		for _, t := range list {
			if !table[t] {
				slog.Warn("wake phrase holds a token the keyword model lacks, the phrase is skipped", "token", t, "tokens", strings.Join(list, " "))
				encoded[i] = nil
				break
			}
		}
	}
	return encoded, nil
}
