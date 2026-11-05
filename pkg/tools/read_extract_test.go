package tools

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rc4"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

type readExtractGolden struct {
	raw      map[string]json.RawMessage
	Fixtures struct {
		Notebooks map[string]string `json:"notebooks"`
		DocxXML   string            `json:"docx_xml"`
		XlsxParts map[string]string `json:"xlsx_parts"`
	} `json:"fixtures"`
}

func loadReadExtractGolden(t *testing.T) *readExtractGolden {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(readExtractGoldenB64)
	if err != nil {
		t.Fatal(err)
	}
	g := &readExtractGolden{}
	if err := json.Unmarshal(data, &g.raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, g); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *readExtractGolden) str(t *testing.T, key string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(g.raw[key], &s); err != nil {
		t.Fatalf("golden %s: %v", key, err)
	}
	return s
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for k := range files {
		names = append(names, k)
	}
	sort.Strings(names)
	if _, ok := files["mimetype"]; ok {
		for i, n := range names {
			if n == "mimetype" {
				names = append([]string{"mimetype"}, append(names[:i:i], names[i+1:]...)...)
				break
			}
		}
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func extractionMessage(err error) string {
	var ee *ExtractionError
	if errors.As(err, &ee) {
		return ee.Msg
	}
	if err != nil {
		return "non-extraction error: " + err.Error()
	}
	return ""
}

func TestReadExtractNotebookGolden(t *testing.T) {
	g := loadReadExtractGolden(t)
	for name, text := range g.Fixtures.Notebooks {
		var want struct{ Ok, Err string }
		if err := json.Unmarshal(g.raw[name], &want); err != nil {
			t.Fatal(err)
		}
		p := writeFixture(t, name+".ipynb", []byte(text))
		got, err := extractNotebook(p, "/work/"+name+".ipynb")
		if got != want.Ok || extractionMessage(err) != want.Err {
			t.Errorf("%s:\n got  %q %q\n want %q %q", name, got, extractionMessage(err), want.Ok, want.Err)
		}
	}
}

func TestReadExtractDocxGolden(t *testing.T) {
	g := loadReadExtractGolden(t)
	p := writeFixture(t, "a.docx", zipOf(t, map[string]string{"word/document.xml": g.Fixtures.DocxXML}))
	got, err := ExtractDocumentText(context.Background(), nil, p)
	if err != nil || got != g.str(t, "docx") {
		t.Fatalf("docx:\n got  %q %v\n want %q", got, err, g.str(t, "docx"))
	}
	_, err = extractDocx(writeFixture(t, "n.docx", zipOf(t, map[string]string{"other.xml": "<x/>"})))
	if extractionMessage(err) != g.str(t, "docx_missing") {
		t.Fatalf("missing part: %v", err)
	}
	_, err = extractDocx(writeFixture(t, "bad.docx", []byte("not a zip")))
	if extractionMessage(err) != g.str(t, "docx_badzip") {
		t.Fatalf("bad zip: %q want %q", extractionMessage(err), g.str(t, "docx_badzip"))
	}
	_, err = extractDocx(writeFixture(t, "x.docx", zipOf(t, map[string]string{"word/document.xml": "<w:document"})))
	if !strings.HasPrefix(extractionMessage(err), "Malformed XML in word/document.xml: ") {
		t.Fatalf("bad xml: %v", err)
	}
}

func TestReadExtractXlsxGolden(t *testing.T) {
	g := loadReadExtractGolden(t)
	p := writeFixture(t, "a.xlsx", zipOf(t, g.Fixtures.XlsxParts))
	got, err := ExtractDocumentText(context.Background(), nil, p)
	if err != nil || got != g.str(t, "xlsx") {
		t.Fatalf("xlsx:\n got  %q %v\n want %q", got, err, g.str(t, "xlsx"))
	}
}

func TestReadExtractSqliteGolden(t *testing.T) {
	g := loadReadExtractGolden(t)
	var stmts []string
	if err := json.Unmarshal(g.raw["sqlite_sql"], &stmts); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "a.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	db.Close()
	got, err := ExtractDocumentText(context.Background(), nil, p)
	if err != nil || got != g.str(t, "sqlite") {
		t.Fatalf("sqlite:\n got  %q %v\n want %q", got, err, g.str(t, "sqlite"))
	}
	_, err = extractSqlite(writeFixture(t, "fake.db", []byte("not sqlite at all")))
	if extractionMessage(err) != g.str(t, "sqlite_fake") {
		t.Fatalf("fake db: %v", err)
	}
}

func TestReadExtractHelpersGolden(t *testing.T) {
	g := loadReadExtractGolden(t)
	if msg := extractionMessage(checkDocumentSize(maxDocumentBytes+1, maxDocumentBytes)); msg != g.str(t, "size_err") {
		t.Fatalf("size error %q", msg)
	}
	if got := needsOCRWarning("/tmp/my scan.pdf", []int{2, 3}, "HostedError: boom"); got != g.str(t, "ocr_warning") {
		t.Fatalf("ocr warning:\n got  %q\n want %q", got, g.str(t, "ocr_warning"))
	}
	gap := pdfGapMap([]int{100, 0, 0, 50, 0, 3}, []string{"Intro   text here", "", "", "Chapter two starts", "", "x"}, []int{2, 3, 5, 6})
	if gap != g.str(t, "gap_map") {
		t.Fatalf("gap map:\n got  %q\n want %q", gap, g.str(t, "gap_map"))
	}
	var b64 []int
	var human []string
	json.Unmarshal(g.raw["b64"], &b64)
	json.Unmarshal(g.raw["human"], &human)
	for i, s := range []string{"", "QQ==", "QUI=", "QUJD", "Q U J D\n", "AAAAAAAA"} {
		if base64Bytes(s) != b64[i] {
			t.Errorf("base64Bytes(%q) = %d want %d", s, base64Bytes(s), b64[i])
		}
	}
	for i, n := range []int{0, 1023, 1024, 1536, 2560, 3584, 1000000} {
		if humanSize(n) != human[i] {
			t.Errorf("humanSize(%d) = %q want %q", n, humanSize(n), human[i])
		}
	}
	if IsExtractableDocument("x.TXT") || !IsExtractableDocument("/a/B.PDF") || IsExtractableDocument(".docx") || !IsExtractableDocument("deck.ppsx") {
		t.Fatal("extension recognition")
	}
	if _, err := ExtractDocumentText(context.Background(), nil, "notes.txt"); extractionMessage(err) != "Unsupported document type: 'notes.txt'" {
		t.Fatalf("unsupported: %v", err)
	}
}

func TestReadExtractJSONParser(t *testing.T) {
	cases := map[string]string{
		"":         "Expecting value: line 1 column 1 (char 0)",
		`{"a" 1}`:  "Expecting ':' delimiter: line 1 column 6 (char 5)",
		"[1 2]":    "Expecting ',' delimiter: line 1 column 4 (char 3)",
		`{"a":1,}`: "Illegal trailing comma before end of object: line 1 column 7 (char 6)",
		`"abc`:     "Unterminated string starting at: line 1 column 1 (char 0)",
		`"a\qb"`:   `Invalid \escape: line 1 column 3 (char 2)`,
		`"\u12"`:   `Invalid \uXXXX escape: line 1 column 3 (char 2)`,
		"[1]x":     "Extra data: line 1 column 4 (char 3)",
		"\"a\nb\"": "Invalid control character at: line 1 column 3 (char 2)",
		"nul":      "Expecting value: line 1 column 1 (char 0)",
		"{1:2}":    "Expecting property name enclosed in double quotes: line 1 column 2 (char 1)",
	}
	for in, want := range cases {
		_, err := pyJSONLoads(in)
		if err == nil || err.Error() != want {
			t.Errorf("pyJSONLoads(%q) = %v want %q", in, err, want)
		}
	}
	v, err := pyJSONLoads(`{"b": [1, 2.5, NaN, -Infinity, "x\ud83d\ude00"], "a": {"k": null}, "b": true}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := pyJSONRepr(v); got != "{'b': True, 'a': {'k': None}}" {
		t.Fatalf("repr %s", got)
	}
	for f, want := range map[float64]string{1e16: "1e+16", 2.5: "2.5", 1e-5: "1e-05", 123456789012345678.0: "1.2345678901234568e+17", 0.1: "0.1", math.Copysign(0, -1): "-0.0", 1e22: "1e+22", 1.5e-7: "1.5e-07", 100.0: "100.0"} {
		if got := pyFloatRepr(f); got != want {
			t.Errorf("pyFloatRepr(%v) = %s want %s", f, got, want)
		}
	}
}

func TestReadExtractDecodeUTF8Replace(t *testing.T) {
	if got := decodeUTF8Replace([]byte("a\xe2\x82Z\xed\xa0\x80b\xf0\x9f\x98")); got != "a\ufffdZ\ufffd\ufffd\ufffdb\ufffd" {
		t.Fatalf("decodeUTF8Replace = %q", got)
	}
}

// Assembles a PDF with a classic cross reference table from object bodies numbered from one
func buildTestPDF(objects []string, trailerExtra string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objects)+1)
	for i, o := range objects {
		offsets[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R %s>>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, trailerExtra, xref)
	return b.Bytes()
}

func zlibBytes(data []byte) []byte {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	w.Write(data)
	w.Close()
	return b.Bytes()
}

func pdfStreamObj(dict string, data []byte) string {
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data)
}

func simpleTextPDF(page1, page2 []byte, wrap func(num int, data []byte) []byte, filter string) []string {
	return []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 /Resources << /Font << /F1 5 0 R >> >> >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		pdfStreamObj(filter, wrap(4, page1)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 7 0 R >>",
		pdfStreamObj(filter, wrap(7, page2)),
	}
}

var pdfPage1 = []byte("BT /F1 12 Tf 72 720 Td (Hello PDF) Tj ( World) Tj 0 -14 Td [(Sec) -20 (ond) -600 (line)] TJ ET")
var pdfPage2 = []byte("BT /F1 10 Tf 72 700 Td (Caf\\351 \\226 page two) Tj ET")

func flatePDF() []byte {
	return buildTestPDF(simpleTextPDF(pdfPage1, pdfPage2, func(_ int, d []byte) []byte { return zlibBytes(d) }, "/Filter /FlateDecode"), "")
}

// Unfiltered two page PDF whose startxref offset points into the wrong byte
func brokenXrefPDF() []byte {
	data := buildTestPDF(simpleTextPDF(pdfPage1, pdfPage2, func(_ int, d []byte) []byte { return d }, ""), "")
	return bytes.Replace(data, []byte("startxref\n"), []byte("startxref\n9"), 1)
}

// Builds a PDF whose core objects live in an object stream indexed by a predicted xref stream
func objectStreamPDF() []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n")
	offsets := map[int]int{}
	write := func(num int, body string) {
		offsets[num] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", num, body)
	}
	write(4, pdfStreamObj("/Filter /FlateDecode", zlibBytes([]byte("BT /F0 11 Tf 50 700 Td <00010002> Tj ET /Fm Do"))))
	cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap 1 begincodespacerange <0000> <FFFF> endcodespacerange 1 beginbfchar <0001> <0048> endbfchar 1 beginbfrange <0002> <0003> <0069> endbfrange endcmap end end"
	write(7, pdfStreamObj("", []byte(cmap)))
	write(9, pdfStreamObj("/Type /XObject /Subtype /Form /BBox [0 0 600 800] /Resources << /Font << /F0 5 0 R >> >>", []byte("BT /F0 11 Tf 50 650 Td <0003> Tj ET")))
	inner := []struct {
		num  int
		body string
	}{
		{1, "<< /Type /Catalog /Pages 2 0 R >>"},
		{2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{3, "<< /Type /Page /Parent 2 0 R /Contents 4 0 R /Resources << /Font << /F0 5 0 R >> /XObject << /Fm 9 0 R >> >> >>"},
		{5, "<< /Type /Font /Subtype /Type0 /BaseFont /ABCDEF+Foo /Encoding /Identity-H /DescendantFonts [6 0 R] /ToUnicode 7 0 R >>"},
		{6, "<< /Type /Font /Subtype /CIDFontType2 /BaseFont /Foo /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /DW 500 /W [1 [600 600]] >>"},
	}
	var header, body bytes.Buffer
	for _, o := range inner {
		fmt.Fprintf(&header, "%d %d ", o.num, body.Len())
		body.WriteString(o.body + "\n")
	}
	stm := append(header.Bytes(), body.Bytes()...)
	write(8, pdfStreamObj(fmt.Sprintf("/Type /ObjStm /N %d /First %d /Filter /FlateDecode", len(inner), header.Len()), zlibBytes(stm)))
	xrefOff := b.Len()
	rows := make([][]byte, 11)
	row := func(typ byte, a uint32, c uint16) []byte {
		r := []byte{typ, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(r[1:], a)
		binary.BigEndian.PutUint16(r[5:], c)
		return r
	}
	rows[0] = row(0, 0, 0xFFFF)
	for i, o := range inner {
		rows[o.num] = row(2, 8, uint16(i))
	}
	for _, n := range []int{4, 7, 8, 9} {
		rows[n] = row(1, uint32(offsets[n]), 0)
	}
	rows[10] = row(1, uint32(xrefOff), 0)
	var enc []byte
	prev := make([]byte, 7)
	for _, r := range rows {
		enc = append(enc, 2)
		for i := range r {
			enc = append(enc, r[i]-prev[i])
		}
		prev = r
	}
	fmt.Fprintf(&b, "10 0 obj\n%s\nendobj\nstartxref\n%d\n%%%%EOF\n", pdfStreamObj("/Type /XRef /Size 11 /Root 1 0 R /W [1 4 2] /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns 7 >>", zlibBytes(enc)), xrefOff)
	return b.Bytes()
}

var pdfPasswordPad = []byte{0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80, 0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A}

func rc4Bytes(key, data []byte) []byte {
	c, err := rc4.NewCipher(key)
	if err != nil {
		panic(err)
	}
	out := make([]byte, len(data))
	c.XORKeyStream(out, data)
	return out
}

// Per object RC4 or AESV2 key from the file key, PDF 1.7 algorithm 1
func pdfObjectKey(key []byte, num, gen int, aesSalt bool) []byte {
	h := md5.New()
	h.Write(key)
	h.Write([]byte{byte(num), byte(num >> 8), byte(num >> 16), byte(gen), byte(gen >> 8)})
	if aesSalt {
		h.Write([]byte("sAlT"))
	}
	n := len(key) + 5
	if n > 16 {
		n = 16
	}
	return h.Sum(nil)[:n]
}

// PDF 2.0 hash algorithm 2.B for revision 6 passwords
func pdfHash2B(password, salt, udata []byte) []byte {
	h := sha256.New()
	h.Write(password)
	h.Write(salt)
	h.Write(udata)
	k := h.Sum(nil)
	for round := 0; ; round++ {
		seq := append(append(append([]byte{}, password...), k...), udata...)
		k1 := bytes.Repeat(seq, 64)
		block, _ := aes.NewCipher(k[:16])
		e := make([]byte, len(k1))
		cipher.NewCBCEncrypter(block, k[16:32]).CryptBlocks(e, k1)
		sum := 0
		for _, b := range e[:16] {
			sum += int(b)
		}
		var hf hash.Hash
		switch sum % 3 {
		case 0:
			hf = sha256.New()
		case 1:
			hf = sha512.New384()
		default:
			hf = sha512.New()
		}
		hf.Write(e)
		k = hf.Sum(nil)
		if round >= 63 && int(e[len(e)-1]) <= round+1-32 {
			break
		}
	}
	return k[:32]
}

var testPDFID = []byte("0123456789abcdef")
var testPDFOwner = bytes.Repeat([]byte{0x5A}, 32)

func rc4R3Key() []byte {
	h := md5.New()
	h.Write(pdfPasswordPad)
	h.Write(testPDFOwner)
	h.Write([]byte{0xFC, 0xFF, 0xFF, 0xFF})
	h.Write(testPDFID)
	key := h.Sum(nil)
	for i := 0; i < 50; i++ {
		s := md5.Sum(key[:16])
		key = s[:]
	}
	return key[:16]
}

func rc4R3User(key []byte) []byte {
	h := md5.New()
	h.Write(pdfPasswordPad)
	h.Write(testPDFID)
	x := rc4Bytes(key, h.Sum(nil))
	for i := 1; i <= 19; i++ {
		k := make([]byte, len(key))
		for j := range key {
			k[j] = key[j] ^ byte(i)
		}
		x = rc4Bytes(k, x)
	}
	return append(x, bytes.Repeat([]byte{0}, 16)...)
}

func aesCBCEncrypt(key, data []byte) []byte {
	pad := 16 - len(data)%16
	data = append(append([]byte{}, data...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := []byte("fedcba9876543210")
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
	return append(iv, out...)
}

func encryptTrailer() string {
	return fmt.Sprintf("/Encrypt 8 0 R /ID [<%x> <%x>] ", testPDFID, testPDFID)
}

// RC4 revision 3 with an empty user password
func rc4PDF() []byte {
	key := rc4R3Key()
	objs := simpleTextPDF(pdfPage1, pdfPage2, func(num int, d []byte) []byte { return rc4Bytes(pdfObjectKey(key, num, 0, false), zlibBytes(d)) }, "/Filter /FlateDecode")
	objs = append(objs, fmt.Sprintf("<< /Filter /Standard /V 2 /R 3 /Length 128 /O <%x> /U <%x> /P -4 >>", testPDFOwner, rc4R3User(key)))
	return buildTestPDF(objs, encryptTrailer())
}

// AESV2 crypt filter with an empty user password
func aesV2PDF() []byte {
	key := rc4R3Key()
	objs := simpleTextPDF(pdfPage1, pdfPage2, func(num int, d []byte) []byte { return aesCBCEncrypt(pdfObjectKey(key, num, 0, true), zlibBytes(d)) }, "/Filter /FlateDecode")
	objs = append(objs, fmt.Sprintf("<< /Filter /Standard /V 4 /R 4 /Length 128 /CF << /StdCF << /CFM /AESV2 /AuthEvent /DocOpen /Length 16 >> >> /StmF /StdCF /StrF /StdCF /O <%x> /U <%x> /P -4 >>", testPDFOwner, rc4R3User(key)))
	return buildTestPDF(objs, encryptTrailer())
}

// AESV3 revision 6 with an empty user password
func aesV3PDF() []byte {
	fileKey := bytes.Repeat([]byte{0x42}, 32)
	vSalt, kSalt := []byte("vsalt123"), []byte("ksalt456")
	u := append(append(pdfHash2B(nil, vSalt, nil), vSalt...), kSalt...)
	interKey := pdfHash2B(nil, kSalt, nil)
	block, _ := aes.NewCipher(interKey)
	ue := make([]byte, 32)
	cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(ue, fileKey)
	objs := simpleTextPDF(pdfPage1, pdfPage2, func(_ int, d []byte) []byte { return aesCBCEncrypt(fileKey, zlibBytes(d)) }, "/Filter /FlateDecode")
	objs = append(objs, fmt.Sprintf("<< /Filter /Standard /V 5 /R 6 /Length 256 /CF << /StdCF << /CFM /AESV3 /Length 32 >> >> /StmF /StdCF /StrF /StdCF /O <%x> /U <%x> /UE <%x> /OE <%x> /P -4 >>", bytes.Repeat([]byte{1}, 48), u, ue, bytes.Repeat([]byte{2}, 32)))
	return buildTestPDF(objs, encryptTrailer())
}

// RC4 document whose user password is not empty
func lockedPDF() []byte {
	key := rc4R3Key()
	objs := simpleTextPDF(pdfPage1, pdfPage2, func(num int, d []byte) []byte { return rc4Bytes(pdfObjectKey(key, num, 0, false), zlibBytes(d)) }, "/Filter /FlateDecode")
	objs = append(objs, fmt.Sprintf("<< /Filter /Standard /V 2 /R 3 /Length 128 /O <%x> /U <%x> /P -4 >>", testPDFOwner, bytes.Repeat([]byte{9}, 32)))
	return buildTestPDF(objs, encryptTrailer())
}

func scannedPDF() []byte {
	return buildTestPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /Contents 4 0 R /Resources << /XObject << /Im0 5 0 R >> >> >>",
		pdfStreamObj("", []byte("q 612 0 0 792 0 0 cm /Im0 Do Q")),
		pdfStreamObj("/Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8", []byte{0x80}),
		"<< /Type /Page /Parent 2 0 R /Contents 7 0 R >>",
		pdfStreamObj("", []byte("BI /W 1 /H 1 /BPC 8 /CS /G ID \x80 EI")),
	}, "")
}

// Minimal sfnt holding only a format 4 cmap that maps the given BMP code points to glyph ids
func testTrueTypeFont(mapping map[rune]uint16) []byte {
	be := func(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }
	codes := make([]int, 0, len(mapping))
	for r := range mapping {
		codes = append(codes, int(r))
	}
	sort.Ints(codes)
	segs := len(codes) + 1
	var ends, starts, deltas, ranges []byte
	for _, c := range codes {
		ends = append(ends, be(uint16(c))...)
		starts = append(starts, be(uint16(c))...)
		deltas = append(deltas, be(mapping[rune(c)]-uint16(c))...)
		ranges = append(ranges, 0, 0)
	}
	ends, starts, deltas, ranges = append(ends, 0xFF, 0xFF), append(starts, 0xFF, 0xFF), append(deltas, 0, 1), append(ranges, 0, 0)
	sub := bytes.Join([][]byte{be(4), be(0), be(0), be(uint16(2 * segs)), be(0), be(0), be(0), ends, be(0), starts, deltas, ranges}, nil)
	binary.BigEndian.PutUint16(sub[2:], uint16(len(sub)))
	cmap := append(bytes.Join([][]byte{be(0), be(1), be(3), be(1), {0, 0, 0, 12}}, nil), sub...)
	head := bytes.Join([][]byte{{0, 1, 0, 0}, be(1), be(16), be(0), be(0), []byte("cmap"), make([]byte, 4), {0, 0, 0, 28}, make([]byte, 4)}, nil)
	binary.BigEndian.PutUint32(head[24:], uint32(len(cmap)))
	return append(head, cmap...)
}

// One page using an embedded TrueType CID font and a Type1 font with a custom builtin encoding
func fontsPDF() []byte {
	font := testTrueTypeFont(map[rune]uint16{'O': 5, 'k': 9})
	type1 := []byte("%!PS-AdobeFont-1.0: Test\n/Encoding 256 array\n0 1 255 {1 index exch /.notdef put} for\ndup 65 /Z put\ndup 66 /eacute put\nreadonly def\ncurrentfile eexec\nBINARY")
	return buildTestPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /Contents 4 0 R /Resources << /Font << /F0 5 0 R /F1 8 0 R >> >> >>",
		pdfStreamObj("", []byte("BT /F0 12 Tf 10 700 Td <00050009> Tj ET BT /F1 12 Tf 10 600 Td (AB) Tj ET")),
		"<< /Type /Font /Subtype /Type0 /BaseFont /Embedded /Encoding /Identity-H /DescendantFonts [6 0 R] >>",
		"<< /Type /Font /Subtype /CIDFontType2 /BaseFont /Embedded /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor 7 0 R /CIDToGIDMap /Identity >>",
		"<< /Type /FontDescriptor /FontName /Embedded /Flags 4 /FontFile2 9 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Custom /FirstChar 65 /LastChar 66 /Widths [600 600] /FontDescriptor 10 0 R >>",
		pdfStreamObj("/Filter /FlateDecode", zlibBytes(font)),
		"<< /Type /FontDescriptor /FontName /Custom /Flags 32 /FontFile 11 0 R >>",
		pdfStreamObj("", type1),
	}, "")
}

const pNS = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`

func pptxFixture(t *testing.T) []byte {
	return zipOf(t, map[string]string{
		"[Content_Types].xml":             `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/></Types>`,
		"_rels/.rels":                     `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`,
		"ppt/presentation.xml":            `<p:presentation ` + pNS + `><p:sldIdLst><p:sldId id="256" r:id="rId2"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/></Relationships>`,
		"ppt/slides/slide1.xml": `<p:sld ` + pNS + `><p:cSld><p:spTree>` +
			`<p:sp><p:nvSpPr><p:cNvPr id="2" name="Title"/><p:cNvSpPr/><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>Quarterly Review</a:t></a:r></a:p></p:txBody></p:sp>` +
			`<p:sp><p:nvSpPr><p:cNvPr id="3" name="Body"/><p:cNvSpPr/><p:nvPr><p:ph type="body" idx="1"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>Revenue up</a:t></a:r></a:p><a:p><a:pPr lvl="1"/><a:r><a:t>Costs down</a:t></a:r></a:p></p:txBody></p:sp>` +
			`<p:sp><p:nvSpPr><p:cNvPr id="4" name="Box"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:txBody><a:p><a:r><a:t>Footnote </a:t></a:r><a:r><a:t>text</a:t></a:r></a:p></p:txBody></p:sp>` +
			`<p:sp><p:nvSpPr><p:cNvPr id="5" name="Num"/><p:cNvSpPr/><p:nvPr><p:ph type="sldNum"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:fld><a:t>7</a:t></a:fld></a:p></p:txBody></p:sp>` +
			`<p:graphicFrame><a:graphic><a:graphicData><a:tbl><a:tblPr firstRow="1"/><a:tr><a:tc><a:txBody><a:p><a:r><a:t>Region</a:t></a:r></a:p></a:txBody></a:tc><a:tc><a:txBody><a:p><a:r><a:t>Sales</a:t></a:r></a:p></a:txBody></a:tc></a:tr><a:tr><a:tc><a:txBody><a:p><a:r><a:t>East</a:t></a:r></a:p></a:txBody></a:tc><a:tc><a:txBody><a:p><a:r><a:t>10</a:t></a:r></a:p></a:txBody></a:tc></a:tr></a:tbl></a:graphicData></a:graphic></p:graphicFrame>` +
			`</p:spTree></p:cSld></p:sld>`,
		"ppt/slides/_rels/slide1.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId9" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesSlide" Target="../notesSlides/notesSlide1.xml"/></Relationships>`,
		"ppt/notesSlides/notesSlide1.xml":  `<p:notes ` + pNS + `><p:cSld><p:spTree><p:sp><p:nvSpPr><p:nvPr><p:ph type="sldImg"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>image</a:t></a:r></a:p></p:txBody></p:sp><p:sp><p:nvSpPr><p:nvPr><p:ph type="body" idx="1"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>Speaker notes here</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:notes>`,
	})
}

const odfNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0"`

func odtFixture(t *testing.T) []byte {
	return zipOf(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text", "content.xml": `<office:document-content ` + odfNS + `><office:body><office:text>` +
		`<text:h text:outline-level="1">Heading</text:h><text:p>Two<text:s text:c="2"/>spaces<text:tab/>tab<text:note><text:note-citation>1</text:note-citation><text:note-body><text:p>Note body</text:p></text:note-body></text:note></text:p>` +
		`<text:list><text:list-item><text:p>Item one</text:p><text:list><text:list-item><text:p>Nested</text:p></text:list-item></text:list></text:list-item></text:list>` +
		`<table:table table:name="T"><table:table-row><table:table-cell><text:p>a</text:p></table:table-cell><table:table-cell table:number-columns-repeated="2"><text:p>b</text:p></table:table-cell></table:table-row></table:table>` +
		`</office:text></office:body></office:document-content>`})
}

func emptyODT(t *testing.T) []byte {
	return zipOf(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text", "content.xml": `<office:document-content ` + odfNS + `><office:body><office:text/></office:body></office:document-content>`})
}

func odsFixture(t *testing.T) []byte {
	return zipOf(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.spreadsheet", "content.xml": `<office:document-content ` + odfNS + `><office:automatic-styles><style:style style:name="ta2" style:family="table"><style:table-properties table:display="false"/></style:style></office:automatic-styles><office:body><office:spreadsheet>` +
		`<table:table table:name="One"><table:table-row><table:table-cell><text:p>x</text:p></table:table-cell></table:table-row><table:table-row table:number-rows-repeated="1048570"><table:table-cell table:number-columns-repeated="16384"/></table:table-row></table:table>` +
		`<table:table table:name="Hidden" table:style-name="ta2"><table:table-row><table:table-cell><text:p>secret</text:p></table:table-cell></table:table-row></table:table>` +
		`<table:table table:name="Two"><table:table-row><table:table-cell><text:p>y</text:p></table:table-cell></table:table-row></table:table>` +
		`</office:spreadsheet></office:body></office:document-content>`})
}

func odpFixture(t *testing.T) []byte {
	return zipOf(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.presentation", "content.xml": `<office:document-content ` + odfNS + `><office:body><office:presentation><draw:page draw:name="p1">` +
		`<draw:frame presentation:class="title"><draw:text-box><text:p>Slide One</text:p></draw:text-box></draw:frame>` +
		`<draw:frame presentation:class="outline"><draw:text-box><text:list><text:list-item><text:p>Point</text:p></text:list-item></text:list></draw:text-box></draw:frame>` +
		`<presentation:notes><draw:frame presentation:class="notes"><draw:text-box><text:p>Say hi</text:p></draw:text-box></draw:frame></presentation:notes>` +
		`</draw:page></office:presentation></office:body></office:document-content>`})
}

func epubFixture(t *testing.T) []byte {
	return zipOf(t, map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf"/></rootfiles></container>`,
		"OEBPS/content.opf":      `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="c1" href="text/ch%201.xhtml" media-type="application/xhtml+xml"/><item id="css" href="s.css" media-type="text/css"/></manifest><spine><itemref idref="c1"/><itemref idref="css"/></spine></package>`,
		"OEBPS/text/ch 1.xhtml":  `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>T</title><style>p{}</style></head><body><h1>Chapter&nbsp;One</h1><p>First <em>para</em>graph.<br/>Next line</p><ul><li>Alpha</li><li>Beta<ul><li>Inner</li></ul></li></ul><blockquote><p>Quoted</p></blockquote><table><tr><th>K</th><th>V</th></tr><tr><td>a</td><td>1</td></tr></table><pre>code  block</pre></body></html>`,
	})
}

func rtfFixture() []byte {
	return []byte(`{\rtf1\ansi\ansicpg1252\deff0{\fonttbl{\f0\fswiss Helvetica;}{\f1\fcharset128 MS Mincho;}}{\stylesheet{\s1\outlinelevel0 heading 1;}}` +
		`\pard\s1 Title\par\pard Caf\'e9 舒? dash{\footnote\pard Foot text}\par{\f1 \'82\'a0}\par` +
		`\trowd\pard\intbl A\cell B\cell\row\pard After\line two\par{\*\generator x;}{\pict 0102}}`)
}

const (
	cfbFreeSect   = 0xFFFFFFFF
	cfbEndOfChain = 0xFFFFFFFE
	cfbNoStream   = 0xFFFFFFFF
)

var oleMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

type cfbStream = struct {
	name string
	data []byte
}

// Builds a version 3 compound file, streams padded past the mini stream cutoff and entries in directory order
func buildTestCFB(streams []cfbStream) []byte {
	const ss = 512
	streams = append([]cfbStream{}, streams...)
	for i := range streams {
		if len(streams[i].data) < 4096 {
			streams[i].data = append(append([]byte{}, streams[i].data...), make([]byte, 4096-len(streams[i].data))...)
		}
	}
	sort.SliceStable(streams, func(i, j int) bool {
		a, b := strings.ToUpper(streams[i].name), strings.ToUpper(streams[j].name)
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	entries := len(streams) + 1
	dirSectors := (entries + 3) / 4
	var fat []uint32
	fat = append(fat, 0xFFFFFFFD)
	for i := 0; i < dirSectors; i++ {
		if i == dirSectors-1 {
			fat = append(fat, cfbEndOfChain)
		} else {
			fat = append(fat, uint32(len(fat)+1))
		}
	}
	starts := make([]uint32, len(streams))
	var body []byte
	for i, s := range streams {
		n := (len(s.data) + ss - 1) / ss
		starts[i] = uint32(len(fat))
		for k := 0; k < n; k++ {
			if k == n-1 {
				fat = append(fat, cfbEndOfChain)
			} else {
				fat = append(fat, uint32(len(fat)+1))
			}
		}
		chunk := make([]byte, n*ss)
		copy(chunk, s.data)
		body = append(body, chunk...)
	}
	header := make([]byte, ss)
	copy(header, oleMagic)
	binary.LittleEndian.PutUint16(header[24:], 0x3E)
	binary.LittleEndian.PutUint16(header[26:], 3)
	binary.LittleEndian.PutUint16(header[28:], 0xFFFE)
	binary.LittleEndian.PutUint16(header[30:], 9)
	binary.LittleEndian.PutUint16(header[32:], 6)
	binary.LittleEndian.PutUint32(header[44:], 1)
	binary.LittleEndian.PutUint32(header[48:], 1)
	binary.LittleEndian.PutUint32(header[56:], 4096)
	binary.LittleEndian.PutUint32(header[60:], cfbEndOfChain)
	binary.LittleEndian.PutUint32(header[68:], cfbEndOfChain)
	for i := 0; i < 109; i++ {
		binary.LittleEndian.PutUint32(header[76+4*i:], cfbFreeSect)
	}
	binary.LittleEndian.PutUint32(header[76:], 0)
	fatSector := make([]byte, ss)
	for i := range fatSector {
		fatSector[i] = 0xFF
	}
	for i, v := range fat {
		binary.LittleEndian.PutUint32(fatSector[4*i:], v)
	}
	dir := make([]byte, dirSectors*ss)
	entry := func(i int, name string, typ byte, child, right, start uint32, size int) {
		e := dir[i*128 : (i+1)*128]
		units := utf16.Encode([]rune(name))
		for k, u := range units {
			binary.LittleEndian.PutUint16(e[2*k:], u)
		}
		binary.LittleEndian.PutUint16(e[64:], uint16(2*len(units)+2))
		e[66] = typ
		binary.LittleEndian.PutUint32(e[68:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[72:], right)
		binary.LittleEndian.PutUint32(e[76:], child)
		binary.LittleEndian.PutUint32(e[116:], start)
		binary.LittleEndian.PutUint32(e[120:], uint32(size))
	}
	entry(0, "Root Entry", 5, 1, cfbNoStream, cfbEndOfChain, 0)
	for i, s := range streams {
		right := uint32(cfbNoStream)
		if i+1 < len(streams) {
			right = uint32(i + 2)
		}
		entry(i+1, s.name, 2, cfbNoStream, right, starts[i], len(s.data))
	}
	out := append(header, fatSector...)
	out = append(out, dir...)
	return append(out, body...)
}

// Word 97 file with one heading paragraph and a hyperlink field, flagged encrypted on request
func docFixture(encrypted bool) []byte {
	text := "Doc heading line\rSecond paragraph \x13 HYPERLINK \"x\" \x14link text\x15 end\r"
	wd := make([]byte, 0x900)
	binary.LittleEndian.PutUint16(wd[0:], 0xA5EC)
	binary.LittleEndian.PutUint16(wd[2:], 0xC1)
	binary.LittleEndian.PutUint16(wd[6:], 0x0409)
	flags := uint16(0x0200)
	if encrypted {
		flags = 0x0300
	}
	binary.LittleEndian.PutUint16(wd[0x0A:], flags)
	binary.LittleEndian.PutUint32(wd[0x4C:], uint32(len(text)))
	copy(wd[0x800:], text)
	var clx []byte
	clx = append(clx, 0x02)
	plc := make([]byte, 16)
	binary.LittleEndian.PutUint32(plc[4:], uint32(len(text)))
	binary.LittleEndian.PutUint32(plc[10:], uint32(0x800*2)|0x40000000)
	clx = append(clx, 16, 0, 0, 0)
	clx = append(clx, plc...)
	binary.LittleEndian.PutUint32(wd[0x1A2:], 0)
	binary.LittleEndian.PutUint32(wd[0x1A6:], uint32(len(clx)))
	return buildTestCFB([]cfbStream{{"WordDocument", wd}, {"1Table", clx}})
}

func biffRec(typ uint16, body []byte) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint16(out, typ)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(body)))
	return append(out, body...)
}

// BIFF8 workbook with shared strings split across a CONTINUE record, a number, an RK, a bool, and a formula
func xlsFixture() []byte {
	le16b := func(v uint16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, v); return b }
	le32b := func(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
	bof := func(dt uint16) []byte {
		return biffRec(0x0809, append(append(le16b(0x0600), le16b(dt)...), make([]byte, 12)...))
	}
	cell := func(r, c uint16) []byte { return append(append(le16b(r), le16b(c)...), le16b(0)...) }
	sst := append(append(le32b(2), le32b(2)...), append(append(le16b(5), 0), "Hello"...)...)
	sst = append(sst, append(le16b(2), 1)...)
	sst = append(sst, 'W', 0)
	cont := append([]byte{1}, 0xF6, 0)
	globalsTail := append(append(biffRec(0x00FC, sst), biffRec(0x003C, cont)...), biffRec(0x000A, nil)...)
	sheetName := append([]byte{5, 0}, "Sheet"...)
	boundLen := len(biffRec(0x0085, append(make([]byte, 6), sheetName...)))
	sheetOffset := len(bof(5)) + boundLen + len(globalsTail)
	bound := biffRec(0x0085, append(append(le32b(uint32(sheetOffset)), 0, 0), sheetName...))
	num := make([]byte, 8)
	binary.LittleEndian.PutUint64(num, math.Float64bits(3.5))
	formula := append(cell(2, 1), 0, 0, 0, 0, 0, 0, 0xFF, 0xFF)
	formula = append(formula, make([]byte, 6)...)
	stream := append(append(bof(5), bound...), globalsTail...)
	stream = append(stream, bof(0x10)...)
	stream = append(stream, biffRec(0x00FD, append(cell(0, 0), le32b(0)...))...)
	stream = append(stream, biffRec(0x00FD, append(cell(0, 1), le32b(1)...))...)
	stream = append(stream, biffRec(0x0203, append(cell(1, 0), num...))...)
	stream = append(stream, biffRec(0x027E, append(cell(1, 1), le32b(7<<2|2)...))...)
	stream = append(stream, biffRec(0x0205, append(cell(2, 0), 1, 0))...)
	stream = append(stream, biffRec(0x0006, formula)...)
	stream = append(stream, biffRec(0x0207, append(append(le16b(4), 0), "calc"...))...)
	stream = append(stream, biffRec(0x000A, nil)...)
	return buildTestCFB([]cfbStream{{"Workbook", stream}})
}

func pptRec(ver, instance, typ uint16, body []byte) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint16(out, ver|instance<<4)
	binary.LittleEndian.PutUint16(out[2:], typ)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(body)))
	return append(out, body...)
}

