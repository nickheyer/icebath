package gateway

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python re pattern compiled on regexp2 with its group numbers, matched over rune slices
type pyRE struct {
	re     *regexp2.Regexp
	groups map[string]int
}

func newPyRE(pattern string) pyRE {
	return pyRE{regexp2.MustCompile(mustTranslatePy(pattern), regexp2.None), mustGroupIndexPy(pattern)}
}

// Every non overlapping match over the runes, the way re.finditer walks a str
func (p pyRE) finditer(text []rune) []*regexp2.Match {
	var out []*regexp2.Match
	m, err := p.re.FindRunesMatch(text)
	for err == nil && m != nil {
		out = append(out, m)
		m, err = p.re.FindNextMatch(m)
	}
	return out
}

// Whether the pattern matches anywhere in the text
func (p pyRE) search(text string) bool {
	m, err := p.re.FindStringMatch(text)
	return err == nil && m != nil
}

// Rune span of a named group, ok false when it did not take part
func (p pyRE) span(m *regexp2.Match, name string) (int, int, bool) {
	g := m.GroupByNumber(p.groups[name])
	if g == nil || len(g.Captures) == 0 {
		return 0, 0, false
	}
	return g.RuneIndex, g.RuneIndex + g.RuneLength, true
}

// Matched text of a named group, empty when it did not take part
func (p pyRE) group(m *regexp2.Match, name string) string {
	g := m.GroupByNumber(p.groups[name])
	if g == nil || len(g.Captures) == 0 {
		return ""
	}
	return g.String()
}

type runeSpan struct{ start, end int }

// Extensions native media delivery accepts, shared by every tag pattern
var MediaDeliveryExts = []string{
	".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tiff", ".svg",
	".mp4", ".mov", ".avi", ".mkv", ".webm", ".3gp",
	".mp3", ".m2a", ".wav", ".ogg", ".opus", ".m4a", ".flac",
	".pdf", ".docx", ".doc", ".odt", ".rtf", ".txt", ".md", ".epub",
	".xlsx", ".xls", ".ods", ".csv", ".tsv", ".json", ".xml", ".yaml", ".yml",
	".kmz", ".kml", ".geojson", ".gpx",
	".pptx", ".ppt", ".odp", ".key",
	".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar", ".apk", ".ipa",
	".html", ".htm"}

var audioExts = map[string]bool{".ogg": true, ".opus": true, ".mp3": true, ".m2a": true, ".wav": true, ".m4a": true, ".flac": true}

const (
	mediaCJKTerminators = "（）〈〉《》：，。；！？、“”‘’【】"
	terminalSentinel    = "<|eos|>"
	bt                  = "`"
)

var (
	mediaTagCleanupRE = newPyRE(`(?i)[` + bt + `"'*_]{0,3}MEDIA:\s*` +
		`(?P<path>` + bt + `[^` + bt + `\n]+?` + bt + `|"[^"\n]+?"|'[^'\n]+?'|` +
		`(?:~/|/|[A-Za-z]:[/\\])\S+?(?:[^\S\n]+\S+?)*?\.(?:` + mediaExtAlternation() + `))` +
		`(?=[\s` + bt + `"'*_,;:)\]}\[` + mediaCJKTerminators + `]|MEDIA:|\.(?:\s|$)|$)[` + bt + `"'*_]{0,3}\.?`)
	mediaExtensionlessTagRE = newPyRE(`(?i)[` + bt + `"'*_]{0,3}MEDIA:\s*` +
		`(?P<path>` + bt + `[^` + bt + `\n]+` + bt + `|"[^"\n]+"|'[^'\n]+'|` +
		`(?:~/|/|[A-Za-z]:[/\\])[^\s\n` + bt + `"']+?)` +
		`(?=[` + bt + `"'\s,;:)\]}` + mediaCJKTerminators + `]|MEDIA:|$)` +
		`[` + bt + `"'*_]{0,3}\s*`)
	fencedCodeRE     = newPyRE("(?s)```[^\\n]*\\n.*?```")
	inlineCodeRE     = newPyRE(bt + "[^" + bt + "\\n]+" + bt)
	mediaTrailRE     = newPyRE(`MEDIA:\s*$`)
	blockquoteLineRE = newPyRE(`(?m)^>.*$`)
	jsonValueStrRE   = newPyRE(`(?<=[:,{\[])\s*"((?:[^"\\\n]|\\.)*)"`)
	jsonBareMediaRE  = newPyRE(`MEDIA:\s*(?:~/|/|[A-Za-z]:[/\\])`)
	blankLineRunRE   = regexp.MustCompile(`\n{3,}`)
)

