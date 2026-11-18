package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	// Pages one /image of a PDF renders at most
	pdfMaxPages = 25
	// Longest a PDF render may take
	pdfRenderLimit = 2 * time.Minute
)

// Page range argument after a PDF path, one page or first-last
var pdfRangeRe = regexp.MustCompile(`^(\d+)(?:\s*-\s*(\d+))?$`)

// Pages of a PDF render, their PNG files in page order and where they sit in the document
type pdfRender struct {
	dir   string
	files []string
	first int
	last  int
	total int
}

// Whether the typed path names a PDF file
func isPDFPath(raw, base string) (string, string, bool) {
	path, remainder, err := resolveImagePath(raw, base)
	if err != nil || !strings.EqualFold(filepath.Ext(path), ".pdf") {
		return "", "", false
	}
	return path, remainder, true
}

// Renders the asked pages of a PDF to PNG files with poppler, the first pages up to the cap without a range
func renderPDFPages(path, pages string) (*pdfRender, error) {
	info, err := exec.LookPath("pdfinfo")
	if err != nil {
		return nil, errors.New(tr("pdf.popplerMissing"))
	}
	render, err := exec.LookPath("pdftoppm")
	if err != nil {
		return nil, errors.New(tr("pdf.popplerMissing"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), pdfRenderLimit)
	defer cancel()
	out, err := exec.CommandContext(ctx, info, path).Output()
	if err != nil {
		return nil, fmt.Errorf("pdfinfo: %w", err)
	}
	total := 0
	for _, line := range strings.Split(string(out), "\n") {
		if rest, found := strings.CutPrefix(line, "Pages:"); found {
			total, _ = strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	if total < 1 {
		return nil, errors.New(tr("pdf.noPages", path))
	}
	first, last := 1, min(total, pdfMaxPages)
	if pages = strings.TrimSpace(pages); pages != "" {
		m := pdfRangeRe.FindStringSubmatch(pages)
		if m == nil {
			return nil, errors.New(tr("pdf.badRange", pages))
		}
		first, _ = strconv.Atoi(m[1])
		last = first
		if m[2] != "" {
			last, _ = strconv.Atoi(m[2])
		}
		if first < 1 || last < first || last > total {
			return nil, errors.New(tr("pdf.rangeOutside", pages, strconv.Itoa(total)))
		}
		if last-first+1 > pdfMaxPages {
			return nil, errors.New(tr("pdf.rangeTooLong", strconv.Itoa(pdfMaxPages)))
		}
	}
	dir, err := os.MkdirTemp("", "icebath-pdf-")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, render, "-png", "-r", "150", "-f", strconv.Itoa(first), "-l", strconv.Itoa(last), path, filepath.Join(dir, "page"))
	if text, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("pdftoppm: %w: %s", err, strings.TrimSpace(string(text)))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "page-*.png"))
	sort.Slice(files, func(i, j int) bool {
		return len(files[i]) < len(files[j]) || (len(files[i]) == len(files[j]) && files[i] < files[j])
	})
	return &pdfRender{dir: dir, files: files, first: first, last: last, total: total}, nil
}

// Renders a PDF off the update loop, attaches every rendered page in order and names the pages it took
func (a *app) attachPDFPages(path, pages string) tea.Cmd {
	return func() tea.Msg {
		rendered, err := renderPDFPages(path, pages)
		return asyncDoneMsg{run: func() tea.Cmd {
			if err != nil {
				a.sys("error: " + err.Error())
				return nil
			}
			cmds := make([]tea.Cmd, 0, len(rendered.files)+1)
			for _, file := range rendered.files {
				cmds = append(cmds, a.attachImagePath(file))
			}
			cmds = append(cmds, func() tea.Msg {
				_ = os.RemoveAll(rendered.dir)
				return nil
			})
			a.sys(tr("pdf.attached", strconv.Itoa(rendered.first), strconv.Itoa(rendered.last), strconv.Itoa(rendered.total), filepath.Base(path)))
			if pages == "" && rendered.last < rendered.total {
				a.sys(tr("pdf.morePages", path, strconv.Itoa(rendered.last+1), strconv.Itoa(min(rendered.total, rendered.last+pdfMaxPages))))
			}
			return tea.Sequence(cmds...)
		}}
	}
}
