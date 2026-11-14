package docconv

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed anydoc.wasm
var moduleBytes []byte

const (
	retryAfter        = 300 * time.Second
	memoryLimitPages  = 32768
	conversionTimeout = 3 * time.Minute

	statusOK       = 0
	statusError    = 1
	statusNoFormat = 2

	pdfEmptyPageChars        = 20
	pdfCoverageMinEmpty      = 2
	pdfCoverageMinRatio      = 0.2
	pdfCoverageAbsoluteEmpty = 10
	pdfPageScanTimeout       = 20 * time.Second
	pdfGapMapMaxEntries      = 20
	gapContextChars          = 60
)

type convertFailure struct {
	kind    string
	message string
	pages   []int
}

func (e *convertFailure) Error() string { return e.kind + ": " + e.message }

func (c *Converter) module(ctx context.Context) (wazero.Runtime, wazero.CompiledModule, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.compiled != nil {
		return c.runtime, c.compiled, nil
	}
	if c.failure != nil && time.Since(c.failedAt) < retryAfter {
		return nil, nil, c.failure
	}
	bg := context.WithoutCancel(ctx)
	rt := wazero.NewRuntimeWithConfig(bg, wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(memoryLimitPages))
	if _, err := wasi_snapshot_preview1.Instantiate(bg, rt); err != nil {
		_ = rt.Close(bg)
		return c.fail(err)
	}
	compiled, err := rt.CompileModule(bg, moduleBytes)
	if err != nil {
		_ = rt.Close(bg)
		return c.fail(err)
	}
	c.runtime, c.compiled, c.failure = rt, compiled, nil
	return rt, compiled, nil
}

func (c *Converter) fail(err error) (wazero.Runtime, wazero.CompiledModule, error) {
	c.failedAt, c.failure = time.Now(), err
	return nil, nil, err
}

func guestWrite(ctx context.Context, mod api.Module, alloc api.Function, data []byte, nonNull bool) (uint32, error) {
	if len(data) == 0 && !nonNull {
		return 0, nil
	}
	res, err := alloc.Call(ctx, uint64(max(len(data), 1)))
	if err != nil {
		return 0, fmt.Errorf("anydoc allocation failed: %w", err)
	}
	ptr := uint32(res[0])
	if ptr == 0 || (len(data) > 0 && !mod.Memory().Write(ptr, data)) {
		return 0, errors.New("anydoc allocation failed")
	}
	return ptr, nil
}

func parseFailure(payload string) error {
	fields := strings.Split(payload, "\x00")
	if len(fields) < 2 {
		return fmt.Errorf("anydoc returned an unreadable error payload %q", payload)
	}
	f := &convertFailure{kind: fields[0] + "Error", message: fields[1]}
	if len(fields) == 4 {
		for _, p := range strings.Split(fields[2], ",") {
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return fmt.Errorf("anydoc returned an unreadable page list %q", fields[2])
			}
			f.pages = append(f.pages, n)
		}
	}
	return f
}

func (c *Converter) convert(ctx context.Context, data []byte, ext string, extIsFallback bool) (string, bool, error) {
	rt, compiled, err := c.module(ctx)
	if err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(ctx, conversionTimeout)
	defer cancel()
	var stderr bytes.Buffer
	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithStderr(&stderr))
	if err != nil {
		return "", false, fmt.Errorf("anydoc module failed to start: %w", err)
	}
	defer mod.Close(context.WithoutCancel(ctx))
	alloc, call := mod.ExportedFunction("anydoc_alloc"), mod.ExportedFunction("anydoc_to_markdown")
	if alloc == nil || call == nil {
		return "", false, errors.New("anydoc module does not export the expected functions")
	}
	dataPtr, err := guestWrite(ctx, mod, alloc, data, true)
	if err != nil {
		return "", false, err
	}
	extPtr, err := guestWrite(ctx, mod, alloc, []byte(ext), false)
	if err != nil {
		return "", false, err
	}
	statusPtr, err := guestWrite(ctx, mod, alloc, make([]byte, 4), true)
	if err != nil {
		return "", false, err
	}
	fallback := uint64(0)
	if extIsFallback {
		fallback = 1
	}
	res, err := call.Call(ctx, uint64(dataPtr), uint64(len(data)), uint64(extPtr), uint64(len(ext)), fallback, uint64(statusPtr))
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", false, fmt.Errorf("anydoc conversion trapped: %w: %s", err, msg)
		}
		return "", false, fmt.Errorf("anydoc conversion trapped: %w", err)
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0])
	var out []byte
	if outLen > 0 {
		view, ok := mod.Memory().Read(outPtr, outLen)
		if !ok {
			return "", false, errors.New("anydoc returned an unreadable result")
		}
		out = bytes.Clone(view)
	}
	status, ok := mod.Memory().ReadUint32Le(statusPtr)
	if !ok {
		return "", false, errors.New("anydoc returned an unreadable status")
	}
	switch status {
	case statusOK:
		return string(out), true, nil
	case statusNoFormat:
		return "", false, nil
	case statusError:
		return "", true, parseFailure(string(out))
	}
	return "", false, fmt.Errorf("anydoc returned unknown status %d", status)
}