// Bare extensions longest first so a shorter one never matches as a prefix of a longer one
func mediaExtAlternation() string {
	exts := make([]string, len(MediaDeliveryExts))
	for i, e := range MediaDeliveryExts {
		exts[i] = strings.TrimLeft(e, ".")
	}
	sort.SliceStable(exts, func(i, j int) bool { return len([]rune(exts[i])) > len([]rune(exts[j])) })
	return strings.Join(exts, "|")
}

// Validated on disk path of an extensionless tag and the offset past it, extended across single spaces
func matchExtensionlessPath(ctx context.Context, scan []rune, m *regexp2.Match) (string, int, bool, error) {
	path := normalizeMediaTagPath(mediaExtensionlessTagRE.group(m, "path"))
	if path == "" {
		return "", 0, false, nil
	}
	start, end, _ := mediaExtensionlessTagRE.span(m, "path")
	safe, err := ValidateMediaDeliveryPath(ctx, path, "")
	if err != nil || safe != "" {
		return safe, end, safe != "", err
	}
	segment := scan[start:]
	for i, r := range segment {
		if r == '\n' {
			segment = segment[:i]
			break
		}
	}
	if nxt := runeIndexFrom(segment, []rune("MEDIA:"), 1); nxt != -1 {
		segment = segment[:nxt]
	}
	pos := end - start
	for range 8 {
		tokEnd, ok := spaceToken(segment, pos)
		if !ok {
			break
		}
		safe, err := ValidateMediaDeliveryPath(ctx, normalizeMediaTagPath(string(segment[:tokEnd])), "")
		if err != nil || safe != "" {
			return safe, start + tokEnd, safe != "", err
		}
		pos = tokEnd
	}
	return "", 0, false, nil
}

// End of a run of spaces and tabs followed by at least one other rune, ok false when none follows
func spaceToken(seg []rune, pos int) (int, bool) {
	i := pos
	for i < len(seg) && (seg[i] == ' ' || seg[i] == '\t') {
		i++
	}
	j := i
	for j < len(seg) && seg[j] != ' ' && seg[j] != '\t' {
		j++
	}
	return j, j > i
}

