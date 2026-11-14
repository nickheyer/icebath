package docconv

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html/charset"
	xunicode "golang.org/x/text/encoding/unicode"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const (
	nsW      = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsS      = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsRel    = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsPkgRel = "http://schemas.openxmlformats.org/package/2006/relationships"

	maxSheetRows = 5000
	maxSheetCols = 256
)

type element struct {
	name     xml.Name
	attrs    []xml.Attr
	text     string
	children []*element
}

func (e *element) is(space, local string) bool {
	return e.name.Space == space && e.name.Local == local
}

func (e *element) attr(space, local string) (string, bool) {
	for _, a := range e.attrs {
		if a.Name.Space == space && a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

func (e *element) child(space, local string) *element {
	for _, c := range e.children {
		if c.is(space, local) {
			return c
		}
	}
	return nil
}

func (e *element) childText(space, local string) string {
	if c := e.child(space, local); c != nil {
		return c.text
	}
	return ""
}

func (e *element) iter(fn func(*element)) {
	fn(e)
	for _, c := range e.children {
		c.iter(fn)
	}
}

func parseXML(data []byte) (*element, error) {
	predecoded := false
	switch {
	case bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}):
		data = data[3:]
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}), bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		decoded, err := xunicode.UTF16(xunicode.BigEndian, xunicode.ExpectBOM).NewDecoder().Bytes(data)
		if err != nil {
			return nil, err
		}
		data, predecoded = decoded, true
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = func(label string, r io.Reader) (io.Reader, error) {
		if predecoded {
			return r, nil
		}
		return charset.NewReaderLabel(label, r)
	}
	var root *element
	var stack []*element
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			el := &element{name: t.Name, attrs: t.Copy().Attr}
			switch {
			case len(stack) > 0:
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, el)
			case root == nil:
				root = el
			default:
				return nil, errors.New("junk after document element")
			}
			stack = append(stack, el)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				if top := stack[len(stack)-1]; len(top.children) == 0 {
					top.text += string(t)
				}
			} else if root != nil && strings.Trim(string(t), " \t\r\n") != "" {
				return nil, errors.New("junk after document element")
			}
		}
	}
	if root == nil {
		return nil, errors.New("no element found")
	}
	return root, nil
}

func badZip(kind string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return &ExtractError{Msg: err.Error(), Err: err}
	}
	return &ExtractError{Msg: "Not a valid " + kind + ": " + err.Error(), Err: err}
}

type zipPackage struct {
	zr   *fileutil.ZipReader
	kind string
}

func openZipPath(p, kind string) (*zipPackage, error) {
	zr, err := fileutil.OpenZip(p)
	if err != nil {
		return nil, badZip(kind, err)
	}
	return &zipPackage{zr: zr, kind: kind}, nil
}

func openZipBytes(data []byte, kind string) (*zipPackage, error) {
	zr, err := fileutil.NewZipReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, badZip(kind, err)
	}
	return &zipPackage{zr: zr, kind: kind}, nil
}

func (z *zipPackage) Close() error {
	return z.zr.Close()
}

