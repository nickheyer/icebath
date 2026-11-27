package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	"github.com/nickheyer/icebath/pkg/browser"
	mcpv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/mcp/v1/mcpv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Failure of a command with the exit status it ends with, its lines already printed
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

// Command lines `icebath mcp` lists after its picker
var usage = []string{
	"icebath mcp                                    Open the catalog picker (default)",
	"icebath mcp catalog                            List the bundled catalog MCPs",
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

// Servers a name can stand for in place of a command or URL
var presets = map[string]struct {
	url, command string
	args         []string
}{"codex": {command: "codex", args: []string{"mcp-server"}}}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

// One command's terminal and the service it asks
type run struct {
	*cli.Console
	service mcpv1connect.McpServiceClient
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := mcpv1connect.NewMcpServiceClient(conn)
	with := func(do func(r *run, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			return do(&run{Console: cli.NewConsole(cmd), service: service}, args)
		}
	}
	root := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP servers: the catalog, installs, custom servers, sign ins and tools",
		Args:  cobra.NoArgs,
		RunE: with(func(r *run, _ []string) error {
			if err := r.picker(); err != nil {
				return err
			}
			r.Say("  Commands:", cli.Cyan)
			for _, line := range usage {
				r.Info(line)
			}
			r.Blank()
			return nil
		}),
	}
	var flow string
	login := &cobra.Command{
		Use:   "login <name>",
		Short: "Sign in to an OAuth MCP server again",
		Args:  cobra.ExactArgs(1),
		RunE:  with(func(r *run, args []string) error { return r.login(args[0], flow) }),
	}
	login.Flags().StringVar(&flow, "flow", "", "browser or device, the server's oauth.flow setting when empty")
	var all bool
	reauth := &cobra.Command{
		Use:   "reauth [name]",
		Short: "Sign in to one OAuth MCP server again, or every one with --all",
		Args:  cobra.MaximumNArgs(1),
		RunE: with(func(r *run, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return r.reauth(name, all)
		}),
	}
	reauth.Flags().BoolVar(&all, "all", false, "sign in to every OAuth server, one at a time")
	root.AddCommand(
		&cobra.Command{Use: "picker", Short: "Open the catalog picker", Args: cobra.NoArgs, RunE: with(func(r *run, _ []string) error { return r.picker() })},
		&cobra.Command{Use: "catalog", Short: "List the bundled catalog MCPs", Args: cobra.NoArgs, RunE: with(func(r *run, _ []string) error { return r.catalog() })},
		&cobra.Command{Use: "install <name>", Short: "Install a catalog MCP", Args: cobra.ExactArgs(1),
			RunE: with(func(r *run, args []string) error { return r.installName(args[0]) })},
		add(with),
		&cobra.Command{Use: "remove <name>", Aliases: []string{"rm"}, Short: "Remove an MCP server", Args: cobra.ExactArgs(1),
			RunE: with(func(r *run, args []string) error { return r.remove(args[0]) })},
		&cobra.Command{Use: "list", Aliases: []string{"ls"}, Short: "List the configured MCP servers", Args: cobra.NoArgs,
			RunE: with(func(r *run, _ []string) error { return r.list() })},
		&cobra.Command{Use: "test <name>", Short: "Connect to an MCP server and list its tools", Args: cobra.ExactArgs(1),
			RunE: with(func(r *run, args []string) error { return r.test(args[0]) })},
		&cobra.Command{Use: "configure <name>", Aliases: []string{"config"}, Short: "Pick the tools of an MCP server", Args: cobra.ExactArgs(1),
			RunE: with(func(r *run, args []string) error { return r.configure(args[0]) })},
		login,
		reauth,
		serve(service),
	)
	return []*cobra.Command{root}
}

// Command that adds a custom server: its flags, the name, and the command's arguments after --args or --
func add(with func(do func(r *run, args []string) error) func(*cobra.Command, []string) error) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <name> (--url <endpoint> | --command <cmd> [--args <arg>...] | --preset <name>)",
		Short: "Add a custom MCP server",
		Example: `  icebath mcp add ink --url "https://mcp.ml.ink/mcp"
  icebath mcp add github --command npx --args @modelcontextprotocol/server-github`,
		DisableFlagParsing: true,
	}
	flags := cmd.Flags()
	flags.String("url", "", "address of an HTTP server")
	flags.String("command", "", "command of a stdio server")
	flags.String("auth", "", "how an HTTP server signs in: oauth or header")
	flags.String("preset", "", "known server to add: "+strings.Join(slices.Sorted(func(yield func(string) bool) {
		for name := range presets {
			if !yield(name) {
				return
			}
		}
	}), ", "))
	flags.StringArray("env", nil, "KEY=VALUE for a stdio server, repeatable")
	flags.Float64("connect-timeout", 0, "seconds a connect may take")
	flags.BoolP("help", "h", false, "help for add")
	cmd.RunE = func(cmd *cobra.Command, argv []string) error {
		var rest []string
		for i, arg := range argv {
			if arg == "--args" || arg == "--" {
				argv, rest = argv[:i], argv[i+1:]
				break
			}
		}
		if err := flags.Parse(argv); err != nil {
			return err
		}
		if help, _ := flags.GetBool("help"); help {
			return cmd.Help()
		}
		positionals := flags.Args()
		if len(positionals) == 0 {
			return errors.New("the following arguments are required: name")
		}
		if len(positionals) > 1 {
			return errors.New("unrecognized arguments: " + strings.Join(positionals[1:], " "))
		}
		a := addArgs{name: positionals[0], args: rest}
		a.url, _ = flags.GetString("url")
		a.command, _ = flags.GetString("command")
		a.auth, _ = flags.GetString("auth")
		a.preset, _ = flags.GetString("preset")
		a.env, _ = flags.GetStringArray("env")
		if flags.Changed("connect-timeout") {
			v, _ := flags.GetFloat64("connect-timeout")
			a.connectTimeout = &v
		}
		return with(func(r *run, _ []string) error { return r.add(a) })(cmd, nil)
	}
	return cmd
}

type addArgs struct {
	name, url, command, auth, preset string
	args, env                        []string
	connectTimeout                   *float64
}

