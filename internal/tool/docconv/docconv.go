// Document to text conversion for the read tool, native for notebooks, Word, Excel and SQLite and through the embedded converter for the rest
package docconv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
)

const (
	MaxConverterBytes = 50 * 1024 * 1024
	MaxDocumentBytes  = 50 * 1024 * 1024
)

var nativeExtensions = map[string]bool{".ipynb": true, ".docx": true, ".xlsx": true, ".db": true, ".sqlite": true, ".sqlite3": true}

var converterExtensions = map[string]bool{
	".doc": true, ".docm": true, ".ppt": true, ".pps": true, ".pot": true, ".pptx": true, ".pptm": true, ".ppsx": true, ".ppsm": true,
	".xls": true, ".xlsm": true, ".xlsb": true, ".odt": true, ".ods": true, ".odp": true, ".rtf": true, ".epub": true, ".pdf": true,
}

type ExtractError struct {
	Msg string
	Err error
}

func (e *ExtractError) Error() string { return e.Msg }

func (e *ExtractError) Unwrap() error { return e.Err }

func extractErr(format string, args ...any) error {
	return &ExtractError{Msg: fmt.Sprintf(format, args...)}
}

type UnfilledError struct {
	Name string
}

func (e *UnfilledError) Error() string {
	return "document conversion needs a " + e.Name + " and none was provided"
}

type HostedOCR interface {
	Enabled(ctx context.Context) bool
	Markdown(ctx context.Context, path string, data []byte) (string, error)
}

type Converter struct {
	ocr HostedOCR

	mu       sync.Mutex
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	failedAt time.Time
	failure  error
}

func New(ocr HostedOCR) (*Converter, error) {
	if ocr == nil {
		return nil, &UnfilledError{Name: "hosted OCR route"}
	}
	return &Converter{ocr: ocr}, nil
}

func (c *Converter) HostedOCRAvailable(ctx context.Context) bool {
	return c.ocr.Enabled(ctx)
}

func suffix(path string) string {
	name := filepath.Base(path)
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i >= len(name)-1 {
		return ""
	}
	return lower(name[i:])
}

func (c *Converter) extension(ctx context.Context, path string) string {
	ext := suffix(path)
	if nativeExtensions[ext] {
		return ext
	}
	if converterExtensions[ext] {
		if _, _, err := c.module(ctx); err == nil {
			return ext
		}
	}
	return ""
}

func (c *Converter) Extractable(ctx context.Context, path string) bool {
	return c.extension(ctx, path) != ""
}

func checkSize(size, limit int64) error {
	if size > limit {
		return extractErr("Document too large to convert (%s bytes, limit is %s)", commas(size), commas(limit))
	}
	return nil
}

func (c *Converter) Extract(ctx context.Context, path string) (string, error) {
	switch ext := c.extension(ctx, path); {
	case ext == ".ipynb":
		data, err := os.ReadFile(path)
		if err != nil {
			return "", extractErr("Not a valid notebook: %v", err)
		}
		return extractNotebook(data, path)
	case ext == ".docx":
		zr, err := openZipPath(path, "DOCX")
		if err != nil {
			return "", err
		}
		defer zr.Close()
		return extractDocx(zr)
	case ext == ".xlsx":
		zr, err := openZipPath(path, "XLSX")
		if err != nil {
			return "", err
		}
		defer zr.Close()
		return extractXlsx(zr)
	case ext == ".db" || ext == ".sqlite" || ext == ".sqlite3":
		return extractSQLite(ctx, path)
	case converterExtensions[ext]:
		return c.extractConverter(ctx, path)
	}
	return "", extractErr("Unsupported document type: %q", path)
}

func (c *Converter) ExtractBytes(ctx context.Context, data []byte, path string) (string, error) {
	if err := checkSize(int64(len(data)), MaxDocumentBytes); err != nil {
		return "", err
	}
	ext := c.extension(ctx, path)
	switch {
	case converterExtensions[ext]:
		return c.extractConverterBytes(ctx, data, path)
	case ext == ".ipynb":
		return extractNotebook(data, path)
	case ext == ".docx":
		zr, err := openZipBytes(data, "DOCX")
		if err != nil {
			return "", err
		}
		defer zr.Close()
		return extractDocx(zr)
	case ext == ".xlsx":
		zr, err := openZipBytes(data, "XLSX")
		if err != nil {
			return "", err
		}
		defer zr.Close()
		return extractXlsx(zr)
	case nativeExtensions[ext]:
		return withTempCopy(data, ext, func(temp string) (string, error) { return extractSQLite(ctx, temp) })
	}
	return "", extractErr("Unsupported document type: %q", path)
}

func withTempCopy(data []byte, ext string, fn func(path string) (string, error)) (string, error) {
	f, err := os.CreateTemp("", "*"+ext)
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return fn(name)
}
