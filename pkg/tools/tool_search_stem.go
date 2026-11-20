package tools

import (
	"strings"
	"sync"
)

// Porter2 English stemmer so "issues" matches create_issue in the deferred tool catalog

var stemExceptions = map[string]string{
	"skis": "ski", "skies": "sky", "dying": "die", "lying": "lie", "tying": "tie",
	"idly": "idl", "gently": "gentl", "ugly": "ugli", "early": "earli", "only": "onli",
	"singly": "singl", "sky": "sky", "news": "news", "howe": "howe", "atlas": "atlas",
	"cosmos": "cosmos", "bias": "bias", "andes": "andes",
}

var stemExceptionsAfter1a = map[string]bool{
	"inning": true, "outing": true, "canning": true, "herring": true, "earring": true,
	"proceed": true, "exceed": true, "succeed": true,
}

var stemDoubles = []string{"bb", "dd", "ff", "gg", "mm", "nn", "pp", "rr", "tt"}

var stemLiEndings = "cdeghkmnrt"

var (
	stemCacheMu sync.Mutex
	stemCache   = map[string]string{}
)

// Memoized stem of one lowercase token
func stemToken(token string) string {
	stemCacheMu.Lock()
	if v, ok := stemCache[token]; ok {
		stemCacheMu.Unlock()
		return v
	}
	stemCacheMu.Unlock()
	out := porter2Stem(token)
	stemCacheMu.Lock()
	if len(stemCache) >= 16384 {
		stemCache = map[string]string{}
	}
	stemCache[token] = out
	stemCacheMu.Unlock()
	return out
}

func stemIsVowel(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u', 'y':
		return true
	}
	return false
}

// Region start after the first non vowel that follows a vowel from position from
func stemRegion(w string, from int) int {
	for i := from; i+1 < len(w); i++ {
		if stemIsVowel(w[i]) && !stemIsVowel(w[i+1]) {
			return i + 2
		}
	}
	return len(w)
}

// Short syllable test for the suffix of the word ending at the given length
func stemEndsShortSyllable(w string) bool {
	n := len(w)
	if n == 2 {
		return stemIsVowel(w[0]) && !stemIsVowel(w[1])
	}
	if n < 3 {
		return false
	}
	c := w[n-1]
	if stemIsVowel(c) || c == 'w' || c == 'x' || c == 'Y' {
		return false
	}
	return stemIsVowel(w[n-2]) && !stemIsVowel(w[n-3])
}

func stemHasVowel(s string) bool {
	for i := 0; i < len(s); i++ {
		if stemIsVowel(s[i]) {
			return true
		}
	}
	return false
}

// Longest listed suffix the word ends with or empty when none matches
func stemLongestSuffix(w string, suffixes []string) string {
	best := ""
	for _, s := range suffixes {
		if strings.HasSuffix(w, s) && len(s) > len(best) {
			best = s
		}
	}
	return best
}

