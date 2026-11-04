package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/home"
)

// Detects git operations that would rewrite the checkout backing this process

var selfRepoWorktreeMutations = map[string]bool{
	"checkout": true, "switch": true, "rebase": true, "merge": true, "pull": true, "restore": true, "clean": true,
	"cherry-pick": true, "revert": true, "bisect": true,
}

var selfRepoWorktreeTargetActions = map[string]bool{"move": true, "remove": true}

var selfRepoStashSafeActions = map[string]bool{"list": true, "show": true, "create": true, "store": true, "drop": true, "clear": true}

var selfRepoResetWorktreeModes = map[string]bool{"--hard": true, "--merge": true, "--keep": true}

var selfRepoKnownGitBuiltins = map[string]bool{
	"add": true, "am": true, "apply": true, "blame": true, "branch": true, "bundle": true, "cat-file": true, "clean": true,
	"clone": true, "commit": true, "config": true, "describe": true, "diff": true, "fetch": true, "format-patch": true,
	"grep": true, "help": true, "init": true, "log": true, "ls-files": true, "ls-remote": true, "ls-tree": true,
	"maintenance": true, "merge-base": true, "mv": true, "notes": true, "push": true, "range-diff": true, "reflog": true,
	"remote": true, "repack": true, "replace": true, "reset": true, "restore": true, "rev-list": true, "rev-parse": true,
	"rm": true, "shortlog": true, "show": true, "show-ref": true, "stash": true, "status": true, "submodule": true,
	"tag": true, "worktree": true,
}

var selfRepoShellExecutables = map[string]bool{"bash": true, "dash": true, "ksh": true, "sh": true, "zsh": true}

var selfRepoAssignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var selfRepoResetHardRe = regexp.MustCompile(`^--h(?:a(?:r(?:d)?)?)?$`)

// A heredoc opener with a quoted delimiter needing its closing quote or a bare word
var selfRepoHeredocOpenerRe = regexp2.MustCompile(`^<<(?<dash>-?)[ \t]*(?:(?<q>['"])(?<quoted>.*?)\k<q>|(?!['"])(?<bare>[^\s;|&<>]*))`, regexp2.None)

var selfRepoNoOptions = map[string]bool{}

// Wrapper executables skipped to reach the real command and the options that consume an argument
var selfRepoWrapperOptionsWithArg = map[string]map[string]bool{
	"sudo": {"-C": true, "--chdir": true, "-c": true, "--close-from": true, "-g": true, "--group": true, "-h": true, "--host": true,
		"-p": true, "--prompt": true, "-R": true, "--chroot": true, "-T": true, "--command-timeout": true, "-u": true, "--user": true},
	"env":     {"-a": true, "--argv0": true, "-C": true, "--chdir": true, "-S": true, "--split-string": true, "-u": true, "--unset": true},
	"command": selfRepoNoOptions, "builtin": selfRepoNoOptions, "nohup": selfRepoNoOptions, "setsid": selfRepoNoOptions,
	"exec": {"-a": true}, "nice": {"-n": true, "--adjustment": true},
	"time": {"-f": true, "--format": true, "-o": true, "--output": true},
}

const selfRepoMaxRecursion = 4

var selfRepoGitGlobalOptionsWithArg = map[string]bool{"-C": true, "-c": true, "--work-tree": true, "--git-dir": true, "--namespace": true, "--exec-path": true}

type selfRepoHeredoc struct {
	delimiter string
	stripTabs bool
	opener    int
	body      []string
}

// One paren, dollar paren, or backtick nesting level and its live quote state
type selfRepoShellContext struct {
	kind   string
	opener int
	quote  byte
}

// RunningSourceRoot is the source checkout backing this binary when one exists
func RunningSourceRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(resolved, ".git")); err != nil {
		return ""
	}
	return resolved
}

