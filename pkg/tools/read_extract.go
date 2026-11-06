package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools/anydoc"
	_ "modernc.org/sqlite"
)

// Extensions with an in house extractor that is always authoritative
var extractableExtensions = setOf(".ipynb", ".docx", ".xlsx", ".db", ".sqlite", ".sqlite3")

// Extensions the embedded anydoc converter renders as Markdown
var nativeDocumentExtensions = setOf(
	".doc", ".docm", ".ppt", ".pps", ".pot", ".pptx", ".pptm", ".ppsx", ".ppsm",
	".xls", ".xlsm", ".xlsb", ".odt", ".ods", ".odp", ".rtf", ".epub", ".pdf")

const (
	maxNativeDocBytes   = 50 * 1024 * 1024
	maxDocumentBytes    = 50 * 1024 * 1024
	maxXlsxRowsPerSheet = 5000
	maxXlsxCols         = 256
	nsW                 = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsS                 = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsRel               = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsPkgRel            = "http://schemas.openxmlformats.org/package/2006/relationships"
)

// ExtractionError reports a supported looking document that cannot be rendered as text
type ExtractionError struct{ Msg string }

func (e *ExtractionError) Error() string { return e.Msg }

func extractionErr(format string, args ...any) error {
	return &ExtractionError{Msg: fmt.Sprintf(format, args...)}
}

