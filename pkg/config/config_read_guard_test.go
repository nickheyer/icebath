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

// Go files of the owner modules allowed to parse config.yaml raw, mirroring the hermes allowlist
var rawConfigReadAllowlist = map[string]bool{
	"pkg/config/facade_load.go":    true,
	"pkg/config/load.go":           true,
	"pkg/config/managed.go":        true,
	"pkg/gateway/config_types.go":  true,
	"pkg/gateway/config_loader.go": true,
	"pkg/gateway/readiness.go":     true,
}

var (
	rawYAMLParseRe = regexp.MustCompile(`\b(SafeLoad|yaml\.Unmarshal|yaml\.NewDecoder)\s*\(`)
	configYAMLRe   = regexp.MustCompile(`"config\.yaml"|\bConfigPath\(`)
)

func TestNoRawConfigYamlReadsOutsideOwnerModules(t *testing.T) {
	const proximity = 6
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root %s: %v", root, err)
	}
	var offenders []string
	for _, top := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if rawConfigReadAllowlist[rel] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			lines := strings.Split(string(data), "\n")
			var cfgLines []int
			for i, ln := range lines {
				if configYAMLRe.MatchString(ln) {
					cfgLines = append(cfgLines, i)
				}
			}
			if len(cfgLines) == 0 {
				return nil
			}
			for i, ln := range lines {
				stripped := strings.TrimSpace(ln)
				if !rawYAMLParseRe.MatchString(ln) || strings.HasPrefix(stripped, "//") {
					continue
				}
				if slices.ContainsFunc(cfgLines, func(j int) bool { return max(i-j, j-i) <= proximity }) {
					offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, i+1, stripped))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("Raw yaml.safe_load of config.yaml outside allowlisted owner modules.\nBehavioral reads must use hermes_cli.config.load_config()/"+
			"load_config_readonly() (or gateway _load_gateway_config); write-back round-trips and raw-file diagnostics must use "+
			"hermes_cli.config.read_user_config_raw().\nOffenders:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func TestReadUserConfigRawExistsAndDocumented(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "facade_load.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var doc string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "ReadUserConfigRaw" {
			doc = fn.Doc.Text()
		}
	}
	if !strings.Contains(doc, "ONLY legal for write-back round-trips and raw-file diagnostics") || !strings.Contains(doc, "load_config()") {
		t.Fatalf("doc %q", doc)
	}
}