// PowerPoint 97 file with one slide of title, body, and a free text box, locked on request
func pptFixture(locked bool) []byte {
	u32 := func(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
	utf16le := func(s string) []byte {
		var out []byte
		for _, u := range utf16.Encode([]rune(s)) {
			out = append(out, byte(u), byte(u>>8))
		}
		return out
	}
	persist := append(append(append(u32(2), u32(0)...), u32(2)...), append(u32(256), u32(0)...)...)
	slwt := pptRec(0xF, 0, 0x0FF0, bytes.Join([][]byte{
		pptRec(0, 0, 0x03F3, persist),
		pptRec(0, 0, 0x0F9F, u32(0)), pptRec(0, 0, 0x0FA0, utf16le("Slide Title")),
		pptRec(0, 0, 0x0F9F, u32(1)), pptRec(0, 0, 0x0FA8, []byte("Point one\rPoint two")),
	}, nil))
	doc := pptRec(0xF, 0, 0x03E8, slwt)
	textbox := func(inner []byte) []byte {
		return pptRec(0xF, 0, 0xF004, pptRec(0xF, 0, 0xF00D, inner))
	}
	drawing := pptRec(0xF, 0, 0x040C, pptRec(0xF, 0, 0xF002, pptRec(0xF, 0, 0xF003, bytes.Join([][]byte{
		textbox(pptRec(0, 0, 0x0F9E, u32(0))),
		textbox(pptRec(0, 0, 0x0F9E, u32(1))),
		textbox(append(pptRec(0, 0, 0x0F9F, u32(4)), pptRec(0, 0, 0x0FA0, utf16le("Free text box"))...)),
	}, nil))))
	slide := pptRec(0xF, 0, 0x03EE, append(pptRec(2, 0, 0x03EF, make([]byte, 24)), drawing...))
	slideOff := len(doc)
	dirOff := slideOff + len(slide)
	dir := pptRec(0, 0, 0x1772, append(u32(1|2<<20), append(u32(0), u32(uint32(slideOff))...)...))
	editOff := dirOff + len(dir)
	edit := pptRec(0, 0, 0x0FF5, bytes.Join([][]byte{u32(256), {0, 0, 0, 3}, u32(0), u32(uint32(dirOff)), u32(1), u32(3), {1, 0, 0, 0}}, nil))
	stream := bytes.Join([][]byte{doc, slide, dir, edit}, nil)
	token := uint32(0xE391C05F)
	if locked {
		token = 0xF3D1C4DF
	}
	current := pptRec(0, 0, 0x0FF6, bytes.Join([][]byte{u32(0x14), u32(token), u32(uint32(editOff)), make([]byte, 8)}, nil))
	return buildTestCFB([]cfbStream{{"Current User", current}, {"PowerPoint Document", stream}})
}

// Binary workbook with one sheet of a shared string, a number, and an inline string
func xlsbFixture(t *testing.T) []byte {
	wide := func(s string) []byte {
		units := utf16.Encode([]rune(s))
		out := make([]byte, 4, 4+2*len(units))
		binary.LittleEndian.PutUint32(out, uint32(len(units)))
		for _, u := range units {
			out = append(out, byte(u), byte(u>>8))
		}
		return out
	}
	rec := func(typ int, body []byte) []byte {
		var out []byte
		if typ >= 128 {
			out = append(out, byte(typ&0x7F)|0x80, byte(typ>>7))
		} else {
			out = append(out, byte(typ))
		}
		out = append(out, byte(len(body)))
		return append(out, body...)
	}
	cellHead := func(col uint32) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint32(b, col); return b }
	rowHdr := func(r uint32) []byte { b := make([]byte, 17); binary.LittleEndian.PutUint32(b, r); return rec(0, b) }
	wb := append(rec(153, append(make([]byte, 8), wide("")...)), rec(156, append(append(make([]byte, 8), wide("rId1")...), wide("Bin")...))...)
	sst := rec(19, append([]byte{0}, wide("shared")...))
	real := make([]byte, 8)
	binary.LittleEndian.PutUint64(real, math.Float64bits(1.25))
	sheet := bytes.Join([][]byte{rowHdr(0), rec(7, append(cellHead(0), 0, 0, 0, 0)), rec(5, append(cellHead(1), real...)), rowHdr(1), rec(6, append(cellHead(0), wide("inline")...))}, nil)
	return zipOf(t, map[string]string{
		"_rels/.rels":                `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="r" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.bin"/></Relationships>`,
		"xl/workbook.bin":            string(wb),
		"xl/_rels/workbook.bin.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.bin"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.bin"/></Relationships>`,
		"xl/sharedStrings.bin":       string(sst),
		"xl/worksheets/sheet1.bin":   string(sheet),
	})
}