func selfRepoResolve(pathStr, base string) string {
	expanded := expandUser(pathStr)
	joined := expanded
	if !filepath.IsAbs(expanded) {
		joined = filepath.Join(base, expanded)
	}
	abs, err := filepath.Abs(joined)
	if err != nil {
		abs = joined
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return resolveNonStrict(abs)
}

func selfRepoIsWithin(p, root string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func selfRepoExecutableName(value string) string {
	base := filepath.Base(strings.ReplaceAll(value, "\\", "/"))
	return strings.ToLower(strings.TrimSuffix(base, ".exe"))
}

func selfRepoAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Deobfuscated words of the simple command at start up to a newline, redirection, or comment
func selfRepoShellWordsAt(command string, start int) []string {
	var words []string
	cursor := start
	for n := 0; n < 64; n++ {
		wordStart, wordEnd, raw := readShellWord(command, cursor)
		if wordStart == wordEnd || (len(words) > 0 && strings.Contains(command[cursor:wordStart], "\n")) ||
			isShellCommentStart(command, wordStart) ||
			(selfRepoAllDigits(raw) && wordEnd < len(command) && (command[wordEnd] == '<' || command[wordEnd] == '>')) {
			break
		}
		words = append(words, deobfuscateShellWordForDetection(raw))
		cursor = wordEnd
	}
	return words
}

// Index of the first positional at or after start, a double dash ends options
func selfRepoConsumeOptions(words []string, start int, optionsWithArg map[string]bool) int {
	index := start
	for index < len(words) && strings.HasPrefix(words[index], "-") && words[index] != "-" {
		if words[index] == "--" {
			return index + 1
		}
		if !strings.Contains(words[index], "=") && optionsWithArg[words[index]] {
			index += 2
		} else {
			index++
		}
	}
	return index
}

// Splits leading assignments and wrappers off into env, executable, and args
func selfRepoCommandParts(words []string) (map[string]string, string, []string, bool) {
	env := map[string]string{}
	index := 0
	for index < len(words) {
		if selfRepoAssignmentRe.MatchString(words[index]) {
			parts := strings.SplitN(words[index], "=", 2)
			env[parts[0]] = parts[1]
			index++
			continue
		}
		executable := selfRepoExecutableName(words[index])
		wrapperOptions, isWrapper := selfRepoWrapperOptionsWithArg[executable]
		if !isWrapper {
			return env, words[index], words[index+1:], true
		}
		if executable == "command" && index+1 < len(words) && (words[index+1] == "-v" || words[index+1] == "-V") {
			break
		}
		index = selfRepoConsumeOptions(words, index+1, wrapperOptions)
	}
	return env, "", nil, false
}

func scopeKeyOf(openers []int) string {
	parts := make([]string, len(openers))
	for i, o := range openers {
		parts[i] = fmt.Sprint(o)
	}
	return strings.Join(parts, ",")
}

// Maps each command start to the tuple of enclosing substitution and subshell openers
func selfRepoScopeKeys(command string, starts []int) map[int][]int {
	contexts := []*selfRepoShellContext{{kind: "root", opener: -1}}
	scopes := map[int][]int{}
	cursor := 0
	uniq := map[int]bool{}
	var ordered []int
	for _, s := range starts {
		if !uniq[s] {
			uniq[s] = true
			ordered = append(ordered, s)
		}
	}
	sort.Ints(ordered)
	for _, start := range ordered {
		for cursor < start {
			ctx := contexts[len(contexts)-1]
			quote := ctx.quote
			char := command[cursor]
			closes := quote == 0 && len(contexts) > 1
			switch {
			case quote != 0 && char == quote:
				ctx.quote = 0
			case quote == '\'':
			case char == '\\' && cursor+1 < start:
				cursor++
			case quote == 0 && (char == '\'' || char == '"'):
				ctx.quote = char
			case strings.HasPrefix(command[cursor:], "$("):
				contexts = append(contexts, &selfRepoShellContext{kind: "$(", opener: cursor})
				cursor++
			case quote == 0 && char == '(':
				contexts = append(contexts, &selfRepoShellContext{kind: "(", opener: cursor})
			case (char == ')' && closes && (ctx.kind == "(" || ctx.kind == "$(")) || (char == '`' && closes && ctx.kind == "`"):
				contexts = contexts[:len(contexts)-1]
			case char == '`':
				contexts = append(contexts, &selfRepoShellContext{kind: "`", opener: cursor})
			}
			cursor++
		}
		key := make([]int, 0, len(contexts)-1)
		for _, c := range contexts[1:] {
			key = append(key, c.opener)
		}
		scopes[start] = key
	}
	return scopes
}

// The list or grouping operator or newline immediately preceding a command start
func selfRepoOperatorBefore(command string, start int) string {
	head := strings.TrimRight(command[:start], " \t\r\n\f\v")
	for _, n := range []int{2, 1} {
		if len(head) >= n {
			tail := head[len(head)-n:]
			switch tail {
			case "&&", "||", ";", "|", "&", "(", "{":
				return tail
			}
		}
	}
	if strings.Contains(command[len(head):start], "\n") {
		return "\n"
	}
	return ""
}

// Directory a cd or pushd would land in when it exists
func selfRepoCdTarget(executable string, args []string, cwd string) string {
	name := selfRepoExecutableName(executable)
	if name != "cd" && name != "pushd" {
		return ""
	}
	index := selfRepoConsumeOptions(args, 0, selfRepoNoOptions)
	if index >= len(args) || args[index] == "-" {
		return ""
	}
	target := selfRepoResolve(args[index], cwd)
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		return target
	}
	return ""
}

// Script string owned by a shell dash c when present
func selfRepoShellScriptArg(args []string) (string, bool) {
	hasC, payload, hasPayload := bashExecPayload(args)
	if hasC {
		return payload, hasPayload && payload != ""
	}
	for index, arg := range args {
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			return "", false
		}
		if strings.Contains(arg[1:], "c") {
			if index+1 < len(args) {
				return args[index+1], args[index+1] != ""
			}
			return "", false
		}
	}
	return "", false
}

