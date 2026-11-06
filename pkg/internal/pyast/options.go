package pyast

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Options holds the ast.parse arguments filename, mode, type_comments, feature_version and optimize and the warnings error filter
type Options struct {
	Filename       string
	Mode           string
	TypeComments   bool
	FeatureVersion *int
	// FeatureMajor is the major of a (major, minor) feature_version tuple whose minor is FeatureVersion
	FeatureMajor     *int
	Optimize         int
	WarningsAsErrors bool
	encoding         string
}

// Warning is a SyntaxWarning or DeprecationWarning ast.parse passes to warnings.warn_explicit
type Warning struct {
	Category string
	Msg      string
	Filename string
	Lineno   int
}

const latestMinor = 14

// ParseWith parses source text like ast.parse with options, returning the tree and the warnings it emitted
func ParseWith(source string, opts Options) (*Node, []Warning, error) {
	if err := checkCompileArgs(opts); err != nil {
		return nil, nil, err
	}
	if err := pystr.StrictUTF8EncodeError(source); err != nil {
		return nil, nil, err
	}
	if strings.IndexByte(source, 0) >= 0 {
		return nil, nil, nullBytesError()
	}
	opts.encoding = "utf-8"
	return parseOpts(translateLines([]byte(source), execMode(opts.Mode)), opts)
}

// ParseBytesWith parses encoded source like ast.parse(bytes) with options, honouring a BOM and a coding declaration
func ParseBytesWith(source []byte, opts Options) (*Node, []Warning, error) {
	if err := checkCompileArgs(opts); err != nil {
		return nil, nil, err
	}
	if bytes.IndexByte(source, 0) >= 0 {
		return nil, nil, nullBytesError()
	}
	text, err := decodeSource(translateLines(source, execMode(opts.Mode)))
	if err != nil {
		return nil, nil, withFilename(err, opts)
	}
	opts.encoding = declaredEncoding(source)
	return parseOpts(text, opts)
}

// ParseNode compiles an AST object like ast.parse(tree), returning a converted, validated and preprocessed copy
func ParseNode(n *Node, opts Options) (*Node, error) {
	if err := checkCompileArgs(opts); err != nil {
		return nil, err
	}
	if n == nil {
		return nil, pyErr("TypeError", "compile() arg 1 must be a string, bytes or AST object")
	}
	mode := opts.Mode
	if mode == "" {
		mode = "exec"
	}
	mod, err := obj2mod(n, mode)
	if err != nil {
		return nil, err
	}
	if err := validateMod(mod); err != nil {
		return nil, err
	}
	if err := compileAST(mod, opts, cpythonStack, len(Walk(mod))); err != nil {
		return nil, withFilename(err, opts)
	}
	return mod, nil
}

func execMode(mode string) bool { return mode == "" || mode == "exec" }

// Rejects a feature_version tuple major other than 3, an optimize level or a mode compile() does not know
func checkCompileArgs(o Options) error {
	if o.FeatureMajor != nil && *o.FeatureMajor != 3 {
		return &LimitError{Class: "ValueError", Msg: fmt.Sprintf("Unsupported major version: %d", *o.FeatureMajor)}
	}
	if o.Optimize < -1 || o.Optimize > 2 {
		return &LimitError{Class: "ValueError", Msg: "compile(): invalid optimize value"}
	}
	switch o.Mode {
	case "", "exec", "eval", "single", "func_type":
		return nil
	}
	return &LimitError{Class: "ValueError", Msg: "compile() mode must be 'exec', 'eval', 'single' or 'func_type'"}
}

func (o Options) filename() string {
	if o.Filename == "" {
		return "<unknown>"
	}
	return o.Filename
}

func withFilename(err error, o Options) error {
	if se, ok := err.(*SyntaxError); ok && se.Filename == "" {
		se.Filename = o.filename()
	}
	return err
}

// Parses translated UTF-8 source with the start rule of the mode and collects the warnings CPython would emit
func parseOpts(buf []byte, o Options) (*Node, []Warning, error) {
	fv := latestMinor
	if o.FeatureVersion != nil && *o.FeatureVersion >= 0 {
		fv = *o.FeatureVersion
	}
	p, n, err := parseLexed(buf, o, fv)
	if err == nil {
		if err = compileAST(n, o, cpythonStack, len(p.toks)); err != nil {
			n = nil
		}
	}
	if err != nil {
		err = withFilename(err, o)
	}
	if o.WarningsAsErrors {
		return n, nil, err
	}
	return n, p.warns, err
}

