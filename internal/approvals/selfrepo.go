package approvals

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// On Windows a loaded program locks its files, so git rewriting the checkout icebath runs from can leave a mix of versions behind; such
// commands are refused there

var (
	worktreeMutations = strs.Set("checkout", "switch", "rebase", "merge", "pull", "restore", "clean", "cherry-pick", "revert", "bisect")
	worktreeTargets   = strs.Set("move", "remove")
	stashSafe         = strs.Set("list", "show", "create", "store", "drop", "clear")
	resetModes        = strs.Set("--hard", "--merge", "--keep")
	gitBuiltins       = strs.Set("add", "am", "apply", "blame", "branch", "bundle", "cat-file", "clean", "clone", "commit", "config", "describe", "diff",
		"fetch", "format-patch", "grep", "help", "init", "log", "ls-files", "ls-remote", "ls-tree", "maintenance", "merge-base", "mv", "notes", "push",
		"range-diff", "reflog", "remote", "repack", "replace", "reset", "restore", "rev-list", "rev-parse", "rm", "shortlog", "show", "show-ref",
		"stash", "status", "submodule", "tag", "worktree")
	resetHard    = regexp.MustCompile(`^--h(?:a(?:r(?:d)?)?)?$`)
	heredocStart = regexp2.MustCompile(`^<<(?<dash>-?)[ \t]*(?:(?<q>['"])(?<quoted>.*?)\k<q>|(?!['"])(?<bare>[^\s;|&<>]*))`, regexp2.None)
	noOptions    = map[string]bool{}
	// Wrappers stepped over to the command they run, with their options that take a value
	repoWrappers = map[string]map[string]bool{
		"sudo": strs.Set("-C", "--chdir", "-c", "--close-from", "-g", "--group", "-h", "--host", "-p", "--prompt", "-R", "--chroot", "-T",
			"--command-timeout", "-u", "--user"),
		"env":     strs.Set("-a", "--argv0", "-C", "--chdir", "-S", "--split-string", "-u", "--unset"),
		"command": noOptions, "builtin": noOptions, "nohup": noOptions, "setsid": noOptions,
		"exec": strs.Set("-a"), "nice": strs.Set("-n", "--adjustment"), "time": strs.Set("-f", "--format", "-o", "--output"),
	}
	gitArgOptions = strs.Set("-C", "-c", "--work-tree", "--git-dir", "--namespace", "--exec-path")
	nonNewline    = regexp.MustCompile(`[^\r\n]`)
)

// Deepest the guard follows aliases and scripts into each other
const maxRepoDepth = 4

// Folder of the source checkout this program was built from, when it is still there as a git checkout
func sourceRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	root := ""
	if filepath.IsAbs(file) {
		if resolved, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(filepath.Dir(file)))); err == nil {
			root = resolved
		}
	} else {
		root = trimmedRoot(filepath.ToSlash(file))
	}
	if root == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return ""
	}
	return root
}

// Root of a build made with trimmed paths: the nearest folder above the program, or above the working folder of a go run, whose go.mod
// module holds the file
func trimmedRoot(file string) string {
	var starts []string
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		starts = append(starts, filepath.Dir(exe))
		for dir := filepath.Dir(exe); filepath.Dir(dir) != dir; dir = filepath.Dir(dir) {
			if strings.HasPrefix(filepath.Base(dir), "go-build") {
				if wd, err := os.Getwd(); err == nil {
					starts = append(starts, wd)
				}
				break
			}
		}
	}
	for _, start := range starts {
		dir, err := filepath.Abs(start)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		for {
			if module := modulePath(filepath.Join(dir, "go.mod")); module != "" && strings.HasPrefix(file, module+"/") {
				if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(file, module+"/")))); err == nil && st.Mode().IsRegular() {
					return dir
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

// Module a go.mod declares, empty when it can't be read or declares none
func modulePath(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "//")
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "module" {
			if unquoted, err := strconv.Unquote(fields[1]); err == nil {
				return unquoted
			}
			return fields[1]
		}
	}
	return ""
}

