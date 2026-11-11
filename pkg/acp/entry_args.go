package acp

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Program name the entry parser prints, the published launcher of the ACP server
const entryProg = "icebath-acp"

const entryDescription = "Run Icebath Agent as an ACP stdio server."

// One store true optional of the entry parser with its option strings in declaration order
type entryOption struct {
	strings []string
	dest    string
	help    string
}

// Optionals of the entry parser in the order argparse registers them
var entryOptions = []*entryOption{
	{strings: []string{"-h", "--help"}, help: "show this help message and exit"},
	{strings: []string{"--version"}, dest: "version", help: "Print Icebath version and exit"},
	{strings: []string{"--check"}, dest: "check", help: "Verify ACP dependencies and adapter imports, then exit"},
	{strings: []string{"--setup"}, dest: "setup", help: "Run interactive Icebath provider/model setup for ACP terminal auth"},
	{strings: []string{"--setup-browser"}, dest: "setup_browser", help: "Prepare PM's pinned Chromium for browser_exec."},
	{strings: []string{"--yes", "-y"}, dest: "assume_yes", help: "Accept setup prompts."},
}

// Parsed flags of the entry point
type entryArgs struct {
	Version      bool
	Check        bool
	Setup        bool
	SetupBrowser bool
	AssumeYes    bool
}

// Name of an optional in an argument error, its option strings joined by slashes
func (o *entryOption) name() string { return strings.Join(o.strings, "/") }

// Option strings of the parser in registration order with the optional each one selects
func entryOptionStrings() ([]string, map[string]*entryOption) {
	var order []string
	byString := map[string]*entryOption{}
	for _, option := range entryOptions {
		for _, s := range option.strings {
			order = append(order, s)
			byString[s] = option
		}
	}
	return order, byString
}

// One reading of an argument as an optional, explicit nil when the string carries no attached value
type entryMatch struct {
	option       *entryOption
	optionString string
	sep          string
	explicit     *string
}

