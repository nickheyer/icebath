package nemorelay

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

const (
	skillLoadHandledMetadataKey     = "nemo_relay.skill_load_handled"
	skillLoadPrecomputedMetadataKey = "nemo_relay.skill_loads"
)

var (
	catRejectedOptions      = []string{"-h", "--help", "--version", "--show-all", "--number-nonblank", "--show-ends", "--number", "--squeeze-blank", "--show-tabs", "--show-nonprinting"}
	catRejectedShortOptions = []rune{'A', 'b', 'e', 'E', 'n', 's', 't', 'T', 'v'}
	batAllowedOptions       = []string{"-p", "--plain"}
	powershellAllowedOpts   = []string{"asbytestream", "encoding", "force", "literalpath", "path", "raw", "readcount"}
	structuredReaderNames   = []string{"read", "readfile", "readtextfile", "readmultiplefiles", "fileread", "readresource", "getfilecontents", "readfilecontent"}
)

type skillLoadSource string

const (
	skillLoadSourceSkillTool      skillLoadSource = "skill_tool"
	skillLoadSourceStructuredRead skillLoadSource = "structured_read"
	skillLoadSourceShellRead      skillLoadSource = "shell_read"
)

type skillLoad struct {
	name   string
	source skillLoadSource
}

// Detects skill loads implied by a tool name and its arguments
func detectSkillLoads(toolName string, args any) []skillLoad {
	normalizedTool := normalizeSkillIdentifier(toolName)
	var source skillLoadSource
	var names []string
	switch {
	case normalizedTool == "skill" || normalizedTool == "skillview" || normalizedTool == "loadskill":
		source, names = skillLoadSourceSkillTool, skillToolNames(args)
	case isStructuredReader(toolName) && !hasPartialReadControls(args):
		source, names = skillLoadSourceStructuredRead, structuredSkillNames(args)
	case isShellTool(normalizedTool):
		source, names = skillLoadSourceShellRead, shellSkillNames(normalizedTool, args)
	default:
		return nil
	}
	loads := make([]skillLoad, 0)
	for _, name := range deduplicateStrings(names) {
		loads = append(loads, skillLoad{name: name, source: source})
	}
	return loads
}

// Reads skill loads a caller already computed into tool metadata
func precomputedSkillLoads(metadata any) ([]skillLoad, bool) {
	object, ok := metadata.(map[string]any)
	if !ok {
		return nil, false
	}
	entries, ok := object[skillLoadPrecomputedMetadataKey].([]any)
	if !ok {
		return nil, false
	}
	seen := map[string]struct{}{}
	loads := make([]skillLoad, 0)
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		nameValue, ok := entry["skill_name"].(string)
		if !ok {
			continue
		}
		name := strings.TrimSpace(nameValue)
		sourceValue, ok := entry["source"].(string)
		if !ok {
			continue
		}
		source := skillLoadSource(sourceValue)
		if source != skillLoadSourceSkillTool && source != skillLoadSourceStructuredRead && source != skillLoadSourceShellRead {
			continue
		}
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		loads = append(loads, skillLoad{name: name, source: source})
	}
	return loads, true
}

func skillToolNames(args any) []string {
	names := make([]string, 0)
	visitNamedValues(args, func(key string, value any) {
		text, ok := value.(string)
		if !ok {
			return
		}
		if key == "skill" || key == "skillname" || key == "name" {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				names = append(names, trimmed)
			}
		}
	})
	return names
}

func structuredSkillNames(args any) []string {
	names := make([]string, 0)
	visitNamedValues(args, func(key string, value any) {
		switch key {
		case "path", "filepath", "filename", "file", "paths", "uri", "absolutepath":
			collectPathSkillNames(value, &names)
		}
	})
	return names
}

