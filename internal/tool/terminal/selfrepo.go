package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool/approval"
)

const selfRepoMaxRecursion = 4

var (
	worktreeMutations    = set("checkout", "switch", "rebase", "merge", "pull", "restore", "clean", "cherry-pick", "revert", "bisect")
	worktreeTargetAction = set("move", "remove")
	stashSafeActions     = set("list", "show", "create", "store", "drop", "clear")
	resetWorktreeModes   = set("--hard", "--merge", "--keep")
	knownGitBuiltins     = set("add", "am", "apply", "blame", "branch", "bundle", "cat-file", "clean", "clone", "commit",
		"config", "describe", "diff", "fetch", "format-patch", "grep", "help", "init", "log",
		"ls-files", "ls-remote", "ls-tree", "maintenance", "merge-base", "mv", "notes", "push",
		"range-diff", "reflog", "remote", "repack", "replace", "reset", "restore", "rev-list",
		"rev-parse", "rm", "shortlog", "show", "show-ref", "stash", "status", "submodule", "tag",
		"worktree")
	repoShellExecutables  = set("bash", "dash", "ksh", "sh", "zsh")
	assignmentRe          = regexp.MustCompile(`(?s)^[A-Za-z_][A-Za-z0-9_]*=(.*)$`)
	resetHardRe           = regexp.MustCompile(`^--h(?:a(?:r(?:d)?)?)?$`)
	wrapperOptionsWithArg = map[string]map[string]bool{
		"sudo": set("-C", "--chdir", "-c", "--close-from", "-g", "--group", "-h", "--host",
			"-p", "--prompt", "-R", "--chroot", "-T", "--command-timeout", "-u", "--user"),
		"env":     set("-a", "--argv0", "-C", "--chdir", "-S", "--split-string", "-u", "--unset"),
		"command": {}, "builtin": {}, "nohup": {}, "setsid": {},
		"exec": set("-a"), "nice": set("-n", "--adjustment"),
		"time": set("-f", "--format", "-o", "--output"),
	}
	gitGlobalOptionsWithArg = set("-C", "-c", "--work-tree", "--git-dir", "--namespace", "--exec-path")
)

func WrapperOptions(executable string) ([]string, bool) {
	opts, ok := wrapperOptionsWithArg[executable]
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(opts))
	for o := range opts {
		out = append(out, o)
	}
	sort.Strings(out)
	return out, true
}

type heredoc struct {
	delimiter string
	stripTabs bool
	opener    int
	body      []string
}

func runningSourceRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	for _, candidate := range []string{dir, filepath.Dir(dir)} {
		if _, err := os.Lstat(filepath.Join(candidate, ".git")); err == nil {
			return candidate
		}
	}
	return ""
}

