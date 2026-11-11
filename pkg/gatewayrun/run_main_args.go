package gatewayrun

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Program name the module entry parser prints, the binary followed by its entry word
const runMainProg = "icebath " + gateway.GatewayRunModuleEntry

const runMainDescription = "Icebath Gateway - Multi-platform messaging"

// One optional of the module entry parser with its option strings in declaration order
type runMainOption struct {
	strings []string
	takes   bool
	help    string
}

// Optionals of the module entry parser in the order argparse registers them
var runMainOptions = []*runMainOption{
	{strings: []string{"-h", "--help"}, help: "show this help message and exit"},
	{strings: []string{"--config", "-c"}, takes: true, help: "Path to gateway config file"},
	{strings: []string{"--verbose", "-v"}, help: "Verbose output"},
}

// Name of an optional in an argument error, its option strings joined by slashes
func (o *runMainOption) name() string { return strings.Join(o.strings, "/") }

// Option strings of the parser in registration order with the optional each one selects
func runMainOptionStrings() ([]string, map[string]*runMainOption) {
	var order []string
	byString := map[string]*runMainOption{}
	for _, option := range runMainOptions {
		for _, s := range option.strings {
			order = append(order, s)
			byString[s] = option
		}
	}
	return order, byString
}

// One reading of an argument as an optional, explicit nil when the string carries no attached value
type runMainMatch struct {
	option       *runMainOption
	optionString string
	sep          string
	explicit     *string
}

