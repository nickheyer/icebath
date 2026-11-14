package config

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	configWriterPathRe   = regexp.MustCompile(`[cC]onfigPath|cfgPath|config\.yaml|\blivePath\b`)
	configWriterModuleRe = regexp.MustCompile(`^pkg/config/(config|facade|load)[a-z_]*\.go$`)
	configWriterTrees    = []string{"pkg", "cmd"}
	configWriterAllowed  = map[string]bool{"pkg/config/safe_dump.go": true}
)

const configWriterSuppress = "// config-writer ok"

// Name of the called function, the selector or the identifier
func configWriterFuncName(e ast.Expr) string {
	switch f := e.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return ""
}

// Index of the argument naming the written path or dumped data, after the context of AtomicYAMLWrite
func configWriterTargetArg(name string) int {
	if name == "AtomicYAMLWrite" {
		return 1
	}
	return 0
}

// True for an AtomicYAMLWrite, SafeDump family or yaml receiver Marshal and Encode call
func configWriterIsYAMLDump(call *ast.CallExpr, src []byte, fset *token.FileSet) bool {
	name := configWriterFuncName(call.Fun)
	switch {
	case name == "AtomicYAMLWrite" || strings.HasPrefix(name, "SafeDump"):
		return true
	case name == "Marshal" || name == "Encode" || name == "NewEncoder":
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && strings.Contains(strings.ToLower(configWriterSource(src, fset, sel.X)), "yaml")
	}
	return false
}

// Source text of a node
func configWriterSource(src []byte, fset *token.FileSet, n ast.Node) string {
	return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
}

// Problems of one Go file, a YAML dump of a config path or any YAML dump inside the config package
func scanConfigYamlWriters(root, path string) []string {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(src), "\n")
	relPath, _ := filepath.Rel(root, path)
	rel := filepath.ToSlash(relPath)
	inConfigModule := configWriterModuleRe.MatchString(rel)
	var problems []string
	flag := func(call *ast.CallExpr, why string) {
		line := fset.Position(call.Pos()).Line
		if strings.Contains(lines[line-1], configWriterSuppress) {
			return
		}
		problems = append(problems, fmt.Sprintf("%s:%d: %s — route it through the config writer seam", rel, line, why))
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := configWriterFuncName(call.Fun)
		if configWriterIsYAMLDump(call, src, fset) {
			if inConfigModule {
				flag(call, "YAML dump inside the config system")
				return true
			}
			if i := configWriterTargetArg(name); i < len(call.Args) {
				first := configWriterSource(src, fset, call.Args[i])
				if configWriterPathRe.MatchString(first) {
					flag(call, fmt.Sprintf("YAML dump of a config path (%s)", first))
				}
			}
			return true
		}
		if name == "WriteFile" && len(call.Args) > 0 {
			target := configWriterSource(src, fset, call.Args[0])
			body := configWriterSource(src, fset, call)
			if configWriterPathRe.MatchString(target) && (strings.Contains(body, "Dump") || strings.Contains(body, "Marshal(")) {
				flag(call, fmt.Sprintf("WriteFile of a YAML dump onto a config path (%s)", target))
			}
		}
		return true
	})
	return problems
}

func TestGuardFlagsPyyamlDumpOfConfigPath(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "pkg", "cli", "bad_writer.go")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	text := "package cli\n\nfunc a(ctx context.Context, configPath string, cfg map[string]any) {\n" +
		"\tconfig.AtomicYAMLWrite(ctx, configPath, cfg, \"\")\n" +
		"}\nfunc b(cfgPath string, cfg map[string]any) { os.WriteFile(cfgPath, must(config.SafeDump(cfg)), 0o644) }\n" +
		"func c(ctx context.Context, otherPath string, cfg map[string]any) {\n" +
		"\tconfig.AtomicYAMLWrite(ctx, otherPath, cfg, \"\")\n}\n"
	if err := os.WriteFile(bad, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	problems := scanConfigYamlWriters(root, bad)
	var got []string
	for _, p := range problems {
		got = append(got, strings.Split(p, ":")[1])
	}
	if !slices.Equal(got, []string{"4", "6"}) {
		t.Fatalf("problems = %q", problems)
	}
}

func TestConfigYamlWritersGoThroughTheWriterSeam(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, top := range configWriterTrees {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var problems []string
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		if configWriterAllowed[filepath.ToSlash(rel)] {
			continue
		}
		problems = append(problems, scanConfigYamlWriters(root, f)...)
	}
	if len(problems) > 0 {
		t.Fatalf("config.yaml must only be written through the comment-preserving writer:\n  %s", strings.Join(problems, "\n  "))
	}
}
