//go:build !icebath_core && !icebath_noextras

package pm

import (
	"bytes"
	"testing"
)

func TestExportedArgparseTheme(t *testing.T) {
	t.Setenv("PYTHON_COLORS", "0")
	if colors := ArgparseThemeFor(&bytes.Buffer{}); colors != (ArgparseColors{}) {
		t.Fatalf("uncolored theme %+v", colors)
	}
	t.Setenv("PYTHON_COLORS", "1")
	if colors := ArgparseThemeFor(&bytes.Buffer{}); colors.Usage != "\x1b[1;34m" || colors.Reset != "\x1b[0m" {
		t.Fatalf("colored theme %+v", colors)
	}
	t.Setenv("COLUMNS", "100")
	if width := ArgparseWidth(); width != 98 {
		t.Fatalf("width %d", width)
	}
}
