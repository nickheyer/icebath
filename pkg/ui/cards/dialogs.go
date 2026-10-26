package cards

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Choice standing for a profile that starts from no provider
const customProvider = "custom"

// Choice standing for no vendor on a custom endpoint
const noneChoice = "none"

// Choice standing for the built in base prompt
const builtInChoice = "built in"

// Choice of where a dialog writes a new entry, with what each place writes
func scopeField(what string) *field {
	var choices []string
	for _, s := range harness.Scopes {
		choices = append(choices, string(s))
	}
	return &field{key: "scope", label: "scope", kind: choiceField, choices: choices, value: choices[0], help: "project writes .icebath/" + what + " here, user writes " + what + " in the config directory"}
}

// Value of a choice field, empty for the none choice
func chosen(f *field) string {
	if f.value == noneChoice {
		return ""
	}
	return f.value
}

// Value unless it equals what the provider gives, so the provider keeps filling it
func override(value, base string) string {
	if value == base {
		return ""
	}
	return value
}

// Dialog that writes a profile, new from a provider or over a written one
func (c *Config) profileForm(old *harness.Instance, provider string) *Form {
	var names []string
	for _, p := range profiles.Defaults() {
		names = append(names, p.Name)
	}
	names = append(names, customProvider)
	var written profiles.Profile
	if old != nil {
		written = old.Written.Clone()
		provider = or(written.Template, customProvider)
	}
	if provider == "" {
		provider = names[0]
	}
	base := func() profiles.Profile {
		p, _ := profiles.Lookup(provider)
		return p
	}
	var protocols []string
	for _, id := range protocol.IDs() {
		protocols = append(protocols, string(id))
	}
	var auths []string
	for _, k := range profiles.AuthKinds() {
		auths = append(auths, string(k))
	}
	sources := []string{string(profiles.SourceEnv), string(profiles.SourceStatic), string(profiles.SourceFile), string(profiles.SourceExec), string(profiles.SourceNone)}

	pick := &field{key: "provider", label: "provider", group: "profile", kind: choiceField, choices: names, value: provider, help: "Provider that fills every field left as it is, custom for any other endpoint"}
	name := &field{key: "name", label: "name", group: "profile", kind: textField, value: written.Name, help: "Letters, digits, dots, dashes and underscores", check: func(v string) error {
		if !profiles.ValidName(v) {
			return errors.New("name must start with a letter or digit and hold only letters, digits, dots, dashes and underscores")
		}
		return nil
	}}
	model := &field{key: "model", label: "model", group: "profile", kind: textField, value: written.Model(), help: "Default model, empty takes the provider default"}
	endpoint := &field{key: "endpoint", label: "endpoint", group: "endpoint", kind: textField, help: "Base URL such as https://api.example.com/v1"}
	vendor := &field{key: "vendor", label: "vendor", group: "endpoint", kind: choiceField, help: "Model family, used to list models"}
	proto := &field{key: "protocol", label: "protocol", group: "endpoint", kind: choiceField, choices: protocols, help: "Wire format of the endpoint"}
	auth := &field{key: "auth", label: "auth", group: "credential", kind: choiceField, choices: auths, help: "How the credential is sent, none for no credential"}
	authName := &field{key: "auth_name", label: "auth name", group: "credential", kind: textField, help: "Header or query parameter such as x-api-key", show: func() bool {
		return auth.value == string(profiles.AuthHeader) || auth.value == string(profiles.AuthQuery)
	}}
	keyFrom := &field{key: "key_from", label: "key from", group: "credential", kind: choiceField, choices: sources, help: "Credential source, read each time the profile opens"}
	key := &field{key: "key", label: "key", group: "credential", kind: textField, show: func() bool { return keyFrom.value != string(profiles.SourceNone) }}
	promptFrom := &field{key: "system_from", label: "prompt", group: "prompt", kind: choiceField, choices: append([]string{builtInChoice}, sources...), help: "Base prompt sent in place of the built in one, none sending no prompt at all"}
	system := &field{key: "system", label: "text", group: "prompt", kind: textField, show: func() bool {
		return promptFrom.value != builtInChoice && promptFrom.value != string(profiles.SourceNone)
	}}
	environment := &field{key: "context", label: "context", group: "prompt", kind: choiceField, choices: []string{"true", "false"}, value: strconv.FormatBool(!written.NoContext), help: "Sends the environment block about the machine and session after the prompt"}
	bare := &field{key: "bare", label: "bare", group: "behavior", kind: choiceField, choices: []string{"true", "false"}, value: strconv.FormatBool(written.Bare), help: "Asks the endpoint to add no prompt or tools of its own"}
	ghost := &field{key: "ghost", label: "ghost", group: "behavior", kind: choiceField, choices: []string{"true", "false"}, value: strconv.FormatBool(written.Ghost), help: "Keeps sessions off disk and asks the endpoint to keep nothing of them"}
	thinking := &field{key: "thinking", label: "thinking", group: "behavior", kind: choiceField, choices: []string{"true", "false"}, help: "Shows the model's thinking in the transcript, false keeps every thinking event out of the window"}
	enabled := &field{key: "enabled", label: "enabled", group: "behavior", kind: choiceField, choices: []string{"true", "false"}, value: strconv.FormatBool(!written.Disabled), help: "Off profiles stay out of pickers"}
	describeKey := func() {
		key.kind = textField
		switch profiles.SourceKind(keyFrom.value) {
		case profiles.SourceEnv:
			key.label, key.help = "variable", "Environment variable holding the credential"
		case profiles.SourceStatic:
			key.kind = secretField
			key.label, key.help = "key", "The credential itself, kept in the profile file"
		case profiles.SourceFile:
			key.label, key.help = "path", "File holding the credential"
		case profiles.SourceExec:
			key.label, key.help = "command", "Command that prints the credential"
		}
	}
	keyFrom.onChange = describeKey
	describePrompt := func() {
		system.kind = textField
		switch profiles.SourceKind(promptFrom.value) {
		case profiles.SourceEnv:
			system.label, system.help = "variable", "Environment variable holding the prompt"
		case profiles.SourceStatic:
			system.kind = areaField
			system.label, system.help = "text", "The prompt itself, kept in the profile file"
		case profiles.SourceFile:
			system.label, system.help = "path", "File holding the prompt"
		case profiles.SourceExec:
			system.label, system.help = "command", "Command that prints the prompt"
		}
	}
	promptFrom.onChange = describePrompt
	fill := func(b profiles.Profile, from profiles.Profile) {
		endpoint.value = or(from.BaseURL, b.BaseURL)
		endpoint.optional = provider != customProvider
		model.placeholder = b.Model()
		model.optional = provider != customProvider
		endpoint.placeholder = b.BaseURL
		vendor.choices = profiles.Vendors()
		if provider == customProvider {
			vendor.choices = append(vendor.choices, noneChoice)
		}
		vendor.value = or(or(from.Vendor, b.Vendor), vendor.choices[len(vendor.choices)-1])
		proto.value = or(string(from.Protocol), string(b.Protocol))
		if proto.value == "" && len(protocols) > 0 {
			proto.value = protocols[0]
		}
		auth.value, authName.value = string(b.Auth), b.AuthName
		if from.Auth != "" {
			auth.value, authName.value = string(from.Auth), from.AuthName
		}
		auth.value = or(auth.value, string(profiles.AuthNone))
		k := b.Key
		if from.Key.Kind != "" {
			k = from.Key
		}
		keyFrom.value, key.value = or(string(k.Kind), string(profiles.SourceNone)), k.Value
		s := b.System
		if from.System.Kind != "" {
			s = from.System
		}
		promptFrom.value, system.value = or(string(s.Kind), builtInChoice), s.Value
		thinking.value = strconv.FormatBool(b.ShowsThinking())
		if from.Thinking != nil {
			thinking.value = strconv.FormatBool(*from.Thinking)
		}
		for _, f := range []*field{endpoint, model, authName, key, system} {
			f.cursor = len([]rune(f.value))
		}
		describeKey()
		describePrompt()
	}
	fill(base(), written)
	pick.onChange = func() {
		provider = pick.value
		fill(base(), profiles.Profile{})
	}
	fields := []*field{pick, name, model, endpoint, vendor, proto, auth, authName, keyFrom, key, promptFrom, system, environment, bare, ghost, thinking, enabled}
	title := "NEW PROFILE"
	oldName := ""
	if old != nil {
		title, oldName = "EDIT "+old.Profile.Name, old.Profile.Name
	}
	return newForm(c.env, title, c.env.Theme.Glyphs.Profiles, fields, func() (tea.Cmd, error) {
		b := base()
		p := written.Clone()
		p.Name = strings.TrimSpace(name.value)
		p.Template = ""
		if provider != customProvider {
			p.Template = provider
		}
		p.Disabled = enabled.value == "false"
		m := strings.TrimSpace(model.value)
		rest := slices.DeleteFunc(slices.Clone(written.Models), func(id string) bool { return id == m || id == written.Model() })
		p.Models = nil
		if m != "" && !(p.Template != "" && m == b.Model() && len(rest) == 0) {
			p.Models = append([]string{m}, rest...)
		}
		p.BaseURL = override(strings.TrimSpace(endpoint.value), b.BaseURL)
		p.Vendor = override(chosen(vendor), b.Vendor)
		p.Protocol = protocol.ID(override(proto.value, string(b.Protocol)))
		p.Auth, p.AuthName = profiles.AuthKind(auth.value), ""
		if authName.visible() {
			p.AuthName = strings.TrimSpace(authName.value)
		}
		if p.Auth == b.Auth && p.AuthName == b.AuthName {
			p.Auth, p.AuthName = "", ""
		}
		p.Key = profiles.Source{Kind: profiles.SourceKind(keyFrom.value), Value: strings.TrimSpace(key.value)}
		if p.Key.Kind == profiles.SourceNone {
			p.Key.Value = ""
		}
		if p.Key == b.Key || (p.Key.Kind == profiles.SourceNone && b.Key.Kind == "") {
			p.Key = profiles.Source{}
		}
		p.System = profiles.Source{}
		if promptFrom.value != builtInChoice {
			p.System = profiles.Source{Kind: profiles.SourceKind(promptFrom.value), Value: strings.TrimSpace(system.value)}
		}
		if p.System.Kind == profiles.SourceNone {
			p.System.Value = ""
		}
		if p.System == b.System {
			p.System = profiles.Source{}
		}
		p.NoContext = environment.value == "false"
		p.Bare = bare.value == "true"
		p.Ghost = ghost.value == "true"
		p.Thinking = nil
		if shown := thinking.value == "true"; shown != b.ShowsThinking() {
			p.Thinking = &shown
		}
		if err := c.env.Editor.SaveProfile(oldName, p); err != nil {
			return nil, err
		}
		c.profiles.refresh()
		cmds := []tea.Cmd{wm.Notify("saved profile "+p.Name, wm.Success), c.checkCredentials()}
		if oldName != "" && oldName == c.current() {
			cmds = append(cmds, func() tea.Msg { return SetProfileMsg{Profile: p.Name} })
		}
		return tea.Batch(cmds...), nil
	})
}

