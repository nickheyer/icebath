package sherpaonnx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypinyin"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tokenizers/sentencepiece"
)

// What text2token reaches outside the package, the pypinyin dictionaries of the runtime environment and the stream print writes to
type Environment struct {
	Stdout         io.Writer
	ImportPypinyin func() (*pypinyin.Module, error)
}

// The keyword arguments of text2token, TokensType cjkchar when empty
type Text2TokenOptions struct {
	TokensType string
	BPEModel   *string
	Lexicon    *string
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
	text, err := pycodecs.Decode(raw, "utf-8")
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

func allCJK(s string) bool {
	for _, r := range s {
		if !isCJK(r) {
			return false
		}
	}
	return s != ""
}

func strList(items []string) string {
	list := make([]any, len(items))
	for i, item := range items {
		list[i] = item
	}
	return pyrepr.Repr(list)
}

// Pinyin of a text, whole syllables for fpinyin and initial plus toned final for ppinyin
func toPinyin(py *pypinyin.Module, txt, outType string) ([]string, error) {
	readings, err := py.Pinyin(txt)
	if err != nil {
		return nil, err
	}
	res := []string{}
	for _, reading := range readings {
		x := reading[0]
		if outType != "ppinyin" {
			res = append(res, x)
			continue
		}
		initial, final := pypinyin.ToInitialsNotStrict(x), pypinyin.ToFinalsToneNotStrict(x)
		if initial == "" && final == "" {
			res = append(res, x)
			continue
		}
		if initial != "" {
			res = append(res, initial)
		}
		if final != "" {
			res = append(res, final)
		}
	}
	return res, nil
}

// Encodes texts into token lists for a tokens.txt vocabulary, dropping every text with a token the vocabulary lacks
func (m *Module) Text2Token(texts []string, tokens string, o Text2TokenOptions) ([][]any, error) {
	tokensType := o.TokensType
	if tokensType == "" {
		tokensType = "cjkchar"
	}
	var py *pypinyin.Module
	if strings.Contains(tokensType, "pinyin") {
		if m.env.ImportPypinyin == nil {
			return nil, errors.New("token type " + tokensType + " needs pinyin dictionaries and the module was loaded without a source for them")
		}
		var err error
		if py, err = m.env.ImportPypinyin(); err != nil {
			return nil, err
		}
	}
	if !isFile(tokens) {
		return nil, &PyError{Class: "AssertionError", Msg: "File not exists, " + tokens}
	}
	tokensTable := map[string]any{}
	lines, err := readLines(tokens)
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		toks := pystr.SplitWhitespace(pystr.Strip(line), -1)
		if len(toks) != 2 {
			return nil, &PyError{Class: "AssertionError", Msg: fmt.Sprint(len(toks))}
		}
		if _, dup := tokensTable[toks[0]]; dup {
			return nil, &PyError{Class: "AssertionError", Msg: "Duplicate token: " + strList(toks) + " "}
		}
		id, ok := pyint.Parse(toks[1])
		if !ok {
			return nil, &PyError{Class: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.StrRepr(toks[1])}
		}
		if id.IsInt64() {
			tokensTable[toks[0]] = int(id.Int64())
		} else {
			tokensTable[toks[0]] = id
		}
	}
	var sp *sentencepiece.Processor
	if strings.Contains(tokensType, "bpe") {
		if o.BPEModel == nil {
			return nil, &PyError{Class: "TypeError", Msg: "argument should be a str or an os.PathLike object where __fspath__ returns a str, not 'NoneType'"}
		}
		if !isFile(*o.BPEModel) {
			return nil, &PyError{Class: "AssertionError", Msg: "File not exists, " + *o.BPEModel}
		}
		if sp, err = sentencepiece.Load(*o.BPEModel); err != nil {
			return nil, err
		}
	}
	phoneTable := map[string][]string{}
	if tokensType == "phone+ppinyin" {
		if o.Lexicon == nil {
			return nil, &PyError{Class: "AssertionError", Msg: "File not exists, None"}
		}
		if *o.Lexicon == "" || !isFile(*o.Lexicon) {
			return nil, &PyError{Class: "AssertionError", Msg: "File not exists, " + *o.Lexicon}
		}
		lexicon, err := readLines(*o.Lexicon)
		if err != nil {
			return nil, err
		}
		for _, line := range lexicon {
			toks := pystr.SplitWhitespace(pystr.Strip(line), -1)
			if len(toks) < 2 {
				return nil, &PyError{Class: "AssertionError", Msg: fmt.Sprint(len(toks))}
			}
			phoneTable[toks[0]] = toks[1:]
		}
	}
	textsList := [][]string{}
	switch tokensType {
	case "cjkchar":
		for _, text := range texts {
			chars := []string{}
			for _, r := range strings.Join(pystr.SplitWhitespace(text, -1), "") {
				chars = append(chars, string(r))
			}
			textsList = append(textsList, chars)
		}
	case "bpe":
		if textsList, err = sp.EncodeBatchAsPieces(texts); err != nil {
			return nil, err
		}
	case "ppinyin", "fpinyin":
		for _, txt := range texts {
			list, err := toPinyin(py, txt, tokensType)
			if err != nil {
				return nil, err
			}
			textsList = append(textsList, list)
		}
	case "phone+ppinyin":
		for _, text := range texts {
			textList := []string{}
			skipText := false
			for _, w := range pystr.SplitWhitespace(pystr.Strip(text), -1) {
				if phones, ok := phoneTable[w]; ok {
					textList = append(textList, phones...)
					continue
				}
				if !allCJK(w) {
					fmt.Fprintf(m.env.Stdout, "Word %s not in lexicon and it is not a CJK character, skipping text: %s.\n", w, text)
					skipText = true
					break
				}
				list, err := toPinyin(py, w, "ppinyin")
				if err != nil {
					return nil, err
				}
				textList = append(textList, list...)
			}
			if !skipText {
				textsList = append(textsList, textList)
			}
		}
	default:
		if tokensType != "cjkchar+bpe" {
			return nil, &PyError{Class: "AssertionError", Msg: "Supported tokens_type are cjkchar, bpe, cjkchar+bpe, ppinyin, fpinyin, phone+ppinyin given " + tokensType}
		}
		for _, text := range texts {
			textList := []string{}
			flush := func(segment string) error {
				if pystr.Strip(segment) == "" {
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
