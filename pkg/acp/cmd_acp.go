package acp

import (
	"context"
	"io"
	"os"
)

// Namespace attribute and the entry flag it forwards, in forwarding order
var acpFlags = [][2]string{
	{"acp_version", "--version"},
	{"check", "--check"},
	{"setup", "--setup"},
	{"setup_browser", "--setup-browser"},
	{"assume_yes", "--yes"},
}

// Entry point the acp subcommand hands its forwarded flags to
var entryMain = Main

// Launches Icebath Agent as an ACP server, the launch flags exported as their env equivalents first
func CmdACP(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	for _, pair := range [][2]string{{"accept_hooks", "ICEBATH_ACCEPT_HOOKS"}, {"ignore_rules", "ICEBATH_IGNORE_RULES"}} {
		if truthy(ns[pair[0]]) {
			os.Setenv(pair[1], "1")
		}
	}
	for _, pair := range [][2]string{{"model", "ICEBATH_MODEL"}, {"provider", "ICEBATH_INFERENCE_PROVIDER"}} {
		if value := ns[pair[0]]; truthy(value) {
			os.Setenv(pair[1], pyStr(value))
		}
	}
	var argv []string
	for _, pair := range acpFlags {
		if truthy(ns[pair[0]]) {
			argv = append(argv, pair[1])
		}
	}
	return entryMain(ctx, argv, w, os.Stderr), nil
}
