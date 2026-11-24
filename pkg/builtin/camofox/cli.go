package camofox

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

const serverHelp = "Server setup: https://github.com/jo-inc/camofox-browser  " +
	"(native: npm install && npm start; Docker: docker run -p 9377:9377 -e CAMOFOX_PORT=9377 " +
	"jo-inc/camofox-browser)"

var (
	cliOut io.Writer = os.Stdout
	// Line prompt of the setup command, the masked prompt for a password
	cliPrompt = provider.SetupPrompt
)

// Exit code of a CLI handler carried as the error the host maps to the process status
func exitErr(code int) error {
	if code == 0 {
		return nil
	}
	return &commands.ExitError{Code: code}
}

// Builds the setup and status subcommands under the camofox command the host passes
func setupCLI(parser any) {
	root, ok := parser.(*cobra.Command)
	if !ok {
		panic(&pyError{"AttributeError", fmt.Sprintf("camofox CLI setup needs a *cobra.Command, got %T", parser)})
	}
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations["subdest"] = "camofox_command"
	var setupURL string
	setup := &cobra.Command{Use: "setup", Short: "Save CAMOFOX_URL (and an optional CAMOFOX_API_KEY) to this profile's .env",
		Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true}
	setup.Flags().StringVar(&setupURL, "url", "", "Camofox server URL (default: prompt, http://localhost:9377)")
	setup.RunE = func(cmd *cobra.Command, _ []string) error {
		ns := map[string]any{"camofox_command": "setup", "url": nil}
		if cmd.Flags().Changed("url") {
			ns["url"] = setupURL
		}
		return handleCLI(ns)
	}
	status := &cobra.Command{Use: "status", Short: "Show the configured Camofox server and whether it answers",
		Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error { return handleCLI(map[string]any{"camofox_command": "status"}) }}
	root.AddCommand(setup, status)
}

// Identity mode the profile's Camofox sessions use
func (s *scope) identityMode() string {
	cfg := s.camofoxConfig()
	if s.identityOverride("", cfg) != nil {
		return "external (CAMOFOX_USER_ID / browser.camofox.user_id)"
	}
	if managedPersistenceEnabled(cfg) {
		return "managed persistence (stable per-profile Camofox profile)"
	}
	return "ephemeral (new Camofox profile per session)"
}

// Prints the configured server, identity mode, reachability and VNC address, 1 when unset or unreachable
func (s *scope) cmdStatus() int {
	serverURL := s.camofoxURL()
	if serverURL == "" {
		fmt.Fprintln(cliOut, "CAMOFOX_URL is not set. Run: icebath camofox setup")
		fmt.Fprintln(cliOut, serverHelp)
		return 1
	}
	fmt.Fprintf(cliOut, "Camofox server: %s\n", serverURL)
	fmt.Fprintf(cliOut, "Identity:       %s\n", s.identityMode())
	if !s.checkAvailable() {
		fmt.Fprintln(cliOut, "Status:         not reachable. Start your server and check CAMOFOX_URL.")
		fmt.Fprintln(cliOut, serverHelp)
		return 1
	}
	fmt.Fprintln(cliOut, "Status:         reachable")
	if vnc := s.vncURL(); vnc != "" {
		fmt.Fprintf(cliOut, "VNC:            %s\n", vnc)
	}
	return 0
}

// Saves CAMOFOX_URL and an optional CAMOFOX_API_KEY to the profile .env, then reports the status
func (s *scope) cmdSetup(serverURL *string) (int, error) {
	fmt.Fprintln(cliOut, "Camofox is an externally managed server; Icebath does not install or start it.")
	chosen := ""
	if serverURL != nil {
		chosen = *serverURL
	}
	if chosen == "" {
		def := s.camofoxURL()
		if def == "" {
			def = "http://localhost:9377"
		}
		chosen = cliPrompt("Camofox server URL", def, false)
	}
	values := [][2]string{{"CAMOFOX_URL", strings.TrimRight(chosen, "/")}}
	if apiKey := cliPrompt("Camofox API key (optional, Enter to skip)", "", true); apiKey != "" {
		values = append(values, [2]string{"CAMOFOX_API_KEY", apiKey})
	}
	for _, kv := range values {
		if err := config.SaveEnvValueContext(s.ctx, s.home, kv[0], kv[1]); err != nil {
			return 0, err
		}
	}
	fmt.Fprintln(cliOut, "Saved to this profile's .env. Enable the tools with: icebath tools enable camofox")
	return s.cmdStatus(), nil
}

// Runs the camofox subcommand the namespace names, status when none
func handleCLI(args any) error {
	ns, _ := args.(map[string]any)
	s := newScope(context.Background(), nil)
	if command, _ := ns["camofox_command"].(string); command == "setup" {
		var serverURL *string
		if v, ok := ns["url"].(string); ok {
			serverURL = &v
		}
		code, err := s.cmdSetup(serverURL)
		if err != nil {
			return err
		}
		return exitErr(code)
	}
	return exitErr(s.cmdStatus())
}