// A bare shell with no script executes whatever it reads on stdin
func selfRepoExecutesHeredocBody(words []string) bool {
	_, executable, args, ok := selfRepoCommandParts(words)
	if !ok || !selfRepoShellExecutables[selfRepoExecutableName(executable)] {
		return false
	}
	if _, has := selfRepoShellScriptArg(args); has {
		return false
	}
	for _, arg := range args {
		if arg != "" && !strings.HasPrefix(arg, "-") {
			return false
		}
	}
	return true
}

func isSpaceByteSelfRepo(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

// A brace at index is a group delimiter on its own word rather than an expansion
func selfRepoGroupToken(masked string, index int) bool {
	before, after := byte(' '), byte(' ')
	if index > 0 {
		before = masked[index-1]
	}
	if index+1 < len(masked) {
		after = masked[index+1]
	}
	return (isSpaceByteSelfRepo(before) || strings.IndexByte("(;&|", before) >= 0) && (isSpaceByteSelfRepo(after) || strings.IndexByte(";)|&", after) >= 0)
}

// Offset where the pipeline carrying a heredoc ends
func selfRepoPipelineEnd(masked string, opener int) int {
	depth := 0
	level := -1
	levelSet := false
	afterPipe := false
	backtick := false
	for sc := newShellScan(masked, 0, -1, scanOpts{comments: true}); ; {
		step, ok := sc.next()
		if !ok {
			break
		}
		if !levelSet && step.i >= opener {
			level, levelSet = depth, true
		}
		if step.kind == stepComment {
			continue
		}
		if step.kind != stepChar || step.quote != 0 {
			afterPipe = false
			continue
		}
		char := masked[step.i]
		if isSpaceByteSelfRepo(char) {
			if char == '\n' && levelSet && level == 0 && depth == 0 && !afterPipe {
				return step.i
			}
			continue
		}
		if char == ';' || char == '&' || char == '|' {
			if char == '&' && ((step.i > 0 && (masked[step.i-1] == '<' || masked[step.i-1] == '>')) || strings.HasPrefix(masked[step.i:], "&>")) {
				continue
			}
			if char == '|' && !strings.HasPrefix(masked[step.i:], "||") {
				afterPipe = true
			} else if levelSet && level == 0 && depth == 0 && (step.i == 0 || masked[step.i-1] != '|') {
				return step.i
			}
			continue
		}
		afterPipe = false
		switch {
		case char == '`':
			if backtick {
				depth--
			} else {
				depth++
			}
			backtick = !backtick
		case char == '(' || (char == '{' && selfRepoGroupToken(masked, step.i)):
			depth++
		case char == ')' || (char == '}' && selfRepoGroupToken(masked, step.i)):
			depth--
			if levelSet && depth < level {
				rest := strings.TrimLeft(masked[step.j:], " \t")
				if !strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, "||") {
					return step.i
				}
				level = depth
			}
		}
	}
	return len(masked)
}

func intSliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Whether a bare shell reads the heredoc body through the owner or anything downstream in its pipeline
func selfRepoShellConsumesHeredoc(masked string, starts []int, scopes map[int][]int, opener int) bool {
	scope := scopes[opener]
	owner, hasOwner := -1, false
	for _, start := range starts {
		if start <= opener && intSliceEqual(scopes[start], scope) && start > owner {
			owner, hasOwner = start, true
		}
	}
	end := selfRepoPipelineEnd(masked, opener)
	var candidates []int
	if hasOwner {
		candidates = append(candidates, owner)
	}
	for _, start := range starts {
		if opener < start && start < end {
			candidates = append(candidates, start)
		}
	}
	for _, start := range candidates {
		if selfRepoExecutesHeredocBody(selfRepoShellWordsAt(masked, start)) {
			return true
		}
	}
	return false
}

// Heredoc openers on one command line with their offsets
func selfRepoHeredocSpecs(line string) []*selfRepoHeredoc {
	var specs []*selfRepoHeredoc
	var quote byte
	index := 0
	for index < len(line) {
		char := line[index]
		if quote != 0 {
			if char == '\\' && quote == '"' && index+1 < len(line) {
				index++
			} else if char == quote {
				quote = 0
			}
		} else if char == '\'' || char == '"' {
			quote = char
		}
		if quote != 0 || !strings.HasPrefix(line[index:], "<<") || strings.HasPrefix(line[index:], "<<<") {
			index++
			continue
		}
		m, _ := selfRepoHeredocOpenerRe.FindStringMatch(line[index:])
		if m == nil {
			break
		}
		start := index
		index += len(m.String())
		delimiter := ""
		if g := m.GroupByName("q"); g != nil && len(g.Captures) > 0 {
			delimiter = m.GroupByName("quoted").String()
		} else {
			delimiter = m.GroupByName("bare").String()
		}
		if delimiter != "" {
			dash := m.GroupByName("dash")
			specs = append(specs, &selfRepoHeredoc{delimiter: delimiter, stripTabs: dash != nil && dash.String() == "-", opener: start})
		}
	}
	return specs
}

// Splits into physical lines keeping their newline terminators
func selfRepoSplitLines(command string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case '\n':
			lines = append(lines, command[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(command) && command[i+1] == '\n' {
				lines = append(lines, command[start:i+2])
				i++
			} else {
				lines = append(lines, command[start:i+1])
			}
			start = i + 1
		}
	}
	if start < len(command) {
		lines = append(lines, command[start:])
	}
	return lines
}

