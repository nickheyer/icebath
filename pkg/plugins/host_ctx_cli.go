package plugins

import (
	"context"
	"errors"
	"sync"
)

// Plugin CLI command registered from the plugin host, its argparse parser living in the child
type hostedCLICommand struct {
	host        *PluginHost
	plugin      string
	name        string
	description string
	setup       any
	handler     any
	mu          sync.Mutex
	built       bool
	parser      int64
	generation  int
}

func init() {
	hostCtxDispatch["register_cli_command"] = hostMethod("register_cli_command", []pyParam{req("name"), req("help"), req("setup_fn"), opt("handler_fn", nil), opt("description", "")},
		func(_ context.Context, h *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			if c.ignoredAfterAbandonedLoad("register_cli_command") {
				return nil, nil
			}
			cmd := &hostedCLICommand{host: h, plugin: c.manifest.ID(), name: argStr(a["name"]), description: argStr(a["description"]),
				setup: a["setup_fn"], handler: a["handler_fn"]}
			return c.registerCLICommand(&CLICommand{Name: cmd.name, Help: argStr(a["help"]), Description: cmd.description,
				Setup: cmd.attach, Parse: cmd.parse}), nil
		})
}

// Builds the plugin parser in the child when the command is attached, raising what setup_fn raised
func (c *hostedCLICommand) attach(any) {
	if err := c.build(c.host.baseCtx); err != nil {
		panic(err)
	}
}

// Adds a value to child params as a held callable id, or as plain data when the plugin passed no callable
func (h *PluginHost) heldParam(params map[string]any, key string, value any, generation *int) {
	if f, ok := value.(*HostedCallable); ok && f != nil {
		params[key+"_ref"] = f.ref
		*generation = f.generation
		return
	}
	params[key] = h.encodeArgs(value)
}

// Creates the parser through setup_fn and sets handler_fn as its func, keeping a parser setup_fn left half built
func (c *hostedCLICommand) build(ctx context.Context) error {
	params := map[string]any{"plugin": c.plugin, "name": c.name, "description": c.description}
	generation := -1
	c.host.heldParam(params, "setup", c.setup, &generation)
	c.host.heldParam(params, "handler", c.handler, &generation)
	var gen *int
	if generation >= 0 {
		gen = &generation
	}
	raw, err := c.host.call(ctx, "build_cli", params, gen, 0, nil)
	if err != nil {
		return err
	}
	reply, _ := raw.(map[string]any)
	parser, _ := wireInt(reply["parser"])
	c.host.mu.Lock()
	current := c.host.generation
	c.host.mu.Unlock()
	c.mu.Lock()
	c.built, c.parser, c.generation = true, parser, current
	c.mu.Unlock()
	if msg, failed := reply["error"].(string); failed {
		return errors.New(msg)
	}
	return nil
}

// Parses argv with the plugin parser onto the top level namespace and runs the func it selects in the child
func (c *hostedCLICommand) parse(ctx context.Context, top map[string]any, argv []string) (CLIOutcome, error) {
	c.mu.Lock()
	built := c.built
	c.mu.Unlock()
	if !built {
		if err := c.build(ctx); err != nil {
			return CLIOutcome{}, err
		}
	}
	c.mu.Lock()
	parser, generation := c.parser, c.generation
	c.mu.Unlock()
	raw, err := c.host.call(ctx, "run_cli", map[string]any{"parser": parser, "top": c.host.encodeArgs(top), "argv": anyList(argv)},
		&generation, 0, nil)
	if err != nil {
		return CLIOutcome{}, err
	}
	reply, _ := raw.(map[string]any)
	out := CLIOutcome{Dispatched: reply["dispatched"] == true}
	if code, ok := wireInt(reply["exit"]); ok {
		out.Code, out.Exited = int(code), true
	} else if rc, ok := wireInt(reply["rc"]); ok {
		out.Code = int(rc)
	}
	for _, e := range listOrEmpty(reply["extras"]) {
		out.Extras = append(out.Extras, wireStr(e))
	}
	return out, nil
}