// Final path component suffix with pathlib rules, empty for dotfiles and trailing dots
func pathSuffix(p string) string {
	name := path.Base(strings.TrimRight(filepath.ToSlash(p), "/"))
	i := strings.LastIndex(name, ".")
	if i > 0 && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

func documentExtension(p string) string {
	ext := strings.ToLower(pathSuffix(p))
	if extractableExtensions[ext] || nativeDocumentExtensions[ext] {
		return ext
	}
	return ""
}

// IsExtractableDocument reports whether read_file renders this path through a document extractor
func IsExtractableDocument(p string) bool { return documentExtension(p) != "" }

func checkDocumentSize(size int64, limit int) error {
	if size > int64(limit) {
		return extractionErr("Document too large to convert (%s bytes, limit is %s)", commaInt(int(size)), commaInt(limit))
	}
	return nil
}

// Writes backend bytes to a private host temp file and returns its path and remover
func tempDocumentCopy(data []byte, suffix string) (string, func(), error) {
	f, err := os.CreateTemp("", "*"+suffix)
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return name, cleanup, nil
}

// Extractors keyed by extension for the formats handled in house
var inHouseExtractors = map[string]func(p string) (string, error){
	".ipynb":   func(p string) (string, error) { return extractNotebook(p, p) },
	".docx":    extractDocx,
	".xlsx":    extractXlsx,
	".db":      extractSqlite,
	".sqlite":  extractSqlite,
	".sqlite3": extractSqlite,
}

// ExtractDocumentText renders a document file on the host as text
func ExtractDocumentText(ctx context.Context, env *Env, p string) (string, error) {
	ext := documentExtension(p)
	if fn, ok := inHouseExtractors[ext]; ok {
		return fn(p)
	}
	if nativeDocumentExtensions[ext] {
		return extractNativeDocument(ctx, env, p)
	}
	return "", extractionErr("Unsupported document type: %s", pyReprStr(p))
}

// ExtractDocumentBytes renders a document already fetched across a file backend boundary
func ExtractDocumentBytes(ctx context.Context, env *Env, data []byte, p string) (string, error) {
	if err := checkDocumentSize(int64(len(data)), maxDocumentBytes); err != nil {
		return "", err
	}
	ext := documentExtension(p)
	if nativeDocumentExtensions[ext] {
		return extractNativeDocumentBytes(ctx, env, data, p)
	}
	if !extractableExtensions[ext] {
		return "", extractionErr("Unsupported document type: %s", pyReprStr(p))
	}
	tmp, cleanup, err := tempDocumentCopy(data, ext)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if ext == ".ipynb" {
		return extractNotebook(tmp, p)
	}
	return inHouseExtractors[ext](tmp)
}

// Raw secret from the env layer and process environment
func rawSecret(env *Env, name string) string {
	if env != nil && env.Secrets != nil {
		return env.Secrets.String(name)
	}
	return os.Getenv(name)
}

// Hosted OCR enablement and key, a direct Firecrawl key being the only route
func hostedOCRConfig(env *Env) (bool, string) {
	apiKey := rawSecret(env, "FIRECRAWL_API_KEY")
	enabled := apiKey != ""
	if section := config.AsMap(cfgTree(env).Get("file_tools")); section != nil {
		if v, ok := section["hosted_ocr"].(bool); ok && !v {
			enabled = false
		}
	}
	return enabled, apiKey
}

// HostedOCRAvailable reports whether scanned PDF pages can go to Firecrawl Parse
func HostedOCRAvailable(env *Env) bool {
	enabled, _ := hostedOCRConfig(env)
	return enabled
}

// Needs OCR result naming the scanned pages and how to recover them
func needsOCRWarning(p string, pages []int, hostedError string) string {
	pageList := "unknown"
	if len(pages) > 0 {
		parts := make([]string, len(pages))
		for i, n := range pages {
			parts[i] = strconv.Itoa(n)
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
		"`pdftoppm -jpeg -r 150 -f <first> -l <last> " + shQuote(p) + " $TMPDIR/page` " +
		"and inspect via vision_analyze, or check whether an OCR skill is " +
		"available (skills_list).]\n"
}

// Normalizes converter output and prepends the coverage note for PDFs
func finalizeNativeText(text, p string, pdfNote func() string) (string, error) {
	if pyStrip(text) == "" {
		return "", extractionErr("Document contains no extractable text")
	}
	note := ""
	if strings.ToLower(pathSuffix(p)) == ".pdf" {
		note = pdfNote()
	}
	return note + strings.TrimRight(text, "\n") + "\n", nil
}

// Hosted OCR for a scanned PDF when a route exists, else the recovery warning
func ocrScannedPDF(ctx context.Context, env *Env, p string, data []byte, pages []int) string {
	enabled, apiKey := hostedOCRConfig(env)
	hostedError := ""
	if enabled {
		md, err := parseHostedPDF(ctx, env, data, filepath.Base(p), apiKey)
		if err == nil {
			return strings.TrimRight(md, "\n") + "\n"
		}
		hostedError = "HostedError: " + err.Error()
	}
	return needsOCRWarning(p, pages, hostedError)
}

// Firecrawl Parse request timeout
const hostedParseTimeout = 300 * time.Second

// Multipart body with the options part and the file part the Parse API expects
func hostedMultipart(boundary, options, filename string, data []byte) []byte {
	filename = strings.NewReplacer(`"`, "_", "\r", "_", "\n", "_").Replace(filename)
	var b bytes.Buffer
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Disposition: form-data; name=\"options\"\r\n\r\n")
	b.WriteString(options)
	b.WriteString("\r\n--" + boundary + "\r\n")
	b.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"" + filename + "\"\r\n")
	b.WriteString("Content-Type: application/pdf\r\n\r\n")
	b.Write(data)
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return b.Bytes()
}

// Parse failure text for the status, the API detail and whether a key was sent
func describeHostedFailure(status int, detail string, keyed bool) string {
	switch {
	case status == 401:
		return "Firecrawl Parse rejected the API key: " + detail
	case status == 402:
		return "Firecrawl Parse is out of credits: " + detail
	case status == 429 && keyed:
		return "Firecrawl Parse rate limit reached: " + detail
	case status == 429:
		return "Firecrawl Parse keyless limit reached, set FIRECRAWL_API_KEY: " + detail
	}
	return "Firecrawl Parse: " + detail
}

// Sends the whole PDF to Firecrawl Parse and returns its Markdown
func parseHostedPDF(ctx context.Context, env *Env, data []byte, filename, apiKey string) (string, error) {
	apiURL := rawSecret(env, "FIRECRAWL_API_URL")
	if apiURL == "" {
		apiURL = firecrawlCloudAPIURL
	}
	target := strings.TrimRight(apiURL, "/") + "/v2/parse"
	options := `{"parsers": [{"type": "pdf", "mode": "auto"}], "origin": ` + jsonCompact("icebath@"+icebathVersion()) + `}`
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	boundary := hex.EncodeToString(raw[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(hostedMultipart(boundary, options, filename, data)))
	if err != nil {
		return "", errors.New("Firecrawl Parse: " + err.Error())
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("User-Agent", icebathUserAgent())
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := webHTTPClient(hostedParseTimeout).Do(req)
	if err != nil {
		return "", errors.New("Firecrawl Parse: " + err.Error())
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", errors.New("Firecrawl Parse: " + err.Error())
	}
	reply := &pyjson.Object{Vals: map[string]any{}}
	if utf8.Valid(body) {
		if v, err := pyjson.Loads(strings.TrimPrefix(string(body), "\ufeff")); err == nil {
			if obj, ok := v.(*pyjson.Object); ok {
				reply = obj
			}
		}
	}
	success, _ := reply.Get("success")
	if resp.StatusCode != 200 || !pyTruthy(success) {
		detail := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if e, _ := reply.Get("error"); pyTruthy(e) {
			detail = pyJSONStr(e)
		}
		return "", errors.New(describeHostedFailure(resp.StatusCode, detail, apiKey != ""))
	}
	dataField, _ := reply.Get("data")
	markdown := ""
	if obj, ok := dataField.(*pyjson.Object); ok {
		if m, ok := obj.Vals["markdown"].(string); ok {
			markdown = m
		}
	}
	if markdown == "" {
		return "", errors.New("Firecrawl Parse returned no Markdown")
	}
	if !strings.HasSuffix(markdown, "\n") {
		markdown += "\n"
	}
	return markdown, nil
}

// Python truthiness of a decoded JSON value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case *big.Int:
		return x.Sign() != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return len(x.Keys) > 0
	}
	return true
}

// Converts a host document through anydoc, sending scanned PDFs to hosted OCR when allowed
func extractNativeDocument(ctx context.Context, env *Env, p string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", &ExtractionError{Msg: err.Error()}
	}
	if err := checkDocumentSize(fi.Size(), maxNativeDocBytes); err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", &ExtractionError{Msg: err.Error()}
	}
	text, err := anydoc.ToMarkdown(ctx, data, p)
	var ce *anydoc.ConvertError
	if errors.As(err, &ce) {
		if ce.NeedsOCR() {
			return ocrScannedPDF(ctx, env, p, data, ce.Pages), nil
		}
		return "", &ExtractionError{Msg: ce.Error()}
	}
	if err != nil {
		return "", &ExtractionError{Msg: err.Error()}
	}
	return finalizeNativeText(text, p, func() string { return pdfCoverageNote(ctx, p, "") })
}