// Physical line plus every line a trailing backslash joins onto it, blanked to keep offsets
func selfRepoJoinContinuedLine(lines []string, index int) (string, int) {
	line := lines[index]
	index++
	for index < len(lines) {
		text := strings.TrimRight(line, "\r\n")
		newline := line[len(text):]
		stripped := strings.TrimRight(text, "\\")
		if newline == "" || (len(text)-len(stripped))%2 == 0 {
			break
		}
		line = text[:len(text)-1] + strings.Repeat(" ", 1+len(newline)) + lines[index]
		index++
	}
	return line, index
}

var selfRepoNonNewlineRe = regexp.MustCompile(`[^\r\n]`)

// Blanks heredoc bodies returning the masked command and the heredocs with their bodies
func selfRepoMaskHeredocs(command string) (string, []*selfRepoHeredoc) {
	var output strings.Builder
	offset := 0
	var pending, finished []*selfRepoHeredoc
	lines := selfRepoSplitLines(command)
	index := 0
	for index < len(lines) {
		if len(pending) == 0 {
			line, next := selfRepoJoinContinuedLine(lines, index)
			index = next
			for _, spec := range selfRepoHeredocSpecs(line) {
				spec.opener += offset
				pending = append(pending, spec)
			}
			output.WriteString(line)
			offset += len(line)
			continue
		}
		line := lines[index]
		index++
		current := pending[0]
		candidate := strings.TrimRight(line, "\r\n")
		compare := candidate
		if current.stripTabs {
			compare = strings.TrimLeft(candidate, "\t")
		}
		if compare == current.delimiter {
			finished = append(finished, pending[0])
			pending = pending[1:]
		} else {
			current.body = append(current.body, line)
		}
		output.WriteString(selfRepoNonNewlineRe.ReplaceAllString(line, " "))
		offset += len(line)
	}
	return output.String(), append(finished, pending...)
}

// Records an inline alias config override
func selfRepoRecordAlias(config string, aliases map[string]string) {
	parts := strings.SplitN(config, "=", 2)
	if len(parts) == 2 && strings.HasPrefix(strings.ToLower(parts[0]), "alias.") {
		aliases[strings.ToLower(parts[0][6:])] = parts[1]
	}
}

// Parses git global options into the target dir, subcommand, sub args, and inline aliases
func selfRepoGitTargetAndSubcommand(args []string, currentDir string, env map[string]string) (string, string, []string, map[string]string) {
	target := currentDir
	workTree := ""
	aliases := map[string]string{}
	index := 0
	for index < len(args) {
		arg := args[index]
		if arg == "--" {
			index++
			break
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		option, value, hasValue := "", "", false
		if selfRepoGitGlobalOptionsWithArg[arg] {
			option = arg
			if index+1 < len(args) {
				value, hasValue = args[index+1], true
			}
			index += 2
		} else {
			for _, o := range []string{"-C", "--work-tree=", "-c"} {
				if strings.HasPrefix(arg, o) && len(arg) > len(o) {
					option, value, hasValue = o, arg[len(o):], true
					break
				}
			}
			index++
		}
		switch {
		case option == "-C" && hasValue:
			target = selfRepoResolve(value, target)
		case strings.HasPrefix(option, "--work-tree") && hasValue:
			workTree = value
		case option == "-c" && hasValue:
			selfRepoRecordAlias(value, aliases)
		}
	}
	explicit := workTree
	if explicit == "" {
		explicit = env["GIT_WORK_TREE"]
	}
	if explicit != "" {
		target = selfRepoResolve(explicit, target)
	}
	if index < len(args) {
		return target, strings.ToLower(args[index]), args[index+1:], aliases
	}
	return target, "", nil, aliases
}

// A long flag present or the letter inside a dash prefixed argument
func selfRepoHasFlag(args []string, long, letter string, shortOnly bool) bool {
	for _, arg := range args {
		if arg == long {
			return true
		}
		if strings.HasPrefix(arg, "-") && strings.Contains(arg[1:], letter) && !(shortOnly && strings.HasPrefix(arg, "--")) {
			return true
		}
	}
	return false
}

func selfRepoMutatesWorktree(subcommand string, args []string) bool {
	switch subcommand {
	case "reset":
		for _, arg := range args {
			if selfRepoResetWorktreeModes[arg] || selfRepoResetHardRe.MatchString(arg) {
				return true
			}
		}
		return false
	case "stash":
		action := "push"
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				action = arg
				break
			}
		}
		return !selfRepoStashSafeActions[action]
	case "clean":
		return !selfRepoHasFlag(args, "--dry-run", "n", true)
	case "restore":
		return selfRepoHasFlag(args, "--worktree", "W", false) || !selfRepoHasFlag(args, "--staged", "S", false)
	}
	return selfRepoWorktreeMutations[subcommand]
}