// Lexes and parses, lexing again with <> from the fill where CPython's pass one ran the barry_as_FLUFL import when that differs
func parseLexed(buf []byte, o Options, fv int) (p *parser, n *Node, err error) {
	var tried []int
	for at := -1; ; {
		p = &parser{lx: tokenizeBarry(buf, o.TypeComments, o.WarningsAsErrors, at), parens: map[*Node]bool{}, filename: o.filename(), fv: fv, warnErrors: o.WarningsAsErrors, mode: o.Mode, barryTok: -1, recBarryTok: -1}
		n, err = p.parseMode(o)
		truth := p.barryTok
		if p.barryChecked {
			truth = p.recBarryTok
		}
		next := truth
		if next < 0 {
			next = -2
		}
		tried = append(tried, at)
		if !barryLexDiffers(p.lx.toks, p.lx.barryTok, truth) || slices.Contains(tried, next) {
			return p, n, err
		}
		at = next
	}
}

// Runs the start rule of the mode over the lexer's tokens and the CPython stack overflow check
func (p *parser) parseMode(o Options) (*Node, error) {
	p.toks = p.lx.toks
	if execMode(o.Mode) && o.Filename != "" {
		p.sourceFile, p.encoding = o.Filename, o.encoding
	}
	start := p.file
	switch o.Mode {
	case "eval":
		start = p.evalInput
	case "single":
		p.toks = singleInputTokens(p.lx, len(p.lx.src))
		start = p.interactive
	case "func_type":
		start = p.funcTypeInput
	}
	return p.withCPythonDepth(p.run(start))
}

// Turns ENDMARKER after any token into NEWLINE plus the implied dedents as _PyPegen_fill_token does for single input
func singleInputTokens(lx *lexer, size int) []token {
	toks := lx.toks
	if len(toks) < 2 || toks[len(toks)-1].kind != tEnd {
		return toks
	}
	end := toks[len(toks)-1]
	end.cursor = size
	nl := end
	nl.kind = tNewline
	out := append(append([]token{}, toks[:len(toks)-1]...), nl)
	if indent := len(lx.indstack) - 1; indent > 0 {
		for i := 0; i < indent; i++ {
			d := end
			d.kind, d.c, d.ec = tDedent, -1, -1
			out = append(out, d)
		}
		out = append(out, nl)
	}
	return append(out, end)
}

// Rule eval of python.gram, expressions followed by newlines and the end marker
func (p *parser) evalInput() *Node {
	body := p.expressions()
	for p.cur().kind == tNewline {
		p.next()
	}
	if p.cur().kind != tEnd {
		p.fail()
	}
	return NewNode("Expression", body)
}

// Rule interactive with statement_newline, then the multiple statements check of bad_single_statement
func (p *parser) interactive() *Node {
	t := p.cur()
	var body []*Node
	switch t.kind {
	case tNewline:
		p.next()
		pass := NewNode("Pass")
		pass.Lineno, pass.ColOffset, pass.EndLineno, pass.EndColOffset = t.l, t.c, t.el, t.ec
		body = []*Node{pass}
	case tEnd:
		p.fail()
	default:
		body = p.statement()
		if len(body) == 1 && body[0].Is("FunctionDef", "AsyncFunctionDef", "ClassDef", "If", "For", "AsyncFor", "While", "With", "AsyncWith", "Try", "TryStar", "Match") {
			if p.cur().kind != tNewline {
				p.fail()
			}
			p.next()
		}
	}
	rest := p.lx.src[min(p.tk(p.fill-1).cursor, len(p.lx.src)):]
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\014':
		case c == '#':
			for i < len(rest) && rest[i] != '\n' {
				i++
			}
		default:
			p.noFullTokenize = true
			p.raiseLast("multiple statements found while compiling a single statement")
		}
	}
	return NewNode("Interactive", body)
}

// Rule func_type, a parenthesized type_expressions list, an arrow and the return expression
func (p *parser) funcTypeInput() *Node {
	p.expectOp("(")
	var argtypes []*Node
	if !p.isOp(")") {
		argtypes = p.typeExpressions()
	}
	p.expectOp(")")
	p.expectOp("->")
	returns := p.expression()
	for p.cur().kind == tNewline {
		p.next()
	}
	if p.cur().kind != tEnd {
		p.fail()
	}
	return NewNode("FunctionType", argtypes, returns)
}

