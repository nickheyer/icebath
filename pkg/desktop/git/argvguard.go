package git

import (
	"regexp"
	"strings"
)

// Vulnerability categories the review pane enables for the git it runs
var enabledUnsafeCategories = map[string]bool{"allowUnsafeAskPass": true, "allowUnsafeSshCommand": true}

const jsSpaceClass = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

const jsDotClass = `[^\n\r\x{2028}\x{2029}]`

type argFlag struct {
	name     string
	value    *string
	isGlobal bool
}

type flagSpec struct {
	short map[byte]bool
	long  map[string]bool
}

var universalShort = map[byte]bool{'c': true}

var globalFlagSpec = flagSpec{
	short: map[byte]bool{'C': true, 'P': false, 'h': false, 'p': false, 'v': false, 'c': true},
	long:  setOf("attr-source", "config-env", "exec-path", "git-dir", "list-cmds", "namespace", "super-prefix", "work-tree"),
}

var commandFlagSpecs = map[string]flagSpec{
	"clone": {
		short: map[byte]bool{'b': true, 'j': true, 'l': false, 'n': false, 'o': true, 'q': false, 's': false, 'u': true},
		long:  setOf("branch", "config", "jobs", "origin", "upload-pack", "u", "template"),
	},
	"commit": {
		short: map[byte]bool{'C': true, 'F': true, 'c': true, 'm': true, 't': true},
		long:  setOf("file", "message", "reedit-message", "reuse-message", "template"),
	},
	"config": {
		short: map[byte]bool{'e': false, 'f': true, 'l': false},
		long:  setOf("blob", "comment", "default", "file", "type", "value"),
	},
	"fetch": {short: map[byte]bool{}, long: setOf("upload-pack")},
	"init":  {short: map[byte]bool{}, long: setOf("template")},
	"pull":  {short: map[byte]bool{}, long: setOf("upload-pack")},
	"push":  {short: map[byte]bool{}, long: setOf("exec", "receive-pack")},
	"rebase": {
		short: map[byte]bool{'X': true, 'f': false, 'i': false, 'k': false, 'm': false, 'n': false, 'q': false, 'r': false, 's': true, 'v': false, 'x': true},
		long:  setOf("exec", "onto", "strategy", "strategy-option"),
	},
}

func setOf(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}

// Short flags a task accepts, the universal -c merged under the task's own
func flagSpecForTask(task *string) flagSpec {
	spec := flagSpec{long: map[string]bool{}}
	if task != nil {
		if s, ok := commandFlagSpecs[*task]; ok {
			spec.long = s.long
		}
	}
	spec.short = map[byte]bool{}
	for k, v := range universalShort {
		spec.short[k] = v
	}
	if task != nil {
		for k, v := range commandFlagSpecs[*task].short {
			spec.short[k] = v
		}
	}
	return spec
}

type expandedToken struct {
	name      string
	value     *string
	needsNext bool
}

// Splits one flag token into flags, long options at their equals sign and short clusters per letter
func expandToken(raw string, spec flagSpec) []expandedToken {
	if strings.HasPrefix(raw, "--") {
		if eq := strings.Index(raw, "="); eq > 2 {
			v := raw[eq+1:]
			return []expandedToken{{name: raw[:eq], value: &v}}
		}
		return []expandedToken{{name: raw, needsNext: spec.long[raw[2:]]}}
	}
	if len(raw) == 2 {
		return []expandedToken{{name: raw, needsNext: spec.short[raw[1]]}}
	}
	chars := raw[1:]
	var out []expandedToken
	for i := 0; i < len(chars); i++ {
		c := chars[i]
		consumes, known := spec.short[c]
		if !known {
			return []expandedToken{{name: raw}}
		}
		if consumes {
			if remainder := chars[i+1:]; remainder != "" {
				allKnown := true
				for j := 0; j < len(remainder); j++ {
					if _, ok := spec.short[remainder[j]]; !ok {
						allKnown = false
						break
					}
				}
				if !allKnown {
					return append(out, expandedToken{name: "-" + string(c), value: &remainder})
				}
			}
		}
		out = append(out, expandedToken{name: "-" + string(c), needsNext: consumes})
	}
	return out
}

func parseGlobalFlags(tokens []string) ([]argFlag, int) {
	var flags []argFlag
	i := 0
	for i < len(tokens) {
		raw := tokens[i]
		if !strings.HasPrefix(raw, "-") || len(raw) < 2 {
			break
		}
		next := i + 1
		for _, tok := range expandToken(raw, globalFlagSpec) {
			f := argFlag{name: tok.name, value: tok.value, isGlobal: true}
			if tok.needsNext && f.value == nil && next < len(tokens) {
				v := tokens[next]
				f.value = &v
				next++
			}
			flags = append(flags, f)
		}
		i = next
	}
	return flags, i
}