// Tool and pattern of a rule, the pattern empty when the rule names none
func splitRule(text string) (string, string) {
	r, err := policy.ParseRule(text)
	if err != nil {
		return text, ""
	}
	return r.Tool, r.Pattern
}

// What a pattern matches for the tool, by the subjects the policy draws from its arguments
func patternHelp(tool string) string {
	switch tool {
	case "Bash":
		return "Command glob such as git * or prefix such as npm run:*, empty matches every command"
	case "Read", "Write", "Edit", "Glob", "Grep":
		return "Path glob such as src/** or ~/.ssh/**, empty matches every path"
	case "WebFetch":
		return "URL glob such as https://docs.example.com/* or domain:example.com, empty matches every url"
	case "BashOutput", "KillShell":
		return "Job id glob, empty matches every job"
	}
	return "Glob over every string argument, empty matches every call"
}

// Dialog that writes a permission rule, new into a scope or over one in the file holding it
func (c *Config) ruleForm(old *harness.RuleEntry) *Form {
	const other = "other"
	tools := []string{"*"}
	if c.env.Tools != nil {
		names := c.env.Tools.Names()
		slices.Sort(names)
		tools = append(tools, names...)
	}
	tools = append(tools, other)
	d, tl, pattern := string(policy.Allow), "*", ""
	if old != nil {
		d = string(old.Decision)
		tl, pattern = splitRule(old.Rule)
	}
	glob := ""
	if !slices.Contains(tools, tl) {
		tl, glob = other, tl
	}
	decision := &field{key: "decision", label: "decision", kind: choiceField, choices: []string{string(policy.Allow), string(policy.Ask), string(policy.Deny)}, value: d, help: "deny wins over ask, ask over allow"}
	pat := &field{key: "pattern", label: "pattern", kind: textField, optional: true, placeholder: "every call", value: pattern, cursor: len([]rune(pattern))}
	toolField := &field{key: "tool", label: "tool", kind: choiceField, choices: tools, value: tl, help: "* for every tool, other for a name glob such as mcp__github__*"}
	globField := &field{key: "glob", label: "tool glob", kind: textField, value: glob, cursor: len([]rune(glob)), help: "Glob over tool names such as mcp__github__*", show: func() bool { return toolField.value == other }}
	toolField.onChange = func() { pat.help = patternHelp(toolField.value) }
	toolField.onChange()
	fields := []*field{decision, toolField, globField, pat}
	scope := scopeField(harness.RulesFile)
	title := "NEW RULE"
	if old == nil {
		fields = append(fields, scope)
	} else {
		title = "EDIT RULE"
	}
	return newForm(c.env, title, c.env.Theme.Glyphs.Rules, fields, func() (tea.Cmd, error) {
		name := toolField.value
		if name == other {
			name = strings.TrimSpace(globField.value)
		}
		text := name
		if p := strings.TrimSpace(pat.value); p != "" {
			text = name + "(" + p + ")"
		}
		var err error
		if old == nil {
			err = c.env.Editor.AddRule(harness.Scope(scope.value), policy.Decision(decision.value), text)
		} else {
			err = c.env.Editor.ReplaceRule(*old, policy.Decision(decision.value), text)
		}
		if err != nil {
			return nil, err
		}
		return tea.Batch(c.loadRules(), wm.Notify("saved "+decision.value+" "+text, wm.Success)), nil
	})
}

