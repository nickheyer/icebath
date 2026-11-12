package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CLIConfigUI is a terminal whose raw line input reports end of input or an interrupt as false
type CLIConfigUI interface {
	TerminalUI
	Input(display string) (string, bool)
}

// MCPArgs carries the parsed icebath mcp flags, a nil ConnectTimeout is an absent flag
type MCPArgs struct {
	Action, Name, URL, Command, Auth, Preset, Flow, Identifier string
	Args, Env                                                  []string
	ConnectTimeout                                             *float64
	All                                                        bool
}

// ProbeDetails is the display extra of one probe, nil counts mean the list was not fetched
type ProbeDetails struct {
	Initialized bool
	SchemaChars map[string]int
	Prompts     *int
	Resources   *int
}

// ProbeError is a redacted probe failure tagged with its extension install failure class
type ProbeError struct {
	Err   error
	Class string
}

func (e *ProbeError) Error() string        { return e.Err.Error() }
func (e *ProbeError) Unwrap() error        { return e.Err }
func (e *ProbeError) FailureClass() string { return e.Class }

type redactedError struct {
	msg  string
	root error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.root }

type probeTimeoutError struct{ msg string }

func (e *probeTimeoutError) Error() string { return e.msg }
func (e *probeTimeoutError) Timeout() bool { return true }

var (
	envVarNameRe                 = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	authSchemePrefixRe           = regexp2.MustCompile(`^(?:Bearer|Basic|Token|Digest)\s+`, regexp2.IgnoreCase)
	envKeySuffixRe               = regexp.MustCompile(`[^A-Za-z0-9_]`)
	digestParam                  = `(?:username|response|opaque|cnonce|nonce|uri|realm|qop|nc|algorithm)\s*=\s*(?:"[^"]*"|'[^']*'|[^\s,]+)`
	credentialHeader             = `(?:(?:Proxy-)?Authorization|(?:` + strings.Join(tools.SecretHeaderNameList, "|") + `))`
	probeMappingRe               = regexp2.MustCompile(`(['"])(`+credentialHeader+`)\1(\s*:\s*)(['"])((?:\\.|(?!\4).)*)\4`, regexp2.IgnoreCase)
	probeUnquotedRe              = regexp2.MustCompile(`(['"])(`+credentialHeader+`)\1(\s*:\s*)(?!['"])([^\s,}\]]+)`, regexp2.IgnoreCase)
	probeWireRe                  = regexp2.MustCompile(`(`+credentialHeader+`)(\s*:\s*)([^\n\r]+)`, regexp2.IgnoreCase)
	probeBareSchemeRe            = regexp2.MustCompile(`\b(Bearer|Basic|Token)(\s+)([^\s"']+)|\b(Digest)(\s+)(`+digestParam+`(?:\s*,\s*`+digestParam+`)*)`, regexp2.IgnoreCase)
	mcpPresets                   = map[string]map[string]any{"codex": {"command": "codex", "args": []any{"mcp-server"}}}
	cliStderr          io.Writer = os.Stderr
	cliNow                       = time.Now
)

// Applies a regexp2 replacement evaluator over the whole text
func replaceAll2(re *regexp2.Regexp, text string, fn func(m *regexp2.Match) string) string {
	out, err := re.ReplaceFunc(text, func(m regexp2.Match) string { return fn(&m) }, -1, -1)
	if err != nil {
		return text
	}
	return out
}

// Numbered capture group text, empty when the group did not take part
func mgroup(m *regexp2.Match, i int) string { return m.GroupByNumber(i).String() }

// Credential header value replaced by stars, keeping a leading auth scheme word
func maskHeaderFieldValue(value string) string {
	if m, _ := authSchemePrefixRe.FindStringMatch(value); m != nil {
		return m.String() + "***"
	}
	return "***"
}

// Text with every credential header value and bare auth scheme span replaced, then force redacted
func redactProbeText(text string) string {
	if text == "" {
		return text
	}
	out := replaceAll2(probeMappingRe, text, func(m *regexp2.Match) string {
		return mgroup(m, 1) + mgroup(m, 2) + mgroup(m, 1) + mgroup(m, 3) + mgroup(m, 4) + maskHeaderFieldValue(mgroup(m, 5)) + mgroup(m, 4)
	})
	out = replaceAll2(probeUnquotedRe, out, func(m *regexp2.Match) string {
		return mgroup(m, 1) + mgroup(m, 2) + mgroup(m, 1) + mgroup(m, 3) + maskHeaderFieldValue(mgroup(m, 4))
	})
	out = replaceAll2(probeWireRe, out, func(m *regexp2.Match) string {
		return mgroup(m, 1) + mgroup(m, 2) + maskHeaderFieldValue(mgroup(m, 3))
	})
	out = replaceAll2(probeBareSchemeRe, out, func(m *regexp2.Match) string {
		if g := mgroup(m, 1); g != "" {
			return g + mgroup(m, 2) + "***"
		}
		return mgroup(m, 4) + mgroup(m, 5) + "***"
	})
	return tools.RedactSensitiveTextWith(nil, out, tools.RedactOptions{Force: true})
}

// Reports whether every non scheme span of a header value is an env reference
func headerValueIsOnlyEnvRefs(value string) bool {
	leftover := envVarPattern.ReplaceAllString(value, "")
	if m, _ := authSchemePrefixRe.FindStringMatch(leftover); m != nil {
		leftover = leftover[len(m.String()):]
	}
	return strings.Trim(leftover, " \t;,") == ""
}

// Header value safe to print, shown only when it is env references alone
func redactHeaderDisplay(name, value string) string {
	if value != "" && headerValueIsOnlyEnvRefs(value) {
		return value
	}
	redactProbeText(name + ": " + value)
	return "***"
}

// First leaf of an error group, the root a probe failure is reported from
func unwrapErrorGroup(err error) error {
	for {
		g, ok := err.(interface{ Unwrap() []error })
		if !ok {
			return err
		}
		kids := g.Unwrap()
		if len(kids) == 0 || kids[0] == nil {
			return err
		}
		err = kids[0]
	}
}

// Probe failure whose text is safe to print, the root error itself when nothing needed redaction
func redactProbeError(err error) error {
	root := unwrapErrorGroup(err)
	safe := redactProbeText(root.Error())
	if safe == root.Error() {
		return root
	}
	return &redactedError{msg: safe, root: root}
}

