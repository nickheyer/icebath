package commands

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Names the namespace key of a flag the way an argparse dest does
func dest(cmd *cobra.Command, name, key string) {
	_ = cmd.Flags().SetAnnotation(name, "dest", []string{key})
}

// Integer of command line text: base ten digits of any script with single underscores between them, an optional sign and white space around, a value beyond the int range clamped to its bounds; other text gives a *strconv.NumError
func parseIntArg(text string) (int, error) {
	n, err := textutil.ParseInt(asciiDigits(text))
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, err
	}
	return int(min(max(n, math.MinInt), math.MaxInt)), nil
}

// Whether r is a decimal digit of a script other than ASCII
func otherScriptDigit(r rune) bool { return r >= utf8.RuneSelf && unicode.Is(unicode.Nd, r) }

// Text with every decimal digit of another script replaced by the ASCII digit of the same value
func asciiDigits(text string) string {
	if !strings.ContainsFunc(text, otherScriptDigit) {
		return text
	}
	return strings.Map(func(r rune) rune {
		if otherScriptDigit(r) {
			return '0' + digitValue(r)
		}
		return r
	}, text)
}

// Value of a decimal digit, found by its offset in its range of the digit table since every script encodes zero to nine in one ascending run
func digitValue(r rune) rune {
	for _, rg := range unicode.Nd.R16 {
		if lo, hi := rune(rg.Lo), rune(rg.Hi); r >= lo && r <= hi {
			return (r - lo) % 10
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if lo, hi := rune(rg.Lo), rune(rg.Hi); r >= lo && r <= hi {
			return (r - lo) % 10
		}
	}
	return 0
}

// Integer flag value reading its text with parseIntArg
type intArg int

// Integer value starting at the default
func newIntArg(def int) *intArg {
	v := intArg(def)
	return &v
}

func (v *intArg) String() string { return strconv.Itoa(int(*v)) }

func (v *intArg) Type() string { return "int" }

func (v *intArg) Set(text string) error {
	n, err := parseIntArg(text)
	if err != nil {
		return err
	}
	*v = intArg(n)
	return nil
}

// Non negative integer flag value of the backup keep count, a negative keep would drop the newest archives
type nonNegativeKeep int

// Keep value starting at the argparse default
func newNonNegativeKeep(def int) *nonNegativeKeep {
	v := nonNegativeKeep(def)
	return &v
}

func (v *nonNegativeKeep) String() string { return strconv.Itoa(int(*v)) }

func (v *nonNegativeKeep) Type() string { return "int" }

// Accepts integer text at or above zero, the keep refusal otherwise
func (v *nonNegativeKeep) Set(text string) error {
	n, err := parseIntArg(text)
	if err != nil || n < 0 {
		return errors.New("must be a non-negative integer (0 keeps everything)")
	}
	*v = nonNegativeKeep(n)
	return nil
}
