package pm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const buildEnvDescription = "Build and inspect explicit Python dependency environments through PM."

// argOption is one argparse optional of the build environment parser
type argOption struct {
	flags  []string
	dest   string
	takes  bool
	append bool
	help   string
	mutex  bool
}

// Options of the build environment parser in declaration order
var buildEnvOptions = []argOption{
	{flags: []string{"-h", "--help"}, dest: "help", help: "show this help message and exit"},
	{flags: []string{"--source"}, dest: "source", takes: true},
	{flags: []string{"--out"}, dest: "out", takes: true},
	{flags: []string{"--python"}, dest: "python", takes: true},
	{flags: []string{"--cache"}, dest: "cache", takes: true},
	{flags: []string{"--extra"}, dest: "extras", takes: true, append: true},
	{flags: []string{"--group"}, dest: "groups", takes: true, append: true},
	{flags: []string{"--all-extras"}, dest: "all_extras"},
	{flags: []string{"--no-install-project"}, dest: "no_install_project"},
	{flags: []string{"--resolve"}, dest: "resolve", help: "resolve the source lock before building"},
	{flags: []string{"--lock-only"}, dest: "lock_only", mutex: true},
	{flags: []string{"--check-lock"}, dest: "check_lock", mutex: true},
	{flags: []string{"--export-requirements"}, dest: "export_requirements", takes: true, mutex: true},
	{flags: []string{"--prune-cache"}, dest: "prune_cache", mutex: true},
	{flags: []string{"--exact-lock"}, dest: "exact_lock", mutex: true, help: "prune to the project lock: entries the uv.lock cannot resolve are deleted"},
	{flags: []string{"--manager-runtime"}, dest: "manager_runtime", mutex: true},
	{flags: []string{"--lock-source"}, dest: "lock_source", takes: true, help: "repo whose uv.lock selects the kept entries for --exact-lock (default: cwd)"},
	{flags: []string{"--upgrade"}, dest: "upgrade"},
	{flags: []string{"--ci"}, dest: "ci"},
	{flags: []string{"--sealed"}, dest: "sealed"},
	{flags: []string{"--offline"}, dest: "offline"},
	{flags: []string{"--explicit"}, dest: "explicit", help: "CLI builds are always explicit requests"},
	{flags: []string{"--record-selection"}, dest: "record_selection", help: "record the --extra list as this install's baseline dependency selection"},
	{flags: []string{"--wheelhouse"}, dest: "wheelhouse", takes: true},
	{flags: []string{"--requirements"}, dest: "requirements", takes: true},
	{flags: []string{"--requirement"}, dest: "requirement", takes: true, append: true},
	{flags: []string{"--lock"}, dest: "lock", takes: true, help: "uv.lock the requirements were exported from; its overrides apply"},
}

// Path dests argparse converts with pathlib Path
var buildEnvPathDests = map[string]bool{"source": true, "out": true, "python": true, "cache": true, "export_requirements": true,
	"lock_source": true, "wheelhouse": true, "requirements": true, "lock": true}

// BuildEnvArgs are the parsed build environment arguments, empty paths standing for None
type BuildEnvArgs struct {
	Source, Out, Python, Cache             string
	Extras, Groups, Requirement            []string
	AllExtras, NoInstallProject, Resolve   bool
	LockOnly, CheckLock, PruneCache        bool
	ExactLock, ManagerRuntime              bool
	ExportRequirements, LockSource         string
	Upgrade, CI, Sealed, Offline, Explicit bool
	RecordSelection                        bool
	Wheelhouse, Requirements, Lock         string
	values                                 map[string]string
}

// argparseExit is a parser exit with its status, the help or usage and error already written
type argparseExit struct{ code int }

func (e *argparseExit) Error() string { return fmt.Sprintf("argparse exit %d", e.code) }

// argTheme is argparse's color theme, every field empty without color
type argTheme struct {
	usage, prog, heading, summaryLong, summaryShort, summaryLabel, longOption, shortOption, label, reset string
}

// Theme argparse picks, colored only when stdout can show color
func argparseTheme(stdout io.Writer) argTheme {
	if !canColorize(stdout) {
		return argTheme{}
	}
	return argTheme{usage: "\x1b[1;34m", prog: "\x1b[1;35m", heading: "\x1b[1;34m", summaryLong: "\x1b[36m",
		summaryShort: "\x1b[32m", summaryLabel: "\x1b[33m", longOption: "\x1b[1;36m", shortOption: "\x1b[1;32m",
		label: "\x1b[1;33m", reset: "\x1b[0m"}
}