func cfgInfo(ui TerminalUI, text string) { fmt.Fprintln(ui.Out(), ui.Color("  "+text, ansiDim)) }
func cfgSuccess(ui TerminalUI, text string) {
	fmt.Fprintln(ui.Out(), ui.Color("  ✓ "+text, ansiGreen))
}
func cfgWarning(ui TerminalUI, text string) {
	fmt.Fprintln(ui.Out(), ui.Color("  ⚠ "+text, ansiYellow))
}
func cfgError(ui TerminalUI, text string) { fmt.Fprintln(ui.Out(), ui.Color("  ✗ "+text, ansiRed)) }

// Yes or no answer from one input line, the default on an empty line, end of input or interrupt
func cfgConfirm(ui CLIConfigUI, question string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	val, ok := ui.Input(ui.Color("  "+question+" ["+hint+"]: ", ansiYellow))
	if !ok {
		fmt.Fprintln(ui.Out())
		return def
	}
	val = strings.ToLower(strings.TrimSpace(val))
	if val == "" {
		return def
	}
	return val == "y" || val == "yes"
}

// Prints tool names padded in green beside descriptions cut to descMax characters
func printTools(ui TerminalUI, found []ToolSummary, width, descMax int) {
	for _, t := range found {
		short := t.Description
		if r := []rune(short); len(r) > descMax {
			short = string(r[:descMax]) + "..."
		}
		fmt.Fprintf(ui.Out(), "    %s %s\n", pyLjust(ui.Color(t.Name, ansiGreen), width), short)
	}
}

// The mcp_servers mapping of a config document, the ctx home user config when cfg is nil
func getMCPServers(ctx context.Context, cfg map[string]any) (map[string]any, error) {
	if cfg == nil {
		raw, err := config.ReadUser(home.Dir(ctx))
		if err != nil {
			return nil, err
		}
		cfg = raw
	}
	if s, ok := cfg["mcp_servers"].(map[string]any); ok && len(s) > 0 {
		return s, nil
	}
	return map[string]any{}, nil
}

// Include and exclude lists of a server entry, nil when the key is absent or not a list
func toolFilters(cfg map[string]any) ([]any, []any) {
	tc, ok := cfg["tools"].(map[string]any)
	if !ok {
		return nil, nil
	}
	include, _ := tc["include"].([]any)
	exclude, _ := tc["exclude"].([]any)
	if _, isList := tc["include"].([]any); isList && include == nil {
		include = []any{}
	}
	return include, exclude
}

// Warns about every suspicious setting of an entry and reports whether it is clean
func validateOrWarn(ui TerminalUI, name string, serverConfig map[string]any) bool {
	issues := ValidateServerEntry(name, serverConfig)
	for _, issue := range issues {
		cfgWarning(ui, issue)
	}
	if len(issues) > 0 {
		cfgWarning(ui, fmt.Sprintf("Server '%s' was NOT saved due to suspicious configuration.", name))
	}
	return len(issues) == 0
}

// Adds or replaces a server entry in config.yaml, false when validation rejects it
func SaveServer(ctx context.Context, ui TerminalUI, name string, serverConfig map[string]any) (bool, error) {
	if !validateOrWarn(ui, name, serverConfig) {
		return false, nil
	}
	h := home.Dir(ctx)
	doc, err := config.ReadUser(h)
	if err != nil {
		return false, err
	}
	servers, ok := doc["mcp_servers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
	}
	servers[name] = serverConfig
	doc["mcp_servers"] = servers
	return true, config.SaveUser(ctx, h, doc)
}

// Named server entry, or the not found hint listing the available names
func lookupServer(ui TerminalUI, name string, servers map[string]any, availableLabel string) (map[string]any, bool) {
	if raw, ok := servers[name]; ok {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		return entry, true
	}
	cfgError(ui, fmt.Sprintf("Server '%s' not found in config.", name))
	if len(servers) > 0 {
		cfgInfo(ui, availableLabel+": "+strings.Join(sortedKeys(servers), ", "))
	}
	return nil, false
}

// Removes a server from config.yaml, dropping the mapping once empty, reporting whether it existed
func RemoveServer(ctx context.Context, name string) (bool, error) {
	h := home.Dir(ctx)
	doc, err := config.ReadUser(h)
	if err != nil {
		return false, err
	}
	servers, _ := doc["mcp_servers"].(map[string]any)
	if _, ok := servers[name]; !ok {
		return false, nil
	}
	delete(servers, name)
	if len(servers) == 0 {
		delete(doc, "mcp_servers")
	}
	return true, config.SaveUser(ctx, h, doc)
}

// Replaces the whole mcp_servers mapping after validating every entry, an empty map removes the key
func ReplaceServers(ctx context.Context, servers map[string]any) (bool, []string, error) {
	var issues []string
	for _, name := range sortedKeys(servers) {
		cfg, ok := servers[name].(map[string]any)
		if !ok {
			issues = append(issues, fmt.Sprintf("Server '%s': expected an object", name))
			continue
		}
		issues = append(issues, ValidateServerEntry(name, cfg)...)
	}
	if len(issues) > 0 {
		return false, issues, nil
	}
	h := home.Dir(ctx)
	doc, err := config.ReadUser(h)
	if err != nil {
		return false, nil, err
	}
	if len(servers) > 0 {
		copied := make(map[string]any, len(servers))
		for k, v := range servers {
			copied[k] = v
		}
		doc["mcp_servers"] = copied
	} else {
		delete(doc, "mcp_servers")
	}
	return true, nil, config.SaveUser(ctx, h, doc)
}

// Env key holding the API key of a server, MCP_<NAME>_API_KEY
func envKeyForServer(name string) string {
	return "MCP_" + strings.Trim(envKeySuffixRe.ReplaceAllString(strings.ToUpper(name), "_"), "_") + "_API_KEY"
}

// Pasted token without a leading Bearer word and surrounding whitespace
func stripBearerPrefix(token string) string {
	stripped := pyStrip(token)
	if r := []rune(stripped); len(r) >= 7 && strings.ToLower(string(r[:7])) == "bearer " {
		return pyStrip(string(r[7:]))
	}
	return stripped
}

// Persisted Authorization header template naming the server env key
func bearerAuthHeaders(name string) map[string]any {
	return map[string]any{"Authorization": "Bearer ${" + envKeyForServer(name) + "}"}
}