// Blocks worktree remove or move aimed at the running root from any directory
func selfRepoInspectGitWorktree(args []string, cwd, root string) string {
	actionIndex := selfRepoConsumeOptions(args, 0, selfRepoNoOptions)
	action := ""
	if actionIndex < len(args) {
		action = strings.ToLower(args[actionIndex])
	}
	targetIndex := selfRepoConsumeOptions(args, actionIndex+1, selfRepoNoOptions)
	if selfRepoWorktreeTargetActions[action] && targetIndex < len(args) && selfRepoResolve(args[targetIndex], cwd) == root {
		return "git worktree " + action
	}
	return ""
}

func selfRepoReadGitAlias(executable, target, alias string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-C", target, "config", "--get", "alias."+alias)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func selfRepoInspectGit(executable string, args []string, currentDir string, env map[string]string, root string, depth int) string {
	target, subcommand, subArgs, inlineAliases := selfRepoGitTargetAndSubcommand(args, currentDir, env)
	if subcommand == "" {
		return ""
	}
	if subcommand == "worktree" {
		return selfRepoInspectGitWorktree(subArgs, target, root)
	}
	if !selfRepoIsWithin(target, root) {
		return ""
	}
	if selfRepoMutatesWorktree(subcommand, subArgs) {
		return "git " + subcommand
	}
	if selfRepoKnownGitBuiltins[subcommand] || depth >= selfRepoMaxRecursion {
		return ""
	}
	alias, ok := inlineAliases[subcommand]
	if !ok {
		alias = selfRepoReadGitAlias(executable, target, subcommand)
	}
	if alias == "" {
		return ""
	}
	if strings.HasPrefix(alias, "!") {
		return selfRepoFindMutation(alias[1:], target, root, depth+1)
	}
	aliasArgs, err := shlexSplit(alias)
	if err != nil {
		return ""
	}
	return selfRepoInspectGit(executable, append(append([]string{}, aliasArgs...), subArgs...), target, map[string]string{}, root, depth+1)
}

func selfRepoInspectGithubCLI(executable string, args []string, currentDir string, env map[string]string, root string, depth int) string {
	if !selfRepoIsWithin(currentDir, root) {
		return ""
	}
	index := selfRepoConsumeOptions(args, 0, map[string]bool{"-R": true, "--repo": true, "--hostname": true})
	if index+1 < len(args) && args[index] == "pr" && args[index+1] == "checkout" {
		return selfRepoExecutableName(executable) + " pr checkout"
	}
	return ""
}

func selfRepoInspectShell(executable string, args []string, currentDir string, env map[string]string, root string, depth int) string {
	script, ok := selfRepoShellScriptArg(args)
	if !ok {
		return ""
	}
	return selfRepoFindMutation(script, currentDir, root, depth+1)
}

type selfRepoInspector func(executable string, args []string, currentDir string, env map[string]string, root string, depth int) string

func selfRepoInspectorFor(name string) selfRepoInspector {
	switch name {
	case "git":
		return selfRepoInspectGit
	case "gh", "hub":
		return selfRepoInspectGithubCLI
	}
	if selfRepoShellExecutables[name] {
		return selfRepoInspectShell
	}
	return nil
}