// Rune index of needle in hay at or after from, minus one when absent
func runeIndexFrom(hay, needle []rune, from int) int {
	for i := from; i+len(needle) <= len(hay); i++ {
		if string(hay[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

// Tag path with its quotes and trailing punctuation removed
func normalizeMediaTagPath(raw string) string {
	path := pystr.Strip(raw)
	if r := []rune(path); len(r) >= 2 && r[0] == r[len(r)-1] && strings.ContainsRune("`\"'", r[0]) {
		path = pystr.Strip(string(r[1 : len(r)-1]))
	}
	return strings.TrimRight(strings.TrimLeft(path, "`\"'"), "`\"',.;:)}]")
}

// Whether the path has no extension or one outside the deliverable set
func pathLacksDeliverableExtension(path string) bool {
	ext := pystr.Lower(pypath.PathSuffix(path))
	for _, e := range MediaDeliveryExts {
		if e == ext {
			return false
		}
	}
	return true
}

// Whether the text carries a media tag or a delivery directive
func hasMediaDirectives(text string) bool {
	return strings.Contains(text, "MEDIA:") || strings.Contains(text, "[[audio_as_voice]]") || strings.Contains(text, "[[as_document]]")
}

// Rune offset where the run of end of sequence tokens closing the text begins, minus one when absent
func terminalSentinelStart(text []rune) int {
	end := len([]rune(pystr.Rstrip(string(text))))
	start := end
	n := len([]rune(terminalSentinel))
	for start >= n && string(text[start-n:start]) == terminalSentinel {
		start -= n
	}
	if start < end {
		return start
	}
	return -1
}

// Offset preserving mask of protected spans, JSON string values and a terminal sentinel
func maskMediaScanText(ctx context.Context, text []rune) ([]rune, error) {
	protected, err := maskProtectedSpans(ctx, text)
	if err != nil {
		return nil, err
	}
	masked := maskJSONStringMedia(protected)
	if start := terminalSentinelStart(text); start >= 0 {
		masked = blankSpans(masked, []runeSpan{{start, len([]rune(pystr.Rstrip(string(text))))}})
	}
	return masked, nil
}

// Deliverable tag spans of the text plus a terminal sentinel when any tag is found
func deliverableTagSpans(ctx context.Context, text []rune) ([]runeSpan, error) {
	masked, err := maskMediaScanText(ctx, text)
	if err != nil {
		return nil, err
	}
	spans, err := realMediaTagSpans(ctx, masked)
	if err != nil {
		return nil, err
	}
	if start := terminalSentinelStart(text); len(spans) > 0 && start >= 0 {
		spans = append(spans, runeSpan{start, len([]rune(pystr.Rstrip(string(text))))})
	}
	return spans, nil
}

type extensionlessHit struct {
	start int
	safe  string
	end   int
}

// Extensionless or unknown extension tags whose path validates for delivery
func extensionlessMediaMatches(ctx context.Context, masked []rune) ([]extensionlessHit, error) {
	var out []extensionlessHit
	for _, m := range mediaExtensionlessTagRE.finditer(masked) {
		path := normalizeMediaTagPath(mediaExtensionlessTagRE.group(m, "path"))
		if path == "" || !pathLacksDeliverableExtension(path) {
			continue
		}
		safe, end, ok, err := matchExtensionlessPath(ctx, masked, m)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, extensionlessHit{m.RuneIndex, safe, end})
		}
	}
	return out, nil
}

// Spans of deliverable tags on a masked copy, unknown extensions only when they validate
func realMediaTagSpans(ctx context.Context, masked []rune) ([]runeSpan, error) {
	var spans []runeSpan
	for _, m := range mediaTagCleanupRE.finditer(masked) {
		spans = append(spans, runeSpan{m.RuneIndex, m.RuneIndex + m.RuneLength})
	}
	hits, err := extensionlessMediaMatches(ctx, masked)
	if err != nil {
		return nil, err
	}
	for _, h := range hits {
		spans = append(spans, runeSpan{h.start, h.end})
	}
	return spans, nil
}

// Copy of the text with every non newline rune inside the spans turned into a space
func blankSpans(text []rune, spans []runeSpan) []rune {
	out := append([]rune(nil), text...)
	for _, s := range spans {
		for i := s.start; i < s.end && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	return out
}

// Text with the spans deleted, overlapping and nested spans merged first
func deleteSpans(text []rune, spans []runeSpan) []rune {
	if len(spans) == 0 {
		return text
	}
	sorted := append([]runeSpan(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start < sorted[j].start
		}
		return sorted[i].end < sorted[j].end
	})
	var merged []runeSpan
	for _, s := range sorted {
		if n := len(merged); n > 0 && s.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, s.end)
		} else {
			merged = append(merged, s)
		}
	}
	out := append([]rune(nil), text...)
	for i := len(merged) - 1; i >= 0; i-- {
		out = append(out[:merged[i].start], out[merged[i].end:]...)
	}
	return out
}

// Text without its media tags and delivery markers, protected spans left intact
func stripMediaTagDirectives(ctx context.Context, text string) (string, error) {
	if text == "" || !hasMediaDirectives(text) {
		return text, nil
	}
	cleaned := []rune(strings.ReplaceAll(strings.ReplaceAll(text, "[[audio_as_voice]]", ""), "[[as_document]]", ""))
	spans, err := deliverableTagSpans(ctx, cleaned)
	if err != nil {
		return "", err
	}
	return string(deleteSpans(cleaned, spans)), nil
}

