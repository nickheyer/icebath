package spotify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Console the spotify command prints to
var cliOut io.Writer = os.Stdout

var cliActions = []string{"login", "status", "logout"}

// Builds the spotify command, an optional login status or logout action plus the login flags
func setupCLI(parser any) {
	cmd, ok := parser.(*cobra.Command)
	if !ok {
		panic(fmt.Sprintf("the CLI parser is %T, expected an argument parser", parser))
	}
	var clientID, redirectURI, scope string
	var noBrowser bool
	var timeout float64
	flags := cmd.Flags()
	flags.StringVar(&clientID, "client-id", "", "Spotify app client_id (or set ICEBATH_SPOTIFY_CLIENT_ID)")
	flags.StringVar(&redirectURI, "redirect-uri", "", "Allow-listed localhost redirect URI for your Spotify app")
	flags.StringVar(&scope, "scope", "", "Override requested Spotify scopes")
	flags.BoolVar(&noBrowser, "no-browser", false, "Do not attempt to open the browser automatically")
	flags.Float64Var(&timeout, "timeout", 0, "Callback/token exchange timeout in seconds")
	commands.PluginOptionalChoice(cmd, "action", "login", cliActions...)
	optional := func(name, value string) any {
		if cmd.Flags().Changed(name) {
			return value
		}
		return nil
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		action := "login"
		if len(args) == 1 {
			action = args[0]
		}
		var timeoutValue any
		if cmd.Flags().Changed("timeout") {
			timeoutValue = timeout
		}
		return handleCLI(map[string]any{"action": action, "client_id": optional("client-id", clientID),
			"redirect_uri": optional("redirect-uri", redirectURI), "scope": optional("scope", scope),
			"no_browser": noBrowser, "timeout": timeoutValue})
	}
}

// Text of a namespace value, empty for None or a falsy value
func nsText(ns map[string]any, key string) string {
	if v := ns[key]; anyutil.IsTrue(v) {
		return anyutil.String(v)
	}
	return ""
}

// Runs the login, status or logout action of a parsed spotify namespace
func handleCLI(args any) error {
	ns, _ := args.(map[string]any)
	action := nsText(ns, "action")
	if action == "" {
		action = "login"
	}
	h := home.Dir(context.Background())
	switch action {
	case "login":
		timeout, _ := ns["timeout"].(float64)
		err := provider.SpotifyLogin(context.Background(), h, provider.SpotifyLoginOptions{
			ClientID: nsText(ns, "client_id"), RedirectURI: nsText(ns, "redirect_uri"), Scope: nsText(ns, "scope"),
			NoBrowser: anyutil.IsTrue(ns["no_browser"]), Timeout: timeout,
		}, cliOut)
		var exit *provider.AuthExitError
		if errors.As(err, &exit) {
			return &commands.ExitError{Code: exit.Code, Msg: exit.Message}
		}
		return err
	case "status":
		return printStatus(h)
	case "logout":
		removed, err := provider.SpotifyLogout(h)
		if err != nil {
			return err
		}
		if removed {
			fmt.Fprintln(cliOut, "Logged out of Spotify.")
		} else {
			fmt.Fprintln(cliOut, "No auth state found for Spotify.")
		}
		return nil
	}
	return &pyError{kind: "KeyError", msg: anyutil.Quote(action)}
}

// Prints whether the profile is logged in and each stored login field that is set
func printStatus(h string) error {
	status, err := provider.SpotifyAuthStatus(h)
	if err != nil {
		return err
	}
	if !anyutil.IsTrue(status["logged_in"]) {
		fmt.Fprintln(cliOut, "spotify: logged out")
		return nil
	}
	fmt.Fprintln(cliOut, "spotify: logged in")
	for _, key := range []string{"auth_type", "client_id", "redirect_uri", "scope", "expires_at", "api_base_url"} {
		if v := status[key]; anyutil.IsTrue(v) {
			fmt.Fprintf(cliOut, "  %s: %s\n", key, anyutil.String(v))
		}
	}
	return nil
}