// Dialog that writes a hook, new into a scope or over one in the file holding it
func (c *Config) hookForm(old *harness.HookEntry) *Form {
	var events []string
	for _, ev := range agent.HookEvents {
		events = append(events, string(ev))
	}
	e := harness.HookEntry{Event: agent.PreToolUse}
	if old != nil {
		e = *old
	}
	timeout := ""
	if e.Timeout > 0 {
		timeout = strconv.Itoa(e.Timeout)
	}
	event := &field{key: "event", label: "event", kind: choiceField, choices: events, value: string(e.Event), help: "PreToolUse can block a call, UserPromptSubmit can block a prompt"}
	tooled := func() bool {
		return event.value == string(agent.PreToolUse) || event.value == string(agent.PostToolUse)
	}
	matcher := &field{key: "matcher", label: "matcher", kind: textField, optional: true, placeholder: "every tool", value: e.Matcher, cursor: len([]rune(e.Matcher)), show: tooled, help: "Regular expression over the tool name such as Edit|Write", check: func(v string) error {
		return harness.CheckMatcher(v)
	}}
	command := &field{key: "command", label: "command", kind: textField, value: e.Command, cursor: len([]rune(e.Command)), help: "Shell command given the event as JSON on stdin, exit 2 blocks with stderr as the reason"}
	secs := &field{key: "timeout", label: "timeout", kind: numberField, integer: true, optional: true, lo: 1, step: 5, start: 60, value: timeout, unit: func(v float64) string { return strconv.Itoa(int(v)) + "s" }, help: "Seconds the command may run, unset means forever"}
	fields := []*field{event, matcher, command, secs}
	scope := scopeField(harness.HooksFile)
	title := "NEW HOOK"
	if old == nil {
		fields = append(fields, scope)
	} else {
		title = "EDIT HOOK"
	}
	return newForm(c.env, title, c.env.Theme.Glyphs.Hooks, fields, func() (tea.Cmd, error) {
		next := harness.HookEntry{Event: agent.HookEvent(event.value), Command: strings.TrimSpace(command.value)}
		if tooled() {
			next.Matcher = strings.TrimSpace(matcher.value)
		}
		if v, ok := secs.number(); ok {
			next.Timeout = int(v)
		}
		var err error
		if old == nil {
			err = c.env.Editor.AddHook(harness.Scope(scope.value), next)
		} else {
			err = c.env.Editor.ReplaceHook(*old, next)
		}
		if err != nil {
			return nil, err
		}
		return tea.Batch(c.loadHooks(), wm.Notify("saved the "+event.value+" hook", wm.Success)), nil
	})
}