func (r *run) servers() ([]*mcpv1.Server, error) {
	response, err := r.service.List(r.Context(), &mcpv1.ListRequest{})
	if err != nil {
		return nil, err
	}
	servers := response.GetServers()
	slices.SortFunc(servers, func(a, b *mcpv1.Server) int { return strings.Compare(a.GetName(), b.GetName()) })
	return servers, nil
}

// Server of the name, or the not found lines listing the names there are
func (r *run) lookup(name, label string) (*mcpv1.Server, error) {
	servers, err := r.servers()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, server := range servers {
		if server.GetName() == name {
			return server, nil
		}
		names = append(names, server.GetName())
	}
	r.Failure(fmt.Sprintf("Server '%s' not found in config.", name))
	if len(names) > 0 {
		r.Info(label + ": " + strings.Join(names, ", "))
	}
	return nil, nil
}

func settingsOf(server *mcpv1.Server) map[string]any {
	var settings map[string]any
	json.Unmarshal([]byte(server.GetSettings()), &settings)
	if settings == nil {
		settings = map[string]any{}
	}
	return settings
}

func encode(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func (r *run) list() error {
	servers, err := r.servers()
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		r.Blank()
		r.Info("No MCP servers configured.")
		r.Blank()
		r.Info("Add one with:")
		r.Info("  icebath mcp add <name> --url <endpoint>")
		r.Info("  icebath mcp add <name> --command <cmd> --args <args...>")
		r.Blank()
		return nil
	}
	r.Blank()
	r.Say("  MCP Servers:", cli.Cyan, cli.Bold)
	r.Blank()
	fmt.Fprintf(r.Out(), "  %s %s %s %s\n", cli.Pad("Name", 16), cli.Pad("Transport", 30), cli.Pad("Tools", 12), cli.Pad("Status", 10))
	fmt.Fprintf(r.Out(), "  %s %s %s %s\n", strings.Repeat("─", 16), strings.Repeat("─", 30), strings.Repeat("─", 12), strings.Repeat("─", 10))
	for _, server := range servers {
		transport := "?"
		if server.GetUrl() != "" {
			transport = server.GetUrl()
		} else if server.GetCommand() != "" {
			transport = strings.Join(append([]string{server.GetCommand()}, server.GetArgs()[:min(2, len(server.GetArgs()))]...), " ")
		}
		if r := []rune(transport); len(r) > 28 {
			transport = string(r[:25]) + "..."
		}
		filter := "all"
		switch {
		case server.GetIncluded():
			filter = fmt.Sprintf("%d selected", len(server.GetInclude()))
		case len(server.GetExclude()) > 0:
			filter = fmt.Sprintf("-%d excluded", len(server.GetExclude()))
		}
		status := r.Paint("✗ disabled", cli.Dim)
		if server.GetEnabled() {
			status = r.Paint("✓ enabled", cli.Green)
		}
		fmt.Fprintf(r.Out(), "  %s %s %s %s\n", cli.Pad(server.GetName(), 16), cli.Pad(transport, 30), cli.Pad(filter, 12), status)
	}
	r.Blank()
	return nil
}

func (r *run) remove(name string) error {
	server, err := r.lookup(name, "Available servers")
	if err != nil || server == nil {
		return err
	}
	if plugin := server.GetPlugin(); plugin != "" {
		r.Failure(fmt.Sprintf("'%s' comes from the plugin %s; disable it instead.", name, plugin))
		return exit(1)
	}
	if !r.Ask(fmt.Sprintf("Remove server '%s'?", name), true) {
		r.Info("Cancelled.")
		return nil
	}
	if _, err := r.service.Remove(r.Context(), &mcpv1.RemoveRequest{Name: name}); err != nil {
		r.Failure(err.Error())
		return exit(1)
	}
	r.Success(fmt.Sprintf("Removed '%s' from config", name))
	if server.GetOauth() {
		r.Success("Cleaned up OAuth tokens")
	}
	return nil
}

// Header value shown with its credential hidden: references to values kept as they are, anything else starred past its scheme word
func redactHeader(value string) string {
	if strings.TrimSpace(regexp.MustCompile(`\$\{[^}]*\}`).ReplaceAllString(value, "")) == "" ||
		regexp.MustCompile(`^(?i:bearer|basic|token)\s+\$\{[^}]*\}\s*$`).MatchString(value) {
		return value
	}
	if scheme, _, ok := strings.Cut(value, " "); ok && regexp.MustCompile(`^(?i:bearer|basic|token|digest)$`).MatchString(scheme) {
		return scheme + " ***"
	}
	return "***"
}

func (r *run) printTools(tools []*mcpv1.ToolSummary, width, descMax int) {
	for _, t := range tools {
		fmt.Fprintf(r.Out(), "    %s %s\n", cli.Pad(r.Paint(t.GetName(), cli.Green), width), strs.Clip(t.GetDescription(), descMax))
	}
}

func (r *run) test(name string) error {
	server, err := r.lookup(name, "Available")
	if err != nil {
		return err
	}
	if server == nil {
		return exit(3)
	}
	settings := settingsOf(server)
	r.Blank()
	r.Say(fmt.Sprintf("  Testing '%s'...", name), cli.Cyan)
	if server.GetUrl() != "" {
		r.Info("Transport: HTTP → " + server.GetUrl())
	} else {
		r.Info("Transport: stdio → " + cmp.Or(server.GetCommand(), "?"))
	}
	headers, _ := settings["headers"].(map[string]any)
	switch {
	case server.GetAuth() == "oauth":
		r.Info("Auth: OAuth 2.0")
	case len(headers) > 0:
		for _, key := range slices.Sorted(func(yield func(string) bool) {
			for k := range headers {
				if !yield(k) {
					return
				}
			}
		}) {
			value, isText := headers[key].(string)
			lower := strings.ToLower(key)
			if isText && (strings.Contains(lower, "key") || strings.Contains(lower, "auth")) {
				fmt.Fprintf(r.Out(), "    %s: %s\n", key, redactHeader(value))
			}
		}
	default:
		r.Info("Auth: none")
	}
	start := time.Now()
	probe, err := r.service.Probe(r.Context(), &mcpv1.ProbeRequest{Name: name})
	if err != nil {
		return err
	}
	if !probe.GetOk() {
		r.Failure(fmt.Sprintf("Connection failed (%.1fs): %s", time.Since(start).Seconds(), probe.GetError()))
		r.Info(probe.GetNextStep())
		return exit(1)
	}
	r.Success(fmt.Sprintf("Connected (%.0fms)", float64(time.Since(start))/float64(time.Millisecond)))
	r.Success(fmt.Sprintf("Tools discovered: %d", len(probe.GetTools())))
	if len(probe.GetTools()) > 0 {
		r.Blank()
		r.printTools(probe.GetTools(), 36, 55)
	}
	r.Blank()
	return nil
}

