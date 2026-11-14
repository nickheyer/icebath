package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
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
		_, err := pyjson.Loads(in)
		if err == nil || err.Error() != want {
			t.Errorf("pyjson.Loads(%q) = %v want %q", in, err, want)
		}
	}
	v, err := pyjson.Loads(`{"b": [1, 2.5, NaN, -Infinity, "x\ud83d\ude00"], "a": {"k": null}, "b": true}`)
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

func pdfStreamObj(dict string, data []byte) string {
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data)
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

const odfNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0"`

func emptyODT(t *testing.T) []byte {
	return zipOf(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text", "content.xml": `<office:document-content ` + odfNS + `><office:body><office:text/></office:body></office:document-content>`})
}

const ()

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
