package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Characters below which a page counts as having yielded no text
	emptyPageChars = 20
	// Empty pages that raise the coverage warning: at least two, and a fifth of the pages or ten of them
	coverageMinEmpty      = 2
	coverageMinRatio      = 0.2
	coverageAbsoluteEmpty = 10
	// Gaps the coverage warning lists before summing up the rest
	gapMapMaxEntries = 20
	// Characters of the text before a gap that label it
	gapContextChars  = 60
	pageScanTimeout  = 20 * time.Second
	ocrDPI           = "300"
	ocrRenderTimeout = 60 * time.Second
	ocrReadTimeout   = 120 * time.Second
)

// Whether this machine has every tool local OCR of scanned PDF pages needs
func LocalOCR() bool {
	_, missing := ocrBinaries()
	return len(missing) == 0
}

// Paths of the OCR tools found on PATH and the names of those missing
func ocrBinaries() (map[string]string, []string) {
	found := map[string]string{}
	var missing []string
	for _, name := range []string{"pdftoppm", "pdftotext", "tesseract"} {
		if bin, err := exec.LookPath(name); err == nil {
			found[name] = bin
		} else {
			missing = append(missing, name)
		}
	}
	return found, missing
}

// Text of a PDF whose listed pages, every page when none are listed, have no text layer: read by the hosted service when there is one,
// else by local OCR, else the warning saying the content is missing, why OCR did not run and how to recover the pages
func scanned(ctx context.Context, data []byte, name string, pages []int, parse func(ctx context.Context, data []byte, filename string) (string, error)) (string, error) {
	hosted := ""
	if parse != nil {
		text, err := parse(ctx, data, filepath.Base(name))
		if err == nil && strings.TrimSpace(text) != "" {
			return strings.TrimRight(text, "\n") + "\n", nil
		}
		if err == nil {
			err = errors.New("the service returned no text")
		}
		hosted = "hosted PDF parsing failed: " + err.Error()
	}
	text, err := ocr(ctx, data, pages)
	if err == nil {
		return text, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return needsOCR(name, pages, hosted, err.Error()), nil
}

// Warning standing in for a PDF's missing pages: which pages, every page when none are named, why OCR did not run and how to recover them
func needsOCR(name string, pages []int, hostedError, localError string) string {
	lead := "[NEEDS OCR: no page of this PDF has a text layer — its content is MISSING below. "
	if len(pages) > 0 {
		numbers := make([]string, len(pages))
		for i, n := range pages {
			numbers[i] = strconv.Itoa(n)
		}
		lead = "[NEEDS OCR: pages " + strings.Join(numbers, ", ") + " of this PDF are scanned images with no text layer — their content is MISSING below. "
	}
	why := ""
	if hostedError != "" {
		why = "Hosted OCR was attempted and failed (" + hostedError + "). "
	}
	if localError != "" {
		why += "Local OCR could not run (" + localError + "). "
	}
	return lead + why + "If the missing pages matter: render just those pages with `pdftoppm -jpeg -r 150 -f <first> -l <last> " +
		terminal.Quote(name) + " $TMPDIR/page` and inspect via vision_analyze, or check whether an OCR skill is available (skills_list).]\n"
}

// Every page of a PDF as text, the listed pages or all of them when none are listed read by tesseract and the rest by pdftotext
func ocr(ctx context.Context, data []byte, pages []int) (string, error) {
	bins, missing := ocrBinaries()
	switch len(missing) {
	case 0:
	case 1:
		return "", fmt.Errorf("%s is not on PATH, install poppler and tesseract", missing[0])
	default:
		return "", fmt.Errorf("%s and %s are not on PATH, install poppler and tesseract", strings.Join(missing[:len(missing)-1], ", "), missing[len(missing)-1])
	}
	langs, err := tesseractLanguages(ctx, bins["tesseract"])
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "icebath-ocr-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	pdf := filepath.Join(dir, "document.pdf")
	if err := os.WriteFile(pdf, data, 0o600); err != nil {
		return "", err
	}
	texts := pageTexts(ctx, pdf)
	if len(texts) == 0 {
		return "", errors.New("pdftotext could not list the pages of the PDF")
	}
	scannedPages := make(map[int]bool, len(pages))
	last := len(texts)
	for _, n := range pages {
		scannedPages[n] = true
		last = max(last, n)
	}
	var read []string
	var b strings.Builder
	for n := 1; n <= last; n++ {
		text := ""
		if n <= len(texts) {
			text = texts[n-1]
		}
		if len(pages) == 0 || scannedPages[n] {
			if text, err = tesseractPage(ctx, bins, langs, pdf, dir, n); err != nil {
				return "", err
			}
			read = append(read, strconv.Itoa(n))
		}
		fmt.Fprintf(&b, "## Page %d\n\n%s\n\n", n, strings.TrimSpace(text))
	}
	return "[OCR: pages " + strings.Join(read, ", ") + " of this PDF have no text layer and were read by tesseract, " +
		"so their text may hold recognition errors]\n" + strings.TrimRight(b.String(), "\n") + "\n", nil
}

// Languages tesseract reads with: English when installed, else every installed language but script detection
func tesseractLanguages(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ocrRenderTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--list-langs").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tesseract could not list its languages: %s", commandFailure(err, out))
	}
	var langs []string
	for _, line := range strings.Split(decodeUTF8(out), "\n") {
		lang := strings.TrimSpace(line)
		if lang == "" || strings.ContainsAny(lang, " :") || lang == "osd" {
			continue
		}
		if lang == "eng" {
			return "eng", nil
		}
		langs = append(langs, lang)
	}
	if len(langs) == 0 {
		return "", errors.New("tesseract has no language data, install a tesseract language pack such as eng")
	}
	return strings.Join(langs, "+"), nil
}

