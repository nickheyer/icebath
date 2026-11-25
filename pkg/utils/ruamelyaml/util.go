package ruamelyaml

import (
	"math/big"
	"strconv"
	"strings"
)

const (
	theEnd         = "\n\x00\r\u0085  "
	theEndSpaceTab = " \n\x00\t\r\u0085  "
)

func in(r rune, set string) bool { return strings.ContainsRune(set, r) }

func itoa(n int) string { return strconv.Itoa(n) }

// Python repr of one character, a surrogate kept as its own code point
func charRepr(r rune) string { return strconv.Quote(string([]rune{r})) }

// Digits Python int() converts before the str conversion limit refuses the text
const intMaxStrDigits = 4300

// ValueError Python int() raises for a decimal text longer than the conversion limit
func intDigitsLimitError(digits int) *Exception {
	return Raise("ValueError", "Exceeds the limit ("+itoa(intMaxStrDigits)+" digits) for integer string conversion: value has "+
		itoa(digits)+" digits; use sys.set_int_max_str_digits() to increase the limit")
}

// YAML version tuple with arbitrary size numbers, the zero value standing for None
type version struct {
	set          bool
	major, minor *big.Int
}

func newVersion(major, minor int64) version {
	return version{set: true, major: big.NewInt(major), minor: big.NewInt(minor)}
}

var (
	v11 = newVersion(1, 1)
	v12 = newVersion(1, 2)
)

func (v version) eq(o version) bool {
	if v.set != o.set {
		return false
	}
	return !v.set || (v.major.Cmp(o.major) == 0 && v.minor.Cmp(o.minor) == 0)
}

// Tuple ordering of two set versions
func (v version) greater(o version) bool {
	if c := v.major.Cmp(o.major); c != 0 {
		return c > 0
	}
	return v.minor.Cmp(o.minor) > 0
}

func (v version) String() string { return "(" + v.major.String() + ", " + v.minor.String() + ")" }
