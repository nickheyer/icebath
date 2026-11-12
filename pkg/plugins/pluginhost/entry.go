package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Exit status of a child whose interpreter could not start or finalize
const (
	exitStartFailed    = 3
	exitFinalizeFailed = 4
	exitFinalizeHung   = 5
	finalizeDeadline   = 20 * time.Second
)

// Runs the plugin host child when this binary was started as one, exiting with its status
func DispatchPluginHostEntry(ctx context.Context) {
	plugins.MarkPluginHostEntry()
	if len(os.Args) > 1 && os.Args[1] == plugins.PluginHostEntry {
		os.Exit(runChild(ctx, os.Args[2:]))
	}
}

// Child modes, the request channel by default, profile extraction and a memory provider command
func runChild(ctx context.Context, args []string) int {
	if len(args) < 2 || args[0] != "--python" {
		fmt.Fprintf(os.Stderr, "plugin host: usage: %s --python <interpreter> [--extract-profiles <dir> <module> | --run-memory-cli <params> <result>]\n", plugins.PluginHostEntry)
		return exitStartFailed
	}
	python := args[1]
	args = args[2:]
	takeTerminalSpec()
	denyNetwork := os.Getenv(plugins.HostDenyNetworkEnv) == "1"
	os.Unsetenv(plugins.HostDenyNetworkEnv)
	var reader, writer *os.File
	if len(args) == 0 || args[0] != "--run-memory-cli" {
		var err error
		if reader, writer, err = takeProtocolStreams(); err != nil {
			fmt.Fprintf(os.Stderr, "plugin host: %v\n", err)
			return exitStartFailed
		}
	}
	h, err := startInterpreter(python)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin host: %v\n", err)
		return exitStartFailed
	}
	code := 0
	err = h.Do(func(g *pyhost.GIL) error {
		return run(g, func(p py) {
			if denyNetwork {
				applyNetworkDenial(p)
			}
		})
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin host: %v\n", err)
		return finalize(h, exitStartFailed)
	}
	switch {
	case len(args) >= 3 && args[0] == "--extract-profiles":
		code = extractProfilesMain(h, writer, args[1], args[2])
	case len(args) >= 3 && args[0] == "--run-memory-cli":
		code = runMemoryCLIMain(h, args[1], args[2])
	default:
		code = serve(ctx, h, reader, writer)
	}
	return finalize(h, code)
}

// Starts the interpreter the parent named, with its PYTHONPATH entries first on sys.path
func startInterpreter(exe string) (*pyhost.Host, error) {
	in, err := pyhost.Discover(exe)
	if err != nil {
		return nil, err
	}
	var extra []string
	for _, p := range filepath.SplitList(os.Getenv("PYTHONPATH")) {
		if p != "" {
			extra = append(extra, p)
		}
	}
	return pyhost.Start(pyhost.Options{Interpreter: in, ExtraPaths: extra, Argv: []string{exe}})
}

// Finalizes the interpreter, a non daemon plugin thread holding it past the deadline ending the process
func finalize(h *pyhost.Host, code int) int {
	os.Stdout.Sync()
	timer := time.AfterFunc(finalizeDeadline, func() {
		fmt.Fprintf(os.Stderr, "plugin host: Py_FinalizeEx did not return within %s, a non daemon plugin thread is still running\n", finalizeDeadline)
		os.Exit(exitFinalizeHung)
	})
	rc, err := h.Finalize(finalizeDeadline)
	timer.Stop()
	if err != nil || rc != 0 {
		fmt.Fprintf(os.Stderr, "plugin host: Py_FinalizeEx returned %d: %v\n", rc, err)
		if code == 0 {
			return exitFinalizeFailed
		}
	}
	return code
}

// Plugin Doctor children refuse outbound socket connects before any plugin code runs
func applyNetworkDenial(p py) {
	deny := p.fn("_deny_network", func(p py, _ []any, _ map[string]any) any {
		throw(&pyhost.Exception{Type: "RuntimeError", Message: "network access is disabled while Plugin Doctor runs"})
		return nil
	})
	socket := p.imp("socket")
	p.set(socket, "create_connection", deny)
	cls := p.get(socket, "socket")
	p.set(cls, "connect", deny)
	p.set(cls, "connect_ex", deny)
}

