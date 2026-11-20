package pyast

// Frames CPython's parser can stack per open bracket, open block and credited token, proved from depth_gen.go
type recBound struct {
	base, bracket, indent, local, global int
	localToks, globalToks                map[int]bool
}

// Pass one bound, which covers every parse that ends in a tree because CPython's first pass then succeeds
var passOneBound = recBound{base: 55, bracket: 39, indent: 8, local: 8, global: 1,
	localToks:  map[int]bool{14: true, 15: true, 16: true, 31: true, 35: true, 621: true, 686: true, 703: true},
	globalToks: map[int]bool{687: true}}

// Bound over both passes with the invalid rules, used when the parse ended in a SyntaxError
var passTwoBound = recBound{base: 62, bracket: 43, indent: 10, local: 14, global: 1,
	localToks:  map[int]bool{cNAME: true, 14: true, 15: true, 16: true, 31: true, 35: true, 621: true, 686: true, 703: true},
	globalToks: map[int]bool{687: true}}

func recOpener(t int) bool { return t == 7 || t == 9 || t == 25 || t == cFSTART || t == cTSTART }
func recCloser(t int) bool { return t == 8 || t == 10 || t == 26 || t == cFEND || t == cTEND }

// Returns the most frames any rule stack reaches on these tokens, line local tokens reset at each NEWLINE
func (b *recBound) frames(types []int) int {
	var localTok, globalTok [1024]bool
	for t := range b.localToks {
		localTok[t] = true
	}
	for t := range b.globalToks {
		globalTok[t] = true
	}
	depth, indent, local, global, worst := 0, 0, 0, 0, 0
	for _, t := range types {
		switch {
		case recOpener(t):
			depth++
		case recCloser(t):
			depth--
		case t == cINDENT:
			indent++
		case t == cDEDENT:
			indent--
		case t == cNEWLINE:
			local = 0
		}
		if uint(t) < uint(len(localTok)) {
			if localTok[t] {
				local++
			}
			if globalTok[t] {
				global++
			}
		}
		worst = max(worst, b.bracket*depth+b.indent*indent+b.local*local+b.global*global)
	}
	return b.base + worst
}
