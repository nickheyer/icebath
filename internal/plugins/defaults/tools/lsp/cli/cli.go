// The lsp commands: how the language servers that check the agent's writes stand, installing them, adding your own, turning them off or on,
// stopping the running ones and trusting the folders they were kept out of, each through the function the page of Language servers uses
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	picked "github.com/nickheyer/icebath/internal/profile"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1/lspv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Exit status a command ends with, its lines already printed
type exit int

func (e exit) Error() string { return "" }

func (e exit) ExitCode() int { return int(e) }

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := lspv1connect.NewLspServiceClient(conn)
	root := &cobra.Command{
		Use:     "lsp",
		GroupID: cli.GroupAbilities,
		Short:   "Install and check the language servers",
		Long: "After the agent writes a file, a language server for that language checks it and tells the agent what is wrong. Use these " +
			"commands to see how the language servers stand, install them, add your own, turn them off or on and stop the running ones. With " +
			"no command after it, lsp shows how they stand.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "✗ There is no lsp command called “%s”. Run icebath lsp --help to see them.\n", args[0])
				return exit(2)
			}
			return status(cmd, service, false)
		},
	}
	var asJSON, installedOnly bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show how the language servers stand",
		Long:  "Shows whether code is checked, how each language server stands, and the folders servers were kept out of.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return status(cmd, service, asJSON) },
	}
	statusCmd.Flags().BoolVar(&asJSON, "json", false, "Print the result as JSON")
	list := &cobra.Command{
		Use:   "list",
		Short: "List the language servers icebath knows",
		Long:  "Lists every language server icebath knows and those you added, how each stands and the files it checks.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Status(cmd.Context(), &lspv1.StatusRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return err
			}
			var rows [][]string
			for _, srv := range response.GetServers() {
				if !installedOnly || srv.GetPath() != "" {
					rows = append(rows, []string{srv.GetId(), srv.GetState(), types(srv.GetExtensions())})
				}
			}
			c := cli.NewConsole(cmd)
			if len(rows) == 0 {
				c.Say("No language server is installed yet. Install one with icebath lsp install <server>.")
				return nil
			}
			c.Table([]string{"Server", "State", "Files it checks"}, rows)
			return nil
		},
	}
	list.Flags().BoolVar(&installedOnly, "installed-only", false, "List only the language servers that are installed")
	install := &cobra.Command{
		Use:     "install <server>",
		Short:   "Install a language server",
		Long:    "Installs the language server you name, as lsp list names it.",
		Example: "  icebath lsp install gopls",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return installed(cmd, service, &lspv1.InstallRequest{Profile: picked.Picked(cmd.Context()), Server: args[0]})
		},
	}
	all := &cobra.Command{
		Use:   "install-all",
		Short: "Install all servers, saying what the others need",
		Long: "Installs each language server that is not installed and that icebath can install, then says for every one it could not install " +
			"what that server needs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return installed(cmd, service, &lspv1.InstallRequest{Profile: picked.Picked(cmd.Context()), All: true})
		},
	}
	stop := &cobra.Command{
		Use:     "stop",
		Aliases: []string{"restart"},
		Short:   "Stop the running language servers",
		Long:    "Stops the language servers that run for the profile. The next edit of a file starts them again.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			// Language servers run inside the icebath that started them, which a command on its own is not
			if cli.OwnServer(cmd.Context()) {
				return c.Fail("✗ Language servers run inside the icebath that started them, and this command runs on its own, so it has none " +
					"to stop. Stop them in the app with /lsp, or reach your server with icebath connect first.")
			}
			response, err := service.Stop(cmd.Context(), &lspv1.StopRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return err
			}
			if response.GetStopped() == 0 {
				c.Say("No language server was running.")
				return nil
			}
			c.Success("Stopped " + strs.Count(int(response.GetStopped()), "language server") + ". The next edit starts them again.")
			return nil
		},
	}
	which := &cobra.Command{
		Use:     "which <server>",
		Short:   "Print where a language server is installed",
		Long:    "Prints the path of the language server's program, or says that it is not installed.",
		Example: "  icebath lsp which gopls",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Which(cmd.Context(), &lspv1.WhichRequest{Profile: picked.Picked(cmd.Context()), Server: args[0]})
			if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeNotFound {
				fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+failure.Message())
				return exit(2)
			}
			if err != nil {
				return err
			}
			if response.GetPath() == "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s is not installed. Install it with icebath lsp install %s.\n", args[0], args[0])
				return exit(1)
			}
			fmt.Fprintln(cmd.OutOrStdout(), response.GetPath())
			return nil
		},
	}
	var command, fileTypes string
	add := &cobra.Command{
		Use:     "add <name> --command <command> --types <file types>",
		Short:   "Add a language server of your own",
		Long:    "Adds a language server that icebath does not know: a name for it, the command that starts it and the file endings it checks.",
		Example: `  icebath lsp add my-go --command "gopls serve" --types .go,.mod`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := service.Add(cmd.Context(), &lspv1.AddRequest{Profile: picked.Picked(cmd.Context()), Name: args[0], Command: command, Types: fileTypes}); err != nil {
				return refused(cmd, err)
			}
			cli.NewConsole(cmd).Success("Added " + args[0] + ". It checks " + strings.Join(strings.FieldsFunc(fileTypes, separator), ", ") + " files.")
			return nil
		},
	}
	add.Flags().StringVar(&command, "command", "", "The command that starts the server, such as gopls serve")
	add.Flags().StringVar(&fileTypes, "types", "", "The file endings it checks, such as .go,.mod")
	remove := &cobra.Command{
		Use:     "remove <server>",
		Aliases: []string{"rm"},
		Short:   "Remove a language server you added",
		Long:    "Removes a language server you added. For a server icebath knows, it puts back what you changed of it.",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := service.Remove(cmd.Context(), &lspv1.RemoveRequest{Profile: picked.Picked(cmd.Context()), Server: args[0]}); err != nil {
				return refused(cmd, err)
			}
			cli.NewConsole(cmd).Success("Removed " + args[0] + ".")
			return nil
		},
	}
	turn := func(on bool) *cobra.Command {
		use, short, long, done := "off <server>", "Turn a language server off", "Turns a language server off, so it never starts.", " is now off."
		if on {
			use, short, long, done = "on <server>", "Turn a language server on", "Turns a language server on again after it was turned off.", " is now on."
		}
		return &cobra.Command{Use: use, Short: short, Long: long, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := service.Turn(cmd.Context(), &lspv1.TurnRequest{Profile: picked.Picked(cmd.Context()), Server: args[0], On: on}); err != nil {
				return refused(cmd, err)
			}
			cli.NewConsole(cmd).Success(args[0] + done)
			return nil
		}}
	}
	trust := &cobra.Command{
		Use:   "trust",
		Short: "Trust the folders servers were kept out of",
		Long: "Trusts the folders language servers were kept out of because they are not trusted. Servers may then run the project's own programs " +
			"there.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The folders servers were kept out of are known to the icebath that runs the servers, which a command on its own is not
			if cli.OwnServer(cmd.Context()) {
				return cli.NewConsole(cmd).Fail("✗ This command runs on its own, so it knows no folder a language server was kept out of. Trust " +
					"them in the app with /lsp, or reach your server with icebath connect first.")
			}
			response, err := service.Trust(cmd.Context(), &lspv1.TrustRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return refused(cmd, err)
			}
			cli.NewConsole(cmd).Success("Trusted " + strings.Join(response.GetFolders(), ", ") + ". Servers run there from the next edit.")
			return nil
		},
	}
	root.AddCommand(statusCmd, list, install, all, stop, which, add, remove, turn(true), turn(false), trust)
	for _, c := range append([]*cobra.Command{root}, root.Commands()...) {
		c.RunE = interruptible(c.RunE)
	}
	return []*cobra.Command{root}
}

