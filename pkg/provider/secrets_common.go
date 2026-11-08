package provider

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Config, env file and terminal seams the secrets CLIs share, the module attributes hermes tests patch
var (
	secretsLoadConfig = func(h string) (map[string]any, error) {
		tree, err := config.Load(h, config.NewSecrets(h).Lookup())
		return map[string]any(tree), err
	}
	secretsSaveConfig   = func(h string, cfg map[string]any) error { return config.Save(h, config.Tree(cfg)) }
	secretsSaveEnvValue = config.SaveEnvValue
	secretsEnvPath      = home.EnvPath
	stdinIsATTY         = func() bool { return streamIsTTY(os.Stdin) }
)

// One secrets CLI invocation with its home and the console every handler prints to
type secretsCLI struct {
	ctx     context.Context
	home    string
	console *pyrich.Console
}

// Handler of one secrets subcommand over the parsed namespace, returning the exit code
type secretsHandler func(cli *secretsCLI, ns map[string]any) (int, error)

// Argument of a secrets subcommand as argparse add_argument takes it
type secretsArg struct {
	Name      string
	Help      string
	StoreTrue bool
}

// Subcommand of a secrets integration with its handler and arguments
type secretsSubcommand struct {
	Name      string
	Help      string
	Func      secretsHandler
	Arguments []secretsArg
}

// Parser of one secrets integration, subcommands keyed by the dest their name lands under
type secretsParser struct {
	Name     string
	Aliases  []string
	Help     string
	Dest     string
	Commands []secretsSubcommand
}

func yn(b bool) string {
	if b {
		return "[green]yes[/green]"
	}
	return "[dim]no[/dim]"
}

// Mapping value of a config entry, empty for a falsy one, Python's AttributeError for any other
func secretsMapping(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if !pyTruthy(v) {
		return map[string]any{}
	}
	panic(fmt.Sprintf("AttributeError: '%s' object has no attribute 'get'", pyTypeName(v)))
}

// The secrets section of a config, empty when absent
func sectionCfg(cfg map[string]any, section string) map[string]any {
	return secretsMapping(secretsMapping(cfg["secrets"])[section])
}

// Stripped string config value, empty for a missing or falsy one
func cfgStr(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok || !pyTruthy(v) {
		return ""
	}
	return pystr.Strip(pyStr(v))
}

// One add_argument spec of a subcommand
func arg(name, helpText string) secretsArg {
	return secretsArg{Name: name, Help: helpText}
}

// A boolean store_true spec of a subcommand
func flag(name, helpText string) secretsArg {
	return secretsArg{Name: name, Help: helpText, StoreTrue: true}
}

// Attaches the subcommands to the parser under dest
func registerSubcommands(parent *secretsParser, dest string, commands []secretsSubcommand) {
	parent.Dest = dest
	parent.Commands = append(parent.Commands, commands...)
}

// Handler of the subcommand the namespace names under the parser's dest, nil when none ran
func (p *secretsParser) handler(ns map[string]any) secretsHandler {
	name, ok := ns[p.Dest].(string)
	if !ok {
		return nil
	}
	for _, cmd := range p.Commands {
		if cmd.Name == name {
			return cmd.Func
		}
	}
	return nil
}

// Prints the integration is disabled hint, true only when the section is enabled
func requireEnabled(console *pyrich.Console, cfg map[string]any, product, command string) bool {
	if pyTruthy(cfg["enabled"]) {
		return true
	}
	console.Print(fmt.Sprintf("[yellow]%s integration is disabled.  Run `icebath secrets %s setup` first.[/yellow]", product, command))
	return false
}

// Two column key and value table inside a cyan panel, the status layout
func printStatusPanel(console *pyrich.Console, title string, rows [][2]string) {
	table := &pyrich.Table{HideHeader: true, NoBox: true, PadX: 2, Columns: []pyrich.Column{{Style: "bold"}, {}}}
	for _, row := range rows {
		table.AddRow(row[0], row[1])
	}
	console.PrintPanel(pyrich.Panel{Title: title, BorderStyle: "cyan", Content: table})
}

// Headed table followed by one warning line per warning
func printTable(console *pyrich.Console, columns []pyrich.Column, rows [][]string, warnings []string, indent string) {
	table := &pyrich.Table{Columns: columns}
	for _, row := range rows {
		table.AddRow(row...)
	}
	console.PrintTable(table)
	for _, w := range warnings {
		console.Print(indent + "[yellow]warning:[/yellow] " + w)
	}
}

// Decodes helper output as utf-8 with undecodable bytes replaced
func decodeCLIOutput(raw []byte) string {
	text, err := pycodecs.DecodeReplace(raw, "utf-8")
	if err != nil {
		return string(raw)
	}
	return text
}