// Words of a command line, double or single quotes keeping spaces and a backslash escaping the next character
func splitArgs(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	started, escaped := false, false
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, started = true, true
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, started = r, true
		case r == ' ' || r == '\t':
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed %c quote", quote)
	}
	if started {
		out = append(out, cur.String())
	}
	return out, nil
}

// Command line of words, quoting any that holds a space, a quote or a backslash
func joinArgs(args []string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'\\") {
			a = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}

// KEY=VALUE words of an environment, sorted by key
func joinEnv(env map[string]string) string {
	var words []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		words = append(words, k+"="+env[k])
	}
	return joinArgs(words)
}

// Environment from KEY=VALUE words
func splitEnv(line string) (map[string]string, error) {
	words, err := splitArgs(line)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q is not KEY=VALUE", w)
		}
		out[k] = v
	}
	return out, nil
}

// Name and value pairs separated by semicolons, sorted by name
func joinHeaders(h map[string]string) string {
	var pairs []string
	for _, k := range slices.Sorted(maps.Keys(h)) {
		pairs = append(pairs, k+": "+h[k])
	}
	return strings.Join(pairs, "; ")
}

// Headers from name and value pairs separated by semicolons
func splitHeaders(line string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(line, ";") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%q is not Name: value", strings.TrimSpace(pair))
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// Dialog that writes an MCP server, new into a scope or over one in the file configuring it
func (c *Config) serverForm(old *harness.ServerConfig) *Form {
	var cfg harness.ServerConfig
	if old != nil {
		cfg = *old
	}
	kind := "stdio"
	if cfg.URL != "" {
		kind = "http"
	}
	text := func(key, label, group, value, help string, optional bool, show func() bool) *field {
		return &field{key: key, label: label, group: group, kind: textField, value: value, cursor: len([]rune(value)), help: help, optional: optional, show: show}
	}
	transport := &field{key: "transport", label: "transport", group: "server", kind: choiceField, choices: []string{"stdio", "http"}, value: kind, help: "stdio runs a program, http talks to a streamable HTTP endpoint"}
	stdio := func() bool { return transport.value == "stdio" }
	web := func() bool { return transport.value == "http" }
	timeout := ""
	if cfg.Timeout > 0 {
		timeout = strconv.Itoa(cfg.Timeout)
	}
	name := text("name", "name", "server", cfg.Name, "Tools are named mcp__name__tool", false, nil)
	command := text("command", "command", "process", cfg.Command, "Program that speaks MCP such as npx or uvx", false, stdio)
	args := text("args", "args", "process", joinArgs(cfg.Args), "Program arguments, quotes keep spaces", true, stdio)
	args.check = func(v string) error { _, err := splitArgs(v); return err }
	env := text("env", "env", "process", joinEnv(cfg.Env), "KEY=VALUE words such as TOKEN=abc DEBUG=1", true, stdio)
	env.check = func(v string) error { _, err := splitEnv(v); return err }
	dir := text("dir", "dir", "process", cfg.Dir, "Working directory, empty for the icebath directory", true, stdio)
	url := text("url", "url", "endpoint", cfg.URL, "Endpoint such as https://mcp.example.com/mcp", false, web)
	headers := text("headers", "headers", "endpoint", joinHeaders(cfg.Headers), "Name: value pairs separated by semicolons", true, web)
	headers.check = func(v string) error { _, err := splitHeaders(v); return err }
	secs := &field{key: "timeout", label: "timeout", group: "options", kind: numberField, integer: true, optional: true, lo: 1, step: 5, start: 30, value: timeout, unit: func(v float64) string { return strconv.Itoa(int(v)) + "s" }, help: "Seconds per handshake and listing, unset means forever"}
	enabled := &field{key: "enabled", label: "enabled", group: "options", kind: choiceField, choices: []string{"true", "false"}, value: strconv.FormatBool(!cfg.Disabled), help: "Off servers stay configured without connecting"}
	fields := []*field{name, transport, command, args, env, dir, url, headers, secs, enabled}
	scope := scopeField(harness.ConfigFile)
	scope.group = "options"
	title, oldName := "NEW MCP SERVER", ""
	if old == nil {
		fields = append(fields, scope)
	} else {
		title, oldName = "EDIT "+old.Name, old.Name
	}
	return newForm(c.env, title, c.env.Theme.Glyphs.MCP, fields, func() (tea.Cmd, error) {
		next := harness.ServerConfig{Name: strings.TrimSpace(name.value), Disabled: enabled.value == "false"}
		if v, ok := secs.number(); ok {
			next.Timeout = int(v)
		}
		if stdio() {
			next.Command = strings.TrimSpace(command.value)
			next.Args, _ = splitArgs(args.value)
			next.Dir = strings.TrimSpace(dir.value)
			if vars, _ := splitEnv(env.value); len(vars) > 0 {
				next.Env = vars
			}
		} else {
			next.URL = strings.TrimSpace(url.value)
			if h, _ := splitHeaders(headers.value); len(h) > 0 {
				next.Headers = h
			}
		}
		if err := c.env.Editor.SaveServer(harness.Scope(scope.value), oldName, next); err != nil {
			return nil, err
		}
		restart := background(func() error { return c.env.Editor.RestartServer(c.env.Ctx, oldName, next) })
		return tea.Batch(c.loadServers(), restart, wm.Notify("saved mcp server "+next.Name, wm.Success)), nil
	})
}