// Whether a character separates file types a person typed
func separator(r rune) bool { return r == ',' || r == ' ' || r == '\t' }

// Says why the service refused a change and ends with status 1
func refused(cmd *cobra.Command, err error) error {
	if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() != connect.CodeUnavailable {
		fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+failure.Message())
		return exit(1)
	}
	return err
}

// Ends with status 130 once the person interrupted the command, as a shell does
func interruptible(run func(cmd *cobra.Command, args []string) error) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		if err != nil && cmd.Context().Err() != nil {
			return exit(130)
		}
		return err
	}
}

// Runs an install, its lines shown as they come, then what it came to, ending with its status
func installed(cmd *cobra.Command, service lspv1connect.LspServiceClient, request *lspv1.InstallRequest) error {
	stream, err := service.Install(cmd.Context(), request)
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		event, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return errors.New("The install ended without saying whether it worked.")
		}
		if err != nil {
			return err
		}
		switch kind := event.GetKind().(type) {
		case *lspv1.InstallEvent_Out:
			fmt.Fprint(cmd.OutOrStdout(), kind.Out)
		case *lspv1.InstallEvent_Err:
			fmt.Fprintln(cmd.ErrOrStderr(), "✗ "+kind.Err)
		case *lspv1.InstallEvent_Outcome:
			outcome(cli.NewConsole(cmd), kind.Outcome)
		case *lspv1.InstallEvent_ExitCode:
			if kind.ExitCode != 0 {
				return exit(kind.ExitCode)
			}
			return nil
		}
	}
}

