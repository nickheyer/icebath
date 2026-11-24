package pythonutil

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
)

// Module tree of Python source text, already decoded so a coding declaration is not applied; the error is a *SyntaxError or a *LimitError
func Parse(src string) (*Node, error) {
	text, err := checkText(src, true)
	if err != nil {
		return nil, err
	}
	tree, _, _, err := parseModule(text, "")
	return tree, err
}

// Module tree of the bytes of a Python file, decoded as its BOM or coding declaration says and as UTF-8 otherwise; the error is a *SyntaxError or a *LimitError
func ParseBytes(src []byte) (*Node, error) {
	text, err := decodeFile(src, "")
	if err != nil {
		return nil, err
	}
	tree, _, _, err := parseModule(text, "")
	return tree, err
}

// Expression tree of source text in eval mode, an Expression node
func parseExpression(src string) (*Node, error) {
	text, err := checkText(src, false)
	if err != nil {
		return nil, err
	}
	p := newParser(text, "")
	tree, err := p.run(p.evalInput)
	if err == nil {
		err = checkDepth(tree)
	}
	if err != nil {
		return nil, finishError(err, "")
	}
	return tree, nil
}

// Parses translated text into a Module and checks its depth and its future statements
func parseModule(text, filename string) (*Node, *lexer, futureInfo, error) {
	p := newParser(text, filename)
	tree, err := p.run(p.file)
	if err == nil {
		err = checkDepth(tree)
	}
	var future futureInfo
	if err == nil {
		future, err = futureParse(tree, p.lx, filename)
	}
	if err != nil {
		return nil, nil, futureInfo{}, finishError(err, filename)
	}
	return tree, p.lx, future, nil
}

func finishError(err error, filename string) error {
	if se, ok := err.(*SyntaxError); ok {
		return se.finish(filename)
	}
	return err
}

func nullBytesError(filename string) *SyntaxError {
	return &SyntaxError{Msg: "source code string cannot contain null bytes", Filename: filename}
}

// Text with its line ends translated, or the error for a null byte or a byte that is not UTF-8, located in the translated lines
func checkText(src string, module bool) (string, error) {
	if strings.IndexByte(src, 0) >= 0 {
		return "", nullBytesError("")
	}
	text := translateNewlines(src, module)
	if err := checkUTF8(text); err != nil {
		return "", err.finish("")
	}
	return text, nil
}

// Text of file bytes: newlines translated, a null byte rejected, then decoded as the BOM or the coding declaration says
func decodeFile(src []byte, filename string) (string, error) {
	if bytes.IndexByte(src, 0) >= 0 {
		return "", nullBytesError(filename)
	}
	text, err := decodeSource(translateNewlines(string(src), true))
	if err != nil {
		return "", err.finish(filename)
	}
	return text, nil
}

// Converts CRLF and CR line ends to LF, ending the last line with one for module source
func translateNewlines(src string, module bool) string {
	if strings.IndexByte(src, '\r') < 0 {
		if !module || strings.HasSuffix(src, "\n") {
			return src
		}
		return src + "\n"
	}
	var b strings.Builder
	b.Grow(len(src) + 1)
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\r' {
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
			c = '\n'
		}
		b.WriteByte(c)
	}
	if module && !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// Coding declared on one line as PEP 263 spells it, false when the line declares none
func codingSpec(line string) (string, bool) {
	i := 0
	for ; i < len(line)-6; i++ {
		if line[i] == '#' {
			break
		}
		if line[i] != ' ' && line[i] != '\t' && line[i] != '\014' {
			return "", false
		}
	}
	for ; i < len(line)-6; i++ {
		if !strings.HasPrefix(line[i:], "coding") {
			continue
		}
		t := i + 6
		if t >= len(line) || (line[t] != ':' && line[t] != '=') {
			continue
		}
		t++
		for t < len(line) && (line[t] == ' ' || line[t] == '\t') {
			t++
		}
		begin := t
		for t < len(line) && (isAlnumByte(line[t]) || line[t] == '-' || line[t] == '_' || line[t] == '.') {
			t++
		}
		if begin < t {
			return normalName(line[begin:t]), true
		}
	}
	return "", false
}

func isAlnumByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Canonical spelling of the utf-8 and latin-1 declarations, any other name as written
func normalName(s string) string {
	buf := make([]byte, 0, 12)
	for i := 0; i < len(s) && i < 12; i++ {
		c := s[i]
		if c == '_' {
			c = '-'
		} else if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf = append(buf, c)
	}
	b := string(buf)
	switch {
	case b == "utf-8" || strings.HasPrefix(b, "utf-8-"):
		return "utf-8"
	case b == "latin-1" || b == "iso-8859-1" || b == "iso-latin-1" || strings.HasPrefix(b, "latin-1-") || strings.HasPrefix(b, "iso-8859-1-") || strings.HasPrefix(b, "iso-latin-1-"):
		return "iso-8859-1"
	}
	return s
}

// Whether a line holds nothing but blanks or a comment, so a declaration on the next line still counts
func onlyCommentLine(line string) bool {
	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case '#', '\n', '\r':
			return true
		case ' ', '\t', '\014':
		default:
			return false
		}
	}
	return true
}

// End of the line starting at start and the start of the next one
func splitLine(src string, start int) (int, int) {
	end := start
	for end < len(src) && src[end] != '\n' {
		end++
	}
	next := end
	if next < len(src) {
		next++
	}
	return end, next
}

// Text of translated source bytes after the UTF-8 BOM, decoded as the coding declaration of line one or two says
func decodeSource(src string) (string, *SyntaxError) {
	bom := strings.HasPrefix(src, "\xef\xbb\xbf")
	if bom {
		src = src[3:]
	}
	enc := ""
	pos := 0
	for lineNo := 1; lineNo <= 2 && pos < len(src); lineNo++ {
		end, next := splitLine(src, pos)
		line := src[pos:end]
		if spec, ok := codingSpec(line); ok {
			if bom && spec != "utf-8" {
				return "", &SyntaxError{Msg: "encoding problem: " + spec + " with BOM", Line: lineNo, EndLine: lineNo, EndColumn: charCount(line), Text: strings.ToValidUTF8(line, "�")}
			}
			enc = spec
			break
		}
		if !onlyCommentLine(line) {
			break
		}
		pos = next
	}
	if enc == "" || enc == "utf-8" {
		if err := checkUTF8(src); err != nil {
			return "", err
		}
		return src, nil
	}
	name, ok := charsetutil.Canonical(enc)
	if !ok {
		return "", &SyntaxError{Msg: "unknown encoding: " + enc}
	}
	text, err := charsetutil.Decode([]byte(src), name)
	if err != nil {
		return "", &SyntaxError{Msg: "source does not decode as " + enc + ": " + err.Error()}
	}
	return text, nil
}

// Error naming the first byte that is not UTF-8 with its line, nil for valid text
func checkUTF8(src string) *SyntaxError {
	if utf8.ValidString(src) {
		return nil
	}
	bad := 0
	for bad < len(src) {
		r, size := utf8.DecodeRuneInString(src[bad:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		bad += size
	}
	lineno := strings.Count(src[:bad], "\n") + 1
	ls := strings.LastIndexByte(src[:bad], '\n') + 1
	le, _ := splitLine(src, ls)
	col := charCount(src[ls:bad]) + 1
	return &SyntaxError{
		Msg:  fmt.Sprintf("Non-UTF-8 code starting with '\\x%02x' on line %d, but no encoding declared; see https://peps.python.org/pep-0263/ for details", src[bad], lineno),
		Line: lineno, Column: col, EndLine: lineno, EndColumn: col,
		Text: strings.ToValidUTF8(src[ls:le], "�"),
	}
}