// First line of the binary's --version output, version unknown on failure or silence
func cliVersion(binary string) string {
	res, err := pysubprocess.RunWith(context.Background(), []string{binary, "--version"}, pysubprocess.Options{Timeout: 5 * time.Second})
	if err != nil || res.Returncode != 0 {
		return "version unknown"
	}
	out := res.Stdout
	if len(out) == 0 {
		out = res.Stderr
	}
	for _, line := range strings.Split(pystr.Strip(decodeCLIOutput(out)), "\n") {
		return line
	}
	return "version unknown"
}

// Env of a secret manager CLI child, tokens kept and the real user home, NO_COLOR defaulted on
func secretCLIEnv(ctx context.Context) (map[string]string, error) {
	env, err := tools.BuildSubprocessEnv(ctx, nil, tools.SubprocessEnvOptions{})
	if err != nil {
		return nil, err
	}
	if _, ok := env["NO_COLOR"]; !ok {
		env["NO_COLOR"] = "1"
	}
	return env, nil
}

// Inputs of the shared token subcommand beyond the console, token and env var
type rotateTokenOptions struct {
	Flag         string
	Intro        string
	Prompt       string
	Verify       func(token string) bool
	Save         func(name, value string) error
	EnvPath      func() string
	ClearCaches  func()
	DisabledNote string
}

// Shared token subcommand, prompt, verify when asked, then persist and clear caches
func rotateToken(console *pyrich.Console, given, tokenEnv string, o rotateTokenOptions) (int, error) {
	token := pystr.Strip(given)
	if token == "" {
		if !stdinIsATTY() {
			console.Print(fmt.Sprintf("[red]No TTY — pass the token with %s.[/red]", o.Flag))
			return 1, nil
		}
		console.Print(o.Intro)
		typed, err := maskedSecretPrompt(o.Prompt)
		if err != nil {
			return 1, err
		}
		token = pystr.Strip(typed)
	}
	if token == "" {
		console.Print("[red]Empty token, aborting.[/red]")
		return 1, nil
	}
	if o.Verify != nil && !o.Verify(token) {
		return 1, nil
	}
	if err := o.Save(tokenEnv, token); err != nil {
		return 1, err
	}
	os.Setenv(tokenEnv, token)
	o.ClearCaches()
	console.Print(fmt.Sprintf("[green]✓[/green] stored in %s as %s.  Takes effect on the next Icebath invocation.", o.EnvPath(), tokenEnv))
	if o.DisabledNote != "" {
		console.Print(o.DisabledNote)
	}
	return 0, nil
}

// Loops until the user enters an integer in 1..count, blank returning 0 when allowed
func promptIndex(console *pyrich.Console, prompt string, count int, allowEmpty bool, emptyMessage string) (int, error) {
	for {
		choice, err := console.Input(prompt)
		if err != nil {
			return 0, err
		}
		choice = pystr.Strip(choice)
		if choice == "" {
			if allowEmpty {
				return 0, nil
			}
			if emptyMessage != "" {
				console.Print(emptyMessage)
			}
			continue
		}
		parsed, ok := pyint.Parse(choice)
		if !ok {
			console.Print("  [red]Enter a number.[/red]")
			continue
		}
		if parsed.IsInt64() && parsed.Int64() >= 1 && parsed.Int64() <= int64(count) {
			return int(parsed.Int64()), nil
		}
		console.Print(fmt.Sprintf("  [red]Out of range — pick 1-%d.[/red]", count))
	}
}

// Mapping stored under key, a new one set when the entry is missing or None
func secretsSetdefaultMapping(cfg map[string]any, key string) map[string]any {
	if m, ok := cfg[key].(map[string]any); ok {
		return m
	}
	if cfg[key] != nil {
		panic(fmt.Sprintf("TypeError: '%s' object does not support item assignment", pyTypeName(cfg[key])))
	}
	m := map[string]any{}
	cfg[key] = m
	return m
}

// Sets secrets.<section>.enabled false in config.yaml and prints the message
func disableSecretSource(cli *secretsCLI, section, message string) (int, error) {
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	secretsSetdefaultMapping(secretsSetdefaultMapping(cfg, "secrets"), section)["enabled"] = false
	if err := secretsSaveConfig(cli.home, cfg); err != nil {
		return 1, err
	}
	cli.console.Print(message)
	return 0, nil
}

// String namespace value, empty for None
func nsText(ns map[string]any, key string) string {
	v := ns[key]
	if v == nil {
		return ""
	}
	return pyStr(v)
}

// Truthy namespace flag
func nsFlag(ns map[string]any, key string) bool {
	return pyTruthy(ns[key])
}

// Copy of the values in sorted order
func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
