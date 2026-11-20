package pm

import (
	"math/big"
	"unicode"
)

// UpdateArgs are pm update's arguments, Target empty for every target
type UpdateArgs struct {
	Names      []string
	Check      bool
	Target     string
	SkipTarget []string
	UV         bool
	NPM        bool
	Termux     bool
}

// Integer a Unicode decimal digit run spells
func digitsValue(digits string) *big.Int {
	n := new(big.Int)
	ten := big.NewInt(10)
	for _, r := range digits {
		run := int64(0)
		for unicode.Is(unicode.Nd, r-rune(run)-1) {
			run++
		}
		n.Mul(n, ten)
		n.Add(n, big.NewInt(run%10))
	}
	return n
}