// What an install came to, as the server groups it: the servers installed, by name, then each thing the others need, once, with the
// command that gives it where a setting does
func outcome(c *cli.Console, told *lspv1.Outcome) {
	c.Blank()
	if done := told.GetDone(); done != "" {
		c.Success(done)
	}
	// Where each server now installed runs from
	for _, done := range told.GetInstalled() {
		if !done.GetAlready() && done.GetPath() != "" {
			c.Say("  " + done.GetServer() + " runs from " + done.GetPath() + ".")
		}
	}
	for _, need := range told.GetNeeds() {
		// A server a person installs themselves did not fail: the line says how
		if need.GetByHand() {
			c.Say(need.GetText())
			continue
		}
		c.Warning(need.GetText())
		if setting := need.GetSetting(); setting != "" {
			c.Say("  icebath config set " + setting + " " + need.GetValue())
		}
	}
}

// File types of a server in a few words: the first five, then how many more
func types(extensions []string) string {
	summary := strings.Join(extensions[:min(len(extensions), 5)], ", ")
	if len(extensions) > 5 {
		summary += " and " + strs.Number(float64(len(extensions)-5)) + " more"
	}
	return summary
}

// Title of a choice of how much a check waits for, the value itself for one not known
func choice(value string) string {
	switch value {
	case lspconfig.WaitDocument:
		return "Edited file"
	case lspconfig.WaitFull:
		return "Full result"
	}
	return value
}

// State of a running language server in words
func running(state string) string {
	switch state {
	case "running", "starting":
		return "Running"
	case "stopped":
		return "Stopped"
	case "error":
		return "Failed"
	}
	return state
}