// KEY=VALUE arguments as a list of the variables of a stdio server, refusing malformed ones
func parseEnv(raw []string) ([]any, error) {
	var out []any
	for _, item := range raw {
		text := strings.TrimSpace(item)
		if text == "" {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("Invalid --env value '%s' (expected KEY=VALUE)", text)
		}
		if key = strings.TrimSpace(key); key == "" {
			return nil, fmt.Errorf("Invalid --env value '%s' (missing variable name)", text)
		}
		if !envNameRE.MatchString(key) {
			return nil, fmt.Errorf("Invalid --env variable name '%s'", key)
		}
		out = append(out, key+"="+value)
	}
	return out, nil
}

func anyList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Warns about every issue of a server's settings and reports whether there were none
func (r *run) checked(name string, settings map[string]any) (bool, error) {
	response, err := r.service.Check(r.Context(), &mcpv1.CheckRequest{Name: name, Settings: encode(settings)})
	if err != nil {
		return false, err
	}
	for _, issue := range response.GetIssues() {
		r.Warning(issue)
	}
	if len(response.GetIssues()) > 0 {
		r.Warning(fmt.Sprintf("Server '%s' was NOT saved due to suspicious configuration.", name))
		return false, nil
	}
	return true, nil
}

func (r *run) save(name string, settings map[string]any) bool {
	response, err := r.service.Save(r.Context(), &mcpv1.SaveRequest{Name: name, Settings: encode(settings)})
	if err != nil {
		r.Failure(err.Error())
		return false
	}
	for _, issue := range response.GetIssues() {
		r.Warning(issue)
	}
	return len(response.GetIssues()) == 0
}

// Adds a server after probing it, offering a pick of its tools, and a disabled save when it can't be reached
func (r *run) add(a addArgs) error {
	name, url, command, args := a.name, a.url, a.command, a.args
	settings := map[string]any{}
	env, err := parseEnv(a.env)
	if err == nil && a.preset != "" {
		preset, ok := presets[a.preset]
		switch {
		case !ok:
			err = fmt.Errorf("Unknown MCP preset: %s", a.preset)
		case url == "" && command == "":
			url, command, args = preset.url, preset.command, preset.args
		}
	}
	if err != nil {
		r.Failure(err.Error())
		return exit(1)
	}
	if url != "" && len(env) > 0 {
		r.Failure("--env is only supported for stdio MCP servers (--command or stdio presets)")
		return exit(1)
	}
	if url == "" && command == "" {
		r.Failure("Must specify --url <endpoint>, --command <cmd>, or --preset <name>")
		r.Info("Examples:")
		r.Info(`  icebath mcp add ink --url "https://mcp.ml.ink/mcp"`)
		r.Info("  icebath mcp add github --command npx --args @modelcontextprotocol/server-github")
		r.Info("  icebath mcp add myserver --preset codex")
		return exit(1)
	}
	servers, err := r.servers()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(servers, func(s *mcpv1.Server) bool { return s.GetName() == name }) &&
		!r.Ask(fmt.Sprintf("Server '%s' already exists. Overwrite?", name), false) {
		r.Info("Cancelled.")
		return nil
	}
	if url != "" {
		settings["url"] = url
	} else {
		settings["command"] = command
		if len(args) > 0 {
			settings["args"] = anyList(args)
		}
		if len(env) > 0 {
			settings["env"] = env
		}
	}
	if a.connectTimeout != nil {
		settings["connect_timeout"] = *a.connectTimeout
	}
	if ok, err := r.checked(name, settings); err != nil || !ok {
		return err
	}
	if url != "" {
		if ok, err := r.httpAuth(name, url, a.auth, settings); err != nil || !ok {
			return err
		}
	}
	r.Blank()
	r.Say(fmt.Sprintf("  Connecting to '%s'...", name), cli.Cyan)
	request := &mcpv1.ProbeRequest{Name: name, Settings: encode(settings)}
	if a.connectTimeout != nil {
		request.ConnectTimeout = *a.connectTimeout
	}
	probe, err := r.service.Probe(r.Context(), request)
	if err != nil {
		return err
	}
	if !probe.GetOk() {
		r.Failure("Failed to connect: " + probe.GetError())
		r.Info(probe.GetNextStep())
		if r.Ask("Save config anyway (you can test later)?", false) {
			settings["enabled"] = false
			if r.save(name, settings) {
				r.Success(fmt.Sprintf("Saved '%s' to config (disabled)", name))
				r.Info("Fix the issue, then: icebath mcp test " + name)
			}
		}
		return nil
	}
	found := probe.GetTools()
	if len(found) == 0 {
		r.Warning("Server connected but reported no tools.")
		if r.Ask("Save config anyway?", true) && r.save(name, settings) {
			r.Success(fmt.Sprintf("Saved '%s' to config", name))
		}
		return nil
	}
	count, ok := r.chooseTools(name, found, settings)
	if !ok {
		return nil
	}
	settings["enabled"] = true
	if r.save(name, settings) {
		r.Blank()
		r.Success(fmt.Sprintf("Saved '%s' to %s (%d/%d tools enabled)", name, mcpconfig.Servers+"."+name, count, len(found)))
		r.Info("Start a new session to use these tools, or run /reload-mcp to load them into open sessions.")
	}
	return nil
}

