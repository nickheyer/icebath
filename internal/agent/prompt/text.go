package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

func isSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func trim(s string) string { return strings.TrimFunc(s, isSpace) }

func lineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

func splitLinesKeep(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if !lineBreak(r) {
			continue
		}
		if r == '\r' && i < len(s) && s[i] == '\n' {
			i++
		}
		out = append(out, s[start:i])
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func splitLines(s string) []string {
	lines := splitLinesKeep(s)
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, lineBreak)
	}
	return lines
}

func joinNonEmpty(parts []string, sep string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func joinTier(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := trim(p); t != "" {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, "\n\n")
}

func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, rest := abs, ""
	for {
		parent := filepath.Dir(dir)
		if rest == "" {
			rest = filepath.Base(dir)
		} else {
			rest = filepath.Join(filepath.Base(dir), rest)
		}
		if parent == dir {
			return abs
		}
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(real, rest)
		}
		dir = parent
	}
}

func ancestors(p string) []string {
	out := []string{p}
	for {
		parent := filepath.Dir(p)
		if parent == p {
			return out
		}
		out = append(out, parent)
		p = parent
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func within(child, parent string) (string, bool) {
	rel, err := filepath.Rel(parent, child)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func roughTokens(text string) int {
	if text == "" {
		return 0
	}
	dense, rest := 0, 0
	for _, r := range text {
		switch {
		case r >= 0x1100 && r <= 0x11ff, r >= 0x2e80 && r <= 0x9fff, r >= 0xa960 && r <= 0xa97f,
			r >= 0xac00 && r <= 0xd7af, r >= 0xf900 && r <= 0xfaff, r >= 0xff00 && r <= 0xffef:
			dense++
		default:
			rest += utf8.RuneLen(r)
		}
	}
	return dense + (rest+3)/4
}
