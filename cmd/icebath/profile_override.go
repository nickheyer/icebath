package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Profile named by a consumed -p or --profile flag, empty when the sticky active_profile chose
var explicitCLIProfile string

// Expands a literal tilde or variable in ICEBATH_HOME once so every reader sees one absolute spelling
func normalizeHomeEnv() {
	raw := os.Getenv(home.EnvHome)
	if strings.TrimSpace(raw) == "" {
		return
	}
	expanded := os.ExpandEnv(strings.TrimSpace(raw))
	if strings.HasPrefix(expanded, "~") {
		if userHome, err := os.UserHomeDir(); err == nil && (len(expanded) == 1 || expanded[1] == '/' || expanded[1] == filepath.Separator) {
			expanded = userHome + expanded[1:]
		}
	}
	if expanded != raw {
		os.Setenv(home.EnvHome, expanded)
	}
}

// True when the adjacent pair gateway run appears anywhere in argv
func argvIsGatewayRun(argv []string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "gateway" && argv[i+1] == "run" {
			return true
		}
	}
	return false
}

// A supervisor launched gateway child must not follow the sticky active_profile
func underGatewaySupervisor(argv []string) bool {
	if os.Getenv("ICEBATH_SUPERVISED_CHILD") != "" || os.Getenv("ICEBATH_S6_SUPERVISED_CHILD") != "" {
		return true
	}
	first := ""
	for _, a := range argv {
		if !strings.HasPrefix(a, "-") {
			first = a
			break
		}
	}
	if first == "gateway" && os.Getenv("INVOCATION_ID") != "" {
		return true
	}
	for _, name := range []string{"ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR", "ICEBATH_GATEWAY_PROFILE_PIN"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

// A bare gateway run inside the s6 image names the gateway-default slot and ignores active_profile
func s6SupervisedGatewayRun(argv []string) bool {
	var words []string
	noSupervise := false
	for _, a := range argv {
		if a == "--no-supervise" {
			noSupervise = true
		}
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
		}
	}
	if len(words) < 2 || words[0] != "gateway" || words[1] != "run" || noSupervise {
		return false
	}
	switch strings.ToLower(os.Getenv("ICEBATH_GATEWAY_NO_SUPERVISE")) {
	case "1", "true", "yes":
		return false
	}
	return gatewaycli.S6Running()
}

// Resolves sudo icebath -p name against the invoking user's home, empty when nothing live is there
func resolveSudoUserProfileEnv(name string) string {
	if name == "default" {
		return ""
	}
	sudoHome := home.SudoInvokerDefaultHome()
	if sudoHome == "" {
		return ""
	}
	candidate := filepath.Join(sudoHome, home.ProfilesDir, name)
	if home.ProfileLive(candidate) {
		return candidate
	}
	return ""
}

// True when uninstall with these arguments removes no user data, parsed with the real uninstall command
func uninstallKeepsData(args []string) bool {
	cli := commands.NewCLI()
	cmd, _, err := cli.Root.Find([]string{"uninstall"})
	if err != nil || cmd == nil {
		return false
	}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.InitDefaultHelpFlag()
	if err := cmd.ParseFlags(args); err != nil {
		return false
	}
	if help, _ := cmd.Flags().GetBool("help"); help {
		return true
	}
	data, _ := cmd.Flags().GetBool("data")
	full, _ := cmd.Flags().GetBool("full")
	return !data && !full
}

// profile list, profile use default, or an uninstall that keeps user data
func isStaleProfileRecoveryCommand(argv []string) bool {
	command := commands.NewCLI().CommandArgv(argv)
	if len(command) >= 2 && command[0] == "profile" && command[1] == "list" {
		return true
	}
	if len(command) >= 2 && command[0] == "profile" && command[1] == "use" {
		return len(command) > 2 && profiles.Casefold(command[2]) == "default"
	}
	return len(command) >= 1 && command[0] == "uninstall" && uninstallKeepsData(command[1:])
}

// Pre parses -p/--profile, sets ICEBATH_HOME and strips the flag, falling back to the sticky active_profile
func applyProfileOverride(argv []string) []string {
	explicitCLIProfile = ""
	cli := commands.NewCLI()
	profileName, consume, index, err := cli.ScanProfileFlag(argv)
	if err != nil {
		code, msg := commands.ExitCode(err)
		fmt.Fprintln(os.Stderr, msg)
		exit(code)
	}
	fromSticky := false
	envHome := os.Getenv(home.EnvHome)
	if profileName == "" && envHome != "" && filepath.Base(filepath.Dir(envHome)) == home.ProfilesDir {
		return argv
	}
	if profileName == "" && !underGatewaySupervisor(argv) && !tools.IsDesktopSSHBackendArgv(argv) && !s6SupervisedGatewayRun(argv) {
		if raw, readErr := os.ReadFile(filepath.Join(home.Root(), "active_profile")); readErr == nil {
			name := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
			if name != "" && name != "default" {
				profileName = name
				fromSticky = true
			}
		}
	}
	if profileName == "" {
		return argv
	}
	resolved, err := profiles.ResolveProfileEnv(profileName)
	if err != nil {
		var valueErr *profiles.ValueError
		switch {
		case errors.Is(err, fs.ErrNotExist):
			resolved = resolveSudoUserProfileEnv(profileName)
			message := err.Error()
			if resolved == "" && fromSticky {
				if isStaleProfileRecoveryCommand(argv) {
					resolved, _ = profiles.ResolveProfileEnv("default")
					fmt.Fprintf(os.Stderr, "Warning: saved profile '%s' no longer exists; running this recovery command in the default profile.\n", profileName)
				} else {
					message = fmt.Sprintf("Saved profile '%s' no longer exists. Switch back with: icebath profile use default", profileName)
				}
			}
			if resolved == "" {
				fmt.Fprintln(os.Stderr, "Error: "+message)
				exit(1)
			}
		case errors.As(err, &valueErr):
			fmt.Fprintln(os.Stderr, "Error: "+err.Error())
			exit(1)
		default:
			fmt.Fprintf(os.Stderr, "Warning: profile override failed (%v), using default\n", err)
			return argv
		}
	}
	os.Setenv(home.EnvHome, resolved)
	if consume > 0 {
		explicitCLIProfile = profileName
		argv = append(append([]string{}, argv[:index]...), argv[index+consume:]...)
	}
	return argv
}

// First command word of argv as the CLI parser splits it, empty for a bare launch
func firstPositional(argv []string) string {
	return commands.NewCLI().FirstPositional(argv)
}

// Registers the CLI tree's argv splitter with pm so its early recovery reads parsed commands
func registerCommandArgv() {
	pm.SetCommandArgv(func(argv []string) []string { return commands.NewCLI().CommandArgv(argv) })
}
