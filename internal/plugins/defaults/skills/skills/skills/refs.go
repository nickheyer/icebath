package skills

import (
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// Folders of a skill whose files its SKILL.md may name for the hub to fetch with it
var supportFolders = map[string]bool{"references": true, "templates": true, "scripts": true, "assets": true, "examples": true}

var (
	localLink = regexp.MustCompile("(?m)(?:\\]\\(|`|(?:^|[\\s\"']))((?:references|templates|scripts|assets|examples)/[^\\s)`\"'<>]+)")
	// A path into a support folder that climbs back out of it
	climbing    = regexp.MustCompile("(?:references|templates|scripts|assets|examples)/(?:[^\\s)`\"'<>]*/)?\\.\\.(?:/|$)")
	flagOnly    = regexp.MustCompile(`^(?:[A-Za-z0-9_~-]|%[0-9A-Fa-f]{2})+$`)
	sameDirLink = regexp.MustCompile(`\]\(([^)\s"'<>]+)`)
	sameDirName = regexp.MustCompile(`^(?:\./)?[A-Za-z0-9][A-Za-z0-9._-]*$`)
	globChars   = regexp.MustCompile(`[*?<>]`)
	alnumEnd    = regexp.MustCompile(`[A-Za-z0-9]$`)
)

// Path and query of a link, the fragment dropped
func splitLink(link string) (string, string) {
	link, _, _ = strings.Cut(link, "#")
	path, query, _ := strings.Cut(link, "?")
	return path, query
}

// Text with its percent escapes decoded, as it is when one is malformed
func unescape(text string) string {
	if decoded, err := url.PathUnescape(text); err == nil {
		return decoded
	}
	return text
}

// Whether a link's query is real key=value or flag syntax rather than a glob in prose
func concrete(query string) bool {
	for _, part := range strings.Split(query, "&") {
		if key, _, ok := strings.Cut(part, "="); ok && key != "" {
			continue
		}
		if !flagOnly.MatchString(part) {
			return false
		}
	}
	return true
}

// Files beside a SKILL.md it names for the hub to fetch with it: in its support folders, or in its own folder; false when it tries to
// climb out of them. Names that differ only in case are dropped, since they would land on one file of a case blind disk
func referenced(skillMD string) (map[string]bool, bool) {
	text := strings.ReplaceAll(skillMD, `\`, "/")
	if climbing.MatchString(text) {
		return nil, false
	}
	paths := map[string]bool{}
	for _, match := range localLink.FindAllStringSubmatch(text, -1) {
		candidate := strings.TrimRight(match[1], ".,;:")
		path, query := splitLink(candidate)
		raw := unescape(path)
		if strings.HasSuffix(candidate, "?") || strings.ContainsAny(raw, "*?[]") || query != "" && !concrete(query) {
			continue
		}
		safe, err := bundlePath(raw)
		if err != nil {
			return nil, false
		}
		base := safe[strings.LastIndex(safe, "/")+1:]
		if supportFolders[strings.SplitN(safe, "/", 2)[0]] && !globChars.MatchString(safe) && alnumEnd.MatchString(base) {
			paths[safe] = true
		}
	}
	for _, match := range sameDirLink.FindAllStringSubmatch(text, -1) {
		raw := strings.TrimRight(match[1], ".,;:")
		path, _ := splitLink(raw)
		name := strings.TrimPrefix(unescape(path), "./")
		if name == "" || strings.Contains(raw, "://") || strings.HasPrefix(raw, "mailto:") || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "/") {
			continue
		}
		if strings.HasPrefix(name, "..") {
			slog.Warn("SKILL.md links outside the skill directory; installing without it", "link", raw)
			continue
		}
		if strings.Contains(name, "/") || strings.ToLower(name) == "skill.md" || !strings.Contains(strings.TrimLeft(name, "."), ".") || !sameDirName.MatchString(name) {
			continue
		}
		safe, err := bundlePath(name)
		if err != nil {
			return nil, false
		}
		paths[safe] = true
	}
	folded := map[string][]string{}
	for p := range paths {
		folded[strings.ToLower(p)] = append(folded[strings.ToLower(p)], p)
	}
	for _, group := range folded {
		if len(group) > 1 {
			for _, p := range group {
				delete(paths, p)
			}
		}
	}
	return paths, true
}
