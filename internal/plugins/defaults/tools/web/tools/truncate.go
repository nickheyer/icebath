package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Longest full text of a page saved to the cache folder
const maxStored = 2_000_000

// Kinds that are never text, whatever else a page holds
var binaryKinds = []string{"SQLite", "ZIP", "gzip", "bzip2", "xz", "7-Zip", "ELF", "Mach-O", "PNG", "JPEG", "GIF", "TIFF", "FLAC", "Ogg"}

// Leading bytes of text a backend gave for a page: of its first n characters, those up to U+00FF read as bytes
func head(content string, n int) string {
	var b strings.Builder
	for _, r := range content {
		if n--; n < 0 {
			break
		}
		if r <= 0xFF {
			b.WriteByte(byte(r))
		}
	}
	return b.String()
}

// Kind of binary file a page turned out to be when it is one that is never text, empty otherwise. NUL bytes are left out on both sides,
// as a text decoder drops them
func binaryKind(content string) string {
	leading := strings.ReplaceAll(head(content, 32), "\x00", "")
	for _, signature := range tools.Signatures {
		prefix := strings.ReplaceAll(signature.Prefix, "\x00", "")
		if prefix == "" || !strings.HasPrefix(leading, prefix) {
			continue
		}
		for _, kind := range binaryKinds {
			if strings.HasPrefix(signature.Name, kind) {
				return signature.Name
			}
		}
	}
	return ""
}

// Kind of document a page turned out to be, a file the document reader turns into text; empty when it is none
func documentKind(content string) string {
	leading := head(content, 1024)
	switch {
	case strings.Contains(leading, "%PDF-"):
		return "PDF document"
	case strings.HasPrefix(leading, "PK\x03\x04"):
		return "ZIP archive (also docx/xlsx/jar/apk)"
	case strings.HasPrefix(leading, "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"):
		return "Office document (doc/xls/ppt)"
	case strings.HasPrefix(leading, `{\rtf`):
		return "RTF document"
	case strings.HasPrefix(leading, "SQLite format 3\x00"):
		return "SQLite database"
	}
	return ""
}

// Extension a kind is read by when the URL names none, the reader telling the others apart by their content
var extensions = map[string]string{
	"PDF document":    ".pdf",
	"RTF document":    ".rtf",
	"SQLite database": ".sqlite",
}

var (
	markdownImage = regexp.MustCompile(`!\[([^\]]*)\]\(\s*data:image/[^;]+;base64,[A-Za-z0-9+/=\s]+\)`)
	bracedImage   = regexp.MustCompile(`\(\s*data:image/[^;]+;base64,[A-Za-z0-9+/=\s]+\)`)
	bareImage     = regexp.MustCompile(`data:image/[^;]+;base64,[A-Za-z0-9+/=]+`)
)

// Text with inline base64 images as [IMAGE: alt] placeholders
func images(text string) string {
	text = markdownImage.ReplaceAllStringFunc(text, func(match string) string {
		if alt := strings.TrimSpace(markdownImage.FindStringSubmatch(match)[1]); alt != "" {
			return "[IMAGE: " + alt + "]"
		}
		return "[IMAGE]"
	})
	return bareImage.ReplaceAllString(bracedImage.ReplaceAllString(text, "[IMAGE]"), "[IMAGE]")
}

// Puts each page's text within the budget, recording what was cut
func (r *registrant) truncate(cfg *config.Config, pages []*web.Page, limit int, record map[string]any) {
	for _, page := range pages {
		switch {
		case page == nil:
		case page.Error != "":
			if page.Content != "" {
				page.Content, _ = r.window(cfg, images(page.Content), page.URL, limit)
			}
		case page.Content == "":
		case binaryKind(page.Content) != "":
			page.Error = fmt.Sprintf("URL returned binary content (%s), not a page. Download it with the terminal (curl -L -o) and use read_file (SQLite/Office/PDF auto-extract) or terminal utilities on the file.", binaryKind(page.Content))
			page.Content = ""
		default:
			clean := images(page.Content)
			text, cut := r.window(cfg, clean, page.URL, limit)
			page.Content = text
			if cut {
				record["pages_truncated"] = record["pages_truncated"].(int) + 1
				record["truncation_metrics"] = append(record["truncation_metrics"].([]any),
					map[string]any{"url": page.URL, "original_size": len([]rune(clean)), "sent_size": len([]rune(text))})
			}
		}
	}
}

// Text within the budget: whole when it fits, else its head and tail cut at line breaks with a footer naming the file the whole text
// is saved to, as the agent's commands see it, and the read_file call that pages through the middle. Cut says it did not fit
func (r *registrant) window(cfg *config.Config, content, url string, limit int) (string, bool) {
	runes := []rune(content)
	if len(runes) <= limit {
		return content, false
	}
	headBudget := int(float64(limit) * 0.75)
	tailBudget := limit - headBudget
	front := runes[:headBudget]
	back := runes[len(runes)-tailBudget:]
	if at := lastIndex(front, '\n'); float64(at) > float64(headBudget)*0.5 {
		front = front[:at]
	}
	if at := index(back, '\n'); at >= 0 && float64(at) < float64(tailBudget)*0.5 {
		back = back[at+1:]
	}
	footer := []string{"", strings.Repeat("─", 8) + " [TRUNCATED] " + strings.Repeat("─", 8),
		fmt.Sprintf("Showing %s chars (head) + %s chars (tail) of %s total clean characters.", strs.Thousands(len(front)), strs.Thousands(len(back)), strs.Thousands(len(runes)))}
	if stored := save(cfg, url, content); stored != "" {
		stored = r.terminal.Visible(cfg, stored)
		middle := strings.Count(string(front), "\n") + 2
		footer = append(footer, "Full text saved to: "+stored,
			fmt.Sprintf(`To read the omitted middle: read_file path="%s" offset=%d limit=200  (the file is the complete page; raise/lower offset to page through it).`, stored, middle))
	} else {
		footer = append(footer, "Full text could not be stored; re-run web_extract on a more specific URL or open it in the browser for the complete page.")
	}
	footer = append(footer, strings.Repeat("─", 29))
	return string(front) + "\n\n[... middle omitted — see footer ...]\n\n" + string(back) + "\n" + strings.Join(footer, "\n"), true
}

func lastIndex(runes []rune, r rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

func index(runes []rune, r rune) int {
	for i, c := range runes {
		if c == r {
			return i
		}
	}
	return -1
}

// Saves the page's whole text to the cache folder and returns its path, empty when it cannot be saved. A text past the cap is cut with
// a note saying so
func save(cfg *config.Config, url, content string) string {
	dir, err := cacheDir(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(url))
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.md", hostSlug(url), hex.EncodeToString(sum[:])[:10]))
	if runes := []rune(content); len(runes) > maxStored {
		content = string(runes[:maxStored]) + fmt.Sprintf("\n\n[... stored copy truncated at %s chars of %s; re-extract a more specific URL for the rest ...]", strs.Thousands(maxStored), strs.Thousands(len(runes)))
	}
	if fsx.Write(path, []byte(content), 0o600) != nil {
		return ""
	}
	return path
}