func repoResolve(p, base string) string {
	expanded := home.ExpandUser(p)
	var joined string
	switch {
	case filepath.IsAbs(expanded):
		joined = expanded
	case runtime.GOOS == "windows" && (strings.HasPrefix(expanded, `\`) || strings.HasPrefix(expanded, "/")):
		joined = filepath.VolumeName(base) + expanded
	default:
		joined = filepath.Join(base, expanded)
	}
	return resolveLenient(joined)
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func isWithin(p, root string) bool {
	if samePath(p, root) {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(p)) {
			return false
		}
		rel, err = filepath.Rel(strings.ToLower(root), strings.ToLower(p))
		if err != nil {
			return false
		}
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func repoExecutableName(value string) string {
	name := pathBase(strings.ReplaceAll(value, `\`, "/"))
	return strings.ToLower(strings.TrimSuffix(name, ".exe"))
}

func shellWordsAt(command string, start int) []string {
	var words []string
	cursor := start
	for range 64 {
		wordStart, wordEnd, raw := approval.ReadShellWord(command, cursor)
		if wordStart == wordEnd || (len(words) > 0 && strings.Contains(command[cursor:wordStart], "\n")) ||
			approval.CommentStart(command, wordStart) ||
			(isDigits(raw) && wordEnd < len(command) && (command[wordEnd] == '<' || command[wordEnd] == '>')) {
			break
		}
		words = append(words, approval.DeobfuscateWord(raw))
		cursor = wordEnd
	}
	return words
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func consumeOptions(words []string, start int, withArg map[string]bool) int {
	index := start
	for index < len(words) && strings.HasPrefix(words[index], "-") && words[index] != "-" {
		if words[index] == "--" {
			return index + 1
		}
		if !strings.Contains(words[index], "=") && withArg[words[index]] {
			index += 2
		} else {
			index++
		}
	}
	return index
}

func commandParts(words []string) (map[string]string, string, []string, bool) {
	env := map[string]string{}
	index := 0
	for index < len(words) {
		if assignmentRe.MatchString(words[index]) {
			name, value, _ := strings.Cut(words[index], "=")
			env[name] = value
			index++
			continue
		}
		executable := repoExecutableName(words[index])
		opts, wrapper := wrapperOptionsWithArg[executable]
		if !wrapper {
			return env, words[index], words[index+1:], true
		}
		if executable == "command" && index+1 < len(words) && (words[index+1] == "-v" || words[index+1] == "-V") {
			break
		}
		index = consumeOptions(words, index+1, opts)
	}
	return env, "", nil, false
}

type scopeContext struct {
	kind   string
	opener int
	quote  byte
}

func scopeKeys(command string, starts []int) map[int]string {
	contexts := []*scopeContext{{kind: "root", opener: -1}}
	scopes := map[int]string{}
	uniq := slices.Clone(starts)
	sort.Ints(uniq)
	uniq = slices.Compact(uniq)
	cursor := 0
	for _, start := range uniq {
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
				contexts = append(contexts, &scopeContext{kind: "$(", opener: cursor})
				cursor++
			case quote == 0 && char == '(':
				contexts = append(contexts, &scopeContext{kind: "(", opener: cursor})
			case (char == ')' && closes && (ctx.kind == "(" || ctx.kind == "$(")) || (char == '`' && closes && ctx.kind == "`"):
				contexts = contexts[:len(contexts)-1]
			case char == '`':
				contexts = append(contexts, &scopeContext{kind: "`", opener: cursor})
			}
			cursor++
		}
		var key strings.Builder
		for _, item := range contexts[1:] {
			fmt.Fprintf(&key, "%d,", item.opener)
		}
		scopes[start] = key.String()
	}
	return scopes
}

func parentScope(scope string) string {
	if scope == "" {
		return ""
	}
	trimmed := strings.TrimSuffix(scope, ",")
	if i := strings.LastIndex(trimmed, ","); i >= 0 {
		return trimmed[:i+1]
	}
	return ""
}

func operatorBefore(command string, start int) string {
	head := strings.TrimRightFunc(command[:start], unicode.IsSpace)
	tails := []string{}
	if len(head) >= 2 {
		tails = append(tails, head[len(head)-2:])
	}
	if len(head) >= 1 {
		tails = append(tails, head[len(head)-1:])
	}
	for _, tail := range tails {
		switch tail {
		case "&&", "||", ";", "|", "&", "(", "{":
			return tail
		}
	}
	if strings.Contains(command[len(head):start], "\n") {
		return "\n"
	}
	return ""
}

func cdTarget(executable string, args []string, cwd string) (string, bool) {
	if name := repoExecutableName(executable); name != "cd" && name != "pushd" {
		return "", false
	}
	index := consumeOptions(args, 0, nil)
	if index >= len(args) || args[index] == "-" {
		return "", false
	}
	target := repoResolve(args[index], cwd)
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		return target, true
	}
	return "", false
}

func shellScriptArg(args []string) (string, bool) {
	if found, payload, has := approval.BashExecPayload(args); found {
		return payload, has
	}
	for index, arg := range args {
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			return "", false
		}
		if strings.Contains(arg[1:], "c") {
			if index+1 < len(args) {
				return args[index+1], true
			}
			return "", false
		}
	}
	return "", false
}

func executesHeredocBody(words []string) bool {
	_, executable, args, ok := commandParts(words)
	if !ok || executable == "" || !repoShellExecutables[repoExecutableName(executable)] {
		return false
	}
	if _, has := shellScriptArg(args); has {
		return false
	}
	for _, arg := range args {
		if arg != "" && !strings.HasPrefix(arg, "-") {
			return false
		}
	}
	return true
}