var (
	entryNegativeNumber = regexp.MustCompile(`^-\.?\p{Nd}`)
	entryANSI           = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// Readings of an argument as an optional the way argparse parses one, isOptional false for a positional
func entryParseOptional(arg string) (matches []entryMatch, isOptional bool) {
	order, byString := entryOptionStrings()
	if arg == "" || arg[0] != '-' {
		return nil, false
	}
	if option, exact := byString[arg]; exact {
		return []entryMatch{{option: option, optionString: arg}}, true
	}
	if len(arg) == 1 {
		return nil, false
	}
	prefix, value, hasSep := strings.Cut(arg, "=")
	if option, exact := byString[prefix]; hasSep && exact {
		return []entryMatch{{option: option, optionString: prefix, sep: "=", explicit: &value}}, true
	}
	if arg[1] == '-' {
		for _, optionString := range order {
			if strings.HasPrefix(optionString, prefix) {
				m := entryMatch{option: byString[optionString], optionString: optionString}
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
				matches = append(matches, entryMatch{option: byString[optionString], optionString: optionString, explicit: &explicit})
			case strings.HasPrefix(optionString, prefix):
				m := entryMatch{option: byString[optionString], optionString: optionString}
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
	if entryNegativeNumber.MatchString(arg) || strings.Contains(arg, " ") {
		return nil, false
	}
	return []entryMatch{{optionString: arg}}, true
}

// Rune count of a string once its color codes are removed
func entryPlainLen(s string) int {
	return utf8.RuneCountInString(entryANSI.ReplaceAllString(s, ""))
}

// Usage line of the entry point, wrapped the way argparse wraps optionals past the text width
func entryUsage(t pm.ArgparseColors) string {
	var parts []string
	for _, option := range entryOptions {
		first := option.strings[0]
		color := t.SummaryLong
		if !strings.HasPrefix(first, "--") {
			color = t.SummaryShort
		}
		parts = append(parts, fmt.Sprintf("[%s%s%s]", color, first, t.Reset))
	}
	prefix := t.Usage + "usage: " + t.Reset
	prefixLen := len("usage: ")
	prog := t.Prog + entryProg + t.Reset
	progLen := len(entryProg)
	width := pm.ArgparseWidth()
	if line := prog + " " + strings.Join(parts, " "); prefixLen+entryPlainLen(line) <= width {
		return prefix + line + "\n"
	}
	getLines := func(parts []string, indent int, withPrefix bool) []string {
		var lines, current []string
		lineLen := indent - 1
		if withPrefix {
			lineLen = prefixLen - 1
		}
		for _, part := range parts {
			if lineLen+1+entryPlainLen(part) > width && len(current) > 0 {
				lines = append(lines, strings.Repeat(" ", indent)+strings.Join(current, " "))
				current, lineLen = nil, indent-1
			}
			current = append(current, part)
			lineLen += entryPlainLen(part) + 1
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
func entryWrap(text string, width int) []string {
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

// Help of the entry point in the argparse layout, usage, description and the options table
func entryHelp(t pm.ArgparseColors) string {
	width := pm.ArgparseWidth()
	type row struct {
		header string
		length int
		help   string
	}
	var rows []row
	longest := 0
	for _, option := range entryOptions {
		var colored []string
		for _, s := range option.strings {
			color := t.LongOption
			if !strings.HasPrefix(s, "--") {
				color = t.ShortOption
			}
			colored = append(colored, color+s+t.Reset)
		}
		length := len(strings.Join(option.strings, ", "))
		longest = max(longest, length)
		rows = append(rows, row{strings.Join(colored, ", "), length, option.help})
	}
	helpPosition := min(longest+2+2, min(24, max(width-20, 4)))
	var b strings.Builder
	b.WriteString(entryUsage(t))
	b.WriteString("\n" + strings.Join(entryWrap(entryDescription, width), "\n") + "\n")
	b.WriteString("\n" + t.Heading + "options:" + t.Reset + "\n")
	actionWidth := helpPosition - 2 - 2
	helpWidth := max(width-helpPosition, 11)
	for _, r := range rows {
		lines := entryWrap(r.help, helpWidth)
		if r.length <= actionWidth {
			b.WriteString("  " + r.header + strings.Repeat(" ", actionWidth-r.length+2) + lines[0] + "\n")
		} else {
			b.WriteString("  " + r.header + "\n" + strings.Repeat(" ", helpPosition) + lines[0] + "\n")
		}
		for _, line := range lines[1:] {
			b.WriteString(strings.Repeat(" ", helpPosition) + line + "\n")
		}
	}
	return b.String()
}

// Parses the entry arguments like argparse, done true once help or an error ended the run with code
func parseEntryArgs(argv []string, stdout, stderr io.Writer) (args entryArgs, code int, done bool) {
	fail := func(option *entryOption, message string) (entryArgs, int, bool) {
		if option != nil {
			message = fmt.Sprintf("argument %s: %s", option.name(), message)
		}
		io.WriteString(stderr, entryUsage(pm.ArgparseThemeFor(stderr)))
		fmt.Fprintf(stderr, "%s: error: %s\n", entryProg, message)
		return entryArgs{}, 2, true
	}
	_, byString := entryOptionStrings()
	readings := make([][]entryMatch, len(argv))
	lastOptional := -1
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			break
		}
		matches, isOptional := entryParseOptional(argv[i])
		if !isOptional {
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
		var taken []*entryOption
		for {
			if m.option == nil {
				extras = append(extras, argv[start])
				break
			}
			if m.explicit == nil {
				taken = append(taken, m.option)
				break
			}
			explicit := *m.explicit
			if m.optionString[1] == '-' || explicit == "" || m.sep != "" || explicit[0] == '-' {
				return fail(m.option, "ignored explicit argument "+pyrepr.Repr(explicit))
			}
			taken = append(taken, m.option)
			_, firstLen := utf8.DecodeRuneInString(explicit)
			chained := "-" + explicit[:firstLen]
			option, known := byString[chained]
			if !known {
				extras = append(extras, "-"+explicit)
				break
			}
			rest := explicit[firstLen:]
			m = entryMatch{option: option, optionString: chained}
			switch {
			case rest == "":
			case rest[0] == '=':
				attached := rest[1:]
				m.sep, m.explicit = "=", &attached
			default:
				m.explicit = &rest
			}
		}
		for _, option := range taken {
			switch option.dest {
			case "":
				io.WriteString(stdout, entryHelp(pm.ArgparseThemeFor(stdout)))
				return entryArgs{}, 0, true
			case "version":
				args.Version = true
			case "check":
				args.Check = true
			case "setup":
				args.Setup = true
			case "setup_browser":
				args.SetupBrowser = true
			case "assume_yes":
				args.AssumeYes = true
			}
		}
		start++
	}
	extras = append(extras, argv[start:]...)
	if len(extras) > 0 {
		return fail(nil, "unrecognized arguments: "+strings.Join(extras, " "))
	}
	return args, 0, false
}