// Rule type_expressions, expressions then an optional starred and an optional double starred expression
func (p *parser) typeExpressions() []*Node {
	var out []*Node
	starNext := func() bool {
		n := p.tk(p.pos + 1)
		return n.kind == tOp && (n.val == "*" || n.val == "**")
	}
	if !p.isOp("*", "**") {
		out = append(out, p.expression())
		for p.isOp(",") && !starNext() {
			save := p.pos
			p.next()
			e, ok := p.speculate(p.expression)
			if !ok {
				p.pos = save
				return out
			}
			out = append(out, e)
		}
		if !p.isOp(",") {
			return out
		}
		p.next()
	}
	if p.isOp("*") {
		p.next()
		out = append(out, p.expression())
		if !p.isOp(",") {
			return out
		}
		if n := p.tk(p.pos + 1); n.kind != tOp || n.val != "**" {
			return out
		}
		p.next()
	}
	p.expectOp("**")
	return append(out, p.expression())
}

// Error after the parser stopped, letting a later tokenizer error or an unclosed bracket replace it as CPython does
func (p *parser) fullTokenize(e error) error {
	lx := p.lx
	if lx.err != nil {
		if lx.errInFString {
			return e
		}
		return lx.err
	}
	if end := p.toks[len(p.toks)-1]; end.kind == tError && len(lx.parenLine) > 0 {
		line := p.toks[max(min(p.fill, len(p.toks)), 1)-1].l
		if p.knownErrLine > 0 {
			line = p.knownErrLine
		}
		if line > lx.parenLine[len(lx.parenLine)-1] {
			return p.genericErrorOf(p.raiseUnclosed)
		}
	}
	return e
}

// Runs a raising function and returns the SyntaxError it raised
func (p *parser) genericErrorOf(f func()) (err error) {
	defer func() {
		r := recover()
		if e, ok := r.(*SyntaxError); ok {
			err = e
			return
		}
		panic(r)
	}()
	f()
	return nil
}

// Coding declared by a UTF-8 BOM or a PEP 263 cookie on line one, or on line two after a comment line
func declaredEncoding(src []byte) string {
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		return "utf-8"
	}
	start := 0
	for line := 0; line < 2 && start < len(src); line++ {
		end := bytes.IndexByte(src[start:], '\n')
		if end < 0 {
			end = len(src)
		} else {
			end += start + 1
		}
		if spec, ok := codingSpec(src[start:end]); ok {
			return spec
		}
		if !onlyCommentLine(src[start:end]) {
			break
		}
		start = end
	}
	return ""
}

// Decodes bytes with a codec and replacement characters as PyUnicode_Decode with errors replace
func decodeReplace(spec string, b []byte) (string, bool) {
	name := codecName(spec)
	if cm, ok := singleByte[name]; ok {
		out, _ := cm.NewDecoder().Bytes(b)
		return string(out), true
	}
	if enc, ok := multiByte[name]; ok {
		out, _ := enc.NewDecoder().Bytes(b)
		return string(out), true
	}
	switch name {
	case "utf_8", "utf_8_sig", "utf8":
		return pystr.DecodeUTF8Replace(b), true
	case "ascii":
		var sb strings.Builder
		for _, c := range b {
			if c >= 0x80 {
				sb.WriteRune(utf8.RuneError)
			} else {
				sb.WriteByte(c)
			}
		}
		return sb.String(), true
	}
	return "", false
}

// Line lineno of the named file as _PyErr_ProgramDecodedTextObject reads it, in chunks of 999 bytes like fgets
func programText(filename string, lineno int, encoding string) (string, bool) {
	data, err := os.ReadFile(filename)
	if err != nil || lineno <= 0 {
		return "", false
	}
	data = translateLines(data, false)
	if encoding == "" {
		encoding = declaredEncoding(data)
	}
	if encoding == "" {
		encoding = "utf-8"
	}
	pos := 0
	var line []byte
	for i := 0; i < lineno; {
		if pos >= len(data) {
			return "", false
		}
		end := pos
		for end < len(data) && end-pos < 999 {
			end++
			if data[end-1] == '\n' {
				break
			}
		}
		line, pos = data[pos:end], end
		if i+1 < lineno && len(line) == 999 && line[998] != '\n' {
			continue
		}
		i++
	}
	if lineno == 1 && len(line) >= 3 {
		line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
	}
	text, ok := decodeReplace(encoding, line)
	return text, ok
}
