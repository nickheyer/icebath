package redact

import (
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// What a value filled from the vault shows as
const filledMark = "«redacted-vault-secret»"

// Most filled values a profile keeps, the oldest dropped first
const filledKept = 64

// Values the autofill vault wrote into web pages, by profile, scrubbed from every text a tool of that profile hands the model whatever
// its settings say, so a password a page echoes never reaches the conversation
var filled = struct {
	mu     sync.Mutex
	values map[string][]string
}{values: map[string][]string{}}

// Remembers a value the vault filled for the profile, and its form with line breaks taken out
func Fill(profile, value string) {
	if value == "" {
		return
	}
	stripped := strings.NewReplacer("\r", "", "\n", "").Replace(value)
	filled.mu.Lock()
	defer filled.mu.Unlock()
	kept := filled.values[profile]
	for _, v := range []string{value, stripped} {
		if v == "" {
			continue
		}
		kept = append(slices.DeleteFunc(kept, func(old string) bool { return old == v }), v)
	}
	if len(kept) > filledKept {
		kept = kept[len(kept)-filledKept:]
	}
	filled.values[profile] = kept
}

// Text with every value the vault filled for the profile replaced, the longest first so no part of one is left behind
func Filled(profile, text string) string {
	if text == "" {
		return text
	}
	filled.mu.Lock()
	values := slices.Clone(filled.values[profile])
	filled.mu.Unlock()
	slices.SortStableFunc(values, func(a, b string) int { return utf8.RuneCountInString(b) - utf8.RuneCountInString(a) })
	for _, v := range values {
		text = strings.ReplaceAll(text, v, filledMark)
	}
	return text
}

// Drops the values the vault filled for the profile, as it is deleted
func Forget(profile string) {
	filled.mu.Lock()
	defer filled.mu.Unlock()
	delete(filled.values, profile)
}

// Moves the values the vault filled for a profile to its new name, as it is renamed
func Rename(old, renamed string) {
	filled.mu.Lock()
	defer filled.mu.Unlock()
	if values, ok := filled.values[old]; ok {
		filled.values[renamed] = append(filled.values[renamed], values...)
		delete(filled.values, old)
	}
}
