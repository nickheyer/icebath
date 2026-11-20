package pluginsdk

import (
	"context"
	"fmt"
)

// One `icebath <name>` subcommand of a plugin, run with the arguments after its name and the options given before it
type CLICommand struct {
	Name        string
	Help        string
	Description string
	// Runs the command and returns what it prints on standard output with its exit code, the plugin having no terminal of its own
	Run func(ctx context.Context, argv []string, options map[string]any) (output string, code int, err error)
}

// Registers a subcommand whose handler answers its output and exit code
func (h *host) RegisterCLICommand(cmd CLICommand) (*Handle, error) {
	if cmd.Run == nil {
		return nil, fmt.Errorf("CLI command %q has no run function", cmd.Name)
	}
	run := h.peer.funcRef(cmd.Name, func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
		output, code, err := cmd.Run(ctx, stringsOf(argAt(args, 0)), mapOrEmpty(asMap(kwargs["options"])))
		if err != nil {
			return nil, err
		}
		return map[string]any{"output": output, "code": int64(code)}, nil
	})
	return h.register("register_cli_command", nil, map[string]any{"name": cmd.Name, "help": cmd.Help, "setup_fn": nil,
		"handler_fn": run, "description": cmd.Description})
}

// Answers the command handler as the parser the host runs the command through, after the setup callback a registration named ran
func (p *peer) buildCLI(ctx context.Context, params map[string]any) (any, error) {
	reply := map[string]any{"parser": int64(0), "error": nil}
	if id, ok := integer(params["handler_ref"]); ok && p.function(id) != nil {
		reply["parser"] = id
	}
	if setup := p.function(params["setup_ref"]); setup != nil {
		if _, err := setup(ctx, []any{}, map[string]any{"name": text(params["name"]), "description": text(params["description"])}); err != nil {
			reply["error"] = err.Error()
		}
	}
	return reply, nil
}

// Runs a plugin command with its arguments and the top level options, answering its output and exit code
func (p *peer) runCLI(ctx context.Context, params map[string]any) (any, error) {
	reply := map[string]any{"dispatched": false, "rc": int64(0), "extras": []any{}}
	fn := p.function(params["parser"])
	if fn == nil {
		return reply, nil
	}
	value, err := fn(ctx, []any{p.decodeList(params["argv"])}, map[string]any{"options": p.decodeMap(params["top"])})
	if err != nil {
		return nil, err
	}
	reply["dispatched"] = true
	switch result := value.(type) {
	case string:
		reply["output"] = result
	case map[string]any:
		if output, ok := result["output"].(string); ok {
			reply["output"] = output
		}
		if code, ok := integer(result["code"]); ok {
			reply["rc"] = code
		}
	default:
		if code, ok := integer(value); ok {
			reply["rc"] = code
		}
	}
	return reply, nil
}