func docmFixture(t *testing.T) []byte {
	return zipOf(t, map[string]string{"word/document.xml": loadReadExtractGolden(t).Fixtures.DocxXML})
}

func xlsmFixture(t *testing.T) []byte {
	return zipOf(t, loadReadExtractGolden(t).Fixtures.XlsxParts)
}

// Documents rendered by anydoc, expected output captured from the firecrawl-anydoc 0.2.4 Python binding
var anydocGoldenCases = []struct {
	name  string
	file  string
	build func(t *testing.T) []byte
	// bytesErr is the error the content detection only bytes path reports when it differs from the path one
	bytesErr string
}{
	{name: "pdf-flate", file: "a.pdf", build: func(*testing.T) []byte { return flatePDF() }},
	{name: "pdf-broken-xref", file: "broken.pdf", build: func(*testing.T) []byte { return brokenXrefPDF() }},
	{name: "pdf-object-stream", file: "objstm.pdf", build: func(*testing.T) []byte { return objectStreamPDF() }},
	{name: "pdf-rc4", file: "rc4.pdf", build: func(*testing.T) []byte { return rc4PDF() }},
	{name: "pdf-aesv2", file: "aesv2.pdf", build: func(*testing.T) []byte { return aesV2PDF() }},
	{name: "pdf-aesv3", file: "aesv3.pdf", build: func(*testing.T) []byte { return aesV3PDF() }},
	{name: "pdf-locked", file: "locked.pdf", build: func(*testing.T) []byte { return lockedPDF() }},
	{name: "pdf-fonts", file: "fonts.pdf", build: func(*testing.T) []byte { return fontsPDF() }},
	{name: "pptx", file: "deck.pptx", build: pptxFixture},
	{name: "odt", file: "a.odt", build: odtFixture},
	{name: "ods", file: "a.ods", build: odsFixture},
	{name: "odp", file: "a.odp", build: odpFixture},
	{name: "epub", file: "a.epub", build: epubFixture},
	{name: "rtf", file: "a.rtf", build: func(*testing.T) []byte { return rtfFixture() }},
	{name: "rtf-as-doc", file: "really-rtf.doc", build: func(*testing.T) []byte { return rtfFixture() }},
	{name: "doc", file: "a.doc", build: func(*testing.T) []byte { return docFixture(false) }},
	{name: "doc-encrypted", file: "b.doc", build: func(*testing.T) []byte { return docFixture(true) }},
	{name: "xls", file: "a.xls", build: func(*testing.T) []byte { return xlsFixture() }},
	{name: "ppt", file: "a.ppt", build: func(*testing.T) []byte { return pptFixture(false) }},
	{name: "ppt-locked", file: "b.ppt", build: func(*testing.T) []byte { return pptFixture(true) }},
	{name: "xlsb", file: "a.xlsb", build: xlsbFixture},
	{name: "docm", file: "a.docm", build: docmFixture},
	{name: "xlsm", file: "a.xlsm", build: xlsmFixture},
	{name: "plain-odt", file: "x.odt", build: func(*testing.T) []byte { return []byte("plain") }, bytesErr: "UnsupportedError: unsupported input: unrecognized file content: name the format explicitly"},
}

