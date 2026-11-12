package pluginhost

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Terminal stream handles the parent passed for plugin CLI commands, read once at child start
var terminalSpec string

// Takes the terminal handles out of the environment so plugin code and its subprocesses never see them
func takeTerminalSpec() {
	terminalSpec = os.Getenv(plugins.HostTerminalEnv)
	os.Unsetenv(plugins.HostTerminalEnv)
}

// Argparse parser class plugin commands build on, its unknown subcommand error three short lines
func (rt *runtime) cliParserClass(p py) Ref {
	rt.mu.Lock()
	cls := rt.cliParserCls
	rt.mu.Unlock()
	if cls != nil {
		return cls
	}
	argparse := p.imp("argparse")
	cls = rt.class(p, "icebath_cli._parser", "IcebathArgumentParser", []any{p.get(argparse, "ArgumentParser")}, map[string]any{
		"_check_value": method(func(p py, self Ref, args []any, _ map[string]any) any {
			if len(args) != 2 {
				throw(&pyhost.Exception{Type: "TypeError", Message: fmt.Sprintf("_check_value() takes 3 positional arguments but %d were given", len(args)+1)})
			}
			action, value := p.obj(args[0]), p.obj(args[1])
			if p.isinstance(action, p.attr("argparse", "_SubParsersAction")) && !p.truthy(p.m(p.get(action, "choices"), "__contains__", value)) {
				prog, text := p.str(p.get(self, "prog")), p.str(value)
				lines := []string{fmt.Sprintf("%s: '%s' is not a `%s` command.", prog, text, prog)}
				choices := p.call(p.attr("builtins", "list"), p.get(action, "choices"))
				close := p.items(p.callKw(p.attr("difflib", "get_close_matches"), []any{text, choices}, map[string]any{"n": 3, "cutoff": 0.6}))
				moved := p.str(rt.decode(p, rt.send(p, "sdk", map[string]any{"op": "moved_command_hint", "args": []any{prog, text}, "kwargs": map[string]any{}}), rt.resolveEventsOnly))
				switch {
				case moved != "":
					lines = append(lines, moved)
				case len(close) > 0:
					names := make([]string, len(close))
					for i, c := range close {
						names[i] = p.str(c)
					}
					lines = append(lines, "Did you mean: "+strings.Join(names, ", ")+"?")
				}
				lines = append(lines, fmt.Sprintf("Run `%s --help` to see all commands.", prog))
				p.m(self, "exit", 2, strings.Join(lines, "\n")+"\n")
				return nil
			}
			return p.m(p.attr("argparse", "ArgumentParser"), "_check_value", self, action, value)
		}),
	})
	rt.mu.Lock()
	if rt.cliParserCls == nil {
		rt.cliParserCls = p.keep(cls)
	}
	cls = rt.cliParserCls
	rt.mu.Unlock()
	return cls
}

// Value a parent sent either as a held callable ref or as plain data
func (rt *runtime) heldOrValue(p py, params map[string]any, key string) Ref {
	if ref, ok := params[key+"_ref"]; ok && ref != nil {
		fn, _ := rt.lookup(toInt(ref))
		if fn == nil {
			throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host callable %d was released", toInt(ref))})
		}
		return fn
	}
	return rt.decode(p, params[key], rt.resolveEventsOnly)
}

// Builds the plugin command parser, runs its setup_fn and sets func to handler_fn, the parser kept even when setup raises
func opBuildCLI(rt *runtime, p py, _ context.Context, params map[string]any) any {
	argparse := p.imp("argparse")
	parser := p.callKw(rt.cliParserClass(p), nil, map[string]any{"prog": "icebath " + pyStr(params["name"]),
		"description": pyStr(params["description"]), "formatter_class": p.get(argparse, "RawDescriptionHelpFormatter")})
	id := rt.remember(p, pyStr(params["plugin"]), parser)
	setup := rt.heldOrValue(p, params, "setup")
	err := p.attempt(func() {
		p.call(setup, parser)
		if handler := rt.heldOrValue(p, params, "handler"); !p.isNone(handler) {
			p.mKw(parser, "set_defaults", nil, map[string]any{"func": handler})
		}
	}, "Exception")
	if err != nil {
		pe := err.(*pyhost.PyError)
		return map[string]any{"parser": id, "error": p.str(pe.Exception())}
	}
	return map[string]any{"parser": id, "error": nil}
}

// Parses argv with the plugin parser onto the top level namespace and runs its func on the terminal
func opRunCLI(rt *runtime, p py, _ context.Context, params map[string]any) any {
	parser, _ := rt.lookup(toInt(params["parser"]))
	if parser == nil {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host parser %d was released", toInt(params["parser"]))})
	}
	argv := []any{}
	for _, a := range orList(params["argv"]).([]any) {
		argv = append(argv, pyStr(a))
	}
	restore := rt.attachTerminal(p)
	defer restore()
	reply := map[string]any{"rc": int64(0), "exit": nil, "dispatched": false, "extras": []any{}}
	err := p.attempt(func() {
		parsed := p.m(parser, "parse_known_args", argv, nil)
		sub, extras := p.item(parsed, 0), p.items(p.item(parsed, 1))
		top, _ := rt.decodeValue(p, orEmptyMap(params["top"]), rt.resolveEventsOnly).(map[string]any)
		ns := p.callKw(p.attr("argparse", "Namespace"), nil, kwOrEmpty(top))
		for _, kv := range p.items(p.m(p.call(p.attr("builtins", "vars"), sub), "items")) {
			p.set(ns, p.str(p.item(kv, 0)), p.item(kv, 1))
		}
		if len(extras) > 0 {
			out := make([]any, len(extras))
			for i, e := range extras {
				out[i] = p.str(e)
			}
			reply["extras"] = out
			return
		}
		fn := p.getOr(ns, "func")
		if fn == nil {
			return
		}
		reply["dispatched"] = true
		result := p.call(fn, ns)
		if p.isinstance(result, p.attr("builtins", "int")) {
			reply["rc"] = p.val(p.call(p.attr("builtins", "int"), result)).(int64)
		}
	}, "BaseException")
	if err == nil {
		return reply
	}
	exc := err.(*pyhost.PyError).Exception()
	switch {
	case p.isinstance(exc, p.attr("builtins", "SystemExit")):
		reply["exit"] = exitCode(p, p.get(exc, "code"))
	case p.isinstance(exc, p.attr("builtins", "Exception")):
		p.m(p.imp("traceback"), "print_exception", exc)
		reply["exit"] = int64(1)
	default:
		throw(err)
	}
	return reply
}

// Points the standard streams at the parent's terminal for the length of a command, flushing on both edges
func (rt *runtime) attachTerminal(p py) func() {
	parts := strings.Split(terminalSpec, ",")
	if len(parts) != 3 {
		throw(&pyhost.Exception{Type: "RuntimeError", Message: "the plugin host was started without the terminal streams a CLI command runs on"})
	}
	osm := p.imp("os")
	flush := func() {
		p.m(p.attr("sys", "stdout"), "flush")
		p.m(p.attr("sys", "stderr"), "flush")
	}
	flush()
	fds := make([]any, 3)
	for i, raw := range parts {
		fds[i] = rt.terminalFD(p, i, raw)
	}
	handles := swapStdHandles(parts)
	saved := make([]Ref, 3)
	for i, fd := range fds {
		saved[i] = p.m(osm, "dup", i)
		p.m(osm, "dup2", fd, i)
	}
	return func() {
		flush()
		handles()
		for i := range saved {
			p.m(osm, "dup2", saved[i], i)
			p.m(osm, "close", saved[i])
		}
	}
}
