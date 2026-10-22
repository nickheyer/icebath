package render

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// Every glyph the interface draws takes one cell, so terminals on older width tables lay rows out the same
func TestInterfaceGlyphsTakeOneCell(t *testing.T) {
	for _, root := range []string{"..", "../../app"} {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || (lit.Kind != token.STRING && lit.Kind != token.CHAR) {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				for _, r := range text {
					if r < 0x80 || unicode.Is(unicode.Mn, r) {
						continue
					}
					if w := ansi.StringWidth(string(r)); w != 1 {
						t.Errorf("%s: %q U+%04X takes %d cells", fset.Position(lit.Pos()), r, r, w)
					}
				}
				return true
			})
			return nil
		})
	}
}
