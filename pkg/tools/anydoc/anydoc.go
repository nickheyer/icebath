// Package anydoc converts documents to Markdown through the embedded anydoc 0.2.4 WebAssembly build
package anydoc

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Module compiled from third_party/anydoc-wasm with cargo build --release --target wasm32-wasip1
//
//go:embed anydoc.wasm
var moduleBytes []byte

// ConvertError is a failure anydoc reported, Kind being the Rust variant name suffixed with Error
type ConvertError struct {
	Kind    string
	Message string
	// Pages lists the 1-indexed scanned pages when Kind is NeedsOcrError
	Pages []int
	// PageCount is the page total when Kind is NeedsOcrError
	PageCount int
}

func (e *ConvertError) Error() string { return e.Kind + ": " + e.Message }

// NeedsOCR reports whether the conversion failed because the document has no text layer
func (e *ConvertError) NeedsOCR() bool { return e.Kind == "NeedsOcrError" }

// Extensions lists the formats the converter accepts, without dots
var Extensions = []string{"doc", "docx", "docm", "odt", "pdf", "ppt", "pps", "pot", "pptx", "pptm", "ppsx", "ppsm", "rtf", "epub", "xlsx", "xlsm", "xlsb", "xls", "ods", "odp", "csv"}

const (
	statusOK       = 0
	statusError    = 1
	statusNoFormat = 2
)

var (
	runtimeOnce sync.Once
	runtime     wazero.Runtime
	compiled    wazero.CompiledModule
	compileErr  error
)

// Compiles the module once per process, caching native code under the user cache directory
func compile(ctx context.Context) error {
	runtimeOnce.Do(func() {
		config := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
		if dir, err := os.UserCacheDir(); err == nil {
			if cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(dir, "icebath", "wazero")); err == nil {
				config = config.WithCompilationCache(cache)
			}
		}
		runtime = wazero.NewRuntimeWithConfig(ctx, config)
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
			compileErr = err
			return
		}
		compiled, compileErr = runtime.CompileModule(ctx, moduleBytes)
	})
	return compileErr
}

// ToMarkdown converts a file's bytes, detecting the format from content with the path extension as fallback
func ToMarkdown(ctx context.Context, data []byte, path string) (string, error) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	out, err := convert(ctx, data, ext, true)
	if errors.Is(err, errNoFormat) {
		return "", &ConvertError{Kind: "UnsupportedError", Message: "unsupported input: unrecognized file content and extension: " + path}
	}
	return out, err
}

// ToMarkdownBytes converts in memory bytes, format naming the parser or empty to detect it from content
func ToMarkdownBytes(ctx context.Context, data []byte, format string) (string, error) {
	format = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".")
	out, err := convert(ctx, data, format, false)
	if errors.Is(err, errNoFormat) {
		if format != "" {
			return "", &ConvertError{Kind: "UnsupportedError", Message: "unsupported input: unrecognized format: " + format}
		}
		return "", &ConvertError{Kind: "UnsupportedError", Message: "unsupported input: unrecognized file content: name the format explicitly"}
	}
	return out, err
}

var errNoFormat = errors.New("no format resolved")

// Runs one conversion in a fresh module instance
func convert(ctx context.Context, data []byte, ext string, extIsFallback bool) (string, error) {
	if err := compile(ctx); err != nil {
		return "", fmt.Errorf("anydoc module failed to compile: %w", err)
	}
	var stderr bytes.Buffer
	mod, err := runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithStderr(&stderr))
	if err != nil {
		return "", fmt.Errorf("anydoc module failed to start: %w", err)
	}
	defer mod.Close(ctx)
	alloc, dealloc, call := mod.ExportedFunction("anydoc_alloc"), mod.ExportedFunction("anydoc_dealloc"), mod.ExportedFunction("anydoc_to_markdown")
	if alloc == nil || dealloc == nil || call == nil {
		return "", errors.New("anydoc module does not export the expected functions")
	}
	dataPtr, err := guestWrite(ctx, mod, alloc, data)
	if err != nil {
		return "", err
	}
	defer dealloc.Call(ctx, uint64(dataPtr), uint64(len(data)))
	extPtr, err := guestWrite(ctx, mod, alloc, []byte(ext))
	if err != nil {
		return "", err
	}
	defer dealloc.Call(ctx, uint64(extPtr), uint64(len(ext)))
	statusPtr, err := guestWrite(ctx, mod, alloc, make([]byte, 4))
	if err != nil {
		return "", err
	}
	defer dealloc.Call(ctx, uint64(statusPtr), 4)
	fallback := uint64(0)
	if extIsFallback {
		fallback = 1
	}
	results, err := call.Call(ctx, uint64(dataPtr), uint64(len(data)), uint64(extPtr), uint64(len(ext)), fallback, uint64(statusPtr))
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("anydoc conversion trapped: %w: %s", err, msg)
		}
		return "", fmt.Errorf("anydoc conversion trapped: %w", err)
	}
	packed := results[0]
	outPtr, outLen := uint32(packed>>32), uint32(packed&0xFFFFFFFF)
	out := ""
	if outLen > 0 {
		text, ok := mod.Memory().Read(outPtr, outLen)
		if !ok {
			return "", errors.New("anydoc returned an unreadable result")
		}
		out = string(text)
		dealloc.Call(ctx, uint64(outPtr), uint64(outLen))
	}
	status, ok := mod.Memory().ReadUint32Le(statusPtr)
	if !ok {
		return "", errors.New("anydoc returned an unreadable status")
	}
	switch status {
	case statusOK:
		return out, nil
	case statusNoFormat:
		return "", errNoFormat
	case statusError:
		return "", parseError(out)
	}
	return "", fmt.Errorf("anydoc returned unknown status %d", status)
}

// Decodes the NUL separated variant, message, pages, and page count payload
func parseError(payload string) error {
	fields := strings.Split(payload, "\x00")
	if len(fields) < 2 {
		return fmt.Errorf("anydoc returned an unreadable error payload %q", payload)
	}
	ce := &ConvertError{Kind: fields[0] + "Error", Message: fields[1]}
	if len(fields) == 4 {
		for _, p := range strings.Split(fields[2], ",") {
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return fmt.Errorf("anydoc returned an unreadable page list %q", fields[2])
			}
			ce.Pages = append(ce.Pages, n)
		}
		n, err := strconv.Atoi(fields[3])
		if err != nil {
			return fmt.Errorf("anydoc returned an unreadable page count %q", fields[3])
		}
		ce.PageCount = n
	}
	return ce
}

// Copies bytes into guest memory and returns the pointer
func guestWrite(ctx context.Context, mod api.Module, alloc api.Function, data []byte) (uint32, error) {
	if len(data) == 0 {
		return 0, nil
	}
	res, err := alloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("anydoc allocation failed: %w", err)
	}
	ptr := uint32(res[0])
	if ptr == 0 || !mod.Memory().Write(ptr, data) {
		return 0, errors.New("anydoc allocation failed")
	}
	return ptr, nil
}
