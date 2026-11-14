package approval

type token struct {
	start, end int
	text       string
	gap        int
}

func tokenize(s string) []token {
	var out []token
	gap := 0
	for i := 0; i < len(s); {
		r, w := runeAt(s, i)
		if pySpace(r) {
			gap++
			i += w
			continue
		}
		start := i
		for i < len(s) {
			r, w = runeAt(s, i)
			if pySpace(r) {
				break
			}
			i += w
		}
		out = append(out, token{start, i, s[start:i], gap})
		gap = 0
	}
	return out
}

func isFlagToken(t string) bool { return len(t) >= 2 && t[0] == '-' }

func boundaryAfter(s string, i int) bool { return !wordAt(s, i) }

func wordBoundaryBefore(s string, i int) bool {
	prev := runeBefore(s, i)
	return prev < 0 || !pyWord(prev)
}

func endsWithWord(s string, t token, word string) bool {
	start, ok := foldSuffix(t.text, word)
	return ok && wordBoundaryBefore(s, t.start+start)
}

func wordPrefix(s string, t token, word string) bool {
	n, ok := foldPrefix(t.text, word)
	return ok && boundaryAfter(s, t.start+n)
}

func gatewayLifecycleFlags(s *subject) bool {
	toks := s.tokens()
	s0, s1 := false, false
	for j, t := range toks {
		if (s0 || s1) && foldEqual(t.text, "gateway") && j+1 < len(toks) &&
			(wordPrefix(s.low, toks[j+1], "stop") || wordPrefix(s.low, toks[j+1], "restart")) {
			return true
		}
		ns0, ns1 := false, false
		switch {
		case isFlagToken(t.text):
			ns1 = s0 || s1
		case s1:
			ns0 = true
		}
		if endsWithWord(s.low, t, "icebath") {
			ns0 = true
		}
		s0, s1 = ns0, ns1
	}
	return false
}

type seedFunc func(s *subject, toks []token) []bool

func seedAfter(word string) seedFunc {
	return func(s *subject, toks []token) []bool {
		seeds := make([]bool, len(toks)+1)
		for j, t := range toks {
			if endsWithWord(s.low, t, word) {
				seeds[j+1] = true
			}
		}
		return seeds
	}
}

var (
	dockerSeeds = seedAfter("docker")
	podmanSeeds = seedAfter("podman")
)

func composeSeeds(s *subject, toks []token) []bool {
	seeds := make([]bool, len(toks)+2)
	for j, t := range toks {
		if endsWithWord(s.low, t, "docker-compose") {
			seeds[j+1] = true
		}
		if endsWithWord(s.low, t, "docker") && j+1 < len(toks) && foldEqual(toks[j+1].text, "compose") {
			seeds[j+2] = true
		}
	}
	return seeds
}

type targetFunc func(s *subject, toks []token, j int) bool

func valueTarget(options ...string) targetFunc {
	return func(s *subject, toks []token, j int) bool {
		t := toks[j].text
		for _, opt := range options {
			if foldEqual(t, opt) {
				return j+1 < len(toks)
			}
			if n, ok := foldPrefix(t, opt+"="); ok {
				rest := t[n:]
				for len(rest) > 0 && rest[0] == '=' {
					rest = rest[1:]
				}
				if rest != "" || j+1 < len(toks) {
					return true
				}
			}
		}
		return false
	}
}

func wordTarget(words ...string) targetFunc {
	return func(s *subject, toks []token, j int) bool {
		for _, w := range words {
			if wordPrefix(s.low, toks[j], w) {
				return true
			}
		}
		return false
	}
}

func containerRule(seed seedFunc, target targetFunc) func(s *subject) bool {
	return func(s *subject) bool {
		toks := s.tokens()
		seeds := seed(s, toks)
		s0, s1 := false, false
		for j, t := range toks {
			s0 = s0 || seeds[j]
			if (s0 || s1) && target(s, toks, j) {
				return true
			}
			ns1 := isFlagToken(t.text) && (s0 || s1)
			ns0 := !isFlagToken(t.text) && s1 && t.gap == 1
			s0, s1 = ns0, ns1
		}
		return false
	}
}