// Converts backend document bytes through anydoc with content detection only
func extractNativeDocumentBytes(ctx context.Context, env *Env, data []byte, p string) (string, error) {
	if err := checkDocumentSize(int64(len(data)), maxNativeDocBytes); err != nil {
		return "", err
	}
	text, err := anydoc.ToMarkdownBytes(ctx, data, "")
	var ce *anydoc.ConvertError
	if errors.As(err, &ce) {
		return "", &ExtractionError{Msg: ce.Error()}
	}
	if err != nil {
		return "", &ExtractionError{Msg: err.Error()}
	}
	return finalizeNativeText(text, p, func() string { return pdfCoverageNoteFromBytes(ctx, data, p) })
}

const (
	pdfEmptyPageChars        = 20
	pdfCoverageMinEmpty      = 2
	pdfCoverageMinRatio      = 0.2
	pdfCoverageAbsoluteEmpty = 10
	pdfPageScanTimeout       = 20 * time.Second
	pdfGapMapMaxEntries      = 20
	gapContextChars          = 60
)

// Per page pdftotext output, nil when the tool is missing or the text is undeterminable
func pdfPageTexts(ctx context.Context, p string) []string {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, pdfPageScanTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, p, "-")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || cctx.Err() != nil {
			return nil
		}
	}
	out := ""
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		out = decodeUTF8Replace(stdout.Bytes())
	}
	var pages []string
	if out != "" {
		pages = strings.Split(out, "\f")
	}
	if len(pages) > 0 && pyStrip(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	if len(pages) == 0 {
		return nil
	}
	return pages
}

// First n characters of s
func firstRunes(s string, n int) string { return runePrefix(s, n) }

// Gap breakdown with each gap labeled by the text extracted before it
func pdfGapMap(counts []int, texts []string, empty []int) string {
	var ranges [][2]int
	for _, pg := range empty {
		if n := len(ranges); n > 0 && ranges[n-1][1] == pg-1 {
			ranges[n-1][1] = pg
			continue
		}
		ranges = append(ranges, [2]int{pg, pg})
	}
	var lines []string
	for idx, r := range ranges {
		if idx >= pdfGapMapMaxEntries {
			break
		}
		a, b := r[0], r[1]
		label := ""
		for prev := a - 2; prev >= 0; prev-- {
			if counts[prev] >= pdfEmptyPageChars {
				snippet := firstRunes(strings.Join(strings.FieldsFunc(texts[prev], pyIsSpace), " "), gapContextChars)
				label = fmt.Sprintf(" — after \"%s\" (p%d)", snippet, prev+1)
				break
			}
		}
		span := fmt.Sprintf("pages %d-%d", a, b)
		if a == b {
			span = fmt.Sprintf("page %d", a)
		}
		n := b - a + 1
		plural := "s"
		if n == 1 {
			plural = ""
		}
		lines = append(lines, fmt.Sprintf("  %s (%d page%s)%s", span, n, plural, label))
	}
	if len(ranges) > pdfGapMapMaxEntries {
		rest := ranges[pdfGapMapMaxEntries:]
		total := 0
		for _, r := range rest {
			total += r[1] - r[0] + 1
		}
		lines = append(lines, fmt.Sprintf("  … %d more gaps (%d pages)", len(rest), total))
	}
	return strings.Join(lines, "\n")
}