// OAuth sign in or bearer token of an HTTP server, false when the user cancelled
func (r *run) httpAuth(name, url, auth string, settings map[string]any) (bool, error) {
	r.Blank()
	if auth == "oauth" {
		settings["auth"] = "oauth"
		r.Info(fmt.Sprintf("Starting OAuth flow for '%s'...", name))
		if r.signin(name, "", settings) {
			r.Success("OAuth configured")
			return true, nil
		}
		delete(settings, "auth")
		r.Info("This server may not support OAuth.")
		if !r.Ask("Continue without authentication?", true) {
			r.Info("Cancelled.")
			return false, nil
		}
		return true, nil
	}
	r.Info("Connecting to " + url)
	if !r.Ask("Does this server require authentication?", true) || auth != "header" && auth != "" {
		return true, nil
	}
	key := mcp.KeyEnv(name)
	has, err := r.service.HasValue(r.Context(), &mcpv1.HasValueRequest{Name: key})
	if err != nil {
		return false, err
	}
	if has.GetSet() {
		r.Success(key + ": already configured")
		settings["headers"] = map[string]any{"Authorization": "Bearer ${" + key + "}"}
		return true, nil
	}
	token := r.Prompt("API key / Bearer token", "", true)
	if token == "" {
		return true, nil
	}
	saved, err := r.service.SaveBearer(r.Context(), &mcpv1.SaveBearerRequest{Name: name, Token: token})
	if err != nil {
		r.Failure(err.Error())
		return false, nil
	}
	var headers map[string]any
	json.Unmarshal([]byte(saved.GetHeaders()), &headers)
	settings["headers"] = headers
	r.Success("Saved to the profile's MCP values as " + saved.GetKey())
	return true, nil
}

// Asks whether to keep every tool, none, or a pick; the count kept, false when cancelled
func (r *run) chooseTools(name string, found []*mcpv1.ToolSummary, settings map[string]any) (int, bool) {
	r.Blank()
	r.Success(fmt.Sprintf("Connected! Found %d tool(s) from '%s':", len(found), name))
	r.Blank()
	r.printTools(found, 40, 60)
	r.Blank()
	choice, ok := r.Input(r.Paint(fmt.Sprintf("  Enable all %d tools? [Y/n/select]: ", len(found)), cli.Yellow))
	if !ok {
		r.Blank()
		r.Info("Cancelled.")
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "n", "no":
		r.Info("Cancelled — server not saved.")
		return 0, false
	case "s", "select":
	default:
		return len(found), true
	}
	labels := make([]string, len(found))
	every := map[int]bool{}
	for i, t := range found {
		labels[i], every[i] = t.GetName()+"  —  "+t.GetDescription(), true
	}
	chosen := cli.Picked(r.Checklist(fmt.Sprintf("Select tools for '%s'", name), labels, every))
	if len(chosen) == 0 {
		r.Info("No tools selected — server not saved.")
		return 0, false
	}
	var names []any
	for _, i := range chosen {
		names = append(names, found[i].GetName())
	}
	settings["tools"] = map[string]any{"include": names}
	return len(names), true
}

// Signs in to a server over the service, showing the link and opening it here, taking the address the browser ended on when it is pasted;
// whether it signed in
func (r *run) signin(name, flow string, settings map[string]any) bool {
	request := &mcpv1.LoginRequest{Name: name, Flow: flow}
	if settings != nil {
		request.Settings = encode(settings)
	}
	stream, err := r.service.Login(r.Context(), request)
	if err != nil {
		r.Failure("Authentication failed: " + err.Error())
		return false
	}
	defer stream.Close()
	events := make(chan *mcpv1.LoginEvent)
	failed := make(chan error, 1)
	go func() {
		defer close(events)
		for {
			event, err := stream.Receive()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					failed <- err
				}
				return
			}
			select {
			case events <- event:
			case <-r.Context().Done():
				return
			}
		}
	}()
	var lines <-chan cli.Line
	var id string
	for {
		select {
		case <-r.Context().Done():
			r.Blank()
			r.Failure("Authentication failed: interrupted")
			return false
		case got := <-lines:
			r.Took()
			lines = nil
			if got.Err != nil || strings.TrimSpace(got.Text) == "" || id == "" {
				continue
			}
			if _, err := r.service.Callback(r.Context(), &mcpv1.CallbackRequest{Id: id, Pasted: strings.TrimSpace(got.Text)}); err != nil {
				r.Failure(err.Error())
				lines = r.Await(r.Paint("  Paste the address: ", cli.Yellow))
			}
		case event, open := <-events:
			if !open {
				select {
				case err := <-failed:
					r.Failure("Authentication failed: " + err.Error())
				default:
					r.Failure("Authentication failed: the sign in ended without an outcome")
				}
				return false
			}
			if event.GetDone() {
				if lines != nil {
					r.Blank()
				}
				return r.signedIn(name, event)
			}
			id = event.GetId()
			if event.GetCode() != "" {
				r.Info("Open this link on any device and enter the code " + r.Paint(event.GetCode(), cli.Bold) + ":")
				r.Say("    "+event.GetLink(), cli.Cyan)
				continue
			}
			r.Info("Approve icebath in the browser:")
			r.Say("    "+event.GetLink(), cli.Cyan)
			if err := browser.Open(r.Context(), event.GetLink()); err != nil {
				r.Info("The browser didn't open; open the link yourself.")
			}
			if r.Interactive() {
				r.Info("If the browser can't reach icebath, paste the address it ended on here.")
				lines = r.Await(r.Paint("  Paste the address: ", cli.Yellow))
			}
		}
	}
}

// Reports how a sign in ended, whether it signed in
func (r *run) signedIn(name string, event *mcpv1.LoginEvent) bool {
	switch {
	case event.GetNoToken():
		r.Warning("Server responded, but no OAuth token was obtained — authentication did not complete.")
		r.Blank()
		r.Info("Some providers (e.g. Google Drive, Atlassian) do not support automatic client registration. For those you must create an OAuth " +
			"client yourself and add its credentials to the server's settings:")
		r.Blank()
		for _, line := range []string{mcpconfig.Servers + "." + name + ":", "  auth: oauth", "  oauth:", `    client_id: "<your-oauth-client-id>"`,
			`    client_secret: "<your-oauth-client-secret>"`} {
			r.Say("    "+line, cli.Dim)
		}
		r.Blank()
		r.Info("Then re-run `icebath mcp login " + name + "`.")
		return false
	case event.GetError() != "":
		r.Failure("Authentication failed: " + event.GetError())
		return false
	case event.GetTools() > 0:
		r.Success(fmt.Sprintf("Authenticated — %d tool(s) available", event.GetTools()))
	default:
		r.Success("Authenticated (server reported no tools)")
	}
	return true
}

