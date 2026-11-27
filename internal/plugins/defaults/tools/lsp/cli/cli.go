// The lsp commands: how the language servers that check the agent's writes stand, and installing, finding and restarting them
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
		Long: "After the agent writes a file, a language server for that language checks it and reports errors back to the agent. Use " +
			"these commands to see which language servers are installed, install them and restart them. With no subcommand, lsp shows " +
			"the status.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "✗ There is no lsp command called “%s”. Run icebath lsp --help to see them.\n", args[0])
				return exit(2)
			}
			return status(cmd, service, false)
		},
	}
	var asJSON, installedOnly, includeManual bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show which language servers run",
		Long:  "Shows whether the language servers are on, which ones run and which failed or were left out.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return status(cmd, service, asJSON) },
	}
	statusCmd.Flags().BoolVar(&asJSON, "json", false, "Print the result as JSON")
	list := &cobra.Command{
		Use:   "list",
		Short: "List the language servers icebath knows",
		Long:  "Lists every language server icebath knows, whether it is installed and the file extensions it checks.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Status(cmd.Context(), &lspv1.StatusRequest{})
			if err != nil {
				return err
			}
			var rows [][]string
			for _, srv := range response.GetServers() {
				if !installedOnly || srv.GetStatus() == "installed" {
					rows = append(rows, []string{srv.GetId(), state(srv.GetStatus()), strings.Join(srv.GetExtensions(), ", ")})
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
			return installed(cmd, service, &lspv1.InstallRequest{Server: args[0]})
		},
	}
	all := &cobra.Command{
		Use:   "install-all",
		Short: "Install every language server icebath can install",
		Long: "Installs every language server that icebath knows how to install. With --include-manual, install-all also tries the ones you would " +
			"normally install yourself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return installed(cmd, service, &lspv1.InstallRequest{All: true, IncludeManual: includeManual})
		},
	}
	all.Flags().BoolVar(&includeManual, "include-manual", false, "Also try the language servers that you would install yourself")
	restart := &cobra.Command{
		Use:   "restart",
		Short: "Stop the running language servers",
		Long:  "Stops every running language server. The next edit starts them again.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := service.Restart(cmd.Context(), &lspv1.RestartRequest{}); err != nil {
				return err
			}
			cli.NewConsole(cmd).Success("Stopped every language server. The next edit starts them again.")
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
			response, err := service.Which(cmd.Context(), &lspv1.WhichRequest{Server: args[0]})
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
	root.AddCommand(statusCmd, list, install, all, restart, which)
	for _, c := range append([]*cobra.Command{root}, root.Commands()...) {
		c.RunE = interruptible(c.RunE)
	}
	return []*cobra.Command{root}
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

// Runs an install, its lines shown as they come, ending with its status
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
			fmt.Fprintln(cmd.ErrOrStderr(), kind.Err)
		case *lspv1.InstallEvent_ExitCode:
			if kind.ExitCode != 0 {
				return exit(kind.ExitCode)
			}
			return nil
		}
	}
}

// State of a language server's program in words
func state(status string) string {
	switch status {
	case "installed":
		return "Installed"
	case "missing":
		return "Not installed"
	case "manual-only":
		return "Install it yourself"
	}
	return status
}

// State of a running language server in words
func running(state string) string {
	switch state {
	case "running":
		return "Running"
	case "starting":
		return "Starting"
	case "stopped":
		return "Stopped"
	case "error":
		return "Failed"
	}
	return state
}

// Title of a choice of the language server settings, the value itself for one not known
func choice(value string) string {
	titles := map[string]string{lspconfig.WaitDocument: "Edited file", lspconfig.WaitFull: "Full result", lspconfig.InstallAuto: "Automatic",
		lspconfig.InstallManual: "Manual"}
	if title, ok := titles[value]; ok {
		return title
	}
	return value
}

