package terminal

import (
	"regexp"
	"strings"
)

// Shell variable name
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Whether the name can be a shell variable's
func VariableName(name string) bool {
	return envName.MatchString(name)
}

// Whether a token is a leading variable assignment
func assignment(token string) bool {
	name, _, ok := strings.Cut(token, "=")
	return ok && name != "" && envName.MatchString(name)
}

// One shell word with its quotes and escapes kept
var shellWord = regexp.MustCompile(`(?s)^(?:'[^']*'?|"(?:\\.|[^"])*"?|\\.|[^\s;|&()])*`)

func readToken(command string, start int) (string, int) {
	end := start + shellWord.FindStringIndex(command[start:])[1]
	return command[start:end], end
}

// One lexical event of the shell scanner
type shellEvent struct {
	kind       string
	start, end int
	atStart    bool
}

// Quote aware scan of a command line shared by the sudo rewrite and the background rewrite, which also tracks groups and quotes
func scan(command string, background bool) []shellEvent {
	var events []shellEvent
	atStart := true
	parens, braces := 0, 0
	twoChar := []string{"&&", "||", ";;"}
	if background {
		twoChar = []string{"&&", "||", "&>"}
	}
	for i, n := 0, len(command); i < n; {
		ch := command[i]
		grouped := parens > 0 || braces > 0
		wasStart := atStart
		var kind string
		var end int
		switch {
		case space(ch):
			kind, end = "ws", i+1
			if grouped {
				kind = "skip"
			}
			atStart = atStart || ch == '\n'
		case ch == '#':
			kind, end = "comment", n
			if j := strings.IndexByte(command[i:], '\n'); j >= 0 {
				end = i + j
			}
		case background && ch == '\\' && i+1 < n:
			kind, end = "escape", i+2
		case background && (ch == '\'' || ch == '"'):
			_, end = readToken(command, i)
			kind = "word"
		case background && (ch == '(' || ch == ')' || ch == '{' && i+1 < n && space(command[i+1]) || ch == '}' && braces > 0):
			switch ch {
			case '(':
				parens++
			case ')':
				parens = max(parens-1, 0)
			case '{':
				braces++
			case '}':
				braces--
			}
			kind, end = "op", i+1
		case background && grouped:
			kind, end = "skip", i+1
		case prefixed(command[i:], twoChar):
			kind, end, atStart = "op", i+2, true
		case strings.IndexByte(";|&()", ch) >= 0:
			kind, end = "op", i+1
			atStart = ch != ')'
		default:
			var token string
			token, end = readToken(command, i)
			kind = "word"
			atStart = atStart && assignment(token)
		}
		end = max(end, i+1)
		events = append(events, shellEvent{kind: kind, start: i, end: end, atStart: wasStart})
		i = end
	}
	return events
}

func prefixed(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Options of env that take an operand, and those that take none
var (
	envValueOptions = map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true, "-a": true, "--argv0": true}
	envFlagOptions  = map[string]bool{"-i": true, "--ignore-environment": true, "-0": true, "--null": true, "-v": true, "--debug": true}
)

// The command with every sudo it runs reading its password from stdin, and how many there are
func RewriteSudo(command string) (string, int) {
	var out []string
	count := 0
	inEnv, operand, options := false, false, false
	for _, ev := range scan(command, false) {
		text := command[ev.start:ev.end]
		out = append(out, text)
		if ev.kind == "op" || ev.kind == "ws" && text == "\n" {
			inEnv, operand, options = false, false, false
		}
		if ev.kind != "word" || !(ev.atStart || inEnv) {
			continue
		}
		words, err := Split(text)
		if err != nil || len(words) != 1 || strings.ContainsAny(text, "$`*?[]~<>{}") {
			inEnv = false
			continue
		}
		word := words[0]
		if inEnv {
			switch {
			case operand:
				operand = false
				continue
			case options && (word == "-" || word == "--"):
				options = false
				continue
			case options && envValueOptions[word]:
				operand = true
				continue
			case options && (envFlagOptions[word] || longValueOption(word) || shortAttached(word)):
				continue
			case options && strings.HasPrefix(word, "-"):
				inEnv = false
				continue
			case assignment(word):
				options = false
				continue
			}
		} else if assignment(text) {
			continue
		}
		executable := word[strings.LastIndex(word, "/")+1:]
		inEnv, options = executable == "env", executable == "env"
		if executable == "sudo" {
			out[len(out)-1] += " -S -p ''"
			count++
		}
	}
	return strings.Join(out, ""), count
}

func longValueOption(word string) bool {
	for option := range envValueOptions {
		if strings.HasPrefix(option, "--") && strings.HasPrefix(word, option+"=") {
			return true
		}
	}
	return false
}

func shortAttached(word string) bool {
	for _, option := range []string{"-u", "-C", "-a"} {
		if strings.HasPrefix(word, option) && len(word) > 2 {
			return true
		}
	}
	return false
}

// `A && B &` wrapped as `A && { B & }` so bash never keeps a subshell waiting on B
func RewriteCompoundBackground(command string) string {
	chain := -1
	var rewrites [][2]int
	for _, ev := range scan(command, true) {
		text := command[ev.start:ev.end]
		switch {
		case ev.kind == "op" && (text == "&&" || text == "||"):
			chain = ev.end
		case ev.kind == "ws" && text == "\n" || ev.kind == "op" && (text == ";" || text == "|" || text == "}"):
			chain = -1
		case ev.kind == "op" && text == "&":
			j := ev.start - 1
			for j >= 0 && space(command[j]) {
				j--
			}
			if j >= 0 && (command[j] == '<' || command[j] == '>') {
				continue
			}
			if chain >= 0 {
				rewrites = append(rewrites, [2]int{chain, ev.start})
			}
			chain = -1
		}
	}
	result := command
	for k := len(rewrites) - 1; k >= 0; k-- {
		insert, amp := rewrites[k][0], rewrites[k][1]
		for insert < amp && space(result[insert]) {
			insert++
		}
		suffix := result[amp+1:]
		tail := strings.TrimLeft(suffix, " \t")
		separator := ""
		if tail != "" && (strings.IndexByte(";\n&|)}", tail[0]) < 0 || strings.HasPrefix(tail, "&>")) {
			separator = " ;"
		}
		result = result[:insert] + "{ " + result[insert:amp] + "& }" + separator + suffix
	}
	return result
}

// What sudo prints when it needs a password it can't ask for
var sudoHeadless = []string{"sudo: a password is required", "sudo: no tty present", "sudo: a terminal is required"}

// Whether sudo failed for want of a way to ask for its password
func SudoHeadless(output string) bool {
	for _, marker := range sudoHeadless {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// What sudo prints when the password it was given is wrong
var sudoWrong = []string{
	"sudo: authentication failed",
	"sudo: incorrect password attempt",
	"sudo: maximum 3 incorrect authentication attempts",
	"sudo: 3 incorrect password attempts",
}

// Whether sudo turned down the password it was given
func SudoWrongPassword(output string) bool {
	lowered := strings.ToLower(output)
	for _, marker := range sudoWrong {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}