// Text of one page rendered by pdftoppm and read by tesseract
func tesseractPage(ctx context.Context, bins map[string]string, langs, pdf, dir string, n int) (string, error) {
	page := strconv.Itoa(n)
	img := filepath.Join(dir, "page"+page)
	renderCtx, cancelRender := context.WithTimeout(ctx, ocrRenderTimeout)
	defer cancelRender()
	if out, err := exec.CommandContext(renderCtx, bins["pdftoppm"], "-r", ocrDPI, "-png", "-f", page, "-l", page, "-singlefile", pdf, img).CombinedOutput(); err != nil {
		return "", fmt.Errorf("pdftoppm could not render page %d: %s", n, commandFailure(err, out))
	}
	readCtx, cancelRead := context.WithTimeout(ctx, ocrReadTimeout)
	defer cancelRead()
	cmd := exec.CommandContext(readCtx, bins["tesseract"], img+".png", "stdout", "-l", langs)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tesseract could not read page %d: %s", n, commandFailure(err, stderr.Bytes()))
	}
	return decodeUTF8(stdout.Bytes()), nil
}

// Last line a failed command wrote, else how it failed
func commandFailure(err error, output []byte) string {
	text := strings.TrimSpace(decodeUTF8(output))
	if last := strings.TrimSpace(text[strings.LastIndexByte(text, '\n')+1:]); last != "" {
		return last
	}
	return err.Error()
}

// Text pdftotext finds on each page of the PDF file, nil when it is missing or fails
func pageTexts(ctx context.Context, file string) []string {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pageScanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, file, "-")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil || stdout.Len() == 0 {
		return nil
	}
	// Every page ends in a form feed, the last one leaving an empty piece
	pages := strings.Split(decodeUTF8(stdout.Bytes()), "\f")
	if strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	if len(pages) == 0 {
		return nil
	}
	return pages
}

// Warning heading a PDF's text when many of its pages yielded none, each gap labeled by the text before it; empty otherwise
func coverage(ctx context.Context, data []byte, name string) string {
	file, remove, err := temporary(data, ".pdf")
	if err != nil {
		return ""
	}
	defer remove()
	texts := pageTexts(ctx, file)
	if len(texts) < 2 {
		return ""
	}
	counts := make([]int, len(texts))
	var empty []int
	for i, page := range texts {
		counts[i] = utf8.RuneCountInString(strings.TrimSpace(page))
		if counts[i] < emptyPageChars {
			empty = append(empty, i+1)
		}
	}
	enough := float64(len(empty))/float64(len(texts)) >= coverageMinRatio || len(empty) >= coverageAbsoluteEmpty
	if len(empty) < coverageMinEmpty || !enough {
		return ""
	}
	return "[EXTRACTION COVERAGE WARNING: " + fmt.Sprintf("%d of %d pages in this PDF yielded no text. ", len(empty), len(texts)) +
		"Those pages are likely scanned images (or blank) — their content is MISSING from the extracted text below, even where " +
		"section headers appear with empty bodies. Unreadable gaps, each labeled with the last text extracted before it:\n" +
		gapMap(counts, texts, empty) + "\n" +
		"Decide which gaps you actually need — do NOT OCR or render everything. For the gaps that matter, render just that range " +
		"with `pdftoppm -jpeg -r 150 -f <first> -l <last> " + terminal.Quote(name) + " $TMPDIR/page` and inspect each image with the " +
		"vision_analyze tool, or use the ocr-and-documents skill (marker-pdf) for bulk OCR of large ranges.]\n"
}

// Runs of empty pages, one line each labeled by the text of the last page before it that has some, the runs past the cap summed up
func gapMap(counts []int, texts []string, empty []int) string {
	var runs [][2]int
	for _, page := range empty {
		if n := len(runs); n > 0 && runs[n-1][1] == page-1 {
			runs[n-1][1] = page
			continue
		}
		runs = append(runs, [2]int{page, page})
	}
	var lines []string
	for _, run := range runs[:min(len(runs), gapMapMaxEntries)] {
		a, b := run[0], run[1]
		label := ""
		for prev := a - 2; prev >= 0; prev-- {
			if counts[prev] >= emptyPageChars {
				label = fmt.Sprintf(" — after \"%s\" (p%d)", strs.Head(strings.Join(strings.Fields(texts[prev]), " "), gapContextChars), prev+1)
				break
			}
		}
		span, plural := fmt.Sprintf("pages %d-%d", a, b), "s"
		if a == b {
			span, plural = fmt.Sprintf("page %d", a), ""
		}
		lines = append(lines, fmt.Sprintf("  %s (%d page%s)%s", span, b-a+1, plural, label))
	}
	if len(runs) > gapMapMaxEntries {
		rest := runs[gapMapMaxEntries:]
		total := 0
		for _, run := range rest {
			total += run[1] - run[0] + 1
		}
		lines = append(lines, fmt.Sprintf("  … %d more gaps (%d pages)", len(rest), total))
	}
	return strings.Join(lines, "\n")
}

// Private temporary file holding the bytes, and its remover
func temporary(data []byte, suffix string) (string, func(), error) {
	f, err := os.CreateTemp("", "icebath-*"+suffix)
	if err != nil {
		return "", nil, err
	}
	remove := func() { os.Remove(f.Name()) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		remove()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, err
	}
	return f.Name(), remove, nil
}