// Color codes of the argparse theme for one stream, all empty when the stream shows no color
type ArgparseColors struct {
	Usage, Prog, Heading, SummaryLong, SummaryShort, SummaryLabel, LongOption, ShortOption, Label, Reset string
}

// ArgparseThemeFor exports argparseTheme
func ArgparseThemeFor(stdout io.Writer) ArgparseColors {
	t := argparseTheme(stdout)
	return ArgparseColors{t.usage, t.prog, t.heading, t.summaryLong, t.summaryShort, t.summaryLabel, t.longOption, t.shortOption, t.label, t.reset}
}

// Reports whether Python would color output on this stream, its environment overrides first
func canColorize(stdout io.Writer) bool {
	switch os.Getenv("PYTHON_COLORS") {
	case "0":
		return false
	case "1":
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminalStream(stdout)
}

var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Length of text without its color codes, in code points
func decoloredLen(s string) int { return utf8.RuneCountInString(ansiSGR.ReplaceAllString(s, "")) }

// Formatter width argparse uses, the terminal columns less two
func argparseWidth() int { return terminalColumns() - 2 }

// ArgparseWidth exports argparseWidth
func ArgparseWidth() int { return argparseWidth() }

// Option invocation for usage and help, metavar the dest upper cased
func (o argOption) metavar() string { return strings.ToUpper(o.dest) }

// Usage text of the parser for a program name, wrapped the way argparse 3.14 wraps it
func buildEnvUsage(prog string, t argTheme) string {
	var parts []string
	inGroup := false
	for i, o := range buildEnvOptions {
		color := t.summaryLong
		if len(o.flags[0]) <= 2 {
			color = t.summaryShort
		}
		part := color + o.flags[0] + t.reset
		if o.takes {
			part = color + o.flags[0] + " " + t.summaryLabel + o.metavar() + t.reset
		}
		if !o.mutex {
			part = "[" + part + "]"
		} else {
			if !inGroup {
				part = "[" + part
			}
			next := i+1 < len(buildEnvOptions) && buildEnvOptions[i+1].mutex
			if next {
				part += " |"
			} else {
				part += "]"
			}
		}
		inGroup = o.mutex
		parts = append(parts, part)
	}
	prefix := "usage: "
	textWidth := argparseWidth()
	usage := strings.Join(append([]string{prog}, parts...), " ")
	if len(prefix)+decoloredLen(usage) > textWidth {
		getLines := func(parts []string, indent string, withPrefix bool) []string {
			var lines, line []string
			lineLen := len(indent) - 1
			if withPrefix {
				lineLen = len(prefix) - 1
			}
			for _, part := range parts {
				partLen := decoloredLen(part)
				if lineLen+1+partLen > textWidth && len(line) > 0 {
					lines = append(lines, indent+strings.Join(line, " "))
					line = nil
					lineLen = len(indent) - 1
				}
				line = append(line, part)
				lineLen += partLen + 1
			}
			if len(line) > 0 {
				lines = append(lines, indent+strings.Join(line, " "))
			}
			if withPrefix {
				lines[0] = lines[0][len(indent):]
			}
			return lines
		}
		var lines []string
		progLen := utf8.RuneCountInString(prog)
		if float64(len(prefix)+progLen) <= 0.75*float64(textWidth) {
			indent := strings.Repeat(" ", len(prefix)+progLen+1)
			lines = getLines(append([]string{prog}, parts...), indent, true)
		} else {
			indent := strings.Repeat(" ", len(prefix))
			lines = append([]string{prog}, getLines(parts, indent, false)...)
		}
		usage = strings.Join(lines, "\n")
	}
	usage = t.prog + prog + t.reset + strings.TrimPrefix(usage, prog)
	return t.usage + prefix + t.reset + usage + "\n\n"
}

var (
	textwrapWordSep    = regexp2.MustCompile(`([\t\n\x0b\x0c\r ]+|(?<=[\w!"'&.,?])-{2,}(?=\w)|[^\t\n\x0b\x0c\r ]+?(?:-(?:(?<=[^\d\W]{2}-)|(?<=[^\d\W]-[^\d\W]-))(?=[^\d\W]-?[^\d\W])|(?=[\t\n\x0b\x0c\r ]|\z)|(?<=[\w!"'&.,?])(?=-{2,}\w)))`, regexp2.None)
	asciiWhitespaceRun = regexp.MustCompile(`[\t\n\x0b\x0c\r ]+`)
)

// Wraps text into lines of at most width code points the way textwrap wrap does with default options
func textwrapWrap(text string, width int, initial, subsequent string) []string {
	text = strings.Map(func(r rune) rune {
		if strings.ContainsRune("\t\n\x0b\x0c\r", r) {
			return ' '
		}
		return r
	}, text)
	var chunks []string
	runes := []rune(text)
	last := 0
	m, _ := textwrapWordSep.FindStringMatch(text)
	for m != nil {
		if m.RuneIndex > last {
			chunks = append(chunks, string(runes[last:m.RuneIndex]))
		}
		if m.RuneLength > 0 {
			chunks = append(chunks, m.String())
		}
		last = m.RuneIndex + m.RuneLength
		m, _ = textwrapWordSep.FindNextMatch(m)
	}
	if last < len(runes) {
		chunks = append(chunks, string(runes[last:]))
	}
	runeLen := func(s string) int { return utf8.RuneCountInString(s) }
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	var lines []string
	for i, j := 0, len(chunks)-1; i < j; i, j = i+1, j-1 {
		chunks[i], chunks[j] = chunks[j], chunks[i]
	}
	for len(chunks) > 0 {
		var line []string
		lineLen := 0
		indent := initial
		if len(lines) > 0 {
			indent = subsequent
		}
		w := width - runeLen(indent)
		if blank(chunks[len(chunks)-1]) && len(lines) > 0 {
			chunks = chunks[:len(chunks)-1]
		}
		for len(chunks) > 0 {
			l := runeLen(chunks[len(chunks)-1])
			if lineLen+l > w {
				break
			}
			line = append(line, chunks[len(chunks)-1])
			chunks = chunks[:len(chunks)-1]
			lineLen += l
		}
		if len(chunks) > 0 && runeLen(chunks[len(chunks)-1]) > w {
			spaceLeft := w - lineLen
			if w < 1 {
				spaceLeft = 1
			}
			if spaceLeft > 0 {
				chunk := []rune(chunks[len(chunks)-1])
				end := spaceLeft
				if len(chunk) > spaceLeft {
					if hyphen := strings.LastIndex(string(chunk[:spaceLeft]), "-"); hyphen > 0 {
						hyphen = utf8.RuneCountInString(string(chunk[:spaceLeft])[:hyphen])
						if strings.Trim(string(chunk[:hyphen]), "-") != "" {
							end = hyphen + 1
						}
					}
				}
				line = append(line, string(chunk[:end]))
				chunks[len(chunks)-1] = string(chunk[end:])
			} else if len(line) == 0 {
				line = append(line, chunks[len(chunks)-1])
				chunks = chunks[:len(chunks)-1]
			}
			lineLen = 0
			for _, c := range line {
				lineLen += runeLen(c)
			}
		}
		if len(line) > 0 && blank(line[len(line)-1]) {
			line = line[:len(line)-1]
		}
		if len(line) > 0 {
			lines = append(lines, indent+strings.Join(line, ""))
		}
	}
	return lines
}

// Full help text of the parser
func buildEnvHelp(prog string, t argTheme) string {
	width := argparseWidth()
	maxHelpPosition := min(24, max(width-20, 4))
	invocation := func(o argOption) string {
		var flags []string
		for _, f := range o.flags {
			if len(f) > 2 {
				flags = append(flags, t.longOption+f+t.reset)
			} else {
				flags = append(flags, t.shortOption+f+t.reset)
			}
		}
		text := strings.Join(flags, ", ")
		if o.takes {
			text += " " + t.label + o.metavar() + t.reset
		}
		return text
	}
	actionMax := 0
	for _, o := range buildEnvOptions {
		actionMax = max(actionMax, decoloredLen(invocation(o))+2)
	}
	helpPosition := min(actionMax+2, maxHelpPosition)
	helpWidth := max(width-helpPosition, 11)
	actionWidth := helpPosition - 2 - 2
	var items strings.Builder
	for _, o := range buildEnvOptions {
		header := invocation(o)
		plain := ansiSGR.ReplaceAllString(header, "")
		indentFirst := 0
		switch {
		case o.help == "":
			items.WriteString("  " + header + "\n")
		case utf8.RuneCountInString(plain) <= actionWidth:
			padded := "  " + plain + strings.Repeat(" ", actionWidth-utf8.RuneCountInString(plain)) + "  "
			items.WriteString(strings.Replace(padded, plain, header, 1))
		default:
			items.WriteString("  " + header + "\n")
			indentFirst = helpPosition
		}
		if o.help != "" {
			lines := textwrapWrap(strings.TrimSpace(asciiWhitespaceRun.ReplaceAllString(o.help, " ")), helpWidth, "", "")
			items.WriteString(strings.Repeat(" ", indentFirst) + lines[0] + "\n")
			for _, line := range lines[1:] {
				items.WriteString(strings.Repeat(" ", helpPosition) + line + "\n")
			}
		}
	}
	description := strings.Join(textwrapWrap(strings.TrimSpace(asciiWhitespaceRun.ReplaceAllString(buildEnvDescription, " ")), max(width, 11), "", ""), "\n")
	text := buildEnvUsage(prog, t) + description + "\n\n" + "\n" + t.heading + "options:" + t.reset + "\n" + items.String() + "\n"
	text = regexp.MustCompile(`\n\n\n+`).ReplaceAllString(text, "\n\n")
	return strings.Trim(text, "\n") + "\n"
}

// Normalized text of a path the way str of pathlib Path prints it
func pyPathText(p string) string {
	if runtime.GOOS == "windows" {
		drive, root, rest := pypath.NtSplitRoot(strings.ReplaceAll(p, "/", `\`))
		var parts []string
		for _, part := range strings.Split(rest, `\`) {
			if part != "" && part != "." {
				parts = append(parts, part)
			}
		}
		text := drive + root + strings.Join(parts, `\`)
		if text == "" {
			return "."
		}
		return text
	}
	root := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		root = "//"
	case strings.HasPrefix(p, "/"):
		root = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	text := root + strings.Join(parts, "/")
	if text == "" {
		return "."
	}
	return text
}

var negativeNumber = regexp.MustCompile(`^-\.?\p{Nd}`)

// One recognized occurrence of an option in the argument strings
type optionMatch struct {
	option   *argOption
	flag     string
	sep      bool
	explicit *string
}

// Option interpretations of one argument string, nil when it is meant as a positional
func parseOptional(arg string) []optionMatch {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	exact := func(flag string) *argOption {
		for i := range buildEnvOptions {
			for _, f := range buildEnvOptions[i].flags {
				if f == flag {
					return &buildEnvOptions[i]
				}
			}
		}
		return nil
	}
	if o := exact(arg); o != nil {
		return []optionMatch{{option: o, flag: arg}}
	}
	if len(arg) == 1 {
		return nil
	}
	if name, value, found := strings.Cut(arg, "="); found {
		if o := exact(name); o != nil {
			return []optionMatch{{option: o, flag: name, sep: true, explicit: &value}}
		}
	}
	var matches []optionMatch
	prefix, value, found := strings.Cut(arg, "=")
	var explicit *string
	if found {
		explicit = &value
	}
	if arg[1] == '-' {
		for i := range buildEnvOptions {
			for _, f := range buildEnvOptions[i].flags {
				if strings.HasPrefix(f, prefix) {
					matches = append(matches, optionMatch{option: &buildEnvOptions[i], flag: f, sep: found, explicit: explicit})
				}
			}
		}
	} else {
		short, tail := arg[:2], arg[2:]
		for i := range buildEnvOptions {
			for _, f := range buildEnvOptions[i].flags {
				if f == short {
					t := tail
					matches = append(matches, optionMatch{option: &buildEnvOptions[i], flag: f, explicit: &t})
				} else if strings.HasPrefix(f, prefix) {
					matches = append(matches, optionMatch{option: &buildEnvOptions[i], flag: f, sep: found, explicit: explicit})
				}
			}
		}
	}
	if len(matches) > 0 {
		return matches
	}
	if negativeNumber.MatchString(arg) || strings.Contains(arg, " ") {
		return nil
	}
	return []optionMatch{{flag: arg}}
}

// Parses build environment arguments with argparse semantics, help and usage errors written and returned as an exit
func parseBuildEnvArgs(argv []string, prog string, stdout, stderr io.Writer) (*BuildEnvArgs, error) {
	t := argparseTheme(stdout)
	fail := func(message string) error {
		io.WriteString(stderr, buildEnvUsageOnly(prog, t))
		fmt.Fprintf(stderr, "%s: error: %s\n", prog, message)
		return &argparseExit{code: 2}
	}
	optionName := func(o *argOption) string { return strings.Join(o.flags, "/") }
	args := &BuildEnvArgs{values: map[string]string{}}
	lists := map[string][]string{}
	flags := map[string]bool{}
	var extras []string
	seenMutex := []*argOption{}
	take := func(o *argOption, values []string) error {
		if o.mutex {
			for _, prior := range seenMutex {
				if prior != o {
					return fail(fmt.Sprintf("argument %s: not allowed with argument %s", optionName(o), optionName(prior)))
				}
			}
			found := false
			for _, prior := range seenMutex {
				found = found || prior == o
			}
			if !found {
				seenMutex = append(seenMutex, o)
			}
		}
		switch {
		case o.dest == "help":
			io.WriteString(stdout, buildEnvHelp(prog, t))
			return &argparseExit{code: 0}
		case !o.takes:
			flags[o.dest] = true
		case o.append:
			lists[o.dest] = append(lists[o.dest], values[0])
		default:
			value := values[0]
			if buildEnvPathDests[o.dest] {
				value = pyPathText(value)
			}
			args.values[o.dest] = value
		}
		return nil
	}
	patterns := make([]byte, len(argv))
	interpretations := make([][]optionMatch, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			patterns[i] = '-'
			for j := i + 1; j < len(argv); j++ {
				patterns[j] = 'A'
			}
			break
		}
		if m := parseOptional(argv[i]); m != nil {
			interpretations[i], patterns[i] = m, 'O'
		} else {
			patterns[i] = 'A'
		}
	}
	for i := 0; i < len(argv); {
		if patterns[i] != 'O' {
			extras = append(extras, argv[i])
			i++
			continue
		}
		matches := interpretations[i]
		if len(matches) > 1 {
			var names []string
			for _, m := range matches {
				names = append(names, m.flag)
			}
			return nil, fail(fmt.Sprintf("ambiguous option: %s could match %s", argv[i], strings.Join(names, ", ")))
		}
		m := matches[0]
		var pending []func() error
		stop := i + 1
		for {
			if m.option == nil {
				extras = append(extras, argv[i])
				break
			}
			o := m.option
			if m.explicit != nil {
				explicit := *m.explicit
				if !o.takes && m.flag[1] != '-' && explicit != "" {
					if m.sep || explicit[0] == '-' {
						return nil, fail(fmt.Sprintf("argument %s: ignored explicit argument %s", optionName(o), pyrepr.StrRepr(explicit)))
					}
					pending = append(pending, func() error { return take(o, nil) })
					next := "-" + explicit[:1]
					var nextOption *argOption
					for k := range buildEnvOptions {
						for _, f := range buildEnvOptions[k].flags {
							if f == next {
								nextOption = &buildEnvOptions[k]
							}
						}
					}
					if nextOption == nil {
						extras = append(extras, "-"+explicit)
						break
					}
					rest := explicit[1:]
					m = optionMatch{option: nextOption, flag: next}
					switch {
					case rest == "":
					case rest[0] == '=':
						r := rest[1:]
						m.sep, m.explicit = true, &r
					default:
						m.explicit = &rest
					}
					continue
				}
				if o.takes {
					value := explicit
					pending = append(pending, func() error { return take(o, []string{value}) })
					break
				}
				return nil, fail(fmt.Sprintf("argument %s: ignored explicit argument %s", optionName(o), pyrepr.StrRepr(explicit)))
			}
			if o.takes {
				if i+1 >= len(argv) || patterns[i+1] != 'A' {
					return nil, fail(fmt.Sprintf("argument %s: expected one argument", optionName(o)))
				}
				value := argv[i+1]
				stop = i + 2
				pending = append(pending, func() error { return take(o, []string{value}) })
			} else {
				pending = append(pending, func() error { return take(o, nil) })
			}
			break
		}
		for _, run := range pending {
			if err := run(); err != nil {
				return nil, err
			}
		}
		i = stop
	}
	if len(extras) > 0 {
		return nil, fail("unrecognized arguments: " + strings.Join(extras, " "))
	}
	v := args.values
	args.Source, args.Out, args.Python, args.Cache = v["source"], v["out"], v["python"], v["cache"]
	args.ExportRequirements, args.LockSource, args.Wheelhouse = v["export_requirements"], v["lock_source"], v["wheelhouse"]
	args.Requirements, args.Lock = v["requirements"], v["lock"]
	args.Extras, args.Groups, args.Requirement = lists["extras"], lists["groups"], lists["requirement"]
	args.AllExtras, args.NoInstallProject, args.Resolve = flags["all_extras"], flags["no_install_project"], flags["resolve"]
	args.LockOnly, args.CheckLock, args.PruneCache = flags["lock_only"], flags["check_lock"], flags["prune_cache"]
	args.ExactLock, args.ManagerRuntime, args.Upgrade = flags["exact_lock"], flags["manager_runtime"], flags["upgrade"]
	args.CI, args.Sealed, args.Offline, args.Explicit = flags["ci"], flags["sealed"], flags["offline"], flags["explicit"]
	args.RecordSelection = flags["record_selection"]
	return args, nil
}

// Usage alone the way print usage writes it
func buildEnvUsageOnly(prog string, t argTheme) string {
	return strings.TrimRight(buildEnvUsage(prog, t), "\n") + "\n"
}

// Client build a project environment command goes through
var clientBuildEnvironment = BuildEnvironment

// Publishes the built extras as the store's venv selection, as a bundle build records it
func recordSelection(ctx context.Context, source string, extras []string) error {
	selected := sortedSet(extras)
	if selected == nil {
		selected = []string{}
	}
	venv, ok := NewVenv(resolvePath(source)).(*Venv)
	if !ok {
		return &PyError{Class: "TypeError", Message: "Venv state package expected"}
	}
	stamp, err := venv.Stamp(ctx, selected, []MemberDir{})
	if err != nil {
		return err
	}
	path, err := FactsPath(ctx)
	if err != nil {
		return err
	}
	facts, err := NewFacts(path, false)
	if err != nil {
		return err
	}
	return facts.RecordState("venv", stamp, selected, "", "")
}

// Reports whether an error is one the build environment command prints instead of raising
func buildEnvHandled(err error) bool {
	var ie *InstallError
	var py *PyError
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var exists *DestinationExistsError
	var toml *TOMLDecodeError
	switch {
	case errors.As(err, &ie), errors.As(err, &pathErr), errors.As(err, &linkErr), errors.As(err, &exists), errors.As(err, &toml):
		return true
	case errors.As(err, &py):
		switch py.Class {
		case "ValueError", "JSONDecodeError", "UnicodeDecodeError", "OSError", "FileNotFoundError", "FileExistsError", "PermissionError", "IsADirectoryError", "NotADirectoryError":
			return true
		}
	}
	return false
}

// Runs the build environment command with argparse semantics and returns its exit status
func BuildEnvMain(ctx context.Context, argv []string, prog string, stdout, stderr io.Writer) int {
	args, err := parseBuildEnvArgs(argv, prog, stdout, stderr)
	var exit *argparseExit
	if errors.As(err, &exit) {
		return exit.code
	}
	t := argparseTheme(stdout)
	fail := func(message string) int {
		io.WriteString(stderr, buildEnvUsageOnly(prog, t))
		fmt.Fprintf(stderr, "%s: error: %s\n", prog, message)
		return 2
	}
	requirements := append([]string{}, args.Requirement...)
	if args.Requirements != "" {
		data, err := os.ReadFile(args.Requirements)
		if err != nil {
			fmt.Fprintln(stderr, pyTraceback(err))
			return 1
		}
		data = trimBOM(data)
		if err := utf8Strict(data); err != nil {
			fmt.Fprintln(stderr, pyTraceback(err))
			return 1
		}
		for _, line := range pystr.Splitlines(string(data), false) {
			if stripped := pystr.Strip(line); stripped != "" && !strings.HasPrefix(strings.TrimLeftFunc(line, pystr.IsSpace), "#") {
				requirements = append(requirements, stripped)
			}
		}
	}
	switch {
	case args.ExactLock:
		if args.CI || args.PruneCache {
			return fail("--exact-lock replaces --ci/--prune-cache: downloaded wheels the lock keeps must survive")
		}
		if args.LockSource == "" {
			return fail("--exact-lock requires --lock-source")
		}
		if args.Cache == "" {
			return fail("--exact-lock requires --cache")
		}
	case args.PruneCache:
		if args.Cache == "" {
			return fail("--prune-cache requires --cache")
		}
	case len(requirements) == 0 && args.Source == "":
		return fail("--source is required for project operations")
	}
	build := !(args.LockOnly || args.CheckLock || args.ExportRequirements != "" || args.PruneCache || args.ExactLock)
	if build && args.Out == "" {
		return fail("--out is required when building an environment")
	}
	if args.ManagerRuntime && args.Python == "" {
		return fail("--manager-runtime requires the target --python")
	}
	if args.RecordSelection && (!build || len(requirements) > 0 || args.ManagerRuntime || args.AllExtras || len(args.Groups) > 0 || len(args.Extras) == 0) {
		return fail("--record-selection records the --extra list of a project environment build")
	}
	executable, err := runBuildEnv(ctx, args, requirements, stdout)
	if err != nil {
		if buildEnvHandled(err) {
			fmt.Fprintf(stderr, "python environment: %s\n", err)
			return 1
		}
		fmt.Fprintln(stderr, pyTraceback(err))
		return 1
	}
	if executable != "" {
		fmt.Fprintln(stdout, executable)
	}
	return 0
}

// Final line of the traceback Python prints for an uncaught error
func pyTraceback(err error) string {
	class := errorClass(err)
	if class == "" {
		class = "RuntimeError"
	}
	return class + ": " + err.Error()
}

// Dispatches the parsed build environment operation and returns the interpreter a build produced
func runBuildEnv(ctx context.Context, args *BuildEnvArgs, requirements []string, stdout io.Writer) (string, error) {
	extras := args.Extras
	if extras == nil {
		extras = []string{}
	}
	switch {
	case args.ExactLock:
		pruned, err := PruneUVCacheToLock(args.Cache, args.LockSource)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(stdout, "pruned %d cache entries outside the lock\n", pruned)
		return "", nil
	case args.PruneCache:
		return "", PruneCache(ctx, PruneCacheOptions{Cache: args.Cache, CI: args.CI})
	case args.CheckLock:
		return "", CheckProjectLock(ctx, CheckProjectLockOptions{Source: args.Source, Python: args.Python, Cache: args.Cache,
			Offline: args.Offline, Explicit: true})
	case args.ExportRequirements != "":
		return "", ExportRequirements(ctx, ExportRequirementsOptions{Source: args.Source, Out: args.ExportRequirements, Extras: extras,
			Python: args.Python, Cache: args.Cache, Explicit: true})
	case args.LockOnly:
		return "", LockProject(ctx, LockProjectOptions{Source: args.Source, Upgrade: args.Upgrade, Python: args.Python, Cache: args.Cache,
			Offline: args.Offline, Explicit: true})
	case args.ManagerRuntime:
		return StageManagerRuntime(ctx, StageManagerRuntimeOptions{Python: args.Python, Destination: args.Out, Project: args.Source,
			Cache: args.Cache, Offline: args.Offline, Wheelhouse: args.Wheelhouse})
	case len(requirements) > 0:
		return BuildRequirementsEnvironment(ctx, BuildRequirementsEnvironmentOptions{Requirements: requirements, Out: args.Out,
			Python: args.Python, Cache: args.Cache, Wheelhouse: args.Wheelhouse, Offline: args.Offline, Sealed: args.Sealed,
			Explicit: true, Lock: args.Lock})
	}
	groups := args.Groups
	if groups == nil {
		groups = []string{}
	}
	executable, err := clientBuildEnvironment(ctx, BuildEnvironmentOptions{Source: args.Source, Out: args.Out, Python: args.Python,
		Cache: args.Cache, Extras: extras, Groups: groups, AllExtras: args.AllExtras, NoInstallProject: args.NoInstallProject,
		Frozen: !args.Resolve, Sealed: args.Sealed, Offline: args.Offline, Explicit: true})
	if err != nil {
		return "", err
	}
	if args.RecordSelection {
		if err := recordSelection(ctx, args.Source, args.Extras); err != nil {
			return "", err
		}
	}
	return executable, nil
}