// Name of the first command that would rewrite root, else empty
func selfRepoFindMutation(command string, cwd, root string, depth int) string {
	if depth > selfRepoMaxRecursion {
		return ""
	}
	masked, heredocs := selfRepoMaskHeredocs(command)
	startSet := map[int]bool{}
	var starts []int
	for _, s := range iterShellCommandStarts(masked) {
		if !startSet[s] {
			startSet[s] = true
			starts = append(starts, s)
		}
	}
	sort.Ints(starts)
	keyed := append([]int{}, starts...)
	for _, h := range heredocs {
		keyed = append(keyed, h.opener)
	}
	scopes := selfRepoScopeKeys(masked, keyed)
	for _, h := range heredocs {
		if selfRepoShellConsumesHeredoc(masked, starts, scopes, h.opener) {
			if op := selfRepoFindMutation(strings.Join(h.body, ""), cwd, root, depth+1); op != "" {
				return op
			}
		}
	}
	cwdByScope := map[string]string{"": cwd}
	pendingCd := map[string]string{}
	for _, start := range starts {
		scope := scopes[start]
		key := scopeKeyOf(scope)
		if _, ok := cwdByScope[key]; !ok {
			parent := cwd
			if len(scope) > 0 {
				if v, ok := cwdByScope[scopeKeyOf(scope[:len(scope)-1])]; ok {
					parent = v
				}
			}
			cwdByScope[key] = parent
		}
		if pending, ok := pendingCd[key]; ok {
			delete(pendingCd, key)
			switch selfRepoOperatorBefore(masked, start) {
			case "&&", ";", "\n":
				cwdByScope[key] = pending
			}
		}
		env, executable, args, ok := selfRepoCommandParts(selfRepoShellWordsAt(masked, start))
		if !ok {
			continue
		}
		currentDir := cwdByScope[key]
		if cdTarget := selfRepoCdTarget(executable, args, currentDir); cdTarget != "" {
			pendingCd[key] = cdTarget
			continue
		}
		if inspect := selfRepoInspectorFor(selfRepoExecutableName(executable)); inspect != nil {
			if op := inspect(executable, args, currentDir, env, root, depth); op != "" {
				return op
			}
		}
	}
	return ""
}

// SelfRepoGuardActive is true only on Windows where NTFS locks loaded modules of the live checkout
func SelfRepoGuardActive() bool {
	return runtime.GOOS == "windows"
}

// DetectSelfRepoGitMutation reports whether a command would rewrite the live source checkout
func DetectSelfRepoGitMutation(env *Env, command, cwd, sourceRoot string) (bool, string) {
	root := sourceRoot
	if root == "" {
		root = RunningSourceRoot()
	}
	if root == "" || command == "" {
		return false, ""
	}
	root = selfRepoResolve(root, string(filepath.Separator))
	if cwd == "" {
		cwd = string(filepath.Separator)
	}
	operation := selfRepoFindMutation(command, selfRepoResolve(cwd, string(filepath.Separator)), root, 0)
	if operation == "" {
		return false, ""
	}
	return true, selfRepoBlockMessage(env, operation, root)
}

func selfRepoBlockMessage(env *Env, operation, root string) string {
	scratch := filepath.Join(home.Process(), "scratch")
	if env != nil && strings.TrimSpace(env.Home) != "" {
		scratch = filepath.Join(env.Home, "scratch")
	}
	return fmt.Sprintf("Blocked: `%s` would rewrite icebath's live source checkout (%s) and can mix module versions in this running process. "+
		"Use a separate worktree or a shared clone on real disk, e.g. `git clone --shared %s %s/<task>` — avoid /tmp for "+
		"clones that install node/python deps: /tmp is usually RAM-backed tmpfs and a few "+
		"dependency installs can fill it and ENOSPC other work. Delete the clone when the branch "+
		"is pushed. To change this checkout, stop icebath, run the command externally, then restart "+
		"icebath.", operation, root, root, scratch)
}
