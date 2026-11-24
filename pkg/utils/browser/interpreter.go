package browserutil

import (
	"errors"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// Environment variable a shell launcher's command string reads the URL from, so no character of the URL is ever read as shell syntax
const urlVariable = "BROWSER_URL"

// Characters cmd.exe reads as syntax even inside an argument: quoting, variable expansion, escapes, command chaining, redirection and grouping
const cmdSyntax = "\"%!^&|<>()\r\n"

// Error for a URL that a launcher run through cmd.exe could only pass by letting cmd.exe read part of it as syntax
var errCmdSyntax = errors.New("cmd.exe would read characters of the url as syntax")

// Command interpreter that reads an argument of a launcher as code
type interpreter uint8

const (
	// An argument that reaches its program as data
	plain interpreter = iota
	// sh, bash, dash, zsh, ksh and their kin, csh and tcsh: $NAME expands, '…' and "…" quote
	posixShell
	// fish: $NAME expands without word splitting, '…' and "…" quote
	fishShell
	// Windows PowerShell and PowerShell: '…' and "…" quote, the typographic quotes included
	powerShell
	// cmd.exe and the batch files it runs, which parse every argument after them
	cmdShell
)

// Command line of a launcher for target and the environment entries it adds: %s in a shell's command string becomes a reference to urlVariable quoted for where it stands, %s in PowerShell code the URL quoted for where it stands, %s anywhere else the URL itself as part of that one argument; an error wrapping errCmdSyntax when cmd.exe would parse the URL and it holds a character cmd.exe reads as syntax
func (l Launcher) command(target string) (argv, env []string, err error) {
	argv = slices.Clone(l.Args)
	kinds := codeKinds(argv)
	for i, arg := range argv {
		if !strings.Contains(arg, "%s") {
			continue
		}
		switch kinds[i] {
		case posixShell, fishShell:
			argv[i] = shellReferenced(arg, kinds[i] == fishShell)
			env = urlEnvironment(target)
		case powerShell:
			argv[i] = powerShellQuoted(arg, target)
		case cmdShell:
			if strings.ContainsAny(target, cmdSyntax) {
				return nil, nil, errCmdSyntax
			}
			argv[i] = strings.ReplaceAll(arg, "%s", target)
		default:
			argv[i] = strings.ReplaceAll(arg, "%s", target)
		}
	}
	return argv, env, nil
}

// Environment entries carrying the URL to a shell, listed in WSLENV too so it also crosses between Windows and the Windows Subsystem for Linux
func urlEnvironment(target string) []string {
	wslenv := urlVariable
	if existing := os.Getenv("WSLENV"); existing != "" {
		wslenv = strings.TrimSuffix(existing, ":") + ":" + urlVariable
	}
	return []string{urlVariable + "=" + target, "WSLENV=" + wslenv}
}

// Lower case program name of an argument, its directory dropped whether it is written with slashes or backslashes
func programName(arg string) string {
	return strings.ToLower(arg[strings.LastIndexAny(arg, `/\`)+1:])
}

// Interpreter a program argument names, plain for any other program
func interpreterOf(arg string) interpreter {
	name := programName(arg)
	if strings.HasSuffix(name, ".bat") || strings.HasSuffix(name, ".cmd") {
		return cmdShell
	}
	switch strings.TrimSuffix(name, ".exe") {
	case "sh", "bash", "rbash", "dash", "zsh", "ksh", "ksh93", "mksh", "oksh", "pdksh", "ash", "hush", "yash", "posh", "csh", "tcsh":
		return posixShell
	case "fish":
		return fishShell
	case "powershell", "pwsh":
		return powerShell
	case "cmd":
		return cmdShell
	}
	return plain
}

// Interpreter that reads each argument as code, for every interpreter named anywhere in the arguments so that wrappers such as env, nohup or busybox in front of it change nothing; cmd.exe claims every argument after it
func codeKinds(args []string) []interpreter {
	kinds := make([]interpreter, len(args))
	mark := func(kind interpreter, indexes []int) {
		for _, j := range indexes {
			if kinds[j] == plain {
				kinds[j] = kind
			}
		}
	}
	for i, arg := range args {
		switch kind := interpreterOf(arg); kind {
		case posixShell, fishShell:
			mark(kind, shellCode(args, i, kind == fishShell))
		case powerShell:
			mark(kind, powerShellCode(args, i))
		case cmdShell:
			for j := i + 1; j < len(args); j++ {
				kinds[j] = cmdShell
			}
		}
	}
	return kinds
}

// Indexes of the arguments a shell at args[at] runs as commands: the command string after a -c option (fish also --command and its -C or --init-command), none when the shell runs a script file or reads standard input
func shellCode(args []string, at int, fish bool) []int {
	var code []int
	command := false
	for j := at + 1; j < len(args); j++ {
		a := args[j]
		switch {
		case a == "--" || a == "-":
			if command && j+1 < len(args) {
				return append(code, j+1)
			}
			return code
		case strings.HasPrefix(a, "--"):
			name, _, inline := strings.Cut(a[2:], "=")
			switch name {
			case "command", "init-command":
				if inline {
					code = append(code, j)
				} else if name == "command" {
					command = true
				} else if j+1 < len(args) {
					j++
					code = append(code, j)
				}
			case "rcfile", "init-file":
				if !inline {
					j++
				}
			}
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			for _, c := range a[1:] {
				switch c {
				case 'c':
					command = true
				case 'o', 'O':
					j++
				case 'C':
					if fish && j+1 < len(args) {
						j++
						code = append(code, j)
					}
				}
			}
		default:
			if command {
				return append(code, j)
			}
			return code
		}
	}
	return code
}

// Indexes of the arguments PowerShell at args[at] runs as code: everything after -Command, the argument after -CommandWithArgs, every argument from the first positional one for Windows PowerShell, none for a -File script or a .ps1 file pwsh runs positionally. Options are matched as PowerShell matches them, by unambiguous prefix and case-insensitively
func powerShellCode(args []string, at int) []int {
	pwsh := strings.TrimSuffix(programName(args[at]), ".exe") == "pwsh"
	rest := func(from int) []int {
		var code []int
		for j := from; j < len(args); j++ {
			code = append(code, j)
		}
		return code
	}
	for j := at + 1; j < len(args); j++ {
		name, option := powerShellOption(args[j])
		switch {
		case !option:
			if pwsh && strings.HasSuffix(strings.ToLower(args[j]), ".ps1") {
				return nil
			}
			return rest(j)
		case name == "cwa" || abbreviates("commandwithargs", name, 8):
			if j+1 < len(args) {
				return []int{j + 1}
			}
			return nil
		case name == "c" || abbreviates("command", name, 2):
			if j+1 < len(args) && args[j+1] == "-" {
				return nil
			}
			return rest(j + 1)
		case abbreviates("file", name, 1):
			return nil
		case powerShellValued(name):
			j++
		}
	}
	return nil
}

// Lower case name of a PowerShell option written with -, -- or /, false for a positional argument
func powerShellOption(arg string) (string, bool) {
	switch {
	case strings.HasPrefix(arg, "--") && len(arg) > 2:
		return strings.ToLower(arg[2:]), true
	case strings.HasPrefix(arg, "-") && len(arg) > 1:
		return strings.ToLower(arg[1:]), true
	case strings.HasPrefix(arg, "/") && len(arg) > 1 && !strings.ContainsAny(arg[1:], `/\.`):
		return strings.ToLower(arg[1:]), true
	}
	return "", false
}

// Whether name is a prefix of option and holds at least the given number of characters
func abbreviates(option, name string, least int) bool {
	return len(name) >= least && strings.HasPrefix(option, name)
}

// Whether a PowerShell option takes the next argument as its value
func powerShellValued(name string) bool {
	switch name {
	case "e", "ec", "ea", "ep", "if", "of", "o", "v", "wd", "w":
		return true
	}
	for _, option := range [...]struct {
		full  string
		least int
	}{{"encodedcommand", 2}, {"encodedarguments", 8}, {"executionpolicy", 2}, {"inputformat", 3}, {"outputformat", 2},
		{"version", 2}, {"windowstyle", 2}, {"workingdirectory", 2}, {"configurationname", 4}, {"settingsfile", 2},
		{"custompipename", 2}, {"psconsolefile", 2}} {
		if abbreviates(option.full, name, option.least) {
			return true
		}
	}
	return false
}

// Shell command string with each %s replaced by a reference to urlVariable that stays one word wherever it stands: unquoted, inside single quotes (closed around it) or inside double quotes, command substitutions included. The URL itself never enters the command string, so even a misread quoting context can only mangle it, never run it
func shellReferenced(script string, fish bool) string {
	var b strings.Builder
	b.Grow(len(script) + 32)
	stack := []byte{0}
	push := func(c byte) { stack = append(stack, c) }
	pop := func() {
		if len(stack) > 1 {
			stack = stack[:len(stack)-1]
		}
	}
	for i := 0; i < len(script); i++ {
		top := stack[len(stack)-1]
		c := script[i]
		if c == '%' && i+1 < len(script) && script[i+1] == 's' {
			b.WriteString(shellReference(top, fish))
			i++
			continue
		}
		b.WriteByte(c)
		var next byte
		if i+1 < len(script) {
			next = script[i+1]
		}
		// Whether the byte after c is taken along with it, an escaped byte or the parenthesis of $(
		takeNext := false
		switch top {
		case '\'':
			switch {
			case c == '\'':
				pop()
			case fish && c == '\\' && (next == '\'' || next == '\\'):
				takeNext = true
			}
		case '"':
			switch {
			case c == '\\':
				takeNext = true
			case c == '"':
				pop()
			case c == '$' && next == '(':
				takeNext = true
				push('(')
			case c == '`' && !fish:
				push('`')
			}
		default:
			switch {
			case c == '\\':
				takeNext = true
			case c == '\'':
				push('\'')
			case c == '"':
				push('"')
			case c == '`' && !fish:
				if top == '`' {
					pop()
				} else {
					push('`')
				}
			case c == '(':
				push('(')
			case c == ')' && top == '(':
				pop()
			}
		}
		if takeNext && i+1 < len(script) {
			i++
			b.WriteByte(script[i])
		}
	}
	return b.String()
}

// Reference to urlVariable for a quoting context: unquoted (also inside a command substitution), inside single quotes, inside double quotes
func shellReference(context byte, fish bool) string {
	switch {
	case fish && context == '\'':
		return `'"$` + urlVariable + `"'`
	case fish:
		return `"$` + urlVariable + `"`
	case context == '\'':
		return `'"${` + urlVariable + `}"'`
	case context == '"':
		return "${" + urlVariable + "}"
	}
	return `"${` + urlVariable + `}"`
}

// PowerShell code with each %s replaced by the URL as a literal for the context it stands in: a single-quoted string outside strings, the URL with its single quotes doubled inside a single-quoted string, the URL with backtick escapes inside a double-quoted string
func powerShellQuoted(code, target string) string {
	var b strings.Builder
	b.Grow(len(code) + 2*len(target))
	stack := []rune{0}
	push := func(r rune) { stack = append(stack, r) }
	pop := func() {
		if len(stack) > 1 {
			stack = stack[:len(stack)-1]
		}
	}
	for i := 0; i < len(code); {
		top := stack[len(stack)-1]
		if strings.HasPrefix(code[i:], "%s") {
			switch top {
			case '\'':
				b.WriteString(powerShellSingle(target))
			case '"':
				b.WriteString(powerShellDouble(target))
			default:
				b.WriteByte('\'')
				b.WriteString(powerShellSingle(target))
				b.WriteByte('\'')
			}
			i += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(code[i:])
		b.WriteString(code[i : i+size])
		i += size
		next, nextSize := utf8.DecodeRuneInString(code[i:])
		// Whether the rune after r is taken along with it: an escaped rune, the second quote of a doubled quote, the parenthesis of $(
		takeNext := false
		switch top {
		case '\'':
			if isPowerShellSingleQuote(r) {
				if nextSize > 0 && isPowerShellSingleQuote(next) {
					takeNext = true
				} else {
					pop()
				}
			}
		case '"':
			switch {
			case r == '`':
				takeNext = true
			case isPowerShellDoubleQuote(r):
				if nextSize > 0 && isPowerShellDoubleQuote(next) {
					takeNext = true
				} else {
					pop()
				}
			case r == '$' && next == '(':
				takeNext = true
				push('(')
			}
		default:
			switch {
			case r == '`':
				takeNext = true
			case isPowerShellSingleQuote(r):
				push('\'')
			case isPowerShellDoubleQuote(r):
				push('"')
			case r == '(' || r == '{':
				push(r)
			case (r == ')' && top == '(') || (r == '}' && top == '{'):
				pop()
			}
		}
		if takeNext && nextSize > 0 {
			b.WriteString(code[i : i+nextSize])
			i += nextSize
		}
	}
	return b.String()
}

// The single quote and the typographic quotes PowerShell treats as one
func isPowerShellSingleQuote(r rune) bool {
	return r == '\'' || r == '‘' || r == '’' || r == '‚' || r == '‛'
}

// The double quote and the typographic quotes PowerShell treats as one
func isPowerShellDoubleQuote(r rune) bool { return r == '"' || r == '“' || r == '”' || r == '„' }

// Text for the inside of a single-quoted PowerShell string, each single quote doubled
func powerShellSingle(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isPowerShellSingleQuote(r) {
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Text for the inside of a double-quoted PowerShell string, the escape character, variables and double quotes escaped with a backtick
func powerShellDouble(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '`' || r == '$' || isPowerShellDoubleQuote(r) {
			b.WriteByte('`')
		}
		b.WriteRune(r)
	}
	return b.String()
}
