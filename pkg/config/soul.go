package config

import (
	"errors"
	"os"
	"strings"

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

// Writes the default SOUL.md when the home has none or only a seeded one
func seedSoul(h string) error {
	path := home.SoulPath(h)
	existing, err := os.ReadFile(path)
	if err == nil && !IsSeededSoul(string(existing)) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && normalizeSoul(string(existing)) == normalizeSoul(DefaultIdentity) {
		return nil
	}
	return AtomicWrite(path, []byte(DefaultIdentity+"\n"), 0o600)
}
