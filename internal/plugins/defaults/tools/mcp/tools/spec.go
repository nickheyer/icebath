package tools

import (
	"cmp"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

const (
	// Seconds the first connection to a server may take
	defaultConnect = 60.0
	// Keepalive of remote servers, and the least one a setting may ask
	defaultKeepalive = 180 * time.Second
	minKeepalive     = 5 * time.Second
)

// Trust a server's settings may give it: full runs its tools as they are, untrusted asks before each tool that may write
const (
	TrustFull      = "full"
	TrustUntrusted = "untrusted"
)

// Settings of one server, its ${NAME} references rendered
type spec struct {
	Name string
	// Settings as kept, before rendering
	Raw map[string]any
	// Plugin that ships it, empty for one the profile sets up
	Plugin string

	Command   string
	Args      []string
	Env       map[string]string
	Cwd       string
	URL       string
	Headers   map[string]string
	Transport string
	Enabled   bool
	// Longest one tool call may run, the setting's when the server sets none
	Timeout        time.Duration
	ConnectTimeout time.Duration
	// Probe interval of the session, zero for none: remote servers and those that set one
	Keepalive   time.Duration
	Lazy        bool
	IdleTimeout time.Duration
	MaxLifetime time.Duration
	// Tools turned on: only those include names when it is set, else all but those exclude names; exact names or globs
	Include, Exclude []string
	Included         bool
	Resources        bool
	Prompts          bool
	Trust            string
	Costs            *costPolicy
	CostErr          error
	Auth             string
	OAuth            mcp.OAuthConfig
	OAuthErr         error
	Sampling         samplingConfig
	Elicitation      elicitConfig
	Events           bool
	Protocol         string
	SkipPreflight    bool
	SSLVerify        any
	ClientCert       any
	ClientKey        any
	Strict           bool
	Identity         [2]string
	Parallel         bool
	// References that rendered to nothing, by where they sit, and env names that were one such reference alone
	Missed map[string]bool
	Unset  map[string]bool
}

type samplingConfig struct {
	Enabled bool
	// Model every request runs on, else the first a request hints
	Model         string
	Allowed       []string
	MaxRPM        int
	Timeout       time.Duration
	MaxTokens     int
	MaxToolRounds int
	Tools         bool
	LogLevel      slog.Level
}

type elicitConfig struct {
	Enabled bool
	Timeout time.Duration
}

// Whether a server reaches over HTTP rather than a child process
func (s *spec) remote() bool { return s.URL != "" || s.Raw["url"] != nil }

func (s *spec) transportName() string {
	if !s.remote() {
		return "stdio"
	}
	return cmp.Or(s.Transport, "http")
}

var (
	truthy = map[string]bool{"true": true, "1": true, "yes": true, "on": true}
	falsy  = map[string]bool{"false": true, "0": true, "no": true, "off": true}
)

// Boolean of a setting, numbers by their value, words from a fixed set, the default otherwise
func boolish(v any, def bool) bool {
	switch b := v.(type) {
	case nil:
		return def
	case bool:
		return b
	case int, int64, float64:
		return cast.ToFloat64(b) != 0
	case string:
		word := strings.ToLower(strings.TrimSpace(b))
		if truthy[word] {
			return true
		}
		if falsy[word] {
			return false
		}
	}
	slog.Warn("MCP setting expected true or false, using the default", "value", v, "default", def)
	return def
}

// Seconds of a setting, false when it is none or not a number
func seconds(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	f, err := cast.ToFloat64E(v)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func duration(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func stringList(v any) []string {
	switch list := v.(type) {
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case []string:
		return slices.Clone(list)
	case string:
		return []string{list}
	}
	return nil
}

func table(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

var reference = regexp.MustCompile(`\$\{([^}]+)\}`)

// Renders ${NAME} references of every text of a value: context names first, then the process environment, then the profile's MCP values.
// Missed references stay as written, recorded by where they sit
type renderer struct {
	config    *config.Config
	values    map[string]any
	workspace string
	missed    map[string]bool
	unset     map[string]bool
}

func newRenderer(cfg *config.Config, workspace string, extra map[string]string) *renderer {
	values := map[string]any{}
	for k, v := range table(cfg.Get(mcpconfig.Env)) {
		values[strings.ToLower(k)] = v
	}
	for k, v := range extra {
		values[strings.ToLower(k)] = v
	}
	if workspace == "" {
		workspace, _ = os.Getwd()
	}
	return &renderer{config: cfg, values: values, workspace: workspace, missed: map[string]bool{}, unset: map[string]bool{}}
}

// Value of a reference body, false when nothing gives it one
func (r *renderer) lookup(body string) (string, bool) {
	name := strings.TrimSpace(body)
	switch name {
	case "userHome":
		return terminal.ExpandUser("~"), true
	case "workspaceFolder":
		return r.workspace, true
	case "workspaceFolderBasename":
		return filepath.Base(r.workspace), true
	case "pathSeparator", "/":
		return string(os.PathSeparator), true
	}
	name = strings.TrimSpace(strings.TrimPrefix(name, "env:"))
	if v, ok := r.config.LookupEnv(name); ok && v != "" {
		return v, true
	}
	if v, ok := r.values[strings.ToLower(name)]; ok && fmt.Sprint(v) != "" {
		return fmt.Sprint(v), true
	}
	return "", false
}

func (r *renderer) render(v any, path string) any {
	switch x := v.(type) {
	case string:
		missed := 0
		out := reference.ReplaceAllStringFunc(x, func(raw string) string {
			if value, ok := r.lookup(raw[2 : len(raw)-1]); ok {
				return value
			}
			missed++
			return raw
		})
		if missed > 0 {
			r.missed[path] = true
			if missed == 1 && reference.FindString(x) == x {
				r.unset[path] = true
			}
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = r.render(item, path+"."+k)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = r.render(item, path+"["+strconv.Itoa(i)+"]")
		}
		return out
	}
	return v
}

// Text of every ${NAME} of a value, rendered with the profile's values
func (r *renderer) text(s string) string {
	return r.render(s, "").(string)
}

// Env of a server: a list of NAME=value, or a table whose names the settings file kept in lower case
func envOf(v any) map[string]string {
	env := map[string]string{}
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if name, value, ok := strings.Cut(fmt.Sprint(item), "="); ok && strings.TrimSpace(name) != "" {
				env[strings.TrimSpace(name)] = value
			}
		}
	case map[string]any:
		for name, value := range x {
			env[strings.ToUpper(name)] = fmt.Sprint(value)
		}
	}
	return env
}

// Typed settings of one server, rendered
func parseSpec(cfg *config.Config, name string, raw map[string]any, workspace string) *spec {
	return parseSpecWith(cfg, name, raw, workspace, nil)
}

// Typed settings of one server, rendered with values given over the profile's own, such as those an install is about to keep
func parseSpecWith(cfg *config.Config, name string, raw map[string]any, workspace string, extra map[string]string) *spec {
	r := newRenderer(cfg, workspace, extra)
	rendered := r.render(raw, "").(map[string]any)
	s := &spec{Name: name, Raw: raw, Enabled: boolish(rendered["enabled"], true), Missed: r.missed, Unset: map[string]bool{}}
	s.Command = strings.TrimSpace(fmt.Sprint(orNil(rendered["command"])))
	s.Args = stringList(rendered["args"])
	s.Env = envOf(rendered["env"])
	for path := range r.unset {
		if rest, ok := strings.CutPrefix(path, ".env"); ok {
			// The settings file keeps env in lower case, a list keeps NAME=value: both point at the name
			if i := strings.Index(rest, "["); i == 0 {
				if at, err := strconv.Atoi(strings.Trim(rest, "[]")); err == nil {
					if list, ok := raw["env"].([]any); ok && at < len(list) {
						key, _, _ := strings.Cut(fmt.Sprint(list[at]), "=")
						s.Unset[strings.TrimSpace(key)] = true
					}
				}
			} else {
				s.Unset[strings.ToUpper(strings.TrimPrefix(rest, "."))] = true
			}
		}
	}
	s.Cwd = terminal.ExpandUser(fmt.Sprint(orNil(rendered["cwd"])))
	if rendered["url"] != nil {
		s.URL = strings.TrimSpace(fmt.Sprint(rendered["url"]))
	}
	s.Headers = map[string]string{}
	for k, v := range table(rendered["headers"]) {
		s.Headers[k] = fmt.Sprint(v)
	}
	s.Transport = strings.ToLower(strings.TrimSpace(fmt.Sprint(orNil(rendered["transport"]))))
	if f, ok := seconds(rendered["timeout"]); ok && f > 0 {
		s.Timeout = duration(f)
	} else if rendered["timeout"] != nil {
		slog.Warn("MCP timeout must be a number of seconds, ignoring it", "server", name, "value", rendered["timeout"])
	}
	s.ConnectTimeout = duration(defaultConnect)
	if f, ok := seconds(rendered["connect_timeout"]); ok && f > 0 {
		s.ConnectTimeout = duration(f)
	}
	if f, ok := seconds(rendered["keepalive_interval"]); ok {
		s.Keepalive = max(minKeepalive, duration(f))
	} else if s.remote() {
		s.Keepalive = defaultKeepalive
	}
	s.Lazy = boolish(rendered["lazy"], false)
	lifecycle := table(rendered["lifecycle"])
	limit := func(key string) time.Duration {
		v := rendered[key]
		if v == nil {
			v = lifecycle[key]
		}
		f, ok := seconds(v)
		if v != nil && (!ok || f < 0) {
			slog.Warn("MCP lifecycle limit must be a positive number of seconds, ignoring it", "server", name, "setting", key, "value", v)
			return 0
		}
		return duration(f)
	}
	s.IdleTimeout, s.MaxLifetime = limit("idle_timeout_seconds"), limit("max_lifetime_seconds")
	filter := table(rendered["tools"])
	_, s.Included = filter["include"].([]any)
	if text, ok := filter["include"].(string); ok {
		s.Included, s.Include = true, []string{text}
	} else {
		s.Include = stringList(filter["include"])
	}
	s.Exclude = stringList(filter["exclude"])
	s.Resources, s.Prompts = boolish(filter["resources"], true), boolish(filter["prompts"], true)
	s.Trust = TrustFull
	if v := rendered["trust"]; v != nil {
		switch word := strings.ToLower(strings.TrimSpace(fmt.Sprint(v))); word {
		case TrustFull, TrustUntrusted:
			s.Trust = word
		default:
			slog.Warn("MCP trust value unknown, treating the server as untrusted (valid: full, untrusted)", "server", name, "value", v)
			s.Trust = TrustUntrusted
		}
	}
	s.Costs, s.CostErr = parseCosts(name, rendered)
	s.Auth = strings.ToLower(strings.TrimSpace(fmt.Sprint(orNil(rendered["auth"]))))
	s.OAuth, s.OAuthErr = mcp.ParseOAuthConfig(table(rendered["oauth"]))
	sampling := table(rendered["sampling"])
	s.Sampling = samplingConfig{Enabled: boolish(sampling["enabled"], true), Model: fmt.Sprint(orNil(sampling["model"])), Allowed: stringList(sampling["allowed_models"]),
		MaxRPM: atLeast(sampling["max_rpm"], 10, 1), Timeout: duration(atLeastF(sampling["timeout"], 30, 1)), MaxTokens: atLeast(sampling["max_tokens_cap"], 4096, 1),
		MaxToolRounds: atLeast(sampling["max_tool_rounds"], 5, 0), Tools: boolish(sampling["expose_client_tools"], false), LogLevel: logLevel(sampling["log_level"])}
	elicit := table(rendered["elicitation"])
	s.Elicitation = elicitConfig{Enabled: boolish(elicit["enabled"], true), Timeout: duration(atLeastF(elicit["timeout"], 300, 1))}
	s.Events = boolish(rendered["events"], false)
	s.Protocol = strings.ToLower(strings.TrimSpace(fmt.Sprint(orNil(rendered["protocol"]))))
	s.SkipPreflight = boolish(rendered["skip_preflight"], false)
	s.SSLVerify = true
	if v, ok := rendered["ssl_verify"]; ok {
		s.SSLVerify = v
	}
	s.ClientCert, s.ClientKey = rendered["client_cert"], rendered["client_key"]
	s.Strict = boolish(rendered["strict_redirect_headers"], false)
	s.Identity = identity(name, cfg, table(rendered["identity_header"]), rendered["identity_header"] != nil)
	s.Parallel = boolish(rendered["supports_parallel_tool_calls"], false)
	return s
}

func orNil(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func atLeast(v any, def, minimum int) int {
	if v == nil {
		return def
	}
	n, err := cast.ToIntE(v)
	if err != nil {
		return def
	}
	return max(n, minimum)
}

func atLeastF(v any, def, minimum float64) float64 {
	f, ok := seconds(v)
	if !ok {
		return def
	}
	return math.Max(f, minimum)
}

func logLevel(v any) slog.Level {
	switch strings.ToLower(fmt.Sprint(orNil(v))) {
	case "debug":
		return slog.LevelDebug
	case "warning", "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// Identity header of a server, a static value or the profile's name, empty when it has none or the block is unusable
func identity(server string, cfg *config.Config, block map[string]any, present bool) [2]string {
	if !present {
		return [2]string{}
	}
	ignore := func(detail string) [2]string {
		slog.Warn("MCP identity_header "+detail+", ignoring it", "server", server)
		return [2]string{}
	}
	if block == nil {
		return ignore("must be a mapping with 'name' and 'value' or 'value_from'")
	}
	name := strings.TrimSpace(fmt.Sprint(orNil(block["name"])))
	if name == "" {
		return ignore("requires a non-empty 'name'")
	}
	switch from := strings.ToLower(strings.TrimSpace(fmt.Sprint(orNil(block["value_from"])))); from {
	case "profile":
		return [2]string{name, cfg.Profile()}
	case "", "static":
		value := fmt.Sprint(orNil(block["value"]))
		if strings.TrimSpace(value) == "" {
			return ignore("with value_from: static requires a non-empty 'value'")
		}
		return [2]string{name, value}
	default:
		return ignore("value_from must be 'static' or 'profile'")
	}
}

// Headers of a remote server with its identity header, unless a header of that name is set in any casing
func (s *spec) headers() map[string]string {
	out := maps.Clone(s.Headers)
	if s.Identity[0] == "" {
		return out
	}
	for k := range out {
		if strings.EqualFold(k, s.Identity[0]) {
			return out
		}
	}
	out[s.Identity[0]] = s.Identity[1]
	return out
}

// Names of the references a remote server's url or headers still carry, which it can't connect without
func (s *spec) unsetRemote() []string {
	var refs []string
	texts := []string{s.URL}
	for _, v := range s.Headers {
		texts = append(texts, v)
	}
	for _, text := range texts {
		for _, m := range reference.FindAllStringSubmatch(text, -1) {
			if !slices.Contains(refs, m[1]) {
				refs = append(refs, m[1])
			}
		}
	}
	slices.Sort(refs)
	return refs
}

// A remote server whose url or headers name a value the profile does not have
type unsetError struct {
	server string
	refs   []string
}

func (e *unsetError) Error() string {
	parts := make([]string, len(e.refs))
	for i, ref := range e.refs {
		parts[i] = "${" + ref + "}"
	}
	return fmt.Sprintf("MCP server '%s': %s in url/headers is not set in the environment or the profile's MCP values (tools.mcp.env)", e.server,
		strings.Join(parts, ", "))
}

// Whether the tool is turned on by the server's tool filter
func (s *spec) allows(tool string) bool {
	if s.Included {
		return matches(tool, s.Include)
	}
	return !matches(tool, s.Exclude)
}

// Whether a tool name is one of the names, or matches one of their globs, case sensitive
func matches(tool string, patterns []string) bool {
	for _, p := range patterns {
		if p == tool || strings.ContainsAny(p, "*?[") && glob(p).MatchString(tool) {
			return true
		}
	}
	return false
}

// Shell glob as a whole string pattern: * any run, ? any one, [...] and [!...] sets
func glob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			end := strings.IndexByte(pattern[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			set := pattern[i+1 : i+1+end]
			if strings.HasPrefix(set, "!") {
				set = "^" + set[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(set, `\`, `\\`) + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile("^" + regexp.QuoteMeta(pattern) + "$")
	}
	return re
}

// Values of a setting that hide whitespace before or after them, warned of once per server and place
func (s *spec) warnWhitespace(seen map[string]bool) {
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch x := v.(type) {
		case string:
			if x != strings.TrimSpace(x) && !seen[s.Name+path] {
				seen[s.Name+path] = true
				slog.Warn("MCP setting has hidden leading or trailing whitespace, which often breaks authentication or connections; check for stray spaces "+
					"or newlines", "server", s.Name, "setting", strings.TrimPrefix(path, "."))
			}
		case map[string]any:
			for k, item := range x {
				walk(item, path+"."+k)
			}
		case []any:
			for i, item := range x {
				walk(item, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(s.Raw, "")
}