func shellSkillNames(toolName string, args any) []string {
	commands := make([]string, 0)
	visitNamedValues(args, func(key string, value any) {
		if key == "command" || key == "cmd" {
			if text, ok := value.(string); ok {
				commands = append(commands, text)
			}
		}
	})
	allowDirectCat := toolName != "powershell" && toolName != "pwsh"
	names := make([]string, 0)
	for _, command := range commands {
		for _, path := range completeReaderPaths(command, allowDirectCat) {
			if name, ok := skillNameFromPath(path); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

func isStructuredReader(toolName string) bool {
	segments := make([]string, 0)
	for _, segment := range strings.FieldsFunc(toolName, func(r rune) bool { return !isASCIIAlphanumericRune(r) }) {
		segments = append(segments, normalizeSkillIdentifier(segment))
	}
	for length := 1; length <= len(segments); length++ {
		if slices.Contains(structuredReaderNames, strings.Join(segments[len(segments)-length:], "")) {
			return true
		}
	}
	return false
}

func isShellTool(toolName string) bool {
	switch toolName {
	case "sh", "bash", "zsh", "fish", "shell", "shellcommand", "exec", "execcommand", "execute", "terminal", "runcommand", "runshellcommand", "shellexec", "powershell", "pwsh":
		return true
	}
	return false
}

func hasPartialReadControls(value any) bool {
	partial := false
	visitNamedValues(value, func(key string, value any) {
		if partial {
			return
		}
		switch normalizeSkillIdentifier(key) {
		case "offset":
			offset, ok := jsonAsInt64(value)
			partial = !ok || offset != 0
		case "limit", "range", "head", "tail", "startline", "endline", "linestart", "lineend":
			partial = value != nil
		}
	})
	return partial
}

// Reads a JSON value as a signed integer the way serde_json as_i64 does
func jsonAsInt64(value any) (int64, bool) {
	if value == nil || !types.IsJSONInteger(value) {
		return 0, false
	}
	return types.JSONInt64(value)
}

// Reads a JSON value as an unsigned integer the way serde_json as_u64 does
func jsonAsUint64(value any) (uint64, bool) {
	if value == nil || !types.IsJSONInteger(value) {
		return 0, false
	}
	return types.JSONUint64(value)
}

func collectPathSkillNames(value any, names *[]string) {
	switch typed := value.(type) {
	case string:
		if name, ok := skillNameFromPath(typed); ok {
			*names = append(*names, name)
		}
	case []any:
		for _, item := range typed {
			collectPathSkillNames(item, names)
		}
	}
}

// Walks every object entry depth first with normalized keys in serde_json order
func visitNamedValues(value any, visit func(key string, value any)) {
	stack := []any{value}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch typed := current.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				visit(normalizeSkillIdentifier(key), typed[key])
				stack = append(stack, typed[key])
			}
		case []any:
			stack = append(stack, typed...)
		}
	}
}

func skillNameFromPath(path string) (string, bool) {
	if strings.TrimSpace(path) != path ||
		strings.HasPrefix(path, "'") || strings.HasPrefix(path, `"`) ||
		strings.HasSuffix(path, "'") || strings.HasSuffix(path, `"`) ||
		strings.HasSuffix(path, "/") || strings.HasSuffix(path, `\`) {
		return "", false
	}
	components := make([]string, 0)
	for _, component := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		components = append(components, component)
	}
	if len(components) < 2 {
		return "", false
	}
	parent, file := components[len(components)-2], components[len(components)-1]
	if !asciiEqualFold(file, "SKILL.md") || parent == "." || parent == ".." || strings.HasSuffix(parent, ":") {
		return "", false
	}
	return parent, true
}

func completeReaderPaths(command string, allowDirectCat bool) []string {
	words, ok := standaloneCommandWords(command)
	if !ok {
		return nil
	}
	return completeReaderWordPaths(words, allowDirectCat)
}

// Splits one simple command and rejects pipelines, redirects and substitutions
func standaloneCommandWords(command string) ([]string, bool) {
	if strings.ContainsAny(command, "\n\r") {
		return nil, false
	}
	words, err := splitShellWords(strings.ReplaceAll(command, `\`, `\\`))
	if err != nil || len(words) == 0 {
		return nil, false
	}
	for _, word := range words {
		if strings.ContainsAny(word, "|&;<>") || strings.Contains(word, "$(") || strings.Contains(word, "`") {
			return nil, false
		}
	}
	return words, true
}

func completeReaderWordPaths(words []string, allowDirectCat bool) []string {
	if len(words) == 0 {
		return nil
	}
	executable, ok := executableName(words[0])
	if !ok {
		return nil
	}
	rest := words[1:]
	switch executable {
	case "cat":
		if allowDirectCat {
			return catPaths(rest)
		}
	case "bat", "batcat":
		return batPaths(rest)
	case "get-content":
		return powershellContentPaths(rest)
	case "sh", "bash", "zsh":
		return posixShellWrapperPaths(executable, rest)
	case "fish":
		return fishShellWrapperPaths(rest)
	case "powershell", "pwsh":
		return powershellWrapperPaths(rest)
	}
	return nil
}

func posixShellWrapperPaths(executable string, words []string) []string {
	if len(words) != 2 {
		return nil
	}
	flag, command := words[0], words[1]
	allowed := (executable == "sh" && flag == "-c") || ((executable == "bash" || executable == "zsh") && (flag == "-c" || flag == "-lc"))
	if !allowed {
		return nil
	}
	commandWords, ok := standaloneCommandWords(command)
	if !ok {
		return nil
	}
	return posixReaderWordPaths(commandWords)
}

func fishShellWrapperPaths(words []string) []string {
	var command string
	switch {
	case len(words) == 2 && words[0] == "-c":
		command = words[1]
	case len(words) == 1:
		if after, found := strings.CutPrefix(words[0], "--command="); found {
			command = after
		}
	default:
		return nil
	}
	commandWords, ok := standaloneCommandWords(command)
	if !ok {
		return nil
	}
	return posixReaderWordPaths(commandWords)
}

