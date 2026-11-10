package googlemeet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/spf13/cobra"
)

var (
	cliOut io.Writer = os.Stdout
	cliErr io.Writer = os.Stderr
)

// Process exit code of a CLI handler carried as an error
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Error for a non zero exit code
func exitErr(code int) error {
	if code == 0 {
		return nil
	}
	return &exitCodeError{code}
}

// Handler node subcommands run with their parsed namespace, swapped by tests
var nodeDispatch = nodeCommand

// Adds run, list, approve, remove, status and ping under the node command
func registerNodeCLI(node *cobra.Command) {
	ns := func(cmd *cobra.Command, extra map[string]any) map[string]any {
		out := map[string]any{"node_cmd": cmd.Name()}
		if p := cmd.Parent(); p != nil && p.Parent() != nil {
			out["meet_command"] = p.Name()
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	var host, displayName string
	var port int
	run := &cobra.Command{Use: "run", Short: "Start a node server on this machine.", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitErr(nodeDispatch(ns(cmd, map[string]any{"host": host, "port": port, "display_name": displayName})))
		}}
	run.Flags().StringVar(&host, "host", "0.0.0.0", "")
	run.Flags().IntVar(&port, "port", 18789, "")
	run.Flags().StringVar(&displayName, "display-name", "icebath-meet-node", "")
	list := &cobra.Command{Use: "list", Short: "List approved remote nodes.", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return exitErr(nodeDispatch(ns(cmd, nil))) }}
	approve := &cobra.Command{Use: "approve name url token", Short: "Register a remote node on the gateway.", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, a []string) error {
			return exitErr(nodeDispatch(ns(cmd, map[string]any{"name": a[0], "url": a[1], "token": a[2]})))
		}}
	node.AddCommand(run, list, approve)
	for _, nh := range [][2]string{{"remove", "Forget a registered node."}, {"status", "Ping a registered node."}, {"ping", "Alias for status."}} {
		node.AddCommand(&cobra.Command{Use: nh[0] + " name", Short: nh[1], Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, a []string) error {
				return exitErr(nodeDispatch(ns(cmd, map[string]any{"name": a[0]})))
			}})
	}
	node.RunE = func(cmd *cobra.Command, _ []string) error {
		return fmt.Errorf("the following arguments are required: node_cmd")
	}
}

// Namespace string value
func nsStr(ns map[string]any, key string) string {
	s, _ := ns[key].(string)
	return s
}

// Dispatches icebath meet node subcommands, returning the process exit code
func nodeCommand(ns map[string]any) int {
	ctx := context.Background()
	reg := newNodeRegistry(ctx, "")
	switch cmd := nsStr(ns, "node_cmd"); cmd {
	case "run":
		return nodeRun(ctx, ns)
	case "list":
		nodes, err := reg.listAll()
		if err != nil {
			fmt.Fprintln(cliErr, err)
			return 1
		}
		if len(nodes) == 0 {
			fmt.Fprintln(cliOut, "no nodes registered")
		}
		for _, n := range nodes {
			tok := []rune(pyrepr.Str(n.Vals["token"]))
			fmt.Fprintf(cliOut, "%s\t%s\ttoken=%s…\n", pyrepr.Str(n.Vals["name"]), pyrepr.Str(n.Vals["url"]), string(tok[:min(6, len(tok))]))
		}
		return 0
	case "approve":
		if err := reg.add(ctx, nsStr(ns, "name"), nsStr(ns, "url"), nsStr(ns, "token")); err != nil {
			fmt.Fprintf(cliErr, "ValueError: %v\n", err)
			return 1
		}
		fmt.Fprintf(cliOut, "approved node %s at %s\n", pyrepr.StrRepr(nsStr(ns, "name")), nsStr(ns, "url"))
		return 0
	case "remove":
		ok, err := reg.remove(ctx, nsStr(ns, "name"))
		if err != nil {
			fmt.Fprintln(cliErr, err)
			return 1
		}
		if ok {
			fmt.Fprintf(cliOut, "removed %s\n", pyrepr.StrRepr(nsStr(ns, "name")))
			return 0
		}
		fmt.Fprintf(cliOut, "no such node: %s\n", pyrepr.StrRepr(nsStr(ns, "name")))
		return 1
	case "status", "ping":
		return nodePing(reg, nsStr(ns, "name"))
	default:
		fmt.Fprintf(cliErr, "unknown node command: %s\n", pyrepr.Repr(ns["node_cmd"]))
		return 2
	}
}

// Pings a registered node and prints the JSON result
func nodePing(reg *nodeRegistry, name string) int {
	entry, err := reg.get(name)
	if err != nil {
		fmt.Fprintln(cliErr, err)
		return 1
	}
	if entry == nil {
		fmt.Fprintf(cliErr, "no such node: %s\n", pyrepr.StrRepr(name))
		return 1
	}
	url, _ := entry.Vals["url"].(string)
	tok, _ := entry.Vals["token"].(string)
	var result *pyjson.Object
	client, err := newNodeClient(url, tok, 10*time.Second)
	if err == nil {
		result, err = client.ping()
	}
	if err != nil {
		text, _ := pyjson.Dumps(pyjson.ObjectOf("ok", false, "error", err.Error()))
		fmt.Fprintln(cliOut, text)
		return 1
	}
	out := pyjson.ObjectOf("ok", true, "node", name)
	out.Update(result)
	text, _ := pyjson.Dumps(out)
	fmt.Fprintln(cliOut, text)
	return 0
}

// Runs a node server until interrupted, printing the token and approve command
func nodeRun(ctx context.Context, ns map[string]any) int {
	host, port := nsStr(ns, "host"), ns["port"].(int)
	server := newNodeServer(ctx, host, port, "", nsStr(ns, "display_name"))
	token, err := server.ensureToken(ctx)
	if err != nil {
		fmt.Fprintf(cliErr, "%s: %v\n", pyExcName(err), err)
		return 1
	}
	fmt.Fprintf(cliOut, "[meet-node] display_name=%s\n[meet-node] listening on ws://%s:%d\n[meet-node] token (copy to gateway): %s\n[meet-node] approve with:\n             icebath meet node approve <name> ws://<host>:%d %s\n",
		server.displayName, host, port, token, port, token)
	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	if err := server.serve(runCtx); err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			fmt.Fprintf(cliErr, "OSError: %v\n", err)
			return 1
		}
		fmt.Fprintf(cliErr, "[meet-node] error: %v\n", err)
		return 2
	}
	return 0
}