func missingConverter(path string, err error) error {
	return extractErr("Cannot convert %q: this format needs the embedded anydoc converter, which failed to load (%v; retried every 5 minutes). "+
		"Convert the file yourself via terminal (e.g. libreoffice --headless --convert-to txt).", path, err)
}

func finalize(text, path string, note func() string) (string, error) {
	if strip(text) == "" {
		return "", extractErr("Document contains no extractable text")
	}
	prefix := ""
	if suffix(path) == ".pdf" {
		prefix = note()
	}
	return prefix + strings.TrimRight(text, "\n") + "\n", nil
}

func (c *Converter) extractConverter(ctx context.Context, path string) (string, error) {
	if _, _, err := c.module(ctx); err != nil {
		return "", missingConverter(path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", &ExtractError{Msg: err.Error(), Err: err}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", &ExtractError{Msg: err.Error(), Err: err}
	}
	if err := checkSize(info.Size(), MaxConverterBytes); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxConverterBytes+1))
	if err != nil {
		return "", &ExtractError{Msg: err.Error(), Err: err}
	}
	if err := checkSize(int64(len(data)), MaxConverterBytes); err != nil {
		return "", err
	}
	text, found, err := c.convert(ctx, data, strings.TrimPrefix(suffix(path), "."), true)
	if err != nil {
		var failure *convertFailure
		if errors.As(err, &failure) && failure.kind == "NeedsOcrError" {
			return c.ocrScannedPDF(ctx, path, data, failure), nil
		}
		return "", &ExtractError{Msg: err.Error(), Err: err}
	}
	if !found {
		return "", extractErr("UnsupportedError: unsupported input: unrecognized file content and extension: %s", path)
	}
	return finalize(text, path, func() string { return pdfCoverageNote(ctx, path, path) })
}

func (c *Converter) extractConverterBytes(ctx context.Context, data []byte, path string) (string, error) {
	if _, _, err := c.module(ctx); err != nil {
		return "", missingConverter(path, err)
	}
	if err := checkSize(int64(len(data)), MaxConverterBytes); err != nil {
		return "", err
	}
	text, found, err := c.convert(ctx, data, "", false)
	if err != nil {
		return "", &ExtractError{Msg: err.Error(), Err: err}
	}
	if !found {
		return "", extractErr("UnsupportedError: unsupported input: unrecognized file content: name the format explicitly")
	}
	return finalize(text, path, func() string {
		note, err := withTempCopy(data, ".pdf", func(temp string) (string, error) { return pdfCoverageNote(ctx, temp, path), nil })
		if err != nil {
			return ""
		}
		return note
	})
}

func (c *Converter) ocrScannedPDF(ctx context.Context, path string, data []byte, failure *convertFailure) string {
	hostedError := ""
	if c.ocr.Enabled(ctx) {
		text, err := c.ocr.Markdown(ctx, path, data)
		if err == nil {
			return strings.TrimRight(text, "\n") + "\n"
		}
		hostedError = err.Error()
	}
	pageList := "unknown"
	if len(failure.pages) > 0 {
		parts := make([]string, len(failure.pages))
		for i, p := range failure.pages {
			parts[i] = strconv.Itoa(p)
		}
		pageList = strings.Join(parts, ", ")
	}
	hosted := ""
	if hostedError != "" {
		hosted = "Hosted OCR was attempted and failed (" + hostedError + "). "
	}
	return "[NEEDS OCR: pages " + pageList + " of this PDF are scanned images " +
		"with no text layer — their content is MISSING below. " + hosted +
		"If the missing pages matter: render just those pages with " +
		"`pdftoppm -jpeg -r 150 -f <first> -l <last> " + shellQuote(path) + " $TMPDIR/page` " +
		"and inspect via vision_analyze, or check whether an OCR skill is " +
		"available (skills_list).]\n"
}