func powershellWrapperPaths(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	flag, command := words[0], words[1:]
	if !asciiEqualFold(flag, "-command") || len(command) == 0 {
		return nil
	}
	if len(command) == 1 {
		commandWords, ok := standaloneCommandWords(command[0])
		if !ok {
			return nil
		}
		return powershellReaderWordPaths(commandWords)
	}
	return powershellReaderWordPaths(command)
}

func posixReaderWordPaths(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	executable, ok := executableName(words[0])
	if !ok {
		return nil
	}
	switch executable {
	case "cat":
		return catPaths(words[1:])
	case "bat", "batcat":
		return batPaths(words[1:])
	}
	return nil
}

func powershellReaderWordPaths(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	executable, ok := executableName(words[0])
	if !ok || executable != "get-content" {
		return nil
	}
	return powershellContentPaths(words[1:])
}

func catPaths(words []string) []string {
	for _, word := range words {
		if isRejectedOption(word, catRejectedOptions, catRejectedShortOptions) ||
			(strings.HasPrefix(word, "-") && word != "--" && !asciiEqualFold(word, "-u")) {
			return nil
		}
	}
	return positionalPaths(words)
}

func batPaths(words []string) []string {
	allowedCount := 0
	for _, word := range words {
		if containsFold(batAllowedOptions, word) {
			allowedCount++
		}
	}
	if allowedCount != 1 {
		return nil
	}
	for _, word := range words {
		if strings.HasPrefix(word, "-") && word != "--" && !containsFold(batAllowedOptions, word) {
			return nil
		}
	}
	return positionalPaths(words)
}

func containsFold(options []string, word string) bool {
	for _, option := range options {
		if asciiEqualFold(word, option) {
			return true
		}
	}
	return false
}

func isRejectedOption(word string, rejected []string, rejectedShort []rune) bool {
	for _, option := range rejected {
		if asciiEqualFold(word, option) || strings.HasPrefix(word, option+"=") {
			return true
		}
	}
	options, found := strings.CutPrefix(word, "-")
	if !found || strings.HasPrefix(options, "-") {
		return false
	}
	for _, option := range options {
		if slices.Contains(rejectedShort, option) {
			return true
		}
	}
	return false
}

func positionalPaths(words []string) []string {
	paths := make([]string, 0)
	for _, word := range words {
		if !strings.HasPrefix(word, "-") {
			paths = append(paths, word)
		}
	}
	return paths
}

func powershellContentPaths(words []string) []string {
	for _, word := range words {
		if !strings.HasPrefix(word, "-") {
			continue
		}
		option, ok := powershellOptionName(word)
		if !ok || !containsFold(powershellAllowedOpts, option) {
			return nil
		}
	}
	paths := make([]string, 0)
	for index := 0; index < len(words); {
		word := words[index]
		option, hasOption := powershellOptionName(word)
		if hasOption && (asciiEqualFold(option, "path") || asciiEqualFold(option, "literalpath")) {
			if value, ok := powershellOptionValue(word); ok {
				paths = append(paths, value)
			} else if index+1 < len(words) {
				paths = append(paths, words[index+1])
				index += 2
				continue
			}
		} else if hasOption && (asciiEqualFold(option, "encoding") || asciiEqualFold(option, "readcount")) {
			if _, ok := powershellOptionValue(word); !ok {
				index += 2
				continue
			}
		}
		if !strings.HasPrefix(word, "-") {
			paths = append(paths, word)
		}
		index++
	}
	return paths
}

func powershellOptionName(word string) (string, bool) {
	rest, found := strings.CutPrefix(word, "-")
	if !found {
		return "", false
	}
	if cut := strings.IndexAny(rest, ":="); cut >= 0 {
		return rest[:cut], true
	}
	return rest, true
}

func powershellOptionValue(word string) (string, bool) {
	cut := strings.IndexAny(word, ":=")
	if cut < 0 || cut+1 >= len(word) {
		return "", false
	}
	return word[cut+1:], true
}

// Returns the lowercase program name without directories or an exe suffix
func executableName(executable string) (string, bool) {
	name := executable
	if cut := strings.LastIndexAny(executable, `/\`); cut >= 0 {
		name = executable[cut+1:]
	}
	name = strings.TrimSuffix(asciiLower(name), ".exe")
	return name, name != ""
}

func asciiLower(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		builder.WriteByte(b)
	}
	return builder.String()
}

// Keeps only ASCII letters and digits in lowercase
func normalizeSkillIdentifier(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if isASCIIAlphanumericRune(r) {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func isASCIIAlphanumericRune(r rune) bool {
	return r < 0x80 && isASCIIAlphanumeric(byte(r))
}

func deduplicateStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// Compares two strings ignoring ASCII case only
func asciiEqualFold(left, right string) bool {
	return len(left) == len(right) && asciiLower(left) == asciiLower(right)
}