// Expected markdown or error text for a golden case, exactly one of the two files exists
func anydocGolden(t *testing.T, name string) (string, string) {
	t.Helper()
	md, mdErr := os.ReadFile(filepath.Join("testdata", "anydoc", name+".md"))
	errText, errErr := os.ReadFile(filepath.Join("testdata", "anydoc", name+".err"))
	switch {
	case mdErr == nil && errErr != nil:
		return string(md), ""
	case errErr == nil && mdErr != nil:
		return "", strings.TrimRight(string(errText), "\n")
	}
	t.Fatalf("golden %s: want exactly one of .md or .err (%v, %v)", name, mdErr, errErr)
	return "", ""
}

func TestReadExtractAnydocGolden(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("FIRECRAWL_API_KEY", "")
	for _, c := range anydocGoldenCases {
		t.Run(c.name, func(t *testing.T) {
			data := c.build(t)
			want, wantErr := anydocGolden(t, c.name)
			got, err := ExtractDocumentText(context.Background(), nil, writeFixture(t, c.file, data))
			if got != want || extractionMessage(err) != wantErr {
				t.Errorf("path:\n got  %q %q\n want %q %q", got, extractionMessage(err), want, wantErr)
			}
			if c.bytesErr != "" {
				wantErr = c.bytesErr
			}
			got, err = ExtractDocumentBytes(context.Background(), nil, data, "/remote/"+c.file)
			if got != want || extractionMessage(err) != wantErr {
				t.Errorf("bytes:\n got  %q %q\n want %q %q", got, extractionMessage(err), want, wantErr)
			}
		})
	}
}