func parseTaskFlags(tokens []string, task *string, flags []argFlag) ([]argFlag, []string) {
	spec := flagSpecForTask(task)
	var positionals []string
	i := 0
	for i < len(tokens) {
		raw := tokens[i]
		if raw == "--" {
			break
		}
		if !strings.HasPrefix(raw, "-") || len(raw) < 2 {
			positionals = append(positionals, raw)
			i++
			continue
		}
		next := i + 1
		for _, tok := range expandToken(raw, spec) {
			f := argFlag{name: tok.name, value: tok.value}
			if tok.needsNext && f.value == nil && next < len(tokens) {
				v := tokens[next]
				f.value = &v
				next++
			}
			flags = append(flags, f)
		}
		i = next
	}
	return flags, positionals
}

type configWrite struct{ key string }

// Key of a key=value assignment, nil when there is no value or the equals sign leads
func parseAssignment(raw *string) *string {
	if raw == nil || *raw == "" {
		return nil
	}
	eq := strings.Index(*raw, "=")
	if eq <= 0 {
		return nil
	}
	key := jsLower(jsTrim((*raw)[:eq]))
	return &key
}

var configWriteFlags = setOf("--add", "--edit", "--remove-section", "--rename-section", "--replace-all", "--unset", "--unset-all", "-e")
var configReadFlags = setOf("--get", "--get-all", "--get-color", "--get-colorbool", "--get-regexp", "--get-urlmatch", "--list", "-l")
var configWriteVerbs = setOf("edit", "remove-section", "rename-section", "set", "unset")
var configReadVerbs = setOf("get", "get-color", "get-colorbool", "list")

// Config key a git config invocation writes, nil when it reads or names no key
func configActionWrite(flags []argFlag, positionals []string) *string {
	operation := func(isWrite bool, pos []string) *string {
		if !isWrite || len(pos) == 0 {
			return nil
		}
		key := jsLower(pos[0])
		return &key
	}
	for _, f := range flags {
		if f.isGlobal {
			continue
		}
		if configWriteFlags[f.name] {
			return operation(true, positionals)
		}
		if configReadFlags[f.name] {
			return nil
		}
	}
	if len(positionals) == 0 {
		return nil
	}
	verb := jsLower(positionals[0])
	if configWriteVerbs[verb] {
		return operation(true, positionals[1:])
	}
	if configReadVerbs[verb] || len(positionals) == 1 {
		return nil
	}
	return operation(true, positionals)
}

func collectConfigWrites(task *string, flags []argFlag, positionals []string) []configWrite {
	var writes []configWrite
	for _, f := range flags {
		if f.name != "-c" && f.name != "--config" && f.name != "--config-env" {
			continue
		}
		if key := parseAssignment(f.value); key != nil {
			writes = append(writes, configWrite{key: *key})
		}
	}
	if task != nil && *task == "config" {
		if key := configActionWrite(flags, positionals); key != nil {
			writes = append(writes, configWrite{key: *key})
		}
	}
	return writes
}

type vulnerability struct{ category, message string }

type flagRule struct {
	task       string
	match      *regexp.Regexp
	category   string
	name       string
	globalOnly bool
	withValue  bool
}

// Flag rule for a literal option text, found anywhere in the flag name
func literalFlagRule(task, flag, category string, globalOnly, withValue bool) flagRule {
	return flagRule{task: task, match: regexp.MustCompile(jsSpaceClass + `*` + regexp.QuoteMeta(strings.ToLower(flag))), category: category, name: flag, globalOnly: globalOnly, withValue: withValue}
}

var unsafeFlagRules = []flagRule{
	{match: regexp.MustCompile(`--(upload|receive)-pack`), category: "allowUnsafePack", name: "--upload-pack or --receive-pack"},
	{task: "clone", match: regexp.MustCompile(`^-\w*u`), category: "allowUnsafePack", name: `/^-\w*u/`},
	literalFlagRule("clone", "--u", "allowUnsafePack", false, false),
	{task: "push", match: regexp.MustCompile(`^--exec$`), category: "allowUnsafePack", name: "--exec"},
	{task: "rebase", match: regexp.MustCompile(`^(-x|--ex(ec?)?)$`), category: "allowUnsafeExec", name: "-x or --exec"},
	literalFlagRule("", "--template", "allowUnsafeTemplateDir", false, false),
	literalFlagRule("", "--exec-path", "allowUnsafeExec", true, true),
	literalFlagRule("", "--git-dir", "allowUnsafeConfigPaths", true, true),
	literalFlagRule("", "--work-tree", "allowUnsafeConfigPaths", true, true),
	{match: regexp.MustCompile(`^-C$`), category: "allowUnsafeConfigPaths", name: "-C", globalOnly: true, withValue: true},
}

type configRule struct {
	match    *regexp.Regexp
	category string
	name     string
}

