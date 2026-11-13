package chrome

import (
	"regexp"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

// Longest word the dictionary door accepts, in UTF-16 code units
const wordMax = 64

// One word of letters, marks and digits with apostrophes and hyphens only inside
var (
	wordRE   = regexp.MustCompile(`^[\pL\pM\pN](?:[\pL\pM\pN'\x{2019}-]*[\pL\pM\pN])?$`)
	letterRE = regexp.MustCompile(`\pL`)
)

// Spell checker word list of a webview session
type Dictionary interface {
	// Adds a word, false when the session keeps no persistent list
	AddWord(word string) bool
	// Removes a word, false when the session keeps no persistent list
	RemoveWord(word string) bool
}

// Webview guest a renderer names by its id
type Guest interface {
	IsDestroyed() bool
	Dictionary() Dictionary
}

// Exactly one bounded NFC word holding a letter, false for anything else
func NormalizeDictionaryWord(raw any) (string, bool) {
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	word := norm.NFC.String(jsTrim(s))
	if len(utf16.Encode([]rune(word))) > wordMax || !wordRE.MatchString(word) || !letterRE.MatchString(word) {
		return "", false
	}
	return word, true
}

// Replaces the misspelling under the menu or adds the word to the sender session dictionary
func (s *Service) ContextMenuSpellcheck(w SpellingEditor, action any) {
	word := jsOr(get(action, "word"), "")
	if word == "" {
		return
	}
	switch get(action, "kind") {
	case "replace":
		w.ReplaceMisspelling(word)
	case "add":
		s.opts.Dictionary.AddWord(word)
	}
}

// Adds a word to the session dictionary of the webview guest the payload names
func (s *Service) ContextMenuGuestAddWord(payload any) {
	word := jsOr(get(payload, "word"), "")
	guest := s.opts.Guests(jsNumber(get(payload, "webContentsId")))
	if word != "" && guest != nil && !guest.IsDestroyed() {
		guest.Dictionary().AddWord(word)
	}
}

// Adds or removes one normalized word for the plugin spellcheck door, the session verdict or false when refused
func (s *Service) SpellcheckDictionary(payload any) bool {
	word, ok := NormalizeDictionaryWord(get(payload, "word"))
	if !ok {
		return false
	}
	switch op := get(payload, "op"); op {
	case "add":
		return s.opts.Dictionary.AddWord(word)
	case "remove":
		return s.opts.Dictionary.RemoveWord(word)
	}
	return false
}