func status(cmd *cobra.Command, service lspv1connect.LspServiceClient, asJSON bool) error {
	response, err := service.Status(cmd.Context(), &lspv1.StatusRequest{})
	if err != nil {
		return err
	}
	if asJSON {
		return statusJSON(cmd.OutOrStdout(), response)
	}
	c := cli.NewConsole(cmd)
	st := response.GetService()
	if st == nil {
		c.Pairs("Language servers", [][2]string{{"Check code", "Off"}})
		c.Say("Turn on Check code in /settings under Tools › Language servers to have the agent's edits checked.")
	} else {
		clients := "None"
		if n := len(st.GetClients()); n > 0 {
			clients = strs.Count(n, "server")
		}
		c.Pairs("Language servers", [][2]string{
			{"Check code", "On"},
			{"What to wait for", choice(st.GetWaitMode())},
			{"Check wait", strs.Span(time.Duration(st.GetWaitTimeout() * float64(time.Second)))},
			{"Install servers", choice(st.GetInstallStrategy())},
			{"Running now", clients},
		})
		if clients := st.GetClients(); len(clients) > 0 {
			var rows [][]string
			for _, client := range clients {
				rows = append(rows, []string{client.GetServer(), running(client.GetState()), client.GetRoot()})
			}
			c.Blank()
			c.Table([]string{"Server", "State", "Folder"}, rows)
		}
		if broken := st.GetBroken(); len(broken) > 0 {
			c.Blank()
			c.Warning(strs.Count(len(broken), "server") + " failed and will be tried again later:")
			var rows [][]string
			for _, b := range broken {
				rows = append(rows, []string{b.GetServer(), b.GetRoot()})
			}
			c.Table([]string{"Server", "Folder"}, rows)
		}
		if disabled := st.GetDisabled(); len(disabled) > 0 {
			c.Blank()
			c.Say("Turned off in /settings under Tools › Language servers › Servers: " + strings.Join(disabled, ", "))
		}
		if skipped := st.GetUntrustedSkipped(); len(skipped) > 0 {
			c.Blank()
			c.Warning(strs.Count(len(skipped), "server") + " did not start, because the folder is not trusted. Add it in /settings under " +
				"Tools › Trusted folders.")
			var rows [][]string
			for _, u := range skipped {
				rows = append(rows, []string{u.GetServer(), u.GetRoot()})
			}
			c.Table([]string{"Server", "Folder"}, rows)
		}
	}
	if warnings := response.GetWarnings(); len(warnings) > 0 {
		c.Blank()
		for _, w := range warnings {
			c.Warning(w)
		}
	}
	var rows [][]string
	for _, srv := range response.GetServers() {
		extensions := srv.GetExtensions()
		summary := strings.Join(extensions[:min(len(extensions), 5)], ", ")
		if len(extensions) > 5 {
			summary += " and " + strs.Number(float64(len(extensions)-5)) + " more"
		}
		about := srv.GetDescription()
		if !srv.GetUntrustedSafe() {
			about = strings.TrimSpace(about + ". Starts only in trusted folders")
		}
		rows = append(rows, []string{srv.GetId(), state(srv.GetStatus()), summary, about})
	}
	c.Blank()
	c.Table([]string{"Server", "State", "Files it checks", "About"}, rows)
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
		Enabled          bool        `json:"enabled"`
		WaitMode         string      `json:"wait_mode"`
		WaitTimeout      float64     `json:"wait_timeout"`
		InstallStrategy  string      `json:"install_strategy"`
		Clients          []client    `json:"clients"`
		Broken           [][2]string `json:"broken"`
		Disabled         []string    `json:"disabled_servers"`
		RetrySeconds     float64     `json:"broken_retry_seconds"`
		WarmupTimeout    float64     `json:"warmup_timeout"`
		ExcludeRoots     any         `json:"exclude_roots"`
		Trusted          []string    `json:"trusted_workspaces"`
		UntrustedSkipped [][2]string `json:"untrusted_skipped"`
	}
	type server struct {
		ID          string   `json:"server_id"`
		Extensions  []string `json:"extensions"`
		Description string   `json:"description"`
		Status      string   `json:"binary_status"`
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
	var info any = struct {
		Enabled bool `json:"enabled"`
	}{}
	if st := response.GetService(); st != nil {
		s := service{Enabled: true, WaitMode: st.GetWaitMode(), WaitTimeout: st.GetWaitTimeout(), InstallStrategy: st.GetInstallStrategy(),
			Clients: []client{}, Broken: pairs(st.GetBroken()), Disabled: texts(st.GetDisabled()), RetrySeconds: st.GetBrokenRetrySeconds(),
			WarmupTimeout: st.GetWarmupTimeout(), ExcludeRoots: texts(st.GetExcludeRoots()), Trusted: texts(st.GetTrustedWorkspaces()),
			UntrustedSkipped: pairs(st.GetUntrustedSkipped())}
		if st.GetExcludeRootsInvalid() {
			s.ExcludeRoots = "INVALID"
		}
		for _, c := range st.GetClients() {
			s.Clients = append(s.Clients, client{Server: c.GetServer(), Root: c.GetRoot(), Folders: texts(c.GetFolders()), State: c.GetState(),
				Running: c.GetRunning()})
		}
		info = s
	}
	registry := []server{}
	for _, srv := range response.GetServers() {
		registry = append(registry, server{ID: srv.GetId(), Extensions: texts(srv.GetExtensions()), Description: srv.GetDescription(),
			Status: srv.GetStatus()})
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(struct {
		Service  any      `json:"service"`
		Registry []server `json:"registry"`
	}{info, registry})
}