// Serves parent requests until shutdown or until the parent closes the channel
func serve(ctx context.Context, h *pyhost.Host, reader io.ReadCloser, writer io.WriteCloser) int {
	rt := newRuntime(h)
	setup := func(p py) {
		level := os.Getenv(plugins.HostLogLevelEnv)
		if level == "" {
			level = "WARNING"
		}
		p.mKw(p.imp("logging"), "basicConfig", nil, map[string]any{"stream": p.attr("sys", "stderr"), "level": level,
			"format": "%(levelname)s %(name)s: %(message)s"})
		rt.setup(p)
	}
	if err := h.Do(func(g *pyhost.GIL) error { return runKeep(g, setup) }); err != nil {
		fmt.Fprintf(os.Stderr, "plugin host: %v\n", err)
		return exitStartFailed
	}
	var ch *plugins.Channel
	ch = plugins.NewChannel(reader, writer, rt.handle, "plugin-host", func(string) { rt.stop() },
		func(origin any) context.Context { return ch.ContextOf(origin) })
	rt.ch = ch
	ch.Start()
	select {
	case <-rt.stopped:
	case <-ctx.Done():
	}
	ch.Close("host shutdown")
	return 0
}

// One shot profile extraction that prints the profiles as JSON on the protocol stream
func extractProfilesMain(h *pyhost.Host, writer io.Writer, path, moduleName string) int {
	rt := newRuntime(h)
	var text string
	err := h.Do(func(g *pyhost.GIL) error {
		return run(g, func(p py) {
			var payload map[string]any
			err := p.attempt(func() {
				entries := []any{}
				for _, profile := range rt.captureProfiles(p, path, moduleName) {
					entries = append(entries, rt.extractionEntry(p, profile))
				}
				payload = map[string]any{"profiles": entries}
			}, "Exception")
			if err != nil {
				pe := err.(*pyhost.PyError)
				payload = map[string]any{"error": pe.Name() + ": " + pe.Message}
			}
			text = p.val(p.m(p.imp("json"), "dumps", payload)).(string)
		})
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin host: %v\n", err)
		return 1
	}
	if _, err := io.WriteString(writer, text); err != nil {
		return 1
	}
	return 0
}

// Runs a memory provider's argparse command on this process's stdio and writes its outcome to resultPath
func runMemoryCLIMain(h *pyhost.Host, paramsJSON, resultPath string) int {
	var params map[string]any
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		fmt.Fprintf(os.Stderr, "plugin host: %v\n", err)
		return 1
	}
	name := pyStr(params["name"])
	dispatched, rc := true, int64(0)
	err := h.Do(func(g *pyhost.GIL) error {
		return run(g, func(p py) {
			err := p.attempt(func() {
				cli := importMemoryCLI(p, pyStr(params["module_name"]), pyStr(params["path"]))
				argparse := p.imp("argparse")
				description := ""
				if d, ok := params["description"].(string); ok {
					description = d
				}
				parser := p.callKw(p.get(argparse, "ArgumentParser"), nil, map[string]any{"prog": "icebath " + name,
					"description": description, "formatter_class": p.get(argparse, "RawDescriptionHelpFormatter")})
				p.m(cli, "register_cli", parser)
				if truthyParam(params["handler"]) {
					p.mKw(parser, "set_defaults", nil, map[string]any{"func": p.get(cli, name+"_command")})
				}
				argv := []any{}
				for _, a := range orList(params["argv"]).([]any) {
					argv = append(argv, pyStr(a))
				}
				parsed := p.m(parser, "parse_args", argv)
				fn := p.getOr(parsed, "func")
				if fn == nil || p.isNone(fn) {
					dispatched = false
					return
				}
				result := p.call(fn, parsed)
				if p.g.KindOf(result.Ptr()) == pyhost.KindInt || p.g.KindOf(result.Ptr()) == pyhost.KindBool {
					rc = p.val(p.call(p.attr("builtins", "int"), result)).(int64)
				}
			}, "BaseException")
			if err == nil {
				return
			}
			pe := err.(*pyhost.PyError)
			exc := pe.Exception()
			switch {
			case p.isinstance(exc, p.attr("builtins", "SystemExit")):
				rc = exitCode(p, p.get(exc, "code"))
			case p.isinstance(exc, p.attr("builtins", "Exception")):
				p.m(p.imp("traceback"), "print_exception", exc)
				rc = 1
			default:
				throw(err)
			}
		})
	})
	h.Do(func(g *pyhost.GIL) error {
		return run(g, func(p py) {
			p.m(p.attr("sys", "stdout"), "flush")
			p.m(p.attr("sys", "stderr"), "flush")
		})
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin host: %v\n", err)
		return 1
	}
	outcome, _ := json.Marshal(map[string]any{"rc": rc, "dispatched": dispatched})
	if err := os.WriteFile(resultPath, outcome, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "plugin host: %v\n", err)
		return 1
	}
	return 0
}

// Process status SystemExit(code) ends Python with, printing a code that is not an integer
func exitCode(p py, code Ref) int64 {
	if p.isNone(code) {
		return 0
	}
	if p.isinstance(code, p.attr("builtins", "int")) {
		return p.val(p.call(p.attr("builtins", "int"), code)).(int64)
	}
	p.mKw(p.imp("builtins"), "print", []any{code}, map[string]any{"file": p.attr("sys", "stderr")})
	return 1
}
