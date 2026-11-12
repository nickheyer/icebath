package plugins

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

const cliPlugin = `
import argparse, sys

def _greet(args):
    for _ in range(args.times):
        print(("HELLO " if args.shout else "hello ") + args.name + " via " + args.command)
    return 3

def _root(args):
    print("root handler", args.command, sorted(k for k in vars(args) if k != "func"))
    return None

def _setup(parser):
    parser.add_argument("--color", choices=["red", "blue"], default="red")
    sub = parser.add_subparsers(dest="hello_command")
    greet = sub.add_parser("greet", help="Greet someone")
    greet.add_argument("name")
    greet.add_argument("--times", type=int, default=1)
    greet.add_argument("--shout", action="store_true")
    greet.set_defaults(func=_greet)
    fail = sub.add_parser("fail")
    fail.set_defaults(func=lambda args: sys.exit("boom exit"))

def register(ctx):
    ctx.register_cli_command(name="hello", help="Say hello", setup_fn=_setup, handler_fn=_root, description="Hello tool")
`

const brokenCLIPlugin = `
def _setup(parser):
    parser.add_argument("--early")
    raise RuntimeError("setup exploded")

def register(ctx):
    ctx.register_cli_command("broken", "Broken", _setup, lambda args: 0)
`

// Output the child wrote to the terminal streams the host was started with during start
func captureTerminal(t *testing.T, start func()) (stdout func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousOut, previousErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	start()
	os.Stdout, os.Stderr = previousOut, previousErr
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	return func() string {
		_ = w.Close()
		<-done
		return buf.String()
	}
}

func TestLiveInProcessPluginCLICommandParsesWithItsOwnParserOnTheTerminal(t *testing.T) {
	installLiveServices(t)
	previous := installedServices()
	withSDK := *previous
	withSDK.PluginSDK = func(_ context.Context, _, op string, args []any, _ map[string]any) (any, error) {
		if op == "moved_command_hint" {
			return "", nil
		}
		return nil, &HostBuiltinError{Type: "ValueError", Message: op}
	}
	InstallServices(&withSDK)
	var m *Manager
	out := captureTerminal(t, func() {
		_, m = liveHomeIsolated(t, liveInterpreters(t)["system"], map[string]string{"clihello": cliPlugin, "clibroken": brokenCLIPlugin}, IsolationInProcess)
	})
	if e := loadedError(m, "clihello"); e != "" {
		t.Fatalf("clihello failed to load: %s", e)
	}
	cmds := m.CLICommands()
	hello := cmds["hello"]
	if hello == nil || hello.Parse == nil || hello.Help != "Say hello" || hello.Description != "Hello tool" {
		t.Fatalf("hello command = %+v", hello)
	}
	hello.Setup(nil)
	ctx := context.Background()
	top := map[string]any{"command": "hello", "verbose": false}
	cases := []struct {
		argv       []string
		code       int
		exited     bool
		dispatched bool
		extras     []string
	}{
		{argv: []string{"greet", "bob", "--times", "2", "--shout"}, code: 3, dispatched: true},
		{argv: nil, dispatched: true},
		{argv: []string{"greet", "--help"}, exited: true},
		{argv: []string{"nope"}, code: 2, exited: true},
		{argv: []string{"--color", "green"}, code: 2, exited: true},
		{argv: []string{"greet", "amy", "stray"}, extras: []string{"stray"}},
		{argv: []string{"fail"}, code: 1, exited: true, dispatched: true},
	}
	for _, tc := range cases {
		got, err := hello.Parse(ctx, top, tc.argv)
		if err != nil {
			t.Fatalf("%v: %v", tc.argv, err)
		}
		if got.Code != tc.code || got.Exited != tc.exited || got.Dispatched != tc.dispatched || strings.Join(got.Extras, " ") != strings.Join(tc.extras, " ") {
			t.Fatalf("%v: outcome %+v", tc.argv, got)
		}
	}
	broken := cmds["broken"]
	if broken == nil || broken.Parse == nil {
		t.Fatalf("broken command = %+v", broken)
	}
	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(r.(error).Error(), "setup exploded") {
				t.Fatalf("setup recovered %v", r)
			}
		}()
		broken.Setup(nil)
	}()
	got, err := broken.Parse(ctx, map[string]any{"command": "broken"}, []string{"--early", "x"})
	if err != nil || got.Dispatched || got.Exited || got.Code != 0 {
		t.Fatalf("half built parser outcome %+v %v", got, err)
	}
	m.pluginHostInstance.Shutdown()
	text := out()
	for _, want := range []string{"HELLO bob via hello\nHELLO bob via hello\n", "root handler hello ['color', 'command', 'hello_command', 'verbose']",
		"usage: icebath hello greet [-h]", "icebath hello: 'nope' is not a `icebath hello` command.\nRun `icebath hello --help` to see all commands.\n",
		"argument --color: invalid choice: 'green'", "boom exit"} {
		if !strings.Contains(text, want) {
			t.Fatalf("terminal output lacks %q:\n%s", want, text)
		}
	}
}
