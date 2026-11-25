package sherpaonnx

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/tokenizers/sentencepiece"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// What text2token reaches outside the package, the stream print writes to
type Environment struct {
	Stdout io.Writer
}

// The keyword arguments of text2token, TokensType cjkchar when empty
type Text2TokenOptions struct {
	TokensType string
	BPEModel   *string
	OutputIDs  bool
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Lines of a UTF-8 text file read with universal newlines
func readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := charsetutil.Decode(raw, "utf-8")
	if err != nil {
		return nil, err
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, nil
}

func isCJK(r rune) bool { return r >= 0x4e00 && r <= 0x9fff }

func strList(items []string) string {
	list := make([]any, len(items))
	for i, item := range items {
		list[i] = item
	}
	return anyutil.Quote(list)
}

// Encodes texts into token lists for a tokens.txt vocabulary, dropping every text with a token the vocabulary lacks
func (m *Module) Text2Token(texts []string, tokens string, o Text2TokenOptions) ([][]any, error) {
	tokensType := o.TokensType
	if tokensType == "" {
		tokensType = "cjkchar"
	}
	if !isFile(tokens) {
		return nil, &KindError{Class: "AssertionError", Msg: "File not exists, " + tokens}
	}
	tokensTable := map[string]any{}
	lines, err := readLines(tokens)
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		toks := strings.Fields(strings.TrimSpace(line))
		if len(toks) != 2 {
			return nil, &KindError{Class: "AssertionError", Msg: fmt.Sprint(len(toks))}
		}
		if _, dup := tokensTable[toks[0]]; dup {
			return nil, &KindError{Class: "AssertionError", Msg: "Duplicate token: " + strList(toks) + " "}
		}
		id, err := textutil.ParseInt(toks[1])
		if err != nil {
			return nil, &KindError{Class: "ValueError", Msg: "token id " + strconv.Quote(toks[1]) + " in " + tokens + " is not a whole number"}
		}
		tokensTable[toks[0]] = int(id)
	}
	var sp *sentencepiece.Processor
	if strings.Contains(tokensType, "bpe") {
		if o.BPEModel == nil {
			return nil, &KindError{Class: "TypeError", Msg: "tokens type " + tokensType + " needs the path of a BPE model"}
		}
		if !isFile(*o.BPEModel) {
			return nil, &KindError{Class: "AssertionError", Msg: "File not exists, " + *o.BPEModel}
		}
		if sp, err = sentencepiece.Load(*o.BPEModel); err != nil {
			return nil, err
		}
	}
	textsList := [][]string{}
	switch tokensType {
	case "cjkchar":
		for _, text := range texts {
			chars := []string{}
			for _, r := range strings.Join(strings.Fields(text), "") {
				chars = append(chars, string(r))
			}
			textsList = append(textsList, chars)
		}
	case "bpe":
		if textsList, err = sp.EncodeBatchAsPieces(texts); err != nil {
			return nil, err
		}
	default:
		if tokensType != "cjkchar+bpe" {
			return nil, &KindError{Class: "AssertionError", Msg: "Supported tokens_type are cjkchar, bpe, cjkchar+bpe given " + tokensType}
		}
		for _, text := range texts {
			textList := []string{}
			flush := func(segment string) error {
				if strings.TrimSpace(segment) == "" {
					return nil
				}
				pieces, err := sp.EncodeAsPieces(segment)
				textList = append(textList, pieces...)
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
				textList = append(textList, string(r))
				start = i + len(string(r))
			}
			if err := flush(text[start:]); err != nil {
				return nil, err
			}
			textsList = append(textsList, textList)
		}
	}
	result := [][]any{}
	for _, text := range textsList {
		textList := []any{}
		containOOV := false
		for _, txt := range text {
			id, ok := tokensTable[txt]
			if !ok {
				fmt.Fprintf(m.env.Stdout, "Can't find token %s in token table, check your tokens.txt see if %s in it. skipping text : %s.\n", txt, txt, strList(text))
				containOOV = true
				break
			}
			if o.OutputIDs {
				textList = append(textList, id)
			} else {
				textList = append(textList, txt)
			}
		}
		if !containOOV {
			result = append(result, textList)
		}
	}
	return result, nil
}
