// Permission rules in the Tool(pattern) form with allow, deny and ask lists
package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/glob"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Kinds of subject a pattern is matched against
const (
	Plain   = "plain"
	Command = "command"
	Path    = "path"
	Domain  = "domain"
)

// One thing a call operates on, drawn from its arguments
type Subject = policy.Subject

// Draws subjects from the arguments of one tool
type Extractor func(args json.RawMessage) []Subject

func field(args json.RawMessage, name string) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return "", false
	}
	var s string
	if raw, ok := m[name]; ok && json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return "", false
}

// Subjects for a shell command argument, one per simple command plus the whole line
func Shell(name string) Extractor {
	return func(args json.RawMessage) []Subject {
		line, ok := field(args, name)
		if !ok {
			return nil
		}
		subjects := Split(line)
		if len(subjects) > 1 {
			subjects = append(subjects, Subject{Kind: Command, Text: strings.TrimSpace(line), Exact: true})
		}
		return subjects
	}
}

// Subjects for path arguments, a missing path meaning the working directory
func Paths(names ...string) Extractor {
	return func(args json.RawMessage) []Subject {
		var out []Subject
		for _, n := range names {
			p, ok := field(args, n)
			if !ok || p == "" {
				p = "."
			}
			out = append(out, Subject{Kind: Path, Text: p})
		}
		return out
	}
}

// Subject for a URL argument matched by domain or by the whole URL
func URLs(name string) Extractor {
	return func(args json.RawMessage) []Subject {
		u, ok := field(args, name)
		if !ok {
			return nil
		}
		return []Subject{{Kind: Domain, Text: u}}
	}
}

// Plain subjects for the named string arguments
func Strings(names ...string) Extractor {
	return func(args json.RawMessage) []Subject {
		var out []Subject
		for _, n := range names {
			if s, ok := field(args, n); ok {
				out = append(out, Subject{Kind: Plain, Text: s})
			}
		}
		return out
	}
}

// Plain subjects for every top level string argument in key order
func AllStrings(args json.RawMessage) []Subject {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Subject
	for _, k := range keys {
		var s string
		if json.Unmarshal(m[k], &s) == nil {
			out = append(out, Subject{Kind: Plain, Text: s})
		}
	}
	return out
}

// Extractors for the tools icebath ships, used for tools a policy names no extractor for
var DefaultSubjects = map[string]Extractor{
	"Bash":       Shell("command"),
	"BashOutput": Strings("bash_id"),
	"KillShell":  Strings("shell_id"),
	"Read":       Paths("file_path"),
	"Write":      Paths("file_path"),
	"Edit":       Paths("file_path"),
	"Glob":       Paths("path"),
	"Grep":       Paths("path"),
	"WebFetch":   URLs("url"),
}

// Splits a command line into its simple commands at the shell operators outside quotes
func Split(line string) []Subject {
	var out []Subject
	var cur strings.Builder
	quote := byte(0)
	unsafe := false
	flush := func() {
		s := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(cur.String()), "({ "))
		for (strings.HasSuffix(s, ")") && strings.Count(s, "(") < strings.Count(s, ")")) || (strings.HasSuffix(s, "}") && strings.Count(s, "{") < strings.Count(s, "}")) {
			s = strings.TrimSpace(s[:len(s)-1])
		}
		if s != "" {
			out = append(out, Subject{Kind: Command, Text: s, Unsafe: unsafe})
		}
		cur.Reset()
		unsafe = false
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		next := byte(0)
		if i+1 < len(line) {
			next = line[i+1]
		}
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
			cur.WriteByte(c)
		case quote == '"':
			switch {
			case c == '\\' && next != 0:
				cur.WriteByte(c)
				i++
				cur.WriteByte(next)
				continue
			case c == '"':
				quote = 0
			case c == '`' || (c == '$' && next == '('):
				unsafe = true
			}
			cur.WriteByte(c)
		case c == '\\' && next != 0:
			cur.WriteByte(c)
			i++
			cur.WriteByte(next)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == '`', c == '$' && next == '(', (c == '<' || c == '>') && next == '(':
			unsafe = true
			cur.WriteByte(c)
		case c == '\n' || c == ';':
			flush()
		case c == '&' && (next == '>' || (i > 0 && (line[i-1] == '>' || line[i-1] == '<'))):
			cur.WriteByte(c)
		case c == '&' || c == '|':
			if next == c || (c == '|' && next == '&') {
				i++
			}
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// Rule text split into its tool glob and optional pattern
type Rule struct {
	Tool       string
	Pattern    string
	HasPattern bool
}

// Parses Tool or Tool(pattern)
func Parse(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, errors.New("rules: empty rule")
	}
	open := strings.IndexByte(s, '(')
	if open < 0 {
		if strings.ContainsAny(s, ") ") {
			return Rule{}, fmt.Errorf("rules: malformed rule %q", s)
		}
		return Rule{Tool: s}, nil
	}
	if !strings.HasSuffix(s, ")") || open == 0 || strings.ContainsAny(s[:open], " \t") {
		return Rule{}, fmt.Errorf("rules: malformed rule %q", s)
	}
	return Rule{Tool: s[:open], Pattern: s[open+1 : len(s)-1], HasPattern: true}, nil
}