// Signs in to a configured OAuth server from scratch, in the browser or on another device; whether it signed in
func (r *run) reauthServer(server *mcpv1.Server, flow string) bool {
	name := server.GetName()
	if server.GetUrl() == "" {
		r.Failure(fmt.Sprintf("Server '%s' has no URL — not an OAuth-capable server", name))
		return false
	}
	if server.GetAuth() != "oauth" {
		r.Failure(fmt.Sprintf("Server '%s' is not configured for OAuth (auth=%s)", name, cmp.Or(server.GetAuth(), "None")))
		r.Info("Use `icebath mcp remove` + `icebath mcp add` to reconfigure auth.")
		return false
	}
	if flow != "" && flow != "browser" && flow != "device" {
		r.Failure("oauth.flow must be browser or device")
		return false
	}
	r.Blank()
	r.Info(fmt.Sprintf("Starting OAuth flow for '%s'...", name))
	return r.signin(name, flow, nil)
}

func (r *run) login(name, flow string) error {
	server, err := r.lookup(name, "Available servers")
	if err != nil || server == nil {
		return err
	}
	if !r.reauthServer(server, flow) {
		return exit(1)
	}
	return nil
}

func (r *run) reauth(name string, all bool) error {
	servers, err := r.servers()
	if err != nil {
		return err
	}
	if all {
		var oauth []*mcpv1.Server
		for _, server := range servers {
			if server.GetAuth() == "oauth" && server.GetUrl() != "" {
				oauth = append(oauth, server)
			}
		}
		if len(oauth) == 0 {
			r.Info("No OAuth-based MCP servers found in config.")
			return nil
		}
		r.Blank()
		r.Info(fmt.Sprintf("Re-authenticating %d OAuth server(s) one at a time...", len(oauth)))
		succeeded := 0
		for _, server := range oauth {
			r.Blank()
			r.Say("  ── "+server.GetName()+" ──", cli.Cyan, cli.Bold)
			if r.reauthServer(server, "") {
				succeeded++
			}
		}
		r.Blank()
		r.Success(fmt.Sprintf("Re-authenticated %d/%d server(s)", succeeded, len(oauth)))
		return nil
	}
	if name == "" {
		r.Failure("Specify a server name, or use --all to re-auth every OAuth server.")
		r.Info("Usage: icebath mcp reauth <name>   |   icebath mcp reauth --all")
		return exit(1)
	}
	return r.login(name, "")
}

// Whether a tool filter entry names the tool: a literal name, or a glob with * ? and [...]
func matches(tool string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.ContainsAny(pattern, "*?[") {
			if globMatch([]rune(pattern), []rune(tool)) {
				return true
			}
		} else if pattern == tool {
			return true
		}
	}
	return false
}

