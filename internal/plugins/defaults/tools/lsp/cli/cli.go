// The lsp commands: how the language servers that check the agent's writes stand, and installing, finding and restarting them
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1/lspv1connect"
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
		Use:   "lsp",
		Short: "Language Server Protocol management",
		Long:  "Manage the LSP layer that powers post-write semantic diagnostics in write_file/patch.",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "unknown lsp subcommand: %s\n", args[0])
				return exit(2)
			}
			return status(cmd, service, false)
		},
	}
	var asJSON, installedOnly, includeManual bool
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show LSP service status",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return status(cmd, service, asJSON) },
	}
	statusCmd.Flags().BoolVar(&asJSON, "json", false, "Emit machine-readable JSON")
	list := &cobra.Command{
		Use:   "list",
		Short: "List supported language servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := service.Status(cmd.Context(), &lspv1.StatusRequest{})
			if err != nil {
				return err
			}
			for _, srv := range response.GetServers() {
				if !installedOnly || srv.GetStatus() == "installed" {
					fmt.Fprintf(cmd.OutOrStdout(), "%s [%s] %s\n", cli.Pad(srv.GetId(), 24), cli.Pad(srv.GetStatus(), 11), strings.Join(srv.GetExtensions(), ","))
				}
			}
			return nil
		},
	}
	list.Flags().BoolVar(&installedOnly, "installed-only", false, "Only show servers whose binary is currently available")
	install := &cobra.Command{
		Use:   "install <server>",
		Short: "Install a server binary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return installed(cmd, service, &lspv1.InstallRequest{Server: args[0]})
		},
	}
	all := &cobra.Command{
		Use:   "install-all",
		Short: "Install every server with a known auto-install recipe",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return installed(cmd, service, &lspv1.InstallRequest{All: true, IncludeManual: includeManual})
		},
	}
	all.Flags().BoolVar(&includeManual, "include-manual", false, "Even attempt servers marked manual-install (best effort)")
	restart := &cobra.Command{
		Use:   "restart",
		Short: "Tear down running LSP clients (next edit re-spawns)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := service.Restart(cmd.Context(), &lspv1.RestartRequest{}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "LSP service shut down. Next edit will respawn clients.")
			return nil
		},
	}
	which := &cobra.Command{
		Use:   "which <server>",
		Short: "Print binary path for a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Which(cmd.Context(), &lspv1.WhichRequest{Server: args[0]})
			if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeNotFound {
				fmt.Fprintln(cmd.ErrOrStderr(), failure.Message())
				return exit(2)
			}
			if err != nil {
				return err
			}
			if response.GetPath() == "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: not installed\n", args[0])
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
			return errors.New("the server ended the install without saying how it went")
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

var markers = map[string]string{"installed": "✓", "missing": "·", "manual-only": "?"}

func status(cmd *cobra.Command, service lspv1connect.LspServiceClient, asJSON bool) error {
	response, err := service.Status(cmd.Context(), &lspv1.StatusRequest{})
	if err != nil {
		return err
	}
	if asJSON {
		return statusJSON(cmd.OutOrStdout(), response)
	}
	st := response.GetService()
	out := []string{"LSP Service", "===========", "  enabled:         " + strconv.FormatBool(st != nil)}
	if st != nil {
		out = append(out, "  wait_mode:       "+st.GetWaitMode(), "  wait_timeout:    "+strconv.FormatFloat(st.GetWaitTimeout(), 'f', -1, 64)+"s",
			"  install_strategy:"+st.GetInstallStrategy())
		if clients := st.GetClients(); len(clients) > 0 {
			out = append(out, fmt.Sprintf("  active clients:  %d", len(clients)))
		} else {
			out = append(out, "  active clients:  none")
		}
		for _, c := range st.GetClients() {
			out = append(out, "    - "+cli.Pad(c.GetServer(), 20)+" state="+cli.Pad(c.GetState(), 10)+" root="+c.GetRoot())
		}
		if broken := st.GetBroken(); len(broken) > 0 {
			out = append(out, fmt.Sprintf("  broken pairs:    %d", len(broken)))
			for _, b := range broken {
				out = append(out, "    - "+cli.Pad(b.GetServer(), 20)+" root="+b.GetRoot())
			}
		}
		if disabled := st.GetDisabled(); len(disabled) > 0 {
			out = append(out, "  disabled in cfg: "+strings.Join(disabled, ", "))
		}
		if skipped := st.GetUntrustedSkipped(); len(skipped) > 0 {
			out = append(out, fmt.Sprintf("  untrusted skips: %d (add the workspace to %s)", len(skipped), toolsconfig.TrustedWorkspaces))
			for _, u := range skipped {
				out = append(out, "    - "+cli.Pad(u.GetServer(), 20)+" root="+u.GetRoot())
			}
		}
	}
	if warnings := response.GetWarnings(); len(warnings) > 0 {
		out = append(out, "", "Backend warnings", "================")
		for _, w := range warnings {
			out = append(out, "  ! "+w)
		}
	}
	out = append(out, "", "Registered Servers", "==================")
	for _, srv := range response.GetServers() {
		extensions := srv.GetExtensions()
		summary := strings.Join(extensions[:min(len(extensions), 5)], ", ")
		if len(extensions) > 5 {
			summary += fmt.Sprintf(", … (+%d)", len(extensions)-5)
		}
		marker, ok := markers[srv.GetStatus()]
		if !ok {
			marker = " "
		}
		out = append(out, "  "+marker+" "+cli.Pad(srv.GetId(), 24)+" ["+cli.Pad(srv.GetStatus(), 11)+"] "+summary)
		if description := srv.GetDescription(); description != "" {
			gate := "  [trusted workspaces only]"
			if srv.GetUntrustedSafe() {
				gate = ""
			}
			out = append(out, "      "+description+gate)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), strings.Join(out, "\n"))
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
