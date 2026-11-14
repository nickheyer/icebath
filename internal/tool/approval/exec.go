package approval

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/shell"
)

type interpreter struct {
	family string
	name   *regexp.Regexp
}

var interpreters = []interpreter{
	{"python", regexp.MustCompile(`\A(?:py(?:\.exe)?|python[23]?(?:\.\p{Nd}+)*(?:\.exe)?)\z`)},
	{"node", regexp.MustCompile(`\A(?:node(?:js)?(?:\.exe)?)\z`)},
	{"perl", regexp.MustCompile(`\A(?:perl[0-9]*(?:\.\p{Nd}+)*(?:\.exe)?)\z`)},
	{"ruby", regexp.MustCompile(`\A(?:ruby[0-9.]*(?:\.exe)?)\z`)},
	{"php", regexp.MustCompile(`\A(?:php(?:\.exe)?)\z`)},
	{"powershell", regexp.MustCompile(`\A(?:powershell(?:\.exe)?|pwsh(?:\.exe)?)\z`)},
	{"bun", regexp.MustCompile(`\A(?:bun(?:\.exe)?)\z`)},
	{"deno", regexp.MustCompile(`\A(?:deno(?:\.exe)?)\z`)},
}

var (
	interpreterExecFlags = map[string]map[string]bool{
		"python": set("-c"), "node": set("-e", "--eval", "-p", "--print"), "perl": set("-e", "--eval"), "ruby": set("-e"),
		"php": set("-r"), "powershell": set("-command", "-c", "-file", "-f"),
		"bun": set("-e", "--eval"), "deno": set("eval", "-e", "--eval"),
	}
	interpreterWithArg = map[string][]string{
		"python": {"-W", "-X", "--check-hash-based-pycs"},
		"node": {"-C", "--conditions", "--cpu-prof-dir", "--diagnostic-dir", "--icu-data-dir", "--import", "--loader",
			"--openssl-config", "--require", "--title"},
		"perl": {"-0", "-F", "-I", "-M", "-m", "-x"}, "ruby": {"-C", "-E", "-F", "-I", "-K", "-r"},
		"php": {"-c", "-d", "-z"},
		"powershell": {"-configurationname", "-custompipename", "-executionpolicy", "-inputformat", "-outputformat",
			"-settingsfile", "-version", "-windowstyle", "-workingdirectory"},
		"bun": {"--config", "--cwd", "--env-file", "--preload", "--require"}, "deno": {"-L", "--log-level"},
	}
	readToolExecFlags = map[string]map[string]bool{
		"sort": set("--compress-program"), "rg": set("--pre", "--hostname-bin"), "ag": set("--pager"),
		"man": set("--pager", "--html", "-P", "-H"),
	}
	readToolLongWithArg = map[string]map[string]bool{
		"rg": set("--after-context", "--before-context", "--color", "--colors", "--context", "--context-separator",
			"--dfa-size-limit", "--encoding", "--engine", "--field-context-separator", "--field-match-separator",
			"--file", "--generate", "--glob", "--hostname-bin", "--hyperlink-format", "--iglob", "--ignore-file",
			"--max-columns", "--max-count", "--max-depth", "--max-filesize", "--path-separator", "--pre", "--pre-glob",
			"--regex-size-limit", "--regexp", "--replace", "--sort", "--sortr", "--threads", "--type", "--type-add",
			"--type-clear", "--type-not"),
		"sort": set("--batch-size", "--buffer-size", "--compress-program", "--field-separator", "--files0-from", "--key",
			"--output", "--parallel", "--random-source", "--sort", "--temporary-directory"),
		"man": set("--config-file", "--encoding", "--extension", "--locale", "--manpath", "--pager", "--preprocessor",
			"--prompt", "--recode", "--sections", "--systems"),
		"ag": set("--ackmate-dir-filter", "--color-line-number", "--color-match", "--color-path", "--depth",
			"--filename-pattern", "--file-search-regex", "--ignore", "--ignore-dir", "--max-count", "--pager",
			"--path-to-ignore", "--width", "--workers"),
	}
	readToolShortWithArg = map[string]string{"rg": "efEmjgdtTABCMr", "sort": "koStT", "man": "CRLmMSserEPp", "ag": "gGmpW"}
	bashOptionsWithArg   = set("-O", "+O", "-o", "+o", "--init-file", "--rcfile")
	shellNames           = set("bash", "sh", "zsh", "ksh", "dash")
)

