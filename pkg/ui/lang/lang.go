// Package lang gives the kit the text of a string key in the language of the application
package lang

import (
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Function of the application that turns a catalog key and its arguments into text
var source atomic.Pointer[func(key txt.Key, args ...any) string]

// Install sets the function the kit asks for the text of a catalog key
func Install(text func(key txt.Key, args ...any) string) {
	source.Store(&text)
}

// Language id the application shows its text in, nil while it has named none
var language atomic.Pointer[string]

// Use names the language the application shows its text in, so capitals follow its casing rules
func Use(id string) { language.Store(&id) }

// Upper is the text in capitals by the casing rules of the language of the application, Turkish and Azerbaijani keeping the dot of their i
func Upper(text string) string {
	if id := language.Load(); id != nil {
		if base, _, _ := strings.Cut(*id, "-"); base == "tr" || base == "az" {
			return strings.ToUpperSpecial(unicode.TurkishCase, text)
		}
	}
	return strings.ToUpper(text)
}

// T is the text of a catalog key in the active language, the name of the key while the application has installed no source
func T(key txt.Key, args ...any) string {
	if text := source.Load(); text != nil {
		return (*text)(key, args...)
	}
	return key.Name()
}

// Or is the text of the catalog entry with a name made at run time, the fallback when the catalog holds no such entry or the application has installed no source
func Or(name, fallback string, args ...any) string {
	if key, known := txt.Named(name); known {
		if text := source.Load(); text != nil {
			return (*text)(key, args...)
		}
	}
	return fallback
}

// OfID is the text kept for an identifier between a key prefix and suffix, identifiers that end in a number sharing the text of n with the number as its argument, the fallback when the application has none
func OfID(prefix, id, suffix, fallback string) string {
	if cut := strings.LastIndexByte(id, '.'); cut >= 0 && id[cut+1:] != "" && strings.Trim(id[cut+1:], "0123456789") == "" {
		return Or(prefix+id[:cut]+".n"+suffix, fallback, id[cut+1:])
	}
	return Or(prefix+id+suffix, fallback)
}

// Function of the application naming the keys that fire an action, set once its keymap is loaded
var keyNamer atomic.Pointer[func(id string) string]

// InstallKeys sets the function that names the keys of an action for the {key:action.id} placeholders of catalog texts
func InstallKeys(name func(id string) string) {
	keyNamer.Store(&name)
}

// Keys is the text with every {key:action.id} placeholder replaced by the keys bound to that action, so a text names a key the way it is bound now
func Keys(text string) string {
	name := keyNamer.Load()
	if name == nil || !strings.Contains(text, "{key:") {
		return text
	}
	var out strings.Builder
	for {
		start := strings.Index(text, "{key:")
		if start < 0 {
			break
		}
		end := strings.IndexByte(text[start:], '}')
		if end < 0 {
			break
		}
		out.WriteString(text[:start])
		out.WriteString((*name)(text[start+len("{key:") : start+end]))
		text = text[start+end+1:]
	}
	out.WriteString(text)
	return out.String()
}
