package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/nickheyer/icebath/pkg/tui"
)

// Failure of a command with the exit status it ends with, its lines already printed
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

// Prints the commands of `icebath mcp` as each declares its use, with what each does
func (r *run) commands(root *cobra.Command) {
	var rows [][2]string
	width := 0
	for _, sub := range root.Commands() {
		if !sub.IsAvailableCommand() || sub.Name() == "help" {
			continue
		}
		use := root.CommandPath() + " " + sub.Use
		rows = append(rows, [2]string{use, sub.Short})
		width = max(width, cli.Width(use))
	}
	r.Say("  Commands:", cli.Cyan)
	for _, row := range rows {
		r.Info(cli.Pad(row[0], width) + "  " + row[1])
	}
}

// Reason a server gave as a sentence: capital first letter, a period at the end
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.ContainsAny(text[len(text)-1:], ".!?") {
		text += "."
	}
	return text
}

// Why a call to the service failed, as a sentence
func reason(err error) string { return sentence(tui.Reason(err)) }

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
		Use:     "mcp",
		GroupID: cli.GroupAbilities,
		Short:   "Add, remove and test MCP servers",
		Long: "MCP servers give the agent more tools, such as access to GitHub or a database. icebath comes with a catalog of known servers " +
			"you can install, and you can add your own. With no subcommand, mcp opens the catalog picker and then lists these commands.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := &run{Console: cli.NewConsole(cmd), service: service}
			if err := r.picker(); err != nil {
				return err
			}
			r.commands(cmd)
			r.Blank()
			return nil
		},
	}
	var flow string
	login := &cobra.Command{
		Use:     "login <name>",
		Short:   "Sign in to an MCP server again",
		Long:    "Signs in again to an MCP server that uses OAuth, in the browser or with a device code.",
		Example: "  icebath mcp login linear\n  icebath mcp login linear --flow device",
		Args:    cobra.ExactArgs(1),
		RunE:    with(func(r *run, args []string) error { return r.login(args[0], flow) }),
	}
	login.Flags().StringVar(&flow, "flow", "", "How to sign in: browser or device, instead of the server's own setting")
	var all bool
	reauth := &cobra.Command{
		Use:   "reauth [name]",
		Short: "Sign in again to one MCP server, or all with --all",
		Long:  "Signs in again to the MCP server you name, or with --all to every server that uses OAuth, one at a time.",
		Args:  cobra.MaximumNArgs(1),
		RunE: with(func(r *run, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return r.reauth(name, all)
		}),
	}
	reauth.Flags().BoolVar(&all, "all", false, "Sign in to every server that uses OAuth, one at a time")
	root.AddCommand(
		&cobra.Command{Use: "picker", Short: "Choose servers to install from the catalog",
			Long: "Opens a list of the servers in the catalog in this terminal, where you pick the ones to install.", Args: cobra.NoArgs,
			RunE: with(func(r *run, _ []string) error { return r.picker() })},
		&cobra.Command{Use: "catalog", Short: "List the MCP servers in the catalog",
			Long: "Lists the known MCP servers that come with icebath, ready to install by name.", Args: cobra.NoArgs,
			RunE: with(func(r *run, _ []string) error { return r.catalog() })},
		&cobra.Command{Use: "install <name>", Short: "Install an MCP server from the catalog",
			Long: "Installs a server from the catalog by the name that mcp catalog lists.", Example: "  icebath mcp install linear",
			Args: cobra.ExactArgs(1),
			RunE: with(func(r *run, args []string) error { return r.installName(args[0]) })},
		add(with),
		&cobra.Command{Use: "remove <name>", Aliases: []string{"rm"}, Short: "Remove an MCP server",
			Long: "Removes the MCP server and its settings from this profile.", Args: cobra.ExactArgs(1),
			RunE: with(func(r *run, args []string) error { return r.remove(args[0]) })},
		&cobra.Command{Use: "list", Aliases: []string{"ls"}, Short: "List your MCP servers",
			Long: "Lists the MCP servers of this profile with how each one runs and whether it is on.", Args: cobra.NoArgs,
			RunE: with(func(r *run, _ []string) error { return r.list() })},
		&cobra.Command{Use: "test <name>", Short: "Connect to an MCP server and list its tools",
			Long: "Connects to the MCP server and lists the tools it offers, to check that it works.", Args: cobra.ExactArgs(1),
			RunE: with(func(r *run, args []string) error { return r.test(args[0]) })},
		&cobra.Command{Use: "configure <name>", Aliases: []string{"config"}, Short: "Choose which tools of an MCP server the agent gets",
			Long: "Opens a list of the server's tools in this terminal, where you turn each tool on or off for the agent.", Args: cobra.ExactArgs(1),
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
		Short: "Add an MCP server of your own",
		Long: "Adds a server that is not in the catalog. Give the address of an HTTP server with --url, the command that runs a local server " +
			"with --command, or a known server with --preset. Put the arguments of the command after --args or --.",
		Example: `  icebath mcp add ink --url "https://mcp.ml.ink/mcp"
  icebath mcp add github --command npx --args @modelcontextprotocol/server-github`,
		DisableFlagParsing: true,
	}
	flags := cmd.Flags()
	flags.String("url", "", "Address of an HTTP server")
	flags.String("command", "", "Command that runs a local server")
	flags.String("auth", "", "How you sign in to an HTTP server: oauth or header")
	flags.String("preset", "", "Known server to add: "+strings.Join(slices.Sorted(func(yield func(string) bool) {
		for name := range presets {
			if !yield(name) {
				return
			}
		}
	}), ", "))
	flags.StringArray("env", nil, "Variable to set for a local server, as KEY=VALUE, repeatable")
	flags.Float64("connect-timeout", 0, "Time limit in seconds to connect")
	flags.BoolP("help", "h", false, "Show this help")
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
			return errors.New("Name the server to add, such as icebath mcp add github --command npx.")
		}
		if len(positionals) > 1 {
			return errors.New("Give one name only, not also " + strings.Join(positionals[1:], " ") + ". Put the arguments of the command after --args.")
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
func (r *run) lookup(name string) (*mcpv1.Server, error) {
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
	r.Failure("No MCP server is called “" + name + "”.")
	if len(names) > 0 {
		r.Info("Your servers: " + strings.Join(names, ", "))
	} else {
		r.Info("You have no MCP servers yet. Add one with icebath mcp install <name> or icebath mcp add.")
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
		r.Info("You have no MCP servers yet.")
		r.Blank()
		r.Info("Add one from the catalog with icebath mcp install <name>, or one of your own with:")
		r.Info("  icebath mcp add <name> --url <endpoint>")
		r.Info("  icebath mcp add <name> --command <cmd> --args <arg>...")
		r.Blank()
		return nil
	}
	r.Blank()
	r.Say("  Your MCP servers:", cli.Cyan, cli.Bold)
	r.Blank()
	var rows [][]string
	for _, server := range servers {
		runs := "Not set"
		if server.GetUrl() != "" {
			runs = server.GetUrl()
		} else if server.GetCommand() != "" {
			runs = strings.Join(append([]string{server.GetCommand()}, server.GetArgs()[:min(2, len(server.GetArgs()))]...), " ")
		}
		if r := []rune(runs); len(r) > 40 {
			runs = string(r[:39]) + "…"
		}
		filter := "All"
		switch {
		case server.GetIncluded():
			filter = strs.Number(float64(len(server.GetInclude()))) + " chosen"
		case len(server.GetExclude()) > 0:
			filter = strs.Number(float64(len(server.GetExclude()))) + " left out"
		}
		rows = append(rows, []string{server.GetName(), runs, filter, r.OnOff(server.GetEnabled())})
	}
	r.Table([]string{"Name", "Address or command", "Tools", "State"}, rows)
	r.Blank()
	return nil
}

func (r *run) remove(name string) error {
	server, err := r.lookup(name)
	if err != nil || server == nil {
		return err
	}
	if plugin := server.GetPlugin(); plugin != "" {
		r.Failure(fmt.Sprintf("%s comes from the plugin %s, so it cannot be removed here. Turn it off instead.", name, plugin))
		return exit(1)
	}
	if !r.Ask(fmt.Sprintf("Remove the MCP server %s? Its settings and sign-in are deleted.", name), true) {
		r.Info("Nothing was removed.")
		return nil
	}
	if _, err := r.service.Remove(r.Context(), &mcpv1.RemoveRequest{Name: name}); err != nil {
		r.Failure(reason(err))
		return exit(1)
	}
	r.Success(fmt.Sprintf("Removed the MCP server %s.", name))
	if server.GetOauth() {
		r.Success("Deleted its sign-in.")
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
	server, err := r.lookup(name)
	if err != nil {
		return err
	}
	if server == nil {
		return exit(3)
	}
	settings := settingsOf(server)
	r.Blank()
	r.Say(fmt.Sprintf("  Connecting to %s…", name), cli.Cyan)
	if server.GetUrl() != "" {
		r.Info("Address: " + server.GetUrl())
	} else {
		r.Info("Command: " + cmp.Or(server.GetCommand(), "Not set"))
	}
	headers, _ := settings["headers"].(map[string]any)
	switch {
	case server.GetAuth() == "oauth":
		r.Info("Sign-in: OAuth")
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
				r.Info("Header " + key + ": " + redactHeader(value))
			}
		}
	default:
		r.Info("Sign-in: None")
	}
	start := time.Now()
	probe, err := r.service.Probe(r.Context(), &mcpv1.ProbeRequest{Name: name})
	if err != nil {
		return err
	}
	if !probe.GetOk() {
		r.Failure(fmt.Sprintf("Could not connect to %s after %s. %s", name, strs.Span(time.Since(start).Round(100*time.Millisecond)),
			sentence(probe.GetError())))
		r.Info(probe.GetNextStep())
		return exit(1)
	}
	r.Success(fmt.Sprintf("Connected to %s in %s.", name, strs.Span(time.Since(start).Round(time.Millisecond))))
	r.Success(fmt.Sprintf("It offers %s.", strs.Count(len(probe.GetTools()), "tool")))
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
			return nil, fmt.Errorf("“%s” is not a variable. Give --env as NAME=value.", text)
		}
		if key = strings.TrimSpace(key); key == "" {
			return nil, fmt.Errorf("“%s” has no variable name. Give --env as NAME=value.", text)
		}
		if !envNameRE.MatchString(key) {
			return nil, fmt.Errorf("“%s” cannot name a variable. Use letters, digits and underscores, and do not start with a digit.", key)
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
		r.Failure(fmt.Sprintf("The MCP server %s was not saved, because its settings look like an attack.", name))
		return false, nil
	}
	return true, nil
}