const (
	bashShortLetters  = "ilrsDcabefhkmnptuvxBCEHPTOo"
	parserLimitDesc   = "command parser limit exceeded"
	malformedExecDesc = "command parser limit or malformed executable payload"
	scriptFlagDesc    = "script execution via -e/-c flag"
	scriptHeredocDesc = "script execution via heredoc"
	shellFlagDesc     = "shell command via -c/-lc flag"
)

func interpreterFamily(executable string) string {
	name := pyLower(pyBasename(executable))
	for _, it := range interpreters {
		if it.name.MatchString(name) {
			return it.family
		}
	}
	return ""
}

func segmentTokens(segment string, start int) ([]string, bool) {
	tokens, err := shell.Lexer{Operators: "<>"}.Split(segment[start:])
	if err != nil {
		return nil, false
	}
	return tokens, true
}

func interpreterExecFlag(family string, args []string) bool {
	flags, withArg := interpreterExecFlags[family], interpreterWithArg[family]
	powershell := family == "powershell"
	skipValue := false
	for _, token := range args {
		if skipValue {
			skipValue = false
			continue
		}
		if token == "--" || (!powershell && !strings.HasPrefix(token, "-")) {
			return family == "deno" && pyLower(token) == "eval"
		}
		option, _, equals := strings.Cut(token, "=")
		comparable := option
		if powershell {
			comparable = pyLower(option)
		}
		if flags[comparable] {
			return true
		}
		attached := false
		for _, short := range withArg {
			if strings.HasPrefix(short, "-") && !strings.HasPrefix(short, "--") && strings.HasPrefix(option, short) && runeCount(option) > runeCount(short) {
				attached = true
				break
			}
		}
		if !powershell && !strings.HasPrefix(option, "--") && runeCount(option) > 2 && !attached {
			for _, r := range option[1:] {
				if flags["-"+string(r)] {
					return true
				}
			}
		}
		skipValue = contains(withArg, comparable) && !equals
	}
	return false
}

func contains(list []string, s string) bool {
	for _, it := range list {
		if it == s {
			return true
		}
	}
	return false
}

func bashExecPayload(args []string) (bool, string, bool) {
	for index := 0; index < len(args); {
		token := args[index]
		if token == "--" || !(strings.HasPrefix(token, "-") || strings.HasPrefix(token, "+")) {
			break
		}
		if bashOptionsWithArg[token] {
			index += 2
			continue
		}
		chars := token[1:]
		valid := true
		for _, r := range chars {
			if !strings.ContainsRune(bashShortLetters, r) {
				valid = false
				break
			}
		}
		if strings.HasPrefix(token, "--") || !valid {
			index++
			continue
		}
		consumed := 0
		if strings.ContainsAny(chars, "Oo") {
			consumed = 1
		}
		if strings.Contains(chars, "c") {
			payload := index + 1 + consumed
			if payload < len(args) {
				return true, args[payload], true
			}
			return true, "", false
		}
		index += 1 + consumed
	}
	return false, "", false
}

func readToolExecFlag(tool string, args []string) (string, string, bool) {
	flags := readToolExecFlags[tool]
	for index := 0; index < len(args); {
		token := args[index]
		if token == "--" {
			break
		}
		option, value, equals := strings.Cut(token, "=")
		payload, hasPayload := value, equals
		matched := ""
		if flags[option] {
			matched = option
		}
		if tool == "man" && (strings.HasPrefix(token, "-P") || strings.HasPrefix(token, "-H")) && runeCount(token) > 2 {
			matched, payload, hasPayload = token[:2], token[2:], true
		}
		switch {
		case matched != "":
			if !hasPayload && index+1 < len(args) {
				payload, hasPayload = args[index+1], true
			}
			if hasPayload && payload != "" {
				return matched, payload, true
			}
			if hasPayload && !strings.Contains(token, "=") {
				index += 2
			} else {
				index++
			}
		case readToolLongWithArg[tool][option] && !equals:
			index += 2
		case strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") && runeCount(token) > 1:
			shorts := readToolShortWithArg[tool]
			rs := []rune(token)
			owner := -1
			for k := 1; k < len(rs); k++ {
				if strings.ContainsRune(shorts, rs[k]) {
					owner = k
					break
				}
			}
			if owner == len(rs)-1 {
				index += 2
			} else {
				index++
			}
		default:
			index++
		}
	}
	return "", "", false
}