func TestReadExtractPDFNeedsOCR(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	p := writeFixture(t, "scan.pdf", scannedPDF())
	got, err := ExtractDocumentText(context.Background(), nil, p)
	if err != nil || got != needsOCRWarning(p, []int{1, 2}, "") {
		t.Fatalf("needs ocr: %q %v", got, err)
	}
	_, err = ExtractDocumentBytes(context.Background(), nil, scannedPDF(), "scan.pdf")
	if extractionMessage(err) != "NeedsOcrError: all 2 pages need OCR" {
		t.Fatalf("bytes needs ocr: %v", err)
	}
	if HostedOCRAvailable(nil) {
		t.Fatal("hosted OCR must be off without a key")
	}
}

func TestReadExtractPDFHostedOCR(t *testing.T) {
	data := scannedPDF()
	status := 200
	var gotOptions, gotFilename, gotType, gotAuth string
	var gotFile []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/parse" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Error(err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				break
			}
			body, _ := io.ReadAll(part)
			switch part.FormName() {
			case "options":
				gotOptions = string(body)
			case "file":
				gotFilename, gotType, gotFile = part.FileName(), part.Header.Get("Content-Type"), body
			}
		}
		w.WriteHeader(status)
		if status == 200 {
			io.WriteString(w, `{"success": true, "data": {"markdown": "# OCR text"}}`)
		} else {
			io.WriteString(w, `{"success": false, "error": "no credits"}`)
		}
	}))
	defer srv.Close()
	t.Setenv("FIRECRAWL_API_KEY", "fc-key")
	t.Setenv("FIRECRAWL_API_URL", srv.URL+"/")
	t.Setenv("PATH", t.TempDir())
	p := writeFixture(t, "scan.pdf", data)
	got, err := ExtractDocumentText(context.Background(), nil, p)
	if err != nil || got != "# OCR text\n" {
		t.Fatalf("hosted ocr: %q %v", got, err)
	}
	wantOptions := `{"parsers": [{"type": "pdf", "mode": "auto"}], "origin": "icebath@` + icebathVersion() + `"}`
	if gotOptions != wantOptions || gotFilename != "scan.pdf" || gotType != "application/pdf" || !bytes.Equal(gotFile, data) || gotAuth != "Bearer fc-key" {
		t.Fatalf("multipart shape: %q %q %q %v %q", gotOptions, gotFilename, gotType, bytes.Equal(gotFile, data), gotAuth)
	}
	status = 402
	got, err = ExtractDocumentText(context.Background(), nil, p)
	if err != nil || got != needsOCRWarning(p, []int{1, 2}, "HostedError: Firecrawl Parse is out of credits: no credits") {
		t.Fatalf("hosted failure: %q %v", got, err)
	}
}