func porter2Stem(word string) string {
	if len(word) <= 2 {
		return word
	}
	w := word
	if strings.HasPrefix(w, "'") {
		w = w[1:]
	}
	if v, ok := stemExceptions[w]; ok {
		return v
	}
	b := []byte(w)
	for i := 0; i < len(b); i++ {
		if b[i] == 'y' && (i == 0 || stemIsVowel(b[i-1])) {
			b[i] = 'Y'
		}
	}
	w = string(b)
	r1 := stemRegion(w, 0)
	for _, prefix := range []string{"gener", "commun", "arsen"} {
		if strings.HasPrefix(w, prefix) {
			r1 = len(prefix)
			break
		}
	}
	r2 := stemRegion(w, r1)
	inR1 := func(suffix string) bool { return len(w)-len(suffix) >= r1 }
	inR2 := func(suffix string) bool { return len(w)-len(suffix) >= r2 }

	// Step 0
	if s := stemLongestSuffix(w, []string{"'", "'s", "'s'"}); s != "" {
		w = w[:len(w)-len(s)]
	}

	// Step 1a
	switch s := stemLongestSuffix(w, []string{"sses", "ied", "ies", "s", "us", "ss"}); s {
	case "sses":
		w = w[:len(w)-2]
	case "ied", "ies":
		if len(w)-len(s) > 1 {
			w = w[:len(w)-len(s)] + "i"
		} else {
			w = w[:len(w)-len(s)] + "ie"
		}
	case "s":
		if len(w) >= 3 && stemHasVowel(w[:len(w)-2]) {
			w = w[:len(w)-1]
		}
	}
	if stemExceptionsAfter1a[w] {
		return w
	}

	// Step 1b
	switch s := stemLongestSuffix(w, []string{"eed", "eedly", "ed", "edly", "ing", "ingly"}); s {
	case "eed", "eedly":
		if inR1(s) {
			w = w[:len(w)-len(s)] + "ee"
		}
	case "ed", "edly", "ing", "ingly":
		stem := w[:len(w)-len(s)]
		if stemHasVowel(stem) {
			w = stem
			switch {
			case strings.HasSuffix(w, "at") || strings.HasSuffix(w, "bl") || strings.HasSuffix(w, "iz"):
				w += "e"
			case stemLongestSuffix(w, stemDoubles) != "":
				w = w[:len(w)-1]
			case stemEndsShortSyllable(w) && r1 >= len(w):
				w += "e"
			}
		}
	}

	// Step 1c
	if n := len(w); n >= 3 && (w[n-1] == 'y' || w[n-1] == 'Y') && !stemIsVowel(w[n-2]) {
		w = w[:n-1] + "i"
	}

	// Step 2
	step2 := []string{"tional", "enci", "anci", "abli", "entli", "izer", "ization", "ational",
		"ation", "ator", "alism", "aliti", "alli", "fulness", "ousli", "ousness", "iveness",
		"iviti", "biliti", "bli", "ogi", "fulli", "lessli", "li"}
	if s := stemLongestSuffix(w, step2); s != "" && inR1(s) {
		stem := w[:len(w)-len(s)]
		switch s {
		case "tional":
			w = stem + "tion"
		case "enci":
			w = stem + "ence"
		case "anci":
			w = stem + "ance"
		case "abli":
			w = stem + "able"
		case "entli":
			w = stem + "ent"
		case "izer", "ization":
			w = stem + "ize"
		case "ational", "ation", "ator":
			w = stem + "ate"
		case "alism", "aliti", "alli":
			w = stem + "al"
		case "fulness":
			w = stem + "ful"
		case "ousli", "ousness":
			w = stem + "ous"
		case "iveness", "iviti":
			w = stem + "ive"
		case "biliti", "bli":
			w = stem + "ble"
		case "ogi":
			if strings.HasSuffix(stem, "l") {
				w = stem + "og"
			}
		case "fulli":
			w = stem + "ful"
		case "lessli":
			w = stem + "less"
		case "li":
			if len(stem) > 0 && strings.IndexByte(stemLiEndings, stem[len(stem)-1]) >= 0 {
				w = stem
			}
		}
	}

	// Step 3
	step3 := []string{"tional", "ational", "alize", "icate", "iciti", "ical", "ful", "ness", "ative"}
	if s := stemLongestSuffix(w, step3); s != "" && inR1(s) {
		stem := w[:len(w)-len(s)]
		switch s {
		case "tional":
			w = stem + "tion"
		case "ational":
			w = stem + "ate"
		case "alize":
			w = stem + "al"
		case "icate", "iciti", "ical":
			w = stem + "ic"
		case "ful", "ness":
			w = stem
		case "ative":
			if inR2(s) {
				w = stem
			}
		}
	}

	// Step 4
	step4 := []string{"al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement", "ment",
		"ent", "ism", "ate", "iti", "ous", "ive", "ize", "ion"}
	if s := stemLongestSuffix(w, step4); s != "" && inR2(s) {
		stem := w[:len(w)-len(s)]
		if s == "ion" {
			if strings.HasSuffix(stem, "s") || strings.HasSuffix(stem, "t") {
				w = stem
			}
		} else {
			w = stem
		}
	}

	// Step 5
	if strings.HasSuffix(w, "e") {
		if inR2("e") {
			w = w[:len(w)-1]
		} else if inR1("e") && !stemEndsShortSyllable(w[:len(w)-1]) {
			w = w[:len(w)-1]
		}
	} else if strings.HasSuffix(w, "l") && inR2("l") && strings.HasSuffix(w[:len(w)-1], "l") {
		w = w[:len(w)-1]
	}
	return strings.ReplaceAll(w, "Y", "y")
}
