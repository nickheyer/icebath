package config

import (
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
)

// Identity text used when a home carries no SOUL.md
const DefaultIdentity = "You are icebath, an agent that lives in the terminal. Be direct: match the length of your reply to the weight of " +
	"the ask, a one-line question gets a one-line answer, and finished work gets a short report of what " +
	"changed, what is verified, and what is left, never a replay of the process. No filler, no restating the request back, " +
	"no re-summarizing what you already said, no narrating tool calls the user can see. Plain claims over adjectives; when " +
	"unsure, say so plainly. Agree because it is right, not because the user said it. Depth is earned, give it when the user " +
	"asks for detail, teaches, or the stakes demand it, not by default."

// Seeded SOUL.md texts that carry no user intent and may be upgraded in place
var legacySouls = []string{DefaultIdentity}

// Text with line endings unified, the BOM dropped and surrounding whitespace trimmed
func normalizeSoul(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.TrimSpace(strings.TrimPrefix(text, "\xef\xbb\xbf"))
}

// Reports whether a SOUL.md still holds an auto seeded text nobody edited
func IsSeededSoul(text string) bool {
	n := normalizeSoul(text)
	for _, t := range legacySouls {
		if n == normalizeSoul(t) {
			return true
		}
	}
	return false
}

// Seeds the default SOUL.md on first run and upgrades an auto seeded one in place, never a customized file
func ensureDefaultSoulMD(h string) error {
	path := home.SoulPath(h)
	if _, err := os.Stat(path); err == nil {
		existing, rerr := os.ReadFile(path)
		if rerr != nil || !utf8.Valid(existing) {
			return nil
		}
		if !IsSeededSoul(string(existing)) {
			return nil
		}
	}
	if err := os.WriteFile(path, []byte(DefaultIdentity), 0o666); err != nil {
		if st, lerr := os.Lstat(path); lerr != nil || st.Mode()&os.ModeSymlink == 0 {
			return err
		}
		tmp, terr := os.CreateTemp(h, ".SOUL.md.*.seed")
		if terr != nil {
			return terr
		}
		if _, werr := tmp.WriteString(DefaultIdentity); werr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return werr
		}
		if cerr := tmp.Close(); cerr != nil {
			os.Remove(tmp.Name())
			return cerr
		}
		if rerr := os.Rename(tmp.Name(), path); rerr != nil {
			os.Remove(tmp.Name())
			return rerr
		}
	}
	SecureFile(h, path)
	return nil
}
