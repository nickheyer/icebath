package document

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

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// The anydoc 0.2.4 document converter behind a C ABI of three exports, built with cargo build --release --target wasm32-wasip1:
// anydoc_alloc(len) ptr, anydoc_dealloc(ptr, len), and anydoc_to_markdown(ptr, len, ext_ptr, ext_len, ext_is_fallback,
// status_ptr), which returns the result's pointer and length packed as ptr<<32 | len
//
//go:embed anydoc.wasm
var anydocWasm []byte

// Statuses anydoc_to_markdown writes: Markdown, an error payload, no format told by the content or the extension
const (
	statusConverted    = 0
	statusFailed       = 1
	statusUnrecognized = 2
)

// Failure the converter reported: its variant, its message, and for NeedsOcr the 1-indexed pages that have no text layer
type convertError struct {
	kind    string
	message string
	pages   []int
}

func (e *convertError) Error() string { return e.message }

// Compiled converter, compiled on first use with its native code cached under the user cache folder; a failed compile is tried
// again by the next call
func (r *Reader) module(ctx context.Context) (wazero.Runtime, wazero.CompiledModule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.compiled != nil {
		return r.runtime, r.compiled, nil
	}
	// Every later call shares what this one sets up, so the caller giving up must not undo it
	ctx = context.WithoutCancel(ctx)
	config := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	var cache wazero.CompilationCache
	if dir, err := os.UserCacheDir(); err == nil {
		if c, err := wazero.NewCompilationCacheWithDir(filepath.Join(dir, "icebath", "wazero")); err == nil {
			cache, config = c, config.WithCompilationCache(c)
		}
	}
	runtime := wazero.NewRuntimeWithConfig(ctx, config)
	compiled, err := compile(ctx, runtime)
	if err != nil {
		runtime.Close(ctx)
		if cache != nil {
			cache.Close(ctx)
		}
		return nil, nil, err
	}
	r.runtime, r.compiled = runtime, compiled
	return runtime, compiled, nil
}

func compile(ctx context.Context, runtime wazero.Runtime) (wazero.CompiledModule, error) {
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
		return nil, err
	}
	return runtime.CompileModule(ctx, anydocWasm)
}

// Markdown of the bytes from a fresh instance of the converter, the content telling the format and ext, without its dot, standing in
// when the content has no signature
func (r *Reader) markdown(ctx context.Context, data []byte, ext string) (string, error) {
	runtime, compiled, err := r.module(ctx)
	if err != nil {
		return "", fmt.Errorf("anydoc module failed to compile: %w", err)
	}
	var stderr bytes.Buffer
	mod, err := runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithStderr(&stderr))
	if err != nil {
		return "", fmt.Errorf("anydoc module failed to start: %w", err)
	}
	// Closing the instance frees everything it allocated
	defer mod.Close(ctx)
	alloc, convert := mod.ExportedFunction("anydoc_alloc"), mod.ExportedFunction("anydoc_to_markdown")
	if alloc == nil || convert == nil {
		return "", errors.New("anydoc module does not export the expected functions")
	}
	dataPtr, err := guestWrite(ctx, mod, alloc, data)
	if err != nil {
		return "", err
	}
	extPtr, err := guestWrite(ctx, mod, alloc, []byte(ext))
	if err != nil {
		return "", err
	}
	statusPtr, err := guestWrite(ctx, mod, alloc, make([]byte, 4))
	if err != nil {
		return "", err
	}
	results, err := convert.Call(ctx, uint64(dataPtr), uint64(len(data)), uint64(extPtr), uint64(len(ext)), 1, uint64(statusPtr))
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("anydoc conversion trapped: %w: %s", err, msg)
		}
		return "", fmt.Errorf("anydoc conversion trapped: %w", err)
	}
	outPtr, outLen := uint32(results[0]>>32), uint32(results[0])
	out := ""
	if outLen > 0 {
		text, ok := mod.Memory().Read(outPtr, outLen)
		if !ok {
			return "", errors.New("anydoc returned an unreadable result")
		}
		out = string(text)
	}
	status, ok := mod.Memory().ReadUint32Le(statusPtr)
	if !ok {
		return "", errors.New("anydoc returned an unreadable status")
	}
	switch status {
	case statusConverted:
		return out, nil
	case statusUnrecognized:
		return "", ErrNotDocument
	case statusFailed:
		return "", convertFailure(out)
	}
	return "", fmt.Errorf("anydoc returned unknown status %d", status)
}

// Failure of a NUL separated payload: the variant, the message, and for NeedsOcr the comma separated pages and the page count
func convertFailure(payload string) error {
	fields := strings.Split(payload, "\x00")
	if len(fields) < 2 {
		return fmt.Errorf("anydoc returned an unreadable error payload %q", payload)
	}
	failure := &convertError{kind: fields[0], message: fields[1]}
	if len(fields) == 4 {
		for _, page := range strings.Split(fields[2], ",") {
			if page == "" {
				continue
			}
			n, err := strconv.Atoi(page)
			if err != nil {
				return fmt.Errorf("anydoc returned an unreadable page list %q", fields[2])
			}
			failure.pages = append(failure.pages, n)
		}
	}
	return failure
}

// Copies bytes into guest memory, returning their pointer, zero for none
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
