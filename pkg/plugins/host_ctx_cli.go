package plugins

import (
	"context"
	"fmt"
	"os"
)

func init() {
	hostCtxDispatch["register_cli_command"] = hostMethod("register_cli_command", []pyParam{req("name"), req("help"), req("run"), opt("description", "")},
		func(_ context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			if c.ignoredAfterAbandonedLoad("register_cli_command") {
				return nil, nil
			}
			run, err := argCallable("register_cli_command", "run", a["run"])
			if err != nil {
				return nil, err
			}
			return c.registerCLICommand(&CLICommand{Name: argStr(a["name"]), Help: argStr(a["help"]), Description: argStr(a["description"]),
				Parse: guestCLIRun(run)}), nil
		})
}

// Runs a guest command with its raw arguments and the top level options, printing the output it returns
func guestCLIRun(run *HostedCallable) func(ctx context.Context, top map[string]any, argv []string) (CLIOutcome, error) {
	return func(ctx context.Context, top map[string]any, argv []string) (CLIOutcome, error) {
		raw, err := run.Call(ctx, []any{anyList(argv)}, map[string]any{"options": top})
		if err != nil {
			return CLIOutcome{}, err
		}
		reply, ok := raw.(map[string]any)
		if !ok {
			return CLIOutcome{}, fmt.Errorf("plugin command answered %T, not its output and exit code", raw)
		}
		if output := wireStr(reply["output"]); output != "" {
			fmt.Fprint(os.Stdout, output)
		}
		code, _ := wireInt(reply["code"])
		return CLIOutcome{Code: int(code), Exited: true, Dispatched: true}, nil
	}
}