func (r Rule) String() string {
	if r.HasPattern {
		return r.Tool + "(" + r.Pattern + ")"
	}
	return r.Tool
}

// Lists of rules by decision, the shape of a permissions block
type Set struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
	Ask   []string `json:"ask"`
}

// Reads a permissions block from JSON, nested under permissions or at the top level
func Load(r io.Reader) (Set, error) {
	var doc struct {
		Set
		Permissions *Set `json:"permissions"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return Set{}, fmt.Errorf("rules: %w", err)
	}
	if doc.Permissions != nil {
		return *doc.Permissions, nil
	}
	return doc.Set, nil
}

func LoadFile(path string) (Set, error) {
	f, err := os.Open(path)
	if err != nil {
		return Set{}, err
	}
	defer f.Close()
	return Load(f)
}

// Writes a permissions block nested under permissions as settings files expect
func (s Set) Save(path string) error {
	data, err := json.MarshalIndent(struct {
		Permissions Set `json:"permissions"`
	}{s}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

type compiled struct {
	rule     Rule
	decision policy.Decision
}

// Policy answering deny for any denied subject, ask for any asked one and allow when every subject is allowed
type Policy struct {
	// Answer when no rule settles a call, Ask when empty
	Default policy.Decision
	// Extractors by tool name, DefaultSubjects when nil
	Subjects map[string]Extractor
	// Base of relative path patterns and paths, the process working directory when empty
	Dir string
	// Current directory consulted ahead of Dir on every match so a moving shell is followed
	DirFunc func() string
	// Expansion of a leading tilde, the user home directory when empty
	Home string
	// Called after every added or removed rule
	OnChange func()
	mu       sync.RWMutex
	rules    []compiled
}

// Compiles rule sets in order, failing on the first malformed rule
func New(sets ...Set) (*Policy, error) {
	p := &Policy{}
	for _, s := range sets {
		for _, group := range []struct {
			decision policy.Decision
			rules    []string
		}{{policy.Allow, s.Allow}, {policy.Deny, s.Deny}, {policy.Ask, s.Ask}} {
			for _, r := range group.rules {
				if err := p.add(group.decision, r); err != nil {
					return nil, err
				}
			}
		}
	}
	return p, nil
}

// Adds one rule under a decision
func (p *Policy) Add(decision policy.Decision, rule string) error {
	if err := p.add(decision, rule); err != nil {
		return err
	}
	p.changed()
	return nil
}

func (p *Policy) add(decision policy.Decision, rule string) error {
	if decision != policy.Allow && decision != policy.Deny && decision != policy.Ask {
		return fmt.Errorf("rules: unknown decision %q", decision)
	}
	r, err := Parse(rule)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.rules {
		if c.decision == decision && c.rule == r {
			return nil
		}
	}
	p.rules = append(p.rules, compiled{rule: r, decision: decision})
	return nil
}

// Drops one rule under a decision and reports whether it was present
func (p *Policy) Remove(decision policy.Decision, rule string) bool {
	r, err := Parse(rule)
	if err != nil {
		return false
	}
	p.mu.Lock()
	kept := p.rules[:0]
	removed := false
	for _, c := range p.rules {
		if c.decision == decision && c.rule == r {
			removed = true
			continue
		}
		kept = append(kept, c)
	}
	p.rules = kept
	p.mu.Unlock()
	if removed {
		p.changed()
	}
	return removed
}

// Drops every rule
func (p *Policy) Clear() {
	p.mu.Lock()
	p.rules = nil
	p.mu.Unlock()
	p.changed()
}

func (p *Policy) changed() {
	if p.OnChange != nil {
		p.OnChange()
	}
}

// Rules recorded under a decision in the order added
func (p *Policy) Rules(decision policy.Decision) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for _, c := range p.rules {
		if c.decision == decision {
			out = append(out, c.rule.String())
		}
	}
	return out
}

// Every rule as a set ready to save
func (p *Policy) Set() Set {
	return Set{Allow: p.Rules(policy.Allow), Deny: p.Rules(policy.Deny), Ask: p.Rules(policy.Ask)}
}

// Subjects the policy would match for a call
func (p *Policy) SubjectsOf(call protocol.ToolCall) []Subject {
	subjects := p.Subjects
	if subjects == nil {
		subjects = DefaultSubjects
	}
	if ex, ok := subjects[call.Name]; ok {
		return ex(call.Args)
	}
	return AllStrings(call.Args)
}

func (p *Policy) Decide(call protocol.ToolCall) policy.Decision {
	d, _ := p.Explain(call)
	return d
}

// Decision with the rule text that settled it or the word default
func (p *Policy) Explain(call protocol.ToolCall) (policy.Decision, string) {
	subjects := p.SubjectsOf(call)
	p.mu.RLock()
	defer p.mu.RUnlock()
	if r, ok := p.any(policy.Deny, call, subjects); ok {
		return policy.Deny, "rule " + r
	}
	if r, ok := p.any(policy.Ask, call, subjects); ok {
		return policy.Ask, "rule " + r
	}
	if r, ok := p.allows(call, subjects); ok {
		return policy.Allow, "rule " + r
	}
	if p.Default == "" {
		return policy.Ask, "default"
	}
	return p.Default, "default"
}

// Reports whether a rule of the decision covers the tool alone or any of its subjects, naming the rule
func (p *Policy) any(decision policy.Decision, call protocol.ToolCall, subjects []Subject) (string, bool) {
	for _, c := range p.rules {
		if c.decision != decision || !glob.Wild(c.rule.Tool, call.Name) {
			continue
		}
		if !c.rule.HasPattern {
			return c.rule.String(), true
		}
		for _, s := range subjects {
			if p.match(c.rule.Pattern, s) {
				return c.rule.String(), true
			}
		}
	}
	return "", false
}

// Allows on a tool wide rule, an exact whole line rule, or allow rules covering every subject
func (p *Policy) allows(call protocol.ToolCall, subjects []Subject) (string, bool) {
	var patterns []compiled
	for _, c := range p.rules {
		if c.decision != policy.Allow || !glob.Wild(c.rule.Tool, call.Name) {
			continue
		}
		if !c.rule.HasPattern {
			return c.rule.String(), true
		}
		patterns = append(patterns, c)
	}
	for _, s := range subjects {
		if !s.Exact {
			continue
		}
		for _, c := range patterns {
			if p.match(c.rule.Pattern, s) {
				return c.rule.String(), true
			}
		}
	}
	required := 0
	var used []string
	for _, s := range subjects {
		if s.Exact {
			continue
		}
		required++
		if s.Unsafe {
			return "", false
		}
		covered := ""
		for _, c := range patterns {
			if p.match(c.rule.Pattern, s) {
				covered = c.rule.String()
				break
			}
		}
		if covered == "" {
			return "", false
		}
		used = append(used, covered)
	}
	if required == 0 {
		return "", false
	}
	return strings.Join(dedupe(used), ", "), true
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Matches a pattern against one subject by the rules of its kind
func (p *Policy) match(pattern string, s Subject) bool {
	if s.Exact {
		return !strings.ContainsAny(pattern, "*?") && pattern == s.Text
	}
	switch s.Kind {
	case Command:
		if base, ok := strings.CutSuffix(pattern, ":*"); ok {
			return s.Text == base || strings.HasPrefix(s.Text, base+" ")
		}
		return glob.Wild(pattern, s.Text)
	case Path:
		sub, err := p.resolve(s.Text)
		if err != nil {
			return false
		}
		if pattern == "**" || strings.HasPrefix(pattern, "**/") {
			return glob.Match(pattern, sub)
		}
		pat, err := p.resolve(pattern)
		if err != nil {
			return false
		}
		return glob.Match(pat, sub) || strings.HasPrefix(sub, pat+"/")
	case Domain:
		if domain, ok := strings.CutPrefix(pattern, "domain:"); ok {
			u, err := url.Parse(s.Text)
			if err != nil {
				return false
			}
			host := strings.ToLower(u.Hostname())
			domain = strings.ToLower(domain)
			return host == domain || strings.HasSuffix(host, "."+domain)
		}
		return glob.Wild(pattern, s.Text)
	}
	return glob.Wild(pattern, s.Text)
}

func (p *Policy) dir() (string, error) {
	if p.DirFunc != nil {
		if d := p.DirFunc(); d != "" {
			return d, nil
		}
	}
	if p.Dir != "" {
		return p.Dir, nil
	}
	return os.Getwd()
}

// Turns a path or path pattern into an absolute slash separated form
func (p *Policy) resolve(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home := p.Home
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			home = h
		}
		path = filepath.Join(home, path[1:])
	}
	if !filepath.IsAbs(path) {
		dir, err := p.dir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(dir, path)
	}
	return filepath.ToSlash(filepath.Clean(path)), nil
}

// Rule text that would allow exactly this call again, for remembering answers
func (p *Policy) Key(call protocol.ToolCall) string {
	subjects := p.SubjectsOf(call)
	var plain []Subject
	for _, s := range subjects {
		if s.Exact {
			return Rule{Tool: call.Name, Pattern: s.Text, HasPattern: true}.String()
		}
		plain = append(plain, s)
	}
	if len(plain) != 1 {
		return call.Name
	}
	s := plain[0]
	switch s.Kind {
	case Domain:
		if u, err := url.Parse(s.Text); err == nil && u.Hostname() != "" {
			return Rule{Tool: call.Name, Pattern: "domain:" + strings.ToLower(u.Hostname()), HasPattern: true}.String()
		}
	case Path:
		if abs, err := p.resolve(s.Text); err == nil {
			return Rule{Tool: call.Name, Pattern: abs, HasPattern: true}.String()
		}
	}
	return Rule{Tool: call.Name, Pattern: s.Text, HasPattern: true}.String()
}
