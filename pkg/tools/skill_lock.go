package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SkillLockSource is what the skills package installs so the guards can see the user's skill locks
type SkillLockSource interface {
	LockedSkillNames(env *Env) []string
	SkillsDir(env *Env) string
	AllSkillsDirs(env *Env) []string
	IsExcludedSkillPath(path, root string) bool
	FindSkillDirName(env *Env, name string) string
}

// SkillLocks is installed by pkg/skills before any terminal or file guard runs
var SkillLocks SkillLockSource

func skillLocks() SkillLockSource {
	if SkillLocks == nil {
		panic("tools: SkillLocks is nil, pkg/skills must install the skill lock source")
	}
	return SkillLocks
}

var (
	skillWritesEveryOperand = setOf("rm", "rmdir", "unlink", "truncate", "shred", "touch", "chmod", "chown", "chgrp", "chattr",
		"setfacl", "mkdir", "mv", "patch", "dos2unix", "unix2dos", "xattr", "ed", "ex", "vi", "vim", "nvim", "nano", "emacs")
	skillWritesLastOperand = setOf("cp", "install", "ln", "rsync", "scp")
	skillRecursiveVerbs    = setOf("rm", "mv", "chmod", "chown", "chgrp", "chattr", "setfacl", "rsync", "find")
	skillInplaceEditors    = setOf("sed", "perl", "ruby")
	skillGitMutations      = setOf("add", "am", "apply", "checkout", "clean", "commit", "merge", "mv", "pull", "rebase", "reset",
		"restore", "revert", "rm", "stash", "switch", "cherry-pick")
	skillInterpretersRe = regexp.MustCompile(`^(?:python[0-9.]*|node|nodejs|deno|bun|ruby|perl|php|bash|sh|zsh|dash|ksh|pwsh|powershell)$`)
	skillWrappers       = setOf("sudo", "env", "command", "builtin", "exec", "nohup", "setsid", "time", "nice", "xargs")
	skillWriteishCodeRe = regexp.MustCompile(`(?i)open\s*\([^)]*['"][rbt]*[wax+][^'"]*['"]|\.write(?:_text|_bytes|lines)?\s*\(|` +
		`\b(?:unlink|rmtree|removedirs|remove|rmdir|rename|truncate|chmod|chown|mkdir|makedirs|symlink|` +
		`copyfile|copytree|copy2?|move|touch)\s*\(|\bos\.replace\s*\(|\.replace\s*\(\s*(?:Path|['"]/)|` +
		`(?:write|append)File(?:Sync)?|(?:rm|unlink|rename|mkdir|copyFile|cp)Sync|` +
		`(?:^|[\s;|&])>{1,2}|\bsed\s+-i|\b(?:mv|rm|cp|tee|touch)\s`)
	skillVarRe             = regexp.MustCompile(`\$\{(\w+)\}|\$(\w+)`)
	skillAssignmentRe      = regexp.MustCompile(`(?s)^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	skillRecursiveCopyRe   = regexp.MustCompile(`^(?:-[A-Za-z]*[rRa][A-Za-z]*|--recursive|--archive)$`)
	skillXargsOptionsArg   = setOf("-n", "-I", "-L", "-P", "-d", "-E", "-s", "-a")
	skillFindExecOpeners   = setOf("-exec", "-execdir", "-ok", "-okdir")
	skillInterpreterFlags  = setOf("-c", "-e", "-E", "--eval", "-Command", "-r")
	skillShellInterpreters = setOf("bash", "sh", "zsh", "dash", "ksh")
)

// SkillLockRecordRefusal is shown when a command would change the lock record while a skill is locked
const SkillLockRecordRefusal = "skills/.usage.json holds the user's skill locks, so the agent cannot change it " +
	"while any skill is locked."

// SkillUnlockRefusal is shown when the agent tries to unlock a skill itself
const SkillUnlockRefusal = "Only the user can unlock a skill. Ask them to run `icebath curator unlock <skill>` " +
	"themselves or use the lock switch in Desktop."

// lockedSkill is one locked skill name with the real path of its directory
type lockedSkill struct {
	name string
	root string
}

// LockedSkillDirs lists name and real path of every locked skill in the active profile
func LockedSkillDirs(env *Env) []lockedSkill {
	src := skillLocks()
	names := setOf(src.LockedSkillNames(env)...)
	if len(names) == 0 {
		return nil
	}
	found := map[string]string{}
	for _, base := range src.AllSkillsDirs(env) {
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue
		}
		filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			name := filepath.Base(filepath.Dir(p))
			if _, seen := found[name]; names[name] && !seen && !src.IsExcludedSkillPath(p, base) {
				found[name] = realPathOf(filepath.Dir(p))
			}
			return nil
		})
	}
	out := make([]lockedSkill, 0, len(found))
	for name, root := range found {
		out = append(out, lockedSkill{name, root})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// IsLockedSkill reports whether a directory, category path or frontmatter name is locked
func IsLockedSkill(env *Env, name string, lockedNames []string) bool {
	src := skillLocks()
	if lockedNames == nil {
		lockedNames = src.LockedSkillNames(env)
	}
	names := setOf(lockedNames...)
	if len(names) == 0 {
		return false
	}
	if names[name] || names[filepath.Base(name)] {
		return true
	}
	dirName := src.FindSkillDirName(env, name)
	return dirName != "" && names[dirName]
}

// Real path of the lock record file
func skillLockRecordPath(env *Env) string {
	return realPathOf(filepath.Join(skillLocks().SkillsDir(env), ".usage.json"))
}

// os.path.realpath, the input on failure
func realPathOf(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	return resolveNonStrict(abs)
}

// Reports whether a path equals the root or lies below it
func skillPathInside(p, root string) bool {
	sep := string(os.PathSeparator)
	return p == root || strings.HasPrefix(p, strings.TrimRight(root, sep)+sep)
}

// SkillLockRefusal is the refusal text for a change to one locked skill
func SkillLockRefusal(name string) string {
	return fmt.Sprintf("Skill '%s' is locked by the user, so the agent cannot change it: skill_manage, "+
		"write_file, patch and terminal all refuse. Don't retry or work around it (no copying it "+
		"under another name); tell the user what you would change. They can unlock it with "+
		"`icebath curator unlock %s` or the lock switch in Desktop.", name, name)
}

// Real paths of every skills root
func skillsRoots(env *Env) []string {
	dirs := skillLocks().AllSkillsDirs(env)
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, realPathOf(d))
	}
	return out
}

// Refusal text when a path is the lock record or inside a locked skill, ancestor widens to directories above
func skillLockHit(env *Env, path string, locked []lockedSkill, record, ancestor string) string {
	real := realPathOf(path)
	sep := string(os.PathSeparator)
	if real == record || (ancestor == "any" && real != sep && skillPathInside(record, real)) {
		return SkillLockRecordRefusal
	}
	for _, l := range locked {
		if skillPathInside(real, l.root) {
			return SkillLockRefusal(l.name)
		}
		if ancestor != "" && real != sep && skillPathInside(l.root, real) {
			if ancestor == "any" {
				return SkillLockRefusal(l.name)
			}
			for _, r := range skillsRoots(env) {
				if skillPathInside(real, r) {
					return SkillLockRefusal(l.name)
				}
			}
		}
	}
	return ""
}

// SkillLockPathRefusal is the write_file and patch guard over paths landing in a locked skill or the record
func SkillLockPathRefusal(env *Env, paths []string) string {
	locked := LockedSkillDirs(env)
	if len(locked) == 0 {
		return ""
	}
	record := skillLockRecordPath(env)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if reason := skillLockHit(env, p, locked, record, ""); reason != "" {
			return reason
		}
	}
	return ""
}

// Shell environment seen by word expansion with HOME and ICEBATH_HOME pinned
func skillShellEnv(env *Env, assigned map[string]string) map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	out["HOME"] = expandUser("~")
	out["ICEBATH_HOME"] = envHome(env)
	for k, v := range assigned {
		out[k] = v
	}
	return out
}

// Substitutes shell variables from the environment keeping unknown references literal
func skillExpandVars(word string, shellEnv map[string]string) string {
	return skillVarRe.ReplaceAllStringFunc(word, func(m string) string {
		sub := skillVarRe.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[2]
		}
		if v, ok := shellEnv[name]; ok {
			return v
		}
		return m
	})
}

// Shell word to absolute paths with variables, tilde, cwd anchoring and filesystem globbing
func skillExpandWord(word, cwd string, shellEnv map[string]string) []string {
	word = skillExpandVars(word, shellEnv)
	home := shellEnv["HOME"]
	if word == "~" || strings.HasPrefix(word, "~/") {
		word = home + word[1:]
	}
	path := filepath.Clean(word)
	if !filepath.IsAbs(word) {
		path = filepath.Clean(filepath.Join(cwd, word))
	}
	if !strings.ContainsAny(word, "*?[") {
		return []string{path}
	}
	if matches := skillGlob(path); len(matches) > 0 {
		return matches
	}
	cut := len(path)
	for _, ch := range "*?[" {
		if i := strings.IndexRune(path, ch); i >= 0 && i < cut {
			cut = i
		}
	}
	prefix := path[:cut]
	sep := string(os.PathSeparator)
	if i := strings.LastIndex(prefix, sep); i >= 0 {
		prefix = prefix[:i]
	}
	if prefix == "" {
		prefix = sep
	}
	return []string{prefix}
}

// Filesystem glob with the Python rule that wildcards never match hidden names
func skillGlob(pattern string) []string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	sep := string(os.PathSeparator)
	patternParts := strings.Split(pattern, sep)
	var out []string
	for _, m := range matches {
		parts := strings.Split(m, sep)
		hidden := false
		for i := range parts {
			if i < len(patternParts) && strings.ContainsAny(patternParts[i], "*?[") &&
				!strings.HasPrefix(patternParts[i], ".") && strings.HasPrefix(parts[i], ".") {
				hidden = true
				break
			}
		}
		if !hidden {
			out = append(out, m)
		}
	}
	return out
}

// Turns unquoted newlines into semicolons so the tokenizer sees command separators
func skillUnquotedNewlinesToSemicolons(command string) string {
	var out strings.Builder
	var quote byte
	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '\n':
			ch = ';'
		}
		out.WriteByte(ch)
	}
	return out.String()
}

// Shell tokens with punctuation characters as their own tokens, whitespace words on unbalanced quotes
func skillTokens(command string) []string {
	tokens, err := shlexSplitPunct(skillUnquotedNewlinesToSemicolons(command), ";&|<>()", false)
	if err != nil {
		return strings.Fields(command)
	}
	return tokens
}

// One simple command with its redirect targets and the separator that ended it
type skillSegment struct {
	argv      []string
	targets   []string
	separator string
}

// Splits tokens into simple commands, reading redirect targets and skipping input redirections
func skillSegments(tokens []string) []skillSegment {
	var out []skillSegment
	var argv, targets []string
	allIn := func(tok, chars string) bool {
		for i := 0; i < len(tok); i++ {
			if !strings.ContainsRune(chars, rune(tok[i])) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if strings.Contains(tok, ">") && allIn(tok, "&>|0123456789") {
			if !strings.HasSuffix(tok, "&") {
				target := ""
				if i+1 < len(tokens) {
					i++
					target = tokens[i]
				}
				if target != "" && !strings.HasPrefix(target, "&") && target != "/dev/null" {
					targets = append(targets, target)
				}
			}
			continue
		}
		if tok == "<" || tok == "<<" || tok == "<<<" || tok == "<<-" {
			if i+1 < len(tokens) {
				i++
			}
			continue
		}
		if tok != "" && allIn(tok, ";&|()") {
			out = append(out, skillSegment{argv, targets, tok})
			argv, targets = nil, nil
			continue
		}
		argv = append(argv, tok)
	}
	return append(out, skillSegment{argv, targets, ""})
}

// Drops leading assignments and transparent wrappers with their options
func skillStripWrappers(argv []string) []string {
	for len(argv) > 0 {
		if selfRepoAssignmentRe.MatchString(argv[0]) {
			argv = argv[1:]
			continue
		}
		head := filepath.Base(argv[0])
		if !skillWrappers[head] {
			break
		}
		takesValue := selfRepoWrapperOptionsWithArg[head]
		if head == "xargs" {
			takesValue = skillXargsOptionsArg
		}
		argv = argv[1:]
		for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
			if takesValue[argv[0]] && len(argv) > 1 {
				argv = argv[2:]
			} else if takesValue[argv[0]] {
				argv = nil
			} else {
				argv = argv[1:]
			}
		}
	}
	return argv
}

// Non option arguments
func skillOperands(args []string) []string {
	var out []string
	for _, a := range args {
		if a != "" && !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// Target directory option of cp, mv, install and ln with the remaining arguments
func skillTargetDirectory(args []string) (string, bool, []string) {
	rest := append([]string(nil), args...)
	for i, arg := range rest {
		if strings.HasPrefix(arg, "--target-directory=") {
			return strings.SplitN(arg, "=", 2)[1], true, append(append([]string(nil), rest[:i]...), rest[i+1:]...)
		}
		if (arg == "-t" || arg == "--target-directory") && i+1 < len(rest) {
			return rest[i+1], true, append(append([]string(nil), rest[:i]...), rest[i+2:]...)
		}
		if strings.HasPrefix(arg, "-t") && len(arg) > 2 && !strings.HasPrefix(arg, "--") {
			return arg[2:], true, append(append([]string(nil), rest[:i]...), rest[i+1:]...)
		}
	}
	return "", false, rest
}

// What a command is checked against, its working dir, the locked skills and the lock record
type skillLockScope struct {
	env      *Env
	cwd      string
	locked   []lockedSkill
	record   string
	depth    int
	shellEnv map[string]string
}

func (s *skillLockScope) hit(word, ancestor string) string {
	for _, path := range skillExpandWord(word, s.cwd, s.shellEnv) {
		if r := skillLockHit(s.env, path, s.locked, s.record, ancestor); r != "" {
			return r
		}
	}
	return ""
}

func (s *skillLockScope) firstHit(words []string, ancestor string) string {
	for _, w := range words {
		if r := s.hit(w, ancestor); r != "" {
			return r
		}
	}
	return ""
}

func skillCheckUnlock(verb string, args []string, s *skillLockScope) string {
	cli := verb == "icebath" || verb == "hermes"
	if !cli && strings.HasPrefix(verb, "python") {
		for _, a := range args {
			if a == "icebath_cli.main" || a == "hermes_cli.main" {
				cli = true
			}
		}
	}
	if !cli {
		return ""
	}
	for i, a := range args {
		if a == "curator" {
			for _, b := range args[i:] {
				if b == "unlock" {
					return SkillUnlockRefusal
				}
			}
			break
		}
	}
	return ""
}

func skillCheckFileVerbs(verb string, args []string, s *skillLockScope) string {
	if skillWritesEveryOperand[verb] || verb == "tee" {
		ancestor := ""
		if skillRecursiveVerbs[verb] {
			ancestor = "any"
		}
		return s.firstHit(skillOperands(args), ancestor)
	}
	if !skillWritesLastOperand[verb] {
		return ""
	}
	target, hasTarget, rest := skillTargetDirectory(args)
	ops := skillOperands(rest)
	dest := target
	if !hasTarget {
		if len(ops) == 0 {
			return ""
		}
		dest = ops[len(ops)-1]
	}
	merges := verb == "rsync"
	for _, a := range rest {
		if skillRecursiveCopyRe.MatchString(a) {
			merges = true
		}
	}
	ancestor := ""
	if merges {
		ancestor = "skills"
	}
	return s.hit(dest, ancestor)
}

func skillCheckInplace(verb string, args []string, s *skillLockScope) string {
	if !skillInplaceEditors[verb] {
		return ""
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--in-place") || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i")) {
			return s.firstHit(skillOperands(args), "")
		}
	}
	return ""
}

// Command groups after find exec style options
func skillFindExecCommands(args []string) [][]string {
	var out [][]string
	var group []string
	open := false
	for _, arg := range args {
		switch {
		case skillFindExecOpeners[arg]:
			group, open = []string{}, true
		case open && (arg == ";" || arg == "+"):
			out = append(out, group)
			group, open = nil, false
		case open:
			group = append(group, arg)
		}
	}
	if len(group) > 0 {
		out = append(out, group)
	}
	return out
}

func skillCheckFind(verb string, args []string, s *skillLockScope) string {
	if verb != "find" {
		return ""
	}
	var starts []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		starts = append(starts, a)
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}
	writes := false
	for _, a := range args {
		if a == "-delete" {
			writes = true
		}
	}
	for _, cmd := range skillFindExecCommands(args) {
		probe := cmd
		if len(s.locked) > 0 {
			probe = make([]string, len(cmd))
			for i, w := range cmd {
				if w == "{}" {
					w = filepath.Join(s.locked[0].root, "SKILL.md")
				}
				probe[i] = w
			}
		}
		writes = writes || (len(probe) > 0 && skillArgvRefusal(s.env, probe, s.cwd, s.locked, s.record, s.depth, s.shellEnv) != "")
	}
	if !writes {
		return ""
	}
	return s.firstHit(starts, "any")
}

func skillCheckGit(verb string, args []string, s *skillLockScope) string {
	if verb != "git" {
		return ""
	}
	where := s.cwd
	rest := append([]string(nil), args...)
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		flag := rest[0]
		rest = rest[1:]
		if flag == "-C" && len(rest) > 0 {
			where = skillExpandWord(rest[0], s.cwd, s.shellEnv)[0]
			rest = rest[1:]
		} else if (flag == "-c" || flag == "--git-dir" || flag == "--work-tree") && len(rest) > 0 {
			rest = rest[1:]
		}
	}
	if len(rest) > 0 && skillGitMutations[rest[0]] {
		return skillLockHit(s.env, where, s.locked, s.record, "any")
	}
	return ""
}

func skillCheckExtract(verb string, args []string, s *skillLockScope) string {
	if verb != "tar" && verb != "unzip" && verb != "bsdtar" {
		return ""
	}
	dest := s.cwd
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-C" || args[i] == "--directory" || args[i] == "-d" {
			dest = skillExpandWord(args[i+1], s.cwd, s.shellEnv)[0]
		}
	}
	bundled := len(args) > 0 && !strings.HasPrefix(args[0], "-") && strings.Contains(args[0], "x")
	extracting := verb == "unzip" || bundled
	for _, a := range args {
		if a == "--extract" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "x")) {
			extracting = true
		}
	}
	if !extracting {
		return ""
	}
	return skillLockHit(s.env, dest, s.locked, s.record, "skills")
}

func skillCheckInterpreter(verb string, args []string, s *skillLockScope) string {
	if !skillInterpretersRe.MatchString(verb) {
		return ""
	}
	for i := 0; i+1 < len(args); i++ {
		if !skillInterpreterFlags[args[i]] {
			continue
		}
		code := args[i+1]
		var reason string
		if skillShellInterpreters[verb] && s.depth < 3 {
			reason = skillCommandRefusal(s.env, code, s.cwd, s.locked, s.record, s.depth+1)
		} else {
			reason = skillCodeRefusal(s.env, code, s.locked, s.record, s.cwd)
		}
		if reason != "" {
			return reason
		}
	}
	return ""
}

var skillArgvChecks []func(string, []string, *skillLockScope) string

func init() {
	skillArgvChecks = []func(string, []string, *skillLockScope) string{
		skillCheckUnlock, skillCheckFileVerbs, skillCheckInplace, skillCheckFind, skillCheckGit, skillCheckExtract, skillCheckInterpreter,
	}
}

// First refusal of the argv checks for one simple command
func skillArgvRefusal(env *Env, argv []string, cwd string, locked []lockedSkill, record string, depth int, shellEnv map[string]string) string {
	if shellEnv == nil {
		shellEnv = skillShellEnv(env, nil)
	}
	scope := &skillLockScope{env, cwd, locked, record, depth, shellEnv}
	verb := filepath.Base(argv[0])
	for _, check := range skillArgvChecks {
		if r := check(verb, argv[1:], scope); r != "" {
			return r
		}
	}
	return ""
}

// Inline interpreter code that looks like it writes and names a locked skill or the record
func skillCodeRefusal(env *Env, code string, locked []lockedSkill, record, cwd string) string {
	if !skillWriteishCodeRe.MatchString(code) {
		return ""
	}
	if cwd != "" {
		if reason := skillLockHit(env, cwd, locked, record, ""); reason != "" {
			return reason
		}
	}
	home := expandUser("~")
	text := strings.ReplaceAll(code, "$ICEBATH_HOME", envHome(env))
	text = strings.ReplaceAll(text, "$HERMES_HOME", envHome(env))
	text = strings.ReplaceAll(text, "~", home)
	skillsRoot := realPathOf(skillLocks().SkillsDir(env))
	if strings.Contains(text, ".usage.json") {
		return SkillLockRecordRefusal
	}
	for _, l := range locked {
		rel := ""
		if skillsRoot != "" && skillPathInside(l.root, skillsRoot) {
			if r, err := filepath.Rel(skillsRoot, l.root); err == nil {
				rel = r
			}
		}
		if strings.Contains(text, l.root) || (rel != "" && strings.Contains(text, "skills/"+rel)) ||
			strings.Contains(text, "skills/"+filepath.Base(l.root)) {
			return SkillLockRefusal(l.name)
		}
	}
	return ""
}

// Walks the simple commands of a shell command tracking cd, assignments and subshell scopes
func skillCommandRefusal(env *Env, command, cwd string, locked []lockedSkill, record string, depth int) string {
	if strings.Contains(command, "<<") {
		if reason := skillCodeRefusal(env, command, locked, record, cwd); reason != "" {
			return reason
		}
	}
	shellEnv := skillShellEnv(env, nil)
	var subshells []string
	for _, seg := range skillSegments(skillTokens(command)) {
		argv := seg.argv
		words := argv
		if len(argv) > 0 && argv[0] == "export" {
			words = argv[1:]
		}
		if len(words) > 0 {
			allAssign := true
			for _, w := range words {
				if !skillAssignmentRe.MatchString(w) {
					allAssign = false
					break
				}
			}
			if allAssign {
				for _, w := range words {
					m := skillAssignmentRe.FindStringSubmatch(w)
					shellEnv[m[1]] = skillExpandVars(m[2], shellEnv)
				}
				argv = nil
			}
		}
		for _, target := range seg.targets {
			for _, path := range skillExpandWord(target, cwd, shellEnv) {
				if reason := skillLockHit(env, path, locked, record, ""); reason != "" {
					return reason
				}
			}
		}
		argv = skillStripWrappers(argv)
		if len(argv) > 1 && (argv[0] == "cd" || argv[0] == "pushd") {
			cwd = skillExpandWord(argv[1], cwd, shellEnv)[0]
		} else if len(argv) > 0 {
			if reason := skillArgvRefusal(env, argv, cwd, locked, record, depth, shellEnv); reason != "" {
				return reason
			}
		}
		for _, ch := range seg.separator {
			if ch == '(' {
				subshells = append(subshells, cwd)
			} else if ch == ')' && len(subshells) > 0 {
				cwd = subshells[len(subshells)-1]
				subshells = subshells[:len(subshells)-1]
			}
		}
	}
	return ""
}

// SkillLockCommandRefusal is the terminal guard, refusal text when a command writes into a locked skill
func SkillLockCommandRefusal(env *Env, command, cwd string) string {
	locked := LockedSkillDirs(env)
	if len(locked) == 0 {
		return ""
	}
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			wd = ""
		}
		cwd = wd
	}
	return skillCommandRefusal(env, command, cwd, locked, skillLockRecordPath(env), 0)
}
