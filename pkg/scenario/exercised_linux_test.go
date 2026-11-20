package scenario

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// One coverage block of a source file with its statement count and whether any process ran it
type coverBlock struct {
	line, col int
	stmts     int
	hit       bool
}

// Source extent of one top level function or function valued variable
type funcSpan struct {
	name                               string
	startLine, startCol, endLine, endC int
}

// Merges the coverage of every scenario process into one row per Go function and replaces the table in one rename
func writeExercised(root, coverDir, work, out string, built time.Time) error {
	profile := filepath.Join(work, "profile.txt")
	text := exec.Command("go", "tool", "covdata", "textfmt", "-i="+coverDir, "-o="+profile)
	text.Dir = root
	if msg, err := text.CombinedOutput(); err != nil {
		return fmt.Errorf("go tool covdata textfmt: %v: %s", err, bytes.TrimSpace(msg))
	}
	blocks, err := readProfile(profile)
	if err != nil {
		return err
	}
	files := make([]string, 0, len(blocks))
	for file := range blocks {
		files = append(files, file)
	}
	sort.Strings(files)
	if changed := changedSince(root, files, built); len(changed) > 0 {
		return fmt.Errorf("%d source files changed after the binary was built (first %s), the table is left as it was, run the suite again on a tree nothing writes to",
			len(changed), changed[0])
	}
	var table bytes.Buffer
	table.WriteString("file\tfunction\tcovered\ttotal\n")
	for _, file := range files {
		spans, err := funcSpans(filepath.Join(root, file))
		if err != nil {
			return err
		}
		covered, total := map[string]int{}, map[string]int{}
		for _, b := range blocks[file] {
			name := enclosing(spans, b)
			if name == "" {
				return fmt.Errorf("%s:%d.%d: coverage block outside every top level declaration", file, b.line, b.col)
			}
			total[name] += b.stmts
			if b.hit {
				covered[name] += b.stmts
			}
		}
		names := make([]string, 0, len(total))
		for name := range total {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&table, "%s\t%s\t%d\t%d\n", file, name, covered[name], total[name])
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".exercised-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(table.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), out)
}

// Lists the source files whose inode changed after the build, their blocks no longer matching their text
func changedSince(root string, files []string, built time.Time) []string {
	var changed []string
	for _, file := range files {
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(root, file), &st); err != nil || time.Unix(st.Ctim.Sec, st.Ctim.Nsec).After(built) {
			changed = append(changed, file)
		}
	}
	return changed
}

// Reads a text coverage profile into blocks keyed by the file path relative to the module root
func readProfile(path string) (map[string][]coverBlock, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	type key struct {
		file      string
		line, col int
	}
	index := map[key]int{}
	blocks := map[string][]coverBlock{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 1<<20), 1<<20)
	for scan.Scan() {
		line := scan.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		colon := strings.LastIndexByte(line, ':')
		fields := strings.Fields(line[colon+1:])
		if colon < 0 || len(fields) != 3 {
			return nil, fmt.Errorf("coverage profile line not understood: %q", line)
		}
		file := strings.TrimPrefix(line[:colon], modulePath+"/")
		start, _, _ := strings.Cut(fields[0], ",")
		ls, cs, _ := strings.Cut(start, ".")
		bl, err1 := strconv.Atoi(ls)
		bc, err2 := strconv.Atoi(cs)
		stmts, err3 := strconv.Atoi(fields[1])
		count, err4 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			return nil, fmt.Errorf("coverage profile line not understood: %q", line)
		}
		k := key{file, bl, bc}
		if i, seen := index[k]; seen {
			blocks[file][i].hit = blocks[file][i].hit || count > 0
			continue
		}
		index[k] = len(blocks[file])
		blocks[file] = append(blocks[file], coverBlock{line: bl, col: bc, stmts: stmts, hit: count > 0})
	}
	return blocks, scan.Err()
}

// Parses one source file into the extents of its functions, methods named Type.Method, and initialized variables
func funcSpans(path string) ([]funcSpan, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var spans []funcSpan
	add := func(name string, from, to token.Pos) {
		a, b := fset.Position(from), fset.Position(to)
		spans = append(spans, funcSpan{name, a.Line, a.Column, b.Line, b.Column})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				name = receiverName(d.Recv.List[0].Type) + "." + name
			}
			add(name, d.Pos(), d.End())
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if v, ok := spec.(*ast.ValueSpec); ok && len(v.Values) > 0 {
					add(v.Names[0].Name, v.Pos(), v.End())
				}
			}
		}
	}
	return spans, nil
}

// Returns the type name of a method receiver without pointer or type parameters
func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.ParenExpr:
		return receiverName(e.X)
	case *ast.IndexExpr:
		return receiverName(e.X)
	case *ast.IndexListExpr:
		return receiverName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// Names the declaration whose extent contains the start of the block
func enclosing(spans []funcSpan, b coverBlock) string {
	for _, s := range spans {
		after := b.line > s.startLine || (b.line == s.startLine && b.col >= s.startCol)
		before := b.line < s.endLine || (b.line == s.endLine && b.col <= s.endC)
		if after && before {
			return s.name
		}
	}
	return ""
}
