package commands

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

var pluginCommandNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// Gives the top level flags their argparse dests and the continue flag its const True
func (c *CLI) rootArgparseShape(root *cobra.Command) {
	dest(root, "in", "in_dir")
	dest(root, "continue", "continue_last")
	constTrue(root, "continue")
	dest(root, "native", "tui_native")
}

// Gives the chat flags their argparse dests and the SUPPRESS defaults that keep top level values alive
func (c *CLI) chatArgparseShape(chat *cobra.Command) {
	dest(chat, "oneshot", "oneshot_exit")
	dest(chat, "format", "output_format")
	dest(chat, "in", "in_dir")
	dest(chat, "continue", "continue_last")
	constTrue(chat, "continue")
	dest(chat, "native", "tui_native")
	noneDefault(chat, "max-turns", "run-budget")
	suppress(chat, "model", "toolsets", "reasoning", "skills", "provider", "base-url", "api-key", "verbose", "resume",
		"no-restore-cwd", "in", "continue", "create-if-missing", "worktree", "accept-hooks", "checkpoints", "yolo",
		"pass-session-id", "ignore-user-config", "ignore-rules", "safe-mode", "tui", "native", "cli")
}

// Install command when a top level word names a catalog memory plugin that resolves nowhere, else empty
func pluginCommandInstallHint(ctx context.Context, prog, value string) string {
	if prog != "icebath" || !pluginCommandNameRe.MatchString(value) {
		return ""
	}
	h := home.Dir(ctx)
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	if memory.FindProviderDir(ctx, &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}, value) != "" {
		return ""
	}
	return memory.CatalogInstallHint(ctx, value, "memory")
}

// Every name and alias a parser level accepts as its subcommand, the argparse subparser choices
func subcommandChoices(cmd *cobra.Command) []string {
	var names []string
	for _, sub := range cmd.Commands() {
		if strings.HasPrefix(sub.Name(), "__") {
			continue
		}
		names = append(names, sub.Name())
		names = append(names, sub.Aliases...)
	}
	return names
}
