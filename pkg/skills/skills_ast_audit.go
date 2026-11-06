package skills

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// One AST deep scan hit, the (file, line, pattern_id, description) tuple of tools/skills_ast_audit.py
type ASTFinding struct {
	File        string
	Line        int
	PatternID   string
	Description string
}

var astIgnoredDirs = map[string]bool{"__pycache__": true, ".venv": true, "venv": true, "node_modules": true}

// Python recursion limit the visitor runs under, sys.getrecursionlimit() of the interpreter
var astScanRecursionLimit = 1000

// Python frames on the stack of icebath skills audit --deep when _scan_source starts, module frame included
const astScanBaseFrames = 9

// Literal argument index, pattern id and description of each builtin whose computed argument is flagged
var astDynamicCalls = map[string]struct {
	arg       int
	pid, desc string
}{
	"__import__": {0, "dynamic_import_computed", "__import__ with non-literal module name"},
	"getattr":    {1, "dynamic_getattr", "getattr with non-literal attribute name"},
}

func astIsImportlib(name string) bool {
	return name == "importlib" || strings.HasPrefix(name, "importlib.")
}

// Findings of one source text, none on a syntax, value or recursion error, MemoryError propagates as a panic
func astScanSource(content, relPath string) []ASTFinding {
	tree, err := pyast.Parse(content)
	if err != nil {
		var le *pyast.LimitError
		if errors.As(err, &le) && le.Class == "MemoryError" {
			panic(err)
		}
		return nil
	}
	s := &astScanner{rel: relPath}
	s.visit(tree, astScanBaseFrames)
	return s.findings
}

type astScanner struct {
	rel      string
	findings []ASTFinding
}

// Pattern hits of one node in the order the NodeVisitor methods append them
func astNodeHits(n *pyast.Node) [][2]string {
	var hits [][2]string
	switch n.Type {
	case "Call":
		f := n.Child("func")
		if attr, _ := f.Str("attr"); f.Is("Attribute") && attr == "import_module" {
			hits = append(hits, [2]string{"dynamic_import", "importlib.import_module() — loads arbitrary modules at runtime"})
		} else if id, _ := f.Str("id"); f.Is("Name") {
			if dc, ok := astDynamicCalls[id]; ok {
				if args := n.Children("args"); len(args) > dc.arg && !args[dc.arg].Is("Constant") {
					hits = append(hits, [2]string{dc.pid, dc.desc})
				}
			}
		}
	case "Subscript":
		v := n.Child("value")
		if attr, _ := v.Str("attr"); v.Is("Attribute") && attr == "__dict__" && !n.Child("slice").Is("Constant") {
			hits = append(hits, [2]string{"dict_access", "__dict__[<computed>] — dynamic attribute access"})
		}
	case "Import":
		for _, a := range n.Children("names") {
			if name, _ := a.Str("name"); astIsImportlib(name) {
				hits = append(hits, [2]string{"importlib_import", "import " + name + " — enables dynamic module loading"})
			}
		}
	case "ImportFrom":
		if mod, _ := n.Str("module"); astIsImportlib(mod) {
			hits = append(hits, [2]string{"importlib_import", "from " + mod + " import ... — enables dynamic module loading"})
		}
	}
	return hits
}

// Pre-order visit counting visitor, hit and iter_fields frames, false once one passes the recursion limit
func (s *astScanner) visit(n *pyast.Node, frames int) bool {
	frames += 2
	if frames > astScanRecursionLimit {
		return false
	}
	if n.Is("Call", "Subscript", "Import", "ImportFrom") {
		for _, h := range astNodeHits(n) {
			if frames+1 > astScanRecursionLimit {
				return false
			}
			s.findings = append(s.findings, ASTFinding{File: s.rel, Line: n.Lineno, PatternID: h[0], Description: h[1]})
		}
		if frames++; frames > astScanRecursionLimit {
			return false
		}
	}
	if frames+1 > astScanRecursionLimit {
		return false
	}
	for _, c := range pyast.IterChildNodes(n) {
		if !s.visit(c, frames) {
			return false
		}
	}
	return true
}

// Findings of one Python file read as utf-8-sig with replacement, none when it cannot be read
func astScanFile(py, rel string) []ASTFinding {
	data, err := os.ReadFile(py)
	if err != nil {
		return nil
	}
	text := strings.TrimPrefix(pystr.DecodeUTF8Replace(data), "\uFEFF")
	return astScanSource(text, rel)
}

// Findings of one .py file or every .py under a directory outside ignored dirs, none for other paths
func ASTScanPath(path string) []ASTFinding {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		if strings.ToLower(filepath.Ext(path)) == ".py" {
			return astScanFile(path, filepath.Base(path))
		}
		return nil
	}
	var files []string
	for _, f := range rglobFiles(path) {
		if !strings.HasSuffix(filepath.Base(f), ".py") {
			continue
		}
		ignored := false
		for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(f)), "/") {
			ignored = ignored || astIgnoredDirs[part]
		}
		if !ignored {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return comparePathParts(files[i], files[j]) < 0
	})
	var out []ASTFinding
	for _, f := range files {
		rel, _ := filepath.Rel(path, f)
		out = append(out, astScanFile(f, filepath.ToSlash(rel))...)
	}
	return out
}

// Ordering of two paths by their parts, as sorting pathlib paths compares them
func comparePathParts(a, b string) int {
	pa, pb := strings.Split(filepath.ToSlash(a), "/"), strings.Split(filepath.ToSlash(b), "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return len(pa) - len(pb)
}

// Plain text report of findings grouped by file with the diagnostic footer
func FormatASTReport(findings []ASTFinding, skillName string) string {
	header := "AST deep scan"
	if skillName != "" {
		header = "AST deep scan: " + skillName
	}
	if len(findings) == 0 {
		return header + "\n  No dynamic import/access patterns detected."
	}
	sorted := append([]ASTFinding(nil), findings...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.PatternID != b.PatternID {
			return a.PatternID < b.PatternID
		}
		return a.Description < b.Description
	})
	lines := []string{header, "  " + strconv.Itoa(len(findings)) + " finding(s):"}
	current := "\x00"
	for _, f := range sorted {
		if f.File != current {
			current = f.File
			lines = append(lines, "  "+f.File)
		}
		lines = append(lines, "    L"+strconv.Itoa(f.Line)+"  "+f.PatternID+"  — "+f.Description)
	}
	lines = append(lines, "", "  Note: diagnostic hints for human review, not security verdicts.")
	return strings.Join(lines, "\n")
}