// Saves a normalized bearer token to the profile .env and returns the header template
func SaveBearerAuthToken(ctx context.Context, name, token string) (map[string]any, error) {
	normalized := stripBearerPrefix(token)
	if normalized == "" || strings.ToLower(normalized) == "bearer" {
		return nil, errors.New("Bearer token is required")
	}
	if err := config.SaveEnvValue(home.Dir(ctx), envKeyForServer(name), normalized); err != nil {
		return nil, err
	}
	return bearerAuthHeaders(name), nil
}

// KEY=VALUE arguments as an env map, rejecting malformed entries and names
func parseEnvAssignments(rawEnv []string) (map[string]any, error) {
	parsed := map[string]any{}
	for _, item := range rawEnv {
		text := pyStrip(item)
		if text == "" {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("Invalid --env value '%s' (expected KEY=VALUE)", text)
		}
		key = pyStrip(key)
		if key == "" {
			return nil, fmt.Errorf("Invalid --env value '%s' (missing variable name)", text)
		}
		if !envVarNameRe.MatchString(key) {
			return nil, fmt.Errorf("Invalid --env variable name '%s'", key)
		}
		parsed[key] = value
	}
	return parsed, nil
}

// Fills transport details from a known preset when none were given, reporting whether it applied
func applyMCPPreset(presetName, url, command string, cmdArgs []string, serverConfig map[string]any) (string, string, []string, bool, error) {
	if presetName == "" {
		return url, command, cmdArgs, false, nil
	}
	preset, ok := mcpPresets[presetName]
	if !ok {
		return url, command, cmdArgs, false, fmt.Errorf("Unknown MCP preset: %s", presetName)
	}
	if url != "" || command != "" {
		return url, command, cmdArgs, false, nil
	}
	url, command = pyStr(preset["url"]), pyStr(preset["command"])
	cmdArgs = nil
	presetArgs, _ := preset["args"].([]any)
	for _, a := range presetArgs {
		cmdArgs = append(cmdArgs, pyStr(a))
	}
	if url != "" {
		serverConfig["url"] = url
	}
	if command != "" {
		serverConfig["command"] = command
	}
	if len(cmdArgs) > 0 {
		serverConfig["args"] = toAnyList(cmdArgs)
	}
	return url, command, cmdArgs, true, nil
}

// String list as a config list
func toAnyList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Connects one server, lists its tools and disconnects, with the default connect timeout and no details
func probeSingleServer(ctx context.Context, env *tools.Env, name string, cfg map[string]any) ([]ToolSummary, error) {
	return probeServer(ctx, env, name, cfg, nil, nil)
}

// One server probe for callers outside the package
func ProbeSingleServer(ctx context.Context, env *tools.Env, name string, cfg map[string]any) ([]ToolSummary, error) {
	return probeSingleServer(ctx, env, name, cfg)
}

// Connect and loop runners the probe uses, swapped by tests to observe the rendered config and both bounds
var (
	probeConnect   = connectServer
	probeRunOnLoop = runOnLoop[[]ToolSummary]
)