func repoResolve(p, base string) string {
	joined := fsx.ExpandHome(p)
	if !filepath.IsAbs(joined) {
		joined = filepath.Join(base, joined)
	}
	abs, err := filepath.Abs(joined)
	if err != nil {
		abs = joined
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return resolve(abs)
}

func within(p, root string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func programName(value string) string {
	return strings.ToLower(strings.TrimSuffix(filepath.Base(strings.ReplaceAll(value, `\`, "/")), ".exe"))
}

func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// Plain words of the simple command at start, up to a newline, a redirection or a comment
func wordsAt(command string, start int) []string {
	var words []string
	cursor := start
	for range 64 {
		wordStart, wordEnd, raw := readWord(command, cursor)
		if wordStart == wordEnd || len(words) > 0 && strings.Contains(command[cursor:wordStart], "\n") || commentAt(command, wordStart) ||
			digits(raw) && wordEnd < len(command) && (command[wordEnd] == '<' || command[wordEnd] == '>') {
			break
		}
		words = append(words, plainWord(raw))
		cursor = wordEnd
	}
	return words
}

// Index of the first operand from start, a -- ending the options
func skipOptions(words []string, start int, withArg map[string]bool) int {
	i := start
	for i < len(words) && strings.HasPrefix(words[i], "-") && words[i] != "-" {
		if words[i] == "--" {
			return i + 1
		}
		if !strings.Contains(words[i], "=") && withArg[words[i]] {
			i += 2
		} else {
			i++
		}
	}
	return i
}

// Leading assignments and wrappers split off: the variables, the command and its arguments
func commandParts(words []string) (map[string]string, string, []string, bool) {
	env := map[string]string{}
	for i := 0; i < len(words); {
		if name, value, ok := strings.Cut(words[i], "="); ok && envAssign.MatchString(words[i]) {
			env[name] = value
			i++
			continue
		}
		program := programName(words[i])
		options, wrapper := repoWrappers[program]
		if !wrapper {
			return env, words[i], words[i+1:], true
		}
		if program == "command" && i+1 < len(words) && (words[i+1] == "-v" || words[i+1] == "-V") {
			break
		}
		i = skipOptions(words, i+1, options)
	}
	return env, "", nil, false
}

func scopeKey(openers []int) string {
	parts := make([]string, len(openers))
	for i, o := range openers {
		parts[i] = strconv.Itoa(o)
	}
	return strings.Join(parts, ",")
}

// Openers of the substitutions and subshells around each command start
func scopes(command string, starts []int) map[int][]int {
	type level struct {
		kind   string
		opener int
		quote  byte
	}
	levels := []*level{{kind: "root", opener: -1}}
	out := map[int][]int{}
	ordered := slices.Clone(starts)
	sort.Ints(ordered)
	ordered = slices.Compact(ordered)
	cursor := 0
	for _, start := range ordered {
		for ; cursor < start; cursor++ {
			top := levels[len(levels)-1]
			quote, char := top.quote, command[cursor]
			closes := quote == 0 && len(levels) > 1
			switch {
			case quote != 0 && char == quote:
				top.quote = 0
			case quote == '\'':
			case char == '\\' && cursor+1 < start:
				cursor++
			case quote == 0 && (char == '\'' || char == '"'):
				top.quote = char
			case strings.HasPrefix(command[cursor:], "$("):
				levels = append(levels, &level{kind: "$(", opener: cursor})
				cursor++
			case quote == 0 && char == '(':
				levels = append(levels, &level{kind: "(", opener: cursor})
			case char == ')' && closes && (top.kind == "(" || top.kind == "$(") || char == '`' && closes && top.kind == "`":
				levels = levels[:len(levels)-1]
			case char == '`':
				levels = append(levels, &level{kind: "`", opener: cursor})
			}
		}
		key := make([]int, 0, len(levels)-1)
		for _, l := range levels[1:] {
			key = append(key, l.opener)
		}
		out[start] = key
	}
	return out
}

// List operator or newline just before a command start
func operatorBefore(command string, start int) string {
	head := strings.TrimRight(command[:start], " \t\r\n\f\v")
	for _, n := range []int{2, 1} {
		if len(head) >= n {
			switch tail := head[len(head)-n:]; tail {
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

// Folder a cd or pushd lands in, when it exists
func cdTarget(executable string, args []string, cwd string) string {
	if name := programName(executable); name != "cd" && name != "pushd" {
		return ""
	}
	i := skipOptions(args, 0, noOptions)
	if i >= len(args) || args[i] == "-" {
		return ""
	}
	target := repoResolve(args[i], cwd)
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		return target
	}
	return ""
}

// Script a shell -c runs, when there is one
func shellScript(args []string) (string, bool) {
	if found, payload, has := dashC(args); found {
		return payload, has && payload != ""
	}
	for i, arg := range args {
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			return "", false
		}
		if strings.Contains(arg[1:], "c") {
			if i+1 < len(args) {
				return args[i+1], args[i+1] != ""
			}
			return "", false
		}
	}
	return "", false
}

// Whether the words are a bare shell, which runs whatever it reads on stdin
func readsStdin(words []string) bool {
	_, executable, args, ok := commandParts(words)
	if !ok || !shellExecutables[programName(executable)] {
		return false
	}
	if _, has := shellScript(args); has {
		return false
	}
	return !slices.ContainsFunc(args, func(arg string) bool { return arg != "" && !strings.HasPrefix(arg, "-") })
}

// Whether the brace at index stands alone as a group delimiter rather than an expansion
func groupBrace(masked string, index int) bool {
	before, after := byte(' '), byte(' ')
	if index > 0 {
		before = masked[index-1]
	}
	if index+1 < len(masked) {
		after = masked[index+1]
	}
	return (spaceByte(before) || strings.IndexByte("(;&|", before) >= 0) && (spaceByte(after) || strings.IndexByte(";)|&", after) >= 0)
}

// Where the pipeline holding a heredoc ends
func pipelineEnd(masked string, opener int) int {
	depth, level, levelSet, afterPipe, backtick := 0, -1, false, false, false
	for sc := scan(masked, 0, -1, scanOptions{comments: true}); ; {
		st, ok := sc.next()
		if !ok {
			return len(masked)
		}
		if !levelSet && st.i >= opener {
			level, levelSet = depth, true
		}
		if st.kind == stepComment {
			continue
		}
		if st.kind != stepChar || st.quote != 0 {
			afterPipe = false
			continue
		}
		char := masked[st.i]
		if spaceByte(char) {
			if char == '\n' && levelSet && level == 0 && depth == 0 && !afterPipe {
				return st.i
			}
			continue
		}
		if char == ';' || char == '&' || char == '|' {
			if char == '&' && (st.i > 0 && (masked[st.i-1] == '<' || masked[st.i-1] == '>') || strings.HasPrefix(masked[st.i:], "&>")) {
				continue
			}
			if char == '|' && !strings.HasPrefix(masked[st.i:], "||") {
				afterPipe = true
			} else if levelSet && level == 0 && depth == 0 && (st.i == 0 || masked[st.i-1] != '|') {
				return st.i
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
		case char == '(' || char == '{' && groupBrace(masked, st.i):
			depth++
		case char == ')' || char == '}' && groupBrace(masked, st.i):
			depth--
			if levelSet && depth < level {
				if rest := strings.TrimLeft(masked[st.j:], " \t"); !strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, "||") {
					return st.i
				}
				level = depth
			}
		}
	}
}

// Whether a bare shell reads the heredoc's body, as its owner or further down its pipeline
func heredocRuns(masked string, starts []int, scoped map[int][]int, opener int) bool {
	scope := scoped[opener]
	owner := -1
	for _, start := range starts {
		if start <= opener && slices.Equal(scoped[start], scope) && start > owner {
			owner = start
		}
	}
	end := pipelineEnd(masked, opener)
	var candidates []int
	if owner >= 0 {
		candidates = append(candidates, owner)
	}
	for _, start := range starts {
		if opener < start && start < end {
			candidates = append(candidates, start)
		}
	}
	return slices.ContainsFunc(candidates, func(start int) bool { return readsStdin(wordsAt(masked, start)) })
}

type heredoc struct {
	delimiter string
	tabs      bool
	opener    int
	body      []string
}

// Heredocs a line opens, with where each opens
func heredocsOn(line string) []*heredoc {
	var out []*heredoc
	var quote byte
	for i := 0; i < len(line); {
		char := line[i]
		if quote != 0 {
			if char == '\\' && quote == '"' && i+1 < len(line) {
				i++
			} else if char == quote {
				quote = 0
			}
		} else if char == '\'' || char == '"' {
			quote = char
		}
		if quote != 0 || !strings.HasPrefix(line[i:], "<<") || strings.HasPrefix(line[i:], "<<<") {
			i++
			continue
		}
		m, _ := heredocStart.FindStringMatch(line[i:])
		if m == nil {
			break
		}
		start := i
		i += len(m.String())
		delimiter := m.GroupByName("bare").String()
		if g := m.GroupByName("q"); g != nil && len(g.Captures) > 0 {
			delimiter = m.GroupByName("quoted").String()
		}
		if delimiter != "" {
			dash := m.GroupByName("dash")
			out = append(out, &heredoc{delimiter: delimiter, tabs: dash != nil && dash.String() == "-", opener: start})
		}
	}
	return out
}

// Physical lines, each with its line ending
func physicalLines(command string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case '\n':
			lines = append(lines, command[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(command) && command[i+1] == '\n' {
				i++
			}
			lines = append(lines, command[start:i+1])
			start = i + 1
		}
	}
	if start < len(command) {
		lines = append(lines, command[start:])
	}
	return lines
}

// Line with every line a trailing backslash joins onto it, the joins blanked so offsets hold
func joinContinued(lines []string, index int) (string, int) {
	line := lines[index]
	for index++; index < len(lines); index++ {
		text := strings.TrimRight(line, "\r\n")
		newline := line[len(text):]
		if newline == "" || (len(text)-len(strings.TrimRight(text, `\`)))%2 == 0 {
			break
		}
		line = text[:len(text)-1] + strings.Repeat(" ", 1+len(newline)) + lines[index]
	}
	return line, index
}

// Command with its heredoc bodies blanked, and the heredocs with their bodies
func maskHeredocs(command string) (string, []*heredoc) {
	var out strings.Builder
	offset := 0
	var pending, finished []*heredoc
	lines := physicalLines(command)
	for index := 0; index < len(lines); {
		if len(pending) == 0 {
			line, next := joinContinued(lines, index)
			index = next
			for _, h := range heredocsOn(line) {
				h.opener += offset
				pending = append(pending, h)
			}
			out.WriteString(line)
			offset += len(line)
			continue
		}
		line := lines[index]
		index++
		current := pending[0]
		compare := strings.TrimRight(line, "\r\n")
		if current.tabs {
			compare = strings.TrimLeft(compare, "\t")
		}
		if compare == current.delimiter {
			finished = append(finished, current)
			pending = pending[1:]
		} else {
			current.body = append(current.body, line)
		}
		out.WriteString(nonNewline.ReplaceAllString(line, " "))
		offset += len(line)
	}
	return out.String(), append(finished, pending...)
}

// Folder git works on, its subcommand, the subcommand's arguments and the aliases set inline with -c
func gitTarget(args []string, dir string, env map[string]string) (string, string, []string, map[string]string) {
	target, workTree := dir, ""
	aliases := map[string]string{}
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		option, value, has := "", "", false
		if gitArgOptions[arg] {
			option = arg
			if i+1 < len(args) {
				value, has = args[i+1], true
			}
			i += 2
		} else {
			for _, o := range []string{"-C", "--work-tree=", "-c"} {
				if strings.HasPrefix(arg, o) && len(arg) > len(o) {
					option, value, has = o, arg[len(o):], true
					break
				}
			}
			i++
		}
		switch {
		case option == "-C" && has:
			target = repoResolve(value, target)
		case strings.HasPrefix(option, "--work-tree") && has:
			workTree = value
		case option == "-c" && has:
			if key, alias, ok := strings.Cut(value, "="); ok && strings.HasPrefix(strings.ToLower(key), "alias.") {
				aliases[strings.ToLower(key[6:])] = alias
			}
		}
	}
	if explicit := cmp.Or(workTree, env["GIT_WORK_TREE"]); explicit != "" {
		target = repoResolve(explicit, target)
	}
	if i < len(args) {
		return target, strings.ToLower(args[i]), args[i+1:], aliases
	}
	return target, "", nil, aliases
}

// Whether a long flag is there, or the letter inside an argument starting with a dash
func hasFlag(args []string, long, letter string, shortOnly bool) bool {
	return slices.ContainsFunc(args, func(arg string) bool {
		return arg == long || strings.HasPrefix(arg, "-") && strings.Contains(arg[1:], letter) && !(shortOnly && strings.HasPrefix(arg, "--"))
	})
}

// Whether the git subcommand with the arguments rewrites the work tree
func rewritesTree(subcommand string, args []string) bool {
	switch subcommand {
	case "reset":
		return slices.ContainsFunc(args, func(arg string) bool { return resetModes[arg] || resetHard.MatchString(arg) })
	case "stash":
		action := "push"
		if i := slices.IndexFunc(args, func(arg string) bool { return !strings.HasPrefix(arg, "-") }); i >= 0 {
			action = args[i]
		}
		return !stashSafe[action]
	case "clean":
		return !hasFlag(args, "--dry-run", "n", true)
	case "restore":
		return hasFlag(args, "--worktree", "W", false) || !hasFlag(args, "--staged", "S", false)
	}
	return worktreeMutations[subcommand]
}

// git worktree remove or move aimed at the root, from any folder
func gitWorktree(args []string, cwd, root string) string {
	at := skipOptions(args, 0, noOptions)
	action := ""
	if at < len(args) {
		action = strings.ToLower(args[at])
	}
	if target := skipOptions(args, at+1, noOptions); worktreeTargets[action] && target < len(args) && repoResolve(args[target], cwd) == root {
		return "git worktree " + action
	}
	return ""
}

// Alias git knows for the subcommand in the folder, empty when none
func gitAlias(executable, target, alias string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, executable, "-C", target, "config", "--get", "alias."+alias).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func inspectGit(executable string, args []string, dir string, env map[string]string, root string, depth int) string {
	target, subcommand, rest, aliases := gitTarget(args, dir, env)
	switch {
	case subcommand == "":
		return ""
	case subcommand == "worktree":
		return gitWorktree(rest, target, root)
	case !within(target, root):
		return ""
	case rewritesTree(subcommand, rest):
		return "git " + subcommand
	case gitBuiltins[subcommand] || depth >= maxRepoDepth:
		return ""
	}
	alias, ok := aliases[subcommand]
	if !ok {
		alias = gitAlias(executable, target, subcommand)
	}
	if alias == "" {
		return ""
	}
	if script, shell := strings.CutPrefix(alias, "!"); shell {
		return findRewrite(script, target, root, depth+1)
	}
	words, err := splitPunct(alias, "", false)
	if err != nil {
		return ""
	}
	return inspectGit(executable, append(words, rest...), target, map[string]string{}, root, depth+1)
}

func inspectGitHub(executable string, args []string, dir string, _ map[string]string, root string, _ int) string {
	if !within(dir, root) {
		return ""
	}
	if i := skipOptions(args, 0, strs.Set("-R", "--repo", "--hostname")); i+1 < len(args) && args[i] == "pr" && args[i+1] == "checkout" {
		return programName(executable) + " pr checkout"
	}
	return ""
}

func inspectShell(_ string, args []string, dir string, _ map[string]string, root string, depth int) string {
	if script, ok := shellScript(args); ok {
		return findRewrite(script, dir, root, depth+1)
	}
	return ""
}

type inspector func(executable string, args []string, dir string, env map[string]string, root string, depth int) string

func inspectorOf(name string) inspector {
	switch {
	case name == "git":
		return inspectGit
	case name == "gh" || name == "hub":
		return inspectGitHub
	case shellExecutables[name]:
		return inspectShell
	}
	return nil
}

// First command of the text that would rewrite the root, empty when none
func findRewrite(command, cwd, root string, depth int) string {
	if depth > maxRepoDepth {
		return ""
	}
	masked, heredocs := maskHeredocs(command)
	starts := commandStarts(masked)
	sort.Ints(starts)
	starts = slices.Compact(starts)
	keyed := slices.Clone(starts)
	for _, h := range heredocs {
		keyed = append(keyed, h.opener)
	}
	scoped := scopes(masked, keyed)
	for _, h := range heredocs {
		if heredocRuns(masked, starts, scoped, h.opener) {
			if op := findRewrite(strings.Join(h.body, ""), cwd, root, depth+1); op != "" {
				return op
			}
		}
	}
	dirs := map[string]string{"": cwd}
	pendingCd := map[string]string{}
	for _, start := range starts {
		scope := scoped[start]
		key := scopeKey(scope)
		if _, ok := dirs[key]; !ok {
			parent := cwd
			if len(scope) > 0 {
				if dir, ok := dirs[scopeKey(scope[:len(scope)-1])]; ok {
					parent = dir
				}
			}
			dirs[key] = parent
		}
		if pending, ok := pendingCd[key]; ok {
			delete(pendingCd, key)
			switch operatorBefore(masked, start) {
			case "&&", ";", "\n":
				dirs[key] = pending
			}
		}
		env, executable, args, ok := commandParts(wordsAt(masked, start))
		if !ok {
			continue
		}
		dir := dirs[key]
		if target := cdTarget(executable, args, dir); target != "" {
			pendingCd[key] = target
			continue
		}
		if inspect := inspectorOf(programName(executable)); inspect != nil {
			if op := inspect(executable, args, dir, env, root, depth); op != "" {
				return op
			}
		}
	}
	return ""
}

// Refusal of a command that would rewrite the checkout icebath runs from, with git in the folder given; empty when it may run. Only on
// Windows, where the files of a running program are locked
func SelfRepo(cfg *config.Config, command, cwd string) string {
	if runtime.GOOS != "windows" || command == "" {
		return ""
	}
	root := sourceRoot()
	if root == "" {
		return ""
	}
	root = repoResolve(root, string(filepath.Separator))
	if cwd == "" {
		cwd = string(filepath.Separator)
	}
	op := findRewrite(command, repoResolve(cwd, string(filepath.Separator)), root, 0)
	if op == "" {
		return ""
	}
	scratch := filepath.Join(cfg.ProfileDir(cfg.Profile()), "scratch")
	return fmt.Sprintf("Blocked: `%s` would rewrite icebath's live source checkout (%s) and can mix module versions in this running process. "+
		"Use a separate worktree or a shared clone on real disk, e.g. `git clone --shared %s %s/<task>` — avoid /tmp for clones that install "+
		"node/python deps: /tmp is usually RAM-backed tmpfs and a few dependency installs can fill it and ENOSPC other work. Delete the clone when "+
		"the branch is pushed. To change this checkout, stop icebath, run the command externally, then restart icebath.", op, root, root, scratch)
}