// Warning header when many PDF pages yielded no text, empty otherwise
func pdfCoverageNote(ctx context.Context, p, displayPath string) string {
	texts := pdfPageTexts(ctx, p)
	if len(texts) < 2 {
		return ""
	}
	counts := make([]int, len(texts))
	var empty []int
	for i, page := range texts {
		counts[i] = pystr.Len(pyStrip(page))
		if counts[i] < pdfEmptyPageChars {
			empty = append(empty, i+1)
		}
	}
	total, nEmpty := len(counts), len(empty)
	enough := float64(nEmpty)/float64(total) >= pdfCoverageMinRatio || nEmpty >= pdfCoverageAbsoluteEmpty
	if nEmpty < pdfCoverageMinEmpty || !enough {
		return ""
	}
	shown := displayPath
	if shown == "" {
		shown = p
	}
	return "[EXTRACTION COVERAGE WARNING: " +
		fmt.Sprintf("%d of %d pages in this PDF yielded no text. ", nEmpty, total) +
		"Those pages are likely scanned images (or blank) — their content " +
		"is MISSING from the extracted text below, even where section " +
		"headers appear with empty bodies. Unreadable gaps, each labeled " +
		"with the last text extracted before it:\n" +
		pdfGapMap(counts, texts, empty) + "\n" +
		"Decide which gaps you actually need — do NOT OCR or render " +
		"everything. For the gaps that matter, render just that range with " +
		"`pdftoppm -jpeg -r 150 -f <first> -l <last> " + shQuote(shown) + " $TMPDIR/page` " +
		"and inspect each image with the vision_analyze tool, or use the " +
		"ocr-and-documents skill (marker-pdf) for bulk OCR of large " +
		"ranges.]\n"
}

// Coverage note for backend PDF bytes through a host temp copy
func pdfCoverageNoteFromBytes(ctx context.Context, data []byte, displayPath string) string {
	tmp, cleanup, err := tempDocumentCopy(data, ".pdf")
	if err != nil {
		return ""
	}
	defer cleanup()
	return pdfCoverageNote(ctx, tmp, displayPath)
}

// Joins extracted lines with one trailing newline, failing when nothing is non blank
func joinedLines(lines []string, emptyError string) (string, error) {
	for _, l := range lines {
		if pyStrip(l) != "" {
			return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n", nil
		}
	}
	return "", &ExtractionError{Msg: emptyError}
}

// Decodes UTF-8 replacing each maximal invalid subpart with U+FFFD like errors=replace
func decodeUTF8Replace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += pystr.UTF8MaximalSubpart(b[i:])
	}
	return sb.String()
}