func TestReadExtractPDFCoverageNote(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'Page one has plenty of text here\\fshort\\f\\fPage four also has lots of text\\f'\n"
	if err := os.WriteFile(filepath.Join(dir, "pdftotext"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	data := flatePDF()
	body, _ := anydocGolden(t, "pdf-flate")
	p := writeFixture(t, "doc.pdf", data)
	got, err := ExtractDocumentText(context.Background(), nil, p)
	if err != nil {
		t.Fatal(err)
	}
	wantHead := "[EXTRACTION COVERAGE WARNING: 2 of 4 pages in this PDF yielded no text. "
	wantGap := "  pages 2-3 (2 pages) — after \"Page one has plenty of text here\" (p1)\n"
	if !strings.HasPrefix(got, wantHead) || !strings.Contains(got, wantGap) || !strings.HasSuffix(got, "ranges.]\n"+body) || !strings.Contains(got, shQuote(p)) {
		t.Fatalf("coverage note:\n%s", got)
	}
	viaBytes, err := ExtractDocumentBytes(context.Background(), nil, data, "/remote/doc.pdf")
	if err != nil || !strings.Contains(viaBytes, "-l <last> /remote/doc.pdf $TMPDIR/page") {
		t.Fatalf("coverage note bytes path names the display path:\n%s", viaBytes)
	}
}

func TestReadExtractNativeErrors(t *testing.T) {
	_, err := ExtractDocumentText(context.Background(), nil, filepath.Join(t.TempDir(), "gone.pdf"))
	if !strings.Contains(extractionMessage(err), "gone.pdf") {
		t.Fatalf("missing file: %v", err)
	}
	_, err = ExtractDocumentText(context.Background(), nil, writeFixture(t, "e.odt", emptyODT(t)))
	if extractionMessage(err) != "Document contains no extractable text" {
		t.Fatalf("empty odt: %v", err)
	}
	_, err = ExtractDocumentBytes(context.Background(), nil, make([]byte, maxDocumentBytes+1), "big.pdf")
	if extractionMessage(err) != "Document too large to convert (52,428,801 bytes, limit is 52,428,800)" {
		t.Fatalf("size: %v", err)
	}
}