var (
	runMainNegativeNumber = regexp.MustCompile(`^-\.?\p{Nd}`)
	runMainANSI           = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// Readings of an argument as an optional the way argparse parses one, isOptional false for a positional
func runMainParseOptional(arg string) (matches []runMainMatch, isOptional bool) {
	order, byString := runMainOptionStrings()
	if arg == "" || arg[0] != '-' {
		return nil, false
	}
	if option, exact := byString[arg]; exact {
		return []runMainMatch{{option: option, optionString: arg}}, true
	}
	if len(arg) == 1 {
		return nil, false
	}
	prefix, value, hasSep := strings.Cut(arg, "=")
	if option, exact := byString[prefix]; hasSep && exact {
		return []runMainMatch{{option: option, optionString: prefix, sep: "=", explicit: &value}}, true
	}
	if arg[1] == '-' {
		for _, optionString := range order {
			if strings.HasPrefix(optionString, prefix) {
				m := runMainMatch{option: byString[optionString], optionString: optionString}
				if hasSep {
					explicit := value
					m.sep, m.explicit = "=", &explicit
				}
				matches = append(matches, m)
			}
		}
	} else {
		_, firstLen := utf8.DecodeRuneInString(arg[1:])
		shortPrefix, shortExplicit := arg[:1+firstLen], arg[1+firstLen:]
		for _, optionString := range order {
			switch {
			case optionString == shortPrefix:
				explicit := shortExplicit
				matches = append(matches, runMainMatch{option: byString[optionString], optionString: optionString, explicit: &explicit})
			case strings.HasPrefix(optionString, prefix):
				m := runMainMatch{option: byString[optionString], optionString: optionString}
				if hasSep {
					explicit := value
					m.sep, m.explicit = "=", &explicit
				}
				matches = append(matches, m)
			}
		}
	}
	if len(matches) > 0 {
		return matches, true
	}
	if runMainNegativeNumber.MatchString(arg) || strings.Contains(arg, " ") {
		return nil, false
	}
	return []runMainMatch{{optionString: arg}}, true
}

// Rune count of a string once its color codes are removed
func runMainPlainLen(s string) int {
	return utf8.RuneCountInString(runMainANSI.ReplaceAllString(s, ""))
}

// Usage line of the module entry, wrapped the way argparse wraps optionals past the text width
func runMainUsage(t pm.ArgparseColors) string {
	parts := []string{
		fmt.Sprintf("[%s-h%s]", t.SummaryShort, t.Reset),
		fmt.Sprintf("[%s--config %sCONFIG%s]", t.SummaryLong, t.SummaryLabel, t.Reset),
		fmt.Sprintf("[%s--verbose%s]", t.SummaryLong, t.Reset),
	}
	prefix := t.Usage + "usage: " + t.Reset
	prefixLen := len("usage: ")
	prog := t.Prog + runMainProg + t.Reset
	progLen := len(runMainProg)
	width := pm.ArgparseWidth()
	if line := prog + " " + strings.Join(parts, " "); prefixLen+runMainPlainLen(line) <= width {
		return prefix + line + "\n"
	}
	getLines := func(parts []string, indent int, withPrefix bool) []string {
		var lines, current []string
		lineLen := indent - 1
		if withPrefix {
			lineLen = prefixLen - 1
		}
		for _, part := range parts {
			if lineLen+1+runMainPlainLen(part) > width && len(current) > 0 {
				lines = append(lines, strings.Repeat(" ", indent)+strings.Join(current, " "))
				current, lineLen = nil, indent-1
			}
			current = append(current, part)
			lineLen += runMainPlainLen(part) + 1
		}
		if len(current) > 0 {
			lines = append(lines, strings.Repeat(" ", indent)+strings.Join(current, " "))
		}
		if withPrefix && len(lines) > 0 {
			lines[0] = lines[0][indent:]
		}
		return lines
	}
	var lines []string
	if float64(prefixLen+progLen) <= 0.75*float64(width) {
		lines = getLines(append([]string{prog}, parts...), prefixLen+progLen+1, true)
	} else {
		lines = append([]string{prog}, getLines(parts, prefixLen, false)...)
	}
	return prefix + strings.Join(lines, "\n") + "\n"
}

// Words of a help text filled to a width, a word longer than the width broken at it
func runMainWrap(text string, width int) []string {
	var lines []string
	current := ""
	for _, word := range strings.Fields(text) {
		for utf8.RuneCountInString(word) > width {
			if current != "" {
				lines = append(lines, current)
				current = ""
			}
			runes := []rune(word)
			lines = append(lines, string(runes[:width]))
			word = string(runes[width:])
		}
		switch {
		case current == "":
			current = word
		case utf8.RuneCountInString(current)+1+utf8.RuneCountInString(word) <= width:
			current += " " + word
		default:
			lines = append(lines, current)
			current = word
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// Help of the module entry in the argparse layout, usage, description and the options table
func runMainHelp(t pm.ArgparseColors) string {
	width := pm.ArgparseWidth()
	actions := []struct {
		header string
		length int
		help   string
	}{
		{fmt.Sprintf("%s-h%s, %s--help%s", t.ShortOption, t.Reset, t.LongOption, t.Reset), len("-h, --help"), runMainOptions[0].help},
		{fmt.Sprintf("%s--config%s, %s-c%s %sCONFIG%s", t.LongOption, t.Reset, t.ShortOption, t.Reset, t.Label, t.Reset), len("--config, -c CONFIG"), runMainOptions[1].help},
		{fmt.Sprintf("%s--verbose%s, %s-v%s", t.LongOption, t.Reset, t.ShortOption, t.Reset), len("--verbose, -v"), runMainOptions[2].help},
	}
	helpPosition := min(len("--config, -c CONFIG")+2+2, min(24, max(width-20, 4)))
	var b strings.Builder
	b.WriteString(runMainUsage(t))
	b.WriteString("\n" + strings.Join(runMainWrap(runMainDescription, width), "\n") + "\n")
	b.WriteString("\n" + t.Heading + "options:" + t.Reset + "\n")
	actionWidth := helpPosition - 2 - 2
	helpWidth := max(width-helpPosition, 11)
	for _, action := range actions {
		lines := runMainWrap(action.help, helpWidth)
		if action.length <= actionWidth {
			b.WriteString("  " + action.header + strings.Repeat(" ", actionWidth-action.length+2) + lines[0] + "\n")
		} else {
			b.WriteString("  " + action.header + "\n" + strings.Repeat(" ", helpPosition) + lines[0] + "\n")
		}
		for _, line := range lines[1:] {
			b.WriteString(strings.Repeat(" ", helpPosition) + line + "\n")
		}
	}
	return b.String()
}

// Parses the module entry arguments like argparse, the config path nil when absent and done true once help or an error ended the run
func parseRunMainArgs(argv []string, stdout, stderr io.Writer) (configPath *string, code int, done bool) {
	fail := func(option *runMainOption, message string) (*string, int, bool) {
		if option != nil {
			message = fmt.Sprintf("argument %s: %s", option.name(), message)
		}
		io.WriteString(stderr, runMainUsage(pm.ArgparseThemeFor(stderr)))
		fmt.Fprintf(stderr, "%s: error: %s\n", runMainProg, message)
		return nil, 2, true
	}
	_, byString := runMainOptionStrings()
	readings := make([][]runMainMatch, len(argv))
	valueLike := make([]bool, len(argv))
	lastOptional := -1
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			for j := i + 1; j < len(argv); j++ {
				valueLike[j] = true
			}
			break
		}
		matches, isOptional := runMainParseOptional(argv[i])
		if !isOptional {
			valueLike[i] = true
			continue
		}
		readings[i] = matches
		lastOptional = i
	}
	var extras []string
	start := 0
	for start <= lastOptional {
		next := start
		for next <= lastOptional && readings[next] == nil {
			next++
		}
		if readings[start] == nil {
			extras = append(extras, argv[start:next]...)
			start = next
		}
		matches := readings[start]
		if len(matches) > 1 {
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.optionString
			}
			return fail(nil, fmt.Sprintf("ambiguous option: %s could match %s", argv[start], strings.Join(names, ", ")))
		}
		m := matches[0]
		type taken struct {
			option *runMainOption
			value  *string
		}
		var actions []taken
		stop := start + 1
		for {
			if m.option == nil {
				extras = append(extras, argv[start])
				break
			}
			if m.explicit == nil {
				if !m.option.takes {
					actions = append(actions, taken{option: m.option})
					break
				}
				if start+1 >= len(argv) || !valueLike[start+1] {
					return fail(m.option, "expected one argument")
				}
				actions = append(actions, taken{option: m.option, value: &argv[start+1]})
				stop = start + 2
				break
			}
			explicit := *m.explicit
			if !m.option.takes && m.optionString[1] != '-' && explicit != "" {
				if m.sep != "" || explicit[0] == '-' {
					return fail(m.option, "ignored explicit argument "+pyrepr.Repr(explicit))
				}
				actions = append(actions, taken{option: m.option})
				_, firstLen := utf8.DecodeRuneInString(explicit)
				chained := "-" + explicit[:firstLen]
				option, known := byString[chained]
				if !known {
					extras = append(extras, "-"+explicit)
					break
				}
				rest := explicit[firstLen:]
				m = runMainMatch{option: option, optionString: chained}
				switch {
				case rest == "":
				case rest[0] == '=':
					attached := rest[1:]
					m.sep, m.explicit = "=", &attached
				default:
					m.explicit = &rest
				}
				continue
			}
			if !m.option.takes {
				return fail(m.option, "ignored explicit argument "+pyrepr.Repr(explicit))
			}
			actions = append(actions, taken{option: m.option, value: &explicit})
			break
		}
		for _, action := range actions {
			switch action.option {
			case runMainOptions[0]:
				io.WriteString(stdout, runMainHelp(pm.ArgparseThemeFor(stdout)))
				return nil, 0, true
			case runMainOptions[1]:
				configPath = action.value
			}
		}
		start = stop
	}
	extras = append(extras, argv[start:]...)
	if len(extras) > 0 {
		return fail(nil, "unrecognized arguments: "+strings.Join(extras, " "))
	}
	return configPath, 0, false
}