func (r *run) save(name string, settings map[string]any) bool {
	response, err := r.service.Save(r.Context(), &mcpv1.SaveRequest{Name: name, Settings: encode(settings)})
	if err != nil {
		r.Failure(reason(err))
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
			err = fmt.Errorf("No preset is called “%s”. Choose one of %s.", a.preset, strings.Join(slices.Sorted(maps.Keys(presets)), ", "))
		case url == "" && command == "":
			url, command, args = preset.url, preset.command, preset.args
		}
	}
	if err != nil {
		r.Failure(err.Error())
		return exit(1)
	}
	if url != "" && len(env) > 0 {
		r.Failure("--env works only for a server that runs on this computer, given with --command or a preset.")
		return exit(1)
	}
	if url == "" && command == "" {
		r.Failure("Give the server with --url <endpoint>, --command <cmd> or --preset <name>.")
		r.Info("For example:")
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
		!r.Ask(fmt.Sprintf("The MCP server %s already exists. Replace its settings?", name), false) {
		r.Info("Nothing was changed.")
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
	r.Say(fmt.Sprintf("  Connecting to %s…", name), cli.Cyan)
	request := &mcpv1.ProbeRequest{Name: name, Settings: encode(settings)}
	if a.connectTimeout != nil {
		request.ConnectTimeout = *a.connectTimeout
	}
	probe, err := r.service.Probe(r.Context(), request)
	if err != nil {
		return err
	}
	if !probe.GetOk() {
		r.Failure(fmt.Sprintf("Could not connect to %s. %s", name, sentence(probe.GetError())))
		r.Info(probe.GetNextStep())
		if r.Ask("Save the server anyway, turned off? You can test it later.", false) {
			settings["enabled"] = false
			if r.save(name, settings) {
				r.Success(fmt.Sprintf("Saved %s, turned off.", name))
				r.Info("Once it is fixed, run icebath mcp test " + name + ".")
			}
		}
		return nil
	}
	found := probe.GetTools()
	if len(found) == 0 {
		r.Warning("The server connected but offers no tools.")
		if r.Ask("Save the server anyway?", true) && r.save(name, settings) {
			r.Success(fmt.Sprintf("Saved %s.", name))
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
		r.Success(fmt.Sprintf("Saved %s, with %s of its %s on.", name, strs.Number(float64(count)), strs.Count(len(found), "tool")))
		r.Info("Start a new session to use these tools, or run /reload-mcp to load them into open sessions.")
	}
	return nil
}

// OAuth sign in or bearer token of an HTTP server, false when the user cancelled
func (r *run) httpAuth(name, url, auth string, settings map[string]any) (bool, error) {
	r.Blank()
	if auth == "oauth" {
		settings["auth"] = "oauth"
		r.Info(fmt.Sprintf("Signing in to %s with OAuth…", name))
		if r.signin(name, "", settings) {
			r.Success("Signed in with OAuth.")
			return true, nil
		}
		delete(settings, "auth")
		r.Info("This server may not sign in with OAuth.")
		if !r.Ask("Add the server without a sign-in?", true) {
			r.Info("Nothing was added.")
			return false, nil
		}
		return true, nil
	}
	r.Info("Address: " + url)
	if !r.Ask("Does this server need an API key or token?", true) || auth != "header" && auth != "" {
		return true, nil
	}
	key := mcp.KeyEnv(name)
	has, err := r.service.HasValue(r.Context(), &mcpv1.HasValueRequest{Name: key})
	if err != nil {
		return false, err
	}
	if has.GetSet() {
		r.Success("The value " + key + " is already set.")
		settings["headers"] = map[string]any{"Authorization": "Bearer ${" + key + "}"}
		return true, nil
	}
	token := r.Prompt("API key or token", "", true)
	if token == "" {
		return true, nil
	}
	saved, err := r.service.SaveBearer(r.Context(), &mcpv1.SaveBearerRequest{Name: name, Token: token})
	if err != nil {
		r.Failure(reason(err))
		return false, nil
	}
	var headers map[string]any
	json.Unmarshal([]byte(saved.GetHeaders()), &headers)
	settings["headers"] = headers
	r.Success("Saved it in /settings under Tools › MCP servers › Values as " + saved.GetKey() + ".")
	return true, nil
}

// Asks whether to keep every tool, none, or a pick; the count kept, false when cancelled
func (r *run) chooseTools(name string, found []*mcpv1.ToolSummary, settings map[string]any) (int, bool) {
	r.Blank()
	r.Success(fmt.Sprintf("Connected to %s, which offers %s:", name, strs.Count(len(found), "tool")))
	r.Blank()
	r.printTools(found, 40, 60)
	r.Blank()
	choice, ok := r.Input(r.Paint(fmt.Sprintf("  Turn on all %s? Type n for none or select to choose them [Y/n/select]: ",
		strs.Count(len(found), "tool")), cli.Yellow))
	if !ok {
		r.Blank()
		r.Info("Nothing was saved.")
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "n", "no":
		r.Info("The server was not saved.")
		return 0, false
	case "s", "select":
	default:
		return len(found), true
	}
	labels := make([]string, len(found))
	every := map[int]bool{}
	for i, t := range found {
		labels[i], every[i] = t.GetName()+"  "+t.GetDescription(), true
	}
	chosen := cli.Picked(r.Checklist("Tools of "+name+" for the agent", labels, every))
	if len(chosen) == 0 {
		r.Info("No tool was chosen, so the server was not saved.")
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
		r.Failure("Could not sign in. " + reason(err))
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
			r.Failure("The sign-in stopped, as you interrupted it.")
			return false
		case got := <-lines:
			r.Took()
			lines = nil
			if got.Err != nil || strings.TrimSpace(got.Text) == "" || id == "" {
				continue
			}
			if _, err := r.service.Callback(r.Context(), &mcpv1.CallbackRequest{Id: id, Pasted: strings.TrimSpace(got.Text)}); err != nil {
				r.Failure(reason(err))
				lines = r.Await(r.Paint("  Paste the address: ", cli.Yellow))
			}
		case event, open := <-events:
			if !open {
				select {
				case err := <-failed:
					r.Failure("Could not sign in. " + reason(err))
				default:
					r.Failure("The sign-in ended without saying whether it worked.")
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
				r.Info("The browser did not open, so open the link yourself.")
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
		r.Warning("The server answered but gave no sign-in token, so the sign-in did not finish.")
		r.Blank()
		r.Info("Providers such as Google Drive and Atlassian do not let icebath register itself. Create an OAuth client there and add it to the " +
			"settings of the server, like this:")
		r.Blank()
		for _, line := range []string{mcpconfig.Servers + "." + name + ":", "  auth: oauth", "  oauth:", `    client_id: "<your-oauth-client-id>"`,
			`    client_secret: "<your-oauth-client-secret>"`} {
			r.Say("    "+line, cli.Dim)
		}
		r.Blank()
		r.Info("Then run icebath mcp login " + name + " again.")
		return false
	case event.GetError() != "":
		r.Failure("Could not sign in. " + sentence(event.GetError()))
		return false
	case event.GetTools() > 0:
		r.Success("Signed in. The server offers " + strs.Count(int(event.GetTools()), "tool") + ".")
	default:
		r.Success("Signed in, but the server offers no tools.")
	}
	return true
}

// Signs in to a configured OAuth server from scratch, in the browser or on another device; whether it signed in
func (r *run) reauthServer(server *mcpv1.Server, flow string) bool {
	name := server.GetName()
	if server.GetUrl() == "" {
		r.Failure(fmt.Sprintf("The MCP server %s runs on this computer and has no address, so it has no sign-in.", name))
		return false
	}
	if server.GetAuth() != "oauth" {
		r.Failure(fmt.Sprintf("The MCP server %s does not sign in with OAuth, as its Sign-in is %s.", name, cmp.Or(server.GetAuth(), "Headers only")))
		r.Info("Set its Sign-in to OAuth in /settings under Tools › MCP servers › Your servers.")
		return false
	}
	if flow != "" && flow != "browser" && flow != "device" {
		r.Failure("--flow must be browser or device.")
		return false
	}
	r.Blank()
	r.Info(fmt.Sprintf("Signing in to %s with OAuth…", name))
	return r.signin(name, flow, nil)
}

func (r *run) login(name, flow string) error {
	server, err := r.lookup(name)
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
			r.Info("None of your MCP servers signs in with OAuth.")
			return nil
		}
		r.Blank()
		r.Info("Signing in again to " + strs.Count(len(oauth), "server") + ", one at a time.")
		succeeded := 0
		for _, server := range oauth {
			r.Blank()
			r.Say("  "+server.GetName(), cli.Cyan, cli.Bold)
			if r.reauthServer(server, "") {
				succeeded++
			}
		}
		r.Blank()
		r.Success("Signed in again to " + strs.Number(float64(succeeded)) + " of " + strs.Count(len(oauth), "server") + ".")
		return nil
	}
	if name == "" {
		r.Failure("Name a server, or pass --all to sign in again to every server that uses OAuth.")
		r.Info("Usage: icebath mcp reauth [name]")
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
			more = " and " + strs.Number(float64(len(shadowed)-5)) + " more"
		}
		r.Warning(fmt.Sprintf("%s you turned back on still match %s, so they stay off: %s%s. Remove the pattern from the Tool filter of %s in "+
			"/settings under Tools › MCP servers › Your servers to turn them on.", strs.Count(len(shadowed), "tool"), strings.Join(globs, ", "),
			strings.Join(shadowed[:min(5, len(shadowed))], ", "), more, name))
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
		return errors.New("Choosing the tools of a server needs a terminal you can type in.")
	}
	server, err := r.lookup(name)
	if err != nil || server == nil {
		return err
	}
	r.Blank()
	r.Say(fmt.Sprintf("  Connecting to %s to list its tools…", name), cli.Cyan)
	probe, err := r.service.Probe(r.Context(), &mcpv1.ProbeRequest{Name: name})
	if err != nil {
		return err
	}
	if !probe.GetOk() {
		r.Failure(fmt.Sprintf("Could not connect to %s. %s", name, sentence(probe.GetError())))
		return exit(1)
	}
	found := probe.GetTools()
	if len(found) == 0 {
		r.Warning("The server offers no tools.")
		return nil
	}
	names := make([]string, len(found))
	labels := make([]string, len(found))
	pre := map[int]bool{}
	for i, t := range found {
		names[i], labels[i] = t.GetName(), t.GetName()+"  "+t.GetDescription()
		switch {
		case server.GetIncluded():
			pre[i] = matches(t.GetName(), server.GetInclude())
		case len(server.GetExclude()) > 0:
			pre[i] = !matches(t.GetName(), server.GetExclude())
		default:
			pre[i] = true
		}
	}
	r.Info(fmt.Sprintf("On now for %s: %s of %s.", name, strs.Number(float64(len(cli.Picked(pre)))), strs.Count(len(found), "tool")))
	r.Blank()
	chosen := r.Checklist("Tools of "+name+" for the agent", labels, pre)
	if slices.Equal(cli.Picked(chosen), cli.Picked(pre)) {
		r.Info("Nothing was changed.")
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
		r.Failure(reason(err))
		return exit(1)
	}
	r.Success(fmt.Sprintf("Saved, with %s of %s on.", strs.Number(float64(count)), strs.Count(len(found), "tool")))
	r.Info("The change takes effect in new sessions, or after /reload-mcp in an open one.")
	return nil
}

// Statuses the picker shows each entry by
const (
	statusAvailable      = "Not installed"
	statusDisabled       = "Installed, off"
	statusEnabled        = "Installed, on"
	statusCustomEnabled  = "Your own, on"
	statusCustomDisabled = "Your own, off"
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
		rows = append(rows, pickerRow{name: server.GetName(), description: cmp.Or(server.GetUrl(), server.GetCommand(), "No address or command"), status: status,
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
		r.Say("  The catalog is empty and you have no MCP servers yet. Add one with icebath mcp add.", cli.Dim)
		r.Blank()
		return
	}
	r.Say("  The catalog and your MCP servers:", cli.Cyan, cli.Bold)
	r.Blank()
	var table [][]string
	for _, row := range rows {
		table = append(table, []string{row.name, row.status, row.description})
	}
	r.Table([]string{"Name", "State", "About"}, table)
	r.Blank()
	r.Say("  Install one with icebath mcp install <name>, or pick from a list with icebath mcp picker.", cli.Dim)
	if len(future) > 0 {
		r.Blank()
		for _, entry := range future {
			r.Warning(fmt.Sprintf("“%s” needs a newer icebath. Update icebath to install it.", entry.GetName()))
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
		i, ok := r.Choose("Pick a server to install or change", labels)
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
		r.Say(fmt.Sprintf("  %s is installed and on.", row.name), cli.Dim)
		return r.submenu("What to do with "+row.name, []menuItem{
			{"Choose its tools again", configure},
			{"Turn it off, keeping its settings", func() error { return r.enable(row.name, false) }},
			{"Uninstall it, deleting its settings and downloaded files", func() error { return r.uninstall(row.name) }},
			{"Install it again, downloading it and asking for its keys again", func() error {
				_, err := r.install(row.entry, "reinstall")
				return err
			}},
		})
	}
	toggle := "Turn it off"
	if !row.server.GetEnabled() {
		toggle = "Turn it on"
	}
	return r.submenu("What to do with "+row.name+", a server of your own", []menuItem{
		{"Choose its tools again", configure},
		{toggle, func() error { return r.enable(row.name, !row.server.GetEnabled()) }},
		{"Remove it", func() error { return r.removeCustom(row.server) }},
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
			r.Failure(name + " is not installed.")
			return nil
		}
		return err
	}
	state := "off"
	if enabled {
		state = "on"
	}
	r.Success(fmt.Sprintf("%s is now %s. The change takes effect in new sessions.", name, state))
	return nil
}

func (r *run) removeCustom(server *mcpv1.Server) error {
	if plugin := server.GetPlugin(); plugin != "" {
		r.Failure(fmt.Sprintf("%s comes from the plugin %s, so it cannot be removed here. Turn it off instead.", server.GetName(), plugin))
		return nil
	}
	if !r.Ask(fmt.Sprintf("Remove the MCP server %s? Its settings are deleted.", server.GetName()), false) {
		return nil
	}
	if _, err := r.service.Remove(r.Context(), &mcpv1.RemoveRequest{Name: server.GetName()}); err != nil {
		return err
	}
	r.Success("Removed " + server.GetName() + ".")
	return nil
}

func (r *run) uninstall(name string) error {
	if !r.Ask(fmt.Sprintf("Uninstall %s? Its settings and downloaded files are deleted.", name), false) {
		return nil
	}
	response, err := r.service.Remove(r.Context(), &mcpv1.RemoveRequest{Name: name, Purge: true})
	if err != nil {
		return err
	}
	if response.GetRemoved() {
		r.Success("Uninstalled " + name + ". Its keys stay in /settings under Tools › MCP servers › Values, so delete them there if you no longer need them.")
	} else {
		r.Say("  "+name+" was not installed.", cli.Dim)
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
			r.Failure("Could not install " + entry.GetName() + ". " + sentence(entry.GetProblem()))
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
	r.Failure("“" + name + "” is not in the catalog. Run icebath mcp catalog to see what it holds.")
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
		what := "install"
		if verb == "reinstall" {
			what = "install again"
		}
		r.Failure("Could not " + what + " " + entry.GetName() + ". " + sentence(string(failure)))
		return false, nil
	}
	return err == nil, err
}

func (r *run) installEntry(entry *mcpv1.Entry) error {
	name := entry.GetName()
	r.Blank()
	r.Say("  Installing the MCP server "+name, cli.Cyan, cli.Bold)
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
		r.Say("  Keys and settings it needs:", cli.Cyan)
		for _, spec := range requires {
			if spec.GetSecret() {
				has, err := r.service.HasValue(r.Context(), &mcpv1.HasValueRequest{Name: spec.GetName()})
				if err != nil {
					return err
				}
				if has.GetSet() {
					r.Success(spec.GetName() + " is already set.")
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
				return installFailure(spec.GetName() + " is needed, and no value was given.")
			}
		}
	}
	native := entry.GetAuth() == "oauth" && entry.GetProvider() == ""
	switch {
	case entry.GetAuth() == "oauth" && !native:
		r.Say(fmt.Sprintf("  This server signs in with your %s account. Connect %s in /providers if you have not yet.", entry.GetProvider(), entry.GetProvider()),
			cli.Yellow)
	case native:
		client := "OAuth"
		if entry.GetHasOauthClient() {
			client = "your own OAuth client"
		}
		r.Say(fmt.Sprintf("  This server signs in with %s in the browser, once it is installed.", client), cli.Dim)
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
		return installFailure(fmt.Sprintf("The settings of the catalog entry %s look like an attack, so it was not installed.", name))
	}
	if native && r.Interactive() {
		r.Blank()
		r.Info(fmt.Sprintf("Signing in to %s with OAuth…", name))
		r.signin(name, "", nil)
	}
	if err := r.selectTools(entry, prior); err != nil {
		return err
	}
	r.Blank()
	r.Success("Installed " + name + " and turned it on. Its tools appear in new sessions.")
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
	hint := "icebath mcp configure " + name
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
		edit := "To change it, edit the Tool filter of " + name + " in /settings under Tools › MCP servers › Your servers, or run " + hint + "."
		if priorExclude != nil {
			r.Say("  Kept your list of " + strs.Count(len(priorExclude), "tool") + " to leave out. " + edit)
		} else {
			r.Say("  Left out the " + strs.Count(len(entry.GetDefaultExcluded()), "tool") + " that the catalog leaves out, and turned on the rest. " + edit)
		}
		return nil
	}
	r.Say(fmt.Sprintf("  Asking %s for its tools…", name), cli.Cyan)
	probe, err := r.service.Probe(r.Context(), &mcpv1.ProbeRequest{Name: name})
	if err != nil {
		return err
	}
	if !probe.GetOk() {
		r.Warning("Could not reach " + name + ". " + sentence(probe.GetError()))
		refine := "Once the server can be reached, run " + hint + " to choose."
		switch {
		case priorInclude != nil:
			r.Say("  Kept your earlier choice of "+strs.Count(len(priorInclude), "tool")+". "+refine, cli.Yellow)
		case priorExclude != nil:
			r.Say("  Kept your list of "+strs.Count(len(priorExclude), "tool")+" to leave out.", cli.Yellow)
		case len(entry.GetDefaultEnabled()) > 0:
			r.Say("  Turned on the "+strs.Count(len(entry.GetDefaultEnabled()), "tool")+" that the catalog suggests. "+refine, cli.Yellow)
		default:
			r.Say("  Installed with every tool on. Once it connects, run "+hint+" to turn some off.", cli.Yellow)
		}
		return nil
	}
	found := probe.GetTools()
	if len(found) == 0 {
		r.Warning("The server offers no tools.")
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
		labels[i] = t.GetName() + "  " + strs.Clip(t.GetDescription(), 60)
	}
	r.Say("  It offers " + strs.Count(len(found), "tool") + ", and " + strs.Number(float64(len(cli.Picked(pre)))) + " are checked to start with.")
	chosen := cli.Picked(r.Checklist("Tools of "+name+" for the agent", labels, pre))
	switch len(chosen) {
	case 0:
		if err := setFilter("include", []string{}); err != nil {
			return err
		}
		r.Warning("No tool is on. Run " + hint + " to change that.")
	case len(found):
		if err := setFilter("include", nil); err != nil {
			return err
		}
		r.Success("All " + strs.Count(len(found), "tool") + " are on. Tools the server adds later are turned on too.")
	default:
		picked := make([]string, len(chosen))
		for i, j := range chosen {
			picked[i] = names[j]
		}
		if err := setFilter("include", picked); err != nil {
			return err
		}
		r.Success(strs.Number(float64(len(picked))) + " of " + strs.Count(len(found), "tool") + " are on.")
	}
	return nil
}