func byteAt(s string, i int) byte {
	if i < 0 {
		i += len(s)
	}
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func groupToken(masked string, index int) bool {
	before, after := byte(' '), byte(' ')
	if index > 0 {
		before = masked[index-1]
	}
	if index+1 < len(masked) {
		after = masked[index+1]
	}
	return (isASCIISpace(before) || strings.IndexByte("(;&|", before) >= 0) && (isASCIISpace(after) || strings.IndexByte(";)|&", after) >= 0)
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

func pipelineEnd(masked string, opener int) int {
	depth, level := 0, 0
	levelSet, afterPipe, backtick := false, false, false
	for st := range approval.ScanShell(masked, 0, len(masked), approval.ScanMode{Comments: true}) {
		if !levelSet && st.Start >= opener {
			level, levelSet = depth, true
		}
		if st.Kind == approval.StepComment {
			continue
		}
		if st.Kind != approval.StepChar || st.Quote != 0 {
			afterPipe = false
			continue
		}
		index := st.Start
		char := masked[index]
		if r := []rune(masked[index:st.End]); len(r) == 1 && unicode.IsSpace(r[0]) {
			if r[0] == '\n' && levelSet && level == 0 && depth == 0 && !afterPipe {
				return index
			}
			continue
		}
		if char == ';' || char == '&' || char == '|' {
			if char == '&' && (strings.IndexByte("<>", byteAt(masked, index-1)) >= 0 && byteAt(masked, index-1) != 0 || strings.HasPrefix(masked[index:], "&>")) {
				continue
			}
			if char == '|' && !strings.HasPrefix(masked[index:], "||") {
				afterPipe = true
			} else if levelSet && level == 0 && depth == 0 && byteAt(masked, index-1) != '|' {
				return index
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
		case char == '(' || (char == '{' && groupToken(masked, index)):
			depth++
		case char == ')' || (char == '}' && groupToken(masked, index)):
			depth--
			if levelSet && depth < level {
				rest := strings.TrimLeft(masked[st.End:], " \t")
				if !strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, "||") {
					return index
				}
				level = depth
			}
		}
	}
	return len(masked)
}

func shellConsumesHeredoc(masked string, starts []int, scopes map[int]string, opener int) bool {
	scope := scopes[opener]
	owner := -1
	for _, start := range starts {
		if start <= opener && scopes[start] == scope && start > owner {
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
	for _, start := range candidates {
		if executesHeredocBody(shellWordsAt(masked, start)) {
			return true
		}
	}
	return false
}

func heredocSpecs(line string) []*heredoc {
	var specs []*heredoc
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
		start := index
		cursor := index + 2
		dash := cursor < len(line) && line[cursor] == '-'
		if dash {
			cursor++
		}
		for cursor < len(line) && (line[cursor] == ' ' || line[cursor] == '\t') {
			cursor++
		}
		delimiter := ""
		if cursor < len(line) && (line[cursor] == '\'' || line[cursor] == '"') {
			q := line[cursor]
			closeAt := -1
			for k := cursor + 1; k < len(line); k++ {
				if line[k] == '\n' {
					break
				}
				if line[k] == q {
					closeAt = k
					break
				}
			}
			if closeAt < 0 {
				break
			}
			delimiter = line[cursor+1 : closeAt]
			index = closeAt + 1
		} else {
			end := cursor
			for end < len(line) {
				r, w := utf8.DecodeRuneInString(line[end:])
				if unicode.IsSpace(r) || strings.ContainsRune(";|&<>", r) {
					break
				}
				end += w
			}
			delimiter = line[cursor:end]
			index = end
		}
		if delimiter != "" {
			specs = append(specs, &heredoc{delimiter: delimiter, stripTabs: dash, opener: start})
		}
	}
	return specs
}

func splitLinesKeepEnds(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			lines = append(lines, text[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			lines = append(lines, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func joinContinuedLine(lines []string, index int) (string, int) {
	line := lines[index]
	index++
	for index < len(lines) {
		text := strings.TrimRight(line, "\r\n")
		newline := line[len(text):]
		if newline == "" || (len(text)-len(strings.TrimRight(text, `\`)))%2 == 0 {
			break
		}
		line = text[:len(text)-1] + strings.Repeat(" ", 1+len(newline)) + lines[index]
		index++
	}
	return line, index
}

func maskHeredocs(command string) (string, []*heredoc) {
	var output strings.Builder
	offset := 0
	var pending, finished []*heredoc
	lines := splitLinesKeepEnds(command)
	index := 0
	for index < len(lines) {
		if len(pending) == 0 {
			var line string
			line, index = joinContinuedLine(lines, index)
			for _, spec := range heredocSpecs(line) {
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
		if current.stripTabs {
			candidate = strings.TrimLeft(candidate, "\t")
		}
		if candidate == current.delimiter {
			finished = append(finished, current)
			pending = pending[1:]
		} else {
			current.body = append(current.body, line)
		}
		for i := 0; i < len(line); i++ {
			if line[i] == '\r' || line[i] == '\n' {
				output.WriteByte(line[i])
			} else {
				output.WriteByte(' ')
			}
		}
		offset += len(line)
	}
	return output.String(), append(finished, pending...)
}

func recordAlias(config string, aliases map[string]string) {
	key, value, found := strings.Cut(config, "=")
	if found && strings.HasPrefix(strings.ToLower(key), "alias.") {
		aliases[strings.ToLower(key[6:])] = value
	}
}

func gitTargetAndSubcommand(args []string, currentDir string, env map[string]string) (string, string, bool, []string, map[string]string) {
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
		if gitGlobalOptionsWithArg[arg] {
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
			target = repoResolve(value, target)
		case option != "" && strings.HasPrefix(option, "--work-tree") && hasValue:
			workTree = value
		case option == "-c" && hasValue:
			recordAlias(value, aliases)
		}
	}
	explicit := workTree
	if explicit == "" {
		explicit = env["GIT_WORK_TREE"]
	}
	if explicit != "" {
		target = repoResolve(explicit, target)
	}
	if index < len(args) {
		return target, strings.ToLower(args[index]), true, args[index+1:], aliases
	}
	return target, "", false, nil, aliases
}

func hasFlag(args []string, long string, letter byte, shortOnly bool) bool {
	for _, arg := range args {
		if arg == long {
			return true
		}
		if strings.HasPrefix(arg, "-") && strings.IndexByte(arg[1:], letter) >= 0 && !(shortOnly && strings.HasPrefix(arg, "--")) {
			return true
		}
	}
	return false
}

func mutatesWorktree(subcommand string, args []string) bool {
	switch subcommand {
	case "reset":
		for _, arg := range args {
			if resetWorktreeModes[arg] || resetHardRe.MatchString(arg) {
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
		return !stashSafeActions[action]
	case "clean":
		return !hasFlag(args, "--dry-run", 'n', true)
	case "restore":
		return hasFlag(args, "--worktree", 'W', false) || !hasFlag(args, "--staged", 'S', false)
	}
	return worktreeMutations[subcommand]
}

func inspectGitWorktree(args []string, cwd, root string) string {
	actionIndex := consumeOptions(args, 0, nil)
	action := ""
	if actionIndex < len(args) {
		action = strings.ToLower(args[actionIndex])
	}
	targetIndex := consumeOptions(args, actionIndex+1, nil)
	if worktreeTargetAction[action] && targetIndex < len(args) && samePath(repoResolve(args[targetIndex], cwd), root) {
		return "git worktree " + action
	}
	return ""
}

func readGitAlias(ctx context.Context, executable, target, alias string) string {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, executable, "-C", target, "config", "--get", "alias."+alias)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(out), replacementChar))
}

type selfRepoScan struct {
	ctx  context.Context
	root string
}

func (s *selfRepoScan) inspectGit(executable string, args []string, currentDir string, env map[string]string, depth int) string {
	target, subcommand, ok, subArgs, inlineAliases := gitTargetAndSubcommand(args, currentDir, env)
	if !ok {
		return ""
	}
	if subcommand == "worktree" {
		return inspectGitWorktree(subArgs, target, s.root)
	}
	if !isWithin(target, s.root) {
		return ""
	}
	if mutatesWorktree(subcommand, subArgs) {
		return "git " + subcommand
	}
	if knownGitBuiltins[subcommand] || depth >= selfRepoMaxRecursion {
		return ""
	}
	alias, inline := inlineAliases[subcommand]
	if !inline {
		alias = readGitAlias(s.ctx, executable, target, subcommand)
	}
	if alias == "" {
		return ""
	}
	if strings.HasPrefix(alias, "!") {
		return s.find(alias[1:], target, depth+1)
	}
	aliasArgs, err := shell.Split(alias)
	if err != nil {
		return ""
	}
	return s.inspectGit(executable, append(aliasArgs, subArgs...), target, map[string]string{}, depth+1)
}

func (s *selfRepoScan) inspectGitHubCLI(executable string, args []string, currentDir string) string {
	if !isWithin(currentDir, s.root) {
		return ""
	}
	index := consumeOptions(args, 0, set("-R", "--repo", "--hostname"))
	if index+1 < len(args) && args[index] == "pr" && args[index+1] == "checkout" {
		return repoExecutableName(executable) + " pr checkout"
	}
	return ""
}

func (s *selfRepoScan) inspect(executable string, args []string, currentDir string, env map[string]string, depth int) string {
	name := repoExecutableName(executable)
	switch {
	case name == "git":
		return s.inspectGit(executable, args, currentDir, env, depth)
	case name == "gh" || name == "hub":
		return s.inspectGitHubCLI(executable, args, currentDir)
	case repoShellExecutables[name]:
		if script, ok := shellScriptArg(args); ok && script != "" {
			return s.find(script, currentDir, depth+1)
		}
	}
	return ""
}

func (s *selfRepoScan) find(command, cwd string, depth int) string {
	if depth > selfRepoMaxRecursion {
		return ""
	}
	masked, heredocs := maskHeredocs(command)
	starts := approval.CommandStarts(masked)
	sort.Ints(starts)
	starts = slices.Compact(starts)
	all := slices.Clone(starts)
	for _, h := range heredocs {
		all = append(all, h.opener)
	}
	scopes := scopeKeys(masked, all)
	for _, h := range heredocs {
		if shellConsumesHeredoc(masked, starts, scopes, h.opener) {
			if op := s.find(strings.Join(h.body, ""), cwd, depth+1); op != "" {
				return op
			}
		}
	}
	cwdByScope := map[string]string{"": cwd}
	pendingCd := map[string]string{}
	for _, start := range starts {
		scope := scopes[start]
		if _, ok := cwdByScope[scope]; !ok {
			parent, ok := cwdByScope[parentScope(scope)]
			if !ok {
				parent = cwd
			}
			cwdByScope[scope] = parent
		}
		if pending, ok := pendingCd[scope]; ok {
			delete(pendingCd, scope)
			switch operatorBefore(masked, start) {
			case "&&", ";", "\n":
				cwdByScope[scope] = pending
			}
		}
		env, executable, args, ok := commandParts(shellWordsAt(masked, start))
		if !ok {
			continue
		}
		currentDir := cwdByScope[scope]
		if target, ok := cdTarget(executable, args, currentDir); ok {
			pendingCd[scope] = target
		} else if op := s.inspect(executable, args, currentDir, env, depth); op != "" {
			return op
		}
	}
	return ""
}

func (t *Terminal) detectSelfRepoMutation(ctx context.Context, command, cwd string) (string, bool, error) {
	root := runningSourceRoot()
	if root == "" || command == "" {
		return "", false, nil
	}
	root = repoResolve(root, string(filepath.Separator))
	start := cwd
	if start == "" {
		start = string(filepath.Separator)
	}
	scan := &selfRepoScan{ctx: ctx, root: root}
	op := scan.find(command, repoResolve(start, string(filepath.Separator)), 0)
	if op == "" {
		return "", false, nil
	}
	h, err := t.o.Homes.Home(ctx)
	if err != nil {
		return "", false, err
	}
	scratch := home.Scratch.In(h)
	return fmt.Sprintf("Blocked: `%s` would rewrite icebath's live source checkout "+
		"(%s) and can mix module versions in this running process. "+
		"Use a separate worktree or a shared clone on real disk, e.g. "+
		"`git clone --shared %s %s/<task>` — avoid /tmp for "+
		"clones that install node/python deps: /tmp is usually RAM-backed tmpfs and a few "+
		"dependency installs can fill it and ENOSPC other work. Delete the clone when the branch "+
		"is pushed. To change this checkout, stop icebath, run the command externally, then restart "+
		"icebath.", op, root, root, scratch), true, nil
}