// Code blocks, inline code and blockquotes blanked, inline code holding a deliverable tag kept scannable
func maskProtectedSpans(ctx context.Context, content []rune) ([]rune, error) {
	var spans []runeSpan
	for _, m := range fencedCodeRE.finditer(content) {
		spans = append(spans, runeSpan{m.RuneIndex, m.RuneIndex + m.RuneLength})
	}
	for _, m := range inlineCodeRE.finditer(content) {
		start, end := m.RuneIndex, m.RuneIndex+m.RuneLength
		if mediaTrailRE.search(string(content[max(0, start-20):start])) {
			continue
		}
		inner := pystr.Strip(string(content[start+1 : end-1]))
		if strings.HasPrefix(pystr.Upper(inner), "MEDIA:") {
			if candidate := normalizeMediaTagPath(string([]rune(inner)[6:])); candidate != "" {
				safe, err := ValidateMediaDeliveryPath(ctx, candidate, "")
				if err != nil {
					return nil, err
				}
				if safe != "" {
					continue
				}
			}
		}
		spans = append(spans, runeSpan{start, end})
	}
	for _, m := range blockquoteLineRE.finditer(content) {
		spans = append(spans, runeSpan{m.RuneIndex, m.RuneIndex + m.RuneLength})
	}
	return blankSpans(content, spans), nil
}

// Bare path media tags inside JSON string values blanked so stored tool text is never delivered again
func maskJSONStringMedia(content []rune) []rune {
	s := string(content)
	if !strings.Contains(s, `"`) || !strings.Contains(s, "MEDIA:") {
		return content
	}
	var spans []runeSpan
	for _, m := range jsonValueStrRE.finditer(content) {
		g := m.GroupByNumber(1)
		if g != nil && len(g.Captures) > 0 && jsonBareMediaRE.search(g.String()) {
			spans = append(spans, runeSpan{g.RuneIndex, g.RuneIndex + g.RuneLength})
		}
	}
	return blankSpans(content, spans)
}

// Media path of a response and whether it goes out as a voice message
type MediaRef struct {
	Path    string
	IsVoice bool
}

// Media tags of the content and the content with them and the delivery markers removed
func ExtractMedia(ctx context.Context, content string) ([]MediaRef, string, error) {
	var media []MediaRef
	hasVoice := strings.Contains(content, "[[audio_as_voice]]")
	cleaned := strings.ReplaceAll(strings.ReplaceAll(content, "[[audio_as_voice]]", ""), "[[as_document]]", "")
	scan, err := maskMediaScanText(ctx, []rune(content))
	if err != nil {
		return nil, "", err
	}
	seen := map[string]bool{}
	add := func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		_, ext := pypath.Splitext(path)
		media = append(media, MediaRef{path, hasVoice && audioExts[pystr.Lower(ext)]})
	}
	for _, m := range mediaTagCleanupRE.finditer(scan) {
		path := normalizeMediaTagPath(mediaTagCleanupRE.group(m, "path"))
		if path == "" {
			continue
		}
		if expanduserRaises(path) != nil {
			continue
		}
		add(pypath.Expanduser(path))
	}
	hits, err := extensionlessMediaMatches(ctx, scan)
	if err != nil {
		return nil, "", err
	}
	for _, h := range hits {
		add(h.safe)
	}
	if len(media) > 0 {
		spans, err := deliverableTagSpans(ctx, []rune(cleaned))
		if err != nil {
			return nil, "", err
		}
		if len(spans) > 0 {
			cleaned = pystr.Strip(blankLineRunRE.ReplaceAllString(string(deleteSpans([]rune(cleaned), spans)), "\n\n"))
		}
	}
	return media, cleaned, nil
}

// Display text without media directives, extensionless tags hidden only when they validate
func StripMediaDirectivesForDisplay(ctx context.Context, text string) (string, error) {
	if !hasMediaDirectives(text) {
		return text, nil
	}
	stripped, err := stripMediaTagDirectives(ctx, text)
	if err != nil {
		return "", err
	}
	return pystr.Rstrip(blankLineRunRE.ReplaceAllString(stripped, "\n\n")), nil
}
