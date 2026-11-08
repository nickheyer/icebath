package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// One icebath proxy invocation, its pool env, namespace and output streams
type proxyCLI struct {
	ctx  context.Context
	env  PoolEnv
	ns   map[string]any
	out  io.Writer
	errw io.Writer
}

// Prints a line on stderr
func proxyErr(errw io.Writer, msg string) {
	fmt.Fprintln(errw, msg)
}

// Runs the proxy server in the foreground
func cmdProxyStart(cli *proxyCLI) (int, error) {
	if !aiohttpAvailable {
		proxyErr(cli.errw, "icebath proxy requires aiohttp. Run `icebath setup` to install it.")
		return 1, nil
	}
	provider := nsText(cli.ns, "provider")
	if provider == "" {
		provider = "nous"
	}
	adapter, err := getAdapter(cli.env, provider)
	if err != nil {
		proxyErr(cli.errw, "Error: "+pyrepr.ExceptionMessage(err))
		return 2, nil
	}
	if !adapter.IsAuthenticated() {
		authHint := "icebath auth add " + adapter.Name()
		if hinter, ok := adapter.(upstreamAuthHinter); ok {
			authHint = hinter.AuthHint()
		}
		proxyErr(cli.errw, fmt.Sprintf("Not logged into %s. Run `%s` first.", adapter.DisplayName(), authHint))
		return 2, nil
	}
	host := nsText(cli.ns, "host")
	if host == "" {
		host = DefaultProxyHost
	}
	port, _ := cli.ns["port"].(int)
	if port == 0 {
		port = DefaultProxyPort
	}
	proxyErr(cli.errw, fmt.Sprintf("Starting Icebath proxy for %s\n"+
		"  Listening on:  http://%s:%d/v1\n"+
		"  Forwarding to: (resolved per-request from your subscription)\n"+
		"  Use any bearer token in the client — the proxy attaches your real credential.\n"+
		"\n"+
		"Press Ctrl+C to stop.", adapter.DisplayName(), pyurl.FormatURLHost(host), port))
	if err := runProxyServer(cli.ctx, adapter, host, port, nil, cli.env.log()); err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			proxyErr(cli.errw, fmt.Sprintf("proxy: failed to bind %s:%d: %s", host, port, pyrepr.ExceptionMessage(err)))
			return 1, nil
		}
		return 1, err
	}
	return 0, nil
}

// Prints the status of each configured upstream adapter
func cmdProxyStatus(cli *proxyCLI) (int, error) {
	fmt.Fprintln(cli.out, "Icebath proxy upstream adapters")
	fmt.Fprintln(cli.out)
	for _, name := range proxyAdapterNames() {
		adapter, err := getAdapter(cli.env, name)
		if err != nil {
			return 1, err
		}
		if !adapter.IsAuthenticated() {
			fmt.Fprintf(cli.out, "  [%-8s] %s — not logged in\n", name, adapter.DisplayName())
			continue
		}
		cred, err := adapter.GetCredential()
		if err != nil {
			fmt.Fprintf(cli.out, "  [%-8s] %s — credentials need attention (%s)\n", name, adapter.DisplayName(), pyrepr.ExceptionMessage(err))
			continue
		}
		expires := ""
		if cred.ExpiresAt != "" {
			expires = fmt.Sprintf(" (bearer expires %s)", cred.ExpiresAt)
		}
		fmt.Fprintf(cli.out, "  [%-8s] %s — ready%s\n", name, adapter.DisplayName(), expires)
	}
	fmt.Fprintln(cli.out)
	fmt.Fprintln(cli.out, "Start the proxy with: icebath proxy start [--provider <name>]")
	return 0, nil
}

// Lists the available proxy upstream providers
func cmdProxyListProviders(cli *proxyCLI) (int, error) {
	fmt.Fprintln(cli.out, "Available proxy upstream providers:")
	for _, name := range proxyAdapterNames() {
		adapter, err := getAdapter(cli.env, name)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(cli.out, "  %s  — %s\n", name, adapter.DisplayName())
	}
	return 0, nil
}

var proxySubcommands = map[string]func(cli *proxyCLI) (int, error){
	"start":     cmdProxyStart,
	"status":    cmdProxyStatus,
	"providers": cmdProxyListProviders,
	"list":      cmdProxyListProviders,
}

// CmdProxy dispatches icebath proxy <subcommand>, a missing or unknown one printing the short help
func CmdProxy(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (int, error) {
	cli := &proxyCLI{ctx: ctx, env: homePoolEnv(h), ns: ns, out: w, errw: errw}
	if handler, ok := proxySubcommands[nsText(ns, "proxy_command")]; ok {
		return handler(cli)
	}
	proxyErr(errw, "icebath proxy — local OpenAI-compatible proxy that attaches your\n"+
		"OAuth-authenticated provider credentials to outbound requests.\n"+
		"\n"+
		"Subcommands:\n"+
		"  icebath proxy start [--provider nous|xai] [--host 127.0.0.1] [--port 8645]\n"+
		"      Run the proxy in the foreground.\n"+
		"  icebath proxy status\n"+
		"      Show which upstream adapters are ready.\n"+
		"  icebath proxy providers\n"+
		"      List available upstream providers.\n")
	return 0, nil
}
