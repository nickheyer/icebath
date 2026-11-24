package regexutil

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/dlclark/regexp2/v2/syntax"
)

// Longest one match of a pattern from Compile may run before it gives up, the guard against catastrophic backtracking
const DefaultTimeout = 2 * time.Second

// Pattern the package or the engine rejects, with the pattern as given and the *syntax.Error saying why
type Error struct {
	Pattern string
	Err     error
}

// Message naming the pattern and what is wrong with it
func (e *Error) Error() string {
	return "invalid regular expression " + strconv.Quote(e.Pattern) + ": " + reason(e.Err)
}

// Syntax error the engine reported
func (e *Error) Unwrap() error { return e.Err }

func reason(err error) string {
	var se *syntax.Error
	if errors.As(err, &se) {
		if len(se.Args) == 0 {
			return se.Code.String()
		}
		return fmt.Sprintf(se.Code.String(), se.Args...)
	}
	return err.Error()
}

// Setting Compile applies to the pattern it builds
type Option func(*settings)

type settings struct {
	flags regexp2.RegexOptions
	whole bool
}

// Option making letters match regardless of case
func IgnoreCase() Option {
	return func(s *settings) { s.flags |= regexp2.IgnoreCase }
}

// Option making the pattern match only a whole subject, never part of one
func Whole() Option {
	return func(s *settings) { s.whole = true }
}

// Pattern from outside the program (a user, a config file, a skill, a plugin, the model) compiled in the package dialect: \w \d \s \b and case folding Unicode-aware, $ also before a final line break, (?P<name>...) (?P=name) \Z {,n} possessive quantifiers \x{...} \N{NAME} and the a flag read as the patterns already written expect, plus lookaround, backreferences, atomic groups and conditionals; groups are numbered left to right and every match gives up after DefaultTimeout with an error whose text quotes the whole subject
func Compile(pattern string, opts ...Option) (*regexp2.Regexp, error) {
	var s settings
	for _, opt := range opts {
		opt(&s)
	}
	re, err := compile(pattern, s.flags, s.whole)
	if err != nil {
		return nil, &Error{Pattern: pattern, Err: err}
	}
	re.MatchTimeout = DefaultTimeout
	return re, nil
}

// Pattern written in the program compiled in the dialect Compile takes but without a time limit, panicking with an *Error when it does not compile
func MustCompile(pattern string) *regexp2.Regexp {
	re, err := compile(pattern, regexp2.None, false)
	if err != nil {
		panic(&Error{Pattern: pattern, Err: err})
	}
	return re
}

// Pattern rewritten into regexp2's default syntax, whose classes and case folding are the Unicode-aware ones, and compiled with groups numbered left to right; whole anchors it at both ends
func compile(pattern string, flags regexp2.RegexOptions, whole bool) (*regexp2.Regexp, error) {
	text, err := translate(pattern)
	if err != nil {
		return nil, err
	}
	if whole {
		text = `\A(?:` + text + `)\z`
	}
	return regexp2.Compile(text, flags, regexp2.OptionMaintainCaptureOrder())
}
