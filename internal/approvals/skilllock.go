package approvals

import (
	"cmp"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// A skill the user locked can't be changed by the agent's commands: writes into its folder, changes to the lock record and unlocking are
// refused

// Refusal of a change to the lock record while a skill is locked
const recordRefusal = "skills/.usage.json holds the user's skill locks, so the agent cannot change it while any skill is locked."

// Refusal of the agent unlocking a skill itself
const unlockRefusal = "Only the user can unlock a skill. Ask them to unlock it themselves."

var (
	writesEveryOperand = strs.Set("rm", "rmdir", "unlink", "truncate", "shred", "touch", "chmod", "chown", "chgrp", "chattr", "setfacl", "mkdir", "mv",
		"patch", "dos2unix", "unix2dos", "xattr", "ed", "ex", "vi", "vim", "nvim", "nano", "emacs")
	writesLastOperand = strs.Set("cp", "install", "ln", "rsync", "scp")
	recursiveVerbs    = strs.Set("rm", "mv", "chmod", "chown", "chgrp", "chattr", "setfacl", "rsync", "find")
	inplaceEditors    = strs.Set("sed", "perl", "ruby")
	gitWrites         = strs.Set("add", "am", "apply", "checkout", "clean", "commit", "merge", "mv", "pull", "rebase", "reset", "restore", "revert", "rm",
		"stash", "switch", "cherry-pick")
	codeInterpreters = regexp.MustCompile(`^(?:python[0-9.]*|node|nodejs|deno|bun|ruby|perl|php|bash|sh|zsh|dash|ksh|pwsh|powershell)$`)
	lockWrappers     = strs.Set("sudo", "env", "command", "builtin", "exec", "nohup", "setsid", "time", "nice", "xargs")
	writeishCode     = regexp.MustCompile(`(?i)open\s*\([^)]*['"][rbt]*[wax+][^'"]*['"]|\.write(?:_text|_bytes|lines)?\s*\(|` +
		`\b(?:unlink|rmtree|removedirs|remove|rmdir|rename|truncate|chmod|chown|mkdir|makedirs|symlink|copyfile|copytree|copy2?|move|touch)\s*\(|` +
		`\bos\.replace\s*\(|\.replace\s*\(\s*(?:Path|['"]/)|(?:write|append)File(?:Sync)?|(?:rm|unlink|rename|mkdir|copyFile|cp)Sync|` +
		`(?:^|[\s;|&])>{1,2}|\bsed\s+-i|\b(?:mv|rm|cp|tee|touch)\s`)
	shellVar        = regexp.MustCompile(`\$\{(\w+)\}|\$(\w+)`)
	shellAssignment = regexp.MustCompile(`(?s)^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	recursiveCopy   = regexp.MustCompile(`^(?:-[A-Za-z]*[rRa][A-Za-z]*|--recursive|--archive)$`)
	xargsArgOptions = strs.Set("-n", "-I", "-L", "-P", "-d", "-E", "-s", "-a")
	findExecOpeners = strs.Set("-exec", "-execdir", "-ok", "-okdir")
	codeFlags       = strs.Set("-c", "-e", "-E", "--eval", "-Command", "-r")
)

// How far a path may be from a locked skill to count: inside it only, or also any folder above it, or a folder above it inside the skills
const (
	insideOnly = ""
	anyAbove   = "any"
	skillsOnly = "skills"
)

type lockedSkill struct {
	name, root string
}

// What a command is checked against: the locked skills, the lock record, the skills folders and the variables a shell would expand
type lockScope struct {
	locked []lockedSkill
	record string
	roots  []string
	skills string
	home   string
	// Variables of the walk in progress, and those every walk starts from
	env, base map[string]string
}

func inside(p, root string) bool {
	sep := string(os.PathSeparator)
	return p == root || strings.HasPrefix(p, strings.TrimRight(root, sep)+sep)
}

func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return resolve(p)
}

// Refusal when the path is the lock record or inside a locked skill, or above one as far as reach allows
func (s *lockScope) hitPath(path, reach string) string {
	real := realPath(path)
	sep := string(os.PathSeparator)
	if real == s.record || reach == anyAbove && real != sep && inside(s.record, real) {
		return recordRefusal
	}
	for _, l := range s.locked {
		if inside(real, l.root) {
			return skills.LockRefusal(l.name)
		}
		if reach == insideOnly || real == sep || !inside(l.root, real) {
			continue
		}
		if reach == anyAbove || slices.ContainsFunc(s.roots, func(root string) bool { return inside(real, root) }) {
			return skills.LockRefusal(l.name)
		}
	}
	return ""
}

// Variables expanded from the shell's environment, unknown ones left as written
func (s *lockScope) expandVars(word string) string {
	return shellVar.ReplaceAllStringFunc(word, func(m string) string {
		sub := shellVar.FindStringSubmatch(m)
		if value, ok := s.env[cmp.Or(sub[1], sub[2])]; ok {
			return value
		}
		return m
	})
}

// Paths a word stands for: variables and ~ expanded, anchored on the folder, globs matched on disk
func (s *lockScope) expand(word, cwd string) []string {
	word = s.expandVars(word)
	if word == "~" || strings.HasPrefix(word, "~/") {
		word = s.env["HOME"] + word[1:]
	}
	path := filepath.Clean(word)
	if !filepath.IsAbs(word) {
		path = filepath.Clean(filepath.Join(cwd, word))
	}
	if !strings.ContainsAny(word, "*?[") {
		return []string{path}
	}
	if found := glob(path); len(found) > 0 {
		return found
	}
	cut := len(path)
	if i := strings.IndexAny(path, "*?["); i >= 0 {
		cut = i
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

// Glob on disk whose wildcards never match hidden names, as a shell's don't
func glob(pattern string) []string {
	found, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	sep := string(os.PathSeparator)
	parts := strings.Split(pattern, sep)
	var out []string
	for _, m := range found {
		hidden := false
		for i, part := range strings.Split(m, sep) {
			if i < len(parts) && strings.ContainsAny(parts[i], "*?[") && !strings.HasPrefix(parts[i], ".") && strings.HasPrefix(part, ".") {
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

func (s *lockScope) hit(word, cwd, reach string) string {
	for _, path := range s.expand(word, cwd) {
		if refusal := s.hitPath(path, reach); refusal != "" {
			return refusal
		}
	}
	return ""
}

func (s *lockScope) firstHit(words []string, cwd, reach string) string {
	for _, word := range words {
		if refusal := s.hit(word, cwd, reach); refusal != "" {
			return refusal
		}
	}
	return ""
}

// Words of a command with newlines outside quotes as separators and punctuation as words of their own; whitespace words when quoting
// doesn't close
func lockTokens(command string) []string {
	var b strings.Builder
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
		b.WriteByte(ch)
	}
	tokens, err := splitPunct(b.String(), ";&|<>()", false)
	if err != nil {
		return strings.Fields(command)
	}
	return tokens
}

// One simple command, where it redirects output to and what ended it
type simple struct {
	argv, targets []string
	separator     string
}

func only(token, chars string) bool {
	return strings.Trim(token, chars) == ""
}

// Simple commands of the words, output redirect targets read off and input redirections skipped
func simples(tokens []string) []simple {
	var out []simple
	var argv, targets []string
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch {
		case strings.Contains(token, ">") && only(token, "&>|0123456789"):
			if !strings.HasSuffix(token, "&") {
				target := ""
				if i+1 < len(tokens) {
					i++
					target = tokens[i]
				}
				if target != "" && !strings.HasPrefix(target, "&") && target != "/dev/null" {
					targets = append(targets, target)
				}
			}
		case token == "<" || token == "<<" || token == "<<<" || token == "<<-":
			if i+1 < len(tokens) {
				i++
			}
		case token != "" && only(token, ";&|()"):
			out = append(out, simple{argv, targets, token})
			argv, targets = nil, nil
		default:
			argv = append(argv, token)
		}
	}
	return append(out, simple{argv, targets, ""})
}

// Arguments with the assignments and wrappers before the command dropped
func stripWrappers(argv []string) []string {
	for len(argv) > 0 {
		if envAssign.MatchString(argv[0]) {
			argv = argv[1:]
			continue
		}
		head := filepath.Base(argv[0])
		if !lockWrappers[head] {
			break
		}
		values := repoWrappers[head]
		if head == "xargs" {
			values = xargsArgOptions
		}
		for argv = argv[1:]; len(argv) > 0 && strings.HasPrefix(argv[0], "-"); {
			switch {
			case values[argv[0]] && len(argv) > 1:
				argv = argv[2:]
			case values[argv[0]]:
				argv = nil
			default:
				argv = argv[1:]
			}
		}
	}
	return argv
}

func operands(args []string) []string {
	var out []string
	for _, arg := range args {
		if arg != "" && !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
		}
	}
	return out
}

// Target folder option of cp, mv, install and ln, and the arguments left
func targetDirectory(args []string) (string, bool, []string) {
	without := func(i, n int) []string { return append(slices.Clone(args[:i]), args[i+n:]...) }
	for i, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--target-directory="):
			return strings.SplitN(arg, "=", 2)[1], true, without(i, 1)
		case (arg == "-t" || arg == "--target-directory") && i+1 < len(args):
			return args[i+1], true, without(i, 2)
		case strings.HasPrefix(arg, "-t") && len(arg) > 2 && !strings.HasPrefix(arg, "--"):
			return arg[2:], true, without(i, 1)
		}
	}
	return "", false, slices.Clone(args)
}

func (s *lockScope) unlock(verb string, args []string) string {
	cli := verb == "icebath" || strings.HasPrefix(verb, "python") && slices.Contains(args, "icebath_cli.main")
	if !cli {
		return ""
	}
	if i := slices.Index(args, "curator"); i >= 0 && slices.Contains(args[i:], "unlock") {
		return unlockRefusal
	}
	return ""
}

func (s *lockScope) fileVerbs(verb string, args []string, cwd string) string {
	if writesEveryOperand[verb] || verb == "tee" {
		reach := insideOnly
		if recursiveVerbs[verb] {
			reach = anyAbove
		}
		return s.firstHit(operands(args), cwd, reach)
	}
	if !writesLastOperand[verb] {
		return ""
	}
	dest, has, rest := targetDirectory(args)
	if !has {
		ops := operands(rest)
		if len(ops) == 0 {
			return ""
		}
		dest = ops[len(ops)-1]
	}
	reach := insideOnly
	if verb == "rsync" || slices.ContainsFunc(rest, recursiveCopy.MatchString) {
		reach = skillsOnly
	}
	return s.hit(dest, cwd, reach)
}

func (s *lockScope) inplace(verb string, args []string, cwd string) string {
	if !inplaceEditors[verb] {
		return ""
	}
	if slices.ContainsFunc(args, func(arg string) bool {
		return strings.HasPrefix(arg, "--in-place") || strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "i")
	}) {
		return s.firstHit(operands(args), cwd, insideOnly)
	}
	return ""
}

// Commands find runs with -exec and the like
func findCommands(args []string) [][]string {
	var out [][]string
	var group []string
	open := false
	for _, arg := range args {
		switch {
		case findExecOpeners[arg]:
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

func (s *lockScope) find(verb string, args []string, cwd string, depth int) string {
	if verb != "find" {
		return ""
	}
	var starts []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			break
		}
		starts = append(starts, arg)
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}
	writes := slices.Contains(args, "-delete")
	for _, command := range findCommands(args) {
		probe := command
		if len(s.locked) > 0 {
			probe = make([]string, len(command))
			for i, word := range command {
				if word == "{}" {
					word = filepath.Join(s.locked[0].root, "SKILL.md")
				}
				probe[i] = word
			}
		}
		writes = writes || len(probe) > 0 && s.argv(probe, cwd, depth) != ""
	}
	if !writes {
		return ""
	}
	return s.firstHit(starts, cwd, anyAbove)
}

func (s *lockScope) git(verb string, args []string, cwd string) string {
	if verb != "git" {
		return ""
	}
	where, rest := cwd, slices.Clone(args)
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		flag := rest[0]
		rest = rest[1:]
		switch {
		case flag == "-C" && len(rest) > 0:
			where, rest = s.expand(rest[0], cwd)[0], rest[1:]
		case (flag == "-c" || flag == "--git-dir" || flag == "--work-tree") && len(rest) > 0:
			rest = rest[1:]
		}
	}
	if len(rest) > 0 && gitWrites[rest[0]] {
		return s.hitPath(where, anyAbove)
	}
	return ""
}

func (s *lockScope) extract(verb string, args []string, cwd string) string {
	if verb != "tar" && verb != "unzip" && verb != "bsdtar" {
		return ""
	}
	dest := cwd
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-C" || args[i] == "--directory" || args[i] == "-d" {
			dest = s.expand(args[i+1], cwd)[0]
		}
	}
	extracting := verb == "unzip" || len(args) > 0 && !strings.HasPrefix(args[0], "-") && strings.Contains(args[0], "x") ||
		slices.ContainsFunc(args, func(arg string) bool {
			return arg == "--extract" || strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "x")
		})
	if !extracting {
		return ""
	}
	return s.hitPath(dest, skillsOnly)
}

func (s *lockScope) interpreter(verb string, args []string, cwd string, depth int) string {
	if !codeInterpreters.MatchString(verb) {
		return ""
	}
	for i := 0; i+1 < len(args); i++ {
		if !codeFlags[args[i]] {
			continue
		}
		code := args[i+1]
		refusal := ""
		if shellExecutables[verb] && depth < 3 {
			refusal = s.command(code, cwd, depth+1)
		} else {
			refusal = s.code(code, cwd)
		}
		if refusal != "" {
			return refusal
		}
	}
	return ""
}

// First refusal of the checks for one simple command
func (s *lockScope) argv(argv []string, cwd string, depth int) string {
	verb, args := filepath.Base(argv[0]), argv[1:]
	for _, refusal := range []func() string{
		func() string { return s.unlock(verb, args) },
		func() string { return s.fileVerbs(verb, args, cwd) },
		func() string { return s.inplace(verb, args, cwd) },
		func() string { return s.find(verb, args, cwd, depth) },
		func() string { return s.git(verb, args, cwd) },
		func() string { return s.extract(verb, args, cwd) },
		func() string { return s.interpreter(verb, args, cwd, depth) },
	} {
		if text := refusal(); text != "" {
			return text
		}
	}
	return ""
}

// Refusal of inline code that looks like it writes and names a locked skill or the record
func (s *lockScope) code(code, cwd string) string {
	if !writeishCode.MatchString(code) {
		return ""
	}
	if cwd != "" {
		if refusal := s.hitPath(cwd, insideOnly); refusal != "" {
			return refusal
		}
	}
	text := strings.ReplaceAll(code, "$ICEBATH_HOME", s.home)
	text = strings.ReplaceAll(text, "~", fsx.ExpandHome("~"))
	if strings.Contains(text, ".usage.json") {
		return recordRefusal
	}
	for _, l := range s.locked {
		rel := ""
		if s.skills != "" && inside(l.root, s.skills) {
			rel, _ = filepath.Rel(s.skills, l.root)
		}
		if strings.Contains(text, l.root) || rel != "" && strings.Contains(text, "skills/"+rel) || strings.Contains(text, "skills/"+filepath.Base(l.root)) {
			return skills.LockRefusal(l.name)
		}
	}
	return ""
}

// Walk over the simple commands of the shell text, following cd, assignments and subshells
func (s *lockScope) command(command, cwd string, depth int) string {
	saved := s.env
	s.env = maps.Clone(s.base)
	defer func() { s.env = saved }()
	if strings.Contains(command, "<<") {
		if refusal := s.code(command, cwd); refusal != "" {
			return refusal
		}
	}
	var subshells []string
	for _, seg := range simples(lockTokens(command)) {
		argv := seg.argv
		words := argv
		if len(argv) > 0 && argv[0] == "export" {
			words = argv[1:]
		}
		if len(words) > 0 && !slices.ContainsFunc(words, func(w string) bool { return !shellAssignment.MatchString(w) }) {
			for _, word := range words {
				m := shellAssignment.FindStringSubmatch(word)
				s.env[m[1]] = s.expandVars(m[2])
			}
			argv = nil
		}
		for _, target := range seg.targets {
			for _, path := range s.expand(target, cwd) {
				if refusal := s.hitPath(path, insideOnly); refusal != "" {
					return refusal
				}
			}
		}
		argv = stripWrappers(argv)
		switch {
		case len(argv) > 1 && (argv[0] == "cd" || argv[0] == "pushd"):
			cwd = s.expand(argv[1], cwd)[0]
		case len(argv) > 0:
			if refusal := s.argv(argv, cwd, depth); refusal != "" {
				return refusal
			}
		}
		for _, ch := range seg.separator {
			switch {
			case ch == '(':
				subshells = append(subshells, cwd)
			case ch == ')' && len(subshells) > 0:
				cwd, subshells = subshells[len(subshells)-1], subshells[:len(subshells)-1]
			}
		}
	}
	return ""
}

// Refusal of a command that writes into a skill the user locked, changes the lock record or unlocks a skill, run in the folder given;
// empty when it may run or nothing is locked
func SkillLock(scope *skills.Scope, command, cwd string) string {
	dirs, err := scope.LockedDirs()
	if err != nil || len(dirs) == 0 {
		return ""
	}
	s := &lockScope{record: realPath(filepath.Join(scope.Dir(), ".usage.json")), skills: realPath(scope.Dir()),
		home: scope.Config.ProfileDir(scope.Config.Profile()), base: map[string]string{}}
	for name, root := range dirs {
		s.locked = append(s.locked, lockedSkill{name, realPath(root)})
	}
	sort.Slice(s.locked, func(i, j int) bool { return s.locked[i].name < s.locked[j].name })
	for _, dir := range scope.Dirs() {
		s.roots = append(s.roots, realPath(dir))
	}
	maps.Copy(s.base, scope.Config.Environ())
	s.base["HOME"], s.base["ICEBATH_HOME"] = fsx.ExpandHome("~"), s.home
	s.env = s.base
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return s.command(command, cwd, 0)
}
