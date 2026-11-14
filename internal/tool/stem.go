package tool

import "strings"

var stemExceptions1 = map[string]string{
	"skis": "ski", "skies": "sky", "idly": "idl", "gently": "gentl", "ugly": "ugli", "early": "earli",
	"only": "onli", "singly": "singl", "sky": "sky", "news": "news", "howe": "howe", "atlas": "atlas",
	"cosmos": "cosmos", "bias": "bias", "andes": "andes", "dying": "die", "lying": "lie", "tying": "tie",
}

var stemExceptions2 = map[string]bool{
	"inning": true, "outing": true, "canning": true, "herring": true, "earring": true,
	"proceed": true, "exceed": true, "succeed": true,
}

var stemRegionPrefixes = []string{"gener", "commun", "arsen", "past", "univers", "later", "emerg", "organ"}

type stemmer struct {
	w      []byte
	p1, p2 int
}

func stemVowel(c byte) bool {
	switch c {
	case 'a', 'e', 'i', 'o', 'u', 'y':
		return true
	}
	return false
}

func stemWord(word string) string {
	if v, ok := stemExceptions1[word]; ok {
		return v
	}
	if len(word) < 3 {
		return word
	}
	s := &stemmer{w: []byte(word)}
	s.prelude()
	s.markRegions()
	s.step1a()
	if stemExceptions2[string(s.w)] {
		return s.postlude()
	}
	s.step1b()
	s.step1c()
	s.step2()
	s.step3()
	s.step4()
	s.step5()
	return s.postlude()
}

func (s *stemmer) prelude() {
	if len(s.w) > 0 && s.w[0] == '\'' {
		s.w = s.w[1:]
	}
	for i := range s.w {
		if s.w[i] == 'y' && (i == 0 || stemVowel(s.w[i-1])) {
			s.w[i] = 'Y'
		}
	}
}

func (s *stemmer) markRegions() {
	n := len(s.w)
	s.p1, s.p2 = n, n
	word := string(s.w)
	start := -1
	for _, p := range stemRegionPrefixes {
		if strings.HasPrefix(word, p) {
			start = len(p)
			break
		}
	}
	if start < 0 {
		start = s.regionAfter(0)
	}
	s.p1 = start
	s.p2 = s.regionAfter(s.p1)
}

func (s *stemmer) regionAfter(from int) int {
	for i := from; i < len(s.w); i++ {
		if stemVowel(s.w[i]) {
			for j := i + 1; j < len(s.w); j++ {
				if !stemVowel(s.w[j]) {
					return j + 1
				}
			}
			return len(s.w)
		}
	}
	return len(s.w)
}

func (s *stemmer) ends(suffix string) bool {
	return strings.HasSuffix(string(s.w), suffix)
}

func (s *stemmer) longest(suffixes ...string) string {
	best := ""
	for _, suf := range suffixes {
		if len(suf) > len(best) && s.ends(suf) {
			best = suf
		}
	}
	return best
}

func (s *stemmer) inR1(suffix string) bool { return len(s.w)-len(suffix) >= s.p1 }

func (s *stemmer) inR2(suffix string) bool { return len(s.w)-len(suffix) >= s.p2 }

func (s *stemmer) replace(suffix, with string) {
	s.w = append(s.w[:len(s.w)-len(suffix)], with...)
}

func (s *stemmer) shortSyllableAt(end int) bool {
	if end >= 3 {
		c := s.w[end-1]
		if !stemVowel(c) && c != 'w' && c != 'x' && c != 'Y' && stemVowel(s.w[end-2]) && !stemVowel(s.w[end-3]) {
			return true
		}
	}
	return end == 2 && stemVowel(s.w[0]) && !stemVowel(s.w[1])
}

func (s *stemmer) step1a() {
	suf := s.longest("'", "'s", "'s'")
	if suf != "" {
		s.replace(suf, "")
	}
	switch suf := s.longest("sses", "ied", "ies", "s", "us", "ss"); suf {
	case "sses":
		s.replace(suf, "ss")
	case "ied", "ies":
		if len(s.w)-len(suf) > 1 {
			s.replace(suf, "i")
		} else {
			s.replace(suf, "ie")
		}
	case "s":
		for i := 0; i < len(s.w)-2; i++ {
			if stemVowel(s.w[i]) {
				s.replace("s", "")
				break
			}
		}
	}
}