// Config key pattern with each dot standing for any character, as the JavaScript source pattern reads
func configPattern(source string) string {
	var b strings.Builder
	b.WriteString(jsSpaceClass + `*`)
	for _, part := range strings.Split(source, ".") {
		if b.Len() > len(jsSpaceClass)+1 {
			b.WriteString(jsDotClass)
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	return b.String()
}

func plainConfigRule(config, category string) configRule {
	return configRule{match: regexp.MustCompile(configPattern(strings.ToLower(config))), category: category, name: config}
}

// Rule whose dots also stand for a subsection, such as credential.<url>.helper
func expandedConfigRule(config, category string) configRule {
	parts := strings.Split(strings.ToLower(config), ".")
	var b strings.Builder
	b.WriteString(jsSpaceClass + `*`)
	for i, part := range parts {
		if i > 0 {
			b.WriteString(`(` + jsDotClass + jsDotClass + `+)?` + jsDotClass)
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	return configRule{match: regexp.MustCompile(b.String()), category: category, name: config}
}

var unsafeConfigRules = []configRule{
	plainConfigRule("alias", "allowUnsafeAlias"),
	plainConfigRule("core.askPass", "allowUnsafeAskPass"),
	plainConfigRule("core.editor", "allowUnsafeEditor"),
	plainConfigRule("core.fsmonitor", "allowUnsafeFsMonitor"),
	plainConfigRule("core.gitProxy", "allowUnsafeGitProxy"),
	plainConfigRule("core.hooksPath", "allowUnsafeHooksPath"),
	plainConfigRule("core.pager", "allowUnsafePager"),
	plainConfigRule("core.sshCommand", "allowUnsafeSshCommand"),
	expandedConfigRule("credential.helper", "allowUnsafeCredentialHelper"),
	expandedConfigRule("diff.command", "allowUnsafeDiffExternal"),
	plainConfigRule("diff.external", "allowUnsafeDiffExternal"),
	expandedConfigRule("difftool.cmd", "allowUnsafeDiffExternal"),
	expandedConfigRule("diff.textconv", "allowUnsafeDiffTextConv"),
	expandedConfigRule("filter.clean", "allowUnsafeFilter"),
	expandedConfigRule("filter.process", "allowUnsafeFilter"),
	expandedConfigRule("filter.smudge", "allowUnsafeFilter"),
	expandedConfigRule("gpg.program", "allowUnsafeGpgProgram"),
	plainConfigRule("include.path", "allowUnsafeInclude"),
	expandedConfigRule("includeIf", "allowUnsafeInclude"),
	plainConfigRule("init.templateDir", "allowUnsafeTemplateDir"),
	expandedConfigRule("pager.", "allowUnsafePager"),
	expandedConfigRule("merge.driver", "allowUnsafeMergeDriver"),
	expandedConfigRule("mergetool.path", "allowUnsafeMergeDriver"),
	expandedConfigRule("mergetool.cmd", "allowUnsafeMergeDriver"),
	expandedConfigRule("protocol.allow", "allowUnsafeProtocolOverride"),
	expandedConfigRule("remote.receivepack", "allowUnsafePack"),
	expandedConfigRule("remote.uploadpack", "allowUnsafePack"),
	plainConfigRule("uploadpack.packObjectsHook", "allowUnsafePack"),
	plainConfigRule("sequence.editor", "allowUnsafeEditor"),
	expandedConfigRule("submodule.update", "allowUnsafeSubmodule"),
	expandedConfigRule("tar.command", "allowUnsafeCommandBinaries"),
	expandedConfigRule("trailer.cmd", "allowUnsafeCommandBinaries"),
	expandedConfigRule("trailer.command", "allowUnsafeCommandBinaries"),
	expandedConfigRule("url.insteadOf", "allowUnsafeUrlRewrite"),
}

// Vulnerabilities simple-git finds in a git argv, unsafe flags first then unsafe config writes
func argvVulnerabilities(tokens []string) []vulnerability {
	flags, taskIndex := parseGlobalFlags(tokens)
	var task *string
	var taskTokens []string
	if taskIndex < len(tokens) {
		t := jsLower(tokens[taskIndex])
		task = &t
		taskTokens = tokens[taskIndex+1:]
	}
	flags, positionals := parseTaskFlags(taskTokens, task, flags)
	var out []vulnerability
	for _, f := range flags {
		for _, rule := range unsafeFlagRules {
			if rule.task != "" && (task == nil || *task != rule.task) {
				continue
			}
			if (rule.globalOnly && !f.isGlobal) || (rule.withValue && f.value == nil) || !rule.match.MatchString(f.name) {
				continue
			}
			prefix := ""
			if rule.task != "" {
				prefix = rule.task + " with option "
			}
			out = append(out, vulnerability{rule.category, "Use of " + prefix + rule.name + " is not permitted without enabling " + rule.category})
		}
	}
	for _, w := range collectConfigWrites(task, flags, positionals) {
		for _, rule := range unsafeConfigRules {
			if rule.match.MatchString(w.key) {
				out = append(out, vulnerability{rule.category, "Configuring " + rule.name + " is not permitted without enabling " + rule.category})
			}
		}
	}
	return out
}

// First vulnerability of the argv whose category the review pane leaves blocked
func blockedArgv(tokens []string) *vulnerability {
	for _, v := range argvVulnerabilities(tokens) {
		if !enabledUnsafeCategories[v.category] {
			return &v
		}
	}
	return nil
}
