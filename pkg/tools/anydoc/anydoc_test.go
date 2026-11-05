package anydoc

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Smallest WordprocessingML package anydoc recognizes from its content types
func minimalDocx(t *testing.T, bodyXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels":         `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":   `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + bodyXML + `</w:body></w:document>`,
	}
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml"} {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(parts[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCSVNamedFormat(t *testing.T) {
	out, err := ToMarkdownBytes(context.Background(), []byte("a,b\n1,2\n"), "csv")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "| a | b |") || !strings.Contains(out, "| 1 | 2 |") {
		t.Fatalf("unexpected csv markdown: %q", out)
	}
}

func TestDocxDetectedFromContent(t *testing.T) {
	data := minimalDocx(t, `<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Title</w:t></w:r></w:p><w:p><w:r><w:rPr><w:b/></w:rPr><w:t>bold</w:t></w:r><w:r><w:t xml:space="preserve"> plain</w:t></w:r></w:p>`)
	out, err := ToMarkdownBytes(context.Background(), data, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Title") || !strings.Contains(out, "**bold** plain") {
		t.Fatalf("unexpected docx markdown: %q", out)
	}
}

func TestUnrecognizedContent(t *testing.T) {
	_, err := ToMarkdownBytes(context.Background(), []byte("just some text"), "")
	var ce *ConvertError
	if !errors.As(err, &ce) || ce.Kind != "UnsupportedError" {
		t.Fatalf("want UnsupportedError, got %v", err)
	}
	if ce.Message != "unsupported input: unrecognized file content: name the format explicitly" {
		t.Fatalf("unexpected message %q", ce.Message)
	}
}

func TestUnknownNamedFormat(t *testing.T) {
	_, err := ToMarkdownBytes(context.Background(), []byte("x"), ".foo")
	var ce *ConvertError
	if !errors.As(err, &ce) || ce.Kind != "UnsupportedError" || ce.Message != "unsupported input: unrecognized format: foo" {
		t.Fatalf("want UnsupportedError, got %v", err)
	}
}

func TestPathDetectsContentBeforeExtension(t *testing.T) {
	data := minimalDocx(t, `<w:p><w:r><w:t>mislabeled</w:t></w:r></w:p>`)
	out, err := ToMarkdown(context.Background(), data, "/tmp/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mislabeled") {
		t.Fatalf("unexpected markdown: %q", out)
	}
}

func TestPathExtensionNamesSignaturelessFormat(t *testing.T) {
	out, err := ToMarkdown(context.Background(), []byte("x,y\n3,4\n"), "/data/points.CSV")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "| 3 | 4 |") {
		t.Fatalf("unexpected markdown: %q", out)
	}
}

func TestPathUnrecognized(t *testing.T) {
	_, err := ToMarkdown(context.Background(), []byte("plain"), "/data/notes.txt")
	var ce *ConvertError
	if !errors.As(err, &ce) || ce.Message != "unsupported input: unrecognized file content and extension: /data/notes.txt" {
		t.Fatalf("got %v", err)
	}
}

func TestScannedPDFReportsPages(t *testing.T) {
	data, err := os.ReadFile("testdata/scanned.pdf")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ToMarkdownBytes(context.Background(), data, "")
	var ce *ConvertError
	if !errors.As(err, &ce) || !ce.NeedsOCR() {
		t.Fatalf("want NeedsOcrError, got %v", err)
	}
	if ce.Message != "all 2 pages need OCR" || len(ce.Pages) != 2 || ce.Pages[0] != 1 || ce.Pages[1] != 2 || ce.PageCount != 2 {
		t.Fatalf("pages not carried: %+v", ce)
	}
}

func TestTruncatedZipIsMalformed(t *testing.T) {
	data := minimalDocx(t, `<w:p><w:r><w:t>x</w:t></w:r></w:p>`)
	_, err := ToMarkdownBytes(context.Background(), data[:len(data)-40], "docx")
	var ce *ConvertError
	if !errors.As(err, &ce) {
		t.Fatalf("want ConvertError, got %v", err)
	}
	t.Logf("kind=%s msg=%s", ce.Kind, ce.Message)
}
