package pyast

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// compileFoldShift is how much deeper compile() enters the preprocess pass than ast.parse does
const compileFoldShift = 32

// CompileStage runs the stages of compile() that follow the parser over a parsed module and returns the first error raised
func CompileStage(tree *Node, filename string) error {
	return compileStages(tree, filename, nil)
}

// Compile compiles source bytes like compile(source, filename, "exec") and returns the error it raises
func Compile(source []byte, filename string) error {
	if bytes.IndexByte(source, 0) >= 0 {
		return nullBytesError()
	}
	o := Options{Filename: filename}
	text, err := decodeSource(translateLines(source, true))
	if err != nil {
		return withFilename(err, o)
	}
	o.encoding = declaredEncoding(source)
	return compileBuffer(text, o)
}

// CompileString compiles source text like compile(source, filename, "exec") and returns the error it raises
func CompileString(source, filename string) error {
	if err := pystr.StrictUTF8EncodeError(source); err != nil {
		return err
	}
	if strings.IndexByte(source, 0) >= 0 {
		return nullBytesError()
	}
	return compileBuffer(translateLines([]byte(source), true), Options{Filename: filename, encoding: "utf-8"})
}

func compileBuffer(buf []byte, o Options) error {
	_, n, err := parseLexed(buf, o, latestMinor)
	if err != nil {
		return withFilename(err, o)
	}
	return compileStages(n, o.filename(), nil)
}

// Runs the future statement pass, the preprocess pass, the symbol table passes and the code generator in the order of _PyAST_Compile
func compileStages(tree *Node, filename string, seen func(tag byte, d int)) error {
	if tree == nil {
		return pyErr("TypeError", "compile() arg 1 must be a string, bytes or AST object")
	}
	if !tree.Is("Module") {
		return pyErr("TypeError", "expected Module node, got %s", tree.Type)
	}
	future, err := futureParse(tree, filename)
	if err != nil {
		return err
	}
	mod := copyTree(tree)
	f := &folder{annotations: future.annotations, limit: cpythonStack.foldLimit - compileFoldShift}
	if !recursesWithin(func() { f.mod(mod) }) {
		return &LimitError{Class: "RecursionError", Msg: fmt.Sprintf("Stack overflow (used %d kB) during compilation", cpythonStack.kB)}
	}
	st, err := buildSymtable(mod, filename, future, seen)
	if err != nil {
		return err
	}
	return runCodegen(mod, st, filename, future, seen)
}

// Copies a tree so the preprocess pass of compile() leaves the tree of the caller as parsed
func copyTree(n *Node) *Node {
	if n == nil {
		return nil
	}
	out := *n
	out.vals = make([]any, len(n.vals))
	for i, v := range n.vals {
		switch x := v.(type) {
		case *Node:
			out.vals[i] = copyTree(x)
		case []*Node:
			list := make([]*Node, len(x))
			for j, c := range x {
				list[j] = copyTree(c)
			}
			out.vals[i] = list
		case []string:
			out.vals[i] = append([]string{}, x...)
		default:
			out.vals[i] = v
		}
	}
	return &out
}