func (s *stemmer) step1b() {
	switch suf := s.longest("eed", "eedly", "ed", "edly", "ing", "ingly"); suf {
	case "eed", "eedly":
		if s.inR1(suf) {
			s.replace(suf, "ee")
		}
	case "ed", "edly", "ing", "ingly":
		stem := len(s.w) - len(suf)
		vowel := false
		for i := 0; i < stem; i++ {
			if stemVowel(s.w[i]) {
				vowel = true
				break
			}
		}
		if !vowel {
			return
		}
		s.replace(suf, "")
		switch {
		case s.ends("at") || s.ends("bl") || s.ends("iz"):
			s.w = append(s.w, 'e')
		case s.longest("bb", "dd", "ff", "gg", "mm", "nn", "pp", "rr", "tt") != "":
			s.w = s.w[:len(s.w)-1]
		case s.p1 == len(s.w) && s.shortSyllableAt(len(s.w)):
			s.w = append(s.w, 'e')
		}
	}
}

func (s *stemmer) step1c() {
	n := len(s.w)
	if n > 2 && (s.w[n-1] == 'y' || s.w[n-1] == 'Y') && !stemVowel(s.w[n-2]) {
		s.w[n-1] = 'i'
	}
}

func (s *stemmer) step2() {
	suf := s.longest("tional", "enci", "anci", "abli", "entli", "izer", "ization", "ational", "ation", "ator",
		"alism", "aliti", "alli", "fulness", "ousli", "ousness", "iveness", "iviti", "biliti", "bli", "ogi",
		"fulli", "lessli", "li")
	if suf == "" || !s.inR1(suf) {
		return
	}
	switch suf {
	case "tional":
		s.replace(suf, "tion")
	case "enci":
		s.replace(suf, "ence")
	case "anci":
		s.replace(suf, "ance")
	case "abli":
		s.replace(suf, "able")
	case "entli":
		s.replace(suf, "ent")
	case "izer", "ization":
		s.replace(suf, "ize")
	case "ational", "ation", "ator":
		s.replace(suf, "ate")
	case "alism", "aliti", "alli":
		s.replace(suf, "al")
	case "fulness":
		s.replace(suf, "ful")
	case "ousli", "ousness":
		s.replace(suf, "ous")
	case "iveness", "iviti":
		s.replace(suf, "ive")
	case "biliti", "bli":
		s.replace(suf, "ble")
	case "ogi":
		if len(s.w) > 3 && s.w[len(s.w)-4] == 'l' {
			s.replace(suf, "og")
		}
	case "fulli":
		s.replace(suf, "ful")
	case "lessli":
		s.replace(suf, "less")
	case "li":
		if len(s.w) > 2 && strings.IndexByte("cdeghkmnrt", s.w[len(s.w)-3]) >= 0 {
			s.replace(suf, "")
		}
	}
}

func (s *stemmer) step3() {
	suf := s.longest("tional", "ational", "alize", "icate", "iciti", "ical", "ful", "ness", "ative")
	if suf == "" || !s.inR1(suf) {
		return
	}
	switch suf {
	case "tional":
		s.replace(suf, "tion")
	case "ational":
		s.replace(suf, "ate")
	case "alize":
		s.replace(suf, "al")
	case "icate", "iciti", "ical":
		s.replace(suf, "ic")
	case "ful", "ness":
		s.replace(suf, "")
	case "ative":
		if s.inR2(suf) {
			s.replace(suf, "")
		}
	}
}

func (s *stemmer) step4() {
	suf := s.longest("al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement", "ment", "ent", "ism",
		"ate", "iti", "ous", "ive", "ize", "ion")
	if suf == "" || !s.inR2(suf) {
		return
	}
	if suf == "ion" {
		if n := len(s.w) - 3; n > 0 && (s.w[n-1] == 's' || s.w[n-1] == 't') {
			s.replace(suf, "")
		}
		return
	}
	s.replace(suf, "")
}

func (s *stemmer) step5() {
	switch {
	case s.ends("e"):
		if s.inR2("e") || s.inR1("e") && !s.shortSyllableAt(len(s.w)-1) {
			s.replace("e", "")
		}
	case s.ends("l"):
		if s.inR2("l") && len(s.w) > 1 && s.w[len(s.w)-2] == 'l' {
			s.replace("l", "")
		}
	}
}

func (s *stemmer) postlude() string {
	return strings.ReplaceAll(string(s.w), "Y", "y")
}
