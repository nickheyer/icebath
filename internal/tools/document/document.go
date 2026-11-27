// Package document renders document files as text: notebooks, Word, Excel and PowerPoint files old and new, OpenDocument, RTF,
// EPUB, PDF and SQLite databases
package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Largest document converted, in bytes
const MaxBytes = 50 << 20

// Failure the model reads: why a document could not be rendered as text
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func fail(format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

// Bytes that neither their content nor their name mark as a document the converter reads
var ErrNotDocument = errors.New("not a document anydoc reads")

// Leading bytes of the formats read by content: an OLE2 compound file (doc, xls, ppt), a zip package, RTF and SQLite
const (
	oleMagic    = "\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1"
	zipMagic    = "PK\x03\x04"
	rtfMagic    = `{\rtf`
	sqliteMagic = "SQLite format 3\x00"
)

// Whether the bytes or the name's extension mark a document Extract reads. The bytes may be only the leading ones of a file
func Is(data []byte, name string) bool {
	switch extension(name) {
	case ".ipynb", ".docx", ".xlsx", ".db", ".sqlite", ".sqlite3",
		".doc", ".docm", ".ppt", ".pps", ".pot", ".pptx", ".pptm", ".ppsx", ".ppsm",
		".xls", ".xlsm", ".xlsb", ".odt", ".ods", ".odp", ".rtf", ".epub", ".pdf":
		return true
	}
	return bytes.HasPrefix(data, []byte(oleMagic)) || bytes.HasPrefix(data, []byte(zipMagic)) || bytes.HasPrefix(data, []byte(rtfMagic)) ||
		bytes.HasPrefix(data, []byte(sqliteMagic)) || isPDF(data)
}

// Whether the first kilobyte holds a PDF header, where readers look for it
func isPDF(data []byte) bool {
	return bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-"))
}

// Lower case extension of the name's last element with its dot, none for a dotfile or a name ending in a dot
func extension(name string) string {
	base := path.Base(strings.TrimRight(filepath.ToSlash(name), "/"))
	if i := strings.LastIndexByte(base, '.'); i > 0 && i < len(base)-1 {
		return strings.ToLower(base[i:])
	}
	return ""
}

// Reads documents, keeping the converter it compiles once for every later call
type Reader struct {
	mu       sync.Mutex
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

func New() *Reader {
	return &Reader{}
}

// Text of the document as Markdown, the name giving its extension and the path shown in notes. Notebooks, docx, xlsx and SQLite
// databases have readers of their own, everything else goes through the converter, which tells the format from the content first.
// Parse reads a PDF without a text layer through a hosted service when not nil, tried before local OCR. A *Error says why the
// document could not be read, ErrNotDocument that it is no document at all
func (r *Reader) Extract(ctx context.Context, data []byte, name string, parse func(ctx context.Context, data []byte, filename string) (string, error)) (string, error) {
	if len(data) > MaxBytes {
		return "", fail("Document too large to convert (%s bytes, limit is %s)", strs.Thousands(len(data)), strs.Thousands(MaxBytes))
	}
	switch ext := extension(name); {
	case ext == ".ipynb":
		return notebook(data, name)
	case ext == ".docx":
		return docx(data)
	case ext == ".xlsx":
		return xlsx(data)
	case ext == ".db", ext == ".sqlite", ext == ".sqlite3", bytes.HasPrefix(data, []byte(sqliteMagic)):
		return sqlite(ctx, data)
	}
	return r.convert(ctx, data, name, parse)
}

// Document converted to Markdown by anydoc, a PDF whose pages lack a text layer going to OCR, a PDF's text headed by the coverage
// warning when many pages yielded none
func (r *Reader) convert(ctx context.Context, data []byte, name string, parse func(ctx context.Context, data []byte, filename string) (string, error)) (string, error) {
	text, err := r.markdown(ctx, data, strings.TrimPrefix(extension(name), "."))
	var failure *convertError
	switch {
	case errors.As(err, &failure) && failure.kind == "NeedsOcr":
		return scanned(ctx, data, name, failure.pages, parse)
	case errors.As(err, &failure) && failure.kind == "Unsupported":
		// Once the format is known the converter only says this of a PDF with no text on any page
		return scanned(ctx, data, name, nil, parse)
	case errors.As(err, &failure) && failure.kind == "Encrypted":
		return "", fail("the document is password protected, decrypt it with its password (a PDF through qpdf --password=PASSWORD " +
			"--decrypt in.pdf out.pdf, an Office file by saving it without a password) and read the decrypted copy")
	case errors.Is(err, ErrNotDocument):
		return "", fmt.Errorf("%s is %w", name, ErrNotDocument)
	case err != nil && ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return "", &Error{Message: err.Error()}
	}
	if strings.TrimSpace(text) == "" {
		return "", fail("Document contains no extractable text")
	}
	note := ""
	if isPDF(data) {
		note = coverage(ctx, data, name)
	}
	return note + strings.TrimRight(text, "\n") + "\n", nil
}

// Lines joined with one trailing newline, failing with the message when none holds more than space
func joined(lines []string, empty string) (string, error) {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n", nil
		}
	}
	return "", &Error{Message: empty}
}