func (z *zipPackage) member(name string) ([]byte, bool, error) {
	f, ok := z.zr.Lookup(name)
	if !ok {
		return nil, false, nil
	}
	rc, err := z.zr.Open(f)
	if err != nil {
		return nil, true, badZip(z.kind, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, true, badZip(z.kind, err)
	}
	return data, true, nil
}

func (z *zipPackage) xmlPart(name string, optional bool) (*element, error) {
	data, ok, err := z.member(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		if optional {
			return &element{name: xml.Name{Local: "missing"}}, nil
		}
		return nil, extractErr("Missing %s", name)
	}
	root, err := parseXML(data)
	if err != nil {
		if optional {
			return &element{name: xml.Name{Local: "missing"}}, nil
		}
		return nil, &ExtractError{Msg: "Malformed XML in " + name + ": " + err.Error(), Err: err}
	}
	return root, nil
}

func joined(lines []string, emptyError string) (string, error) {
	for _, line := range lines {
		if strip(line) != "" {
			return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n", nil
		}
	}
	return "", extractErr("%s", emptyError)
}

func extractDocx(z *zipPackage) (string, error) {
	root, err := z.xmlPart("word/document.xml", false)
	if err != nil {
		return "", err
	}
	var lines []string
	root.iter(func(para *element) {
		if !para.is(nsW, "p") {
			return
		}
		guide := map[*element]bool{}
		para.iter(func(n *element) {
			if n.is(nsW, "rt") {
				n.iter(func(g *element) { guide[g] = true })
			}
		})
		var b strings.Builder
		para.iter(func(n *element) {
			if guide[n] || n.name.Space != nsW {
				return
			}
			switch n.name.Local {
			case "t":
				b.WriteString(n.text)
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		})
		lines = append(lines, strings.Split(b.String(), "\n")...)
	})
	return joined(lines, "DOCX contains no extractable text")
}

func stringItemText(item *element) string {
	var b strings.Builder
	for _, c := range item.children {
		switch {
		case c.is(nsS, "t"):
			b.WriteString(c.text)
		case c.is(nsS, "r"):
			b.WriteString(c.childText(nsS, "t"))
		}
	}
	return b.String()
}

func extractXlsx(z *zipPackage) (string, error) {
	sst, err := z.xmlPart("xl/sharedStrings.xml", true)
	if err != nil {
		return "", err
	}
	var shared []string
	sst.iter(func(item *element) {
		if item.is(nsS, "si") {
			shared = append(shared, stringItemText(item))
		}
	})
	relsRoot, err := z.xmlPart("xl/_rels/workbook.xml.rels", true)
	if err != nil {
		return "", err
	}
	rels := map[string]string{}
	relsRoot.iter(func(rel *element) {
		if !rel.is(nsPkgRel, "Relationship") {
			return
		}
		if id, _ := rel.attr("", "Id"); id != "" {
			target, _ := rel.attr("", "Target")
			rels[id] = target
		}
	})
	workbook, err := z.xmlPart("xl/workbook.xml", false)
	if err != nil {
		return "", err
	}
	var out []string
	var failure error
	workbook.iter(func(sheet *element) {
		if failure != nil || !sheet.is(nsS, "sheet") {
			return
		}
		rid, _ := sheet.attr(nsRel, "id")
		target := strings.TrimLeft(rels[rid], "/")
		part := target
		if !strings.HasPrefix(target, "xl/") {
			part = "xl/" + target
		}
		part = path.Clean(part)
		state, ok := sheet.attr("", "state")
		if !ok {
			state = "visible"
		}
		if state == "hidden" || state == "veryHidden" {
			return
		}
		data, found, err := z.member(part)
		if err != nil {
			failure = err
			return
		}
		if !found {
			return
		}
		rows, err := sheetRows(data, shared)
		if err != nil {
			return
		}
		name, ok := sheet.attr("", "name")
		if !ok {
			name = "Sheet"
		}
		out = append(out, "# ── Sheet: "+name+" ──")
		if len(rows) == 0 {
			out = append(out, "(empty)")
		}
		for _, row := range rows {
			out = append(out, strings.Join(row, "\t"))
		}
		out = append(out, "")
	})
	if failure != nil {
		return "", failure
	}
	return joined(out, "XLSX has no visible sheets with content")
}

func colIndex(ref string) int {
	const saturated = 1 << 40
	idx := 0
	for _, ch := range ref {
		if !unicode.IsLetter(ch) {
			break
		}
		if idx < saturated {
			idx = idx*26 + int(unicode.ToUpper(ch)) - 'A' + 1
		}
	}
	return max(idx-1, 0)
}

func sheetRows(data []byte, shared []string) ([][]string, error) {
	root, err := parseXML(data)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	root.iter(func(row *element) {
		if len(rows) >= maxSheetRows || !row.is(nsS, "row") {
			return
		}
		cells := map[int]string{}
		maxCol := -1
		row.iter(func(cell *element) {
			if !cell.is(nsS, "c") {
				return
			}
			col := maxCol + 1
			if ref, _ := cell.attr("", "r"); ref != "" {
				col = colIndex(ref)
			}
			if col < maxSheetCols {
				cells[col] = cellValue(cell, shared)
				maxCol = max(maxCol, col)
			}
		})
		values := make([]string, maxCol+1)
		for i := range values {
			values[i] = cells[i]
		}
		rows = append(rows, values)
	})
	for len(rows) > 0 {
		blank := true
		for _, v := range rows[len(rows)-1] {
			if strip(v) != "" {
				blank = false
				break
			}
		}
		if !blank {
			break
		}
		rows = rows[:len(rows)-1]
	}
	return rows, nil
}

func cellValue(cell *element, shared []string) string {
	value := cell.childText(nsS, "v")
	typ, _ := cell.attr("", "t")
	switch typ {
	case "s":
		idx, err := strconv.Atoi(strip(value))
		if err != nil {
			return ""
		}
		if idx < 0 {
			idx += len(shared)
		}
		if idx < 0 || idx >= len(shared) {
			return ""
		}
		return shared[idx]
	case "inlineStr":
		if inline := cell.child(nsS, "is"); inline != nil {
			return stringItemText(inline)
		}
		return ""
	case "b":
		switch strip(value) {
		case "1", "true", "TRUE":
			return "TRUE"
		}
		return "FALSE"
	case "e":
		if value == "" {
			return "#ERROR"
		}
	}
	return value
}
