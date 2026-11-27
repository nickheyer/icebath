package document

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html/charset"
)

const (
	nsW      = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsS      = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsRel    = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsPkgRel = "http://schemas.openxmlformats.org/package/2006/relationships"
	// Rows read of each sheet, and columns of each row
	maxSheetRows = 5000
	maxSheetCols = 256
)

// Text of the paragraphs of word/document.xml with their tabs and breaks, ruby guide text left out
func docx(data []byte) (string, error) {
	pkg, err := openPackage(data, "DOCX")
	if err != nil {
		return "", err
	}
	root, err := pkg.part("word/document.xml", false)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, p := range root.all(nsW, "p") {
		var b strings.Builder
		p.walk(func(e *element) bool {
			if e.name.Space != nsW {
				return true
			}
			switch e.name.Local {
			case "rt":
				return false
			case "t":
				b.WriteString(e.text)
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
			return true
		})
		lines = append(lines, strings.Split(b.String(), "\n")...)
	}
	return joined(lines, "DOCX contains no extractable text")
}

// Visible worksheets as rows of tab separated cells under a heading each
func xlsx(data []byte) (string, error) {
	pkg, err := openPackage(data, "XLSX")
	if err != nil {
		return "", err
	}
	strs, err := pkg.part("xl/sharedStrings.xml", true)
	if err != nil {
		return "", err
	}
	var shared []string
	for _, item := range strs.all(nsS, "si") {
		shared = append(shared, richText(item))
	}
	relations, err := pkg.part("xl/_rels/workbook.xml.rels", true)
	if err != nil {
		return "", err
	}
	targets := map[string]string{}
	for _, rel := range relations.all(nsPkgRel, "Relationship") {
		if id := rel.attr("", "Id", ""); id != "" {
			targets[id] = rel.attr("", "Target", "")
		}
	}
	workbook, err := pkg.part("xl/workbook.xml", false)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, sheet := range workbook.all(nsS, "sheet") {
		target := strings.TrimLeft(targets[sheet.attr(nsRel, "id", "")], "/")
		if !strings.HasPrefix(target, "xl/") {
			target = "xl/" + target
		}
		part := path.Clean(target)
		state := sheet.attr("", "state", "visible")
		if _, ok := pkg.files[part]; state == "hidden" || state == "veryHidden" || !ok {
			continue
		}
		var rows [][]string
		if err := pkg.decode(part, func(decoder *xml.Decoder) (err error) {
			rows, err = sheetRows(decoder, shared)
			return err
		}); err != nil {
			var damaged *Error
			if errors.As(err, &damaged) {
				return "", err
			}
			continue
		}
		lines = append(lines, "# ── Sheet: "+sheet.attr("", "name", "Sheet")+" ──")
		if len(rows) == 0 {
			lines = append(lines, "(empty)")
		}
		for _, row := range rows {
			lines = append(lines, strings.Join(row, "\t"))
		}
		lines = append(lines, "")
	}
	return joined(lines, "XLSX has no visible sheets with content")
}

// Rows of a worksheet, capped in rows and columns, the blank rows at its end dropped; reading stops at the row cap
func sheetRows(decoder *xml.Decoder, shared []string) ([][]string, error) {
	var rows [][]string
	var tree treeBuilder
	for len(rows) < maxSheetRows {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !tree.building() {
			if start, ok := token.(xml.StartElement); !ok || start.Name.Space != nsS || start.Name.Local != "row" {
				continue
			}
		}
		if row := tree.take(token); row != nil {
			for _, r := range row.all(nsS, "row") {
				if len(rows) < maxSheetRows {
					rows = append(rows, rowValues(r, shared))
				}
			}
		}
	}
	for len(rows) > 0 && strings.TrimSpace(strings.Join(rows[len(rows)-1], "")) == "" {
		rows = rows[:len(rows)-1]
	}
	return rows, nil
}

// Cells of a row by column, a cell without a reference taking the column after the last one placed
func rowValues(row *element, shared []string) []string {
	cells := map[int]string{}
	last := -1
	for _, cell := range row.all(nsS, "c") {
		col := last + 1
		if ref := cell.attr("", "r", ""); ref != "" {
			col = column(ref)
		}
		if col < maxSheetCols {
			cells[col] = cellValue(cell, shared)
			last = max(last, col)
		}
	}
	values := make([]string, last+1)
	for c := range values {
		values[c] = cells[c]
	}
	return values
}

// Zero based column of a cell reference from its letters, counted past the column cap without overflowing
func column(ref string) int {
	n := 0
	for _, r := range ref {
		if !unicode.IsLetter(r) {
			break
		}
		n = min(n*26+int(unicode.ToUpper(r))-'A'+1, maxSheetCols+1)
	}
	return max(n-1, 0)
}

// Shown value of a cell: shared or inline string, boolean, error, or the stored value
func cellValue(cell *element, shared []string) string {
	value := cell.child(nsS, "v").textOf()
	switch cell.attr("", "t", "") {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || i < 0 || i >= len(shared) {
			return ""
		}
		return shared[i]
	case "inlineStr":
		inline := cell.child(nsS, "is")
		if inline == nil {
			return ""
		}
		return richText(inline)
	case "b":
		switch strings.TrimSpace(value) {
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

// Text of a shared or inline string, its runs joined and its phonetic guides left out
func richText(item *element) string {
	var b strings.Builder
	for _, child := range item.children {
		if child.name.Space != nsS {
			continue
		}
		switch child.name.Local {
		case "t":
			b.WriteString(child.text)
		case "r":
			b.WriteString(child.child(nsS, "t").textOf())
		}
	}
	return b.String()
}

// Office document's zip package with its entries by name, the last of a repeated name winning
type officePackage struct {
	files map[string]*zip.File
	// DOCX or XLSX, naming it in errors
	kind string
}

func openPackage(data []byte, kind string) (*officePackage, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	// Entries are only read by name, so names that would be unsafe to extract to are no concern
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, packageFailure(kind, err)
	}
	pkg := &officePackage{files: map[string]*zip.File{}, kind: kind}
	for _, f := range reader.File {
		pkg.files[f.Name] = f
	}
	return pkg, nil
}

func packageFailure(kind string, err error) error {
	switch {
	case errors.Is(err, zip.ErrFormat):
		return fail("Not a valid %s: File is not a zip file", kind)
	case errors.Is(err, zip.ErrChecksum):
		return fail("Not a valid %s: Bad CRC-32", kind)
	}
	return fail("Not a valid %s: %s", kind, err.Error())
}

// Runs read over a decoder of the entry, a damaged archive failing with an *Error and a malformed part with the decoder's error
func (p *officePackage) decode(name string, read func(*xml.Decoder) error) error {
	rc, err := p.files[name].Open()
	if err != nil {
		return packageFailure(p.kind, err)
	}
	defer rc.Close()
	entry := &entryReader{reader: rc}
	err = read(xmlDecoder(entry))
	if entry.err != nil {
		return packageFailure(p.kind, entry.err)
	}
	return err
}

// Reader of an archive entry keeping the error reading it failed with
type entryReader struct {
	reader io.Reader
	err    error
}

func (r *entryReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

// Root element of an XML part; an optional part that is missing or malformed giving nil
func (p *officePackage) part(name string, optional bool) (*element, error) {
	if _, ok := p.files[name]; !ok {
		if optional {
			return nil, nil
		}
		return nil, fail("Missing %s", name)
	}
	var root *element
	err := p.decode(name, func(decoder *xml.Decoder) (err error) {
		root, err = parseXML(decoder)
		return err
	})
	var damaged *Error
	switch {
	case errors.As(err, &damaged):
		return nil, err
	case err != nil && optional:
		return nil, nil
	case err != nil:
		return nil, fail("Malformed XML in %s: %s", name, err.Error())
	}
	return root, nil
}

// Decoder of an XML document in UTF-8, UTF-16 or UTF-32, or in the encoding its declaration names; a byte order mark or the zero
// bytes of UTF-16 and UTF-32 outranking the declaration
func xmlDecoder(r io.Reader) *xml.Decoder {
	buffered := bufio.NewReader(r)
	head, _ := buffered.Peek(4)
	enc, mark := detect(head)
	buffered.Discard(mark)
	var source io.Reader = buffered
	if enc != nil {
		source = enc.NewDecoder().Reader(buffered)
	}
	decoder := xml.NewDecoder(source)
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if enc != nil {
			return input, nil
		}
		return charset.NewReaderLabel(label, input)
	}
	return decoder
}

// Element of an XML part: its name, its attributes, the text before its first child, and its child elements
type element struct {
	name     xml.Name
	attrs    []xml.Attr
	text     string
	children []*element
}

// Elements built from the tokens of a decoder
type treeBuilder struct {
	open []*element
}

// Whether an element is open
func (b *treeBuilder) building() bool {
	return len(b.open) > 0
}

// Takes the next token, returning the outermost element once it closes
func (b *treeBuilder) take(token xml.Token) *element {
	switch t := token.(type) {
	case xml.StartElement:
		e := &element{name: t.Name, attrs: t.Copy().Attr}
		if n := len(b.open); n > 0 {
			b.open[n-1].children = append(b.open[n-1].children, e)
		}
		b.open = append(b.open, e)
	case xml.EndElement:
		outermost := b.open[0]
		if b.open = b.open[:len(b.open)-1]; len(b.open) == 0 {
			return outermost
		}
	case xml.CharData:
		if n := len(b.open); n > 0 && len(b.open[n-1].children) == 0 {
			b.open[n-1].text += string(t)
		}
	}
	return nil
}

// Root element of a whole XML document
func parseXML(decoder *xml.Decoder) (*element, error) {
	var tree treeBuilder
	var root *element
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !tree.building() {
			switch t := token.(type) {
			case xml.StartElement:
				if root != nil {
					return nil, xmlError(decoder, "junk after document element")
				}
			case xml.CharData:
				if len(bytes.TrimSpace(t)) > 0 {
					return nil, xmlError(decoder, "text outside the document element")
				}
			}
		}
		if e := tree.take(token); e != nil {
			root = e
		}
	}
	if root == nil {
		return nil, xmlError(decoder, "no element found")
	}
	return root, nil
}

func xmlError(decoder *xml.Decoder, msg string) error {
	line, _ := decoder.InputPos()
	return &xml.SyntaxError{Msg: msg, Line: line}
}

// Visits the element and its descendants in document order, not entering those visit returns false for
func (e *element) walk(visit func(*element) bool) {
	if e == nil || !visit(e) {
		return
	}
	for _, child := range e.children {
		child.walk(visit)
	}
}

// The element and its descendants with the name, in document order
func (e *element) all(space, local string) []*element {
	var found []*element
	e.walk(func(x *element) bool {
		if x.name.Space == space && x.name.Local == local {
			found = append(found, x)
		}
		return true
	})
	return found
}

// First child with the name, nil when none
func (e *element) child(space, local string) *element {
	if e == nil {
		return nil
	}
	for _, c := range e.children {
		if c.name.Space == space && c.name.Local == local {
			return c
		}
	}
	return nil
}

// Text of the element, empty when there is none
func (e *element) textOf() string {
	if e == nil {
		return ""
	}
	return e.text
}

// Value of the attribute, the fallback when absent
func (e *element) attr(space, local, fallback string) string {
	for _, a := range e.attrs {
		if a.Name.Space == space && a.Name.Local == local {
			return a.Value
		}
	}
	return fallback
}
