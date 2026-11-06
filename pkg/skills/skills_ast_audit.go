package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