type finding struct {
	desc       string
	payload    string
	hasPayload bool
}

func executionFindings(command string) []finding {
	var out []finding
	for _, segment := range topSegments(command) {
		for _, sp := range wordSpans(segment) {
			executable := deobfuscateWord(sp.word)
			tokens, ok := segmentTokens(segment, sp.start)
			name := pyLower(pyBasename(executable))
			family := interpreterFamily(executable)
			_, readTool := readToolExecFlags[name]
			if !ok {
				if family != "" || readTool {
					out = append(out, finding{desc: malformedExecDesc})
				}
				continue
			}
			if len(tokens) == 0 {
				continue
			}
			args := tokens[1:]
			if family != "" && interpreterExecFlag(family, args) {
				out = append(out, finding{desc: scriptFlagDesc})
				continue
			}
			if family != "" && hasHeredocToken(args) {
				out = append(out, finding{desc: scriptHeredocDesc})
				continue
			}
			if shellNames[name] {
				if found, payload, has := bashExecPayload(args); found {
					out = append(out, finding{desc: shellFlagDesc, payload: payload, hasPayload: has})
				}
			}
			if readTool {
				if option, program, found := readToolExecFlag(name, args); found {
					out = append(out, finding{desc: "arbitrary program execution via " + name + " " + option, payload: program, hasPayload: true})
				}
			}
		}
	}
	return out
}

func hasHeredocToken(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "<<") {
			return true
		}
	}
	return false
}

func splitEnvString(payload string) ([]string, bool) {
	escapes := map[rune]string{'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v",
		'#': "#", '$': "$", '"': "\"", '\'': "'", '\\': "\\"}
	rs := []rune(payload)
	var args []string
	var word strings.Builder
	var quote rune
	started := false
	for index := 0; index < len(rs); {
		char := rs[index]
		index++
		if char == '\\' {
			if index == len(rs) {
				return nil, false
			}
			escaped := rs[index]
			if quote == '\'' && escaped != '\'' && escaped != '\\' {
				word.WriteRune(char)
				started = true
				continue
			}
			index++
			if escaped == 'c' {
				if quote != 0 {
					return nil, false
				}
				break
			}
			if escaped == '_' && quote == 0 {
				if started {
					args = append(args, word.String())
				}
				word.Reset()
				started = false
				continue
			}
			if _, ok := escapes[escaped]; !ok && escaped != '_' {
				return nil, false
			}
			if escaped == '_' {
				word.WriteByte(' ')
			} else {
				word.WriteString(escapes[escaped])
			}
			started = true
			continue
		}
		if (char == '\'' || char == '"') && (quote == 0 || char == quote) {
			if quote == 0 {
				quote = char
			} else {
				quote = 0
			}
			started = true
			continue
		}
		if quote == 0 && strings.ContainsRune(" \t\n\r\v\f", char) {
			if started {
				args = append(args, word.String())
			}
			word.Reset()
			started = false
			continue
		}
		if quote == 0 && char == '#' && !started {
			break
		}
		if char == '$' && quote != '\'' {
			return nil, false
		}
		word.WriteRune(char)
		started = true
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		args = append(args, word.String())
	}
	return args, true
}

func envSplitPayload(tokens []string) (string, bool) {
	for index := 1; index < len(tokens); {
		token := tokens[index]
		if token == "--" || !strings.HasPrefix(token, "-") {
			return "", false
		}
		option, value, equals := strings.Cut(token, "=")
		if option == "--split-string" || strings.HasPrefix(token, "-S") {
			attached := len(token) > 2
			if option == "--split-string" {
				attached = equals
			}
			if !attached {
				index++
			}
			payload := ""
			switch {
			case attached && option == "--split-string":
				payload = value
			case attached:
				payload = token[2:]
			case index < len(tokens):
				payload = tokens[index]
			}
			args, ok := splitEnvString(payload)
			if !ok {
				return "", false
			}
			if index+1 < len(tokens) {
				args = append(args, tokens[index+1:]...)
			}
			return shell.Join(args), true
		}
		if !equals && wrapperArgOptions["env"][option] {
			index += 2
		} else {
			index++
		}
	}
	return "", false
}
