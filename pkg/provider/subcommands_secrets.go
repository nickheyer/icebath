package provider

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// The secrets command parser, the bitwarden and onepassword integrations under secrets_command
type secretsCommandParser struct {
	Help         string
	Description  string
	Dest         string
	Integrations []*secretsParser
}

const secretsCommandDescription = "Pull API keys from an external secret manager at process startup " +
	"instead of storing them in ~/.icebath/.env.  Supports Bitwarden " +
	"Secrets Manager and 1Password.  See: " +
	"https://hermes-agent.nousresearch.com/docs/user-guide/secrets/"

// Attaches the secrets subcommand tree with both integrations registered
func buildSecretsParser() *secretsCommandParser {
	bitwarden := &secretsParser{Name: "bitwarden", Aliases: []string{"bw"}, Help: "Bitwarden Secrets Manager integration"}
	onepassword := &secretsParser{Name: "onepassword", Aliases: []string{"op", "1password"}, Help: "1Password (op:// references) integration"}
	registerBitwardenCLI(bitwarden)
	registerOnePasswordCLI(onepassword)
	return &secretsCommandParser{
		Help:         "Manage external secret sources (Bitwarden, 1Password)",
		Description:  secretsCommandDescription,
		Dest:         "secrets_command",
		Integrations: []*secretsParser{bitwarden, onepassword},
	}
}

// Integration parser named by its name or one of its aliases, nil for an unknown one
func (p *secretsCommandParser) integration(name string) *secretsParser {
	for _, integration := range p.Integrations {
		if integration.Name == name {
			return integration
		}
		for _, alias := range integration.Aliases {
			if alias == name {
				return integration
			}
		}
	}
	return nil
}

// argparse style help of the secrets command, its integrations listed as choices
func (p *secretsCommandParser) printHelp(w io.Writer) {
	names := make([]string, 0, len(p.Integrations))
	for _, integration := range p.Integrations {
		names = append(names, integration.Name)
	}
	choices := "{" + strings.Join(names, ",") + "}"
	var b strings.Builder
	fmt.Fprintf(&b, "usage: icebath secrets [-h] %s ...\n\n%s\n\npositional arguments:\n  %s\n", choices, p.Description, choices)
	for _, integration := range p.Integrations {
		label := integration.Name
		if len(integration.Aliases) > 0 {
			label += " (" + strings.Join(integration.Aliases, ", ") + ")"
		}
		fmt.Fprintf(&b, "    %-20s%s\n", label, integration.Help)
	}
	b.WriteString("\noptions:\n  -h, --help            show this help message and exit\n")
	io.WriteString(w, b.String())
}

// argparse style help of one integration, its subcommands listed as choices
func (p *secretsParser) printHelp(w io.Writer) {
	names := make([]string, 0, len(p.Commands))
	for _, cmd := range p.Commands {
		names = append(names, cmd.Name)
	}
	choices := "{" + strings.Join(names, ",") + "}"
	var b strings.Builder
	fmt.Fprintf(&b, "usage: icebath secrets %s [-h] %s ...\n\npositional arguments:\n  %s\n", p.Name, choices, choices)
	for _, cmd := range p.Commands {
		fmt.Fprintf(&b, "    %-20s%s\n", cmd.Name, cmd.Help)
	}
	b.WriteString("\noptions:\n  -h, --help            show this help message and exit\n")
	io.WriteString(w, b.String())
}

// SecretsCommand runs icebath secrets from the parsed namespace, printing help when no integration was named
func SecretsCommand(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	parser := buildSecretsParser()
	return dispatchSecrets(parser, &secretsCLI{ctx: ctx, home: h, console: pyrich.NewConsole(w)}, ns, w)
}

// Runs the handler the namespace names, help for a missing integration or subcommand
func dispatchSecrets(parser *secretsCommandParser, cli *secretsCLI, ns map[string]any, w io.Writer) (int, error) {
	sub, ok := ns[parser.Dest].(string)
	if !ok {
		parser.printHelp(w)
		return 0, nil
	}
	integration := parser.integration(sub)
	if integration == nil {
		return 2, fmt.Errorf("argument secrets_command: invalid choice: %s", pyrepr.Repr(sub))
	}
	handler := integration.handler(ns)
	if handler == nil {
		integration.printHelp(w)
		return 0, nil
	}
	return handler(cli, ns)
}