func globMatch(p, s []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for i := len(s); i >= 0; i-- {
				if globMatch(p[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
		case '[':
			end := slices.Index(p[1:], ']')
			if end < 0 {
				if len(s) == 0 || s[0] != '[' {
					return false
				}
				break
			}
			if len(s) == 0 {
				return false
			}
			class, negate := p[1:end+1], false
			if len(class) > 0 && class[0] == '!' {
				class, negate = class[1:], true
			}
			hit := false
			for i := 0; i < len(class); i++ {
				if i+2 < len(class) && class[i+1] == '-' {
					hit = hit || class[i] <= s[0] && s[0] <= class[i+2]
					i += 2
				} else if class[i] == s[0] {
					hit = true
				}
			}
			if hit == negate {
				return false
			}
			p, s = p[end+2:], s[1:]
			continue
		default:
			if len(s) == 0 || s[0] != p[0] {
				return false
			}
		}
		p, s = p[1:], s[1:]
	}
	return len(s) == 0
}

// Exclude list after a checklist edit: its globs kept, the literal names of the tools turned back on dropped and those turned off added,
// with a warning for tools a glob still keeps off
func (r *run) rebuildExclude(name string, exclude, tools []string, chosen map[int]bool) []string {
	var globs []string
	literals := map[string]bool{}
	for _, pattern := range exclude {
		if strings.ContainsAny(pattern, "*?[") {
			globs = append(globs, pattern)
		} else {
			literals[pattern] = true
		}
	}
	var shadowed []string
	for i, tool := range tools {
		if chosen[i] {
			delete(literals, tool)
			if matches(tool, globs) {
				shadowed = append(shadowed, tool)
			}
		} else if !matches(tool, exclude) {
			literals[tool] = true
		}
	}
	if len(shadowed) > 0 {
		slices.Sort(shadowed)
		more := ""
		if len(shadowed) > 5 {
			more = " ..."
		}
		r.Warning(fmt.Sprintf("%d re-enabled tool(s) still match glob exclude pattern(s) %s and stay excluded: %s%s. Remove the pattern from %s.%s.tools.exclude "+
			"to enable them.", len(shadowed), strings.Join(globs, ", "), strings.Join(shadowed[:min(5, len(shadowed))], ", "), more, mcpconfig.Servers, name))
	}
	return append(globs, slices.Sorted(func(yield func(string) bool) {
		for literal := range literals {
			if !yield(literal) {
				return
			}
		}
	})...)
}

// Picks the tools of a server from a checklist over the tools it lists
func (r *run) configure(name string) error {
	if !r.Interactive() {
		return errors.New("'icebath mcp configure' requires an interactive terminal.")
	}
	server, err := r.lookup(name, "Available")
	if err != nil || server == nil {
		return err
	}
	r.Blank()
	r.Say(fmt.Sprintf("  Connecting to '%s' to discover tools...", name), cli.Cyan)
	probe, err := r.service.Probe(r.Context(), &mcpv1.ProbeRequest{Name: name})
	if err != nil {
		return err
	}
	if !probe.GetOk() {
		r.Failure("Failed to connect: " + probe.GetError())
		return exit(1)
	}
	found := probe.GetTools()
	if len(found) == 0 {
		r.Warning("Server reports no tools.")
		return nil
	}
	names := make([]string, len(found))
	labels := make([]string, len(found))
	pre := map[int]bool{}
	for i, t := range found {
		names[i], labels[i] = t.GetName(), t.GetName()+"  —  "+t.GetDescription()
		switch {
		case server.GetIncluded():
			pre[i] = matches(t.GetName(), server.GetInclude())
		case len(server.GetExclude()) > 0:
			pre[i] = !matches(t.GetName(), server.GetExclude())
		default:
			pre[i] = true
		}
	}
	r.Info(fmt.Sprintf("Currently %d/%d tools enabled for '%s'.", len(cli.Picked(pre)), len(found), name))
	r.Blank()
	chosen := r.Checklist(fmt.Sprintf("Select tools for '%s'", name), labels, pre)
	if slices.Equal(cli.Picked(chosen), cli.Picked(pre)) {
		r.Info("No changes made.")
		return nil
	}
	count := len(cli.Picked(chosen))
	request := &mcpv1.SetFilterRequest{Name: name}
	excludeMode := len(server.GetExclude()) > 0 && !server.GetIncluded()
	switch {
	case count == len(found) && !excludeMode:
		request.Clear = true
	case excludeMode:
		request.Mode, request.Values = "exclude", r.rebuildExclude(name, server.GetExclude(), names, chosen)
		request.Clear = len(request.Values) == 0
	default:
		request.Mode = "include"
		for _, i := range cli.Picked(chosen) {
			request.Values = append(request.Values, names[i])
		}
	}
	if _, err := r.service.SetFilter(r.Context(), request); err != nil {
		r.Failure(err.Error())
		return exit(1)
	}
	r.Success(fmt.Sprintf("Updated config: %d/%d tools enabled", count, len(found)))
	r.Info("Run /reload-mcp for changes to take effect.")
	return nil
}

// Statuses the picker shows each entry by
const (
	statusAvailable      = "available"
	statusDisabled       = "installed (disabled)"
	statusEnabled        = "enabled"
	statusCustomEnabled  = "custom — enabled"
	statusCustomDisabled = "custom — disabled"
)

// Row of the picker: a catalog entry, or a custom server when entry is nil
type pickerRow struct {
	name, description, status string
	entry                     *mcpv1.Entry
	server                    *mcpv1.Server
}

func (r *run) rows() ([]pickerRow, []*mcpv1.Entry, error) {
	catalog, err := r.service.Catalog(r.Context(), &mcpv1.CatalogRequest{})
	if err != nil {
		return nil, nil, err
	}
	servers, err := r.servers()
	if err != nil {
		return nil, nil, err
	}
	stored := map[string]*mcpv1.Server{}
	for _, server := range servers {
		stored[server.GetName()] = server
	}
	var rows []pickerRow
	var future []*mcpv1.Entry
	listed := map[string]bool{}
	for _, entry := range catalog.GetEntries() {
		if entry.GetProblem() != "" {
			if entry.GetFuture() {
				future = append(future, entry)
			}
			continue
		}
		listed[entry.GetName()] = true
		row := pickerRow{name: entry.GetName(), description: entry.GetDescription(), status: statusAvailable, entry: entry, server: stored[entry.GetName()]}
		if row.server != nil {
			row.status = statusDisabled
			if row.server.GetEnabled() {
				row.status = statusEnabled
			}
		}
		rows = append(rows, row)
	}
	for _, server := range servers {
		if listed[server.GetName()] {
			continue
		}
		status := statusCustomDisabled
		if server.GetEnabled() {
			status = statusCustomEnabled
		}
		rows = append(rows, pickerRow{name: server.GetName(), description: cmp.Or(server.GetUrl(), server.GetCommand(), "(no transport)"), status: status,
			server: server})
	}
	return rows, future, nil
}

func formatRow(row pickerRow) string {
	return fmt.Sprintf("%s %s %s", cli.Pad(row.name, 18), cli.Pad(row.status, 24), row.description)
}

func (r *run) printRows(rows []pickerRow, future []*mcpv1.Entry) {
	r.Blank()
	if len(rows) == 0 {
		r.Say("  No MCPs in the catalog or configured.", cli.Dim)
		r.Blank()
		return
	}
	r.Say("  MCP Catalog + configured servers:", cli.Cyan, cli.Bold)
	r.Blank()
	fmt.Fprintf(r.Out(), "  %s %s Description\n", cli.Pad("Name", 18), cli.Pad("Status", 24))
	fmt.Fprintf(r.Out(), "  %s %s %s\n", strings.Repeat("-", 18), strings.Repeat("-", 24), strings.Repeat("-", 11))
	for _, row := range rows {
		fmt.Fprintf(r.Out(), "  %s\n", formatRow(row))
	}
	r.Blank()
	r.Say("  Install: icebath mcp install <name>    Picker: icebath mcp", cli.Dim)
	if len(future) > 0 {
		r.Blank()
		for _, entry := range future {
			r.Say(fmt.Sprintf("  ⚠ '%s' requires a newer icebath — update icebath to install this entry.", entry.GetName()), cli.Yellow)
		}
		r.Blank()
	}
	r.Blank()
}

func (r *run) catalog() error {
	rows, future, err := r.rows()
	if err != nil {
		return err
	}
	r.printRows(rows, future)
	return nil
}

// Picker that shows the catalog and the custom servers and acts on the one picked, until it is left; a printed list off a terminal
func (r *run) picker() error {
	for {
		rows, future, err := r.rows()
		if err != nil {
			return err
		}
		if len(rows) == 0 || !r.Interactive() {
			r.printRows(rows, future)
			return nil
		}
		labels := make([]string, len(rows))
		for i, row := range rows {
			labels[i] = formatRow(row)
		}
		i, ok := r.Choose("MCP Catalog  —  pick an entry to act on it", labels)
		if !ok {
			return nil
		}
		if err := r.act(rows[i]); err != nil {
			return err
		}
	}
}

// Installs, turns on or offers what can be done to the row picked, by how it stands
func (r *run) act(row pickerRow) error {
	configure := func() error {
		if err := r.configure(row.name); err != nil && !errors.As(err, new(exit)) {
			return err
		}
		return nil
	}
	if row.entry != nil {
		switch {
		case row.server == nil:
			_, err := r.install(row.entry, "install")
			return err
		case !row.server.GetEnabled():
			return r.enable(row.name, true)
		}
		r.Blank()
		r.Say(fmt.Sprintf("  '%s' is already enabled.", row.name), cli.Dim)
		return r.submenu(fmt.Sprintf("Action for '%s'", row.name), []menuItem{
			{"Configure tools (probe server + re-pick)", configure},
			{"Disable (keep config, stop loading on next session)", func() error { return r.enable(row.name, false) }},
			{"Uninstall (remove config and any cloned files)", func() error { return r.uninstall(row.name) }},
			{"Reinstall (re-clone, re-prompt for credentials)", func() error {
				_, err := r.install(row.entry, "reinstall")
				return err
			}},
		})
	}
	toggle := "Disable"
	if !row.server.GetEnabled() {
		toggle = "Enable"
	}
	return r.submenu(fmt.Sprintf("Action for '%s' (custom)", row.name), []menuItem{
		{"Configure tools (probe server + re-pick)", configure},
		{toggle, func() error { return r.enable(row.name, !row.server.GetEnabled()) }},
		{"Remove from config", func() error { return r.removeCustom(row.server) }},
	})
}

type menuItem struct {
	label string
	run   func() error
}

func (r *run) submenu(title string, items []menuItem) error {
	labels := make([]string, len(items))
	for i, item := range items {
		labels[i] = item.label
	}
	if i, ok := r.Choose(title, labels); ok {
		return items[i].run()
	}
	return nil
}

func (r *run) enable(name string, enabled bool) error {
	if _, err := r.service.Enable(r.Context(), &mcpv1.EnableRequest{Name: name, Enabled: enabled}); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			r.Say(fmt.Sprintf("  '%s' is not installed.", name), cli.Red)
			return nil
		}
		return err
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	r.Say(fmt.Sprintf("  ✓ '%s' %s. Start a new icebath session for changes to take effect.", name, state))
	return nil
}