// Notebook source or text field as a string, joining list fragments
func notebookSourceText(source any) string {
	if list, ok := source.([]any); ok {
		var b strings.Builder
		for _, item := range list {
			if s, ok := item.(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	if s, ok := source.(string); ok {
		return s
	}
	return ""
}

func humanSize(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%d KB", int(math.RoundToEven(float64(n)/1024)))
	}
	return fmt.Sprintf("%d B", n)
}

var nonBase64Re = regexp.MustCompile(`[^0-9+/=A-Za-z]`)

// Approximate decoded size of a base64 payload with whitespace ignored
func base64Bytes(payload string) int {
	clean := nonBase64Re.ReplaceAllString(payload, "")
	pad := len(clean) - len(strings.TrimRight(clean, "="))
	return max(0, (len(clean)*3)/4-min(2, pad))
}

// Strips ANSI escapes and keeps only the final carriage return frame of each line
func cleanStreamText(text string) string {
	lines := strings.Split(strings.ReplaceAll(StripANSI(text), "\r\n", "\n"), "\n")
	for i, line := range lines {
		last := ""
		for _, f := range strings.Split(line, "\r") {
			if f != "" {
				last = f
			}
		}
		lines[i] = last
	}
	return strings.Join(lines, "\n")
}

const maxNotebookOutputChars = 20000

// nbformat v3 keys that hold mime payloads flat on the output
var v3MimeKeys = [][2]string{{"png", "image/png"}, {"jpeg", "image/jpeg"}, {"svg", "image/svg+xml"}, {"html", "text/html"}}

// One notebook output as compact text with heavy payloads replaced by sized placeholders
func notebookOutputText(output any) (string, error) {
	out, ok := output.(*pyjson.Object)
	if !ok {
		return "", nil
	}
	otype, _ := out.Get("output_type")
	if otype == "stream" {
		text, _ := out.Get("text")
		body := cleanStreamText(notebookSourceText(text))
		if pyStrip(body) == "" {
			return "", nil
		}
		return body, nil
	}
	if err := pyJSONHashable(otype); err != nil {
		return "", err
	}
	if otype == "error" || otype == "pyerr" {
		tbText := ""
		if tb, ok := out.Vals["traceback"].([]any); ok {
			var parts []string
			for _, l := range tb {
				if s, ok := l.(string); ok {
					parts = append(parts, s)
				}
			}
			tbText = cleanStreamText(strings.Join(parts, "\n"))
		}
		ename, ok := out.Get("ename")
		if !ok {
			ename = ""
		}
		evalue, ok := out.Get("evalue")
		if !ok {
			evalue = ""
		}
		header := strings.TrimRight("Error: "+pyJSONStr(ename)+": "+pyJSONStr(evalue), ": ")
		return strings.TrimRightFunc(header+"\n"+tbText, pyIsSpace), nil
	}
	if otype != "execute_result" && otype != "display_data" && otype != "pyout" {
		return "", nil
	}
	data, ok := out.Vals["data"].(*pyjson.Object)
	if !ok {
		data = &pyjson.Object{Vals: map[string]any{}}
		switch t := out.Vals["text"].(type) {
		case string, []any:
			data.Set("text/plain", t)
		}
		for _, km := range v3MimeKeys {
			if v, ok := out.Get(km[0]); ok {
				data.Set(km[1], v)
			}
		}
	}
	if _, ok := data.Get("application/vnd.jupyter.widget-view+json"); ok {
		return "[interactive widget — omitted]", nil
	}
	for _, mime := range []string{"text/plain", "text/markdown"} {
		if v, ok := data.Get(mime); ok {
			if body := cleanStreamText(notebookSourceText(v)); pyStrip(body) != "" {
				return body, nil
			}
		}
	}
	for _, mime := range data.Keys {
		if strings.HasPrefix(mime, "image/") {
			return fmt.Sprintf("[%s output — %s, omitted]", mime, humanSize(base64Bytes(notebookSourceText(data.Vals[mime])))), nil
		}
	}
	if v, ok := data.Get("text/html"); ok {
		return fmt.Sprintf("[text/html output — %s chars, omitted]", commaInt(pystr.Len(notebookSourceText(v)))), nil
	}
	names := strings.Join(data.Keys, ", ")
	if names == "" {
		names = "unknown"
	}
	return "[" + names + " output — omitted]", nil
}

// Rendered outputs of a code cell capped at the per cell character budget
func notebookOutputs(cell *pyjson.Object, jqPointer, filename string) (string, error) {
	outputs, ok := cell.Vals["outputs"].([]any)
	if !ok {
		return "", nil
	}
	var parts []string
	for _, o := range outputs {
		t, err := notebookOutputText(o)
		if err != nil {
			return "", err
		}
		if t != "" {
			parts = append(parts, t)
		}
	}
	joined := strings.Join(parts, "\n")
	total := pystr.Len(joined)
	if total <= maxNotebookOutputChars {
		return joined, nil
	}
	hint := ""
	if jqPointer != "" && filename != "" {
		hint = " — full output: jq -r '" + jqPointer + "' " + shQuote(filename)
	}
	return firstRunes(joined, maxNotebookOutputChars) + fmt.Sprintf("\n… [%s output chars truncated%s]", commaInt(total-maxNotebookOutputChars), hint), nil
}

var notebookCellLabels = map[string]string{"markdown": "Markdown", "code": "Code", "raw": "Raw"}

// Renders a Jupyter notebook as labeled cells with compact outputs
func extractNotebook(p, displayPath string) (string, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", extractionErr("Not a valid notebook: %s", err.Error())
	}
	parsed, err := pyjson.Loads(decodeUTF8Replace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	if err != nil {
		return "", extractionErr("Not a valid notebook: %s", err.Error())
	}
	nb, ok := parsed.(*pyjson.Object)
	if !ok {
		return "", extractionErr("Notebook root is not an object")
	}
	type pointedCell struct {
		pointer string
		cell    any
	}
	var cells []pointedCell
	if rawCells, ok := nb.Vals["cells"].([]any); ok {
		for i, c := range rawCells {
			cells = append(cells, pointedCell{fmt.Sprintf(".cells[%d].outputs", i), c})
		}
	} else {
		worksheets, ok := nb.Get("worksheets")
		if !ok {
			worksheets = []any{}
		}
		wsList, err := pyJSONIter(worksheets)
		if err != nil {
			return "", err
		}
		for wi, ws := range wsList {
			wsObj, ok := ws.(*pyjson.Object)
			if !ok {
				continue
			}
			wsCells, ok := wsObj.Get("cells")
			if !ok {
				wsCells = []any{}
			}
			items, err := pyJSONIter(wsCells)
			if err != nil {
				return "", err
			}
			for ci, c := range items {
				cells = append(cells, pointedCell{fmt.Sprintf(".worksheets[%d].cells[%d].outputs", wi, ci), c})
			}
		}
	}
	if len(cells) == 0 {
		return "", extractionErr("Notebook contains no cells")
	}
	counts := map[string]int{}
	var out []string
	for _, pc := range cells {
		cell, isObj := pc.cell.(*pyjson.Object)
		var typ any
		if isObj {
			typ, _ = cell.Get("cell_type")
		}
		if err := pyJSONHashable(typ); err != nil {
			return "", err
		}
		name, _ := typ.(string)
		label, known := notebookCellLabels[name]
		if !known {
			continue
		}
		counts[name]++
		suffix := ""
		if name != "raw" {
			suffix = " " + strconv.Itoa(counts[name])
		}
		src, ok := cell.Get("source")
		if !ok {
			src = ""
		}
		source := strings.TrimRight(notebookSourceText(src), "\n")
		out = append(out, "# ── "+label+" cell"+suffix+" ──", source, "")
		if name == "code" {
			rendered, err := notebookOutputs(cell, pc.pointer, displayPath)
			if err != nil {
				return "", err
			}
			if rendered != "" {
				out = append(out, fmt.Sprintf("# ── Output (cell %d) ──", counts[name]), strings.TrimRight(rendered, "\n"), "")
			}
		}
	}
	return joinedLines(out, "Notebook contains no readable cells")
}

// Opened OOXML package with entries addressable by exact name
type zipPackage struct {
	files map[string]*zip.File
	names []string
	kind  string
}

// Maps archive and read failures to the error text of a bad package
func zipFailure(kind string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return &ExtractionError{Msg: err.Error()}
	}
	switch {
	case errors.Is(err, zip.ErrFormat):
		return extractionErr("Not a valid %s: File is not a zip file", kind)
	case errors.Is(err, zip.ErrChecksum):
		return extractionErr("Not a valid %s: Bad CRC-32", kind)
	}
	return extractionErr("Not a valid %s: %s", kind, err.Error())
}

func openZipPackage(p, kind string) (*zipPackage, func(), error) {
	r, err := zip.OpenReader(p)
	if err != nil {
		return nil, func() {}, zipFailure(kind, err)
	}
	pkg := &zipPackage{files: map[string]*zip.File{}, kind: kind}
	for _, f := range r.File {
		if _, seen := pkg.files[f.Name]; !seen {
			pkg.names = append(pkg.names, f.Name)
		}
		pkg.files[f.Name] = f
	}
	return pkg, func() { r.Close() }, nil
}

// Bytes of one entry, reporting absence separately from a damaged archive
func (z *zipPackage) read(name string) ([]byte, bool, error) {
	f, ok := z.files[name]
	if !ok {
		return nil, false, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, true, zipFailure(z.kind, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, true, zipFailure(z.kind, err)
	}
	return data, true, nil
}

// Parses a package part, optional parts yielding an empty element when absent or malformed
func (z *zipPackage) xmlPart(name string, optional bool) (*etNode, error) {
	data, ok, err := z.read(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		if optional {
			return &etNode{tag: "missing", attrs: map[string]string{}}, nil
		}
		return nil, extractionErr("Missing %s", name)
	}
	root, err := etParse(data)
	if err != nil {
		if optional {
			return &etNode{tag: "missing", attrs: map[string]string{}}, nil
		}
		return nil, extractionErr("Malformed XML in %s: %s", name, err.Error())
	}
	return root, nil
}

// Paragraph text of word/document.xml with tabs and breaks, ruby guides excluded
func extractDocx(p string) (string, error) {
	pkg, closeFn, err := openZipPackage(p, "DOCX")
	if err != nil {
		return "", err
	}
	root, err := pkg.xmlPart("word/document.xml", false)
	closeFn()
	if err != nil {
		return "", err
	}
	w := "{" + nsW + "}"
	breaks := map[string]string{w + "tab": "\t", w + "br": "\n", w + "cr": "\n"}
	var lines []string
	for _, para := range root.all(w + "p") {
		guide := map[*etNode]bool{}
		for _, rt := range para.all(w + "rt") {
			rt.iter("", func(n *etNode) { guide[n] = true })
		}
		var b strings.Builder
		para.iter("", func(n *etNode) {
			if guide[n] {
				return
			}
			if n.tag == w+"t" {
				b.WriteString(n.text)
			} else {
				b.WriteString(breaks[n.tag])
			}
		})
		lines = append(lines, strings.Split(b.String(), "\n")...)
	}
	return joinedLines(lines, "DOCX contains no extractable text")
}

// Base text of a shared or inline string, excluding phonetic runs
func xlsxStringText(item *etNode, s string) string {
	var b strings.Builder
	for _, child := range item.children {
		switch child.tag {
		case s + "t":
			b.WriteString(child.text)
		case s + "r":
			t, _ := child.findtext(s + "t")
			b.WriteString(t)
		}
	}
	return b.String()
}

// Visible worksheets as tab separated rows under sheet headers
func extractXlsx(p string) (string, error) {
	s, r, pr := "{"+nsS+"}", "{"+nsRel+"}", "{"+nsPkgRel+"}"
	pkg, closeFn, err := openZipPackage(p, "XLSX")
	if err != nil {
		return "", err
	}
	defer closeFn()
	sst, err := pkg.xmlPart("xl/sharedStrings.xml", true)
	if err != nil {
		return "", err
	}
	var shared []string
	for _, item := range sst.all(s + "si") {
		shared = append(shared, xlsxStringText(item, s))
	}
	relsRoot, err := pkg.xmlPart("xl/_rels/workbook.xml.rels", true)
	if err != nil {
		return "", err
	}
	rels := map[string]string{}
	for _, rel := range relsRoot.all(pr + "Relationship") {
		if id := rel.get("Id", ""); id != "" {
			rels[id] = rel.get("Target", "")
		}
	}
	workbook, err := pkg.xmlPart("xl/workbook.xml", false)
	if err != nil {
		return "", err
	}
	var out []string
	for _, sheet := range workbook.all(s + "sheet") {
		target := strings.TrimLeft(rels[sheet.get(r+"id", "")], "/")
		if !strings.HasPrefix(target, "xl/") {
			target = "xl/" + target
		}
		part := path.Clean(target)
		state := sheet.get("state", "visible")
		if _, ok := pkg.files[part]; state == "hidden" || state == "veryHidden" || !ok {
			continue
		}
		data, _, err := pkg.read(part)
		if err != nil {
			return "", err
		}
		rows, err := xlsxSheetRows(data, shared)
		if err != nil {
			continue
		}
		out = append(out, "# ── Sheet: "+sheet.get("name", "Sheet")+" ──")
		if len(rows) == 0 {
			out = append(out, "(empty)")
		}
		for _, row := range rows {
			out = append(out, strings.Join(row, "\t"))
		}
		out = append(out, "")
	}
	return joinedLines(out, "XLSX has no visible sheets with content")
}

// Zero based column of a cell reference from its bijective base 26 letters
func xlsxColIndex(ref string) int {
	idx := 0
	for _, ch := range ref {
		if !unicode.IsLetter(ch) {
			break
		}
		idx = idx*26 + int(unicode.ToUpper(ch)) - int('A') + 1
	}
	return max(idx-1, 0)
}

// Rows of one worksheet, capped in rows and columns, with trailing blank rows dropped
func xlsxSheetRows(data []byte, shared []string) ([][]string, error) {
	root, err := etParse(data)
	if err != nil {
		return nil, err
	}
	return sheetRowsIn(root, shared, "{"+nsS+"}"), nil
}

// Rows of a parsed worksheet whose SpreadsheetML namespace prefix is s
func sheetRowsIn(root *etNode, shared []string, s string) [][]string {
	var rows [][]string
	for i, row := range root.all(s + "row") {
		if i >= maxXlsxRowsPerSheet {
			break
		}
		cells := map[int]string{}
		maxCol := -1
		for _, cell := range row.all(s + "c") {
			col := maxCol + 1
			if ref := cell.get("r", ""); ref != "" {
				col = xlsxColIndex(ref)
			}
			if col < maxXlsxCols {
				cells[col] = xlsxCellValue(cell, shared, s)
				maxCol = max(maxCol, col)
			}
		}
		values := make([]string, maxCol+1)
		for c := range values {
			values[c] = cells[c]
		}
		rows = append(rows, values)
	}
	for len(rows) > 0 {
		blank := true
		for _, v := range rows[len(rows)-1] {
			if pyStrip(v) != "" {
				blank = false
				break
			}
		}
		if !blank {
			break
		}
		rows = rows[:len(rows)-1]
	}
	return rows
}

var pyIntUnderscoreRe = regexp.MustCompile(`^[+-]?[0-9](?:_?[0-9])*$`)

// Python int of a string with whitespace, sign and digit separators
func pyIntParse(s string) (int, bool) {
	t := pyStrip(s)
	if !pyIntUnderscoreRe.MatchString(t) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.ReplaceAll(t, "_", ""))
	return n, err == nil
}

// Display value of one cell from shared, inline, boolean, error or raw storage
func xlsxCellValue(cell *etNode, shared []string, s string) string {
	value, _ := cell.findtext(s + "v")
	typ := cell.get("t", "")
	switch typ {
	case "s":
		i, ok := pyIntParse(value)
		if !ok {
			return ""
		}
		if i < 0 {
			i += len(shared)
		}
		if i < 0 || i >= len(shared) {
			return ""
		}
		return shared[i]
	case "inlineStr":
		inline := cell.find(s + "is")
		if inline == nil {
			return ""
		}
		return xlsxStringText(inline, s)
	case "b":
		switch pyStrip(value) {
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

var sqliteMagic = []byte("SQLite format 3\x00")

const (
	sqlitePreviewRows = 5
	sqliteMaxTables   = 200
	sqliteCellChars   = 80
)

// Percent encodes a path with slash kept safe the way pathlib as_uri does
func fileURIPath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '/' || c == '_' || c == '.' || c == '-' || c == '~' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func sqlQuoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// Schema overview of a SQLite file with row counts and the first rows of each table
func extractSqlite(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	head := make([]byte, len(sqliteMagic))
	n, _ := io.ReadFull(f, head)
	f.Close()
	if !bytes.Equal(head[:n], sqliteMagic) {
		return "", extractionErr("not a SQLite database (magic bytes do not match)")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	db, err := sql.Open("sqlite", "file://"+fileURIPath(resolveNonStrict(abs))+"?mode=ro&immutable=1")
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return "", err
	}
	out, err := sqliteOverview(db)
	if err != nil {
		return "", extractionErr("SQLite read failed: %s", err.Error())
	}
	return out, nil
}

type sqliteObject struct{ typ, name, sql string }

func sqliteOverview(db *sql.DB) (string, error) {
	rows, err := db.Query("SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type DESC, name")
	if err != nil {
		return "", err
	}
	var tables, others []sqliteObject
	for rows.Next() {
		var o sqliteObject
		if err := rows.Scan(&o.typ, &o.name, &o.sql); err != nil {
			rows.Close()
			return "", err
		}
		if o.typ == "table" {
			tables = append(tables, o)
		} else {
			others = append(others, o)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	out := []string{"# SQLite database", "", fmt.Sprintf("%d table(s), %d index/view/trigger(s)", len(tables), len(others))}
	for i, t := range tables {
		if i >= sqliteMaxTables {
			break
		}
		quoted := sqlQuoteIdent(t.name)
		var count int64
		if err := db.QueryRow("SELECT COUNT(*) FROM " + quoted).Scan(&count); err != nil {
			return "", err
		}
		out = append(out, "", fmt.Sprintf("## %s  (%s rows)", t.name, commaInt(int(count))), pyStrip(t.sql))
		cols, preview, err := sqlitePreview(db, quoted)
		if err != nil {
			return "", err
		}
		if len(preview) > 0 {
			out = append(out, "| "+strings.Join(cols, " | ")+" |", "|"+strings.Repeat("---|", len(cols)))
			for _, row := range preview {
				cells := make([]string, len(row))
				for k, v := range row {
					cells[k] = sqliteCell(v)
				}
				out = append(out, "| "+strings.Join(cells, " | ")+" |")
			}
		}
	}
	if len(tables) > sqliteMaxTables {
		out = append(out, fmt.Sprintf("\n... %d more tables omitted", len(tables)-sqliteMaxTables))
	}
	if len(others) > 0 {
		out = append(out, "", "## Indexes / views / triggers")
		for _, o := range others {
			out = append(out, "- "+o.typ+" "+o.name)
		}
	}
	out = append(out, "", "Query it with the terminal: sqlite3 <path> 'SELECT ...'  (or Python's sqlite3 module).")
	return strings.Join(out, "\n"), nil
}

// Column names and first rows of a table, read through unary plus so values keep their storage class
func sqlitePreview(db *sql.DB, quoted string) ([]string, [][]any, error) {
	probe, err := db.Query("SELECT * FROM " + quoted + " LIMIT 0")
	if err != nil {
		return nil, nil, err
	}
	cols, err := probe.Columns()
	probe.Close()
	if err != nil {
		return nil, nil, err
	}
	exprs := make([]string, len(cols))
	for i, c := range cols {
		exprs[i] = "+" + sqlQuoteIdent(c)
	}
	rows, err := db.Query(fmt.Sprintf("SELECT %s FROM %s LIMIT %d", strings.Join(exprs, ", "), quoted, sqlitePreviewRows))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		out = append(out, vals)
	}
	return cols, out, rows.Err()
}

// One preview cell with NULL, blob size, escaped pipes and the width cap
func sqliteCell(v any) string {
	var text string
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("<blob %d bytes>", len(x))
	case int64:
		text = strconv.FormatInt(x, 10)
	case float64:
		text = pyFloatRepr(x)
	case string:
		text = x
	default:
		text = fmt.Sprint(x)
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "|", `\|`), "\n", " ")
	if pystr.Len(text) <= sqliteCellChars {
		return text
	}
	return firstRunes(text, sqliteCellChars-1) + "…"
}