// Connects one server, lists its tools with 80 character descriptions and disconnects, failures redacted and classed
func probeServer(ctx context.Context, env *tools.Env, name string, cfg map[string]any, connectTimeout *float64, details *ProbeDetails) ([]ToolSummary, error) {
	if issues := ValidateServerEntry(name, cfg); len(issues) > 0 {
		return nil, &ProbeError{Err: errors.New(strings.Join(issues, "; ")), Class: "config_rejected"}
	}
	r, err := renderServerConfig(ctx, env, cfg)
	if err != nil {
		return nil, probeFailed(err)
	}
	timeout := 30.0
	if connectTimeout != nil {
		timeout = *connectTimeout
	} else if raw, ok := r.Config["connect_timeout"]; !ok {
		timeout = 30
	} else if f, ok := pyFloat(raw); ok {
		timeout = max(1.0, f)
	}
	r.Config["connect_timeout"] = timeout
	ensureMCPLoop()
	defer stopMCPLoopIfIdle()
	found, err := probeRunOnLoop(ctx, secondsDuration(timeout+10), func(c context.Context) ([]ToolSummary, error) {
		var claimed []*serverTask
		if details != nil {
			details.Initialized = false
		}
		cctx, cancel := context.WithTimeout(c, secondsDuration(timeout))
		srv, err := probeConnect(cctx, env, name, r, func(s *serverTask) { claimed = append(claimed, s) })
		timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded) && c.Err() == nil
		cancel()
		if details != nil && len(claimed) > 0 {
			claimed[0].mu.Lock()
			details.Initialized = claimed[0].initializeResult != nil
			claimed[0].mu.Unlock()
		}
		if err != nil {
			var te *TimeoutError
			if timedOut || errors.As(err, &te) {
				return nil, &probeTimeoutError{fmt.Sprintf("Connecting to MCP server '%s' timed out after %.0fs (bounded by connect_timeout; an OAuth login also by oauth.timeout)", name, timeout)}
			}
			return nil, err
		}
		defer srv.shutdown(context.WithoutCancel(c))
		srv.mu.Lock()
		listed := append([]*Tool(nil), srv.tools...)
		session, initRes := srv.session, srv.initializeResult
		srv.mu.Unlock()
		out := make([]ToolSummary, 0, len(listed))
		for _, t := range listed {
			desc := t.Description
			if rr := []rune(desc); len(rr) > 80 {
				desc = string(rr[:77]) + "..."
			}
			out = append(out, ToolSummary{Name: t.Name, Description: desc})
		}
		if details != nil {
			details.SchemaChars = map[string]int{}
			for _, t := range listed {
				details.SchemaChars[t.Name] = len(pyJSONCompactASCII(convertToolSchema(name, t)))
			}
			filter, _ := r.Config["tools"].(map[string]any)
			var caps *ServerCapabilities
			if initRes != nil {
				caps = initRes.Capabilities
			}
			wanted := func(capName string, advertised bool) bool {
				if !parseBoolish(filter[capName], true) {
					return false
				}
				return caps == nil || advertised
			}
			if session != nil && wanted("prompts", caps != nil && caps.Prompts != nil) {
				if prompts, err := session.ListPrompts(c); err == nil {
					n := len(prompts)
					details.Prompts = &n
				}
			}
			if session != nil && wanted("resources", caps != nil && caps.Resources != nil) {
				if resources, err := session.ListResources(c); err == nil {
					n := len(resources)
					details.Resources = &n
				}
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, probeFailed(err)
	}
	return found, nil
}

// Redacted probe failure tagged with the class of the original error
func probeFailed(err error) error {
	return &ProbeError{Err: redactProbeError(err), Class: ProbeFailureClass(err)}
}

// Reports whether an OAuth token file exists for the server
func oauthTokensPresent(ctx context.Context, name string) bool {
	return NewTokenStorage(ctx, name, "").HasCachedTokens()
}

// Transport options of a server entry for the OAuth provider and device login requests
func cliHTTPOptions(name, url string, cfg map[string]any) HTTPOptions {
	sslVerify, ok := cfg["ssl_verify"]
	if !ok {
		sslVerify = true
	}
	return HTTPOptions{URL: url, StrictRedirectHeaders: pyTruthy(cfg["strict_redirect_headers"]), SSLVerify: sslVerify,
		ClientCert: cfg["client_cert"], ClientKey: cfg["client_key"],
		ConnectTimeout: secondsDuration(floatOr(cfg, "connect_timeout", defaultConnectTimeout)), ReadTimeout: 300 * time.Second, ServerName: name}
}

// OAuth or bearer token setup of an HTTP server, false when the user cancelled
func configureHTTPAuth(ctx context.Context, env *tools.Env, ui CLIConfigUI, name, url, authType string, serverConfig map[string]any) bool {
	fmt.Fprintln(ui.Out())
	if authType == "oauth" {
		cfgInfo(ui, fmt.Sprintf("Starting OAuth flow for '%s'...", name))
		oauthCfg, _ := serverConfig["oauth"].(map[string]any)
		provider, err := getManager().getOrBuildProvider(ctx, name, url, oauthCfg, cliHTTPOptions(name, url, serverConfig))
		oauthOK := false
		switch {
		case err != nil:
			cfgWarning(ui, "OAuth error: "+excStr(err))
		case provider != nil:
			serverConfig["auth"] = "oauth"
			cfgSuccess(ui, "OAuth configured (tokens will be acquired on first connection)")
			oauthOK = true
		default:
			cfgWarning(ui, "OAuth setup failed — MCP SDK auth module not available")
		}
		if !oauthOK {
			cfgInfo(ui, "This server may not support OAuth.")
			if !cfgConfirm(ui, "Continue without authentication?", true) {
				cfgInfo(ui, "Cancelled.")
				return false
			}
		}
		return true
	}
	cfgInfo(ui, "Connecting to "+url)
	needsAuth := cfgConfirm(ui, "Does this server require authentication?", true)
	if needsAuth && (authType == "header" || authType == "") {
		envKey := envKeyForServer(name)
		if v, _ := catalogEnvValue(ctx, env, envKey); v != "" {
			cfgSuccess(ui, envKey+": already configured")
			serverConfig["headers"] = bearerAuthHeaders(name)
		} else if apiKey := ui.Prompt("API key / Bearer token", "", true); apiKey != "" {
			headers, err := SaveBearerAuthToken(ctx, name, apiKey)
			if err != nil {
				cfgError(ui, err.Error())
				return false
			}
			serverConfig["headers"] = headers
			cfgSuccess(ui, fmt.Sprintf("Saved to %s/.env as %s", home.Display(home.Dir(ctx)), envKey))
		}
	}
	return true
}

// Asks enable all, select or cancel and returns the enabled tool count, false when cancelled
func chooseTools(ui CLIConfigUI, name string, found []ToolSummary, serverConfig map[string]any) (int, bool) {
	fmt.Fprintln(ui.Out())
	cfgSuccess(ui, fmt.Sprintf("Connected! Found %d tool(s) from '%s':", len(found), name))
	fmt.Fprintln(ui.Out())
	printTools(ui, found, 40, 60)
	fmt.Fprintln(ui.Out())
	choice, ok := ui.Input(ui.Color(fmt.Sprintf("  Enable all %d tools? [Y/n/select]: ", len(found)), ansiYellow))
	if !ok {
		fmt.Fprintln(ui.Out())
		cfgInfo(ui, "Cancelled.")
		return 0, false
	}
	choice = strings.ToLower(strings.TrimSpace(choice))
	if choice == "n" || choice == "no" {
		cfgInfo(ui, "Cancelled — server not saved.")
		return 0, false
	}
	if choice != "s" && choice != "select" {
		return len(found), true
	}
	labels := make([]string, len(found))
	all := map[int]bool{}
	for i, t := range found {
		labels[i] = t.Name + "  —  " + t.Description
		all[i] = true
	}
	chosen := ui.Checklist(fmt.Sprintf("Select tools for '%s'", name), labels, all, nil)
	if len(chosen) == 0 {
		cfgInfo(ui, "No tools selected — server not saved.")
		return 0, false
	}
	var names []any
	for _, i := range sortedIndexes(chosen) {
		names = append(names, found[i].Name)
	}
	tc, ok := serverConfig["tools"].(map[string]any)
	if !ok {
		tc = map[string]any{}
		serverConfig["tools"] = tc
	}
	tc["include"] = names
	return len(names), true
}

// Checked indexes in ascending order
func sortedIndexes(set map[int]bool) []int {
	var out []int
	for i, on := range set {
		if on {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// Adds a server after probing it, offering tool selection and a disabled save on failure
func cmdMCPAdd(ctx context.Context, env *tools.Env, ui CLIConfigUI, a MCPArgs) int {
	name, url, command, cmdArgs := a.Name, a.URL, a.Command, a.Args
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	serverConfig := map[string]any{}
	explicitEnv, err := parseEnvAssignments(a.Env)
	if err == nil {
		url, command, cmdArgs, _, err = applyMCPPreset(a.Preset, url, command, append([]string(nil), cmdArgs...), serverConfig)
	}
	if err != nil {
		cfgError(ui, err.Error())
		return 0
	}
	if url != "" && len(explicitEnv) > 0 {
		cfgError(ui, "--env is only supported for stdio MCP servers (--command or stdio presets)")
		return 0
	}
	if url == "" && command == "" {
		cfgError(ui, "Must specify --url <endpoint>, --command <cmd>, or --preset <name>")
		cfgInfo(ui, "Examples:")
		cfgInfo(ui, `  icebath mcp add ink --url "https://mcp.ml.ink/mcp"`)
		cfgInfo(ui, "  icebath mcp add github --command npx --args @modelcontextprotocol/server-github")
		cfgInfo(ui, "  icebath mcp add myserver --preset mypreset")
		return 0
	}
	existing, err := getMCPServers(ctx, nil)
	if err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	_, present := existing[name]
	fresh := !present
	if !fresh && !cfgConfirm(ui, fmt.Sprintf("Server '%s' already exists. Overwrite?", name), false) {
		cfgInfo(ui, "Cancelled.")
		return 0
	}
	record := func(saved bool, failureClass string) {
		if !fresh {
			return
		}
		source, outcome := "local", "failed"
		if url != "" {
			source = "url"
		}
		if saved {
			outcome, failureClass = "success", ""
		}
		RecordMCPInstall(ctx, source, "", outcome, failureClass, nil)
	}
	if url != "" {
		serverConfig["url"] = url
	} else {
		serverConfig["command"] = command
		if len(cmdArgs) > 0 {
			serverConfig["args"] = toAnyList(cmdArgs)
		}
		if len(explicitEnv) > 0 {
			serverConfig["env"] = explicitEnv
		}
	}
	if a.ConnectTimeout != nil {
		serverConfig["connect_timeout"] = *a.ConnectTimeout
	}
	if !validateOrWarn(ui, name, serverConfig) {
		record(false, "config_rejected")
		return 0
	}
	if url != "" && !configureHTTPAuth(ctx, env, ui, name, url, a.Auth, serverConfig) {
		return 0
	}
	save := func() bool {
		saved, err := SaveServer(ctx, ui, name, serverConfig)
		if err != nil {
			cfgError(ui, err.Error())
		}
		return saved && err == nil
	}
	fmt.Fprintln(ui.Out())
	fmt.Fprintln(ui.Out(), ui.Color(fmt.Sprintf("  Connecting to '%s'...", name), ansiCyan))
	found, err := cliProbe(ctx, env, name, serverConfig, nil, nil)
	if err != nil {
		cfgError(ui, "Failed to connect: "+probeFailureReason(err))
		cfgInfo(ui, probeFailureNextStep(ctx, name, err))
		saved := false
		if cfgConfirm(ui, "Save config anyway (you can test later)?", false) {
			serverConfig["enabled"] = false
			if saved = save(); saved {
				cfgSuccess(ui, fmt.Sprintf("Saved '%s' to config (disabled)", name))
				cfgInfo(ui, "Fix the issue, then: icebath mcp test "+name)
			}
		}
		record(saved, ProbeFailureClass(err))
		return 0
	}
	if len(found) == 0 {
		cfgWarning(ui, "Server connected but reported no tools.")
		if cfgConfirm(ui, "Save config anyway?", true) {
			saved := save()
			record(saved, "config_rejected")
			if saved {
				cfgSuccess(ui, fmt.Sprintf("Saved '%s' to config", name))
			}
		}
		return 0
	}
	toolCount, ok := chooseTools(ui, name, found, serverConfig)
	if !ok {
		return 0
	}
	serverConfig["enabled"] = true
	saved := save()
	record(saved, "config_rejected")
	if saved {
		fmt.Fprintln(ui.Out())
		cfgSuccess(ui, fmt.Sprintf("Saved '%s' to %s/config.yaml (%d/%d tools enabled)", name, home.Display(home.Dir(ctx)), toolCount, len(found)))
		cfgInfo(ui, "Start a new session to use these tools, or run /reload-mcp to load them into open sessions.")
	}
	return 0
}

// Removes a configured server after confirmation and cleans up its OAuth tokens
func cmdMCPRemove(ctx context.Context, ui CLIConfigUI, name string) int {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	if _, ok := lookupServer(ui, name, servers, "Available servers"); !ok {
		return 0
	}
	if !cfgConfirm(ui, fmt.Sprintf("Remove server '%s'?", name), true) {
		cfgInfo(ui, "Cancelled.")
		return 0
	}
	if _, err := RemoveServer(ctx, name); err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	cfgSuccess(ui, fmt.Sprintf("Removed '%s' from config", name))
	if _, err := getManager().remove(ctx, name, ""); err == nil {
		cfgSuccess(ui, "Cleaned up OAuth tokens")
	}
	return 0
}

// Prints every configured server with its transport, tool filter and enabled state
func cmdMCPList(ctx context.Context, ui TerminalUI) int {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	out := ui.Out()
	if len(servers) == 0 {
		fmt.Fprintln(out)
		cfgInfo(ui, "No MCP servers configured.")
		fmt.Fprintln(out)
		cfgInfo(ui, "Add one with:")
		cfgInfo(ui, "  icebath mcp add <name> --url <endpoint>")
		cfgInfo(ui, "  icebath mcp add <name> --command <cmd> --args <args...>")
		fmt.Fprintln(out)
		return 0
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Color("  MCP Servers:", ansiCyan, ansiBold))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s %s %s %s\n", pyLjust("Name", 16), pyLjust("Transport", 30), pyLjust("Tools", 12), pyLjust("Status", 10))
	fmt.Fprintf(out, "  %s %s %s %s\n", strings.Repeat("─", 16), strings.Repeat("─", 30), strings.Repeat("─", 12), strings.Repeat("─", 10))
	for _, name := range sortedKeys(servers) {
		cfg, _ := servers[name].(map[string]any)
		transport := "?"
		if v, ok := cfg["url"]; ok {
			transport = pyStr(v)
		} else if v, ok := cfg["command"]; ok {
			transport = pyStr(v)
			if args, ok := cfg["args"].([]any); ok && len(args) > 0 {
				var parts []string
				for _, a := range args[:min(2, len(args))] {
					parts = append(parts, pyStr(a))
				}
				transport += " " + strings.Join(parts, " ")
			}
		}
		if r := []rune(transport); len(r) > 28 {
			transport = string(r[:25]) + "..."
		}
		include, exclude := toolFilters(cfg)
		toolsStr := "all"
		if include != nil {
			toolsStr = fmt.Sprintf("%d selected", len(include))
		} else if len(exclude) > 0 {
			toolsStr = fmt.Sprintf("-%d excluded", len(exclude))
		}
		status := ui.Color("✗ disabled", ansiDim)
		if ServerEnabled(cfg) {
			status = ui.Color("✓ enabled", ansiGreen)
		}
		fmt.Fprintf(out, "  %s %s %s %s\n", pyLjust(name, 16), pyLjust(transport, 30), pyLjust(toolsStr, 12), status)
	}
	fmt.Fprintln(out)
	return 0
}

// Redacted connect error text of a failed probe
func probeFailureReason(err error) string {
	return redactProbeText(formatConnectError(err))
}

// Extension install failure class of a failed probe from its tag or error type, never its message
func ProbeFailureClass(err error) (class string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Debug("MCP probe failure not classified", "panic", recovered)
			class = "connect_failed"
		}
	}()
	var tagged interface{ FailureClass() string }
	if errors.As(err, &tagged) {
		if c := tagged.FailureClass(); c != "" {
			return c
		}
	}
	root := unwrapErrorGroup(err)
	var invalid *InvalidMCPURLError
	if errors.As(root, &invalid) {
		return "config_invalid"
	}
	if probeAuthRejected(root) {
		return "auth_required"
	}
	for _, node := range iterExceptionNodes(err) {
		var abi *NodeABIMismatchError
		if errors.Is(node, os.ErrNotExist) || errors.As(node, &abi) {
			return "server_start_failed"
		}
	}
	return "connect_failed"
}

// Reports whether a probe root is an auth failure or an HTTP 401 or 403 answer
func probeAuthRejected(root error) bool {
	if isAuthError(root) {
		return true
	}
	var hse *HTTPStatusError
	return errors.As(root, &hse) && (hse.Status == 401 || hse.Status == 403)
}

// The one command that fixes a failed probe, sign in, missing command or a recheck
func probeFailureNextStep(ctx context.Context, name string, err error) string {
	root := unwrapErrorGroup(err)
	if probeAuthRejected(root) {
		return "The server rejected the sign-in. Run: icebath mcp login " + name
	}
	var abi *NodeABIMismatchError
	if errors.As(root, &abi) {
		return "After rebuilding it under Icebath's Node as above, run: icebath mcp test " + name
	}
	if strings.Contains(formatConnectError(err), "missing executable") {
		return fmt.Sprintf("Install that command, or set mcp_servers.%s.command in %s/config.yaml to its full path.", name, home.Display(home.Dir(ctx)))
	}
	return "Check the server is running and the URL/command in its config, then run: icebath mcp test " + name
}

// Probes one configured server, exit 0 connected, 1 failed, 3 not in the config
func cmdMCPTest(ctx context.Context, env *tools.Env, ui TerminalUI, name string) int {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	cfg, ok := lookupServer(ui, name, servers, "Available")
	if !ok {
		return 3
	}
	out := ui.Out()
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Color(fmt.Sprintf("  Testing '%s'...", name), ansiCyan))
	if v, ok := cfg["url"]; ok {
		cfgInfo(ui, "Transport: HTTP → "+pyStr(v))
	} else {
		command := "?"
		if v, ok := cfg["command"]; ok {
			command = pyStr(v)
		}
		cfgInfo(ui, "Transport: stdio → "+command)
	}
	headers, _ := cfg["headers"].(map[string]any)
	switch {
	case pyStr(cfg["auth"]) == "oauth":
		cfgInfo(ui, "Auth: OAuth 2.0")
	case pyTruthy(cfg["headers"]):
		for _, k := range sortedKeys(headers) {
			v, isStr := headers[k].(string)
			lower := strings.ToLower(k)
			if isStr && (strings.Contains(lower, "key") || strings.Contains(lower, "auth")) {
				fmt.Fprintf(out, "    %s: %s\n", k, redactHeaderDisplay(k, v))
			}
		}
	default:
		cfgInfo(ui, "Auth: none")
	}
	start := cliNow()
	found, err := cliProbe(ctx, env, name, cfg, nil, nil)
	if err != nil {
		cfgError(ui, fmt.Sprintf("Connection failed (%.1fs): %s", cliNow().Sub(start).Seconds(), probeFailureReason(err)))
		cfgInfo(ui, probeFailureNextStep(ctx, name, err))
		return 1
	}
	cfgSuccess(ui, fmt.Sprintf("Connected (%.0fms)", float64(cliNow().Sub(start))/float64(time.Millisecond)))
	cfgSuccess(ui, fmt.Sprintf("Tools discovered: %d", len(found)))
	if len(found) > 0 {
		fmt.Fprintln(out)
		printTools(ui, found, 36, 55)
	}
	fmt.Fprintln(out)
	return 0
}

// Login device seam, the RFC 8628 flow outside tests
var cliLoginDevice = LoginDevice

// Forces a fresh browser or device OAuth flow for one server and verifies a token landed
func reauthOAuthServer(ctx context.Context, env *tools.Env, ui TerminalUI, name string, serverConfig map[string]any, flow string) bool {
	url := pyStr(serverConfig["url"])
	if !pyTruthy(serverConfig["url"]) {
		cfgError(ui, fmt.Sprintf("Server '%s' has no URL — not an OAuth-capable server", name))
		return false
	}
	if auth, ok := serverConfig["auth"]; !ok || pyStr(auth) != "oauth" {
		shown := "None"
		if ok {
			shown = pyStr(auth)
		}
		cfgError(ui, fmt.Sprintf("Server '%s' is not configured for OAuth (auth=%s)", name, shown))
		cfgInfo(ui, "Use `icebath mcp remove` + `icebath mcp add` to reconfigure auth.")
		return false
	}
	oauthCfg, _ := serverConfig["oauth"].(map[string]any)
	if oauthCfg == nil {
		oauthCfg = map[string]any{}
	}
	selected := flow
	if selected == "" {
		selected = "browser"
		if v, ok := oauthCfg["flow"]; ok {
			selected = pyStr(v)
		}
	}
	if selected != "browser" && selected != "device" {
		cfgError(ui, "oauth.flow must be browser or device")
		return false
	}
	if selected == "browser" {
		getManager().evict(ctx, name, "")
		if err := NewTokenStorage(ctx, name, "").Remove(true); err != nil {
			cfgWarning(ui, "Could not clear existing OAuth state: "+excStr(err))
		}
	}
	fmt.Fprintln(ui.Out())
	cfgInfo(ui, fmt.Sprintf("Starting OAuth flow for '%s'...", name))
	fail := func(err error) bool {
		text := err.Error()
		if humanized, ok := HumanizeOAuthRegistrationError(name, err.Error(), url); ok && humanized != "" {
			text = humanized
		}
		cfgError(ui, "Authentication failed: "+redactProbeText(text))
		return false
	}
	if selected == "device" {
		if err := cliLoginDevice(ctx, name, url, oauthCfg, cliHTTPOptions(name, url, serverConfig)); err != nil {
			return fail(err)
		}
	}
	probeConfig := make(map[string]any, len(serverConfig)+1)
	for k, v := range serverConfig {
		probeConfig[k] = v
	}
	probeOAuth := make(map[string]any, len(oauthCfg)+1)
	for k, v := range oauthCfg {
		probeOAuth[k] = v
	}
	probeOAuth["flow"] = selected
	probeConfig["oauth"] = probeOAuth
	window := LoginConnectTimeout(probeConfig)
	found, err := cliProbe(ForceInteractiveOAuth(ctx), env, name, probeConfig, &window, nil)
	if err != nil {
		return fail(err)
	}
	if !oauthTokensPresent(ctx, name) {
		cfgWarning(ui, "Server responded, but no OAuth token was obtained — authentication did not complete.")
		fmt.Fprintln(ui.Out())
		cfgInfo(ui, "Some providers (e.g. Google Drive, Atlassian) do not support automatic client registration. For those you must create an OAuth client yourself and add its credentials to config.yaml:")
		fmt.Fprintln(ui.Out())
		for _, line := range []string{"mcp_servers:", "  " + name + ":", "    url: " + url, "    auth: oauth", "    oauth:",
			`      client_id: "<your-oauth-client-id>"`, `      client_secret: "<your-oauth-client-secret>"`} {
			fmt.Fprintln(ui.Out(), ui.Color("    "+line, ansiDim))
		}
		fmt.Fprintln(ui.Out())
		cfgInfo(ui, "Then re-run `icebath mcp login "+name+"`.")
		return false
	}
	if len(found) > 0 {
		cfgSuccess(ui, fmt.Sprintf("Authenticated — %d tool(s) available", len(found)))
	} else {
		cfgSuccess(ui, "Authenticated (server reported no tools)")
	}
	return true
}

// Probe seam of the login and reauth commands, the real probe outside tests
var cliProbe = probeServer

// Runs an explicit browser or device authorization for one OAuth server
func cmdMCPLogin(ctx context.Context, env *tools.Env, ui TerminalUI, name, flow string) int {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	if cfg, ok := lookupServer(ui, name, servers, "Available servers"); ok {
		reauthOAuthServer(ctx, env, ui, name, cfg, flow)
	}
	return 0
}

// Re-authenticates one OAuth server or every OAuth server in turn
func cmdMCPReauth(ctx context.Context, env *tools.Env, ui TerminalUI, name string, all bool) int {
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	if all {
		var names []string
		for _, n := range sortedKeys(servers) {
			c, _ := servers[n].(map[string]any)
			if pyStr(c["auth"]) == "oauth" && pyTruthy(c["url"]) {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			cfgInfo(ui, "No OAuth-based MCP servers found in config.")
			return 0
		}
		fmt.Fprintln(ui.Out())
		cfgInfo(ui, fmt.Sprintf("Re-authenticating %d OAuth server(s) one at a time...", len(names)))
		succeeded := 0
		for _, n := range names {
			fmt.Fprintln(ui.Out())
			fmt.Fprintln(ui.Out(), ui.Color("  ── "+n+" ──", ansiCyan, ansiBold))
			c, _ := servers[n].(map[string]any)
			if reauthOAuthServer(ctx, env, ui, n, c, "") {
				succeeded++
			}
		}
		fmt.Fprintln(ui.Out())
		cfgSuccess(ui, fmt.Sprintf("Re-authenticated %d/%d server(s)", succeeded, len(names)))
		return 0
	}
	if name == "" {
		cfgError(ui, "Specify a server name, or use --all to re-auth every OAuth server.")
		cfgInfo(ui, "Usage: icebath mcp reauth <name>   |   icebath mcp reauth --all")
		return 0
	}
	if cfg, ok := lookupServer(ui, name, servers, "Available servers"); ok {
		reauthOAuthServer(ctx, env, ui, name, cfg, "")
	}
	return 0
}

// New exclude list after a checklist edit, keeping globs and toggling literal entries
func rebuildExcludeList(ui TerminalUI, name string, exclude []any, toolNames []string, chosen map[int]bool) []any {
	var globs []string
	literals := map[string]bool{}
	oldSet := map[string]bool{}
	for _, p := range exclude {
		s := pyStr(p)
		oldSet[s] = true
		if strings.ContainsAny(s, "*?[") {
			globs = append(globs, s)
		}
	}
	for s := range oldSet {
		if !strings.ContainsAny(s, "*?[") {
			literals[s] = true
		}
	}
	globSet := map[string]bool{}
	for _, g := range globs {
		globSet[g] = true
	}
	newLiterals := map[string]bool{}
	for s := range literals {
		newLiterals[s] = true
	}
	shadowSet := map[string]bool{}
	for i, tn := range toolNames {
		if chosen[i] {
			delete(newLiterals, tn)
			if len(globs) > 0 && MatchesNameFilter(tn, globSet) {
				shadowSet[tn] = true
			}
		}
	}
	for i, tn := range toolNames {
		if !chosen[i] && !MatchesNameFilter(tn, oldSet) {
			newLiterals[tn] = true
		}
	}
	shadowed := make([]string, 0, len(shadowSet))
	for tn := range shadowSet {
		shadowed = append(shadowed, tn)
	}
	if len(shadowed) > 0 {
		sort.Strings(shadowed)
		more := ""
		if len(shadowed) > 5 {
			more = " ..."
		}
		cfgWarning(ui, fmt.Sprintf("%d re-enabled tool(s) still match glob exclude pattern(s) %s and stay excluded: %s%s. Remove the pattern from mcp_servers.%s.tools.exclude in config.yaml to enable them.",
			len(shadowed), pyStrListRepr(globs), strings.Join(shadowed[:min(5, len(shadowed))], ", "), more, name))
	}
	sortedLiterals := make([]string, 0, len(newLiterals))
	for s := range newLiterals {
		sortedLiterals = append(sortedLiterals, s)
	}
	sort.Strings(sortedLiterals)
	return append(toAnyList(globs), toAnyList(sortedLiterals)...)
}

// Reconfigures which tools of a server are enabled through a checklist over its probed tools
func cmdMCPConfigure(ctx context.Context, env *tools.Env, ui TerminalUI, name string) int {
	if !ui.Interactive() {
		fmt.Fprintln(cliStderr, "Error: 'icebath mcp configure' requires an interactive terminal.")
		return 1
	}
	servers, err := getMCPServers(ctx, nil)
	if err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	cfg, ok := lookupServer(ui, name, servers, "Available")
	if !ok {
		return 0
	}
	fmt.Fprintln(ui.Out())
	fmt.Fprintln(ui.Out(), ui.Color(fmt.Sprintf("  Connecting to '%s' to discover tools...", name), ansiCyan))
	all, err := cliProbe(ctx, env, name, cfg, nil, nil)
	if err != nil {
		cfgError(ui, "Failed to connect: "+redactProbeText(err.Error()))
		return 0
	}
	if len(all) == 0 {
		cfgWarning(ui, "Server reports no tools.")
		return 0
	}
	include, exclude := toolFilters(cfg)
	toolNames := make([]string, len(all))
	for i, t := range all {
		toolNames[i] = t.Name
	}
	total := len(all)
	asSet := func(items []any) map[string]bool {
		set := map[string]bool{}
		for _, p := range items {
			set[pyStr(p)] = true
		}
		return set
	}
	pre := map[int]bool{}
	for i, tn := range toolNames {
		switch {
		case include != nil:
			if MatchesNameFilter(tn, asSet(include)) {
				pre[i] = true
			}
		case len(exclude) > 0:
			if !MatchesNameFilter(tn, asSet(exclude)) {
				pre[i] = true
			}
		default:
			pre[i] = true
		}
	}
	cfgInfo(ui, fmt.Sprintf("Currently %d/%d tools enabled for '%s'.", len(pre), total, name))
	fmt.Fprintln(ui.Out())
	labels := make([]string, total)
	for i, t := range all {
		labels[i] = t.Name + "  —  " + t.Description
	}
	chosen := ui.Checklist(fmt.Sprintf("Select tools for '%s'", name), labels, pre, nil)
	if sameIndexSet(chosen, pre) {
		cfgInfo(ui, "No changes made.")
		return 0
	}
	h := home.Dir(ctx)
	doc, err := config.ReadUser(h)
	if err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	docServers, _ := doc["mcp_servers"].(map[string]any)
	entry, _ := docServers[name].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	chosenCount := len(sortedIndexes(chosen))
	excludeMode := len(exclude) > 0 && include == nil
	ensureTools := func() map[string]any {
		tc, ok := entry["tools"].(map[string]any)
		if !ok {
			tc = map[string]any{}
			entry["tools"] = tc
		}
		return tc
	}
	switch {
	case chosenCount == total && !excludeMode:
		delete(entry, "tools")
	case excludeMode:
		if newExclude := rebuildExcludeList(ui, name, exclude, toolNames, chosen); len(newExclude) == 0 {
			delete(entry, "tools")
		} else {
			tc := ensureTools()
			tc["exclude"] = newExclude
			delete(tc, "include")
		}
	default:
		var names []any
		for _, i := range sortedIndexes(chosen) {
			names = append(names, toolNames[i])
		}
		tc := ensureTools()
		tc["include"] = names
		delete(tc, "exclude")
	}
	if docServers == nil {
		docServers = map[string]any{}
	}
	docServers[name] = entry
	doc["mcp_servers"] = docServers
	if err := config.SaveUser(tools.WithEnvHome(ctx, env), h, doc); err != nil {
		cfgError(ui, err.Error())
		return 1
	}
	cfgSuccess(ui, fmt.Sprintf("Updated config: %d/%d tools enabled", chosenCount, total))
	cfgInfo(ui, "Run /reload-mcp for changes to take effect.")
	return 0
}

// Reports whether two checklist selections hold the same indexes
func sameIndexSet(a, b map[int]bool) bool {
	x, y := sortedIndexes(a), sortedIndexes(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

var mcpUsage = []string{
	"icebath mcp                                    Open the catalog picker (default)",
	"icebath mcp catalog                            List Nous-approved MCPs",
	"icebath mcp install <name>                     Install a catalog MCP",
	"icebath mcp add <name> --url <endpoint>        Add a custom MCP server",
	"icebath mcp add <name> --command <cmd>         Add a stdio server",
	"icebath mcp add <name> --preset <preset>       Add from a known preset",
	"icebath mcp remove <name>                      Remove a server",
	"icebath mcp list                               List configured servers",
	"icebath mcp test <name>                        Test connection",
	"icebath mcp configure <name>                   Toggle tools",
	"icebath mcp login <name>                       Re-authenticate OAuth",
	"icebath mcp reauth <name> | --all              Re-auth one or all OAuth servers",
}

// Dispatches one icebath mcp subcommand and returns the process exit code
func MCPCommand(ctx context.Context, env *tools.Env, ui CLIConfigUI, a MCPArgs) (int, error) {
	switch a.Action {
	case "picker":
		return 0, RunPicker(ctx, env, ui)
	case "catalog":
		return 0, ShowCatalog(ctx, ui)
	case "install":
		return InstallByName(ctx, env, ui, a.Identifier)
	case "add":
		return cmdMCPAdd(ctx, env, ui, a), nil
	case "remove", "rm":
		return cmdMCPRemove(ctx, ui, a.Name), nil
	case "list", "ls":
		return cmdMCPList(ctx, ui), nil
	case "test":
		return cmdMCPTest(ctx, env, ui, a.Name), nil
	case "configure", "config":
		return cmdMCPConfigure(ctx, env, ui, a.Name), nil
	case "login":
		return cmdMCPLogin(ctx, env, ui, a.Name, a.Flow), nil
	case "reauth":
		return cmdMCPReauth(ctx, env, ui, a.Name, a.All), nil
	}
	if err := RunPicker(ctx, env, ui); err != nil {
		return 0, err
	}
	fmt.Fprintln(ui.Out(), ui.Color("  Commands:", ansiCyan))
	for _, line := range mcpUsage {
		cfgInfo(ui, line)
	}
	fmt.Fprintln(ui.Out())
	return 0, nil
}