func status(cmd *cobra.Command, service lspv1connect.LspServiceClient, asJSON bool) error {
	response, err := service.Status(cmd.Context(), &lspv1.StatusRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil {
		return err
	}
	if asJSON {
		return statusJSON(cmd.OutOrStdout(), response)
	}
	c := cli.NewConsole(cmd)
	c.Pairs("Language servers", [][2]string{
		{"Check code", c.OnOff(response.GetChecking())},
		{"Install when needed", c.OnOff(response.GetInstallWhenNeeded())},
		{"What to wait for", choice(response.GetWaitMode())},
		{"Check wait", strs.Span(time.Duration(response.GetWaitTimeout() * float64(time.Second)))},
	})
	if !response.GetChecking() {
		c.Say("No language server runs while Check code is off. icebath config set " + lspconfig.Enabled + " true turns it on.")
	}
	if clients := response.GetClients(); len(clients) > 0 {
		var rows [][]string
		for _, client := range clients {
			rows = append(rows, []string{client.GetServer(), running(client.GetState()), client.GetRoot()})
		}
		c.Blank()
		c.Table([]string{"Server", "State", "Folder"}, rows)
	}
	if broken := response.GetBroken(); len(broken) > 0 {
		c.Blank()
		c.Warning(strs.Count(len(broken), "server") + " failed to start. icebath lsp stop lets the next edit start them again.")
		var rows [][]string
		for _, b := range broken {
			rows = append(rows, []string{b.GetServer(), b.GetRoot()})
		}
		c.Table([]string{"Server", "Folder"}, rows)
	}
	if skipped := response.GetUntrustedSkipped(); len(skipped) > 0 {
		c.Blank()
		c.Warning(strs.Count(len(skipped), "server") + " did not start, because the folder is not trusted. icebath lsp trust trusts it.")
		var rows [][]string
		for _, u := range skipped {
			rows = append(rows, []string{u.GetServer(), u.GetRoot()})
		}
		c.Table([]string{"Server", "Folder"}, rows)
	}
	if response.GetExcludeRootsInvalid() {
		c.Blank()
		c.Warning("Skipped folders is not a list of folders, so no folder is checked until it is one.")
	}
	for _, w := range response.GetWarnings() {
		c.Blank()
		c.Warning(w)
	}
	var rows [][]string
	for _, srv := range response.GetServers() {
		about := srv.GetDescription()
		if !srv.GetUntrustedSafe() {
			about = strings.TrimSpace(about + ". Starts only in trusted folders")
		}
		rows = append(rows, []string{srv.GetId(), srv.GetState(), types(srv.GetExtensions()), about})
	}
	c.Blank()
	c.Table([]string{"Server", "State", "Files it checks", "About"}, rows)
	// A command on its own sees what is installed, never what another icebath runs
	if cli.OwnServer(cmd.Context()) {
		c.Blank()
		c.Info("This command runs on its own, so it shows what is installed and none of the servers a running icebath has started.")
	}
	return nil
}

// The status as JSON, keys in the order the service names them
func statusJSON(w io.Writer, response *lspv1.StatusResponse) error {
	type client struct {
		Server  string   `json:"server_id"`
		Root    string   `json:"workspace_root"`
		Folders []string `json:"workspace_folders"`
		State   string   `json:"state"`
		Running bool     `json:"running"`
	}
	type service struct {
		Enabled           bool        `json:"enabled"`
		WaitMode          string      `json:"wait_mode"`
		WaitTimeout       float64     `json:"wait_timeout"`
		InstallWhenNeeded bool        `json:"install_when_needed"`
		Clients           []client    `json:"clients"`
		Broken            [][2]string `json:"broken"`
		Disabled          []string    `json:"disabled_servers"`
		RetrySeconds      float64     `json:"broken_retry_seconds"`
		WarmupTimeout     float64     `json:"warmup_timeout"`
		ExcludeRoots      any         `json:"exclude_roots"`
		Trusted           []string    `json:"trusted_workspaces"`
		UntrustedSkipped  [][2]string `json:"untrusted_skipped"`
	}
	type server struct {
		ID          string   `json:"server_id"`
		Extensions  []string `json:"extensions"`
		Description string   `json:"description"`
		State       string   `json:"state"`
		Path        string   `json:"path"`
		Added       bool     `json:"added"`
		Command     string   `json:"command,omitempty"`
	}
	pairs := func(in []*lspv1.Pair) [][2]string {
		out := [][2]string{}
		for _, p := range in {
			out = append(out, [2]string{p.GetServer(), p.GetRoot()})
		}
		return out
	}
	texts := func(in []string) []string {
		if in == nil {
			return []string{}
		}
		return in
	}
	s := service{Enabled: response.GetChecking(), WaitMode: response.GetWaitMode(), WaitTimeout: response.GetWaitTimeout(),
		InstallWhenNeeded: response.GetInstallWhenNeeded(), Clients: []client{}, Broken: pairs(response.GetBroken()),
		Disabled: texts(response.GetDisabled()), RetrySeconds: response.GetBrokenRetrySeconds(), WarmupTimeout: response.GetWarmupTimeout(),
		ExcludeRoots: texts(response.GetExcludeRoots()), Trusted: texts(response.GetTrustedWorkspaces()), UntrustedSkipped: pairs(response.GetUntrustedSkipped())}
	if response.GetExcludeRootsInvalid() {
		s.ExcludeRoots = "INVALID"
	}
	for _, c := range response.GetClients() {
		s.Clients = append(s.Clients, client{Server: c.GetServer(), Root: c.GetRoot(), Folders: texts(c.GetFolders()), State: c.GetState(), Running: c.GetRunning()})
	}
	registry := []server{}
	for _, srv := range response.GetServers() {
		registry = append(registry, server{ID: srv.GetId(), Extensions: texts(srv.GetExtensions()), Description: srv.GetDescription(), State: srv.GetState(),
			Path: srv.GetPath(), Added: srv.GetAdded(), Command: srv.GetCommand()})
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(struct {
		Service  service  `json:"service"`
		Registry []server `json:"registry"`
	}{s, registry})
}