func pdfPageTexts(ctx context.Context, path string) []string {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pdfPageScanTimeout)
	defer cancel()
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, path, "-")
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil
	}
	out := ""
	if err == nil {
		out = strings.ToValidUTF8(stdout.String(), "�")
	}
	if out == "" {
		return nil
	}
	pages := strings.Split(out, "\f")
	if strip(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	if len(pages) == 0 {
		return nil
	}
	return pages
}

func gapMap(counts []int, texts []string, empty []int) string {
	type span struct{ a, b int }
	var ranges []span
	for i, p := range empty {
		if i > 0 && p == empty[i-1]+1 {
			ranges[len(ranges)-1].b = p
			continue
		}
		ranges = append(ranges, span{p, p})
	}
	var lines []string
	for _, r := range ranges[:min(len(ranges), pdfGapMapMaxEntries)] {
		label := ""
		for prev := r.a - 2; prev >= 0; prev-- {
			if counts[prev] >= pdfEmptyPageChars {
				snippet := strings.Join(strings.FieldsFunc(texts[prev], isSpace), " ")
				snippet = snippet[:runeOffset(snippet, gapContextChars)]
				label = ` — after "` + snippet + `" (p` + strconv.Itoa(prev+1) + ")"
				break
			}
		}
		text := "page " + strconv.Itoa(r.a)
		if r.a != r.b {
			text = "pages " + strconv.Itoa(r.a) + "-" + strconv.Itoa(r.b)
		}
		n := r.b - r.a + 1
		plural := "s"
		if n == 1 {
			plural = ""
		}
		lines = append(lines, "  "+text+" ("+strconv.Itoa(n)+" page"+plural+")"+label)
	}
	if len(ranges) > pdfGapMapMaxEntries {
		rest := ranges[pdfGapMapMaxEntries:]
		pages := 0
		for _, r := range rest {
			pages += r.b - r.a + 1
		}
		lines = append(lines, "  … "+strconv.Itoa(len(rest))+" more gaps ("+strconv.Itoa(pages)+" pages)")
	}
	return strings.Join(lines, "\n")
}

func pdfCoverageNote(ctx context.Context, path, display string) string {
	texts := pdfPageTexts(ctx, path)
	if len(texts) < 2 {
		return ""
	}
	counts := make([]int, len(texts))
	var empty []int
	for i, page := range texts {
		counts[i] = utf8.RuneCountInString(strip(page))
		if counts[i] < pdfEmptyPageChars {
			empty = append(empty, i+1)
		}
	}
	total, nEmpty := len(counts), len(empty)
	enough := float64(nEmpty)/float64(total) >= pdfCoverageMinRatio || nEmpty >= pdfCoverageAbsoluteEmpty
	if nEmpty < pdfCoverageMinEmpty || !enough {
		return ""
	}
	return "[EXTRACTION COVERAGE WARNING: " +
		strconv.Itoa(nEmpty) + " of " + strconv.Itoa(total) + " pages in this PDF yielded no text. " +
		"Those pages are likely scanned images (or blank) — their content " +
		"is MISSING from the extracted text below, even where section " +
		"headers appear with empty bodies. Unreadable gaps, each labeled " +
		"with the last text extracted before it:\n" +
		gapMap(counts, texts, empty) + "\n" +
		"Decide which gaps you actually need — do NOT OCR or render " +
		"everything. For the gaps that matter, render just that range with " +
		"`pdftoppm -jpeg -r 150 -f <first> -l <last> " + shellQuote(display) + " $TMPDIR/page` " +
		"and inspect each image with the vision_analyze tool, or use the " +
		"ocr-and-documents skill (marker-pdf) for bulk OCR of large " +
		"ranges.]\n"
}
