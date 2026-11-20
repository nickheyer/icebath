package gateway

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
)

var (
	pendingMediaRe = pyre.MustCompile(`(?i)MEDIA:\s*(?:$|[` + "`" + `"'~/\\]|[A-Za-z](?::|$))` +
		`|(?<![A-Za-z0-9])[` + "`" + `"'*_]{0,3}M(?:E(?:D(?:I(?:A)?)?)?)?$`)
	pendingFlagRe  = regexp.MustCompile(`\[\[?[A-Za-z_]*\]?$`)
	streamFlags    = []string{"[[audio_as_voice]]", "[[as_document]]"}
	pendingImageRe = pyre.MustCompile(`!\[[^\]]*(?:\](?:\([^)\s]*)?)?\Z|(?<![\w)\]"'”’!?.*_~` + "`" + `])!\Z` +
		`|<(?:i(?:m(?:g(?:\s+(?:s(?:r(?:c(?:=["']?[^\s"'<>]*["']?\s*/?)?)?)?)?)?)?)?)?\Z` +
		`|</(?:i(?:m(?:g)?)?)?\Z`)
	mdImageTagLineRe   = regexp.MustCompile(`(?:` + MDImageTagPattern.String() + `)(?:\r?\n){0,2}`)
	htmlImageTagLineRe = regexp.MustCompile(`(?:` + HTMLImageTagPattern.String() + `)(?:\r?\n){0,2}`)
)

// Text with the image tags delivered as attachments hidden along with up to two line breaks after each
func StripImageTagsForDisplay(text string) string {
	if !strings.Contains(text, "![") && !strings.Contains(text, "<img") {
		return text
	}
	images, _ := ExtractImages(text)
	if len(images) == 0 {
		return text
	}
	urls := map[string]bool{}
	for _, img := range images {
		urls[img.URL] = true
	}
	for _, p := range []struct {
		re    *regexp.Regexp
		group int
	}{{mdImageTagLineRe, 2}, {htmlImageTagLineRe, 1}} {
		text = p.re.ReplaceAllStringFunc(text, func(match string) string {
			if urls[p.re.FindStringSubmatch(match)[p.group]] {
				return ""
			}
			return match
		})
	}
	return text
}

// True while the end of the cleaned streamed text may still become a hidden attachment
func HasPendingMediaDirective(cleaned string) bool {
	line := cleaned
	if i := strings.LastIndex(cleaned, "\n"); i >= 0 {
		line = cleaned[i+1:]
	}
	if ok, err := pendingMediaRe.MatchString(line); err == nil && ok {
		return true
	}
	if ok, err := pendingImageRe.MatchString(cleaned); err == nil && ok {
		return true
	}
	flag := pendingFlagRe.FindString(line)
	if flag == "" && !pendingFlagRe.MatchString(line) {
		return false
	}
	for _, f := range streamFlags {
		if strings.HasPrefix(f, flag) {
			return true
		}
	}
	return false
}