func (r *run) removeCustom(server *mcpv1.Server) error {
	if plugin := server.GetPlugin(); plugin != "" {
		r.Say(fmt.Sprintf("  '%s' comes from the plugin %s; disable it instead.", server.GetName(), plugin), cli.Red)
		return nil
	}
	if !r.Ask(fmt.Sprintf("Remove '%s' from the MCP servers?", server.GetName()), false) {
		return nil
	}
	if _, err := r.service.Remove(r.Context(), &mcpv1.RemoveRequest{Name: server.GetName()}); err != nil {
		return err
	}
	r.Say(fmt.Sprintf("  ✓ Removed '%s'", server.GetName()))
	return nil
}

func (r *run) uninstall(name string) error {
	if !r.Ask(fmt.Sprintf("Uninstall '%s'?", name), false) {
		return nil
	}
	response, err := r.service.Remove(r.Context(), &mcpv1.RemoveRequest{Name: name, Purge: true})
	if err != nil {
		return err
	}
	if response.GetRemoved() {
		r.Say(fmt.Sprintf("  ✓ Uninstalled '%s'. Credentials in the profile's MCP values preserved — delete them if no longer needed.", name))
	} else {
		r.Say(fmt.Sprintf("  '%s' was not installed", name), cli.Dim)
	}
	return nil
}

func (r *run) installName(name string) error {
	catalog, err := r.service.Catalog(r.Context(), &mcpv1.CatalogRequest{})
	if err != nil {
		return err
	}
	for _, entry := range catalog.GetEntries() {
		if entry.GetName() != name && "official/"+entry.GetName() != name {
			continue
		}
		if entry.GetProblem() != "" {
			r.Say("  ✗ install failed: "+entry.GetProblem(), cli.Red)
			return exit(1)
		}
		ok, err := r.install(entry, "install")
		if err != nil {
			return err
		}
		if !ok {
			return exit(1)
		}
		return nil
	}
	r.Say(fmt.Sprintf("  ✗ '%s' is not in the catalog. Run `icebath mcp catalog` to see available entries.", name), cli.Red)
	return exit(1)
}

// Failure of an install that stops it, printed as the install failing
type installFailure string

func (f installFailure) Error() string { return string(f) }

// Installs a catalog entry: its repository cloned, its credentials asked, its settings kept, a sign in for one that needs it, then its tools
// picked; whether it installed
func (r *run) install(entry *mcpv1.Entry, verb string) (bool, error) {
	err := r.installEntry(entry)
	if failure, ok := errors.AsType[installFailure](err); ok {
		r.Say(fmt.Sprintf("  ✗ %s failed: %s", verb, string(failure)), cli.Red)
		return false, nil
	}
	return err == nil, err
}

func (r *run) installEntry(entry *mcpv1.Entry) error {
	name := entry.GetName()
	r.Blank()
	r.Say(fmt.Sprintf("  Installing MCP '%s'", name), cli.Cyan, cli.Bold)
	if entry.GetDescription() != "" {
		r.Say("  "+entry.GetDescription(), cli.Dim)
	}
	if entry.GetSource() != "" {
		r.Say("  Source: "+entry.GetSource(), cli.Dim)
	}
	r.Blank()
	servers, err := r.servers()
	if err != nil {
		return err
	}
	var prior *mcpv1.Server
	for _, server := range servers {
		if server.GetName() == name {
			prior = server
		}
	}
	dir := ""
	if entry.GetFromSource() {
		cloned, err := r.service.GitInstall(r.Context(), &mcpv1.GitInstallRequest{Entry: name})
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimRight(cloned.GetOutput(), "\n"), "\n") {
			if line != "" {
				r.Say(line, cli.Dim)
			}
		}
		if cloned.GetError() != "" {
			return installFailure(cloned.GetError())
		}
		dir = cloned.GetDir()
	}
	values := map[string]string{}
	if requires := entry.GetRequires(); len(requires) > 0 {
		r.Blank()
		r.Say("  Configure credentials:", cli.Cyan)
		for _, spec := range requires {
			if spec.GetSecret() {
				has, err := r.service.HasValue(r.Context(), &mcpv1.HasValueRequest{Name: spec.GetName()})
				if err != nil {
					return err
				}
				if has.GetSet() {
					r.Say(fmt.Sprintf("  ✓ %s already set", spec.GetName()))
					continue
				}
			}
			value := r.Prompt(cmp.Or(spec.GetPrompt(), spec.GetName()), spec.GetDefault(), spec.GetSecret())
			switch {
			case value != "":
				if spec.GetSecret() {
					if _, err := r.service.SetValue(r.Context(), &mcpv1.SetValueRequest{Name: spec.GetName(), Value: value}); err != nil {
						return err
					}
				}
				values[spec.GetName()] = value
			case spec.GetRequired():
				return installFailure(spec.GetName() + " is required but no value was provided")
			}
		}
	}
	native := entry.GetAuth() == "oauth" && entry.GetProvider() == ""
	switch {
	case entry.GetAuth() == "oauth" && !native:
		r.Say(fmt.Sprintf("  This MCP uses %s OAuth. Sign in to %s in icebath if you have not already.", entry.GetProvider(), entry.GetProvider()), cli.Yellow)
	case native:
		client := "native OAuth 2.1"
		if entry.GetHasOauthClient() {
			client = "your pre-registered OAuth client"
		}
		r.Say(fmt.Sprintf("  This MCP uses %s; you sign in once it is installed (browser flow).", client), cli.Dim)
	}
	config, err := r.service.InstallConfig(r.Context(), &mcpv1.InstallConfigRequest{Entry: name, InstallDir: dir, Values: values})
	if err != nil {
		return err
	}
	saved, err := r.service.Save(r.Context(), &mcpv1.SaveRequest{Name: name, Settings: config.GetSettings()})
	if err != nil {
		return err
	}
	if len(saved.GetIssues()) > 0 {
		for _, issue := range saved.GetIssues() {
			r.Warning(issue)
		}
		return installFailure(fmt.Sprintf("catalog entry '%s' rejected: suspicious command/args configuration", name))
	}
	if native && r.Interactive() {
		r.Blank()
		r.Info(fmt.Sprintf("Starting OAuth flow for '%s'...", name))
		r.signin(name, "", nil)
	}
	if err := r.selectTools(entry, prior); err != nil {
		return err
	}
	r.Blank()
	r.Say(fmt.Sprintf("  ✓ Installed '%s' (enabled). Start a new icebath session to load its tools.", name))
	if note := strings.TrimSpace(entry.GetPostInstall()); note != "" {
		r.Blank()
		for _, line := range strings.Split(note, "\n") {
			r.Say("  "+line, cli.Dim)
		}
	}
	r.Blank()
	return nil
}

