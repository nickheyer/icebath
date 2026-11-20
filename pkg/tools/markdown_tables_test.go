package tools

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

// Display cell index of every pipe in the line, negative widths counted as one cell
func tableColumnOffsets(line string) []int {
	var cells []int
	width := 0
	for _, ch := range line {
		if ch == '|' {
			cells = append(cells, width)
		}
		if w := pyunicode.WcsWidth(string(ch)); w > 0 {
			width += w
		} else {
			width++
		}
	}
	return cells
}

func sameOffsets(rows []string) bool {
	if len(rows) == 0 {
		return true
	}
	first := tableColumnOffsets(rows[0])
	for _, r := range rows[1:] {
		if !reflect.DeepEqual(tableColumnOffsets(r), first) {
			return false
		}
	}
	return true
}

func TestSplitStripsOuterPipesAndTrims(t *testing.T) {
	if got := SplitTableRow("| a | b | c |"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %q", got)
	}
	if got := SplitTableRow("|配置|状态|"); !reflect.DeepEqual(got, []string{"配置", "状态"}) {
		t.Fatalf("got %q", got)
	}
	if got := SplitTableRow("a | b | c"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %q", got)
	}
}

func TestLooksLikeTableRow(t *testing.T) {
	for _, row := range []string{"| a | b |", "a | b | c"} {
		if !LooksLikeTableRow(row) {
			t.Errorf("%q should look like a table row", row)
		}
	}
	for _, row := range []string{"not a table", "a | b", ""} {
		if LooksLikeTableRow(row) {
			t.Errorf("%q should not look like a table row", row)
		}
	}
}

func TestHorizontalKeptWhenTableFits(t *testing.T) {
	src := "| Name | Age |\n|------|-----|\n| Alice | 30 |\n| Bob | 25 |\n"
	width := 100
	out := RealignMarkdownTables(src, &width)
	var bodyRows []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(pystr.Strip(ln), "|") {
			bodyRows = append(bodyRows, ln)
		}
	}
	if len(bodyRows) != 4 {
		t.Fatalf("rows %q", bodyRows)
	}
	if !sameOffsets(bodyRows) {
		t.Fatalf("misaligned:\n%s", out)
	}
}

func TestVerticalFallbackWrapsLongCellTextWithIndent(t *testing.T) {
	src := "| Key | Value |\n|-----|-------|\n| x | this value is long enough that wrapping the value to fit a narrow terminal width is required even in vertical mode |\n"
	width := 60
	lines := strings.Split(RealignMarkdownTables(src, &width), "\n")
	if !strings.HasPrefix(lines[0], "Key: x") {
		t.Fatalf("first line %q", lines[0])
	}
	valueIdx := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "Value:") {
			valueIdx = i
			break
		}
	}
	if valueIdx < 0 || !strings.HasPrefix(lines[valueIdx+1], "  ") {
		t.Fatalf("no indented continuation after Value in %q", lines)
	}
	for _, line := range lines {
		if pyunicode.WcsWidth(line) > 60 {
			t.Fatalf("line over budget %q", line)
		}
	}
}

func TestMultipleTablesInOneText(t *testing.T) {
	src := "First:\n\n| 配置 | 值 |\n|------|----|\n| 通义 | 1 |\n\nSecond:\n\n| model | n |\n|-------|---|\n| gpt   | 2 |\n"
	out := RealignMarkdownTables(src, nil)
	var blocks [][]string
	var current []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "|") {
			current = append(current, line)
		} else if len(current) > 0 {
			blocks = append(blocks, current)
			current = nil
		}
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks %d in\n%s", len(blocks), out)
	}
	for _, block := range blocks {
		if !sameOffsets(block) {
			t.Fatalf("block did not align:\n%s", strings.Join(block, "\n"))
		}
	}
}