// Tool filter of a freshly installed entry: the manifest's exclude list kept, else the tools it lists picked from, falling back to the filter it
// had before or the manifest's when it can't be reached
func (r *run) selectTools(entry *mcpv1.Entry, prior *mcpv1.Server) error {
	r.Blank()
	name := entry.GetName()
	hint := fmt.Sprintf("`icebath mcp configure %s`", name)
	var priorInclude, priorExclude []string
	if prior != nil {
		if prior.GetIncluded() {
			priorInclude = append([]string{}, prior.GetInclude()...)
		} else if len(prior.GetExclude()) > 0 {
			priorExclude = prior.GetExclude()
		}
	}
	setFilter := func(mode string, values []string) error {
		_, err := r.service.SetFilter(r.Context(), &mcpv1.SetFilterRequest{Name: name, Mode: mode, Values: values, Clear: values == nil})
		return err
	}
	if len(entry.GetDefaultExcluded()) > 0 && priorInclude == nil {
		edit := fmt.Sprintf("Edit %s.%s.tools.exclude or run %s to change.", mcpconfig.Servers, name, hint)
		if priorExclude != nil {
			r.Say(fmt.Sprintf("  Kept your existing exclude list (%d entries). %s", len(priorExclude), edit))
		} else {
			r.Say(fmt.Sprintf("  Applied manifest exclude list (%d entries); everything else stays enabled. %s", len(entry.GetDefaultExcluded()), edit))
		}
		return nil
	}
	r.Say(fmt.Sprintf("  Probing '%s' for available tools...", name), cli.Cyan)
	probe, err := r.service.Probe(r.Context(), &mcpv1.ProbeRequest{Name: name})
	if err != nil {
		return err
	}
	if !probe.GetOk() {
		r.Say("  Probe failed: "+probe.GetError(), cli.Yellow)
		refine := fmt.Sprintf("Run %s after the server is reachable to refine.", hint)
		switch {
		case priorInclude != nil:
			r.Say(fmt.Sprintf("  Couldn't probe server. Kept your previous tool selection (%d tools). %s", len(priorInclude), refine), cli.Yellow)
		case priorExclude != nil:
			r.Say(fmt.Sprintf("  Couldn't probe server. Kept your existing exclude list (%d entries).", len(priorExclude)), cli.Yellow)
		case len(entry.GetDefaultEnabled()) > 0:
			r.Say(fmt.Sprintf("  Couldn't probe server. Applied manifest default (%d tools). %s", len(entry.GetDefaultEnabled()), refine), cli.Yellow)
		default:
			r.Say(fmt.Sprintf("  Couldn't probe server; installed with no tool filter (all tools enabled when reachable). Run %s after first connect "+
				"to prune.", hint), cli.Yellow)
		}
		return nil
	}
	found := probe.GetTools()
	if len(found) == 0 {
		r.Say("  Server reported no tools.", cli.Yellow)
		return nil
	}
	names := make([]string, len(found))
	for i, t := range found {
		names[i] = t.GetName()
	}
	preferred := priorInclude
	if preferred == nil && len(entry.GetDefaultEnabled()) > 0 {
		preferred = entry.GetDefaultEnabled()
	}
	if !r.Interactive() {
		if preferred == nil {
			return setFilter("include", nil)
		}
		kept := []string{}
		for _, n := range preferred {
			if slices.Contains(names, n) {
				kept = append(kept, n)
			}
		}
		return setFilter("include", kept)
	}
	if preferred == nil {
		preferred = names
	}
	pre := map[int]bool{}
	labels := make([]string, len(found))
	for i, t := range found {
		pre[i] = slices.Contains(preferred, t.GetName())
		labels[i] = fmt.Sprintf("%s  —  %s", t.GetName(), strs.Clip(t.GetDescription(), 60))
	}
	r.Say(fmt.Sprintf("  Found %d tool(s). Pre-checked: %d.", len(found), len(cli.Picked(pre))))
	chosen := cli.Picked(r.Checklist(fmt.Sprintf("Select tools for '%s'", name), labels, pre))
	switch len(chosen) {
	case 0:
		if err := setFilter("include", []string{}); err != nil {
			return err
		}
		r.Say(fmt.Sprintf("  No tools selected. Run %s to change.", hint), cli.Yellow)
	case len(found):
		if err := setFilter("include", nil); err != nil {
			return err
		}
		r.Say(fmt.Sprintf("  ✓ All %d tools enabled (no filter — new tools the server adds later will be auto-enabled).", len(found)))
	default:
		picked := make([]string, len(chosen))
		for i, j := range chosen {
			picked[i] = names[j]
		}
		if err := setFilter("include", picked); err != nil {
			return err
		}
		r.Say(fmt.Sprintf("  ✓ %d/%d tools enabled.", len(picked), len(found)))
	}
	return nil
}
